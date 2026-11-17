package kafkainbox

import (
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
	// Tenants looks up the tenant of each record in the tenant list.
	Tenants Lookup
	// Retention is the done retention of the outbox worker: the time that the inbox keeps the ID of a record that it
	// wrote. The topic's retention must not be longer (ST-SUP-RP01-LC35).
	Retention time.Duration
	// Logs and Meter give the log events and the counters of the inbox; nil gives none.
	Logs  otellog.LoggerProvider
	Meter metric.MeterProvider
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
	retention time.Duration
	principal string
	options   []kgo.Opt
	logger    otellog.Logger
	records   metric.Int64Counter

	mu     sync.Mutex
	failed string
}

// New gives the inbox of the configuration. It checks the subscription and the settings, and loads the TLS files of
// the connection; it opens no connection: Run does.
func New(c Config) (*Inbox, error) {
	if err := c.Subscription.check(); err != nil {
		return nil, err
	}
	if c.Queue == nil || c.DB == nil || c.Tenants == nil || c.Retention <= 0 {
		return nil, errors.New("kafkainbox: the inbox needs the queue, the worker role's database, the tenant list and the done retention")
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
		retention: c.Retention, principal: principal.Subject().String(), options: append(options, c.Options...)}
	if c.Logs != nil {
		in.logger = c.Logs.Logger(instrumentation)
	}
	if c.Meter != nil {
		if in.records, err = c.Meter.Meter(instrumentation).Int64Counter("kafka.consumer.records",
			metric.WithDescription("The records that the Kafka inbox read, by subscription, outcome and reason."),
			metric.WithUnit("{record}")); err != nil {
			return nil, err
		}
	}
	return in, nil
}

// Name is the name of the inbox among the components of the worker mode: inbox-<subscription>.
func (in *Inbox) Name() string { return "inbox-" + in.sub.Name }

// Purge removes at most limit refused records older than RefusedRetention, for the purge timer of the outbox
// worker (outbox.Worker.AddPurge, ST-SUP-RP01-LC37).
func (in *Inbox) Purge(ctx context.Context, limit int) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	return purgeRefused(ctx, in.db, limit, RefusedRetention)
}

// ErrRetention is the error of Run when the topic's retention is longer than the done retention of the outbox, or
// infinite with no InfiniteRetentionSetting: an offset reset could then apply a record twice (ST-SUP-RP01-LC35).
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
	var attempt int
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
				attribute.String("stage", RetentionStage))
			return err
		case err != nil:
			in.emit(ctx, "kafka.consumer.failed", otellog.SeverityWarn, "the Kafka inbox failed and opens its client again",
				attribute.String("failure", failureClass(err)))
		}
		attempt = min(attempt+1, 16)
		wait := min(backoffMax, backoffMin<<attempt)
		wait = wait/2 + rand.N(wait/2+1)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
	return nil
}

// failureClass gives the fixed class of a failure of a session, with no error text.
func failureClass(err error) string {
	var kafkaErr *kerr.Error
	switch {
	case errors.As(err, &kafkaErr):
		return "broker:" + kafkaErr.Message
	case errors.Is(err, errDatabase):
		return "database"
	case errors.Is(err, errCommit):
		return "commit"
	}
	return "broker"
}

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
	for {
		fetches := client.PollRecords(ctx, MaxPoll)
		if ctx.Err() != nil || fetches.IsClientClosed() {
			return ctx.Err()
		}
		var fetchErr error
		fetches.EachError(func(_ string, _ int32, err error) {
			if fetchErr == nil && !errors.Is(err, context.Canceled) {
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
				return fmt.Errorf("%w: %w", errCommit, err)
			}
		}
		client.AllowRebalance()
		if failure != nil {
			return failure
		}
		if fetchErr != nil {
			return fetchErr
		}
	}
}

// checkRetention reads the retention.ms of the topic with the describe configs request, and refuses a retention
// longer than the done retention, or an infinite one with no InfiniteRetentionSetting, with ErrRetention.
func (in *Inbox) checkRetention(ctx context.Context, client *kgo.Client) error {
	ctx, cancel := context.WithTimeout(ctx, retentionTimeout)
	defer cancel()
	request := kmsg.NewPtrDescribeConfigsRequest()
	resource := kmsg.NewDescribeConfigsRequestResource()
	resource.ResourceType = kmsg.ConfigResourceTypeTopic
	resource.ResourceName = in.settings.Topic
	resource.ConfigNames = []string{"retention.ms"}
	request.Resources = append(request.Resources, resource)
	response, err := request.RequestWith(ctx, client)
	if err != nil {
		return err
	}
	if len(response.Resources) != 1 {
		return errors.New("kafkainbox: the broker described no topic")
	}
	described := response.Resources[0]
	if err := kerr.ErrorForCode(described.ErrorCode); err != nil {
		return err
	}
	for _, config := range described.Configs {
		if config.Name != "retention.ms" || config.Value == nil {
			continue
		}
		ms, err := strconv.ParseInt(*config.Value, 10, 64)
		if err != nil {
			return errors.New("kafkainbox: the topic's retention.ms is not a number")
		}
		return RetentionFits(ms, in.retention, in.settings.AcceptInfiniteRetention)
	}
	return errors.New("kafkainbox: the broker gave no retention.ms of the topic")
}

// RetentionFits refuses with ErrRetention a topic retention of retentionMS milliseconds that is longer than the done
// retention, or that is infinite (a negative value) when acceptInfinite is false.
func RetentionFits(retentionMS int64, done time.Duration, acceptInfinite bool) error {
	if retentionMS < 0 {
		if acceptInfinite {
			return nil
		}
		return fmt.Errorf("%w: the retention is infinite; set %s=1 to accept that an offset reset beyond the done retention can apply a record twice",
			ErrRetention, InfiniteRetentionSetting)
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
		Principal: in.principal, Origin: in.sub.Name, MustLand: true, Idempotent: true}
	if err := outbox.ValidateMessage(message); err != nil || len(record.Value) > in.queue.PayloadLimit(in.sub.Name) {
		return in.refuse(ctx, record, checked, ReasonNotOutboxMessage)
	}
	written, err := in.write(ctx, t, message)
	if err != nil {
		return fmt.Errorf("%w: %w", errDatabase, err)
	}
	if written {
		in.count(ctx, OutcomeWritten, "")
	} else {
		in.count(ctx, OutcomeDuplicate, "")
	}
	return nil
}

// write writes message in a transaction of the tenant t and commits it (ST-SUP-RP01-LC35).
func (in *Inbox) write(ctx context.Context, t tenant.Tenant, message outbox.Message) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	tx, err := tenant.Begin(ctx, in.db, t, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	written, err := in.queue.Receive(ctx, tx, message)
	if err != nil {
		return false, err
	}
	if err := tenant.Commit(ctx, tx, t); err != nil {
		return false, err
	}
	return written, nil
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
	keyTenant, resourceKey, found := strings.Cut(string(record.Key), kafkaoutbox.KeySeparator)
	if !found || keyTenant != tenantID {
		return c, ReasonKeyMismatch
	}
	if len(resourceKey) < 1 || len(resourceKey) > 256 {
		return c, ReasonBadResourceKey
	}
	c.resourceKey = resourceKey
	return c, ""
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
	in.emit(ctx, "kafka.consumer.refused", otellog.SeverityWarn, "the Kafka inbox refused a record: "+reason,
		attribute.String("subscription", in.sub.Name), attribute.String("reason", reason),
		attribute.Int64("partition", int64(record.Partition)), attribute.Int64("offset", record.Offset))
	return nil
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
	record.SetSeverityText(map[otellog.Severity]string{otellog.SeverityWarn: "WARN", otellog.SeverityError: "ERROR"}[severity])
	record.SetBody(attribute.StringValue(body))
	record.AddAttributes(attributes...)
	in.logger.Emit(ctx, record)
}
