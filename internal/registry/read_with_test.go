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
		"runtime/go.mod":                  "module github.com/910-software-solutions/stego/registry/runtime\n\ngo 1.26\n",
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

func TestReadByClient(t *testing.T) {
	t.Run("ST-SUP-UC01-R09-T06 The registry takes a variable that only a client program reads, and refuses one that is required, secret or read by a mode", func(t *testing.T) {
		client := "    secret: false\n    read_by_client: true\n    example: \"2m\"\n"
		if _, err := parseEnv(t, "    secret: false\n    example: \"2m\"\n", client); err != nil {
			t.Fatalf("a variable that only a client program reads is refused: %v", err)
		}
		// Each case marks a variable read_by_client together with a fact that only a variable of the service has.
		expectRefused(t, []refusal{
			{"a secret", "    secret: true\n", "    secret: true\n    read_by_client: true\n", "ENV_A_DATABASE_URL", "read_by_client"},
			{"a required variable", "    required: false\n    default: \"5m\"\n", "    required: true\n    read_by_client: true\n", "ENV_A_TIMEOUT", "read_by_client"},
			{"a mode that reads it", "    example: \"2m\"\n", "    read_by_client: true\n    read_by_mode: api\n    example: \"2m\"\n", "ENV_A_TIMEOUT", "read_by_client"},
			{"a runtime package that reads it", "    example: \"2m\"\n", "    read_by_client: true\n    read_with: service\n    example: \"2m\"\n", "ENV_A_TIMEOUT", "read_by_client"},
			{"a condition of required when", "    example: \"2m\"\n", "    read_by_client: true\n    required_when: \"the service has a secret\"\n    example: \"2m\"\n", "ENV_A_TIMEOUT", "read_by_client"},
		})
		// The shipped cli-application states APPLY_CONFIG, the file of the configuration of the CLI, as read by the
		// client alone (walkthrough 17, W17-D2).
		shipped, err := Load(filepath.Join("..", "..", "registry"))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, variable := range shipped.Component("cli-application").Variables() {
			if variable.Name == "APPLY_CONFIG" {
				found = variable.ReadByClient
			}
		}
		if !found {
			t.Error("the shipped cli-application does not state APPLY_CONFIG with read_by_client: true")
		}
	})
}
