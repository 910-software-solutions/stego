// Package authzclock carries the clocks of the authorization runtime: the clock of the revocation retention, and
// the clock of the waits of WaitFor, which bounds a wait and spaces its reads (ST-SUP-RP06-LC18, ST-SUP-RP06-LC24).
// The runtime uses the real clock. A test of the runtime module gives clocks that it moves itself, so it proves the
// retention, the wait cap and the waiter limits with no wall-clock limit. This package is internal to the module,
// so the configuration of package authz has no clock field and the code that a service generates cannot change the
// clocks (review of PR #387, L1).
package authzclock

import (
	"context"
	"time"
)

// Clock is the clock of the revocation retention. A lease.Clock satisfies it.
type Clock interface {
	Now() time.Time
}

// WaitClock is the clock of the waits of WaitFor.
type WaitClock interface {
	// WithTimeout gives a context that ends d after it is made, or when parent ends.
	WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc)
	// After gives a channel that receives the time when d has passed.
	After(d time.Duration) <-chan time.Time
}

// Clocks are the clocks of one authorization runtime. A nil clock is the real clock.
type Clocks struct {
	Retention Clock
	Wait      WaitClock
	// Decision bounds each call of the decision provider with the decision time limit. It is apart from Wait, so a
	// test of the waits sees only their bounds.
	Decision WaitClock
}

// set is the setter that the authz package registers when it starts.
var set func(cfg any, clocks Clocks) bool

// Register sets the setter. Only the authz package calls it, once, from its init.
func Register(setter func(cfg any, clocks Clocks) bool) {
	if set == nil {
		set = setter
	}
}

// Set gives cfg, a *authz.Config, the clocks, and reports false for any other value.
func Set(cfg any, clocks Clocks) bool {
	if set == nil || cfg == nil {
		return false
	}
	return set(cfg, clocks)
}

// setGate is the setter of the clock of a schema gate that the authz package registers when it starts.
var setGate func(gate any, now func() time.Time) bool

// RegisterGate sets the setter of the clock of a schema gate. Only the authz package calls it, once, from its init.
func RegisterGate(setter func(gate any, now func() time.Time) bool) {
	if setGate == nil {
		setGate = setter
	}
}

// SetGate gives gate, a *authz.SchemaGate, the clock now of its digest bound (ST-SUP-RP10-LC59), and reports false
// for any other value.
func SetGate(gate any, now func() time.Time) bool {
	if setGate == nil || gate == nil {
		return false
	}
	return setGate(gate, now)
}
