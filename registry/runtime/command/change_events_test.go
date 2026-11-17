package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/outbox/kafkaoutbox"
)

// changeEventsOf gives the change events of each named entity as the generator names them for the service orders,
// such as orders.order.created for the entity Order (ST-SUP-RC20-OC40).
func changeEventsOf(entities ...string) []ChangeEvents {
	events := make([]ChangeEvents, len(entities))
	for i, entity := range entities {
		name := "orders." + strings.ToLower(entity)
		events[i] = ChangeEvents{Entity: entity, Created: name + ".created", Updated: name + ".updated", Deleted: name + ".deleted"}
	}
	return events
}

// stagedRow is one outbox message as the test reads it.
type stagedRow struct {
	destination, kind, resourceKey, payload, outgoing string
	idempotent, mustLand                              bool
}

// String is the row in the short form that the tests compare: the destination, the kind and the resource key.
func (m stagedRow) String() string { return m.destination + " " + m.kind + " " + m.resourceKey }

// staged reads the outbox messages of the tenant in the order of their sequence, without those of the audit and the
// authorization runtimes.
func (e *env) staged(tenantID string) []stagedRow {
	e.t.Helper()
	rows, err := e.admin.QueryContext(e.ctx, `SELECT destination, kind, resource_key, convert_from(payload, 'UTF8'), outgoing_key,
 idempotent, must_land FROM stego_outbox.messages WHERE tenant_id = $1 AND destination NOT IN ('audit-sink', $2) ORDER BY sequence`,
		tenantID, authz.DestinationName)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var all []stagedRow
	for rows.Next() {
		var m stagedRow
		if err := rows.Scan(&m.destination, &m.kind, &m.resourceKey, &m.payload, &m.outgoing, &m.idempotent, &m.mustLand); err != nil {
			e.t.Fatal(err)
		}
		all = append(all, m)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return all
}

// kinds gives the short form of each message.
func kinds(messages []stagedRow) string {
	parts := make([]string, len(messages))
	for i, m := range messages {
		parts[i] = m.String()
	}
	return strings.Join(parts, "; ")
}

// wantChange checks that m is the change event of the row id of entity with action, as a collection write stages
// it: the Kafka destination, idempotent and not must land, the payload of the entity, the action and the ID only, and
// the outgoing key over the tenant, the principal, the inbound key, the destination and the event name.
func wantChange(t *testing.T, m stagedRow, entity, resourceKey, action, id, tenantID, principal, inbound string) {
	t.Helper()
	kind := "orders." + strings.ToLower(entity) + "." + action
	payload := fmt.Sprintf(`{"entity":%q,"action":%q,"id":%q}`, entity, action, id)
	key := idempotency.Derive(tenantID, principal, inbound, kafkaoutbox.DestinationName, kind)
	if m.destination != kafkaoutbox.DestinationName || m.kind != kind || m.resourceKey != resourceKey || m.payload != payload ||
		!m.idempotent || m.mustLand || m.outgoing != key {
		t.Errorf("the message is %+v, want the change event %s of %s with the payload %s, idempotent, not must land, and the key %s",
			m, kind, resourceKey, payload, key)
	}
}

func TestChangeEvents(t *testing.T) {
	if ChangeEventDestination != kafkaoutbox.DestinationName {
		t.Fatalf("the change events go to %q, want the Kafka destination %q", ChangeEventDestination, kafkaoutbox.DestinationName)
	}
	e := newEnv(t)
	e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('ce-c', 'o-c1', 'open'), ('ce-w', 'o-w1', 'open'),
 ('ce-n', 'o-n1', 'open'), ('ce-f', 'o-f1', 'open'), ('ce-rb', 'o-rb1', 'open'), ('ce-cf', 'o-cf1', 'open'), ('ce-x', 'o-x1', 'open')`)
	withEvents := func(spec *CommandSpec[testProbe]) {
		spec.ChangeEvents = changeEventsOf("Order", "Reservation")
	}

	t.Run("ST-SUP-RP10-LC36-T01 A command's create, update and delete stage their change events in the order of the calls with its effects", func(t *testing.T) {
		cmd := e.command("place-order", withEvents, e.orders, e.reservationE)
		p := u7(t)
		_, err := e.run(cmd, e.request("ce-c", p, "k-ce1", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			orders, reservations := repo(t, h, e.orders), repo(t, h, e.reservationE)
			o, err := orders.Create(ctx, order{ID: "o-c2", Status: "new"})
			if err != nil {
				return err
			}
			if err := h.Effects().Stage(ctx, "billing", "noted", o, json.RawMessage(`{"n":1}`)); err != nil {
				return err
			}
			v := o.Value()
			v.Status = "placed"
			if o, err = orders.Update(ctx, o, v); err != nil {
				return err
			}
			// A second update of one row in one call stages a second event with a key of its own.
			v.Status = "paid"
			if o, err = orders.Update(ctx, o, v); err != nil {
				return err
			}
			r, err := reservations.Create(ctx, reservation{ID: "r-c1", OrderID: "o-c2", SKU: "s"})
			if err != nil {
				return err
			}
			if err := reservations.Delete(ctx, r); err != nil {
				return err
			}
			old, err := orders.GetForDelete(ctx, "o-c1")
			if err != nil {
				return err
			}
			if err := orders.Delete(ctx, old); err != nil {
				return err
			}
			return h.Effects().Stage(ctx, "billing", "cancelled", o, json.RawMessage(`{"n":2}`))
		}))
		if err != nil {
			t.Fatal(err)
		}
		got := e.staged("ce-c")
		want := "kafka orders.order.created order:o-c2; billing noted order:o-c2; kafka orders.order.updated order:o-c2; " +
			"kafka orders.order.updated order:o-c2; kafka orders.reservation.created Reservation:r-c1; " +
			"kafka orders.reservation.deleted Reservation:r-c1; kafka orders.order.deleted order:o-c1; billing cancelled order:o-c2"
		if kinds(got) != want {
			t.Fatalf("the outbox holds %s,\nwant %s", kinds(got), want)
		}
		subject := p.Subject().String()
		wantChange(t, got[0], "Order", "order:o-c2", "created", "o-c2", "ce-c", subject, lp("k-ce1", "o-c2"))
		wantChange(t, got[2], "Order", "order:o-c2", "updated", "o-c2", "ce-c", subject, lp("k-ce1", "o-c2"))
		wantChange(t, got[3], "Order", "order:o-c2", "updated", "o-c2", "ce-c", subject, lp(lp("k-ce1", "o-c2"), "1"))
		wantChange(t, got[4], "Reservation", "Reservation:r-c1", "created", "r-c1", "ce-c", subject, lp("k-ce1", "r-c1"))
		wantChange(t, got[5], "Reservation", "Reservation:r-c1", "deleted", "r-c1", "ce-c", subject, lp("k-ce1", "r-c1"))
		wantChange(t, got[6], "Order", "order:o-c1", "deleted", "o-c1", "ce-c", subject, lp("k-ce1", "o-c1"))
		// A command without change events, as in a service without kafka-producer, stages none.
		plain := e.command("place-plain", nil, e.orders)
		if _, err := e.run(plain, e.request("ce-c", p, "k-ce1-plain", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, e.orders).Create(ctx, order{ID: "o-c3", Status: "new"})
			return err
		})); err != nil {
			t.Fatal(err)
		}
		if n := len(e.staged("ce-c")); n != len(got) {
			t.Errorf("a command with no change events staged %d messages, want none", n-len(got))
		}
	})

	t.Run("ST-SUP-RP10-LC36-T02 A worker's writes in RunWorker and RunConsumer stage their change events, and a redelivery stages none again", func(t *testing.T) {
		w := e.worker("sync-partner-orders", "sync", func(spec *WorkerSpec[testProbe]) {
			spec.ChangeEvents = changeEventsOf("Order", "Reservation")
		}, e.orders, e.reservationE)
		err := RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "ce-w"), nil, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
			if _, err := updateOrder(ctx, h, e.orders, "o-w1", "synced"); err != nil {
				return err
			}
			if err := h.Effects().Stage(ctx, "billing", "charge-requested", mustRef(ctx, h, e.orders, "o-w1"), json.RawMessage(`{}`)); err != nil {
				return err
			}
			_, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-w1", OrderID: "o-w1", SKU: "s"})
			return err
		}))
		if err != nil {
			t.Fatal(err)
		}
		got := e.staged("ce-w")
		if want := "kafka orders.order.updated order:o-w1; billing charge-requested order:o-w1; kafka orders.reservation.created Reservation:r-w1"; kinds(got) != want {
			t.Fatalf("the outbox holds %s,\nwant %s", kinds(got), want)
		}
		account := "service_account:sync-partner-orders"
		due := fmt.Sprint(tick.Unix())
		wantChange(t, got[0], "Order", "order:o-w1", "updated", "o-w1", "ce-w", account, lp("sync-partner-orders", due, "o-w1"))
		wantChange(t, got[2], "Reservation", "Reservation:r-w1", "created", "r-w1", "ce-w", account, lp("sync-partner-orders", due, "r-w1"))

		consumer := e.worker("apply-replies", "sync", func(spec *WorkerSpec[testProbe]) {
			spec.Interval = 0
			spec.Stagers = map[string]string{"reserve-and-charge": "edit"}
			spec.ChangeEvents = changeEventsOf("Order")
		}, e.orders)
		e.consumer(consumer, nil, func(ctx context.Context, m map[string]string, _ string, h *Handle[testProbe]) error {
			_, err := updateOrder(ctx, h, e.orders, m["order"], "replied")
			return err
		})
		id := e.stage("ce-n", "o-n1", map[string]string{"order": "o-n1"})
		e.waitState(t, id, "done")
		changes := func() []stagedRow {
			var only []stagedRow
			for _, m := range e.staged("ce-n") {
				if m.destination == kafkaoutbox.DestinationName {
					only = append(only, m)
				}
			}
			return only
		}
		got = changes()
		if len(got) != 1 {
			t.Fatalf("the consumer's write staged %s, want its one change event", kinds(got))
		}
		wantChange(t, got[0], "Order", "order:o-n1", "updated", "o-n1", "ce-n", "service_account:apply-replies", lp(id.String(), "o-n1"))
		// A dispatch again of the done message is a replay of its key: the fill does not run, and no second event is
		// staged.
		e.redeliver(id)
		e.waitState(t, id, "done")
		if got := changes(); len(got) != 1 {
			t.Errorf("after the redelivery the outbox holds %s, want the one change event", kinds(got))
		}
	})

	t.Run("ST-SUP-RP10-LC36-T03 A rolled-back command or worker run leaves no change event", func(t *testing.T) {
		cmd := e.command("place-order", withEvents, e.orders, e.reservationE)
		refused := errors.New("the fill refuses after its writes")
		_, err := e.run(cmd, e.request("ce-rb", u7(t), "k-rb", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			if _, err := repo(t, h, e.orders).Create(ctx, order{ID: "o-rb2", Status: "new"}); err != nil {
				return err
			}
			if _, err := updateOrder(ctx, h, e.orders, "o-rb1", "changed"); err != nil {
				return err
			}
			return refused
		}))
		if !errors.Is(err, refused) {
			t.Fatalf("the run gave %v, want the error of the fill", err)
		}
		w := e.worker("sync-partner-orders", "sync", func(spec *WorkerSpec[testProbe]) {
			spec.ChangeEvents = changeEventsOf("Order")
		}, e.orders)
		err = RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "ce-rb"), nil, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
			if _, err := updateOrder(ctx, h, e.orders, "o-rb1", "changed"); err != nil {
				return err
			}
			return refused
		}))
		if !errors.Is(err, refused) {
			t.Fatalf("the worker run gave %v, want the error of the fill", err)
		}
		if got := e.staged("ce-rb"); len(got) != 0 || e.count(`SELECT count(*) FROM orders WHERE tenant_id = 'ce-rb' AND status <> 'open'`) != 0 {
			t.Errorf("the rolled-back runs left %s", kinds(got))
		}
	})

	t.Run("ST-SUP-RP10-LC36-T04 A conflict that runs the command again leaves the change events of the last attempt only", func(t *testing.T) {
		cmd := e.command("place-order", func(spec *CommandSpec[testProbe]) {
			withEvents(spec)
			spec.RetryOnConflict = 2
		}, e.orders, e.reservationE)
		attempts := 0
		result, err := e.run(cmd, e.request("ce-cf", u7(t), "k-cf", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			attempts++
			if _, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-cf1", OrderID: "o-cf1", SKU: "s"}); err != nil {
				return err
			}
			orders := repo(t, h, e.orders)
			o, err := orders.Get(ctx, "o-cf1")
			if err != nil {
				return err
			}
			if attempts == 1 {
				// Another transaction moves the version of the row: the update of the first attempt is a conflict.
				e.exec(`UPDATE orders SET version = version + 1 WHERE tenant_id = 'ce-cf' AND id = 'o-cf1'`)
			}
			v := o.Value()
			v.Status = "placed"
			_, err = orders.Update(ctx, o, v)
			return err
		}))
		if err != nil || attempts != 2 || result.Attempts != 2 {
			t.Fatalf("the run gave %v after %d calls of the fill (%d attempts), want success at the second", err, attempts, result.Attempts)
		}
		if want := "kafka orders.reservation.created Reservation:r-cf1; kafka orders.order.updated order:o-cf1"; kinds(e.staged("ce-cf")) != want {
			t.Errorf("the outbox holds %s, want %s", kinds(e.staged("ce-cf")), want)
		}
	})

	t.Run("ST-SUP-RP10-LC36-T05 A replay of a completed key stages no second change event", func(t *testing.T) {
		cmd := e.command("place-order", withEvents, e.orders, e.reservationE)
		fill := fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, e.orders).Create(ctx, order{ID: "o-x2", Status: "new"})
			return err
		})
		if _, err := e.run(cmd, e.request("ce-x", u7(t), "k-rp", `{}`), fill); err != nil {
			t.Fatal(err)
		}
		again, err := e.run(cmd, e.request("ce-x", u7(t), "k-rp", `{}`), fill)
		if err != nil || !again.Replayed {
			t.Fatalf("the second run gave %+v, %v; want the replay", again, err)
		}
		if want := "kafka orders.order.created order:o-x2"; kinds(e.staged("ce-x")) != want {
			t.Errorf("the outbox holds %s, want %s", kinds(e.staged("ce-x")), want)
		}
	})

	t.Run("ST-SUP-RP10-LC36-T06 A fenced worker run leaves no change event", func(t *testing.T) {
		const name = "stego.worker.fenced-sync"
		if err := lease.Declare(e.ctx, e.admin, name); err != nil {
			t.Fatal(err)
		}
		h1, err := lease.New(e.pool, name, "h1", lease.Settings{})
		if err != nil {
			t.Fatal(err)
		}
		stale, err := h1.Acquire(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		e.exec(`UPDATE leases SET expires_at = now() - interval '1 second' WHERE name = '` + name + `'`)
		h2, err := lease.New(e.pool, name, "h2", lease.Settings{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h2.Acquire(e.ctx); err != nil {
			t.Fatal(err)
		}
		w := e.worker("fenced-sync", "sync", func(spec *WorkerSpec[testProbe]) {
			spec.ChangeEvents = changeEventsOf("Order")
		}, e.orders)
		err = RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "ce-f"), stale, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := updateOrder(ctx, h, e.orders, "o-f1", "fenced")
			return err
		}))
		if !errors.Is(err, lease.ErrFenced) {
			t.Fatalf("the run with the stale token gave %v, want ErrFenced", err)
		}
		if got := e.staged("ce-f"); len(got) != 0 {
			t.Errorf("the fenced run left %s", kinds(got))
		}
	})

	t.Run("ST-SUP-RP10-LC36-T07 A fill cannot stage a change event itself", func(t *testing.T) {
		// The command declares the Kafka destination with the event names, as a collection write does.
		cmd := e.command("place-order", func(spec *CommandSpec[testProbe]) {
			withEvents(spec)
			spec.Destinations = append(spec.Destinations, Destination{Name: kafkaoutbox.DestinationName, Idempotent: true,
				Effects: []string{"orders.order.created", "orders.order.updated"}})
		}, e.orders, e.reservationE)
		_, err := e.run(cmd, e.request("ce-f", u7(t), "k-forge", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			o, err := repo(t, h, e.orders).Get(ctx, "o-f1")
			if err != nil {
				return err
			}
			// The fill wrote no row, and forges the event of an update.
			return h.Effects().Stage(ctx, kafkaoutbox.DestinationName, "orders.order.updated", o,
				json.RawMessage(`{"entity":"Order","action":"updated","id":"o-f1"}`))
		}))
		if !errors.Is(err, ErrInternal) {
			t.Fatalf("the forged change event gave %v, want an internal error", err)
		}
		// A worker whose destination states no effect names may stage any name but a change event.
		w := e.worker("forge-sync", "sync", func(spec *WorkerSpec[testProbe]) {
			spec.ChangeEvents = changeEventsOf("Order")
		}, e.orders)
		err = RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "ce-f"), nil, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
			return h.Effects().Stage(ctx, "billing", "orders.order.deleted", mustRef(ctx, h, e.orders, "o-f1"), json.RawMessage(`{}`))
		}))
		if !errors.Is(err, ErrInternal) {
			t.Fatalf("the worker's forged change event gave %v, want an internal error", err)
		}
		if got := e.staged("ce-f"); len(got) != 0 {
			t.Errorf("the forged events left %s", kinds(got))
		}
	})

	t.Run("ST-SUP-RP10-LC36-T08 The change events of more writes than one batch holds enter the outbox in the order of the writes", func(t *testing.T) {
		cmd := e.command("place-order", withEvents, e.orders, e.reservationE)
		const n = 40
		_, err := e.run(cmd, e.request("ce-x", u7(t), "k-many", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			reservations := repo(t, h, e.reservationE)
			for i := range n {
				if _, err := reservations.Create(ctx, reservation{ID: fmt.Sprintf("r-m%02d", i), OrderID: "o-x1", SKU: "s"}); err != nil {
					return err
				}
			}
			// The change events do not count against the effects of the fill.
			return h.Effects().Stage(ctx, "billing", "noted", mustRef(ctx, h, e.orders, "o-x1"), json.RawMessage(`{}`))
		}))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, m := range e.staged("ce-x") {
			if m.kind == "orders.reservation.created" || m.kind == "noted" {
				got = append(got, m.resourceKey)
			}
		}
		var want []string
		for i := range n {
			want = append(want, fmt.Sprintf("Reservation:r-m%02d", i))
		}
		want = append(want, "order:o-x1")
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("the outbox holds %v, want %v", got, want)
		}
	})
}

// mustRef reads the row id of entity through the handle, for an effect that names it.
func mustRef[T any](ctx context.Context, h *Handle[testProbe], e *Entity[T], id string) Ref {
	r, err := Repo(h, e)
	if err != nil {
		panic(err)
	}
	v, err := r.Get(ctx, id)
	if err != nil {
		panic(err)
	}
	return v
}

func TestChangeEventSpecs(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name   string
		events []ChangeEvents
		other  []Destination
	}{
		{"an entity that is not written", changeEventsOf("Order", "Reservation", "Charge"), nil},
		{"an entity given twice", append(changeEventsOf("Order", "Reservation"),
			ChangeEvents{Entity: "Order", Created: "other.order.created", Updated: "other.order.updated", Deleted: "other.order.deleted"}), nil},
		{"a written entity with none", changeEventsOf("Order"), nil},
		{"an empty name", []ChangeEvents{changeEventsOf("Order")[0], {Entity: "Reservation", Created: "c", Updated: "u"}}, nil},
		{"a name that is not an outbox kind", []ChangeEvents{changeEventsOf("Order")[0], {Entity: "Reservation", Created: "c d", Updated: "u", Deleted: "d"}}, nil},
		{"a name of two entities", []ChangeEvents{changeEventsOf("Order")[0], {Entity: "Reservation", Created: "orders.order.created", Updated: "u", Deleted: "d"}}, nil},
		{"a change event declared as an effect", changeEventsOf("Order", "Reservation"),
			[]Destination{{Name: "billing", Idempotent: true, Effects: []string{"orders.order.created"}}}},
	}
	t.Run("ST-SUP-RP10-LC36-T09 A command or a worker with wrong change events is refused", func(t *testing.T) {
		for _, c := range cases {
			_, err := NewCommand(CommandSpec[testProbe]{Name: "place-order", Writes: []Declared{e.orders, e.reservationE},
				Probe: newTestProbe, ChangeEvents: c.events, Destinations: c.other})
			if err == nil {
				t.Errorf("the command with %s was not refused", c.name)
			}
			_, err = NewWorker(WorkerSpec[testProbe]{Name: "sync-orders", Permission: "sync", Interval: time.Minute,
				Writes: []Declared{e.orders, e.reservationE}, Probe: newTestProbe, ChangeEvents: c.events, Destinations: c.other})
			if err == nil {
				t.Errorf("the worker with %s was not refused", c.name)
			}
		}
		if _, err := NewCommand(CommandSpec[testProbe]{Name: "place-order", Writes: []Declared{e.orders, e.reservationE},
			Probe: newTestProbe, ChangeEvents: changeEventsOf("Order", "Reservation"),
			Destinations: []Destination{{Name: kafkaoutbox.DestinationName, Idempotent: true, Effects: []string{"orders.order.created"}}}}); err != nil {
			t.Errorf("the command with the change events and the Kafka destination of a collection write was refused: %v", err)
		}
	})
}
