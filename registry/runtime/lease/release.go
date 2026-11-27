package lease

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/910-software-solutions/stego/registry/runtime/otel"
)

// Each running process of a release reports itself in the database, so that the contract step of a schema change
// can refuse while a release that still needs the old column runs (decision on finding F8 of the schema-change
// research, owner, 2026-11-13; ST-SUP-RP09-LC14). A release is the name of the last migration that its adapter
// registers: the migration names sort in the order of the schema history, so a release whose name sorts before the
// expand migration of a change does not know the change and reads its old column.

// ReleaseEvery is the time between two reports of one process, and ReleaseWindow how many of those times a report
// stays current: a process that stops without its last report stays active for at most ReleaseWindow times
// ReleaseEvery.
const (
	ReleaseEvery  = 10 * time.Second
	ReleaseWindow = 3
)

// releasesText is the text of Releases: the table of the reports, one row for each process, and its two functions.
// %[1]s is the runtime role, quoted.
const releasesText = `CREATE TABLE IF NOT EXISTS stego_schema.releases (
	holder text PRIMARY KEY,
	release text NOT NULL,
	version text NOT NULL,
	every_ms integer NOT NULL,
	seen_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS releases_seen_at ON stego_schema.releases (seen_at);
REVOKE ALL ON TABLE stego_schema.releases FROM PUBLIC;
CREATE OR REPLACE FUNCTION stego_schema.release_beat(text, text, text, integer)
 RETURNS void LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_release$BEGIN
  IF $1 IS NULL OR $2 IS NULL OR $3 IS NULL OR $4 IS NULL OR $1 !~ '^[0-9a-f]{32}$' OR $2 !~ '^[0-9a-z_-]{1,200}$'
   OR $3 !~ '^[ -~]{0,64}$' OR $4 < 1000 OR $4 > 60000 THEN
    RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'stego release report: an argument is out of range';
  END IF;
  INSERT INTO stego_schema.releases AS r (holder, release, version, every_ms, seen_at)
   VALUES ($1, $2, $3, $4, pg_catalog.clock_timestamp())
   ON CONFLICT (holder) DO UPDATE SET release = EXCLUDED.release, version = EXCLUDED.version, every_ms = EXCLUDED.every_ms,
   seen_at = EXCLUDED.seen_at;
END$stego_release$;
CREATE OR REPLACE FUNCTION stego_schema.release_stop(text)
 RETURNS void LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_release$DELETE FROM stego_schema.releases r WHERE r.holder = $1$stego_release$;
REVOKE ALL ON FUNCTION stego_schema.release_beat(text, text, text, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION stego_schema.release_stop(text) FROM PUBLIC;
GRANT USAGE ON SCHEMA stego_schema TO %[1]s;
GRANT EXECUTE ON FUNCTION stego_schema.release_beat(text, text, text, integer) TO %[1]s;
GRANT EXECUTE ON FUNCTION stego_schema.release_stop(text) TO %[1]s;
`

// releaseRole is the grammar of the runtime role that Releases grants EXECUTE to.
var releaseRole = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Releases gives the text that makes the table of the release reports, stego_schema.releases, with an index on its
// report time, and its two functions, as the owner role (ST-SUP-RP09-LC14): release_beat writes the report of one
// process, with the time of the database, and touches no row of another process, so a session that holds its own
// row cannot stop the reports of the others (security review of PR #316, M1); release_stop deletes the report of one
// process. Both are SECURITY DEFINER functions of the owner role with the search path pg_catalog, pg_temp and a lock
// timeout of 1 s, and the text grants EXECUTE
// on them to the runtime role alone, so the runtime role can report its own process and cannot read the table, set a
// time, or change the report of a process whose random holder it does not know. The migration of a step at expand
// with a sync trigger runs it, so a release before the step reports itself from the time that step ran. Each
// statement runs again with no error on a database that it made.
func Releases(runtimeRole string) (string, error) {
	if !releaseRole.MatchString(runtimeRole) {
		return "", fmt.Errorf("lease: the runtime role %q is not a lower-case SQL name of at most 63 characters", runtimeRole)
	}
	return fmt.Sprintf(releasesText, `"`+runtimeRole+`"`), nil
}

// releaseName is the grammar of a release: the name of a migration of the ledger.
var releaseName = regexp.MustCompile(`^[0-9a-z_-]{1,200}$`)

// CleanReleases deletes in tx, a transaction of the owner role, the reports older than one day, which processes that
// ended with no Stop left, and passes over each such row that another session holds, so it waits for no row lock of
// the runtime role; on a database with no table of the reports it does nothing. It keeps each report of a release
// that sorts before the expand migration of an open seal change (ST-SUP-RP09-LC15). The migration job runs it on each run
// (ST-SUP-RP09-LC14; security review of PR #316, M1).
func CleanReleases(ctx context.Context, tx *sql.Tx) error {
	var table bool
	if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.to_regclass('stego_schema.releases') IS NOT NULL").Scan(&table); err != nil || !table {
		return err
	}
	// A report of a release before the expand migration of an open seal change stays, so the contract of the seal
	// still sees that such a release ran after its pass started (ST-SUP-RP09-LC15).
	var seals bool
	if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.to_regclass('stego_schema.seals') IS NOT NULL").Scan(&seals); err != nil {
		return err
	}
	keep := ""
	if seals {
		keep = ` AND NOT EXISTS (SELECT 1 FROM stego_schema.seals s WHERE o.release COLLATE "C" < s.expand COLLATE "C")`
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM stego_schema.releases r WHERE r.holder IN (SELECT o.holder FROM stego_schema.releases o
 WHERE o.seen_at < pg_catalog.clock_timestamp() - interval '1 day'`+keep+` FOR UPDATE SKIP LOCKED)`)
	return err
}

// Beat reports one process of a release in the database until Stop.
type Beat struct {
	db                       *sql.DB
	holder, release, version string
	every                    time.Duration
	stop                     chan struct{}
	done                     chan struct{}
	once                     sync.Once
	failing                  bool
	// made is true once the catalog of the database has shown the function release_beat: until then each report
	// first reads the catalog, and calls the function only when it is there, so a database where no step has made
	// it logs no error in PostgreSQL (walkthrough 3, item 5).
	made bool
}

// beatMade reads the catalog for the two functions of the release reports by their exact signatures, so an overload
// of either name neither stops nor starts the reports (security review of PR #354, L1). It reads pg_proc and
// pg_namespace, which every role may read, so it needs no grant and no schema, and it names each operator in
// pg_catalog (I3). The argument types are text (oid 25) and integer (oid 23).
const beatMade = `SELECT pg_catalog.count(DISTINCT p.proname) OPERATOR(pg_catalog.=) 2
 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) p.pronamespace
 WHERE n.nspname OPERATOR(pg_catalog.=) 'stego_schema' AND (
  (p.proname OPERATOR(pg_catalog.=) 'release_beat' AND p.proargtypes::pg_catalog.text OPERATOR(pg_catalog.=) '25 25 25 23')
  OR (p.proname OPERATOR(pg_catalog.=) 'release_stop' AND p.proargtypes::pg_catalog.text OPERATOR(pg_catalog.=) '25'))`

// ready reports whether the functions of the reports are there. It reads the catalog only until it has seen them
// once, because no step removes them.
func (b *Beat) ready(ctx context.Context) (bool, error) {
	if b.made {
		return true, nil
	}
	if err := b.db.QueryRowContext(ctx, beatMade).Scan(&b.made); err != nil {
		return false, err
	}
	return b.made, nil
}

// StartBeat reports the process as the release release, with the label version (STEGO_SERVICE_VERSION), on db, the
// pool of the runtime role: once before it returns and then every ReleaseEvery (ST-SUP-RP09-LC14). The holder is 16
// random bytes, so no other process can stop the report. A report that fails, such as on a database where no step
// has made the table yet, is logged once with no error text of the database and tried again at the next time; a later
// report never stops the process, and the first one does when the catalog shows the function and its call fails, so
// the process restarts and serves no write unreported (security re-review of PR #443, N2). On a database where no step has made the function the report reads only the catalog, so
// PostgreSQL logs no error, and logs at the debug level only; once the catalog shows the function, it calls it. The label keeps its printable ASCII characters, at most 64.
func StartBeat(ctx context.Context, db *sql.DB, release, version string) (*Beat, error) {
	return startBeat(ctx, db, release, version, ReleaseEvery)
}

func startBeat(ctx context.Context, db *sql.DB, release, version string, every time.Duration) (*Beat, error) {
	if db == nil || !releaseName.MatchString(release) {
		return nil, fmt.Errorf("lease: a release report needs the pool and a release that is a migration name, not %q", release)
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	b := &Beat{db: db, holder: hex.EncodeToString(raw), release: release, version: label(version), every: every,
		stop: make(chan struct{}), done: make(chan struct{})}
	// A first report that fails while the function is there stops the start, so the process restarts and never
	// serves a write unreported while a seal is open (security re-review of PR #443, N2).
	if err := b.beat(ctx); err != nil {
		return nil, fmt.Errorf("lease: the first release report of %s failed: %w", release, err)
	}
	go b.run()
	return b, nil
}

// label keeps the printable ASCII characters of the version, at most 64.
func label(version string) string {
	var b strings.Builder
	for _, r := range version {
		if r >= ' ' && r <= '~' && b.Len() < 64 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// beat writes one report under a time limit of one interval. It gives the error of a report that failed while the
// catalog shows the function, and nil when no step has made the function yet.
func (b *Beat) beat(parent context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), b.every)
	defer cancel()
	made, err := b.ready(ctx)
	if err == nil && !made {
		// No step has made the function yet: the database has had no rename at expand, and no contract step reads
		// the reports. The report is a quiet no-op until a step makes it.
		slog.Debug("lease: the release report has no function yet", otel.SlogEventKey, "lease.release_report.pending", "release", b.release)
		return nil
	}
	if err == nil {
		_, err = b.db.ExecContext(ctx, "SELECT stego_schema.release_beat($1, $2, $3, $4)", b.holder, b.release, b.version, int(b.every/time.Millisecond))
	}
	var state interface{ SQLState() string }
	switch {
	case err != nil && errors.As(err, &state) && (state.SQLState() == "42883" || state.SQLState() == "3F000"):
		// No step has made the function yet: the database has had no rename at expand, and no contract step reads
		// the reports.
		slog.Debug("lease: the release report has no function yet", otel.SlogEventKey, "lease.release_report.pending", "release", b.release)
	case err != nil && !b.failing:
		b.failing = true
		slog.Warn("lease: the release report failed; it is tried again", otel.SlogEventKey, "lease.release_report.failed", "release", b.release)
	case err == nil && b.failing:
		b.failing = false
		slog.Info("lease: the release report works again", otel.SlogEventKey, "lease.release_report.recovered", "release", b.release)
	}
	if err != nil && made {
		return err
	}
	return nil
}

func (b *Beat) run() {
	defer close(b.done)
	ticker := time.NewTicker(b.every)
	defer ticker.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-ticker.C:
			b.beat(context.Background())
		}
	}
}

// Holder gives the random holder of the report.
func (b *Beat) Holder() string { return b.holder }

// Stop ends the reports and deletes the report of the process, so that a contract step need not wait for the
// report to age. It runs once; a second call does nothing.
func (b *Beat) Stop(ctx context.Context) error {
	var err error
	b.once.Do(func() {
		close(b.stop)
		<-b.done
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.every)
		defer cancel()
		made, stopErr := b.ready(ctx)
		if stopErr == nil && !made {
			// With no function there is no report to delete.
			return
		}
		if stopErr == nil {
			_, stopErr = b.db.ExecContext(ctx, "SELECT stego_schema.release_stop($1)", b.holder)
		}
		if stopErr != nil {
			err = errors.New("lease: the delete of the release report failed")
		}
	})
	return err
}
