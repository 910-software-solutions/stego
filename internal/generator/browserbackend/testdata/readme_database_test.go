package browser

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	schema "example.com/browser-test/out/browser/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// readmeBlocks gives the blocks of one language of the component README (readme.md, which the harness copies beside
// this file), in their order, each as its statements.
func readmeBlocks(t *testing.T, language string) [][]string {
	t.Helper()
	data, err := os.ReadFile("readme.md")
	if err != nil {
		t.Fatal(err)
	}
	var blocks [][]string
	for _, match := range regexp.MustCompile("(?s)```"+language+"\n(.*?)```").FindAllStringSubmatch(string(data), -1) {
		var statements []string
		for _, line := range strings.Split(match[1], "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "--") {
				continue
			}
			statements = append(statements, strings.TrimSuffix(line, ";"))
		}
		blocks = append(blocks, statements)
	}
	return blocks
}

func TestReadmeDatabaseSetup(t *testing.T) {
	t.Run("ST-SUP-RC02-OC56-T03 The database setup of the browser-backend README, run as it is written, passes the mode migrate and Verify", func(t *testing.T) {
		dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
		if dsn == "" {
			if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
				t.Fatal("database fixture is required")
			}
			t.Skip("database fixture is not configured")
		}
		blocks := readmeBlocks(t, "sql")
		if len(blocks) != 2 {
			t.Fatalf("the README holds %d SQL blocks, want 2: one for the server and one for the database", len(blocks))
		}
		// The README gives no secret in its SQL: psql sets each one with \password, which sends only its hash.
		psql := readmeBlocks(t, "psql")
		if len(psql) != 1 {
			t.Fatalf("the README holds %d psql blocks, want 1 that sets the secrets", len(psql))
		}
		for _, block := range blocks {
			for _, statement := range block {
				if strings.Contains(strings.ToUpper(statement), "PASSWORD") {
					t.Errorf("the README SQL holds a secret: %q", statement)
				}
			}
		}
		var suffix [6]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			t.Fatal(err)
		}
		service := fmt.Sprintf("readme_%x", suffix)
		runtimePassword, migrationPassword := fmt.Sprintf("rt%x", suffix), fmt.Sprintf("mg%x", suffix)
		fill := strings.NewReplacer("<service>", service)
		passwords := map[string]string{service + "_runtime": runtimePassword, service + "_migrator": migrationPassword}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatal("invalid database fixture configuration")
		}
		admin := stdlib.OpenDB(*cfg)
		defer admin.Close()
		t.Cleanup(func() {
			cleanup := stdlib.OpenDB(*cfg)
			defer cleanup.Close()
			// The README revokes the CONNECT of PUBLIC on the default database of the server, which other tests share.
			for _, query := range []string{"GRANT CONNECT ON DATABASE postgres TO PUBLIC", "DROP DATABASE IF EXISTS " + service + " WITH (FORCE)", "DROP ROLE IF EXISTS " + service + "_migrator", "DROP ROLE IF EXISTS " + service + "_runtime", "DROP ROLE IF EXISTS " + service + "_owner"} {
				if _, err := cleanup.ExecContext(context.Background(), query); err != nil {
					t.Error(err)
				}
			}
		})
		run := func(db *sql.DB, statements []string) {
			t.Helper()
			for _, statement := range statements {
				if _, err := db.ExecContext(ctx, fill.Replace(statement)); err != nil {
					t.Fatalf("the README statement %q: %v", statement, err)
				}
			}
		}
		run(admin, blocks[0])
		// \password <role> of psql runs ALTER ROLE with the hash of the secret it reads; the test gives the secret.
		for _, line := range psql[0] {
			role, ok := strings.CutPrefix(fill.Replace(line), `\password `)
			password, known := passwords[role]
			if !ok || !known {
				t.Fatalf("the README psql line %q is not \\password of the runtime or the migration login", line)
			}
			if _, err := admin.ExecContext(ctx, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" PASSWORD "+quoteLiteral(password)); err != nil {
				t.Fatalf("setting the secret of %s: %v", role, err)
			}
			delete(passwords, role)
		}
		if len(passwords) != 0 {
			t.Fatalf("the README psql block sets no secret for %v", passwords)
		}
		inDatabase := cfg.Copy()
		inDatabase.Database = service
		adminInDatabase := stdlib.OpenDB(*inDatabase)
		defer adminInDatabase.Close()
		run(adminInDatabase, blocks[1])

		login := func(user, password string) string {
			return (&url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port)), Path: "/" + service, RawQuery: "sslmode=disable"}).String()
		}
		t.Setenv("STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK", "1")
		t.Setenv("DATABASE_URL", "")
		t.Setenv("DATABASE_URL_FILE", "")
		t.Setenv("STEGO_MIGRATION_DATABASE_URL_FILE", "")
		t.Setenv("STEGO_MIGRATION_DATABASE_URL", login(service+"_migrator", migrationPassword))
		t.Setenv("STEGO_RUNTIME_DATABASE_ROLE", service+"_runtime")
		// The Job runs at each release, so a second run passes too.
		for attempt := 1; attempt <= 2; attempt++ {
			if err := schema.Migrate(ctx); err != nil {
				fault, object := classOf(err)
				t.Fatalf("run %d of the mode migrate on the README setup: %v (stage %s, fault %s, object %s)", attempt, err, schema.Stage(err), fault, object)
			}
		}
		runtimeConfig, err := pgx.ParseConfig(login(service+"_runtime", runtimePassword))
		if err != nil {
			t.Fatal(err)
		}
		runtimeConfig.RuntimeParams["search_path"] = "pg_catalog"
		runtimeDB := stdlib.OpenDB(*runtimeConfig)
		defer runtimeDB.Close()
		if err := schema.Verify(ctx, runtimeDB); err != nil {
			fault, object := classOf(err)
			t.Fatalf("the runtime login of the README setup does not pass Verify: %v (fault %s, object %s)", err, fault, object)
		}
		// The runtime login cannot connect to the default database of the server.
		elsewhere := runtimeConfig.Copy()
		elsewhere.Database = "postgres"
		other := stdlib.OpenDB(*elsewhere)
		defer other.Close()
		if err := other.PingContext(ctx); err == nil || !strings.Contains(err.Error(), "permission denied for database") {
			t.Errorf("the runtime login of the README setup connects to the database postgres: %v", err)
		}
	})
}

// quoteLiteral gives the SQL string literal of a test secret, which holds only letters and digits.
func quoteLiteral(text string) string { return "'" + strings.ReplaceAll(text, "'", "''") + "'" }
