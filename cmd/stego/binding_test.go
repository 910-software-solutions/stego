package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// bindingFaults reads one generated Go file with go/ast and gives each construct by which the service would decide
// at startup or per request what runs (ST-GEN-INV14): a func init, an import of plugin, a read of the declaration
// file or of the binary's own file or build information, and a look at struct tags.
func bindingFaults(t *testing.T, name string, data []byte) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, data, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	var faults []string
	for _, spec := range file.Imports {
		if path, _ := strconv.Unquote(spec.Path.Value); path == "plugin" {
			faults = append(faults, "imports plugin")
		}
	}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if strings.HasPrefix(comment.Text, "//go:embed") && strings.Contains(comment.Text, ".yaml") {
				faults = append(faults, "embeds "+comment.Text)
			}
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncDecl:
			if n.Recv == nil && n.Name.Name == "init" {
				faults = append(faults, "declares func init")
			}
		case *ast.BasicLit:
			if n.Kind == token.STRING && strings.Contains(n.Value, "service.yaml") {
				faults = append(faults, "names service.yaml")
			}
		case *ast.SelectorExpr:
			if x, ok := n.X.(*ast.Ident); ok {
				switch x.Name + "." + n.Sel.Name {
				case "os.Executable", "debug.ReadBuildInfo", "reflect.StructTag":
					faults = append(faults, "calls "+x.Name+"."+n.Sel.Name)
				}
			}
			if inner, ok := n.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Tag" && (n.Sel.Name == "Get" || n.Sel.Name == "Lookup") {
				faults = append(faults, "reads a struct tag")
			}
		}
		return true
	})
	return faults
}

// generatedGo reads every Go file that apply wrote into the output folder of the project, by its path in it.
func generatedGo(t *testing.T, project string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	out := filepath.Join(project, "out")
	err := filepath.WalkDir(out, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(out, path)
		files[relative] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestServiceBindsItsDeclarationAtBuildTime(t *testing.T) {
	if testing.Short() {
		t.Skip("applies a project, builds the generated service and runs it")
	}
	t.Run("ST-GEN-INV14-T A generated service reads no declaration, scans nothing and registers nothing by name, tag or init", func(t *testing.T) {
		// ST-DECL-TD03 on the runtime data path, with two modes: a pooled service with authorization, tenancy, the
		// command and query components and the postgres-adapter.
		service := edit(t, td03Service(t), "base_path:", "modes: [api, worker]\nbase_path:")
		project := td03Project(t, service)
		if apply := stegoIn(t, project, nil, "apply"); apply.code != 0 {
			t.Fatalf("stego apply refused the declaration, exit %d:\n%s\n%s", apply.code, apply.stdout, apply.stderr)
		}
		files := generatedGo(t, project)
		if len(files) == 0 || files["main.go"] == nil {
			t.Fatalf("apply wrote no main.go: %d Go files", len(files))
		}
		for name, data := range files {
			for _, fault := range bindingFaults(t, name, data) {
				t.Errorf("the generated file out/%s %s", name, fault)
			}
		}
		// The construction in main.go is fixed when the code is generated: a second apply of the same declaration
		// gives the same bytes.
		first := string(files["main.go"])
		if again := stegoIn(t, project, nil, "apply"); again.code != 0 {
			t.Fatalf("the second stego apply failed, exit %d:\n%s\n%s", again.code, again.stdout, again.stderr)
		}
		if second := string(generatedGo(t, project)["main.go"]); second != first {
			t.Error("a second apply of the same declaration wrote another main.go")
		}

		// The generated service builds into one binary from one main package.
		binary := filepath.Join(t.TempDir(), "service")
		buildWithRuntime(t, project, "-o", binary, "./out")
		if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
			t.Errorf("the build wrote no binary: %v", err)
		}
	})
}
