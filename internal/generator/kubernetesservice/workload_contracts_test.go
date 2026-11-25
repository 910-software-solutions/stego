package kubernetesservice

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
)

// manifestItems gives the items of each rendered manifest template of the files, by path, with the placeholders of
// the template given fixed values.
func manifestItems(t *testing.T, files []gen.File) map[string][]map[string]any {
	t.Helper()
	fill := strings.NewReplacer("{{.Namespace}}", "test", "{{.Image}}", "registry.example.test/widget@sha256:"+strings.Repeat("a", 64), "{{.FSGroup}}", "10001")
	manifests := map[string][]map[string]any{}
	for _, file := range files {
		if !strings.HasPrefix(file.Path, "deploy/render/") || !strings.HasSuffix(file.Path, ".json.tmpl") {
			continue
		}
		var list struct{ Items []map[string]any }
		if err := json.Unmarshal([]byte(fill.Replace(string(file.Content))), &list); err != nil {
			t.Fatalf("%s: %v", file.Path, err)
		}
		manifests[file.Path] = list.Items
	}
	if len(manifests) != 3 {
		t.Fatalf("want the service, worker and RPC manifests, got %d", len(manifests))
	}
	return manifests
}

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
	ctx := rpcContext()
	ctx.ComponentConfig["dns_namespace"] = "openshift-dns"
	ctx.ComponentConfig["dns_port"] = 5353
	files, _, err := (&Generator{}).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestWorkloadContracts(t *testing.T) {
	files := workloadFiles(t)
	manifests := manifestItems(t, files)
	kinds := func(items []map[string]any, kind string) []map[string]any {
		var found []map[string]any
		for _, item := range items {
			if item["kind"] == kind {
				found = append(found, item)
			}
		}
		return found
	}
	serviceContainer := func(t *testing.T) map[string]any {
		deployments := kinds(manifests["deploy/render/manifest.json.tmpl"], "Deployment")
		if len(deployments) != 1 {
			t.Fatalf("the service manifest holds %d Deployments", len(deployments))
		}
		return field(deployments[0], "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
	}

	t.Run("ST-SUP-RC15-OC02-T01 Every emitted Pod selects the RuntimeDefault seccomp profile", func(t *testing.T) {
		for name, items := range manifests {
			for _, deployment := range kinds(items, "Deployment") {
				if field(deployment, "spec", "template", "spec", "securityContext", "seccompProfile", "type") != "RuntimeDefault" {
					t.Fatalf("the Pod of %s in %s does not select RuntimeDefault", field(deployment, "metadata", "name"), name)
				}
			}
		}
	})

	t.Run("ST-SUP-RC15-OC05-T01 The startup and liveness probes use /livez", func(t *testing.T) {
		container := serviceContainer(t)
		for _, probe := range []string{"startupProbe", "livenessProbe"} {
			if got := field(container, probe, "httpGet", "path"); got != "/livez" {
				t.Fatalf("the %s path is %v, want /livez", probe, got)
			}
		}
	})

	t.Run("ST-SUP-RC15-OC05-T02 The readiness probe uses /readyz", func(t *testing.T) {
		if got := field(serviceContainer(t), "readinessProbe", "httpGet", "path"); got != "/readyz" {
			t.Fatalf("the readiness probe path is %v, want /readyz", got)
		}
	})

	t.Run("ST-SUP-RC15-OC06-T01 The NetworkPolicy declares both Ingress and Egress for every workload Pod", func(t *testing.T) {
		for name, items := range manifests {
			policies := kinds(items, "NetworkPolicy")
			for _, deployment := range kinds(items, "Deployment") {
				labels, _ := field(deployment, "spec", "template", "metadata", "labels").(map[string]any)
				selected := false
				for _, policy := range policies {
					types, _ := field(policy, "spec", "policyTypes").([]any)
					both := len(types) == 2 && types[0] == "Ingress" && types[1] == "Egress"
					match, _ := field(policy, "spec", "podSelector", "matchLabels").(map[string]any)
					covers := len(match) > 0
					for key, value := range match {
						covers = covers && labels[key] == value
					}
					selected = selected || (both && covers)
				}
				if !selected {
					t.Fatalf("no NetworkPolicy with Ingress and Egress selects the Pods of %s in %s", field(deployment, "metadata", "name"), name)
				}
			}
		}
	})

	t.Run("ST-SUP-RC15-OC07-T01 DNS egress goes only to the declared resolver namespace and port, over UDP and TCP", func(t *testing.T) {
		for name, items := range manifests {
			for _, policy := range kinds(items, "NetworkPolicy") {
				dns := 0
				for _, rule := range field(policy, "spec", "egress").([]any) {
					ports, _ := field(rule, "ports").([]any)
					protocols := map[any]any{}
					for _, port := range ports {
						protocols[field(port, "protocol")] = field(port, "port")
					}
					if _, udp := protocols["UDP"]; !udp {
						continue
					}
					dns++
					to := field(rule, "to").([]any)
					if len(to) != 1 || field(to[0], "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name") != "openshift-dns" ||
						protocols["UDP"] != float64(5353) || protocols["TCP"] != float64(5353) || len(ports) != 2 {
						t.Fatalf("%s: the DNS rule %v is not to openshift-dns on 5353 over UDP and TCP only", name, rule)
					}
				}
				if dns != 1 {
					t.Fatalf("%s: %d DNS rules, want one", name, dns)
				}
			}
		}
	})

	t.Run("ST-SUP-RC15-OC39-T03 The emitted network policy uses no Technology Preview resource", func(t *testing.T) {
		for name, items := range manifests {
			for _, item := range items {
				kind, version := item["kind"], item["apiVersion"]
				switch kind {
				case "AdminNetworkPolicy", "BaselineAdminNetworkPolicy", "EgressFirewall", "DNSNameResolver":
					t.Fatalf("%s holds the Technology Preview resource %s", name, kind)
				case "NetworkPolicy":
					if version != "networking.k8s.io/v1" {
						t.Fatalf("%s holds a NetworkPolicy of %v", name, version)
					}
				}
				if strings.Contains(fmt.Sprint(version), "policy.networking.k8s.io") || strings.Contains(fmt.Sprint(version), "openshift.io") {
					t.Fatalf("%s holds %v %v", name, version, kind)
				}
			}
		}
	})

	t.Run("ST-SUP-RC15-OC37-T01 The service container sets STEGO_HTTP_REQUIRE_TLS to 1", func(t *testing.T) {
		for _, variable := range serviceContainer(t)["env"].([]any) {
			if field(variable, "name") == "STEGO_HTTP_REQUIRE_TLS" {
				if field(variable, "value") != "1" {
					t.Fatalf("STEGO_HTTP_REQUIRE_TLS is %v", field(variable, "value"))
				}
				return
			}
		}
		t.Fatal("the service container does not set STEGO_HTTP_REQUIRE_TLS")
	})

	t.Run("ST-SUP-RC15-OC37-T02 The TLS settings point at the mounted key pair", func(t *testing.T) {
		container := serviceContainer(t)
		env := map[any]any{}
		for _, variable := range container["env"].([]any) {
			env[field(variable, "name")] = field(variable, "value")
		}
		deployment := kinds(manifests["deploy/render/manifest.json.tmpl"], "Deployment")[0]
		secrets := map[any]any{}
		for _, volume := range field(deployment, "spec", "template", "spec", "volumes").([]any) {
			secrets[field(volume, "name")] = field(volume, "secret", "secretName")
		}
		mounted := ""
		for _, mount := range container["volumeMounts"].([]any) {
			if secrets[field(mount, "name")] == "widget-files" {
				mounted, _ = field(mount, "mountPath").(string)
			}
		}
		if mounted == "" || env["STEGO_HTTP_TLS_CERT"] != path.Join(mounted, "tls.crt") || env["STEGO_HTTP_TLS_KEY"] != path.Join(mounted, "tls.key") {
			t.Fatalf("the TLS settings %v and %v do not name the key pair under the mount %q of the service Secret", env["STEGO_HTTP_TLS_CERT"], env["STEGO_HTTP_TLS_KEY"], mounted)
		}
	})
}

// ignored tells whether a build context path is left out by the patterns of a .dockerignore file, as Docker reads
// them: each pattern in order, the last one that matches decides, and ! admits again. A pattern matches the path or
// a folder that holds it; ** matches any number of path elements.
func ignored(patterns []string, name string) bool {
	excluded := false
	for _, pattern := range patterns {
		admit := strings.HasPrefix(pattern, "!")
		pattern = strings.TrimSuffix(strings.TrimPrefix(pattern, "!"), "/")
		expression := "^" + strings.NewReplacer(`\*\*/`, `(.*/)?`, `\*\*`, `.*`, `\*`, `[^/]*`, `\?`, `[^/]`).Replace(regexp.QuoteMeta(pattern)) + "(/.*)?$"
		if regexp.MustCompile(expression).MatchString(name) {
			excluded = !admit
		}
	}
	return excluded
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
				if ignored(patterns, path) != want {
					t.Fatalf("%s: %s excluded is %v, want %v", name, path, !want, want)
				}
			}
		}
	})

	t.Run("ST-SUP-RC15-OC31-T03 Hidden files and private-key files stay out of the build context", func(t *testing.T) {
		for name, patterns := range ignores {
			for _, path := range []string{"internal/.env", "internal/.git/config", "out/.hidden", "internal/tls.key", "internal/server.pem", "internal/id.p12", "out/k.pfx"} {
				if !ignored(patterns, path) {
					t.Fatalf("%s admits %s", name, path)
				}
			}
		}
	})

	t.Run("ST-SUP-RC15-OC32-T01 The Containerfile builds with CGO disabled", func(t *testing.T) {
		for name, text := range images {
			build := text[:strings.Index(text, stages(text)[1])]
			if !regexp.MustCompile(`(?m)^ENV .*\bCGO_ENABLED=0\b`).MatchString(build) || !strings.Contains(build, "go build") {
				t.Fatalf("%s: the builder stage does not build with CGO_ENABLED=0:\n%s", name, build)
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
		// The ClusterRoles that a ClusterRoleBinding binds to the allocator's ServiceAccount, widget-queue: the declared
		// allocation roles are ClusterRoles too, which the allocator grants to other accounts, and are not its own.
		bound := map[any]bool{}
		for _, item := range doc.Items {
			if item["kind"] != "ClusterRoleBinding" {
				continue
			}
			for _, subject := range item["subjects"].([]any) {
				if field(subject, "kind") == "ServiceAccount" && field(subject, "name") == "widget-queue" {
					bound[field(item, "roleRef", "name")] = true
				}
			}
		}
		for _, item := range doc.Items {
			if item["kind"] != "ClusterRole" || !bound[field(item, "metadata", "name")] {
				continue
			}
			for _, rule := range item["rules"].([]any) {
				rules = append(rules, rule.(map[string]any))
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
