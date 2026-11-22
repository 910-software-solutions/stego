package probe

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/otel"
)

// The schema backfill observations are what a backfill of the lease runtime takes.
var _ lease.Observer = Backfill{}

func TestProbeBuiltins(t *testing.T) {
	t.Run("ST-SUP-RP12-LC21-T01 The service events are built-in observations on the probe's sinks", func(t *testing.T) {
		c := startCollector(t)
		stdout := &lines{}
		r := newProbe(t, newTelemetry(t), "", stdout)
		r.Service(context.Background(), ServiceReady)
		r.Service(context.Background(), ServiceFailed)
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"service.ready": "INFO", "service.failed": "ERROR"}
		got := stdout.all()
		if len(got) != 2 {
			t.Fatalf("the console holds %q, want two lines", got)
		}
		for _, line := range got {
			var decoded map[string]any
			if err := json.Unmarshal([]byte(line), &decoded); err != nil {
				t.Fatal(err)
			}
			name, _ := decoded["event.name"].(string)
			if want[name] == "" || decoded["severity"] != want[name] || decoded["attributes"] != nil {
				t.Fatalf("the console line is %q", line)
			}
		}
		records := c.logRecords()
		if len(records) != 2 {
			t.Fatalf("the collector holds %d log records, want 2", len(records))
		}
		for _, record := range records {
			if want[record.EventName] != record.SeverityText || len(record.Attributes) != 0 {
				t.Fatalf("the log record is %s %s with %v", record.EventName, record.SeverityText, attributes(record.Attributes))
			}
		}
		_, err := Declare(Spec{Name: "service.ready", Kind: Event, Severity: Info, EmittedBy: []string{command}})
		if err == nil || !strings.Contains(err.Error(), "service.ready") || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("the declaration of service.ready returned %v, want a refusal by name", err)
		}
		// The worker observations carry the worker name, the fixed outcome and the fixed cause class only (ST-SUP-RC35-OC06).
		builtins, err := NewBuiltins([]string{"sync-partner-orders"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		recorder := NewRecorder(builtins)
		worker := recorder.Worker("sync-partner-orders")
		worker.RunFailed(context.Background(), WorkerFenced)
		worker.RunFailedCause(context.Background(), WorkerUndecided, "undecided")
		worker.RunFailedCause(context.Background(), WorkerRolledBack, "a cause that is not fixed")
		worker.TickSkipped()
		worker.TenantUnreached()
		recorder.Worker("undeclared").TickSkipped()
		var kept []string
		for _, e := range recorder.All() {
			kept = append(kept, e.Name+" "+e.Severity.String()+" "+e.String())
		}
		if !slices.Equal(kept, []string{
			"worker.run.failed ERROR worker.run.failed{worker=sync-partner-orders outcome=fenced cause=unknown}",
			"worker.run.failed ERROR worker.run.failed{worker=sync-partner-orders outcome=undecided cause=undecided}",
			"worker.tick.skipped WARN worker.tick.skipped{worker=sync-partner-orders}",
			"worker.tenant.unreached WARN worker.tenant.unreached{worker=sync-partner-orders}",
		}) || recorder.Dropped() != 2 {
			t.Fatalf("the recorder holds %v with %d drops", kept, recorder.Dropped())
		}
	})

	t.Run("ST-SUP-RP12-LC22-T01 Built-in service events are on the console in every setting", func(t *testing.T) {
		if mode := os.Getenv(childSetting); mode != "" && strings.HasPrefix(mode, "service") {
			var c *collector
			if mode == "service-collector" {
				c = startCollector(t)
			}
			telemetry, err := otel.New("orders", SpanNames()...)
			if err != nil {
				os.Exit(2)
			}
			r, err := Start(telemetry, nil)
			if err != nil {
				os.Exit(3)
			}
			r.Service(unsampled(), ServiceReady)
			r.Close(context.Background())
			if c != nil {
				count := 0
				for _, record := range c.logRecords() {
					if record.EventName == "service.ready" {
						count++
					}
				}
				os.WriteFile(os.Getenv("STEGO_PROBE_TEST_RESULT"), []byte(strconv.Itoa(count)), 0o600)
			}
			os.Exit(0)
		}
		result := filepath.Join(t.TempDir(), "result")
		sc := unsampledSpanContext()
		var form []string
		for _, settings := range [][]string{
			{childSetting + "=service"},
			{childSetting + "=service", consoleSetting + "=off"},
			{childSetting + "=service-collector", "STEGO_PROBE_TEST_RESULT=" + result},
			{childSetting + "=service", consoleSetting + "=logfmt"},
		} {
			stdout, _ := runChild(t, "^TestProbeBuiltins$/^ST-SUP-RP12-LC22-T01_", settings...)
			lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
			if len(lines) != 2 || !strings.HasPrefix(lines[1], `{"timestamp":"`) || !strings.Contains(lines[1], `"event.name":"telemetry.runtime.stopped"`) || strings.Contains(lines[1], "tenant") {
				t.Fatalf("%v: the child wrote %q, want the line of service.ready and the JSON line of telemetry.runtime.stopped", settings, stdout)
			}
			var decoded map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
				t.Fatalf("%v: the line %q is not JSON: %v", settings, lines[0], err)
			}
			var keys []string
			for k := range decoded {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			if form == nil {
				form = keys
			}
			if !slices.Equal(keys, form) || !slices.Equal(keys, []string{"event.name", "message", "service.instance.id", "service.name", "severity", "span_id", "timestamp", "trace_id"}) {
				t.Fatalf("%v: the line has the keys %v, want the fixed form %v", settings, keys, form)
			}
			if decoded["event.name"] != "service.ready" || decoded["trace_id"] != sc.TraceID().String() {
				t.Fatalf("%v: the line is %q", settings, lines[0])
			}
		}
		if got, err := os.ReadFile(result); err != nil || string(got) != "1" {
			t.Fatalf("the collector got %q log records service.ready (%v), want 1", got, err)
		}
	})

	t.Run("ST-SUP-RP12-LC22-T01 A full queue of declared observations does not drop a built-in event", func(t *testing.T) {
		clearEnvironment(t)
		event := reservationRefused(t, CountsAttempt)
		stdout := &lines{}
		r := newProbe(t, newTelemetry(t), "json", stdout)
		release := stdout.hold()
		emitCalls(r, event, 1)
		stdout.waitEntered(t)
		emitCalls(r, event, 2*QueueLimit)
		if r.Delivery().ConsoleDropped == 0 {
			t.Fatal("the queue of declared observations is not full")
		}
		r.Service(context.Background(), ServiceReady)
		release()
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := stdout.everything()
		if len(got) != QueueLimit+3 || !strings.Contains(strings.Join(got, ""), `"event.name":"service.ready"`) || !strings.Contains(got[len(got)-1], `"event.name":"telemetry.runtime.stopped"`) {
			t.Fatalf("the console holds %d lines, want %d with service.ready and telemetry.runtime.stopped last", len(got), QueueLimit+3)
		}
	})

	t.Run("ST-SUP-RP12-LC21-T03 A run that committed is the Info worker.run.completed with the worker name, which the console writes from the level of STEGO_LOG_LEVEL", func(t *testing.T) {
		clearEnvironment(t)
		builtins, err := NewBuiltins([]string{"ship-orders"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			level                 string
			completed, failed, ok int
		}{{"", 1, 1, 1}, {"debug", 1, 1, 1}, {"warn", 0, 1, 1}, {"error", 0, 1, 1}} {
			level, err := consoleLevel(c.level)
			if err != nil {
				t.Fatal(err)
			}
			stdout := &lines{}
			r, err := start(newTelemetry(t), builtins, "off", stdout)
			if err != nil {
				t.Fatal(err)
			}
			r.console.level = level
			worker := r.Worker("ship-orders")
			worker.RunCompleted(context.Background())
			worker.RunFailedCause(context.Background(), WorkerRolledBack, "fill_error")
			// A worker name that the Builtins do not hold is dropped.
			r.Worker("t-0198a1b2-private").RunCompleted(context.Background())
			r.Service(context.Background(), ServiceReady)
			if err := r.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := map[string]int{}
			for _, line := range stdout.everything() {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				event, _ := record["event.name"].(string)
				got[event]++
				if event == "worker.run.completed" && (record["severity"] != "INFO" || !strings.Contains(line, `"ship-orders"`) || strings.Contains(line, "tenant")) {
					t.Fatalf("the line of a run that committed is %s, want INFO with the worker name only", line)
				}
				if strings.Contains(line, "private") {
					t.Fatalf("a console line holds a name that the Builtins do not hold: %s", line)
				}
			}
			if got["worker.run.completed"] != c.completed || got["worker.run.failed"] != c.failed || got["service.ready"] != c.ok {
				t.Fatalf("STEGO_LOG_LEVEL=%q gave the lines %v, want %d worker.run.completed, %d worker.run.failed and %d service.ready", c.level, got, c.completed, c.failed, c.ok)
			}
		}
		t.Setenv("STEGO_LOG_LEVEL", "verbose-private")
		if _, err := Start(newTelemetry(t), builtins); err == nil || strings.Contains(err.Error(), "verbose-private") {
			t.Fatalf("Start with a value of STEGO_LOG_LEVEL that is not a level gave %v, want a refusal that does not hold the value", err)
		}
	})

	t.Run("ST-SUP-RP12-LC21-T04 A burst of runs that commit leaves the places of worker.run.failed and the service events", func(t *testing.T) {
		clearEnvironment(t)
		builtins, err := NewBuiltins([]string{"ship-orders"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		stdout := &lines{}
		r, err := start(newTelemetry(t), builtins, "off", stdout)
		if err != nil {
			t.Fatal(err)
		}
		worker := r.Worker("ship-orders")
		release := stdout.hold()
		worker.RunCompleted(context.Background())
		stdout.waitEntered(t)
		// The writer holds one line; the burst fills the queue but its reserve.
		for range 2 * BuiltinQueueLimit {
			worker.RunCompleted(context.Background())
		}
		worker.RunFailedCause(context.Background(), WorkerRolledBack, "fill_error")
		r.Service(context.Background(), ServiceFailed)
		release()
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		completed, failed, service := 0, 0, 0
		for _, line := range stdout.everything() {
			switch {
			case strings.Contains(line, `"event.name":"worker.run.completed"`):
				completed++
			case strings.Contains(line, `"event.name":"worker.run.failed"`):
				failed++
			case strings.Contains(line, `"event.name":"service.failed"`):
				service++
			}
		}
		if failed != 1 || service != 1 || completed != 1+BuiltinQueueLimit-BuiltinReserve || r.console.dropped() == 0 {
			t.Fatalf("the burst gave %d worker.run.completed, %d worker.run.failed, %d service.failed and %d dropped, want %d, 1, 1 and the rest dropped",
				completed, failed, service, r.console.dropped(), 1+BuiltinQueueLimit-BuiltinReserve)
		}
	})

	t.Run("ST-SUP-RP12-LC21-T02 The console writes a built-in worker event at most at the rate limit, and counts the rest", func(t *testing.T) {
		clearEnvironment(t)
		builtins, err := NewBuiltins([]string{"ship-orders"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		stdout := &lines{}
		r, err := start(newTelemetry(t), builtins, "off", stdout)
		if err != nil {
			t.Fatal(err)
		}
		clock := &handClock{at: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
		r.console.mu.Lock()
		r.console.now = clock.now
		r.console.mu.Unlock()
		count := func(event string) (lines, dropped int) {
			for _, line := range stdout.everything() {
				if strings.Contains(line, `"event.name":"`+event+`"`) {
					if strings.Contains(line, `"stego.dropped":`) {
						dropped++
					} else {
						lines++
					}
				}
			}
			return lines, dropped
		}
		wait := func(event string, want int) {
			t.Helper()
			for n, _ := count(event); n < want; n, _ = count(event) {
				time.Sleep(5 * time.Millisecond)
			}
		}
		// A failing consumer sends worker.run.failed for each run (walkthrough 9: 123 lines in a few seconds).
		worker := r.Worker("ship-orders")
		const burst = otel.ConsoleLimit + 5
		for range burst {
			worker.RunFailedCause(context.Background(), WorkerRolledBack, "fill_error")
		}
		// worker.tick.failed has a budget of its own; its line comes after the burst in the one queue.
		worker.TickFailed(context.Background())
		wait("worker.tick.failed", 1)
		// A service event is never held.
		for range burst {
			r.Service(context.Background(), ServiceReady)
		}
		wait("service.ready", burst)
		if n, dropped := count("worker.run.failed"); n != otel.ConsoleLimit || dropped != 0 {
			t.Fatalf("the burst gave %d lines and %d count lines, want the limit %d and no count line yet", n, dropped, otel.ConsoleLimit)
		}
		// The window ends: the next line of the event writes the count of the dropped lines first, as ERROR.
		clock.move(otel.ConsoleWindow)
		worker.RunFailedCause(context.Background(), WorkerRolledBack, "fill_error")
		wait("worker.run.failed", otel.ConsoleLimit+1)
		var countLine map[string]any
		for _, line := range stdout.everything() {
			if strings.Contains(line, `"stego.dropped":`) {
				if err := json.Unmarshal([]byte(line), &countLine); err != nil {
					t.Fatal(err)
				}
			}
		}
		if countLine["event.name"] != "worker.run.failed" || countLine["attributes"].(map[string]any)["stego.dropped"] != float64(burst-otel.ConsoleLimit) || countLine["severity"] != "ERROR" {
			t.Fatalf("the count line is %v, want worker.run.failed, ERROR and %d dropped", countLine, burst-otel.ConsoleLimit)
		}
		// The stop ends each window: a burst after it is counted before the last line.
		for range burst {
			worker.RunFailedCause(context.Background(), WorkerRolledBack, "fill_error")
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := stdout.everything()
		if n, dropped := count("worker.run.failed"); n != 2*otel.ConsoleLimit || dropped != 2 || !strings.Contains(got[len(got)-1], `"event.name":"telemetry.runtime.stopped"`) {
			t.Fatalf("after the stop the console holds %d lines and %d count lines of worker.run.failed, want %d and 2 before telemetry.runtime.stopped", n, dropped, 2*otel.ConsoleLimit)
		}
	})

	t.Run("ST-SUP-RP12-LC21-T05 The count line of a window comes at the end of the window with no later line of its event name", func(t *testing.T) {
		clearEnvironment(t)
		builtins, err := NewBuiltins([]string{"ship-orders"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		stdout := &lines{}
		r, err := start(newTelemetry(t), builtins, "off", stdout)
		if err != nil {
			t.Fatal(err)
		}
		clock := &handClock{at: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
		r.console.mu.Lock()
		r.console.now = clock.now
		r.console.mu.Unlock()
		// The test gives the ticks of the watch itself, so no wall clock bounds it.
		r.console.unwatch(context.Background())
		ticks := make(chan time.Time)
		r.console.watch(ticks, nil)
		worker := r.Worker("ship-orders")
		const burst = otel.ConsoleLimit + 4
		for range burst {
			worker.RunFailedCause(context.Background(), WorkerRolledBack, "fill_error")
		}
		r.Service(context.Background(), ServiceReady)
		for !slices.ContainsFunc(stdout.everything(), func(line string) bool { return strings.Contains(line, `"service.ready"`) }) {
			runtime.Gosched()
		}
		counted := func() string {
			for _, line := range stdout.everything() {
				if strings.Contains(line, `"stego.dropped":`) {
					return line
				}
			}
			return ""
		}
		// A tick in the window writes no count line; a tick after its end writes it, with no later line of
		// worker.run.failed (walkthrough 25, L-2).
		ticks <- clock.now()
		ticks <- clock.now()
		if line := counted(); line != "" {
			t.Fatalf("a tick in the window wrote the count line %q", line)
		}
		clock.move(otel.ConsoleWindow)
		ticks <- clock.now()
		ticks <- clock.now()
		line := counted()
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("after the window the console holds no count line: %v", stdout.everything())
		}
		attrs, _ := decoded["attributes"].(map[string]any)
		if decoded["event.name"] != "worker.run.failed" || decoded["severity"] != "ERROR" || attrs["stego.dropped"] != float64(burst-otel.ConsoleLimit) {
			t.Fatalf("the count line is %q, want worker.run.failed, ERROR and %d dropped", line, burst-otel.ConsoleLimit)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ST-SUP-RP12-LC21-T06 A run that waits for the relations of a row is a WARN worker.run.failed, and each other failed run an ERROR", func(t *testing.T) {
		clearEnvironment(t)
		builtins, err := NewBuiltins([]string{"record-jobs"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		c := startCollector(t)
		stdout := &lines{}
		r, err := start(newTelemetry(t), builtins, "off", stdout)
		if err != nil {
			t.Fatal(err)
		}
		worker := r.Worker("record-jobs")
		worker.RunFailedCause(context.Background(), WorkerUndecided, RelationsPendingCause)
		worker.RunFailedCause(context.Background(), WorkerUndecided, "undecided")
		worker.RunFailedCause(context.Background(), WorkerRolledBack, "fill_error")
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{RelationsPendingCause: "WARN", "undecided": "ERROR", "fill_error": "ERROR"}
		got := map[string]string{}
		for _, line := range stdout.everything() {
			var decoded map[string]any
			if err := json.Unmarshal([]byte(line), &decoded); err != nil || decoded["event.name"] != "worker.run.failed" {
				continue
			}
			attrs, _ := decoded["attributes"].(map[string]any)
			cause, _ := attrs["stego.cause"].(string)
			got[cause], _ = decoded["severity"].(string)
		}
		if !maps.Equal(got, want) {
			t.Fatalf("the console lines of worker.run.failed have the severities %v, want %v", got, want)
		}
		otlp := map[string]string{}
		for _, record := range c.logRecords() {
			if record.EventName == "worker.run.failed" {
				for _, kv := range record.Attributes {
					if kv.Key == "cause" {
						otlp[kv.Value.GetStringValue()] = record.SeverityText
					}
				}
			}
		}
		if !maps.Equal(otlp, want) {
			t.Fatalf("the OTLP records of worker.run.failed have the severities %v, want %v", otlp, want)
		}
	})

	t.Run("ST-SUP-RP12-LC23-T01 The schema backfill observations are built in and their names are reserved", func(t *testing.T) {
		builtins, err := NewBuiltins(nil, []SchemaChange{{ID: "user-email-rename", Open: true}, {ID: "orders-retype", Open: false}})
		if err != nil {
			t.Fatal(err)
		}
		if got := builtins.Changes(); !slices.Equal(got, []string{"user-email-rename"}) {
			t.Fatalf("the enum of the field change is %v, want the open change alone", got)
		}
		recorder := NewRecorder(builtins)
		var observer lease.Observer = recorder.Backfill()
		observer.BackfillRows("user-email-rename", 200)
		observer.BackfillFailedRows("user-email-rename", 2)
		observer.BackfillDuplicateGroups("user-email-rename", 1)
		observer.BackfillCompleted("user-email-rename")
		observer.BackfillStalled("user-email-rename")
		observer.BackfillRows("orders-retype", 5)
		var kept []string
		for _, e := range recorder.All() {
			kept = append(kept, e.Kind.String()+" "+e.Severity.String()+" "+e.String())
		}
		want := []string{
			"counter INFO schema.backfill.rows{change=user-email-rename value=200}",
			"counter WARN schema.backfill.failed_rows{change=user-email-rename value=2}",
			"counter WARN schema.backfill.duplicate_groups{change=user-email-rename value=1}",
			"event INFO schema.backfill.completed{change=user-email-rename}",
			"event ERROR schema.backfill.stalled{change=user-email-rename}",
		}
		if !slices.Equal(kept, want) || recorder.Dropped() != 1 {
			t.Fatalf("the recorder holds %v with %d drops, want %v and the finished change dropped", kept, recorder.Dropped(), want)
		}
		for _, name := range []string{"schema.backfill.rows", "schema.backfill.stalled"} {
			if _, err := Declare(Spec{Name: name, Kind: Counter, Severity: Info, EmittedBy: []string{command}}); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("the declaration of %s returned %v, want a refusal by name", name, err)
			}
		}
	})
}

// handClock is the clock of the rate limit of a test, which the test moves.
type handClock struct {
	mu sync.Mutex
	at time.Time
}

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
