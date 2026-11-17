package kafkaconsumer

import (
	"slices"
	"testing"

	"github.com/jsell-rh/stego/internal/registry"
)

func TestComponentDeclaration(t *testing.T) {
	t.Run("ST-SUP-RC38-OC02-T01 The shipped component declares the settings of the inbox, the shared connection settings and its needs", func(t *testing.T) {
		reg, err := registry.Load("../../../registry")
		if err != nil {
			t.Fatal(err)
		}
		consumer, producer := reg.Component("kafka-consumer"), reg.Component("kafka-producer")
		if consumer == nil || producer == nil || consumer.Stage != "alpha" {
			t.Fatal("the registry holds no alpha kafka-consumer and kafka-producer")
		}
		facts := map[string]registry.EnvironmentVariable{}
		for _, v := range consumer.Variables() {
			facts[v.Name] = v
		}
		for _, name := range []string{"STEGO_KAFKA_CONSUMER_TOPIC", "STEGO_KAFKA_CONSUMER_TOPIC_FILE", "STEGO_KAFKA_CONSUMER_GROUP",
			"STEGO_KAFKA_CONSUMER_GROUP_FILE", "STEGO_KAFKA_CONSUMER_ACCEPT_INFINITE_RETENTION"} {
			if _, ok := facts[name]; !ok {
				t.Errorf("the component does not declare %s", name)
			}
		}
		// Each connection variable has the facts of the Kafka destination, so one service with both lists it once.
		for _, v := range producer.Variables() {
			if v.Name == "STEGO_KAFKA_TOPIC" || v.Name == "STEGO_KAFKA_TOPIC_FILE" {
				if _, ok := facts[v.Name]; ok {
					t.Errorf("the component declares the producer topic %s", v.Name)
				}
				continue
			}
			got, ok := facts[v.Name]
			if !ok || got.Type != v.Type || got.IsSecret() != v.IsSecret() || got.IsRequired() != v.IsRequired() || got.OneOf != v.OneOf {
				t.Errorf("the connection variable %s differs from the one of kafka-producer", v.Name)
			}
		}
		var needs []string
		for _, n := range consumer.Needs {
			needs = append(needs, n.Kind+" "+n.Name+" "+n.Variable)
		}
		for _, want := range []string{"topic {service}-subscription STEGO_KAFKA_CONSUMER_TOPIC", "egress {service}-kafka "} {
			if !slices.Contains(needs, want) {
				t.Errorf("the component does not need %q: %v", want, needs)
			}
		}
	})
}
