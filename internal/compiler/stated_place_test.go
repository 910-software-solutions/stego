package compiler

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/gen"
)

func TestComponentStatesItsPlace(t *testing.T) {
	t.Run("ST-SUP-INV09-T A component that leaves out its layer, its part in a request flow or the side of a port is refused", func(t *testing.T) {
		// placed gives a test input whose stub-api component.yaml is changed by the replacement given.
		placed := func(old, replacement string) ReconcilerInput {
			input := snapshotTestInput(t)
			path := filepath.Join(input.RegistryDir, "components", "stub-api", "component.yaml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if old != "" && !strings.Contains(string(data), old) {
				t.Fatalf("stub-api holds no %q", old)
			}
			writeFile(t, path, strings.Replace(string(data), old, replacement, 1))
			return input
		}
		for _, tc := range []struct{ name, old, replacement, statement string }{
			{"no layer", "  layer: handler\n", "", "architecture.layer"},
			{"no part in a request flow", "  flow: Serves the test.\n", "", "architecture.flow"},
			{"no side of storage-adapter", "    storage-adapter: outbound\n", "", "architecture.ports.storage-adapter"},
			{"the layer middleware", "  layer: handler\n", "  layer: middleware\n", "architecture.layer"},
		} {
			plan, err := Reconcile(placed(tc.old, tc.replacement))
			// The registry refuses the component when it is loaded, before any generator runs.
			if plan != nil || err == nil || !strings.HasPrefix(err.Error(), "loading registry: ") || !strings.Contains(err.Error(), "component stub-api") || !strings.Contains(err.Error(), tc.statement) {
				t.Fatalf("%s: stub-api was not refused by name and statement %s: %v", tc.name, tc.statement, err)
			}
		}
		model := func(input ReconcilerInput) ([]byte, ArchitectureModel) {
			t.Helper()
			plan, err := Reconcile(input)
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range plan.GeneratedFiles {
				if file.Path == "architecture/architecture.json" {
					var parsed ArchitectureModel
					if err := json.Unmarshal(file.Content, &parsed); err != nil {
						t.Fatal(err)
					}
					return file.Content, parsed
				}
			}
			t.Fatal("the plan holds no architecture model")
			return nil, ArchitectureModel{}
		}
		input := placed("", "")
		first, parsed := model(input)
		found := false
		for _, component := range parsed.Components {
			if component.Name != "stub-api" {
				continue
			}
			found = true
			sides := map[string]string{}
			for _, port := range component.Ports {
				sides[strings.TrimPrefix(port.Port, "port:")] = port.Side
			}
			if component.Layer != "handler" || component.FlowPart != "Serves the test." || sides["storage-adapter"] != "outbound" || sides["http-server"] != "inbound" {
				t.Fatalf("the model places stub-api at %q, %q, %v; want the stated layer, flow part and port sides", component.Layer, component.FlowPart, sides)
			}
		}
		if !found {
			t.Fatal("the model holds no stub-api")
		}
		// The generator of stub-api emits other code; the model is derived from the registry, so it does not move.
		input.Generators["stub-api"] = &stubGenerator{files: []gen.File{{Path: "internal/api/handler.go", Content: []byte("package api\n\nconst changed = true\n")}}}
		second, _ := model(input)
		if !bytes.Equal(first, second) {
			t.Fatal("a change to the emitted code of stub-api moved the architecture model")
		}
	})
}
