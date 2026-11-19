package tenant

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"weak"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// MigrationJob is the name of the process run that migrates the schema. Only that run may open the owner
// connector (ST-SUP-RP04-LC06); the modes api, worker, controller and all may not.
const MigrationJob = "migrate"

// OpenOwner opens the database as the owner role, for the migration job only (ST-SUP-RP04-LC06). run names what the
// process runs: MigrationJob, or the mode it runs, such as api, worker, controller or all. For any run other than
// MigrationJob it refuses with an error that names the run and opens no connection. It checks cfg with Guard and
// each connection with the pooler guard's check of app.tenant before it returns.
func OpenOwner(ctx context.Context, run string, cfg Config) (*sql.DB, error) {
	if run != MigrationJob {
		return nil, fmt.Errorf("tenant: the owner connector opens only in the migration job, and this process runs %q", run)
	}
	db, err := open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	remember(db)
	return db, nil
}

// Open opens the database as the runtime role, for the api, worker and controller modes. It checks cfg with Guard,
// each connection with the pooler guard's check of app.tenant, and that the role owns no table, is not a superuser
// and holds no BYPASSRLS, also through a role it is a member of (ST-SUP-RP04-LC06). When a check fails it closes the
// database and returns an error that names the role and each fault, so startup fails.
func Open(ctx context.Context, cfg Config) (*sql.DB, error) {
	db, err := open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := checkRuntimeRole(ctx, db, cfg.Claims); err != nil {
		_ = db.Close()
		return nil, err
	}
	remember(db)
	return db, nil
}

// opened holds a weak pointer to each pool that Open returned, so Opened can tell such a pool from one that
// sql.Open or another connector made. A pool that is no longer reachable leaves the set.
var opened = struct {
	mu    sync.Mutex
	pools map[weak.Pointer[sql.DB]]struct{}
}{pools: map[weak.Pointer[sql.DB]]struct{}{}}

func remember(db *sql.DB) {
	key := weak.Make(db)
	opened.mu.Lock()
	opened.pools[key] = struct{}{}
	opened.mu.Unlock()
	runtime.AddCleanup(db, func(key weak.Pointer[sql.DB]) {
		opened.mu.Lock()
		delete(opened.pools, key)
		opened.mu.Unlock()
	}, key)
}

// Opened reports whether db is a pool that Open or OpenOwner returned and that is not closed (ST-SUP-RP04-LC08): a
// pool whose connections the pooler guard checks each time they come back, so a connection with a tenant set is
// closed. It reports false for any other pool, for a copy of a pool value, whose address differs, and for a closed
// pool. The set holds weak pointers only, so it keeps no pool alive, and a pool made later at a reused address does
// not match. The command and query runtimes take only a pool for which it reports true (ST-SUP-RP10-LC30,
// ST-SUP-RP11-LC14).
func Opened(db *sql.DB) bool {
	if db == nil {
		return false
	}
	opened.mu.Lock()
	_, ok := opened.pools[weak.Make(db)]
	opened.mu.Unlock()
	if !ok {
		return false
	}
	// A ping with a context that has ended reads only the closed flag of the pool: database/sql refuses a closed
	// pool before it looks at the context, and it opens no connection.
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	return errors.Is(db.PingContext(ended), context.Canceled)
}

// Connection gives the connection configuration that Open and OpenOwner open with, after the checks that they make
// before any connection: Guard, which refuses a session setting (ST-SUP-RP04-LC04), and the transport check, which
// refuses a configuration or a connection string whose TLS does not verify the server, unless cfg.InsecureLoopback
// holds and every target is a loopback IP address (ST-SUP-RP04-LC09). A connector that opens a session of its own,
// such as the listener of pgconn (ST-SUP-RP01-LC20), opens it with this configuration and checks each session with
// CheckSession. The result is a copy, so a change of the caller's value after the check changes nothing.
func Connection(cfg Config) (*pgx.ConnConfig, error) {
	if err := Guard(cfg); err != nil {
		return nil, err
	}
	conn, err := connConfig(cfg)
	if err != nil {
		return nil, err
	}
	// conn is a copy of the opener's configuration or the parsed connection string, made before the check, so a
	// change of the caller's value after the check changes nothing: the pool opens with the TLS policy that the
	// check saw. A connection string gets the same check as a configuration (ST-SUP-RP04-LC09).
	if err := transport(conn, cfg.InsecureLoopback); err != nil {
		return nil, err
	}
	if len(cfg.Params) > 0 && conn.RuntimeParams == nil {
		conn.RuntimeParams = map[string]string{}
	}
	for name, value := range cfg.Params {
		conn.RuntimeParams[name] = value
	}
	return conn, nil
}

// CheckSession is the pooler guard's check of one session (ST-SUP-RP04-LC04): current_setting('app.tenant', true)
// must be empty outside a transaction. Open and OpenOwner run it on each new connection and each time a connection
// comes back to the pool.
func CheckSession(ctx context.Context, conn *pgx.Conn) error { return checkConn(ctx, conn) }

func open(ctx context.Context, cfg Config) (*sql.DB, error) {
	conn, err := Connection(cfg)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*conn,
		stdlib.OptionAfterConnect(func(ctx context.Context, conn *pgx.Conn) error {
			conn.PgConn().CustomData()[planGeneration] = planResets.Load()
			return checkConn(ctx, conn)
		}),
		stdlib.OptionResetSession(func(ctx context.Context, conn *pgx.Conn) error {
			if err := resetPlans(ctx, conn); err != nil {
				return driver.ErrBadConn
			}
			if err := checkConn(ctx, conn); err != nil {
				// The pool discards the connection, and the next request gets a new one.
				return driver.ErrBadConn
			}
			return nil
		}),
	)
	if b := cfg.Pool; b.MaxOpen > 0 || b.MaxIdle > 0 || b.MaxLifetime > 0 || b.MaxIdleTime > 0 {
		if b.MaxOpen > 0 {
			db.SetMaxOpenConns(b.MaxOpen)
		}
		if b.MaxIdle > 0 {
			db.SetMaxIdleConns(b.MaxIdle)
		}
		db.SetConnMaxLifetime(b.MaxLifetime)
		db.SetConnMaxIdleTime(b.MaxIdleTime)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("tenant: open the database: %w", err)
	}
	return db, nil
}

// planResets counts the calls of ResetStatements, and planGeneration is the key of the custom data of a connection
// that holds the count when the connection last dropped its prepared statements, or opened.
var planResets atomic.Int64

const planGeneration = "stego.tenant.plan_generation"

// StalePlan tells whether err is the refusal of the server to run a prepared statement whose result columns changed
// since the connection prepared it, SQLSTATE 0A000 "cached plan must not change result type", as after a column of
// its table was dropped and added again (ST-SUP-RP04-LC11). The statement wrote nothing, so the call may run again.
func StalePlan(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "0A000" && strings.Contains(pg.Message, "cached plan must not change result type")
}

// ResetStatements makes each connection of each pool that Open or OpenOwner gave drop the statements that it
// prepared, at its next use from the pool, so a statement that a change of a table made stale fails once in the
// process and not once on each connection (ST-SUP-RP04-LC11). A caller calls it when StalePlan(err) holds.
func ResetStatements() { planResets.Add(1) }

// resetPlans drops the prepared statements of conn when ResetStatements was called since conn last dropped them or
// opened. An error leaves the connection to the pool to discard.
func resetPlans(ctx context.Context, conn *pgx.Conn) error {
	now := planResets.Load()
	data := conn.PgConn().CustomData()
	if at, ok := data[planGeneration].(int64); ok && at >= now {
		return nil
	}
	if err := conn.DeallocateAll(ctx); err != nil {
		return fmt.Errorf("tenant: drop the prepared statements of the connection: %w", err)
	}
	data[planGeneration] = now
	return nil
}

// checkRuntimeRole fails when the role of db, or a role it is a member of, is a superuser, holds BYPASSRLS or owns
// a table, and when the role of db is a member of an outbox relay role, <service role prefix>_outbox_relay, which
// reads and changes the messages of every tenant (ST-SUP-RP01-LC19), or of a role that may run the outbox relay
// functions, such as their owner or a hook role, or of an idempotency purge role, <service role
// prefix>_idempotency_purge or the owner of a purge function idempotency_keys_purge, which reads and deletes the key
// rows of every tenant (ST-SUP-RP07-LC11). A member of such a role can take it with SET ROLE, so membership counts
// as holding it.
func checkRuntimeRole(ctx context.Context, db *sql.DB, claims bool) error {
	var role string
	if err := db.QueryRowContext(ctx, "SELECT current_user").Scan(&role); err != nil {
		return fmt.Errorf("tenant: read the runtime role: %w", err)
	}
	var faults []string
	privileged, err := db.QueryContext(ctx, `SELECT r.rolname, r.rolsuper, r.rolbypassrls FROM pg_roles r
WHERE (r.rolsuper OR r.rolbypassrls) AND pg_has_role(current_user, r.oid, 'MEMBER')
ORDER BY r.rolname`)
	if err != nil {
		return fmt.Errorf("tenant: read the roles of the runtime role %s: %w", role, err)
	}
	defer privileged.Close()
	for privileged.Next() {
		var name string
		var super, bypass bool
		if err := privileged.Scan(&name, &super, &bypass); err != nil {
			return fmt.Errorf("tenant: read the roles of the runtime role %s: %w", role, err)
		}
		via := ""
		if name != role {
			via = " through its membership of " + name
		}
		if super {
			faults = append(faults, "is a superuser"+via)
		}
		if bypass {
			faults = append(faults, "holds BYPASSRLS"+via)
		}
	}
	if err := privileged.Err(); err != nil {
		return fmt.Errorf("tenant: read the roles of the runtime role %s: %w", role, err)
	}
	rows, err := db.QueryContext(ctx, `SELECT format('%I.%I', n.nspname, c.relname)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p')
	AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'
	AND pg_has_role(current_user, c.relowner, 'MEMBER')
ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("tenant: read the tables of the runtime role %s: %w", role, err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return fmt.Errorf("tenant: read the tables of the runtime role %s: %w", role, err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("tenant: read the tables of the runtime role %s: %w", role, err)
	}
	if len(tables) > 0 {
		faults = append(faults, "owns the tables "+strings.Join(tables, ", "))
	}
	// The runtime role must not take the outbox relay role or a hook role, the worker role that runs the relay
	// functions and the authz state function (ST-SUP-RP06-LC21), or the idempotency purge role (ST-SUP-RP07-LC11),
	// with SET ROLE.
	members, err := db.QueryContext(ctx, `SELECT r.rolname FROM pg_roles r
WHERE r.rolname <> current_user AND pg_has_role(current_user, r.oid, 'MEMBER')
	AND (r.rolname LIKE '%\_outbox\_relay' OR r.rolname LIKE '%\_idempotency\_purge'
		OR EXISTS (SELECT 1 FROM pg_catalog.pg_proc AS p WHERE p.proname = 'idempotency_keys_purge' AND p.proowner = r.oid)
		OR (pg_catalog.to_regprocedure('stego_outbox.claim(timestamptz, integer, uuid, bigint)') IS NOT NULL
			AND pg_catalog.has_function_privilege(r.oid, 'stego_outbox.claim(timestamptz, integer, uuid, bigint)', 'EXECUTE')))
ORDER BY r.rolname`)
	if err != nil {
		return fmt.Errorf("tenant: read the roles of the runtime role %s: %w", role, err)
	}
	defer members.Close()
	var taken []string
	for members.Next() {
		var name string
		if err := members.Scan(&name); err != nil {
			return fmt.Errorf("tenant: read the roles of the runtime role %s: %w", role, err)
		}
		taken = append(taken, name)
	}
	if err := members.Err(); err != nil {
		return fmt.Errorf("tenant: read the roles of the runtime role %s: %w", role, err)
	}
	if len(taken) > 0 {
		faults = append(faults, "is a member of the outbox relay, idempotency purge or hook roles "+strings.Join(taken, ", "))
	}
	// The role itself must not be an outbox relay or purge role by its name, and, unless it is the role of the
	// worker mode's pool, must not run the relay functions itself (ST-SUP-RP01-LC19). A role with no USAGE on the
	// schema stego_outbox, such as the relay login role, cannot run them.
	var named, runs bool
	if err := db.QueryRowContext(ctx, `SELECT current_user LIKE '%\_outbox\_relay' OR current_user LIKE '%\_idempotency\_purge',
	CASE WHEN pg_catalog.to_regnamespace('stego_outbox') IS NULL
			OR NOT pg_catalog.has_schema_privilege(current_user, 'stego_outbox', 'USAGE') THEN false
		ELSE COALESCE(pg_catalog.to_regprocedure('stego_outbox.claim(timestamptz, integer, uuid, bigint)') IS NOT NULL
			AND pg_catalog.has_function_privilege(current_user, 'stego_outbox.claim(timestamptz, integer, uuid, bigint)', 'EXECUTE'), false) END`).Scan(&named, &runs); err != nil {
		return fmt.Errorf("tenant: read the roles of the runtime role %s: %w", role, err)
	}
	if named {
		faults = append(faults, "is named as an outbox relay or idempotency purge role")
	}
	if runs && !claims {
		faults = append(faults, "may run the outbox relay function stego_outbox.claim, which only the worker mode's role may")
	}
	if len(faults) > 0 {
		return errors.New("tenant: the runtime role " + role + " " + strings.Join(faults, " and ") +
			", so the service does not start (ST-STD-TXN-R03)")
	}
	return nil
}

// tableNameStatement gives the count of the parts of the SQL text of a table name, and the schema and the name of
// the table that it names.
const tableNameStatement = `SELECT pg_catalog.cardinality(pg_catalog.parse_ident($1::pg_catalog.text)), n.nspname, c.relname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.oid = pg_catalog.to_regclass($1::pg_catalog.text)`

// ValidateForeignKey validates the foreign key key of table, which the table holds NOT VALID, in the transaction tx
// of the migration job, as the role that owns table (ST-SUP-RP04-LC10). table is the name of the table as SQL text,
// qualified with its schema, and key the name of the key with no quotes. It refuses a table name with no schema
// before it runs a statement on the table, since PostgreSQL would find such a name through the search path, temporary
// tables first (ST-SUP-RP04-LC10-T07; security review of PR #266, L4). It reads the schema and the name of table
// from the catalog and quotes each, so each statement that it runs names the table with its schema
// (ST-STD-TXN-R19).
//
// PostgreSQL 17 validates a foreign key with one query as the current role when that role can read the columns of
// the key in both tables and owns each table that has row-level security. Under FORCE ROW LEVEL SECURITY that query
// sees only the rows of the tenant setting, so the owner role would validate a key over a table with rows that the
// key refuses. When table or the table that the key refers to has row-level security, ValidateForeignKey therefore
// first revokes SELECT on table from the current role in tx, which makes PostgreSQL check each row of table past
// row-level security; when the key refers to table itself, it grants SELECT on the referenced columns again, which
// that check reads. After the validation it grants SELECT on table again. No other session sees the revoke, and a
// rollback of tx takes it back. When the current role can still read each column of the key in table, as through a
// grant to PUBLIC or a role that it inherits from, it fails and validates nothing. It runs the validation with
// row_security off, so that a PostgreSQL that takes the one query in spite of the revoke fails with SQLSTATE 42501
// and does not validate the key over rows that it does not see (security review of PR #320, L2 and info,
// orchestrator under the owner's delegation, 2026-11-14), and sets row_security back to its value before, so the
// later statements of tx run as before. A row that the key refuses fails the validation with SQLSTATE 23503.
func ValidateForeignKey(ctx context.Context, tx *sql.Tx, table, key string) error {
	return validateForeignKey(ctx, tx, table, key, true)
}

// validateForeignKey is ValidateForeignKey; with revoke false it keeps the SELECT of the current role, as a
// PostgreSQL would run the validation that takes its one query in spite of the revoke, which the test of the second
// guard needs.
func validateForeignKey(ctx context.Context, tx *sql.Tx, table, key string, revoke bool) error {
	var parts int
	var schema, name string
	err := tx.QueryRowContext(ctx, tableNameStatement, table).Scan(&parts, &schema, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("tenant: %s has no foreign key %s", table, key)
	}
	if err != nil {
		return err
	}
	if parts != 2 {
		return fmt.Errorf("tenant: the table %s of the foreign key %s names no schema", table, key)
	}
	qualified := pgx.Identifier{schema}.Sanitize() + "." + pgx.Identifier{name}.Sanitize()
	var setting string
	if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.current_setting('row_security')").Scan(&setting); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "SELECT pg_catalog.set_config('row_security', 'off', true)"); err != nil {
		return err
	}
	var secured, self bool
	var referenced string
	err = tx.QueryRowContext(ctx, `SELECT f.relrowsecurity OR r.relrowsecurity, k.conrelid = k.confrelid,
  (SELECT pg_catalog.string_agg(pg_catalog.quote_ident(a.attname), ', ' ORDER BY c.n) FROM pg_catalog.unnest(k.confkey) WITH ORDINALITY AS c (attnum, n)
    JOIN pg_catalog.pg_attribute a ON a.attrelid = k.confrelid AND a.attnum = c.attnum)
FROM pg_catalog.pg_constraint k JOIN pg_catalog.pg_class f ON f.oid = k.conrelid JOIN pg_catalog.pg_class r ON r.oid = k.confrelid
WHERE k.conrelid = pg_catalog.to_regclass($1::pg_catalog.text) AND k.conname = $2 AND k.contype = 'f'`, qualified, key).Scan(&secured, &self, &referenced)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("tenant: %s has no foreign key %s", table, key)
	}
	if err != nil {
		return err
	}
	revoked := secured && revoke
	if revoked {
		statements := []string{"REVOKE SELECT ON TABLE " + qualified + " FROM CURRENT_USER"}
		if self {
			statements = append(statements, "GRANT SELECT ("+referenced+") ON TABLE "+qualified+" TO CURRENT_USER")
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		// PostgreSQL takes its one query only when the role reads each column of the key in table.
		var reads bool
		if err := tx.QueryRowContext(ctx, `SELECT pg_catalog.bool_and(pg_catalog.has_column_privilege(k.conrelid, c.attnum, 'SELECT'))
FROM pg_catalog.pg_constraint k, pg_catalog.unnest(k.conkey) AS c (attnum)
WHERE k.conrelid = pg_catalog.to_regclass($1::pg_catalog.text) AND k.conname = $2 AND k.contype = 'f'`, qualified, key).Scan(&reads); err != nil {
			return err
		}
		if reads {
			return fmt.Errorf("tenant: the current role reads the columns of the foreign key %s of %s after its SELECT is revoked, so the validation would not read every row", key, table)
		}
	}
	if _, err := tx.ExecContext(ctx, "ALTER TABLE "+qualified+" VALIDATE CONSTRAINT "+pgx.Identifier{key}.Sanitize()); err != nil {
		return err
	}
	if revoked {
		// The revoke of SELECT on the table also takes back a SELECT on its columns.
		for _, statement := range []string{"REVOKE SELECT ON TABLE " + qualified + " FROM CURRENT_USER", "GRANT SELECT ON TABLE " + qualified + " TO CURRENT_USER"} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx, "SELECT pg_catalog.set_config('row_security', $1, true)", setting)
	return err
}
