package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/delivery"
)

// These tests prove that apply writes the platform requirements of a service from the needs that its components
// state (ST-DLV-UC01), on the service of ST-DLV-TD01.

// requirementsDeclaration is the rest-crud declaration of the service orders of ST-DLV-TD01: postgres-adapter, a
// Kafka producer through kafka-events, the SSO JWT verifier rh-sso-auth and health-check. The issuer setting holds a
// host name, which the platform requirements must not take.
const requirementsDeclaration = `kind: service
name: orders
archetype: rest-crud
language: go
tenancy: {kind: pooled, key: tenant_id, claim: org_id, provisioning: {resource_type: service, provision: provision, retire: retire}}
authorization: {provider: spicedb, schema_path: authz/schema.zed, subject_from_identity: user}
audit: {level: change}
mixins: [durable-events, kafka-events]
overrides:
  auth-provider: rh-sso-auth
  rh-sso-auth: {issuer: https://sso.example.com/auth/realms/example, audience: orders}
  postgres-adapter: {schema_generation: tables-v1}
entities:
  - name: Order
    resource_type: order
    permissions: {create: public, read: view, list: view}
    public_reasons: {create: a new order has no resource to decide on before it exists}
    fields: [{name: item, type: string}]
collections:
  orders: {entity: Order, operations: [create, read, list]}
`

// requirementsNeeds are the needs that the components of the fixture registry state: between them one need of each
// of the nine kinds of ST-VO53. The names of postgres-adapter hold the service name through {service}.
var requirementsNeeds = map[string]string{
	"postgres-adapter": `  - {kind: secret, name: "{service}-database", keys: [url, password]}
  - {kind: backing store, name: "{service}-db", engine: postgres}
`,
	"kafka-producer": `  - {kind: topic, name: order-events}
`,
	"rh-sso-auth": `  - {kind: identity issuer, name: sso}
  - {kind: egress, name: payments, port: 443}
`,
	"health-check": `  - {kind: configuration, name: LOG_LEVEL}
  - {kind: ingress, name: http, port: 8080}
  - {kind: cluster permission, name: read-config-maps, resource: configmaps, verbs: [get]}
  - {kind: pod security, name: pod-security, level: restricted}
`,
}

// requirementsRegistry gives a copy of the shipped registry whose components state the needs of ST-DLV-TD01.
func requirementsRegistry(t *testing.T) string {
	t.Helper()
	registry := shippedRegistryCopy(t)
	for component, needs := range requirementsNeeds {
		setNeeds(t, registry, component, needs)
	}
	return registry
}

// setNeeds gives the component of the registry copy the needs list, in place of the one it states.
func setNeeds(t *testing.T, registry, component, needs string) {
	t.Helper()
	path := filepath.Join(registry, "components", component, "component.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text, _, _ := strings.Cut(string(data), "\nneeds:\n")
	text = strings.TrimRight(text, "\n") + "\n"
	if needs != "" {
		text += "needs:\n" + needs
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// requirementsProject makes the project of ST-DLV-TD01 in a new folder, as a Git repository with one commit, and
// gives the folder.
func requirementsProject(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "orders")
	writeRequirementsDeclaration(t, dir, requirementsDeclaration)
	git(t, dir, "init", "-q")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "the service orders")
	return dir
}

func writeRequirementsDeclaration(t *testing.T, dir, declaration string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "service.yaml"), []byte(declaration), 0o644); err != nil {
		t.Fatal(err)
	}
	withRuntimeSchema(t, dir, "service.yaml", declaration)
}

// requirementsEnv is the environment of a run of stego on the project of ST-DLV-TD01.
func requirementsEnv(registry string) []string {
	return []string{"STEGO_REGISTRY=" + registry, "STEGO_MODULE=example.com/orders", "STEGO_GO_VERSION=1.26.11"}
}

// runInProject runs the stego binary in the folder with the environment added.
func runInProject(t *testing.T, dir string, env []string, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, stegoBinary(t), args...)
	command.Dir = dir
	command.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func mustRunInProject(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	stdout, stderr, err := runInProject(t, dir, env, args...)
	if err != nil {
		t.Fatalf("stego %s: %v\n%s\n%s", strings.Join(args, " "), err, stdout, stderr)
	}
	return stdout
}

// readRequirements reads the platform requirements of the project through the strict decoder.
func readRequirements(t *testing.T, dir string) (delivery.Requirements, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, compiler.RequirementsFile))
	if err != nil {
		t.Fatal(err)
	}
	requirements, err := delivery.LoadRequirements(data)
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	return requirements, data
}

// folderDigests gives the digest of each file of the folder, out of .git.
func folderDigests(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		relative, _ := filepath.Rel(dir, path)
		files[filepath.ToSlash(relative)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// wantRequirements are the nine requirements that the components of ST-DLV-TD01 state, each with its component.
var wantRequirements = []delivery.Requirement{
	{Kind: delivery.Secret, Name: "orders-database", Keys: []string{"password", "url"}, Components: []string{"postgres-adapter"}},
	{Kind: delivery.Configuration, Name: "LOG_LEVEL", Components: []string{"health-check"}},
	{Kind: delivery.BackingStore, Name: "orders-db", Engine: "postgres", Components: []string{"postgres-adapter"}},
	{Kind: delivery.Topic, Name: "order-events", Components: []string{"kafka-producer"}},
	{Kind: delivery.IdentityIssuer, Name: "sso", Components: []string{"rh-sso-auth"}},
	{Kind: delivery.Ingress, Name: "http", Port: 8080, Components: []string{"health-check"}},
	{Kind: delivery.Egress, Name: "payments", Port: 443, Components: []string{"rh-sso-auth"}},
	{Kind: delivery.ClusterPermission, Name: "read-config-maps", Resource: "configmaps", Verbs: []string{"get"}, Components: []string{"health-check"}},
	{Kind: delivery.PodSecurity, Name: "pod-security", Level: "restricted", Components: []string{"health-check"}},
}

// byName gives the requirements by their names.
func byName(requirements []delivery.Requirement) map[string]delivery.Requirement {
	named := map[string]delivery.Requirement{}
	for _, requirement := range requirements {
		named[requirement.Name] = requirement
	}
	return named
}

// expectStated fails the test unless each wanted requirement is written, with its kind, shape and components.
func expectStated(t *testing.T, written []delivery.Requirement, want []delivery.Requirement) {
	t.Helper()
	got := byName(written)
	for _, requirement := range want {
		found, ok := got[requirement.Name]
		if !ok {
			t.Errorf("the platform requirements do not state the need %s", requirement.Name)
			continue
		}
		if !requirementsEqual(found, requirement) {
			t.Errorf("the platform requirements state %s as %+v, want %+v", requirement.Name, found, requirement)
		}
	}
}

func requirementsEqual(a, b delivery.Requirement) bool {
	return a.Kind == b.Kind && a.Name == b.Name && slices.Equal(a.Keys, b.Keys) && a.Engine == b.Engine && a.Port == b.Port &&
		a.APIGroup == b.APIGroup && a.Resource == b.Resource && slices.Equal(a.Verbs, b.Verbs) && a.Level == b.Level && slices.Equal(a.Components, b.Components)
}

func TestPlatformRequirementsApply(t *testing.T) {
	t.Run("ST-DLV-UC01-R01-T01 Applying the plan writes platform requirements that state each need of the components, with its keys and ports", func(t *testing.T) {
		registry := requirementsRegistry(t)
		dir := requirementsProject(t)
		env := requirementsEnv(registry)
		plan := mustRunInProject(t, dir, env, "plan")
		if !strings.Contains(plan, "generate: "+compiler.RequirementsFile) {
			t.Fatalf("the plan does not list %s:\n%s", compiler.RequirementsFile, plan)
		}
		mustRunInProject(t, dir, env, "apply")
		requirements, data := readRequirements(t, dir)
		expectStated(t, requirements.Requirements, wantRequirements)
		// The environment check tests keep this file as the fixture of ST-DLV-TD02.
		fixture, err := os.ReadFile(filepath.Join(environmentFixtureDir, "platform-requirements.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, fixture) {
			t.Fatalf("the fixture of ST-DLV-TD02 is not the file that apply writes:\n%s", data)
		}
	})

	t.Run("ST-DLV-UC01-R02-T01 The written platform requirements hold no value that differs per environment", func(t *testing.T) {
		registry := requirementsRegistry(t)
		dir := requirementsProject(t)
		mustRunInProject(t, dir, requirementsEnv(registry), "apply")
		_, data := readRequirements(t, dir)
		expectOnlyRequirementFields(t, data)
	})

	t.Run("ST-DLV-UC01-R02-T02 A component need that holds a host name stops the apply, which names the component and the need and writes nothing", func(t *testing.T) {
		registry := requirementsRegistry(t)
		setNeeds(t, registry, "postgres-adapter", `  - {kind: secret, name: "{service}-database", keys: [url, password], host: db.prod.example.com}
  - {kind: backing store, name: "{service}-db", engine: postgres}
`)
		dir := requirementsProject(t)
		before := folderDigests(t, dir)
		_, stderr, err := runInProject(t, dir, requirementsEnv(registry), "apply")
		if err == nil {
			t.Fatal("apply accepted a need that holds a host name")
		}
		for _, part := range []string{"postgres-adapter", "orders-database", "host"} {
			if !strings.Contains(stderr, part) {
				t.Errorf("the refusal does not name %s:\n%s", part, stderr)
			}
		}
		if after := folderDigests(t, dir); !maps.Equal(before, after) {
			t.Fatalf("the refused apply changed the project folder: %v, then %v", before, after)
		}
	})

	t.Run("ST-DLV-UC01-R03-T01 Two applies of the same declaration and registry write the same platform requirements bytes", func(t *testing.T) {
		registry := requirementsRegistry(t)
		first := requirementsProject(t)
		second := filepath.Join(t.TempDir(), "elsewhere", "deeper", "orders")
		if err := os.MkdirAll(filepath.Dir(second), 0o755); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command("cp", "-a", first, second).CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, output)
		}
		mustRunInProject(t, first, requirementsEnv(registry), "apply")
		time.Sleep(1100 * time.Millisecond)
		mustRunInProject(t, second, append(requirementsEnv(registry), "TZ=Pacific/Kiritimati", "LANG=de_DE.UTF-8", "HOSTNAME=another-host", "STEGO_UNRELATED=1"), "apply")
		_, a := readRequirements(t, first)
		_, b := readRequirements(t, second)
		if !bytes.Equal(a, b) {
			t.Fatalf("the two applies wrote different platform requirements:\n%s\n---\n%s", a, b)
		}
	})

	t.Run("ST-DLV-UC01-R03-T02 A second apply with no change leaves the platform requirements as they are", func(t *testing.T) {
		registry := requirementsRegistry(t)
		dir := requirementsProject(t)
		mustRunInProject(t, dir, requirementsEnv(registry), "apply")
		_, first := readRequirements(t, dir)
		mustRunInProject(t, dir, requirementsEnv(registry), "apply")
		_, second := readRequirements(t, dir)
		if !bytes.Equal(first, second) {
			t.Fatalf("the second apply changed the platform requirements:\n%s\n---\n%s", first, second)
		}
	})

	// conflict states a second need orders-database for the Kafka producer, and expects the refusal to name both
	// components, the need and each of the parts, the kinds or the shapes, that differ.
	conflict := func(t *testing.T, kafkaNeeds string, differ ...string) {
		t.Helper()
		registry := requirementsRegistry(t)
		setNeeds(t, registry, "kafka-producer", requirementsNeeds["kafka-producer"]+kafkaNeeds)
		dir := requirementsProject(t)
		before := folderDigests(t, dir)
		_, stderr, err := runInProject(t, dir, requirementsEnv(registry), "apply")
		if err == nil {
			t.Fatal("apply accepted one need that two components state differently")
		}
		for _, part := range append([]string{"kafka-producer", "postgres-adapter", "orders-database"}, differ...) {
			if !strings.Contains(stderr, part) {
				t.Errorf("the refusal does not name %s:\n%s", part, stderr)
			}
		}
		if after := folderDigests(t, dir); !maps.Equal(before, after) {
			t.Fatalf("the refused apply changed the project folder: %v, then %v", before, after)
		}
		if _, err := os.Stat(filepath.Join(dir, ".stego", "state.yaml")); !os.IsNotExist(err) {
			t.Fatalf("the refused apply wrote a generation record: %v", err)
		}
	}
	t.Run("ST-DLV-UC01-R04-T01 Two components that state one need with different kinds stop the apply, which names both and the need and writes nothing", func(t *testing.T) {
		conflict(t, `  - {kind: configuration, name: "{service}-database"}
`, "different kinds", "configuration", "secret")
	})
	t.Run("ST-DLV-UC01-R04-T02 Two components that state one need with different shapes stop the apply, which names both and the need and writes nothing", func(t *testing.T) {
		conflict(t, `  - {kind: secret, name: "{service}-database", keys: [dsn]}
`, "different shapes", "keys dsn", "keys password, url")
	})

	handEdited := func(t *testing.T) (string, []string) {
		t.Helper()
		registry := requirementsRegistry(t)
		dir := requirementsProject(t)
		env := requirementsEnv(registry)
		mustRunInProject(t, dir, env, "apply")
		path := filepath.Join(dir, compiler.RequirementsFile)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		edited := strings.Replace(string(data), "port: 443", "port: 8443", 1)
		if edited == string(data) {
			t.Fatalf("the platform requirements hold no line port: 443:\n%s", data)
		}
		if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir, env
	}
	t.Run("ST-DLV-UC01-R05-T01 Applying the plan refuses a hand edit of the platform requirements and names the file", func(t *testing.T) {
		dir, env := handEdited(t)
		before := folderDigests(t, dir)
		_, stderr, err := runInProject(t, dir, env, "apply")
		if err == nil {
			t.Fatal("apply accepted a hand edit of the platform requirements")
		}
		if !strings.Contains(stderr, "hand-edited") || !strings.Contains(stderr, compiler.RequirementsFile) {
			t.Fatalf("the refusal does not name %s as edited by hand:\n%s", compiler.RequirementsFile, stderr)
		}
		if after := folderDigests(t, dir); !maps.Equal(before, after) {
			t.Fatalf("the refused apply changed the project folder: %v, then %v", before, after)
		}
	})
	t.Run("ST-DLV-UC01-R05-T02 The drift check names a hand edit of the platform requirements and fails", func(t *testing.T) {
		dir, env := handEdited(t)
		stdout, stderr, err := runInProject(t, dir, env, "drift")
		if err == nil {
			t.Fatalf("drift passed after a hand edit of the platform requirements:\n%s", stdout)
		}
		if !strings.Contains(stdout+stderr, "modified: "+compiler.RequirementsFile) {
			t.Fatalf("drift does not name %s as drifted:\n%s\n%s", compiler.RequirementsFile, stdout, stderr)
		}
	})

	t.Run("ST-DLV-UC01-FL-T Applying the plan writes the platform requirements with the rest of the generated files", func(t *testing.T) {
		registry := requirementsRegistry(t)
		dir := requirementsProject(t)
		stdout := mustRunInProject(t, dir, requirementsEnv(registry), "apply")
		if !strings.Contains(stdout, "generate: "+compiler.RequirementsFile) || !strings.Contains(stdout, "generate: main.go") {
			t.Fatalf("the plan of apply does not list the platform requirements with the other generated files:\n%s", stdout)
		}
		requirements, data := readRequirements(t, dir)
		expectStated(t, requirements.Requirements, wantRequirements)
		if len(requirements.Requirements) != len(wantRequirements) {
			t.Fatalf("the platform requirements state %d needs, want the nine of the components", len(requirements.Requirements))
		}
		if compiler.RequirementsFile != delivery.RequirementsFile {
			t.Fatalf("the compiler writes the platform requirements at %s, and Delivering names %s", compiler.RequirementsFile, delivery.RequirementsFile)
		}
		state, err := compiler.LoadState(filepath.Join(dir, ".stego", "state.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if got := state.LastApplied.ProjectFiles[compiler.RequirementsFile]; got != hex.EncodeToString(sum[:]) {
			t.Fatalf("the generation record lists %s with the hash %q, want %x", compiler.RequirementsFile, got, sum)
		}
		if len(state.LastApplied.Files) == 0 {
			t.Fatal("the generation record lists no other generated file")
		}
	})
}

// requirementFields are the fields a requirement may have, and the shape fields of each kind.
var (
	requirementFields = []string{"kind", "name", "components"}
	kindShapes        = map[string][]string{"secret": {"keys"}, "backing store": {"engine"}, "ingress": {"port"}, "egress": {"port"},
		"cluster permission": {"api_group", "resource", "verbs"}, "pod security": {"level"}}
	// environmentValue is a value that only an environment gives: a host name, an IP address or range, a URL or a
	// path, so any text with a dot, a colon, a slash or an @.
	environmentValue = regexp.MustCompile(`[.:/@]`)
)

// expectOnlyRequirementFields fails the test unless each entry of the platform requirements holds only a kind, a
// name, the shape of its kind and the components that need it, and no value holds an environment value.
func expectOnlyRequirementFields(t *testing.T, data []byte) {
	t.Helper()
	var file map[string]any
	if err := yaml.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(file)); !slices.Equal(keys, []string{"requirements", "service", "version"}) {
		t.Errorf("the platform requirements hold the fields %v, want requirements, service and version", keys)
	}
	entries, _ := file["requirements"].([]any)
	if len(entries) == 0 {
		t.Fatalf("the platform requirements hold no requirement:\n%s", data)
	}
	for _, item := range entries {
		entry, _ := item.(map[string]any)
		kind, _ := entry["kind"].(string)
		allowed := append(slices.Clone(requirementFields), kindShapes[kind]...)
		for field, value := range entry {
			if !slices.Contains(allowed, field) {
				t.Errorf("the requirement %v holds the field %s, which a %s requirement does not have", entry["name"], field, kind)
			}
			for _, text := range texts(value) {
				if environmentValue.MatchString(text) || text == "orders-prod" {
					t.Errorf("the requirement %v holds the value %q in %s, which only an environment gives", entry["name"], text, field)
				}
			}
		}
	}
}

// texts gives the text of a value and of each item of a list value.
func texts(value any) []string {
	switch value := value.(type) {
	case string:
		return []string{value}
	case []any:
		var all []string
		for _, item := range value {
			all = append(all, texts(item)...)
		}
		return all
	}
	return nil
}

// reconcileRequirements runs Write the platform requirements in this process: it plans the project with the
// registry, through the port of the compiler, which wrap may change, and gives the platform requirements of the
// plan.
func reconcileRequirements(t *testing.T, dir, registry string, wrap func(compiler.RequirementsWriter) compiler.RequirementsWriter) ([]byte, error) {
	t.Helper()
	t.Chdir(dir)
	for _, entry := range requirementsEnv(registry) {
		name, value, _ := strings.Cut(entry, "=")
		t.Setenv(name, value)
	}
	input, err := buildReconcilerInput()
	if err != nil {
		t.Fatal(err)
	}
	if wrap != nil {
		input.WriteRequirements = wrap(input.WriteRequirements)
	}
	plan, err := compiler.Reconcile(input)
	if err != nil {
		return nil, err
	}
	for _, file := range plan.GeneratedFiles {
		if file.Path == compiler.RequirementsFile {
			return file.Bytes(), nil
		}
	}
	t.Fatalf("the plan holds no %s", compiler.RequirementsFile)
	return nil, nil
}

func mustReconcileRequirements(t *testing.T, dir, registry string, wrap func(compiler.RequirementsWriter) compiler.RequirementsWriter) delivery.Requirements {
	t.Helper()
	data, err := reconcileRequirements(t, dir, registry, wrap)
	if err != nil {
		t.Fatal(err)
	}
	requirements, err := delivery.LoadRequirements(data)
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	return requirements
}

func TestPlatformRequirementsDerivation(t *testing.T) {
	t.Run("ST-DLV-INV01-T The platform requirements state each need of the components, with its kind and shape, also when two needs have one kind", func(t *testing.T) {
		registry := requirementsRegistry(t)
		dir := requirementsProject(t)
		first := mustReconcileRequirements(t, dir, registry, nil)
		expectStated(t, first.Requirements, wantRequirements)

		setNeeds(t, registry, "rh-sso-auth", requirementsNeeds["rh-sso-auth"]+"  - {kind: secret, name: payments-api-key, keys: [token]}\n")
		second := mustReconcileRequirements(t, dir, registry, nil)
		apiKey := delivery.Requirement{Kind: delivery.Secret, Name: "payments-api-key", Keys: []string{"token"}, Components: []string{"rh-sso-auth"}}
		expectStated(t, second.Requirements, append(slices.Clone(wantRequirements), apiKey))

		setNeeds(t, registry, "rh-sso-auth", requirementsNeeds["rh-sso-auth"])
		setNeeds(t, registry, "health-check", strings.Replace(requirementsNeeds["health-check"], "  - {kind: configuration, name: LOG_LEVEL}\n", "", 1))
		third := mustReconcileRequirements(t, dir, registry, nil)
		if _, stated := byName(third.Requirements)["LOG_LEVEL"]; stated {
			t.Fatal("the platform requirements still state LOG_LEVEL after health-check stops stating it")
		}
		var others []delivery.Requirement
		for _, requirement := range wantRequirements {
			if requirement.Name != "LOG_LEVEL" {
				others = append(others, requirement)
			}
		}
		expectStated(t, third.Requirements, others)
		if len(third.Requirements) != len(others) {
			t.Fatalf("the platform requirements state %d needs after one was taken out, want %d", len(third.Requirements), len(others))
		}
	})

	t.Run("ST-DLV-INV02-T A need that holds an environment value is refused, and the requirements hold no such value", func(t *testing.T) {
		registry := requirementsRegistry(t)
		dir := requirementsProject(t)
		data, err := reconcileRequirements(t, dir, registry, nil)
		if err != nil {
			t.Fatal(err)
		}
		expectOnlyRequirementFields(t, data)
		for _, change := range []struct{ field, value string }{
			{"values", "{password: s3cr3t-password}"}, {"host", "db.prod.example.com"}, {"address", "10.0.0.5"}, {"namespace", "orders-prod"},
		} {
			changed := requirementsRegistry(t)
			setNeeds(t, changed, "postgres-adapter", `  - {kind: secret, name: "{service}-database", keys: [url, password], `+change.field+`: `+change.value+`}
  - {kind: backing store, name: "{service}-db", engine: postgres}
`)
			data, err := reconcileRequirements(t, dir, changed, nil)
			if err == nil {
				t.Errorf("a need with %s %s gave the requirements:\n%s", change.field, change.value, data)
				continue
			}
			for _, part := range []string{"postgres-adapter", "orders-database", "field " + change.field} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("the refusal of %s does not name %s: %v", change.field, part, err)
				}
			}
		}
	})

	t.Run("ST-DLV-DS01-T01 The same declaration gives the same requirements bytes, whatever order the components load in", func(t *testing.T) {
		registry := requirementsRegistry(t)
		dir := requirementsProject(t)
		// Each of the first two derivations runs in a new stego process.
		var runs [][]byte
		for range 2 {
			copied := copyFolder(t, dir)
			mustRunInProject(t, copied, requirementsEnv(registry), "apply")
			_, data := readRequirements(t, copied)
			runs = append(runs, data)
		}
		reversed, err := reconcileRequirements(t, dir, registry, func(write compiler.RequirementsWriter) compiler.RequirementsWriter {
			return func(service string, components []compiler.ComponentNeeds) ([]byte, error) {
				components = slices.Clone(components)
				slices.Reverse(components)
				return write(service, components)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, reversed)
		for i, run := range runs[1:] {
			if !bytes.Equal(runs[0], run) {
				t.Fatalf("derivation %d gave other bytes than the first:\n%s\n---\n%s", i+2, runs[0], run)
			}
		}
	})

	t.Run("ST-DLV-DS01-T02 The requirements come from the declaration and the stated needs alone", func(t *testing.T) {
		registry := requirementsRegistry(t)
		dir := requirementsProject(t)
		first, err := reconcileRequirements(t, dir, registry, nil)
		if err != nil {
			t.Fatal(err)
		}
		other := copyFolder(t, dir)
		t.Setenv("HOSTNAME", "another-host.example.com")
		t.Setenv("TZ", "Pacific/Kiritimati")
		t.Setenv("STEGO_UNRELATED", "db.prod.example.com")
		time.Sleep(1100 * time.Millisecond)
		second, err := reconcileRequirements(t, other, registry, nil)
		if err != nil {
			t.Fatal(err)
		}
		// A component that the registry holds and the declaration does not use states a need.
		setNeeds(t, registry, "cli-application", "  - {kind: secret, name: unused-secret, keys: [token]}\n")
		setNeeds(t, registry, "keycloak-provider", "  - {kind: egress, name: keycloak, port: 8443}\n")
		third, err := reconcileRequirements(t, dir, registry, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i, run := range [][]byte{second, third} {
			if !bytes.Equal(first, run) {
				t.Fatalf("derivation %d gave other bytes than the first:\n%s\n---\n%s", i+2, first, run)
			}
		}
		if bytes.Contains(third, []byte("keycloak")) {
			t.Fatalf("the requirements hold the need of a component that the declaration does not use:\n%s", third)
		}
		if !bytes.Contains(first, []byte("name: orders-database")) {
			t.Fatalf("the requirements do not fill the service name into the need names:\n%s", first)
		}
	})
}

// requirementsFillDeclaration is the declaration of ST-DLV-TD01 with the collection of the fill project of
// ST-EXT-TD01 and its fill qualified-fill bound, so a test can change a fill of the service.
func requirementsFillDeclaration(withKafka bool) string {
	head, _, _ := strings.Cut(requirementsDeclaration, "entities:\n")
	if !withKafka {
		head = strings.Replace(head, "mixins: [durable-events, kafka-events]", "mixins: [durable-events]", 1)
	}
	return head + `entities:
  - name: User
    resource_type: member
    permissions: {create: public, read: view, list: view, delete: edit}
    public_reasons: {create: a new member has no resource to decide on before it exists}
    fields: [{name: email, type: string}]
collections:
  org-users: {entity: User, operations: [create, read, delete]}
slots:
  - {slot: before_create, collection: org-users, gate: [qualified-fill]}
`
}

func TestPlatformRequirementsChangeOnlyThroughTheDeclaration(t *testing.T) {
	t.Run("ST-DLV-UC01-R05-T03 A need changes in the platform requirements when the declaration or the registry changes, and not when a fill changes", func(t *testing.T) {
		p := newFillProject(t)
		p.registry = requirementsRegistry(t)
		// Each step applies as an author does: the fill is tested and signed against the current generated code
		// before the declaration binds it again.
		apply := func(withKafka bool) {
			unbound, _, _ := strings.Cut(requirementsFillDeclaration(withKafka), "slots:\n")
			p.write(t, "service.yaml", unbound)
			p.must(t, "no-key", "apply")
			t.Log(p.must(t, "no-key", "test"))
			t.Log(p.must(t, "no-key", "fill", "status"))
			p.must(t, "listed-ssh", "fill", "sign", "qualified-fill")
			p.write(t, "service.yaml", requirementsFillDeclaration(withKafka))
			p.must(t, "no-key", "apply")
		}
		apply(true)
		first, firstData := readRequirements(t, p.dir)

		// The fill gets a new call that names the environment variable EXTRA_TOKEN and does not read the environment
		// (ST-DLV-Q06 (A)): a qualified fill may not import os.
		p.replace(t, "fills/qualified-fill/policy.go", "\treturn &slots.SlotResult{Ok: true}, nil", "\t_ = variableName(\"EXTRA_TOKEN\")\n\treturn &slots.SlotResult{Ok: true}, nil")
		p.write(t, "fills/qualified-fill/policy.go", string(p.read(t, "fills/qualified-fill/policy.go"))+"\n// variableName gives the name of an environment variable.\nfunc variableName(name string) string { return name }\n")
		apply(true)
		if _, data := readRequirements(t, p.dir); !bytes.Equal(firstData, data) {
			t.Fatalf("a change of a fill changed the platform requirements:\n%s\n---\n%s", firstData, data)
		}

		// The declaration takes out the Kafka producer.
		apply(false)
		third, _ := readRequirements(t, p.dir)
		expectChange(t, first.Requirements, third.Requirements, "order-events", nil)

		// The registry copy adds the key sslmode to the secret of postgres-adapter.
		setNeeds(t, p.registry, "postgres-adapter", strings.Replace(requirementsNeeds["postgres-adapter"], "keys: [url, password]", "keys: [url, password, sslmode]", 1))
		apply(false)
		fourth, _ := readRequirements(t, p.dir)
		secret := delivery.Requirement{Kind: delivery.Secret, Name: "orders-database", Keys: []string{"password", "sslmode", "url"}, Components: []string{"postgres-adapter"}}
		expectChange(t, third.Requirements, fourth.Requirements, "orders-database", &secret)
	})
}

// expectChange fails the test unless after differs from before only in the requirement named: gone when want is
// nil, else as want.
func expectChange(t *testing.T, before, after []delivery.Requirement, name string, want *delivery.Requirement) {
	t.Helper()
	old, changed := byName(before), byName(after)
	if want == nil {
		if _, stated := changed[name]; stated {
			t.Errorf("the platform requirements still state %s", name)
		}
	} else if got, stated := changed[name]; !stated || !requirementsEqual(got, *want) {
		t.Errorf("the platform requirements state %s as %+v, want %+v", name, got, *want)
	}
	delete(old, name)
	delete(changed, name)
	if len(old) != len(changed) {
		t.Errorf("the platform requirements hold %d other requirements, want %d", len(changed), len(old))
	}
	for other, requirement := range old {
		if got, stated := changed[other]; !stated || !requirementsEqual(got, requirement) {
			t.Errorf("the requirement %s changed from %+v to %+v", other, requirement, got)
		}
	}
}
