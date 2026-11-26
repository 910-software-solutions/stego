package kafkaconsumer

import (
	"fmt"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/types"
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
			"var Subscriptions = []kafkainbox.Subscription{",
			`{Service: "fulfilment", Name: "order-changes", Source: "orders", Worker: "ship-orders"`,
			`Events: []string{"orders.order.created", "orders.order.deleted"}`,
			"kafkainbox.ReadSettings(s.Service, s.Name)",
			"func Inboxes(env service.InboxEnv) ([]*kafkainbox.Inbox, error)",
			"func CheckSettings() (variable, problem string)",
			"kafkainbox.CheckSettings(Subscriptions[0].Service, names())",
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
			"nine subscriptions": func(c *gen.Context) {
				for i := 2; i <= 9; i++ {
					name := fmt.Sprintf("more-%d", i)
					c.Subscriptions = append(c.Subscriptions, types.Subscription{Name: name, Source: "orders", Events: []string{"orders.order.created"}})
					c.Workers = append(c.Workers, gen.Worker{Worker: types.Worker{Name: "w-" + name, Trigger: "consumes " + name}})
				}
			},
			"a second subscription that no worker consumes": func(c *gen.Context) {
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
	t.Run("ST-SUP-RC38-OC01-T05 Each declared subscription gets its inbox in the order of the declaration, and the migration text once", func(t *testing.T) {
		// Decision on ST-SUP-Q204: two subscriptions, each with a worker of its own.
		c := context()
		c.Subscriptions = append(c.Subscriptions, types.Subscription{Name: "carrier-changes", Source: "carriers", Events: []string{"carriers.carrier.updated"}})
		c.Workers = append(c.Workers, gen.Worker{Worker: types.Worker{Name: "track-carriers", Trigger: "consumes carrier-changes"}})
		files, _, err := new(Generator).Generate(c)
		if err != nil {
			t.Fatal(err)
		}
		source := string(files[0].Content)
		if _, err := parser.ParseFile(token.NewFileSet(), "subscriptions.go", source, 0); err != nil {
			t.Fatal(err)
		}
		first := strings.Index(source, `Name: "order-changes", Source: "orders", Worker: "ship-orders"`)
		second := strings.Index(source, `Name: "carrier-changes", Source: "carriers", Worker: "track-carriers"`)
		if first < 0 || second < first || !strings.Contains(source, `Events: []string{"carriers.carrier.updated"}`) {
			t.Errorf("subscriptions.go does not hold the two subscriptions in the order of the declaration:\n%s", source)
		}
		if n := strings.Count(source, "func Migration("); n != 1 || strings.Count(source, "kafkainbox.New(") != 1 {
			t.Errorf("subscriptions.go holds %d migration functions and %d constructors, want one of each in a loop:\n%s", n, strings.Count(source, "kafkainbox.New("), source)
		}
	})
}
