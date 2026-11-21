package service

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/audit/auditsink"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/command"
	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/modes"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/outbox/kafkainbox"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// The fixed names of the connection settings of the worker mode (ST-SUP-RP14-LC04), each with its file form.
const (
	// WorkerDatabaseURLSetting is the connection string of the worker role, the only role that may claim outbox
	// messages (ST-SUP-RP01-LC19), and WorkerDatabaseURLFileSetting a file that holds it.
	WorkerDatabaseURLSetting     = "STEGO_WORKER_DATABASE_URL"
	WorkerDatabaseURLFileSetting = "STEGO_WORKER_DATABASE_URL_FILE"
	// RelayDatabaseURLSetting is the connection string of the relay login role, which raises the revocation floor
	// (ST-SUP-RP06-LC13), and RelayDatabaseURLFileSetting a file that holds it.
	RelayDatabaseURLSetting     = "STEGO_RELAY_DATABASE_URL"
	RelayDatabaseURLFileSetting = "STEGO_RELAY_DATABASE_URL_FILE"
)

// The names of the components that Start runs, as the readiness and the errors name them.
const (
	OutboxWorkerName = "outbox-worker"
	RelayWatchName   = "authz-relay-watch"
	ControllerName   = "controller-lease"
	AnchorJobName    = "audit-anchor"
	MaintainJobName  = "audit-maintain"
	ResealJobName    = "reseal"
	SealJobName      = "seal"
)

// ResealInterval is the interval of the re-seal job of the secret fields in the controller mode (ST-SUP-RP14-LC12).
const ResealInterval = 10 * time.Minute

// ControllerLease is the name of the controller lease, under which the jobs of the controller mode run: one replica
// holds it at a time (ST-SUP-RP09-LC13). The migration job makes its row (ST-SUP-RP14-LC03).
const ControllerLease = "stego.controller"

// MaintainInterval is the interval of the audit maintenance loop, which the declaration does not state: each run
// makes the partitions that are missing and drops at most one partition per call (ST-SUP-RP05-LC10).
const MaintainInterval = time.Hour

// DefaultDrainBound is the drain bound of the worker mode when the start configuration gives none: the time that
// the claimed messages have to finish at shutdown (ST-SUP-RP09-LC06).
const DefaultDrainBound = 20 * time.Second

// WorkerSettings are the connection settings of the worker mode, which ReadWorkerSettings reads.
type WorkerSettings struct {
	// WorkerDatabaseURL is the connection string of the worker role.
	WorkerDatabaseURL string
	// RelayDatabaseURL is the connection string of the relay login role, or empty for a service with no relay.
	RelayDatabaseURL string
	// AuditSink are the settings of the system that the audit-sink port binds: an HTTPS endpoint, or the default
	// table in the service database (ST-SUP-RP05-LC15).
	AuditSink auditsink.Settings
}

// ReadWorkerSettings reads the connection settings of the worker mode from the process environment by their fixed
// names (ST-SUP-RP14-LC04). relay tells whether the service has the authorization relay, whose connection string is
// then required too. A missing or malformed setting is refused with an error that names it and not its value.
func ReadWorkerSettings(relay bool) (WorkerSettings, error) {
	var s WorkerSettings
	var err error
	if s.WorkerDatabaseURL, err = connectionString(WorkerDatabaseURLSetting, WorkerDatabaseURLFileSetting); err != nil {
		return WorkerSettings{}, err
	}
	if relay {
		if s.RelayDatabaseURL, err = connectionString(RelayDatabaseURLSetting, RelayDatabaseURLFileSetting); err != nil {
			return WorkerSettings{}, err
		}
	}
	if s.AuditSink, err = auditsink.ReadSettings(); err != nil {
		return WorkerSettings{}, err
	}
	return s, nil
}

// WorkerParts are what varies with the declaration for the components of the worker and controller modes, which
// the generated code gives in Parts: the constructors of the service's component packages and the declared values.
type WorkerParts struct {
	// NewWorker gives the outbox worker on queue with config, with the destinations of the service's components
	// registered on it, such as the audit sink on sink, the system of the worker settings (ST-SUP-RC18-OC30).
	NewWorker func(queue *outbox.Queue, config outbox.WorkerConfig, sink audit.Sink) (*outbox.Worker, error)
	// NewRelay builds the authorization relay on the relay role's pool and registers it on the worker as the
	// destination authz (ST-SUP-RP06-LC06). Nil: the service has no relay.
	NewRelay func(ctx context.Context, db *sql.DB, relationships authz.Relationships, worker *outbox.Worker,
		meter metric.MeterProvider, logger otellog.LoggerProvider) (*authz.Relay, error)
	// WatchInterval is the interval of the relay's late-revocation watch (ST-SUP-RP06-LC14).
	WatchInterval time.Duration
	// AuditJobs gives the audit anchor job and the maintenance loop (ST-SUP-RC28-OC08). Nil: the service has no
	// audit jobs.
	AuditJobs func(lease audit.LeaseOf, tenants audit.TenantsOf, db *sql.DB, outbox audit.Enqueuer) (*audit.Anchorer, *audit.Maintainer, error)
	// AnchorPeriod is the declared anchor period of the audit-log component, the interval of the anchor job.
	AnchorPeriod time.Duration
	// RetentionMonths is the declared retention of the audit-log component, whose value the source of audit_maintain
	// holds, which the schema manifest check compares (ST-SUP-RP14-LC06).
	RetentionMonths int
	// TenantSchema is the schema of the tenant list stego_tenants, whose tenants the audit jobs visit
	// (ST-SUP-RP04-LC07).
	TenantSchema string
	// Tickers gives the ticker of each declared worker with an every trigger on the command runtime of the workers
	// (ST-SUP-RC35-OC04); the worker mode runs each and stops it at shutdown. Nil: the service has no such worker.
	Tickers func(env TickerEnv) ([]*command.Ticker, error)
	// Consumers registers on the outbox worker the one destination handler of each declared worker with a consumes
	// trigger, which calls RunConsumer on the command runtime of the workers (ST-SUP-RC35-OC06). Nil: the service
	// has no such worker.
	Consumers func(rt *command.Runtime, worker *outbox.Worker, observe func(name string) command.WorkerObserver) error
	// Inboxes gives the Kafka inbox of each declared subscription, in the order of the declaration, which writes each
	// record that it takes into the outbox for the worker that consumes the subscription (ST-SUP-RC38-OC01). The
	// worker mode runs each, and the purge timer of the outbox worker purges the refused records. Nil: the service
	// declares no subscription.
	Inboxes func(env InboxEnv) ([]*kafkainbox.Inbox, error)
	// ConsumerTables are the tables in public of the entities that the workers with a consumes trigger write, which
	// the migration job grants the worker role SELECT, INSERT and UPDATE on (decision on ST-SUP-Q141, orchestrator
	// under the owner's delegation, 2026-11-15).
	ConsumerTables []string
	// ConsumerReadTables are the tables in public of the entities that the workers with a consumes trigger only read,
	// each with the columns of its view, which the migration job grants the worker role SELECT on, and nothing more
	// (ST-SUP-RC35-OC19, decision ST-SUP-Q186 (a), security review of PR #451, L4).
	ConsumerReadTables map[string][]string
}

// InboxEnv is what the Kafka inbox of each subscription gets from the worker mode.
type InboxEnv struct {
	// Queue is the outbox queue on the pool of the worker role, DB, on which the inbox writes.
	Queue *outbox.Queue
	DB    *sql.DB
	// Tenants looks a tenant ID up in the tenant list, on the pool of the runtime role (ST-SUP-RP01-LC35), and
	// TenantSchema is the schema of that list, which the write of each record reads again in its transaction.
	Tenants      kafkainbox.Lookup
	TenantSchema string
	// Retention is the done retention of the outbox worker, which the topic's retention must not exceed.
	Retention time.Duration
	// Logs and Meter are the providers of the service's telemetry.
	Logs  otellog.LoggerProvider
	Meter metric.MeterProvider
}

// WorkerLeasePrefix is the prefix of the name of the lease of a single worker: stego.worker.<worker name>
// (ST-SUP-RC35-OC05). The migration job makes the row of each (Migration.WorkerLeases).
const WorkerLeasePrefix = "stego.worker."

// TickerEnv is what the tickers of the declared workers get from the worker mode.
type TickerEnv struct {
	// Runtime is the command runtime of the workers.
	Runtime *command.Runtime
	// Tenants reads the tenant list, which holds no retired tenant (ST-SUP-RP04-LC07, ST-SUP-RC27-OC12).
	Tenants func(ctx context.Context) ([]tenant.Tenant, error)
	// Lease gives the client of the lease of the single worker name, stego.worker.<name>, with the lease TTL and a
	// renew interval of two thirds of it (ST-SUP-RC35-OC05).
	Lease func(name string, ttl time.Duration) (*lease.Client, error)
	// Observe gives the built-in worker observations of the worker name (ST-SUP-RP12-LC21).
	Observe func(name string) command.WorkerObserver
	// Clock is the clock of the ticks, which StartConfig gives.
	Clock command.Clock
}

// StartConfig is how one process runs: the modes it runs, the HTTP server of the api mode, and the bounds of the
// shutdown.
type StartConfig struct {
	// Selection is the modes the process runs, as modes.Parse gives them.
	Selection modes.Selection
	// Workers are the connection settings of the worker mode; Start reads them only when the selection runs it.
	Workers WorkerSettings
	// Server is the HTTP server of the api mode, which the shutdown drains. Nil: the api mode has nothing to drain.
	Server *http.Server
	// PropagationDelay is the wait of the api mode's shutdown after readiness fails.
	PropagationDelay time.Duration
	// DrainBound is the time the claimed outbox messages have to finish at shutdown; zero is DefaultDrainBound.
	DrainBound time.Duration
	// Holder names this process as a holder of the controller lease; empty is the host name with a random suffix.
	Holder string
	// LeaseSettings are the settings of the controller lease; the zero value is the lease runtime's defaults.
	LeaseSettings lease.Settings
	// WorkerConfig are the limits of the outbox worker; the zero value is outbox.DefaultWorkerConfig.
	WorkerConfig *outbox.WorkerConfig
	// Clock is the clock of the tickers of the declared workers and of their leases; nil is the system clock. A test
	// of the service gives its own.
	Clock command.Clock
}

// component is one started component and its state.
type component struct {
	name    string
	mode    modes.Mode
	running atomic.Bool
	failed  atomic.Bool
}

// Workers are the components that Start started for the selected modes: the outbox worker with its destinations
// and the relay's watch in the worker mode, and the controller lease with the periodic audit jobs in the controller
// mode. Shutdown stops them once.
type Workers struct {
	selection  modes.Selection
	readiness  *modes.Readiness
	sequencer  *modes.Sequencer
	components []*component
	wg         sync.WaitGroup
	cancel     context.CancelFunc

	workerDB, relayDB *sql.DB
	worker            *outbox.Worker
	// inboxes are the Kafka inboxes of the declared subscriptions, in the order of the declaration, by their component
	// names: a stopped inbox, such as one that the retention of its topic stopped, and an inbox whose sessions fail in
	// a row make the worker mode not ready (ST-SUP-RP14-LC08; security review of PR #418, M2).
	inboxes    []namedInbox
	relay      *authz.Relay
	periodic   *lease.Periodic
	tickers    []*command.Ticker
	anchorer   *audit.Anchorer
	maintainer *audit.Maintainer
	reconcile  *command.WorkerReconcile

	// controller ends the controller lease loop, which releases the lease; controllerDone is closed when it returned.
	controller     context.CancelFunc
	controllerDone chan struct{}
	held           atomic.Pointer[lease.Held]

	once        sync.Once
	shutdownErr error
}

// Start starts the components of the selected modes (ST-SUP-RP14-LC04), each in its own goroutine, and returns once
// they run. In the worker mode it opens the pool of the worker role through tenant.Open, builds the outbox worker on
// it with the service's destinations, and the relay on the relay role's pool with its watch; in the controller mode
// it holds the controller lease on the runtime role's pool and runs the audit anchor job at the anchor period and
// the maintenance loop at MaintainInterval under it, so two replicas never run the same job. The api mode starts no
// component: the generated code serves its HTTP server on Readiness().Listener. A selection that runs the worker
// mode without the worker role's connection string is refused, naming the setting. A failed step stops what Start
// started and closes what it opened.
func (svc *Service) Start(ctx context.Context, c StartConfig) (_ *Workers, err error) {
	if len(c.Selection) == 0 {
		return nil, errors.New("service: Start needs the selected modes")
	}
	for _, m := range c.Selection {
		if m != modes.API && m != modes.Worker && m != modes.Controller && m != modes.All {
			return nil, fmt.Errorf("service: unknown mode %q", m)
		}
	}
	w := &Workers{selection: c.Selection}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	w.cancel = cancel
	defer func() {
		if err != nil {
			cancel()
			w.wg.Wait()
			w.closePools()
		}
	}()
	registry := modes.NewRegistry()
	var workerSet, controllerSet []modes.Component
	if c.Selection.Has(modes.Worker) {
		if workerSet, err = svc.startWorker(ctx, c, w); err != nil {
			return nil, err
		}
	}
	if c.Selection.Has(modes.Controller) {
		if controllerSet, err = svc.startController(c, w); err != nil {
			return nil, err
		}
	}
	if err := registry.Register(modes.Worker, workerSet...); err != nil {
		return nil, err
	}
	if err := registry.Register(modes.Controller, controllerSet...); err != nil {
		return nil, err
	}
	w.readiness = modes.NewReadiness(svc.pool)
	// A database that lacks a table or a column of the manifest makes each mode not ready from the start, so a
	// rollout of a release before a contract step stops at its first replica (ST-SUP-RP14-LC06).
	w.readiness.Block(svc.blocked)
	w.readiness.WatchQueue(controllerQueue{w: w})
	drain := c.DrainBound
	if drain <= 0 {
		drain = DefaultDrainBound
	}
	w.sequencer = &modes.Sequencer{Readiness: w.readiness,
		API: &modes.APIShutdown{Server: c.Server, PropagationDelay: c.PropagationDelay}}
	if w.worker != nil {
		// The tickers start no run, and the running ones and the claimed messages finish within the drain bound;
		// a run that is still running then rolls back (ST-SUP-RC35-OC10).
		worker, tickers := w.worker, w.tickers
		w.sequencer.Worker = &modes.WorkerShutdown{DrainBound: drain,
			StopClaiming: func() {
				worker.StopClaiming()
				for _, t := range tickers {
					t.Stop()
				}
			},
			Finish: func(ctx context.Context) error {
				var wg sync.WaitGroup
				errs := make([]error, len(tickers)+1)
				for i, t := range tickers {
					wg.Add(1)
					go func() {
						defer wg.Done()
						errs[i] = t.Finish(ctx)
					}()
				}
				errs[len(tickers)] = worker.Finish(ctx)
				wg.Wait()
				return errors.Join(errs...)
			}}
	}
	if w.controllerDone != nil {
		w.sequencer.Controller = &modes.ControllerShutdown{Release: w.releaseController}
	}
	for _, comp := range registry.Components(c.Selection) {
		state := w.track(comp.Name)
		w.wg.Add(1)
		go func(comp modes.Component) {
			defer w.wg.Done()
			defer state.running.Store(false)
			if err := comp.Run(runCtx); err != nil && runCtx.Err() == nil {
				state.failed.Store(true)
			}
		}(comp)
	}
	return w, nil
}

// track gives the state of the component name and marks it running.
func (w *Workers) track(name string) *component {
	for _, c := range w.components {
		if c.name == name {
			c.running.Store(true)
			return c
		}
	}
	c := &component{name: name}
	c.running.Store(true)
	w.components = append(w.components, c)
	return c
}

// startWorker opens the worker role's pool and builds the outbox worker, the relay and its watch.
func (svc *Service) startWorker(ctx context.Context, c StartConfig, w *Workers) ([]modes.Component, error) {
	p := svc.parts.Workers
	if p.NewWorker == nil {
		return nil, errors.New("service: the worker mode needs the outbox worker of the parts")
	}
	if c.Workers.WorkerDatabaseURL == "" {
		return nil, settingError(WorkerDatabaseURLSetting+" or "+WorkerDatabaseURLFileSetting, "must be set for the worker mode")
	}
	if p.NewRelay != nil && c.Workers.RelayDatabaseURL == "" {
		return nil, settingError(RelayDatabaseURLSetting+" or "+RelayDatabaseURLFileSetting, "must be set for the worker mode of a service with the relay")
	}
	config := outbox.DefaultWorkerConfig()
	if c.WorkerConfig != nil {
		config = *c.WorkerConfig
	}
	config.Meter, config.Logger = svc.telemetry.MeterProvider(), svc.telemetry.LoggerProvider()
	// A message of the Kafka inbox that is done frees the later messages of its key that are parked as poison, with the
	// audit row of each resolution, in its transaction (ST-SUP-RP01-LC39). Only a service with the inbox has such
	// messages, and its worker role holds the audit grants of its consuming workers (ST-SUP-Q141).
	if config.Auditor == nil && p.Inboxes != nil {
		config.Auditor = &resolveAuditor{writer: svc.writer, settings: svc.parts.AuditSettings, service: svc.parts.Service,
			version: svc.version, declaration: svc.parts.DeclarationVersion}
	}
	if svc.tenants != nil {
		// A destination registered with ActiveTenants, such as the after slots, gets no message of a tenant that is
		// not active (ST-SUP-RC27-OC12).
		config.Tenants = svc.tenants.gate
	}
	cfg, err := svc.parts.TenantConfig(c.Workers.WorkerDatabaseURL)
	if err != nil {
		return nil, err
	}
	// The worker role is the one role that runs the outbox relay functions (ST-SUP-RP01-LC19).
	cfg.Claims = true
	if w.workerDB, err = tenant.Open(ctx, cfg); err != nil {
		return nil, err
	}
	// Each step in flight holds one connection, and the claims and the purge timer one more each.
	if limit := w.workerDB.Stats().MaxOpenConnections; limit > 0 && limit < config.Concurrency+2 {
		w.workerDB.SetMaxOpenConns(config.Concurrency + 2)
	}
	queue, err := svc.parts.NewQueue(w.workerDB)
	if err != nil {
		return nil, err
	}
	sink, err := auditsink.New(c.Workers.AuditSink, w.workerDB)
	if err != nil {
		return nil, err
	}
	if w.worker, err = p.NewWorker(queue, config, sink); err != nil {
		return nil, err
	}
	if p.Consumers != nil {
		if svc.workers == nil {
			return nil, errors.New("service: the consuming workers need the command runtime of the workers")
		}
		if err := p.Consumers(svc.workers, w.worker, func(name string) command.WorkerObserver { return svc.probes.Worker(name) }); err != nil {
			return nil, err
		}
	}
	var inboxes []*kafkainbox.Inbox
	if p.Inboxes != nil {
		if p.TenantSchema == "" {
			return nil, errors.New("service: the Kafka inbox needs the schema of the tenant list")
		}
		lookup, err := kafkainbox.TenantList(svc.pool, p.TenantSchema)
		if err != nil {
			return nil, err
		}
		if inboxes, err = p.Inboxes(InboxEnv{Queue: queue, DB: w.workerDB, Retention: w.worker.Retention(),
			Logs: svc.telemetry.LoggerProvider(), Meter: svc.telemetry.MeterProvider(), Tenants: lookup, TenantSchema: p.TenantSchema}); err != nil {
			return nil, inboxSetting(err)
		}
		// Each subscription reads in a consumer group of its own (decision on ST-SUP-Q204).
		if err := kafkainbox.CheckGroups(inboxes); err != nil {
			return nil, inboxSetting(err)
		}
		for i, inbox := range inboxes {
			// The refused records of every subscription are in one table, which the timer of the outbox purges once
			// (ST-SUP-RP01-LC37).
			if i == 0 {
				if err := w.worker.AddPurge(inbox.Purge); err != nil {
					return nil, err
				}
			}
			// The worker's own identity resolves the parked messages that a done message of the inbox frees, never a
			// column of the row (ST-SUP-RP01-LC39; security review of PR #457, L2).
			if err := w.worker.SetResolver(inbox.Destination(), inbox.Principal()); err != nil {
				return nil, err
			}
		}
	}
	// Each registered destination has a claim slot of its own, so a service with more destinations than Concurrency
	// runs one slot for each, and each slot holds one connection (ST-SUP-RP01-LC34). Each Kafka inbox holds one more
	// while it writes a poll.
	if slots := max(config.Concurrency, len(w.worker.Registrations())) + len(inboxes); w.workerDB.Stats().MaxOpenConnections < slots+2 &&
		w.workerDB.Stats().MaxOpenConnections > 0 {
		w.workerDB.SetMaxOpenConns(slots + 2)
	}
	set := []modes.Component{{Name: OutboxWorkerName, Run: w.worker.Run}}
	for _, inbox := range inboxes {
		// Each inbox runs in the worker mode; a start that it refuses for good, such as a topic retention longer than
		// the done retention, stops it, and the readiness of the worker mode names it (ST-SUP-RC38-OC01).
		w.inboxes = append(w.inboxes, namedInbox{name: inbox.Name(), state: inbox})
		set = append(set, modes.Component{Name: inbox.Name(), Run: inbox.Run})
	}
	if p.Tickers != nil {
		if svc.workers == nil || p.TenantSchema == "" {
			return nil, errors.New("service: the workers need the command runtime of the workers and the schema of the tenant list")
		}
		holder := c.Holder
		if holder == "" {
			holder = defaultHolder()
		}
		schema, pool, clock := p.TenantSchema, svc.pool, c.Clock
		env := TickerEnv{Runtime: svc.workers, Clock: clock,
			Tenants: func(ctx context.Context) ([]tenant.Tenant, error) { return tenant.List(ctx, pool, schema) },
			Lease: func(name string, ttl time.Duration) (*lease.Client, error) {
				return lease.New(pool, WorkerLeasePrefix+name, holder, lease.Settings{TTL: ttl, Renew: ttl * 2 / 3, Clock: clock})
			},
			Observe: func(name string) command.WorkerObserver { return svc.probes.Worker(name) },
		}
		if w.tickers, err = p.Tickers(env); err != nil {
			return nil, err
		}
		for _, t := range w.tickers {
			set = append(set, modes.Component{Name: "worker-" + t.Name(), Run: t.Run})
		}
	}
	if p.NewRelay != nil {
		if p.WatchInterval <= 0 {
			return nil, errors.New("service: the relay's watch needs a positive interval")
		}
		relayCfg, err := svc.parts.TenantConfig(c.Workers.RelayDatabaseURL)
		if err != nil {
			return nil, err
		}
		if w.relayDB, err = tenant.Open(ctx, relayCfg); err != nil {
			return nil, err
		}
		if w.relay, err = p.NewRelay(ctx, w.relayDB, svc.provider, w.worker, svc.telemetry.MeterProvider(), svc.telemetry.LoggerProvider()); err != nil {
			return nil, err
		}
		worker, relay, interval := w.worker, w.relay, p.WatchInterval
		set = append(set, modes.Component{Name: RelayWatchName, Run: func(ctx context.Context) error {
			return relay.Watch(ctx, interval, func(ctx context.Context) (time.Duration, error) {
				return worker.OldestAge(ctx, authz.DestinationName)
			})
		}})
	}
	return set, nil
}

// WorkerTuplesInterval is the interval of the job worker-tuples of the controller mode: it runs at start and then
// once an hour (ST-SUP-RC35-OC13).
const WorkerTuplesInterval = time.Hour

// workerReconcile gives the reconcile of the worker tuples when the service declares its tenant provisioning, which
// binds authorization, and its schema declares the definition tenant, and else nil (ST-SUP-RC35-OC13). It runs with
// no declared worker too, so the tuples of the last removed worker are deleted.
func (svc *Service) workerReconcile() (*command.WorkerReconcile, error) {
	if svc.tenants == nil || !svc.parts.declaresTenant() {
		return nil, nil
	}
	if svc.provider == nil {
		return nil, errors.New("service: the reconcile of the worker tuples needs the SpiceDB provider")
	}
	return command.NewWorkerReconcile(svc.tenants.rt, svc.parts.Workers.TenantSchema, svc.parts.WorkerNames, svc.provider, svc.parts.Release)
}

// resealJob gives the re-seal job of the readable secret fields of the service, or nil for a service with none
// (ST-SUP-RP14-LC12).
func (svc *Service) resealJob() (*command.Reseal, error) {
	p := svc.parts
	if len(p.Reseal) == 0 {
		return nil, nil
	}
	if p.Workers.TenantSchema == "" {
		return nil, errors.New("service: the re-seal job of the secret fields needs the schema of the tenant list")
	}
	return command.NewReseal(svc.pool, p.Workers.TenantSchema, svc.ring, p.Reseal)
}

// sealJob gives the seal job of the open seal steps of the service, or nil for a service with none
// (ST-SUP-RP10-LC50, ST-SUP-RP14-LC12): it holds the backfill lease of each change for the holder of the controller.
func (svc *Service) sealJob(holder string, settings lease.Settings) (*command.SealJob, error) {
	p := svc.parts
	if len(p.Seal) == 0 {
		return nil, nil
	}
	if p.Workers.TenantSchema == "" {
		return nil, errors.New("service: the seal job of the secret fields needs the schema of the tenant list")
	}
	return command.NewSealJob(svc.pool, p.Workers.TenantSchema, svc.ring, p.Seal, holder, settings)
}

// startController builds the audit jobs, the reconcile of the worker tuples and the controller lease loop that runs
// them.
func (svc *Service) startController(c StartConfig, w *Workers) ([]modes.Component, error) {
	p := svc.parts.Workers
	reconcile, err := svc.workerReconcile()
	if err != nil {
		return nil, err
	}
	reseal, err := svc.resealJob()
	if err != nil {
		return nil, err
	}
	holder := c.Holder
	if holder == "" {
		holder = defaultHolder()
	}
	seal, err := svc.sealJob(holder, c.LeaseSettings)
	if err != nil {
		return nil, err
	}
	if p.AuditJobs == nil && reconcile == nil && reseal == nil && seal == nil {
		return nil, nil
	}
	if p.AuditJobs != nil && (p.AnchorPeriod <= 0 || p.TenantSchema == "") {
		return nil, errors.New("service: the audit jobs need the anchor period and the schema of the tenant list")
	}
	client, err := lease.New(svc.pool, ControllerLease, holder, c.LeaseSettings)
	if err != nil {
		return nil, err
	}
	leaseOf := func() *lease.Held { return w.held.Load() }
	var jobs []lease.Job
	if p.AuditJobs != nil {
		enqueuer, err := svc.parts.AuditQueue(svc.queue)
		if err != nil {
			return nil, err
		}
		schema, pool := p.TenantSchema, svc.pool
		tenants := func(ctx context.Context) ([]tenant.Tenant, error) { return tenant.List(ctx, pool, schema) }
		if w.anchorer, w.maintainer, err = p.AuditJobs(leaseOf, tenants, svc.pool, enqueuer); err != nil {
			return nil, err
		}
		anchorer, maintainer := w.anchorer, w.maintainer
		jobs = append(jobs,
			lease.Job{Name: AnchorJobName, Interval: p.AnchorPeriod, Run: func(ctx context.Context, _ *lease.Held) error {
				return anchorer.Tick(ctx, time.Now())
			}},
			lease.Job{Name: MaintainJobName, Interval: MaintainInterval, Run: func(ctx context.Context, _ *lease.Held) error {
				_, err := maintainer.Run(ctx)
				return err
			}})
	}
	if reconcile != nil {
		w.reconcile = reconcile
		jobs = append(jobs, lease.Job{Name: command.WorkerTuplesJob, Interval: WorkerTuplesInterval, Degraded: reconcile.Degraded,
			Run: func(ctx context.Context, held *lease.Held) error {
				// Each tenant's write is fenced under the controller lease (security review of PR #379, L3).
				var fencer command.Fencer
				if held != nil {
					fencer = held
				}
				_, err := reconcile.Run(ctx, fencer)
				return err
			}})
	}
	if reseal != nil {
		// The re-seal job moves each sealed value to the first key of the ring, so an earlier key can go after a
		// rotation (ST-SUP-RP10-LC46).
		jobs = append(jobs, lease.Job{Name: ResealJobName, Interval: ResealInterval, Run: func(ctx context.Context, _ *lease.Held) error {
			_, err := reseal.Run(ctx)
			return err
		}})
	}
	if seal != nil {
		// The seal job seals and checks each row of an open seal step in passes, under the backfill lease of its
		// change (ST-SUP-RP10-LC50).
		jobs = append(jobs, lease.Job{Name: SealJobName, Interval: ResealInterval, Run: func(ctx context.Context, _ *lease.Held) error {
			_, err := seal.Run(ctx)
			return err
		}})
	}
	if w.periodic, err = lease.NewPeriodic(lease.PeriodicConfig{Lease: leaseOf, Retry: client.RetryEvery(), Jobs: jobs}); err != nil {
		return nil, err
	}
	controllerCtx, stop := context.WithCancel(context.Background())
	w.controller, w.controllerDone = stop, make(chan struct{})
	hold := func(ctx context.Context) error {
		// The lease loop ends with the controller's own context, which the controller shutdown ends, or with ctx.
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-controllerCtx.Done():
				cancel()
			case <-ctx.Done():
			}
		}()
		defer close(w.controllerDone)
		for ctx.Err() == nil {
			_ = client.Hold(ctx, func(ctx context.Context, h *lease.Held) error {
				w.held.Store(h)
				defer w.held.CompareAndSwap(h, nil)
				select {
				case <-ctx.Done():
				case <-h.Done():
				}
				return nil
			})
			select {
			case <-ctx.Done():
			case <-time.After(client.RetryEvery()):
			}
		}
		return nil
	}
	return []modes.Component{{Name: ControllerName, Run: hold}, {Name: lease.PeriodicName, Run: w.periodic.Run}}, nil
}

// releaseController ends the controller lease loop, which releases the lease, and waits for it.
func (w *Workers) releaseController(ctx context.Context) error {
	w.controller()
	select {
	case <-w.controllerDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// defaultHolder is the host name with a random suffix, so two processes on one host are two holders.
func defaultHolder() string {
	host, _ := os.Hostname()
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return host + "-" + hex.EncodeToString(suffix)
}

// controllerQueue is the readiness of the controller mode's components: ready while the controller lease loop and
// the periodic jobs run and every job, the anchor job and the maintenance loop are healthy, and always ready when
// the service has no controller job.
type controllerQueue struct{ w *Workers }

func (q controllerQueue) QueueReady() bool { return q.w.controllerReason() == "" }

// controllerReason is why the controller mode's components are not ready, or empty.
func (w *Workers) controllerReason() string {
	if w.periodic == nil {
		return ""
	}
	if reason := w.stopped(ControllerName, lease.PeriodicName); reason != "" {
		return reason
	}
	var unhealthy []string
	// The reason names each job whose last run failed, such as worker-tuples (ST-SUP-RC35-OC13).
	if ok, reason := w.periodic.Health(); !ok {
		unhealthy = append(unhealthy, reason)
	}
	if w.anchorer != nil {
		if ok, _ := w.anchorer.Health(); !ok {
			unhealthy = append(unhealthy, "the "+AnchorJobName+" job has a boundary that is due and not anchored")
		}
	}
	if w.maintainer != nil {
		if ok, _ := w.maintainer.Health(); !ok {
			unhealthy = append(unhealthy, "the "+MaintainJobName+" job found no partition of the next month")
		}
	}
	return strings.Join(unhealthy, "; ")
}

// stopped names each of the components names that does not run.
func (w *Workers) stopped(names ...string) string {
	var out []string
	for _, c := range w.components {
		for _, name := range names {
			if c.name == name && !c.running.Load() {
				text := "the component " + c.name + " stopped"
				// A stopped inbox names the stage that stopped it, as the docs and the event kafka.consumer.start_refused
				// give it (core builder 16).
				for _, inbox := range w.inboxes {
					if c.name == inbox.name {
						if stage := inbox.state.Failed(); stage != "" {
							text += " at the stage " + stage
						}
					}
				}
				out = append(out, text)
			}
		}
	}
	return strings.Join(out, "; ")
}

// Readiness is the readiness of the modes, whose Listener the HTTP server of the api mode serves on.
func (w *Workers) Readiness() *modes.Readiness { return w.readiness }

// Ready answers the readiness of each selected mode (ST-SUP-RP09-LC05) and names each mode that is not ready and
// why: the worker mode is ready while the worker role's pool reaches the database, the outbox worker and the
// relay's watch run, and neither the Kafka inbox nor a destination of the outbox fails in a row; the controller mode while the controller lease loop and the periodic jobs run and every job is
// healthy, whether or not this replica holds the lease. The reason names components and jobs, never a tenant.
func (w *Workers) Ready(ctx context.Context) error {
	var errs []error
	for _, m := range w.selection.Modes() {
		if err := w.readiness.Ready(ctx, m); err != nil {
			if m == modes.Controller && w.periodic != nil && w.readiness != nil {
				if reason := w.controllerReason(); reason != "" {
					err = fmt.Errorf("%s: %w: %s", m, modes.ErrNotReady, reason)
				}
			}
			errs = append(errs, err)
			continue
		}
		if m == modes.Worker && w.workerDB != nil {
			if err := w.workerDB.PingContext(ctx); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w: the worker role's pool does not reach the database", m, modes.ErrNotReady))
			} else if reason := w.stopped(append([]string{OutboxWorkerName, RelayWatchName}, w.inboxNames()...)...); reason != "" {
				errs = append(errs, fmt.Errorf("%s: %w: %s", m, modes.ErrNotReady, reason))
			} else if reason := w.inboxFailing(); reason != "" {
				errs = append(errs, fmt.Errorf("%s: %w: %s", m, modes.ErrNotReady, reason))
			} else if reason := w.destinationFailing(); reason != "" {
				// A destination that fails each delivery in a row, such as the Kafka destination in a broker outage,
				// makes the worker mode not ready, as a Kafka inbox that fails does (walkthrough 9).
				errs = append(errs, fmt.Errorf("%s: %w: %s", m, modes.ErrNotReady, reason))
			}
		}
	}
	return errors.Join(errs...)
}

// Handler serves the probes of the process: /readyz answers 200 when every selected mode is ready and 503 with the
// reasons of Ready when not; /livez answers 200 while the process serves, and no worker state fails it.
func (w *Workers) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err := w.Ready(req.Context()); err != nil {
			rw.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintln(rw, err)
			return
		}
		_, _ = fmt.Fprintln(rw, "ready")
		// A degraded periodic job, such as a fenced reconcile of the worker tuples, is named and stays ready.
		if w.periodic != nil {
			if _, degraded := w.periodic.Health(); degraded != "" {
				_, _ = fmt.Fprintln(rw, degraded)
			}
		}
	})
	mux.HandleFunc("GET /livez", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(rw, "live")
	})
	return mux
}

// Worker is the outbox worker of the worker mode, or nil when the selection does not run it.
func (w *Workers) Worker() *outbox.Worker { return w.worker }

// Relay is the authorization relay of the worker mode, or nil.
func (w *Workers) Relay() *authz.Relay { return w.relay }

// Periodic is the periodic job component of the controller mode, or nil.
func (w *Workers) Periodic() *lease.Periodic { return w.periodic }

// Reconcile is the reconcile of the worker tuples of the controller mode, or nil (ST-SUP-RC35-OC13).
func (w *Workers) Reconcile() *command.WorkerReconcile { return w.reconcile }

// Anchorer and Maintainer are the audit jobs of the controller mode, or nil.
func (w *Workers) Anchorer() *audit.Anchorer     { return w.anchorer }
func (w *Workers) Maintainer() *audit.Maintainer { return w.maintainer }

// Holds tells whether this process holds the controller lease now.
func (w *Workers) Holds() bool {
	h := w.held.Load()
	return h != nil && h.Err() == nil
}

// Shutdown stops the started components once (ST-SUP-RP14-LC04): it makes every mode not ready, then runs the
// shutdown of each selected mode in its own order (ST-SUP-RP09-LC06): the api mode waits the propagation delay and
// drains the HTTP server; the worker mode stops claiming and lets the claimed messages finish within the drain
// bound; the controller mode releases the controller lease after its running jobs end. Then it ends every
// component, waits for each to return and closes the worker and relay pools. A later call returns the result of the
// first and stops nothing again.
func (w *Workers) Shutdown(ctx context.Context) error {
	w.once.Do(func() {
		err := w.sequencer.Shutdown(ctx, w.selection)
		w.cancel()
		w.wg.Wait()
		w.closePools()
		w.shutdownErr = err
	})
	return w.shutdownErr
}

func (w *Workers) closePools() {
	for _, db := range []*sql.DB{w.relayDB, w.workerDB} {
		if db != nil {
			_ = db.Close()
		}
	}
}

// KafkaConsumerComponent is the registry component whose settings the Kafka inbox reads (ST-SUP-RC38).
const KafkaConsumerComponent = "kafka-consumer"

// inboxSetting gives a refused setting of the Kafka inbox as a *SettingError of the component kafka-consumer, so the
// start names its stage service.start.setting.kafka-consumer.<variable> and no value (ST-SUP-RP14-LC05).
func inboxSetting(err error) error {
	var setting *kafkainbox.SettingError
	if errors.As(err, &setting) {
		return &SettingError{Component: KafkaConsumerComponent, Variable: setting.Variable, names: setting.Variable, problem: setting.Problem}
	}
	return err
}

// destinationFailing names the first destination of the outbox worker that fails each delivery in a row, with its
// fixed failure class, or gives "".
func (w *Workers) destinationFailing() string {
	if w.worker == nil {
		return ""
	}
	return w.worker.Failing()
}

// namedInbox is one Kafka inbox by its component name, inbox-<subscription>, and what it tells of its state.
type namedInbox struct {
	name  string
	state interface {
		// Failing gives the failure class while the sessions of the inbox fail in a row, or "".
		Failing() string
		// Failed gives the stage of the failure that stopped the inbox for good, such as kafka-consumer.retention.
		Failed() string
	}
}

// inboxNames gives the component name of each Kafka inbox.
func (w *Workers) inboxNames() []string {
	names := make([]string, len(w.inboxes))
	for i, inbox := range w.inboxes {
		names[i] = inbox.name
	}
	return names
}

// inboxFailing names each Kafka inbox and its fixed failure class while its sessions fail in a row, or gives "".
func (w *Workers) inboxFailing() string {
	var out []string
	for _, inbox := range w.inboxes {
		if class := inbox.state.Failing(); class != "" {
			out = append(out, "the component "+inbox.name+" fails: "+class)
		}
	}
	return strings.Join(out, "; ")
}
