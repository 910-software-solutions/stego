package otel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/jsell-rh/stego/registry/runtime/internal/logline"
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
// provider, so that no line of the runtime is plain text. The line has the one form of internal/logline (walkthrough
// 25, L-1): the timestamp in UTC, the severity, the service name and the instance identity (SetIdentity), the event
// name, the message, the trace and the span of the context, and the attributes. The event name is the value of the attribute SlogEventKey when it has the
// form of an event name; else the message, when it has that form, such as command.fill.failed; else SlogEvent. Only a
// line of a package of the runtime names its event, and a line of other code one of GeneratedEvents: another line of
// other code, such as of a fill, gets SlogEvent. Each
// attribute key gets the prefix stego., as the attributes of the runtime's records have it, and the keys of a group
// are joined with a dot. A line holds at most SlogAttributeLimit attributes of at most SlogValueLimit characters each.
// The handler writes the values that the caller gives, so a caller gives no value of a
// setting, no payload and no error text, as for a record of the log provider. One write of out takes each line.
type SlogHandler struct {
	out    *lockedWriter
	attrs  []slog.Attr
	groups []string
	// level is the lowest level that the handler writes; the zero value is slog.LevelInfo.
	level slog.Level
}

// lockedWriter writes each line of all the handlers of one output in one call, one line at a time. It holds the
// identity of the writer that the lines name, which SetIdentity gives once the telemetry runtime started.
type lockedWriter struct {
	mu       sync.Mutex
	out      io.Writer
	identity atomic.Pointer[slogIdentity]
}

// slogIdentity is the service name and the instance identity of the lines of a handler.
type slogIdentity struct{ service, instance string }

// SetIdentity gives the service name and the instance identity that each later line of the handler, and of each
// handler that WithAttrs and WithGroup made from it, names (walkthrough 25, L-1): the start of the service and the
// migration job give those of the telemetry runtime (otel.Runtime.Service and Instance). Before it, a line names the
// service of OTEL_SERVICE_NAME, or unknown_service, and no instance.
func (h *SlogHandler) SetIdentity(service, instance string) {
	h.out.identity.Store(&slogIdentity{service: service, instance: instance})
}

// GeneratedEvents are the event names that a line of log/slog of code outside the runtime may name: the lines of
// the code that STEGO generates, such as the line of the HTTP server of main that listens. A fill may name them too;
// they are INFO lines of the start, and no line of the security class is one of them.
var GeneratedEvents = map[string]bool{"service.listening": true, "service.audit_sink.selected": true}

// NewSlogHandler gives the handler that writes its lines of level INFO or higher on out.
func NewSlogHandler(out io.Writer) *SlogHandler {
	return &SlogHandler{out: &lockedWriter{out: out}}
}

// NewSlogHandlerAt gives the handler that writes its lines of level or higher on out, such as the level of
// STEGO_LOG_LEVEL that ParseLogLevel gives (decision on ST-SUP-Q210).
func NewSlogHandlerAt(out io.Writer, level slog.Level) *SlogHandler {
	return &SlogHandler{out: &lockedWriter{out: out}, level: level}
}

// Enabled takes each record of the level of the handler or higher: INFO or higher when no level is given; and each
// record of INFO or higher of a context that Security marks, at each level.
func (h *SlogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.level || (level >= slog.LevelInfo && ctx != nil && ctx.Value(securityKey{}) != nil)
}

type securityKey struct{}

// Security marks ctx for a line of the security class, which the handler writes at each level of STEGO_LOG_LEVEL,
// as the console writes the service events (security review of PR #492, L3): such as the line of another entry
// under the ID of an audit entry, of a stored answer that does not open, of a decision that the authorization
// provider could not make, and the seal and re-seal pass lines. docs/operations.md lists the class.
func Security(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, securityKey{}, true)
}

// LogLevelSetting is the setting of the lowest severity of the log lines that a process of the runtime writes on its
// standard output and standard error: debug, info, warn or error, and info when it has no value (decision on
// ST-SUP-Q210). The records that go to OTLP do not depend on it, as a collector filters them by severity.
const LogLevelSetting = "STEGO_LOG_LEVEL"

// ParseLogLevel gives the level of a value of LogLevelSetting: debug, info, warn or error, and info for an empty
// value. Any other value fails with fixed text, which does not hold the value.
func ParseLogLevel(value string) (slog.Level, error) {
	switch value {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, errors.New("otel: STEGO_LOG_LEVEL must be debug, info, warn or error")
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
func (h *SlogHandler) Handle(ctx context.Context, record slog.Record) error {
	event := ""
	attrs := append([]slog.Attr{}, h.attrs...)
	var own []slog.Attr
	// Only a line of a package of the runtime names its event: a line of other code, such as a fill, keeps the event
	// name stego.log, so it cannot pass as an event of the runtime (security review of PR #485, I1).
	runtimeLine := fromRuntime(record)
	record.Attrs(func(a slog.Attr) bool {
		if len(h.groups) == 0 && a.Key == SlogEventKey && event == "" {
			if v := a.Value.Resolve(); v.Kind() == slog.KindString && eventName.MatchString(v.String()) &&
				(runtimeLine || GeneratedEvents[v.String()]) {
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
	service, instance := os.Getenv("OTEL_SERVICE_NAME"), ""
	if id := h.out.identity.Load(); id != nil {
		service, instance = id.service, id.instance
	}
	head := logline.Head{Time: record.Time, Severity: slogSeverity(record.Level), Service: service, Instance: instance,
		Event: event, Message: record.Message}
	if ctx != nil {
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			head.TraceID, head.SpanID = sc.TraceID().String(), sc.SpanID().String()
		}
	}
	b := logline.AppendHead(make([]byte, 0, 256), head)
	written := 0
	for _, a := range attrs {
		b = appendSlogAttr(b, "stego", a, &written)
	}
	b = logline.AppendEnd(b, written)
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
// and its key, and an empty attribute gives nothing. written counts the attributes written, which opens the object
// of the attributes at the first.
func appendSlogAttr(b []byte, prefix string, a slog.Attr, written *int) []byte {
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
			b = appendSlogAttr(b, key, member, written)
		}
		return b
	}
	// At most SlogAttributeLimit attributes, as the log provider keeps (security review of PR #485, L2).
	if *written >= SlogAttributeLimit {
		return b
	}
	b = logline.AppendKey(b, *written, key)
	*written++
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

// fromRuntime tells whether a record is of a function of a package of the runtime, by its source (slog.Record.Source,
// with no import of the package runtime, which the fill rules refuse in the closure of a fill). A record with no
// program counter is not.
func fromRuntime(record slog.Record) bool {
	source := record.Source()
	return source != nil && strings.HasPrefix(source.Function, runtimePackages)
}
