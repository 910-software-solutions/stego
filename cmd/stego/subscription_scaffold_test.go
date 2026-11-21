package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// subscriptionDeclaration is what an author adds to the rpc-service scaffold to take the change events of two other
// services: two subscriptions, the one worker that consumes each, and the acceptances of the alpha components
// (decision on ST-SUP-Q204).
const subscriptionDeclaration = `subscriptions:
  - name: order-changes
    source: orders
    events: [orders.order.created]
  - name: carrier-changes
    source: carriers
    events: [carriers.carrier.updated]
workers:
  - name: record-orders
    trigger: consumes order-changes
    writes: [Greeting]
    resource_type: greeting
    permission: record
    fill: record-orders
  - name: record-carriers
    trigger: consumes carrier-changes
    writes: [Greeting]
    resource_type: greeting
    permission: record
    fill: record-carriers
experimental_acceptances:
  - {component: worker, accepted_until: 2027-03-31, reason: the scaffold records each new order}
  - {component: kafka-consumer, accepted_until: 2027-03-31, reason: the scaffold takes the order events of orders}
`

// subscriptionSchema is the schema of the scaffold with the grant of each worker: its service account holds the
// relation worker_record_orders or worker_record_carriers of each tenant (ST-SUP-RC35-OC13).
const subscriptionSchema = `definition user {}

// The operators who resolve a parked outbox message (ST-SUP-RC29-OC20).
definition outbox_destination {
	relation operator: user
	permission resolve = operator
}

definition service {
	relation operator: user
	permission provision = operator
	permission retire = operator
}

definition greeting {
	relation viewer: user
	relation tenant: tenant
	permission view = viewer
	permission record = tenant->record
}

definition service_account {}

definition tenant {
	relation worker_record_orders: service_account
	relation worker_record_carriers: service_account
	permission record = worker_record_orders + worker_record_carriers
}
`

func TestRPCSubscriptionScaffold(t *testing.T) {
	t.Parallel()
	// subscribed scaffolds an rpc-service, adds the subscription and its worker, and makes the worker's fill with
	// fill create, as the author does (docs/subscriptions.md).
	subscribed := func(t *testing.T, environment []string) string {
		t.Helper()
		project, _ := initialized(t, "rpc-service")
		declaration, err := os.ReadFile(filepath.Join(project, "service.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		putFile(t, project, "service.yaml", string(declaration)+subscriptionDeclaration)
		putFile(t, project, "authz/schema.zed", subscriptionSchema)
		for _, fill := range []string{"record-orders", "record-carriers"} {
			if run := stegoIn(t, project, environment, "fill", "create", fill, "--slot", "worker.worker"); run.code != 0 {
				t.Fatalf("stego fill create %s: exit %d: %s%s", fill, run.code, run.stdout, run.stderr)
			}
		}
		return project
	}

	t.Run("ST-SUP-RC38-OC01-T04 A scaffolded rpc-service with a subscription validates, applies and builds: the shipped archetype binds the outbox", func(t *testing.T) {
		environment := moduleOf("rpc-service")
		project := subscribed(t, environment)
		// The fills are tested, then signed with listed-ssh of ST-EXT-TD02, a key that the test run makes.
		keys := testFillKeys(t)
		putFile(t, project, "SIGNERS", keys.signers)
		signing := append(append(slices.Clone(environment), keys.goEnv...), "GOFLAGS=-modcacherw",
			"HOME="+keys.homes["listed-ssh"], "XDG_CONFIG_HOME="+filepath.Join(keys.homes["listed-ssh"], ".config"))
		for _, command := range [][]string{{"test"}, {"fill", "sign", "get-greeting"}, {"fill", "sign", "record-orders"}, {"fill", "sign", "record-carriers"}} {
			if run := stegoIn(t, project, signing, command...); run.code != 0 {
				t.Fatalf("stego %v: exit %d: %s%s", command, run.code, run.stdout, run.stderr)
			}
		}
		for _, command := range [][]string{{"validate"}, {"apply"}, {"deps"}, {"apply"}, {"drift"}} {
			if run := stegoIn(t, project, environment, command...); run.code != 0 {
				t.Fatalf("stego %v: exit %d: %s%s", command, run.code, run.stdout, run.stderr)
			}
		}
		if _, err := os.Stat(filepath.Join(project, "out", "internal", "subscriptions")); err != nil {
			t.Errorf("apply wrote no package of the subscription: %v", err)
		}
		// The platform requirements and the reference list the settings and the topic need of each subscription by
		// its name (ST-SUP-RC38-OC02, decision on ST-SUP-Q204).
		requirements, err := os.ReadFile(filepath.Join(project, "deploy", "platform-requirements.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		reference, err := os.ReadFile(filepath.Join(project, "deploy", "environment-variables.md"))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"STEGO_KAFKA_CONSUMER_ORDER_CHANGES_TOPIC", "STEGO_KAFKA_CONSUMER_CARRIER_CHANGES_TOPIC_FILE",
			"STEGO_KAFKA_CONSUMER_CARRIER_CHANGES_GROUP", "STEGO_KAFKA_CONSUMER_ORDER_CHANGES_ACCEPT_INFINITE_RETENTION",
			"one_of: order-changes-topic", "one_of: carrier-changes-topic", "-sub-order-changes", "-sub-carrier-changes"} {
			if !strings.Contains(string(requirements), want) {
				t.Errorf("deploy/platform-requirements.yaml does not hold %s", want)
			}
		}
		if !strings.Contains(string(reference), "STEGO_KAFKA_CONSUMER_CARRIER_CHANGES_TOPIC") || strings.Contains(string(requirements), "{subscription}") ||
			strings.Contains(string(requirements), "STEGO_KAFKA_CONSUMER_TOPIC") {
			t.Errorf("the reference or the platform requirements do not list the settings of each subscription by its name alone")
		}
		build := exec.Command("go", "build", "-mod=readonly", "-o", filepath.Join(t.TempDir(), "service"), "./out")
		build.Dir = project
		build.Env = append(os.Environ(), "GOWORK=off")
		if output, err := build.CombinedOutput(); err != nil {
			t.Errorf("go build ./out: %v\n%s", err, output)
		}
	})

	t.Run("ST-SUP-RC38-OC01-T04 A scaffolded rpc-service with a subscription validates, applies and builds: with no outbox binding, the one fault is the port", func(t *testing.T) {
		// A registry whose rpc-service archetype does not bind durable-outbox, as before W8-B2: the port fault names
		// the cause, and no check says that the gRPC method needs grpc-application.
		archetype, err := os.ReadFile(filepath.Join(shippedRegistryCopy(t), "archetypes", "rpc-service", "archetype.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		unbound := strings.Replace(string(archetype), "  durable-outbox: outbox\n", "", 1)
		if unbound == string(archetype) {
			t.Fatal("the shipped rpc-service archetype does not bind durable-outbox")
		}
		environment := append(moduleOf("rpc-service"), "STEGO_REGISTRY="+registryWith(t, "archetypes/rpc-service/archetype.yaml", unbound))
		project, _ := initialized(t, "rpc-service")
		declaration, err := os.ReadFile(filepath.Join(project, "service.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		putFile(t, project, "service.yaml", string(declaration)+subscriptionDeclaration)
		putFile(t, project, "authz/schema.zed", subscriptionSchema)
		for _, command := range [][]string{{"validate"}, {"fill", "create", "record-orders", "--slot", "worker.worker"}} {
			run := stegoIn(t, project, environment, command...)
			output := run.stdout + run.stderr
			if run.code == 0 || !strings.Contains(output, `unresolved port "durable-outbox" required by "kafka-consumer"`) {
				t.Errorf("stego %v: exit %d, want the fault of the port durable-outbox: %s", command, run.code, output)
			}
			if strings.Contains(output, "needs the grpc-application component") {
				t.Errorf("stego %v says that the gRPC method needs grpc-application, which the archetype holds: %s", command, output)
			}
		}
	})
}
