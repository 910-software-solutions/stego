package kafkainbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/jsell-rh/stego/registry/runtime/internal/pgtest"
	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
	"github.com/jsell-rh/stego/registry/runtime/kafkatest"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/outbox/kafkaoutbox"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// testLogs writes each log event of the inbox to the log of the test.
type testLogs struct {
	noop.LoggerProvider
	t *testing.T
}

func (p testLogs) Logger(string, ...otellog.LoggerOption) otellog.Logger { return testLogger{t: p.t} }

type testLogger struct {
	noop.Logger
	t *testing.T
}

func (l testLogger) Emit(_ context.Context, record otellog.Record) {
	var attributes []string
	record.WalkAttributes(func(kv attribute.KeyValue) bool {
		attributes = append(attributes, string(kv.Key)+"="+kv.Value.Emit())
		return true
	})
	l.t.Logf("%s: %s %v", record.EventName(), record.Body().AsString(), attributes)
}

func TestMain(m *testing.M) { os.Exit(kafkatest.Wrap(func() int { return pgtest.Main(m) })) }

type decision struct{ seal.DecisionMark }

func requested(t testing.TB, id string, action tenant.RequestAction) tenant.Tenant {
	t.Helper()
	tn, err := tenant.New(tenant.Requested(action, id, decision{seal.NewDecisionMark(id, string(action))}))
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

const rolePassword = "kafkainbox-test"

// env is one database of the local PostgreSQL server with the outbox queue, the tenant list, the table of the refused
// records, a runtime role and a worker role, and the local Kafka broker.
type env struct {
	admin, runtime, worker *sql.DB
	// workerRole is the name of the worker role.
	workerRole string
	queue      *outbox.Queue
	broker     *kafkatest.Broker
	producer   *kgo.Client
}

func newEnv(t *testing.T) *env {
	t.Helper()
	broker := kafkatest.Start(t)
	ctx := context.Background()
	root, err := sql.Open("pgx", pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	tag := hex.EncodeToString(suffix)
	roles := [2]string{"ki_rt_" + tag, "ki_worker_" + tag}
	for _, role := range roles {
		if _, err := root.ExecContext(ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '"+rolePassword+"'"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role) })
	}
	relay := "ki_" + tag + outbox.RelaySuffix
	t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+relay) })
	database := pgtest.Database(t, "kafkainbox")
	admin, err := sql.Open("pgx", database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	t.Cleanup(func() {
		for _, role := range roles {
			_, _ = admin.ExecContext(context.Background(), "DROP OWNED BY "+role)
		}
	})
	queueText, err := outbox.Migration(relay, roles[0], roles[1])
	if err != nil {
		t.Fatal(err)
	}
	listText, err := tenant.Migration("public", roles[0])
	if err != nil {
		t.Fatal(err)
	}
	refusedText, err := Migration(roles[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{queueText, listText, refusedText,
		"GRANT USAGE ON SCHEMA public TO " + roles[0],
		// The worker role reads the tenant list in the write of each record, as the step kafka_inbox grants.
		"GRANT SELECT ON public.stego_tenants TO " + roles[1]} {
		if _, err := admin.ExecContext(ctx, text); err != nil {
			t.Fatal(err)
		}
	}
	open := func(role string) *sql.DB {
		u, err := url.Parse(database)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(role, rolePassword)
		db, err := sql.Open("pgx", u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	e := &env{admin: admin, runtime: open(roles[0]), worker: open(roles[1]), workerRole: roles[1], broker: broker}
	if e.queue, err = outbox.New(e.worker, outbox.Options{}); err != nil {
		t.Fatal(err)
	}
	if e.producer, err = kgo.NewClient(kgo.SeedBrokers(broker.Admin), kgo.DefaultProduceTopic("unused"),
		kgo.RecordPartitioner(kgo.StickyKeyPartitioner(nil))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.producer.Close)
	return e
}

// provision adds id to the tenant list, and retires it when retire is true.
func (e *env) provision(t *testing.T, id string, retire bool) {
	t.Helper()
	ctx := context.Background()
	if err := tenant.Provision(ctx, e.runtime, "public", requested(t, id, tenant.ProvisionAction), nil); err != nil {
		t.Fatal(err)
	}
	if retire {
		if err := tenant.Retire(ctx, e.runtime, "public", requested(t, id, tenant.RetireAction), nil); err != nil {
			t.Fatal(err)
		}
	}
}

// topic creates a new topic with partitions and the retention retentionMS.
func (e *env) topic(t *testing.T, partitions int32, retentionMS string, configs ...[2]string) string {
	t.Helper()
	name := "orders-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	request := kmsg.NewPtrCreateTopicsRequest()
	entry := kmsg.NewCreateTopicsRequestTopic()
	entry.Topic, entry.NumPartitions, entry.ReplicationFactor = name, partitions, 1
	config := kmsg.NewCreateTopicsRequestTopicConfig()
	config.Name, config.Value = "retention.ms", &retentionMS
	entry.Configs = append(entry.Configs, config)
	for _, c := range configs {
		extra := kmsg.NewCreateTopicsRequestTopicConfig()
		value := c[1]
		extra.Name, extra.Value = c[0], &value
		entry.Configs = append(entry.Configs, extra)
	}
	request.Topics = append(request.Topics, entry)
	request.TimeoutMillis = 10000
	response, err := request.RequestWith(context.Background(), e.producer)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range response.Topics {
		if err := kerr.ErrorForCode(result.ErrorCode); err != nil {
			t.Fatal(err)
		}
	}
	return name
}

// topicNamed creates the topic name with one partition and a retention of one day.
func (e *env) topicNamed(t *testing.T, name string) {
	t.Helper()
	request := kmsg.NewPtrCreateTopicsRequest()
	entry := kmsg.NewCreateTopicsRequestTopic()
	retention := "86400000"
	config := kmsg.NewCreateTopicsRequestTopicConfig()
	config.Name, config.Value = "retention.ms", &retention
	entry.Topic, entry.NumPartitions, entry.ReplicationFactor = name, 1, 1
	entry.Configs = append(entry.Configs, config)
	request.Topics = append(request.Topics, entry)
	request.TimeoutMillis = 10000
	if _, err := request.RequestWith(context.Background(), e.producer); err != nil {
		t.Fatal(err)
	}
}

// event is one record as the Kafka destination of a source service sends it, with headers that a test can change.
type event struct {
	eventID, kind, tenant, resourceKey, contentType string
	key                                             *string
	value                                           string
	extra                                           []kgo.RecordHeader
	drop                                            string
}

func change(tenantID, kind, id string) event {
	return event{eventID: uuid.NewString(), kind: kind, tenant: tenantID, resourceKey: "order:" + id,
		contentType: ContentType, value: `{"entity":"Order","action":"created","id":"` + id + `"}`}
}

func (e *env) produce(t *testing.T, topic string, events ...event) {
	t.Helper()
	for _, ev := range events {
		key := kafkaoutbox.Key(ev.tenant, ev.resourceKey)
		if ev.key != nil {
			key = *ev.key
		}
		record := &kgo.Record{Topic: topic, Key: []byte(key), Value: []byte(ev.value)}
		for _, h := range []kgo.RecordHeader{{Key: kafkaoutbox.HeaderEventID, Value: []byte(ev.eventID)},
			{Key: kafkaoutbox.HeaderEventType, Value: []byte(ev.kind)}, {Key: kafkaoutbox.HeaderTenant, Value: []byte(ev.tenant)},
			{Key: kafkaoutbox.HeaderContentType, Value: []byte(ev.contentType)}} {
			if h.Key != ev.drop {
				record.Headers = append(record.Headers, h)
			}
		}
		record.Headers = append(record.Headers, ev.extra...)
		if err := e.producer.ProduceSync(context.Background(), record).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
}

var sub = Subscription{Service: "fulfilment", Name: "order-changes", Source: "orders", Worker: "ship-orders",
	Events: []string{"orders.order.created"}}

// inbox builds the inbox of the topic in the group, with the done retention of the outbox.
func (e *env) inbox(t *testing.T, topic, group string, retention time.Duration) *Inbox {
	t.Helper()
	in, err := New(Config{Subscription: sub, Queue: e.queue, DB: e.worker, Retention: retention,
		Settings: Settings{Connection: kafkaoutbox.Settings{Brokers: []string{e.broker.Plain.Addr()}, InsecureLoopback: true},
			Topic: topic, Group: group},
		Tenants: e.lookup(t), TenantSchema: "public", Logs: testLogs{t: t}, Options: debugOptions()})
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// lookup gives the Lookup of the tenant list of the runtime role.
func (e *env) lookup(t *testing.T) Lookup {
	t.Helper()
	l, err := TenantList(e.runtime, "public")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// run runs in until the committed offsets of group cover want records of the topic, then stops it.
func (e *env) run(t *testing.T, in *Inbox, topic, group string, want int64) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- in.Run(ctx) }()
	for e.committed(t, topic, group) < want {
		select {
		case err := <-done:
			cancel()
			t.Fatalf("the inbox stopped before it committed %d records: %v", want, err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// committed gives the sum of the committed offsets of group on topic.
func (e *env) committed(t *testing.T, topic, group string) int64 {
	t.Helper()
	request := kmsg.NewPtrOffsetFetchRequest()
	request.Group = group
	fetch := kmsg.NewOffsetFetchRequestTopic()
	fetch.Topic = topic
	fetch.Partitions = []int32{0, 1, 2}
	request.Topics = append(request.Topics, fetch)
	response, err := request.RequestWith(context.Background(), e.producer)
	if err != nil {
		// The coordinator of a new group can be not yet available: no offset is committed yet.
		if kerr.IsRetriable(err) {
			return 0
		}
		t.Fatal(err)
	}
	var sum int64
	for _, tp := range response.Topics {
		for _, p := range tp.Partitions {
			if p.Offset > 0 {
				sum += p.Offset
			}
		}
	}
	return sum
}

// alter sets the config name of topic to value.
func (e *env) alter(t *testing.T, topic, name, value string) {
	t.Helper()
	request := kmsg.NewPtrIncrementalAlterConfigsRequest()
	resource := kmsg.NewIncrementalAlterConfigsRequestResource()
	resource.ResourceType, resource.ResourceName = kmsg.ConfigResourceTypeTopic, topic
	config := kmsg.NewIncrementalAlterConfigsRequestResourceConfig()
	config.Name, config.Op, config.Value = name, kmsg.IncrementalAlterConfigOpSet, &value
	resource.Configs = append(resource.Configs, config)
	request.Resources = append(request.Resources, resource)
	response, err := request.RequestWith(context.Background(), e.producer)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range response.Resources {
		if err := kerr.ErrorForCode(r.ErrorCode); err != nil {
			t.Fatal(err)
		}
	}
}

// handClock is a clock of the inbox that only the test moves.
type handClock struct {
	mu sync.Mutex
	at time.Time
}

func newHandClock() *handClock { return &handClock{at: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)} }

func (c *handClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *handClock) move(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func (e *env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInbox(t *testing.T) {
	e := newEnv(t)
	e.provision(t, "acme", false)
	e.provision(t, "gone", true)
	week := 7 * 24 * time.Hour

	t.Run("ST-SUP-RP01-LC35-T01 A record of an active tenant becomes one outbox message, and a second read of it writes nothing", func(t *testing.T) {
		topic := e.topic(t, 3, "86400000")
		events := []event{change("acme", "orders.order.created", "o-1"), change("acme", "orders.order.created", "o-2"),
			change("acme", "orders.order.created", "o-3")}
		e.produce(t, topic, events...)
		e.run(t, e.inbox(t, topic, "group-a", week), topic, "group-a", 3)
		for _, ev := range events {
			id := MessageID(sub.Name, "acme", ev.eventID)
			var destination, kind, principal, origin, resourceKey, state, tenantID string
			var mustLand, idempotent bool
			if err := e.admin.QueryRowContext(context.Background(), `SELECT destination, kind, principal, origin, resource_key, state,
				tenant_id, must_land, idempotent FROM stego_outbox.messages WHERE id = $1`, id).Scan(&destination, &kind, &principal,
				&origin, &resourceKey, &state, &tenantID, &mustLand, &idempotent); err != nil {
				t.Fatalf("the message of event %s: %v", ev.eventID, err)
			}
			if destination != sub.Name || kind != ev.kind || principal != "service_account:ship-orders" || origin != sub.Name ||
				resourceKey != ev.resourceKey || state != "pending" || tenantID != "acme" || !mustLand || !idempotent {
				t.Fatalf("the message of event %s is %s %s %s %s %s %s %s %t %t", ev.eventID, destination, kind, principal, origin,
					resourceKey, state, tenantID, mustLand, idempotent)
			}
		}
		// A second group reads the same records again, as a redelivery after a crash before the commit does.
		e.run(t, e.inbox(t, topic, "group-b", week), topic, "group-b", 3)
		if n := e.count(t, "SELECT count(*) FROM stego_outbox.messages WHERE destination = $1", sub.Name); n != 3 {
			t.Fatalf("after a second read the outbox holds %d messages, want 3", n)
		}
		// The event ID of acme again with another payload is refused, and the first message stays (security review of
		// PR #418, L2).
		reused := change("acme", "orders.order.created", "o-9")
		reused.eventID = events[0].eventID
		e.produce(t, topic, reused)
		e.run(t, e.inbox(t, topic, "group-c", week), topic, "group-c", 4)
		if n := e.count(t, "SELECT count(*) FROM stego_inbox.refused WHERE topic = $1 AND reason = 'event-id-reused' AND event_id = $2", topic, reused.eventID); n != 1 {
			t.Fatalf("the reused event ID gave %d refused rows, want 1", n)
		}
		if n := e.count(t, "SELECT count(*) FROM stego_outbox.messages WHERE id = $1 AND resource_key = 'order:o-1'", MessageID(sub.Name, "acme", events[0].eventID)); n != 1 {
			t.Fatal("the first message of the reused event ID did not stay")
		}
		if a, b := MessageID(sub.Name, "acme", events[0].eventID), MessageID(sub.Name, "other", events[0].eventID); a == b {
			t.Fatal("one event ID gives one message ID for two tenants")
		}
	})

	t.Run("ST-SUP-RP01-LC35-T05 A record whose tenant a Retire retires after the lookup is refused, and its write waits for the Retire", func(t *testing.T) {
		ctx := context.Background()
		// A lookup that is stale: it read stale as active before a Retire of it.
		e.provision(t, "stale", true)
		topic := e.topic(t, 1, "86400000")
		e.produce(t, topic, change("stale", "orders.order.created", "s-1"))
		in := e.inbox(t, topic, "group-stale", week)
		in.tenants = func(ctx context.Context, id string) (tenant.Tenant, Listing, error) {
			return requested(t, id, tenant.ProvisionAction), Active, nil
		}
		e.run(t, in, topic, "group-stale", 1)
		if n := e.count(t, "SELECT count(*) FROM stego_inbox.refused WHERE topic = $1 AND reason = $2", topic, ReasonRetiredTenant); n != 1 {
			t.Fatalf("the record of a tenant retired after the lookup gave %d refused rows, want 1", n)
		}

		// A Retire that runs while the record is read: the write waits for the Retire to commit, then refuses.
		e.provision(t, "late", false)
		entered, release, retired := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			retired <- tenant.Retire(ctx, e.runtime, "public", requested(t, "late", tenant.RetireAction), []tenant.Hook{
				func(context.Context, tenant.Querier, tenant.Tenant) error { close(entered); <-release; return nil }})
		}()
		<-entered
		topic = e.topic(t, 1, "86400000")
		late := change("late", "orders.order.created", "l-1")
		e.produce(t, topic, late)
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- e.inbox(t, topic, "group-late", week).Run(runCtx) }()
		// The write waits on the lock of the tenant list, which the Retire holds; a write that does not wait commits
		// the offset first.
		for {
			if e.committed(t, topic, "group-late") > 0 {
				close(release)
				cancel()
				t.Fatal("the inbox wrote the record of a tenant while a Retire of it ran")
			}
			if e.count(t, "SELECT count(*) FROM pg_catalog.pg_locks WHERE locktype = 'advisory' AND NOT granted") > 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		close(release)
		if err := <-retired; err != nil {
			t.Fatal(err)
		}
		for e.committed(t, topic, "group-late") < 1 {
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if n := e.count(t, "SELECT count(*) FROM stego_outbox.messages WHERE id = $1", MessageID(sub.Name, "late", late.eventID)); n != 0 {
			t.Fatal("the record of a tenant retired while it was read is in the outbox")
		}
		if n := e.count(t, "SELECT count(*) FROM stego_inbox.refused WHERE topic = $1 AND reason = $2", topic, ReasonRetiredTenant); n != 1 {
			t.Fatalf("the record of a tenant retired while it was read gave %d refused rows, want 1", n)
		}
	})

	t.Run("ST-SUP-RP01-LC36-T01 A record that fails a check is refused with its fixed reason, a kind that the subscription does not take is skipped, and every offset is committed", func(t *testing.T) {
		before := e.count(t, "SELECT count(*) FROM stego_outbox.messages")
		topic := e.topic(t, 1, "86400000")
		badKey := "acme\x1forder:x"
		cases := []struct {
			ev     event
			reason string
		}{
			{change("nobody", "orders.order.created", "u-1"), ReasonUnknownTenant},
			{change("gone", "orders.order.created", "r-1"), ReasonRetiredTenant},
			{change("acme", "billing.invoice.created", "f-1"), ReasonForeignKind},
			{func() event {
				ev := change("acme", "orders.order.created", "b-1")
				ev.eventID = "not-a-uuid"
				return ev
			}(), ReasonBadEventID},
			{func() event {
				ev := change("acme", "orders.order.created", "m-1")
				ev.drop = kafkaoutbox.HeaderTenant
				return ev
			}(), ReasonMissingHeader},
			{func() event { ev := change("other", "orders.order.created", "k-1"); ev.key = &badKey; return ev }(), ReasonKeyMismatch},
			{func() event { ev := change("acme", "orders.order.created", "j-1"); ev.value = "{not json"; return ev }(), ReasonNotJSON},
			{func() event {
				ev := change("acme", "orders.order.created", "c-1")
				ev.contentType = "text/plain"
				return ev
			}(), ReasonBadContentType},
			{func() event { ev := change("ac\x01me", "orders.order.created", "t-1"); return ev }(), ReasonBadTenant},
			{func() event {
				ev := change("acme", "orders.order.created", "x-1")
				ev.value = `{"entity":"Invoice","action":"created","id":"x-1"}`
				return ev
			}(), ReasonKindMismatch},
			{func() event {
				ev := change("acme", "orders.order.created", "d-1")
				ev.extra = []kgo.RecordHeader{{Key: kafkaoutbox.HeaderTenant, Value: []byte("acme")}}
				return ev
			}(), ReasonDuplicateHeader},
			// The key names another row than the payload (security review of PR #418, N4).
			{func() event {
				ev := change("acme", "orders.order.created", "w-1")
				ev.resourceKey = "order:z-9"
				return ev
			}(), ReasonKeyMismatch},
			// The payload names its row once and in the exact case; the key is exactly <type>:<row ID> (security review
			// of PR #421, L5).
			{func() event {
				ev := change("acme", "orders.order.created", "v-2")
				ev.value = `{"entity":"Order","action":"created","id":"v-1","ID":"v-2"}`
				return ev
			}(), ReasonKindMismatch},
			{func() event {
				ev := change("acme", "orders.order.created", "v-3")
				ev.value = `{"entity":"Order","action":"created","id":"v-4","id":"v-3"}`
				return ev
			}(), ReasonKindMismatch},
			{func() event {
				ev := change("acme", "orders.order.created", "v-5")
				ev.resourceKey = "Bad Type:v-5"
				return ev
			}(), ReasonKeyMismatch},
			{func() event {
				ev := change("acme", "orders.order.created", "v-6")
				ev.resourceKey = "order:x:v-6"
				return ev
			}(), ReasonKeyMismatch},
		}
		var events []event
		for _, c := range cases {
			events = append(events, c.ev)
		}
		skipped := change("acme", "orders.order.updated", "s-1")
		events = append(events, skipped)
		e.produce(t, topic, events...)
		e.run(t, e.inbox(t, topic, "group-refuse", week), topic, "group-refuse", int64(len(events)))
		if n := e.count(t, "SELECT count(*) FROM stego_outbox.messages"); n != before {
			t.Fatalf("the refused and skipped records wrote %d outbox messages, want none", n-before)
		}
		for i, c := range cases {
			var reason string
			var tenantID, eventID sql.NullString
			if err := e.admin.QueryRowContext(context.Background(), `SELECT reason, tenant_id, event_id::text FROM stego_inbox.refused
				WHERE subscription = $1 AND topic = $2 AND record_offset = $3`, sub.Name, topic, i).Scan(&reason, &tenantID, &eventID); err != nil {
				t.Fatalf("the refused record at offset %d (%s): %v", i, c.reason, err)
			}
			if reason != c.reason {
				t.Fatalf("the record at offset %d was refused for %s, want %s", i, reason, c.reason)
			}
			// A refused row holds no header text that failed its form check.
			if c.reason == ReasonBadEventID && eventID.Valid || c.reason == ReasonBadTenant && tenantID.Valid {
				t.Fatalf("the refused row of %s holds the text that failed its check", c.reason)
			}
		}
		if n := e.count(t, "SELECT count(*) FROM stego_inbox.refused WHERE topic = $1", topic); n != len(cases) {
			t.Fatalf("%d refused rows, want %d: the skipped kind is no refusal", n, len(cases))
		}
		// A second read writes no second refused row.
		e.run(t, e.inbox(t, topic, "group-refuse-2", week), topic, "group-refuse-2", int64(len(events)))
		if n := e.count(t, "SELECT count(*) FROM stego_inbox.refused WHERE topic = $1", topic); n != len(cases) {
			t.Fatalf("after a second read %d refused rows, want %d", n, len(cases))
		}
	})

	t.Run("ST-SUP-RP01-LC35-T02 The inbox does not start on a topic that keeps its records longer than the outbox keeps their IDs", func(t *testing.T) {
		topic := e.topic(t, 1, "2592000000")
		in := e.inbox(t, topic, "group-retention", week)
		if err := in.Run(context.Background()); !errors.Is(err, ErrRetention) || in.Failed() != RetentionStage {
			t.Fatalf("Run on a topic of 30 days with a done retention of 7 days gave %v at the stage %q, want ErrRetention at %s", err, in.Failed(), RetentionStage)
		}
		infinite := e.topic(t, 1, "-1")
		if err := e.inbox(t, infinite, "group-infinite", week).Run(context.Background()); !errors.Is(err, ErrRetention) ||
			!strings.Contains(err.Error(), InfiniteRetentionSetting) {
			t.Fatalf("Run on a topic of infinite retention gave %v, want ErrRetention that names %s", err, InfiniteRetentionSetting)
		}
		// A compacted topic keeps the last record of each key without end (security review of PR #418, M1).
		compact := e.topic(t, 1, "86400000", [2]string{"cleanup.policy", "compact,delete"})
		if err := e.inbox(t, compact, "group-compact", week).Run(context.Background()); !errors.Is(err, ErrRetention) ||
			!strings.Contains(err.Error(), "compacted") {
			t.Fatalf("Run on a compacted topic gave %v, want ErrRetention that names the compaction", err)
		}
		// A broker that gives no cleanup.policy, or one with no value, is refused as for retention.ms (security review
		// of PR #418, N3).
		config := func(name string, value *string) kmsg.DescribeConfigsResponseResourceConfig {
			c := kmsg.NewDescribeConfigsResponseResourceConfig()
			c.Name, c.Value = name, value
			return c
		}
		day, policy := "86400000", "delete"
		for name, configs := range map[string][]kmsg.DescribeConfigsResponseResourceConfig{
			"no cleanup.policy":            {config("retention.ms", &day)},
			"a cleanup.policy of no value": {config("retention.ms", &day), config("cleanup.policy", nil)},
			"no retention.ms":              {config("cleanup.policy", &policy)},
		} {
			if _, _, err := topicRetention(configs); err == nil {
				t.Fatalf("the configs with %s were taken", name)
			}
		}
		if ms, got, err := topicRetention([]kmsg.DescribeConfigsResponseResourceConfig{config("retention.ms", &day), config("cleanup.policy", &policy)}); err != nil || ms != 86400000 || got != "delete" {
			t.Fatalf("the full configs gave %d %q %v", ms, got, err)
		}
		// A topic that becomes compacted after the inbox started stops the inbox at the next check of the session,
		// with no new session (security review of PR #418, N1).
		changing := e.topic(t, 1, "86400000")
		clock := newHandClock()
		watcher := e.inbox(t, changing, "group-recheck", week)
		watcher.now = clock.now
		e.produce(t, changing, change("acme", "orders.order.created", "q-1"))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- watcher.Run(ctx) }()
		for e.committed(t, changing, "group-recheck") < 1 {
			select {
			case err := <-done:
				t.Fatalf("the inbox stopped before it read the topic: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
		}
		e.alter(t, changing, "cleanup.policy", "compact")
		clock.move(2 * retentionRecheck)
		if err := <-done; !errors.Is(err, ErrRetention) || watcher.Failed() != RetentionStage {
			t.Fatalf("the inbox on a topic that became compacted gave %v at the stage %q, want ErrRetention at %s", err, watcher.Failed(), RetentionStage)
		}
		for _, c := range []struct {
			ms     int64
			policy string
			accept bool
			fits   bool
		}{{604800000, "delete", false, true}, {604800001, "delete", false, false}, {-1, "delete", false, false}, {-1, "delete", true, true},
			{1000, "delete", false, true}, {1000, "compact", false, false}, {1000, "delete, compact", false, false}, {1000, "compact", true, true}} {
			if err := RetentionFits(c.ms, c.policy, week, c.accept); (err == nil) != c.fits {
				t.Fatalf("RetentionFits(%d, %s, 7d, %t) = %v, want fits %t", c.ms, c.policy, c.accept, err, c.fits)
			}
		}
		// An accepted infinite retention is logged once, at info (security review of PR #418, L5).
		logs := &recorder{}
		accepting := e.inbox(t, infinite, "group-accept", week)
		accepting.settings.AcceptInfiniteRetention = true
		accepting.logger = logs.Logger("")
		e.produce(t, infinite, change("acme", "orders.order.created", "a-1"))
		e.run(t, accepting, infinite, "group-accept", 1)
		if n := logs.count("kafka.consumer.retention_accepted"); n != 1 {
			t.Fatalf("the accepted infinite retention gave %d info events, want 1", n)
		}
	})

	t.Run("ST-SUP-RP01-LC37-T01 The worker role writes, reads and purges the refused records, changes none, and the purge removes only the old ones", func(t *testing.T) {
		ctx := context.Background()
		if _, err := e.admin.ExecContext(ctx, `INSERT INTO stego_inbox.refused (subscription, topic, partition, record_offset, reason, refused_at)
			VALUES ('order-changes', 'purge-topic', 0, 1, 'unknown-tenant', now() - interval '31 days'),
			       ('order-changes', 'purge-topic', 0, 2, 'unknown-tenant', now() - interval '29 days')`); err != nil {
			t.Fatal(err)
		}
		in := e.inbox(t, "purge-topic", "group-purge", week)
		n, err := in.Purge(ctx, 100)
		if err != nil || n != 1 {
			t.Fatalf("Purge removed %d rows with %v, want the one row older than 30 days", n, err)
		}
		if left := e.count(t, "SELECT count(*) FROM stego_inbox.refused WHERE topic = 'purge-topic'"); left != 1 {
			t.Fatalf("%d rows left, want 1", left)
		}
		if _, err := e.worker.ExecContext(ctx, "UPDATE stego_inbox.refused SET reason = 'bad-kind'"); err == nil {
			t.Fatal("the worker role changed a refused record")
		}
		if _, err := e.runtime.ExecContext(ctx, "SELECT 1 FROM stego_inbox.refused"); err == nil {
			t.Fatal("the runtime role read the refused records")
		}
		// A table of an earlier release holds the unnamed CHECK of a shorter list of reasons: the migration drops it
		// and adds the CHECK of this list, by its name, and a run again changes nothing (security review of PR #418b, N2).
		if _, err := e.admin.ExecContext(ctx, `ALTER TABLE stego_inbox.refused DROP CONSTRAINT `+reasonsCheck()+`;
			ALTER TABLE stego_inbox.refused ADD CONSTRAINT refused_reason_check CHECK (reason IN ('unknown-tenant', 'bad-kind')) NOT VALID`); err != nil {
			t.Fatal(err)
		}
		insert := func(offset int, reason string) error {
			_, err := e.admin.ExecContext(ctx, `INSERT INTO stego_inbox.refused (subscription, topic, partition, record_offset, reason)
				VALUES ('order-changes', 'check-topic', 0, $1, $2)`, offset, reason)
			return err
		}
		if err := insert(1, ReasonEventIDReused); err == nil {
			t.Fatal("the CHECK of the earlier list took a reason that it does not hold")
		}
		migration, err := Migration(e.workerRole)
		if err != nil {
			t.Fatal(err)
		}
		oid := func() (oids []string) {
			rows, err := e.admin.QueryContext(ctx, `SELECT conname || ':' || oid::text FROM pg_catalog.pg_constraint
				WHERE conrelid = 'stego_inbox.refused'::regclass AND contype = 'c' AND conname LIKE 'refused_reason%' ORDER BY conname`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				var o string
				if err := rows.Scan(&o); err != nil {
					t.Fatal(err)
				}
				oids = append(oids, o)
			}
			return oids
		}
		for range 2 {
			if _, err := e.admin.ExecContext(ctx, migration); err != nil {
				t.Fatal(err)
			}
		}
		after := oid()
		if len(after) != 1 || !strings.HasPrefix(after[0], reasonsCheck()+":") {
			t.Fatalf("after the migration the reasons CHECKs are %v, want only %s", after, reasonsCheck())
		}
		if err := insert(1, ReasonEventIDReused); err != nil {
			t.Fatalf("the CHECK of this list refused the reason %s: %v", ReasonEventIDReused, err)
		}
		if err := insert(2, "no-such-reason"); err == nil {
			t.Fatal("the CHECK of this list took a reason that is not one of its reasons")
		}
		if _, err := e.admin.ExecContext(ctx, migration); err != nil || !slices.Equal(oid(), after) {
			t.Fatalf("a run again changed the CHECK: %v, %v (%v)", oid(), after, err)
		}
		text, err := Migration(strings.TrimPrefix("x", "x") + "ki_worker_again")
		if err != nil || !strings.Contains(text, "GRANT SELECT, INSERT, DELETE ON TABLE stego_inbox.refused") {
			t.Fatalf("Migration gave %v", err)
		}
		if _, err := Migration("Bad-Role"); err == nil {
			t.Fatal("Migration took a role name that is not a lower-case SQL name")
		}
	})
}

func TestSettings(t *testing.T) {
	t.Run("ST-SUP-RP01-LC38-T01 The inbox reads its topic and group by fixed names, defaults the group, and refuses a setting by its name", func(t *testing.T) {
		t.Setenv(kafkaoutbox.BrokersSetting, "127.0.0.1:9092")
		t.Setenv(kafkaoutbox.InsecureLoopbackSetting, "1")
		t.Setenv(kafkaoutbox.TopicSetting, "own-events")
		t.Setenv(TopicSetting, "")
		if _, err := ReadSettings("fulfilment", "order-changes"); err == nil || !strings.Contains(err.Error(), TopicSetting) {
			t.Fatalf("ReadSettings with no topic gave %v, want a refusal that names %s", err, TopicSetting)
		}
		t.Setenv(TopicSetting, "orders-events")
		s, err := ReadSettings("fulfilment", "order-changes")
		if err != nil {
			t.Fatal(err)
		}
		if s.Topic != "orders-events" || s.Group != "fulfilment.order-changes" || s.Connection.Topic != "" || s.AcceptInfiniteRetention {
			t.Fatalf("ReadSettings gave %+v", s)
		}
		t.Setenv(GroupSetting, "platform.fulfilment")
		t.Setenv(InfiniteRetentionSetting, "1")
		if s, err = ReadSettings("fulfilment", "order-changes"); err != nil || s.Group != "platform.fulfilment" || !s.AcceptInfiniteRetention {
			t.Fatalf("ReadSettings gave %+v, %v", s, err)
		}
		t.Setenv(InfiniteRetentionSetting, "yes")
		if _, err := ReadSettings("fulfilment", "order-changes"); err == nil || !strings.Contains(err.Error(), InfiniteRetentionSetting) {
			t.Fatalf("ReadSettings gave %v, want a refusal that names %s", err, InfiniteRetentionSetting)
		}
		t.Setenv(InfiniteRetentionSetting, "")
		t.Setenv(TopicSetting, "__consumer_offsets")
		if _, err := ReadSettings("fulfilment", "order-changes"); err == nil || strings.Contains(err.Error(), "__consumer") {
			t.Fatalf("ReadSettings of an internal topic gave %v, want a refusal with no value", err)
		}
	})
}

func TestSubscription(t *testing.T) {
	t.Run("ST-SUP-RP01-LC36-T02 New refuses a subscription whose kinds are not outbox kinds of its source", func(t *testing.T) {
		db, err := sql.Open("pgx", "postgres://127.0.0.1:1/none")
		if err != nil {
			t.Fatal(err)
		}
		queue, err := outbox.New(db, outbox.Options{})
		if err != nil {
			t.Fatal(err)
		}
		settings := Settings{Connection: kafkaoutbox.Settings{Brokers: []string{"127.0.0.1:9092"}, InsecureLoopback: true}, Topic: "t", Group: "g"}
		lookup := func(context.Context, string) (tenant.Tenant, Listing, error) {
			return tenant.Tenant{}, NotListed, nil
		}
		for _, s := range []Subscription{
			{Service: "a", Name: "b", Source: "orders", Worker: "w", Events: nil},
			{Service: "a", Name: "b", Source: "orders", Worker: "w", Events: []string{"billing.invoice.created"}},
			{Service: "a", Name: "b", Source: "orders", Worker: "w", Events: []string{"orders.order.created", "orders.order.created"}},
			{Service: "A", Name: "b", Source: "orders", Worker: "w", Events: []string{"orders.order.created"}},
		} {
			if _, err := New(Config{Subscription: s, Settings: settings, Queue: queue, DB: db, Tenants: lookup, TenantSchema: "public", Retention: time.Hour}); err == nil {
				t.Fatalf("New took the subscription %+v", s)
			}
		}
		good := Subscription{Service: "a", Name: "b", Source: "orders", Worker: "w", Events: []string{"orders.order.created"}}
		in, err := New(Config{Subscription: good, Settings: settings, Queue: queue, DB: db, Tenants: lookup, TenantSchema: "public", Retention: time.Hour})
		if err != nil || in.Name() != "inbox-b" {
			t.Fatalf("New gave %v", err)
		}
	})
}

// debugOptions logs the client of the inbox when STEGO_TEST_KAFKA_DEBUG=1.
func debugOptions() []kgo.Opt {
	if os.Getenv("STEGO_TEST_KAFKA_DEBUG") != "1" {
		return nil
	}
	return []kgo.Opt{kgo.WithLogger(kgo.BasicLogger(os.Stderr, kgo.LogLevelInfo, nil))}
}

func TestTenantList(t *testing.T) {
	t.Run("ST-SUP-RP01-LC35-T04 The lookup builds the tenant from the entry of the list and tells active, retired and not listed", func(t *testing.T) {
		e := newEnv(t)
		e.provision(t, "t-active", false)
		e.provision(t, "t-retired", true)
		ctx := context.Background()
		lookup := e.lookup(t)
		for _, c := range []struct {
			id, tenant string
			want       Listing
		}{
			{"t-active", "t-active", Active}, {"t-retired", "t-retired", Retired}, {"t-unknown", "", NotListed},
			{"bad\x1fid", "", NotListed}, {"", "", NotListed}, {strings.Repeat("a", 256), "", NotListed},
		} {
			got, listing, err := lookup(ctx, c.id)
			if err != nil || listing != c.want || got.ID() != c.tenant {
				t.Fatalf("the lookup of %q gave %q, %v, %v; want %q and %v", c.id, got.ID(), listing, err, c.tenant, c.want)
			}
		}
		if _, err := TenantList(nil, "public"); err == nil {
			t.Fatal("TenantList took no database")
		}
		quoted, err := TenantList(e.runtime, `no"such`)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := quoted(ctx, "t-active"); err == nil {
			t.Fatal("the lookup in a schema that does not exist gave no error")
		}
	})
}

// recorder is a log provider that counts the events of each name.
type recorder struct {
	noop.LoggerProvider
	mu     sync.Mutex
	events []otellog.Record
}

func (r *recorder) Logger(string, ...otellog.LoggerOption) otellog.Logger { return recordLogger{r: r} }

func (r *recorder) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.EventName() == name {
			n++
		}
	}
	return n
}

// suppressed gives the attribute suppressed of the last event of name, or -1.
func (r *recorder) suppressed(name string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	value := int64(-1)
	for _, e := range r.events {
		if e.EventName() == name {
			e.WalkAttributes(func(kv attribute.KeyValue) bool {
				if kv.Key == "suppressed" {
					value = kv.Value.AsInt64()
				}
				return true
			})
		}
	}
	return value
}

type recordLogger struct {
	noop.Logger
	r *recorder
}

func (l recordLogger) Emit(_ context.Context, record otellog.Record) {
	l.r.mu.Lock()
	l.r.events = append(l.r.events, record.Clone())
	l.r.mu.Unlock()
}

func TestAlerts(t *testing.T) {
	t.Run("ST-SUP-RP01-LC36-T03 The warn event of a refusal reason comes at most once in 10 seconds, with the number it held back", func(t *testing.T) {
		in := &Inbox{sub: sub, alerts: map[string]*alertState{}}
		logs := &recorder{}
		in.logger = logs.Logger("")
		at := time.Unix(1_800_000_000, 0)
		in.now = func() time.Time { return at }
		for i := range 3 {
			in.alert(context.Background(), ReasonUnknownTenant, 0, int64(i))
		}
		in.alert(context.Background(), ReasonBadKind, 0, 9)
		if n := logs.count("kafka.consumer.refused"); n != 2 {
			t.Fatalf("four refusals of two reasons in one instant gave %d events, want 2", n)
		}
		at = at.Add(alertEvery)
		in.alert(context.Background(), ReasonUnknownTenant, 0, 3)
		if n, held := logs.count("kafka.consumer.refused"), logs.suppressed("kafka.consumer.refused"); n != 3 || held != 2 {
			t.Fatalf("after 10 s the events are %d with %d held back, want 3 with 2", n, held)
		}
	})
}

func TestFailureClass(t *testing.T) {
	t.Run("ST-SUP-RP01-LC36-T05 The failure class names the operation that failed at the broker, and no error text", func(t *testing.T) {
		secret := errors.New("dial tcp 10.0.0.7:9093: the topic secret-orders refused alice")
		cases := []struct {
			err  error
			want string
		}{
			{&opError{op: opDescribeConfigs, err: kerr.TopicAuthorizationFailed}, "broker:TOPIC_AUTHORIZATION_FAILED (describe-configs)"},
			{&opError{op: opFetch, err: fmt.Errorf("fetch of a partition: %w", kerr.TopicAuthorizationFailed)}, "broker:TOPIC_AUTHORIZATION_FAILED (fetch)"},
			{&opError{op: opFetch, err: kerr.GroupAuthorizationFailed}, "broker:GROUP_AUTHORIZATION_FAILED (fetch)"},
			{&opError{op: opCommit, err: fmt.Errorf("%w: %w", errCommit, kerr.GroupAuthorizationFailed)}, "broker:GROUP_AUTHORIZATION_FAILED (commit)"},
			{&opError{op: opCommit, err: fmt.Errorf("%w: %w", errCommit, secret)}, "broker (commit)"},
			{&opError{op: opDescribeConfigs, err: secret}, "broker (describe-configs)"},
			{fmt.Errorf("%w: %w", errDatabase, secret), "database"},
			{secret, "broker"},
		}
		for _, c := range cases {
			if got := failureClass(c.err); got != c.want {
				t.Errorf("the class of %v is %q, want %q", c.err, got, c.want)
			}
		}
	})
}

func TestFailing(t *testing.T) {
	t.Run("ST-SUP-RP01-LC36-T04 Sessions that fail in a row make the inbox report its failure class and the gauge 1, and a session that reads clears both", func(t *testing.T) {
		e := newEnv(t)
		reader := sdkmetric.NewManualReader()
		meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		name := "orders-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
		in, err := New(Config{Subscription: sub, Queue: e.queue, DB: e.worker, Retention: 7 * 24 * time.Hour, Tenants: e.lookup(t), TenantSchema: "public",
			Settings: Settings{Connection: kafkaoutbox.Settings{Brokers: []string{e.broker.Plain.Addr()}, InsecureLoopback: true},
				Topic: name, Group: "group-failing"}, Meter: meter, FailingAfter: 2})
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var attempts []int
		in.backoff = func(attempt int) time.Duration {
			mu.Lock()
			defer mu.Unlock()
			attempts = append(attempts, attempt)
			return 10 * time.Millisecond
		}
		clock := newHandClock()
		in.now = clock.now
		gauge := func() int64 {
			var data metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &data); err != nil {
				t.Fatal(err)
			}
			for _, scope := range data.ScopeMetrics {
				for _, m := range scope.Metrics {
					if g, ok := m.Data.(metricdata.Gauge[int64]); ok && m.Name == "kafka.consumer.failing" && len(g.DataPoints) == 1 {
						return g.DataPoints[0].Value
					}
				}
			}
			return -1
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- in.Run(ctx) }()
		defer func() { cancel(); <-done }()
		// The topic does not exist yet: each session fails at the read of its retention.
		for in.Failing() == "" {
			time.Sleep(20 * time.Millisecond)
		}
		if class := in.Failing(); class != "broker:UNKNOWN_TOPIC_OR_PARTITION (describe-configs)" || gauge() != 1 {
			t.Fatalf("the failing inbox gives the class %q and the gauge %d", class, gauge())
		}
		e.topicNamed(t, name)
		for in.Failing() != "" {
			time.Sleep(20 * time.Millisecond)
		}
		if g := gauge(); g != 0 {
			t.Fatalf("the gauge is %d after the inbox reads the topic, want 0", g)
		}
		// A broker that goes away after the inbox read the topic fails the session, and the inbox reports its failure.
		// The session read before it failed, so the backoff starts again at the first attempt (security review of PR
		// #418, N5).
		mu.Lock()
		before := len(attempts)
		mu.Unlock()
		e.broker.Plain.Down()
		defer e.broker.Plain.Up()
		clock.move(2 * retentionRecheck)
		for in.Failing() == "" {
			time.Sleep(20 * time.Millisecond)
		}
		mu.Lock()
		first := attempts[before]
		mu.Unlock()
		if first != 1 {
			t.Fatalf("the backoff after a session that read starts at the attempt %d, want 1", first)
		}
	})
}
