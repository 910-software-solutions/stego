// Package oteldeadline carries the deadline function of the shutdown bound of a telemetry runtime of package otel
// (ST-SUP-RC17-OC29). The runtime uses context.WithTimeout, which reads the real clock. A test of another package of
// the runtime module, such as the probe runtime, gives a deadline function that it ends itself, so it proves that
// the probe runtime and the telemetry runtime share one shutdown limit with no wall-clock limit. This package is
// internal to the module, so the code that a service generates cannot change the bound.
package oteldeadline

import (
	"context"
	"time"
)

// Deadline gives a context that ends d after it is made, or when parent ends.
type Deadline func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc)

// set is the setter that the otel package registers when it starts.
var set func(runtime any, bound Deadline) bool

// Register sets the setter. Only the otel package calls it, once, from its init.
func Register(setter func(runtime any, bound Deadline) bool) {
	if set == nil {
		set = setter
	}
}

// Set gives runtime, a *otel.Runtime that has not shut down, the deadline function of its shutdown bound, and
// reports false for any other value. The test calls it before the runtime shuts down.
func Set(runtime any, bound Deadline) bool {
	if set == nil || runtime == nil || bound == nil {
		return false
	}
	return set(runtime, bound)
}
