package declaration

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/types"
)

// todoFields gives ST-DECL-TD02 with the fields of Todo given, the modes given (none when empty), the schema
// changes and the schema history.
func todoFields(fields []string, modes string, changes []string, steps ...string) string {
	service := strings.Replace(td02, "      - {name: title, type: string, min_length: 1, max_length: 255}\n      - {name: done, type: bool, default: false}\n", "      - "+strings.Join(fields, "\n      - ")+"\n", 1)
	if modes != "" {
		service += "modes: " + modes + "\n"
	}
	if len(changes) > 0 {
		service += "schema_changes:\n  - " + strings.Join(changes, "\n  - ") + "\n"
	}
	if len(steps) > 0 {
		var items []string
		for _, step := range steps {
			change, phase, _ := strings.Cut(step, " ")
			items = append(items, fmt.Sprintf("{change: %s, phase: %s}", change, phase))
		}
		service += "schema_history:\n  - " + strings.Join(items, "\n  - ") + "\n"
	}
	return service
}

var (
	// renamed are the fields of Todo after the rename of title to summary.
	renamed = []string{"{name: summary, type: string, min_length: 1, max_length: 255}", "{name: done, type: bool, default: false}"}
	rename  = "{id: todo-title-rename, kind: rename, entity: Todo, field: summary, from: title}"
)

func schemaFaults(t *testing.T, service string, rules ...string) []ValidationError {
	t.Helper()
	return compositionFaults(t, service, Facts{}, rules...)
}

func TestSchemaChangeKind(t *testing.T) {
	t.Run("ST-DECL-INV57-T A rename with its expand step is accepted", func(t *testing.T) {
		wantNoFault(t, schemaFaults(t, todoFields(renamed, "", []string{rename}, "todo-title-rename expand")))
	})
	priority := append(append([]string(nil), renamed...), "{name: priority, type: int64}", "{name: owner, type: string}")
	for _, c := range []struct{ name, change, names string }{
		{"the kind move", "{id: todo-title-rename, kind: move, entity: Todo, field: summary, from: title}", `"move"`},
		{"the ID TodoRename", "{id: TodoRename, kind: rename, entity: Todo, field: summary, from: title}", `"TodoRename"`},
		{"a rename with no From", "{id: todo-title-rename, kind: rename, entity: Todo, field: summary}", "no from"},
		{"a rename with a Fill value", "{id: todo-title-rename, kind: rename, entity: Todo, field: summary, from: title, fill_value: x}", "fill_value"},
		{"a retype with no Inverse conversion", "{id: todo-title-rename, kind: retype, entity: Todo, field: priority, from: string, conversion: text_to_integer}", "no inverse_conversion"},
		{"a require with no Fill value", "{id: todo-title-rename, kind: require, entity: Todo, field: owner}", "no fill_value"},
		{"a drop with no Definition", "{id: todo-title-rename, kind: drop, entity: Todo, field: legacy}", "no definition"},
	} {
		t.Run("ST-DECL-INV57-T A schema change with "+c.name+" is refused", func(t *testing.T) {
			faults := schemaFaults(t, todoFields(priority, "", []string{c.change}, strings.SplitN(c.change, ",", 2)[0][5:]+" expand"), "schema-change-kind")
			wantFault(t, faults, c.names)
		})
	}
	t.Run("ST-DECL-INV57-T A second change with the ID todo-title-rename is refused", func(t *testing.T) {
		service := todoFields(renamed, "", []string{rename, "{id: todo-title-rename, kind: add, entity: Todo, field: done}"}, "todo-title-rename expand")
		wantFault(t, schemaFaults(t, service, "schema-change-kind"), `"todo-title-rename"`)
	})
}

func TestSchemaChangeEntity(t *testing.T) {
	titled := []string{"{name: summary, type: string}", "{name: title, type: string}", "{name: done, type: bool, default: false}"}
	t.Run("ST-DECL-INV58-T A rename that agrees with Todo is accepted", func(t *testing.T) {
		wantNoFault(t, schemaFaults(t, todoFields(renamed, "", []string{rename}, "todo-title-rename expand")))
	})
	for _, c := range []struct {
		name   string
		fields []string
		change string
		field  string
	}{
		{"the rename while Todo still declares title", titled, rename, "summary"},
		{"a rename to a field that Todo does not declare", renamed, "{id: todo-title-rename, kind: rename, entity: Todo, field: headline, from: title}", "headline"},
		{"a drop of done while Todo still declares done", renamed, "{id: todo-title-rename, kind: drop, entity: Todo, field: done, definition: bool}", "done"},
		{"a drop with no field of Todo", renamed, "{id: todo-title-rename, kind: drop, entity: Todo, definition: \"summary: string\"}", ""},
		{"a unique that names no declared invariant", renamed, "{id: todo-title-rename, kind: unique, entity: Todo, field: unique_summary}", "unique_summary"},
		{"a unique that names a declared invariant of kind no_overlap", renamed, "{id: todo-title-rename, kind: unique, entity: Todo, field: summary_window}", "summary_window"},
		{"a retype from the type that the field has now", renamed, "{id: todo-title-rename, kind: retype, entity: Todo, field: summary, from: string, conversion: text_to_text, inverse_conversion: text_to_text}", "summary"},
	} {
		t.Run("ST-DECL-INV58-T "+c.name+" is refused", func(t *testing.T) {
			service := todoFields(c.fields, "", []string{c.change}, "todo-title-rename expand") + "invariants:\n  - {name: summary_window, kind: no_overlap, on: Todo, range: 'starts_at, ends_at', by: [summary]}\n"
			faults := schemaFaults(t, service, "schema-change-entity")
			wantFault(t, faults, `"todo-title-rename"`, `"Todo"`, `"`+c.field+`"`)
		})
	}
	t.Run("ST-DECL-INV58-T A unique that names a declared invariant of kind unique is accepted", func(t *testing.T) {
		service := todoFields(renamed, "", []string{"{id: todo-unique-summary, kind: unique, entity: Todo, field: unique_summary}"}, "todo-unique-summary expand") +
			"invariants:\n  - {name: unique_summary, kind: unique, on: Todo, by: [summary]}\n"
		wantNoFault(t, schemaFaults(t, service))
	})
}

func TestSchemaSteps(t *testing.T) {
	note := "{id: note-add, kind: add, entity: Todo, field: note}"
	noted := append(append([]string(nil), renamed...), "{name: note, type: string, optional: true}")
	t.Run("ST-DECL-INV59-T The steps expand then contract are accepted", func(t *testing.T) {
		wantNoFault(t, schemaFaults(t, todoFields(renamed, "", []string{rename}, "todo-title-rename expand", "todo-title-rename contract")))
	})
	for _, c := range []struct {
		name    string
		fields  []string
		changes []string
		steps   []string
		names   string
	}{
		{"a contract step alone", renamed, []string{rename}, []string{"todo-title-rename contract"}, `"todo-title-rename"`},
		{"the steps contract then expand", renamed, []string{rename}, []string{"todo-title-rename contract", "todo-title-rename expand"}, `"todo-title-rename"`},
		{"two expand steps", renamed, []string{rename}, []string{"todo-title-rename expand", "todo-title-rename expand"}, `"todo-title-rename"`},
		{"two contract steps", renamed, []string{rename}, []string{"todo-title-rename expand", "todo-title-rename contract", "todo-title-rename contract"}, `"todo-title-rename"`},
		{"a step for the change todo-unknown", renamed, []string{rename}, []string{"todo-title-rename expand", "todo-unknown expand"}, `"todo-unknown"`},
		{"the change with no step", renamed, []string{rename}, nil, `"todo-title-rename"`},
		{"an add change with an expand and a contract step", noted, []string{note}, []string{"note-add expand", "note-add contract"}, `"note-add"`},
	} {
		t.Run("ST-DECL-INV59-T "+c.name+" are refused", func(t *testing.T) {
			wantFault(t, schemaFaults(t, todoFields(c.fields, "", c.changes, c.steps...), "schema-step"), c.names)
		})
	}
}

func TestSchemaChangeOpen(t *testing.T) {
	fields := []string{"{name: summary, type: int64}", "{name: done, type: bool, default: false}"}
	retype := "{id: summary-retype, kind: retype, entity: Todo, field: summary, from: string, conversion: text_to_integer, inverse_conversion: integer_to_text}"
	t.Run("ST-DECL-INV60-T A second open change on summary is refused", func(t *testing.T) {
		faults := schemaFaults(t, todoFields(fields, "", []string{rename, retype}, "todo-title-rename expand", "summary-retype expand"), "schema-change-open")
		wantFault(t, faults, `"todo-title-rename"`, `"summary-retype"`, "summary")
	})
	t.Run("ST-DECL-INV60-T A change after a finished one is accepted", func(t *testing.T) {
		service := todoFields(fields, "", []string{rename, retype}, "todo-title-rename expand", "todo-title-rename contract", "summary-retype expand")
		wantNoFault(t, schemaFaults(t, service))
	})
}

func TestSchemaChangeMode(t *testing.T) {
	noted := append(append([]string(nil), renamed...), "{name: note, type: string, optional: true}")
	t.Run("ST-DECL-INV61-T A rename at expand with the modes api is refused, and checking adds no mode", func(t *testing.T) {
		service := todoFields(renamed, "[api]", []string{rename}, "todo-title-rename expand")
		wantFault(t, schemaFaults(t, service, "schema-change-mode"), `"todo-title-rename"`, "controller")
		declared := readDeclaration(t, service)
		ValidateComposition(declared, Facts{})
		if strings.Join(declared.Modes, ",") != "api" {
			t.Errorf("checking changed the modes to %v", declared.Modes)
		}
	})
	for _, c := range []struct{ name, service string }{
		{"a rename at expand with no modes", todoFields(renamed, "", []string{rename}, "todo-title-rename expand")},
		{"a rename at contract with the modes api", todoFields(renamed, "[api]", []string{rename}, "todo-title-rename expand", "todo-title-rename contract")},
		{"only an add at expand with the modes api", todoFields(noted, "[api]", []string{"{id: note-add, kind: add, entity: Todo, field: note}"}, "note-add expand")},
	} {
		t.Run("ST-DECL-INV61-T "+c.name+" is accepted", func(t *testing.T) {
			wantNoFault(t, schemaFaults(t, c.service))
		})
	}
}

func TestSchemaChangeValues(t *testing.T) {
	with := func(owner, change string) string {
		fields := append(append([]string(nil), renamed...), owner)
		return todoFields(fields, "", []string{change}, strings.SplitN(change, ",", 2)[0][5:]+" expand")
	}
	required := "{name: owner, type: string, max_length: 20}"
	t.Run("ST-DECL-INV62-T A require of owner with the fill value nobody is accepted", func(t *testing.T) {
		wantNoFault(t, schemaFaults(t, with(required, "{id: owner-require, kind: require, entity: Todo, field: owner, fill_value: nobody}")))
	})
	for _, c := range []struct{ name, owner, change string }{
		{"a retype whose Definition gives an int32 the default 0 in its range", "{name: owner, type: int64}", "{id: points-retype, kind: retype, entity: Todo, field: owner, from: int32, conversion: integer_to_int64, inverse_conversion: int64_to_integer, definition: \"int32, min 0, max 100, default 0\"}"},
		{"a drop whose Definition gives a double the whole default 1", required, "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, definition: \"double, default 1, first\"}"},
		{"a drop whose Definition gives a string a quoted default", required, "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, definition: \"string, max_length 5, default \\\"ab\\\", first\"}"},
	} {
		t.Run("ST-DECL-INV62-T "+c.name+" is accepted", func(t *testing.T) {
			wantNoFault(t, schemaFaults(t, with(c.owner, c.change)))
		})
	}
	for _, c := range []struct{ name, owner, change, names string }{
		{"a require of a field with a default", "{name: owner, type: string, max_length: 20, default: x}", "{id: owner-require, kind: require, entity: Todo, field: owner, fill_value: nobody}", `"owner-require"`},
		{"a fill value longer than 20", required, "{id: owner-require, kind: require, entity: Todo, field: owner, fill_value: " + strings.Repeat("n", 21) + "}", "max_length 20"},
		{"a drop of legacy whose Definition is int64 with the fill value abc", required, "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, definition: \"int64, first\", fill_value: abc}", "abc"},
		{"a retype of owner whose Definition is not of its type From", "{name: owner, type: int64}", "{id: owner-retype, kind: retype, entity: Todo, field: owner, from: string, conversion: text_to_int64, inverse_conversion: int64_to_text, definition: int32}", "the retype is from string"},
		{"a retype of owner whose Definition cannot be read", "{name: owner, type: int64}", "{id: owner-retype, kind: retype, entity: Todo, field: owner, from: string, conversion: text_to_int64, inverse_conversion: int64_to_text, definition: \"string, colour red\"}", "cannot be read"},
		// The default of a Definition suits the field that the Definition gives (security review of PR #408).
		{"a retype whose Definition gives an int32 the default abc", "{name: owner, type: int64}", "{id: points-retype, kind: retype, entity: Todo, field: owner, from: int32, conversion: integer_to_int64, inverse_conversion: int64_to_integer, definition: \"int32, default abc\"}", "does not suit the type int32"},
		{"a retype whose Definition gives an int32 the default 1.5", "{name: owner, type: int64}", "{id: points-retype, kind: retype, entity: Todo, field: owner, from: int32, conversion: integer_to_int64, inverse_conversion: int64_to_integer, definition: \"int32, default 1.5\"}", "does not suit the type int32"},
		{"a retype whose Definition default is above its max", "{name: owner, type: int64}", "{id: points-retype, kind: retype, entity: Todo, field: owner, from: int32, conversion: integer_to_int64, inverse_conversion: int64_to_integer, definition: \"int32, min 0, max 100, default 200\"}", "default 200"},
		{"a retype whose Definition default is longer than its max_length", "{name: owner, type: int64}", "{id: owner-retype, kind: retype, entity: Todo, field: owner, from: string, conversion: text_to_int64, inverse_conversion: int64_to_text, definition: \"string, max_length 3, default abcd\"}", "longer than the max_length 3"},
		{"a retype whose Definition default does not match its pattern", "{name: owner, type: int64}", "{id: owner-retype, kind: retype, entity: Todo, field: owner, from: string, conversion: text_to_int64, inverse_conversion: int64_to_text, definition: \"string, default ABC, pattern ^[a-z]+$\"}", "does not match the pattern"},
		{"a drop whose Definition gives an int64 the default x", required, "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, definition: \"int64, default x, first\"}", "does not suit the type int64"},
	} {
		t.Run("ST-DECL-INV62-T "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, schemaFaults(t, with(c.owner, c.change), "schema-change-value"), c.names)
		})
	}
}

func TestSchemaChangeCount(t *testing.T) {
	many := func(contract bool) string {
		fields := append([]string(nil), renamed...)
		changes := []string{rename}
		steps := []string{"todo-title-rename expand"}
		if contract {
			steps = append(steps, "todo-title-rename contract")
		}
		for i := 1; i <= 127; i++ {
			fields = append(fields, fmt.Sprintf("{name: note_%d, type: string, optional: true}", i))
			changes = append(changes, fmt.Sprintf("{id: note-%d-add, kind: add, entity: Todo, field: note_%d}", i, i))
			steps = append(steps, fmt.Sprintf("note-%d-add expand", i))
		}
		return todoFields(fields, "", changes, steps...)
	}
	t.Run("ST-DECL-INV64-T 128 open schema changes are refused", func(t *testing.T) {
		wantFault(t, schemaFaults(t, many(false), "schema-change-count"), "128")
	})
	t.Run("ST-DECL-INV64-T 127 open schema changes, with the rename at contract, are accepted", func(t *testing.T) {
		wantNoFault(t, schemaFaults(t, many(true)))
	})
}

// The relax kind (decision on finding F12 of the schema-change research, owner, 2026-11-13).
func TestSchemaRelax(t *testing.T) {
	longer := []string{"{name: title, type: string, min_length: 1, max_length: 500}", "{name: done, type: bool, default: false}"}
	relax := "{id: todo-title-relax, kind: relax, entity: Todo, field: title, definition: max_length 255}"
	t.Run("ST-DECL-INV57-T A relax with its field and its definition is accepted", func(t *testing.T) {
		wantNoFault(t, schemaFaults(t, todoFields(longer, "", []string{relax}, "todo-title-relax expand")))
	})
	for _, c := range []struct{ name, change, names string }{
		{"a relax with no Definition", "{id: todo-title-relax, kind: relax, entity: Todo, field: title}", "no definition"},
		{"a relax with no Field", "{id: todo-title-relax, kind: relax, entity: Todo, definition: max_length 255}", "no field"},
		{"a relax with a From", "{id: todo-title-relax, kind: relax, entity: Todo, field: title, from: text, definition: max_length 255}", "from"},
		{"a relax with a Fill value", "{id: todo-title-relax, kind: relax, entity: Todo, field: title, fill_value: x, definition: max_length 255}", "fill_value"},
	} {
		t.Run("ST-DECL-INV57-T A schema change with "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, schemaFaults(t, todoFields(longer, "", []string{c.change}, "todo-title-relax expand"), "schema-change-kind"), c.names)
		})
	}
	t.Run("ST-DECL-INV58-T A relax of a field that the entity does not declare is refused", func(t *testing.T) {
		change := "{id: todo-title-relax, kind: relax, entity: Todo, field: heading, definition: max_length 255}"
		wantFault(t, schemaFaults(t, todoFields(longer, "", []string{change}, "todo-title-relax expand"), "schema-change-entity"), `"todo-title-relax"`)
	})
	t.Run("ST-DECL-INV59-T A relax with a contract step is refused", func(t *testing.T) {
		wantFault(t, schemaFaults(t, todoFields(longer, "", []string{relax}, "todo-title-relax expand", "todo-title-relax contract"), "schema-step"), "relax has an expand step only")
	})
	t.Run("ST-DECL-INV60-T Two relaxes of one field are accepted, and a relax after the expand step of an open change on the field is refused", func(t *testing.T) {
		longest := []string{"{name: title, type: string, min_length: 1, max_length: 900}", "{name: done, type: bool, default: false}"}
		again := "{id: todo-title-relax-2, kind: relax, entity: Todo, field: title, definition: max_length 500}"
		wantNoFault(t, schemaFaults(t, todoFields(longest, "", []string{relax, again}, "todo-title-relax expand", "todo-title-relax-2 expand")))
		renamedLonger := []string{"{name: summary, type: string, min_length: 1, max_length: 500}", "{name: done, type: bool, default: false}"}
		summary := "{id: todo-summary-relax, kind: relax, entity: Todo, field: summary, definition: max_length 255}"
		wantFault(t, schemaFaults(t, todoFields(renamedLonger, "", []string{rename, summary}, "todo-title-rename expand", "todo-summary-relax expand"), "schema-change-open"),
			`"todo-summary-relax"`, `"todo-title-rename"`)
		wantNoFault(t, schemaFaults(t, todoFields(renamedLonger, "", []string{rename, summary}, "todo-title-rename expand", "todo-title-rename contract", "todo-summary-relax expand")))
	})
	t.Run("ST-DECL-INV62-T Two relaxes of one field, the later one an enum made a string, are each checked against the field as it left them", func(t *testing.T) {
		values := "{id: todo-title-relax, kind: relax, entity: Todo, field: title, definition: values a b}"
		text := "{id: todo-title-relax-2, kind: relax, entity: Todo, field: title, definition: \"type enum, values a b c\"}"
		wantNoFault(t, schemaFaults(t, todoFields([]string{"{name: title, type: string}", "{name: done, type: bool, default: false}"}, "", []string{values, text}, "todo-title-relax expand", "todo-title-relax-2 expand")))
	})
	t.Run("ST-DECL-INV60-T A relax before the expand step of an open change on its field is accepted", func(t *testing.T) {
		retype := "{id: title-retype, kind: retype, entity: Todo, field: title, from: string, conversion: text_to_integer, inverse_conversion: integer_to_text}"
		service := todoFields([]string{"{name: title, type: int64}", "{name: done, type: bool, default: false}"}, "", []string{relax, retype}, "todo-title-relax expand", "title-retype expand")
		wantNoFault(t, schemaFaults(t, service, "schema-change-open"))
	})
	with := func(field, definition string) string {
		return todoFields([]string{field, "{name: done, type: bool, default: false}"}, "", []string{"{id: todo-title-relax, kind: relax, entity: Todo, field: title, definition: \"" + definition + "\"}"}, "todo-title-relax expand")
	}
	for _, c := range []struct{ name, field, definition string }{
		{"a longer max_length", "{name: title, type: string, max_length: 500}", "max_length 255"},
		{"a new enum value", "{name: title, type: enum, values: [a, b, c]}", "values a b"},
		{"a new default", "{name: title, type: string, default: x}", "no default"},
		{"a required field made optional", "{name: title, type: string, optional: true}", "required"},
		{"an enum made a string", "{name: title, type: string}", "type enum, values a b"},
		{"all at once", "{name: title, type: enum, values: [a, b, c], optional: true, default: a}", "values a b, required, no default"},
	} {
		t.Run("ST-DECL-INV62-T The relax of "+c.name+" is accepted", func(t *testing.T) {
			wantNoFault(t, schemaFaults(t, with(c.field, c.definition)))
		})
	}
	for _, c := range []struct{ name, field, definition, names string }{
		{"a shorter max_length", "{name: title, type: string, max_length: 100}", "max_length 255", "only makes a max_length longer"},
		{"a max_length taken away", "{name: title, type: string}", "max_length 255", "needs a retype"},
		{"an enum value taken away", "{name: title, type: enum, values: [a]}", "values a b", "only adds enum values"},
		{"the same enum values", "{name: title, type: enum, values: [a, b]}", "values a b", "relax nothing"},
		{"a field made required", "{name: title, type: string}", "required", "needs a require"},
		{"a default where the field has none", "{name: title, type: string}", "no default", "has no default now"},
		{"an enum made a string with a max_length", "{name: title, type: string, max_length: 9}", "type enum, values a b", "no max_length"},
		{"an enum made an integer", "{name: title, type: int64}", "type enum, values a b", "no max_length"},
		{"an unknown item", "{name: title, type: string, max_length: 500}", "max_length 255, pattern x", "not one a relax takes"},
		{"the same max_length", "{name: title, type: string, max_length: 255}", "max_length 255", "only makes a max_length longer"},
		{"an item twice", "{name: title, type: string, max_length: 500}", "max_length 255, max_length 200", "twice"},
	} {
		t.Run("ST-DECL-INV62-T The relax of "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, schemaFaults(t, with(c.field, c.definition), "schema-change-value"), c.names)
		})
	}
}

// Each schema change is checked against its field as its own step leaves it, with each later step undone (decision
// on ST-DECL-Q17 (a), orchestrator under the owner's delegation, 2026-11-14).
func TestSchemaStepShape(t *testing.T) {
	done := "{name: done, type: bool, default: false}"
	toInt := "{id: summary-retype, kind: retype, entity: Todo, field: summary, from: string, conversion: text_to_int64, inverse_conversion: int64_to_text, definition: \"string, pattern ^[0-9]{1,9}$\"}"
	toText := "{id: summary-retype-2, kind: retype, entity: Todo, field: summary, from: int64, conversion: int64_to_text, inverse_conversion: text_to_int64}"
	text := []string{"{name: summary, type: string, pattern: '^-?[0-9]{1,18}$'}", done}
	t.Run("ST-DECL-INV58-T Two retypes of one field, string to int64 and then int64 to string, are each checked at their own step and accepted", func(t *testing.T) {
		service := todoFields(text, "", []string{toInt, toText}, "summary-retype expand", "summary-retype contract", "summary-retype-2 expand")
		wantNoFault(t, schemaFaults(t, service))
	})
	t.Run("ST-DECL-INV58-T A middle retype from the type that the field has after its own step is refused, and the fault names the step and the shape", func(t *testing.T) {
		middle := "{id: summary-retype-2, kind: retype, entity: Todo, field: summary, from: int64, conversion: integer_to_int64, inverse_conversion: int64_to_integer}"
		last := "{id: summary-retype-3, kind: retype, entity: Todo, field: summary, from: int64, conversion: int64_to_text, inverse_conversion: text_to_int64}"
		service := todoFields(text, "", []string{toInt, middle, last},
			"summary-retype expand", "summary-retype contract", "summary-retype-2 expand", "summary-retype-2 contract", "summary-retype-3 expand")
		wantFault(t, schemaFaults(t, service, "schema-change-entity"), `"summary-retype-2"`, "the retype is from int64", "after its step schema_history[2] (Todo.summary: int64, with each later step undone)")
	})
	t.Run("ST-DECL-INV58-T A retype and then a require of the same field are refused, as the require adds the field after the retype's step", func(t *testing.T) {
		retype := "{id: owner-retype, kind: retype, entity: Todo, field: owner, from: int64, conversion: int64_to_text, inverse_conversion: text_to_int64}"
		require := "{id: owner-require, kind: require, entity: Todo, field: owner, fill_value: x}"
		service := todoFields(append(append([]string(nil), renamed...), "{name: owner, type: string}"), "", []string{retype, require},
			"owner-retype expand", "owner-retype contract", "owner-require expand")
		wantFault(t, schemaFaults(t, service, "schema-change-entity"), `"owner-retype"`, "does not declare the field", "after its step schema_history[0]")
	})
	t.Run("ST-DECL-INV62-T A require and then a retype of the same field: the fill value suits the field at the require's step", func(t *testing.T) {
		require := "{id: owner-require, kind: require, entity: Todo, field: owner, fill_value: nobody}"
		retype := "{id: owner-retype, kind: retype, entity: Todo, field: owner, from: string, conversion: text_to_int64, inverse_conversion: int64_to_text, fill_value: 0}"
		fields := append(append([]string(nil), renamed...), "{name: owner, type: int64}")
		service := todoFields(fields, "", []string{require, retype}, "owner-require expand", "owner-require contract", "owner-retype expand")
		wantNoFault(t, schemaFaults(t, service))
		wrong := strings.Replace(service, "fill_value: nobody", "fill_value: 7", 1)
		wantFault(t, schemaFaults(t, wrong, "schema-change-value"), `"owner-require"`, "does not suit the type string", "after the step schema_history[0] is Todo.owner: string")
	})
	t.Run("ST-DECL-INV62-T A relax and then a retype of the same field: the relax is checked against the Definition of the retype", func(t *testing.T) {
		relax := "{id: todo-title-relax, kind: relax, entity: Todo, field: title, definition: max_length 255}"
		retype := func(definition string) string {
			return "{id: title-retype, kind: retype, entity: Todo, field: title, from: string, conversion: text_to_int64, inverse_conversion: int64_to_text, fill_value: 0" + definition + "}"
		}
		fields := []string{"{name: title, type: int64}", done}
		wantNoFault(t, schemaFaults(t, todoFields(fields, "", []string{relax, retype(", definition: \"string, max_length 500\"")}, "todo-title-relax expand", "title-retype expand")))
		wantFault(t, schemaFaults(t, todoFields(fields, "", []string{relax, retype("")}, "todo-title-relax expand", "title-retype expand"), "schema-change-value"),
			`"todo-title-relax"`, "needs a retype", "after the step schema_history[0] is Todo.title: string,")
	})
	t.Run("ST-DECL-INV58-T A relax and then a rename of the same field: the relax names the field by its name at its own step", func(t *testing.T) {
		relax := "{id: todo-title-relax, kind: relax, entity: Todo, field: title, definition: max_length 255}"
		fields := []string{"{name: summary, type: string, min_length: 1, max_length: 500}", done}
		wantNoFault(t, schemaFaults(t, todoFields(fields, "", []string{relax, rename}, "todo-title-relax expand", "todo-title-rename expand")))
	})
	t.Run("ST-DECL-INV62-T Two relaxes, the later one not a relax of the field that the earlier one leaves, refuse the later one only", func(t *testing.T) {
		first := "{id: todo-title-relax, kind: relax, entity: Todo, field: title, definition: max_length 255}"
		second := "{id: todo-title-relax-2, kind: relax, entity: Todo, field: title, definition: max_length 600}"
		fields := []string{"{name: title, type: string, max_length: 500}", done}
		wantFault(t, schemaFaults(t, todoFields(fields, "", []string{first, second}, "todo-title-relax expand", "todo-title-relax-2 expand"), "schema-change-value"),
			`"todo-title-relax-2"`, "only makes a max_length longer")
	})
	t.Run("ST-DECL-INV60-T An add finishes at its expand step, so a later rename of the field that it adds is accepted", func(t *testing.T) {
		add := "{id: note-add, kind: add, entity: Todo, field: note}"
		memo := "{id: note-rename, kind: rename, entity: Todo, field: memo, from: note}"
		fields := append(append([]string(nil), renamed...), "{name: memo, type: string, optional: true}")
		wantNoFault(t, schemaFaults(t, todoFields(fields, "", []string{add, memo}, "note-add expand", "note-rename expand")))
	})
}

// A later rename, retype or relax with no field cannot be undone; checking refuses it by its own check and does not
// stop (security review of PR #372, M1).
func TestSchemaStepShapeNoField(t *testing.T) {
	fields := append(append([]string(nil), renamed...), "{name: owner, type: string, optional: true}")
	for _, later := range []string{
		"{id: b, kind: retype, entity: Todo, from: int64, conversion: text_to_int64, inverse_conversion: int64_to_text}",
		"{id: b, kind: relax, entity: Todo, definition: max_length 3}",
		"{id: b, kind: rename, entity: Todo, from: zz}",
	} {
		t.Run("ST-DECL-INV58-T An add and then a "+strings.Fields(later)[3]+" with no field: the later change is refused for its missing field", func(t *testing.T) {
			service := todoFields(fields, "", []string{"{id: a, kind: add, entity: Todo, field: owner}", later}, "a expand", "b expand")
			wantFault(t, schemaFaults(t, service, "schema-change-kind"), `"b"`, "no field")
			for _, fault := range schemaFaults(t, service, "schema-change-entity") {
				if strings.Contains(fault.Message, `"a"`) {
					t.Errorf("checking refuses the add, whose shape is not known: %v", fault)
				}
			}
		})
	}
	t.Run("ST-DECL-INV63-T A retype and then a relax with no field: the conversion check skips the retype and does not stop", func(t *testing.T) {
		service := priorityRetype(types.FieldTypeInt32, "", "text_to_integer", "integer_to_text", "", 0)
		service.SchemaChanges = append(service.SchemaChanges, types.SchemaChange{ID: "b", Kind: "relax", Entity: "Todo", Definition: "max_length 3"})
		service.SchemaHistory = append(service.SchemaHistory, types.SchemaStep{Change: "b", Phase: "expand"})
		if faults := ValidateSchemaConversions(service, nil, true); len(faults) != 0 {
			t.Errorf("checking gives %v, want none", faults)
		}
		if _, found, reason := FieldAtStep(service, service.SchemaChanges[0]); found || !strings.Contains(reason, "names no field") {
			t.Errorf("FieldAtStep gives found %v and %q, want the later relax named as one that cannot be undone", found, reason)
		}
	})
}

// An add of the field that an open rename renames from, after the rename's expand step, is refused: the old column
// still holds the field (security review of PR #372, L1).
func TestSchemaAddAfterOpenRename(t *testing.T) {
	fields := append(append([]string(nil), renamed...), "{name: title, type: string, optional: true}")
	add := "{id: title-add, kind: add, entity: Todo, field: title}"
	t.Run("ST-DECL-INV60-T An add of title after the expand step of the open rename of title to summary is refused", func(t *testing.T) {
		service := todoFields(fields, "", []string{rename, add}, "todo-title-rename expand", "title-add expand")
		wantFault(t, schemaFaults(t, service, "schema-change-open"), `"title-add"`, `"todo-title-rename"`)
	})
	t.Run("ST-DECL-INV60-T The same add after the contract step of the rename is accepted", func(t *testing.T) {
		service := todoFields(fields, "", []string{rename, add}, "todo-title-rename expand", "todo-title-rename contract", "title-add expand")
		wantNoFault(t, schemaFaults(t, service, "schema-change-open"))
	})
}

// Short random schema histories never stop checking (security review of PR #372, M1). The seed is fixed, so each run
// checks the same histories, and the test counts histories, not time.
func TestSchemaStepShapeRandom(t *testing.T) {
	t.Run("ST-DECL-INV58-T 3000 random short schema histories are checked with no panic", func(t *testing.T) {
		random := rand.New(rand.NewSource(372))
		kinds := []string{"add", "rename", "retype", "require", "drop", "unique", "relax", "move"}
		names := []string{"", "title", "summary", "owner", "zz"}
		values := []string{"", "string", "int64", "max_length 3", "required", "no default", "values a b", "string, max_length 500", "x: string"}
		pick := func(list []string) string { return list[random.Intn(len(list))] }
		conversions := map[string]types.Conversion{"text_to_int64": {Name: "text_to_int64", From: types.FieldTypeString, To: types.FieldTypeInt64}, "int64_to_text": {Name: "int64_to_text", From: types.FieldTypeInt64, To: types.FieldTypeString, Total: true}}
		for n := 0; n < 3000; n++ {
			service := &types.ServiceDeclaration{Entities: []types.Entity{{Name: "Todo", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString}, {Name: "owner", Type: types.FieldTypeInt64, Optional: true}}}}}
			for i := 0; i < 1+random.Intn(5); i++ {
				change := types.SchemaChange{ID: fmt.Sprintf("c%d", i), Kind: pick(kinds), Entity: "Todo", Field: pick(names), From: pick(append(names, "string", "int64")),
					Conversion: pick([]string{"", "text_to_int64", "int64_to_text"}), InverseConversion: pick([]string{"", "text_to_int64", "int64_to_text"}), Definition: pick(values)}
				if random.Intn(3) == 0 {
					change.FillValue = pick([]string{"x", "7"})
				}
				service.SchemaChanges = append(service.SchemaChanges, change)
				for _, phase := range []string{"expand", "contract"}[:1+random.Intn(2)] {
					service.SchemaHistory = append(service.SchemaHistory, types.SchemaStep{Change: change.ID, Phase: phase})
				}
			}
			random.Shuffle(len(service.SchemaHistory), func(i, j int) {
				service.SchemaHistory[i], service.SchemaHistory[j] = service.SchemaHistory[j], service.SchemaHistory[i]
			})
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("checking stops on %+v %+v: %v", service.SchemaChanges, service.SchemaHistory, r)
					}
				}()
				validateSchemaChangeEntity(service)
				validateSchemaChangeOpen(service)
				validateSchemaChangeValues(service)
				ValidateSchemaConversions(service, conversions, true)
				for _, change := range service.SchemaChanges {
					FieldAtStep(service, change)
				}
			}()
		}
	})
}

// withTag gives ST-DECL-TD02 with the schema changes and steps given and, unless tag is empty, the entity Tag of
// tag, such as {name: Tag, dropped_by: tag-drop, fields: [{name: label, type: string}]}, with its collection tags.
func withTag(tag string, changes []string, steps ...string) string {
	service := todoFields([]string{"{name: title, type: string, min_length: 1, max_length: 255}", "{name: done, type: bool, default: false}"}, "", changes, steps...)
	if tag == "" {
		return service
	}
	service = strings.Replace(service, "entities:\n", "entities:\n  - "+tag+"\n", 1)
	return strings.Replace(service, "collections:\n", "collections:\n  tags:\n    entity: Tag\n    operations: [create, list]\n", 1)
}

func TestSchemaChangeDrop(t *testing.T) {
	dropped := "{name: Tag, dropped_by: tag-drop, fields: [{name: label, type: string}]}"
	entityDrop := "{id: tag-drop, kind: drop, entity: Tag}"
	t.Run("ST-DECL-INV58-T A drop of the entity Tag, which stays declared with dropped_by, is accepted", func(t *testing.T) {
		wantNoFault(t, schemaFaults(t, withTag(dropped, []string{entityDrop}, "tag-drop expand")))
		read, _, err := parser.ReadServiceDeclaration([]byte(withTag(dropped, []string{entityDrop}, "tag-drop expand")), "service.yaml")
		if err != nil {
			t.Fatal(err)
		}
		if len(read.Entities) != 1 || len(read.DroppedEntities) != 1 || len(read.Collections) != 1 || len(read.DroppedCollections) != 1 {
			t.Errorf("the declaration keeps %d live and %d dropped entities, %d live and %d dropped collections; want 1 of each",
				len(read.Entities), len(read.DroppedEntities), len(read.Collections), len(read.DroppedCollections))
		}
	})
	for _, c := range []struct{ name, tag, change, names string }{
		{"a drop of Tag that Tag declares live", "{name: Tag, fields: [{name: label, type: string}]}", entityDrop, "add dropped_by: tag-drop"},
		{"a drop of Tag that the declaration does not declare", "", entityDrop, "declares no entity Tag with dropped_by: tag-drop"},
		{"a dropped Tag that names another change", "{name: Tag, dropped_by: other-drop, fields: [{name: label, type: string}]}", entityDrop, "other-drop"},
	} {
		t.Run("ST-DECL-INV58-T "+c.name+" is refused", func(t *testing.T) {
			service := withTag(c.tag, []string{c.change}, "tag-drop expand")
			faults := schemaFaults(t, service, "schema-change-entity")
			if !slices.ContainsFunc(faults, func(f ValidationError) bool { return strings.Contains(f.Message, c.names) }) {
				t.Errorf("no fault names %q: %+v", c.names, faults)
			}
		})
	}
	t.Run("ST-DECL-INV58-T A dropped Tag whose dropped_by names no schema change is refused", func(t *testing.T) {
		wantFault(t, schemaFaults(t, withTag(dropped, nil), "schema-change-entity"), "tag-drop", "declares no such change")
	})
	for _, c := range []struct{ name, change, names string }{
		{"a Definition", "{id: tag-drop, kind: drop, entity: Tag, definition: \"label: string\"}", "takes no definition"},
		{"a fill value", "{id: tag-drop, kind: drop, entity: Tag, fill_value: x}", "takes no fill_value"},
	} {
		t.Run("ST-DECL-INV57-T A drop of the whole entity Tag with "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, schemaFaults(t, withTag(dropped, []string{c.change}, "tag-drop expand"), "schema-change-kind"), c.names)
		})
	}
	for _, name := range []string{"id", "created_time", "updated_time", "version", "authz_token"} {
		t.Run("ST-DECL-INV58-T A drop of the column "+name+" that STEGO adds itself is refused", func(t *testing.T) {
			change := "{id: legacy-drop, kind: drop, entity: Todo, field: " + name + ", definition: \"string, first\"}"
			wantFault(t, schemaFaults(t, todoFields([]string{"{name: title, type: string, min_length: 1, max_length: 255}", "{name: done, type: bool, default: false}"}, "", []string{change}, "legacy-drop expand"), "schema-change-entity"), "adds to each table itself")
		})
	}
	with := func(change string) string {
		return todoFields([]string{"{name: title, type: string, min_length: 1, max_length: 255}", "{name: done, type: bool, default: false}"}, "",
			[]string{change}, "legacy-drop expand")
	}
	for _, c := range []struct{ name, change string }{
		{"a required string with min_length 1 and a fill value", "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, fill_value: x, definition: \"string, min_length 1, after title\"}"},
		{"an optional enum with no fill value", "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, definition: \"enum, values a b, optional, after done\"}"},
		{"a required int32 at the first place", "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, definition: \"int32, min 0, unique, pii, first\"}"},
	} {
		t.Run("ST-DECL-INV62-T A drop of "+c.name+" is accepted", func(t *testing.T) {
			wantNoFault(t, schemaFaults(t, with(c.change)))
		})
	}
	for _, c := range []struct{ name, change, names string }{
		{"a required string with min_length 1 and no fill value", "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, definition: \"string, min_length 1, after title\"}", "shorter than the min_length 1"},
		{"a required enum with no fill value", "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, definition: \"enum, values a b, after title\"}", "give the drop a fill_value"},
		{"a required timestamp with no fill value", "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, definition: \"timestamp, after title\"}", "has no zero value"},
		{"a Definition with no place", "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, definition: \"string\"}", "gives no place"},
		{"a place after a field that Todo does not have", "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, definition: \"string, after colour\"}", "after colour"},
		{"an item twice", "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, definition: \"string, optional, optional, first\"}", "optional twice"},
		{"pii and secret both", "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, definition: \"string, pii, secret, first\"}", "sensitivity twice"},
		{"values of a string", "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, definition: \"string, values a b, first\"}", "only for an enum"},
		{"a double with the fill value NaN", "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, fill_value: .nan, definition: \"double, first\"}", "fill value NaN does not suit"},
		{"a double with the fill value Inf", "{id: legacy-drop, kind: drop, entity: Todo, field: legacy, fill_value: .inf, definition: \"double, first\"}", "fill value +Inf does not suit"},
	} {
		t.Run("ST-DECL-INV62-T A drop of "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, schemaFaults(t, with(c.change), "schema-change-value"), c.names)
		})
	}
}

func TestSchemaSeal(t *testing.T) {
	secret := []string{"{name: title, type: string, min_length: 1, max_length: 255, sensitivity: secret}", "{name: done, type: bool, default: false}"}
	seal := "{id: todo-title-seal, kind: seal, entity: Todo, field: title, from: pii}"
	rules := []string{"schema-change-kind", "schema-change-entity", "schema-change-seal", "schema-change-open", "schema-step"}
	t.Run("ST-DECL-INV102-T A seal of a readable secret is accepted at expand and at contract, from pii, public, secret or none", func(t *testing.T) {
		for _, change := range []string{seal, "{id: todo-title-seal, kind: seal, entity: Todo, field: title, from: public}", "{id: todo-title-seal, kind: seal, entity: Todo, field: title}",
			"{id: todo-title-seal, kind: seal, entity: Todo, field: title, from: secret}"} {
			wantNoFault(t, schemaFaults(t, todoFields(secret, "", []string{change}, "todo-title-seal expand"), rules...))
		}
		wantNoFault(t, schemaFaults(t, todoFields(secret, "", []string{seal}, "todo-title-seal expand", "todo-title-seal contract"), rules...))
		// An add of the field and a relax of it before the seal finish at their expand steps (ST-DECL-INV102).
		note := []string{"{name: title, type: string, min_length: 1, max_length: 255}", "{name: done, type: bool, default: false}", "{name: note, type: string, optional: true, max_length: 300, sensitivity: secret}"}
		wantNoFault(t, schemaFaults(t, todoFields(note, "", []string{"{id: add-todo-note, kind: add, entity: Todo, field: note}",
			"{id: todo-note-relax, kind: relax, entity: Todo, field: note, definition: max_length 200}", "{id: todo-note-seal, kind: seal, entity: Todo, field: note}"},
			"add-todo-note expand", "todo-note-relax expand", "todo-note-seal expand"), rules...))
	})
	for _, c := range []struct {
		name   string
		fields []string
		change []string
		steps  []string
		names  []string
	}{
		{"of a field that is not secret", []string{"{name: title, type: string, min_length: 1, max_length: 255, sensitivity: pii}", "{name: done, type: bool, default: false}"},
			[]string{seal}, []string{"todo-title-seal expand"}, []string{"todo-title-seal", "readable secret"}},
		{"of a compare-only secret", []string{"{name: title, type: string, min_length: 1, max_length: 255, sensitivity: secret, compare_only: true}", "{name: done, type: bool, default: false}"},
			[]string{seal}, []string{"todo-title-seal expand"}, []string{"todo-title-seal", "readable secret"}},
		{"of a field that the entity does not declare", secret, []string{"{id: todo-title-seal, kind: seal, entity: Todo, field: note}"},
			[]string{"todo-title-seal expand"}, []string{"todo-title-seal", "does not declare"}},
		{"from the sensitivity confidential", secret, []string{"{id: todo-title-seal, kind: seal, entity: Todo, field: title, from: confidential}"},
			[]string{"todo-title-seal expand"}, []string{"todo-title-seal", "public, pii or secret", "ST-DECL-INV102"}},
		{"with a definition", secret, []string{"{id: todo-title-seal, kind: seal, entity: Todo, field: title, definition: string}"},
			[]string{"todo-title-seal expand"}, []string{"todo-title-seal", "definition"}},
		{"twice of one field", secret, []string{seal, "{id: todo-title-seal-again, kind: seal, entity: Todo, field: title}"},
			[]string{"todo-title-seal expand", "todo-title-seal contract", "todo-title-seal-again expand"}, []string{"todo-title-seal", "todo-title-seal-again", "at most one seal"}},
		{"before a relax of the field", secret, []string{seal, "{id: todo-title-relax, kind: relax, entity: Todo, field: title, definition: max_length 100}"},
			[]string{"todo-title-seal expand", "todo-title-relax expand"}, []string{"todo-title-relax", "todo-title-seal", "only an add or a relax before the seal"}},
	} {
		t.Run("ST-DECL-INV102-T A seal "+c.name+" is refused", func(t *testing.T) {
			faults := schemaFaults(t, todoFields(c.fields, "", c.change, c.steps...), rules...)
			if len(faults) == 0 {
				t.Fatal("want a fault, got none")
			}
			text := ""
			for _, fault := range faults {
				text += fault.Message + "\n"
			}
			for _, part := range c.names {
				if !strings.Contains(text, part) {
					t.Errorf("the faults do not name %s:\n%s", part, text)
				}
			}
		})
	}
	t.Run("ST-DECL-INV102-T A seal at expand needs the controller mode for its seal job", func(t *testing.T) {
		wantFault(t, schemaFaults(t, todoFields(secret, "[api]", []string{seal}, "todo-title-seal expand"), "schema-change-mode"), "todo-title-seal", "controller")
	})
}
