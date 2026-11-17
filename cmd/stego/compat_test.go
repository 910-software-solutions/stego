package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// compatResult is stego compat --json as the test reads it, with no STEGO code.
type compatResult struct {
	BaseRef    string `json:"base_ref"`
	BaseCommit string `json:"base_commit"`
	Faults     []struct {
		Rule    string `json:"rule"`
		Entity  string `json:"entity"`
		Field   string `json:"field"`
		Message string `json:"message"`
		Declare []struct {
			ID    string `json:"id"`
			Kind  string `json:"kind"`
			Field string `json:"field"`
			From  string `json:"from"`
			Needs string `json:"needs"`
			Phase string `json:"phase"`
		} `json:"declare"`
	} `json:"faults"`
}

// exitCode gives the exit status of a finished stego run.
func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("stego did not run: %v", err)
	}
	return exit.ExitCode()
}

// compat runs stego compat in the service folder and gives its output and exit status.
func (s *smallService) compat(t *testing.T, args ...string) (string, int) {
	t.Helper()
	stdout, stderr, err := s.stego(t, append([]string{"compat"}, args...)...)
	return stdout + stderr, exitCode(t, err)
}

func (s *smallService) compatJSON(t *testing.T) (compatResult, int) {
	t.Helper()
	stdout, stderr, err := s.stego(t, "compat", "--base", "HEAD", "--json")
	var result compatResult
	if jsonErr := json.Unmarshal([]byte(stdout), &result); jsonErr != nil {
		t.Fatalf("stego compat --json printed no JSON object: %v\n%s%s", jsonErr, stdout, stderr)
	}
	return result, exitCode(t, err)
}

// noteFields gives the declaration of ST-GEN-TD03 with other fields of Note.
func noteFields(fields string) string {
	return strings.Replace(smallServiceDeclaration, "fields: [{name: title, type: string}]", "fields: "+fields, 1)
}

const (
	renamedTitle = "[{name: heading, type: string}]"
	titleRename  = "schema_changes:\n  - {id: note-title-rename, kind: rename, entity: Note, field: heading, from: title}\nschema_history:\n  - {change: note-title-rename, phase: expand}\n"
	titleUnique  = "invariants:\n  - {name: unique_title, kind: unique, on: Note, by: [title]}\nschema_changes:\n  - {id: note-title-unique, kind: unique, entity: Note, field: unique_title}\nschema_history:\n  - {change: note-title-unique, phase: expand}\n"
)

func TestCompat(t *testing.T) {
	t.Parallel()
	t.Run("ST-GEN-UC14-R01-T01 A base declaration that is not the one its record names is refused", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		recorded := s.record(t)["last_applied"].(map[string]any)["service_hash"].(string)
		changed := strings.Replace(smallServiceDeclaration, "{name: title, type: string}", "{name: title, type: string, max_length: 80}", 1)
		s.write(t, "service.yaml", changed)
		git(t, s.service, "commit", "-q", "-am", "change max_length without apply")
		commit := git(t, s.service, "rev-parse", "HEAD")
		output, code := s.compat(t, "--base", "HEAD")
		if code != 1 {
			t.Fatalf("exit %d, want 1:\n%s", code, output)
		}
		sum := sha256.Sum256([]byte(changed))
		base := hex.EncodeToString(sum[:])
		for _, want := range []string{"HEAD", commit, recorded, base} {
			if !strings.Contains(output, want) {
				t.Errorf("the fault does not name %q:\n%s", want, output)
			}
		}
		// Plan refuses the same base, and its last line asks for no schema change, as the fault needs none
		// (walkthrough 7, rank 3).
		stdout, stderr, err := s.stego(t, "plan", "--base", "HEAD")
		if code := exitCode(t, err); code == 0 || !strings.Contains(stderr, "correct each fault as its text says") || strings.Contains(stdout+stderr, "declare the schema changes") {
			t.Fatalf("plan gave exit %d, want a refusal that asks to correct each fault and names no schema change:\n%s%s", code, stdout, stderr)
		}
	})
	t.Run("ST-GEN-UC14-R02-T01 With no record at the base ref, any stored shape is accepted", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		fresh := filepath.Join(t.TempDir(), "notes")
		if err := os.MkdirAll(fresh, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, fresh, "service.yaml", smallServiceDeclaration, 0o644)
		git(t, fresh, "init", "-q")
		git(t, fresh, "add", "-A")
		git(t, fresh, "commit", "-q", "-m", "first declaration, no record")
		writeFile(t, fresh, "service.yaml", noteFields("[{name: heading, type: string}, {name: owner, type: string}]"), 0o644)
		s.service = fresh
		output, code := s.compat(t, "--base", "HEAD")
		if code != 0 || !strings.Contains(output, "No fault: the base holds no record") {
			t.Fatalf("exit %d, want 0 and no fault:\n%s", code, output)
		}
	})
	t.Run("ST-GEN-UC14-R01-T01 A base that cannot be read whole is refused, and the caller's Git variables do not move the check", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		s.env = append(s.env, "GIT_DIR="+filepath.Join(s.parent, "no-such-repository"), "GIT_OBJECT_DIRECTORY="+filepath.Join(s.parent, "no-such-objects"))
		if output, code := s.compat(t, "--base", "HEAD"); code != 0 {
			t.Fatalf("with GIT_DIR and GIT_OBJECT_DIRECTORY set: exit %d, want 0:\n%s", code, output)
		}
		big := strings.Repeat("# padding\n", 500_000)
		s.write(t, "service.yaml", smallServiceDeclaration+big)
		git(t, s.service, "commit", "-q", "-am", "a declaration larger than a declaration may be")
		if output, code := s.compat(t, "--base", "HEAD"); code != 2 || !strings.Contains(output, "at most") {
			t.Errorf("an oversized base declaration: exit %d, want 2 and the limit:\n%s", code, output)
		}
		git(t, s.service, "reset", "-q", "--hard", "HEAD~1")
		blob := git(t, s.service, "rev-parse", "HEAD:.stego/state.yaml")
		if err := os.Remove(filepath.Join(s.service, ".git", "objects", blob[:2], blob[2:])); err != nil {
			t.Fatal(err)
		}
		s.write(t, "service.yaml", noteFields(renamedTitle))
		if output, code := s.compat(t, "--base", "HEAD"); code != 2 {
			t.Errorf("an unreadable base record: exit %d, want 2, not a base with no record:\n%s", code, output)
		}
	})
	t.Run("ST-GEN-UC14-R02-T01 A base with no service.yaml at the folder's path is refused while the service has a record", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		empty := git(t, s.service, "hash-object", "-t", "tree", "-w", "/dev/null")
		base := git(t, s.service, "commit-tree", empty, "-m", "the service is not at this path")
		s.write(t, "service.yaml", noteFields(renamedTitle))
		if output, code := s.compat(t, "--base", base); code != 2 || !strings.Contains(output, "holds no service.yaml at the path") {
			t.Errorf("exit %d, want 2 and a refusal that names the missing service.yaml:\n%s", code, output)
		}
		// The folder moved in the head, and the head's record is gone: the base still holds the record of a release.
		moved := filepath.Join(s.parent, "moved")
		if err := os.MkdirAll(moved, 0o755); err != nil {
			t.Fatal(err)
		}
		git(t, s.service, "checkout", "-q", "--", ".")
		git(t, s.service, "mv", "service.yaml", "moved-service.yaml")
		git(t, s.service, "commit", "-q", "-m", "keep the service at the root only in the base")
		released := git(t, s.service, "rev-parse", "HEAD~1")
		other := *s
		other.service = filepath.Join(s.service, "authz")
		writeFile(t, other.service, "service.yaml", noteFields(renamedTitle), 0o644)
		if output, code := other.compat(t, "--base", released); code != 2 || !strings.Contains(output, "it holds the record of a release in the root") {
			t.Errorf("a moved folder with no record in the head: exit %d, want 2 and the folder of the base's record:\n%s", code, output)
		}
	})
	t.Run("ST-GEN-UC14-R03-T01 Each fault names the entity, the field, the rule and the schema change and phase to declare", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		heads := []struct {
			name, declaration, field string
			declare                  []string // kind, field, from, needs for each change to declare
		}{
			{"rename", noteFields(renamedTitle), "title", []string{"rename heading title "}},
			{"retype", noteFields("[{name: title, type: int32}]"), "title", []string{"retype title string a conversion and an inverse conversion"}},
			{"require", noteFields("[{name: title, type: string}, {name: owner, type: string}]"), "owner", []string{"require owner  a fill value"}},
			{"unique", smallServiceDeclaration + "invariants:\n  - {name: unique_title, kind: unique, on: Note, by: [title]}\n", "unique_title", []string{"unique unique_title  "}},
			{"drop and add", noteFields("[{name: body, type: string, optional: true}]"), "title", []string{"drop title  the Definition string", "add body  "}},
		}
		for _, head := range heads {
			s.write(t, "service.yaml", head.declaration)
			result, code := s.compatJSON(t)
			if code != 1 || len(result.Faults) != 1 {
				t.Errorf("%s: exit %d with %d fault(s), want 1 and one fault: %+v", head.name, code, len(result.Faults), result.Faults)
				continue
			}
			fault := result.Faults[0]
			if fault.Entity != "Note" || fault.Field != head.field || fault.Rule != "ST-GEN-INV33" {
				t.Errorf("%s: the fault names %s.%s and the rule %s, want Note.%s and ST-GEN-INV33", head.name, fault.Entity, fault.Field, fault.Rule, head.field)
			}
			var got []string
			for _, change := range fault.Declare {
				if change.Phase != "expand" {
					t.Errorf("%s: the change %s is at %q, want expand", head.name, change.ID, change.Phase)
				}
				got = append(got, change.Kind+" "+change.Field+" "+change.From+" "+change.Needs)
			}
			if strings.Join(got, "|") != strings.Join(head.declare, "|") {
				t.Errorf("%s: the fault declares %q, want %q", head.name, got, head.declare)
			}
		}
	})
	t.Run("ST-GEN-UC14-R04-T01 The check names the base ref and its commit with and without a fault", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		commit := git(t, s.service, "rev-parse", "HEAD")
		output, code := s.compat(t, "--base", "HEAD")
		if code != 0 || !strings.Contains(output, "HEAD") || !strings.Contains(output, commit) {
			t.Errorf("unchanged: exit %d, want 0 and HEAD at %s:\n%s", code, commit, output)
		}
		s.write(t, "service.yaml", noteFields(renamedTitle))
		output, code = s.compat(t, "--base", "HEAD")
		if code != 1 || !strings.Contains(output, "HEAD") || !strings.Contains(output, commit) {
			t.Errorf("renamed: exit %d, want 1 and HEAD at %s:\n%s", code, commit, output)
		}
		result, code := s.compatJSON(t)
		if code != 1 || result.BaseRef != "HEAD" || result.BaseCommit != commit {
			t.Errorf("--json: exit %d, base ref %q and base commit %q, want 1, HEAD and %s", code, result.BaseRef, result.BaseCommit, commit)
		}
	})
	t.Run("ST-GEN-UC13-R07-T01 A fill value that a later release adds to a retype at expand is refused", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		retype := func(fill string) string {
			return noteFields("[{name: title, type: int64}]") + "schema_changes:\n  - {id: note-title-retype, kind: retype, entity: Note, field: title, from: string, conversion: text_to_int64, inverse_conversion: int64_to_text" +
				fill + "}\nschema_history:\n  - {change: note-title-retype, phase: expand}\n"
		}
		// The release of the retype, with its fill value, is the base (decision on ST-GEN-Q24 (c)).
		s.write(t, "service.yaml", retype(", fill_value: 0"))
		s.must(t, "apply", "--base", "HEAD")
		git(t, s.service, "add", "-A")
		git(t, s.service, "commit", "-q", "-m", "the retype at expand")
		for _, fill := range []string{", fill_value: 1", ""} {
			s.write(t, "service.yaml", retype(fill))
			output, code := s.compat(t, "--base", "HEAD")
			if code != 1 || !strings.Contains(output, "ST-GEN-INV33") || !strings.Contains(output, "note-title-retype") || !strings.Contains(output, "also its fill value") {
				t.Errorf("a head with %q in place of the fill value 0: exit %d, want 1 and a fault that names the change and its fill value:\n%s", fill, code, output)
			}
		}
		s.write(t, "service.yaml", retype(", fill_value: 0"))
		if output, code := s.compat(t, "--base", "HEAD"); code != 0 {
			t.Errorf("the head with the fill value of the base: exit %d, want 0:\n%s", code, output)
		}
	})
	t.Run("ST-GEN-UC14-FL-T An undeclared rename is refused against the base, and the same rename as a schema change is accepted", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		commit := git(t, s.service, "rev-parse", "HEAD")
		s.write(t, "service.yaml", noteFields(renamedTitle))
		output, code := s.compat(t, "--base", "HEAD")
		if code != 1 {
			t.Fatalf("undeclared rename: exit %d, want 1:\n%s", code, output)
		}
		for _, want := range []string{"Note.title", "ST-GEN-INV33", "rename note-title-rename (Note.heading from title) at expand", "HEAD", commit} {
			if !strings.Contains(output, want) {
				t.Errorf("undeclared rename: the output does not name %q:\n%s", want, output)
			}
		}
		s.write(t, "service.yaml", noteFields(renamedTitle)+titleRename)
		output, code = s.compat(t, "--base", "HEAD")
		if code != 0 || !strings.Contains(output, commit) {
			t.Errorf("declared rename: exit %d, want 0 and the commit %s:\n%s", code, commit, output)
		}
		fresh := filepath.Join(t.TempDir(), "notes")
		if err := os.MkdirAll(fresh, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, fresh, "service.yaml", smallServiceDeclaration, 0o644)
		git(t, fresh, "init", "-q")
		git(t, fresh, "add", "-A")
		git(t, fresh, "commit", "-q", "-m", "first release, no record")
		freshCommit := git(t, fresh, "rev-parse", "HEAD")
		writeFile(t, fresh, "service.yaml", noteFields(renamedTitle), 0o644)
		s.service = fresh
		output, code = s.compat(t, "--base", "HEAD")
		if code != 0 || !strings.Contains(output, freshCommit) {
			t.Errorf("first release: exit %d, want 0 and the commit %s:\n%s", code, freshCommit, output)
		}
	})
}

// relaxNotes gives the declaration of ST-GEN-TD03 with the fields of Note before the relax steps: title of at most
// 40 characters, the enum mood, the required size and the optional enum kind.
func relaxNotes(fields string) string {
	return noteFields(fields)
}

const relaxBaseFields = "[{name: title, type: string, min_length: 1, max_length: 40}, {name: mood, type: enum, values: [calm, busy]}, {name: size, type: string, max_length: 8}, {name: kind, type: enum, optional: true, values: [a, b]}]"

func TestCompatRelax(t *testing.T) {
	t.Run("ST-GEN-UC14-R03-T02 stego compat accepts each relax of a field of the base and refuses each narrowing", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		// The release before is a first release with the fields of relaxBaseFields.
		if err := os.RemoveAll(filepath.Join(s.service, ".git")); err != nil {
			t.Fatal(err)
		}
		s.write(t, "service.yaml", relaxNotes(relaxBaseFields))
		s.must(t, "apply")
		git(t, s.service, "init", "-q")
		git(t, s.service, "add", "-A")
		git(t, s.service, "commit", "-q", "-m", "notes before the relax steps")
		field := func(from, to string) string { return strings.Replace(relaxBaseFields, from, to, 1) }
		relaxed := func(fields string, changes ...string) string {
			declaration := relaxNotes(fields) + "schema_changes:\n"
			var steps []string
			for _, change := range changes {
				declaration += "  - " + change + "\n"
				id := strings.SplitN(strings.TrimPrefix(change, "{id: "), ",", 2)[0]
				steps = append(steps, "  - {change: "+id+", phase: expand}\n")
			}
			return declaration + "schema_history:\n" + strings.Join(steps, "")
		}
		const (
			title = "{id: note-title-relax, kind: relax, entity: Note, field: title, definition: max_length 40}"
			mood  = "{id: note-mood-relax, kind: relax, entity: Note, field: mood, definition: values calm busy}"
			size  = "{id: note-size-relax, kind: relax, entity: Note, field: size, definition: \"required, no default\"}"
			kind  = "{id: note-kind-relax, kind: relax, entity: Note, field: kind, definition: \"type enum, values a b\"}"
		)
		longer := field("max_length: 40", "max_length: 80")
		more := field("values: [calm, busy]", "values: [calm, busy, sleepy]")
		optional := field("{name: size, type: string, max_length: 8}", "{name: size, type: string, max_length: 8, optional: true, default: m}")
		text := field("{name: kind, type: enum, optional: true, values: [a, b]}", "{name: kind, type: string, optional: true}")
		all := strings.NewReplacer("max_length: 40", "max_length: 80", "values: [calm, busy]", "values: [calm, busy, sleepy]",
			"{name: size, type: string, max_length: 8}", "{name: size, type: string, max_length: 8, optional: true, default: m}",
			"{name: kind, type: enum, optional: true, values: [a, b]}", "{name: kind, type: string, optional: true}").Replace(relaxBaseFields)
		for _, c := range []struct{ name, declaration string }{
			{"a longer max_length", relaxed(longer, title)},
			{"a new enum value", relaxed(more, mood)},
			{"a required field made optional with a new default", relaxed(optional, size)},
			{"an enum made a string in place", relaxed(text, kind)},
			{"all four", relaxed(all, title, mood, size, kind)},
		} {
			s.write(t, "service.yaml", c.declaration)
			if output, code := s.compat(t, "--base", "HEAD"); code != 0 {
				t.Errorf("%s: exit %d, want 0:\n%s", c.name, code, output)
			}
		}
		for _, c := range []struct{ name, declaration, says string }{
			{"a shorter max_length", relaxed(field("max_length: 40", "max_length: 20"), title), "only makes a max_length longer"},
			{"an enum value taken away", relaxed(field("values: [calm, busy]", "values: [calm]"), mood), "only adds enum values"},
			{"an optional field made required", relaxed(field("{name: kind, type: enum, optional: true, values: [a, b]}", "{name: kind, type: enum, values: [a, b]}"),
				"{id: note-kind-relax, kind: relax, entity: Note, field: kind, definition: required}"), "needs a require"},
			{"a retype that needs a rewrite", relaxed(field("{name: kind, type: enum, optional: true, values: [a, b]}", "{name: kind, type: string, optional: true, max_length: 9}"), kind), "no max_length"},
			{"an undeclared shorter max_length", relaxNotes(field("max_length: 40", "max_length: 20")), "a relax only makes a max_length longer"},
		} {
			s.write(t, "service.yaml", c.declaration)
			output, code := s.compat(t, "--base", "HEAD")
			if code != 1 || !strings.Contains(output, c.says) {
				t.Errorf("%s: exit %d, want 1 and %q:\n%s", c.name, code, c.says, output)
			}
		}
		// An undeclared relax names the relax to declare.
		s.write(t, "service.yaml", relaxNotes(longer))
		result, code := s.compatJSON(t)
		if code != 1 || len(result.Faults) != 1 || len(result.Faults[0].Declare) != 1 || result.Faults[0].Declare[0].Kind != "relax" || result.Faults[0].Declare[0].Needs != "the definition max_length 40" {
			t.Errorf("an undeclared longer max_length: exit %d, faults %+v; want one that declares a relax with the definition max_length 40", code, result.Faults)
		}
	})
}
