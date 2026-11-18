package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jsell-rh/stego/internal/delivery"
)

// These tests prove stego env check (ST-DLV-UC02) on the files of ST-DLV-TD02: the platform requirements that
// stego apply writes for the service of ST-DLV-TD01, kept as a fixture, and an environment file that meets them.
// ST-DLV-UC01-R01-T01 holds the fixture to the file that apply writes.

// environmentFixtureDir holds the files of ST-DLV-TD02.
var environmentFixtureDir = filepath.Join("..", "..", "internal", "delivery", "testdata", "environment")

// passMessage is the part of the report of a check that passes.
const passMessage = "meets each platform requirement"

// envCase is a copy of the files of ST-DLV-TD02 in a new folder.
type envCase struct {
	dir string
}

// newEnvCase copies the two files of ST-DLV-TD02 into a new folder.
func newEnvCase(t *testing.T) *envCase {
	t.Helper()
	c := &envCase{dir: t.TempDir()}
	for _, name := range []string{"platform-requirements.yaml", "dev.yaml"} {
		data, err := os.ReadFile(filepath.Join(environmentFixtureDir, name))
		if err != nil {
			t.Fatal(err)
		}
		c.write(t, name, string(data))
	}
	return c
}

func (c *envCase) read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(c.dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (c *envCase) write(t *testing.T, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(c.dir, name), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// change replaces the text old, which the file must hold once, by new.
func (c *envCase) change(t *testing.T, name, old, new string) {
	t.Helper()
	data := c.read(t, name)
	if strings.Count(data, old) != 1 {
		t.Fatalf("%s does not hold %q once", name, old)
	}
	c.write(t, name, strings.Replace(data, old, new, 1))
}

// check runs stego env check as a child process in the folder, with --requirements and the environment file, and
// gives what it printed on standard output and standard error and whether it exited 0.
func (c *envCase) check(t *testing.T, requirements, environment string) (string, string, bool) {
	t.Helper()
	stdout, stderr, err := runInProject(t, c.dir, nil, "env", "check", "--requirements", requirements, environment)
	return stdout, stderr, err == nil
}

// unmetLines gives the lines of the report that name an unmet requirement.
func unmetLines(stdout string) []string {
	var lines []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "missing: ") || strings.HasPrefix(line, "wrong type: ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// unusedLines gives the lines of the report that list an unused value.
func unusedLines(stdout string) []string {
	var lines []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "unused: ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// expectFailed fails the test unless the check failed, said nothing of a pass and named each part.
func expectFailed(t *testing.T, stdout, stderr string, passed bool, parts ...string) {
	t.Helper()
	if passed {
		t.Fatalf("the check passed:\n%s\n%s", stdout, stderr)
	}
	if strings.Contains(stdout+stderr, passMessage) {
		t.Errorf("the failed check says that the environment meets the requirements:\n%s\n%s", stdout, stderr)
	}
	for _, part := range parts {
		if !strings.Contains(stdout+stderr, part) {
			t.Errorf("the check does not name %s:\n%s\n%s", part, stdout, stderr)
		}
	}
}

// expectPassed fails the test unless the check passed, said so and named no unmet requirement.
func expectPassed(t *testing.T, stdout, stderr string, passed bool) {
	t.Helper()
	if !passed || !strings.Contains(stdout, passMessage) || len(unmetLines(stdout)) != 0 {
		t.Fatalf("the check did not pass with each requirement met:\n%s\n%s", stdout, stderr)
	}
}

// fileState is the digest and the modification time of a file.
type fileState struct {
	digest   string
	modified time.Time
}

// envFolderState gives the state of each file of the folder.
func envFolderState(t *testing.T, dir string) map[string]fileState {
	t.Helper()
	files := map[string]fileState{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files[path] = fileState{digest: hex.EncodeToString(sum[:]), modified: info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

const (
	passwordLine = "    password: CANARY-SECRET-VALUE\n"
	egressBlock  = "egresses:\n  payments:\n    address: payments.dev.example.com\n    port: 443\n"
	imageLine    = "image: quay.io/example/orders@sha256:3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a\n"
	configBlock  = "configuration:\n  LOG_LEVEL: info\n"
)

func TestEnvironmentCheck(t *testing.T) {
	t.Run("ST-DLV-UC02-R01-T01 The environment check names each unmet requirement with what is missing, and fails", func(t *testing.T) {
		c := newEnvCase(t)
		c.change(t, "dev.yaml", passwordLine, "")
		c.change(t, "dev.yaml", egressBlock, "")
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed)
		unmet := unmetLines(stdout)
		if len(unmet) != 2 || !strings.Contains(unmet[0], "secret orders-database") || !strings.Contains(unmet[0], "password") ||
			!strings.Contains(unmet[1], "egress payments") || !strings.HasPrefix(unmet[1], "missing: ") {
			t.Fatalf("the check does not name exactly the secret orders-database with the key password and the egress payments as missing:\n%s\n%s", stdout, stderr)
		}
	})

	t.Run("ST-DLV-UC02-R01-T02 The environment check passes when the environment file meets each requirement", func(t *testing.T) {
		c := newEnvCase(t)
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectPassed(t, stdout, stderr, passed)
	})

	t.Run("ST-DLV-UC02-R02-T01 The environment check refuses an image named by tag", func(t *testing.T) {
		c := newEnvCase(t)
		c.change(t, "dev.yaml", imageLine, "image: quay.io/example/orders:latest\n")
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "image", "quay.io/example/orders:latest", "digest")
	})

	t.Run("ST-DLV-UC02-R02-T02 The environment check refuses a secret without a key that its requirement names", func(t *testing.T) {
		c := newEnvCase(t)
		c.change(t, "dev.yaml", passwordLine, "")
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "secret orders-database", "password")
	})

	t.Run("ST-DLV-UC02-R03-T01 The environment check does not read secret values, and shows none, whether it passes or fails", func(t *testing.T) {
		c := newEnvCase(t)
		original := c.read(t, "dev.yaml")
		var outputs []string
		for _, value := range []string{"CANARY-SECRET-VALUE", "0", "{canary: CANARY-SECRET-VALUE}"} {
			c.write(t, "dev.yaml", strings.ReplaceAll(original, ": CANARY-SECRET-VALUE\n", ":"+map[bool]string{true: "", false: " "}[value == ""]+value+"\n"))
			stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
			if !passed {
				t.Fatalf("the check failed with the secret value %q:\n%s\n%s", value, stdout, stderr)
			}
			outputs = append(outputs, stdout+"\x00"+stderr)
		}
		for i, output := range outputs[1:] {
			if output != outputs[0] {
				t.Errorf("the output of run %d differs from the first:\n%q\n%q", i+2, output, outputs[0])
			}
		}
		c.write(t, "dev.yaml", original)
		c.change(t, "dev.yaml", passwordLine, "")
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "secret orders-database", "password")
		outputs = append(outputs, stdout+stderr)
		for i, output := range outputs {
			if strings.Contains(output, "CANARY-SECRET-VALUE") {
				t.Errorf("run %d shows the canary secret value:\n%s", i+1, output)
			}
		}
	})

	t.Run("ST-DLV-UC02-R03-T02 The environment check changes no file, whether it passes or fails", func(t *testing.T) {
		c := newEnvCase(t)
		unmet := strings.Replace(c.read(t, "dev.yaml"), egressBlock, "", 1)
		c.write(t, "unmet.yaml", unmet)
		for _, run := range []struct {
			file string
			pass bool
		}{{"dev.yaml", true}, {"unmet.yaml", false}} {
			before := envFolderState(t, c.dir)
			time.Sleep(10 * time.Millisecond)
			stdout, stderr, passed := c.check(t, "platform-requirements.yaml", run.file)
			if passed != run.pass {
				t.Fatalf("the check of %s passed %v, want %v:\n%s\n%s", run.file, passed, run.pass, stdout, stderr)
			}
			if after := envFolderState(t, c.dir); !maps.Equal(before, after) {
				t.Fatalf("the check of %s changed the folder: %v, then %v", run.file, before, after)
			}
		}
	})

	t.Run("ST-DLV-UC02-R04-T01 An environment file that cannot be read is a failed check that names the file and the place", func(t *testing.T) {
		c := newEnvCase(t)
		c.change(t, "dev.yaml", "service: orders\n", "service: orders: again\n")
		if line := strings.Split(c.read(t, "dev.yaml"), "\n")[3]; line != "service: orders: again" {
			t.Fatalf("line 4 of the environment file is %q", line)
		}
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "environment file dev.yaml", "line 4")
		stdout, stderr, passed = c.check(t, "platform-requirements.yaml", "no-such-environment.yaml")
		expectFailed(t, stdout, stderr, passed, "environment file no-such-environment.yaml")
	})

	t.Run("ST-DLV-UC02-R04-T02 Platform requirements that cannot be read or have an unknown shape are a failed check that names the file and the place", func(t *testing.T) {
		c := newEnvCase(t)
		stdout, stderr, passed := c.check(t, "no-such-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "platform requirements file no-such-requirements.yaml")
		original := c.read(t, "platform-requirements.yaml")
		c.change(t, "platform-requirements.yaml", "version: 2\n", "version: 99\n")
		stdout, stderr, passed = c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "platform requirements file platform-requirements.yaml", "field version")
		head, _, _ := strings.Cut(original, "requirements:\n  - ")
		c.write(t, "platform-requirements.yaml", head+"requirements:\n  orders-database: {kind: secret, keys: [url, password], components: [postgres-adapter]}\n")
		stdout, stderr, passed = c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "platform requirements file platform-requirements.yaml", "field requirements")
	})

	t.Run("ST-DLV-UC02-R04-T03 An environment file of an unknown shape is a failed check that names the place", func(t *testing.T) {
		c := newEnvCase(t)
		c.change(t, "dev.yaml", "secrets:\n  orders-database:\n    url: CANARY-SECRET-VALUE\n"+passwordLine, "secrets:\n  - orders-database\n")
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "environment file dev.yaml", "field secrets")
	})

	t.Run("ST-DLV-UC02-R05-T01 A value that no requirement names is listed as unused, and the check still passes", func(t *testing.T) {
		c := newEnvCase(t)
		c.change(t, "dev.yaml", configBlock, configBlock+"  LOG_FORMAT: json\n")
		c.change(t, "dev.yaml", "secrets:\n", "secrets:\n  legacy-cache:\n    token: CANARY-SECRET-VALUE\n")
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectPassed(t, stdout, stderr, passed)
		unused := strings.Join(unusedLines(stdout), "\n")
		for _, value := range []string{"secret legacy-cache", "configuration LOG_FORMAT"} {
			if !strings.Contains(unused, value) {
				t.Errorf("the check does not list %s as unused:\n%s", value, stdout)
			}
		}
	})

	t.Run("ST-DLV-UC02-R05-T02 An unused value does not change why the check fails", func(t *testing.T) {
		c := newEnvCase(t)
		c.change(t, "dev.yaml", configBlock, configBlock+"  LOG_FORMAT: json\n")
		c.change(t, "dev.yaml", egressBlock, "")
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed)
		unmet := unmetLines(stdout)
		if len(unmet) != 1 || !strings.Contains(unmet[0], "egress payments") {
			t.Fatalf("the check does not name the egress payments as the one unmet requirement:\n%s\n%s", stdout, stderr)
		}
		if unused := unusedLines(stdout); len(unused) != 1 || !strings.Contains(unused[0], "configuration LOG_FORMAT") {
			t.Fatalf("the check does not list LOG_FORMAT as unused:\n%s", stdout)
		}
		if strings.Contains(strings.Join(unmet, "\n")+stderr, "LOG_FORMAT") {
			t.Fatalf("the check names LOG_FORMAT as a cause of the failure:\n%s\n%s", stdout, stderr)
		}
	})
}

func TestEnvironmentCheckFlow(t *testing.T) {
	t.Run("ST-DLV-UC02-FL-T Checking an environment file that supplies each need reports that it meets every requirement", func(t *testing.T) {
		c := newEnvCase(t)
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectPassed(t, stdout, stderr, passed)
		if unused := unusedLines(stdout); len(unused) != 0 {
			t.Fatalf("the check lists unused values: %v", unused)
		}
	})
}

// TestEnvironmentVariablesCheck proves the environment check of the environment variable list (ST-DLV-UC02-R05,
// R06) and the follow-ups of the review of #338 (F1 to F7).
func TestEnvironmentVariablesCheck(t *testing.T) {
	// The key file of SpiceDB is a required variable of the type path; the key of the list cursors is not required
	// since only the api mode reads it (ST-SUP-RP14-LC11).
	const cursorLine = "  STEGO_SPICEDB_KEY_FILE: /etc/orders/spicedb/key\n"
	t.Run("ST-DLV-UC02-R06-T01 A required environment variable with no value fails the check, and is named", func(t *testing.T) {
		c := newEnvCase(t)
		c.change(t, "dev.yaml", cursorLine, "")
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "STEGO_SPICEDB_KEY_FILE")
		// A required secret variable has no configuration need: the check names it as a variable, and reads only
		// its name when the file gives it. A required variable is in no group of one of, so the pair leaves its group.
		c = newEnvCase(t)
		c.write(t, "platform-requirements.yaml", strings.ReplaceAll(c.read(t, "platform-requirements.yaml"), "    one_of: database-url\n", ""))
		c.change(t, "platform-requirements.yaml", "  - name: DATABASE_URL\n    read_by:\n      - postgres-adapter\n    type: string\n    required: false\n",
			"  - name: DATABASE_URL\n    read_by:\n      - postgres-adapter\n    type: string\n    required: true\n")
		// The variable has the secret source orders-database, key url: the key of that Secret gives it a value, as
		// the base wires it.
		stdout, stderr, passed = c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectPassed(t, stdout, stderr, passed)
		c.change(t, "dev.yaml", "    url: CANARY-SECRET-VALUE\n", "")
		stdout, stderr, passed = c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "variable DATABASE_URL")
		// A Secret named like the variable is not a value of it: the base reads only the Secret of the source
		// (#375 security review L2).
		c.change(t, "dev.yaml", "secrets:\n", "secrets:\n  DATABASE_URL:\n    url: CANARY-SECRET-VALUE\n    DATABASE_URL: CANARY-SECRET-VALUE\n")
		stdout, stderr, passed = c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "variable DATABASE_URL")
		// A secret variable with no source never has a value: the base writes no entry for it and no envFrom, so a
		// Secret of its name does not reach the service, with any key (#378 security review L2).
		c = newEnvCase(t)
		c.write(t, "platform-requirements.yaml", strings.ReplaceAll(c.read(t, "platform-requirements.yaml"), "    one_of: database-url\n", ""))
		c.change(t, "platform-requirements.yaml", "    required: false\n    description: The PostgreSQL connection string of the runtime role, or DATABASE_URL_FILE in its place, not the two. The process does not start when the two have no value.\n    secret: true\n    secret_source:\n      name: orders-database\n      key: url\n",
			"    required: true\n    description: The PostgreSQL connection string of the runtime role.\n    secret: true\n")
		c.change(t, "dev.yaml", "secrets:\n", "secrets:\n  DATABASE_URL:\n    value: CANARY-SECRET-VALUE\n")
		stdout, stderr, passed = c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "variable DATABASE_URL")
		c.change(t, "dev.yaml", "    value: CANARY-SECRET-VALUE\n", "    DATABASE_URL: CANARY-SECRET-VALUE\n")
		stdout, stderr, passed = c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "variable DATABASE_URL: the base wires no value for the secret environment variable DATABASE_URL: it has no secret source",
			"so a Secret named DATABASE_URL does not reach the service; state a secret source for it in the component that reads it (postgres-adapter)")
		if strings.Contains(stdout+stderr, "CANARY") {
			t.Fatalf("the check shows the value of a secret variable:\n%s\n%s", stdout, stderr)
		}
	})
	t.Run("ST-DLV-UC02-R06-T02 An optional variable with no value does not fail the check, and an empty value of a required one does", func(t *testing.T) {
		c := newEnvCase(t)
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectPassed(t, stdout, stderr, passed)
		if strings.Contains(stdout, "PORT") {
			t.Fatalf("the check names the optional variable PORT:\n%s", stdout)
		}
		for _, empty := range []string{"\"\"", "\"  \"", "\"\\t\""} {
			c := newEnvCase(t)
			c.change(t, "dev.yaml", cursorLine, "  STEGO_SPICEDB_KEY_FILE: "+empty+"\n")
			stdout, stderr, passed = c.check(t, "platform-requirements.yaml", "dev.yaml")
			expectFailed(t, stdout, stderr, passed, "STEGO_SPICEDB_KEY_FILE", "empty")
			if blank := empty != "\"\""; blank != strings.Contains(stdout, "wrong type: configuration STEGO_SPICEDB_KEY_FILE: the configuration value of STEGO_SPICEDB_KEY_FILE is white space only") {
				t.Errorf("the value %s of the required variable: the check names it as white space only %v, want %v:\n%s", empty, !blank, blank, stdout)
			}
		}
		// A value of white space only of the optional PORT is given, and the service reads it as it is, so the check
		// fails with a row of the wrong type; an empty or null value gives no value (#380 security review L2).
		for _, blank := range []string{"\"  \"", "\"\\t\""} {
			c := newEnvCase(t)
			c.change(t, "dev.yaml", configBlock, configBlock+"  PORT: "+blank+"\n")
			stdout, stderr, passed = c.check(t, "platform-requirements.yaml", "dev.yaml")
			expectFailed(t, stdout, stderr, passed, "wrong type: configuration PORT", "the configuration value of PORT is white space only", "remove PORT from the environment file")
		}
		for _, empty := range []string{"\"\"", "null", ""} {
			c := newEnvCase(t)
			c.change(t, "dev.yaml", configBlock, configBlock+"  PORT: "+empty+"\n")
			stdout, stderr, passed = c.check(t, "platform-requirements.yaml", "dev.yaml")
			expectPassed(t, stdout, stderr, passed)
		}
	})
	t.Run("ST-DLV-UC02-R05-T03 An unknown environment variable is listed as unused, and a value of a listed variable is not", func(t *testing.T) {
		c := newEnvCase(t)
		c.change(t, "dev.yaml", configBlock, configBlock+"  STEGO_UNKNOWN_SETTING: \"1\"\n  PORT: \"8443\"\n")
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectPassed(t, stdout, stderr, passed)
		unused := unusedLines(stdout)
		if len(unused) != 1 || !strings.Contains(unused[0], "configuration STEGO_UNKNOWN_SETTING") || !strings.Contains(unused[0], "no listed environment variable") {
			t.Fatalf("the check does not list exactly STEGO_UNKNOWN_SETTING as unused:\n%s", stdout)
		}
	})
	t.Run("ST-DLV-UC02-R05-T04 A value is matched to a requirement by its kind and its name", func(t *testing.T) {
		c := newEnvCase(t)
		c.change(t, "dev.yaml", "topics:\n", "topics:\n  orders-database:\n    topic: dev.orders.database\n")
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectPassed(t, stdout, stderr, passed)
		if unused := unusedLines(stdout); len(unused) != 1 || !strings.Contains(unused[0], "topic orders-database") {
			t.Fatalf("the check does not list the topic orders-database as unused:\n%s", stdout)
		}
	})
	t.Run("ST-DLV-UC02-R01-T03 The check prints each text of a file escaped, so that a file cannot forge a line", func(t *testing.T) {
		c := newEnvCase(t)
		c.change(t, "dev.yaml", "    engine: postgres\n", "    engine: \"mysql\\r\\u001b[2Kmet: forged\\u202e\"\n")
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "backing store orders-db")
		if strings.ContainsAny(stdout, "\r\x1b\u202e") || !strings.Contains(stdout, `\u{001B}`) {
			t.Fatalf("the check prints a control or bidirectional character as it is:\n%q", stdout)
		}
	})
	t.Run("ST-DLV-UC02-R01-T04 A wider grant meets its requirement and prints a notice", func(t *testing.T) {
		c := newEnvCase(t)
		c.change(t, "dev.yaml", "    level: restricted\n", "    level: baseline\n")
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectPassed(t, stdout, stderr, passed)
		for _, part := range []string{"notice: pod security pod-security", "baseline", "notice: cluster permission read-config-maps", "list"} {
			if !strings.Contains(stdout, part) {
				t.Errorf("the check does not print %q:\n%s", part, stdout)
			}
		}
	})
	t.Run("ST-DLV-UC02-R02-T03 A secret key with an empty or null value counts as missing", func(t *testing.T) {
		// A text of white space only, an empty map and an empty list are empty too (#378 security review L3).
		for _, empty := range []string{" \"\"", "", " null", " \" \"", " \"\\t \\n\"", " {}", " []"} {
			c := newEnvCase(t)
			c.change(t, "dev.yaml", passwordLine, "    password:"+empty+"\n")
			stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
			expectFailed(t, stdout, stderr, passed, "secret orders-database", "password")
		}
	})
	t.Run("ST-DLV-UC02-R02-T04 A secret environment variable given as a plain configuration value is of the wrong type", func(t *testing.T) {
		c := newEnvCase(t)
		c.change(t, "dev.yaml", configBlock, configBlock+"  DATABASE_URL: CANARY-SECRET-VALUE\n")
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "wrong type: configuration DATABASE_URL", "secret")
		if strings.Contains(stdout+stderr, "CANARY") {
			t.Fatalf("the check shows the value of the secret variable:\n%s\n%s", stdout, stderr)
		}
		// A value of white space only of a secret variable is a secret given as configuration too (#380 security
		// review L2).
		c = newEnvCase(t)
		c.change(t, "dev.yaml", configBlock, configBlock+"  DATABASE_URL: \" \"\n")
		stdout, stderr, passed = c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "wrong type: configuration DATABASE_URL: the environment variable DATABASE_URL is secret")
	})
	t.Run("ST-DLV-UC02-R02-T05 Each configuration value has the type, the allowed values and the range of its variable", func(t *testing.T) {
		for _, bad := range []struct{ line, parts string }{
			{"  GOMAXPROCS: eighty\n", "configuration GOMAXPROCS|the type integer|not an integer"},
			{"  GOMAXPROCS: 2000\n", "configuration GOMAXPROCS|more than 1024"},
			{"  GOMAXPROCS: 0\n", "configuration GOMAXPROCS|less than 1"},
			{"  JWK_CERT_URL: ftp://keys.example.com/certs\n", "configuration JWK_CERT_URL|not an HTTPS URL"},
			{"  JWK_CERT_URL: keys.example.com\n", "configuration JWK_CERT_URL|not an absolute URL"},
		} {
			c := newEnvCase(t)
			c.change(t, "dev.yaml", configBlock, configBlock+bad.line)
			stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
			expectFailed(t, stdout, stderr, passed, append([]string{"wrong type"}, strings.Split(bad.parts, "|")...)...)
		}
		// A required variable that the platform requirements state as a configuration need has its type and its
		// allowed values too.
		c := newEnvCase(t)
		c.change(t, "dev.yaml", configBlock, "configuration:\n  LOG_LEVEL: loud\n")
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "wrong type: configuration LOG_LEVEL", "not one of the allowed values debug, info, warn, error")
		c = newEnvCase(t)
		c.change(t, "dev.yaml", "  STEGO_SPICEDB_ENDPOINT: spicedb.dev.example.com:50051\n", "  STEGO_SPICEDB_ENDPOINT: https://spicedb.dev.example.com\n")
		stdout, stderr, passed = c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "wrong type: configuration STEGO_SPICEDB_ENDPOINT", "the type address")
		// Values of the right type, inside the allowed values and the range, pass.
		c = newEnvCase(t)
		c.change(t, "dev.yaml", configBlock, "configuration:\n  LOG_LEVEL: warn\n  GOMAXPROCS: 4\n  JWK_CERT_URL: https://keys.example.com/certs\n")
		stdout, stderr, passed = c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectPassed(t, stdout, stderr, passed)
	})
	t.Run("ST-DLV-UC02-R02-T07 A port outside 1 to 65535 fails the check, and every variable that is a port states that range", func(t *testing.T) {
		for _, bad := range []struct{ line, parts string }{
			{"  PORT: 70000\n", "configuration PORT|more than 65535"},
			{"  PORT: 0\n", "configuration PORT|less than 1"},
		} {
			c := newEnvCase(t)
			c.change(t, "dev.yaml", configBlock, configBlock+bad.line)
			stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
			expectFailed(t, stdout, stderr, passed, append([]string{"wrong type"}, strings.Split(bad.parts, "|")...)...)
		}
		for _, port := range []string{"1", "8443", "65535"} {
			c := newEnvCase(t)
			c.change(t, "dev.yaml", configBlock, configBlock+"  PORT: "+port+"\n")
			stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
			expectPassed(t, stdout, stderr, passed)
		}
		// Each environment variable of a shipped component that is an integer port, PORT or a name that ends in
		// _PORT, states the range of a TCP port, so that the check of every service refuses a port out of it.
		files, err := filepath.Glob(filepath.Join("..", "..", "registry", "components", "*", "component.yaml"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no component of the registry: %v", err)
		}
		ports := 0
		for _, file := range files {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var component struct {
				Environment []struct {
					Name    string         `yaml:"name"`
					Type    string         `yaml:"type"`
					Allowed map[string]any `yaml:"allowed"`
				} `yaml:"environment"`
			}
			if err := yaml.Unmarshal(data, &component); err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			for _, variable := range component.Environment {
				if variable.Type != "integer" || (variable.Name != "PORT" && !strings.HasSuffix(variable.Name, "_PORT")) {
					continue
				}
				ports++
				if variable.Allowed["min"] != "1" || variable.Allowed["max"] != "65535" || len(variable.Allowed) != 2 {
					t.Errorf("%s: the port variable %s states the range %v, not 1 to 65535", file, variable.Name, variable.Allowed)
				}
			}
		}
		if ports == 0 {
			t.Fatal("no component of the registry states a port variable; the test checks nothing")
		}
	})
	t.Run("ST-DLV-UC02-R02-T06 Each entry has the shape of its requirement kind", func(t *testing.T) {
		for _, bad := range []struct{ old, new, parts string }{
			{"    address: payments.dev.example.com\n", "    address: https://payments.dev.example.com/pay\n", "egress payments|not a host name"},
			{"    verbs: [get, list]\n", "    verbs: [get, list, destroy]\n", "cluster permission read-config-maps|the verb \"destroy\""},
			{"    resource: configmaps\n", "    resource: Config Maps\n", "cluster permission read-config-maps|not a Kubernetes resource name"},
		} {
			c := newEnvCase(t)
			c.change(t, "dev.yaml", bad.old, bad.new)
			stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
			expectFailed(t, stdout, stderr, passed, append([]string{"wrong type"}, strings.Split(bad.parts, "|")...)...)
		}
	})
	t.Run("ST-DLV-UC02-R04-T05 The help and the documentation give the environment schema, each with an example that the check reads", func(t *testing.T) {
		var stdout bytes.Buffer
		if err := runEnvTo([]string{"check", "--help"}, &stdout); err != nil {
			t.Fatalf("stego env check --help: %v", err)
		}
		doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "environment-file.md"))
		if err != nil {
			t.Fatal(err)
		}
		_, helpExample, _ := strings.Cut(envUsage, "\nExample:\n")
		_, docExample, _ := strings.Cut(string(doc), "```yaml\n")
		docExample, _, _ = strings.Cut(docExample, "```")
		requirements := delivery.Requirements{Version: delivery.RequirementsVersion, Service: "tasks", Requirements: []delivery.Requirement{}}
		for name, example := range map[string]string{"the help": strings.ReplaceAll(helpExample, "\n  ", "\n")[2:], "docs/environment-file.md": docExample} {
			for _, field := range []string{"version", "service", "environment", "image", "destination", "secrets", "configuration", "backing_stores", "egresses"} {
				if !strings.Contains(example, field+":") {
					t.Errorf("the example of %s has no field %s", name, field)
				}
			}
			fit, err := delivery.CheckEnvironment(requirements, []byte(example))
			if err != nil {
				t.Errorf("the check does not read the example of %s: %v\n%s", name, err, example)
			}
			for _, row := range fit.Rows {
				t.Errorf("the check does not read the example of %s as one of the right types: %s: %s", name, row.Requirement, row.Detail)
			}
		}
		for _, section := range []string{"version", "destination", "secrets", "configuration", "backing_stores", "topics", "identity_issuers", "ingresses", "egresses", "cluster_permissions", "pod_security"} {
			if !strings.Contains(envUsage, section) || !strings.Contains(string(doc), section) {
				t.Errorf("the help or the documentation does not name %s", section)
			}
		}
		if !strings.Contains(envUsage, "docs/environment-file.md") {
			t.Error("the help does not name docs/environment-file.md")
		}
	})
	t.Run("ST-DLV-UC02-R01-T05 An error of the check is printed escaped too", func(t *testing.T) {
		c := newEnvCase(t)
		c.change(t, "dev.yaml", passwordLine, passwordLine+"    \"X\u202eY\": a\n    \"X\u202eY\": b\n")
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "dev.yaml")
		expectFailed(t, stdout, stderr, passed, "environment file dev.yaml", `\u{202E}`)
		if strings.ContainsRune(stdout+stderr, '\u202e') {
			t.Fatalf("the check prints a bidirectional character as it is:\n%q", stderr)
		}
	})
	t.Run("ST-DLV-UC02-R04-T04 A file that is not a regular file is a failed check", func(t *testing.T) {
		c := newEnvCase(t)
		if err := syscall.Mkfifo(filepath.Join(c.dir, "pipe.yaml"), 0o600); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, passed := c.check(t, "platform-requirements.yaml", "pipe.yaml")
		expectFailed(t, stdout, stderr, passed, "environment file pipe.yaml", "not a regular file")
	})
}
