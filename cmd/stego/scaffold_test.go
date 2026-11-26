package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/fill"
	"github.com/jsell-rh/stego/internal/parser"
	"gopkg.in/yaml.v3"
)

func TestFillNamesCannotEscapeApplicationDirectory(t *testing.T) {
	for _, name := range []string{"../escape", "nested/fill", "/tmp/fill", "..", ".", "bad\\name", "CON", "type", "main", "_", "1policy", "bad.name"} {
		if _, err := fillPackageName(name); err == nil {
			t.Errorf("accepted unsafe fill name %q", name)
		}
	}
}

func TestFillScaffoldRejectsExistingDirectoryAndLinks(t *testing.T) {
	project := t.TempDir()
	out := t.TempDir()
	if err := os.Symlink(out, filepath.Join(project, "fills")); err != nil {
		t.Fatal(err)
	}
	if err := writeFillScaffold(project, "policy", []byte("declaration"), []byte("source")); err == nil {
		t.Fatal("accepted a symbolic link for fills")
	}
	if entries, err := os.ReadDir(out); err != nil || len(entries) != 0 {
		t.Fatalf("changed a path outside the project: %v", err)
	}
	project = t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "fills", "policy"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := writeFillScaffold(project, "policy", []byte("declaration"), []byte("source")); err == nil {
		t.Fatal("accepted an existing application directory")
	}
}

// This test builds the real generated service and executes a scaffold method.
// No database or HTTP server is needed for this contract check.
func TestFillWorkflowBuild(t *testing.T) {
	t.Run("ST-EXT-UC01-R01-T01 A scaffolded fill compiles against its slot contract, and its test runs in the generated service", func(t *testing.T) {
		registry := shippedRegistryCopy(t)
		project := serviceFolder(t)
		t.Chdir(project)
		t.Setenv("STEGO_REGISTRY", registry)
		t.Setenv("STEGO_MODULE", "example.com/fill-workflow")
		t.Setenv("STEGO_GO_VERSION", "1.26.11")
		t.Setenv("GOWORK", "off")
		if err := runInit([]string{"--archetype", "rest-crud"}); err != nil {
			t.Fatal(err)
		}
		if err := runFillCreate([]string{"policy", "--slot", "before_create", "--collection", "widgets"}); err != nil {
			t.Fatal(err)
		}
		source, err := os.ReadFile(filepath.Join(project, "fills/policy/fill.go"))
		if err != nil {
			t.Fatal(err)
		}
		declaration := onRuntimePath(`kind: service
name: fill-workflow
archetype: rest-crud
language: go
entities:
  - name: Widget
    fields:
      - {name: label, type: string}
collections:
  widgets:
    entity: Widget
    operations: [create, read]
`)
		binding := `slots:
  - collection: widgets
    slot: before_create
    gate: [policy]
`
		// The first apply, with no fill bound, writes the slot contracts that
		// the fill compiles against.
		writeProjectFile(t, "service.yaml", declaration)
		if err := runApply(nil); err != nil {
			t.Fatal(err)
		}
		contractTest := `package policy
import (
  "context"
  "errors"
  "testing"
  slots "example.com/fill-workflow/out/slots"
)
func TestScaffoldDeniesUnimplementedPolicy(t *testing.T) {
  var policy slots.BeforeCreateSlot = New()
  result, err := policy.Evaluate(context.Background(), &slots.BeforeCreateRequest{})
  if result != nil || !errors.Is(err, ErrNotImplemented) {
    t.Fatalf("unfinished policy returned success: %v, %v", result, err)
  }
}
`
		if err := os.WriteFile("fills/policy/fill_test.go", []byte(contractTest), 0644); err != nil {
			t.Fatal(err)
		}
		if err := runDependencies(nil); err != nil {
			t.Fatal(err)
		}
		// stego test records the fill's evidence, and the service author signs
		// it with a test key that SIGNERS lists, so that apply accepts the
		// bound fill.
		if err := runTest(nil); err != nil {
			t.Fatal(err)
		}
		keys := testFillKeys(t)
		if err := os.WriteFile(fill.SignersFile, []byte(keys.signers), 0644); err != nil {
			t.Fatal(err)
		}
		for _, entry := range keys.goEnv {
			name, value, _ := strings.Cut(entry, "=")
			t.Setenv(name, value)
		}
		t.Setenv("HOME", keys.homes["listed-ssh"])
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(keys.homes["listed-ssh"], ".config"))
		if err := runFillSign([]string{"policy"}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("service.yaml", []byte(declaration+binding), 0644); err != nil {
			t.Fatal(err)
		}
		if err := runApply(nil); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"test", "-mod=readonly", "./..."}, {"build", "-mod=readonly", "-o", filepath.Join(t.TempDir(), "service"), "./out"}} {
			cmd := exec.Command("go", args...)
			cmd.Dir = project
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("go %v: %v\n%s", args, err, output)
			}
		}
		resolvedModule, err := os.ReadFile("go.mod")
		if err != nil {
			t.Fatal(err)
		}
		resolvedSums, err := os.ReadFile("go.sum")
		if err != nil {
			t.Fatal(err)
		}
		if err := runApply(nil); err != nil {
			t.Fatal(err)
		}
		afterApply, err := os.ReadFile("go.mod")
		if err != nil || !bytes.Equal(resolvedModule, afterApply) {
			t.Fatalf("repeated apply changed the resolved module: %v", err)
		}
		if err := runDependencies(nil); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"test", "-mod=readonly", "./..."}, {"mod", "tidy"}} {
			cmd := exec.Command("go", args...)
			cmd.Dir = project
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("after repeated apply, go %v: %v\n%s", args, err, output)
			}
		}
		afterTidy, err := os.ReadFile("go.mod")
		if err != nil || !bytes.Equal(resolvedModule, afterTidy) {
			t.Fatalf("dependency resolution changed the module after repeated apply: %v\nbefore:\n%s\nafter:\n%s", err, resolvedModule, afterTidy)
		}
		afterSums, err := os.ReadFile("go.sum")
		if err != nil || !bytes.Equal(resolvedSums, afterSums) {
			t.Fatalf("dependency resolution changed checksums after repeated apply: %v", err)
		}
		input, err := buildReconcilerInput()
		if err != nil {
			t.Fatal(err)
		}
		plan, err := compiler.Reconcile(input)
		if err != nil || plan.HasChanges() {
			t.Fatalf("generation is not stable after dependency resolution: %v", err)
		}
		drift, err := compiler.DetectDrift(project, filepath.Join(project, "out"))
		if err != nil || drift.HasDrift() {
			t.Fatalf("resolved service has output drift: %v", err)
		}
		after, err := os.ReadFile("fills/policy/fill.go")
		if err != nil || !bytes.Equal(source, after) {
			t.Fatalf("apply changed the application fill: %v", err)
		}
	})
}

// --- Reading of the fill manifest (internal/fill) ---

// fillFixture is a fill manifest under testdata/fill.
func fillFixture(name string) string {
	return filepath.Join("testdata", "fill", name)
}

func TestParseFill(t *testing.T) {
	f, err := fill.ParseFill(fillFixture("fill.yaml"))
	if err != nil {
		t.Fatalf("ParseFill: %v", err)
	}
	if f.Kind != "fill" {
		t.Errorf("kind = %q, want %q", f.Kind, "fill")
	}
	if f.Name != "admin-creation-policy" {
		t.Errorf("name = %q, want %q", f.Name, "admin-creation-policy")
	}
	if f.Implements != "rest-api.before_create" {
		t.Errorf("implements = %q, want %q", f.Implements, "rest-api.before_create")
	}
	if f.Collection != "org-users" {
		t.Errorf("collection = %q, want %q", f.Collection, "org-users")
	}
}

func TestParseFillTyped(t *testing.T) {
	f, err := fill.ParseFill(fillFixture("fill.yaml"))
	if err != nil {
		t.Fatalf("ParseFill: %v", err)
	}
	if f.Name != "admin-creation-policy" {
		t.Errorf("name = %q, want %q", f.Name, "admin-creation-policy")
	}
}

func TestRoundTripFill(t *testing.T) {
	f, err := fill.ParseFill(fillFixture("fill.yaml"))
	if err != nil {
		t.Fatalf("ParseFill: %v", err)
	}
	data, err := yaml.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var f2 fill.Fill
	if err := yaml.Unmarshal(data, &f2); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
	if !reflect.DeepEqual(*f, f2) {
		t.Errorf("round-trip mismatch:\n  original: %+v\n  got:      %+v", *f, f2)
	}
}

func TestFillRejectsDeprecatedEntityKey(t *testing.T) {
	input := `
kind: fill
name: my-policy
implements: rest-api.before_create
entity: User
`
	var f fill.Fill
	err := yaml.Unmarshal([]byte(input), &f)
	if err == nil {
		t.Fatal("expected error for deprecated 'entity' key in fill declaration, got nil")
	}
	if !strings.Contains(err.Error(), "renamed to 'collection'") {
		t.Errorf("error message should mention renaming to 'collection', got: %v", err)
	}
}

func TestFillUnmarshal(t *testing.T) {
	input := `
kind: fill
name: admin-creation-policy
implements: rest-api.before_create
collection: org-users
`
	var f fill.Fill
	if err := yaml.Unmarshal([]byte(input), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.Name != "admin-creation-policy" {
		t.Errorf("name = %q, want %q", f.Name, "admin-creation-policy")
	}
	if f.Implements != "rest-api.before_create" {
		t.Errorf("implements = %q, want %q", f.Implements, "rest-api.before_create")
	}
	if f.Collection != "org-users" {
		t.Errorf("collection = %q, want %q", f.Collection, "org-users")
	}
}

func TestStrictFillManifest(t *testing.T) {
	err := parser.DecodeStrict([]byte("kind: fill\nqualifed_by: operator\n"), "fill.yaml", &fill.Fill{})
	if err == nil || !strings.Contains(err.Error(), "qualifed_by") {
		t.Fatalf("got %v, want error containing %q", err, "qualifed_by")
	}
}

// goInProject runs the go command in the project folder, with the module files read only, and fails the test on an
// error.
func goInProject(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=readonly")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, output)
	}
}

// walkthroughBuild is the project of the fifth walkthrough after its first build, which the first test that needs it
// makes, and the binary of that build. Each later test works in a copy of it, so the package builds the walkthrough
// once and not once per test.
var walkthroughBuild struct {
	sync.Mutex
	project, binary string
}

// walkthroughFirstBuild gives the project of the fifth walkthrough after its first build in a new working folder,
// with the environment of the walkthrough, and gives the path of the binary. The first test that calls it runs the
// user flow (walkthroughFlow) and keeps a copy of the result; a later test gets a copy of that copy. When the flow
// fails, no copy is kept, and the next test runs the flow again.
func walkthroughFirstBuild(t *testing.T) string {
	t.Helper()
	walkthroughBuild.Lock()
	defer walkthroughBuild.Unlock()
	if walkthroughBuild.project == "" {
		binary := walkthroughFlow(t)
		folder, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		// The guard runs before any copy: a copy is the same project only when no file of the project names the
		// folder that it was made in.
		if err := sameProjectInCopy(folder); err != nil {
			t.Fatal(err)
		}
		// Each attempt copies into a new folder, which it removes when a copy fails, so a later attempt never copies
		// into what an earlier one left.
		keep, err := os.MkdirTemp(fixtureRoot, "walkthrough-first-build-")
		if err != nil {
			t.Fatal(err)
		}
		for _, copy := range [][2]string{{folder, filepath.Join(keep, "service")}, {binary, filepath.Join(keep, "inventory")}} {
			if output, err := exec.Command("cp", "-a", copy[0], copy[1]).CombinedOutput(); err != nil {
				_ = os.RemoveAll(keep)
				t.Fatalf("keep the first build of the walkthrough: %v\n%s", err, output)
			}
		}
		walkthroughBuild.project, walkthroughBuild.binary = filepath.Join(keep, "service"), filepath.Join(keep, "inventory")
		return binary
	}
	project := copyFolder(t, walkthroughBuild.project)
	binary := filepath.Join(t.TempDir(), "inventory")
	if output, err := exec.Command("cp", "-a", walkthroughBuild.binary, binary).CombinedOutput(); err != nil {
		t.Fatalf("copy the binary of the walkthrough: %v\n%s", err, output)
	}
	t.Chdir(project)
	walkthroughSettings(t)
	signingSettings(t, testFillKeys(t))
	return binary
}

// sameProjectInCopy gives an error when a copy of the project in folder is not the same project: a regular file names
// the folder, as it is or with its links resolved, or a symlink has an absolute target, a target that names the folder,
// a target that does not resolve, or a target that resolves out of the folder. The target is resolved through each
// link on its path (filepath.EvalSymlinks) and compared with the resolved folder, so a link such as b/../outside, where
// b links to .., is out of the folder although its text is not (sec-507 L1). A copy of such a link does not point into
// the copy.
func sameProjectInCopy(folder string) error {
	resolved, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return err
	}
	names := [][]byte{[]byte(folder)}
	if resolved != folder {
		names = append(names, []byte(resolved))
	}
	named := func(data []byte) bool {
		for _, name := range names {
			if bytes.Contains(data, name) {
				return true
			}
		}
		return false
	}
	return filepath.WalkDir(folder, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(name)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) || named([]byte(target)) {
				return fmt.Errorf("%s links to %s, so a copy of the project %s is not the same project", name, target, folder)
			}
			end, err := filepath.EvalSymlinks(name)
			if err != nil {
				return fmt.Errorf("%s links to %s, which does not resolve, so a copy of the project %s is not the same project: %w", name, target, folder, err)
			}
			inside, err := filepath.Rel(resolved, end)
			if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) || filepath.IsAbs(inside) {
				return fmt.Errorf("%s links to %s, which resolves to %s out of the folder, so a copy of the project %s is not the same project", name, target, end, folder)
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if named(data) {
			return fmt.Errorf("%s names the folder of the project %s, so a copy of the project is not the same project", name, folder)
		}
		return nil
	})
}

func TestWalkthroughCopyGuard(t *testing.T) {
	t.Run("ST-STD-LOCAL-R05-T04 The walkthrough copy guard refuses a project link that resolves out of the project folder or does not resolve", func(t *testing.T) {
		project := func(t *testing.T) string {
			t.Helper()
			root := t.TempDir()
			folder := filepath.Join(root, "project")
			if err := os.MkdirAll(filepath.Join(folder, "deep"), 0o755); err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string]string{filepath.Join(root, "outside"): "x", filepath.Join(folder, "inside"): "y"} {
				if err := os.WriteFile(name, []byte(data), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			return folder
		}
		link := func(t *testing.T, target, name string) {
			t.Helper()
			if err := os.Symlink(target, name); err != nil {
				t.Fatal(err)
			}
		}
		// The reproduction of sec-507 L1: deep/b links to .., so deep/a, whose text b/../outside stays in the folder,
		// resolves to the file beside the folder.
		folder := project(t)
		link(t, "..", filepath.Join(folder, "deep", "b"))
		link(t, "b/../outside", filepath.Join(folder, "deep", "a"))
		if err := sameProjectInCopy(folder); err == nil || !strings.Contains(err.Error(), "out of the folder") {
			t.Fatalf("the guard gave %v for a link that resolves out of the folder through another link", err)
		}
		folder = project(t)
		link(t, "missing", filepath.Join(folder, "deep", "broken"))
		if err := sameProjectInCopy(folder); err == nil || !strings.Contains(err.Error(), "does not resolve") {
			t.Fatalf("the guard gave %v for a link that does not resolve", err)
		}
		// Links that resolve in the folder, also through another link, keep the copy the same project.
		folder = project(t)
		link(t, "deep", filepath.Join(folder, "down"))
		link(t, "../inside", filepath.Join(folder, "deep", "up"))
		link(t, "down/up", filepath.Join(folder, "chain"))
		if err := sameProjectInCopy(folder); err != nil {
			t.Fatalf("the guard refused links that resolve in the folder: %v", err)
		}
	})
}

// walkthroughFlow runs the user flow of the fifth walkthrough up to its first binary, with no step by hand: stego
// init, the inventory declaration, stego fill create for the command, the query and the consuming worker, validate,
// apply, deps and go build. It gives the path of the binary.
func walkthroughFlow(t *testing.T) string {
	t.Helper()
	walkthroughProject(t)
	for name, slot := range map[string]string{"place-order": "command.command", "open-orders": "query.query", "fulfill-orders": "worker.worker"} {
		mustRun(t, runFillCreate, name, "--slot", slot)
		for _, file := range []string{"fill.yaml", "fill.go", "fill_test.go"} {
			if _, err := os.Stat(filepath.Join("fills", name, file)); err != nil {
				t.Fatalf("fill create wrote no %s for %s: %v", file, name, err)
			}
		}
	}
	// The fills are qualified before the first apply, which writes their contracts (ST-GEN-INV10).
	qualifyInProcess(t)
	mustRun(t, runValidate)
	mustRun(t, runApply)
	mustRun(t, runDependencies)
	binary := filepath.Join(t.TempDir(), "inventory")
	goInProject(t, "build", "-o", binary, "./out")
	goInProject(t, "vet", "./fills/...")
	goInProject(t, "test", "./fills/...")
	return binary
}

func TestCapabilityFillScaffold(t *testing.T) {
	t.Run("ST-EXT-UC01-R01-T03 Fills scaffolded for a command, a query and a consuming worker compile against their contracts and build into the service", func(t *testing.T) {
		walkthroughFirstBuild(t)
		data, err := os.ReadFile(filepath.Join("fills", "fulfill-orders", fill.ManifestFile))
		var manifest fill.Fill
		if err == nil {
			err = yaml.Unmarshal(data, &manifest)
		}
		if err != nil || manifest.Implements != "worker.worker" || manifest.Name != "fulfill-orders" || manifest.Kind != "fill" {
			t.Fatalf("the manifest of fulfill-orders is %+v, %v; want the fill fulfill-orders of the slot worker.worker", manifest, err)
		}
		// The package of a fill is its name with no hyphen, as Go names a package (walkthrough 7, B12).
		for name, pkg := range map[string]string{"place-order": "placeorder", "open-orders": "openorders", "fulfill-orders": "fulfillorders"} {
			for _, file := range []string{"fill.go", "fill_test.go"} {
				source, err := os.ReadFile(filepath.Join("fills", name, file))
				if err != nil || !regexp.MustCompile(`(?m)^package `+pkg+`$`).Match(source) {
					t.Fatalf("fills/%s/%s is not in the package %s: %v\n%s", name, file, pkg, err, source)
				}
			}
		}
		// The test of an unfinished fill does not name ErrNotImplemented, so it still compiles when the author writes
		// the fill and removes the variable; when the fill is written, it fails and says to replace it (walkthrough 11,
		// W11-B7).
		for _, name := range []string{"place-order", "open-orders", "fulfill-orders"} {
			if test, err := os.ReadFile(filepath.Join("fills", name, "fill_test.go")); err != nil || strings.Contains(string(test), "ErrNotImplemented") {
				t.Fatalf("fills/%s/fill_test.go names ErrNotImplemented: %v\n%s", name, err, test)
			}
		}
		source, err := os.ReadFile(filepath.Join("fills", "place-order", "fill.go"))
		if err != nil {
			t.Fatal(err)
		}
		written := strings.Replace(string(source), "return nil, ErrNotImplemented", "return nil, nil", 1)
		if written == string(source) {
			t.Fatalf("fills/place-order/fill.go has no unfinished return:\n%s", source)
		}
		writeProjectFile(t, "fills/place-order/fill.go", written)
		command := exec.Command("go", "test", "-count=1", "./fills/place-order")
		command.Env = append(os.Environ(), "GOFLAGS=-mod=readonly")
		output, err := command.CombinedOutput()
		writeProjectFile(t, "fills/place-order/fill.go", string(source))
		if err == nil || !strings.Contains(string(output), "replace it in fill_test.go with the tests of what the fill does") {
			t.Fatalf("the scaffold test of a written fill gave %v, want a failure that says to replace it:\n%s", err, output)
		}
		// The fill of a worker serves no collection, so its manifest holds no collection key (walkthrough 7, B12).
		if strings.Contains(string(data), "collection") {
			t.Fatalf("the manifest of fulfill-orders holds a collection key:\n%s", data)
		}
		// A fill that the declaration does not name, and a fill that exists, are refused, and nothing is written.
		before := projectTree(t)
		if _, err := capture(t, runFillCreate, "restock", "--slot", "worker.worker"); err == nil || !strings.Contains(err.Error(), "no worker of the declaration names the fill") {
			t.Fatalf("fill create wrote a fill that no worker names: %v", err)
		}
		if _, err := capture(t, runFillCreate, "place-order", "--slot", "command.command"); err == nil {
			t.Fatal("fill create wrote over the fill place-order")
		}
		if _, err := capture(t, runFillCreate, "place-order", "--slot", "command"); err == nil || !strings.Contains(err.Error(), "command.command") {
			t.Fatalf("fill create of the slot command does not name the slot command.command: %v", err)
		}
		if !maps.Equal(before, projectTree(t)) {
			t.Fatal("a refused fill create changed the project")
		}
	})
}

func TestCapabilityAddedLater(t *testing.T) {
	t.Run("ST-EXT-UC01-R01-T04 A command and a worker declared after the first build get their contracts in a later apply", func(t *testing.T) {
		binary := walkthroughFirstBuild(t)
		// A second command, declared after go.mod exists, with its fill from fill create before the apply that writes
		// its contract.
		declaration, err := os.ReadFile("service.yaml")
		if err != nil {
			t.Fatal(err)
		}
		withCommand := strings.Replace(string(declaration), "\nqueries:", walkthroughFixture(t, "grant-clerk.yaml")+"\nqueries:", 1)
		writeProjectFile(t, "service.yaml", withCommand)
		mustRun(t, runFillCreate, "grant-clerk", "--slot", "command.command")
		qualifyInProcess(t)
		mustRun(t, runApply)
		mustRun(t, runDependencies)
		goInProject(t, "build", "-o", binary, "./out")
		// A worker with an every trigger whose fill folder holds only its manifest: apply writes the worker's
		// contract and then names the fill, which the service needs to build.
		schema, err := os.ReadFile("authz/schema.zed")
		if err != nil {
			t.Fatal(err)
		}
		writeProjectFile(t, "authz/schema.zed", strings.Replace(string(schema), "\trelation worker_fulfill_orders: service_account\n",
			"\trelation worker_fulfill_orders: service_account\n\trelation worker_low_stock_scan: service_account\n", 1))
		writeProjectFile(t, "service.yaml", strings.Replace(withCommand, "    fill: fulfill-orders\n",
			"    fill: fulfill-orders\n  - name: low-stock-scan\n    trigger: every 1m\n    writes: [StockItem]\n    resource_type: stock_item\n    permission: edit\n    fill: low-stock-scan\n", 1))
		writeProjectFile(t, "fills/low-stock-scan/fill.yaml", "kind: fill\nname: low-stock-scan\nimplements: worker.worker\n")
		mustRun(t, runValidate)
		printed, err := capture(t, runApply)
		if err == nil || !strings.Contains(err.Error(), `fill "low-stock-scan" holds no Go code yet`) || !strings.Contains(err.Error(), "workers[1].fill") {
			t.Fatalf("apply did not name the fill with no code: %v\n%s", err, printed)
		}
		contract, err := os.ReadFile("out/workers/workers.go")
		if err != nil || !strings.Contains(string(contract), "type LowStockScanFill func(ctx context.Context, tick time.Time, h *LowStockScanHandle) error") {
			t.Fatalf("apply did not write the contract of low-stock-scan: %v", err)
		}
		writeProjectFile(t, "fills/low-stock-scan/fill.go", `package low_stock_scan

import (
	"context"
	"time"

	workers "example.com/inventory/out/workers"
)

func New() workers.LowStockScanFill {
	return func(ctx context.Context, tick time.Time, h *workers.LowStockScanHandle) error { return nil }
}
`)
		// A fill that holds code is held to qualification: apply refuses it until stego test and a sign-off
		// qualify it.
		if _, err := capture(t, runApply); err == nil || !strings.Contains(err.Error(), `fill "low-stock-scan" is untested`) {
			t.Fatalf("apply did not refuse the fill with code and no tests: %v", err)
		}
		writeProjectFile(t, "fills/low-stock-scan/fill_test.go", "package low_stock_scan\n\nimport \"testing\"\n\n// TestBuilds holds that the fill builds against its contract.\nfunc TestBuilds(t *testing.T) {}\n")
		qualifyInProcess(t)
		if printed := mustRun(t, runApply); !strings.Contains(printed, "No changes") {
			t.Fatalf("apply after the fill was written changed files:\n%s", printed)
		}
		mustRun(t, runDependencies)
		goInProject(t, "build", "-o", binary, "./out")
		goInProject(t, "test", "./fills/...")
	})
}
