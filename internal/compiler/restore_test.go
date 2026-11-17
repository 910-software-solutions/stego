package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestApplyRestoresMissingFiles(t *testing.T) {
	t.Run("ST-GEN-UC03-R03-T04 Apply writes a missing generated file again with no flag, and still refuses a changed one", func(t *testing.T) {
		projectDir, registryDir := setupTestProject(t)
		outDir := filepath.Join(projectDir, "out")
		generators := map[string]gen.Generator{
			"stub-api": &stubGenerator{
				files: []gen.File{
					{Path: "internal/api/handler.go", Content: []byte("package api\n")},
					{Path: "internal/api/routes.go", Content: []byte("package api\n\n// Routes.\n")},
				},
				wiring: &gen.Wiring{Imports: []string{"internal/api"}, Constructors: []string{"api.NewHandler()"}, Routes: []string{`mux.Handle("/widgets", handler)`}},
			},
			"stub-store": &stubGenerator{},
		}
		input := ReconcilerInput{ProjectDir: projectDir, RegistryDir: registryDir, Generators: generators, GoVersion: "1.22", ModuleName: "github.com/test/svc"}
		plan, err := Reconcile(input)
		if err != nil {
			t.Fatal(err)
		}
		if err := Apply(plan, projectDir, outDir); err != nil {
			t.Fatal(err)
		}
		handler := filepath.Join(outDir, "internal", "api", "handler.go")
		routes := filepath.Join(outDir, "internal", "api", "routes.go")
		want, err := os.ReadFile(handler)
		if err != nil {
			t.Fatal(err)
		}

		// The whole out folder is removed, as in a clone that does not hold it: apply writes it again with no flag.
		if err := os.RemoveAll(outDir); err != nil {
			t.Fatal(err)
		}
		plan, err = Reconcile(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.HandEdited) != 0 || !strings.Contains(FormatPlan(plan), "restore:  out/internal/api/handler.go") && !strings.Contains(FormatPlan(plan), "restore:  internal/api/handler.go") {
			t.Fatalf("want the missing files restored and no hand edit, got hand edits %v and the plan:\n%s", plan.HandEdited, FormatPlan(plan))
		}
		if err := Apply(plan, projectDir, outDir); err != nil {
			t.Fatalf("apply refused the missing files: %v", err)
		}
		if got, err := os.ReadFile(handler); err != nil || string(got) != string(want) {
			t.Fatalf("the restored file holds %q, %v; want %q", got, err, want)
		}

		// One file is edited by hand and another is removed: apply names only the edited one and writes nothing.
		if err := os.WriteFile(handler, []byte("package api\n\n// edited\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(routes); err != nil {
			t.Fatal(err)
		}
		plan, err = Reconcile(input)
		if err != nil {
			t.Fatal(err)
		}
		err = Apply(plan, projectDir, outDir)
		if err == nil || !strings.Contains(err.Error(), "1 hand-edited generated file(s)") || !strings.Contains(err.Error(), "handler.go") || strings.Contains(err.Error(), "routes.go") {
			t.Fatalf("want a refusal that names only handler.go, got %v", err)
		}
		if _, err := os.Stat(routes); !os.IsNotExist(err) {
			t.Fatalf("the refused apply wrote routes.go: %v", err)
		}
	})
}
