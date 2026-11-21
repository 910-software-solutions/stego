package compiler

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestRenamedPatchableField(t *testing.T) {
	t.Run("ST-GEN-UC12-R04-T03 A collection patches a renamed field by its API name, also after two renames", func(t *testing.T) {
		input := snapshotTestInput(t)
		// Widget.label is renamed to title, and title to heading; the collection names heading as patchable, by
		// its declared name (walkthrough 16, W16-N1).
		service := `kind: service
name: test-service
archetype: test-arch
language: go

entities:
  - name: Widget
    fields:
      - { name: heading, type: string }
      - { name: note, type: string }

collections:
  widgets:
    entity: Widget
    operations: [create, read, patch]
    patchable: [heading, note]

schema_changes:
  - {id: widget-label-rename, kind: rename, entity: Widget, field: title, from: label}
  - {id: widget-title-rename, kind: rename, entity: Widget, field: heading, from: title}
schema_history:
  - {change: widget-label-rename, phase: expand}
  - {change: widget-label-rename, phase: contract}
  - {change: widget-title-rename, phase: expand}
`
		writeFile(t, filepath.Join(input.ProjectDir, "service.yaml"), service)
		api := &capturingGenerator{stubGenerator: stubGenerator{files: []gen.File{{Path: "internal/api/handler.go", Content: []byte("package api\n")}}}}
		input.Generators["stub-api"] = api
		if validation, err := Validate(input); err != nil || validation.HasErrors() {
			t.Fatalf("checking refused the declaration: %v %v", FormatValidation(validation), err)
		}
		if _, err := Reconcile(input); err != nil {
			t.Fatalf("generation refused a rename of a patchable field: %v", err)
		}
		ctx := api.capturedCtx
		if len(ctx.Collections) != 1 || !slices.Equal(ctx.Collections[0].Patchable, []string{"label", "note"}) {
			t.Fatalf("the generator got the collections %+v; want widgets with the patchable fields label, the API name, and note", ctx.Collections)
		}
		if len(ctx.Entities) != 1 || ctx.Entities[0].Fields[0].Name != "label" || ctx.Entities[0].Fields[0].ColumnName() != "heading" {
			t.Fatalf("the generator got the entities %+v; want Widget.label in the column heading", ctx.Entities)
		}
		data, err := os.ReadFile(filepath.Join(input.ProjectDir, "service.yaml"))
		if err != nil || string(data) != service {
			t.Fatalf("generation changed the declaration: %v", err)
		}
	})
}
