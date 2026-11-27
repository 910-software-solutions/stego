package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/mod/modfile"

	"github.com/910-software-solutions/stego/internal/appbuild"
	"github.com/910-software-solutions/stego/internal/compiler"
	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/localtest"
	"github.com/910-software-solutions/stego/internal/registry"
)

// allocationTask is the file that the component hello emits: a background task that splits 10.00 USD in three
// with the money package of the runtime library, prints the parts, and waits for the end of the service.
const allocationTask = `package hello

import (
	"context"
	"fmt"

	"github.com/910-software-solutions/stego/registry/runtime/money"
)

// Task prints one allocation of the runtime library.
type Task struct{}

// NewTask makes the task.
func NewTask() *Task { return &Task{} }

// Run prints the allocation and waits until the service stops.
func (*Task) Run(ctx context.Context) error {
	total, err := money.New(1000, "USD")
	if err != nil {
		return err
	}
	parts, err := total.Allocate(1, 1, 1)
	if err != nil {
		return err
	}
	fmt.Println("allocated", parts[0], parts[1], parts[2])
	<-ctx.Done()
	return nil
}
`

// runtimeService writes a common registry that holds a copy of registry/runtime and the archetype svc-arch with
// the runtime pin of the shipped archetypes, and a project of the service svc on it, whose one component hello
// emits allocationTask. It gives the reconciler input and the pin.
func runtimeService(t *testing.T) (compiler.ReconcilerInput, registry.RuntimePin) {
	t.Helper()
	_, source, _, _ := runtime.Caller(0)
	repository := filepath.Join(filepath.Dir(source), "..", "..")
	shipped, err := registry.Load(filepath.Join(repository, "registry"))
	if err != nil {
		t.Fatal(err)
	}
	pin := *shipped.Archetype("rest-crud").Runtime
	parent := t.TempDir()
	common, project := filepath.Join(parent, "registry"), filepath.Join(parent, "svc")
	if err := os.MkdirAll(common, 0o755); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("cp", "-a", filepath.Join(repository, "registry", "runtime"), filepath.Join(common, "runtime")).CombinedOutput(); err != nil {
		t.Fatalf("copy registry/runtime: %v\n%s", err, output)
	}
	files := map[string]string{
		filepath.Join(common, "archetypes/svc-arch/archetype.yaml"): "kind: archetype\nname: svc-arch\nlanguage: go\nversion: 1.0.0\nruntime:\n  module: " + pin.Module + "\n  version: " + pin.Version + "\n  sum: " + pin.Sum + "\ncomponents: [hello]\nconventions:\n  layout: flat\n",
		filepath.Join(common, "components/hello/component.yaml"):    "kind: component\nenvironment: []\narchitecture:\n  layer: handler\n  flow: Serves the test.\n  ports: {}\nname: hello\nversion: 1.0.0\nstage: ga\noutput_namespace: internal/hello\nrequires: []\nprovides: []\nslots: []\n",
		filepath.Join(project, "service.yaml"):                      "kind: service\nname: svc\narchetype: svc-arch\nlanguage: go\n",
	}
	for name, data := range files {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return compiler.ReconcilerInput{
		ProjectDir: project, RegistryDir: common, GoVersion: "1.26.11", ModuleName: "example.com/svc",
		Generators: map[string]gen.Generator{"hello": staticGenerator{
			files:  []gen.File{{Path: "internal/hello/hello.go", Content: []byte(allocationTask)}},
			wiring: &gen.Wiring{Imports: []string{"internal/hello"}, Constructors: []string{"hello.NewTask()"}, BackgroundTasks: []int{0}},
		}},
	}, pin
}

// staticGenerator emits the same files and wiring for every declaration.
type staticGenerator struct {
	files  []gen.File
	wiring *gen.Wiring
}

func (g staticGenerator) Generate(gen.Context) ([]gen.File, *gen.Wiring, error) {
	return g.files, g.wiring, nil
}

func goOutput(t *testing.T, command *exec.Cmd) string {
	t.Helper()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(command.Args, " "), err, output)
	}
	return string(output)
}

// runAllocation starts the service binary, reads its first line of output, stops it with SIGTERM, and gives the
// line.
func runAllocation(t *testing.T, binary string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary)
	command.Env = append(os.Environ(), "STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK=")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	line, readErr := bufio.NewReader(stdout).ReadString('\n')
	_ = command.Process.Signal(syscall.SIGTERM)
	waitErr := command.Wait()
	if readErr != nil {
		t.Fatalf("the service printed no line: %v; wait: %v\n%s", readErr, waitErr, stderr.String())
	}
	if waitErr != nil {
		t.Fatalf("the service did not stop cleanly on SIGTERM: %v\n%s", waitErr, stderr.String())
	}
	return strings.TrimSpace(line)
}

func TestRuntimePinBuildsWithNoNetwork(t *testing.T) {
	t.Run("ST-STD-RUNTIME-R04-T02 A generated service that imports a runtime package resolves the pin from the common registry with no network, builds with the pinned Go and runs", func(t *testing.T) {
		goTool := pinnedGo(t)
		versionCommand := exec.Command(goTool, "env", "GOVERSION")
		versionCommand.Env = localtest.GoEnv()
		version := strings.TrimSpace(goOutput(t, versionCommand))
		if version != appbuild.GoVersion {
			t.Fatalf("%s is %s; this test builds with the pinned Go %s: set STEGO_TEST_PINNED_GO", goTool, version, appbuild.GoVersion)
		}
		input, pin := runtimeService(t)
		// The checksums of the modules that the runtime library requires come from its own go.sum, as the sum
		// database gives them; localtest.CheckSums checks each line that the resolution writes.
		_, source, _, _ := runtime.Caller(0)
		repository := filepath.Join(filepath.Dir(source), "..", "..")
		runtimeSums, err := os.ReadFile(filepath.Join(repository, "registry", "runtime", "go.sum"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(input.ProjectDir, "go.sum"), runtimeSums, 0o644); err != nil {
			t.Fatal(err)
		}
		plan, err := compiler.Reconcile(input)
		if err != nil {
			t.Fatal(err)
		}
		if err := compiler.Apply(plan, input.ProjectDir, ""); err != nil {
			t.Fatal(err)
		}

		// stego deps: the go command reads the pin from the common registry's runtime folder through the local
		// proxy, and every other module from the checked module cache of localtest, with no network
		// (offlineModules), with the pinned Go SDK first on PATH.
		offlineModules(t)
		t.Setenv("PATH", filepath.Dir(goTool)+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("GOWORK", "off")
		t.Setenv("GOTOOLCHAIN", "local")
		t.Setenv("GOFLAGS", "")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := appbuild.ResolveDependencies(ctx, input); err != nil {
			t.Fatal(err)
		}
		localtest.CheckSums(t, input.ProjectDir)
		data, err := os.ReadFile(filepath.Join(input.ProjectDir, "go.mod"))
		if err != nil {
			t.Fatal(err)
		}
		module, err := modfile.Parse("go.mod", data, nil)
		if err != nil {
			t.Fatal(err)
		}
		required := ""
		for _, requirement := range module.Require {
			if registry.IsStegoModule(requirement.Mod.Path) {
				required += requirement.Mod.Path + " " + requirement.Mod.Version + ";"
			}
		}
		if required != pin.Module+" "+pin.Version+";" || len(module.Replace) != 0 {
			t.Fatalf("go.mod requires %q with %d replace directives, want only the pin %s %s:\n%s", required, len(module.Replace), pin.Module, pin.Version, data)
		}
		sums, err := os.ReadFile(filepath.Join(input.ProjectDir, "go.sum"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(sums), pin.Module+" "+pin.Version+" "+pin.Sum+"\n") || !strings.Contains(string(sums), pin.Module+" "+pin.Version+"/go.mod h1:") {
			t.Fatalf("go.sum lacks the pin's lines:\n%s", sums)
		}
		if _, err := os.Stat(filepath.Join(input.ProjectDir, "go.work")); !os.IsNotExist(err) {
			t.Fatalf("the project holds a work file: %v", err)
		}
		drift, err := compiler.DetectDrift(input.ProjectDir, "")
		if err != nil || drift.HasDrift() {
			t.Fatalf("the resolved project drifts: %v\n%s", err, compiler.FormatDrift(drift))
		}

		// A build from the checked module cache, which holds the pin that stego deps staged, with no network.
		binary := filepath.Join(t.TempDir(), "svc")
		build := exec.Command(goTool, "build", "-mod=readonly", "-o", binary, "./out")
		build.Dir = input.ProjectDir
		build.Env = localtest.GoEnv()
		goOutput(t, build)
		want := "allocated 334 USD 333 USD 333 USD"
		if line := runAllocation(t, binary); line != want {
			t.Fatalf("the service printed %q, want %q", line, want)
		}

		// Local development and CI: the work file of scripts/runtime-workspace.sh (localtest.RuntimeWorkspace) resolves
		// the pin from the checkout's registry/runtime folder, not from the module cache, with no network.
		work := localtest.RuntimeWorkspace(t, input.ProjectDir)
		workspace := exec.Command(goTool, "build", "-o", binary, "./out")
		workspace.Dir = input.ProjectDir
		workspace.Env = localtest.GoEnv(work)
		goOutput(t, workspace)
		folder := exec.Command(goTool, "list", "-m", "-f", "{{.Dir}}", pin.Module)
		folder.Dir = input.ProjectDir
		folder.Env = localtest.GoEnv(work)
		runtimeDir, err := filepath.EvalSymlinks(filepath.Join(repository, "registry", "runtime"))
		if err != nil {
			t.Fatal(err)
		}
		if dir := strings.TrimSpace(goOutput(t, folder)); dir != runtimeDir {
			t.Fatalf("the workspace build takes the runtime library from %s, not from %s", dir, runtimeDir)
		}
		if line := runAllocation(t, binary); line != want {
			t.Fatalf("the workspace build printed %q, want %q", line, want)
		}
	})
}
