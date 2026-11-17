package worker

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/command"
	"github.com/jsell-rh/stego/internal/types"
)

func TestChangeEvents(t *testing.T) {
	t.Run("ST-SUP-RC35-OC14-T01 With kafka-producer a worker gives the handle the change events of each entity it writes", func(t *testing.T) {
		ctx := workerContext(workerSlot)
		ctx.PeerNamespaces[command.KafkaComponent] = "events"
		ctx.PeerNamespaces["outbox"] = "outbox"
		var source []byte
		for _, f := range generated(t, ctx) {
			if bytes.Contains(f, []byte("command.WorkerSpec[")) {
				source = append(source, f...)
			}
		}
		// Both the worker with an every trigger, which RunWorker runs, and the worker with a consumes trigger, which
		// RunConsumer runs, give the events of Order.
		want := `[]command.ChangeEvents{{Entity: "Order", Created: "orders.order.created", Updated: "orders.order.updated", Deleted: "orders.order.deleted"}},`
		if n := bytes.Count(source, []byte(want)); n != 2 {
			t.Errorf("the worker declarations hold %d times %s, want twice:\n%s", n, want, source)
		}
		for path, f := range generated(t, workerContext(workerSlot)) {
			if bytes.Contains(f, []byte("ChangeEvents")) {
				t.Errorf("%s gives change events without kafka-producer", path)
			}
		}
		for name, change := range map[string]func(*gen.Context){
			"no outbox component":                       func(c *gen.Context) { delete(c.PeerNamespaces, "outbox") },
			"an event type that Kafka keeps for itself": func(c *gen.Context) { c.ServiceName = "__orders" },
			"an event type longer than 128 characters":  func(c *gen.Context) { c.ServiceName = strings.Repeat("o", 120) },
			"an effect named as a change event": func(c *gen.Context) {
				c.Destinations = append(c.Destinations, gen.Destination{Name: "billing", Component: "outbox", MustLand: &yes, Idempotent: &yes})
				c.Workers[0].Worker.Effects = []types.Effect{{Destination: "billing", Name: "orders.order.updated"}}
			},
		} {
			c := workerContext(workerSlot)
			c.PeerNamespaces[command.KafkaComponent] = "events"
			c.PeerNamespaces["outbox"] = "outbox"
			change(&c)
			if files, _, err := new(Generator).Generate(c); err == nil || files != nil || !strings.Contains(err.Error(), "ST-SUP-RC35-OC14") {
				t.Errorf("%s: generation gave %d files and %v, want a refusal that names ST-SUP-RC35-OC14", name, len(files), err)
			}
		}
	})
}
