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

// stegoReportFailure writes one fixed record before process exit. The stage
// comes from generated code. The cause is never formatted or serialized.
// A blocked output can retain one worker until the process exits.
func stegoReportFailure(output io.Writer, err error) {
 stage := "service.run"
 var tasks, abortedTasks, modes []string
 if failure, ok := err.(*stegoServiceFailure); ok { stage = failure.stage; tasks = failure.tasks; abortedTasks = failure.abortedTasks; modes = failure.modes }
 record := struct {
  Timestamp string ` + "`json:\"timestamp\"`" + `
  Severity string ` + "`json:\"severity\"`" + `
  Event string ` + "`json:\"event.name\"`" + `
  Message string ` + "`json:\"message\"`" + `
  Stage string ` + "`json:\"stage\"`" + `
  Tasks []string ` + "`json:\"tasks,omitempty\"`" + `
  AbortedTasks []string ` + "`json:\"aborted_tasks,omitempty\"`" + `
  Modes []string ` + "`json:\"modes,omitempty\"`" + `
 }{time.Now().UTC().Format(time.RFC3339Nano), "ERROR", "service.failed", "Service failed", stage, tasks, abortedTasks, modes}
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
