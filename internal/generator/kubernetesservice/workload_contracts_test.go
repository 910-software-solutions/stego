package kubernetesservice

import (
	"os"
	"os/exec"
	"path/filepath"

	"bytes"
	"encoding/json"
	"fmt"
	"github.com/jsell-rh/stego/internal/generator/kubernetesworkload"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
)

// field reads a nested field of a decoded object by the keys given.
func field(value any, keys ...string) any {
	for _, key := range keys {
		object, _ := value.(map[string]any)
		value = object[key]
	}
	return value
}

func workloadFiles(t *testing.T) []gen.File {
	t.Helper()
	files, _, err := (&Generator{}).Generate(rpcContext())
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// The contracts of the rendered manifests (OC01, OC02, OC05, OC06, OC07, OC37, OC39) are tested on the output of the
// generated render command, in testdata/render_contracts_test.go, which TestGeneratedDeploymentRenderer runs.

// ignored tells whether a build context path is left out by the patterns of a .dockerignore file, as Docker reads
// them: each pattern in order, the last one that matches decides, and ! admits again. A pattern matches the path or
// a folder that holds it; ** matches any number of path elements, * any run of characters other than /, and ? one.
// moby/patternmatcher, which BuildKit uses, is not a dependency of STEGO: adding it moves other modules of go.mod,
// so TestIgnoreMatcher holds this matcher to the cases of the Docker documentation instead.
func ignored(t *testing.T, patterns []string, name string) bool {
	t.Helper()
	excluded := false
	for _, pattern := range patterns {
		admit := strings.HasPrefix(pattern, "!")
		pattern = strings.TrimSuffix(strings.TrimPrefix(pattern, "!"), "/")
		expression := "^" + strings.NewReplacer(`\*\*/`, `(.*/)?`, `\*\*`, `.*`, `\*`, `[^/]*`, `\?`, `[^/]`).Replace(regexp.QuoteMeta(pattern)) + "(/.*)?$"
		matcher, err := regexp.Compile(expression)
		if err != nil {
			t.Fatalf("the pattern %q: %v", pattern, err)
		}
		if matcher.MatchString(name) {
			excluded = !admit
		}
	}
	return excluded
}

func TestIgnoreMatcher(t *testing.T) {
	t.Run("ST-SUP-RC15-OC31-T02 The build context admits only the module files and the declared source directories", func(t *testing.T) {
		// The cases of the .dockerignore rules in the Docker documentation, and of the patterns this component emits.
		for _, tc := range []struct {
			patterns []string
			name     string
			excluded bool
		}{
			{[]string{"*/temp*"}, "somedir/temporary.txt", true},
			{[]string{"*/temp*"}, "temp", false},
			{[]string{"*/*/temp*"}, "somedir/subdir/temporary.txt", true},
			{[]string{"temp?"}, "tempa", true},
			{[]string{"temp?"}, "tempab", false},
			{[]string{"**/*.go"}, "a/b/c.go", true},
			{[]string{"**/*.go"}, "c.go", true},
			{[]string{"*.md", "!README*.md"}, "README-secret.md", false},
			{[]string{"*.md", "!README*.md", "README-secret.md"}, "README-secret.md", true},
			{[]string{"**", "!internal/", "!internal/**"}, "internal/a/b.go", false},
			{[]string{"**", "!internal/", "!internal/**"}, "internals/a.go", true},
			{[]string{"**", "!internal/**", "**/.*"}, "internal/.env", true},
		} {
			if got := ignored(t, tc.patterns, tc.name); got != tc.excluded {
				t.Fatalf("%v and %s: excluded %v, want %v", tc.patterns, tc.name, got, tc.excluded)
			}
		}
	})
}

func TestImageContracts(t *testing.T) {
	files := workloadFiles(t)
	images := map[string]string{}
	ignores := map[string][]string{}
	for _, file := range files {
		switch path.Base(file.Path) {
		case "Containerfile":
			images[file.Path] = string(file.Content)
		case "Containerfile.dockerignore":
			ignores[file.Path] = strings.Fields(string(file.Content))
		}
	}
	if len(images) != 3 || len(ignores) != 3 {
		t.Fatalf("want the Containerfile and the ignore file of the service, the worker and the RPC process, got %d and %d", len(images), len(ignores))
	}
	stages := func(text string) []string { return regexp.MustCompile(`(?m)^FROM .*$`).FindAllString(text, -1) }

	t.Run("ST-SUP-RC15-OC31-T01 The build context ignore file excludes everything first", func(t *testing.T) {
		for name, patterns := range ignores {
			if patterns[0] != "**" && patterns[0] != "*" {
				t.Fatalf("%s starts with %q, not a pattern that excludes the whole context", name, patterns[0])
			}
		}
	})

	t.Run("ST-SUP-RC15-OC31-T02 The build context admits only the module files and the declared source directories", func(t *testing.T) {
		// The declared source directory of the test context is internal; the generated code is under out.
		for name, patterns := range ignores {
			for path, want := range map[string]bool{"go.mod": false, "go.sum": false, "internal/a.go": false, "out/x.go": false, "cmd/x/main.go": true, "docs/a.md": true, "Makefile": true} {
				if ignored(t, patterns, path) != want {
					t.Fatalf("%s: %s excluded is %v, want %v", name, path, !want, want)
				}
			}
		}
	})

	t.Run("ST-SUP-RC15-OC31-T03 Hidden files and private-key files stay out of the build context", func(t *testing.T) {
		for name, patterns := range ignores {
			for _, path := range []string{"internal/.env", "internal/.git/config", "out/.hidden", "internal/tls.key", "internal/server.pem", "internal/id.p12", "out/k.pfx",
				"internal/k.p8", "internal/id_rsa", "out/id_ed25519", "internal/id_ecdsa", "internal/key.der", "internal/store.jks", "internal/java.keystore", "internal/putty.ppk"} {
				if !ignored(t, patterns, path) {
					t.Fatalf("%s admits %s", name, path)
				}
			}
		}
	})

	t.Run("ST-SUP-RC15-OC32-T01 The Containerfile builds with CGO disabled", func(t *testing.T) {
		for name, text := range images {
			build := text[:strings.Index(text, stages(text)[1])]
			// The last setting of CGO_ENABLED before the go build is 0: an ENV or an inline setting after it, or a
			// later one of 1, fails the test.
			goBuild := regexp.MustCompile(`(?m)^RUN .*\bgo build\b`).FindStringIndex(build)
			if goBuild == nil {
				t.Fatalf("%s: the builder stage runs no go build", name)
			}
			settings := regexp.MustCompile(`\bCGO_ENABLED=(\S+)`).FindAllStringSubmatchIndex(build, -1)
			last := ""
			for _, setting := range settings {
				if setting[0] < goBuild[1] {
					last = build[setting[2]:setting[3]]
				} else {
					t.Fatalf("%s: CGO_ENABLED is set after the go build:\n%s", name, build)
				}
			}
			if last != "0" {
				t.Fatalf("%s: the go build runs with CGO_ENABLED %q, want 0:\n%s", name, last, build)
			}
		}
	})

	t.Run("ST-SUP-RC15-OC32-T02 The builder image is pinned by digest", func(t *testing.T) {
		for name, text := range images {
			if !regexp.MustCompile(`^FROM \S+@sha256:[0-9a-f]{64} AS build$`).MatchString(stages(text)[0]) {
				t.Fatalf("%s: the builder stage %q is not pinned by digest", name, stages(text)[0])
			}
		}
	})

	t.Run("ST-SUP-RC15-OC32-T03 The runtime stage is scratch", func(t *testing.T) {
		for name, text := range images {
			if all := stages(text); len(all) != 2 || all[1] != "FROM scratch" {
				t.Fatalf("%s: the stages are %v, want the builder and FROM scratch", name, all)
			}
		}
	})

	t.Run("ST-SUP-RC15-OC32-T04 The image runs as user 65532:65532", func(t *testing.T) {
		for name, text := range images {
			runtime := text[strings.Index(text, "FROM scratch"):]
			if users := regexp.MustCompile(`(?m)^USER .*$`).FindAllString(runtime, -1); len(users) != 1 || users[0] != "USER 65532:65532" {
				t.Fatalf("%s: the runtime stage sets the users %v", name, users)
			}
		}
	})

	t.Run("ST-SUP-RC15-OC32-T05 The container starts an executable named explicitly", func(t *testing.T) {
		for name, text := range images {
			entry := regexp.MustCompile(`(?m)^ENTRYPOINT (.*)$`).FindAllStringSubmatch(text, -1)
			var form []string
			if len(entry) != 1 || json.Unmarshal([]byte(entry[0][1]), &form) != nil || len(form) != 1 || !strings.HasPrefix(form[0], "/") ||
				!strings.Contains(text, "--chmod=0555 "+form[0]+" "+form[0]+"\n") {
				t.Fatalf("%s: the ENTRYPOINT %v is not one absolute executable in exec form that the image holds", name, entry)
			}
		}
	})
}

// allocatorRules gives every rule of every ClusterRole that the rendered allocator manifest of a declaration binds to
// the allocator.
func allocatorRules(t *testing.T, c gen.Context) []map[string]any {
	t.Helper()
	files, _, err := new(Generator).Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	var rules []map[string]any
	for _, f := range files {
		if f.Path != "deploy/render/worker-queue.json.tmpl" {
			continue
		}
		tmpl, err := template.New("manifest").Funcs(template.FuncMap{"allocationID": func(string, string) string { return strings.Repeat("0", 32) }}).Parse(string(f.Content))
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := tmpl.Execute(&b, struct {
			Namespace, Image string
			FSGroup          int
		}{"allocation-check", "registry.test/widget@sha256:" + strings.Repeat("a", 64), 10001}); err != nil {
			t.Fatal(err)
		}
		var doc struct{ Items []map[string]any }
		if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		// The roles that a ClusterRoleBinding or a RoleBinding binds to the allocator's ServiceAccount, widget-queue,
		// whatever kind of role it refers to. The declared allocation roles are ClusterRoles too, which the allocator
		// grants to other accounts with the verb bind and holds no rule of itself: bind lets it make a binding of
		// the role for another account, not use the role, so they are not its own rules (review of #503, L7).
		bound := map[string]bool{}
		for _, item := range doc.Items {
			if item["kind"] != "ClusterRoleBinding" && item["kind"] != "RoleBinding" {
				continue
			}
			subjects, _ := item["subjects"].([]any)
			for _, subject := range subjects {
				if field(subject, "kind") == "ServiceAccount" && field(subject, "name") == "widget-queue" {
					bound[fmt.Sprint(field(item, "roleRef", "kind"))+"/"+fmt.Sprint(field(item, "roleRef", "name"))] = true
				}
			}
		}
		for _, item := range doc.Items {
			kind := fmt.Sprint(item["kind"])
			if (kind != "ClusterRole" && kind != "Role") || !bound[kind+"/"+fmt.Sprint(field(item, "metadata", "name"))] {
				continue
			}
			// An aggregated role takes the rules of every role that its selector matches, so no rule list of the
			// manifest bounds it.
			if item["aggregationRule"] != nil {
				t.Fatalf("the allocator holds the aggregated role %v", field(item, "metadata", "name"))
			}
			items, _ := item["rules"].([]any)
			for _, rule := range items {
				object, ok := rule.(map[string]any)
				if !ok {
					t.Fatalf("the role %v holds a rule that is not an object: %v", field(item, "metadata", "name"), rule)
				}
				rules = append(rules, object)
			}
		}
	}
	if len(rules) == 0 {
		t.Fatal("the allocator manifest holds no ClusterRole rule")
	}
	return rules
}

func TestAllocatorHasNoSecretAccess(t *testing.T) {
	declarations := map[string]func() gen.Context{"plain": allocationContext, "isolated": isolatedAllocationContext, "network-isolated": networkProfileContext, "peer": peerAllocationContext}
	strings := func(value any) []string {
		var out []string
		for _, item := range value.([]any) {
			out = append(out, item.(string))
		}
		return out
	}
	t.Run("ST-SUP-RC15-OC22-T01 No allocator ClusterRole grants access to Secrets", func(t *testing.T) {
		for name, declare := range declarations {
			for _, rule := range allocatorRules(t, declare()) {
				if rule["resources"] != nil && slices.Contains(strings(rule["resources"]), "secrets") {
					t.Fatalf("%s: the rule %v names secrets", name, rule)
				}
			}
		}
	})
	t.Run("ST-SUP-RC15-OC22-T02 No allocator ClusterRole holds a wildcard that would cover Secrets", func(t *testing.T) {
		for name, declare := range declarations {
			for _, rule := range allocatorRules(t, declare()) {
				if rule["resources"] == nil {
					continue
				}
				groups := []string{}
				if rule["apiGroups"] != nil {
					groups = strings(rule["apiGroups"])
				}
				for _, resource := range strings(rule["resources"]) {
					if (resource == "*" || resource == "*/*") && (slices.Contains(groups, "") || slices.Contains(groups, "*")) {
						t.Fatalf("%s: the rule %v grants every core resource", name, rule)
					}
					if covers, _ := path.Match(resource, "secrets"); covers && resource != "secrets" && (len(groups) == 0 || slices.Contains(groups, "") || slices.Contains(groups, "*")) {
						t.Fatalf("%s: the rule %v holds the pattern %q that covers secrets", name, rule, resource)
					}
				}
			}
		}
	})
}

// stegoKeys gives each label or annotation key of the STEGO key space that the text holds: the keys that start with
// stego. and a domain, in any quoting.
var stegoKeyPattern = regexp.MustCompile(`stego\.[a-z0-9.-]+/[A-Za-z0-9_.{}-]*`)

func TestKeyPrefix(t *testing.T) {
	t.Run("ST-SUP-RC15-OC46-T01 The emitted manifests and scripts write and reserve only keys of the STEGO prefix", func(t *testing.T) {
		contexts := map[string]gen.Context{"service": rpcContext(), "allocation": allocationContext(), "network": networkProfileContext(), "peer": peerAllocationContext(), "isolated": isolatedAllocationContext()}
		keys := 0
		for name, ctx := range contexts {
			files, _, err := new(Generator).Generate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				text := string(file.Content)
				if strings.Contains(text, "stego"+".dev") {
					t.Fatalf("%s: %s holds the old key prefix", name, file.Path)
				}
				for _, key := range stegoKeyPattern.FindAllString(text, -1) {
					keys++
					if !strings.HasPrefix(key, KeyPrefix) {
						t.Fatalf("%s: %s writes the key %q, which is not under %s", name, file.Path, key, KeyPrefix)
					}
				}
			}
		}
		// The kubernetes-workload component writes the configuration annotation of its Pods.
		workload, _, err := new(kubernetesworkload.Generator).Generate(gen.Context{OutputNamespace: "workload"})
		if err != nil {
			t.Fatal(err)
		}
		workloadKeys := 0
		for _, file := range workload {
			if strings.Contains(string(file.Content), "stego"+".dev") {
				t.Fatalf("kubernetes-workload: %s holds the old key prefix", file.Path)
			}
			for _, key := range stegoKeyPattern.FindAllString(string(file.Content), -1) {
				workloadKeys++
				if !strings.HasPrefix(key, KeyPrefix) {
					t.Fatalf("kubernetes-workload: %s writes the key %q", file.Path, key)
				}
			}
		}
		// The cluster check scripts write and select by the same keys.
		scripts, err := filepath.Glob(filepath.Join("..", "..", "..", "scripts", "*allocation*.py"))
		if err != nil || len(scripts) == 0 {
			t.Fatalf("no allocation check script: %v", err)
		}
		for _, script := range scripts {
			data, err := os.ReadFile(script)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "stego"+".dev") {
				t.Fatalf("%s holds the old key prefix", script)
			}
		}
		// No tracked file of the repository holds the old prefix, apart from the records under data/ that state the
		// decision, the tests that search for it on purpose, and the upgrade note that tells an operator to check for it.
		list := exec.Command("git", "grep", "-l", "stego"+".dev", "--", ".", ":!data")
		list.Dir = filepath.Join("..", "..", "..")
		output, err := list.Output()
		if err != nil && len(output) != 0 {
			t.Fatal(err)
		}
		for _, name := range strings.Fields(string(output)) {
			switch name {
			case "internal/generator/kubernetesservice/workload_contracts_test.go", "internal/generator/kubernetesclient/generator_test.go", "docs/upgrade.md":
			default:
				t.Fatalf("%s holds the old key prefix", name)
			}
		}
		if keys == 0 || workloadKeys == 0 {
			t.Fatalf("the emitted files hold %d STEGO keys and the workload files %d, so the test reads nothing", keys, workloadKeys)
		}
		// A declaration may not take a key under the STEGO prefix as the owner label of an allocation profile.
		for _, owner := range []string{KeyPrefix + "owner", "team." + KeyPrefix + "owner"} {
			ctx := allocationContext()
			ctx.ComponentConfig["allocation_profiles"].([]any)[0].(object)["owner_label"] = owner
			if _, _, err := new(Generator).Generate(ctx); err == nil {
				t.Fatalf("the owner label %q under the STEGO prefix was accepted", owner)
			}
		}
		// A namespace identity label may not take a key under the STEGO prefix or a subdomain of it either.
		for _, key := range []string{KeyPrefix + "linked-id", "team." + KeyPrefix + "linked-id"} {
			ctx := allocationContext()
			ctx.ComponentConfig["allocation_profiles"].([]any)[0].(object)["identity_labels"] = []any{object{"field": "linked_id", "key": key}}
			if _, _, err := new(Generator).Generate(ctx); err == nil {
				t.Fatalf("the identity label %q under the STEGO prefix was accepted", key)
			}
		}
		if !reservedKeyPrefix(KeyPrefix+"owner") || reservedKeyPrefix("example.test/owner") || reservedKeyPrefix("notstego.jsell-rh.github.io/owner") {
			t.Fatal("the reserved prefix is not exactly the STEGO domain and its subdomains")
		}
	})
}
