package compiler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/localtest"
	"github.com/jsell-rh/stego/internal/types"
)

// The proofs of the runtime data path through the stego binary: a service with data access on the runtime data path
// only (ST-SUP-RC01-OC41, ST-SUP-RC20-OC39), the examples on it, and a service generated from testdata/runtime_wiring/project/service.yaml whose
// migration job and binary run against a local PostgreSQL server and a local SpiceDB container.

// wiringFixture is the folder of the test service: its project files and the check that runs in its module.
const wiringFixture = "testdata/runtime_wiring"

// wiringModule is the module of the test service, which its fills import.
const wiringModule = "example.com/shop"

// wiringPinnedGo is the Go command that builds the stego binary and the generated service: STEGO_TEST_PINNED_GO, else
// bin/go of the SDK that runs the test.
func wiringPinnedGo() string {
	if name := os.Getenv("STEGO_TEST_PINNED_GO"); name != "" {
		return name
	}
	return filepath.Join(goruntime.GOROOT(), "bin", "go")
}

// wiringPath is the PATH entry of a go command and of the stego binary: the pinned Go first. The tests give it to
// localtest.GoEnv, which adds no workspace, no toolchain switch and no module proxy: the modules come from the module
// cache that localtest fills once (ST-STD-LOCAL-R03).
func wiringPath() string {
	return "PATH=" + filepath.Dir(wiringPinnedGo()) + string(os.PathListSeparator) + os.Getenv("PATH")
}

// stegoFixture is the stego binary of this commit, which the tests share.
var stegoFixture SharedFixture

// stegoBinary builds the stego binary of this commit once per test run and gives its path.
func stegoBinary(t *testing.T) string {
	t.Helper()
	return filepath.Join(stegoFixture.Dir(t, func(t *testing.T, dir string) {
		cmd := exec.Command(wiringPinnedGo(), "build", "-o", filepath.Join(dir, "stego"), "./cmd/stego")
		cmd.Dir = filepath.Join("..", "..")
		cmd.Env = localtest.GoEnv(wiringPath())
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building stego: %v\n%s", err, output)
		}
	}), "stego")
}

// postgresTLS is held by each test while its check runs, when the check turns TLS on in the PostgreSQL server of
// STEGO_TEST_POSTGRES_DSN with a certificate of its own and resets it at its end: the checks of TestRuntimeWiring,
// TestRuntimeGRPC, TestRelationshipOutbox and TestRevocationWiring. So only one of these checks runs at a time, and
// the builds before them run in parallel.
var postgresTLS sync.Mutex

// runTLSCheck runs the inner go test cmd of a check that turns TLS on in the PostgreSQL server, while it holds
// postgresTLS, and gives its output and its error.
func runTLSCheck(cmd *exec.Cmd) ([]byte, error) {
	postgresTLS.Lock()
	defer postgresTLS.Unlock()
	return cmd.CombinedOutput()
}

// buildBeside runs build in a goroutine and gives a function that waits for build and gives its error. The test waits
// for build when it ends too, also after a failure before the wait, so no build writes in a folder of the test after
// the test removed it. build must call no method of the test that stops it.
func buildBeside(t *testing.T, build func() error) func() error {
	t.Helper()
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		err = build()
	}()
	t.Cleanup(func() { <-done })
	return func() error {
		<-done
		return err
	}
}

// wiringProject is one project folder of the test service with a registry copy of its own.
type wiringProject struct {
	t      *testing.T
	dir    string
	binary string
}

// runtimeArchetype is the components list of the rest-crud archetype of the registry copy on the runtime data path:
// the shipped list with the tenancy component.
const runtimeArchetype = "components:\n  - rest-api\n  - postgres-adapter\n  - tenancy\n  - domain-probe\n  - command\n  - query\n  - outbox\n  - otel-tracing\n  - health-check\n"

// noQueryArchetype is runtimeArchetype without the query generator.
const noQueryArchetype = "components:\n  - rest-api\n  - postgres-adapter\n  - tenancy\n  - domain-probe\n  - command\n  - outbox\n  - otel-tracing\n  - health-check\n"

// storeArchetype is runtimeArchetype without the command and query generators: the component set of a service on the
// GORM store, which is refused now (ST-SUP-RC20-OC39).
const storeArchetype = "components:\n  - rest-api\n  - postgres-adapter\n  - tenancy\n  - domain-probe\n  - outbox\n  - otel-tracing\n  - health-check\n"

// newWiringProject copies the registry of this repository with the components of the archetype given, each alpha
// component of the test staged as ga, and the project files of the fixture into a new folder.
func newWiringProject(t *testing.T, binary, components string) *wiringProject {
	t.Helper()
	p := newRegistryProject(t, binary, components)
	if err := os.CopyFS(p.dir, os.DirFS(filepath.Join(wiringFixture, "project"))); err != nil {
		t.Fatal(err)
	}
	return p
}

// newRegistryProject copies the registry of this repository with the components of the archetype given, each alpha
// component of the test staged as ga, into a new folder, with a go.mod of the module wiringModule and no project
// files.
func newRegistryProject(t *testing.T, binary, components string) *wiringProject {
	t.Helper()
	p := &wiringProject{t: t, dir: t.TempDir(), binary: binary}
	if err := os.CopyFS(filepath.Join(p.dir, "registry"), os.DirFS(filepath.Join("..", "..", "registry"))); err != nil {
		t.Fatal(err)
	}
	p.archetype(components)
	sunset := regexp.MustCompile(`sunset: "?[0-9-]+"?\n`)
	for _, name := range []string{"tenancy", "command", "query", "domain-probe", "audit-log", "idempotency", "spicedb-authz"} {
		file := filepath.Join(p.dir, "registry", "components", name, "component.yaml")
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		p.write(filepath.Join("registry", "components", name, "component.yaml"),
			sunset.ReplaceAllString(strings.Replace(string(data), "stage: alpha\n", "stage: ga\n", 1), ""))
	}
	p.write("go.mod", "module "+wiringModule+"\n\ngo 1.26.11\n")
	return p
}

// archetype sets the components of the rest-crud archetype of the registry copy.
func (p *wiringProject) archetype(components string) {
	p.t.Helper()
	file := filepath.Join(p.dir, "registry", "archetypes", "rest-crud", "archetype.yaml")
	data, err := os.ReadFile(file)
	if err != nil {
		p.t.Fatal(err)
	}
	text := regexp.MustCompile(`(?s)components:\n(  - [a-z-]+\n)+`).ReplaceAllString(string(data), components)
	p.write(filepath.Join("registry", "archetypes", "rest-crud", "archetype.yaml"), text)
}

func (p *wiringProject) write(name, text string) {
	p.t.Helper()
	full := filepath.Join(p.dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

func (p *wiringProject) read(name string) string {
	p.t.Helper()
	data, err := os.ReadFile(filepath.Join(p.dir, name))
	if err != nil {
		p.t.Fatal(err)
	}
	return string(data)
}

// stego runs the stego binary in the project with its registry copy.
func (p *wiringProject) stego(args ...string) string {
	p.t.Helper()
	output, err := p.stegoRun(args...)
	if err != nil {
		p.t.Fatalf("stego %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

// stegoRun runs the stego binary in the project with its registry copy and gives its output and its error. Before a
// plan or an apply it qualifies the fills of the project (qualifyFills), as the service author does.
func (p *wiringProject) stegoRun(args ...string) (string, error) {
	p.t.Helper()
	if len(args) > 0 && (args[0] == "plan" || args[0] == "apply") {
		p.qualifyFills()
	}
	return p.stegoExec(nil, args...)
}

// stegoExec runs the stego binary in the project with its registry copy and the environment entries extra.
func (p *wiringProject) stegoExec(extra []string, args ...string) (string, error) {
	p.t.Helper()
	cmd := exec.Command(p.binary, args...)
	cmd.Dir = p.dir
	cmd.Env = append(localtest.GoEnv(wiringPath(), "STEGO_REGISTRY="+filepath.Join(p.dir, "registry"), "STEGO_MODULE="+wiringModule), extra...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// qualifyFills qualifies each fill of the project, as plan and apply require (ST-GEN-INV10): it gives a fill that
// holds Go code and no test file a test file that only builds it, lists a key in SIGNERS that the test makes for this
// project only, never a real signing key, runs stego test and signs each fill with that key. A fill that does not
// qualify stays as it is, so plan and apply give their own fault for it.
func (p *wiringProject) qualifyFills() {
	t := p.t
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(p.dir, "fills"))
	if err != nil {
		return
	}
	keyDir := filepath.Join(p.dir, "..", filepath.Base(p.dir)+"-signing")
	key := filepath.Join(keyDir, "test-key")
	if _, err := os.Stat(key); err != nil {
		if err := os.MkdirAll(keyDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "wiring-test", "-f", key).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v\n%s", err, output)
		}
		if err := os.WriteFile(filepath.Join(keyDir, "gitconfig"), []byte("[gpg]\n\tformat = ssh\n[user]\n\tsigningkey = "+key+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	public, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(public))
	line := "wiring-test@example.com " + fields[0] + " " + fields[1] + "\n"
	signers, _ := os.ReadFile(filepath.Join(p.dir, "SIGNERS"))
	if !strings.Contains(string(signers), line) {
		p.write("SIGNERS", string(signers)+line)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
		files, err := os.ReadDir(filepath.Join(p.dir, "fills", entry.Name()))
		if err != nil {
			continue
		}
		pkg, tested := "", false
		for _, file := range files {
			name := file.Name()
			if strings.HasSuffix(name, "_test.go") {
				tested = true
			} else if strings.HasSuffix(name, ".go") && pkg == "" {
				data, err := os.ReadFile(filepath.Join(p.dir, "fills", entry.Name(), name))
				if match := regexp.MustCompile(`(?m)^package (\w+)`).FindSubmatch(data); err == nil && match != nil {
					pkg = string(match[1])
				}
			}
		}
		if pkg != "" && !tested {
			p.write(filepath.Join("fills", entry.Name(), "qualify_test.go"), "package "+pkg+"\n\nimport \"testing\"\n\n// TestBuilds holds that the fill builds.\nfunc TestBuilds(t *testing.T) {}\n")
		}
	}
	signing := []string{"GIT_CONFIG_GLOBAL=" + filepath.Join(keyDir, "gitconfig"), "GIT_CONFIG_NOSYSTEM=1", "SSH_AUTH_SOCK=", "GOFLAGS=-modcacherw"}
	_, _ = p.stegoExec(signing, "test")
	for _, name := range names {
		_, _ = p.stegoExec(signing, "fill", "sign", name)
	}
}

// git runs git in the project.
func (p *wiringProject) git(args ...string) {
	p.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=wiring", "-c", "user.email=wiring@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = p.dir
	if output, err := cmd.CombinedOutput(); err != nil {
		p.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

// goCommand runs the pinned go command in the project.
func (p *wiringProject) goCommand(args ...string) {
	p.t.Helper()
	if err := p.goRun(args...); err != nil {
		p.t.Fatal(err)
	}
}

// goRun runs the pinned go command in the project and gives its error with its output.
func (p *wiringProject) goRun(args ...string) error {
	cmd := exec.Command(wiringPinnedGo(), args...)
	cmd.Dir = p.dir
	cmd.Env = localtest.GoEnv(wiringPath(), "GOFLAGS=-mod=mod")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	// A tidy takes only modules that a checked go.sum of the repository names (localtest.SumsChecked).
	if len(args) >= 2 && args[0] == "mod" && args[1] == "tidy" {
		return localtest.SumsChecked(p.dir)
	}
	return nil
}

// useLocalRuntime makes the module of the project take the runtime library from this repository's registry/runtime
// folder, as the work file of scripts/runtime-workspace.sh does for a local build.
func (p *wiringProject) useLocalRuntime() {
	p.t.Helper()
	if err := p.addLocalRuntime(); err != nil {
		p.t.Fatal(err)
	}
}

// addLocalRuntime is useLocalRuntime, and gives its error.
func (p *wiringProject) addLocalRuntime() error {
	runtimeDir, err := filepath.Abs(filepath.Join("..", "..", "registry", "runtime"))
	if err != nil {
		return err
	}
	replace := "replace github.com/jsell-rh/stego/registry/runtime => " + runtimeDir
	name := filepath.Join(p.dir, "go.mod")
	mod, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	if strings.Contains(string(mod), replace) {
		return nil
	}
	return os.WriteFile(name, []byte(string(mod)+"\n"+replace+"\n"), 0o644)
}

// release applies the project, takes the runtime library from this repository (useLocalRuntime), tidies the module
// and builds the service into binary, and gives the first error. It calls no method of the test that stops it, so it
// can run in a goroutine of its own (buildBeside). The fills of the project are qualified before (qualifyFills).
func (p *wiringProject) release(binary string) error {
	if output, err := p.stegoExec(nil, "apply"); err != nil {
		return fmt.Errorf("stego apply: %v\n%s", err, output)
	}
	if err := p.addLocalRuntime(); err != nil {
		return err
	}
	if err := p.goRun("mod", "tidy"); err != nil {
		return err
	}
	return p.goRun("build", "-o", binary, "./out")
}

// afterSlotBinding binds the fill order-created of testdata/runtime_wiring/after at orders.after_create.
const afterSlotBinding = "slots:\n  - collection: orders\n    slot: after_create\n    fan-out:\n      - order-created\n"

// applyWithAfterSlot applies the project with the after slot of orders bound to a qualified fill, as a service
// author does (ST-EXT-UC02): a first apply writes the slot contracts with no binding, stego test records the fill's
// evidence, a key that SIGNERS lists signs it, and a second apply binds and wires it.
func (p *wiringProject) applyWithAfterSlot() {
	t := p.t
	t.Helper()
	if err := os.CopyFS(filepath.Join(p.dir, "fills"), os.DirFS(filepath.Join(wiringFixture, "after", "fills"))); err != nil {
		t.Fatal(err)
	}
	p.stego("apply")
	p.useLocalRuntime()
	p.goCommand("mod", "tidy")
	keys := t.TempDir()
	key := filepath.Join(keys, "signer")
	if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "signer", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, output)
	}
	public, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(public))
	p.write("SIGNERS", "signer@example.com "+fields[0]+" "+fields[1]+"\n")
	home := filepath.Join(keys, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"gpg.format", "ssh"}, {"user.signingkey", key}} {
		cmd := exec.Command("git", "config", "--global", pair[0], pair[1])
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config: %v\n%s", err, output)
		}
	}
	signed := func(args ...string) string {
		cmd := exec.Command(p.binary, args...)
		cmd.Dir = p.dir
		cmd.Env = localtest.GoEnv(wiringPath(), "STEGO_REGISTRY="+filepath.Join(p.dir, "registry"), "STEGO_MODULE="+wiringModule, "GOFLAGS=-mod=mod -modcacherw",
			"HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
		output, _ := cmd.CombinedOutput()
		return string(output)
	}
	if output := signed("test"); !strings.Contains(output, "fill order-created: tests passed") {
		t.Fatalf("stego test did not record the evidence of order-created:\n%s", output)
	}
	if output := signed("fill", "sign", "order-created"); !strings.Contains(output, "signed_at") {
		t.Fatalf("stego fill sign order-created:\n%s", output)
	}
	declaration := p.read("service.yaml")
	p.write("service.yaml", strings.Replace(declaration, "commands:\n", afterSlotBinding+"commands:\n", 1))
	p.stego("apply")
}

// planned gives the paths that a plan output lists with the action given.
func planned(output, action string) map[string]bool {
	paths := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), action+":"); ok {
			paths[strings.TrimSpace(rest)] = true
		}
	}
	return paths
}

func TestRuntimeDataPath(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the stego binary")
	}
	t.Parallel()
	stego := stegoBinary(t)
	t.Run("ST-SUP-RC01-OC41-T05 A service with data access is generated on the runtime data path only, and is refused without the command and query components", func(t *testing.T) {
		t.Parallel()
		fresh := newWiringProject(t, stego, runtimeArchetype)
		generated := planned(fresh.stego("plan"), "generate")
		for _, want := range []string{RuntimeWiringNamespace + "/runtime.go", "internal/storage/tables.go", "commands/handlers/handlers.go", "queries/serve/serve.go"} {
			if !generated[want] {
				t.Errorf("the plan of the service does not generate %s", want)
			}
		}
		for _, refused := range []string{"internal/storage/store.go", "internal/storage/models.go"} {
			if generated[refused] {
				t.Errorf("the plan of the service generates the store file %s", refused)
			}
		}
		fresh.stego("apply")
		record := fresh.read(".stego/state.yaml")
		if !strings.Contains(record, "format_version: 6") || strings.Contains(record, "data_path") {
			t.Errorf("the record is not of format version 6 without a data path:\n%s", record)
		}

		// The same declaration without the command or the query generator: no component serves its data, so the
		// components that served it on the GORM store refuse the run, and the fault names ST-SUP-RC20-OC39.
		for _, components := range []string{storeArchetype, noQueryArchetype} {
			lacking := newWiringProject(t, stego, components)
			if components == storeArchetype {
				// The declaration of a release on the GORM store: its collections and entities, with no command, no
				// query and no setting of the command component.
				full := lacking.read("service.yaml")
				store := full[:strings.Index(full, "commands:")]
				store = strings.Replace(store, "  command:\n    proto_files:\n      - {path: proto/shop.proto, import_path: shop/v1/shop.proto}\n", "", 1)
				lacking.write("service.yaml", store)
				if err := os.RemoveAll(filepath.Join(lacking.dir, "fills")); err != nil {
					t.Fatal(err)
				}
			}
			output, err := lacking.stegoRun("plan")
			if err == nil || !strings.Contains(output, "ST-SUP-RC20-OC39") || !strings.Contains(output, "command") || !strings.Contains(output, "query component") {
				t.Errorf("the plan of a service with data access and without the command and query generators gave %v, want a refusal that names ST-SUP-RC20-OC39:\n%s", err, output)
			}
			if _, err := os.Stat(filepath.Join(lacking.dir, "out")); !os.IsNotExist(err) {
				t.Errorf("the refused plan wrote the output folder: %v", err)
			}
		}
	})

	t.Run("ST-SUP-RC11-OC31-T01 A service on the runtime data path gets no jwt-auth middleware and its verifier takes the declared settings", func(t *testing.T) {
		t.Parallel()
		runtime := newWiringProject(t, stego, runtimeArchetype)
		runtime.stego("apply")
		if _, err := os.Stat(filepath.Join(runtime.dir, "out", "internal", "auth")); !os.IsNotExist(err) {
			t.Errorf("the service on the runtime data path has the jwt-auth package out/internal/auth: %v", err)
		}
		main := runtime.read("out/main.go")
		if strings.Contains(main, "/out/internal/auth\"") || strings.Contains(main, "NewAuthMiddleware") {
			t.Error("the main.go of the service on the runtime data path wires the jwt-auth middleware")
		}
		if !strings.Contains(main, "service.Middleware(") {
			t.Error("the main.go of the service on the runtime data path does not wrap the routes in the runtime verifier")
		}
		wiring := runtime.read("out/" + RuntimeWiringNamespace + "/runtime.go")
		for _, want := range []string{`declaredIssuer    = "https://issuer.wiring.test/realms/r1"`, `declaredAudience  = "wiring-shop"`} {
			if !strings.Contains(wiring, want) {
				t.Errorf("the runtime verifier does not take the declared setting %s", want)
			}
		}
	})

	t.Run("ST-SUP-RC20-OC54-T04 The members token and pin of verify-order, which the fixture proto marks debug_redact, are secrets in the generated service", func(t *testing.T) {
		t.Parallel()
		shop := newWiringProject(t, stego, runtimeArchetype)
		shop.stego("apply")
		var document struct {
			Paths map[string]map[string]struct {
				RequestBody struct {
					Content map[string]struct {
						Schema struct {
							Properties map[string]map[string]any `json:"properties"`
						} `json:"schema"`
					} `json:"content"`
				} `json:"requestBody"`
			} `json:"paths"`
		}
		if err := json.Unmarshal([]byte(shop.read("out/internal/api/openapi.json")), &document); err != nil {
			t.Fatal(err)
		}
		body := document.Paths["/api/shop/v1/orders/{orderId}/verify"]["post"].RequestBody.Content["application/json"].Schema.Properties
		for name, secret := range map[string]bool{"token": true, "pin": true, "orderId": false} {
			member := body[name]
			if secret && (member == nil || member["x-sensitive"] != true || member["writeOnly"] != true || member["format"] != "password") {
				t.Errorf("the request member %s of verify-order is %v, want x-sensitive, writeOnly and the format password", name, member)
			}
			if !secret && member != nil && member["x-sensitive"] != nil {
				t.Errorf("the plain request member %s of verify-order is %v", name, member)
			}
		}
		// The service reads the key ring, which keys the fingerprint of a verify-order body (ST-SUP-RP14-LC12).
		if !strings.Contains(shop.read("out/"+RuntimeWiringNamespace+"/runtime.go"), "service.ReadKeyRing()") {
			t.Error("the runtime wiring of the service does not read the key ring")
		}
	})

	t.Run("ST-SUP-RC01-OC41-T06 An example on the runtime data path keeps its generated output", func(t *testing.T) {
		t.Parallel()
		for _, example := range []string{"user-management", "user-management-rhsso"} {
			p := &wiringProject{t: t, dir: t.TempDir()}
			if err := os.CopyFS(p.dir, os.DirFS(filepath.Join("..", "..", "examples", example))); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(stego, "plan")
			cmd.Dir = p.dir
			registryDir, err := filepath.Abs(filepath.Join("..", "..", "registry"))
			if err != nil {
				t.Fatal(err)
			}
			cmd.Env = localtest.GoEnv(wiringPath(), "STEGO_REGISTRY="+registryDir)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("stego plan of %s: %v\n%s", example, err, output)
			}
			if !strings.Contains(string(output), "Summary: 0 to generate, 0 to update, 0 to delete") {
				t.Errorf("the plan of the example %s changes its generated output:\n%s", example, output)
			}
			if record := p.read(".stego/state.yaml"); !strings.Contains(record, "format_version: 6") || strings.Contains(record, "data_path") {
				t.Errorf("the record of the example %s is not of format version 6 without a data path", example)
			}
			if _, err := os.Stat(filepath.Join(p.dir, "out", "internal", "storage", "store.go")); err == nil {
				t.Errorf("the example %s holds the GORM store", example)
			}
			// An apply writes the record again, with the compiler of this commit: it states the runtime data path still.
			apply := exec.Command(stego, "apply")
			apply.Dir, apply.Env = p.dir, cmd.Env
			if output, err := apply.CombinedOutput(); err != nil {
				t.Fatalf("stego apply of %s: %v\n%s", example, err, output)
			}
			if record := p.read(".stego/state.yaml"); strings.Contains(record, "data_path") || !strings.Contains(record, RuntimeWiringNamespace) {
				t.Errorf("the record that apply writes for the example %s states a data path or lacks the runtime wiring", example)
			}
		}
	})
}

// wiringSpiceDBImage is the pinned SpiceDB image of the local stand-in (ST-STD-LOCAL-R04), as the runtime's tests pin
// it.
const wiringSpiceDBImage = "authzed/spicedb:v1.57.0@sha256:a170d372c9f6b921bb6849064df6ca3566dd5b174882c1a2c4300c2ae1d60efd"

// startWiringSpiceDB starts a local SpiceDB container without privilege, with an in-memory datastore and limits, and
// gives its endpoint and preshared key. It removes the container when the test ends.
func startWiringSpiceDB(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker is not on the PATH: this test needs a local SpiceDB container")
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	name, key := "stego-wiring-spicedb-"+hex.EncodeToString(suffix), "wiring-"+hex.EncodeToString(suffix)
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name, "--memory", "256m", "--cpus", "1",
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL", "--stop-timeout", "1",
		"-p", "127.0.0.1::50051", wiringSpiceDBImage, "serve", "--grpc-preshared-key", key, "--datastore-engine", "memory")
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run SpiceDB: %v\n%s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		out, err := exec.Command("docker", "port", name, "50051/tcp").Output()
		if err == nil && len(out) > 0 {
			return strings.TrimSpace(strings.Split(string(out), "\n")[0]), key
		}
	}
	t.Fatal("the SpiceDB container publishes no port")
	return "", ""
}

// wiringCheckIDs are the checks of the generated service that TestRuntimeWiring runs, each with the ID of the test
// record it proves.
var wiringCheckIDs = []string{
	"ST-SUP-RC01-OC21-T03 The generated migration job runs as the owner role and the service as the runtime role",
	"ST-SUP-RC01-OC41-T04 The runtime pool is opened through tenant.Open with the adapter's verified TLS configuration",
	"ST-SUP-RC01-OC41-T07 The generated migration job runs again on its own database and refuses a database with tables it did not make",
	"ST-SUP-RC29-OC23-T02 The generated migration job writes the schema to SpiceDB before the service starts",
	"ST-SUP-RC27-OC11-T02 The provisioning routes pass the tenant check on an empty service",
	"ST-SUP-RC27-OC09-T01 A caller without the provision permission cannot provision a tenant",
	"ST-SUP-RC27-OC09-T03 A grant on the service-level resource of one tenant provisions and retires no tenant",
	"ST-SUP-RC27-OC10-T01 A provision and a retire each leave one audit row in their transaction",
	"ST-SUP-RC35-OC02-T02 A generated service runs one runner for each declared worker in the worker mode",
	"ST-SUP-RC35-OC06-T04 A consuming worker that writes commits as the worker role, which may write nothing it did not declare",
	"ST-SUP-RC27-OC12-T05 A retired tenant gets no worker run from the next tick, and its rows stay",
	"ST-SUP-RC27-OC11-T01 A request for an unknown or retired tenant is refused before any handler",
	"ST-SUP-RC20-OC37-T07 A collection create and read reach the generated handlers through the generated main.go",
	"ST-SUP-RC32-OC02-T03 A declared command reaches its fill through the generated main.go",
	"ST-SUP-RC33-OC02-T04 A declared query reaches its fill through the generated main.go",
	"ST-SUP-RP14-LC04-T02 The generated service runs its workers through the service runtime",
	"ST-SUP-RP14-LC04-T03 The generated worker delivers each audit entry and anchor to the default table",
	"ST-SUP-RC18-OC31-T02 The generated worker sends to Kafka with the settings of the process, and does not start without them or with no topic",
	"ST-SUP-RC20-OC40-T02 A change event of a generated write arrives once with its key and headers, waits out a broker outage in the outbox, and keeps the order of its entity",
	"ST-SUP-RC20-OC40-T03 A replay of a write with its idempotency key stages no second event",
	"ST-SUP-RC20-OC40-T04 A write that rolls back after it staged its event leaves no outbox message and no record",
	"ST-SUP-RC32-OC14-T02 The write of a command sends the change event of its row to Kafka",
	"ST-SUP-RC35-OC14-T02 The writes of a worker send the change events of their rows to Kafka",
	"ST-SUP-RC35-OC13-T01 Provisioning a tenant writes one tuple for each worker in that tenant only",
	"ST-SUP-RC27-OC13-T01 Provision gives each first administrator the admin relation in the new tenant only, which the relay writes to SpiceDB",
	"ST-SUP-RC27-OC18-T01 Provision relates the tenant resource to the service-level resource, so the first administrators read the rows of the tenant",
	"ST-SUP-RC35-OC13-T06 The reconcile at start gives a tenant of an earlier release each worker's tuple and deletes the tuple of a removed worker",
	"ST-SUP-RC20-OC48-T01 A secret field is written by a request, never answered, and kept by a write that leaves it out",
	"ST-SUP-RC32-OC20-T01 A command fill reads a readable secret and checks a compare-only one",
	"ST-SUP-RP10-LC44-T03 A sealed value copied to another order does not open in the generated service",
	"ST-SUP-RP10-LC46-T02 After a key rotation the generated controller moves each sealed value to the new key",
	"ST-SUP-RP03-LC16-T03 The generated service writes the lines of a failed charge, the re-seal job and a refused step of the migration job as JSON lines with stego attributes",
}

// secretStoredCheck is the check of the generated service that proves that a secret field is never stored in clear
// (ST-DECL-UC05-R06, decision ST-DECL-Q44).
const secretStoredCheck = "ST-DECL-UC05-R06-T02 A secret field is never stored"

// The checks of the generated service that prove the UC requirements of the tenant provisioning (ST-DECL-UC10),
// which TestRuntimeWiring runs after those of wiringCheckIDs.
const (
	provisionFirstCheck = "ST-DECL-UC10-R04-T02 An operator provisions the first tenant of an empty service"
	provisionDecided    = "ST-DECL-UC10-R01-T01 A caller without the provision permission cannot provision a tenant"
	provisionAudited    = "ST-DECL-UC10-R02-T01 A provision and a retire each leave one audit row in their transaction"
	provisionGated      = "ST-DECL-UC10-R04-T01 A request for an unknown or retired tenant is refused before any handler"
	provisionWorkers    = "ST-DECL-UC10-R03-T01 A provisioned tenant gives each worker its tuple"
)

// TestRuntimeWiring generates the test service with stego apply, builds it with the pinned Go, and runs the check of
// testdata/runtime_wiring/check in its module once: the check runs the migration job and the service binary against
// the local PostgreSQL server of localtest (STEGO_TEST_POSTGRES_DSN, else a container that the test binary starts), a local SpiceDB container and the local Kafka broker of
// kafkatest, and sends collection calls, one command and one query over HTTP. A missing server fails the test: a
// missing result is not a pass.
func TestRuntimeWiring(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a generated service")
	}
	t.Parallel()
	postgres := "STEGO_TEST_POSTGRES_DSN=" + localtest.PostgresDSN(t)
	stego := stegoBinary(t)
	// The next release adds the entity Tag with no schema step, and declares its three modes out of order, which the
	// check of ST-GEN-INV26 starts it with. Its apply and build run beside the apply and build of the first release:
	// no step needs its binary before the check.
	next := newWiringProject(t, stego, runtimeArchetype)
	declaration := next.read("service.yaml")
	next.write("service.yaml", strings.Replace(declaration, "entities:\n", "modes: [controller, api, worker]\nentities:\n  - name: Tag\n    fields:\n      - {name: label, type: string}\n", 1))
	next.qualifyFills()
	nextBinary := filepath.Join(next.dir, "bin", "service")
	nextBuilt := buildBeside(t, func() error { return next.release(nextBinary) })
	p := newWiringProject(t, stego, runtimeArchetype)
	p.applyWithAfterSlot()
	// The check is the files of testdata/runtime_wiring/check: the wiring checks and the checks of the secret fields.
	for _, name := range []string{"wiring_test.go", "secret_test.go"} {
		check, err := os.ReadFile(filepath.Join(wiringFixture, "check", name))
		if err != nil {
			t.Fatal(err)
		}
		p.write("out/wiringcheck/"+name, string(check))
	}
	p.useLocalRuntime()
	p.goCommand("mod", "tidy")
	// The build of the service runs beside its vet: both only read the tidied module.
	binary := filepath.Join(p.dir, "bin", "service")
	built := buildBeside(t, func() error { return p.goRun("build", "-o", binary, "./out") })
	p.goCommand("vet", "./out/...")
	if err := built(); err != nil {
		t.Fatal(err)
	}
	t.Run("ST-SUP-RC18-OC31-T03 The generated service links no test support of the runtime library", func(t *testing.T) {
		cmd := exec.Command(wiringPinnedGo(), "list", "-deps", "./out")
		cmd.Dir = p.dir
		cmd.Env = localtest.GoEnv(wiringPath(), "GOFLAGS=-mod=mod")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go list -deps ./out: %v\n%s", err, output)
		}
		deps := strings.Fields(string(output))
		if !slices.Contains(deps, "github.com/jsell-rh/stego/registry/runtime/outbox/kafkaoutbox") {
			t.Fatalf("the generated service does not link the Kafka destination, so the check proves nothing:\n%s", output)
		}
		for _, dep := range deps {
			if strings.HasSuffix(dep, "/kafkatest") || strings.HasSuffix(dep, "/pgtest") || strings.HasSuffix(dep, "/authntest") {
				t.Errorf("the generated service links the test support %s", dep)
			}
		}
	})
	t.Run("ST-SUP-RC32-OC21-T01 The key ring of the secret fields reaches the api and worker pods only", func(t *testing.T) {
		requirements := p.read("deploy/platform-requirements.yaml")
		if !strings.Contains(requirements, "wiring-shop-secret-key-ring") || !strings.Contains(requirements, "STEGO_SECRET_KEY_RING_FILE") {
			t.Fatalf("the platform requirements do not list the key ring and its variable:\n%s", requirements)
		}
		for _, file := range []string{"deploy/base/deployment.yaml", "deploy/base/worker-deployment.yaml"} {
			if !strings.Contains(p.read(file), "STEGO_SECRET_KEY_RING_FILE") {
				t.Errorf("%s does not wire STEGO_SECRET_KEY_RING_FILE", file)
			}
		}
		for _, file := range []string{"deploy/components/secret-files/mount-files.yaml", "deploy/components/secret-files/mount-worker-files.yaml"} {
			if !strings.Contains(p.read(file), "secretName: wiring-shop-secret-key-ring") {
				t.Errorf("%s does not mount the key ring", file)
			}
		}
		if job := p.read("deploy/base/migration-job.yaml"); strings.Contains(job, "SECRET_KEY_RING") || strings.Contains(job, "secret-key-ring") {
			t.Error("the migration Job reads the key ring")
		}
	})
	// A service on the runtime data path needs all three modes: declared modes without worker and controller are
	// refused by validate, naming the features that need them (ST-DECL-INV89).
	subset := newWiringProject(t, stego, runtimeArchetype)
	subset.write("service.yaml", strings.Replace(subset.read("service.yaml"), "entities:\n", "modes: [api]\nentities:\n", 1))
	t.Run("ST-DECL-INV89-T Declared modes that leave out a mode the declared features need are refused", func(t *testing.T) {
		output, err := subset.stegoRun("validate")
		if err == nil || !strings.Contains(output, "[mode-needed] modes") || !strings.Contains(output, "do not hold worker,") ||
			!strings.Contains(output, "do not hold controller,") || !strings.Contains(output, "outbox relay") {
			t.Fatalf("stego validate of the modes [api] gave %v, want the faults of worker and controller:\n%s", err, output)
		}
	})
	// The next release is built beside the steps above; the check needs its binary.
	if err := nextBuilt(); err != nil {
		t.Fatalf("the next release: %v", err)
	}
	endpoint, key := startWiringSpiceDB(t)
	cmd := exec.Command(wiringPinnedGo(), "test", "-count=1", "-timeout=480s", "-v", "-run", "^TestWiring$", "./out/wiringcheck")
	cmd.Dir = p.dir
	cmd.Env = localtest.GoEnv(wiringPath(), postgres, "STEGO_TEST_SPICEDB="+endpoint, "STEGO_TEST_SPICEDB_KEY="+key, "STEGO_WIRING_BINARY="+binary,
		"STEGO_WIRING_NEXT_BINARY="+nextBinary)
	output, runErr := runTLSCheck(cmd)
	passed := func(t *testing.T, id string) {
		t.Helper()
		name := strings.ReplaceAll(id, " ", "_")
		if !strings.Contains(string(output), "--- PASS: TestWiring/"+name+" ") {
			t.Fatalf("the check %s of the generated service did not run and pass (go test: %v):\n%s", strings.SplitN(id, " ", 2)[0], runErr, output)
		}
	}
	for _, id := range wiringCheckIDs {
		t.Run(id, func(t *testing.T) { passed(t, id) })
	}
	t.Run(provisionFirstCheck, func(t *testing.T) { passed(t, provisionFirstCheck) })
	t.Run(provisionDecided, func(t *testing.T) { passed(t, provisionDecided) })
	t.Run(provisionAudited, func(t *testing.T) { passed(t, provisionAudited) })
	t.Run(provisionGated, func(t *testing.T) { passed(t, provisionGated) })
	t.Run(provisionWorkers, func(t *testing.T) { passed(t, provisionWorkers) })
	t.Run(modesCheck, func(t *testing.T) { passed(t, modesCheck) })
	t.Run(secretStoredCheck, func(t *testing.T) { passed(t, secretStoredCheck) })
}

// modesCheck is the check of the generated service that starts the binary with no mode option, with --mode api and
// with --mode api --mode worker, and the refused starts of the modes (ST-GEN-INV26).
const modesCheck = "ST-GEN-INV26-T The assembler wires each declared mode as a named set in one binary that a repeatable mode flag selects"

func TestKeyRingWiring(t *testing.T) {
	t.Run("ST-SUP-RP14-LC12-T02 A service with a pii field reads the key ring, and the controller seals each open seal under its lease", func(t *testing.T) {
		service := func(sensitivity string) *types.ServiceDeclaration {
			return &types.ServiceDeclaration{Entities: []types.Entity{{Name: "Member", Fields: []types.Field{{Name: "email", Type: types.FieldTypeString, Sensitivity: sensitivity}}}},
				SchemaChanges: []types.SchemaChange{{ID: "member-email-seal", Kind: "seal", Entity: "Member", Field: "email", From: "pii"}, {ID: "member-code-seal", Kind: "seal", Entity: "Member", Field: "code"}},
				SchemaHistory: []types.SchemaStep{{Change: "member-email-seal", Phase: "expand"}, {Change: "member-code-seal", Phase: "expand"}, {Change: "member-code-seal", Phase: "contract"}}}
		}
		for _, c := range []struct {
			sensitivity string
			ring        bool
		}{{"pii", true}, {"secret", true}, {"", false}, {"public", false}} {
			ring, seals := sealing(service(c.sensitivity))
			if ring != c.ring || !slices.Equal(seals, []string{"member-email-seal"}) {
				t.Errorf("a field of the sensitivity %q gives the ring %t and the open seals %v; want %t and member-email-seal alone", c.sensitivity, ring, seals, c.ring)
			}
		}
		withRing, err := renderRuntimeWiring(runtimeWiringData{Ring: true, Seals: []string{"member-email-seal"}})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`"github.com/jsell-rh/stego/registry/runtime/sealed"`, "service.ReadKeyRing()", "settings.Ring = ring", "KeyRing: true",
			"Seal: svcstorage.SealColumns", `SealLeases: []string{"member-email-seal"}`} {
			if !strings.Contains(string(withRing), want) {
				t.Errorf("the wiring of a service with a pii field and an open seal does not hold %s", want)
			}
		}
		plain, err := renderRuntimeWiring(runtimeWiringData{})
		if err != nil {
			t.Fatal(err)
		}
		for _, gone := range []string{"registry/runtime/sealed", "ReadKeyRing", "KeyRing:", "SealColumns", "SealLeases"} {
			if strings.Contains(string(plain), gone) {
				t.Errorf("the wiring of a service with no secret and no pii field holds %s", gone)
			}
		}
	})
}

func TestCommandSecretRing(t *testing.T) {
	t.Run("ST-SUP-RC32-OC21-T03 A service whose command message has a member marked debug_redact reads the key ring", func(t *testing.T) {
		proto := func(option string) gen.Context {
			return gen.Context{ComponentConfig: map[string]any{"proto_files": []any{map[string]any{"path": "proto/keys.proto", "import_path": "keys/v1/keys.proto"}}},
				ModuleName: "example.com/keys", OutDirName: "out", OutputNamespace: "commands",
				Inputs: map[string][]byte{"proto/keys.proto": []byte("syntax = \"proto3\";\npackage keys.v1;\nmessage Key { string value = 1" + option + "; }\n" +
					"message IssueKeyRequest { string label = 1; }\nmessage IssueKeyResponse { map<string, Key> keys = 1; }\n")}}
		}
		for _, c := range []struct {
			option string
			ring   bool
		}{{" [debug_redact = true]", true}, {" [debug_redact = false]", false}, {"", false}} {
			ring, err := commandSecrets(proto(c.option))
			if err != nil || ring != c.ring {
				t.Errorf("a member of a map value of a response with the option %q gives the ring %t (%v), want %t", c.option, ring, err, c.ring)
			}
		}
		if ring, err := commandSecrets(gen.Context{}); err != nil || ring {
			t.Errorf("a service with no command component gives the ring %t (%v), want false", ring, err)
		}
	})
}

func TestUnmarkedSecretMembers(t *testing.T) {
	t.Run("ST-SUP-RC32-OC28-T01 Validate warns of a command request member with the name of a secret field of a written entity and no debug_redact", func(t *testing.T) {
		ctx := gen.Context{ComponentConfig: map[string]any{"proto_files": []any{map[string]any{"path": "proto/pay.proto", "import_path": "pay/v1/pay.proto"}}},
			ModuleName: "example.com/pay", OutDirName: "out", OutputNamespace: "commands",
			Inputs: map[string][]byte{"proto/pay.proto": []byte("syntax = \"proto3\";\npackage pay.v1;\n" +
				"message VerifyPayoutRequest { string payout_account_id = 1; string payout_pin = 2; string note = 3; string iban = 4; }\nmessage VerifyPayoutResponse { bool verified = 1; }\n" +
				"message ResetPinRequest { string payout_pin = 1 [debug_redact = true]; }\nmessage ResetPinResponse {}\n")}}
		service := &types.ServiceDeclaration{
			Entities: []types.Entity{{Name: "PayoutAccount", Fields: []types.Field{{Name: "payout_pin", Type: types.FieldTypeString, Sensitivity: "secret", CompareOnly: true},
				{Name: "iban", Type: types.FieldTypeString, Sensitivity: "pii"}, {Name: "note", Type: types.FieldTypeString}}}},
			Commands: []types.Command{{Name: "verify-payout", Writes: []string{"PayoutAccount"}}, {Name: "reset-pin", Writes: []string{"PayoutAccount"}}},
		}
		warnings := unmarkedSecretMembers(service, ctx)
		if len(warnings) != 1 || warnings[0].Rule != "unmarked-secret-member" || warnings[0].Path != "commands[0]" || warnings[0].Found != "payout_pin" ||
			!strings.Contains(warnings[0].Message, "[debug_redact = true]") {
			t.Fatalf("the warnings are %+v, want one unmarked-secret-member of payout_pin at commands[0]", warnings)
		}
		// A command that does not write the entity gets no warning.
		service.Commands[0].Writes = []string{"Other"}
		if warnings := unmarkedSecretMembers(service, ctx); len(warnings) != 0 {
			t.Errorf("a command that does not write the entity gets %+v", warnings)
		}
	})
}

func TestRelateWiring(t *testing.T) {
	t.Run("ST-SUP-RC01-OC52-T02 The wiring of a relate step gives the relate job its changes and the migration job its lease", func(t *testing.T) {
		svc := &types.ServiceDeclaration{
			SchemaChanges: []types.SchemaChange{{ID: "payout-tenant-relate", Kind: "relate", Entity: "Payout", Relation: "tenant"},
				{ID: "payout-note-add", Kind: "add", Entity: "Payout", Field: "note"}},
			SchemaHistory: []types.SchemaStep{{Change: "payout-note-add", Phase: "expand"}, {Change: "payout-tenant-relate", Phase: "expand"}}}
		if got := relating(svc); !slices.Equal(got, []string{"payout-tenant-relate"}) {
			t.Fatalf("the relate steps of the declaration are %v, want payout-tenant-relate alone", got)
		}
		wiring, err := renderRuntimeWiring(runtimeWiringData{Seals: []string{"member-email-seal"}, Relates: relating(svc)})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Relate: svcstorage.RelateChanges", `SealLeases: []string{"member-email-seal", "payout-tenant-relate"}`} {
			if !strings.Contains(string(wiring), want) {
				t.Errorf("the wiring of a service with a relate step does not hold %s", want)
			}
		}
		none, err := renderRuntimeWiring(runtimeWiringData{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(none), "RelateChanges") || strings.Contains(string(none), "SealLeases") {
			t.Error("the wiring of a service with no relate step and no seal names the relate job or a backfill lease")
		}
	})
}
