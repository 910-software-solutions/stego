package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowserServiceMigrateMode(t *testing.T) {
	t.Run("ST-SUP-RC02-OC56-T02 A browser-service gets the mode migrate in main.go and the migration Job with the owner Secret and the runtime role name in its base", func(t *testing.T) {
		project, _ := initialized(t, "browser-service")
		if run := stegoIn(t, project, moduleOf("browser-service"), "apply"); run.code != 0 {
			t.Fatalf("stego apply of the browser-service scaffold: exit %d: %s", run.code, run.stderr)
		}
		read := func(name string) string {
			t.Helper()
			data, err := os.ReadFile(filepath.Join(project, name))
			if err != nil {
				t.Fatal(err)
			}
			return string(data)
		}
		main := read("out/main.go")
		migrate := strings.Index(main, `os.Args[1] == "migrate"`)
		if migrate < 0 || !strings.Contains(main, "schema.Migrate(context.Background())") || !strings.Contains(main, "stage: schema.Stage(err)") {
			t.Fatalf("main.go runs no mode migrate through browser/schema.Migrate:\n%s", main)
		}
		// The start checks the schema before the access check, and a refusal takes the stage that Stage gives.
		check, access := strings.Index(main, `stegoStage = "database.schema"`), strings.Index(main, `stegoStage = "database.access"`)
		if check < 0 || access < 0 || check > access || !strings.Contains(main[check:access], "schema.Verify(ctx, db)") || !strings.Contains(main[check:access], "stegoStage = schema.Stage(err)") {
			t.Fatalf("main.go does not run browser/schema.Verify at the stage database.schema before the access check:\n%s", main)
		}
		// The failure record names the fault class and the object of a refused schema check.
		if !strings.Contains(main, "FaultClass()") || !strings.Contains(main, `json:"fault,omitempty"`) {
			t.Fatal("the failure record of main.go names no fault class")
		}
		job := read("deploy/base/migration-job.yaml")
		// The Job holds each variable that the mode migrate reads: the owner connection from its Secret and the name
		// of the runtime login from the ConfigMap, and no credential of the runtime login (ST-DLV-INV13).
		for _, want := range []string{"kind: Job", "- migrate", "name: STEGO_MIGRATION_DATABASE_URL", "name: greeter-database-owner", "key: url",
			"name: STEGO_RUNTIME_DATABASE_ROLE"} {
			if !strings.Contains(job, want) {
				t.Errorf("the migration Job of the base does not hold %q:\n%s", want, job)
			}
		}
		for _, refused := range []string{"name: DATABASE_URL\n", "name: DATABASE_URL_FILE\n", "key: url\n                  name: greeter-database\n"} {
			if strings.Contains(job, refused) {
				t.Errorf("the migration Job of the base holds %q, a credential of the runtime login:\n%s", refused, job)
			}
		}
		if deployment := read("deploy/base/deployment.yaml"); strings.Contains(deployment, "database-owner") {
			t.Error("the Deployment of the base reads the owner Secret, which only the migration Job may read")
		}
		if requirements := read("deploy/platform-requirements.yaml"); !strings.Contains(requirements, "name: greeter-database-owner") {
			t.Error("the platform requirements name no owner Secret greeter-database-owner")
		}
	})
}
