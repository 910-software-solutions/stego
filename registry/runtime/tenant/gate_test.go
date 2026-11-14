package tenant

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
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

	t.Run("ST-SUP-RC27-OC11-T05 A lookup that read the list before a Retire and its Forget does not store its answer", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		gate, err := NewGate(env.runtimeDB, "public", MaxGateAge)
		if err != nil {
			t.Fatal(err)
		}
		// The lookup reads g5 as active; the Retire commits and Forget runs before the lookup stores its answer.
		g5 := mustRequested(t, "g5-"+strconv.FormatInt(time.Now().UnixNano(), 36))
		if err := Provision(ctx, env.runtimeDB, "public", g5, nil); err != nil {
			t.Fatal(err)
		}
		gate.read = func(string) {
			gate.read = nil
			if err := Retire(ctx, env.runtimeDB, "public", g5, nil); err != nil {
				t.Error(err)
			}
			gate.Forget(g5.ID())
		}
		if ok, err := gate.Active(ctx, g5); err != nil || !ok {
			t.Fatalf("the lookup that read the list before the Retire gave %t, %v; want the active answer it read", ok, err)
		}
		if ok, err := gate.Active(ctx, g5); err != nil || ok {
			t.Fatalf("after the Retire and its Forget the gate gave %t, %v; want the retired tenant refused", ok, err)
		}
		// The same with lookups in flight on four goroutines, each forcing a read of the list as a cold or expired
		// entry does, across the Retire and the Forget that ServeTenant's Changed runs after the commit.
		gate.read = nil
		stale := 0
		const rounds = 300
		for i := range rounds {
			tn := mustRequested(t, fmt.Sprintf("race-%d-%d", time.Now().UnixNano(), i))
			if err := Provision(ctx, env.runtimeDB, "public", tn, nil); err != nil {
				t.Fatal(err)
			}
			var stop atomic.Bool
			var wg sync.WaitGroup
			for range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for !stop.Load() {
						gate.Forget(tn.ID())
						_, _ = gate.Active(ctx, tn)
					}
				}()
			}
			time.Sleep(2 * time.Millisecond)
			if err := Retire(ctx, env.runtimeDB, "public", tn, nil); err != nil {
				t.Fatal(err)
			}
			stop.Store(true)
			gate.Forget(tn.ID())
			wg.Wait()
			if ok, err := gate.Active(ctx, tn); err != nil || ok {
				stale++
			}
		}
		if stale != 0 {
			t.Fatalf("a retired tenant was active in the cache after its Retire and Forget in %d of %d rounds", stale, rounds)
		}
	})
}
