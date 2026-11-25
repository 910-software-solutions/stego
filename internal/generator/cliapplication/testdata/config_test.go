package command

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAtomicConfigurationWrites(t *testing.T) {
	t.Run("ST-SUP-RC04-OC08-T01 A configuration write syncs the file before the rename and the directory after it", func(t *testing.T) {
		// The emitted config.go is in the package folder, where go test runs.
		set := token.NewFileSet()
		file, err := parser.ParseFile(set, "config.go", nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var body *ast.BlockStmt
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "saveConfiguration" {
				body = function.Body
			}
		}
		if body == nil {
			t.Fatal("the emitted config.go declares no saveConfiguration")
		}
		// The calls of the body in source order, each as receiver.Method with the text of its arguments.
		var calls []string
		ast.Inspect(body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
					receiver, _ := selector.X.(*ast.Ident)
					text := ""
					if receiver != nil {
						text = receiver.Name + "."
					}
					text += selector.Sel.Name + "("
					for _, argument := range call.Args {
						start, end := set.Position(argument.Pos()).Offset, set.Position(argument.End()).Offset
						source, _ := os.ReadFile("config.go")
						text += string(source[start:end]) + ","
					}
					calls = append(calls, text)
				}
			}
			return true
		})
		order := []string{"root.OpenFile(temporary,os.O_WRONLY|os.O_CREATE|os.O_EXCL", "file.Sync(", "root.Rename(temporary,name", "directory.Sync("}
		next := 0
		for _, call := range calls {
			if next < len(order) && strings.HasPrefix(strings.ReplaceAll(call, " ", ""), order[next]) {
				next++
			}
		}
		if next != len(order) {
			t.Fatalf("saveConfiguration does not call, in order, %v: it stops before %q; its calls are %v", order, order[next], calls)
		}
		if !strings.Contains(strings.Join(calls, " "), "rand.Text(") {
			t.Fatal("the temporary name is not random")
		}
	})

	t.Run("ST-SUP-RC04-OC08-T02 A failed configuration write leaves the earlier configuration unchanged", func(t *testing.T) {
		directory := privateDir(t)
		root, err := os.OpenRoot(directory)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		earlier := configuration{URL: "https://records.example.test", TokenFile: "/token"}
		if err := saveConfiguration(root, "config.json", earlier); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(filepath.Join(directory, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := saveConfiguration(root, "config.json", configuration{URL: "https://" + strings.Repeat("a", 70000), TokenFile: "/token"}); err == nil {
			t.Fatal("a configuration over 64 KiB was saved")
		}
		if after, _ := os.ReadFile(filepath.Join(directory, "config.json")); string(after) != string(before) {
			t.Fatal("the refused save changed the configuration")
		}
		// A folder at the name is refused before any temporary file is made, and keeps no temporary file.
		if err := os.Mkdir(filepath.Join(directory, "taken"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := saveConfiguration(root, "taken", earlier); err == nil {
			t.Fatal("a save onto a folder succeeded")
		}
		// A name in a folder that does not exist passes the check of the name, so the temporary file is written
		// and the rename fails: the temporary file is removed.
		if err := saveConfiguration(root, "missing/config.json", earlier); err == nil {
			t.Fatal("a save into a folder that does not exist succeeded")
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".stego-cli-") {
				t.Fatalf("the failed save left the temporary file %s", entry.Name())
			}
		}
	})

	t.Run("ST-SUP-RC04-OC08-T03 A reader never sees partial JSON while the CLI writes the configuration", func(t *testing.T) {
		directory := privateDir(t)
		root, err := os.OpenRoot(directory)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		saved := map[string]bool{}
		values := make([]configuration, 200)
		for i := range values {
			values[i] = configuration{URL: "https://records.example.test/" + strings.Repeat("x", i*97), TokenFile: "/token"}
			saved[values[i].URL] = true
		}
		if err := saveConfiguration(root, "config.json", values[0]); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(1)
		var fault error
		reads := 0
		go func() {
			defer wait.Done()
			reader, err := os.OpenRoot(directory)
			if err != nil {
				fault = err
				return
			}
			defer reader.Close()
			for {
				select {
				case <-done:
					return
				default:
				}
				cfg, err := loadConfiguration(reader, "config.json")
				if err != nil || !saved[cfg.URL] {
					fault = err
					if err == nil {
						fault = os.ErrInvalid
					}
					return
				}
				reads++
			}
		}()
		for _, value := range values {
			if err := saveConfiguration(root, "config.json", value); err != nil {
				t.Fatal(err)
			}
		}
		close(done)
		wait.Wait()
		if fault != nil {
			t.Fatalf("after %d complete reads, a read saw a configuration that was not saved whole: %v", reads, fault)
		}
		if reads == 0 {
			t.Fatal("the reader read no configuration while the saves ran, so the test shows nothing")
		}
	})
}
