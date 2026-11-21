package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/types"
)

// exitStatus is an error that ends stego with its own exit status, after the command printed what it found.
type exitStatus struct {
	code   int
	reason string
}

func (e exitStatus) Error() string { return e.reason }

// compatUsage is the exit status of stego compat for a usage error or a base ref that it cannot read.
const compatUsage = 2

// compatGitTimeout bounds each git command that stego compat runs.
const compatGitTimeout = time.Minute

// runCompat implements stego compat (ST-GEN-CLI07): it reads the declaration and the record at the base ref from
// the service's repository, checks the head declaration in the working folder against them (ST-GEN-QRY04), and
// prints the base ref and its commit and each fault. It exits 0 with no fault, 1 with a fault, and 2 for a usage
// error or a ref that it cannot read. It writes nothing in the service folder. To compare the committed migrate.go
// with a new generation (warnUngenerated), it resolves the registry, which can clone it into the cache of the user,
// and runs go list, as stego plan does.
func runCompat(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("compat", flag.ContinueOnError)
	baseRef := fs.String("base", "", "the commit or branch of the version that runs now")
	structured := fs.Bool("json", false, "print the base and the faults as one JSON object")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return exitStatus{compatUsage, err.Error()}
	}
	if fs.NArg() != 0 || *baseRef == "" {
		return exitStatus{compatUsage, "compat takes --base <ref> and no argument: stego compat --base <ref> [--json]"}
	}
	if strings.HasPrefix(*baseRef, "-") {
		return exitStatus{compatUsage, fmt.Sprintf("the base ref %q starts with -, which git reads as an option", *baseRef)}
	}
	base, err := readBase(*baseRef)
	if err == nil {
		err = refuseMovedService(base)
	}
	if err != nil {
		return exitStatus{compatUsage, err.Error()}
	}
	head, err := parser.ParseServiceDeclaration("service.yaml")
	if err != nil {
		return err
	}
	check, err := compiler.CheckBase(base, head)
	if err != nil {
		return err
	}
	// compat generates nothing, so it takes the generated files of the head from the record of the head, which
	// stego apply writes and the drift check holds to the generated files (ST-GEN-INV38).
	headState, err := compiler.LoadState(".stego/state.yaml")
	if err != nil {
		return err
	}
	if headState.LastApplied != nil {
		bodies, err := compiler.MigrationBodyFaults(base, headState.LastApplied.Files, recordedMigrateFiles(headState.LastApplied.Files))
		if err != nil {
			return err
		}
		check.Faults = append(check.Faults, bodies...)
		warnUngenerated(*baseRef, headState.LastApplied.Files, os.Stderr)
	}
	if *structured {
		document, err := json.MarshalIndent(check, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(document))
	} else {
		fmt.Fprint(stdout, formatBaseCheck(check))
	}
	if check.HasFaults() {
		return exitStatus{1, fmt.Sprintf("the head declaration has %d fault(s) against the base ref %s", len(check.Faults), check.BaseRef)}
	}
	return nil
}

// formatBaseCheck gives the check as text: the base it took, then each fault with its entity, field, rule and the
// schema changes to declare.
func formatBaseCheck(check compiler.BaseCheck) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Base: %s at commit %s, taken as the version that runs now.\n", check.BaseRef, check.BaseCommit)
	if !check.Released {
		out.WriteString("No fault: the base holds no record, so no release of the service has run, and any stored shape is accepted.\n")
		return out.String()
	}
	if !check.HasFaults() {
		out.WriteString("No fault: the head can run beside the base on a database with rows.\n")
		return out.String()
	}
	for _, fault := range check.Faults {
		where := fault.Entity
		if fault.Field != "" {
			where += "." + fault.Field
		}
		if where == "" {
			where = "declaration"
		}
		fmt.Fprintf(&out, "fault %s %s: %s\n", fault.Rule, where, fault.Message)
		for _, change := range fault.Declare {
			fmt.Fprintf(&out, "  declare: %s\n", change)
		}
	}
	return out.String()
}

// readBase reads the base ref from the Git repository that holds the working folder: the commit it resolves to,
// and service.yaml and .stego/state.yaml of the service folder at that commit. A file that the commit does not
// hold at that path is nil. Every other failure to read the commit or a file fails, so that an unreadable file is
// never taken as an absent one; a file that is no regular file, or that is larger than a declaration may be, is
// refused before it is read.
func readBase(ref string) (compiler.BaseSource, error) {
	base := compiler.BaseSource{Ref: ref}
	prefix, err := compatGit("rev-parse", "--show-prefix")
	if err != nil {
		return base, fmt.Errorf("the working folder is not in a Git repository, so the base ref %s cannot be read: %w", ref, err)
	}
	commit, err := compatGit("rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		if empty, emptyErr := noCommit(); emptyErr == nil && empty {
			return base, fmt.Errorf("the base ref %s names no commit of the service's repository, which has no commit: for the first release, run with no --base", ref)
		}
		return base, fmt.Errorf("the base ref %s names no commit of the service's repository", ref)
	}
	base.Commit = strings.TrimSpace(string(commit))
	folder := string(bytes.TrimSpace(prefix))
	for _, file := range []struct {
		name   string
		target *[]byte
	}{{"service.yaml", &base.Declaration}, {".stego/state.yaml", &base.Record}} {
		data, found, err := readBaseFile(base.Commit, folder+file.name)
		if err != nil {
			return base, fmt.Errorf("read %s at the base ref %s (commit %s): %w", folder+file.name, ref, base.Commit, err)
		}
		if found {
			*file.target = data
		}
	}
	return base, nil
}

// readBaseFile reads one file of a commit by its path from the root of the repository, and tells whether the
// commit holds it.
func readBaseFile(commit, name string) ([]byte, bool, error) {
	listing, err := compatGit("ls-tree", "-z", "--full-tree", commit, "--", name)
	if err != nil {
		return nil, false, err
	}
	if len(listing) == 0 {
		return nil, false, nil
	}
	entry, path, ok := strings.Cut(strings.TrimSuffix(string(listing), "\x00"), "\t")
	fields := strings.Fields(entry)
	if !ok || path != name || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
		return nil, false, fmt.Errorf("the commit holds %s as %q, which is no regular file", name, entry)
	}
	size, err := compatGit("cat-file", "-s", fields[2])
	if err != nil {
		return nil, false, err
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(string(size)), 10, 64); err != nil || n > parser.MaxDocumentBytes {
		return nil, false, fmt.Errorf("the file has %s bytes; at most %d are read", strings.TrimSpace(string(size)), parser.MaxDocumentBytes)
	}
	data, err := compatGit("cat-file", "blob", fields[2])
	if err != nil {
		return nil, false, err
	}
	return append([]byte{}, data...), true, nil
}

// refuseMovedService refuses a base that holds no service.yaml at the path of the service folder while a release of
// the service may have run: when the service in the working folder has a record, or when the base tree holds a
// record of a release at another path. The folder moved, or the base is not a version of this service, and the check
// cannot take an absent declaration as a first release.
func refuseMovedService(base compiler.BaseSource) error {
	if base.Declaration != nil {
		return nil
	}
	state, err := compiler.LoadState(".stego/state.yaml")
	if err != nil {
		return err
	}
	if state.LastApplied != nil {
		return fmt.Errorf("the base ref %s (commit %s) holds no service.yaml at the path of this service folder, and the service has a record, so a release of it has run: if the folder moved, run the check from the folder at the path that the base holds, or name a base that holds this folder", base.Ref, base.Commit)
	}
	released, err := releasedFolders(base.Commit)
	if err != nil {
		return fmt.Errorf("read the records of the base ref %s (commit %s): %w", base.Ref, base.Commit, err)
	}
	if len(released) > 0 {
		return fmt.Errorf("the base ref %s (commit %s) holds no service.yaml at the path of this service folder, and it holds the record of a release in %s: if the service folder moved, run the check from the folder at the path that the base holds", base.Ref, base.Commit, strings.Join(released, ", "))
	}
	return nil
}

// maxBaseRecords bounds the records that releasedFolders reads in one base tree.
const maxBaseRecords = 64

// releasedFolders gives each folder of the commit whose .stego/state.yaml records a release. A record that cannot
// be read counts as one.
func releasedFolders(commit string) ([]string, error) {
	listing, err := compatGit("ls-tree", "-r", "-z", "--name-only", "--full-tree", commit)
	if err != nil {
		return nil, err
	}
	var folders []string
	read := 0
	for _, name := range strings.Split(strings.TrimSuffix(string(listing), "\x00"), "\x00") {
		if name != ".stego/state.yaml" && !strings.HasSuffix(name, "/.stego/state.yaml") {
			continue
		}
		if read++; read > maxBaseRecords {
			return nil, fmt.Errorf("the commit holds more than %d records", maxBaseRecords)
		}
		data, found, err := readBaseFile(commit, name)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if released, err := compiler.RecordsRelease(data, name); err != nil || released {
			folder := strings.TrimSuffix(strings.TrimSuffix(name, ".stego/state.yaml"), "/")
			if folder == "" {
				folder = "the root"
			}
			folders = append(folders, folder)
		}
	}
	return folders, nil
}

// compatGit runs one git command in the working folder with a time limit and gives its standard output. Git gets
// no GIT_ variable of the environment, so the caller's environment cannot point it at another repository or object
// store, and it follows no replace ref and fetches no missing object.
func compatGit(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), compatGitTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	var stdout, stderr bytes.Buffer
	var env []string
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "GIT_") {
			env = append(env, variable)
		}
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "LC_ALL=C")
	command.Stdout, command.Stderr, command.Env = &stdout, &stderr, env
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// defaultBaseRef gives the full name of the repository's default branch, which plan and apply check against when
// the author names no base (ST-GEN-UC12-R05): the branch that refs/remotes/origin/HEAD names, else a local main,
// else a local master. With no Git repository, or none of these, there is no base, and found is false. A symbolic
// ref that names anything but a branch of origin is refused, so that a crafted origin/HEAD cannot point the check at
// a tag, a note or a ref outside the remote.
func defaultBaseRef() (ref string, found bool, err error) {
	if _, err := compatGit("rev-parse", "--is-inside-work-tree"); err != nil {
		// Only git's own answer that the folder is in no repository means no repository; every other failure, such
		// as a repository of another owner or no git at all, fails rather than skip the check.
		if strings.Contains(err.Error(), "not a git repository") && !gitEntryAbove() {
			return "", false, nil
		}
		return "", false, fmt.Errorf("git cannot read the repository of the service folder, so the base check cannot run: %w", err)
	}
	if target, err := compatGit("symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil {
		name := strings.TrimSpace(string(target))
		if !strings.HasPrefix(name, "refs/remotes/origin/") || name == "refs/remotes/origin/HEAD" {
			return "", false, fmt.Errorf("refs/remotes/origin/HEAD names %q, which is no branch of origin; set it with git remote set-head origin --auto, or name the base with --base", name)
		}
		if _, err := compatGit("check-ref-format", name); err != nil {
			return "", false, fmt.Errorf("refs/remotes/origin/HEAD names %q, which is not a valid ref name; name the base with --base", name)
		}
		if _, err := compatGit("rev-parse", "--verify", "--quiet", "--end-of-options", name+"^{commit}"); err != nil {
			return "", false, fmt.Errorf("refs/remotes/origin/HEAD names %s, which names no commit; fetch origin, or name the base with --base", name)
		}
		return name, true, nil
	}
	for _, name := range []string{"refs/heads/main", "refs/heads/master"} {
		if _, err := compatGit("rev-parse", "--verify", "--quiet", "--end-of-options", name+"^{commit}"); err == nil {
			return name, true, nil
		}
	}
	if empty, err := noCommit(); err != nil {
		return "", false, err
	} else if empty {
		return "", false, errNoCommit
	}
	return "", false, fmt.Errorf("the repository of the service folder has no default branch: refs/remotes/origin/HEAD is not set and there is no branch main or master; name the base with --base")
}

// errNoCommit is the answer of defaultBaseRef for a repository with no commit, whose default branch is not born: it
// holds no base, so no record of a release.
var errNoCommit = errors.New("the repository of the service folder has no commit")

// noCommit tells whether the repository of the working folder is a new repository before its first commit: HEAD
// names no commit (its branch is not born), no ref exists, and the repository is not shallow. A checkout by commit,
// such as git fetch <url> <ref> and git checkout FETCH_HEAD, has a commit at HEAD and no ref, so "no ref" alone is
// not "no commit": such a checkout has a base that the check cannot find, and it is refused rather than taken as a
// first release (finding H1 of the security review of PR 318). Also, HEAD can be unborn with no ref while the object
// store holds the commits of a fetch, as after git fetch <url> <ref> and git checkout FETCH_HEAD -- ., git restore
// --source=FETCH_HEAD, git read-tree -u FETCH_HEAD, or an orphan branch with the other branches deleted: a
// repository whose object store holds a commit, or that has a FETCH_HEAD, is not a new repository (the medium
// finding of round 2 of that review).
func noCommit() (bool, error) {
	if _, err := compatGit("rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil {
		return false, nil
	}
	refs, err := compatGit("for-each-ref", "--count=1", "--format=%(refname)")
	if err != nil {
		return false, fmt.Errorf("git cannot read the refs of the repository of the service folder, so the base check cannot run: %w", err)
	}
	if len(bytes.TrimSpace(refs)) != 0 {
		return false, nil
	}
	shallow, err := compatGit("rev-parse", "--is-shallow-repository")
	if err != nil {
		return false, fmt.Errorf("git cannot tell whether the repository of the service folder is shallow, so the base check cannot run: %w", err)
	}
	if strings.TrimSpace(string(shallow)) != "false" {
		return false, nil
	}
	fetchHead, err := compatGit("rev-parse", "--git-path", "FETCH_HEAD")
	if err != nil {
		return false, fmt.Errorf("git cannot name the FETCH_HEAD of the repository of the service folder, so the base check cannot run: %w", err)
	}
	if _, err := os.Lstat(strings.TrimSpace(string(fetchHead))); err == nil || !errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	types, err := compatGit("cat-file", "--batch-all-objects", "--unordered", "--batch-check=%(objecttype)")
	if err != nil {
		return false, fmt.Errorf("git cannot list the objects of the repository of the service folder, so the base check cannot run: %w", err)
	}
	for _, objectType := range strings.Split(string(types), "\n") {
		if objectType == "commit" {
			return false, nil
		}
	}
	return true, nil
}

// gitEntryAbove tells whether a .git entry exists in the working folder or in a folder above it, up to the root. A
// .git that git cannot read, such as the link of a worktree that was removed, is a broken repository, not the
// absence of one, so the check fails rather than skip.
func gitEntryAbove() bool {
	logical, err := os.Getwd()
	if err != nil {
		return true
	}
	// git walks the physical path, and the working folder can be a symbolic link, so both paths are walked.
	physical, err := filepath.EvalSymlinks(logical)
	if err != nil {
		return true
	}
	return gitEntryOnPath(logical) || gitEntryOnPath(physical)
}

// gitEntryOnPath tells whether a .git entry exists in the folder or in a folder above it, up to the root.
func gitEntryOnPath(folder string) bool {
	for {
		if _, err := os.Lstat(filepath.Join(folder, ".git")); err == nil || !errors.Is(err, os.ErrNotExist) {
			return true
		}
		parent := filepath.Dir(folder)
		if parent == folder {
			return false
		}
		folder = parent
	}
}

// baseGate is what plan and apply learn from the check against the base before they plan: the base they took, and
// the add steps that apply writes into the declaration (ST-GEN-UC12-R06).
type baseGate struct {
	ref     string
	checked bool
	// base is the declaration and the record at the base, which the check of the bodies of the released
	// migrations reads after the plan (ST-GEN-INV38).
	base  compiler.BaseSource
	steps []types.SchemaChange
	data  []byte
	// head is the declaration as plan and apply read it, and newSteps the positions, from 1, of the steps of its
	// schema history that the base does not hold (ST-GEN-UC12-R01).
	head     *types.ServiceDeclaration
	newSteps []int
}

// baseRemedy gives what the author does about the faults of the base check: declare the schema changes that it names
// when a fault names one, or else correct each fault as its text says, as a base that was changed without stego apply
// needs no schema change (walkthrough 7, rank 3). A base whose declaration is not the one its record names is often
// a branch that holds a declaration committed before apply, while the version that runs is a release tag: the remedy
// then also names --base <release tag> (walkthrough 8 and 9, rank 3).
func baseRemedy(check compiler.BaseCheck) string {
	record := false
	for _, fault := range check.Faults {
		if len(fault.Declare) > 0 {
			return "declare the schema changes that it names"
		}
		record = record || fault.Rule == compiler.BaseRecordRule
	}
	if record {
		return fmt.Sprintf("correct each fault as its text says; or, if the version that runs is a release tag and not %s, name that tag with --base <release tag>", check.BaseRef)
	}
	return "correct each fault as its text says"
}

// checkBaseBeforePlan runs the check against the base for plan and apply (ST-GEN-UC12-R05): against the ref the
// author names, or the default branch. It reads service.yaml in the working folder, adds the missing add steps to
// it in memory, and refuses, with the faults, a declaration that the check refuses then. It writes nothing.
func checkBaseBeforePlan(named string, stdout io.Writer) (baseGate, error) {
	gate := baseGate{ref: named}
	if named == "" {
		ref, found, err := defaultBaseRef()
		if errors.Is(err, errNoCommit) {
			if err := firstRelease(stdout); err != nil {
				return gate, err
			}
			return gate, refuseUncheckedSeal()
		}
		if err != nil {
			return gate, err
		}
		if !found {
			fmt.Fprintln(stdout, "No base check: the service folder is in no Git repository, and --base names no base.")
			return gate, refuseUncheckedSeal()
		}
		gate.ref = ref
	} else if strings.HasPrefix(named, "-") {
		return gate, fmt.Errorf("the base ref %q starts with -, which git reads as an option", named)
	}
	base, err := readBase(gate.ref)
	if err == nil {
		err = refuseMovedService(base)
	}
	if err != nil {
		return gate, err
	}
	data, err := parser.ReadDocument("service.yaml")
	if err != nil {
		return gate, err
	}
	head, err := parser.ParseServiceDeclarationFromBytes(data, "service.yaml")
	if err != nil {
		return gate, err
	}
	steps, err := compiler.MissingAddSteps(base, head)
	if err != nil {
		return gate, err
	}
	check, err := compiler.CheckBase(base, compiler.WithSchemaSteps(head, steps))
	if err != nil {
		return gate, err
	}
	gate.checked = true
	gate.base = base
	if check.HasFaults() {
		fmt.Fprint(stdout, formatBaseCheck(check))
		return gate, fmt.Errorf("the check against the base ref %s refuses the declaration, so nothing is written: %s", gate.ref, baseRemedy(check))
	}
	if check.Released {
		fmt.Fprintf(stdout, "Base: %s at commit %s; the declaration can run beside it.\n", gate.ref, base.Commit)
	} else {
		fmt.Fprintf(stdout, "Base: %s at commit %s holds no record, so no release has run, and any stored shape is accepted.\n", gate.ref, base.Commit)
		if err := refuseUncheckedSeal(); err != nil {
			return gate, err
		}
	}
	if len(steps) > 0 {
		gate.steps = steps
		if gate.data, err = compiler.AppendSchemaSteps(data, steps); err != nil {
			return gate, err
		}
	}
	gate.head = head
	if gate.newSteps, err = compiler.NewSchemaSteps(base, head); err != nil {
		return gate, err
	}
	return gate, nil
}

// refuseUncheckedSeal refuses plan and apply with no check against the base of a release when the declaration in the
// working folder declares a seal whose contract step it does not declare: a seal moves rows that a release before
// wrote, and only the check against the record of that release refuses a seal over a release whose processes do not
// report themselves, whose writes the contract would drop (security re-review of PR #443, N1).
func refuseUncheckedSeal() error {
	data, err := parser.ReadDocument("service.yaml")
	if err != nil {
		return err
	}
	head, err := parser.ParseServiceDeclarationFromBytes(data, "service.yaml")
	if err != nil {
		return err
	}
	contracted := map[string]bool{}
	for _, step := range head.SchemaHistory {
		if step.Phase == "contract" {
			contracted[step.Change] = true
		}
	}
	for _, change := range head.SchemaChanges {
		if change.Kind == "seal" && !contracted[change.ID] {
			return fmt.Errorf("the declaration has the open seal %s, which moves rows that a release before wrote, and no check against the record of that release ran: give --base <release tag or commit of the release that runs now>", change.ID)
		}
	}
	return nil
}

// firstRelease lets plan and apply go on with no base check in a repository with no commit, when the service has no
// record of a release: no release of the service has run, so any stored shape is accepted (ST-GEN-UC14-R02,
// decision on finding G1 of the walkthrough re-run, 2026-11-14). When the service has a record, a release may have
// run, and the base check cannot be skipped: the author commits the service first.
func firstRelease(stdout io.Writer) error {
	state, err := compiler.LoadState(".stego/state.yaml")
	if err != nil {
		return err
	}
	if state.LastApplied != nil {
		return fmt.Errorf("the repository of the service folder has no commit, and the service has a record, so a release of it may have run: commit the service folder with its record, or name the base with --base")
	}
	fmt.Fprintln(stdout, "No base check: the repository has no commit and the service has no record, so this is the first release. Commit the service folder with its record before the next release.")
	return nil
}

// printNewSchemaSteps lists each schema step of the declaration that the base does not hold, with its change, its
// kind, its phase, and the migration, the trigger and the backfill that it adds (ST-GEN-UC12-R01). The migration is
// the file of the plan that holds the step's SQL. An add adds no trigger and no backfill; a rename at expand adds the
// sync trigger and the backfill, which the migration job runs under its lease before the rollout (decision on finding
// F5 of the schema-change research, owner, 2026-11-13).
func printNewSchemaSteps(stdout io.Writer, gate baseGate, plan *compiler.Plan) {
	if gate.head == nil || len(gate.newSteps) == 0 {
		return
	}
	kinds := map[string]types.SchemaChange{}
	for _, change := range gate.head.SchemaChanges {
		kinds[change.ID] = change
	}
	fmt.Fprintln(stdout, "\nNew schema steps:")
	for _, position := range gate.newSteps {
		step := gate.head.SchemaHistory[position-1]
		change := kinds[step.Change]
		target := change.Entity
		if change.Field != "" {
			target += "." + change.Field
		}
		migration := "none"
		name := postgresadapter.StepMigrationName(position, step.Change, step.Phase)
		if plan != nil {
			for _, file := range plan.Files {
				if strings.HasSuffix(file.Path, "/migrations/"+name+".sql") {
					migration = file.Path
				}
			}
		}
		trigger, backfill := postgresadapter.StepTrigger(change, step.Phase), postgresadapter.StepBackfill(change, step.Phase)
		if trigger == "" {
			trigger = "none"
		}
		switch {
		case change.Kind == "seal" && step.Phase == "expand":
			// The seal job of the controller seals the rows, as the runtime role with the key ring (ST-SUP-RP10-LC50).
			backfill = "the seal job of the controller under the lease " + postgresadapter.BackfillLease(change.ID)
		case backfill == "":
			backfill = "none"
		default:
			backfill = "in the migration job under the lease " + backfill
		}
		fmt.Fprintf(stdout, "  %d. %s: %s %s at %s; migration %s; trigger %s; backfill %s\n", position, step.Change, change.Kind, target, step.Phase, migration, trigger, backfill)
	}
}

// printOpenSeals warns of each seal whose expand step the base holds and whose contract step the head does not
// declare: the clear column holds the secret, in the table, the WAL and each backup, until the contract step, so the
// contract belongs in the next release (security review of PR #443, L2).
func printOpenSeals(stdout io.Writer, gate baseGate) {
	if gate.head == nil {
		return
	}
	isNew := map[int]bool{}
	for _, position := range gate.newSteps {
		isNew[position] = true
	}
	kinds := map[string]string{}
	for _, change := range gate.head.SchemaChanges {
		kinds[change.ID] = change.Kind
	}
	expanded, contracted := map[string]bool{}, map[string]bool{}
	for i, step := range gate.head.SchemaHistory {
		if kinds[step.Change] != "seal" {
			continue
		}
		switch {
		case step.Phase == "contract":
			contracted[step.Change] = true
		case !isNew[i+1]:
			expanded[step.Change] = true
		}
	}
	for _, step := range gate.head.SchemaHistory {
		if expanded[step.Change] && !contracted[step.Change] {
			fmt.Fprintf(stdout, "warning: the seal %s is open since a release before: its clear column still holds the secret in the table, the WAL and each backup; declare its contract step in this release when the seal job has completed a pass\n", step.Change)
			delete(expanded, step.Change)
		}
	}
}

// printSchemaSteps names each add step that apply writes, or wrote, into the declaration.
func printSchemaSteps(stdout io.Writer, verb string, steps []types.SchemaChange) {
	for _, step := range steps {
		target := step.Entity
		if step.Field != "" {
			target += "." + step.Field
		}
		fmt.Fprintf(stdout, "%s schema step %s: add %s at expand\n", verb, step.ID, target)
	}
}

// plannedMigrateFiles gives the content of each generated migrate.go of the adapter that the plan writes, by its path
// in the output folder, from which the check of a changed body reads the earlier digests that the head registers
// (ST-GEN-INV38, ST-SUP-RC01-OC51).
func plannedMigrateFiles(plan *compiler.Plan) map[string][]byte {
	files := map[string][]byte{}
	for _, file := range plan.GeneratedFiles {
		if path.Base(file.Path) == postgresadapter.MigrateFile {
			files[file.Path] = file.Bytes()
		}
	}
	return files
}

// recordedMigrateFiles gives the content of each generated migrate.go of the adapter in the out folder that the
// record of the head lists, by its path in the output folder, and only when the file holds the digest that the record
// lists: compat generates nothing, so a file that apply did not write gives no earlier digest (ST-GEN-INV38).
func recordedMigrateFiles(recorded map[string]string) map[string][]byte {
	files := map[string][]byte{}
	for name, digest := range recorded {
		if path.Base(name) != postgresadapter.MigrateFile {
			continue
		}
		content, err := os.ReadFile(filepath.Join("out", filepath.FromSlash(name)))
		if err == nil && compiler.HashBytes(content) == digest {
			files[name] = content
		}
	}
	return files
}

// warnUngenerated warns on stderr when a generated migrate.go of the adapter in the out folder, or its digest in the
// record of the head, is not what a new generation of the head gives (security review of PR #476, L1). compat checks
// the committed files, which an author can change by hand together with their digests, so the gate of ST-GEN-INV38
// in CI is stego plan --base, which generates the head again. The warning changes no fault and no exit status. When
// the head does not generate here, such as with no registry, compat says that it did not compare.
func warnUngenerated(ref string, recorded map[string]string, stderr io.Writer) {
	// The note gives a fixed class of the failure, never the error text, which can hold the URL of the registry with
	// its credentials (security review of PR #482, L2).
	input, err := buildReconcilerInput()
	class := "registry"
	var plan *compiler.Plan
	if err == nil {
		class = "generation"
		plan, err = compiler.Reconcile(input)
	}
	if err != nil {
		fmt.Fprintf(stderr, "note: compat did not generate the head (cause: %s), so it did not compare the committed migrate.go with a new generation; run stego plan --base %s, which generates it and holds ST-GEN-INV38, to see the cause.\n", class, ref)
		return
	}
	var differ []string
	for name, content := range plannedMigrateFiles(plan) {
		committed, err := os.ReadFile(filepath.Join(input.OutDir, filepath.FromSlash(name)))
		if err != nil || !bytes.Equal(committed, content) || recorded[name] != compiler.HashBytes(content) {
			differ = append(differ, "out/"+name)
		}
	}
	if len(differ) == 0 {
		return
	}
	sort.Strings(differ)
	fmt.Fprintf(stderr, "warning: %s, or its digest in .stego/state.yaml, is not what a new generation of the head gives, and compat checks the committed files. Run stego apply and commit what it writes. In CI, stego plan --base %s is the gate of ST-GEN-INV38, as it generates the head again.\n", strings.Join(differ, ", "), ref)
}

// refuseChangedBodies refuses a plan whose generated SQL file of a ledgered migration that the record at the base
// lists has another digest than that record lists (ST-GEN-INV38), for plan and apply before anything is written. A
// gate that checked no base refuses nothing.
func refuseChangedBodies(gate baseGate, plan *compiler.Plan, stdout io.Writer) error {
	if !gate.checked || plan == nil || plan.NewState == nil || plan.NewState.LastApplied == nil {
		return nil
	}
	faults, err := compiler.MigrationBodyFaults(gate.base, plan.NewState.LastApplied.Files, plannedMigrateFiles(plan))
	if err != nil || len(faults) == 0 {
		return err
	}
	fmt.Fprint(stdout, formatBaseCheck(compiler.BaseCheck{BaseRef: gate.ref, BaseCommit: gate.base.Commit, Released: true, Faults: faults}))
	return fmt.Errorf("the check against the base ref %s refuses the changed body of %d released migration(s), so nothing is written: keep each body, and bring the change in a new schema step", gate.ref, len(faults))
}
