package service

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/command"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/manifest"
	"github.com/jsell-rh/stego/registry/runtime/modes"
	"github.com/jsell-rh/stego/registry/runtime/otel"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/query"
	"github.com/jsell-rh/stego/registry/runtime/sealed"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// ReachTimeout bounds the check that SpiceDB answers, which New makes before it gives the service.
const ReachTimeout = 10 * time.Second

// Parts are what varies with the declaration of one service, which the generated code gives New
// (ST-SUP-RP14-LC02): names and the constructors of the service's own component packages.
type Parts struct {
	// Service is the name of the service, and DeclarationVersion the SHA-256 of its declaration, which each audit row
	// carries.
	Service, DeclarationVersion string
	// Instance is the instance identity of the process that the generated HTTP telemetry of the service holds, a UUID
	// v4 in lower case. New starts the telemetry runtime with it (otel.NewWithInstance), so the lines of the HTTP layer
	// and of the runtime of one process name one instance. Empty gives the telemetry runtime an identity of its own.
	Instance string
	// SchemaSteps are the IDs of the schema changes of this release whose last step is expand and that have a sync
	// trigger; the command runtime sets the writer marker to them in each transaction that writes
	// (ST-SUP-RC01-OC23).
	SchemaSteps []string
	// Release is the name of the last migration that the adapter of this release registers. New reports each process
	// of the release under it in the database until Close, so that the contract step of a schema change refuses
	// while a release that does not know the change runs (lease.StartBeat, ST-SUP-RP09-LC14). Empty reports nothing.
	Release string
	// Migrations are the ledger digests of the migrations of this release by name, the SHA-256 of each body, which
	// the generated code gives (MigrationDigests of the adapter). New refuses a database whose ledger row of one of
	// these names holds another digest (ST-SUP-RP14-LC13); nil checks no digest.
	Migrations map[string]string
	// EarlierMigrations are the earlier digests of a migration of Migrations by name, which New accepts as well: the
	// digests of the bodies that earlier versions of the adapter wrote for the migration, before a part of the body
	// moved out of it and out of its digest (EarlierMigrationDigests of the adapter, ST-SUP-RC01-OC51).
	EarlierMigrations map[string][]string
	// SubjectType is the subject type of the schema that a verified identity maps to, and TenantClaim the dotted path
	// of the claim that names the caller's tenant.
	SubjectType, TenantClaim string
	// ProblemType and ProblemCode are the problem base and the problem code prefix of a refused credential.
	ProblemType, ProblemCode string
	// TenantConfig gives the connection configuration of the PostgreSQL adapter for a connection string, with its
	// verified TLS policy (ST-SUP-RP04-LC09).
	TenantConfig func(dsn string) (tenant.Config, error)
	// NewQueue gives the outbox queue of the runtime role's pool, with the payload limits of the destinations.
	NewQueue func(db *sql.DB) (*outbox.Queue, error)
	// AuditQueue gives the audit enqueuer on the outbox queue, which the audit writer takes.
	AuditQueue func(queue *outbox.Queue) (audit.Enqueuer, error)
	// NewAuthz gives the authorization runtime of the service over the decision provider.
	NewAuthz func(db *sql.DB, provider authz.Decision, queue *outbox.Queue) (*authz.Runtime, error)
	// AuditSettings are the audit settings of the service.
	AuditSettings audit.Settings
	// Commands are the entities of the declared commands, and Collections those of the generated collection
	// commands: each set that is not empty gets a command runtime of its own, since an entity registers with one
	// runtime only.
	Commands, Collections []command.Declared
	// WorkerEntities are the entities that the declared workers write, which get a command runtime of their own,
	// and WorkerNames the names of the declared workers, the values of the field worker of the built-in worker
	// observations (ST-SUP-RP12-LC21).
	WorkerEntities []command.Declared
	WorkerNames    []string
	// Definitions are the definitions of the authorization schema of the service. Only a schema that declares the
	// definition tenant has worker relations: without it, no reconcile of the worker tuples runs and a Retire reads
	// no undeclared worker relation (ST-SUP-RC35-OC13, security re-review of PR #379, H1).
	Definitions []string
	// Relations are the relations of the authorization schema of the service. The relation service of the type tenant
	// makes Provision relate the tenant resource to the service-level resource of the tenant, and Retire delete it
	// (ST-SUP-RC27-OC18, ST-SUP-RP10-LC52).
	Relations []authz.Relation
	// Workers are the parts of the worker and controller modes, which Start runs (ST-SUP-RP14-LC04).
	Workers WorkerParts
	// OwnerRole is the owner role of the service, which the migration job runs as and which must own each table of
	// the schema manifest and each trigger function (ST-SUP-RP14-LC06).
	OwnerRole string
	// WorkerRole and OutboxRelayRole are the roles of the outbox worker and of the outbox relay functions, which the
	// manifest of the outbox names with the runtime role, the role that New connects as (ST-SUP-RP14-LC06).
	WorkerRole, OutboxRelayRole string
	// RelayRole is the login role of the authorization relay, and AuthzSchema the schema of the revocation tables,
	// which the manifest of the authorization runtime names (ST-SUP-RP14-LC06).
	RelayRole, AuthzSchema string
	// PurgeRole is the role that owns the purge function of the key table, which the manifest of the key table names
	// (ST-SUP-RP14-LC06).
	PurgeRole string
	// Grants are the GRANT statements of the runtime role, each without its grantee, which the migration job grants
	// (Migration.Grants): the manifest checks that the table of each declared entity grants the runtime role exactly
	// these privileges (ST-SUP-RP14-LC06).
	Grants []string
	// Schema is the schema manifest of the tables of the declaration, which the PostgreSQL adapter emits
	// (ST-SUP-RC01-OC12). New adds the manifest of the audit record to it, for the anchor period of Workers
	// (ST-SUP-RP14-LC06).
	Schema manifest.Manifest
	// Destinations are the declared rules of each outbox destination of the service that its components and workers
	// register on the outbox worker, but authz, whose rules New adds itself. The resolver of the api mode applies the
	// stricter of these rules and the rules of the row of a parked message, and refuses a message of a destination
	// that they do not name (ST-SUP-RP14-LC10, security review of PR #421, L3).
	Destinations []outbox.Rules
	// Reseal are the readable secret fields of the entities of the service, which the controller mode moves to the
	// first key of the key ring of Settings (ST-SUP-RP10-LC46, ST-SUP-RP14-LC12). A service with one needs the key ring.
	Reseal []command.ResealColumn
	// Seal are the readable secret fields whose seal step is open, which the seal job of the controller mode seals
	// under the backfill lease of each change (ST-SUP-RP10-LC50, ST-SUP-RP14-LC12). A service with one needs the key
	// ring.
	Seal []command.SealColumn
	// Relate are the relate steps at expand: the relate job of the controller mode stages the tuple of the new
	// relation of each row of the entity through the authz outbox under the backfill lease of each change, in passes
	// that complete only after the releases before the step stopped (decision ST-SUP-Q212 (a)).
	Relate []command.RelateChange
	// KeyRing tells that an entity of the service has a secret field, readable or compare-only, or a pii field: the
	// key ring keys the idempotency fingerprints, which a request body with a secret or pii field needs, and seals
	// the stored answers, so New refuses to start without it (ST-SUP-RP14-LC12; security review of PR #444, M1).
	KeyRing bool
	// Provisioning is the declared tenant provisioning of a pooled service, or nil for a service that declares none
	// (ST-DECL-EN27). With it, New serves Provision and Retire (TenantRoutes), and Middleware and Authenticate refuse
	// a request of a tenant that the tenant list in Workers.TenantSchema does not hold as active (ST-SUP-RC27-OC09,
	// ST-SUP-RC27-OC11).
	Provisioning *Provisioning
}

func (p Parts) check() error {
	switch {
	case p.Service == "" || p.DeclarationVersion == "":
		return errors.New("service: the parts need the service name and the declaration version")
	case p.SubjectType == "" || p.TenantClaim == "" || p.ProblemCode == "":
		return errors.New("service: the parts need the subject type, the tenant claim and the problem code")
	case p.OwnerRole == "":
		return errors.New("service: the parts need the owner role, which owns the tables of the schema manifest")
	case p.WorkerRole == "" || p.OutboxRelayRole == "" || p.RelayRole == "" || p.AuthzSchema == "" || p.PurgeRole == "":
		return errors.New("service: the parts need the worker, outbox relay, relay and purge roles and the authorization schema, which the manifests of the outbox, the authorization runtime and the key table name")
	case len(p.Grants) == 0:
		return errors.New("service: the parts give no GRANT statement of the runtime role (Parts.Grants), so the schema manifest cannot check the grants of the entity tables; generate the service again, which gives the statements that its migration job grants")
	case p.TenantConfig == nil || p.NewQueue == nil || p.AuditQueue == nil || p.NewAuthz == nil:
		return errors.New("service: the parts need the adapter's connection configuration, the outbox queue, the audit queue and the authorization runtime")
	case len(p.SchemaSteps) > 0 && p.Release == "":
		// A release with schema steps has migrations, so the start check of the ledger must run: parts that name
		// no last migration fail closed, and never skip it (#373 security review L3, ST-SUP-RP14-LC07).
		return fmt.Errorf("service: the parts name the schema steps %s but no release, the last migration of the release (Parts.Release), "+
			"so the start check of the migration ledger %s cannot run; generate the service again with its PostgreSQL adapter", strings.Join(p.SchemaSteps, ", "), LedgerTable)
	}
	return nil
}

// declaresTenant tells whether the authorization schema of the parts declares the definition tenant.
func (p Parts) declaresTenant() bool { return slices.Contains(p.Definitions, authz.TenantType) }

// Identity is the release identity of the parts: the release and the workers digest of the declared workers, which
// the migration job writes in the release identity row and the reconcile of the worker tuples checks
// (ST-SUP-RC35-OC13).
func (p Parts) Identity() tenant.ReleaseIdentity {
	return tenant.ReleaseIdentity{Release: p.Release, Workers: tenant.WorkersDigest(p.WorkerNames)}
}

// Service holds the runtime library objects of one running service, which New gives.
type Service struct {
	parts Parts
	// ring is the key ring of the secret fields, or nil (ST-SUP-RP14-LC12).
	ring          *sealed.Ring
	provider      *authz.SpiceDB
	pool, auditDB *sql.DB
	// chargeDB is the small pool of the charges of checks of counted values (ST-SUP-RP10-LC54).
	chargeDB *sql.DB
	keys     *authn.KeySet
	verifier *authn.Verifier
	// issuer is the issuer whose tokens the verifier accepts, the one trusted issuer of an admin of Provision
	// (ST-SUP-RC27-OC13).
	issuer      string
	spice       *grpc.ClientConn
	telemetry   *otel.Runtime
	probes      *probe.Runtime
	queue       *outbox.Queue
	authz       *authz.Runtime
	writer      *audit.Writer
	commands    *command.Runtime
	collections *command.Runtime
	workers     *command.Runtime
	queries     *query.Runtime
	parked      *parked
	version     string
	beat        *lease.Beat
	tenants     *tenantProvisioning
	// blocked is the reason that makes each mode not ready from the start: the start check found a table or a
	// column of the manifest missing (ST-SUP-RP14-LC06), or empty.
	blocked string
}

// New wires the runtimes of one service from its settings and its declared parts (ST-SUP-RP14-LC02): it opens the
// main pool of the runtime role through tenant.Open with the adapter's connection configuration, reports the
// process under its release (Parts.Release) until Close, refuses a database whose migration ledger does not hold
// that release (ST-SUP-RP14-LC07), alerts a database that does not hold the schema manifest (ST-SUP-RP14-LC06),
// opens the audit pool and the small pool of the charges of counted checks,
// connects to SpiceDB with the preshared key and checks that it answers, starts the telemetry and the probes, and
// gives the verifier, the authorization runtime, the audit writer, the outbox queue and the command and query
// runtimes. It refuses settings or parts that lack a part, and closes what it opened when a step fails.
func New(ctx context.Context, s Settings, p Parts) (_ *Service, err error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	svc := &Service{parts: p}
	identifyLog(p.Service, "")
	defer func() {
		// What a failed step left open is closed.
		if err != nil {
			_ = svc.Close()
		}
	}()
	pools, err := p.TenantConfig(s.DatabaseURL)
	if err != nil {
		return nil, &databaseError{err: err, settings: true}
	}
	if svc.pool, err = tenant.Open(ctx, pools); err != nil {
		return nil, &databaseError{err: err}
	}
	// A database whose migration ledger does not hold the last migration of this release has not had the migration
	// Job of the release: the process stops, and starts again until the Job is done (ST-SUP-RP14-LC07). The check
	// runs before the release report, so a process that stops never reports its release.
	if p.Release != "" {
		if err := checkRelease(ctx, svc.pool, p.Service, p.Release, p.Migrations, p.EarlierMigrations); err != nil {
			return nil, err
		}
	}
	// The process reports its release before the schema check, so a contract step sees it even while the check
	// alerts (decision on finding F8, owner, 2026-11-13).
	if p.Release != "" {
		if svc.beat, err = lease.StartBeat(ctx, svc.pool, p.Release, s.ServiceVersion); err != nil {
			return nil, err
		}
	}
	classes, blocking, err := checkSchema(ctx, svc.pool, p)
	if err != nil {
		return nil, err
	}
	svc.blocked = blockedReason(blocking)
	if svc.auditDB, err = tenant.Open(ctx, pools); err != nil {
		return nil, &databaseError{err: err}
	}
	// The charges of checks of counted values take a small pool of their own, so a full main pool never makes a
	// check wait for a second main connection (security review of PR #451, L2).
	charges := pools
	charges.Pool = tenant.PoolBounds{MaxOpen: chargeConnections, MaxIdle: 1, MaxLifetime: pools.Pool.MaxLifetime, MaxIdleTime: pools.Pool.MaxIdleTime}
	if svc.chargeDB, err = tenant.Open(ctx, charges); err != nil {
		return nil, &databaseError{err: err}
	}
	if svc.queue, err = p.NewQueue(svc.pool); err != nil {
		return nil, err
	}
	auditQueue, err := p.AuditQueue(svc.queue)
	if err != nil {
		return nil, err
	}
	if svc.writer, err = audit.NewWriter(auditQueue, nil); err != nil {
		return nil, err
	}
	if svc.spice, err = DialSpiceDB(s.SpiceDB); err != nil {
		return nil, &spiceDBError{err: err, settings: true}
	}
	if err := reach(ctx, svc.spice); err != nil {
		return nil, err
	}
	if svc.provider, err = authz.NewSpiceDB(svc.spice); err != nil {
		return nil, err
	}
	if svc.authz, err = p.NewAuthz(svc.pool, svc.provider, svc.queue); err != nil {
		return nil, err
	}
	// The telemetry runtime takes the instance identity of the HTTP telemetry of the process, when the generated code
	// gives it, so each line of one process names one instance (core49 follow-up).
	if p.Instance != "" {
		svc.telemetry, err = otel.NewWithInstance(p.Service, p.Instance, probe.SpanNames()...)
	} else {
		svc.telemetry, err = otel.New(p.Service, probe.SpanNames()...)
	}
	if err != nil {
		return nil, err
	}
	identifyLog(svc.telemetry.Service(), svc.telemetry.Instance())
	builtins, err := probe.NewBuiltins(p.WorkerNames, nil)
	if err != nil {
		return nil, err
	}
	if svc.probes, err = probe.Start(svc.telemetry, builtins); err != nil {
		return nil, err
	}
	// A database that does not satisfy the schema manifest is an alert, not a refusal (F10, owner, 2026-11-13).
	for _, class := range classes {
		svc.probes.SchemaMismatch(ctx, class)
	}
	source := authn.KeySource{URL: s.KeysURL, File: s.KeysFile, CAFile: s.CAFile}
	switch {
	case s.Discovery:
		source = authn.KeySource{Issuer: s.Issuer, CAFile: s.CAFile}
	case s.KeyFile != "":
		source = authn.KeySource{PEMFile: s.KeyFile}
	}
	if svc.keys, err = authn.NewKeySet(ctx, source); err != nil {
		return nil, err
	}
	svc.issuer = s.Issuer
	if svc.verifier, err = authn.New(authn.Settings{Issuer: s.Issuer, Audience: s.Audience, Algorithms: s.Algorithms,
		SubjectClaim: s.SubjectClaim, TenantClaim: p.TenantClaim,
		Header: s.Header, RoleClaim: s.RoleClaim, ProblemType: p.ProblemType, ProblemCode: p.ProblemCode,
		Keys: svc.keys}); err != nil {
		return nil, err
	}
	config := command.Config{DB: svc.pool, AuditDB: svc.auditDB, ChargeDB: svc.chargeDB, Authz: svc.authz, Audit: svc.writer, SchemaSteps: p.SchemaSteps,
		AuditSettings: p.AuditSettings, Outbox: svc.queue, Probes: svc.probes, Service: p.Service,
		ServiceVersion: s.ServiceVersion, DeclarationVersion: p.DeclarationVersion, ProblemBase: p.ProblemType, Ring: s.Ring,
		// Each command runtime logs through the log provider of the service, so the lines command.attempts.failed and
		// command.attempts.pool_saturated of a command or a collection are JSON lines with stego.* attributes, as each
		// other log line of the runtime (ST-SUP-RP10-LC54; walkthrough 21, U-E).
		Logger: svc.telemetry.LoggerProvider()}
	// The counter of the charges of counted checks that cannot commit (ST-SUP-RP10-LC54; security review of PR #482, I4).
	config.Meter = svc.telemetry.MeterProvider()
	if err := ringNeeded(p, s.Ring); err != nil {
		return nil, err
	}
	svc.ring = s.Ring
	var gate *tenant.Gate
	if p.Provisioning != nil {
		if p.Workers.TenantSchema == "" {
			return nil, errors.New("service: the tenant provisioning needs the schema of the tenant list")
		}
		if gate, err = tenant.NewGate(svc.pool, p.Workers.TenantSchema, tenant.MaxGateAge); err != nil {
			return nil, err
		}
		// A consumer of the service runs no fill for a message of a tenant that is not active (ST-SUP-RC27-OC12).
		config.Tenants = gate
	}
	if len(p.Commands) > 0 {
		config.Entities = p.Commands
		if svc.commands, err = command.New(ctx, config); err != nil {
			return nil, err
		}
	}
	if len(p.Collections) > 0 {
		config.Entities = p.Collections
		if svc.collections, err = command.New(ctx, config); err != nil {
			return nil, err
		}
	}
	if len(p.WorkerEntities) > 0 {
		// The worker runs and the consumed messages run on the runtime of the workers' entities; a consumer runs no
		// fill for a message of a tenant that is not active (ST-SUP-RC27-OC12).
		config.Entities = p.WorkerEntities
		// A consumer attempt that does not commit writes one warn log line through the log provider (ST-SUP-RC35-OC06).
		workers := config
		if p.Provisioning != nil && p.declaresTenant() && svc.provider != nil {
			// A consumer runs no fill before the tuple of its worker is in SpiceDB, which Provision and the reconcile
			// of the controller write (ST-SUP-RC35-OC18).
			workers.WorkerTuples = svc.provider
		}
		if svc.workers, err = command.New(ctx, workers); err != nil {
			return nil, err
		}
	}
	if p.Provisioning != nil {
		// Provision and Retire are commands of their own, on a command runtime with no entity (ST-SUP-RC27-OC09),
		// which logs the outcomes that write no audit row (ST-SUP-RC27-OC10).
		config.Entities = nil
		provisioning, err := command.New(ctx, config)
		if err != nil {
			return nil, err
		}
		if svc.tenants, err = newProvisioning(svc, provisioning, p.Provisioning, p.Workers.TenantSchema, gate); err != nil {
			return nil, err
		}
	}
	if svc.queries, err = query.New(query.Config{DB: svc.pool, AuditDB: svc.auditDB, Audit: svc.writer,
		AuditSettings: p.AuditSettings, Authz: svc.authz, Service: p.Service, ServiceVersion: s.ServiceVersion,
		DeclarationVersion: p.DeclarationVersion, CursorKey: s.CursorKey,
		ExtraCursorKeys: extraKeys(s.ExtraCursorKey), ProblemBase: p.ProblemType,
		Meter: svc.telemetry.MeterProvider()}); err != nil {
		return nil, err
	}
	// The tenant list of the operator, when the schema declares list_tenants (ST-SUP-RC27-OC16); its cursor is sealed
	// as the query cursors are.
	if svc.tenants != nil && p.Provisioning.List != "" {
		if svc.tenants.list, err = command.ServeTenantList(svc.tenants.rt, command.TenantListSpec{
			ResourceType: p.Provisioning.ResourceType, Schema: p.Workers.TenantSchema, SubjectType: p.SubjectType,
			Active: svc.tenants.gate.Active, Cursors: svc.queries}); err != nil {
			return nil, err
		}
	}
	// The parked-message list and resolve of an operator, which the generated code serves in the api mode only
	// (ST-SUP-RP14-LC09, ST-SUP-RP14-LC10).
	svc.version = s.ServiceVersion
	if svc.parked, err = newParked(svc); err != nil {
		return nil, err
	}
	return svc, nil
}

// LedgerTable is the migration ledger of the PostgreSQL adapter: one row for each migration that a migration job
// applied, with its name in the column version.
const LedgerTable = "stego_schema.migrations"

// ErrMigrationPending is the refusal of a start on a database whose migration ledger does not hold the last
// migration of the release (ST-SUP-RP14-LC07).
var ErrMigrationPending = errors.New("the migration Job of this release has not finished")

// checkRelease reads the migration ledger as the runtime role and refuses a database whose ledger is not there, that
// the runtime role cannot read, or that does not hold the migration release, the last migration of this release
// (ST-SUP-RP14-LC07). On plain Kubernetes nothing runs the migration Job before the rollout, so the pod of a new
// release can start first: it stops, and Kubernetes starts it again until the Job is done. The refusal names the
// ledger, the migration and what to do, on a log line of the level error and in its error, because the failure record
// of the process names the stage only. Neither holds a value of a setting.
func checkRelease(ctx context.Context, db *sql.DB, service, release string, digests map[string]string, earlier map[string][]string) error {
	// The catalog alone tells whether the ledger is there and readable, so that no name resolution in a schema
	// without USAGE fails the read.
	var present, readable bool
	if err := db.QueryRowContext(ctx, `SELECT count(*) > 0, COALESCE(bool_and(pg_catalog.has_schema_privilege(n.oid, 'USAGE')
  AND pg_catalog.has_table_privilege(c.oid, 'SELECT')), false)
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'stego_schema' AND c.relname = 'migrations' AND c.relkind = 'r'`).Scan(&present, &readable); err != nil {
		return fmt.Errorf("service: read the migration ledger %s in the catalog: %w", LedgerTable, err)
	}
	var applied bool
	if readable {
		if err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM "+LedgerTable+" WHERE version = $1)", release).Scan(&applied); err != nil {
			return fmt.Errorf("service: read the migration ledger %s: %w", LedgerTable, err)
		}
	}
	var state, cause string
	switch {
	case applied:
		return checkDigests(ctx, db, release, digests, earlier)
	case !present:
		state, cause = "is not in the database, so no migration Job has run on it", MigrationCauseNoLedger
	case !readable:
		state, cause = "is not readable by the runtime role; the migration Job of this release grants SELECT on it", MigrationCauseNotGranted
	default:
		state, cause = "does not hold the migration "+release+", the last migration of this release", MigrationCauseMissing
	}
	text := fmt.Sprintf("the migration ledger %s %s. Run the migration Job of this release (the service binary with the argument migrate, "+
		"in Kubernetes the Job %s-migrate-<release> of the base) and wait until it completes; this process stops, and it starts again until then",
		LedgerTable, state, service)
	slog.ErrorContext(ctx, "service: "+text, otel.SlogEventKey, "service.start.migration_refused", "migration", release, "ledger", LedgerTable, "stage", "service.start.migration")
	return &migrationError{err: fmt.Errorf("service: %w: %s", ErrMigrationPending, text), cause: cause}
}

// ErrMigrationChanged is the refusal of a start on a database whose migration ledger holds another body of a
// migration of the release (ST-SUP-RP14-LC13).
var ErrMigrationChanged = errors.New("the ledger holds another body of a migration of this release")

// checkDigests reads, as the runtime role, the ledger row of each migration of digests and refuses a row whose
// digest is not the digest of this release (ST-SUP-RP14-LC13), and a migration of digests that has no row, as the
// migration Job of this release writes a row for each (ErrMigrationPending; security review of PR 448, I1). A row of
// a name that digests does not give, such as a step of a later release, passes. The refusal names the migration and
// the digests, no part of a body, and the remedy, on a log line of the level error and in its error; New writes
// nothing before it. A row that holds an earlier digest of its migration (earlier) passes.
func checkDigests(ctx context.Context, db *sql.DB, release string, digests map[string]string, earlier map[string][]string) error {
	if len(digests) == 0 {
		return nil
	}
	// The ledger holds one row for each applied migration, so the check reads them all.
	rows, err := db.QueryContext(ctx, "SELECT version, digest FROM "+LedgerTable)
	if err != nil {
		return fmt.Errorf("service: read the digests of the migration ledger %s: %w", LedgerTable, err)
	}
	defer rows.Close()
	seen := make(map[string]bool, len(digests))
	for rows.Next() {
		var name, digest string
		if err := rows.Scan(&name, &digest); err != nil {
			return fmt.Errorf("service: read the digests of the migration ledger %s: %w", LedgerTable, err)
		}
		want, ok := digests[name]
		seen[name] = ok
		if ok && digest != want && !slices.Contains(earlier[name], digest) {
			text := fmt.Sprintf("the migration ledger %s holds the digest %q for the migration %s, and this release gives the digest %q for its body. "+
				"The body of an applied migration never changes, so a change comes as a new schema step: deploy the release that applied it, "+
				"or a release that keeps its body and brings the change in a new schema step; nothing repairs the digest", LedgerTable, digest, name, want)
			slog.ErrorContext(ctx, "service: "+text, otel.SlogEventKey, "service.start.migration_refused", "migration", name, "ledger", LedgerTable, "stage", "service.start.migration")
			return &migrationError{err: fmt.Errorf("service: %w: %s", ErrMigrationChanged, text), cause: MigrationCauseDigest}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("service: read the digests of the migration ledger %s: %w", LedgerTable, err)
	}
	missing := make([]string, 0)
	for name := range digests {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	text := fmt.Sprintf("the migration ledger %s holds no row of the migration %s of this release, which the migration Job of this release applies "+
		"before the release migration %s. Run the migration Job of this release and wait until it completes; this process stops, and it starts again until then",
		LedgerTable, strings.Join(missing, ", "), release)
	slog.ErrorContext(ctx, "service: "+text, otel.SlogEventKey, "service.start.migration_refused", "migration", missing[0], "ledger", LedgerTable, "stage", "service.start.migration")
	return &migrationError{err: fmt.Errorf("service: %w: %s", ErrMigrationPending, text), cause: MigrationCauseMissing}
}

// migrationError is a start that the check of the migration ledger refused (ST-SUP-RP14-LC07, ST-SUP-RP14-LC13): its
// error is ErrMigrationPending or ErrMigrationChanged with the text of the log line. The failure record of the
// process names its fault class, the ledger as its object, a fixed remedy and a cause class, so the record tells the
// reason with no separate log line (walkthrough 24, D-l). None of them holds a migration name, a digest or a value.
type migrationError struct {
	err   error
	cause string
}

func (e *migrationError) Error() string { return e.err.Error() }
func (e *migrationError) Unwrap() error { return e.err }

// The fault classes of a start that the check of the migration ledger refused: a migration of the release that the
// ledger does not hold, and a migration whose ledger row holds another digest.
const (
	MigrationPendingFault = "migration.pending"
	MigrationChangedFault = "migration.changed"
)

// The cause classes of a refused check of the migration ledger: no ledger, a ledger that the runtime role may not
// read, a migration of the release that it does not hold, and another digest of a migration of the release.
const (
	MigrationCauseNoLedger   = "ledger_missing"
	MigrationCauseNotGranted = "ledger_not_granted"
	MigrationCauseMissing    = "migration_missing"
	MigrationCauseDigest     = "digest_changed"
)

// FaultClass gives the fault class: pending or changed.
func (e *migrationError) FaultClass() string {
	if errors.Is(e.err, ErrMigrationChanged) {
		return MigrationChangedFault
	}
	return MigrationPendingFault
}

// FaultObject gives the object of the fault, the migration ledger.
func (e *migrationError) FaultObject() string { return "migration_ledger" }

// FaultRemedy gives what to do, a fixed text for each fault class.
func (e *migrationError) FaultRemedy() string {
	if errors.Is(e.err, ErrMigrationChanged) {
		return "the ledger holds another body of a migration of this release, and the body of an applied migration never changes: deploy the release that applied it, or a release that keeps its body and brings the change in a new schema step"
	}
	return "run the migration Job of this release (the service binary with the argument migrate, in Kubernetes the Job <service>-migrate-<release> of the base) and wait until it completes; this process starts again until then"
}

// CauseClass gives the class of the cause, a fixed name.
func (e *migrationError) CauseClass() string { return e.cause }

// MaxLoggedFaults is the most faults of the schema manifest check that the start of a service logs, each on its own
// line.
const MaxLoggedFaults = 50

// checkSchema checks the database against the schema manifest of the service, of the audit record, of the outbox, of
// the authorization runtime and of the key table (ST-SUP-RP14-LC06): it reads the catalog as the runtime role before
// New opens anything else. A database that does not satisfy the manifest is an alert and not a refusal (decision on
// finding F10 of the schema-change research, owner, 2026-11-13): checkSchema logs each fault, up to MaxLoggedFaults, on a line of its own at
// the level error, with its class (manifest.FaultClass), which names only objects of the catalog and no row value,
// and gives the classes of the faults, each once, so that New emits the probe observation schema.manifest.mismatch
// for each class and starts, and the faults that block (manifest.Mismatch.Blocking), with which the process starts
// not ready (ST-SUP-RP14-LC06); the migration job refuses the same database. Any other failure of the check,
// and parts that give no manifest, it refuses. The audit triggers' functions are those that the audit DDL makes for
// the declared anchor period (Parts.Workers.AnchorPeriod), and audit_maintain is the one of the declared retention
// (Parts.Workers.RetentionMonths).
func checkSchema(ctx context.Context, db *sql.DB, p Parts) (classes, blocking []string, err error) {
	// The check runs as the runtime role, which alone may run audit_maintain.
	var runtimeRole string
	if err := db.QueryRowContext(ctx, "SELECT current_user").Scan(&runtimeRole); err != nil {
		return nil, nil, fmt.Errorf("service: read the runtime role: %w", err)
	}
	// The runtime role is not the owner role and is no member of it (ST-SUP-RC01-OC21), which New refuses and does not
	// alert: the owner role owns the tables, so it is not held by their row-level security, and it runs the backfill,
	// whose sync trigger passes the writes of a transaction that names the change in stego.backfill_change, a setting
	// that any role can set (security review of PR #348). A role is a member of itself, and a superuser of each role,
	// so the one test refuses the owner role, each member of it and a superuser.
	var owner bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_roles r
WHERE r.rolname = $1::pg_catalog.text AND pg_catalog.pg_has_role(current_user, r.oid, 'MEMBER'))`, p.OwnerRole).Scan(&owner); err != nil {
		return nil, nil, fmt.Errorf("service: read the membership of the runtime role: %w", err)
	}
	if owner {
		return nil, nil, fmt.Errorf("service: the runtime role %s is the owner role %s or a member of it; give DATABASE_URL the login of the runtime role", runtimeRole, p.OwnerRole)
	}
	want, err := joinedManifest(p, runtimeRole)
	if err != nil {
		return nil, nil, err
	}
	err = manifest.Check(ctx, db, want)
	var mismatch *manifest.Mismatch
	if errors.As(err, &mismatch) {
		for i, fault := range mismatch.Faults {
			class := manifest.FaultClass(fault)
			// At most MaxLoggedFaults lines, and one line with the count of the rest, as a role that can make
			// catalog objects can make thousands of faults (review of PR #305, L2). Each fault still gives its class.
			// A fault that blocks says that the service starts not ready (ST-SUP-RP14-LC06, ST-SUP-RP15-LC05).
			if i < MaxLoggedFaults {
				text := "service: the database does not satisfy the schema manifest; the service starts, and the migration job refuses it"
				if slices.Contains(mismatch.Blocking, fault) {
					text = "service: the database does not satisfy the schema manifest; the service starts not ready, and the migration job refuses it"
				}
				slog.ErrorContext(ctx, text, otel.SlogEventKey, "service.schema_manifest.fault", "class", class, "fault", fault)
			}
			if !slices.Contains(classes, class) {
				classes = append(classes, class)
			}
		}
		if rest := len(mismatch.Faults) - MaxLoggedFaults; rest > 0 {
			slog.ErrorContext(ctx, "service: the database does not satisfy the schema manifest; more faults are not logged", otel.SlogEventKey, "service.schema_manifest.faults_held", "faults", rest)
		}
		return classes, mismatch.Blocking, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("service: %w", err)
	}
	return nil, nil, nil
}

// Blocked gives, for each mode of selection, the error of a mode that is not ready because the start check found a
// table or a column of the manifest missing (ST-SUP-RP14-LC06, ST-SUP-RP09-LC05), or nil. The generated readiness of
// a process that runs the api mode alone, which starts no worker, reads it too.
func (svc *Service) Blocked(selection modes.Selection) error {
	if svc == nil || svc.blocked == "" {
		return nil
	}
	var errs []error
	for _, m := range selection.Modes() {
		errs = append(errs, fmt.Errorf("%s: %w: %s", m, modes.ErrNotReady, svc.blocked))
	}
	return errors.Join(errs...)
}

// blockedReason gives the reason of the readiness of a process whose start check found the faults blocking, each a
// missing table or column of the manifest (ST-SUP-RP14-LC06): a fixed text with the count of the faults and the
// remedy. It is empty with no fault.
func blockedReason(blocking []string) string {
	if len(blocking) == 0 {
		return ""
	}
	// The readiness answers network callers, so the reason names no table or column; the log lines of the start name
	// each one (security review of PR #457, L3).
	return fmt.Sprintf("the database does not hold the schema manifest of this release (%d faults, which the log of the start names); a later release changed the database, so roll forward to that release", len(blocking))
}

// Ping gives, for a process of the api mode alone, which starts no worker, the error of an api mode that is not ready
// because the database does not answer a ping, with the fixed reason of modes.DatabaseUnreachable, or nil (ST-SUP-RP09-LC05).
// The error of the ping goes only to the log (security review of PR #457, L3).
func (svc *Service) Ping(ctx context.Context) error {
	if svc == nil || svc.pool == nil {
		return fmt.Errorf("%s: %w: %s", modes.API, modes.ErrNotReady, modes.DatabaseUnreachable)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := svc.pool.PingContext(ctx); err != nil {
		slog.WarnContext(ctx, "service: the database is not reachable", otel.SlogEventKey, "service.database.unreachable", "mode", string(modes.API), "error", err)
		return fmt.Errorf("%s: %w: %s", modes.API, modes.ErrNotReady, modes.DatabaseUnreachable)
	}
	return nil
}

// joinedManifest is the schema manifest that New checks at the start and the migration job checks at its end: the
// manifest of the declared parts (Parts.Schema) joined with the manifests of the audit record, the outbox, the
// authorization runtime and the key table for the runtime role runtimeRole, owned by the owner role of the parts.
func joinedManifest(p Parts, runtimeRole string) (manifest.Manifest, error) {
	record, err := audit.Manifest(audit.Config{AnchorPeriod: p.Workers.AnchorPeriod, RetentionMonths: p.Workers.RetentionMonths, RuntimeRole: runtimeRole})
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("service: the manifest of the audit record: %w", err)
	}
	queue, err := outbox.Manifest(p.OutboxRelayRole, runtimeRole, p.WorkerRole)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("service: the manifest of the outbox: %w", err)
	}
	// The outbox relay role owns the state hook functions of the authorization runtime too, so its manifest is in
	// the check, or the confined relay role would own functions that the manifest does not name.
	revocations, err := authz.Manifest(p.AuthzSchema, runtimeRole, p.RelayRole, p.OutboxRelayRole, p.WorkerRole)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("service: the manifest of the authorization runtime: %w", err)
	}
	// The migration job makes the key table in idempotency.KeySchema, where Wrap and Purge name it.
	keys, err := idempotency.ManifestWithConsumers(idempotency.KeySchema, p.PurgeRole, runtimeRole, consumerRoles(&p, p.WorkerRole))
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("service: the manifest of the key table: %w", err)
	}
	want := manifest.Join(p.Schema, record, queue, revocations, keys, identityManifest(p, runtimeRole))
	want.Owner = p.OwnerRole
	if err := entityGrants(&want, p, runtimeRole); err != nil {
		return manifest.Manifest{}, err
	}
	// The grants of a consuming worker on a table whose grants the manifest checks; the key table names its
	// consumer roles itself (ST-SUP-Q136).
	for _, g := range consumerGrants(&p) {
		for i := range want.Tables {
			t := &want.Tables[i]
			if t.CheckGrants && t.Schema == g.schema && t.Name == g.table && t.Name != "idempotency_keys" {
				t.Grants = append(t.Grants, manifest.Grant{Role: p.WorkerRole, Privileges: g.privileges, Columns: g.columns})
			}
		}
	}
	return want, nil
}

// runtimeGrant reads a GRANT statement of the runtime role, without its grantee: its privileges, the kind of its
// object and the quoted object. grantedTable reads the schema and the name of a table from its object. The manifest
// of the entity tables reads the grants on a table only.
var (
	runtimeGrant = regexp.MustCompile(`^GRANT ([A-Z]+(?:, [A-Z]+)*) ON (TABLE|SEQUENCE|SCHEMA) ("[^"]+"(?:\."[^"]+")?)$`)
	grantedTable = regexp.MustCompile(`^"([^"]+)"\."([^"]+)"$`)
)

// tableKind is the kind of the object of a grant on a table.
const tableKind = "TABLE"

// The columns of the grants of the worker role on the table of an entity with a resource type, which the
// authorization migration gives so the worker stores the token of each row (ST-SUP-RC29).
var (
	tokenReadColumns  = []string{"tenant_id", "id"}
	tokenWriteColumns = []string{"authz_token"}
)

// entityGrants makes the manifest check the grants of the table of each declared entity (ST-SUP-RP14-LC06): the
// privileges of the runtime role that the GRANT statements of the parts give on it, and, on a table with the
// authorization token, SELECT of the tenant and the key and UPDATE of the token for the worker role. The worker's
// grants of a consuming worker follow. So a grant that the migration job did not give, and one that it gave and that
// is gone, is a fault. It refuses a statement on a table that is not of the form that the generated code writes.
func entityGrants(want *manifest.Manifest, p Parts, runtimeRole string) error {
	granted := map[[2]string][]string{}
	for _, statement := range p.Grants {
		match := runtimeGrant.FindStringSubmatch(statement)
		var table []string
		if match != nil && match[2] == tableKind {
			table = grantedTable.FindStringSubmatch(match[3])
		}
		if match != nil && match[2] != tableKind {
			continue
		}
		if table == nil {
			return fmt.Errorf("service: the runtime grant %q of the parts is not of the form that the generated code writes, with each table named with its schema, so the schema manifest cannot check it (Parts.Grants)", statement)
		}
		key := [2]string{table[1], table[2]}
		granted[key] = append(granted[key], strings.Split(match[1], ", ")...)
	}
	for i := range want.Tables {
		t := &want.Tables[i]
		if !t.Entity {
			continue
		}
		t.CheckGrants = true
		if privileges := granted[[2]string{t.Schema, t.Name}]; len(privileges) > 0 {
			t.Grants = append(t.Grants, manifest.Grant{Role: runtimeRole, Privileges: privileges})
		}
		if slices.ContainsFunc(t.Columns, func(c manifest.Column) bool { return c.Name == tokenWriteColumns[0] }) {
			t.Grants = append(t.Grants, manifest.Grant{Role: p.WorkerRole, Privileges: []string{"SELECT"}, Columns: tokenReadColumns},
				manifest.Grant{Role: p.WorkerRole, Privileges: []string{"UPDATE"}, Columns: tokenWriteColumns})
		}
	}
	return nil
}

// consumerRoles are the roles that run a consuming worker's fill: the worker role, whose outbox worker runs it in
// the transaction of the claimed message, when the service has a consuming worker (ST-SUP-RC35-OC06, ST-SUP-Q136).
func consumerRoles(p *Parts, workerRole string) []string {
	if p == nil || p.Workers.Consumers == nil {
		return nil
	}
	return []string{workerRole}
}

// reach checks that SpiceDB answers its health check within ReachTimeout, so a service whose SpiceDB cannot be
// reached does not start.
func reach(ctx context.Context, conn *grpc.ClientConn) error {
	ctx, cancel := context.WithTimeout(ctx, ReachTimeout)
	defer cancel()
	answer, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		return &spiceDBError{err: err}
	}
	if answer.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		return &spiceDBError{notServing: true}
	}
	return nil
}

// spiceDBError is a start whose SpiceDB the service cannot use: the settings of the connection are refused
// (settings), SpiceDB does not answer its health check within ReachTimeout, or it answers that it does not serve
// (notServing). The failure record of the process names the fault class, spicedb as its object, a fixed remedy and a
// cause class, with no text of the cause, as the failure record of the migration job does for its SpiceDB steps
// (walkthrough 24, D-m). Its error text names only the setting, never the endpoint, the key or the text of the cause.
type spiceDBError struct {
	err                  error
	settings, notServing bool
}

func (e *spiceDBError) Error() string {
	switch {
	case e.settings:
		return fmt.Sprintf("service: SpiceDB needs the settings %s and %s", SpiceDBEndpointSetting, SpiceDBKeyFileSetting)
	case e.notServing:
		return fmt.Sprintf("service: SpiceDB at the setting %s is not serving", SpiceDBEndpointSetting)
	}
	return fmt.Sprintf("service: SpiceDB at the setting %s cannot be reached", SpiceDBEndpointSetting)
}

// Unwrap gives the cause of the health check, so the failure record classes it; the error text never shows it.
func (e *spiceDBError) Unwrap() error { return e.err }

// The fault classes of a start whose SpiceDB the service cannot use: the connection, as the migration job names it
// (MigrateSpiceDBFault), and the settings of the connection.
const (
	SpiceDBConnectionFault = MigrateSpiceDBFault
	SpiceDBSettingsFault   = "spicedb.settings"
)

// FaultClass gives the fault class: the settings or the connection.
func (e *spiceDBError) FaultClass() string {
	if e.settings {
		return SpiceDBSettingsFault
	}
	return SpiceDBConnectionFault
}

// FaultObject gives the object of the fault, the SpiceDB of the service.
func (e *spiceDBError) FaultObject() string { return "spicedb" }

// FaultRemedy gives what to do, a fixed text for each fault class.
func (e *spiceDBError) FaultRemedy() string {
	if e.settings {
		return "the settings of SpiceDB are refused: set " + SpiceDBEndpointSetting + " and " + SpiceDBKeyFileSetting + ", then start the service again"
	}
	return "the service cannot reach SpiceDB or SpiceDB does not serve: the service starts again; when it does not pass, check that SpiceDB runs and serves, that " +
		SpiceDBEndpointSetting + " names it and that " + SpiceDBKeyFileSetting + " holds its preshared key"
}

// CauseClass gives the class of the cause: invalid_setting for refused settings, not_serving for a health check that
// answers that SpiceDB does not serve, the class of the gRPC status code of the health check, such as unavailable or
// deadline_exceeded (authz.StatusClass), and else connection_failed. It never holds the text of the cause.
func (e *spiceDBError) CauseClass() string {
	switch {
	case e.settings:
		return "invalid_setting"
	case e.notServing:
		return "not_serving"
	}
	if class := authz.StatusClass(e.err); class != "" {
		return class
	}
	return "connection_failed"
}

// DialSpiceDB connects to SpiceDB with its preshared key, over TLS 1.2 or later that the roots of the settings verify
// (the system roots, with the CA file added; ST-SUP-RP14-LC14), or without TLS to a loopback address when the
// settings allow it for a local test.
func DialSpiceDB(s SpiceDB) (*grpc.ClientConn, error) {
	if s.Endpoint == "" || s.Key == "" {
		return nil, fmt.Errorf("service: SpiceDB needs the settings %s and %s", SpiceDBEndpointSetting, SpiceDBKeyFileSetting)
	}
	transport := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: s.Roots})
	if s.InsecureLoopback {
		transport = insecure.NewCredentials()
	}
	return grpc.NewClient(s.Endpoint, grpc.WithTransportCredentials(transport),
		grpc.WithPerRPCCredentials(presharedKey{token: s.Key, plaintext: s.InsecureLoopback}))
}

// presharedKey sends the preshared key of SpiceDB on each call.
type presharedKey struct {
	token     string
	plaintext bool
}

func (k presharedKey) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + k.token}, nil
}

func (k presharedKey) RequireTransportSecurity() bool { return !k.plaintext }

// databaseError is a start whose database pool did not open: the connection configuration of the adapter
// (settings), the connection, or the checks of the runtime role refused it. The failure record of the process names
// which of the three as its fault class, the database as its object, the remedy and a cause class, fixed texts with no
// text of the cause (walkthrough 21, U-F).
type databaseError struct {
	err      error
	settings bool
}

func (e *databaseError) Error() string {
	return "service: the database pool did not open: " + e.err.Error()
}

func (e *databaseError) Unwrap() error { return e.err }

// The fault classes of a database pool that did not open.
const (
	DatabaseSettingsFault    = "database.settings"
	DatabaseRuntimeRoleFault = "database.runtime_role"
	DatabaseConnectionFault  = "database.connection"
)

// FaultClass gives the fault class: the settings, the runtime role or the connection.
func (e *databaseError) FaultClass() string {
	switch {
	case e.settings:
		return DatabaseSettingsFault
	case errors.Is(e.err, tenant.ErrRuntimeRole):
		return DatabaseRuntimeRoleFault
	}
	return DatabaseConnectionFault
}

// FaultObject gives the object of the fault, the database of the service.
func (e *databaseError) FaultObject() string { return "database" }

// FaultRemedy gives what to do, a fixed text for each fault class.
func (e *databaseError) FaultRemedy() string {
	switch e.FaultClass() {
	case DatabaseSettingsFault:
		return "the connection settings of the database are refused: correct DATABASE_URL and the TLS settings of the database, then start the service again"
	case DatabaseRuntimeRoleFault:
		return "the runtime role fails a check, such as a role that owns a table or holds BYPASSRLS: give the service the runtime role that the migration job makes, then start the service again"
	}
	return "the database did not accept the connection or closed it, as while the database starts or after a restart of its server: the service starts again; when it does not pass, check DATABASE_URL and that the database runs"
}

// CauseClass gives the class of the cause that the failure record cannot class from the standard library or the
// SQLSTATE: connection_closed for a connection that ended in the middle of a message, as when the database server
// restarts (walkthrough 21, step 11); refused for a runtime role that fails a check; invalid_setting for a refused
// connection setting; and connection_failed for any other cause of the connection. It never holds the text of the
// cause.
func (e *databaseError) CauseClass() string {
	switch {
	case errors.Is(e.err, io.EOF) || errors.Is(e.err, io.ErrUnexpectedEOF):
		return "connection_closed"
	case e.settings:
		return "invalid_setting"
	case errors.Is(e.err, tenant.ErrRuntimeRole):
		return "refused"
	}
	return "connection_failed"
}

// UseJSONLog makes log/slog, and the standard logger with it, write each line of the process as one JSON line on
// standard error, in the format of the console lines of the log provider, with the attributes under stego.
// (otel.SlogHandler; ST-SUP-RP03-LC16, walkthrough 21, U-E). So the lines of the runtime that do not go through the
// log provider, such as the lines of the seal jobs of the controller and the refused step of the migration job, are
// JSON lines too. The generated code calls it first in NewService and in Migrate. The handler writes the lines of
// the level of STEGO_LOG_LEVEL or higher, INFO when it has no value (decision on ST-SUP-Q210); a value that is not a
// level gives INFO here, and the start of the service and the migration job refuse it (probe.Start, Migrate).
func UseJSONLog() {
	level, err := otel.ParseLogLevel(os.Getenv(logLevelSetting))
	if err != nil {
		level = slog.LevelInfo
	}
	slog.SetDefault(slog.New(otel.NewSlogHandlerAt(os.Stderr, level)))
}

// identifyLog gives the handler of log/slog that UseJSONLog set the service name and the instance identity that its
// lines name (walkthrough 25, L-1). It does nothing when another handler is the default.
func identifyLog(service, instance string) {
	if h, ok := slog.Default().Handler().(*otel.SlogHandler); ok {
		h.SetIdentity(service, instance)
	}
}

// StartStage gives the stage of a start that ReadSettings or New refused, for the failure record of the process,
// which holds no error text and no value (ST-SUP-RP14-LC05): service.start.setting.<component>.<variable> for a
// setting that ReadSettings refused, with the fixed name of the setting and the registry component that reads it,
// such as service.start.setting.postgres-adapter.DATABASE_URL; service.start.database for a database pool that did
// not open; service.start.spicedb for a SpiceDB that the service cannot use; service.start.authn.discovery for a discovery document that the key set refused,
// service.start.authn.keys for another key source that it refused, service.start.schema_manifest for a database that
// does not hold the schema manifest (ST-SUP-RP14-LC06), service.start.migration for a database whose migration
// ledger does not hold the last migration of the release (ST-SUP-RP14-LC07), and empty for every other failure.
func StartStage(err error) string {
	var setting *SettingError
	var database *databaseError
	var spice *spiceDBError
	switch {
	case errors.As(err, &setting) && setting.Component != "" && setting.Variable != "":
		return "service.start.setting." + setting.Component + "." + setting.Variable
	case errors.As(err, &database):
		return "service.start.database"
	case errors.As(err, &spice):
		return "service.start.spicedb"
	case errors.Is(err, manifest.ErrNotSatisfied):
		return "service.start.schema_manifest"
	case errors.Is(err, ErrMigrationPending) || errors.Is(err, ErrMigrationChanged):
		return "service.start.migration"
	case errors.Is(err, authn.ErrDiscovery):
		return "service.start.authn.discovery"
	case errors.Is(err, authn.ErrKeys):
		return "service.start.authn.keys"
	}
	return ""
}

// Verifier is the verifier of the bearer tokens; Middleware verifies the token of each request before next.
func (svc *Service) Verifier() *authn.Verifier { return svc.verifier }

// Middleware verifies the bearer token of each request before next; a request whose token the verifier refuses is
// answered 401 and reaches no handler. For a service that declares its provisioning, a request whose verified tenant
// is not an active tenant of the tenant list is answered 403 with the code tenant.not_provisioned and reaches no
// handler (ST-SUP-RC27-OC11). Middleware knows no router, so it exempts no route: a service that serves the two
// provisioning routes wraps its main router with RoutedMiddleware.
func (svc *Service) Middleware(next http.Handler) http.Handler {
	return svc.RoutedMiddleware(nil, next)
}

// RoutedMiddleware is Middleware for the main router of the service, router, which next ends in. The tenant check
// lets a request of a tenant that is not active through only when router matches it to the pattern of Provision or
// of Retire, so a route of another handler under <base path>/stego/ is never exempt (ST-SUP-RC27-OC11). With no
// router, no request is exempt.
func (svc *Service) RoutedMiddleware(router *http.ServeMux, next http.Handler) http.Handler {
	if svc.tenants != nil {
		next = svc.tenants.gated(router, next)
	}
	return svc.verifier.Middleware(next)
}

// Commands is the command runtime of the declared commands, and nil when the service declares none.
func (svc *Service) Commands() *command.Runtime { return svc.commands }

// WorkerCommands is the command runtime of the declared workers, and nil when the service declares none.
func (svc *Service) WorkerCommands() *command.Runtime { return svc.workers }

// Collections is the command runtime of the generated collection commands, and nil when the service has none.
func (svc *Service) Collections() *command.Runtime { return svc.collections }

// Queries is the query runtime.
func (svc *Service) Queries() *query.Runtime { return svc.queries }

// Probes is the probe runtime, which binds the probe call of each query call.
func (svc *Service) Probes() *probe.Runtime { return svc.probes }

// Authz is the authorization runtime, Writer the audit writer and Queue the outbox queue.
func (svc *Service) Authz() *authz.Runtime    { return svc.authz }
func (svc *Service) Writer() *audit.Writer    { return svc.writer }
func (svc *Service) Queue() *outbox.Queue     { return svc.queue }
func (svc *Service) Telemetry() *otel.Runtime { return svc.telemetry }

// Close releases what New opened, and deletes the report of the process.
func (svc *Service) Close() error {
	var errs []error
	if svc.probes != nil {
		errs = append(errs, svc.probes.Close(context.Background()))
	} else if svc.telemetry != nil {
		errs = append(errs, svc.telemetry.Shutdown(context.Background()))
	}
	if svc.verifier != nil {
		svc.verifier.Stop()
	}
	if svc.keys != nil {
		svc.keys.Stop()
	}
	if svc.spice != nil {
		errs = append(errs, svc.spice.Close())
	}
	if svc.beat != nil {
		errs = append(errs, svc.beat.Stop(context.Background()))
	}
	for _, db := range []*sql.DB{svc.chargeDB, svc.auditDB, svc.pool} {
		if db != nil {
			errs = append(errs, db.Close())
		}
	}
	return errors.Join(errs...)
}

// chargeConnections is the most connections of the pool of the charges: a charge is one short statement, so a few
// connections serve the checks of all the commands that the main pool runs at one time.
const chargeConnections = 4

// identityManifest is the manifest of the table of the release identity row in the schema of the tenant list: one
// row, read by the runtime role only (ST-SUP-RP14-LC06).
func identityManifest(p Parts, runtimeRole string) manifest.Manifest {
	schema := p.Workers.TenantSchema
	if schema == "" {
		schema = "public"
	}
	return manifest.Manifest{Tables: []manifest.Table{{
		Schema: schema, Name: tenant.IdentityTable,
		Columns: []manifest.Column{
			{Name: "only_row", Type: "boolean", NotNull: true, Default: "true"},
			{Name: "release", Type: "text", NotNull: true},
			{Name: "workers", Type: "text", NotNull: true},
			{Name: "written_at", Type: "timestamp with time zone", NotNull: true, Default: "now()"},
		},
		Constraints: []manifest.Constraint{
			{Name: tenant.IdentityTable + "_pkey", Kind: manifest.KindPrimaryKey, Definition: "PRIMARY KEY (only_row)"},
			{Name: tenant.IdentityTable + "_only_row_check", Kind: manifest.KindCheck, Definition: "CHECK (only_row)"},
		},
		Indexes:     []manifest.Index{{Name: tenant.IdentityTable + "_pkey", Columns: []string{"only_row"}}},
		CheckGrants: true,
		Grants:      []manifest.Grant{{Role: runtimeRole, Privileges: []string{"SELECT"}}},
	}}}
}

// extraKeys gives the extra cursor key as the list of query.Config, or none.
func extraKeys(key []byte) [][]byte {
	if len(key) == 0 {
		return nil
	}
	return [][]byte{key}
}

// ringNeeded refuses a service with a secret field, readable or compare-only, or a pii field, and no key ring: with
// no ring the idempotency store keeps an unkeyed SHA-256 fingerprint of each request body, which a reader of the
// database can test with guessed values of a short secret such as a PIN, and an answer in clear (ST-SUP-RP14-LC12;
// security review of PR #444, M1).
func ringNeeded(p Parts, ring *sealed.Ring) error {
	if ring == nil && (p.KeyRing || len(p.Reseal) > 0 || len(p.Seal) > 0) {
		return settingError(KeyRingFileSetting, "is not read: the service has a secret or a pii field and no key ring")
	}
	return nil
}
