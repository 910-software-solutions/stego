package compiler

import (
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestBrowserSchemaDigest(t *testing.T) {
	t.Run("ST-DLV-UC07-R06-T08 The migration Job of a browser-service takes the digest of its session schema, not of no file", func(t *testing.T) {
		const empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		files := []gen.File{{Path: "main.go", Content: []byte("package main\n")}, {Path: "browser/schema.sql", Content: []byte("CREATE TABLE public.stego_browser_sessions ();\n")},
			{Path: "browser/schema/schema.go", Content: []byte("package schema\n")}, {Path: "browser/handlers.go", Content: []byte("package browser\n")}}
		digest := schemaDigest(files, nil)
		if digest == empty {
			t.Fatal("the browser-service takes the digest of no file, so each release reuses one Job name")
		}
		for name, changed := range map[string][]gen.File{
			"a changed session table":  {files[0], {Path: files[1].Path, Content: []byte("CREATE TABLE public.stego_browser_sessions (id bytea);\n")}, files[2], files[3]},
			"a changed schema package": {files[0], files[1], {Path: files[2].Path, Content: []byte("package schema // v2\n")}, files[3]},
		} {
			if schemaDigest(changed, nil) == digest {
				t.Errorf("%s keeps the name of the Job", name)
			}
		}
		if schemaDigest([]gen.File{{Path: "main.go", Content: []byte("package main // v2\n")}, files[1], files[2], {Path: files[3].Path, Content: []byte("package browser // v2\n")}}, nil) != digest {
			t.Error("a change outside the schema changes the name of the Job")
		}
	})
}
