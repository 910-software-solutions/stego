package kafkaconsumer

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

func context() gen.Context {
	return gen.Context{OutputNamespace: "internal/subscriptions", ServiceName: "fulfilment",
		PeerNamespaces: map[string]string{"outbox": "internal/outbox", "worker": "workers"},
		Subscriptions:  []types.Subscription{{Name: "order-changes", Source: "orders", Events: []string{"orders.order.created", "orders.order.deleted"}}},
		Workers: []gen.Worker{{Worker: types.Worker{Name: "ship-orders", Trigger: "consumes order-changes"}},
			{Worker: types.Worker{Name: "sweep", Trigger: "every 1m"}}}}
}

func TestGenerate(t *testing.T) {
	t.Run("ST-SUP-RC38-OC01-T01 The component emits the declared subscription, the constructor of its inbox and the migration text", func(t *testing.T) {
		files, wiring, err := new(Generator).Generate(context())
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 || files[0].Path != "internal/subscriptions/subscriptions.go" || wiring == nil {
			t.Fatalf("the component emits %d files, the first %v", len(files), files)
		}
		source := string(files[0].Content)
		if _, err := parser.ParseFile(token.NewFileSet(), "subscriptions.go", source, 0); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			`kafkainbox.Subscription{Service: "fulfilment", Name: "order-changes", Source: "orders", Worker: "ship-orders"`,
			`Events: []string{"orders.order.created", "orders.order.deleted"}`,
			"kafkainbox.ReadSettings(Subscription.Service, Subscription.Name)",
			"func Inbox(env service.InboxEnv) (*kafkainbox.Inbox, error)",
			"Retention: env.Retention",
			"func Migration(workerRole string) (string, error) { return kafkainbox.Migration(workerRole) }",
		} {
			if !strings.Contains(source, want) {
				t.Errorf("subscriptions.go does not hold %s:\n%s", want, source)
			}
		}
	})
	t.Run("ST-SUP-RC38-OC01-T02 The component refuses a context that its inbox cannot serve", func(t *testing.T) {
		cases := map[string]func(*gen.Context){
			"no subscription": func(c *gen.Context) { c.Subscriptions = nil },
			"two subscriptions": func(c *gen.Context) {
				c.Subscriptions = append(c.Subscriptions, types.Subscription{Name: "more", Source: "orders", Events: []string{"orders.order.created"}})
			},
			"no outbox":           func(c *gen.Context) { delete(c.PeerNamespaces, "outbox") },
			"no worker component": func(c *gen.Context) { delete(c.PeerNamespaces, "worker") },
			"no consuming worker": func(c *gen.Context) { c.Workers = c.Workers[1:] },
			"a setting":           func(c *gen.Context) { c.ComponentConfig = map[string]any{"topic": "x"} },
			"two consuming workers": func(c *gen.Context) {
				c.Workers = append(c.Workers, gen.Worker{Worker: types.Worker{Name: "b", Trigger: "consumes order-changes"}})
			},
		}
		for name, change := range cases {
			c := context()
			change(&c)
			if err := new(Generator).ValidateContext(c); err == nil {
				t.Errorf("the context with %s is accepted", name)
			}
		}
	})
}
