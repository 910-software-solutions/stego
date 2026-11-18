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

// stegoCauseClass gives the fixed class of a cause of the operating system that a start meets, such as a listen port
// that another process holds, and empty for any other cause. It never holds the text of the cause (walkthrough 8,
// W8-B5).
func stegoCauseClass(err error) string {
 switch {
 case errors.Is(err, syscall.EADDRINUSE): return "address_in_use"
 case errors.Is(err, syscall.EADDRNOTAVAIL): return "address_not_available"
 case errors.Is(err, syscall.EACCES): return "permission_denied"
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
 if errors.As(err, &classified) && stegoFaultName(classified.FaultClass()) && stegoFaultName(classified.FaultObject()) {
  fault, object = classified.FaultClass(), classified.FaultObject()
 }
 record := struct {
  Timestamp string ` + "`json:\"timestamp\"`" + `
  Severity string ` + "`json:\"severity\"`" + `
  Event string ` + "`json:\"event.name\"`" + `
  Message string ` + "`json:\"message\"`" + `
  Stage string ` + "`json:\"stage\"`" + `
  Tasks []string ` + "`json:\"tasks,omitempty\"`" + `
  AbortedTasks []string ` + "`json:\"aborted_tasks,omitempty\"`" + `
  Modes []string ` + "`json:\"modes,omitempty\"`" + `
  Fault string ` + "`json:\"fault,omitempty\"`" + `
  Object string ` + "`json:\"object,omitempty\"`" + `
  Cause string ` + "`json:\"cause,omitempty\"`" + `
 }{time.Now().UTC().Format(time.RFC3339Nano), "ERROR", "service.failed", "Service failed", stage, tasks, abortedTasks, modes, fault, object, stegoCauseClass(err)}
 done := make(chan struct{})
 go func() {
  defer close(done)
  json.NewEncoder(output).Encode(record)
 }()
 timer := time.NewTimer(time.Second)
 defer timer.Stop()
 select {
 case <-done:
 case <-timer.C:
 }
}
`
