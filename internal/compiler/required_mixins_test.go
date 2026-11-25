package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMixinRequiresMixin(t *testing.T) {
	t.Run("ST-SUP-INV16-T A declaration that binds a mixin without a mixin it requires is refused, naming both", func(t *testing.T) {
		// requiring gives a test input whose registry holds the mixins of ST-SUP-TD01 that this test names, each
		// with the mixins it requires, and whose declaration binds the mixins given.
		requiring := func(bound ...string) ReconcilerInput {
			input := snapshotTestInput(t)
			requires := map[string]string{"durable-events": "", "kafka-events": "durable-events", "chain-a": "", "chain-b": "chain-a", "chain-c": "chain-b"}
			for name, required := range requires {
				yaml := "kind: mixin\nname: " + name + "\nversion: 1.0.0\n"
				if required != "" {
					yaml += "requires_mixins: [" + required + "]\n"
				}
				yaml += "adds_components: []\nadds_slots: []\noverrides: none\n"
				if err := os.MkdirAll(filepath.Join(input.RegistryDir, "mixins", name), 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(input.RegistryDir, "mixins", name, "mixin.yaml"), yaml)
			}
			withMixins(t, input, bound...)
			return input
		}
		refused := func(words []string, bound ...string) {
			t.Helper()
			result, err := Validate(requiring(bound...))
			if err != nil {
				t.Fatal(err)
			}
			text := FormatValidation(result)
			if !result.HasErrors() {
				t.Fatalf("the declaration that binds %v passed checking", bound)
			}
			for _, word := range words {
				if !strings.Contains(text, `"`+word+`"`) {
					t.Fatalf("the refusal of %v does not name %s:\n%s", bound, word, text)
				}
			}
			if plan, err := Reconcile(requiring(bound...)); plan != nil || err == nil {
				t.Fatalf("the declaration that binds %v was planned", bound)
			}
		}
		passes := func(bound ...string) {
			t.Helper()
			result, err := Validate(requiring(bound...))
			if err != nil || result.HasErrors() {
				t.Fatalf("the declaration that binds %v was refused: %v\n%s", bound, err, FormatValidation(result))
			}
		}
		refused([]string{"kafka-events", "durable-events"}, "kafka-events")
		passes("kafka-events", "durable-events")
		passes("durable-events")
		// chain-c has chain-b, which it requires, and chain-b lacks chain-a: the refusal names the mixin whose
		// required mixin is missing, not the first mixin of the declaration.
		refused([]string{"chain-b", "chain-a"}, "chain-c", "chain-b")
	})
}
