package service

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strings"
	"testing"
)

// startFault is the part of a refused start that the failure record of the process reads.
type startFault interface {
	FaultClass() string
	FaultObject() string
	FaultRemedy() string
	CauseClass() string
}

// TestStartFaults proves that a start that SpiceDB or the migration ledger refuses gives the failure record its fault
// class, its object, a remedy and a cause class, with no value (walkthrough 24, D-m and D-l).
func TestStartFaults(t *testing.T) {
	check := func(t *testing.T, name string, err error, stage, fault, object, cause string, hidden ...string) {
		t.Helper()
		var c startFault
		if StartStage(err) != stage || !errors.As(err, &c) {
			t.Fatalf("%s gave %v at the stage %q, want %s with a fault", name, err, StartStage(err), stage)
		}
		if c.FaultClass() != fault || c.FaultObject() != object || c.FaultRemedy() == "" || c.CauseClass() != cause {
			t.Errorf("%s gives the fault %q, the object %q, the remedy %q and the cause %q, want %q, %q and %q", name,
				c.FaultClass(), c.FaultObject(), c.FaultRemedy(), c.CauseClass(), fault, object, cause)
		}
		for _, text := range []string{c.FaultClass(), c.FaultObject(), c.FaultRemedy(), c.CauseClass()} {
			for _, value := range hidden {
				if strings.Contains(text, value) {
					t.Errorf("%s: the failure record shows the value %q: %q", name, value, text)
				}
			}
		}
	}

	t.Run("ST-SUP-RP14-LC05-T07 A SpiceDB that a start cannot reach gives the failure record the fault spicedb.connection, the object spicedb, a remedy and a cause class, with no value", func(t *testing.T) {
		// A port that no process listens on: SpiceDB is down.
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		endpoint := listener.Addr().String()
		_ = listener.Close()
		conn, err := DialSpiceDB(SpiceDB{Endpoint: endpoint, Key: secretValue, InsecureLoopback: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		err = reach(context.Background(), conn)
		check(t, "a SpiceDB that is down", err, "service.start.spicedb", SpiceDBConnectionFault, "spicedb", "unavailable", secretValue, endpoint)
		if strings.Contains(err.Error(), endpoint) || strings.Contains(err.Error(), secretValue) {
			t.Errorf("the error of the start shows the endpoint or the key: %v", err)
		}
		check(t, "a SpiceDB that does not serve", &spiceDBError{notServing: true}, "service.start.spicedb", SpiceDBConnectionFault, "spicedb", "not_serving")
		if _, err := DialSpiceDB(SpiceDB{}); err == nil {
			t.Fatal("DialSpiceDB takes no settings")
		}
		check(t, "refused settings of SpiceDB", &spiceDBError{err: errors.New("no settings"), settings: true}, "service.start.spicedb", SpiceDBSettingsFault, "spicedb", "invalid_setting")
	})

	t.Run("ST-SUP-RP14-LC05-T08 A start that the check of the migration ledger refuses gives the failure record the fault, the ledger, a remedy and a cause class, with no migration name", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		db, err := sql.Open("pgx", f.database)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		const release = "002_release"
		err = checkRelease(ctx, db, "todo-svc", release, nil, nil)
		if !errors.Is(err, ErrMigrationPending) {
			t.Fatalf("the check of a database with no ledger gave %v, want ErrMigrationPending", err)
		}
		check(t, "a database with no ledger", err, "service.start.migration", MigrationPendingFault, "migration_ledger", MigrationCauseNoLedger, release)
		if _, err := f.admin.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS stego_schema;
CREATE TABLE IF NOT EXISTS stego_schema.migrations (version text PRIMARY KEY, digest text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now());
INSERT INTO stego_schema.migrations (version, digest) VALUES ('001_initial', 'd1')`); err != nil {
			t.Fatal(err)
		}
		err = checkRelease(ctx, db, "todo-svc", release, nil, nil)
		check(t, "a ledger without the last migration of the release", err, "service.start.migration", MigrationPendingFault, "migration_ledger", MigrationCauseMissing, release)
		if _, err := f.admin.ExecContext(ctx, `INSERT INTO stego_schema.migrations (version, digest) VALUES ('002_release', 'd2')`); err != nil {
			t.Fatal(err)
		}
		err = checkRelease(ctx, db, "todo-svc", release, map[string]string{"001_initial": "d1", "002_release": "d2", "003_step": "d3"}, nil)
		check(t, "a ledger without a migration of the release", err, "service.start.migration", MigrationPendingFault, "migration_ledger", MigrationCauseMissing, "003_step")
		err = checkRelease(ctx, db, "todo-svc", release, map[string]string{"001_initial": "d9f00d", "002_release": "d2"}, nil)
		if !errors.Is(err, ErrMigrationChanged) {
			t.Fatalf("the check of another digest gave %v, want ErrMigrationChanged", err)
		}
		check(t, "a ledger with another digest", err, "service.start.migration", MigrationChangedFault, "migration_ledger", MigrationCauseDigest, "001_initial", "d9f00d")
		if err := checkRelease(ctx, db, "todo-svc", release, map[string]string{"001_initial": "d1", "002_release": "d2"}, nil); err != nil {
			t.Fatalf("the check of a ledger that holds the release gave %v", err)
		}
	})
}
