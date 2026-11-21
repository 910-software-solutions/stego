package lease

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The seal checkpoint of ST-SUP-RP09-LC15 (decision ST-DECL-Q44). The seal job of a seal change runs in the
// controller mode, as the runtime role, because only the api, the worker and the controller modes read the key ring;
// so its checkpoint is not the owner's row of stego_schema.backfills (ST-SUP-RC01-OC25), but the row of the change in
// stego_schema.seals, which the runtime role reads and moves only through three fenced SECURITY DEFINER functions. A
// release before the seal step writes only the clear column, so a sealed value that the job checked can go stale
// while such a release runs: a pass starts only while no such release reports itself, and a pass completes only when
// no such release reported itself after it started.

// sealsText is the text of SealsText: the table of the seal checkpoints and its three functions. %[1]s is the
// runtime role, quoted.
const sealsText = `CREATE TABLE IF NOT EXISTS stego_schema.seals (
	change_id text PRIMARY KEY,
	expand text NOT NULL,
	last_tenant text,
	last_id uuid,
	sealed_rows bigint NOT NULL DEFAULT 0,
	checked_rows bigint NOT NULL DEFAULT 0,
	pass_started timestamptz,
	completed_at timestamptz,
	token bigint NOT NULL DEFAULT 0,
	created_at timestamptz NOT NULL DEFAULT pg_catalog.clock_timestamp()
);
ALTER TABLE stego_schema.seals ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT pg_catalog.clock_timestamp();
CREATE OR REPLACE FUNCTION stego_schema.seal_instance()
 RETURNS text LANGUAGE sql STABLE SET search_path = pg_catalog, pg_temp
 AS $stego_seal$SELECT d.oid::text || '/' || pg_catalog.to_char(pg_catalog.pg_postmaster_start_time() AT TIME ZONE 'UTC', 'YYYYMMDDHH24MISSUS')
  FROM pg_catalog.pg_database d WHERE d.datname = pg_catalog.current_database()$stego_seal$;
REVOKE ALL ON FUNCTION stego_schema.seal_instance() FROM PUBLIC;
ALTER TABLE stego_schema.seals ADD COLUMN IF NOT EXISTS armed_in text;
ALTER TABLE stego_schema.seals ADD COLUMN IF NOT EXISTS armed_at timestamptz;
ALTER TABLE stego_schema.seals ALTER COLUMN armed_in SET DEFAULT stego_schema.seal_instance();
REVOKE ALL ON TABLE stego_schema.seals FROM PUBLIC;
CREATE OR REPLACE FUNCTION stego_schema.seal_read(text)
 RETURNS TABLE (last_tenant text, last_id text, sealed_rows bigint, checked_rows bigint, started boolean, completed boolean, waiting boolean, stale boolean)
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_seal$SELECT s.last_tenant, s.last_id::text, s.sealed_rows, s.checked_rows, s.pass_started IS NOT NULL, s.completed_at IS NOT NULL,
  GREATEST(s.created_at, s.armed_at) + interval '3 minutes' > pg_catalog.clock_timestamp() OR EXISTS (SELECT 1 FROM stego_schema.releases r WHERE r.release COLLATE "C" < s.expand COLLATE "C"
   AND r.seen_at + r.every_ms * 3 * interval '1 millisecond' > pg_catalog.clock_timestamp()),
  s.pass_started IS NOT NULL AND EXISTS (SELECT 1 FROM stego_schema.releases r WHERE r.release COLLATE "C" < s.expand COLLATE "C"
   AND r.seen_at + r.every_ms * 3 * interval '1 millisecond' > s.pass_started)
  FROM stego_schema.seals s WHERE s.change_id = $1$stego_seal$;
CREATE OR REPLACE FUNCTION stego_schema.seal_checkpoint(text, text, uuid, bigint, bigint, boolean, text, text, bigint)
 RETURNS integer LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_seal$DECLARE n bigint;
BEGIN
  PERFORM 1 FROM public.leases l WHERE l.name = $7 AND $7 = 'stego.backfill.' || $1 AND l.holder = $8 AND l.token = $9 AND l.expires_at > pg_catalog.clock_timestamp() FOR SHARE;
  IF NOT FOUND THEN
    RETURN 0;
  END IF;
  IF $6 THEN
    UPDATE stego_schema.seals s SET armed_in = stego_schema.seal_instance(), armed_at = pg_catalog.clock_timestamp()
     WHERE s.change_id = $1 AND s.token <= $9 AND s.armed_in IS DISTINCT FROM stego_schema.seal_instance();
    IF FOUND THEN
      RETURN -1;
    END IF;
    IF EXISTS (SELECT 1 FROM stego_schema.seals s WHERE s.change_id = $1 AND GREATEST(s.created_at, s.armed_at) + interval '3 minutes' > pg_catalog.clock_timestamp())
     OR EXISTS (SELECT 1 FROM stego_schema.seals s JOIN stego_schema.releases r ON r.release COLLATE "C" < s.expand COLLATE "C"
     WHERE s.change_id = $1 AND r.seen_at + r.every_ms * 3 * interval '1 millisecond' > pg_catalog.clock_timestamp()) THEN
      RETURN -1;
    END IF;
    WITH changed AS (UPDATE stego_schema.seals s SET last_tenant = NULL, last_id = NULL, sealed_rows = 0, checked_rows = 0,
     pass_started = pg_catalog.clock_timestamp(), completed_at = NULL, token = $9 WHERE s.change_id = $1 AND s.token <= $9 RETURNING 1)
    SELECT pg_catalog.count(*) INTO n FROM changed;
    RETURN n;
  END IF;
  WITH changed AS (UPDATE stego_schema.seals s SET last_tenant = $2, last_id = $3, sealed_rows = $4, checked_rows = $5, token = $9
   WHERE s.change_id = $1 AND s.token <= $9 AND s.pass_started IS NOT NULL AND s.completed_at IS NULL RETURNING 1)
  SELECT pg_catalog.count(*) INTO n FROM changed;
  RETURN n;
END$stego_seal$;
CREATE OR REPLACE FUNCTION stego_schema.seal_complete(text, text, text, bigint)
 RETURNS integer LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_seal$DECLARE n bigint;
BEGIN
  PERFORM 1 FROM public.leases l WHERE l.name = $2 AND $2 = 'stego.backfill.' || $1 AND l.holder = $3 AND l.token = $4 AND l.expires_at > pg_catalog.clock_timestamp() FOR SHARE;
  IF NOT FOUND THEN
    RETURN 0;
  END IF;
  IF EXISTS (SELECT 1 FROM stego_schema.seals s JOIN stego_schema.releases r ON r.release COLLATE "C" < s.expand COLLATE "C"
   WHERE s.change_id = $1 AND r.seen_at + r.every_ms * 3 * interval '1 millisecond' > s.pass_started) THEN
    UPDATE stego_schema.seals s SET last_tenant = NULL, last_id = NULL, pass_started = NULL, completed_at = NULL, token = $4
     WHERE s.change_id = $1 AND s.token <= $4;
    RETURN -1;
  END IF;
  WITH changed AS (UPDATE stego_schema.seals s SET completed_at = pg_catalog.clock_timestamp(), token = $4
   WHERE s.change_id = $1 AND s.token <= $4 AND s.pass_started IS NOT NULL RETURNING 1)
  SELECT pg_catalog.count(*) INTO n FROM changed;
  RETURN n;
END$stego_seal$;
REVOKE ALL ON FUNCTION stego_schema.seal_read(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION stego_schema.seal_checkpoint(text, text, uuid, bigint, bigint, boolean, text, text, bigint) FROM PUBLIC;
REVOKE ALL ON FUNCTION stego_schema.seal_complete(text, text, text, bigint) FROM PUBLIC;
GRANT USAGE ON SCHEMA stego_schema TO %[1]s;
GRANT EXECUTE ON FUNCTION stego_schema.seal_read(text) TO %[1]s;
GRANT EXECUTE ON FUNCTION stego_schema.seal_checkpoint(text, text, uuid, bigint, bigint, boolean, text, text, bigint) TO %[1]s;
GRANT EXECUTE ON FUNCTION stego_schema.seal_complete(text, text, text, bigint) TO %[1]s;
CREATE OR REPLACE FUNCTION stego_schema.release_stop(text)
 RETURNS void LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_seal$BEGIN
  DELETE FROM stego_schema.releases r WHERE r.holder = $1
   AND NOT EXISTS (SELECT 1 FROM stego_schema.seals s WHERE r.release COLLATE "C" < s.expand COLLATE "C");
END$stego_seal$;
REVOKE ALL ON FUNCTION stego_schema.release_stop(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION stego_schema.release_stop(text) TO %[1]s;
`

// SealsText gives the text that makes the table of the seal checkpoints, stego_schema.seals, and its three
// functions, as the owner role (ST-SUP-RP09-LC15): seal_read gives the row of a change, with whether a release before
// its expand migration reports itself now, or the row is younger than 3 minutes, three times the longest report
// interval, so that each process of a release before has reported itself before a first pass (waiting; security
// review of PR #443, L1) and whether one reported itself after the start of the pass
// (stale); seal_checkpoint starts a pass, which it refuses with -1 while such a release reports itself, or moves its
// position. A restore keeps created_at from the dump, so the row also keeps the database instance where a start
// last saw it, armed_in (the OID of the database and the start time of the server, seal_instance), and the time of
// that first sight, armed_at: a start in another instance, as after a restore or a restart of the server, sets the
// two and refuses with -1, and the 3 minutes count from the later of created_at and armed_at (security review of
// PR #470, sec-470); the caller commits that refusal; seal_complete marks the pass completed, or starts it again with -1 when such a release reported itself
// after its start. seal_checkpoint and seal_complete change the row only under the lease stego.backfill.<change ID>
// of the holder with its token, as ST-SUP-RP09-LC07 states, and give 0 when they are fenced. All three are SECURITY
// DEFINER functions of the owner role with the search path pg_catalog, pg_temp and a lock timeout of 1 s, and the
// text grants EXECUTE on them to the runtime role alone and no right on the table. It also makes release_stop keep
// the report of a process of a release before the expand migration of an open seal change, so the stop of such a
// process, as at the end of a roll back, still marks the pass stale and the contract still sees it. The expand
// migration of a seal runs it after the release reports (Releases), which the functions read, and each later step
// that runs Releases runs it again after them. Each statement runs again with no error on
// a database that it made.
func SealsText(runtimeRole string) (string, error) {
	if !releaseRole.MatchString(runtimeRole) {
		return "", fmt.Errorf("lease: the runtime role %q is not a lower-case SQL name of at most 63 characters", runtimeRole)
	}
	return fmt.Sprintf(sealsText, `"`+runtimeRole+`"`), nil
}

// SealState is the row of a seal change in stego_schema.seals as seal_read gives it.
type SealState struct {
	// LastTenant and LastID are the position of the pass: the last row that it checked, or empty at its start.
	LastTenant, LastID string
	// Sealed and Checked count the rows of the pass that the job sealed and that it checked.
	Sealed, Checked int64
	// Started tells that a pass runs or completed, and Completed that it completed.
	Started, Completed bool
	// Waiting tells that a release before the expand migration of the change reports itself now, or that the row
	// is younger than 3 minutes, and Stale that one reported itself after the start of the pass. A row that no start
	// saw in this database instance does not read as waiting: its first start refuses and sets its guard time.
	Waiting, Stale bool
}

// ErrNoSeal is the error of ReadSeal for a change with no row in stego_schema.seals.
var ErrNoSeal = errors.New("lease: the seal change has no checkpoint row")

// RowQuerier runs one statement in a transaction, as *sql.Tx and tenant.Querier do.
type RowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ReadSeal reads the row of the seal change in tx through seal_read.
func ReadSeal(ctx context.Context, tx RowQuerier, change string) (SealState, error) {
	var s SealState
	var tenant, id sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT last_tenant, last_id, sealed_rows, checked_rows, started, completed, waiting, stale FROM stego_schema.seal_read($1)`, change).
		Scan(&tenant, &id, &s.Sealed, &s.Checked, &s.Started, &s.Completed, &s.Waiting, &s.Stale)
	if errors.Is(err, sql.ErrNoRows) {
		return SealState{}, ErrNoSeal
	}
	if err != nil {
		return SealState{}, err
	}
	s.LastTenant, s.LastID = tenant.String, id.String
	return s, nil
}

// SealCheckpoint moves the position of the pass of the change in tx under held: the last tenant and row that the pass
// checked and its counts, or, with start, starts a new pass. It gives ErrFenced when the lease or the row refused the
// write, and ErrSealWaiting for a start while a release before the expand migration reports itself, while the row is
// younger than 3 minutes, or at the first start in this database instance, whose guard time the caller commits.
func SealCheckpoint(ctx context.Context, tx RowQuerier, held *Held, change, lastTenant, lastID string, sealed, checked int64, start bool) error {
	var tenant, id any
	if lastTenant != "" {
		tenant = lastTenant
	}
	if lastID != "" {
		id = lastID
	}
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT stego_schema.seal_checkpoint($1, $2, $3, $4, $5, $6, $7, $8, $9)`, change, tenant, id, sealed, checked, start,
		held.Name(), held.client.holder, held.Token()).Scan(&n); err != nil {
		return err
	}
	switch n {
	case 1:
		return nil
	case -1:
		return ErrSealWaiting
	}
	return fmt.Errorf("lease %s: %w: the seal checkpoint of %s did not move", held.Name(), ErrFenced, change)
}

// SealComplete marks the pass of the change completed in tx under held. It gives ErrSealStale, and starts the pass
// again, which the caller commits, when a release before the expand migration reported itself after the start of the
// pass, and ErrFenced when the lease or the row refused the write.
func SealComplete(ctx context.Context, tx RowQuerier, held *Held, change string) error {
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT stego_schema.seal_complete($1, $2, $3, $4)`, change, held.Name(), held.client.holder, held.Token()).Scan(&n); err != nil {
		return err
	}
	switch n {
	case 1:
		return nil
	case -1:
		return ErrSealStale
	}
	return fmt.Errorf("lease %s: %w: the seal pass of %s did not complete", held.Name(), ErrFenced, change)
}

// The refusals of the seal checkpoint (ST-SUP-RP09-LC15).
var (
	// ErrSealWaiting is the refusal to start a pass while a release before the expand migration reports itself.
	ErrSealWaiting = errors.New("lease: a release before the seal step runs, so the seal pass waits")
	// ErrSealStale is the refusal to complete a pass after a release before the expand migration reported itself
	// after its start: such a release writes the clear column alone, so the pass starts again.
	ErrSealStale = errors.New("lease: a release before the seal step ran during the pass, so the pass starts again")
)
