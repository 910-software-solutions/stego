package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// emitsNoCode are the components that the registry ships and that emit no code into a service, each with the reason.
var emitsNoCode = map[string]string{
	"ci-pipeline": "it states the pins of each CI provider, and delivery.WritePipeline writes the CI file from them (ST-SUP-RC25-G01, ST-DLV-CMD02-H)",
}

func TestCompilerContainsEveryGenerator(t *testing.T) {
	t.Run("ST-STD-TRUST-R01-T01 The compiler contains a generator for every component the registry ships", func(t *testing.T) {
		entries, err := os.ReadDir("../../registry/components")
		if err != nil {
			t.Fatal(err)
		}
		generators := defaultGenerators()
		var components []string
		for _, entry := range entries {
			if entry.IsDir() {
				components = append(components, entry.Name())
			}
		}
		if len(components) == 0 {
			t.Fatal("the registry ships no components")
		}
		for _, name := range components {
			if reason, ok := emitsNoCode[name]; ok {
				t.Logf("component %q has no generator: %s", name, reason)
				if generators[name] != nil {
					t.Errorf("component %q emits no code, but the compiler has a generator for it", name)
				}
				continue
			}
			if generators[name] == nil {
				t.Errorf("component %q has no generator in the compiler", name)
			}
		}
		var extra []string
		for name := range generators {
			if _, err := os.Stat("../../registry/components/" + name + "/component.yaml"); err != nil {
				extra = append(extra, name)
			}
		}
		sort.Strings(extra)
		if len(extra) > 0 {
			t.Errorf("generators with no registry component: %v", extra)
		}
	})
}

func TestCompilerLoadsNoGeneratorCode(t *testing.T) {
	t.Run("ST-STD-TRUST-R02-T02 No compiler source imports the plugin package, so the compiler cannot load code at run time", func(t *testing.T) {
		var scanned int
		for _, root := range []string{"../../cmd", "../../internal"} {
			err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.IsDir() && entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
					return nil
				}
				file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
				if err != nil {
					return err
				}
				scanned++
				for _, spec := range file.Imports {
					if name, _ := strconv.Unquote(spec.Path.Value); name == "plugin" {
						t.Errorf("%s imports the plugin package", path)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		if scanned == 0 {
			t.Fatal("no compiler source was scanned")
		}
	})
}
