package lease

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestSealCheckpoint(t *testing.T) {
	t.Run("ST-SUP-RP09-LC15-T01 The seal checkpoint is fenced, waits for a release before, starts a pass again after one, and keeps its reports", func(t *testing.T) {
		ctx := context.Background()
		d := openDatabase(t)
		role := "lease_seal_rt"
		d.exec(t, "DROP ROLE IF EXISTS "+role)
		d.exec(t, "CREATE ROLE "+role+" LOGIN PASSWORD 'lease-seal'")
		t.Cleanup(func() {
			_, _ = d.other.Exec("DROP OWNED BY " + role)
			_, _ = d.other.Exec("DROP ROLE IF EXISTS " + role)
		})
		if _, err := SealsText("Bad-Role"); err == nil {
			t.Error("SealsText accepts a role that is not a lower-case SQL name")
		}
		releases, err := Releases(role)
		if err != nil {
			t.Fatal(err)
		}
		seals, err := SealsText(role)
		if err != nil {
			t.Fatal(err)
		}
		// The text runs again with no error on a database that it made.
		for range 2 {
			d.exec(t, releases)
			d.exec(t, seals)
		}
		const change, expand = "todo-title-seal", "step_0002_todo-title-seal_expand"
		d.exec(t, "INSERT INTO stego_schema.seals (change_id, expand) VALUES ($1, $2)", change, expand)
		d.exec(t, "INSERT INTO public.leases (name) VALUES ($1), ('stego.backfill.other')", "stego.backfill."+change)
		d.exec(t, "GRANT SELECT, UPDATE ON TABLE public.leases TO "+role)
		config, err := pgx.ParseConfig(d.dsn)
		if err != nil {
			t.Fatal(err)
		}
		config.User, config.Password = role, "lease-seal"
		pool := stdlib.OpenDB(*config)
		t.Cleanup(func() { _ = pool.Close() })
		inTx := func(fn func(tx *sql.Tx) error) error {
			tx, err := pool.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if err := fn(tx); err != nil {
				return err
			}
			return tx.Commit()
		}
		read := func() SealState {
			t.Helper()
			var s SealState
			if err := inTx(func(tx *sql.Tx) error {
				var err error
				s, err = ReadSeal(ctx, tx, change)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return s
		}
		// The runtime role reads and moves the checkpoint only through the functions.
		if _, err := pool.ExecContext(ctx, "UPDATE stego_schema.seals SET completed_at = now()"); sqlState(err) != "42501" {
			t.Fatalf("the runtime role changes the table of the seal checkpoints: %v", err)
		}
		if _, err := pool.ExecContext(ctx, "SELECT * FROM stego_schema.backfills"); sqlState(err) != "42501" {
			t.Fatalf("the runtime role reads the checkpoints of the migration job: %v", err)
		}
		if err := inTx(func(tx *sql.Tx) error { _, err := ReadSeal(ctx, tx, "no-such-change"); return err }); !errors.Is(err, ErrNoSeal) {
			t.Fatalf("a change with no row gives %v, want ErrNoSeal", err)
		}
		client, err := New(pool, "stego.backfill."+change, "holder-1", Settings{})
		if err != nil {
			t.Fatal(err)
		}
		held, err := client.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if s := read(); s.Started || s.Completed || s.Waiting || s.Stale {
			t.Fatalf("a new seal reads %+v, want no pass", s)
		}
		// A release before the expand migration reports itself: the pass waits.
		d.exec(t, "INSERT INTO stego_schema.releases VALUES ('old', 'step_0001_x_expand', 'v1', 10000, clock_timestamp())")
		d.exec(t, "INSERT INTO stego_schema.releases VALUES ('new', $1, 'v2', 10000, clock_timestamp())", expand)
		if s := read(); !s.Waiting {
			t.Fatalf("the seal reads %+v while a release before reports itself, want waiting", s)
		}
		start := func() error {
			return inTx(func(tx *sql.Tx) error { return SealCheckpoint(ctx, tx, held, change, "", "", 0, 0, true) })
		}
		if err := start(); !errors.Is(err, ErrSealWaiting) {
			t.Fatalf("a start while a release before reports itself gives %v, want ErrSealWaiting", err)
		}
		// The release before stopped more than three intervals ago: a pass starts and moves.
		d.exec(t, "UPDATE stego_schema.releases SET seen_at = clock_timestamp() - interval '1 minute' WHERE holder = 'old'")
		if err := start(); err != nil {
			t.Fatal(err)
		}
		if err := inTx(func(tx *sql.Tx) error {
			return SealCheckpoint(ctx, tx, held, change, "tenant-a", "00000000-0000-0000-0000-000000000007", 3, 5, false)
		}); err != nil {
			t.Fatal(err)
		}
		if s := read(); !s.Started || s.Completed || s.Stale || s.LastTenant != "tenant-a" || s.LastID != "00000000-0000-0000-0000-000000000007" || s.Sealed != 3 || s.Checked != 5 {
			t.Fatalf("the seal reads %+v after a move", s)
		}
		// A stale token, the lease of another change and a change with no row move nothing.
		for _, c := range []struct {
			name          string
			change, lease string
			token         int64
		}{
			{"a stale token", change, "stego.backfill." + change, held.Token() - 1},
			{"the lease of another change", change, "stego.backfill.other", held.Token()},
			{"a change with no row", "other", "stego.backfill.other", held.Token()},
		} {
			var n int64
			if err := pool.QueryRowContext(ctx, "SELECT stego_schema.seal_checkpoint($1, 'tenant-z', NULL, 0, 0, false, $2, 'holder-1', $3)", c.change, c.lease, c.token).Scan(&n); err != nil || n != 0 {
				t.Errorf("%s moves the checkpoint: %d, %v", c.name, n, err)
			}
		}
		if s := read(); s.LastTenant != "tenant-a" {
			t.Fatalf("a fenced write moved the checkpoint to %q", s.LastTenant)
		}
		// A release before reports itself during the pass: the pass does not complete and starts again.
		d.exec(t, "UPDATE stego_schema.releases SET seen_at = clock_timestamp() WHERE holder = 'old'")
		// The caller commits after ErrSealStale, so the start of the pass again stays.
		complete := func() (stale error) {
			err := inTx(func(tx *sql.Tx) error {
				stale = SealComplete(ctx, tx, held, change)
				if errors.Is(stale, ErrSealStale) {
					return nil
				}
				return stale
			})
			if err != nil {
				return err
			}
			return stale
		}
		if err := complete(); !errors.Is(err, ErrSealStale) {
			t.Fatalf("a completion after a report of a release before gives %v, want ErrSealStale", err)
		}
		if s := read(); s.Started || s.Completed || s.LastTenant != "" {
			t.Fatalf("the seal reads %+v after a stale pass, want no pass", s)
		}
		d.exec(t, "UPDATE stego_schema.releases SET seen_at = clock_timestamp() - interval '1 minute' WHERE holder = 'old'")
		if err := start(); err != nil {
			t.Fatal(err)
		}
		if err := complete(); err != nil {
			t.Fatal(err)
		}
		if s := read(); !s.Completed || s.Stale || s.Waiting {
			t.Fatalf("the seal reads %+v after a pass, want completed", s)
		}
		// A release before that reports itself after the pass makes it stale.
		d.exec(t, "UPDATE stego_schema.releases SET seen_at = clock_timestamp() WHERE holder = 'old'")
		if s := read(); !s.Stale || !s.Waiting {
			t.Fatalf("the seal reads %+v after a release before ran again, want stale", s)
		}
		// The cleanup of the reports keeps the old reports of a release before an open seal, and deletes them when the
		// seal is closed.
		d.exec(t, "UPDATE stego_schema.releases SET seen_at = clock_timestamp() - interval '25 hours'")
		clean(t, d)
		var holders []string
		rows, err := d.other.QueryContext(ctx, "SELECT holder FROM stego_schema.releases ORDER BY holder")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				t.Fatal(err)
			}
			holders = append(holders, h)
		}
		rows.Close()
		if len(holders) != 1 || holders[0] != "old" {
			t.Fatalf("the cleanup leaves %v, want the report of the release before alone", holders)
		}
		d.exec(t, "DELETE FROM stego_schema.seals")
		clean(t, d)
		var left int
		if err := d.other.QueryRowContext(ctx, "SELECT count(*) FROM stego_schema.releases").Scan(&left); err != nil || left != 0 {
			t.Fatalf("the cleanup after the seal leaves %d reports: %v", left, err)
		}
	})
}
