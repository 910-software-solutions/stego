package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type privateFailure struct{}

func (privateFailure) Error() string { panic("error formatting exposed private data") }

type classifiedFailure struct{ class, object string }

func (classifiedFailure) Error() string         { panic("error formatting exposed private data") }
func (f classifiedFailure) FaultClass() string  { return f.class }
func (f classifiedFailure) FaultObject() string { return f.object }

func TestFailureRecordFaultClass(t *testing.T) {
	t.Run("ST-SUP-RC02-OC57-T02 The failure record names a fault class and object of the strict form and drops any other", func(t *testing.T) {
		for _, c := range []struct {
			cause                 classifiedFailure
			wantFault, wantObject string
		}{
			{classifiedFailure{"constraint", "stego_browser_sessions_payload_check"}, "constraint", "stego_browser_sessions_payload_check"},
			{classifiedFailure{"column", "public.stego_browser_sessions.changed_at"}, "column", "public.stego_browser_sessions.changed_at"},
			{classifiedFailure{"table", "password=secret"}, "", ""},
			{classifiedFailure{"Table", "x"}, "", ""},
			{classifiedFailure{"table\n", "x"}, "", ""},
			{classifiedFailure{"table", strings.Repeat("a", 129)}, "", ""},
			{classifiedFailure{"", "x"}, "", ""},
		} {
			var output bytes.Buffer
			stegoReportFailure(&output, &stegoServiceFailure{stage: "database.schema", cause: c.cause})
			var record map[string]any
			if e := failureRecord(output.Bytes(), &record); e != nil {
				t.Fatal(e)
			}
			fault, _ := record["fault"].(string)
			object, _ := record["object"].(string)
			if fault != c.wantFault || object != c.wantObject || record["stage"] != "database.schema" {
				t.Fatalf("the cause %q %q gave the record %v, want the fault %q and the object %q", c.cause.class, c.cause.object, record, c.wantFault, c.wantObject)
			}
		}
	})
}

// remediedFailure is a classified failure that also names its remedy.
type remediedFailure struct {
	classifiedFailure
	remedy string
}

func (f remediedFailure) FaultRemedy() string { return f.remedy }

func TestFailureRecordRemedy(t *testing.T) {
	t.Run("ST-SUP-RC02-OC57-T03 The failure record names the remedy of a classified fault, a fixed text of printable ASCII, and drops any other", func(t *testing.T) {
		const remedy = "do the steps of docs/upgrade.md#move-a-browser-service-database-to-the-owner-and-migrator-logins"
		for _, c := range []struct {
			cause      error
			wantRemedy string
		}{
			{remediedFailure{classifiedFailure{"owner", "public.stego_browser_sessions"}, remedy}, remedy},
			{remediedFailure{classifiedFailure{"owner", "public.stego_browser_sessions"}, remedy + "\nforged line"}, ""},
			{remediedFailure{classifiedFailure{"owner", "public.stego_browser_sessions"}, "caf\u00e9"}, ""},
			{remediedFailure{classifiedFailure{"owner", "public.stego_browser_sessions"}, strings.Repeat("a", 513)}, ""},
			{remediedFailure{classifiedFailure{"table", "public.stego_browser_sessions"}, ""}, ""},
			// A remedy with no valid class is dropped with the class, so the record never names a remedy alone.
			{remediedFailure{classifiedFailure{"Owner", "x"}, remedy}, ""},
		} {
			var output bytes.Buffer
			stegoReportFailure(&output, &stegoServiceFailure{stage: "database.schema", cause: c.cause})
			var record map[string]any
			if e := failureRecord(output.Bytes(), &record); e != nil {
				t.Fatal(e)
			}
			got, _ := record["remedy"].(string)
			if got != c.wantRemedy {
				t.Fatalf("the cause %#v gave the record %v, want the remedy %q", c.cause, record, c.wantRemedy)
			}
			if bytes.Count(output.Bytes(), []byte("\n")) != 1 {
				t.Fatalf("the record is not one line: %q", output.String())
			}
		}
	})
}

func TestFailureRecordCauseClass(t *testing.T) {
	t.Run("ST-GEN-INV16-T a listen port that another process holds names the cause class address_in_use and no error text", func(t *testing.T) {
		held, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		_, cause := net.Listen("tcp", held.Addr().String())
		if cause == nil {
			t.Fatal("a second listen on a held port was taken")
		}
		var output bytes.Buffer
		stegoReportFailure(&output, &stegoServiceFailure{stage: "http.listen", cause: cause})
		var record map[string]any
		if e := failureRecord(output.Bytes(), &record); e != nil {
			t.Fatal(e)
		}
		if record["stage"] != "http.listen" || record["cause"] != "address_in_use" || strings.Contains(output.String(), held.Addr().String()) {
			t.Fatalf("a held listen port gave the record %v, want the stage http.listen and the cause address_in_use with no address", record)
		}
	})
}

// sqlFailure is a cause of the database with its SQLSTATE, as pgconn.PgError gives it, whose text holds a secret.
type sqlFailure struct{ code string }

func (sqlFailure) Error() string      { panic("error formatting exposed private data") }
func (f sqlFailure) SQLState() string { return f.code }

func TestFailureRecordDatabaseCause(t *testing.T) {
	t.Run("ST-GEN-INV16-T a failed step of the migration job or the start names the cause class and the SQLSTATE and no error text", func(t *testing.T) {
		for _, c := range []struct {
			code, cause, sqlstate string
		}{
			{"55P03", "lock_timeout", "55P03"}, {"57014", "statement_timeout", "57014"}, {"40P01", "deadlock", "40P01"},
			{"42501", "not_granted", "42501"}, {"28P01", "authentication_failed", "28P01"}, {"08006", "connection_failed", "08006"},
			{"3D000", "database_not_found", "3D000"}, {"23505", "constraint_violation", "23505"}, {"42P01", "schema", "42P01"},
			{"57P01", "admin_shutdown", "57P01"}, {"57P02", "crash_shutdown", "57P02"},
			{"XX000", "database", "XX000"}, {"55p03", "", ""}, {"password=secret", "", ""}, {"", "", ""},
		} {
			var output bytes.Buffer
			// The migration job wraps the cause of a step, as service.StepError does.
			cause := fmt.Errorf("migrate: tables: %w", sqlFailure{c.code})
			stegoReportFailure(&output, &stegoServiceFailure{stage: "migrate.tables", cause: cause})
			var record map[string]any
			if e := failureRecord(output.Bytes(), &record); e != nil {
				t.Fatal(e)
			}
			got, _ := record["cause"].(string)
			sqlstate, _ := record["sqlstate"].(string)
			if record["stage"] != "migrate.tables" || got != c.cause || sqlstate != c.sqlstate || strings.Contains(output.String(), "secret") {
				t.Fatalf("the SQLSTATE %q gave the record %v, want the cause %q and the sqlstate %q", c.code, record, c.cause, c.sqlstate)
			}
		}
	})
	t.Run("ST-GEN-INV16-T a start step that a network, a file or a certificate refused names the cause class and no error text", func(t *testing.T) {
		held, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := held.Addr().String()
		held.Close()
		_, refused := net.Dial("tcp", address)
		if refused == nil {
			t.Fatal("a dial of a closed port was taken")
		}
		_, missing := os.Open(filepath.Join(t.TempDir(), "upstream-ca.pem"))
		for _, c := range []struct {
			cause error
			want  string
		}{
			{refused, "connection_refused"},
			{&net.DNSError{Err: "no such host", Name: "upstream.invalid", IsNotFound: true}, "name_not_resolved"},
			{fmt.Errorf("client: %w", missing), "file_not_found"},
			{x509.UnknownAuthorityError{}, "certificate_refused"},
			{fmt.Errorf("discovery: %w", context.DeadlineExceeded), "timeout"},
			{fmt.Errorf("tenant: open the database: %w", io.ErrUnexpectedEOF), "connection_closed"},
			{privateFailure{}, ""},
			// A cause that gives its own class, such as a refused call to SpiceDB, names it in the strict form only.
			{fmt.Errorf("migrate: %w", classedFailure{"unavailable"}), "unavailable"},
			{classedFailure{"Connection refused by upstream.invalid"}, ""},
			{classedFailure{""}, ""},
		} {
			var output bytes.Buffer
			stegoReportFailure(&output, &stegoServiceFailure{stage: "browser.upstream", cause: c.cause})
			var record map[string]any
			if e := failureRecord(output.Bytes(), &record); e != nil {
				t.Fatal(e)
			}
			got, _ := record["cause"].(string)
			if record["stage"] != "browser.upstream" || got != c.want || strings.Contains(output.String(), "upstream.invalid") ||
				strings.Contains(output.String(), address) || strings.Contains(output.String(), "upstream-ca.pem") {
				t.Fatalf("the cause %T gave the record %v, want the cause class %q with no error text", c.cause, record, c.want)
			}
		}
	})
}

func TestFailureRecordDoesNotFormatCause(t *testing.T) {
	t.Run("ST-GEN-INV16-T the record never formats its cause and keeps it for errors.Is and errors.As", func(t *testing.T) {
		cause := privateFailure{}
		err := &stegoServiceFailure{stage: "database.open", cause: cause}
		if !errors.Is(err, cause) {
			t.Fatal("failure lost its cause")
		}
		var target privateFailure
		if !errors.As(err, &target) {
			t.Fatal("failure lost its type")
		}
		if got := err.Error(); got != "service failed at database.open" {
			t.Fatal("invalid safe error", got)
		}
		for _, input := range []error{err, cause} {
			var output bytes.Buffer
			stegoReportFailure(&output, input)
			var record map[string]any
			if e := failureRecord(output.Bytes(), &record); e != nil {
				t.Fatal(e)
			}
			if len(record) != 6 || record["service.name"] != "unknown_service" || record["event.name"] != "service.failed" || record["severity"] != "ERROR" || record["message"] != "Service failed" {
				t.Fatal("invalid failure record", record)
			}
			stage := "service.run"
			if input == err {
				stage = "database.open"
			}
			if record["stage"] != stage {
				t.Fatal("incorrect stage", record)
			}
			if _, e := time.Parse(time.RFC3339Nano, record["timestamp"].(string)); e != nil {
				t.Fatal(e)
			}
		}
		// The modes that the process runs are data of the record, with no error text (ST-GEN-INV26).
		var output bytes.Buffer
		stegoReportFailure(&output, &stegoServiceFailure{stage: "service.start.database", cause: cause, modes: []string{"api", "worker"}})
		var record map[string]any
		if e := failureRecord(output.Bytes(), &record); e != nil {
			t.Fatal(e)
		}
		if modes, _ := json.Marshal(record["modes"]); len(record) != 7 || string(modes) != `["api","worker"]` || record["stage"] != "service.start.database" {
			t.Fatal("invalid failure record with modes", record)
		}
	})
}

type blockedFailureOutput struct{ entered, release, done chan struct{} }

func (w *blockedFailureOutput) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	defer close(w.done)
	return len(p), nil
}

func TestFailureOutputHasDeadline(t *testing.T) {
	t.Run("ST-GEN-INV16-T the write of the record has a deadline", func(t *testing.T) {
		writer := &blockedFailureOutput{make(chan struct{}), make(chan struct{}), make(chan struct{})}
		returned := make(chan struct{})
		go func() { stegoReportFailure(writer, privateFailure{}); close(returned) }()
		defer func() { close(writer.release); <-writer.done }()
		<-writer.entered
		select {
		case <-returned:
		// The deadline is one second (ST-GEN-INV16); 1.5 s leaves room for a slow run and refuses a longer one.
		case <-time.After(1500 * time.Millisecond):
			t.Fatal("failure output blocked exit")
		}
	})
}

type failedFailureOutput struct{ calls int }

func (w *failedFailureOutput) Write([]byte) (int, error) { w.calls++; return 0, io.ErrClosedPipe }
func TestFailureOutputDoesNotRetry(t *testing.T) {
	t.Run("ST-GEN-INV16-T the write of the record is not retried", func(t *testing.T) {
		writer := &failedFailureOutput{}
		stegoReportFailure(writer, privateFailure{})
		if writer.calls != 1 {
			t.Fatal("failure output retried")
		}
	})
}

func TestFailureTaskNamesAreData(t *testing.T) {
	t.Run("ST-GEN-INV16-T the stage names of the record are data", func(t *testing.T) {
		var output bytes.Buffer
		stegoReportFailure(&output, &stegoServiceFailure{stage: "service.run", tasks: []string{"worker[0]"}, cause: privateFailure{}})
		if !strings.Contains(output.String(), `"stego.tasks":"worker[0]"`) {
			t.Fatal("missing task name")
		}
	})
}

// classedFailure is a cause that gives its own cause class and whose text holds a value.
type classedFailure struct{ class string }

func (c classedFailure) Error() string      { return "dial upstream.invalid: " + c.class }
func (c classedFailure) CauseClass() string { return c.class }

// failureRecord checks a record against the one form of the lines of the runtime (CheckLine; walkthrough 25, L-1) and
// decodes it with its attributes at the top level by their names, such as stage for stego.stage, and the lists
// stego.tasks, stego.aborted_tasks and stego.modes as lists.
func failureRecord(data []byte, record *map[string]any) error {
	if err := CheckLine(data); err != nil {
		return err
	}
	if err := json.Unmarshal(data, record); err != nil {
		return err
	}
	attrs, _ := (*record)["attributes"].(map[string]any)
	delete(*record, "attributes")
	for key, value := range attrs {
		name := strings.TrimPrefix(key, "stego.")
		if text, ok := value.(string); ok && (name == "tasks" || name == "aborted_tasks" || name == "modes") {
			var list []any
			for _, item := range strings.Split(text, ",") {
				list = append(list, item)
			}
			value = list
		}
		(*record)[name] = value
	}
	return nil
}
