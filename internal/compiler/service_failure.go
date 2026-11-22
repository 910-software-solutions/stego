package compiler

import (
	"fmt"
	"regexp"
)

// componentName is the form of a registry component name that a stage may hold.
var componentName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// componentStage is the stage of a step of one component in main.go, such as component.stego-runtime.constructor[0]:
// it names the component, so the failure record of a refused start tells which component refused it, with no error
// text (ST-GEN-INV16). A name outside the form of a registry component name gives the index of the component.
func componentStage(index int, name, step string, stepIndex int) string {
	if componentName.MatchString(name) {
		return fmt.Sprintf("component.%s.%s[%d]", name, step, stepIndex)
	}
	return fmt.Sprintf("component[%d].%s[%d]", index, step, stepIndex)
}

// The process boundary must not format error causes. Driver and component
// errors can contain credentials, query values, paths, or provider responses.
const serviceFailureSource = `

type stegoServiceFailure struct {
 stage string
 cause error
 tasks []string
 abortedTasks []string
 modes []string
}

func (e *stegoServiceFailure) Error() string { return "service failed at " + e.stage }
func (e *stegoServiceFailure) Unwrap() error { return e.cause }

// stegoFaultName tells whether a fault class or object has the form of a name of the generated code,
// ^[a-z][a-z0-9_.]{0,127}$; the record drops any other text.
func stegoFaultName(value string) bool {
 if len(value) == 0 || len(value) > 128 || value[0] < 'a' || value[0] > 'z' { return false }
 for i := 1; i < len(value); i++ {
  c := value[i]
  if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '.') { return false }
 }
 return true
}

// stegoRemedyText tells whether a remedy has the form of a fixed text of the generated code: 1 to 512 bytes of
// printable ASCII, so no line feed or other control character reaches the record; the record drops any other text.
func stegoRemedyText(value string) bool {
 if len(value) == 0 || len(value) > 512 { return false }
 for i := 0; i < len(value); i++ {
  if value[i] < 0x20 || value[i] > 0x7e { return false }
 }
 return true
}

// stegoSQLState gives the SQLSTATE of a cause of the database, such as 55P03, when it has the form of one: five
// characters of 0-9 and A-Z. Any other cause gives empty.
func stegoSQLState(err error) string {
 var coded interface{ SQLState() string }
 if !errors.As(err, &coded) { return "" }
 code := coded.SQLState()
 if len(code) != 5 { return "" }
 for i := 0; i < len(code); i++ {
  if c := code[i]; !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') { return "" }
 }
 return code
}

// stegoCauseClass gives the fixed class of a cause that a start or the migration job meets: of the database by its
// SQLSTATE, such as lock_timeout for 55P03 (#423), of the operating system, such as a listen port that another
// process holds (walkthrough 8, W8-B5), of the network, of a file and of a certificate; the class that the cause
// gives itself (CauseClass) in the strict form of a name; and empty for any other cause. It never holds the text of
// the cause.
func stegoCauseClass(err error) string {
 if code := stegoSQLState(err); code != "" {
  switch {
  case code == "55P03": return "lock_timeout"
  case code == "57014": return "statement_timeout"
  case code == "40P01": return "deadlock"
  case code == "40001": return "serialization_failure"
  case code == "42501": return "not_granted"
  case code == "53300": return "too_many_connections"
  case code == "57P03": return "database_starting"
  // The server ended the session: an operator stopped it, or it restarts after a crash of one of its processes
  // (walkthrough 21, U-F).
  case code == "57P01": return "admin_shutdown"
  case code == "57P02": return "crash_shutdown"
  case code == "3D000": return "database_not_found"
  case code[:2] == "28": return "authentication_failed"
  case code[:2] == "08": return "connection_failed"
  case code[:2] == "53": return "insufficient_resources"
  case code[:2] == "23": return "constraint_violation"
  case code[:2] == "42": return "schema"
  }
  return "database"
 }
 var dns *net.DNSError
 var unknownAuthority x509.UnknownAuthorityError
 var invalid x509.CertificateInvalidError
 var hostname x509.HostnameError
 switch {
 case errors.Is(err, syscall.EADDRINUSE): return "address_in_use"
 case errors.Is(err, syscall.EADDRNOTAVAIL): return "address_not_available"
 case errors.Is(err, syscall.ECONNREFUSED): return "connection_refused"
 case errors.Is(err, syscall.ECONNRESET): return "connection_reset"
 // A connection that ended in the middle of a message, as when the database server restarts (walkthrough 21, U-F).
 case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF): return "connection_closed"
 case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH): return "network_unreachable"
 case errors.Is(err, syscall.EACCES), errors.Is(err, fs.ErrPermission): return "permission_denied"
 case errors.As(err, &dns): return "name_not_resolved"
 case errors.As(err, &unknownAuthority), errors.As(err, &invalid), errors.As(err, &hostname): return "certificate_refused"
 case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded): return "timeout"
 case errors.Is(err, fs.ErrNotExist): return "file_not_found"
 }
 // A cause that the standard library does not class may give its own fixed class, such as unavailable for a
 // SpiceDB that refused the connection of the migration job (walkthrough 19, small items); the record takes it
 // only in the strict form of a name of the generated code.
 var classed interface{ CauseClass() string }
 if errors.As(err, &classed) {
  // One call: a second call could give another value than the one checked.
  if class := classed.CauseClass(); stegoFaultName(class) { return class }
 }
 return ""
}

// stegoReportFailure writes one fixed record before process exit. The stage
// comes from generated code. The cause is never formatted or serialized.
// A blocked output can retain one worker until the process exits.
func stegoReportFailure(output io.Writer, err error) {
 stage := "service.run"
 var tasks, abortedTasks, modes []string
 if failure, ok := err.(*stegoServiceFailure); ok { stage = failure.stage; tasks = failure.tasks; abortedTasks = failure.abortedTasks; modes = failure.modes }
 // A refused schema check names its fault class and the generated object, two fixed names of the generated code
 // and never the text of the cause (ST-SUP-RC02-OC57).
 var fault, object string
 var classified interface{ FaultClass() string; FaultObject() string }
 if errors.As(err, &classified) {
  // One call of each: a second call could give another value than the one checked.
  if class, named := classified.FaultClass(), classified.FaultObject(); stegoFaultName(class) && stegoFaultName(named) {
   fault, object = class, named
  }
 }
 // A fault that knows its remedy names it, a fixed text of the generated code such as the section of the upgrade
 // guide that moves a database to the role layout of the release (walkthrough 11, W11-B1). It is never the text of
 // the cause.
 var remedy string
 var remedied interface{ FaultRemedy() string }
 if fault != "" && errors.As(err, &remedied) {
  if text := remedied.FaultRemedy(); stegoRemedyText(text) { remedy = text }
 }
 // The record has the one form of each line of the runtime (walkthrough 25, L-1): the timestamp in UTC, the
 // severity, the service name, the event name and the message, and its fields under attributes with the prefix
 // stego.; a list is one text with its names joined by commas.
 service := stegoServiceName
 if service == "" { service = "unknown_service" }
 text := func(b []byte, value string) []byte { encoded, _ := json.Marshal(value); return append(b, encoded...) }
 line := append([]byte(` + "`" + `{"timestamp":` + "`" + `), text(nil, time.Now().UTC().Format(time.RFC3339Nano))...)
 line = append(line, ` + "`" + `,"severity":"ERROR","service.name":` + "`" + `...)
 line = text(line, service)
 line = append(line, ` + "`" + `,"event.name":"service.failed","message":"Service failed","attributes":{"stego.stage":` + "`" + `...)
 line = text(line, stage)
 for _, attr := range []struct{ key, value string }{{"stego.tasks", strings.Join(tasks, ",")}, {"stego.aborted_tasks", strings.Join(abortedTasks, ",")},
  {"stego.modes", strings.Join(modes, ",")}, {"stego.fault", fault}, {"stego.object", object}, {"stego.remedy", remedy},
  {"stego.cause", stegoCauseClass(err)}, {"stego.sqlstate", stegoSQLState(err)}} {
  if attr.value == "" { continue }
  line = append(line, ',')
  line = text(line, attr.key)
  line = append(line, ':')
  line = text(line, attr.value)
 }
 line = append(line, "}}\n"...)
 done := make(chan struct{})
 go func() {
  defer close(done)
  if _, werr := output.Write(line); werr != nil { return }
 }()
 timer := time.NewTimer(time.Second)
 defer timer.Stop()
 select {
 case <-done:
 case <-timer.C:
 }
}
`
