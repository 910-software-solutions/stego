package compiler

import (
	"strings"
	"testing"
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
			"Inbox: subscriptions.Inbox,",
			"inboxText, err := subscriptions.Migration(WorkerRole)",
			"Inbox: inboxText,",
			`return "service.start.setting." + setting.component + "." + setting.variable`,
			`if err = checkConsumerTopic(); err != nil {`,
			`variable: "STEGO_KAFKA_CONSUMER_TOPIC"`,
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
