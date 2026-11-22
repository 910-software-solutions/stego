// Package logline is the one form of each JSON line that a process of the runtime writes on its standard output and
// standard error (ST-SUP-RP03-LC16; walkthrough 25, L-1): the console of the log provider, the log/slog handler, the
// console of the probe and the local log of the controller write each line with it, and Check reads a line against
// the form. A line is one JSON object on one line, with these members in this order:
//
//	timestamp            UTC, RFC 3339 with nanoseconds, such as 2026-11-22T07:29:43.822602285Z
//	severity             DEBUG, INFO, WARN, ERROR or FATAL
//	service.name         the name of the service
//	service.instance.id  the identity of the writer in the process, when the writer knows it
//	event.name           the event name, such as worker.run.failed
//	message              a fixed text
//	trace_id, span_id    the trace and the span of the line, when it has them
//	attributes           the attributes, when the line has them
//
// Each key of attributes has the prefix stego., or it is a name of the OpenTelemetry semantic conventions for HTTP
// and RPC (http.*, rpc.*, error.type). A value is a string, a number or a bool. The file uses the standard library
// only, so a test of the generated code can read a copy of Check (internal/generator/oteltracing).
package logline

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Head is the part of a line before its attributes.
type Head struct {
	Time     time.Time
	Severity string
	Service  string
	Instance string
	Event    string
	Message  string
	TraceID  string
	SpanID   string
}

// UnknownService is the service name of a line whose writer does not know the name yet, as the OpenTelemetry
// resource names it.
const UnknownService = "unknown_service"

// AppendHead appends the start of the line of h to b: the members before attributes. A zero time is the time of the
// call, and an empty service name is UnknownService.
func AppendHead(b []byte, h Head) []byte {
	at := h.Time
	if at.IsZero() {
		at = time.Now()
	}
	service := h.Service
	if service == "" {
		service = UnknownService
	}
	b = append(b, `{"timestamp":`...)
	b = AppendText(b, at.UTC().Format(time.RFC3339Nano))
	b = append(b, `,"severity":`...)
	b = AppendText(b, h.Severity)
	b = append(b, `,"service.name":`...)
	b = AppendText(b, service)
	if h.Instance != "" {
		b = append(b, `,"service.instance.id":`...)
		b = AppendText(b, h.Instance)
	}
	b = append(b, `,"event.name":`...)
	b = AppendText(b, h.Event)
	b = append(b, `,"message":`...)
	b = AppendText(b, h.Message)
	if h.TraceID != "" && h.SpanID != "" {
		b = append(b, `,"trace_id":`...)
		b = AppendText(b, h.TraceID)
		b = append(b, `,"span_id":`...)
		b = AppendText(b, h.SpanID)
	}
	return b
}

// AppendKey appends the key of the attribute number n of the line (0 for the first), which opens the member
// attributes, and the colon. The caller appends the value.
func AppendKey(b []byte, n int, key string) []byte {
	if n == 0 {
		b = append(b, `,"attributes":{`...)
	} else {
		b = append(b, ',')
	}
	b = AppendText(b, Key(key))
	return append(b, ':')
}

// AppendEnd appends the end of a line with n attributes and the line feed.
func AppendEnd(b []byte, n int) []byte {
	if n > 0 {
		b = append(b, '}')
	}
	return append(b, '}', '\n')
}

// Key gives the key of an attribute on the line: the key itself when it has the prefix stego. or is a name of the
// semantic conventions for HTTP and RPC (http.*, rpc.*, error.type), and else the key with the prefix stego., such
// as stego.worker for worker.
func Key(key string) string {
	switch {
	case strings.HasPrefix(key, "stego."), strings.HasPrefix(key, "http."), strings.HasPrefix(key, "rpc."), key == "error.type":
		return key
	}
	return "stego." + key
}

// AppendText appends text as a JSON string. An invalid UTF-8 byte becomes U+FFFD, and a control character, the line
// separator and the paragraph separator are escaped, so the line is one line.
func AppendText(b []byte, text string) []byte {
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
			b = append(b, "�"...)
		case r == ' ' || r == ' ':
			b = append(b, '\\', 'u', '2', '0', '2', hex[r&0xf])
		default:
			b = append(b, text[i:i+size]...)
		}
		i += size
	}
	return append(b, '"')
}

// The members of a line, in their order, and whether each must be there.
var members = []struct {
	name     string
	required bool
}{
	{"timestamp", true}, {"severity", true}, {"service.name", true}, {"service.instance.id", false},
	{"event.name", true}, {"message", true}, {"trace_id", false}, {"span_id", false}, {"attributes", false},
}

var (
	eventForm   = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$`)
	traceForm   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanForm    = regexp.MustCompile(`^[0-9a-f]{16}$`)
	stampForm   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,9})?Z$`)
	severitySet = map[string]bool{"DEBUG": true, "INFO": true, "WARN": true, "ERROR": true, "FATAL": true}
)

// Check tells whether line is one line of the form of the package: one JSON object and one line feed at its end,
// with the members in their order, no other member and no key twice. Its error names the member that is wrong, and
// never a value of the line.
func Check(line []byte) error {
	if len(line) == 0 || line[len(line)-1] != '\n' || bytes.Count(line, []byte{'\n'}) != 1 {
		return errors.New("logline: a line is one JSON object and one line feed")
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errors.New("logline: a line is a JSON object")
	}
	next := 0
	var trace, span bool
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return errors.New("logline: the line is not JSON")
		}
		name, _ := t.(string)
		at := -1
		for i := next; i < len(members); i++ {
			if members[i].name == name {
				at = i
				break
			}
		}
		if at < 0 {
			return errors.New("logline: the member " + strconv.Quote(name) + " is not of the form, or it is out of order or twice")
		}
		for i := next; i < at; i++ {
			if members[i].required {
				return errors.New("logline: the line has no member " + members[i].name)
			}
		}
		next = at + 1
		if name == "attributes" {
			if err := checkAttributes(dec); err != nil {
				return err
			}
			continue
		}
		var value string
		if err := dec.Decode(&value); err != nil {
			return errors.New("logline: the member " + name + " is not a string")
		}
		ok := true
		switch name {
		case "timestamp":
			_, perr := time.Parse(time.RFC3339Nano, value)
			ok = perr == nil && stampForm.MatchString(value)
		case "severity":
			ok = severitySet[value]
		case "service.name", "service.instance.id":
			ok = value != ""
		case "event.name":
			ok = eventForm.MatchString(value)
		case "trace_id":
			ok, trace = traceForm.MatchString(value), true
		case "span_id":
			ok, span = spanForm.MatchString(value), true
		}
		if !ok {
			return errors.New("logline: the member " + name + " does not have its form")
		}
	}
	for i := next; i < len(members); i++ {
		if members[i].required {
			return errors.New("logline: the line has no member " + members[i].name)
		}
	}
	if trace != span {
		return errors.New("logline: a line has trace_id and span_id together, or neither")
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return errors.New("logline: the object does not end")
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("logline: the line has more than one JSON value")
	}
	return nil
}

// checkAttributes reads the object of the attributes: each key once, in the form of Key, and each value a string, a
// number or a bool.
func checkAttributes(dec *json.Decoder) error {
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errors.New("logline: attributes is not an object")
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return errors.New("logline: the attributes are not JSON")
		}
		key, _ := t.(string)
		if key == "" || Key(key) != key || !strings.Contains(key, ".") || seen[key] {
			return errors.New("logline: the attribute " + strconv.Quote(key) + " has no prefix stego. and no name of the semantic conventions, or it is there twice")
		}
		seen[key] = true
		v, err := dec.Token()
		if err != nil {
			return errors.New("logline: the value of an attribute is not JSON")
		}
		switch v.(type) {
		case string, json.Number, bool:
		default:
			return errors.New("logline: the value of the attribute " + key + " is not a string, a number or a bool")
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return errors.New("logline: the attributes do not end")
	}
	if len(seen) == 0 {
		return errors.New("logline: a line with no attributes has no member attributes")
	}
	return nil
}
