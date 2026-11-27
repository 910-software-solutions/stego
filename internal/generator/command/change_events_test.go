package command

import (
	"bytes"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/types"
)

func TestChangeEvents(t *testing.T) {
	t.Run("ST-SUP-RC32-OC14-T01 With kafka-producer a command gives the handle the change events of each entity it writes", func(t *testing.T) {
		svc := declaration(t)
		ctx := commandContext(t, svc, commandSlot)
		ctx.PeerNamespaces[KafkaComponent] = "events"
		ctx.PeerNamespaces["outbox"] = "outbox"
		var source []byte
		for _, f := range generated(t, ctx) {
			if bytes.Contains(f, []byte("command.CommandSpec[")) {
				source = append(source, f...)
			}
		}
		var parts []string
		for _, entity := range []string{"Reservation", "Charge", "Order", "Hold"} {
			name := "orders." + strings.ToLower(entity)
			parts = append(parts, `{Entity: "`+entity+`", Created: "`+name+`.created", Updated: "`+name+`.updated", Deleted: "`+name+`.deleted"}`)
		}
		want := "[]command.ChangeEvents{" + strings.Join(parts, ", ") + "},"
		if n := bytes.Count(source, []byte(want)); n != 1 {
			t.Errorf("the command declaration holds %d times %s, want once:\n%s", n, want, source)
		}
		// Without kafka-producer a command gives no change events.
		for path, f := range generated(t, commandContext(t, svc, commandSlot)) {
			if bytes.Contains(f, []byte("ChangeEvents")) {
				t.Errorf("%s gives change events without kafka-producer", path)
			}
		}
		for name, change := range map[string]func(*types.ServiceDeclaration) (drop string){
			"no outbox component":                       func(*types.ServiceDeclaration) string { return "outbox" },
			"an event type that Kafka keeps for itself": func(s *types.ServiceDeclaration) string { s.Name = "__orders"; return "" },
			"an event type longer than 128 characters":  func(s *types.ServiceDeclaration) string { s.Name = strings.Repeat("o", 120); return "" },
			"an effect named as a change event": func(s *types.ServiceDeclaration) string {
				s.Commands[0].Effects = append(s.Commands[0].Effects, types.Effect{Destination: "billing", Name: "orders.charge.created"})
				return ""
			},
		} {
			s := declaration(t)
			drop := change(s)
			c := commandContext(t, s, commandSlot)
			c.PeerNamespaces[KafkaComponent] = "events"
			c.PeerNamespaces["outbox"] = "outbox"
			delete(c.PeerNamespaces, drop)
			if files, _, err := new(Generator).Generate(c); err == nil || files != nil || !strings.Contains(err.Error(), "ST-SUP-RC32-OC14") {
				t.Errorf("%s: generation gave %d files and %v, want a refusal that names ST-SUP-RC32-OC14", name, len(files), err)
			}
		}
	})
}
