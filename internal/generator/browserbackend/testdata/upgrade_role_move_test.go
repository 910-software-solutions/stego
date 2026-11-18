package browser

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
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

// roleMoveHeading is the heading of the section of the upgrade guide (upgrade.md, which the harness copies beside
// this file) that moves a database of the old layout, and roleMoveAnchor the anchor that the remedy of the fault names.
const (
	roleMoveHeading = "## Move a browser-service database to the owner and migrator logins"
	roleMoveAnchor  = "docs/upgrade.md#move-a-browser-service-database-to-the-owner-and-migrator-logins"
)

// roleMoveBlocks gives the blocks of one language of the role move section of the upgrade guide, in their order,
// each as its statements: one statement on each line, with no comment line.
func roleMoveBlocks(t *testing.T, language string) [][]string {
	t.Helper()
	data, err := os.ReadFile("upgrade.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(data), "\n"+roleMoveHeading+"\n")
	if !ok {
		t.Fatalf("the upgrade guide has no section %q", roleMoveHeading)
	}
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	var blocks [][]string
	for _, match := range regexp.MustCompile("(?s)```"+language+"\n(.*?)```").FindAllStringSubmatch(section, -1) {
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

func TestUpgradeRoleMove(t *testing.T) {
	t.Run("ST-SUP-RC02-OC56-T04 The role move of the upgrade guide, run as it is written on a database of the old layout, passes the mode migrate and keeps the sessions", func(t *testing.T) {
		dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
		if dsn == "" {
			if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
				t.Fatal("database fixture is required")
			}
			t.Skip("database fixture is not configured")
		}
		sqlBlocks, psqlBlocks := roleMoveBlocks(t, "sql"), roleMoveBlocks(t, "psql")
		if len(sqlBlocks) != 3 || len(psqlBlocks) != 1 {
			t.Fatalf("the role move holds %d SQL blocks and %d psql blocks, want 3 (the server, the database, the check) and 1 (the secret)", len(sqlBlocks), len(psqlBlocks))
		}
		for _, block := range sqlBlocks {
			for _, statement := range block {
				if strings.Contains(strings.ReplaceAll(strings.ToUpper(statement), "PASSWORD NULL", ""), "PASSWORD") {
					t.Errorf("the role move holds a secret in its SQL: %q", statement)
				}
			}
		}
		var suffix [6]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			t.Fatal(err)
		}
		service := fmt.Sprintf("upgrade_%x", suffix)
		owner, runtime, migrator := service+"_owner", service+"_runtime", service+"_migrator"
		ownerPassword, runtimePassword, migratorPassword := fmt.Sprintf("ow%x", suffix), fmt.Sprintf("rt%x", suffix), fmt.Sprintf("mg%x", suffix)
		fill := strings.NewReplacer("<service>", service)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
			for _, query := range []string{"DROP DATABASE IF EXISTS " + service + " WITH (FORCE)", "DROP ROLE IF EXISTS " + migrator, "DROP ROLE IF EXISTS " + runtime, "DROP ROLE IF EXISTS " + owner} {
				if _, err := cleanup.ExecContext(context.Background(), query); err != nil {
					t.Error(err)
				}
			}
		})
		exec := func(db *sql.DB, statements ...string) {
			t.Helper()
			for _, statement := range statements {
				if _, err := db.ExecContext(ctx, statement); err != nil {
					t.Fatalf("%q: %v", statement, err)
				}
			}
		}
		login := func(user, password string) string {
			return (&url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port)), Path: "/" + service, RawQuery: "sslmode=disable"}).String()
		}
		open := func(dsn string) *sql.DB {
			t.Helper()
			config, err := pgx.ParseConfig(dsn)
			if err != nil {
				t.Fatal(err)
			}
			config.RuntimeParams["search_path"] = "pg_catalog"
			db := stdlib.OpenDB(*config)
			t.Cleanup(func() { db.Close() })
			return db
		}

		// The old layout of a release before browser-backend 4.6.0: a LOGIN owner that owns the database and made the
		// session table, a runtime login with the four grants, and the default grants of PostgreSQL to PUBLIC.
		exec(admin,
			"CREATE ROLE "+owner+" LOGIN PASSWORD "+quoteLiteral(ownerPassword),
			"CREATE ROLE "+runtime+" LOGIN PASSWORD "+quoteLiteral(runtimePassword),
			"CREATE DATABASE "+service+" OWNER "+owner+" TEMPLATE template0")
		oldOwner := open(login(owner, ownerPassword))
		exec(oldOwner, schema.SQL,
			"GRANT SELECT, INSERT, UPDATE, DELETE ON public.stego_browser_sessions TO "+runtime,
			`INSERT INTO public.stego_browser_sessions(id_hash,payload,state,expires_at) VALUES(decode(repeat('11',32),'hex'),decode(repeat('00',28),'hex'),'login',CURRENT_TIMESTAMP + interval '1 hour')`)
		oldOwner.Close()

		// The migration Job of the new release on the old layout stops with the class owner and names the section of
		// the upgrade guide as its remedy.
		t.Setenv("STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK", "1")
		t.Setenv("DATABASE_URL", "")
		t.Setenv("DATABASE_URL_FILE", "")
		t.Setenv("STEGO_MIGRATION_DATABASE_URL_FILE", "")
		t.Setenv("STEGO_MIGRATION_DATABASE_URL", login(owner, ownerPassword))
		t.Setenv("STEGO_RUNTIME_DATABASE_ROLE", runtime)
		err = schema.Migrate(ctx)
		var fault *schema.Fault
		if !errors.As(err, &fault) || fault.FaultClass() != "owner" || schema.Stage(err) != "database.schema" {
			class, object := classOf(err)
			t.Fatalf("the Job on the old layout gave %v (stage %s, fault %s, object %s), want the fault owner at database.schema", err, schema.Stage(err), class, object)
		}
		if remedy := fault.FaultRemedy(); !strings.Contains(remedy, roleMoveAnchor) {
			t.Fatalf("the fault owner names the remedy %q, want the section %s", remedy, roleMoveAnchor)
		}

		// The steps of the section, in their order, as they are written.
		exec(admin, mapStatements(fill, sqlBlocks[0])...)
		for _, line := range psqlBlocks[0] {
			if line == "SHOW password_encryption" {
				var encryption string
				if err := admin.QueryRowContext(ctx, line).Scan(&encryption); err != nil || encryption != "scram-sha-256" {
					t.Fatalf("SHOW password_encryption gives %q, %v; the guide asks for scram-sha-256", encryption, err)
				}
				continue
			}
			role, ok := strings.CutPrefix(fill.Replace(line), `\password `)
			if !ok || role != migrator {
				t.Fatalf("the psql line %q is not \\password of the migration login", line)
			}
			exec(admin, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" PASSWORD "+quoteLiteral(migratorPassword))
		}
		inDatabase := cfg.Copy()
		inDatabase.Database = service
		adminInDatabase := stdlib.OpenDB(*inDatabase)
		defer adminInDatabase.Close()
		exec(adminInDatabase, mapStatements(fill, sqlBlocks[1])...)
		for _, query := range mapStatements(fill, sqlBlocks[2]) {
			rows, err := adminInDatabase.QueryContext(ctx, query)
			if err != nil {
				t.Fatalf("the check %q: %v", query, err)
			}
			found := rows.Next()
			rows.Close()
			if found {
				t.Fatalf("the check %q gives a row after the move", query)
			}
		}

		// The Job with the migration login passes, and again at the next release; the runtime login passes Verify and
		// still reads the session of the old release.
		t.Setenv("STEGO_MIGRATION_DATABASE_URL", login(migrator, migratorPassword))
		for attempt := 1; attempt <= 2; attempt++ {
			if err := schema.Migrate(ctx); err != nil {
				class, object := classOf(err)
				t.Fatalf("run %d of the Job after the move: %v (stage %s, fault %s, object %s)", attempt, err, schema.Stage(err), class, object)
			}
		}
		runtimeDB := open(login(runtime, runtimePassword))
		if err := schema.Verify(ctx, runtimeDB); err != nil {
			class, object := classOf(err)
			t.Fatalf("the runtime login does not pass Verify after the move: %v (fault %s, object %s)", err, class, object)
		}
		var sessions int
		if err := runtimeDB.QueryRowContext(ctx, "SELECT count(*) FROM public.stego_browser_sessions").Scan(&sessions); err != nil || sessions != 1 {
			t.Fatalf("the runtime login reads %d sessions after the move (%v), want the 1 session of the old release", sessions, err)
		}
		// A login that is a member of the owner role, other than the migration login, or a member of the runtime login,
		// can take that role: the Job refuses each with the class owner or login, and passes again after the REVOKE
		// that the guide gives (security review of PR #435, M2).
		extra := service + "_extra"
		exec(admin, "CREATE ROLE "+extra+" LOGIN")
		t.Cleanup(func() {
			cleanup := stdlib.OpenDB(*cfg)
			defer cleanup.Close()
			_, _ = cleanup.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+extra)
		})
		for _, c := range []struct{ role, class string }{{owner, "owner"}, {runtime, "login"}} {
			exec(admin, "GRANT "+c.role+" TO "+extra)
			found := 0
			for _, query := range mapStatements(fill, sqlBlocks[2]) {
				rows, err := adminInDatabase.QueryContext(ctx, query)
				if err != nil {
					t.Fatal(err)
				}
				for rows.Next() {
					found++
				}
				rows.Close()
			}
			if found != 1 {
				t.Fatalf("the checks of the guide give %d rows for a member %s of %s, want 1", found, extra, c.role)
			}
			if class, _ := classOf(schema.Migrate(ctx)); class != c.class {
				t.Fatalf("the Job with a member %s of %s gave the class %q, want %s", extra, c.role, class, c.class)
			}
			exec(admin, "REVOKE "+c.role+" FROM "+extra)
			if err := schema.Migrate(ctx); err != nil {
				t.Fatalf("the Job after the REVOKE: %v", err)
			}
		}
		// The old owner login cannot log in any more: it is NOLOGIN, and PASSWORD NULL removed its old password.
		old := open(login(owner, ownerPassword))
		if err := old.PingContext(ctx); err == nil || !(strings.Contains(err.Error(), "not permitted to log in") || strings.Contains(err.Error(), "28P01")) {
			t.Fatalf("the old owner login still logs in after the move: %v", err)
		}
	})
}

// mapStatements gives the statements with each <service> replaced.
func mapStatements(fill *strings.Replacer, statements []string) []string {
	out := make([]string, len(statements))
	for i, statement := range statements {
		out[i] = fill.Replace(statement)
	}
	return out
}
