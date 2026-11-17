package declaration

import (
	"strings"
	"testing"
)

// subscribed gives the declaration of orders, which consumes the change events of shop through the subscription
// shop-orders and the worker ship, with the lines given added to the subscription.
func subscribed(extra string, items ...string) string {
	if len(items) == 0 {
		items = []string{worker("ship", "trigger: consumes shop-orders", "writes: [Order]", "fill: apply")}
	}
	return workers(items...) + "subscriptions:\n  - name: shop-orders\n    source: shop\n    events: [shop.order.created, shop.order.updated]\n" + extra
}

func TestSubscriptionNames(t *testing.T) {
	t.Run("ST-DECL-INV96-T A subscription with a name of its own and one consuming worker is accepted", func(t *testing.T) {
		wantNoFault(t, workerFaults(t, subscribed("")))
	})
	t.Run("ST-DECL-INV96-T A second subscription is refused while kafka-consumer is alpha", func(t *testing.T) {
		service := subscribed("  - name: more-orders\n    source: shop\n    events: [shop.order.deleted]\n",
			worker("ship", "trigger: consumes shop-orders", "writes: [Order]", "fill: apply"),
			worker("more", "trigger: consumes more-orders", "writes: [Order]", "fill: other"))
		wantFault(t, workerFaults(t, service, "subscription-count"), "at most 1", "a later version")
	})
	for _, c := range []struct{ name, service string }{
		{"the name of a worker", strings.Replace(subscribed(""), "- name: shop-orders", "- name: ship", 1)},
		{"a destination that a component declares", strings.Replace(subscribed(""), "- name: shop-orders", "- name: partner-orders", 1)},
		{"a reserved destination", strings.Replace(subscribed(""), "- name: shop-orders", "- name: authz", 1)},
		{"not in lower kebab case", strings.Replace(subscribed(""), "- name: shop-orders", "- name: Shop_Orders", 1)},
	} {
		t.Run("ST-DECL-INV96-T A subscription named "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, workerFaults(t, c.service, "subscription-name"))
		})
	}
}

func TestSubscriptionEvents(t *testing.T) {
	for _, c := range []struct{ name, from, to, want string }{
		{"a kind of another service", "shop.order.updated]", "billing.invoice.created]", "each kind starts with the source shop"},
		{"a kind with an unknown action", "shop.order.updated]", "shop.order.shipped]", "created, updated or deleted"},
		{"a kind given twice", "shop.order.updated]", "shop.order.created]", "twice"},
		{"no kind", "[shop.order.created, shop.order.updated]", "[]", "give 1 to 32"},
		{"the service itself as the source", "source: shop", "source: todo-svc", "the service itself"},
		{"a source that is not a service name", "source: shop", "source: Shop", "lower kebab case"},
	} {
		t.Run("ST-DECL-INV97-T A subscription with "+c.name+" is refused", func(t *testing.T) {
			service := strings.Replace(subscribed(""), c.from, c.to, 1)
			rules := []string{"subscription-events"}
			if strings.HasPrefix(c.to, "source") {
				rules = []string{"subscription-source"}
			}
			wantFault(t, workerFaults(t, service, rules...), c.want)
		})
	}
}

func TestSubscriptionConsumer(t *testing.T) {
	t.Run("ST-DECL-INV98-T A subscription that no worker consumes is refused", func(t *testing.T) {
		service := subscribed("", worker("ship", "trigger: every 1m", "writes: [Order]", "fill: apply"))
		wantFault(t, workerFaults(t, service, "subscription-consumer"), "0 workers", "consumes shop-orders")
	})
	t.Run("ST-DECL-INV98-T A command or a worker that stages an effect for a subscription is refused", func(t *testing.T) {
		service := subscribed("", worker("ship", "trigger: consumes shop-orders", "writes: [Order]", "fill: apply"),
			worker("feed", "trigger: every 1m", "writes: [Order]", "fill: other", "effects:\n      - {destination: shop-orders, name: fake}"))
		wantFault(t, workerFaults(t, service, "worker-effect"), "whose messages come only from Kafka")
	})
}

func TestSubscriptionMode(t *testing.T) {
	t.Run("ST-DECL-INV89-T Declared modes without worker are refused for a service with a subscription", func(t *testing.T) {
		service := strings.Replace(subscribed(""), "modes: [api, worker]", "modes: [api]", 1)
		faults := workerFaults(t, service)
		found := false
		for _, f := range faults {
			if f.Rule == "mode-needed" && strings.Contains(f.Message, "the Kafka inbox of the subscription shop-orders") {
				found = true
			}
		}
		if !found {
			t.Fatalf("no mode-needed fault names the Kafka inbox: %v", faults)
		}
	})
}
