package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readWithRegistry writes a registry folder that holds the component of envComponent, its variable
// ENV_A_DATABASE_URL read with the runtime package given, and a runtime folder whose package service holds Go code.
func readWithRegistry(t *testing.T, readWith string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"components/env-a/component.yaml": strings.Replace(envComponent, "    secret: true\n", "    secret: true\n    read_with: "+readWith+"\n", 1),
		"runtime/go.mod":                  "module github.com/jsell-rh/stego/registry/runtime\n\ngo 1.26\n",
		"runtime/service/service.go":      "package service\n",
		"runtime/query/query_test.go":     "package query\n",
	}
	for name, text := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestReadWith(t *testing.T) {
	t.Run("ST-SUP-UC01-R09-T05 The registry refuses a variable whose Read with names no package of the runtime library that holds Go code", func(t *testing.T) {
		if _, err := LoadDirectories(readWithRegistry(t, "service")); err != nil {
			t.Fatalf("a variable read with the package service is refused: %v", err)
		}
		for _, c := range []struct{ name, readWith, want string }{
			{"a package that the runtime does not hold", "ledger", "has no package ledger"},
			{"a package that holds only tests", "query", "has no package query"},
			{"a path that leaves the runtime module", "../service", "is not the path of a package"},
			{"a path in upper case", "Service", "is not the path of a package"},
		} {
			_, err := LoadDirectories(readWithRegistry(t, c.readWith))
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "ENV_A_DATABASE_URL") || !strings.Contains(err.Error(), "read_with") {
				t.Errorf("%s: want a refusal that names the variable, read_with and %q, got %v", c.name, c.want, err)
			}
		}
	})
}
