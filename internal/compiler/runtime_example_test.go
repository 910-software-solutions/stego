package compiler

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// exampleFixture is the folder of the check that runs the example user-management on the runtime data path.
const exampleFixture = "testdata/runtime_example"

// TestRuntimeExample copies the example user-management as it is committed, with its generated output and its
// record, builds it with the pinned Go against the runtime module of this repository, and runs the check of
// testdata/runtime_example/check in its module once: the check runs the migration job and the service binary
// against the local PostgreSQL server of STEGO_TEST_POSTGRES_DSN and a local SpiceDB container, and calls the
// example's collections over HTTP; then it changes the migrated database once for each kind of object of the schema
// manifest and sees each start refused, and starts the service with no DATABASE_URL and on a closed database port and
// reads the stage of each failure record; last it provisions a tenant through the generated route, creates and reads
// an organization, retires the tenant and starts the service again, and the readiness stays healthy (#379 gap). A
// missing server fails the test: a missing result is not a pass.
func TestRuntimeExample(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the example service")
	}
	t.Parallel()
	if os.Getenv("STEGO_TEST_POSTGRES_DSN") == "" {
		t.Fatal("STEGO_TEST_POSTGRES_DSN is not set: this test needs a local PostgreSQL server")
	}
	p := &wiringProject{t: t, dir: t.TempDir()}
	if err := os.CopyFS(p.dir, os.DirFS(filepath.Join("..", "..", "examples", "user-management"))); err != nil {
		t.Fatal(err)
	}
	if record := p.read(".stego/state.yaml"); !strings.Contains(record, "format_version: 6") || strings.Contains(record, "out/internal/storage/store.go") {
		t.Fatal("the record of the example user-management is not of format version 6 on the runtime data path")
	}
	check, err := os.ReadFile(filepath.Join(exampleFixture, "check", "example_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	p.write("out/examplecheck/example_test.go", string(check))
	p.useLocalRuntime()
	p.goCommand("mod", "tidy")
	p.goCommand("vet", "./out/...")
	binary := filepath.Join(p.dir, "bin", "service")
	p.goCommand("build", "-o", binary, "./out")
	endpoint, key := startWiringSpiceDB(t)
	cmd := exec.Command(wiringPinnedGo(), "test", "-count=1", "-timeout=15m", "-v", "-run", "^(TestExample|TestSchemaManifest|TestStartFailure|TestProvisionRetire)$", "./out/examplecheck")
	cmd.Dir = p.dir
	cmd.Env = wiringGoEnv("STEGO_TEST_SPICEDB="+endpoint, "STEGO_TEST_SPICEDB_KEY="+key, "STEGO_EXAMPLE_BINARY="+binary)
	output, runErr := cmd.CombinedOutput()
	t.Run("ST-SUP-RC20-OC39-T02 The example user-management runs on the runtime data path with its scoped collections, upsert, implicit fields and fills", func(t *testing.T) {
		name := "ST-SUP-RC20-OC39-T02_The_example_user-management_runs_on_the_runtime_data_path_with_its_scoped_collections,_upsert,_implicit_fields_and_fills"
		if !strings.Contains(string(output), "--- PASS: TestExample/"+name+" ") {
			t.Fatalf("the check of the example did not run and pass (go test: %v):\n%s", runErr, output)
		}
	})
	t.Run("ST-SUP-RC01-OC12-T08 A start on a database that does not satisfy the schema manifest alerts, and the migration job refuses it", func(t *testing.T) {
		name := "ST-SUP-RC01-OC12-T08_A_start_on_a_database_that_does_not_satisfy_the_schema_manifest_alerts,_and_the_migration_job_refuses_it"
		if !strings.Contains(string(output), "--- PASS: TestSchemaManifest/"+name+" ") {
			t.Fatalf("the schema manifest check of the example did not run and pass (go test: %v):\n%s", runErr, output)
		}
	})
	t.Run("ST-SUP-RP14-LC05-T04 The service binary of the example names the missing setting or the database in its failure record, with no value", func(t *testing.T) {
		name := "ST-SUP-RP14-LC05-T04_The_service_binary_of_the_example_names_the_missing_setting_or_the_database_in_its_failure_record,_with_no_value"
		if !strings.Contains(string(output), "--- PASS: TestStartFailure/"+name+" ") {
			t.Fatalf("the start failure check of the example did not run and pass (go test: %v):\n%s", runErr, output)
		}
	})
	t.Run(provisionRetireCheck, func(t *testing.T) {
		name := strings.ReplaceAll(provisionRetireCheck, " ", "_")
		if !strings.Contains(string(output), "--- PASS: TestProvisionRetire/"+name+" ") {
			t.Fatalf("the provisioning check of the example did not run and pass (go test: %v):\n%s", runErr, output)
		}
	})
}

// provisionRetireCheck is the check of TestProvisionRetire in the module of the example, with the ID of the test
// record it proves.
const provisionRetireCheck = "ST-SUP-RC35-OC13-T13 The generated user-management example provisions a tenant, creates and reads an organization, retires the tenant, and stays ready"
