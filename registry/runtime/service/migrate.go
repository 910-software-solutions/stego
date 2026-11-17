package service

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/audit/auditsink"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/manifest"
	"github.com/jsell-rh/stego/registry/runtime/otel"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// MigrationTimeout is the longest time that the migration job runs when Migration gives none.
const MigrationTimeout = 10 * time.Minute

// The steps of the migration job, in the order that Migrate runs them (ST-SUP-RP14-LC03). A fault names one.
const (
	StepSettings            = "settings"
	StepOpen                = "open"
	StepOwnerRole           = "owner_role"
	StepJobLock             = "job_lock"
	StepLedgerCheck         = "ledger_check"
	StepAuthorizationSchema = "authorization_schema"
	StepTables              = "tables"
	StepTenantKeys          = "tenant_keys"
	StepTenantKeysValidate  = "tenant_keys_validate"
	StepOutboxQueue         = "outbox_queue"
	StepOutboxSequence      = "outbox_sequence"
	StepAuditTables         = "audit_tables"
	StepAuditSink           = "audit_sink"
	StepInbox               = "kafka_inbox"
	StepIdempotencyKeys     = "idempotency_keys"
	StepAuthorizationTables = "authorization_tables"
	StepRuntimeGrants       = "runtime_grants"
	StepAuditPartitions     = "audit_partitions"
	StepReleaseReports      = "release_reports"
	StepSchemaAgain         = "authorization_schema_again"
	StepSchemaServed        = "authorization_schema_served"
	StepManifestBefore      = "manifest_before_backfill"
	StepBackfill            = "backfill"
	StepManifest            = "manifest"
	StepReleaseIdentity     = "release_identity"
)

// BackfillTimeout is the longest time that the backfill step of the migration job runs when Migration gives none.
// A job that it stops resumes from the checkpoint on its next run.
const BackfillTimeout = time.Hour

// Migration is what varies with the declaration of the migration job of one service, which the generated code
// gives Migrate: the roles, the adapter's connection configuration and ledger, the ordered migration texts of the
// component packages, the schema writer and the grants of the runtime role.
type Migration struct {
	// OwnerRole is the role that owns the database objects; RuntimeRole the role that the service connects as;
	// WorkerRole the role that the worker mode connects as, which writes the default audit sink (ST-SUP-RP05-LC15).
	OwnerRole, RuntimeRole, WorkerRole string
	// PurgeRole is the idempotency purge role of the service, <service role prefix>_idempotency_purge, which the
	// tenancy role migration makes and which owns the purge function of the key table (ST-SUP-RP07-LC11).
	PurgeRole string
	// OutboxRelayRole is the outbox relay role of the service, which may read every message of the queue; the job
	// reads the highest sequence as that role to check the identity sequence of the queue (ST-SUP-RP14-LC03).
	OutboxRelayRole string
	// TenantConfig gives the connection configuration of the PostgreSQL adapter for a connection string.
	TenantConfig func(dsn string) (tenant.Config, error)
	// LedgerCheck reads the database and refuses one whose tables this release did not make, writing nothing; Ledger
	// runs the ledger of the adapter, which makes the tables of the declaration. Each gets the job's one connection,
	// as the owner role.
	LedgerCheck, Ledger func(ctx context.Context, conn *sql.Conn) error
	// Outbox, Audit and Authorization are the migration texts of the outbox queue, the audit tables and the
	// authorization tables, for the roles of the service; the idempotency keys take the runtime's own text.
	Outbox, Audit, Authorization string
	// Inbox is the migration text of the table of the refused records of the Kafka inbox, kafkainbox.Migration for the
	// worker role, for a service that declares a subscription; empty for one that declares none (ST-SUP-RP01-LC37).
	Inbox string
	// WriteSchema writes the authorization schema of the release to SpiceDB, which refuses a schema that removes a
	// definition or a relation that has relationships (ST-SUP-RC29-OC23).
	WriteSchema func(ctx context.Context, conn grpc.ClientConnInterface) error
	// Definitions are the definitions of the schema that WriteSchema writes. After the last write the job waits
	// until SpiceDB serves each of them (authz.AwaitSchema), so the release does not start while a read of a new
	// resource type fails (StepSchemaServed; walkthrough 3, G4; ST-SUP-Q140).
	Definitions []string
	// Parts are the declared parts of the service, which service.New takes: the job checks the database against the
	// same joined schema manifest at its end (StepManifest), and refuses a database that does not satisfy it, as the
	// start of a pod only alerts (decision on finding F10 of the schema-change research, owner, 2026-11-13, and the
	// orchestrator under the owner's delegation, 2026-11-14).
	Parts *Parts
	// Grants are the GRANT statements of the runtime role, each without its grantee.
	Grants []string
	// TenantSchema is the schema of the tenant list stego_tenants, which the tables step makes with the migration
	// text of the tenant runtime (ST-SUP-RP04-LC07); empty is public.
	TenantSchema string
	// WorkerLeases are the names of the declared single workers: the audit step makes the row of the lease
	// stego.worker.<name> of each (ST-SUP-RC35-OC05). Each is a worker name in lower kebab case.
	WorkerLeases []string
	// Timeout is the longest time that the job runs; zero is MigrationTimeout.
	Timeout time.Duration
	// Backfills are the backfills of the schema changes of this release whose last step is expand, which the job
	// runs last, before the rollout of the release (decision on finding F5 of the schema-change research, owner,
	// 2026-11-13).
	Backfills []Backfill
	// BackfillLease are the lease settings of the backfills, and BackfillTimeout the longest time that the backfill
	// step runs, apart from Timeout; zero is BackfillTimeout. Holder names the job in the lease; empty is the host
	// name and the process ID.
	BackfillLease   lease.Settings
	BackfillTimeout time.Duration
	Holder          string
	// Service names the service on the probe runtime of the backfills, which reports their progress as counts
	// (ST-GEN-UC13-R06).
	Service string
}

// Backfill is the backfill of one schema change at expand: its change ID, the generated step of one batch, its
// settings (ST-GEN-UC13), and the table in public whose completeness CHECK, Settings.Check, the backfill validates.
type Backfill struct {
	Change   string
	Step     lease.Step
	Settings lease.BackfillSettings
	Table    string
}

// StepError is the failure of one step of the migration job: it names the step, and its text holds no value of a
// setting.
type StepError struct {
	Step string
	Err  error
}

func (e *StepError) Error() string { return "migrate: " + e.Step + ": " + e.Err.Error() }
func (e *StepError) Unwrap() error { return e.Err }

// failed gives the failure of a step of the migration job.
func failed(step string, err error) error { return &StepError{Step: step, Err: err} }

// Stage gives the stage of a failure of Migrate, such as migrate.ledger_check, which names the step and no error
// text, for the failure record of the process.
func Stage(err error) string {
	var step *StepError
	if errors.As(err, &step) {
		return "migrate." + step.Step
	}
	return "migrate"
}

// stepped, when a test of the package sets it, runs at the start of each step with the step's name and the job's
// connection.
var stepped func(step string, conn *sql.Conn)

// awaitSchema waits until SpiceDB serves each definition of the written schema; a test of the package replaces it.
var awaitSchema = authz.AwaitSchema

// retried, when a test of the package sets it, runs each time a migration text waited too long for a lock and is
// run again, with the step's name.
var retried func(step string)

// jobLock is the key of the job lock: the session advisory lock that the migration job holds from its start to its
// backfill step, so a second job of the same database waits for the first, within its time limit, and does not race it on the
// objects that both make (followups P2: two jobs at once failed on pg_type with 23505).
const jobLock = "1937007983,1785684555"

// The lock limits of a migration text (followups P1, research F1): each text runs in one transaction with a lock
// timeout, so a statement that needs a strong lock on a table that a long transaction of the running release holds
// waits at most textLockTimeout, and the reads and writes of the release that queue behind it wait no longer. A text
// whose lock wait ends at the timeout runs again after a pause, at most textAttempts times.
const (
	textLockTimeout = "1s"
	textAttempts    = 10
)

// Migrate runs the migration job of a service (tenant.MigrationJob) on one connection, with a time limit
// (ST-SUP-RP14-LC03). It opens the database with the migration settings through tenant.OpenOwner and the adapter's
// connection configuration, and runs, as the owner role and in this order: the read-only ledger check, which
// refuses a database whose tables this release did not make and writes nothing; WriteSchema, which SpiceDB refuses
// for a schema that removes what has relationships, so a refused schema leaves the database as it was; the ledger of
// the adapter and then the tenant list of the tenant runtime (tenant.Migration) in TenantSchema; the foreign key of
// each owned table to the tenant list that the joined schema manifest names (manifest.Table.TenantKey), added NOT
// VALID (StepTenantKeys) and then validated (StepTenantKeysValidate); the outbox, audit, idempotency and authorization migrations, each in one transaction, the audit step
// with the lease table and the row of the controller lease (ControllerLease); the grants of
// the runtime role; the audit partitions of this month and the next; the delete of the release reports older than
// one day (lease.CleanReleases); WriteSchema again, so the release ends on
// its own schema; the wait until SpiceDB serves every definition of that schema (StepSchemaServed); the schema manifest check before the backfills (StepManifestBefore); the backfills; and the schema
// manifest check again (StepManifest). It stops at the first step that fails, with a StepError that names the step. After the owner role
// it takes the job lock, which it holds until the backfill, so a second job of the database waits for it (StepJobLock); and
// it runs the tenant list and each migration text under a lock timeout of 1 s, and again after a pause when a lock
// wait ends at that timeout, at most 10 times, so no read or write of the running release queues long behind it. Each step runs again
// on a database that it made, so the job runs on every release.
func Migrate(parent context.Context, s MigrationSettings, m Migration) error {
	if m.OwnerRole == "" || m.RuntimeRole == "" || m.WorkerRole == "" || m.PurgeRole == "" || m.OutboxRelayRole == "" || m.TenantConfig == nil || m.LedgerCheck == nil || m.Ledger == nil ||
		m.WriteSchema == nil || len(m.Definitions) == 0 || m.Outbox == "" || m.Audit == "" || m.Authorization == "" || m.Parts == nil {
		return failed(StepSettings, errors.New("the migration job needs its roles, the adapter, the ledger, the migration texts, the schema writer with its definitions and the declared parts"))
	}
	for _, name := range m.WorkerLeases {
		if !workerName.MatchString(name) {
			return failed(StepSettings, fmt.Errorf("the worker lease %q is not a worker name in lower kebab case", name))
		}
	}
	keys, err := idempotency.MigrationWithConsumers(m.PurgeRole, m.RuntimeRole, consumerRoles(m.Parts, m.WorkerRole))
	if err != nil {
		return failed(StepSettings, err)
	}
	workerText, floorText, err := workerGrants(m)
	if err != nil {
		return failed(StepSettings, err)
	}
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = MigrationTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	config, err := m.TenantConfig(s.DatabaseURL)
	if err != nil {
		return failed(StepSettings, err)
	}
	db, err := tenant.OpenOwner(ctx, tenant.MigrationJob, config)
	if err != nil {
		return failed(StepOpen, err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return failed(StepOpen, err)
	}
	defer conn.Close()
	// The owner role runs each step with the search path public, pg_temp, so a name with no schema in a migration
	// text, such as the key table of the idempotency migration, resolves in public, never in a schema named after
	// the owner role ("$user") or the login of the job (ST-STD-TXN-R19).
	asOwner := "SET ROLE " + quote(m.OwnerRole) + "; SET search_path = public, pg_temp"
	step := func(name string, run func() error) error {
		if stepped != nil {
			stepped(name, conn)
		}
		if err := run(); err != nil {
			// A refused step says why in a text that names the change or the table, counts and release labels and
			// no row value, which the job logs, as its failure record names the step only (ST-SUP-RC01-OC24,
			// ST-SUP-Q124 (a)).
			var refused interface{ Refusal() string }
			if errors.As(err, &refused) {
				slog.ErrorContext(ctx, "migrate: the step is refused", "step", name, "refusal", refused.Refusal())
			}
			return failed(name, err)
		}
		return nil
	}
	if err := step(StepOwnerRole, func() error { _, err := conn.ExecContext(ctx, asOwner); return err }); err != nil {
		return err
	}
	// The job lock is a session lock of the job's connection: a second job waits here until the first ends its
	// schema steps, and a wait past the time limit of the job fails this step.
	if err := step(StepJobLock, func() error { return takeJobLock(ctx, conn) }); err != nil {
		return err
	}
	locked := true
	unlock := func() {
		if locked {
			locked = false
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_catalog.pg_advisory_unlock("+jobLock+")")
		}
	}
	defer unlock()
	if err := step(StepLedgerCheck, func() error { return m.LedgerCheck(ctx, conn) }); err != nil {
		return err
	}
	spice, err := DialSpiceDB(s.SpiceDB)
	if err != nil {
		return failed(StepAuthorizationSchema, err)
	}
	defer spice.Close()
	// The token of the schema before the write: the wait after the last write reads at least as fresh as it, as a
	// check at a floor of the running release does (StepSchemaServed).
	var since authz.Token
	if err := step(StepAuthorizationSchema, func() error {
		var err error
		if since, err = authz.SchemaToken(ctx, spice); err != nil {
			return err
		}
		return m.WriteSchema(ctx, spice)
	}); err != nil {
		return err
	}
	tenants := m.TenantSchema
	if tenants == "" {
		tenants = "public"
	}
	tenantList, err := tenant.Migration(tenants, m.RuntimeRole)
	if err != nil {
		return failed(StepSettings, err)
	}
	// The table of the release identity row, beside the tenant list, which the last step writes (ST-SUP-RP14-LC03).
	identityTable, err := tenant.IdentityMigration(tenants, m.RuntimeRole)
	if err != nil {
		return failed(StepSettings, err)
	}
	tenantList += identityTable
	sinkTable, err := auditsink.Migration(m.WorkerRole)
	if err != nil {
		return failed(StepSettings, err)
	}
	if err := step(StepTables, func() error {
		if _, err := conn.ExecContext(ctx, asOwner); err != nil {
			return err
		}
		if err := m.Ledger(ctx, conn); err != nil {
			return err
		}
		// The tenant list is the same for every service, and its text runs again on a database that it made
		// (ST-SUP-Q97).
		return textStep(ctx, conn, m.OwnerRole, StepTables, tenantList)
	}); err != nil {
		return err
	}
	// After the tenant list, the foreign key of each owned table to it: added NOT VALID, which scans no row under its
	// short strong lock, and then validated, which blocks no write (decision on ST-SUP-Q124 (a), orchestrator under
	// the owner's delegation, 2026-11-14).
	listKeys, err := tenantKeys(*m.Parts, m.RuntimeRole)
	if err != nil {
		return failed(StepSettings, err)
	}
	if err := step(StepTenantKeys, func() error { return addTenantKeys(ctx, conn, m.OwnerRole, tenants, listKeys) }); err != nil {
		return err
	}
	if err := step(StepTenantKeysValidate, func() error { return validateTenantKeys(ctx, conn, m.OwnerRole, tenants, listKeys) }); err != nil {
		return err
	}
	texts := []struct{ name, text string }{
		{StepOutboxQueue, m.Outbox},
		{StepAuditTables, m.Audit + ";\n" + controllerLease(m.RuntimeRole, m.WorkerLeases)},
		{StepAuditSink, sinkTable},
		{StepInbox, m.Inbox},
		{StepIdempotencyKeys, keys},
		// The grants of the worker role are in the one transaction of the authorization tables: a table-level REVOKE
		// also takes back the column grants on the token of each protected table, which the authorization text gives
		// again, and the floor exists only after that text. So no committed state lacks a grant that the running
		// release needs (security review of PR #337, M2).
		{StepAuthorizationTables, workerText + m.Authorization + ";\n" + floorText},
		{StepRuntimeGrants, granted(m.Grants, m.RuntimeRole)},
	}
	for _, t := range texts {
		if t.text == "" && t.name == StepInbox {
			continue
		}
		if err := step(t.name, func() error { return textStep(ctx, conn, m.OwnerRole, t.name, t.text) }); err != nil {
			return err
		}
		if t.name == StepOutboxQueue {
			if err := step(StepOutboxSequence, func() error { return checkOutboxSequence(ctx, conn, m.OwnerRole, m.OutboxRelayRole) }); err != nil {
				return err
			}
		}
	}
	if err := step(StepAuditPartitions, func() error { return maintainAudit(ctx, conn, m.OwnerRole) }); err != nil {
		return err
	}
	// The job, not the runtime role, deletes the old release reports, and passes over a row that a session holds
	// (security review of PR #316, M1).
	if err := step(StepReleaseReports, func() error {
		return ownerStep(ctx, conn, m.OwnerRole, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, "SELECT pg_catalog.set_config('lock_timeout', $1, true)", textLockTimeout); err != nil {
				return err
			}
			return lease.CleanReleases(ctx, tx)
		})
	}); err != nil {
		return err
	}
	if err := step(StepSchemaAgain, func() error { return m.WriteSchema(ctx, spice) }); err != nil {
		return err
	}
	// SpiceDB reads at a quantized revision, which for some seconds after the write does not know a new definition:
	// the job ends only when SpiceDB serves every definition, within the bound of authz.AwaitSchema, and else fails
	// closed with authz.ErrSchemaNotServed, so the release does not start (ST-SUP-Q140).
	if err := step(StepSchemaServed, func() error { return awaitSchema(ctx, spice, m.Definitions, since) }); err != nil {
		return err
	}
	// Before the backfills, which write the rows of the entity tables as the owner role, the job refuses a database
	// that does not satisfy the joined schema manifest of the service, so no trigger, rule or constraint that the
	// manifest does not name runs as the owner role in them; only the completeness CHECK of an open change may still
	// be NOT VALID, in its exact form (review of PR #305, M1, orchestrator under the owner's delegation, 2026-11-14).
	if err := step(StepManifestBefore, func() error { return checkManifest(ctx, conn, m, true) }); err != nil {
		return err
	}
	// The backfill runs after the job lock is released: two jobs of the database share it under the lease of each
	// change, so one moves rows and the other waits for the lease (ST-GEN-UC13-R01).
	unlock()
	if err := step(StepBackfill, func() error { return runBackfills(parent, db, m) }); err != nil {
		// A batch that found a trigger that the manifest does not name on its table wrote nothing; the job refuses
		// the database at its schema manifest check, which names each fault, and else at the backfill (decision on
		// the #305 info, orchestrator under the owner's delegation, 2026-11-14).
		if errors.Is(err, lease.ErrTrigger) {
			check, cancel := context.WithTimeout(parent, manifest.Timeout)
			defer cancel()
			if merr := checkManifest(check, conn, m, true); merr != nil {
				return failed(StepManifest, merr)
			}
		}
		return err
	}
	// Last, after the texts, which make again the objects that they own, and after the backfills, which validate the
	// completeness CHECK of each open change, the job checks again, as the rollout would find the database (F10). The
	// step has its own time limit, manifest.Timeout, from parent, as a backfill may end near the time limit of the
	// job (review of PR #305, L1).
	if err := step(StepManifest, func() error {
		ctx, cancel := context.WithTimeout(parent, manifest.Timeout)
		defer cancel()
		return checkManifest(ctx, conn, m, false)
	}); err != nil {
		return err
	}
	// Last, the release identity row: the release and the workers digest of this job's release, so only a controller
	// of this release reconciles the worker tuples (ST-SUP-RC35-OC13). It has the time limit of the last check.
	return step(StepReleaseIdentity, func() error {
		ctx, cancel := context.WithTimeout(parent, manifest.Timeout)
		defer cancel()
		return ownerStep(ctx, conn, m.OwnerRole, func(tx *sql.Tx) error {
			return tenant.WriteIdentity(ctx, tx, tenants, m.Parts.Identity())
		})
	})
}

// checkManifest checks the catalog of the database on the job's one connection (manifest.CheckCatalog), as the
// owner role with the search path public, pg_temp, against the schema manifest that service.New checks
// (joinedManifest) for the runtime role of m, with the search path and the settings that a new session of the
// runtime role gets (review of PR #305, M2). Before the backfills (before), the completeness CHECK of each backfill
// of m alone may be NOT VALID. A mismatch is a *manifest.Mismatch, which names each fault; the failure record
// names the step only.
func checkManifest(ctx context.Context, conn *sql.Conn, m Migration, before bool) error {
	want, err := joinedManifest(*m.Parts, m.RuntimeRole)
	if err != nil {
		return err
	}
	job := manifest.Job{RuntimeRole: m.RuntimeRole, BeforeBackfill: before}
	for _, b := range m.Backfills {
		job.Open = append(job.Open, manifest.Open{Schema: "public", Table: b.Table, Check: b.Settings.Check})
	}
	return manifest.CheckCatalog(ctx, conn, want, job)
}

// BackfillLeaseName gives the lease of the backfill of a schema change: stego.backfill.<change ID>
// (ST-GEN-UC13-R01).
func BackfillLeaseName(change string) string { return "stego.backfill." + change }

// runBackfills runs the backfill of each schema change of m in turn, as the owner role, the backfill role of the
// sync trigger (ST-SUP-RC01-OC32; decision on finding F4, orchestrator under the owner's delegation, 2026-11-13),
// each under its own lease, so that of two jobs that run at the same time one moves rows and the other waits
// (ST-GEN-UC13-R01). A job whose batch was fenced, whose lease was lost or whose batch session broke (lease.ErrSession,
// such as an administrator shutdown of the backend; review of PR #305, L3) waits for the lease again with new
// sessions, and finds the change complete or resumes it from the checkpoint (ST-GEN-UC13-R04); after a broken session
// it first waits sessionPause. Each batch accepts on the table of its change only the triggers that the joined schema
// manifest names for it, and a batch that finds another one ends the step with lease.ErrTrigger (decision on the #305
// info, orchestrator under the owner's delegation, 2026-11-14). The step ends when every change is complete,
// or at BackfillTimeout with an error, and the next run of the job resumes.
func runBackfills(parent context.Context, db *sql.DB, m Migration) error {
	return runBackfillsWithin(parent, db, m, context.WithTimeout)
}

// deadline gives a context that ends d after it is made. The job uses context.WithTimeout, which reads the real
// clock; a test gives a clock that it moves itself, so it proves the backfill time limit of ST-SUP-RP09-LC12 with
// no wall-clock limit.
type deadline func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc)

// runBackfillsWithin is runBackfills with the clock of the backfill time limit.
func runBackfillsWithin(parent context.Context, db *sql.DB, m Migration, bound deadline) error {
	if len(m.Backfills) == 0 {
		return nil
	}
	timeout := m.BackfillTimeout
	if timeout <= 0 {
		timeout = BackfillTimeout
	}
	ctx, cancel := bound(parent, timeout)
	defer cancel()
	holder := m.Holder
	if holder == "" {
		holder = defaultHolder()
	}
	var changes []probe.SchemaChange
	for _, b := range m.Backfills {
		changes = append(changes, probe.SchemaChange{ID: b.Change, Open: true})
	}
	builtins, err := probe.NewBuiltins(nil, changes)
	if err != nil {
		return err
	}
	if m.Service == "" {
		return errors.New("the backfills need the service name")
	}
	if m.Parts == nil {
		return errors.New("the backfills need the declared parts")
	}
	want, err := joinedManifest(*m.Parts, m.RuntimeRole)
	if err != nil {
		return err
	}
	telemetry, err := otel.New(m.Service, probe.SpanNames()...)
	if err != nil {
		return err
	}
	defer telemetry.Close()
	probes, err := probe.Start(telemetry, builtins)
	if err != nil {
		return err
	}
	defer func() { _ = probes.Close(context.WithoutCancel(ctx)) }()
	// Two sessions as the owner role: the lease renews on one while a batch holds a transaction on the other.
	session := func() (endedTxConn, error) {
		conn, err := db.Conn(ctx)
		if err != nil {
			return endedTxConn{}, err
		}
		if _, err := conn.ExecContext(ctx, "SET ROLE "+quote(m.OwnerRole)+"; SET search_path = public, pg_temp"); err != nil {
			_ = conn.Close()
			return endedTxConn{}, err
		}
		return endedTxConn{conn}, nil
	}
	release := func(conn endedTxConn) { releaseBackfillSession(ctx, conn) }
	for _, b := range m.Backfills {
		settings := b.Settings
		if settings.Observer == nil {
			settings.Observer = probes.Backfill()
		}
		// Each batch accepts on the table only the triggers that the manifest names for it, the sync trigger of the
		// open change among them (decision on the #305 info, orchestrator under the owner's delegation, 2026-11-14).
		settings.Schema, settings.Table, settings.Triggers = "public", b.Table, nil
		found := false
		for _, t := range want.Tables {
			if t.Schema != "public" || t.Name != b.Table {
				continue
			}
			found = true
			settings.Triggers = map[string]string{}
			for _, trigger := range slices.Concat(t.Triggers, t.OptionalTriggers) {
				settings.Triggers[trigger.Name] = trigger.Function.Name
			}
		}
		if !found {
			return fmt.Errorf("backfill %s: the schema manifest has no table public.%s", b.Change, b.Table)
		}
		backfill, err := lease.NewBackfill(b.Change, b.Step, settings)
		if err != nil {
			return err
		}
		name := BackfillLeaseName(b.Change)
		// Each hold of the lease gets new sessions, since a statement that a lost lease cancels can break its
		// connection.
		hold := func() error {
			leaseConn, err := session()
			if err != nil {
				return err
			}
			defer release(leaseConn)
			batchConn, err := session()
			if err != nil {
				return err
			}
			defer release(batchConn)
			if err := lease.Declare(ctx, leaseConn, name); err != nil {
				return err
			}
			client, err := lease.New(leaseConn, name, holder, m.BackfillLease)
			if err != nil {
				return err
			}
			return client.Hold(ctx, func(ctx context.Context, held *lease.Held) error {
				return backfill.Run(ctx, batchConn, held)
			})
		}
		for {
			err := hold()
			if err == nil {
				break
			}
			if !errors.Is(err, lease.ErrFenced) && !errors.Is(err, lease.ErrLost) && !errors.Is(err, lease.ErrStopped) && !errors.Is(err, lease.ErrSession) {
				return fmt.Errorf("backfill %s: %w", b.Change, err)
			}
			if ctx.Err() != nil {
				return fmt.Errorf("backfill %s: %w", b.Change, ctx.Err())
			}
			// After a broken session the job waits the retry period of the lease before it opens new sessions, so a
			// failover that flaps does not make it open sessions in a tight loop (review of PR #305, info).
			if errors.Is(err, lease.ErrSession) {
				select {
				case <-ctx.Done():
					return fmt.Errorf("backfill %s: %w", b.Change, ctx.Err())
				case <-time.After(sessionPause(m.BackfillLease)):
				}
			}
		}
	}
	return nil
}

// endedTxConn is a session of the backfill step whose transactions end only on the goroutine that runs them. A
// transaction of database/sql whose context ends is rolled back on a goroutine of its own, and with the pgx driver,
// which has no connection validator, that rollback closes the *sql.Conn. A Close at the same time as the next
// statement on the Conn, such as the reset of the role when the session goes back to the pool, gives that statement
// a nil driver connection, and it panics. So a transaction begins with a context that does not end: each statement
// in it still has the context of the caller, which cancels the statement, and the caller ends the transaction with
// its own Commit or Rollback before the session is used again. A transaction that holds the lease row stays fenced
// until it ends, so a commit after the end of the context writes nothing that another holder could take over.
type endedTxConn struct{ *sql.Conn }

// BeginTx begins a transaction whose end only Commit or Rollback gives. A context that has ended begins none.
func (c endedTxConn) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.Conn.BeginTx(context.WithoutCancel(ctx), opts)
}

// backfillResetTimeout is the longest wait for the reset of a backfill session when it goes back to the pool.
const backfillResetTimeout = 5 * time.Second

// releaseBackfillSession gives the backfill session conn back to the pool as the login role, with its own search
// path. Each transaction of the session ended on this goroutine before (endedTxConn), so no Close of database/sql
// runs at the same time as the reset. A session whose reset fails can still run as the owner role, so it is closed
// and does not go back to the pool (review of PR #327, low). The reset runs after the end of the job's context too,
// but for at most backfillResetTimeout, so a server that does not answer cannot hold the release; a reset that
// ends at the timeout fails, and the session is closed (review of PR #332, INFO-1).
func releaseBackfillSession(ctx context.Context, conn endedTxConn) {
	reset, cancel := context.WithTimeout(context.WithoutCancel(ctx), backfillResetTimeout)
	defer cancel()
	if _, err := conn.ExecContext(reset, "RESET ROLE; RESET search_path"); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	_ = conn.Close()
}

// MinSessionPause is the shortest wait of the backfill step after a broken session, whatever the retry period of the
// lease, so a retry period of nearly zero does not make the job open sessions in a tight loop (review of PR #310,
// info).
const MinSessionPause = 100 * time.Millisecond

// sessionPause is the wait of the backfill step after a broken session: the retry period of the lease settings, or
// lease.DefaultRetry when they give none, and never less than MinSessionPause.
func sessionPause(settings lease.Settings) time.Duration {
	retry := settings.Retry
	if retry <= 0 {
		retry = lease.DefaultRetry
	}
	return max(retry, MinSessionPause)
}

// errSequenceBehind is the refusal of a queue whose identity sequence would give a message a sequence that a stored
// message already has or that is below one, as after a logical replication failover that does not carry the state
// of the sequences: the delivery order of a key follows the sequence (ST-SUP-RP01-LC03).
var errSequenceBehind = errors.New("the identity sequence of stego_outbox.messages is behind its highest stored sequence; set it past that sequence as the owner (SELECT setval) before the job runs again")

// checkOutboxSequence refuses a queue whose identity sequence is behind its highest stored sequence. It reads the
// highest sequence first, as the outbox relay role, the one role whose policy sees every message, and the state of
// the sequence second, as the owner, which owns it, so an enqueue that commits between the two reads only raises the
// state of the sequence and never makes a healthy queue look behind. It refuses, and does not move the sequence
// itself, so a person sees the fault and its cause.
func checkOutboxSequence(ctx context.Context, conn *sql.Conn, owner, relay string) error {
	return ownerStep(ctx, conn, owner, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+quote(relay)); err != nil {
			return err
		}
		var highest sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT max(m.sequence) FROM stego_outbox.messages m`).Scan(&highest); err != nil {
			return err
		}
		if betweenSequenceReads != nil {
			betweenSequenceReads()
		}
		if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+quote(owner)); err != nil {
			return err
		}
		var last int64
		var called bool
		if err := tx.QueryRowContext(ctx, `SELECT s.last_value, s.is_called FROM stego_outbox.messages_sequence_seq s`).Scan(&last, &called); err != nil {
			return err
		}
		if !highest.Valid {
			return nil
		}
		// The next value is last_value when is_called is false, and last_value + 1 when it is true.
		if (called && last < highest.Int64) || (!called && last <= highest.Int64) {
			return errSequenceBehind
		}
		return nil
	})
}

// betweenSequenceReads, when a test of the package sets it, runs between the two reads of checkOutboxSequence.
var betweenSequenceReads func()

// controllerLease gives the statements of the lease table and the row of the controller lease, under which the audit
// jobs run (ST-SUP-RP14-LC04), with SELECT and UPDATE on the table for the runtime role, the role of the lease
// client; the table belongs to no tenant. Each statement runs again with no error on a database that it made.
func controllerLease(runtimeRole string, workers []string) string {
	text := lease.LeasesTable + ";\nINSERT INTO public.leases (name) VALUES ('" + ControllerLease + "') ON CONFLICT (name) DO NOTHING;\n"
	// The lease of each single worker (ST-SUP-RC35-OC05); Migrate checked each name against workerName.
	for _, name := range workers {
		text += "INSERT INTO public.leases (name) VALUES ('" + WorkerLeasePrefix + name + "') ON CONFLICT (name) DO NOTHING;\n"
	}
	return text + "GRANT SELECT, UPDATE ON TABLE public.leases TO " + quote(runtimeRole) + ";\n"
}

// workerName is the form of a declared worker name: lower kebab case, at most 63 characters (ST-DECL-INV65).
var workerName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// granted gives the statements of the grants to the runtime role.
func granted(grants []string, role string) string {
	var text strings.Builder
	for _, grant := range grants {
		text.WriteString(grant + " TO " + quote(role) + ";\n")
	}
	return text.String()
}

// grantObject is the object of a GRANT statement of the runtime role: TABLE or SEQUENCE and the quoted name.
var grantObject = regexp.MustCompile(`^GRANT [A-Z, ]+ ON (TABLE|SEQUENCE) ("[^"]+"\."[^"]+")$`)

// tableGrant is one grant of the worker role on a table: the schema, the table and the privileges.
type tableGrant struct {
	schema, table string
	privileges    []string
	// columns limits the privileges to these columns; empty is the whole table.
	columns []string
}

// consumerGrants are the grants of the worker role that the fill of a consuming worker needs in the transaction of
// the claimed message, which the outbox worker opens as the worker role (ST-SUP-RC35-OC06): SELECT, INSERT and
// UPDATE on the table of each entity that a consuming worker writes (WorkerParts.ConsumerTables), and no DELETE,
// since a worker declares no delete; INSERT on the audit events and SELECT, INSERT and UPDATE on the chain
// heads, which the audit row of each write needs; SELECT, INSERT and UPDATE on the key table, which RunConsumer
// writes the message identity to; and SELECT on the revocation floor, which each decision reads. Each table keeps
// its row-level security and its tenant policy for every role. A service with no consuming worker gets none
// (decision on ST-SUP-Q141, orchestrator under the owner's delegation, 2026-11-15).
func consumerGrants(p *Parts) []tableGrant {
	if p == nil || p.Workers.Consumers == nil {
		return nil
	}
	var grants []tableGrant
	for _, table := range p.Workers.ConsumerTables {
		grants = append(grants, tableGrant{schema: "public", table: table, privileges: []string{"SELECT", "INSERT", "UPDATE"}})
	}
	grants = append(grants,
		// The insert of an audit event returns no row, so it needs INSERT only (security review of PR #337, L4).
		tableGrant{schema: audit.RecordSchema, table: audit.EventsTable, privileges: []string{"INSERT"}},
		tableGrant{schema: audit.RecordSchema, table: audit.HeadsTable, privileges: []string{"SELECT", "INSERT", "UPDATE"}},
		tableGrant{schema: idempotency.KeySchema, table: "idempotency_keys", privileges: []string{"SELECT", "INSERT", "UPDATE"}})
	if p.AuthzSchema != "" {
		grants = append(grants, tableGrant{schema: p.AuthzSchema, table: "authz_floor", privileges: []string{"SELECT"}},
			// A consuming worker that creates a row with a reference relates its tuples and records their revocation,
			// as the runtime role does, with the same least grants (walkthrough 8, W8-B1; ST-SUP-RP06-LC18).
			tableGrant{schema: p.AuthzSchema, table: "authz_revocations", privileges: []string{"SELECT"}},
			tableGrant{schema: p.AuthzSchema, table: "authz_revocations", privileges: []string{"INSERT"},
				columns: []string{"id", "tenant_id", "principal", "tuple_count"}},
			tableGrant{schema: p.AuthzSchema, table: "authz_revocation_tuples", privileges: []string{"SELECT"}},
			tableGrant{schema: p.AuthzSchema, table: "authz_revocation_tuples", privileges: []string{"INSERT"},
				columns: []string{"message_id", "revocation_id", "tenant_id"}})
	}
	return grants
}

// authzTables are the tables of the authorization text that the worker role gets grants on: they exist only after
// that text, so their grants are in the floor part of the step.
var authzTables = map[string]bool{"authz_floor": true, "authz_revocations": true, "authz_revocation_tuples": true}

// workerGrants gives the grants of the worker role, which the step authorization_tables runs in its one transaction:
// tables before the authorization text, whose text gives the worker role its column grants on the token of each
// protected table again, and floor after it, as that text makes the revocation floor. It first takes back from the worker role each privilege on
// each table and sequence that the runtime role gets (Migration.Grants), on the audit events and chain heads, on the
// key table and on the revocation floor, so a grant of an earlier release whose consuming worker the declaration no
// longer has is gone; it then gives the worker role the grants of consumerGrants and nothing more. It refuses a
// consumer table that is not a lower-case SQL name or that the runtime role gets no grant on: the worker role never
// gets a table that the runtime role does not (decision on ST-SUP-Q141, orchestrator under the owner's delegation,
// 2026-11-15).
func workerGrants(m Migration) (tables, floor string, err error) {
	role := quote(m.WorkerRole)
	var text strings.Builder
	runtimeTables := map[string]bool{}
	for _, grant := range m.Grants {
		match := grantObject.FindStringSubmatch(grant)
		if match == nil {
			continue
		}
		runtimeTables[match[2]] = true
		text.WriteString("REVOKE ALL ON " + match[1] + " " + match[2] + " FROM " + role + ";\n")
	}
	if m.Parts == nil {
		return text.String(), "", nil
	}
	// Each fixed table is a schema and a name, which the statement quotes, so it names the table with its schema
	// (ST-STD-TXN-R19).
	fixed := [][2]string{{audit.RecordSchema, audit.EventsTable}, {audit.RecordSchema, audit.HeadsTable}, {idempotency.KeySchema, "idempotency_keys"}}
	for _, table := range fixed {
		text.WriteString("REVOKE ALL ON TABLE " + quote(table[0]) + "." + quote(table[1]) + " FROM " + role + ";\n")
	}
	var floorText strings.Builder
	if m.Parts.AuthzSchema != "" {
		for _, table := range []string{"authz_floor", "authz_revocations", "authz_revocation_tuples"} {
			floorText.WriteString("REVOKE ALL ON TABLE " + quote(m.Parts.AuthzSchema) + "." + quote(table) + " FROM " + role + ";\n")
		}
	}
	for _, table := range m.Parts.Workers.ConsumerTables {
		if !sqlName.MatchString(table) || !runtimeTables[quote("public")+"."+quote(table)] {
			return "", "", fmt.Errorf("the consumer table %q is not a table that the runtime role gets a grant on", table)
		}
	}
	for _, g := range consumerGrants(m.Parts) {
		privileges := strings.Join(g.privileges, ", ")
		if len(g.columns) > 0 {
			quoted := make([]string, len(g.columns))
			for i, c := range g.columns {
				quoted[i] = quote(c)
			}
			privileges += " (" + strings.Join(quoted, ", ") + ")"
		}
		statement := "GRANT " + privileges + " ON TABLE " + quote(g.schema) + "." + quote(g.table) + " TO " + role + ";\n"
		if authzTables[g.table] {
			floorText.WriteString(statement)
			continue
		}
		text.WriteString(statement)
	}
	return text.String(), floorText.String(), nil
}

// sqlName is a lower-case SQL name of at most 63 characters, which needs no quotes.
var sqlName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// ownerStep runs one step of the migration job in one transaction on conn, as the owner role from its start: a step
// can reset the role, as the outbox migration does after it made the relay functions.
func ownerStep(ctx context.Context, conn *sql.Conn, owner string, run func(*sql.Tx) error) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+quote(owner)); err != nil {
		return err
	}
	if err := run(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// textStep runs a migration text as the owner role in one transaction under the lock timeout of a text, and runs it
// again after a pause when a lock wait ends at the timeout (SQLSTATE 55P03), at most textAttempts times. Each text
// runs again on a database that it made, so a run that the timeout ended changes nothing that the next run needs.
func textStep(ctx context.Context, conn *sql.Conn, owner, name, text string) error {
	return lockedStep(ctx, conn, owner, name, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, text)
		return err
	})
}

// lockedStep runs run as the owner role in one transaction under the lock timeout of a text, and again after a
// pause when a lock wait ends at the timeout, at most textAttempts times, as textStep runs a text.
func lockedStep(ctx context.Context, conn *sql.Conn, owner, name string, run func(*sql.Tx) error) error {
	for attempt := 1; ; attempt++ {
		err := ownerStep(ctx, conn, owner, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, "SELECT pg_catalog.set_config('lock_timeout', $1, true)", textLockTimeout); err != nil {
				return err
			}
			return run(tx)
		})
		if err == nil || !lockTimedOut(err) || attempt >= textAttempts {
			return err
		}
		if retried != nil {
			retried(name)
		}
		if err := pause(ctx, attempt); err != nil {
			return err
		}
	}
}

// takeJobLock takes the job lock on conn. It tries the lock, and while another job holds it, it pauses and tries
// again, until ctx ends at the time limit of the job. A blocking pg_advisory_lock would hold a snapshot while it
// waits, and CREATE INDEX CONCURRENTLY of the job that holds the lock waits for every snapshot, so the two jobs
// would deadlock (security review of PR #295, L1). Between two tries the connection holds no open transaction.
func takeJobLock(ctx context.Context, conn *sql.Conn) error {
	for attempt := 1; ; attempt++ {
		var taken bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_catalog.pg_try_advisory_lock("+jobLock+")").Scan(&taken); err != nil {
			return err
		}
		if taken {
			return nil
		}
		if waited != nil {
			waited()
		}
		if err := pause(ctx, attempt); err != nil {
			return fmt.Errorf("another migration job holds the job lock: %w", err)
		}
	}
}

// waited is called after each try of the job lock that another job holds; the tests set it.
var waited func()

// lockTimedOut tells whether err ends a wait for a lock at the lock timeout: SQLSTATE 55P03, lock_not_available.
func lockTimedOut(err error) bool {
	var state interface{ SQLState() string }
	return errors.As(err, &state) && state.SQLState() == "55P03"
}

// pause waits before the next run of a text that waited too long for a lock: a time that doubles with each attempt
// from 100 ms to at most 5 s, of which a random part, so two jobs do not try again at once.
func pause(ctx context.Context, attempt int) error {
	wait := min(100*time.Millisecond<<min(attempt-1, 6), 5*time.Second)
	wait = wait/2 + rand.N(wait/2+1)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// maintainAudit runs the audit partition maintenance until it has no step left, so the partitions of this month and
// the next exist before the service writes an audit row.
func maintainAudit(ctx context.Context, conn *sql.Conn, owner string) error {
	for range 64 {
		var more bool
		if err := ownerStep(ctx, conn, owner, func(tx *sql.Tx) error {
			// By its schema, so no function of the same name earlier on the search path runs in its place.
			return tx.QueryRowContext(ctx, "SELECT step_left FROM public."+audit.MaintainFunction+"()").Scan(&more)
		}); err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
	return errors.New("the audit partition maintenance did not finish")
}

// quote quotes one SQL name.
func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// CopyBackfills gives the backfill of each copy, the batch of a rename at expand, over the tenant list stego_tenants
// in tenantSchema (empty is public), with the default batch settings of the lease runtime, a pause of
// BackfillPause between batches, and the validation of the completeness CHECK of each before it is marked complete
// (decision on ST-SUP-Q120 (c), orchestrator under the owner's delegation, 2026-11-14).
func CopyBackfills(copies []lease.Copy, tenantSchema string) ([]Backfill, error) {
	if tenantSchema == "" {
		tenantSchema = "public"
	}
	var out []Backfill
	for _, c := range copies {
		c.Tenants = quote(tenantSchema) + ".stego_tenants"
		step, err := c.Step()
		if err != nil {
			return nil, err
		}
		key, parts := c.Settings()
		out = append(out, Backfill{Change: c.Change, Step: step, Settings: lease.BackfillSettings{FillDigest: c.FillDigest, Key: key, KeyParts: parts, Pause: BackfillPause,
			Validate: c.Validate(), Check: c.Check}, Table: c.Table})
	}
	return out, nil
}

// BackfillPause is the wait of a backfill of the migration job after each batch, which leaves room for the writes of
// the running release and for the replicas (decision on finding F6, orchestrator under the owner's delegation,
// 2026-11-13).
const BackfillPause = 50 * time.Millisecond

// tenantKey is the foreign key of one owned table to the tenant list, as the joined schema manifest names it
// (manifest.Table.TenantKey).
type tenantKey struct {
	schema, table string
	key           manifest.TenantKey
}

// tenantKeys gives the tenant key of each table of the joined schema manifest of the declared parts that names one.
func tenantKeys(declared Parts, runtimeRole string) ([]tenantKey, error) {
	want, err := joinedManifest(declared, runtimeRole)
	if err != nil {
		return nil, err
	}
	if err := want.Validate(); err != nil {
		return nil, err
	}
	var keys []tenantKey
	for _, t := range want.Tables {
		if t.TenantKey != nil {
			keys = append(keys, tenantKey{schema: t.Schema, table: t.Name, key: *t.TenantKey})
		}
	}
	return keys, nil
}

// keyState reads whether the table of k has a constraint of the name of k, and whether it is validated. It reads
// the catalog with the operators of pg_catalog and takes no lock on the table.
func keyState(ctx context.Context, tx *sql.Tx, k tenantKey) (present, validated bool, err error) {
	err = tx.QueryRowContext(ctx, `SELECT k.convalidated FROM pg_catalog.pg_constraint k JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) k.conrelid
  JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
  WHERE n.nspname OPERATOR(pg_catalog.=) $1::pg_catalog.name AND c.relname OPERATOR(pg_catalog.=) $2::pg_catalog.name AND k.conname OPERATOR(pg_catalog.=) $3::pg_catalog.name`,
		k.schema, k.table, k.key.Name).Scan(&validated)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	return err == nil, validated, err
}

// addTenantKeys adds, for each tenant key that its table does not have, the foreign key from the tenant column to
// stego_tenants of the schema tenants, ON DELETE NO ACTION and ON UPDATE NO ACTION, so no delete or change of a
// list row writes a row of a tenant, and NOT VALID, so it scans no row under the SHARE ROW EXCLUSIVE lock that it
// takes on both tables. Each key is one transaction under the lock timeout of a text and its retry, so no write of
// the running release queues long behind it; a run again finds the key and takes no lock (decision on ST-SUP-Q124
// (a), orchestrator under the owner's delegation, 2026-11-14).
func addTenantKeys(ctx context.Context, conn *sql.Conn, owner, tenants string, keys []tenantKey) error {
	for _, k := range keys {
		statement := "ALTER TABLE ONLY " + quote(k.schema) + "." + quote(k.table) + " ADD CONSTRAINT " + quote(k.key.Name) +
			" FOREIGN KEY (" + quote(k.key.Column) + ") REFERENCES " + quote(tenants) + ".stego_tenants (tenant_id) ON DELETE NO ACTION ON UPDATE NO ACTION NOT VALID"
		if err := lockedStep(ctx, conn, owner, StepTenantKeys, func(tx *sql.Tx) error {
			present, _, err := keyState(ctx, tx, k)
			if err != nil || present {
				return err
			}
			_, err = tx.ExecContext(ctx, statement)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// tenantKeyRefusal is the refusal of the validation of a tenant key: its text names the table, the key and the
// remedy, and no row value, so the migration job may log it.
type tenantKeyRefusal struct{ text string }

func (r *tenantKeyRefusal) Error() string   { return r.text }
func (r *tenantKeyRefusal) Refusal() string { return r.text }

// validateTenantKeys validates each tenant key that is not validated, each in one transaction under the lock
// timeout of a text and its retry, with VALIDATE CONSTRAINT, which takes a lock that blocks no read and no write. A
// row of a tenant that the tenant list does not hold fails it (SQLSTATE 23503), and the step refuses with the
// remedy, to provision the tenant or remove its rows, in a text that names no row value (decision on ST-SUP-Q124
// (a), orchestrator under the owner's delegation, 2026-11-14). A key that the step before did not leave on its table
// is a fault.
//
// PostgreSQL 17 validates a foreign key with one query as the current role, which under FORCE ROW LEVEL SECURITY
// sees only the rows of the tenant setting, so the owner role would validate a key over a table with rows of unlisted
// tenants. The step therefore validates each key with tenant.ValidateForeignKey, which revokes SELECT on the table
// from the owner role in the transaction of the step, so PostgreSQL checks every row past row-level security, fails
// when the owner role can still read a column of the table, runs the validation with row_security off and grants
// SELECT again after it (ST-SUP-RP04-LC10; security review of PR #320, L2 and info, orchestrator under the owner's
// delegation, 2026-11-14).
func validateTenantKeys(ctx context.Context, conn *sql.Conn, owner, tenants string, keys []tenantKey) error {
	for _, k := range keys {
		table := quote(k.schema) + "." + quote(k.table)
		err := lockedStep(ctx, conn, owner, StepTenantKeysValidate, func(tx *sql.Tx) error {
			present, validated, err := keyState(ctx, tx, k)
			switch {
			case err != nil:
				return err
			case !present:
				return fmt.Errorf("%s.%s has no tenant key %s", k.schema, k.table, k.key.Name)
			case validated:
				return nil
			}
			return tenant.ValidateForeignKey(ctx, tx, table, k.key.Name)
		})
		var state interface{ SQLState() string }
		if errors.As(err, &state) && state.SQLState() == "23503" {
			return &tenantKeyRefusal{text: fmt.Sprintf("a row of %s.%s has a tenant that the tenant list of the schema %s does not hold, so the tenant key %s is not validated; provision the tenant or remove its rows, then run the migration job again",
				k.schema, k.table, tenants, k.key.Name)}
		}
		if err != nil {
			return err
		}
	}
	return nil
}
