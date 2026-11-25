package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// evolutionFixture is the folder of the TD04 harness: the seed of the database and the check that runs the service
// binaries of the releases on it.
const evolutionFixture = "testdata/schema_evolution"

// evolutionSpiceDBImage is the pinned SpiceDB image of the harness.
const evolutionSpiceDBImage = "authzed/spicedb:v1.57.0@sha256:a170d372c9f6b921bb6849064df6ca3566dd5b174882c1a2c4300c2ae1d60efd"

// evolution is ST-GEN-TD04: a git repository with the commits of examples/user-management at each release, the
// service binary of each release, and the folder of the last release, where the check runs.
type evolution struct {
	repo, bin string
	env       []string
	plans     map[string]string
}

// The declaration of v1 of ST-GEN-TD04, by the edit of the declaration of the example: the field OrgSetting.rank.
var evolutionV1 = [2]string{"      - { name: generation, type: int64 }\n", "      - { name: generation, type: int64 }\n      - { name: rank, type: int64 }\n"}

// The edits of v1 of ST-GEN-TD04 for the relate of a field: the optional field Organization.approver, which the
// public create and the update of v1 can set and change, as a release before the relationship does (walkthrough 25,
// P2-F); and the field Organization.quota, an int32 with min 0 and no max that the collection patches, which v2
// widens to an int64 (walkthrough 26, P3-RT).
var evolutionV1Approver = [][2]string{
	{"    permissions: {create: public, read: view, list: view, delete: edit}\n", "    permissions: {create: public, read: view, list: view, update: edit, patch: edit, delete: edit}\n"},
	{"      - { name: description, type: string, optional: true, max_length: 1000 }\n",
		"      - { name: description, type: string, optional: true, max_length: 1000 }\n      - { name: approver, type: string, optional: true, max_length: 255 }\n" +
			"      - { name: quota, type: int32, min: 0, default: 0 }\n"},
	{"    operations: [create, read, delete, list]\n", "    operations: [create, read, update, delete, list, patch]\n    patchable: [quota]\n"},
}

// The declaration of v2 of ST-GEN-TD04, by the edits of the declaration of v1: the add of User.nickname, the rename
// of User.email to contact_email, the retype of OrgSetting.rank from int64 to string, the require of User.locale and
// the widening of the patchable Organization.quota from int32 to int64, each at expand. The drop and the unique of ST-GEN-TD04 v2 wait for their builds (interim scope of the
// harness, accepted by the orchestrator, 2026-11-13).
var evolutionV2 = [][2]string{
	{"      - { name: email, type: string, unique: true, max_length: 255 }\n",
		"      - { name: contact_email, type: string, unique: true, max_length: 255 }\n      - { name: nickname, type: string, optional: true, max_length: 50 }\n" +
			"      - { name: locale, type: string, max_length: 10 }\n"},
	{"      - { name: rank, type: int64 }\n", "      - { name: rank, type: string, pattern: \"^-?[0-9]{1,18}$\" }\n"},
	{"      - { name: quota, type: int32, min: 0, default: 0 }\n", "      - { name: quota, type: int64, min: 0, default: 0 }\n"},
	{"\ncollections:\n", "\nschema_changes:\n  - {id: user-nickname-add, kind: add, entity: User, field: nickname}\n" +
		"  - {id: user-email-rename, kind: rename, entity: User, field: contact_email, from: email}\n" +
		"  - {id: orgsetting-rank-retype, kind: retype, entity: OrgSetting, field: rank, from: int64, conversion: int64_to_text, inverse_conversion: text_to_int64}\n" +
		"  - {id: user-locale-require, kind: require, entity: User, field: locale, fill_value: en}\n" +
		"  - {id: organization-quota-retype, kind: retype, entity: Organization, field: quota, from: int32, definition: \"int32, min 0, default 0\", conversion: integer_to_int64, inverse_conversion: int64_to_integer}\n" +
		"schema_history:\n  - {change: user-nickname-add, phase: expand}\n  - {change: user-email-rename, phase: expand}\n" +
		"  - {change: orgsetting-rank-retype, phase: expand}\n  - {change: user-locale-require, phase: expand}\n  - {change: organization-quota-retype, phase: expand}\n\ncollections:\n"},
}

// The declaration of v3 of ST-GEN-TD04, by the edit of the declaration of v2: the contract steps of the rename, the
// retype, the require and the widening at the end of the schema history (ST-GEN-UC15).
var evolutionV3 = [2]string{"  - {change: organization-quota-retype, phase: expand}\n",
	"  - {change: organization-quota-retype, phase: expand}\n  - {change: user-email-rename, phase: contract}\n" +
		"  - {change: orgsetting-rank-retype, phase: contract}\n  - {change: user-locale-require, phase: contract}\n  - {change: organization-quota-retype, phase: contract}\n"}

// newEvolution makes the repository of ST-GEN-TD04 with the commit v1, the example as it is, and the commit v2,
// which stego plan --base v1 and stego apply make from the declaration of v2, and builds the service binary of each
// with the pinned Go against the runtime module of this repository.
func newEvolution(t *testing.T) *evolution {
	t.Helper()
	stego := stegoBinary(t)
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	root := strings.TrimSpace(string(top))
	parent := t.TempDir()
	e := &evolution{repo: filepath.Join(parent, "user-management"), bin: filepath.Join(parent, "bin"), plans: map[string]string{}}
	home := filepath.Join(parent, ".home")
	// The module cache stays the one of the test run, so no build downloads into the temporary home.
	cache, err := exec.Command(pinnedGo(t), "env", "GOMODCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	e.env = append(os.Environ(), "HOME="+home, "GOMODCACHE="+strings.TrimSpace(string(cache)), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GOWORK=off", "GOTOOLCHAIN=local",
		"STEGO_REGISTRY="+filepath.Join(root, "registry"), "PATH="+filepath.Dir(pinnedGo(t))+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GIT_AUTHOR_NAME=td04", "GIT_AUTHOR_EMAIL=td04@example.test", "GIT_COMMITTER_NAME=td04", "GIT_COMMITTER_EMAIL=td04@example.test")
	if output, err := exec.Command("cp", "-a", filepath.Join(root, "examples", "user-management"), e.repo).CombinedOutput(); err != nil {
		t.Fatalf("copy the example: %v\n%s", err, output)
	}
	e.git(t, "init", "-q")
	// v1 is the example with the required field OrgSetting.rank, an int64, which v2 retypes: the example has no other
	// stored integer, and its generation is the counter that the server keeps.
	example, err := os.ReadFile(filepath.Join(e.repo, "service.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	v1 := string(example)
	for _, edit := range append([][2]string{evolutionV1}, evolutionV1Approver...) {
		if !strings.Contains(v1, edit[0]) {
			t.Fatalf("the declaration of the example has no %q to edit for v1", edit[0])
		}
		v1 = strings.Replace(v1, edit[0], edit[1], 1)
	}
	if err := os.WriteFile(filepath.Join(e.repo, "service.yaml"), []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}
	// No release of the example ran on the database of ST-GEN-TD04, so v1 is its first release, with no record, and
	// with no architecture model, which apply derives anew and would otherwise refuse as a file the record does not
	// hold.
	for _, file := range []string{filepath.Join(".stego", "state.yaml"), filepath.Join("architecture", "architecture.json")} {
		if err := os.Remove(filepath.Join(e.repo, file)); err != nil {
			t.Fatal(err)
		}
	}
	// The architecture model and view are project files of the removed record, so they go with it; else the
	// first apply finds them as hand edits.
	if err := os.RemoveAll(filepath.Join(e.repo, "architecture")); err != nil {
		t.Fatal(err)
	}
	// The files of the migration Job name the schema of the example, which v1 changes, so they go with the record
	// too (ST-DLV-UC07-R06).
	for _, file := range []string{"deploy/base/migration-job.yaml", "deploy/components/secret-files/remove-job-env.yaml", "deploy/components/secret-files/mount-job-files.yaml",
		"deploy/components/fs-group/job-fs-group.yaml"} {
		if err := os.Remove(filepath.Join(e.repo, filepath.FromSlash(file))); err != nil {
			t.Fatal(err)
		}
	}
	if out, failed := e.stego(t, stego, "apply"); failed != "" {
		t.Fatalf("stego apply of v1: %s\n%s", failed, out)
	}
	e.git(t, "add", "-A")
	e.git(t, "commit", "-q", "-m", "v1")
	e.git(t, "tag", "v1")
	e.build(t, "v1", root)
	// v1r is v1 with the relation tenant of organization and its relate step, a release beside v2 (decision
	// ST-SUP-Q212 (a); walkthrough 24, P2-R2). Its build runs while v2 and v3 are made.
	mainline := strings.TrimSpace(e.git(t, "rev-parse", "--abbrev-ref", "HEAD"))
	e.git(t, "checkout", "-q", "-b", "relate", "v1")
	for file, edits := range map[string][][2]string{"service.yaml": evolutionV1R, filepath.Join("authz", "schema.zed"): evolutionV1RSchema} {
		text, err := os.ReadFile(filepath.Join(e.repo, file))
		if err != nil {
			t.Fatal(err)
		}
		edited := string(text)
		for _, edit := range edits {
			if !strings.Contains(edited, edit[0]) {
				t.Fatalf("%s of v1 has no %q to edit for v1r", file, edit[0])
			}
			edited = strings.Replace(edited, edit[0], edit[1], 1)
		}
		if err := os.WriteFile(filepath.Join(e.repo, file), []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.plans["v1r plan"], e.plans["v1r plan error"] = e.stego(t, stego, "plan", "--base", "v1")
	if out, failed := e.stego(t, stego, "apply", "--base", "v1"); failed != "" {
		t.Fatalf("stego apply of v1r: %s\n%s", failed, out)
	}
	e.git(t, "add", "-A")
	e.git(t, "commit", "-q", "-m", "v1r")
	e.git(t, "tag", "v1r")
	e.git(t, "checkout", "-q", mainline)
	goBin := pinnedGo(t)
	relateBuilt := e.buildBeside(t, goBin, "v1r", root)
	declaration, err := os.ReadFile(filepath.Join(e.repo, "service.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	v2 := string(declaration)
	for _, edit := range evolutionV2 {
		if !strings.Contains(v2, edit[0]) {
			t.Fatalf("the declaration of the example has no %q to edit for v2", edit[0])
		}
		v2 = strings.Replace(v2, edit[0], edit[1], 1)
	}
	if err := os.WriteFile(filepath.Join(e.repo, "service.yaml"), []byte(v2), 0o644); err != nil {
		t.Fatal(err)
	}
	e.plans["v2 plan"], e.plans["v2 plan error"] = e.stego(t, stego, "plan", "--base", "v1")
	e.plans["v2 apply"], e.plans["v2 apply error"] = e.stego(t, stego, "apply", "--base", "v1")
	if e.plans["v2 apply error"] != "" {
		t.Fatalf("stego apply of v2: %s\n%s", e.plans["v2 apply error"], e.plans["v2 apply"])
	}
	e.git(t, "add", "-A")
	e.git(t, "commit", "-q", "-m", "v2")
	e.git(t, "tag", "v2")
	// The builds of v2 and v3 run beside the steps that follow, as the build of v1r does: no step needs their
	// binaries before the check.
	v2Built := e.buildBeside(t, goBin, "v2", root)
	committed, err := os.ReadFile(filepath.Join(e.repo, "service.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(committed), evolutionV3[0]) {
		t.Fatalf("the declaration of v2 has no %q to edit for v3", evolutionV3[0])
	}
	v3 := strings.Replace(string(committed), evolutionV3[0], evolutionV3[1], 1)
	if err := os.WriteFile(filepath.Join(e.repo, "service.yaml"), []byte(v3), 0o644); err != nil {
		t.Fatal(err)
	}
	e.plans["v3 plan"], e.plans["v3 plan error"] = e.stego(t, stego, "plan", "--base", "v2")
	e.plans["v3 apply"], e.plans["v3 apply error"] = e.stego(t, stego, "apply", "--base", "v2")
	if e.plans["v3 apply error"] != "" {
		t.Fatalf("stego apply of v3: %s\n%s", e.plans["v3 apply error"], e.plans["v3 apply"])
	}
	e.git(t, "add", "-A")
	e.git(t, "commit", "-q", "-m", "v3")
	e.git(t, "tag", "v3")
	v3Built := e.buildBeside(t, goBin, "v3", root)
	// A head that removes the finished change and its steps: the check against v3 refuses it, as only a new
	// baseline may fold a finished change (ST-GEN-UC15-R03).
	removed := strings.Replace(strings.Replace(strings.Replace(v3, "  - {id: user-email-rename, kind: rename, entity: User, field: contact_email, from: email}\n", "", 1),
		"  - {change: user-email-rename, phase: expand}\n", "", 1), "  - {change: user-email-rename, phase: contract}\n", "", 1)
	if err := os.WriteFile(filepath.Join(e.repo, "service.yaml"), []byte(removed), 0o644); err != nil {
		t.Fatal(err)
	}
	e.plans["removed compat"], e.plans["removed compat error"] = e.stego(t, stego, "compat", "--base", "v3")
	e.git(t, "checkout", "-q", "--", "service.yaml")
	// A head that renames email with no schema change: plan refuses it and writes nothing.
	head := strings.Replace(string(declaration), evolutionV2[0][0], strings.Replace(evolutionV2[0][0], "email", "contact_email", 1), 1)
	if err := os.WriteFile(filepath.Join(e.repo, "service.yaml"), []byte(head), 0o644); err != nil {
		t.Fatal(err)
	}
	e.plans["undeclared plan"], e.plans["undeclared plan error"] = e.stego(t, stego, "plan", "--base", "v1")
	e.plans["undeclared status"] = e.git(t, "status", "--porcelain", "--untracked-files=all")
	e.git(t, "checkout", "-q", "--", "service.yaml")
	// The builds of v1r, v2 and v3 run beside each other and beside the steps above (they take about the time of one
	// build of the package). Each one is waited for, so no build outlives the test.
	for _, built := range []func() error{relateBuilt, v2Built, v3Built} {
		if err := built(); err != nil {
			t.Error(err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	return e
}

// The edits of v1r of ST-GEN-TD04: the relate step of the relation tenant of Organization, and the relation in the
// schema, through which the auditor of a tenant views each organization of the tenant; and the relate step of the
// relation approver from the field approver (walkthrough 25, P2-F). A field-sourced relationship needs a create that
// decides a permission (ST-SUP-Q118), so the create of v1r is decided on create_organization of the service-level
// resource, and the approver of an organization views it.
var (
	evolutionV1R = [][2]string{
		{"\ncollections:\n", "\nschema_changes:\n  - {id: organization-tenant-relate, kind: relate, entity: Organization, relation: tenant}\n" +
			"  - {id: organization-approver-relate, kind: relate, entity: Organization, relation: approver}\n" +
			"schema_history:\n  - {change: organization-tenant-relate, phase: expand}\n  - {change: organization-approver-relate, phase: expand}\n\ncollections:\n"},
		{"    permissions: {create: public, read: view, list: view, update: edit, patch: edit, delete: edit}\n    public_reasons: {create: a new organization has no resource to decide on before it exists}\n",
			"    permissions: {create: create_organization, read: view, list: view, update: edit, patch: edit, delete: edit}\n" +
				"    relationships:\n      - {relation: approver, subject_source: field, subject_field: approver, subject_type: user}\n"},
	}
	evolutionV1RSchema = [][2]string{
		{"definition organization {\n\trelation owner: user\n\trelation viewer: user\n\tpermission view = owner + viewer\n",
			"definition tenant {\n\trelation auditor: user\n}\n\ndefinition organization {\n\trelation owner: user\n\trelation viewer: user\n\trelation tenant: tenant\n" +
				"\trelation approver: user\n\tpermission view = owner + viewer + tenant->auditor + approver\n"},
		{"\tpermission list_tenants = operator\n", "\tpermission list_tenants = operator\n\trelation organization_creator: user\n\tpermission create_organization = organization_creator\n"},
	}
)

// git runs git in the repository and gives its output.
func (e *evolution) git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = e.repo, e.env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

// stego runs the stego binary in the repository and gives its output and its error, or "".
func (e *evolution) stego(t *testing.T, stego string, args ...string) (string, string) {
	t.Helper()
	cmd := exec.Command(stego, args...)
	cmd.Dir, cmd.Env = e.repo, e.env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), err.Error()
	}
	return string(output), ""
}

// build builds the service binary of the release at the tag into bin/<tag>, from a copy of the commit, against the
// runtime module of this repository, as each commit of ST-GEN-TD04 uses the registry at the commit under test.
func (e *evolution) build(t *testing.T, tag, root string) {
	t.Helper()
	if err := e.buildErr(t, tag, root); err != nil {
		t.Fatal(err)
	}
}

// buildErr is build, which gives its failure as an error, so it can run beside other steps.
func (e *evolution) buildErr(t *testing.T, tag, root string) error {
	return e.buildWith(pinnedGo(t), tag, root)
}

// buildWith is build with the Go binary goBin; it calls no method of a testing.T, so it can run in a goroutine.
func (e *evolution) buildWith(goBin, tag, root string) error {
	dir := filepath.Join(filepath.Dir(e.repo), "build-"+tag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	archive := exec.Command("sh", "-c", `git archive "$1" | tar -x -C "$2"`, "sh", tag, dir)
	archive.Dir, archive.Env = e.repo, e.env
	if output, err := archive.CombinedOutput(); err != nil {
		return fmt.Errorf("check out %s: %v\n%s", tag, err, output)
	}
	for _, args := range [][]string{
		{"mod", "edit", "-replace", "github.com/jsell-rh/stego/registry/runtime=" + filepath.Join(root, "registry", "runtime")},
		{"mod", "tidy"},
		{"build", "-o", filepath.Join(e.bin, tag), "./out"},
	} {
		cmd := exec.Command(goBin, args...)
		cmd.Dir, cmd.Env = dir, e.env
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("go %s for %s: %v\n%s", strings.Join(args, " "), tag, err, output)
		}
	}
	return nil
}

// buildBeside starts build of the release at the tag in a goroutine and gives a function that waits for the build and
// gives its error. The test waits for the build when it ends too, also after a failure before the wait, so no build
// writes in the folder of the test after the test removed it.
func (e *evolution) buildBeside(t *testing.T, goBin, tag, root string) func() error {
	t.Helper()
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		err = e.buildWith(goBin, tag, root)
	}()
	t.Cleanup(func() { <-done })
	return func() error {
		<-done
		return err
	}
}

// startEvolutionSpiceDB starts a local SpiceDB container, unprivileged, with the memory datastore, and gives its
// address and preshared key. The container is removed when the test ends.
func startEvolutionSpiceDB(t *testing.T) (string, string) {
	t.Helper()
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	name, key := "stego-td04-spicedb-"+hex.EncodeToString(suffix), "td04-"+hex.EncodeToString(suffix)
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name, "--memory", "512m", "--cpus", "1",
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL", "--stop-timeout", "1",
		"-p", "127.0.0.1::50051", evolutionSpiceDBImage, "serve", "--grpc-preshared-key", key, "--datastore-engine", "memory")
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run SpiceDB: %v\n%s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		out, err := exec.Command("docker", "port", name, "50051/tcp").Output()
		if err == nil && len(out) > 0 {
			return strings.TrimSpace(strings.Split(string(out), "\n")[0]), key
		}
	}
	t.Fatal("the SpiceDB container publishes no port")
	return "", ""
}

// userSchema gives the schema of User in the OpenAPI document of the release at the tag.
func (e *evolution) userSchema(t *testing.T, tag string) map[string]any {
	t.Helper()
	var document struct {
		Components struct {
			Schemas map[string]map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal([]byte(e.git(t, "show", tag+":out/internal/api/openapi.json")), &document); err != nil {
		t.Fatal(err)
	}
	schema, ok := document.Components.Schemas["User"]
	if !ok {
		t.Fatalf("the OpenAPI document of %s has no schema User", tag)
	}
	return schema
}

// TestSchemaEvolution runs ST-GEN-TD04: it makes the repository with the releases v1, v2 and v3, builds their
// service binaries, and runs the check of testdata/schema_evolution/check in the module of v2 once, against the local
// PostgreSQL server of STEGO_TEST_POSTGRES_DSN and a local SpiceDB container. A missing server fails the test: a
// missing result is not a pass.
func TestSchemaEvolution(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the releases of the example user-management")
	}
	// The test has its own folder, SpiceDB container and databases, and no other test of the package uses the
	// PostgreSQL server, so it runs beside the other parallel tests.
	t.Parallel()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Fatal("STEGO_TEST_POSTGRES_DSN is not set: this test needs a local PostgreSQL server")
	}
	e := newEvolution(t)
	check, err := os.ReadFile(filepath.Join(evolutionFixture, "check", "evolution_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := filepath.Abs(filepath.Join(evolutionFixture, "seed.sql"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(filepath.Dir(e.repo), "build-v2")
	if err := os.MkdirAll(filepath.Join(dir, "out", "evolutioncheck"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out", "evolutioncheck", "evolution_test.go"), check, 0o644); err != nil {
		t.Fatal(err)
	}
	endpoint, key := startEvolutionSpiceDB(t)
	// STEGO_TD04_CHECK_RUN narrows the check to the tests it names, for a run by hand; CI runs every check.
	args := []string{"test", "-count=1", "-timeout=1500s", "-v"}
	if only := os.Getenv("STEGO_TD04_CHECK_RUN"); only != "" {
		args = append(args, "-run", only)
	}
	run := exec.Command(pinnedGo(t), append(args, "./out/evolutioncheck")...)
	run.Dir = dir
	run.Env = append(e.env, "STEGO_TEST_SPICEDB="+endpoint, "STEGO_TEST_SPICEDB_KEY="+key, "STEGO_TD04_SEED="+seed,
		"STEGO_TD04_V1="+filepath.Join(e.bin, "v1"), "STEGO_TD04_V2="+filepath.Join(e.bin, "v2"), "STEGO_TD04_V3="+filepath.Join(e.bin, "v3"),
		"STEGO_TD04_V1R="+filepath.Join(e.bin, "v1r"))
	var output bytes.Buffer
	run.Stdout, run.Stderr = &output, &output
	runErr := run.Run()
	passed := func(id string) bool {
		for _, line := range strings.Split(output.String(), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "--- PASS: ") && strings.Contains(line, "/"+id+"_") {
				return true
			}
		}
		return false
	}
	result := func(t *testing.T, id string) {
		t.Helper()
		if !passed(id) {
			t.Fatalf("the check %s did not run and pass (go test: %v):\n%s", id, runErr, output.String())
		}
	}

	t.Run("ST-GEN-UC12-R02-T01 After the expand migration the old column is as it was and the new one is nullable", func(t *testing.T) {
		result(t, "ST-GEN-UC12-R02-T01")
	})
	t.Run("ST-GEN-UC12-R03-T04 Each release reads what the other writes for a retype and a require at expand, and no value is lost", func(t *testing.T) {
		result(t, "ST-GEN-UC12-R03-T04")
	})
	t.Run("ST-GEN-UC12-R03-T05 A patchable int32 widened to an int64 keeps each value through both releases, and the new release holds the int32 range until the contract", func(t *testing.T) {
		if plan := e.plans["v2 plan"]; !strings.Contains(plan, "organization-quota-retype: retype Organization.quota at expand; migration internal/storage/migrations/step_0005_organization-quota-retype_expand.sql; trigger organizations_quota_sync; backfill in the migration job under the lease stego.backfill.organization-quota-retype") {
			t.Fatalf("stego plan --base v1 of v2 does not list the widening of Organization.quota:\n%s", plan)
		}
		result(t, "ST-GEN-UC12-R03-T05")
	})
	t.Run("ST-GEN-UC12-R04-T01 After the rename the API still names the field email", func(t *testing.T) {
		result(t, "ST-GEN-UC12-R04-T01")
		// The User schema of v2 is the one of v1 with the added field nickname, and holds email and no
		// contact_email.
		before, after := e.userSchema(t, "v1"), e.userSchema(t, "v2")
		properties := func(schema map[string]any) map[string]any {
			p, _ := schema["properties"].(map[string]any)
			return p
		}
		got := properties(after)
		if _, ok := got["contact_email"]; ok || got["email"] == nil || got["nickname"] == nil {
			t.Fatalf("the User schema of v2 has the properties %v, want email and nickname and no contact_email", got)
		}
		trimmed := map[string]any{}
		for k, v := range got {
			if k != "nickname" && k != "locale" {
				trimmed[k] = v
			}
		}
		// The require adds the required field locale (decision on ST-GEN-Q22 (a)).
		required, _ := after["required"].([]any)
		required = slices.DeleteFunc(slices.Clone(required), func(v any) bool { return v == "locale" })
		if !reflect.DeepEqual(trimmed, properties(before)) || !reflect.DeepEqual(required, before["required"]) || got["locale"] == nil {
			t.Fatalf("the User schema of v2 is not the one of v1 with nickname and the required locale:\n v1 %v\n v2 %v", before, after)
		}
	})
	t.Run("ST-GEN-UC12-FL-T The email rename starts while v1 and v2 both run on a table with rows", func(t *testing.T) {
		plan := e.plans["v2 plan"]
		if e.plans["v2 plan error"] != "" || !strings.Contains(plan, "user-email-rename: rename User.contact_email at expand; migration internal/storage/migrations/step_0002_user-email-rename_expand.sql; trigger users_contact_email_sync; backfill in the migration job under the lease stego.backfill.user-email-rename") {
			t.Fatalf("stego plan --base v1 of v2 (%s) does not list the rename with its kind, phase, migration, trigger and backfill:\n%s", e.plans["v2 plan error"], plan)
		}
		for _, line := range []string{"orgsetting-rank-retype: retype OrgSetting.rank at expand; migration internal/storage/migrations/step_0003_orgsetting-rank-retype_expand.sql; trigger org_settings_rank_sync; backfill in the migration job under the lease stego.backfill.orgsetting-rank-retype",
			"user-locale-require: require User.locale at expand; migration internal/storage/migrations/step_0004_user-locale-require_expand.sql; trigger none; backfill none"} {
			if !strings.Contains(plan, line) {
				t.Fatalf("stego plan --base v1 of v2 does not list %q:\n%s", line, plan)
			}
		}
		if refused := e.plans["undeclared plan"]; e.plans["undeclared plan error"] == "" || !strings.Contains(refused, "rename") || e.plans["undeclared status"] != " M service.yaml\n" {
			t.Fatalf("the plan of a head that renames email with no schema change exits with %q, leaves %q, and says:\n%s\nwant a refusal that names a schema change of kind rename, and nothing written",
				e.plans["undeclared plan error"], e.plans["undeclared status"], refused)
		}
		result(t, "ST-GEN-UC12-FL-T")
	})
	t.Run("ST-GEN-UC13-R01-T01 Of two migration jobs only the lease holder moves rows", func(t *testing.T) { result(t, "ST-GEN-UC13-R01-T01") })
	t.Run("ST-GEN-UC13-R02-T01 The batch of a job whose lease expired fails as fenced", func(t *testing.T) { result(t, "ST-GEN-UC13-R02-T01") })
	t.Run("ST-GEN-UC13-R03-T01 A batch that meets a locked row times out and leaves the checkpoint where it was", func(t *testing.T) {
		result(t, "ST-GEN-UC13-R03-T01")
	})
	t.Run("ST-GEN-UC13-R04-T01 A backfill that is killed between batches resumes from its checkpoint", func(t *testing.T) {
		result(t, "ST-GEN-UC13-R04-T01")
	})
	t.Run("ST-GEN-UC13-R05-T01 The change is complete with its time when no row is left", func(t *testing.T) { result(t, "ST-GEN-UC13-R05-T01") })
	t.Run("ST-GEN-UC13-R06-T01 Progress is counts with no row value, and a stall is a probe signal", func(t *testing.T) {
		result(t, "ST-GEN-UC13-R06-T01")
	})
	t.Run("ST-GEN-UC13-FL-T The email rename backfills 10,000 rows under the lease while v1 and v2 write", func(t *testing.T) {
		result(t, "ST-GEN-UC13-FL-T")
	})
	t.Run("ST-GEN-UC15-R01-T01 The contract migration refuses while rows are left and writes no ledger row", func(t *testing.T) {
		result(t, "ST-GEN-UC15-R01-T01")
	})
	t.Run("ST-GEN-UC15-R02-T01 After contract the old column and the trigger are gone and the contract release does not need them", func(t *testing.T) {
		result(t, "ST-GEN-UC15-R02-T01")
	})
	t.Run("ST-SUP-RP10-LC58-T03 In a rolling deploy that adds a relation, each row of the release before gets its tuple after the release before stops", func(t *testing.T) {
		if plan := e.plans["v1r plan"]; e.plans["v1r plan error"] != "" || !strings.Contains(plan, "organization-tenant-relate: relate Organization#tenant at expand; migration internal/storage/migrations/step_0001_organization-tenant-relate_expand.sql; trigger none; backfill the relate job of the controller under the lease stego.backfill.organization-tenant-relate") {
			t.Fatalf("stego plan --base v1 of v1r (%s) does not list the relate step:\n%s", e.plans["v1r plan error"], plan)
		}
		result(t, "ST-SUP-RP10-LC58-T03")
	})
	t.Run("ST-SUP-RP10-LC58-T04 In a rolling deploy that relates a field, a former subject that the release before replaced loses its access, and the subject of the field gets it", func(t *testing.T) {
		if plan := e.plans["v1r plan"]; !strings.Contains(plan, "organization-approver-relate: relate Organization#approver at expand; migration internal/storage/migrations/step_0002_organization-approver-relate_expand.sql") {
			t.Fatalf("stego plan --base v1 of v1r does not list the relate step of the field:\n%s", plan)
		}
		result(t, "ST-SUP-RP10-LC58-T04")
	})
	t.Run("ST-GEN-UC15-FL-T The email rename finishes while v2 and v3 both run, and v1 can no longer start", func(t *testing.T) {
		plan := e.plans["v3 plan"]
		if e.plans["v3 plan error"] != "" || !strings.Contains(plan, "user-email-rename: rename User.contact_email at contract; migration internal/storage/migrations/step_0006_user-email-rename_contract.sql; trigger none; backfill none") {
			t.Fatalf("stego plan --base v2 of v3 (%s) does not list the contract migration of the rename:\n%s", e.plans["v3 plan error"], plan)
		}
		if e.plans["removed compat error"] == "" || !strings.Contains(e.plans["removed compat"], "user-email-rename") {
			t.Fatalf("stego compat --base v3 of a head that removes the finished change exits with %q and says:\n%s\nwant exit 1 and the change named",
				e.plans["removed compat error"], e.plans["removed compat"])
		}
		result(t, "ST-GEN-UC15-FL-T")
	})
}
