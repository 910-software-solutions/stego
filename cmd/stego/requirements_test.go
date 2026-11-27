package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

	"github.com/910-software-solutions/stego/internal/compiler"
	"github.com/910-software-solutions/stego/internal/delivery"
	"github.com/910-software-solutions/stego/internal/gen"
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
	"health-check": `  - {kind: ingress, name: http, port: 8080}
  - {kind: cluster permission, name: read-config-maps, resource: configmaps, verbs: [get]}
  - {kind: pod security, name: pod-security, level: restricted}
`,
}

// requirementsVariable is the required environment variable LOG_LEVEL of ST-DLV-TD01, which health-check declares:
// the platform requirements derive the configuration need LOG_LEVEL from it (ST-DLV-INV11).
const requirementsVariable = `  - {name: LOG_LEVEL, type: string, required: true, secret: false, description: "The level of the log of the service.", allowed: {values: [debug, info, warn, error]}}
`

// requirementsRegistry gives a copy of the shipped registry whose components state the needs and the variable of
// ST-DLV-TD01, and no other need: the other shipped components state no need in the copy.
func requirementsRegistry(t *testing.T) string {
	t.Helper()
	registry := shippedRegistryCopy(t)
	entries, err := os.ReadDir(filepath.Join(registry, "components"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			setNeeds(t, registry, entry.Name(), requirementsNeeds[entry.Name()])
		}
	}
	addVariables(t, registry, "health-check", requirementsVariable)
	return registry
}

// removeVariables takes the entries out of the environment list of the component of the registry copy.
func removeVariables(t *testing.T, registry, component, entries string) {
	t.Helper()
	path := filepath.Join(registry, "components", component, "component.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), entries) {
		t.Fatalf("the component %s does not declare %s", component, entries)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), entries, "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// addVariables adds the entries to the environment list of the component of the registry copy.
func addVariables(t *testing.T, registry, component, entries string) {
	t.Helper()
	path := filepath.Join(registry, "components", component, "component.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	switch {
	case strings.Contains(text, "\nenvironment: []\n"):
		text = strings.Replace(text, "\nenvironment: []\n", "\nenvironment:\n"+entries, 1)
	case strings.Contains(text, "\nenvironment:\n"):
		text = strings.Replace(text, "\nenvironment:\n", "\nenvironment:\n"+entries, 1)
	default:
		t.Fatalf("the component %s states no environment list", component)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
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
	command.Env = stegoEnv(env...)
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
	t.Parallel()
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

	t.Run("ST-DLV-UC01-R01-T02 The shipped components state their real platform needs, and an environment file with no database does not meet them", func(t *testing.T) {
		registry := shippedRegistryCopy(t)
		dir := requirementsProject(t)
		env := requirementsEnv(registry)
		mustRunInProject(t, dir, env, "apply")
		requirements, _ := readRequirements(t, dir)
		expectStated(t, requirements.Requirements, []delivery.Requirement{
			{Kind: delivery.Secret, Name: "orders-database", Keys: []string{"url", "worker-url"}, Components: []string{"postgres-adapter"}},
			{Kind: delivery.Secret, Name: "orders-database-owner", Keys: []string{"url"}, Components: []string{"postgres-adapter"}},
			{Kind: delivery.Secret, Name: "orders-database-relay", Keys: []string{"url"}, Components: []string{"spicedb-authz"}},
			{Kind: delivery.Secret, Name: "orders-query-cursor", Keys: []string{"key"}, Components: []string{"query"}},
			{Kind: delivery.Secret, Name: "orders-spicedb", Keys: []string{"preshared-key"}, Components: []string{"spicedb-authz"}},
			{Kind: delivery.BackingStore, Name: "orders-db", Engine: "postgres", Components: []string{"postgres-adapter"}},
			{Kind: delivery.Topic, Name: "orders-events", Components: []string{"kafka-producer"}},
			{Kind: delivery.IdentityIssuer, Name: "orders-issuer", Components: []string{"rh-sso-auth"}},
			{Kind: delivery.Ingress, Name: "http", Port: 8080, Components: []string{"health-check"}},
			{Kind: delivery.Egress, Name: "orders-db-network", Port: 5432, Components: []string{"postgres-adapter"}},
			{Kind: delivery.Egress, Name: "orders-issuer-keys", Port: 443, Components: []string{"rh-sso-auth"}},
			{Kind: delivery.Egress, Name: "orders-kafka", Port: 9093, Components: []string{"kafka-producer"}},
			{Kind: delivery.Egress, Name: "spicedb", Port: 50051, Components: []string{"spicedb-authz"}},
			{Kind: delivery.PodSecurity, Name: "pod-security", Level: "restricted", Components: []string{"health-check"}},
		})
		// No need name holds a value of a setting: the issuer setting of rh-sso-auth is a URL with a host name.
		for _, requirement := range requirements.Requirements {
			if strings.Contains(requirement.Name, "example") {
				t.Errorf("the need %s holds a value of a setting", requirement.Name)
			}
		}
		// The walkthrough file of G6: only the three configuration values, and no database, no secret and no issuer.
		environmentFile := filepath.Join(dir, "staging-nodb.yaml")
		if err := os.WriteFile(environmentFile, []byte("version: 1\nservice: orders\nenvironment: staging\n"+
			"image: quay.io/example/orders@sha256:"+strings.Repeat("ab", 32)+"\n"+
			"destination: {cluster: https://api.example.com:6443, namespace: orders, overlay_path: overlays/staging}\n"+
			"configuration:\n  STEGO_QUERY_CURSOR_KEY_FILE: /etc/orders/cursor/key\n  STEGO_SPICEDB_ENDPOINT: spicedb.example.com:50051\n"+
			"  STEGO_SPICEDB_KEY_FILE: /etc/orders/spicedb/key\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err := runInProject(t, dir, env, "env", "check", "--requirements", compiler.RequirementsFile, environmentFile)
		if err == nil {
			t.Fatalf("the check passes an environment file with no database:\n%s\n%s", stdout, stderr)
		}
		for _, part := range []string{"missing: backing store orders-db", "missing: secret orders-database", "missing: identity issuer orders-issuer", "missing: ingress http", "missing: egress spicedb"} {
			if !strings.Contains(stdout, part) {
				t.Errorf("the check does not name %s:\n%s\n%s", part, stdout, stderr)
			}
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
		conflict(t, `  - {kind: topic, name: "{service}-database"}
`, "different kinds", "topic", "secret")
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
		// Beside the nine needs, each requirement is a configuration need of a required variable of the list that is
		// not secret (ST-DLV-INV11).
		wanted := byName(wantRequirements)
		listed := map[string]delivery.AcceptedVariable{}
		for _, variable := range requirements.Environment {
			listed[variable.Name] = variable
		}
		for _, requirement := range requirements.Requirements {
			if _, ok := wanted[requirement.Name]; ok {
				continue
			}
			if variable, ok := listed[requirement.Name]; !ok || requirement.Kind != delivery.Configuration || !variable.Required || variable.Secret {
				t.Errorf("the platform requirements state %s %s, which is no need of the components and no required variable", requirement.Kind, requirement.Name)
			}
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
	if keys := slices.Sorted(maps.Keys(file)); !slices.Equal(keys, []string{"environment", "requirements", "service", "version"}) {
		t.Errorf("the platform requirements hold the fields %v, want environment, requirements, service and version", keys)
	}
	// An entry of the environment list holds its facts only, never an example; a secret one holds no default and no
	// allowed values (ST-DLV-INV02, ST-DLV-INV12).
	loaded, err := delivery.LoadRequirements(data)
	if err != nil {
		t.Fatal(err)
	}
	stated := loaded.Requirements
	variables, _ := file["environment"].([]any)
	for _, item := range variables {
		entry, _ := item.(map[string]any)
		for field := range entry {
			if !slices.Contains([]string{"name", "read_by", "type", "required", "default", "description", "secret", "allowed", "secret_source", "refused_at_start", "read_by_mode", "one_of"}, field) {
				t.Errorf("the environment variable %v holds the field %s", entry["name"], field)
			}
		}
		// A secret source names a secret requirement of the file and one of its keys, and nothing else (ST-DLV-Q19).
		if source, given := entry["secret_source"].(map[string]any); given {
			name, _ := source["name"].(string)
			if len(source) != 2 || !slices.ContainsFunc(stated, func(r delivery.Requirement) bool {
				return r.Kind == delivery.Secret && r.Name == name && slices.Contains(r.Keys, fmt.Sprint(source["key"]))
			}) {
				t.Errorf("the secret source of %v is not one key of a secret requirement of the file: %v", entry["name"], source)
			}
		}
		if entry["secret"] == true && (entry["default"] != nil || entry["allowed"] != nil) {
			t.Errorf("the secret environment variable %v holds a default or allowed values", entry["name"])
		}
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
		removeVariables(t, registry, "health-check", requirementsVariable)
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
		if len(third.Requirements) != len(first.Requirements)-1 {
			t.Fatalf("the platform requirements state %d needs after one was taken out, want %d", len(third.Requirements), len(first.Requirements)-1)
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
			return func(service string, components []compiler.ComponentNeeds) (compiler.RequirementsFiles, error) {
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
	t.Parallel()
	t.Run("ST-DLV-UC01-R05-T03 A need changes in the platform requirements when the declaration or the registry changes, and not when a fill changes", func(t *testing.T) {
		p := newFillProject(t)
		p.registry = requirementsRegistry(t)
		// Each step applies as an author does: the fill is tested and signed against the current generated code
		// before the declaration binds it again.
		apply := func(withKafka bool) {
			unbound, _, _ := strings.Cut(requirementsFillDeclaration(withKafka), "slots:\n")
			p.write(t, "service.yaml", unbound)
			p.must(t, "no-key", "apply")
			t.Log(p.unproved(t, "no-key", "no-tests-fill"))
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

// TestEnvironmentVariablesApply proves that apply writes the list of every environment variable that the service
// accepts in the platform requirements and the reference rendered from it (ST-DLV-UC01-R03, R05 to R09).
func TestEnvironmentVariablesApply(t *testing.T) {
	t.Parallel()
	t.Run("ST-DLV-UC01-R06-T01 Applying the plan writes every environment variable of the active set in the platform requirements, in name order, with its facts", func(t *testing.T) {
		registry := requirementsRegistry(t)
		dir := requirementsProject(t)
		mustRunInProject(t, dir, requirementsEnv(registry), "apply")
		requirements, _ := readRequirements(t, dir)
		var names []string
		byVariable := map[string]delivery.AcceptedVariable{}
		for _, variable := range requirements.Environment {
			names = append(names, variable.Name)
			byVariable[variable.Name] = variable
		}
		if !slices.IsSorted(names) || len(names) < 40 {
			t.Fatalf("the list is not in name order or misses variables: %v", names)
		}
		level, ok := byVariable["LOG_LEVEL"]
		if !ok || !level.Required || level.Type != "string" || level.Allowed != "debug, info, warn, error" || !slices.Equal(level.ReadBy, []string{"health-check"}) {
			t.Fatalf("LOG_LEVEL is listed as %+v", level)
		}
		timeout, ok := byVariable["STEGO_DATABASE_CONNECTION_MAX_LIFETIME"]
		if !ok || timeout.Default == nil || *timeout.Default != "30m" || timeout.Allowed != "1s to 24h" || timeout.Description == "" {
			t.Fatalf("STEGO_DATABASE_CONNECTION_MAX_LIFETIME is listed as %+v", timeout)
		}
		if issuer := byVariable["STEGO_AUTH_ISSUER"]; issuer.Default == nil || *issuer.Default != "https://sso.example.com/auth/realms/example" {
			t.Fatalf("the default of STEGO_AUTH_ISSUER is not the declared issuer: %+v", issuer)
		}
		if url := byVariable["DATABASE_URL"]; !url.Secret || url.Default != nil {
			t.Fatalf("DATABASE_URL is not listed as secret: %+v", url)
		}
		for _, name := range []string{"STEGO_GRPC_ADDR", "STEGO_BROWSER_ORIGIN"} {
			if _, listed := byVariable[name]; listed {
				t.Errorf("the list holds %s of a component that the service does not use", name)
			}
		}
	})

	t.Run("ST-DLV-UC01-R07-T03 Applying the plan writes deploy/environment-variables.md from the list, with one section for each component", func(t *testing.T) {
		registry := requirementsRegistry(t)
		dir := requirementsProject(t)
		stdout := mustRunInProject(t, dir, requirementsEnv(registry), "apply")
		if !strings.Contains(stdout, "generate: "+compiler.ReferenceFile) {
			t.Fatalf("the plan does not list %s:\n%s", compiler.ReferenceFile, stdout)
		}
		data, err := os.ReadFile(filepath.Join(dir, compiler.ReferenceFile))
		if err != nil {
			t.Fatal(err)
		}
		requirements, _ := readRequirements(t, dir)
		reference := string(data)
		for _, variable := range requirements.Environment {
			if !strings.Contains(reference, "| `"+variable.Name+"` |") {
				t.Errorf("the reference has no row of %s", variable.Name)
			}
		}
		for _, section := range []string{"## health-check", "## postgres-adapter", "## rh-sso-auth"} {
			if !strings.Contains(reference, "\n"+section+"\n") {
				t.Errorf("the reference has no section %s", section)
			}
		}
		if compiler.ReferenceFile != delivery.ReferenceFile {
			t.Fatalf("the compiler writes the reference at %s, and Delivering names %s", compiler.ReferenceFile, delivery.ReferenceFile)
		}
	})

	t.Run("ST-DLV-UC01-R08-T01 No secret value, secret default or secret setting reaches a file that apply writes, or a refusal", func(t *testing.T) {
		const canary = "CANARY-SECRET-VALUE"
		registry := requirementsRegistry(t)
		dir := filepath.Join(t.TempDir(), "orders")
		// The secret setting connection_string of postgres-adapter holds the canary text.
		declaration := strings.Replace(requirementsDeclaration, "postgres-adapter: {schema_generation: tables-v1}",
			"postgres-adapter: {schema_generation: tables-v1, connection_string: \"postgres://orders:"+canary+"@db/orders\"}", 1)
		writeRequirementsDeclaration(t, dir, declaration)
		git(t, dir, "init", "-q")
		git(t, dir, "add", "-A")
		git(t, dir, "commit", "-q", "-m", "the service orders")
		before := folderDigests(t, dir)
		// The process environment of apply holds the canary too, in a secret variable and a library variable.
		processEnv := append(requirementsEnv(registry), "DATABASE_URL=postgres://orders:"+canary+"@db/orders", "PGPASSWORD="+canary, "STEGO_AUDIT_SINK_TOKEN_FILE=/"+canary)
		stdout, stderr, err := runInProject(t, dir, processEnv, "apply")
		if err != nil {
			t.Fatalf("apply: %v\n%s\n%s", err, stdout, stderr)
		}
		for path, digest := range folderDigests(t, dir) {
			if before[path] == digest {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, path))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte(canary)) {
				t.Errorf("the written file %s holds the value of the secret setting", path)
			}
		}
		if strings.Contains(stdout+stderr, canary) {
			t.Errorf("apply shows the value of the secret setting:\n%s\n%s", stdout, stderr)
		}
		requirements, _ := readRequirements(t, dir)
		for _, variable := range requirements.Environment {
			if variable.Secret && (variable.Default != nil || variable.Allowed != "") {
				t.Errorf("the secret variable %s is listed with a default or allowed values", variable.Name)
			}
		}
		// A secret variable with a default, and a need name of a secret setting, are refused, and the refusal
		// does not show the canary.
		for component, change := range map[string]func(){
			"health-check": func() {
				addVariables(t, registry, "health-check", "  - {name: HEALTH_TOKEN, type: string, required: false, secret: true, default: "+canary+", description: \"The token.\"}\n")
			},
			"postgres-adapter": func() {
				setNeeds(t, registry, "postgres-adapter", requirementsNeeds["postgres-adapter"]+"  - {kind: topic, name: \"{settings.connection_string}\"}\n")
			},
			"health-check ": func() {
				addVariables(t, registry, "health-check", "  - {name: HEALTH_TOKEN, type: string, required: false, secret: true, example: "+canary+", description: \"The token.\"}\n")
			},
			"postgres-adapter ": func() {
				addVariables(t, registry, "postgres-adapter", "  - {name: STORE_URL, type: string, required: false, secret: false, default: \"{settings.connection_string}\", description: \"The store.\"}\n")
			},
		} {
			fresh := requirementsRegistry(t)
			registry = fresh
			change()
			project := requirementsProject(t)
			before := folderDigests(t, project)
			stdout, stderr, err := runInProject(t, project, requirementsEnv(registry), "apply")
			if err == nil {
				t.Errorf("%s: apply accepted a secret in a default or a name", component)
			}
			if strings.Contains(stdout+stderr, canary) || !strings.Contains(stderr, strings.TrimSpace(component)) {
				t.Errorf("%s: the refusal shows the canary or does not name the component:\n%s", component, stderr)
			}
			if after := folderDigests(t, project); !maps.Equal(before, after) {
				t.Errorf("%s: the refused apply changed the project folder", component)
			}
		}
		// A secret field of runtime-configuration with a default of the canary is refused, with no canary shown.
		fresh := requirementsRegistry(t)
		archetype := filepath.Join(fresh, "archetypes", "rest-crud", "archetype.yaml")
		raw, err := os.ReadFile(archetype)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(archetype, []byte(strings.Replace(string(raw), "components:\n", "components:\n  - runtime-configuration\n", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		withField := filepath.Join(t.TempDir(), "orders")
		writeRequirementsDeclaration(t, withField, strings.Replace(requirementsDeclaration, "overrides:\n", `overrides:
  runtime-configuration:
    groups:
      - name: Worker
        fields:
          - {name: Token, env: WIDGET_TOKEN, type: string, min_length: 1, max_length: 64, required: false, secret: true, default: `+canary+`, description: "The token."}
`, 1))
		git(t, withField, "init", "-q")
		git(t, withField, "add", "-A")
		git(t, withField, "commit", "-q", "-m", "the service orders")
		stdout, stderr, err = runInProject(t, withField, requirementsEnv(fresh), "apply")
		if err == nil || strings.Contains(stdout+stderr, canary) {
			t.Errorf("a secret field with a default is accepted, or the refusal shows it: %v\n%s", err, stderr)
		}
		// stego env check on the written requirements and an environment file whose secrets hold the canary shows
		// no canary, whether it passes or fails.
		envDir := t.TempDir()
		written, err := os.ReadFile(filepath.Join(dir, compiler.RequirementsFile))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(envDir, "platform-requirements.yaml"), written, 0o644); err != nil {
			t.Fatal(err)
		}
		environment, err := os.ReadFile(filepath.Join(environmentFixtureDir, "dev.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range []string{string(environment), strings.Replace(string(environment), "secrets:\n", "secrets:\n  DATABASE_URL:\n    value: "+canary+"\n  PGPASSWORD:\n    value: "+canary+"\n", 1) + "  extra: " + canary + "\n"} {
			if err := os.WriteFile(filepath.Join(envDir, "dev.yaml"), []byte(file), 0o644); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, _ := runInProject(t, envDir, nil, "env", "check", "--requirements", "platform-requirements.yaml", "dev.yaml")
			if strings.Contains(stdout+stderr, canary) {
				t.Errorf("stego env check shows the canary:\n%s\n%s", stdout, stderr)
			}
		}
	})

	t.Run("ST-DLV-UC01-R09-T02 Two components that state one variable with different facts stop the apply, which names both and the variable and writes nothing", func(t *testing.T) {
		registry := requirementsRegistry(t)
		addVariables(t, registry, "postgres-adapter", "  - {name: LOG_LEVEL, type: string, required: false, default: info, secret: false, description: \"The level of the log.\"}\n")
		dir := requirementsProject(t)
		before := folderDigests(t, dir)
		_, stderr, err := runInProject(t, dir, requirementsEnv(registry), "apply")
		if err == nil {
			t.Fatal("apply accepted one variable that two components state differently")
		}
		for _, part := range []string{"health-check", "postgres-adapter", "LOG_LEVEL", "required flag"} {
			if !strings.Contains(stderr, part) {
				t.Errorf("the refusal does not name %s:\n%s", part, stderr)
			}
		}
		if after := folderDigests(t, dir); !maps.Equal(before, after) {
			t.Fatalf("the refused apply changed the project folder")
		}
	})

	t.Run("ST-DLV-UC01-R03-T03 Two applies of the same declaration and registry write the same reference bytes", func(t *testing.T) {
		registry := requirementsRegistry(t)
		first := requirementsProject(t)
		second := copyFolder(t, first)
		mustRunInProject(t, first, requirementsEnv(registry), "apply")
		mustRunInProject(t, second, append(requirementsEnv(registry), "TZ=Pacific/Kiritimati", "LANG=de_DE.UTF-8", "STEGO_UNRELATED=1"), "apply")
		a, err := os.ReadFile(filepath.Join(first, compiler.ReferenceFile))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(second, compiler.ReferenceFile))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("the two applies wrote other references:\n%s\n---\n%s", a, b)
		}
	})

	t.Run("ST-DLV-UC01-R05-T04 Applying the plan refuses a hand edit of the reference and names the file", func(t *testing.T) {
		registry := requirementsRegistry(t)
		dir := requirementsProject(t)
		env := requirementsEnv(registry)
		mustRunInProject(t, dir, env, "apply")
		path := filepath.Join(dir, compiler.ReferenceFile)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, []byte("| `EXTRA` | string | no | none | no | - | - | Added by hand. |\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		_, stderr, err := runInProject(t, dir, env, "apply")
		if err == nil || !strings.Contains(stderr, "hand-edited") || !strings.Contains(stderr, compiler.ReferenceFile) {
			t.Fatalf("apply accepted a hand edit of the reference, or the refusal does not name it: %v\n%s", err, stderr)
		}
	})
}

func TestEnvironmentSettingDefaults(t *testing.T) {
	t.Run("ST-DLV-UC01-R06-T02 A default from a setting that breaks the variable's type or allowed values stops the apply", func(t *testing.T) {
		registry := requirementsRegistry(t)
		// The declaration gives schema_generation the text tables-v1, which is no integer.
		addVariables(t, registry, "postgres-adapter", "  - {name: STORE_GENERATION, type: integer, required: false, secret: false, default: \"{settings.schema_generation}\", description: \"The generation.\"}\n")
		dir := requirementsProject(t)
		before := folderDigests(t, dir)
		_, stderr, err := runInProject(t, dir, requirementsEnv(registry), "apply")
		if err == nil {
			t.Fatal("apply accepted a default that is not of the variable's type")
		}
		for _, part := range []string{"postgres-adapter", "STORE_GENERATION", "schema_generation", "not an integer"} {
			if !strings.Contains(stderr, part) {
				t.Errorf("the refusal does not name %s:\n%s", part, stderr)
			}
		}
		if after := folderDigests(t, dir); !maps.Equal(before, after) {
			t.Fatal("the refused apply changed the project folder")
		}
	})
}

func TestEnvironmentGuardAndSecretSources(t *testing.T) {
	t.Run("ST-DLV-UC01-R06-T03 Apply marks each variable that the start guard refuses, and validate and apply warn on a secret source that they do not keep", func(t *testing.T) {
		registry := requirementsRegistry(t)
		dir := requirementsProject(t)
		env := requirementsEnv(registry)
		// The registry copy states the secret orders-database with the keys url and password only, so the sources
		// of the migration URL (in orders-database-owner) and the worker URL name needs and keys that the service does not state.
		for _, command := range []string{"validate", "apply"} {
			_, stderr := mustRunInProjectStderr(t, dir, env, command)
			for _, want := range []string{"WARNING: the environment variable STEGO_MIGRATION_DATABASE_URL", "secret source orders-database-owner key url",
				"WARNING: the environment variable STEGO_WORKER_DATABASE_URL", "secret source orders-database key worker-url"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stego %s does not warn %q:\n%s", command, want, stderr)
				}
			}
			if strings.Contains(stderr, "WARNING: the environment variable DATABASE_URL ") {
				t.Errorf("stego %s warns on the source of DATABASE_URL, which the service states:\n%s", command, stderr)
			}
		}
		requirements, _ := readRequirements(t, dir)
		refused := 0
		for _, variable := range requirements.Environment {
			if variable.RefusedAtStart != gen.RefusedVariable(variable.Name) {
				t.Errorf("the variable %s has refused_at_start %v; the start guard refuses it: %v", variable.Name, variable.RefusedAtStart, gen.RefusedVariable(variable.Name))
			}
			if variable.RefusedAtStart {
				refused++
			}
		}
		if refused < 20 {
			t.Errorf("the platform requirements mark %d variables as refused at start, fewer than the PG names that pgx reads", refused)
		}
		deployment, err := os.ReadFile(filepath.Join(dir, "deploy", "base", "deployment.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, variable := range requirements.Environment {
			if variable.RefusedAtStart && strings.Contains(string(deployment), "- name: "+variable.Name+"\n") {
				t.Errorf("the base wires %s, which the start guard refuses", variable.Name)
			}
		}
	})
}

func TestStrictSecretSources(t *testing.T) {
	t.Run("ST-DLV-UC01-R06-T04 With --strict, validate, plan and apply refuse a secret source that they do not keep, and write nothing", func(t *testing.T) {
		registry := requirementsRegistry(t)
		dir := requirementsProject(t)
		env := requirementsEnv(registry)
		// The registry copy states the secret orders-database with the keys url and password only (as in
		// ST-DLV-UC01-R06-T03), and it leaves out some components of the needs, so several secret sources are not kept.
		for _, args := range [][]string{{"validate", "--strict"}, {"validate", "--json", "--strict"}, {"plan", "--strict"}, {"apply", "--strict"}} {
			stdout, stderr, err := runInProject(t, dir, env, args...)
			text := stdout + stderr
			if err == nil {
				t.Errorf("stego %s passes with secret sources that it does not keep:\n%s", strings.Join(args, " "), text)
			}
			for _, want := range []string{"--strict refuses ", "secret source(s) that the run does not keep", "STEGO_MIGRATION_DATABASE_URL", "secret source orders-database-owner key url",
				"STEGO_WORKER_DATABASE_URL", "secret source orders-database key worker-url", "run without --strict"} {
				if !strings.Contains(text, want) {
					t.Errorf("stego %s does not name %q:\n%s", strings.Join(args, " "), want, text)
				}
			}
			if strings.Contains(stderr, "WARNING: the environment variable") {
				t.Errorf("stego %s warns and refuses the same source:\n%s", strings.Join(args, " "), stderr)
			}
			// A refused run writes no file: no deploy/, no saved plan or state in .stego/ and no out/ (#375 security
			// review, test gaps).
			for _, folder := range []string{"deploy", ".stego", "out"} {
				if _, err := os.Stat(filepath.Join(dir, folder)); !os.IsNotExist(err) {
					t.Fatalf("stego %s writes %s/ although it refuses: %v", strings.Join(args, " "), folder, err)
				}
			}
		}
		// With add steps pending, plan --strict and apply --strict run the same check, refuse, and change no file;
		// before, plan ran only the declaration checks there (#375 security review L1).
		steps := requirementsProject(t)
		mustRunInProjectStderr(t, steps, env, "apply")
		git(t, steps, "add", "-A")
		git(t, steps, "commit", "-q", "-m", "the applied service orders")
		declaration := strings.Replace(requirementsDeclaration, "fields: [{name: item, type: string}]",
			"fields: [{name: item, type: string}, {name: note, type: string, optional: true}]", 1)
		if err := os.WriteFile(filepath.Join(steps, "service.yaml"), []byte(declaration), 0o644); err != nil {
			t.Fatal(err)
		}
		if stdout, stderr := mustRunInProjectStderr(t, steps, env, "plan", "--base", "HEAD"); !strings.Contains(stdout, "Apply writes the") ||
			!strings.Contains(stderr, "WARNING: the environment variable STEGO_WORKER_DATABASE_URL") {
			t.Fatalf("stego plan without --strict does not list the add steps and warn of the secret source:\n%s\n%s", stdout, stderr)
		}
		before := tree(t, steps)
		for _, args := range [][]string{{"plan", "--strict", "--base", "HEAD"}, {"apply", "--strict", "--base", "HEAD"}} {
			stdout, stderr, err := runInProject(t, steps, env, args...)
			text := stdout + stderr
			if err == nil || !strings.Contains(text, "--strict refuses ") || !strings.Contains(text, "secret source orders-database key worker-url") {
				t.Errorf("stego %s with add steps pending: %v, want the refusal of --strict that names the secret source:\n%s", strings.Join(args, " "), err, text)
			}
			if strings.Contains(stdout, "Apply writes the") {
				t.Errorf("stego %s lists the add steps although --strict refuses:\n%s", strings.Join(args, " "), stdout)
			}
			if args[0] == "apply" && !strings.Contains(stderr, "apply wrote the old service.yaml back, without the add steps") {
				t.Errorf("stego %s does not say that it wrote the old service.yaml back:\n%s", strings.Join(args, " "), text)
			}
			if after := tree(t, steps); !maps.Equal(before, after) {
				t.Errorf("stego %s changed a file of the service folder although --strict refuses", strings.Join(args, " "))
			}
		}
		// Without --strict, the same run warns and goes on (ST-DLV-UC01-R06-T03).
		_, stderr := mustRunInProjectStderr(t, dir, env, "validate")
		if !strings.Contains(stderr, "WARNING: the environment variable STEGO_WORKER_DATABASE_URL") {
			t.Errorf("stego validate without --strict does not warn:\n%s", stderr)
		}
		// With the shipped registry, every source is kept, so --strict passes.
		shipped := shippedRegistryCopy(t)
		other := requirementsProject(t)
		_, stderr = mustRunInProjectStderr(t, other, requirementsEnv(shipped), "apply", "--strict")
		if strings.Contains(stderr, "WARNING: the environment variable") {
			t.Errorf("stego apply --strict with the shipped registry warns:\n%s", stderr)
		}
	})
}

// mustRunInProjectStderr runs the stego binary in the project folder and fails the test when it exits non-zero; it
// gives standard output and standard error.
func mustRunInProjectStderr(t *testing.T, dir string, env []string, args ...string) (string, string) {
	t.Helper()
	stdout, stderr, err := runInProject(t, dir, env, args...)
	if err != nil {
		t.Fatalf("stego %s: %v\n%s\n%s", strings.Join(args, " "), err, stdout, stderr)
	}
	return stdout, stderr
}

func TestMigrationCredentialStaysOutOfTheBase(t *testing.T) {
	t.Run("ST-DLV-UC07-R03-T10 No serving file of the base or of the Component names the credential of the migration job, which has a Secret of its own", func(t *testing.T) {
		registry := shippedRegistryCopy(t)
		dir := requirementsProject(t)
		mustRunInProject(t, dir, requirementsEnv(registry), "apply")
		requirements, _ := readRequirements(t, dir)
		service := requirements.Service
		migration := 0
		for _, variable := range requirements.Environment {
			if variable.ReadByMode == "migrate" {
				migration++
			}
			if variable.Name == "STEGO_MIGRATION_DATABASE_URL" &&
				(variable.SecretSource == nil || variable.SecretSource.Name != service+"-database-owner" || variable.SecretSource.Key != "url") {
				t.Errorf("STEGO_MIGRATION_DATABASE_URL takes its value from %+v, want the key url of %s-database-owner", variable.SecretSource, service)
			}
		}
		if migration != 2 {
			t.Errorf("the platform requirements mark %d variables as read by the migration job, want STEGO_MIGRATION_DATABASE_URL and its _FILE form", migration)
		}
		// The owner credential is in a secret need of its own, and the need that the Deployment reads does not hold it
		// (ST-DLV-Q20, ST-DLV-INV13).
		keys := map[string][]string{}
		for _, requirement := range requirements.Requirements {
			if requirement.Kind == delivery.Secret {
				keys[requirement.Name] = requirement.Keys
			}
		}
		if got := keys[service+"-database"]; !slices.Equal(got, []string{"url", "worker-url"}) {
			t.Errorf("the secret need %s-database states the keys %v, want url and worker-url only", service, got)
		}
		if got := keys[service+"-database-owner"]; !slices.Equal(got, []string{"url"}) {
			t.Errorf("the secret need %s-database-owner states the keys %v, want url", service, got)
		}
		job := map[string]bool{"migration-job.yaml": true, "remove-job-env.yaml": true, "mount-job-files.yaml": true}
		written, jobFiles := 0, 0
		for _, folder := range []string{filepath.Join(dir, "deploy", "base"), filepath.Join(dir, "deploy", "components")} {
			err := filepath.WalkDir(folder, func(file string, entry os.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				data, err := os.ReadFile(file)
				if err != nil {
					return err
				}
				written++
				if job[filepath.Base(file)] {
					jobFiles++
					if filepath.Base(file) != "remove-job-env.yaml" && !strings.Contains(string(data), service+"-database-owner") {
						t.Errorf("%s does not name the Secret %s-database-owner of the migration job:\n%s", file, service, data)
					}
					return nil
				}
				for _, banned := range []string{"migration-url", "STEGO_MIGRATION_DATABASE_URL", "database-owner"} {
					if strings.Contains(string(data), banned) {
						t.Errorf("%s names %s, the credential of the migration job:\n%s", file, banned, data)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		// The Component exists, so the check reads its patches too.
		if _, err := os.Stat(filepath.Join(dir, "deploy", "components", "secret-files", "mount-files.yaml")); err != nil || written < 8 || jobFiles != 3 {
			t.Fatalf("apply wrote %d files of the base and the Component, %d of the Job: %v", written, jobFiles, err)
		}
	})
}
