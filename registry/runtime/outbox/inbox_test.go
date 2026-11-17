package outbox

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

func TestQueueReceive(t *testing.T) {
	t.Run("ST-SUP-RP01-LC35-T03 Receive writes a message once: a second write of its ID is a no-op that changes nothing", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		ctx := context.Background()
		m := message("order-changes", "order:o-1")
		receive := func(tenantID string, m Message) Received {
			t.Helper()
			tn := testTenant(t, tenantID)
			tx, err := tenant.Begin(ctx, db, tn, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			got, err := q.Receive(ctx, tx, m)
			if err != nil {
				t.Fatal(err)
			}
			if err := tenant.Commit(ctx, tx, tn); err != nil {
				t.Fatal(err)
			}
			return got
		}
		if got := receive("tenant-a", m); got != ReceivedWritten {
			t.Fatalf("the first Receive gave %v, want written", got)
		}
		if got := receive("tenant-a", m); got != ReceivedDuplicate {
			t.Fatalf("a second Receive of the same message gave %v, want duplicate", got)
		}
		changed := m
		changed.Payload = []byte(`{"version":2}`)
		if got := receive("tenant-a", changed); got != ReceivedDiffers {
			t.Fatalf("a Receive of one ID with another payload gave %v, want differs", got)
		}
		if n := count(t, db, "SELECT count(*) FROM stego_outbox.messages WHERE id = $1", m.ID); n != 1 {
			t.Fatalf("%d rows of the message, want 1", n)
		}
		var payload string
		if err := admin(t, db).QueryRow(`SELECT convert_from(payload, 'UTF8') FROM stego_outbox.messages WHERE id = $1`, m.ID).Scan(&payload); err != nil || payload != `{"version":1}` {
			t.Fatalf("the second Receive changed the payload to %s (%v)", payload, err)
		}
		// Enqueue keeps its refusal of an ID that the queue holds.
		tn := testTenant(t, "tenant-a")
		tx, err := tenant.Begin(ctx, db, tn, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if err := q.Enqueue(ctx, tx, m); err == nil {
			t.Fatal("Enqueue wrote a second message with one ID")
		}
	})
}

func TestWorkerAddedPurge(t *testing.T) {
	t.Run("ST-SUP-RP01-LC37-T02 The purge timer runs each added purger in batches until a batch is not full", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		w := newWorker(t, q, func(c *WorkerConfig) { c.PurgeEvery = time.Second })
		var calls, left atomic.Int64
		left.Store(int64(MaxBatchSize) + 3)
		if err := w.AddPurge(func(_ context.Context, limit int) (int64, error) {
			calls.Add(1)
			n := min(left.Load(), int64(limit))
			left.Add(-n)
			return n, nil
		}); err != nil {
			t.Fatal(err)
		}
		failing := errors.New("the table is not there")
		var failed atomic.Int64
		if err := w.AddPurge(func(context.Context, int) (int64, error) { failed.Add(1); return 0, failing }); err != nil {
			t.Fatal(err)
		}
		if err := w.AddPurge(nil); err == nil {
			t.Fatal("AddPurge took a nil purger")
		}
		startWorker(t, w)
		for left.Load() != 0 || calls.Load() < 2 || failed.Load() < 1 {
			time.Sleep(20 * time.Millisecond)
		}
		if err := w.AddPurge(func(context.Context, int) (int64, error) { return 0, nil }); err == nil {
			t.Fatal("AddPurge took a purger after Run started")
		}
		if w.Retention() != DefaultRetention {
			t.Fatalf("Retention is %v, want %v", w.Retention(), DefaultRetention)
		}
	})
}
