package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/compiler"
)

// This test proves that apply writes the architecture view architecture/index.html beside the model, rendered from
// the model alone, and that the record covers it (ST-GEN-UC08-R01, R07).
func TestArchitectureViewApply(t *testing.T) {
	t.Run("ST-GEN-UC08-R01-T03 Apply writes the architecture view beside the model, from the model alone, and the record lists it", func(t *testing.T) {
		registryDir := requirementsRegistry(t)
		dir := requirementsProject(t)
		mustRunInProject(t, dir, requirementsEnv(registryDir), "apply")
		model, _, data := readArchitecture(t, dir)
		view, err := os.ReadFile(filepath.Join(dir, compiler.ArchitectureViewFile))
		if err != nil {
			t.Fatalf("apply wrote no view: %v", err)
		}
		state, err := compiler.LoadState(filepath.Join(dir, ".stego", "state.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if state.LastApplied.ProjectFiles[compiler.ArchitectureViewFile] != compiler.HashBytes(view) {
			t.Fatalf("the record does not list %s with its hash", compiler.ArchitectureViewFile)
		}
		start := []byte(`<script type="application/json" id="model">`)
		i := bytes.Index(view, start)
		end := bytes.Index(view[max(i, 0):], []byte("</script>"))
		var embedded, written any
		if i < 0 || end < 0 || json.Unmarshal(view[i+len(start):i+end], &embedded) != nil || json.Unmarshal(data, &written) != nil || !reflect.DeepEqual(embedded, written) {
			t.Fatal("the view does not embed the written model as the same JSON value")
		}
		want, err := compiler.RenderArchitectureView(data)
		if err != nil || !bytes.Equal(want, view) {
			t.Fatalf("the view is not the one the model alone gives: %v", err)
		}
		for _, n := range model.Needs {
			if !strings.Contains(string(view), `data-id="`+n.ID+`"`) {
				t.Errorf("the view does not show the platform edge %s", n.ID)
			}
		}
		if len(model.Needs) == 0 {
			t.Fatal("the fixture states no need")
		}
		mustRunInProject(t, dir, requirementsEnv(registryDir), "apply")
		again, err := os.ReadFile(filepath.Join(dir, compiler.ArchitectureViewFile))
		if err != nil || !bytes.Equal(again, view) {
			t.Fatal("a second apply of the same inputs changed the view")
		}
	})
}
