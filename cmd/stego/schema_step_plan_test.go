package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/mod/semver"
)

// noteBody gives Note of ST-GEN-TD03 an optional field body.
func noteBody(declaration string) string {
	return strings.Replace(declaration, "fields: [{name: title, type: string}]", "fields: [{name: title, type: string}, {name: body, type: string, optional: true}]", 1)
}

// copyService copies the service folder, with its Git repository, beside it and gives the fixture of the copy.
func (s *smallService) copyService(t *testing.T, name string) *smallService {
	t.Helper()
	copied := *s
	copied.service = filepath.Join(s.parent, name)
	if output, err := exec.Command("cp", "-a", s.service, copied.service).CombinedOutput(); err != nil {
		t.Fatalf("copy the service folder: %v\n%s", err, output)
	}
	return &copied
}

func TestSchemaStepPlan(t *testing.T) {
	t.Parallel()
	t.Run("ST-GEN-UC12-R05-T01 Plan and apply refuse an undeclared change and write nothing", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		s.must(t, "plan")
		s.write(t, "service.yaml", noteFields(renamedTitle))
		before := tree(t, s.service)
		for _, args := range [][]string{{"plan"}, {"apply", "--base", "HEAD"}} {
			stdout, stderr, err := s.stego(t, args...)
			output := stdout + stderr
			if err == nil {
				t.Errorf("stego %s exits 0 on an undeclared rename:\n%s", args[0], output)
			}
			for _, want := range []string{"Note.title", "rename note-title-rename (Note.heading from title) at expand"} {
				if !strings.Contains(output, want) {
					t.Errorf("stego %s does not name %q:\n%s", args[0], want, output)
				}
			}
			if after := tree(t, s.service); !maps.Equal(before, after) {
				t.Errorf("stego %s changed a file of the service folder", args[0])
			}
		}
	})
	t.Run("ST-GEN-UC12-R05-T01 With no --base, plan takes the default branch, refuses a crafted origin/HEAD and says when there is no base", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		commit := git(t, s.service, "rev-parse", "HEAD")
		git(t, s.service, "tag", "v1")
		for _, c := range []struct{ target, want string }{
			{"refs/tags/v1", "which is no branch of origin"},
			{"refs/heads/main", "which is no branch of origin"},
			{"refs/remotes/origin/gone", "which names no commit"},
		} {
			git(t, s.service, "symbolic-ref", "refs/remotes/origin/HEAD", c.target)
			stdout, stderr, err := s.stego(t, "plan")
			if err == nil || !strings.Contains(stdout+stderr, c.want) {
				t.Errorf("origin/HEAD at %s: %v, want a refusal that says %q:\n%s%s", c.target, err, c.want, stdout, stderr)
			}
		}
		git(t, s.service, "update-ref", "refs/remotes/origin/main", commit)
		git(t, s.service, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
		if planned := s.must(t, "plan"); !strings.Contains(planned, "Base: refs/remotes/origin/main at commit "+commit) {
			t.Errorf("plan does not take the branch that origin/HEAD names:\n%s", planned)
		}
		git(t, s.service, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
		if planned := s.must(t, "plan"); !strings.Contains(planned, "Base: refs/heads/main at commit "+commit) {
			t.Errorf("with no origin, plan does not take the local main:\n%s", planned)
		}
		git(t, s.service, "branch", "-m", "trunk")
		if stdout, stderr, err := s.stego(t, "plan"); err == nil || !strings.Contains(stdout+stderr, "has no default branch") {
			t.Errorf("a repository with no default branch: %v, want a refusal that asks for --base:\n%s%s", err, stdout, stderr)
		}
		git(t, s.service, "branch", "-m", "main")
		noGit := *s
		noGit.env = append(slices.Clone(s.env[1:]), "PATH="+filepath.Dir(stegoBinary(t)))
		if stdout, stderr, err := noGit.stego(t, "plan"); err == nil || !strings.Contains(stdout+stderr, "git cannot read the repository") {
			t.Errorf("with no git to run: %v, want a refusal, not a plan with no check:\n%s%s", err, stdout, stderr)
		}
		s.unreleased(t)
		if planned := s.must(t, "plan"); !strings.Contains(planned, "Base: refs/heads/main at commit "+git(t, s.service, "rev-parse", "main")+" holds no record") {
			t.Errorf("plan on a base with no record does not name the base and its commit:\n%s", planned)
		}
		other := s.copyService(t, "notes-no-git")
		if output, err := exec.Command("rm", "-rf", filepath.Join(other.service, ".git")).CombinedOutput(); err != nil {
			t.Fatalf("remove the repository: %v\n%s", err, output)
		}
		other.write(t, "service.yaml", noteFields(renamedTitle))
		if planned := other.must(t, "plan"); !strings.Contains(planned, "No base check") {
			t.Errorf("plan in a folder with no repository does not say that it has no base:\n%s", planned)
		}
		// The service folder seen through a symbolic link from a folder with no .git, while its real parent holds a
		// .git that git cannot read: git walks the real path, so the check must stop too.
		linked := filepath.Join(t.TempDir(), "linked")
		holder := filepath.Join(s.parent, "holder")
		if err := os.MkdirAll(holder, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, holder, ".git", "gitdir: "+filepath.Join(s.parent, "removed-worktree")+"\n", 0o644)
		if output, err := exec.Command("cp", "-a", other.service, filepath.Join(holder, "notes")).CombinedOutput(); err != nil {
			t.Fatalf("copy the service folder: %v\n%s", err, output)
		}
		if err := os.Symlink(filepath.Join(holder, "notes"), linked); err != nil {
			t.Fatal(err)
		}
		through := *other
		through.service = linked
		through.env = append(slices.Clone(other.env), "PWD="+linked)
		if stdout, stderr, err := through.stego(t, "plan"); err == nil || !strings.Contains(stdout+stderr, "git cannot read the repository") {
			t.Errorf("a linked folder whose real parent holds a .git that git cannot read: %v, want a refusal:\n%s%s", err, stdout, stderr)
		}
		writeFile(t, other.service, ".git", "gitdir: "+filepath.Join(s.parent, "removed-worktree")+"\n", 0o644)
		if stdout, stderr, err := other.stego(t, "plan"); err == nil || !strings.Contains(stdout+stderr, "git cannot read the repository") {
			t.Errorf("a .git link to a removed worktree: %v, want a refusal, not a plan with no check:\n%s%s", err, stdout, stderr)
		}
	})
	t.Run("ST-GEN-UC14-R02-T02 In a repository with no commit, plan and apply take a service with no record as its first release", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		// stego init tells the author to commit before the first release.
		fresh := filepath.Join(s.parent, "fresh")
		if err := os.MkdirAll(filepath.Join(fresh, ".stego"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, fresh, ".stego/config.yaml", s.read(t, ".stego/config.yaml"), 0o644)
		initialize := exec.Command(stegoBinary(t), "init", "--archetype", "rest-crud")
		initialize.Dir, initialize.Env = fresh, s.env
		if output, err := initialize.CombinedOutput(); err != nil || !strings.Contains(string(output), "Commit the service folder to Git before its first release") {
			t.Errorf("stego init: %v, want the hint to commit before the first release:\n%s", err, output)
		}
		// A new repository with no commit, as git init and stego init leave it.
		unborn := s.copyService(t, "notes-unborn")
		if output, err := exec.Command("rm", "-rf", filepath.Join(unborn.service, ".git")).CombinedOutput(); err != nil {
			t.Fatalf("remove the repository: %v\n%s", err, output)
		}
		git(t, unborn.service, "init", "-q", "-b", "main")
		// The service has a record, so a release may have run: the check is not skipped.
		before := tree(t, unborn.service)
		for _, args := range [][]string{{"plan"}, {"apply"}} {
			if stdout, stderr, err := unborn.stego(t, args...); err == nil || !strings.Contains(stdout+stderr, "has no commit, and the service has a record") {
				t.Errorf("stego %s with a record and no commit: %v, want a refusal that asks for a commit:\n%s%s", args[0], err, stdout, stderr)
			}
		}
		if after := tree(t, unborn.service); !maps.Equal(before, after) {
			t.Error("a refused plan or apply changed a file of the service folder")
		}
		// With no record, it is the first release: plan and apply run with no base check.
		if err := os.Remove(filepath.Join(unborn.service, ".stego", "state.yaml")); err != nil {
			t.Fatal(err)
		}
		if planned := unborn.must(t, "plan"); !strings.Contains(planned, "No base check: the repository has no commit and the service has no record, so this is the first release") {
			t.Errorf("plan in a repository with no commit does not take the first release:\n%s", planned)
		}
		if applied := unborn.must(t, "apply"); !strings.Contains(applied, "this is the first release") {
			t.Errorf("apply in a repository with no commit does not take the first release:\n%s", applied)
		}
		// A base that the author names is still read: in a repository with no commit it names none.
		if stdout, stderr, err := unborn.stego(t, "plan", "--base", "main"); err == nil || !strings.Contains(stdout+stderr, "which has no commit: for the first release, run with no --base") {
			t.Errorf("plan --base main with no commit: %v, want a refusal that says to run with no --base:\n%s%s", err, stdout, stderr)
		}
		// A repository with a commit on a branch that is not the default one still has no default branch.
		git(t, unborn.service, "add", "-A")
		git(t, unborn.service, "commit", "-q", "-m", "notes")
		git(t, unborn.service, "branch", "-m", "trunk")
		if stdout, stderr, err := unborn.stego(t, "plan"); err == nil || !strings.Contains(stdout+stderr, "has no default branch") {
			t.Errorf("a repository with a commit and no default branch: %v, want a refusal that asks for --base:\n%s%s", err, stdout, stderr)
		}
	})
	t.Run("ST-GEN-UC14-R02-T03 A checkout by commit with no ref is not a repository with no commit", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		// A change on a branch renames title with no schema change and removes the record of the release.
		git(t, s.service, "checkout", "-q", "-b", "change")
		s.write(t, "service.yaml", noteFields(renamedTitle))
		git(t, s.service, "rm", "-q", ".stego/state.yaml")
		git(t, s.service, "commit", "-q", "-am", "rename title and remove the record")
		git(t, s.service, "checkout", "-q", "main")
		for _, fetch := range []struct {
			name     string
			args     []string
			checkout []string
		}{
			{"full", []string{"fetch", "-q", s.service, "change"}, []string{"checkout", "-q", "FETCH_HEAD"}},
			{"depth-1", []string{"fetch", "-q", "--depth", "1", "file://" + s.service, "change"}, []string{"checkout", "-q", "FETCH_HEAD"}},
			// The files of a shallow fetch with HEAD not moved: HEAD is not born, but the repository is shallow, so
			// it has a history that the check cannot read.
			{"depth-1-files", []string{"fetch", "-q", "--depth", "1", "file://" + s.service, "change"}, []string{"checkout", "-q", "FETCH_HEAD", "--", "."}},
		} {
			// The shape of a CI clone by commit: git init, git fetch of the change, git checkout FETCH_HEAD.
			checkout := *s
			checkout.service = filepath.Join(s.parent, "checkout-"+fetch.name)
			if err := os.MkdirAll(checkout.service, 0o755); err != nil {
				t.Fatal(err)
			}
			git(t, checkout.service, "init", "-q")
			git(t, checkout.service, fetch.args...)
			git(t, checkout.service, fetch.checkout...)
			if refs := git(t, checkout.service, "for-each-ref"); refs != "" {
				t.Fatalf("%s: the checkout has refs %q, want none", fetch.name, refs)
			}
			before := tree(t, checkout.service)
			for _, args := range [][]string{{"plan"}, {"apply"}} {
				stdout, stderr, err := checkout.stego(t, args...)
				if err == nil || strings.Contains(stdout+stderr, "No base check") || !strings.Contains(stdout+stderr, "has no default branch") {
					t.Errorf("%s: stego %s on a checkout by commit with no ref and no record: %v, want a refusal that names the missing default branch:\n%s%s", fetch.name, args[0], err, stdout, stderr)
				}
			}
			if after := tree(t, checkout.service); !maps.Equal(before, after) {
				t.Errorf("%s: a refused plan or apply changed a file of the checkout", fetch.name)
			}
		}
		// HEAD is not born, no ref exists and the repository is not shallow, but the object store holds the commits
		// of the change or a FETCH_HEAD exists: the files came from a commit, so this is no new repository.
		for _, unborn := range []struct {
			name  string
			steps [][]string
			// objectsOnly removes FETCH_HEAD, so that only the commits in the object store tell; fetchHeadOnly
			// removes the objects, so that only FETCH_HEAD tells.
			objectsOnly, fetchHeadOnly bool
		}{
			{name: "pathspec-checkout", steps: [][]string{{"fetch", "-q", s.service, "change"}, {"checkout", "-q", "FETCH_HEAD", "--", "."}}},
			{name: "restore", steps: [][]string{{"fetch", "-q", s.service, "change"}, {"restore", "--source=FETCH_HEAD", "--worktree", "--", "."}}},
			{name: "read-tree", steps: [][]string{{"fetch", "-q", s.service, "change"}, {"read-tree", "-u", "--reset", "FETCH_HEAD"}}},
			{name: "orphan-main-deleted", steps: [][]string{{"fetch", "-q", "--no-write-fetch-head", "--update-head-ok", s.service, "change:refs/heads/main"}, {"checkout", "-q", "main"}, {"checkout", "-q", "--orphan", "fresh"}, {"update-ref", "-d", "refs/heads/main"}}},
			{name: "objects-only", steps: [][]string{{"fetch", "-q", s.service, "change"}, {"read-tree", "-u", "--reset", "FETCH_HEAD"}}, objectsOnly: true},
			{name: "fetch-head-only", steps: [][]string{{"fetch", "-q", s.service, "change"}, {"read-tree", "-u", "--reset", "FETCH_HEAD"}}, fetchHeadOnly: true},
		} {
			checkout := *s
			checkout.service = filepath.Join(s.parent, "unborn-"+unborn.name)
			if err := os.MkdirAll(checkout.service, 0o755); err != nil {
				t.Fatal(err)
			}
			git(t, checkout.service, "init", "-q", "-b", "main")
			for _, step := range unborn.steps {
				git(t, checkout.service, step...)
			}
			if unborn.objectsOnly {
				if err := os.Remove(filepath.Join(checkout.service, ".git", "FETCH_HEAD")); err != nil {
					t.Fatal(err)
				}
			}
			if unborn.fetchHeadOnly {
				objects := filepath.Join(checkout.service, ".git", "objects")
				if err := os.RemoveAll(objects); err != nil {
					t.Fatal(err)
				}
				for _, folder := range []string{"info", "pack"} {
					if err := os.MkdirAll(filepath.Join(objects, folder), 0o755); err != nil {
						t.Fatal(err)
					}
				}
			}
			if refs := git(t, checkout.service, "for-each-ref"); refs != "" {
				t.Fatalf("%s: the checkout has refs %q, want none", unborn.name, refs)
			}
			if _, err := os.Stat(filepath.Join(checkout.service, ".stego", "state.yaml")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s: the checkout has a record (%v), want the change that removes it", unborn.name, err)
			}
			before := tree(t, checkout.service)
			for _, args := range [][]string{{"plan"}, {"apply"}} {
				stdout, stderr, err := checkout.stego(t, args...)
				if err == nil || strings.Contains(stdout+stderr, "No base check") || !strings.Contains(stdout+stderr, "has no default branch") {
					t.Errorf("%s: stego %s with HEAD not born, no ref and the files of a fetched commit: %v, want a refusal that names the missing default branch:\n%s%s", unborn.name, args[0], err, stdout, stderr)
				}
			}
			if after := tree(t, checkout.service); !maps.Equal(before, after) {
				t.Errorf("%s: a refused plan or apply changed a file of the checkout", unborn.name)
			}
		}
		// An orphan branch: HEAD is not born, but the repository has refs, so it has commits.
		orphan := s.copyService(t, "notes-orphan")
		git(t, orphan.service, "branch", "-m", "main", "trunk")
		git(t, orphan.service, "checkout", "-q", "--orphan", "fresh")
		if err := os.Remove(filepath.Join(orphan.service, ".stego", "state.yaml")); err != nil {
			t.Fatal(err)
		}
		if stdout, stderr, err := orphan.stego(t, "plan"); err == nil || strings.Contains(stdout+stderr, "No base check") || !strings.Contains(stdout+stderr, "has no default branch") {
			t.Errorf("stego plan on an orphan branch of a repository with refs: %v, want a refusal that names the missing default branch:\n%s%s", err, stdout, stderr)
		}
	})
	t.Run("ST-DECL-UC03-R03-T02 Plan refuses a declaration that checking refuses, also when it needs add steps", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		// A new optional field needs an add step, and the runtime data path serves no money field yet, so checking
		// refuses it.
		s.write(t, "service.yaml", strings.Replace(smallServiceDeclaration, "fields: [{name: title, type: string}]", "fields: [{name: title, type: string}, {name: price, type: money, currency: EUR, optional: true}]", 1))
		const fault = "has the field price of type money"
		if stdout, stderr, err := s.stego(t, "validate"); err == nil || !strings.Contains(stdout+stderr, fault) {
			t.Fatalf("stego validate: %v, want a refusal that names the money field:\n%s%s", err, stdout, stderr)
		}
		before := tree(t, s.service)
		for _, args := range [][]string{{"plan"}, {"plan", "--base", "HEAD"}, {"apply", "--base", "HEAD"}} {
			stdout, stderr, err := s.stego(t, args...)
			if err == nil || !strings.Contains(stdout+stderr, fault) {
				t.Errorf("stego %s: %v, want the refusal of validate:\n%s%s", strings.Join(args, " "), err, stdout, stderr)
			}
			if strings.Contains(stdout, "Apply writes the") {
				t.Errorf("stego %s lists the add steps of a declaration that checking refuses:\n%s", strings.Join(args, " "), stdout)
			}
		}
		if after := tree(t, s.service); !maps.Equal(before, after) {
			t.Error("a refused plan or apply changed a file of the service folder")
		}
	})
	t.Run("ST-GEN-UC12-R06-T01 Apply writes the same add steps for a new entity and a new optional field each time", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		changed := noteBody(secondEntityDeclaration)
		s.write(t, "service.yaml", changed)
		other := s.copyService(t, "notes-copy")
		before := tree(t, s.service)
		planned := s.must(t, "plan", "--base", "HEAD")
		for _, want := range []string{"add-note-body", "add-tag"} {
			if !strings.Contains(planned, want) {
				t.Errorf("plan does not list %s:\n%s", want, planned)
			}
		}
		if !maps.Equal(before, tree(t, s.service)) {
			t.Error("plan changed a file")
		}
		// apply --saved-plan refuses, names each step and the remedy, and writes nothing (walkthrough 11, rank 3).
		stdout, stderr, err := s.stego(t, "apply", "--base", "HEAD", "--saved-plan")
		for _, want := range []string{"add-note-body (add Note.body at expand)", "add-tag (add Tag at expand)", "against the base HEAD",
			"Run stego apply --base HEAD without --saved-plan"} {
			if err == nil || !strings.Contains(stdout+stderr, want) {
				t.Errorf("apply --saved-plan with add steps to write: %v, want a refusal that names %q:\n%s%s", err, want, stdout, stderr)
			}
		}
		if !maps.Equal(before, tree(t, s.service)) {
			t.Error("the refused apply --saved-plan changed a file")
		}
		applied := s.must(t, "apply", "--base", "HEAD")
		for _, want := range []string{"add-note-body", "add-tag"} {
			if !strings.Contains(applied, want) {
				t.Errorf("apply does not name %s:\n%s", want, applied)
			}
		}
		declaration := s.read(t, "service.yaml")
		wantTail := "schema_changes:\n  - {id: add-note-body, kind: add, entity: Note, field: body}\n  - {id: add-tag, kind: add, entity: Tag}\n" +
			"schema_history:\n  - {change: add-note-body, phase: expand}\n  - {change: add-tag, phase: expand}\n"
		if !strings.HasPrefix(declaration, changed) || !strings.HasSuffix(declaration, wantTail) {
			t.Errorf("apply wrote service.yaml as:\n%s\nwant the declaration with this end:\n%s", declaration, wantTail)
		}
		recorded := s.record(t)["last_applied"].(map[string]any)["service_hash"]
		if want := sha256Text(declaration); recorded != want {
			t.Errorf("the record names the declaration digest %v, want %s, the digest of the new service.yaml", recorded, want)
		}
		other.must(t, "apply", "--base", "HEAD")
		if again := other.read(t, "service.yaml"); again != declaration {
			t.Errorf("a second apply of the same declaration wrote other steps:\n%s\nwant:\n%s", again, declaration)
		}
		required := strings.Replace(declaration, "{name: body, type: string, optional: true}", "{name: body, type: string, optional: true}, {name: owner, type: string}", 1)
		s.write(t, "service.yaml", required)
		stdout, stderr, err = s.stego(t, "apply", "--base", "HEAD")
		if err == nil || !strings.Contains(stdout+stderr, "require note-owner-require (Note.owner) with a fill value at expand") {
			t.Errorf("apply of a new required field with no default: %v, want a refusal that names the require to declare:\n%s%s", err, stdout, stderr)
		}
		if s.read(t, "service.yaml") != required {
			t.Error("the refused apply wrote a step in service.yaml")
		}
	})
	t.Run("ST-GEN-UC12-R02-T03 Plan and apply refuse a declared schema change whose kind generation does not carry out yet", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		s.write(t, "service.yaml", noteFields("[{name: title, type: string}]")+titleUnique)
		before := tree(t, s.service)
		for _, command := range []string{"plan", "apply"} {
			stdout, stderr, err := s.stego(t, command, "--base", "HEAD")
			if err == nil || !strings.Contains(stdout+stderr, "does not carry out the schema change note-title-unique (unique) yet") {
				t.Errorf("stego %s of a declared unique: %v, want a refusal that names the change and its kind:\n%s%s", command, err, stdout, stderr)
			}
		}
		if !maps.Equal(before, tree(t, s.service)) {
			t.Error("a refused plan or apply changed a file")
		}
		if stdout, stderr, err := s.stego(t, "validate"); err != nil {
			t.Errorf("stego validate refuses the declared unique, which checking accepts: %v\n%s%s", err, stdout, stderr)
		}
	})
	t.Run("ST-SUP-RC01-OC49-T05 Plan refuses a field made secret over a base in clear with the seal step, and lists the seal step with the seal job and the same 001_initial", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		s.write(t, "service.yaml", noteFields("[{name: title, type: string}, {name: body, type: string, optional: true, sensitivity: pii}]"))
		s.must(t, "apply", "--base", "HEAD")
		git(t, s.service, "add", "-A")
		git(t, s.service, "commit", "-q", "-m", "body in clear")
		// The apply wrote the add step of body into the declaration of the base.
		base := s.read(t, "service.yaml")
		secret := strings.Replace(base, "sensitivity: pii", "sensitivity: secret", 1)
		if !strings.HasSuffix(secret, "phase: expand}\n") {
			t.Fatalf("the declaration of the base does not end with its schema history:\n%s", secret)
		}
		s.write(t, "service.yaml", secret)
		stdout, stderr, err := s.stego(t, "plan", "--base", "HEAD")
		if err == nil || !strings.Contains(stdout+stderr, "declare a seal step for body") || !strings.Contains(stdout+stderr, "seal note-body-seal (Note.body)") {
			t.Fatalf("plan of a field made secret: %v, want a refusal that names the seal step:\n%s%s", err, stdout, stderr)
		}
		sealed := strings.Replace(secret, "schema_changes:\n", "schema_changes:\n  - {id: note-body-seal, kind: seal, entity: Note, field: body, from: pii}\n", 1) +
			"  - {change: note-body-seal, phase: expand}\n"
		s.write(t, "service.yaml", sealed)
		planned := s.must(t, "plan", "--base", "HEAD")
		want := "2. note-body-seal: seal Note.body at expand; migration internal/storage/migrations/step_0002_note-body-seal_expand.sql; trigger none; backfill the seal job of the controller under the lease stego.backfill.note-body-seal"
		if !strings.Contains(planned, "New schema steps:\n  "+want+"\n") {
			t.Errorf("plan does not list the seal step as\n%s\n%s", want, planned)
		}
		s.must(t, "apply", "--base", "HEAD", "--saved-plan")
		// The next plan over the release of the seal warns that the clear column still holds the secret until the
		// contract (security review of PR #443, L2).
		git(t, s.service, "add", "-A")
		git(t, s.service, "commit", "-q", "-m", "seal")
		if again := s.must(t, "plan", "--base", "HEAD"); !strings.Contains(again, "warning: the seal note-body-seal is open since a release before") {
			t.Errorf("plan over the open seal gives no warning:\n%s", again)
		}
		if migration := s.read(t, "out/internal/storage/migrations/step_0002_note-body-seal_expand.sql"); !strings.HasPrefix(migration, "ALTER TABLE public.\"notes\" ADD COLUMN \"body_sealed\" bytea;\n") {
			t.Errorf("the migration of the seal step is\n%s", migration)
		}
		if base := git(t, s.service, "show", "HEAD:out/internal/storage/migrations/001_initial.sql"); strings.TrimSpace(base) != strings.TrimSpace(s.read(t, "out/internal/storage/migrations/001_initial.sql")) {
			t.Errorf("001_initial changed with the seal step:\nbase %s\nnow %s", base, s.read(t, "out/internal/storage/migrations/001_initial.sql"))
		}
		// With no check against a base that holds a record, plan refuses the open seal and names --base (security
		// re-review of PR #443, N1).
		if err := os.RemoveAll(filepath.Join(s.service, ".git")); err != nil {
			t.Fatal(err)
		}
		if stdout, stderr, err := s.stego(t, "plan"); err == nil || !strings.Contains(stdout+stderr, "open seal note-body-seal") || !strings.Contains(stdout+stderr, "give --base") {
			t.Errorf("plan of an open seal with no base check: %v, want a refusal that names --base:\n%s%s", err, stdout, stderr)
		}
	})
	t.Run("ST-GEN-UC12-R01-T02 Plan lists a new add step with its kind, phase and migration, and no trigger or backfill", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		s.write(t, "service.yaml", noteBody(smallServiceDeclaration))
		s.must(t, "apply", "--base", "HEAD")
		planned := s.must(t, "plan", "--base", "HEAD")
		want := "1. add-note-body: add Note.body at expand; migration internal/storage/migrations/step_0001_add-note-body_expand.sql; trigger none; backfill none"
		if !strings.Contains(planned, "New schema steps:\n  "+want+"\n") {
			t.Errorf("plan does not list the add step as\n%s\n%s", want, planned)
		}
		migration := s.read(t, "out/internal/storage/migrations/step_0001_add-note-body_expand.sql")
		if migration != "ALTER TABLE public.\"notes\" ADD COLUMN \"body\" text;\n" {
			t.Errorf("the migration of the add step is\n%s", migration)
		}
		// The search index of the field is a migration of its own, built CONCURRENTLY (decision on finding F2).
		if index := s.read(t, "out/internal/storage/migrations/step_0001_add-note-body_expand_index_01.sql"); index != "CREATE INDEX CONCURRENTLY IF NOT EXISTS \"notes_body_search\" ON public.\"notes\" (\"tenant_id\", \"body\");\n" {
			t.Errorf("the index migration of the add step is\n%s", index)
		}
		// 001_initial is the baseline, the body of the release that runs, so its ledger digest does not change.
		if base := git(t, s.service, "show", "HEAD:out/internal/storage/migrations/001_initial.sql"); strings.TrimSpace(base) != strings.TrimSpace(s.read(t, "out/internal/storage/migrations/001_initial.sql")) {
			t.Errorf("001_initial changed with the add step:\nbase %s\nnow %s", base, s.read(t, "out/internal/storage/migrations/001_initial.sql"))
		}
		// Once the step is in the base, it is not new.
		git(t, s.service, "add", "-A")
		git(t, s.service, "commit", "-q", "-m", "body")
		if again := s.must(t, "plan", "--base", "HEAD"); strings.Contains(again, "New schema steps") {
			t.Errorf("plan lists a step that the base holds:\n%s", again)
		}
	})
	t.Run("ST-GEN-UC12-R01-T01 Plan lists a new schema step with its kind, phase, migration, trigger and backfill", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		s.write(t, "service.yaml", noteFields(renamedTitle)+titleRename)
		planned := s.must(t, "plan", "--base", "HEAD")
		want := "1. note-title-rename: rename Note.heading at expand; migration internal/storage/migrations/step_0001_note-title-rename_expand.sql; " +
			"trigger notes_heading_sync; backfill in the migration job under the lease stego.backfill.note-title-rename"
		if !strings.Contains(planned, "New schema steps:\n  "+want+"\n") {
			t.Errorf("plan does not list the rename step as\n%s\n%s", want, planned)
		}
	})
	t.Run("ST-GEN-INV36-T A schema step on a base that pins an old postgres-adapter is refused and names the release to deploy first", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		// R0: the registry of R1 with the postgres-adapter version before the one that holds ST-SUP-RC01-OC28.
		git(t, s.registry, "checkout", "-q", "--detach", s.r1)
		definition := filepath.Join(s.registry, "components", "postgres-adapter", "component.yaml")
		data, err := os.ReadFile(definition)
		if err != nil {
			t.Fatal(err)
		}
		version := regexp.MustCompile(`\nversion: ([0-9.]+)\n`)
		if found := version.FindStringSubmatch(string(data)); found == nil || semver.Compare("v"+found[1], "v6.1.0") < 0 {
			t.Fatalf("the postgres-adapter of R1 is not 6.1.0 or later:\n%s", data)
		}
		writeFile(t, filepath.Dir(definition), "component.yaml", version.ReplaceAllString(string(data), "\nversion: 6.0.1\n"), 0o644)
		git(t, s.registry, "commit", "-q", "-am", "R0")
		r0 := git(t, s.registry, "rev-parse", "HEAD")
		git(t, s.registry, "branch", "r0", r0)
		git(t, s.registry, "checkout", "-q", "--detach", s.r1)
		s.pin(t, r0)
		s.must(t, "apply", "--base", "HEAD")
		git(t, s.service, "add", "-A")
		git(t, s.service, "commit", "-q", "-m", "base at R0")
		head := noteBody(smallServiceDeclaration) + "schema_changes:\n  - {id: add-note-body, kind: add, entity: Note, field: body}\nschema_history:\n  - {change: add-note-body, phase: expand}\n"
		s.pin(t, s.r1)
		s.write(t, "service.yaml", head)
		stdout, stderr, err := s.stego(t, "compat", "--base", "HEAD")
		if code := exitCode(t, err); code != 1 || !strings.Contains(stdout+stderr, "ST-GEN-INV36") || !strings.Contains(stdout+stderr, "deploy a release with postgres-adapter 6.1.0 or later") {
			t.Errorf("compat on a base at postgres-adapter 6.0.1: exit %d, want 1 and a fault that names 6.1.0 as the release to deploy first:\n%s%s", code, stdout, stderr)
		}
		// The base moves to R1 first, with no new step, and then the same head passes.
		s.write(t, "service.yaml", smallServiceDeclaration)
		s.must(t, "apply", "--base", "HEAD")
		git(t, s.service, "add", "-A")
		git(t, s.service, "commit", "-q", "-m", "base at R1")
		s.write(t, "service.yaml", head)
		if stdout, stderr, err := s.stego(t, "compat", "--base", "HEAD"); exitCode(t, err) != 0 {
			t.Errorf("compat on a base at postgres-adapter 6.1.0 refuses the add step: %v\n%s%s", err, stdout, stderr)
		}
	})
	t.Run("ST-GEN-INV36-T A rename step on a base that pins a runtime library before the fixed sync template is refused and names the runtime to deploy first", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		record := s.read(t, ".stego/state.yaml")
		pin := regexp.MustCompile(`(module: github.com/910-software-solutions/stego/registry/runtime\n\s+version: )(v[0-9.]+)`)
		found := pin.FindStringSubmatch(record)
		if found == nil {
			t.Fatalf("the record pins no runtime library:\n%s", record)
		}
		s.write(t, "service.yaml", noteFields(renamedTitle)+titleRename)
		if stdout, stderr, err := s.stego(t, "compat", "--base", "HEAD"); exitCode(t, err) != 0 {
			t.Fatalf("compat on a base at the runtime library %s refuses the rename: %v\n%s%s", found[2], err, stdout, stderr)
		}
		// The same base, whose record pins the runtime library before the one that holds the fixed template.
		s.write(t, "service.yaml", smallServiceDeclaration)
		s.write(t, ".stego/state.yaml", pin.ReplaceAllString(record, "${1}v0.94.0"))
		git(t, s.service, "commit", "-q", "-am", "base at v0.94.0")
		s.write(t, "service.yaml", noteFields(renamedTitle)+titleRename)
		stdout, stderr, err := s.stego(t, "compat", "--base", "HEAD")
		if code := exitCode(t, err); code != 1 || !strings.Contains(stdout+stderr, "ST-GEN-INV36") || !strings.Contains(stdout+stderr, "deploy a release with the runtime library v0.95.0 or later") {
			t.Errorf("compat on a base at the runtime library v0.94.0: exit %d, want 1 and a fault that names v0.95.0 as the runtime to deploy first:\n%s%s", code, stdout, stderr)
		}
		// An add step needs no sync trigger, so the same base takes it.
		s.write(t, "service.yaml", noteBody(smallServiceDeclaration)+"schema_changes:\n  - {id: add-note-body, kind: add, entity: Note, field: body}\nschema_history:\n  - {change: add-note-body, phase: expand}\n")
		if stdout, stderr, err := s.stego(t, "compat", "--base", "HEAD"); exitCode(t, err) != 0 {
			t.Errorf("compat on a base at the runtime library v0.94.0 refuses an add step: %v\n%s%s", err, stdout, stderr)
		}
	})
	t.Run("ST-GEN-UC12-R06-T02 Without apply, the check refuses a new field that has no add step", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		changed := noteBody(smallServiceDeclaration)
		s.write(t, "service.yaml", changed)
		result, code := s.compatJSON(t)
		if code != 1 || len(result.Faults) != 1 {
			t.Fatalf("exit %d with %+v, want 1 and one fault", code, result.Faults)
		}
		fault := result.Faults[0]
		if fault.Entity != "Note" || fault.Field != "body" || fault.Rule != "ST-GEN-INV33" || len(fault.Declare) != 1 ||
			fault.Declare[0].ID != "add-note-body" || fault.Declare[0].Kind != "add" || fault.Declare[0].Phase != "expand" {
			t.Errorf("the fault is %+v, want Note, body, ST-GEN-INV33 and add-note-body of kind add at expand", fault)
		}
		if s.read(t, "service.yaml") != changed {
			t.Error("stego compat changed service.yaml")
		}
	})
}

// sha256Text gives the SHA-256 of a text in hexadecimal.
func sha256Text(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
