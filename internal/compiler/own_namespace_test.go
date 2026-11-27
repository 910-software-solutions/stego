package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/registry"
	"github.com/910-software-solutions/stego/internal/types"
)

func TestComponentWritesOnlyItsOwnNamespace(t *testing.T) {
	t.Run("ST-SUP-INV02-T Every file a component plans is in its own namespace", func(t *testing.T) {
		input := snapshotTestInput(t)
		input.Generators["stub-store"] = &stubGenerator{files: []gen.File{{Path: "internal/storage/store.go", Content: []byte("package storage\n")}}}
		plan, err := Reconcile(input)
		if err != nil {
			t.Fatal(err)
		}
		owner := map[string]string{"internal/api/": "stub-api", "internal/storage/": "stub-store"}
		for _, file := range plan.Files {
			for prefix := range owner {
				if strings.HasPrefix(file.Path, prefix) {
					delete(owner, prefix)
				}
			}
		}
		if len(owner) != 0 {
			t.Fatalf("the plan holds no file of %v: %v", owner, plan.Files)
		}
		// bad-writer declares internal/bad and plans a file in the namespace of stub-store, a file in a folder that
		// only starts with the same letters, and a file outside the output.
		input = snapshotTestInput(t)
		dir := filepath.Join(input.RegistryDir, "components", "bad-writer")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "component.yaml"), "kind: component\nenvironment: []\narchitecture:\n  layer: handler\n  flow: Writes the test.\n  ports: {}\n"+
			"name: bad-writer\nversion: 1.0.0\nstage: ga\noutput_namespace: internal/bad\nrequires: []\nprovides: []\nslots: []\n")
		path := filepath.Join(input.RegistryDir, "archetypes", "test-arch", "archetype.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, strings.Replace(string(data), "  - stub-store\n", "  - stub-store\n  - bad-writer\n", 1))
		input.Generators["bad-writer"] = &stubGenerator{files: []gen.File{
			{Path: "internal/storage/extra.go", Content: []byte("package storage\n")},
			{Path: "internal/bad-extra/x.go", Content: []byte("package x\n")},
			{Path: "../outside.go", Content: []byte("package outside\n")},
			// The path starts with the namespace and leaves it again, so a check by prefix alone fails the test.
			{Path: "internal/bad/../storage/y.go", Content: []byte("package storage\n")},
			{Path: "internal/bad/own.go", Content: []byte("package bad\n")},
		}}
		plan, err = Reconcile(input)
		if err == nil || plan != nil {
			t.Fatalf("the plan of bad-writer was accepted: %v", plan)
		}
		for _, word := range []string{"bad-writer", "internal/storage/extra.go is in the namespace internal/storage of the component stub-store",
			"internal/bad-extra/x.go is outside internal/bad", "../outside.go is outside internal/bad", "internal/bad/../storage/y.go is outside internal/bad"} {
			if !strings.Contains(err.Error(), word) {
				t.Fatalf("the refusal does not name %q: %v", word, err)
			}
		}
		if strings.Contains(err.Error(), "internal/bad/own.go") {
			t.Fatalf("the refusal names the file inside the namespace: %v", err)
		}
		if _, err := os.Stat(filepath.Join(input.ProjectDir, "out")); !os.IsNotExist(err) {
			t.Fatalf("a refused plan wrote the output folder: %v", err)
		}
	})
}

func TestOneSlotNameHasOneContract(t *testing.T) {
	t.Run("ST-SUP-INV04-T A mixin that replaces a part of its archetype is refused, and a mixin that only adds is accepted", func(t *testing.T) {
		// Two components of one active set that offer the slot check, each with its own contract: the slot files are
		// refused, so the contract of a fill never depends on the order of a map (sec-500 M1).
		root := t.TempDir()
		files := map[string]string{"common/types.proto": "syntax = \"proto3\";\npackage common;\nmessage Result {\n  bool ok = 1;\n}\n"}
		for _, name := range []string{"first", "second"} {
			files["components/"+name+"/component.yaml"] = "kind: component\nenvironment: []\narchitecture:\n  layer: handler\n  flow: Serves the test.\n  ports: {}\nname: " + name +
				"\nversion: 1.0.0\nstage: ga\nslots:\n  - name: check\n    phase: before_store\n    operations: [create]\n    proto: " + name + ".Check\n"
			files["components/"+name+"/slots/check.proto"] = "syntax = \"proto3\";\npackage " + name + ";\nimport \"stego/common/types.proto\";\nservice Check {\n  rpc Evaluate(Request) returns (common.Result);\n}\nmessage Request {\n  string id = 1;\n}\n"
		}
		for name, data := range files {
			file := filepath.Join(root, name)
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, file, data)
		}
		reg, err := registry.Load(root)
		if err != nil {
			t.Fatal(err)
		}
		declaration := &types.ServiceDeclaration{Slots: []types.SlotDeclaration{{Slot: "check", Gate: []string{"policy"}}}}
		for range 10 {
			_, err := generateSlotFiles("slots", map[string]*registry.Component{"first": reg.Component("first"), "second": reg.Component("second")}, declaration, reg)
			if err == nil || !strings.Contains(err.Error(), `slot "check" is defined twice`) || !strings.Contains(err.Error(), "components/first") || !strings.Contains(err.Error(), "components/second") {
				t.Fatalf("two contracts of the slot check were not refused by name: %v", err)
			}
		}
		if _, err := generateSlotFiles("slots", map[string]*registry.Component{"first": reg.Component("first")}, declaration, reg); err != nil {
			t.Fatalf("one contract of the slot check was refused: %v", err)
		}
	})
}
