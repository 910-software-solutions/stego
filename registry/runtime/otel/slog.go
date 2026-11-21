package otel

import (
	"context"
	"io"
	"log/slog"
	"math"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SlogEvent is the event name of a log/slog line whose message is a text and that names no event of its own.
const SlogEvent = "stego.log"

// SlogEventKey is the key of the attribute that gives the event name of a log/slog line, such as
// slog.Info("command: seal of a secret column", SlogEventKey, "command.seal.pass", ...). The line does not hold it
// as an attribute.
const SlogEventKey = "event.name"

// eventName is the form of an event name of the runtime, such as command.attempts.failed.
var eventName = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$`)

// SlogHandler is the log/slog handler of a process of the runtime (ST-SUP-RP03-LC16; walkthrough 21, U-E): it writes
// each record of level INFO or higher as one JSON line on its output, in the format of the console lines of the log
// provider, so that no line of the runtime is plain text. The line holds the timestamp, the severity, the event
// name, the message and the attributes. The event name is the value of the attribute SlogEventKey when it has the
// form of an event name; else the message, when it has that form, such as command.fill.failed; else SlogEvent. Only a
// line of a package of the runtime names its event: a line of other code, such as a fill, gets SlogEvent. Each
// attribute key gets the prefix stego., as the attributes of the runtime's records have it, and the keys of a group
// are joined with a dot. A line holds at most SlogAttributeLimit attributes of at most SlogValueLimit characters each.
// The handler writes the values that the caller gives, so a caller gives no value of a
// setting, no payload and no error text, as for a record of the log provider. One write of out takes each line.
type SlogHandler struct {
	out    *lockedWriter
	attrs  []slog.Attr
	groups []string
}

// lockedWriter writes each line of all the handlers of one output in one call, one line at a time.
type lockedWriter struct {
	mu  sync.Mutex
	out io.Writer
}

// NewSlogHandler gives the handler that writes its lines on out.
func NewSlogHandler(out io.Writer) *SlogHandler {
	return &SlogHandler{out: &lockedWriter{out: out}}
}

// Enabled takes each record of level INFO or higher.
func (h *SlogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

// WithAttrs gives a handler that adds attrs to each line.
func (h *SlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	next := *h
	next.attrs = append(append([]slog.Attr{}, h.attrs...), qualified(h.groups, attrs)...)
	return &next
}

// WithGroup gives a handler whose attributes are in the group name.
func (h *SlogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.groups = append(append([]string{}, h.groups...), name)
	return &next
}

// qualified gives attrs under groups, as one group attribute per group.
func qualified(groups []string, attrs []slog.Attr) []slog.Attr {
	for i := len(groups) - 1; i >= 0; i-- {
		values := make([]any, len(attrs))
		for j, a := range attrs {
			values[j] = a
		}
		attrs = []slog.Attr{slog.Group(groups[i], values...)}
	}
	return attrs
}

// Handle writes the line of record.
func (h *SlogHandler) Handle(_ context.Context, record slog.Record) error {
	event := ""
	attrs := append([]slog.Attr{}, h.attrs...)
	var own []slog.Attr
	// Only a line of a package of the runtime names its event: a line of other code, such as a fill, keeps the event
	// name stego.log, so it cannot pass as an event of the runtime (security review of PR #485, I1).
	runtimeLine := fromRuntime(record.PC)
	record.Attrs(func(a slog.Attr) bool {
		if runtimeLine && len(h.groups) == 0 && a.Key == SlogEventKey && event == "" {
			if v := a.Value.Resolve(); v.Kind() == slog.KindString && eventName.MatchString(v.String()) {
				event = v.String()
				return true
			}
		}
		own = append(own, a)
		return true
	})
	attrs = append(attrs, qualified(h.groups, own)...)
	if event == "" {
		event = SlogEvent
		if runtimeLine && eventName.MatchString(record.Message) {
			event = record.Message
		}
	}
	at := record.Time
	if at.IsZero() {
		at = time.Now()
	}
	b := make([]byte, 0, 256)
	b = append(b, `{"timestamp":`...)
	b = appendJSONText(b, at.UTC().Format(time.RFC3339Nano))
	b = append(b, `,"severity":`...)
	b = appendJSONText(b, slogSeverity(record.Level))
	b = append(b, `,"event.name":`...)
	b = appendJSONText(b, event)
	b = append(b, `,"message":`...)
	b = appendJSONText(b, record.Message)
	first, written := true, 0
	for _, a := range attrs {
		b = appendSlogAttr(b, "stego", a, &first, &written)
	}
	if !first {
		b = append(b, '}')
	}
	b = append(b, '}', '\n')
	h.out.mu.Lock()
	defer h.out.mu.Unlock()
	_, err := h.out.out.Write(b)
	return err
}

// slogSeverity is the severity name of a level.
func slogSeverity(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "ERROR"
	case level >= slog.LevelWarn:
		return "WARN"
	case level >= slog.LevelInfo:
		return "INFO"
	}
	return "DEBUG"
}

// appendSlogAttr appends the attribute a under the key prefix: a group gives each of its attributes under the prefix
// and its key, and an empty attribute gives nothing. first tells whether no attribute is written yet, which opens the
// object of the attributes.
func appendSlogAttr(b []byte, prefix string, a slog.Attr, first *bool, written *int) []byte {
	v := a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return b
	}
	key := prefix
	switch {
	case a.Key != "" && prefix == "stego":
		// A key that has the prefix already, such as stego.action, keeps it once.
		key = prefix + "." + strings.TrimPrefix(a.Key, "stego.")
	case a.Key != "":
		key = prefix + "." + a.Key
	}
	if v.Kind() == slog.KindGroup {
		for _, member := range v.Group() {
			b = appendSlogAttr(b, key, member, first, written)
		}
		return b
	}
	// At most SlogAttributeLimit attributes, as the log provider keeps (security review of PR #485, L2).
	if *written >= SlogAttributeLimit {
		return b
	}
	*written++
	if *first {
		b = append(b, `,"attributes":{`...)
		*first = false
	} else {
		b = append(b, ',')
	}
	b = appendJSONText(b, key)
	b = append(b, ':')
	switch v.Kind() {
	case slog.KindBool:
		return strconv.AppendBool(b, v.Bool())
	case slog.KindInt64:
		return strconv.AppendInt(b, v.Int64(), 10)
	case slog.KindUint64:
		return strconv.AppendUint(b, v.Uint64(), 10)
	case slog.KindFloat64:
		if f := v.Float64(); !math.IsNaN(f) && !math.IsInf(f, 0) {
			return strconv.AppendFloat(b, f, 'g', -1, 64)
		}
	case slog.KindTime:
		return appendJSONText(b, v.Time().UTC().Format(time.RFC3339Nano))
	}
	return appendJSONText(b, limitText(v.String()))
}

// The limits of a line of SlogHandler, as the log provider of the runtime holds them: at most SlogAttributeLimit
// attributes, and at most SlogValueLimit characters of each value; the rest is dropped.
const (
	SlogAttributeLimit = 12
	SlogValueLimit     = 256
)

// limitText gives the first SlogValueLimit characters of text.
func limitText(text string) string {
	n := 0
	for i := range text {
		if n == SlogValueLimit {
			return text[:i]
		}
		n++
	}
	return text
}

// runtimePackages is the prefix of the functions of the packages of the runtime.
const runtimePackages = "github.com/jsell-rh/stego/registry/runtime/"

// fromRuntime tells whether the program counter of a record is in a package of the runtime. A record with no
// program counter is not.
func fromRuntime(pc uintptr) bool {
	if pc == 0 {
		return false
	}
	frame, _ := runtime.CallersFrames([]uintptr{pc}).Next()
	return strings.HasPrefix(frame.Function, runtimePackages)
}
