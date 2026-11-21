package compiler

import (
	"slices"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

func TestRuntimeWiringInbox(t *testing.T) {
	t.Run("ST-SUP-RC38-OC01-T03 The runtime wiring of a service with a subscription gives the worker mode its inbox and the migration job its table", func(t *testing.T) {
		data := runtimeWiringData{Audit: "m/out/internal/auditlog", Authz: "m/out/internal/authz", Outbox: "m/out/internal/outbox",
			Storage: "m/out/internal/storage", Tenancy: "m/out/internal/tenancy", Workers: "m/out/workers/runners", Consumers: true,
			Inbox: "m/out/internal/subscriptions", Service: "fulfilment", ProblemCode: "fulfilment", Modes: []string{"api", "worker", "controller"}}
		source, err := renderRuntimeWiring(data)
		if err != nil {
			t.Fatal(err)
		}
		text := string(source)
		for _, want := range []string{
			`subscriptions "m/out/internal/subscriptions"`,
			"Inboxes: subscriptions.Inboxes,",
			"inboxText, err := subscriptions.Migration(WorkerRole)",
			"Inbox: inboxText,",
			`return "service.start.setting." + setting.component + "." + setting.variable`,
			`if err = checkConsumerTopic(); err != nil {`,
			`if variable, problem := subscriptions.CheckSettings(); variable != "" {`,
		} {
			if !strings.Contains(text, want) {
				t.Errorf("the runtime wiring does not hold %s", want)
			}
		}
		data.Inbox = ""
		if source, err = renderRuntimeWiring(data); err != nil || strings.Contains(string(source), "subscriptions") {
			t.Errorf("the runtime wiring of a service with no subscription names the inbox (%v)", err)
		}
	})
}

func TestPerSubscription(t *testing.T) {
	t.Run("ST-SUP-RC38-OC02-T04 A variable and a need with {subscription} are listed once for each declared subscription, and the others once", func(t *testing.T) {
		reg, err := registry.Load("../../registry")
		if err != nil {
			t.Fatal(err)
		}
		consumer := reg.Component("kafka-consumer")
		subscriptions := []types.Subscription{{Name: "order-changes"}, {Name: "carrier-changes"}}
		variables, needs := perSubscription(consumer.Variables(), consumer.Needs, subscriptions)
		groups := map[string][]string{}
		names := map[string]registry.EnvironmentVariable{}
		for _, v := range variables {
			if strings.Contains(v.Name, "{") || strings.Contains(v.OneOf, "{") {
				t.Errorf("the variable %s of the group %q holds a placeholder", v.Name, v.OneOf)
			}
			names[v.Name] = v
			if v.OneOf != "" {
				groups[v.OneOf] = append(groups[v.OneOf], v.Name)
			}
		}
		for _, sub := range []string{"ORDER_CHANGES", "CARRIER_CHANGES"} {
			for _, suffix := range []string{"TOPIC", "TOPIC_FILE", "GROUP", "GROUP_FILE", "ACCEPT_INFINITE_RETENTION"} {
				if v, ok := names["STEGO_KAFKA_CONSUMER_"+sub+"_"+suffix]; !ok || v.ReadByMode != "worker" || v.Description == "" {
					t.Errorf("no variable STEGO_KAFKA_CONSUMER_%s_%s of the worker mode with its description", sub, suffix)
				}
			}
		}
		if !slices.Equal(groups["order-changes-topic"], []string{"STEGO_KAFKA_CONSUMER_ORDER_CHANGES_TOPIC", "STEGO_KAFKA_CONSUMER_ORDER_CHANGES_TOPIC_FILE"}) ||
			!slices.Equal(groups["carrier-changes-topic"], []string{"STEGO_KAFKA_CONSUMER_CARRIER_CHANGES_TOPIC", "STEGO_KAFKA_CONSUMER_CARRIER_CHANGES_TOPIC_FILE"}) {
			t.Errorf("the groups of one of are %v", groups)
		}
		if _, ok := names["STEGO_KAFKA_BROKERS"]; !ok || len(variables) != len(consumer.Variables())+5 {
			t.Errorf("the variables of the service are %d, want each connection variable once and five for each subscription", len(variables))
		}
		var topics []string
		for _, n := range needs {
			if n.Kind == "topic" {
				topics = append(topics, n.Name+" "+n.Variable+" "+n.Access)
			}
		}
		if want := []string{"{service}-sub-order-changes STEGO_KAFKA_CONSUMER_ORDER_CHANGES_TOPIC read",
			"{service}-sub-carrier-changes STEGO_KAFKA_CONSUMER_CARRIER_CHANGES_TOPIC read"}; !slices.Equal(topics, want) {
			t.Errorf("the topic needs are %v, want %v", topics, want)
		}
		if variables, needs := perSubscription(consumer.Variables(), consumer.Needs, nil); len(needs) != len(consumer.Needs)-1 ||
			slices.ContainsFunc(variables, func(v registry.EnvironmentVariable) bool { return strings.Contains(v.Name, "CONSUMER") }) {
			t.Errorf("a service with no subscription lists %d needs and the variables %v", len(needs), variables)
		}
	})
}
