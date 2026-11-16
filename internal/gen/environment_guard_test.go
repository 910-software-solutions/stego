package gen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvironmentGuard(t *testing.T) {
	dir := t.TempDir()
	t.Run("ST-SUP-UC01-R10-T04 A process refuses at start a variable that a library would read and that the service does not take, and names it, never its value", func(t *testing.T) {
		program := "package main\n\nimport \"os\"\n\nfunc main() {\n\tstegoRefuseEnvironment()\n\t_, _ = os.Stdout.WriteString(\"started\")\n}\n" + EnvironmentGuardSource
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(program), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/guard\ngo 1.26\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		binary := filepath.Join(dir, "guard")
		build := exec.Command("go", "build", "-o", binary, ".")
		build.Dir = dir
		build.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, output)
		}
		run := func(extra ...string) (string, error) {
			command := exec.Command(binary)
			for _, entry := range os.Environ() {
				name, _, _ := strings.Cut(entry, "=")
				if !RefusedVariable(name) && name != "GODEBUG" {
					command.Env = append(command.Env, entry)
				}
			}
			command.Env = append(command.Env, extra...)
			output, err := command.CombinedOutput()
			return string(output), err
		}
		for _, refused := range []string{"PGSSLROOTCERT=/CANARY", "PGPASSWORD=CANARY", "SSL_CERT_FILE=/CANARY", "GRPC_ENFORCE_ALPN_ENABLED=CANARY", "GODEBUG=panicnil=1,tlsrsakex=1", "GODEBUG=x509sha1=1", "GODEBUG=http2debug=2", "GODEBUG=panicnil=1, http2debug=1"} {
			output, err := run(refused)
			name, _, _ := strings.Cut(refused, "=")
			if err == nil || !strings.Contains(output, name) || strings.Contains(output, "CANARY") || strings.Contains(output, "started") {
				t.Errorf("%s: the process started, does not name the variable, or shows its value: %v\n%s", name, err, output)
			}
		}
		for _, allowed := range []string{"", "GODEBUG=panicnil=1", "GRPC_GO_LOG_SEVERITY_LEVEL=info", "PGHOST=", "TZ=UTC"} {
			if output, err := run(allowed); err != nil || output != "started" {
				t.Errorf("%q: the process did not start: %v\n%s", allowed, err, output)
			}
		}
		if !RefusedVariable("PGHOST") || RefusedVariable("GRPC_GO_LOG_FORMATTER") || RefusedVariable("TZ") {
			t.Error("RefusedVariable does not give the names of the guard")
		}
		guardRun = run
	})
	t.Run("ST-SUP-UC01-R10-T05 The guard refuses each name that pgx reads and starts a process with another name that starts with PG, such as PG_IMAGE", func(t *testing.T) {
		if guardRun == nil {
			t.Fatal("the guard binary of ST-SUP-UC01-R10-T04 is not built: a missing result is not a pass")
		}
		for _, name := range PGVariables {
			if output, err := guardRun(name + "=CANARY"); err == nil || !strings.Contains(output, name) || strings.Contains(output, "CANARY") {
				t.Errorf("%s: the process started, or does not name the variable, or shows its value: %v\n%s", name, err, output)
			}
		}
		for _, allowed := range []string{"PG_IMAGE=docker.io/library/postgres:17", "PGX_TEST_DATABASE=x", "PGDATA=/var/lib/postgresql/data", "PG=1"} {
			name, _, _ := strings.Cut(allowed, "=")
			if output, err := guardRun(allowed); err != nil || output != "started" || RefusedVariable(name) {
				t.Errorf("%q: the guard refuses a name that pgx does not read: %v\n%s", allowed, err, output)
			}
		}
	})
}

// guardRun runs the guard binary that ST-SUP-UC01-R10-T04 builds, with extra entries in its environment.
var guardRun func(extra ...string) (string, error)
