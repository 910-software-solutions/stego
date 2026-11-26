package compiler

import (
	"slices"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/registry"
)

func TestDeclaredModes(t *testing.T) {
	t.Run("ST-GEN-INV26-T The assembler wires each declared mode as a named set in one binary that a repeatable mode flag selects: the declared modes of the glue", func(t *testing.T) {
		all := []string{"api", "worker", "controller"}
		for _, c := range []struct {
			stated, want []string
		}{
			{nil, all},
			{[]string{}, all},
			{[]string{"controller", "api", "worker"}, all},
			{[]string{"worker", "api"}, []string{"api", "worker"}},
		} {
			if got := declaredModes(c.stated); !slices.Equal(got, c.want) {
				t.Errorf("the modes %v gave %v, want %v", c.stated, got, c.want)
			}
		}
		source, err := renderRuntimeWiring(runtimeWiringData{Service: "s", ProblemCode: "s", Modes: []string{"api", "worker"}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(source), `var declaredModes = modes.Selection{"api", "worker"}`) {
			t.Fatalf("the glue does not hold the declared modes:\n%s", source)
		}
	})
}

func TestDeclaredModeVariable(t *testing.T) {
	t.Run("ST-GEN-INV26-T The assembler wires each declared mode as a named set in one binary that a repeatable mode flag selects: STEGO_MODE takes the declared modes", func(t *testing.T) {
		variables := []registry.EnvironmentVariable{
			{Name: "STEGO_MODE", Type: "list", Allowed: &registry.AllowedValues{Values: []string{"api", "worker", "controller", "all"}}},
			{Name: "STEGO_SERVICE_VERSION", Type: "string"},
		}
		narrowed := declaredModeVariables(variables, []string{"worker", "api"})
		if got := narrowed[0].Allowed.Values; !slices.Equal(got, []string{"api", "worker", "all"}) {
			t.Fatalf("STEGO_MODE allows %v, want api, worker and all", got)
		}
		if got := variables[0].Allowed.Values; len(got) != 4 {
			t.Fatalf("the registry's list changed to %v", got)
		}
		if narrowed[1].Allowed != nil {
			t.Fatalf("another variable changed: %+v", narrowed[1])
		}
		for _, value := range []string{"api", "api,worker", "all"} {
			if err := registry.CheckVariableValue(registry.VariableType(narrowed[0].Type), narrowed[0].Allowed, value); err != nil {
				t.Errorf("STEGO_MODE %q was refused: %v", value, err)
			}
		}
		if err := registry.CheckVariableValue(registry.VariableType(narrowed[0].Type), narrowed[0].Allowed, "api,controller"); err == nil {
			t.Error("STEGO_MODE api,controller was accepted, but the declaration does not state controller")
		}
		if got := declaredModeVariables(variables, nil)[0].Allowed.Values; !slices.Equal(got, []string{"api", "worker", "controller", "all"}) {
			t.Fatalf("with no declared modes STEGO_MODE allows %v, want all four", got)
		}
	})
}
