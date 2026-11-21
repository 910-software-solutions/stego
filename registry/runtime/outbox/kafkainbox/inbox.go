package kafkainbox

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"

	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/outbox/kafkaoutbox"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// instrumentation names the logger and the meter of the inbox.
const instrumentation = "github.com/jsell-rh/stego/registry/runtime/outbox/kafkainbox"

// The bounds of the inbox.
const (
	// MaxEvents is the most event kinds of one subscription.
	MaxEvents = 32
	// MaxPoll is the most records of one poll. The inbox commits their offsets after it wrote them.
	MaxPoll = 100
	// operationTimeout bounds the write of one record and each read of the tenant list.
	operationTimeout = 5 * time.Second
	// commitTimeout bounds one commit of offsets.
	commitTimeout = 10 * time.Second
	// retentionTimeout bounds the read of the topic's retention.
	retentionTimeout = 10 * time.Second
	// backoffMin and backoffMax bound the wait before the inbox opens its client again after a failure.
	backoffMin = 500 * time.Millisecond
	backoffMax = 30 * time.Second
	// DefaultFailingAfter is the number of failed sessions in a row after which the inbox reports that it fails.
	DefaultFailingAfter = 5
	// pollWait bounds one poll, so a topic with no new record still shows that the inbox reads it.
	pollWait = 5 * time.Second
	// retentionRecheck is how often a session reads the retention and the cleanup policy of the topic again, so a
	// topic that a person changes to a longer retention or to compaction stops the inbox without a new session, and
	// a broker that no longer answers fails the session (security review of PR #418, N1).
	retentionRecheck = time.Minute
	// alertEvery is the shortest time between two warn events of one refusal reason.
	alertEvery = 10 * time.Second
)

// ContentType is the content type of a change event (ST-SUP-RP01-LC30).
const ContentType = "application/json"

// kindPattern is the form of an outbox kind, and namePattern the form of a service, subscription or worker name.
var (
	kindPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
)

// namespace is the UUID namespace of the message IDs of the inbox.
var namespace = uuid.MustParse("7d1c6f0e-6a3b-4a59-9d6c-2f9b1e0c5a41")

// MessageID is the ID of the outbox message of the record with the event ID eventID of the tenant tenantID in the
// subscription subscription: a UUID version 5 over the three, each prefixed by its length, so one event of one
// tenant gives one message in one subscription, and an event ID of one tenant never stands for the event of another
// (ST-SUP-RP01-LC35).
func MessageID(subscription, tenantID, eventID string) uuid.UUID {
	var b strings.Builder
	for _, part := range []string{subscription, tenantID, eventID} {
		b.WriteString(strconv.Itoa(len(part)))
		b.WriteByte(':')
		b.WriteString(part)
	}
	return uuid.NewSHA1(namespace, []byte(b.String()))
}

// Subscription is what the generated code declares of one subscription (ST-DECL-EN28).
type Subscription struct {
	// Service is the name of the service that consumes.
	Service string
	// Name is the subscription's name: the destination of its messages in the outbox, which the consuming worker
	// consumes, and the stager of each of them (ST-SUP-RP10-LC16).
	Name string
	// Source is the service whose change events the subscription takes: each kind starts with its name and a period.
	Source string
	// Events are the event kinds that the subscription takes, 1 to MaxEvents.
	Events []string
	// Worker is the consuming worker: its service account is the staging principal of each message.
	Worker string
}

// check refuses a subscription that the inbox cannot serve.
func (s Subscription) check() error {
	for _, name := range []string{s.Service, s.Name, s.Source, s.Worker} {
		if !namePattern.MatchString(name) {
			return errors.New("kafkainbox: the service, the subscription, the source and the worker must be names in lower kebab case")
		}
	}
	if len(s.Events) == 0 || len(s.Events) > MaxEvents {
		return fmt.Errorf("kafkainbox: a subscription takes 1 to %d event kinds", MaxEvents)
	}
	for i, kind := range s.Events {
		if !kindPattern.MatchString(kind) || !strings.HasPrefix(kind, s.Source+".") || slices.Contains(s.Events[:i], kind) {
			return fmt.Errorf("kafkainbox: the event kind %q is not an outbox kind of %s, or it is given twice", kind, s.Source)
		}
	}
	return nil
}

// Listing is the state of one tenant ID in the tenant list: not listed, listed and active, or listed and retired.
type Listing int

const (
	// NotListed is a tenant ID that the tenant list does not hold, or that is not a valid tenant ID.
	NotListed Listing = iota
	// Active is a tenant that the tenant list holds with no retire time.
	Active
	// Retired is a tenant that the tenant list holds with a retire time.
	Retired
)

// Lookup reads the entry of a tenant ID in the tenant list.
type Lookup func(ctx context.Context, id string) (tenant.Tenant, Listing, error)

// TenantList gives the Lookup of the tenant list stego_tenants of schema on db, the pool of the runtime role
// (ST-SUP-RP01-LC35). The ID of a record header is only the key of the read: the tenant is built from the tenant
// column of the entry that the statement read, through the row reader of the tenant runtime (tenant.ScanRows,
// ST-SUP-RP04-LC01), so a header is never a source of a tenant. An ID that is not a valid tenant ID is not listed,
// and the statement is not sent; a failed read is an error.
func TenantList(db *sql.DB, schema string) (Lookup, error) {
	if db == nil || schema == "" || strings.ContainsRune(schema, 0) {
		return nil, errors.New("kafkainbox: the tenant list needs a database and a schema")
	}
	query := `SELECT tenant_id, retired_at IS NULL FROM "` + strings.ReplaceAll(schema, `"`, `""`) + `".stego_tenants WHERE tenant_id = $1`
	return func(ctx context.Context, id string) (tenant.Tenant, Listing, error) {
		if tenant.ValidID(id) != nil {
			return tenant.Tenant{}, NotListed, nil
		}
		rows, err := db.QueryContext(ctx, query, id)
		if err != nil {
			return tenant.Tenant{}, NotListed, fmt.Errorf("kafkainbox: read the entry of the tenant list: %w", err)
		}
		defer rows.Close()
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return tenant.Tenant{}, NotListed, fmt.Errorf("kafkainbox: read the entry of the tenant list: %w", err)
			}
			return tenant.Tenant{}, NotListed, nil
		}
		var active bool
		t, err := tenant.ScanRows(rows, &active)
		if err != nil {
			return tenant.Tenant{}, NotListed, err
		}
		if !active {
			return t, Retired, nil
		}
		return t, Active, nil
	}, nil
}

// Config is what the inbox needs.
type Config struct {
	Subscription Subscription
	Settings     Settings
	// Queue is the outbox queue of the service, and DB the pool of the worker role, on which the inbox opens the
	// transaction of each record and writes the refused records.
	Queue *outbox.Queue
	DB    *sql.DB
	// Tenants looks up the tenant of each record in the tenant list, and TenantSchema is the schema of that list,
	// which the write transaction of each record reads again under the lock of the tenant list (tenant.HoldList).
	Tenants      Lookup
	TenantSchema string
	// Retention is the done retention of the outbox worker: the time that the inbox keeps the ID of a record that it
	// wrote. The topic's retention must not be longer (ST-SUP-RP01-LC35).
	Retention time.Duration
	// Logs and Meter give the log events and the counters of the inbox; nil gives none.
	Logs  otellog.LoggerProvider
	Meter metric.MeterProvider
	// FailingAfter is the number of failed sessions in a row after which Failing gives the failure class; zero is
	// DefaultFailingAfter.
	FailingAfter int
	// Options are added to the options of the client, for a test only.
	Options []kgo.Opt
}

// Inbox is the Kafka inbox of one subscription.
type Inbox struct {
	sub       Subscription
	settings  Settings
	queue     *outbox.Queue
	db        *sql.DB
	tenants   Lookup
	schema    string
	retention time.Duration
	principal string
	options   []kgo.Opt
	logger    otellog.Logger
	records   metric.Int64Counter

	failingAfter int
	// backoff gives the wait before the next session after the attempt-th failure in a row, and now the clock of
	// the alerts; a test sets them.
	backoff  func(attempt int) time.Duration
	now      func() time.Time
	accepted sync.Once

	mu       sync.Mutex
	failed   string
	failures int
	failing  string
	alerts   map[string]*alertState
	// attempts is the count of the sessions in a row since the inbox last read, which sets the backoff of the next
	// session (security review of PR #418, N5).
	attempts int
}

// alertState is the last warn event of one refusal reason and the refusals since it that sent none.
type alertState struct {
	last       time.Time
	suppressed int64
}

// New gives the inbox of the configuration. It checks the subscription and the settings, and loads the TLS files of
// the connection; it opens no connection: Run does.
func New(c Config) (*Inbox, error) {
	if err := c.Subscription.check(); err != nil {
		return nil, err
	}
	if c.Queue == nil || c.DB == nil || c.Tenants == nil || c.TenantSchema == "" || c.Retention <= 0 {
		return nil, errors.New("kafkainbox: the inbox needs the queue, the worker role's database, the tenant list with its schema and the done retention")
	}
	// The settings name the settings of their subscription in each refusal (ST-SUP-RP01-LC38).
	switch c.Settings.Subscription {
	case "":
		c.Settings.Subscription = c.Subscription.Name
	case c.Subscription.Name:
	default:
		return nil, errors.New("kafkainbox: the settings are those of another subscription")
	}
	if err := c.Settings.Check(); err != nil {
		return nil, err
	}
	connection, err := kafkaoutbox.Connection(c.Settings.Connection)
	if err != nil {
		return nil, err
	}
	principal, err := authz.System(c.Subscription.Worker)
	if err != nil {
		return nil, err
	}
	options := append(connection,
		kgo.ClientID("stego-inbox"),
		kgo.ConsumerGroup(c.Settings.Group),
		kgo.ConsumeTopics(c.Settings.Topic),
		// The offset of a record is committed only after its transaction committed (ST-SUP-RP01-LC36).
		kgo.DisableAutoCommit(),
		// No rebalance takes a partition between the write of a poll and the commit of its offsets.
		kgo.BlockRebalanceOnPoll(),
		// A new group reads the topic from its start: the IDs of the records make a second write a no-op.
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.FetchIsolationLevel(kgo.ReadCommitted()),
		kgo.FetchMaxBytes(kafkaoutbox.MaxReadBytes),
		kgo.FetchMaxPartitionBytes(kafkaoutbox.MaxReadBytes),
	)
	in := &Inbox{sub: c.Subscription, settings: c.Settings, queue: c.Queue, db: c.DB, tenants: c.Tenants,
		schema: c.TenantSchema, retention: c.Retention, principal: principal.Subject().String(), options: append(options, c.Options...),
		failingAfter: c.FailingAfter, backoff: backoff, now: time.Now, alerts: map[string]*alertState{}}
	if in.failingAfter <= 0 {
		in.failingAfter = DefaultFailingAfter
	}
	if c.Logs != nil {
		in.logger = c.Logs.Logger(instrumentation)
	}
	if c.Meter != nil {
		if in.records, err = c.Meter.Meter(instrumentation).Int64Counter("kafka.consumer.records",
			metric.WithDescription("The records that the Kafka inbox read, by subscription, outcome and reason."),
			metric.WithUnit("{record}")); err != nil {
			return nil, err
		}
		// kafka.consumer.failing is 1 while the inbox fails, so an alert reads it (security review of PR #418, M2).
		if _, err = c.Meter.Meter(instrumentation).Int64ObservableGauge("kafka.consumer.failing",
			metric.WithDescription("1 while the Kafka inbox of the subscription fails each session, else 0."),
			metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
				value := int64(0)
				if in.Failing() != "" {
					value = 1
				}
				o.Observe(value, metric.WithAttributes(attribute.String("subscription", in.sub.Name)))
				return nil
			})); err != nil {
			return nil, err
		}
	}
	return in, nil
}

// backoff is the wait before the next session after the attempt-th failure in a row: it doubles from backoffMin up
// to backoffMax, with jitter.
func backoff(attempt int) time.Duration {
	wait := min(backoffMax, backoffMin<<min(attempt, 16))
	return wait/2 + rand.N(wait/2+1)
}

// Failing gives the fixed failure class of the last session when FailingAfter sessions in a row failed, or empty.
// The readiness of the worker mode names it, and kafka.consumer.failing is 1 while it is set; a session that reads
// the topic again clears it (security review of PR #418, M2).
func (in *Inbox) Failing() string {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.failing
}

// sessionFailed counts a failed session in a row, and sets the failure class from FailingAfter of them.
func (in *Inbox) sessionFailed(class string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.failures++
	if in.failures >= in.failingAfter {
		in.failing = class
	}
}

// reading clears the failures and the backoff: the session read records of the topic, or the broker answered a check
// of the topic. A poll that ends at its bound with no record does not count, as the client retries a lost broker
// with no error (security review of PR #418, N1, N5).
func (in *Inbox) reading() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.failures, in.failing, in.attempts = 0, "", 0
}

// nextAttempt counts one more session since the inbox last read, and gives the count for the backoff.
func (in *Inbox) nextAttempt() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.attempts++
	return in.attempts
}

// Name is the name of the inbox among the components of the worker mode: inbox-<subscription>.
func (in *Inbox) Name() string { return "inbox-" + in.sub.Name }

// Destination is the outbox destination that the inbox writes each record to: the name of its subscription.
func (in *Inbox) Destination() string { return in.sub.Name }

// Principal is the system principal of the consuming worker of the subscription, the staging principal of each
// message, as text: the resolver of the parked messages that a done message frees (ST-SUP-RP01-LC39).
func (in *Inbox) Principal() string { return in.principal }

// Purge removes at most limit refused records older than RefusedRetention, for the purge timer of the outbox
// worker (outbox.Worker.AddPurge, ST-SUP-RP01-LC37).
func (in *Inbox) Purge(ctx context.Context, limit int) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	return purgeRefused(ctx, in.db, limit, RefusedRetention)
}

// ErrRetention is the error of Run when the topic's retention is longer than the done retention of the outbox, or
// infinite with no InfiniteRetentionSetting of its subscription: an offset reset could then apply a record twice (ST-SUP-RP01-LC35).
var ErrRetention = errors.New("kafkainbox: the topic keeps its records longer than the inbox keeps their IDs")

// RetentionStage is the stage that names a start of the inbox that ErrRetention stopped.
const RetentionStage = "kafka-consumer.retention"

// Failed gives the stage of the failure that stopped Run for good, such as RetentionStage, or empty.
func (in *Inbox) Failed() string {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.failed
}

// Run reads the topic until ctx ends (ST-SUP-RP01-LC36). It opens the client, checks the topic's retention, and then
// polls: it writes each record of a poll in its order, in a transaction of its own, and commits the offsets of the
// records that it wrote, refused or skipped, and only of those, before the next poll. A failure that a later attempt
// can pass, such as a database or broker outage, closes the client and opens it again after a backoff, so the
// records whose offsets were not committed are read again and their second write is a no-op. A retention that
// ErrRetention refuses stops Run with that error and the log event kafka.consumer.start_refused.
func (in *Inbox) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		err := in.session(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, ErrRetention):
			in.mu.Lock()
			in.failed = RetentionStage
			in.mu.Unlock()
			in.emit(ctx, "kafka.consumer.start_refused", otellog.SeverityError, "the Kafka inbox did not start: "+RetentionStage,
				attribute.String("subscription", in.sub.Name), attribute.String("stage", RetentionStage))
			return err
		case err != nil:
			class := failureClass(err)
			in.sessionFailed(class)
			// The subscription names the inbox of a service with several (decision on ST-SUP-Q204).
			in.emit(ctx, "kafka.consumer.failed", otellog.SeverityWarn, "the Kafka inbox failed and opens its client again",
				attribute.String("subscription", in.sub.Name), attribute.String("failure", class))
		}
		// A session that read before it failed, or ended clean, starts the backoff again (reading).
		attempt := in.nextAttempt()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(in.backoff(attempt)):
		}
	}
	return nil
}

// failureClass gives the fixed class of a failure of a session, with no error text. A failure of a request to the
// broker names the operation that failed, describe-configs, fetch or commit, after the broker's code, such as
// broker:TOPIC_AUTHORIZATION_FAILED (describe-configs), so an operator sees which ACL operation the user lacks
// (walkthrough 10, W10-B1; ST-SUP-RP01-LC36).
func failureClass(err error) string {
	var kafkaErr *kerr.Error
	var failed *opError
	op := ""
	if errors.As(err, &failed) {
		op = " (" + failed.op + ")"
	}
	switch {
	case errors.As(err, &kafkaErr):
		return "broker:" + kafkaErr.Message + op
	case errors.Is(err, errDatabase):
		return "database"
	}
	return "broker" + op
}

// The operations of a session at the broker that a failure class names.
const (
	opDescribeConfigs = "describe-configs"
	opFetch           = "fetch"
	opCommit          = "commit"
)

// opError is a failure of one operation of a session at the broker. It names the operation and wraps the error.
type opError struct {
	op  string
	err error
}

func (e *opError) Error() string { return "kafkainbox: " + e.op + ": " + e.err.Error() }
func (e *opError) Unwrap() error { return e.err }

var (
	errDatabase = errors.New("kafkainbox: the database refused a write")
	errCommit   = errors.New("kafkainbox: the commit of the offsets failed")
)

// session runs one client until a failure or the end of ctx.
func (in *Inbox) session(ctx context.Context) error {
	client, err := kgo.NewClient(in.options...)
	if err != nil {
		return err
	}
	defer client.Close()
	// A poll blocks rebalances until AllowRebalance: the close leaves the group, so it allows them first.
	defer client.AllowRebalance()
	if err := in.checkRetention(ctx, client); err != nil {
		return err
	}
	checked := in.now()
	in.reading()
	for {
		// A poll ends at its bound when no record comes; only records or a check that the broker answers show that
		// the inbox reads the topic (N1).
		pollCtx, cancelPoll := context.WithTimeout(ctx, pollWait)
		fetches := client.PollRecords(pollCtx, MaxPoll)
		cancelPoll()
		if ctx.Err() != nil || fetches.IsClientClosed() {
			return ctx.Err()
		}
		var fetchErr error
		fetches.EachError(func(_ string, _ int32, err error) {
			if fetchErr == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				fetchErr = err
			}
		})
		var taken []*kgo.Record
		var failure error
		for _, record := range fetches.Records() {
			if failure = in.take(ctx, record); failure != nil {
				break
			}
			taken = append(taken, record)
		}
		if len(taken) > 0 {
			// The records of one partition come in their order, so the taken records are a first part of each
			// partition's records, and the commit of their highest offset commits no record that was not taken.
			commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
			err := client.CommitRecords(commitCtx, taken...)
			cancel()
			if err != nil {
				client.AllowRebalance()
				return &opError{op: opCommit, err: fmt.Errorf("%w: %w", errCommit, err)}
			}
		}
		client.AllowRebalance()
		if failure != nil {
			return failure
		}
		if fetchErr != nil {
			return &opError{op: opFetch, err: fetchErr}
		}
		if len(taken) > 0 {
			in.reading()
		}
		// The retention and the policy of the topic are read again in the session; a broker that answers counts as
		// read, so an idle topic stays ready, and one that does not answer fails the session (N1).
		if in.now().Sub(checked) >= retentionRecheck {
			if err := in.checkRetention(ctx, client); err != nil {
				return err
			}
			checked = in.now()
			in.reading()
		}
	}
}

// checkRetention reads the retention.ms of the topic with the describe configs request, and refuses a retention
// longer than the done retention, or an infinite one with no InfiniteRetentionSetting of the subscription, with ErrRetention.
func (in *Inbox) checkRetention(ctx context.Context, client *kgo.Client) error {
	ctx, cancel := context.WithTimeout(ctx, retentionTimeout)
	defer cancel()
	request := kmsg.NewPtrDescribeConfigsRequest()
	resource := kmsg.NewDescribeConfigsRequestResource()
	resource.ResourceType = kmsg.ConfigResourceTypeTopic
	resource.ResourceName = in.settings.Topic
	resource.ConfigNames = []string{"retention.ms", "cleanup.policy"}
	request.Resources = append(request.Resources, resource)
	response, err := request.RequestWith(ctx, client)
	if err != nil {
		return &opError{op: opDescribeConfigs, err: err}
	}
	if len(response.Resources) != 1 {
		return &opError{op: opDescribeConfigs, err: errors.New("kafkainbox: the broker described no topic")}
	}
	described := response.Resources[0]
	if err := kerr.ErrorForCode(described.ErrorCode); err != nil {
		return &opError{op: opDescribeConfigs, err: err}
	}
	ms, policy, err := topicRetention(described.Configs)
	if err != nil {
		return err
	}
	if err := RetentionFits(ms, policy, in.retention, in.settings.AcceptInfiniteRetention, Setting(InfiniteRetentionSetting, in.sub.Name)); err != nil {
		return err
	}
	if ms < 0 || compacted(policy) {
		// The acceptance of a topic that keeps records without end is logged once (security review of PR #418, L5).
		in.accepted.Do(func() {
			in.emit(ctx, "kafka.consumer.retention_accepted", otellog.SeverityInfo,
				"the Kafka inbox reads a topic that keeps records without end, as "+Setting(InfiniteRetentionSetting, in.sub.Name)+"=1 accepts",
				attribute.String("subscription", in.sub.Name))
		})
	}
	return nil
}

// topicRetention reads the retention.ms and the cleanup.policy of a topic from the configs that the broker described,
// and refuses a config that is missing or has no value, as the inbox cannot know what the broker keeps (security
// review of PR #418, N3).
func topicRetention(configs []kmsg.DescribeConfigsResponseResourceConfig) (int64, string, error) {
	var ms int64
	var policy string
	var foundMS, foundPolicy bool
	for _, config := range configs {
		if config.Value == nil {
			continue
		}
		switch config.Name {
		case "retention.ms":
			value, err := strconv.ParseInt(*config.Value, 10, 64)
			if err != nil {
				return 0, "", errors.New("kafkainbox: the topic's retention.ms is not a number")
			}
			ms, foundMS = value, true
		case "cleanup.policy":
			policy, foundPolicy = *config.Value, true
		}
	}
	switch {
	case !foundMS:
		return 0, "", errors.New("kafkainbox: the broker gave no retention.ms of the topic")
	case !foundPolicy:
		return 0, "", errors.New("kafkainbox: the broker gave no cleanup.policy of the topic")
	}
	return ms, policy, nil
}

// compacted reports whether a cleanup.policy compacts the topic: compact, alone or with delete.
func compacted(policy string) bool {
	for _, p := range strings.Split(policy, ",") {
		if strings.TrimSpace(p) == "compact" {
			return true
		}
	}
	return false
}

// RetentionFits refuses with ErrRetention a topic retention of retentionMS milliseconds that is longer than the done
// retention, or that is infinite (a negative value) or compacted (a cleanup.policy that holds compact, which keeps the
// last record of each key without end) when acceptInfinite is false. Its error names setting, the setting of the
// subscription that accepts an infinite retention.
func RetentionFits(retentionMS int64, policy string, done time.Duration, acceptInfinite bool, setting string) error {
	if compacted(policy) {
		if acceptInfinite {
			return nil
		}
		return fmt.Errorf("%w: the topic is compacted, so it keeps the last record of each key without end; set %s=1 to accept that an offset reset beyond the done retention can apply a record twice",
			ErrRetention, setting)
	}
	if retentionMS < 0 {
		if acceptInfinite {
			return nil
		}
		return fmt.Errorf("%w: the retention is infinite; set %s=1 to accept that an offset reset beyond the done retention can apply a record twice",
			ErrRetention, setting)
	}
	if time.Duration(retentionMS)*time.Millisecond > done {
		return fmt.Errorf("%w: give the outbox a done retention at least as long as the topic's", ErrRetention)
	}
	return nil
}

// Outcomes of one record, the values of the attribute outcome of kafka.consumer.records.
const (
	OutcomeWritten   = "written"
	OutcomeDuplicate = "duplicate"
	OutcomeSkipped   = "skipped"
	OutcomeRefused   = "refused"
)

// take writes one record: into the outbox when every check passes, into the refused records when one fails, or
// nowhere for a kind of the source that the subscription does not take. It returns an error only for a failure that
// a later attempt can pass, and then the record's offset is not committed.
func (in *Inbox) take(ctx context.Context, record *kgo.Record) error {
	checked, reason := in.check(record)
	switch {
	case reason == OutcomeSkipped:
		in.count(ctx, OutcomeSkipped, "")
		return nil
	case reason != "":
		return in.refuse(ctx, record, checked, reason)
	}
	lookupCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	t, listing, err := in.tenants(lookupCtx, checked.tenant)
	cancel()
	if err != nil {
		return fmt.Errorf("%w: %w", errDatabase, err)
	}
	switch listing {
	case NotListed:
		return in.refuse(ctx, record, checked, ReasonUnknownTenant)
	case Retired:
		return in.refuse(ctx, record, checked, ReasonRetiredTenant)
	}
	message := outbox.Message{ID: MessageID(in.sub.Name, t.ID(), checked.eventID), Destination: in.sub.Name,
		ResourceKey: checked.resourceKey, Kind: checked.kind, Payload: json.RawMessage(record.Value),
		Principal: in.principal, Origin: in.sub.Name, MustLand: true, Idempotent: true,
		// The position of the record keeps the order of its key as in its topic, also for a record that an offset
		// reset sends again (ST-SUP-RP01-LC03, ST-SUP-RP01-LC35).
		Position: &outbox.Position{Partition: record.Partition, Offset: record.Offset}}
	if err := outbox.ValidateMessage(message); err != nil || len(record.Value) > in.queue.PayloadLimit(in.sub.Name) {
		return in.refuse(ctx, record, checked, ReasonNotOutboxMessage)
	}
	received, err := in.write(ctx, t, message)
	if errors.Is(err, errRetiredSince) {
		return in.refuse(ctx, record, checked, ReasonRetiredTenant)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errDatabase, err)
	}
	switch received {
	case outbox.ReceivedWritten:
		in.count(ctx, OutcomeWritten, "")
	case outbox.ReceivedDuplicate:
		in.count(ctx, OutcomeDuplicate, "")
	default:
		// The event ID of the tenant was used for another message: the first one stays (security review of PR
		// #418, L2).
		return in.refuse(ctx, record, checked, ReasonEventIDReused)
	}
	return nil
}

// errRetiredSince is the error of a write whose tenant was retired after the lookup of the record.
var errRetiredSince = errors.New("kafkainbox: the tenant was retired after the lookup")

// write writes message in a transaction of the tenant t and commits it (ST-SUP-RP01-LC35). The transaction first
// takes the lock of the tenant list for t and reads again that t is active, so a record never lands after a Retire
// of t committed (security review of PR #418, L1); a tenant retired since the lookup is errRetiredSince.
func (in *Inbox) write(ctx context.Context, t tenant.Tenant, message outbox.Message) (outbox.Received, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	// READ COMMITTED: each statement reads what committed before it, so the read after the lock sees the Retire.
	tx, err := tenant.Begin(ctx, in.db, t, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := tenant.HoldList(ctx, tx, t); err != nil {
		return 0, err
	}
	active, err := tenant.ActiveIn(ctx, tx, in.schema, t)
	if err != nil {
		return 0, err
	}
	if !active {
		return 0, errRetiredSince
	}
	received, err := in.queue.Receive(ctx, tx, message)
	if err != nil {
		return 0, err
	}
	if err := tenant.Commit(ctx, tx, t); err != nil {
		return 0, err
	}
	return received, nil
}

// checked are the parts of a record that passed their form check.
type checked struct {
	eventID, kind, tenant, resourceKey string
}

// check checks the headers, the key and the value of record (ST-SUP-RP01-LC36). It gives the parts that passed their
// form check and the fixed reason of the first check that failed, OutcomeSkipped for a kind of the source that the
// subscription does not take, or empty when every check passed.
func (in *Inbox) check(record *kgo.Record) (checked, string) {
	var c checked
	headers := map[string]string{}
	for _, h := range record.Headers {
		if _, ok := headers[h.Key]; ok {
			return c, ReasonDuplicateHeader
		}
		headers[h.Key] = string(h.Value)
	}
	eventID, hasID := headers[kafkaoutbox.HeaderEventID]
	kind, hasKind := headers[kafkaoutbox.HeaderEventType]
	tenantID, hasTenant := headers[kafkaoutbox.HeaderTenant]
	if !hasID || !hasKind || !hasTenant {
		return c, ReasonMissingHeader
	}
	if id, err := uuid.Parse(eventID); err != nil || id.String() != eventID {
		return c, ReasonBadEventID
	}
	c.eventID = eventID
	if !kindPattern.MatchString(kind) {
		return c, ReasonBadKind
	}
	c.kind = kind
	if !strings.HasPrefix(kind, in.sub.Source+".") {
		return c, ReasonForeignKind
	}
	if !slices.Contains(in.sub.Events, kind) {
		return c, OutcomeSkipped
	}
	if tenant.ValidID(tenantID) != nil {
		return c, ReasonBadTenant
	}
	c.tenant = tenantID
	if headers[kafkaoutbox.HeaderContentType] != ContentType {
		return c, ReasonBadContentType
	}
	if len(record.Value) > in.queue.PayloadLimit(in.sub.Name) {
		return c, ReasonTooLarge
	}
	if len(record.Value) == 0 || !json.Valid(record.Value) {
		return c, ReasonNotJSON
	}
	rowID, entity, ok := payloadOfKind(record.Value, kind)
	if !ok {
		return c, ReasonKindMismatch
	}
	keyTenant, resourceKey, found := strings.Cut(string(record.Key), kafkaoutbox.KeySeparator)
	if !found || keyTenant != tenantID {
		return c, ReasonKeyMismatch
	}
	if len(resourceKey) < 1 || len(resourceKey) > 256 {
		return c, ReasonBadResourceKey
	}
	// The key names the row of the payload: the resource key of a change event is exactly <type>:<row ID>, where the
	// type is the resource type of the entity or, for an entity with none, the entity name (security review of PR
	// #418, N4; of PR #421, L5).
	keyType, keyID, found := strings.Cut(resourceKey, ":")
	if !found || keyID != rowID || !typeOfEntity(keyType, entity) {
		return c, ReasonKeyMismatch
	}
	c.resourceKey = resourceKey
	return c, ""
}

// payloadOfKind reports whether payload is a change event of kind <source>.<entity>.<action>: an object whose entity,
// in lower kebab case, and action are those of the kind, with a row ID (ST-SUP-RC20-OC40; security review of PR
// #418, L6). It gives the row ID and the entity of the payload. It reads the members entity, action and id by their
// exact names, and refuses a payload that holds one of them twice or in another case, such as ID beside id, which a
// decoder of the fill could read in place of the member that the inbox checked (security review of PR #421, L5).
func payloadOfKind(payload []byte, kind string) (id, entity string, ok bool) {
	parts := strings.Split(kind, ".")
	if len(parts) != 3 {
		return "", "", false
	}
	members, ok := eventMembers(payload)
	if !ok || members["id"] == "" {
		return "", "", false
	}
	return members["id"], members["entity"], kebab(members["entity"]) == parts[1] && members["action"] == parts[2]
}

// eventMembers reads the string members entity, action and id of the top-level object of payload by their exact
// names. It refuses a payload that is not an object, and one that holds one of these members twice, in another case,
// or with a value that is not a string.
func eventMembers(payload []byte) (map[string]string, bool) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if open, err := decoder.Token(); err != nil || open != json.Delim('{') {
		return nil, false
	}
	members := map[string]string{}
	for decoder.More() {
		token, err := decoder.Token()
		name, isName := token.(string)
		if err != nil || !isName {
			return nil, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false
		}
		folded := strings.ToLower(name)
		if folded != "entity" && folded != "action" && folded != "id" {
			continue
		}
		if _, seen := members[folded]; seen || name != folded {
			return nil, false
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return nil, false
		}
		members[folded] = text
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return nil, false
	}
	return members, true
}

// resourceTypeName is the form of a declared resource type: a lower-case SQL name.
var resourceTypeName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// typeOfEntity reports whether keyType can be the type of the resource key of a change event of entity: the entity
// name, which the key holds for an entity with no resource type, or a name of the form of a resource type. The inbox
// does not hold the resource types of the source's entities, so it binds the type to the entity by form only.
func typeOfEntity(keyType, entity string) bool {
	return keyType == entity || resourceTypeName.MatchString(keyType)
}

// kebab gives an entity name in lower kebab case, as the change event kind names it: a hyphen before each capital
// letter but the first, and each letter in lower case, such as org-setting for OrgSetting.
func kebab(name string) string {
	var b strings.Builder
	for i, r := range name {
		if 'A' <= r && r <= 'Z' {
			if i > 0 {
				b.WriteByte('-')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// refuse writes the refused record, with the parts that passed their form check, raises its alert and counts it. A
// failed write is a failure that a later attempt can pass.
func (in *Inbox) refuse(ctx context.Context, record *kgo.Record, c checked, reason string) error {
	r := Refusal{Subscription: in.sub.Name, Topic: record.Topic, Partition: record.Partition, Offset: record.Offset,
		Reason: reason, EventID: c.eventID, Tenant: c.tenant, Kind: c.kind}
	writeCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := storeRefusal(writeCtx, in.db, r); err != nil {
		return fmt.Errorf("%w: %w", errDatabase, err)
	}
	in.count(ctx, OutcomeRefused, reason)
	in.alert(ctx, reason, record.Partition, record.Offset)
	return nil
}

// alert sends the warn event kafka.consumer.refused of a refused record, at most once in alertEvery for each reason,
// with the number of refusals of the reason since the last event that sent none, so a flood of bad records does not
// flood the log (security review of PR #418, L3). The counter kafka.consumer.records counts each refusal.
func (in *Inbox) alert(ctx context.Context, reason string, partition int32, offset int64) {
	now := in.now()
	in.mu.Lock()
	state := in.alerts[reason]
	if state == nil {
		state = &alertState{}
		in.alerts[reason] = state
	} else if now.Sub(state.last) < alertEvery {
		state.suppressed++
		in.mu.Unlock()
		return
	}
	suppressed := state.suppressed
	state.last, state.suppressed = now, 0
	in.mu.Unlock()
	in.emit(ctx, "kafka.consumer.refused", otellog.SeverityWarn, "the Kafka inbox refused a record: "+reason,
		attribute.String("subscription", in.sub.Name), attribute.String("reason", reason),
		attribute.Int64("partition", int64(partition)), attribute.Int64("offset", offset),
		attribute.Int64("suppressed", suppressed))
}

// count adds one record to the counter kafka.consumer.records.
func (in *Inbox) count(ctx context.Context, outcome, reason string) {
	if in.records == nil {
		return
	}
	attributes := []attribute.KeyValue{attribute.String("subscription", in.sub.Name), attribute.String("outcome", outcome)}
	if reason != "" {
		attributes = append(attributes, attribute.String("reason", reason))
	}
	in.records.Add(ctx, 1, metric.WithAttributes(attributes...))
}

// emit writes one log event of the inbox, with fixed text and no value of a record.
func (in *Inbox) emit(ctx context.Context, name string, severity otellog.Severity, body string, attributes ...attribute.KeyValue) {
	if in.logger == nil {
		return
	}
	var record otellog.Record
	record.SetEventName(name)
	record.SetSeverity(severity)
	record.SetSeverityText(map[otellog.Severity]string{otellog.SeverityInfo: "INFO", otellog.SeverityWarn: "WARN", otellog.SeverityError: "ERROR"}[severity])
	record.SetBody(attribute.StringValue(body))
	record.AddAttributes(attributes...)
	in.logger.Emit(ctx, record)
}
