package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/localtest"
	"github.com/jsell-rh/stego/internal/registry"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

// These tests run stego deps in process, as a service author does, against a
// project that stego apply made from the built-in registry. Go's module tools
// fetch through the module proxy that the environment names.

func dependencyProject(t *testing.T) {
	t.Helper()
	generatingProject(t, generatingDeclaration)
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	offlineModules(t)
	mustRun(t, runApply)
}

// offlineModules sets the environment of the go commands that stego deps and stego apply run: the module cache of
// localtest is their GOMODCACHE and their only module proxy, with no sum database, no private or insecure module and
// no version control (ST-STD-LOCAL-R03). The test checks the go.sum that a resolution writes (localtest.CheckSums).
func offlineModules(t *testing.T) {
	t.Helper()
	localtest.RequireModules(t)
	for _, entry := range localtest.GoEnv() {
		key, value, _ := strings.Cut(entry, "=")
		switch key {
		case "GOMODCACHE", "GOPROXY", "GOSUMDB", "GONOSUMDB", "GONOSUMCHECK", "GOPRIVATE", "GONOPROXY", "GOINSECURE", "GOVCS", "GOAUTH", "GOENV":
			t.Setenv(key, value)
		}
	}
}

func moduleFiles(t *testing.T) (string, string) {
	t.Helper()
	module, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile("go.sum")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(module), string(sums)
}

func noPendingTransaction(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(".stego/transaction.yaml"); !os.IsNotExist(err) {
		t.Fatalf("a resolution left an apply journal: %v", err)
	}
}

func TestDependenciesChangeModuleFilesOnlyOnSuccess(t *testing.T) {
	t.Run("ST-BLD-UC01-R01-T01 A failed resolution leaves go.mod and go.sum as they were", func(t *testing.T) {
		dependencyProject(t)
		mustRun(t, runDependencies)
		localtest.CheckSums(t, ".")
		module, sums := moduleFiles(t)
		// A source file that imports a module the cache does not hold makes
		// Go's module tools fail.
		writeProjectFile(t, "extra/extra.go", "package extra\n\nimport _ \"example.invalid/stego-missing/module\"\n")
		printed, err := capture(t, runDependencies)
		if err == nil {
			t.Fatalf("a failed resolution reported success:\n%s", printed)
		}
		if after, afterSums := moduleFiles(t); after != module || afterSums != sums {
			t.Fatal("a failed resolution changed go.mod or go.sum")
		}
		noPendingTransaction(t)
	})
	t.Run("ST-BLD-UC01-R01-T02 An interrupted resolution leaves the module files as they were, or as resolved", func(t *testing.T) {
		dependencyProject(t)
		original, originalSums := moduleFiles(t)
		// Find the resolved pair once; each round then starts again from the
		// original pair.
		mustRun(t, runDependencies)
		localtest.CheckSums(t, ".")
		resolved, resolvedSums := moduleFiles(t)
		if resolved == original && resolvedSums == originalSums {
			t.Fatal("the fixture resolves to the same module files; the test shows nothing")
		}
		// Keep the interrupt from ending the test process, at every delay.
		guard := make(chan os.Signal, 1)
		signal.Notify(guard, os.Interrupt)
		defer signal.Stop(guard)
		interrupted := 0
		for _, delay := range []time.Duration{0, time.Millisecond, 5 * time.Millisecond, 20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond} {
			writeProjectFile(t, "go.mod", original)
			if originalSums == "" {
				if err := os.Remove("go.sum"); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			} else {
				writeProjectFile(t, "go.sum", originalSums)
			}
			mustRun(t, runApply)
			timer := time.AfterFunc(delay, func() { _ = syscall.Kill(os.Getpid(), syscall.SIGINT) })
			_, err := capture(t, runDependencies)
			timer.Stop()
			time.Sleep(10 * time.Millisecond)
			for len(guard) > 0 {
				<-guard
			}
			if err != nil {
				interrupted++
			}
			if errors.Is(err, compiler.ErrRecoveryRequired) {
				mustRun(t, runRecover)
			}
			noPendingTransaction(t)
			module, sums := moduleFiles(t)
			if (module != original || sums != originalSums) && (module != resolved || sums != resolvedSums) {
				t.Fatalf("after an interrupt at %s, go.mod and go.sum are neither the old pair nor the resolved pair: %v", delay, err)
			}
		}
		if interrupted == 0 {
			t.Fatal("no interrupt reached a resolution; the test shows nothing")
		}
		t.Logf("%d of the resolutions were interrupted", interrupted)
	})
}

func TestDependenciesRecordTheResolvedModules(t *testing.T) {
	t.Run("ST-BLD-UC01-R01-T04 The record that resolution writes names go.mod and go.sum as resolved, so a later apply of the same sources leaves the record as it is", func(t *testing.T) {
		dependencyProject(t)
		original, originalSums := moduleFiles(t)
		mustRun(t, runDependencies)
		localtest.CheckSums(t, ".")
		resolved, resolvedSums := moduleFiles(t)
		if resolved == original && resolvedSums == originalSums {
			t.Fatal("the fixture resolves to the same module files; the test shows nothing")
		}
		state, err := compiler.LoadState(".stego/state.yaml")
		if err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string]string{"go.mod": resolved, "go.sum": resolvedSums} {
			if recorded := state.LastApplied.Inputs.Files[name]; !recorded.Exists || recorded.SHA256 != compiler.HashBytes([]byte(data)) {
				t.Errorf("the record names %s with the digest %s, not that of the resolved file", name, recorded.SHA256)
			}
		}
		// An apply of the same sources, as in a fresh clone of the committed project, changes no record.
		record, err := os.ReadFile(".stego/state.yaml")
		if err != nil {
			t.Fatal(err)
		}
		mustRun(t, runApply)
		after, err := os.ReadFile(".stego/state.yaml")
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(record) {
			t.Fatalf("an apply after stego deps changed the record:\n--- after deps\n%s\n--- after apply\n%s", record, after)
		}
	})
}

func TestGeneratedFolderRefusedBeforeBuild(t *testing.T) {
	t.Run("ST-EXT-UC02-R02-T13 plan, apply and deps refuse a package under out that holds a file go build takes that is not a Go file, or a link", func(t *testing.T) {
		dependencyProject(t)
		mustRun(t, runPlan)
		outside := filepath.Join(t.TempDir(), "elsewhere.go")
		if err := os.WriteFile(outside, []byte("package api\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			name string
			add  func(t *testing.T)
			want string
		}{
			{"an assembler file", func(t *testing.T) { writeProjectFile(t, "out/internal/api/stub.s", "TEXT ·x(SB),0,$0\n") }, `holds "stub.s", which is not a Go file`},
			{"a system object", func(t *testing.T) { writeProjectFile(t, "out/internal/api/blob_amd64.syso", "\x7fELF") }, `holds "blob_amd64.syso", which is not a Go file`},
			{"a C file", func(t *testing.T) { writeProjectFile(t, "out/internal/api/glue.c", "int x;\n") }, `holds "glue.c", which is not a Go file`},
			// A name with control characters is named only escaped (sec-431 LOW-1).
			{"a name with control characters", func(t *testing.T) { writeProjectFile(t, "out/internal/api/x\x1b[2K\r.s", "\n") }, `holds "x\x1b[2K\r.s", which is not a Go file`},
			{"a linked Go file", func(t *testing.T) {
				if err := os.Symlink(outside, filepath.Join("out", "internal", "api", "elsewhere.go")); err != nil {
					t.Fatal(err)
				}
			}, `holds "elsewhere.go", which is not a regular file`},
		} {
			before := treeOf(t, "out")
			c.add(t)
			for _, run := range []struct {
				name string
				fn   func([]string) error
			}{{"plan", runPlan}, {"apply", runApply}, {"deps", runDependencies}} {
				printed, err := capture(t, run.fn)
				if err == nil || !strings.Contains(err.Error(), c.want) || strings.ContainsAny(err.Error(), "\x1b\r") || !strings.Contains(err.Error(), "plan, apply and deps refuse it too") {
					t.Fatalf("%s: stego %s gave %v, want a refusal that says %q:\n%s", c.name, run.name, err, c.want, printed)
				}
			}
			// The file is removed, and plan passes again.
			if err := os.RemoveAll(filepath.Join("out", "internal", "api", "stub.s")); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"out/internal/api/x\x1b[2K\r.s", "out/internal/api/blob_amd64.syso", "out/internal/api/glue.c", "out/internal/api/elsewhere.go"} {
				if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			if after := treeOf(t, "out"); !maps.Equal(before, after) {
				t.Fatalf("%s: a refused run changed the generated folder", c.name)
			}
			mustRun(t, runPlan)
		}
	})
}

func TestDependenciesFollowTheCapturedSource(t *testing.T) {
	t.Run("ST-BLD-UC01-R02-T01 A saved overlay flag and ambient GOFLAGS cannot change what resolution reads", func(t *testing.T) {
		dependencyProject(t)
		mustRun(t, runDependencies)
		localtest.CheckSums(t, ".")
		project, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		dependency := t.TempDir()
		writeProjectFile(t, filepath.Join(dependency, "go.mod"), "module example.com/overlay\ngo 1.26.11\n")
		writeProjectFile(t, filepath.Join(dependency, "overlay.go"), "package overlay\n")
		raw, err := os.ReadFile("go.mod")
		if err != nil {
			t.Fatal(err)
		}
		module, err := modfile.Parse("go.mod", raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := module.AddReplace("example.com/overlay", "", dependency, ""); err != nil {
			t.Fatal(err)
		}
		if raw, err = module.Format(); err != nil {
			t.Fatal(err)
		}
		writeProjectFile(t, "go.mod", string(raw))
		const source = "package extra\n"
		writeProjectFile(t, "extra/extra.go", source)
		outside := t.TempDir()
		virtual := filepath.Join(outside, "virtual.go")
		writeProjectFile(t, virtual, "package extra\n\nimport _ \"example.com/overlay\"\n")
		encoded, err := json.Marshal(map[string]any{"Replace": map[string]string{filepath.Join(project, "extra/extra.go"): virtual}})
		if err != nil {
			t.Fatal(err)
		}
		overlay := filepath.Join(outside, "overlay.json")
		writeProjectFile(t, overlay, string(encoded))
		config := filepath.Join(outside, "go.env")
		writeProjectFile(t, config, "GOFLAGS=-overlay="+overlay+"\nGOPROXY=off\n")
		t.Setenv("GOENV", config)
		t.Setenv("GOPROXY", "")
		t.Setenv("GOFLAGS", "-stego-invalid-ambient-flag")
		mustRun(t, runDependencies)
		localtest.CheckSums(t, ".")
		raw, err = os.ReadFile("go.mod")
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := modfile.Parse("go.mod", raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, requirement := range resolved.Require {
			if requirement.Mod.Path == "example.com/overlay" {
				t.Fatal("a saved overlay flag reached the module requirements")
			}
		}
		if actual, err := os.ReadFile("extra/extra.go"); err != nil || string(actual) != source {
			t.Fatal("resolution changed the source", err)
		}
	})
	t.Run("ST-BLD-UC01-R02-T02 Resolution ignores an ambient vendor flag and a missing go.work", func(t *testing.T) {
		dependencyProject(t)
		writeProjectFile(t, "local/go.mod", "module example.com/local\ngo 1.26.11\n")
		writeProjectFile(t, "local/local.go", "package local\n")
		raw, err := os.ReadFile("go.mod")
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, []byte("\nrequire example.com/local v0.0.0\nreplace example.com/local => ./local\n")...)
		writeProjectFile(t, "go.mod", string(raw))
		writeProjectFile(t, "extra/extra.go", "package extra\n\nimport _ \"example.com/local\"\n")
		t.Setenv("GOWORK", filepath.Join(t.TempDir(), "missing.work"))
		t.Setenv("GOFLAGS", "-mod=vendor")
		var first string
		for i := 0; i < 2; i++ {
			mustRun(t, runDependencies)
			localtest.CheckSums(t, ".")
		localtest.CheckSums(t, ".")
			module, _ := moduleFiles(t)
			if i == 0 {
				first = module
			} else if module != first {
				t.Fatal("two resolutions of the same source gave different go.mod files")
			}
		}
		if !strings.Contains(first, "replace example.com/local => ./local") {
			t.Fatal("resolution dropped the local replacement")
		}
	})
}

func TestDependenciesWaitForGeneration(t *testing.T) {
	t.Run("ST-BLD-UC01-R03-T01 Resolution refuses out-of-date generated code and names apply", func(t *testing.T) {
		generatingProject(t, generatingDeclaration)
		before := projectTree(t)
		if _, err := capture(t, runDependencies); err == nil || !strings.Contains(err.Error(), "stego apply") {
			t.Fatalf("resolution ran before the first apply: %v", err)
		}
		if !maps.Equal(before, projectTree(t)) {
			t.Fatal("a refused resolution changed the project")
		}
		mustRun(t, runApply)
		writeProjectFile(t, "service.yaml", strings.Replace(generatingDeclaration, "body", "text", 1))
		before = projectTree(t)
		if _, err := capture(t, runDependencies); err == nil || !strings.Contains(err.Error(), "stego apply") {
			t.Fatalf("resolution ran on generated code that is out of date: %v", err)
		}
		if !maps.Equal(before, projectTree(t)) {
			t.Fatal("a refused resolution changed the project")
		}
	})
}

// walkthroughProject makes a project from the declaration of the fifth walkthrough (testdata/walkthrough5): the
// inventory service with the command place-order, the query open-orders and the worker fulfill-orders, whose one
// trigger consumes the effect of place-order. stego init runs first, as a service author does.
func walkthroughProject(t *testing.T) {
	t.Helper()
	_, source, _, _ := runtime.Caller(0)
	fixture := filepath.Join(filepath.Dir(source), "testdata", "walkthrough5")
	t.Chdir(serviceFolder(t))
	walkthroughSettings(t)
	mustRun(t, runInit, "--archetype", "rest-crud")
	for name, target := range map[string]string{"schema.zed": "authz/schema.zed", "inventory.proto": "proto/inventory.proto", "service.yaml": "service.yaml"} {
		data, err := os.ReadFile(filepath.Join(fixture, name))
		if err != nil {
			t.Fatal(err)
		}
		writeProjectFile(t, target, string(data))
	}
}

// walkthroughSettings sets the environment in which the commands of stego run for the project of the fifth
// walkthrough.
func walkthroughSettings(t *testing.T) {
	t.Helper()
	t.Setenv("STEGO_REGISTRY", shippedRegistry(t))
	t.Setenv("STEGO_MODULE", "example.com/inventory")
	t.Setenv("STEGO_GO_VERSION", "1.26.11")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
}

// walkthroughFixture gives a file of testdata/walkthrough5.
func walkthroughFixture(t *testing.T, name string) string {
	t.Helper()
	_, source, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(source), "testdata", "walkthrough5", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The fills of the walkthrough project, each with its manifest and one Go file that imports only its contract.
var walkthroughFills = map[string]struct{ implements, source string }{
	"place-order": {"command.command", `package place_order

import (
	"context"
	"errors"

	commands "example.com/inventory/out/commands"
)

func New() commands.PlaceOrderFill {
	return func(ctx context.Context, req *commands.PlaceOrderRequest, h *commands.PlaceOrderHandle) (*commands.PlaceOrderResponse, error) {
		return nil, errors.New("place-order: not written")
	}
}
`},
	"open-orders": {"query.query", `package open_orders

import (
	"context"
	"errors"

	queries "example.com/inventory/out/queries"
)

func Fill(ctx context.Context, req queries.OpenOrdersRequest, h *queries.OpenOrdersHandle) (queries.OpenOrdersResponse, error) {
	var none queries.OpenOrdersResponse
	return none, errors.New("open-orders: not written")
}
`},
	"fulfill-orders": {"worker.worker", `package fulfill_orders

import (
	"context"
	"encoding/json"
	"errors"

	workers "example.com/inventory/out/workers"
)

func New() workers.FulfillOrdersFill {
	return func(ctx context.Context, message json.RawMessage, effect string, h *workers.FulfillOrdersHandle) error {
		return errors.New("fulfill-orders: not written")
	}
}
`},
}

func TestDependenciesCheckTheFillsWithTheResolvedModules(t *testing.T) {
	t.Run("ST-BLD-UC01-R01-T03 Resolution checks the fills with the go.mod it resolves, once fills exist", func(t *testing.T) {
		walkthroughProject(t)
		writeWalkthroughFills(t)
		qualifyInProcess(t)
		// The first apply writes the go.mod of the component requirements and a go.sum with the runtime pin only.
		mustRun(t, runApply)
		// A go build before stego deps asks for the go.sum entry of the runtime's go.mod, as the fifth walkthrough
		// found. With it, go list loads the module graph, which needs more than the go.mod of the first apply holds.
		// The download reads the runtime pin from the registry's runtime folder, so the test needs no published tag.
		download := exec.Command("go", "mod", "download", "github.com/jsell-rh/stego/registry/runtime")
		download.Env = append(os.Environ(), "GOPROXY="+runtimeProxy(t)+",off", "GONOSUMDB=github.com/jsell-rh/stego/registry/runtime", "GOFLAGS=-mod=mod")
		if output, err := download.CombinedOutput(); err != nil {
			t.Fatalf("go mod download: %v\n%s", err, output)
		}
		module, sums := moduleFiles(t)
		list := exec.Command("go", "list", "-mod=readonly", "-e", "-deps", "./fills/...")
		if output, err := list.CombinedOutput(); err == nil || !strings.Contains(string(output), "updates to go.mod needed") {
			t.Fatalf("the fixture does not show the state of the walkthrough, which the test needs: %v\n%s", err, output)
		}
		// Validate and apply check the fills with a copy of go.mod and go.sum that go list completes, and write
		// neither file.
		mustRun(t, runValidate)
		mustRun(t, runApply)
		if after, afterSums := moduleFiles(t); after != module || afterSums != sums {
			t.Fatal("validate or apply changed go.mod or go.sum")
		}
		// A fill that breaks the fill rules stops the resolution, which names it and keeps the module files.
		writeProjectFile(t, "fills/place-order/bad.go", "package place_order\n\nimport _ \"os\"\n")
		printed, err := capture(t, runDependencies)
		if err == nil || !strings.Contains(err.Error(), "place-order") || !strings.Contains(err.Error(), "may not import os") {
			t.Fatalf("resolution did not refuse the fill that imports os: %v\n%s", err, printed)
		}
		if after, afterSums := moduleFiles(t); after != module || afterSums != sums {
			t.Fatal("a refused resolution changed go.mod or go.sum")
		}
		noPendingTransaction(t)
		if err := os.Remove("fills/place-order/bad.go"); err != nil {
			t.Fatal(err)
		}
		mustRun(t, runDependencies)
		localtest.CheckSums(t, ".")
		if after, _ := moduleFiles(t); after == module {
			t.Fatal("resolution left go.mod as the first apply wrote it")
		}
		mustRun(t, runValidate)
	})
}

// runtimeProxy writes a module proxy folder that serves the archetype rest-crud's runtime pin from the shipped
// registry's runtime folder, as stego deps stages it, and gives its file URL.
func runtimeProxy(t *testing.T) string {
	t.Helper()
	reg, err := registry.LoadDirectories(shippedRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	pin := reg.Archetype("rest-crud").Runtime
	escaped, err := module.EscapePath(pin.Module)
	if err != nil {
		t.Fatal(err)
	}
	proxy := t.TempDir()
	versions := filepath.Join(proxy, filepath.FromSlash(escaped), "@v")
	if err := os.MkdirAll(versions, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.WriteRuntimeArchive(reg.RuntimeFolder(), pin.Module, pin.Version, filepath.Join(versions, pin.Version+".zip")); err != nil {
		t.Fatal(err)
	}
	modData, err := os.ReadFile(filepath.Join(reg.RuntimeFolder(), "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{pin.Version + ".mod": string(modData), pin.Version + ".info": fmt.Sprintf("{\"Version\":%q}\n", pin.Version), "list": pin.Version + "\n"} {
		if err := os.WriteFile(filepath.Join(versions, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return "file://" + filepath.ToSlash(proxy)
}
