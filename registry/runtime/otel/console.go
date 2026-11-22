package otel

import (
	"context"
	"errors"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/jsell-rh/stego/registry/runtime/internal/logline"
)

// The bounds of the console lines of the log provider (ST-SUP-RP03-LC16).
const (
	// ConsoleQueue is the most lines that wait for the console writer; a line that does not fit is dropped and
	// counted (ConsoleDropped), and the record is never blocked.
	ConsoleQueue = 256
	// ConsoleLimit is the most lines of one event name that the console writes in one ConsoleWindow, for the lines
	// of severity WARN and, in a budget of their own, for the lines of severity ERROR or FATAL, so that many WARN
	// lines of an event name never keep its ERROR lines off the console (security review of PR #421, I1).
	ConsoleLimit = 10
	// ConsoleWindow is the window of the rate limit of one event name. When it ends with lines of the event name
	// dropped, the console writes one line with the event name and the count of the dropped lines.
	ConsoleWindow = time.Minute
	// consoleNames is the most event names that the rate limit follows at one time. The event names of the runtime
	// are fixed, so the bound holds only against a name that a caller makes; a line of a name over it is dropped and
	// counted.
	consoleNames = 256
)

// The instrumentation scopes whose records the console does not write: the probe writes its built-in events to
// the console itself, and its declared observations carry declared values (ST-SUP-RP12-LC22); the controller writes
// its logs to standard error itself when no collector is set (ST-SUP-RP02-LC20).
const (
	probeScope      = "stego/probe"
	controllerScope = "stego/controller"
)

// droppedMessage is the message of the line that counts the dropped lines of one event name.
const droppedMessage = "the console dropped lines of this event name in the last window of its rate limit"

// console is the log processor that writes each record of severity WARN or higher as one JSON line to standard
// output, whether or not an OTLP endpoint is set (ST-SUP-RP03-LC16). The line has the one form of the lines of the
// runtime (internal/logline; walkthrough 25, L-1): the timestamp in UTC, the severity, the service name, the instance
// identity, the event name, the message, the trace and the span of the record, and the attributes under
// attributes, each key with the prefix stego. or a name of the semantic conventions for HTTP and RPC. The records of the runtime hold no value of a
// setting, no payload and no error text (ST-SUP-RP14-LC05), so the line holds none. A bounded queue takes the
// lines: a full queue drops the line and counts it, and never blocks the record. One goroutine writes the lines and
// keeps the rate limit of each event name.
type console struct {
	out io.Writer
	// service and instance name the writer on each line; they do not change after the start.
	service, instance string
	now               func() time.Time
	skip              func(scope string) bool
	queue             chan consoleLine
	flush             chan chan struct{}
	stop              chan struct{}
	done              chan struct{}

	dropped atomic.Uint64
	failed  atomic.Uint64
	once    sync.Once

	// The state of the writer goroutine only.
	windows map[consoleBudget]*consoleWindow
	line    []byte
}

// consoleLine is the copy of one record that the console writes.
type consoleLine struct {
	at       time.Time
	severity otellog.Severity
	event    string
	message  string
	trace    string
	span     string
	attrs    []attribute.KeyValue
}

// consoleBudget is the key of one rate limit: an event name, and whether its lines are of severity ERROR or higher.
type consoleBudget struct {
	event  string
	errors bool
}

// budgetOf gives the budget of a line of event of severity.
func budgetOf(event string, severity otellog.Severity) consoleBudget {
	return consoleBudget{event: event, errors: severity >= otellog.SeverityError}
}

// consoleWindow is the rate limit of one event name: when its window started, the lines written in it and the lines
// dropped in it.
type consoleWindow struct {
	start   time.Time
	written int
	dropped uint64
}

// newConsole starts the console of the service and instance on out. now is the clock of the rate limit; ticks tells the writer to close the
// windows that ended, and may be nil, as for a test that moves its own clock and calls ForceFlush. skip tells
// whether the records of an instrumentation scope stay off the console.
func newConsole(out io.Writer, service, instance string, now func() time.Time, ticks <-chan time.Time, skip func(scope string) bool) *console {
	c := &console{out: out, service: service, instance: instance, now: now, skip: skip, queue: make(chan consoleLine, ConsoleQueue),
		flush: make(chan chan struct{}), stop: make(chan struct{}), done: make(chan struct{}),
		windows: map[consoleBudget]*consoleWindow{}}
	go c.run(ticks)
	return c
}

// consoleSkip gives the scopes that stay off the console: the probe's always, and the controller's when no collector
// is set, as the controller then writes its logs to standard error itself.
func consoleSkip(collector bool) func(scope string) bool {
	return func(scope string) bool {
		return scope == probeScope || (scope == controllerScope && !collector)
	}
}

// Enabled tells the logger that the console takes a record of severity WARN or higher of a scope that it does not
// skip.
func (c *console) Enabled(_ context.Context, param sdklog.EnabledParameters) bool {
	return param.Severity >= otellog.SeverityWarn && (c.skip == nil || !c.skip(param.InstrumentationScope.Name))
}

// OnEmit queues the line of a record of severity WARN or higher. It never blocks: when the queue is full, it drops
// the line and counts it.
func (c *console) OnEmit(_ context.Context, record *sdklog.Record) error {
	if record == nil || record.Severity() < otellog.SeverityWarn || (c.skip != nil && c.skip(record.InstrumentationScope().Name)) {
		return nil
	}
	line := consoleLine{at: record.Timestamp(), severity: record.Severity(), event: record.EventName(),
		message: valueText(record.Body())}
	if line.at.IsZero() {
		line.at = record.ObservedTimestamp()
	}
	if record.TraceID().IsValid() && record.SpanID().IsValid() {
		line.trace, line.span = record.TraceID().String(), record.SpanID().String()
	}
	if n := record.AttributesLen(); n > 0 {
		line.attrs = make([]attribute.KeyValue, 0, n)
		record.WalkAttributes(func(kv attribute.KeyValue) bool {
			line.attrs = append(line.attrs, kv)
			return true
		})
	}
	select {
	case <-c.stop:
		return nil
	default:
	}
	select {
	case c.queue <- line:
	default:
		c.dropped.Add(1)
	}
	return nil
}

// ForceFlush waits until the writer wrote each line queued before the call and closed the windows that ended by
// the clock, or until ctx ends.
func (c *console) ForceFlush(ctx context.Context) error {
	ack := make(chan struct{})
	select {
	case c.flush <- ack:
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown stops the writer after it wrote the queued lines and, for each event name with dropped lines, the line
// with their count, as its window ends at shutdown. It waits until ctx ends at most.
func (c *console) Shutdown(ctx context.Context) error {
	c.once.Do(func() { close(c.stop) })
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return errors.New("the console did not stop in time")
	}
}

func (c *console) run(ticks <-chan time.Time) {
	defer close(c.done)
	for {
		select {
		case line := <-c.queue:
			c.take(line)
		case <-ticks:
			c.closeWindows(false)
		case ack := <-c.flush:
			c.drain()
			c.closeWindows(false)
			close(ack)
		case <-c.stop:
			c.drain()
			c.closeWindows(true)
			return
		}
	}
}

// drain writes the lines that wait in the queue.
func (c *console) drain() {
	for {
		select {
		case line := <-c.queue:
			c.take(line)
		default:
			return
		}
	}
}

// take writes the line of a record within the rate limit of its event name and of its budget, WARN or ERROR and
// higher, and counts it as dropped over it.
func (c *console) take(line consoleLine) {
	c.closeWindows(false)
	budget := budgetOf(line.event, line.severity)
	w := c.windows[budget]
	if w == nil {
		if len(c.windows) >= consoleNames {
			c.dropped.Add(1)
			return
		}
		w = &consoleWindow{start: c.now()}
		c.windows[budget] = w
	}
	if w.written >= ConsoleLimit {
		w.dropped++
		return
	}
	w.written++
	c.write(c.recordLine(c.line[:0], line))
}

// closeWindows ends each window that ended by the clock, or each window when all is true, and writes one line with
// the count of the dropped lines for each that dropped lines, in the order of the event names, the WARN budget
// first. The line of the ERROR budget has the severity ERROR.
func (c *console) closeWindows(all bool) {
	if len(c.windows) == 0 {
		return
	}
	now := c.now()
	var ended []consoleBudget
	for budget, w := range c.windows {
		if all || now.Sub(w.start) >= ConsoleWindow {
			ended = append(ended, budget)
		}
	}
	slices.SortFunc(ended, func(a, b consoleBudget) int {
		if a.event != b.event {
			return strings.Compare(a.event, b.event)
		}
		switch {
		case a.errors == b.errors:
			return 0
		case a.errors:
			return 1
		}
		return -1
	})
	for _, budget := range ended {
		if dropped := c.windows[budget].dropped; dropped > 0 {
			severity := otellog.SeverityWarn
			if budget.errors {
				severity = otellog.SeverityError
			}
			c.write(c.droppedLine(c.line[:0], now, budget.event, severity, dropped))
		}
		delete(c.windows, budget)
	}
}

func (c *console) write(line []byte) {
	c.line = line
	if _, err := c.out.Write(line); err != nil {
		c.failed.Add(1)
	}
}

// recordLine is the JSON line of a record in the form of internal/logline.
func (c *console) recordLine(b []byte, line consoleLine) []byte {
	b = logline.AppendHead(b, logline.Head{Time: line.at, Severity: consoleSeverity(line.severity), Service: c.service,
		Instance: c.instance, Event: line.event, Message: line.message, TraceID: line.trace, SpanID: line.span})
	for i, kv := range line.attrs {
		b = logline.AppendKey(b, i, string(kv.Key))
		b = appendJSONValue(b, kv.Value)
	}
	return logline.AppendEnd(b, len(line.attrs))
}

// droppedLine is the line of the lines of one event name and budget that the rate limit dropped in one window: the
// event name, the severity of the budget and their count, stego.dropped.
func (c *console) droppedLine(b []byte, at time.Time, event string, severity otellog.Severity, dropped uint64) []byte {
	b = logline.AppendHead(b, logline.Head{Time: at, Severity: consoleSeverity(severity), Service: c.service,
		Instance: c.instance, Event: event, Message: droppedMessage})
	b = logline.AppendKey(b, 0, "dropped")
	b = strconv.AppendUint(b, dropped, 10)
	return logline.AppendEnd(b, 1)
}

// consoleSeverity is the severity name of a record of severity WARN or higher.
func consoleSeverity(severity otellog.Severity) string {
	switch {
	case severity >= otellog.SeverityFatal:
		return "FATAL"
	case severity >= otellog.SeverityError:
		return "ERROR"
	}
	return "WARN"
}

// valueText is the text of a body: the string itself, or the text form of another value.
func valueText(v attribute.Value) string {
	if v.Type() == attribute.STRING {
		return v.AsString()
	}
	if v.Type() == attribute.INVALID {
		return ""
	}
	return v.Emit()
}

// appendJSONValue appends an attribute value as JSON: a bool, a whole number and a finite number as themselves, and
// every other value as a JSON string of its text form.
func appendJSONValue(b []byte, v attribute.Value) []byte {
	switch v.Type() {
	case attribute.BOOL:
		return strconv.AppendBool(b, v.AsBool())
	case attribute.INT64:
		return strconv.AppendInt(b, v.AsInt64(), 10)
	case attribute.FLOAT64:
		if f := v.AsFloat64(); !math.IsNaN(f) && !math.IsInf(f, 0) {
			return strconv.AppendFloat(b, f, 'g', -1, 64)
		}
	}
	return appendJSONText(b, valueText(v))
}

// appendJSONText appends text as a JSON string (logline.AppendText).
func appendJSONText(b []byte, text string) []byte { return logline.AppendText(b, text) }
