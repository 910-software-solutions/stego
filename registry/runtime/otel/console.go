package otel

import (
	"context"
	"errors"
	"io"
	"math"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// The bounds of the console lines of the log provider (ST-SUP-RP03-LC16).
const (
	// ConsoleQueue is the most lines that wait for the console writer; a line that does not fit is dropped and
	// counted (ConsoleDropped), and the record is never blocked.
	ConsoleQueue = 256
	// ConsoleLimit is the most lines of one event name that the console writes in one ConsoleWindow.
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
// output, whether or not an OTLP endpoint is set (ST-SUP-RP03-LC16). The line holds the timestamp, the severity,
// the event name, the message and the attributes of the record. The records of the runtime hold no value of a
// setting, no payload and no error text (ST-SUP-RP14-LC05), so the line holds none. A bounded queue takes the
// lines: a full queue drops the line and counts it, and never blocks the record. One goroutine writes the lines and
// keeps the rate limit of each event name.
type console struct {
	out   io.Writer
	now   func() time.Time
	skip  func(scope string) bool
	queue chan consoleLine
	flush chan chan struct{}
	stop  chan struct{}
	done  chan struct{}

	dropped atomic.Uint64
	failed  atomic.Uint64
	once    sync.Once

	// The state of the writer goroutine only.
	windows map[string]*consoleWindow
	line    []byte
}

// consoleLine is the copy of one record that the console writes.
type consoleLine struct {
	at       time.Time
	severity otellog.Severity
	event    string
	message  string
	attrs    []attribute.KeyValue
}

// consoleWindow is the rate limit of one event name: when its window started, the lines written in it and the lines
// dropped in it.
type consoleWindow struct {
	start   time.Time
	written int
	dropped uint64
}

// newConsole starts the console on out. now is the clock of the rate limit; ticks tells the writer to close the
// windows that ended, and may be nil, as for a test that moves its own clock and calls ForceFlush. skip tells
// whether the records of an instrumentation scope stay off the console.
func newConsole(out io.Writer, now func() time.Time, ticks <-chan time.Time, skip func(scope string) bool) *console {
	c := &console{out: out, now: now, skip: skip, queue: make(chan consoleLine, ConsoleQueue),
		flush: make(chan chan struct{}), stop: make(chan struct{}), done: make(chan struct{}),
		windows: map[string]*consoleWindow{}}
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

// take writes the line of a record within the rate limit of its event name, and counts it as dropped over it.
func (c *console) take(line consoleLine) {
	c.closeWindows(false)
	w := c.windows[line.event]
	if w == nil {
		if len(c.windows) >= consoleNames {
			c.dropped.Add(1)
			return
		}
		w = &consoleWindow{start: c.now()}
		c.windows[line.event] = w
	}
	if w.written >= ConsoleLimit {
		w.dropped++
		return
	}
	w.written++
	c.write(c.recordLine(c.line[:0], line))
}

// closeWindows ends each window that ended by the clock, or each window when all is true, and writes one line with
// the count of the dropped lines for each that dropped lines, in the order of the event names.
func (c *console) closeWindows(all bool) {
	if len(c.windows) == 0 {
		return
	}
	now := c.now()
	var ended []string
	for name, w := range c.windows {
		if all || now.Sub(w.start) >= ConsoleWindow {
			ended = append(ended, name)
		}
	}
	slices.Sort(ended)
	for _, name := range ended {
		if dropped := c.windows[name].dropped; dropped > 0 {
			c.write(c.droppedLine(c.line[:0], now, name, dropped))
		}
		delete(c.windows, name)
	}
}

func (c *console) write(line []byte) {
	c.line = line
	if _, err := c.out.Write(line); err != nil {
		c.failed.Add(1)
	}
}

// recordLine is the JSON line of a record: the timestamp, the severity, the event name, the message and the
// attributes, in that order.
func (c *console) recordLine(b []byte, line consoleLine) []byte {
	b = appendHead(b, line.at, line.severity, line.event)
	b = append(b, `,"message":`...)
	b = appendJSONText(b, line.message)
	if len(line.attrs) > 0 {
		b = append(b, `,"attributes":{`...)
		for i, kv := range line.attrs {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendJSONText(b, string(kv.Key))
			b = append(b, ':')
			b = appendJSONValue(b, kv.Value)
		}
		b = append(b, '}')
	}
	return append(b, '}', '\n')
}

// droppedLine is the line of the lines of one event name that the rate limit dropped in one window: the event name
// and their count.
func (c *console) droppedLine(b []byte, at time.Time, event string, dropped uint64) []byte {
	b = appendHead(b, at, otellog.SeverityWarn, event)
	b = append(b, `,"message":`...)
	b = appendJSONText(b, droppedMessage)
	b = append(b, `,"dropped":`...)
	b = strconv.AppendUint(b, dropped, 10)
	return append(b, '}', '\n')
}

func appendHead(b []byte, at time.Time, severity otellog.Severity, event string) []byte {
	b = append(b, `{"timestamp":`...)
	b = appendJSONText(b, at.UTC().Format(time.RFC3339Nano))
	b = append(b, `,"severity":`...)
	b = appendJSONText(b, consoleSeverity(severity))
	b = append(b, `,"event.name":`...)
	return appendJSONText(b, event)
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

// appendJSONText appends text as a JSON string. An invalid UTF-8 byte becomes U+FFFD, and a control character, the
// line separator and the paragraph separator are escaped, so the line is one line.
func appendJSONText(b []byte, text string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	for i := 0; i < len(text); {
		c := text[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\':
				b = append(b, '\\', c)
			case c == '\n':
				b = append(b, '\\', 'n')
			case c == '\r':
				b = append(b, '\\', 'r')
			case c == '\t':
				b = append(b, '\\', 't')
			case c < 0x20 || c == 0x7f:
				b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			default:
				b = append(b, c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			b = append(b, `�`...)
		case r == ' ' || r == ' ':
			b = append(b, '\\', 'u', '2', '0', '2', hex[r&0xf])
		default:
			b = append(b, text[i:i+size]...)
		}
		i += size
	}
	return append(b, '"')
}
