package browser

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	schema "example.com/browser-test/out/browser/schema"
)

// classOf gives the fault class and the object of a refusal of the schema check, or two empty strings.
func classOf(err error) (string, string) {
	var fault *schema.Fault
	if !errors.As(err, &fault) {
		return "", ""
	}
	return fault.FaultClass(), fault.FaultObject()
}

func TestBrowserSchemaMigrate(t *testing.T) {
	t.Run("ST-SUP-RC02-OC56-T01 The mode migrate runs Bootstrap as the owner and Verify as the runtime login", func(t *testing.T) {
		db := database(t)
		raw, _ := schemaFixtures.Load(db)
		fixture := raw.(schemaFixture)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		t.Setenv("STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK", "1")
		t.Setenv("DATABASE_URL_FILE", "")
		t.Setenv("STEGO_MIGRATION_DATABASE_URL_FILE", "")
		// With no owner setting, the job refuses at the stage of the setting and makes nothing.
		t.Setenv("DATABASE_URL", fixture.runtimeURL)
		t.Setenv("STEGO_MIGRATION_DATABASE_URL", "")
		err := schema.Migrate(ctx)
		if err == nil || schema.Stage(err) != "service.start.setting.browser-backend.STEGO_MIGRATION_DATABASE_URL" {
			t.Fatalf("the job with no owner setting gave the stage %q, want the stage of STEGO_MIGRATION_DATABASE_URL", schema.Stage(err))
		}
		if strings.Contains(err.Error(), fixture.runtimeURL) {
			t.Fatal("the refusal holds the connection string")
		}
		if schema.Verify(ctx, db) == nil {
			t.Fatal("the refused job made the schema")
		}
		// The administrator login takes the role of the database owner; the job makes the schema and checks it as
		// the runtime login, and a second run changes nothing.
		t.Setenv("STEGO_MIGRATION_DATABASE_URL", fixture.adminURL)
		for run := 1; run <= 2; run++ {
			if err := schema.Migrate(ctx); err != nil {
				t.Fatalf("run %d of the job: %v (stage %s)", run, err, schema.Stage(err))
			}
		}
		if err := schema.Verify(ctx, db); err != nil {
			t.Fatal("the runtime login does not pass Verify after the job", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO public.stego_browser_sessions(id_hash,payload,state,expires_at) VALUES(decode(repeat('00',32),'hex'),decode(repeat('00',28),'hex'),'login',CURRENT_TIMESTAMP)`); err != nil {
			t.Fatal("the runtime login cannot write a session after the job", err)
		}
		// A runtime connection that does not open is database.access, not a schema fault.
		t.Setenv("DATABASE_URL", "postgres://nobody:private-password@127.0.0.1:1/none?sslmode=disable")
		err = schema.Migrate(ctx)
		if err == nil || schema.Stage(err) != "database.access" || strings.Contains(err.Error(), "private-password") {
			t.Fatalf("the job with a runtime connection that does not open gave %v at the stage %q, want database.access with no value", err, schema.Stage(err))
		}
	})
}

func TestBrowserSchemaFaultClass(t *testing.T) {
	t.Run("ST-SUP-RC02-OC57-T01 A refused schema check names its fault class and the generated object", func(t *testing.T) {
		cases := []struct {
			name, change, class, object string
		}{
			{"missing-table", "", "table", "public.stego_browser_sessions"},
			{"extra-column", "ALTER TABLE public.stego_browser_sessions ADD COLUMN extra text", "column", "public.stego_browser_sessions"},
			{"changed-default", "ALTER TABLE public.stego_browser_sessions ALTER COLUMN changed_at SET DEFAULT '2000-01-01'::timestamptz", "column", "public.stego_browser_sessions.changed_at"},
			{"missing-check", "ALTER TABLE public.stego_browser_sessions DROP CONSTRAINT stego_browser_sessions_payload_check", "constraint", "stego_browser_sessions_payload_check"},
			{"missing-index", "DROP INDEX public.stego_browser_sessions_expiry", "index", "stego_browser_sessions_expiry"},
			{"row-policy", "ALTER TABLE public.stego_browser_sessions ENABLE ROW LEVEL SECURITY", "table", "public.stego_browser_sessions"},
			{"public-grant", "GRANT SELECT ON public.stego_browser_sessions TO PUBLIC", "grant", "public.stego_browser_sessions"},
			{"schema-create", "GRANT CREATE ON SCHEMA public TO ", "grant", "public.stego_browser_sessions"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				db := database(t)
				raw, _ := schemaFixtures.Load(db)
				fixture := raw.(schemaFixture)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if c.change != "" {
					migrate(t, db)
					change := c.change
					if strings.HasSuffix(change, " TO ") {
						change += fixture.user
					}
					if _, err := fixture.owner.ExecContext(ctx, change); err != nil {
						t.Fatal("drift fixture", err)
					}
				}
				err := schema.Verify(ctx, db)
				class, object := classOf(err)
				if class != c.class || object != c.object || schema.Stage(err) != "database.schema" || !errors.Is(err, schema.ErrSchema) {
					t.Fatalf("Verify gave the class %q and the object %q at the stage %q, want %s %s at database.schema", class, object, schema.Stage(err), c.class, c.object)
				}
				if strings.Contains(err.Error(), fixture.user) {
					t.Fatal("the fault names the login")
				}
			})
		}
		t.Run("owner-as-runtime", func(t *testing.T) {
			db := database(t)
			migrate(t, db)
			raw, _ := schemaFixtures.Load(db)
			fixture := raw.(schemaFixture)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if class, _ := classOf(schema.Verify(ctx, fixture.owner)); class != "login" {
				t.Fatalf("Verify on the owner connection gave the class %q, want login", class)
			}
		})
		t.Run("closed-connection", func(t *testing.T) {
			db := database(t)
			db.Close()
			err := schema.Verify(context.Background(), db)
			if class, _ := classOf(err); err == nil || class != "" || schema.Stage(err) != "database.access" {
				t.Fatalf("Verify on a closed pool gave %v at the stage %q, want database.access with no class", err, schema.Stage(err))
			}
		})
	})
}
