package tenant

import (
	"context"
	"testing"
	"time"
)

func TestGate(t *testing.T) {
	t.Run("ST-SUP-RC27-OC11-T03 The tenant gate answers from its cache for less than its age, at most 5 s, and then reads the list again", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		if _, err := NewGate(env.runtimeDB, "public", MaxGateAge+time.Millisecond); err == nil {
			t.Fatal("a gate whose answers may be older than 5 s was made")
		}
		if _, err := NewGate(env.runtimeDB, "public", 0); err == nil {
			t.Fatal("a gate with no age was made")
		}
		gate, err := NewGate(env.runtimeDB, "public", MaxGateAge)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Unix(1_800_000_000, 0)
		gate.now = func() time.Time { return now }
		active := func(id string) bool {
			t.Helper()
			ok, err := gate.Active(ctx, mustRequested(t, id))
			if err != nil {
				t.Fatal(err)
			}
			return ok
		}
		if active("g1") {
			t.Fatal("a tenant that the list does not hold is active")
		}
		if err := Provision(ctx, env.runtimeDB, "public", mustRequested(t, "g1"), nil); err != nil {
			t.Fatal(err)
		}
		// The cached answer stands for less than the age, and the list is read again at the age.
		now = now.Add(MaxGateAge - time.Millisecond)
		if active("g1") {
			t.Fatal("the gate read the list again before its age ended")
		}
		now = now.Add(time.Millisecond)
		if !active("g1") {
			t.Fatal("the gate gave a cached answer at its age, or the provisioned tenant is not active")
		}
		if err := Retire(ctx, env.runtimeDB, "public", mustRequested(t, "g1"), nil); err != nil {
			t.Fatal(err)
		}
		if !active("g1") {
			t.Fatal("the gate read the list again before its age ended")
		}
		// Forget, which Provision and Retire call on their replica, drops the cached answer at once.
		gate.Forget("g1")
		if active("g1") {
			t.Fatal("a retired tenant is active after Forget")
		}
		if _, err := gate.Active(ctx, Tenant{}); err == nil {
			t.Fatal("the gate took the zero tenant")
		}
		// A failed read of the list is an error, not an answer, so the caller refuses the request.
		broken, err := NewGate(env.runtimeDB, "no_such_schema", MaxGateAge)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := broken.Active(ctx, mustRequested(t, "g2")); err == nil || ok {
			t.Fatalf("the gate on a schema with no tenant list gave %t, %v; want an error", ok, err)
		}
		// The cache holds at most MaxGateEntries answers.
		for i := range MaxGateEntries + 1 {
			gate.entries[string(rune('a'+i%26))+time.Duration(i).String()] = gateEntry{at: now}
			if len(gate.entries) > MaxGateEntries {
				break
			}
		}
		active("g3")
		if len(gate.entries) > MaxGateEntries {
			t.Fatalf("the cache holds %d answers, more than %d", len(gate.entries), MaxGateEntries)
		}
	})
}
