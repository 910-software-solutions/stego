package probe

import (
	"context"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jsell-rh/stego/registry/runtime/otel"
)

// format is the console form of declared observations.
type format uint8

const (
	formatOff format = iota
	formatJSON
	formatLogfmt
)

// consoleSink writes one line for each record to standard output through its bounded queue. It starts no
// OpenTelemetry provider and opens no connection (ST-SUP-RP12-LC18). The built-in service and worker events are
// on it in every setting, as one JSON line (ST-SUP-RP12-LC22); declared observations and the schema backfill
// observations only when STEGO_PROBE_CONSOLE turns it on (ST-SUP-RP12-LC16).
type consoleSink struct {
	queue    *queue
	builtins *queue
	mu       sync.Mutex
	out      io.Writer
	format   format
	service  string
	instance string
	line     []byte
	// now is the clock of the rate limit of the built-in events, and windows the window of each event name and
	// budget, both under mu.
	now     func() time.Time
	windows map[budget]*window
	// level is the lowest severity of a built-in worker event that the console writes, from STEGO_LOG_LEVEL; the zero
	// value writes each (decision on ST-SUP-Q210).
	level Severity
}

// budget is the key of one rate limit of the built-in events: the event name, and whether it is of severity ERROR,
// which has a budget of its own, as on the console of the log provider (otel.ConsoleLimit).
type budget struct {
	event  string
	errors bool
}

// window is the rate limit of one budget: when its window started, the lines written in it and the lines dropped.
type window struct {
	start   time.Time
	written int
	dropped uint64
}

// limitedNames is the most budgets that the rate limit follows at one time; the built-in event names are fixed, so
// it is only a bound.
const limitedNames = 64

// droppedMessage is the message of the line that counts the dropped lines of one built-in event name.
const droppedMessage = "the console dropped lines of this event name in the last window of its rate limit"

func newConsoleSink(out io.Writer, f format, service, instance string) *consoleSink {
	s := &consoleSink{out: out, format: f, service: service, instance: instance, now: time.Now, windows: map[budget]*window{}}
	s.queue = newQueue(s.write)
	s.builtins = newBoundedQueue(BuiltinQueueLimit, s.write)
	return s
}

// BuiltinQueueLimit is the most built-in service and worker events that the console holds in their own queue, so
// a full queue of declared observations never drops one (ST-SUP-RP12-LC22).
const BuiltinQueueLimit = 64

// BuiltinReserve is the places of the queue of the built-in events that an INFO worker event does not take, so a
// burst of runs that commit never drops a worker.run.failed or a service event.
const BuiltinReserve = 16

// send queues the record when the console takes it: a built-in service event, and a built-in worker event of the
// level of the console or higher, in its own queue.
func (s *consoleSink) send(r *record) {
	switch {
	case r.obs.class == serviceClass:
		s.builtins.put(r)
	case r.obs.class == workerClass:
		switch {
		case r.obs.severity < s.level:
		case r.obs.severity < Warn:
			// An INFO worker event, such as worker.run.completed of each run that commits, never takes the last
			// BuiltinReserve places, which stay for worker.run.failed and the service events (security review of
			// PR #492, L1).
			s.builtins.putBelow(r, BuiltinReserve)
		default:
			s.builtins.put(r)
		}
	case s.formatOf(r) != formatOff:
		s.queue.put(r)
	}
}

func (s *consoleSink) dropped() uint64 { return s.queue.dropped.Load() + s.builtins.dropped.Load() }
func (s *consoleSink) failed() uint64  { return s.queue.failed.Load() + s.builtins.failed.Load() }

// drain writes what both queues hold, then the line of telemetry.runtime.stopped, all until ctx ends. It tells
// whether it wrote everything.
func (s *consoleSink) drain(ctx context.Context, stopped *record) bool {
	ok := s.queue.drain(ctx)
	if !s.builtins.drain(ctx) {
		ok = false
	}
	if !ok {
		return false
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Each window ends at the stop: the count lines of the dropped built-in events come before the last line.
		s.mu.Lock()
		err := s.closeWindows(true)
		s.mu.Unlock()
		if err != nil {
			s.builtins.failed.Add(1)
		}
		if !safeWrite(s.write, stopped) {
			s.builtins.failed.Add(1)
		}
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *consoleSink) formatOf(r *record) format {
	switch r.obs.class {
	case serviceClass, workerClass:
		return formatJSON
	}
	return s.format
}

// limited tells whether the rate limit holds the line of a record: a built-in worker event of severity WARN or
// higher, such as worker.run.failed, which a failing worker sends for each run (walkthrough 9). A built-in service
// event is always written (ST-SUP-RP12-LC22), and a counter and a declared observation are not held.
func limited(r *record) bool {
	return r.obs.class == workerClass && r.obs.kind == Event && r.obs.severity >= Warn
}

// admit closes the windows that ended and tells whether the line of r is within the rate limit of its event name and
// budget: at most otel.ConsoleLimit lines in one otel.ConsoleWindow, for the WARN lines and for the ERROR lines each.
// A line over it is counted, and the window writes the count when it ends.
func (s *consoleSink) admit(r *record) (bool, error) {
	err := s.closeWindows(false)
	if !limited(r) {
		return true, err
	}
	key := budget{event: r.obs.signal, errors: r.obs.severity >= Error}
	w := s.windows[key]
	if w == nil {
		if len(s.windows) >= limitedNames {
			return false, err
		}
		w = &window{start: s.now()}
		s.windows[key] = w
	}
	if w.written >= otel.ConsoleLimit {
		w.dropped++
		return false, err
	}
	w.written++
	return true, err
}

// closeWindows ends each window that ended by the clock, or each window when all is true, and writes one line with
// the count of the dropped lines for each that dropped lines, in the order of the event names, the WARN budget first.
func (s *consoleSink) closeWindows(all bool) error {
	if len(s.windows) == 0 {
		return nil
	}
	now := s.now()
	var ended []budget
	for key, w := range s.windows {
		if all || now.Sub(w.start) >= otel.ConsoleWindow {
			ended = append(ended, key)
		}
	}
	slices.SortFunc(ended, func(a, b budget) int {
		if c := strings.Compare(a.event, b.event); c != 0 {
			return c
		}
		switch {
		case a.errors == b.errors:
			return 0
		case a.errors:
			return 1
		}
		return -1
	})
	var failed error
	for _, key := range ended {
		if dropped := s.windows[key].dropped; dropped > 0 {
			severity := Warn
			if key.errors {
				severity = Error
			}
			s.line = s.droppedLine(s.line[:0], now, key.event, severity, dropped)
			if _, err := s.out.Write(s.line); err != nil {
				failed = err
			}
		}
		delete(s.windows, key)
	}
	return failed
}

// droppedLine is the JSON line of the lines of one built-in event name and budget that the rate limit dropped in one
// window: the timestamp, the severity of the budget, the service name, the instance identity, the event name, the
// message and the count.
func (s *consoleSink) droppedLine(b []byte, at time.Time, event string, severity Severity, dropped uint64) []byte {
	b = append(b, `{"timestamp":`...)
	b = appendJSONString(b, at.UTC().Format(time.RFC3339Nano))
	b = append(b, `,"severity":`...)
	b = appendJSONString(b, severity.String())
	b = append(b, `,"service.name":`...)
	b = appendJSONString(b, s.service)
	b = append(b, `,"service.instance.id":`...)
	b = appendJSONString(b, s.instance)
	b = append(b, `,"event.name":`...)
	b = appendJSONString(b, event)
	b = append(b, `,"message":`...)
	b = appendJSONString(b, droppedMessage)
	b = append(b, `,"dropped":`...)
	b = strconv.AppendUint(b, dropped, 10)
	return append(b, '}', '\n')
}

// write writes the line of one record in one write call, within the rate limit of the built-in events. The two
// queues share the buffer and the writer.
func (s *consoleSink) write(r *record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok, err := s.admit(r)
	if !ok {
		return err
	}
	if s.formatOf(r) == formatLogfmt {
		s.line = s.logfmt(s.line[:0], r)
	} else {
		s.line = s.json(s.line[:0], r)
	}
	if _, werr := s.out.Write(s.line); werr != nil {
		return werr
	}
	return err
}

// json is the JSON line of a record: the timestamp, the severity, the service name, the instance identity, the
// name, the stego.outcome, the histogram or count value, the correlation and the declared fields, in that order
// (ST-SUP-RP12-LC07). A built-in line carries no tenant, principal or operation (ST-SUP-RP12-LC22).
func (s *consoleSink) json(b []byte, r *record) []byte {
	b = append(b, `{"timestamp":`...)
	b = appendJSONString(b, r.time.UTC().Format(time.RFC3339Nano))
	b = append(b, `,"severity":`...)
	b = appendJSONString(b, r.obs.severity.String())
	b = append(b, `,"service.name":`...)
	b = appendJSONString(b, s.service)
	b = append(b, `,"service.instance.id":`...)
	b = appendJSONString(b, s.instance)
	b = append(b, `,"event.name":`...)
	b = appendJSONString(b, r.obs.signal)
	if r.outcome != "" {
		b = append(b, `,"stego.outcome":`...)
		b = appendJSONString(b, r.outcome)
	}
	if text, ok := consoleValue(r); ok {
		b = append(b, `,"value":`...)
		b = append(b, text...)
	}
	if r.spanCtx.IsValid() {
		b = append(b, `,"trace_id":`...)
		b = appendJSONString(b, r.spanCtx.TraceID().String())
		b = append(b, `,"span_id":`...)
		b = appendJSONString(b, r.spanCtx.SpanID().String())
	}
	if r.obs.class == declared {
		b = append(b, `,"tenant":`...)
		b = appendJSONString(b, r.tenant)
		b = append(b, `,"principal":`...)
		b = appendJSONString(b, r.principal)
		b = append(b, `,"operation":`...)
		b = appendJSONString(b, r.operation)
	}
	if len(r.obs.fields) > 0 {
		b = append(b, `,"fields":{`...)
		for i, f := range r.obs.fields {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendJSONString(b, f.name)
			b = append(b, ':')
			b = appendJSONValue(b, r.values[i])
		}
		b = append(b, '}')
	}
	return append(b, '}', '\n')
}

// logfmt is the logfmt line of a record, with the keys of the JSON line and each declared field as fields.<name>.
func (s *consoleSink) logfmt(b []byte, r *record) []byte {
	pair := func(b []byte, key, value string) []byte {
		if len(b) > 0 {
			b = append(b, ' ')
		}
		b = append(b, key...)
		b = append(b, '=')
		return appendLogfmtValue(b, value)
	}
	b = pair(b, "timestamp", r.time.UTC().Format(time.RFC3339Nano))
	b = pair(b, "severity", r.obs.severity.String())
	b = pair(b, "service.name", s.service)
	b = pair(b, "service.instance.id", s.instance)
	b = pair(b, "event.name", r.obs.signal)
	if r.outcome != "" {
		b = pair(b, "stego.outcome", r.outcome)
	}
	if text, ok := consoleValue(r); ok {
		b = pair(b, "value", string(text))
	}
	if r.spanCtx.IsValid() {
		b = pair(b, "trace_id", r.spanCtx.TraceID().String())
		b = pair(b, "span_id", r.spanCtx.SpanID().String())
	}
	if r.obs.class == declared {
		b = pair(b, "tenant", r.tenant)
		b = pair(b, "principal", r.principal)
		b = pair(b, "operation", r.operation)
	}
	for i, f := range r.obs.fields {
		b = pair(b, "fields."+f.name, r.values[i].String())
	}
	return append(b, '\n')
}

// consoleValue is the histogram value of a record, or the count that a built-in backfill counter adds.
func consoleValue(r *record) ([]byte, bool) {
	switch {
	case r.hasNumber:
		return strconv.AppendFloat(nil, r.number, 'g', -1, 64), true
	case r.obs.class == backfillClass && r.obs.kind == Counter:
		return strconv.AppendInt(nil, r.count, 10), true
	}
	return nil, false
}

func appendJSONValue(b []byte, v Value) []byte {
	switch v.kind {
	case integerValue, measureValue:
		return strconv.AppendInt(b, v.number, 10)
	case boolValue:
		return strconv.AppendBool(b, v.flag)
	}
	return appendJSONString(b, v.text)
}

// appendJSONString appends text as a JSON string. Text is valid UTF-8 here; an invalid byte becomes U+FFFD.
func appendJSONString(b []byte, text string) []byte {
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

// appendLogfmtValue appends a logfmt value, quoted when it is empty or holds a space, a quote, an equals sign or a
// control character.
func appendLogfmtValue(b []byte, value string) []byte {
	plain := value != ""
	for i := 0; i < len(value) && plain; i++ {
		c := value[i]
		if c <= ' ' || c == '"' || c == '=' || c == '\\' || c == 0x7f || c >= utf8.RuneSelf {
			plain = false
		}
	}
	if plain {
		return append(b, value...)
	}
	return appendJSONString(b, value)
}
