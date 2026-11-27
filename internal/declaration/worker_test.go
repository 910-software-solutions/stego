package declaration

import (
	"fmt"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/parser"
	"github.com/910-software-solutions/stego/internal/types"
)

// orders is ST-DECL-TD02 with the modes given (none when empty) and the owned entity Order with the fields status
// (string) and synced (bool).
func orders(modes string) string {
	service := strings.Replace(td02, "entities:\n", "entities:\n  - name: Order\n    fields:\n      - {name: status, type: string}\n      - {name: synced, type: bool}\n", 1)
	if modes != "" {
		service += "modes: " + modes + "\n"
	}
	return service
}

// worker writes one worker with the lines given after its name.
func worker(name string, lines ...string) string {
	return "  - name: " + name + "\n    " + strings.Join(lines, "\n    ") + "\n"
}

// syncWorker is the worker sync-partner-orders with the trigger every 1m that writes Order, bound to a fill of the
// worker slot, with the effect partner-orders: order-changed, and the lines given.
func syncWorker(lines ...string) string {
	return worker("sync-partner-orders", append([]string{"trigger: every 1m", "writes: [Order]", "fill: sync", "effects:\n      - {destination: partner-orders, name: order-changed}"}, lines...)...)
}

// workers gives the declaration of orders, with the modes api and worker, and the workers given.
func workers(items ...string) string {
	return orders("[api, worker]") + "workers:\n" + strings.Join(items, "")
}

// workerFacts are the facts with the outbox destination partner-orders that an active component declares, the
// fills sync and apply of the worker slot, the fill reserve of the command slot and the fill policy of
// before_create.
func workerFacts() Facts {
	return Facts{
		FillSlots:    map[string]string{"sync": WorkerSlot, "apply": WorkerSlot, "other": WorkerSlot, "reserve": CommandSlot, "policy": "rest-api.before_create"},
		Destinations: map[string]bool{"partner-orders": true},
	}
}

func workerFaults(t *testing.T, service string, rules ...string) []ValidationError {
	t.Helper()
	return compositionFaults(t, service, workerFacts(), rules...)
}

func TestWorkerNames(t *testing.T) {
	t.Run("ST-DECL-INV65-T The worker sync-partner-orders is accepted", func(t *testing.T) {
		wantNoFault(t, workerFaults(t, workers(syncWorker())))
	})
	t.Run("ST-DECL-INV65-T Two workers named sync-partner-orders are refused", func(t *testing.T) {
		wantFault(t, workerFaults(t, workers(syncWorker(), syncWorker()), "worker-name"), `"sync-partner-orders"`)
	})
	t.Run("ST-DECL-INV65-T A worker and a command both named sync-partner-orders are refused", func(t *testing.T) {
		service := workers(syncWorker()) + "commands:\n  - name: sync-partner-orders\n    writes: [Order]\n    fill: reserve\n    route: POST /sync\n"
		wantFault(t, workerFaults(t, service, "worker-name"), `"sync-partner-orders"`)
	})
	t.Run("ST-DECL-INV65-T A worker and a controller both named sync-partner-orders are refused", func(t *testing.T) {
		service := workers(syncWorker()) + "controllers:\n  - {name: sync-partner-orders, sources: [scan, watch], resync: 10m, replicas: 2, lease_ttl: 15s, tenants: each}\n"
		wantFault(t, workerFaults(t, service, "worker-name"), `"sync-partner-orders"`)
	})
	for _, name := range []string{"SyncOrders", "9-sync", "sync_orders", strings.Repeat("s", 64)} {
		t.Run("ST-DECL-INV65-T The worker name "+name+" is refused", func(t *testing.T) {
			service := workers(worker(name, "trigger: every 1m", "writes: [Order]", "fill: sync"))
			wantFault(t, workerFaults(t, service, "worker-name"), `"`+name+`"`)
		})
	}
}

func TestWorkerTrigger(t *testing.T) {
	with := func(trigger string) string {
		return workers(worker("sync-partner-orders", trigger, "writes: [Order]", "fill: sync", "effects:\n      - {destination: partner-orders, name: order-changed}"))
	}
	for _, trigger := range []string{"trigger: every 1m", "trigger: consumes order-changes"} {
		t.Run("ST-DECL-INV66-T A worker with the "+trigger+" is accepted", func(t *testing.T) {
			wantNoFault(t, workerFaults(t, with(trigger)))
		})
	}
	for _, c := range []struct{ name, trigger string }{
		{"every 5s", "trigger: every 5s"}, {"every 25h", "trigger: every 25h"}, {"every 1.5m", "trigger: every 1.5m"},
		{"hourly", "trigger: hourly"}, {"consumes partner-orders, which a component declares", "trigger: consumes partner-orders"},
		{"consumes authz", "trigger: consumes authz"}, {"no trigger", "permission: sync"},
	} {
		t.Run("ST-DECL-INV66-T A worker with the trigger "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, workerFaults(t, with(c.trigger), "worker-trigger"), `"sync-partner-orders"`)
		})
	}
	t.Run("ST-DECL-INV66-T Two workers that both consume order-changes are refused", func(t *testing.T) {
		service := workers(worker("apply-a", "trigger: consumes order-changes", "writes: [Order]", "fill: apply"), worker("apply-b", "trigger: consumes order-changes", "writes: [Order]", "fill: other"))
		wantFault(t, workerFaults(t, service, "worker-trigger"), `"apply-b"`, `"apply-a"`, "consumes order-changes")
	})
}

func TestWorkerWrites(t *testing.T) {
	with := func(writes string) string {
		return workers(worker("sync-partner-orders", "trigger: every 1m", "writes: "+writes, "fill: sync"))
	}
	t.Run("ST-DECL-INV67-T A worker that writes Order is accepted", func(t *testing.T) {
		wantNoFault(t, workerFaults(t, with("[Order]")))
	})
	t.Run("ST-DECL-INV67-T A worker that writes no entity is refused", func(t *testing.T) {
		wantFault(t, workerFaults(t, with("[]"), "worker-writes"), `"sync-partner-orders"`, "no entity")
	})
	t.Run("ST-DECL-INV67-T A worker that writes Order and Invoice, which is not declared, is refused", func(t *testing.T) {
		wantFault(t, workerFaults(t, with("[Order, Invoice]"), "worker-writes"), `"sync-partner-orders"`, `"Invoice"`)
	})
	reads := func(writes, reads string) string {
		return workers(worker("sync-partner-orders", "trigger: every 1m", "writes: "+writes, "reads: "+reads, "fill: sync"))
	}
	t.Run("ST-DECL-INV67-T A worker that only reads Order is accepted", func(t *testing.T) {
		wantNoFault(t, workerFaults(t, reads("[]", "[Order]")))
	})
	t.Run("ST-DECL-INV67-T A worker that writes Order and reads Invoice, which is not declared, is refused", func(t *testing.T) {
		wantFault(t, workerFaults(t, reads("[Order]", "[Invoice]"), "worker-reads"), `"sync-partner-orders"`, `"Invoice"`)
	})
	t.Run("ST-DECL-INV67-T A worker that writes and reads Order is refused", func(t *testing.T) {
		wantFault(t, workerFaults(t, reads("[Order]", "[Order]"), "worker-reads"), `"sync-partner-orders"`, `"Order"`, "both")
	})
}

func TestWorkerFill(t *testing.T) {
	with := func(fill string) string {
		return workers(worker("sync-partner-orders", "trigger: every 1m", "writes: [Order]", fill))
	}
	t.Run("ST-DECL-INV68-T A worker with a fill of the worker slot is accepted", func(t *testing.T) {
		wantNoFault(t, workerFaults(t, with("fill: sync")))
	})
	for _, c := range []struct{ name, fill, names string }{
		{"no fill", "fill: []", "no fill"},
		{"two fills", "fill: [sync, apply]", `"apply"`},
		{"a fill of the command slot", "fill: reserve", `"reserve"`},
		{"a fill of before_create", "fill: policy", `"policy"`},
	} {
		t.Run("ST-DECL-INV68-T A worker with "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, workerFaults(t, with(c.fill), "worker-fill"), `"sync-partner-orders"`, c.names)
		})
	}
}

func TestWorkerEffects(t *testing.T) {
	apply := func(lines ...string) string {
		return worker("apply-partner-replies", append([]string{"trigger: consumes partner-replies", "writes: [Order]", "fill: apply"}, lines...)...)
	}
	with := func(effects ...string) string {
		return workers(worker("sync-partner-orders", "trigger: every 1m", "writes: [Order]", "fill: sync", "effects:\n      - "+strings.Join(effects, "\n      - ")), apply())
	}
	for _, c := range []struct{ name, service string }{
		{"partner-orders: order-changed", with("{destination: partner-orders, name: order-changed}")},
		{"partner-replies: reply-expected, which a worker consumes", with("{destination: partner-replies, name: reply-expected}")},
	} {
		t.Run("ST-DECL-INV70-T A worker effect "+c.name+" is accepted", func(t *testing.T) {
			wantNoFault(t, workerFaults(t, c.service))
		})
	}
	for _, c := range []struct{ name, service, worker, names string }{
		{"nowhere: order-changed", with("{destination: nowhere, name: order-changed}"), "sync-partner-orders", `"nowhere"`},
		{"partner-orders: order-changed twice", with("{destination: partner-orders, name: order-changed}", "{destination: partner-orders, name: order-changed}"), "sync-partner-orders", `"order-changed"`},
		{"authz: holder", with("{destination: authz, name: holder}"), "sync-partner-orders", "only through the authorization handle"},
		{"audit: entry", with("{destination: audit, name: entry}"), "sync-partner-orders", "only through the audit runtime"},
		{"partner-replies: again from the worker that consumes it", workers(syncWorker(), apply("effects:\n      - {destination: partner-replies, name: again}")), "apply-partner-replies", "consumes itself"},
	} {
		t.Run("ST-DECL-INV70-T A worker effect "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, workerFaults(t, c.service, "worker-effect"), `"`+c.worker+`"`, c.names)
		})
	}
	t.Run("ST-DECL-INV42-T A command effect for a destination that a worker consumes is accepted", func(t *testing.T) {
		service := workers(syncWorker(), apply()) + "commands:\n  - name: reserve\n    writes: [Order]\n    fill: reserve\n    route: POST /reserve\n    effects:\n      - {destination: partner-replies, name: reply}\n"
		wantNoFault(t, workerFaults(t, service))
	})
}

func TestWorkerMode(t *testing.T) {
	for _, c := range []struct{ name, modes string }{{"the modes api and worker", "[api, worker]"}, {"the modes omitted", ""}} {
		t.Run("ST-DECL-INV71-T A worker with "+c.name+" is accepted", func(t *testing.T) {
			wantNoFault(t, workerFaults(t, orders(c.modes)+"workers:\n"+syncWorker()))
		})
	}
	t.Run("ST-DECL-INV71-T A worker with the modes api only is refused", func(t *testing.T) {
		faults := workerFaults(t, orders("[api]")+"workers:\n"+syncWorker(), "worker-mode")
		wantFault(t, faults, "sync-partner-orders", "modes api")
	})
}

func TestWorkerLease(t *testing.T) {
	for _, c := range []struct {
		name  string
		lines []string
	}{{"no lease setting", nil}, {"Single no", []string{"single: false"}}, {"Lease TTL 30s", []string{"lease_ttl: 30s"}}} {
		t.Run("ST-DECL-INV74-T A worker with "+c.name+" is accepted", func(t *testing.T) {
			wantNoFault(t, workerFaults(t, workers(syncWorker(c.lines...))))
		})
	}
	t.Run("ST-DECL-INV74-T Single and Lease TTL get their defaults", func(t *testing.T) {
		declared := readDeclaration(t, workers(syncWorker()))
		if w := declared.Workers[0]; !WorkerSingle(w) || WorkerLeaseTTL(w) != "15s" {
			t.Errorf("the worker is single %v with the lease TTL %s, want true and 15s", WorkerSingle(w), WorkerLeaseTTL(w))
		}
		got := map[string]Setting{}
		for _, setting := range ResolvedSettings(declared) {
			got[setting.Path] = setting
		}
		if s := got["workers[0].single"]; s.Value != true || !s.Default {
			t.Errorf("checking reports single %+v, want the default true", s)
		}
		if s := got["workers[0].lease_ttl"]; s.Value != "15s" || !s.Default {
			t.Errorf("checking reports the lease TTL %+v, want the default 15s", s)
		}
	})
	consumer := func(line string) string {
		return workers(worker("sync-partner-orders", "trigger: consumes order-changes", "writes: [Order]", "fill: sync", line))
	}
	for _, c := range []struct{ name, service, field string }{
		{"Single no and Lease TTL 30s", workers(syncWorker("single: false", "lease_ttl: 30s")), "lease TTL"},
		{"Lease TTL 5s", workers(syncWorker("lease_ttl: 5s")), "lease TTL"},
		{"Lease TTL 10m", workers(syncWorker("lease_ttl: 10m")), "lease TTL"},
		{"consumes order-changes and Single yes", consumer("single: true"), "single"},
		{"consumes order-changes and Lease TTL 30s", consumer("lease_ttl: 30s"), "lease TTL"},
	} {
		t.Run("ST-DECL-INV74-T A worker with "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, workerFaults(t, c.service, "worker-lease"), `"sync-partner-orders"`, "the "+c.field)
		})
	}
}

func TestWorkerCycles(t *testing.T) {
	stage := func(name, consumes string, effects ...string) string {
		lines := []string{"trigger: consumes " + consumes, "writes: [Order]", "fill: sync"}
		if len(effects) > 0 {
			var items []string
			for _, effect := range effects {
				destination, effectName, _ := strings.Cut(effect, ": ")
				items = append(items, fmt.Sprintf("{destination: %s, name: %s}", destination, effectName))
			}
			lines = append(lines, "effects:\n      - "+strings.Join(items, "\n      - "))
		}
		return worker(name, lines...)
	}
	cycleFaults := func(t *testing.T, service string) []ValidationError {
		t.Helper()
		return workerFaults(t, service, "worker-cycle")
	}
	t.Run("ST-DECL-INV76-T Workers that stage without a cycle are accepted", func(t *testing.T) {
		wantNoFault(t, cycleFaults(t, workers(stage("a", "x", "y: next"), stage("b", "y"))))
	})
	t.Run("ST-DECL-INV76-T A cycle of two workers is refused and named", func(t *testing.T) {
		faults := cycleFaults(t, workers(stage("a", "x", "y: next"), stage("b", "y", "x: back")))
		wantFault(t, faults, "x, a, y, b, x")
		if faults[0].Found != "x, a, y, b, x" {
			t.Errorf("the fault found %q, want the cycle x, a, y, b, x", faults[0].Found)
		}
	})
	t.Run("ST-DECL-INV76-T A cycle of three workers is refused and named", func(t *testing.T) {
		faults := cycleFaults(t, workers(stage("c", "z", "x: start"), stage("b", "y", "z: loop"), stage("a", "x", "y: next")))
		wantFault(t, faults, "x, a, y, b, z, c, x")
	})
}

func TestWorkerCount(t *testing.T) {
	many := func(n int) string {
		var items []string
		for i := 1; i <= n; i++ {
			items = append(items, worker(fmt.Sprintf("w-%d", i), "trigger: every 1m", "writes: [Order]", "fill: sync"))
		}
		return workers(items...)
	}
	t.Run("ST-DECL-INV79-T A declaration with 127 workers is accepted", func(t *testing.T) {
		wantNoFault(t, workerFaults(t, many(127)))
	})
	t.Run("ST-DECL-INV79-T A declaration with 128 workers is refused", func(t *testing.T) {
		wantFault(t, workerFaults(t, many(128), "worker-count"), "128")
	})
}

func TestWorkerEffectStagingType(t *testing.T) {
	stager := func(resourceType string) string {
		return worker("sync-documents", "trigger: every 1m", "writes: [Todo]", "fill: sync", "resource_type: "+resourceType, "permission: view", "effects:\n      - {destination: document-replies, name: reply}")
	}
	t.Run("ST-DECL-INV70-T With authorization bound, a worker on document that stages for a worker on document is accepted", func(t *testing.T) {
		expectAccepted(t, checkOperations(t, td03Staging(t, stager("document"), ""), operationFacts()))
	})
	t.Run("ST-DECL-INV70-T With authorization bound, a worker on folder that stages for a worker on document is refused", func(t *testing.T) {
		expectOneFault(t, checkOperations(t, td03Staging(t, stager("folder"), ""), operationFacts()), "worker-effect", "sync-documents", "document-replies: reply", `"folder"`, `"document"`)
	})
}

// listOrders is ST-DECL-TD02 with the modes api and worker and the owned entity Order with the fields status (enum
// open, closed), synced (bool), due_at (timestamp), amount (int64), note (optional string) and body (jsonb), and
// Lock at most 50, and the worker scan-orders with the trigger every 1m that writes Order, bound to the fill sync,
// with the lists given.
func listOrders(lists ...string) string {
	service := strings.Replace(td02, "entities:\n", "entities:\n  - name: Order\n    lock_at_most: 50\n    fields:\n"+
		"      - {name: status, type: enum, values: [open, closed]}\n      - {name: synced, type: bool}\n"+
		"      - {name: due_at, type: timestamp, client_set: true}\n      - {name: amount, type: int64}\n"+
		"      - {name: note, type: string, optional: true}\n      - {name: body, type: jsonb}\n", 1)
	service += "modes: [api, worker]\nworkers:\n" + worker("scan-orders", "trigger: every 1m", "writes: [Order]", "fill: sync",
		"lists:\n      "+strings.Join(lists, "\n      "))
	return service
}

func TestWorkerLists(t *testing.T) {
	accepted := []struct{ name, lists string }{
		{"one equals term", "- {name: open-orders, entity: Order, filter: [{field: status, op: equals, value: open}]}"},
		{"every operator", "- {name: due, entity: Order, lock: true, filter: [{field: due_at, op: less_than, now: true}, {field: amount, op: greater_than_or_equals, value: 10}, {field: amount, op: less_than_or_equals, value: 99}, {field: synced, op: not_equals, value: true}, {field: note, op: is_set}, {field: due_at, op: greater_than, value: \"2026-01-01T00:00:00Z\"}, {field: status, op: not_equals, value: closed}, {field: amount, op: less_than, value: 1000}]}"},
		{"two lists", "- {name: open-orders, entity: Order, filter: [{field: status, op: equals, value: open}]}\n      - {name: synced-orders, entity: Order, filter: [{field: synced, op: equals, value: true}]}"},
		{"two parameters", "- {name: of-status, entity: Order, filter: [{field: status, op: equals, param: status}, {field: due_at, op: less_than, param: due_before}, {field: synced, op: equals, value: false}]}"},
	}
	for _, c := range accepted {
		t.Run("ST-DECL-INV94-T A worker list with "+c.name+" is accepted", func(t *testing.T) {
			wantNoFault(t, workerFaults(t, listOrders(c.lists)))
		})
	}
	refused := []struct{ name, lists, names string }{
		{"a list name used twice", "- {name: open-orders, entity: Order, filter: [{field: status, op: equals, value: open}]}\n      - {name: open-orders, entity: Order, filter: [{field: synced, op: equals, value: true}]}", "used twice"},
		{"a list name that is not kebab case", "- {name: OpenOrders, entity: Order, filter: [{field: status, op: equals, value: open}]}", "kebab"},
		{"an entity that the worker does not write", "- {name: open-todos, entity: Todo, filter: [{field: done, op: equals, value: false}]}", `"Todo"`},
		{"no term", "- {name: all, entity: Order, filter: []}", "one to 8"},
		{"nine terms", "- {name: many, entity: Order, filter: [" + strings.Repeat("{field: amount, op: greater_than, value: 1}, ", 8) + "{field: amount, op: greater_than, value: 1}]}", "9 terms"},
		{"a field that the entity does not declare", "- {name: x, entity: Order, filter: [{field: colour, op: equals, value: red}]}", `"colour"`},
		{"an unknown operator", "- {name: x, entity: Order, filter: [{field: amount, op: like, value: 1}]}", "operator"},
		{"a value of another type", "- {name: x, entity: Order, filter: [{field: amount, op: equals, value: ten}]}", "field type int64"},
		{"an enum value that is not declared", "- {name: x, entity: Order, filter: [{field: status, op: equals, value: lost}]}", "field type enum"},
		{"now on a field that is not a timestamp", "- {name: x, entity: Order, filter: [{field: amount, op: less_than, now: true}]}", "timestamp"},
		{"no value", "- {name: x, entity: Order, filter: [{field: amount, op: equals}]}", "needs a value"},
		{"a value and now", "- {name: x, entity: Order, filter: [{field: due_at, op: less_than, now: true, value: \"2026-01-01T00:00:00Z\"}]}", "not both"},
		{"is_set with a value", "- {name: x, entity: Order, filter: [{field: note, op: is_set, value: a}]}", "no value"},
		{"an order operator on a bool", "- {name: x, entity: Order, filter: [{field: synced, op: less_than, value: true}]}", "bool"},
		{"a jsonb field compared", "- {name: x, entity: Order, filter: [{field: body, op: equals, value: a}]}", "is_set"},
		{"a parameter used twice", "- {name: x, entity: Order, filter: [{field: amount, op: greater_than, param: amount}, {field: amount, op: less_than, param: amount}]}", `"amount" is used twice`},
		{"a parameter and a value", "- {name: x, entity: Order, filter: [{field: amount, op: equals, value: 1, param: amount}]}", "one of a value, now and a parameter"},
		{"a parameter and now", "- {name: x, entity: Order, filter: [{field: due_at, op: less_than, now: true, param: due}]}", "one of a value, now and a parameter"},
		{"a parameter name that is not snake case", "- {name: x, entity: Order, filter: [{field: amount, op: equals, param: Amount}]}", "lower snake case"},
		{"is_set with a parameter", "- {name: x, entity: Order, filter: [{field: note, op: is_set, param: note}]}", "no value"},
		{"a parameter on a jsonb field", "- {name: x, entity: Order, filter: [{field: body, op: equals, param: body}]}", "is_set"},
		{"an order operator on a bool parameter", "- {name: x, entity: Order, filter: [{field: synced, op: greater_than, param: synced}]}", "bool"},
	}
	for _, c := range refused {
		t.Run("ST-DECL-INV94-T A worker list with "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, workerFaults(t, listOrders(c.lists), "worker-list"), `"scan-orders"`, c.names)
		})
	}
	// A command declares its lists as a worker does (walkthrough 7, B1 and NB4).
	commandLists := func(lists string) string {
		return listOrders("- {name: open-orders, entity: Order, filter: [{field: status, op: equals, value: open}]}") +
			"commands:\n" + command("place-orders", "writes: [Order]", "fill: place", "route: POST /orders/{orderId}/place", "lists:\n      "+lists)
	}
	t.Run("ST-DECL-INV94-T A command list with a parameter and a lock is accepted", func(t *testing.T) {
		wantNoFault(t, workerFaults(t, commandLists("- {name: of-status, entity: Order, lock: true, filter: [{field: status, op: equals, param: status}, {field: due_at, op: less_than, now: true}]}"), "command-list", "worker-list"))
	})
	for _, c := range []struct{ name, lists, names string }{
		{"an entity that the command does not write", "- {name: todos, entity: Todo, filter: [{field: done, op: equals, param: done}]}", "the command does not write"},
		{"a parameter used twice", "- {name: x, entity: Order, filter: [{field: amount, op: greater_than, param: amount}, {field: amount, op: less_than, param: amount}]}", `"amount" is used twice`},
		{"a field that the entity does not declare", "- {name: x, entity: Order, filter: [{field: colour, op: equals, param: colour}]}", `"colour"`},
	} {
		t.Run("ST-DECL-INV94-T A command list with "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, workerFaults(t, commandLists(c.lists), "command-list", "worker-list"), `command "place-orders"`, `list "`, c.names)
		})
	}
	readOrders := func(list string) string {
		return strings.Replace(strings.Replace(listOrders(list), "writes: [Order]", "writes: []\n    reads: [Order]", 1), "scan-orders", "report-orders", 1)
	}
	t.Run("ST-DECL-INV94-T A list of an entity that the worker reads is accepted", func(t *testing.T) {
		wantNoFault(t, workerFaults(t, readOrders("- {name: open-orders, entity: Order, filter: [{field: status, op: equals, value: open}]}")))
	})
	t.Run("ST-DECL-INV94-T A locked list of an entity that the worker reads is refused", func(t *testing.T) {
		wantFault(t, workerFaults(t, readOrders("- {name: due, entity: Order, lock: true, filter: [{field: due_at, op: less_than, now: true}]}"), "worker-list"), `"report-orders"`, `"due"`, "only reads")
	})
	t.Run("ST-DECL-INV94-T A locked list of an entity with no Lock at most is refused", func(t *testing.T) {
		service := strings.Replace(listOrders("- {name: due, entity: Order, lock: true, filter: [{field: due_at, op: less_than, now: true}]}"), "    lock_at_most: 50\n", "", 1)
		wantFault(t, workerFaults(t, service, "worker-list"), `"scan-orders"`, `"due"`, "Lock at most")
	})
}

func TestLockAtMost(t *testing.T) {
	entity := func(line string) string {
		return strings.Replace(td02, "  - name: Todo\n", "  - name: Todo\n"+line, 1)
	}
	for _, line := range []string{"", "    lock_at_most: 1\n", "    lock_at_most: 1000\n"} {
		t.Run("ST-DECL-INV81-T The Lock at most "+strings.TrimSpace(line)+" is accepted", func(t *testing.T) {
			if faults := faultsOf(readDeclaration(t, entity(line))); len(faults) != 0 {
				t.Fatalf("want no fault, got %+v", faults)
			}
		})
	}
	for _, value := range []string{"0", "1001", "-3", "2.5"} {
		t.Run("ST-DECL-INV81-T The Lock at most "+value+" is refused", func(t *testing.T) {
			wantFault(t, faultsOf(readDeclaration(t, entity("    lock_at_most: "+value+"\n"))), `"Todo"`, value)
		})
	}
	for _, value := range []string{"many"} {
		t.Run("ST-DECL-INV81-T The Lock at most "+value+" is refused when it is read", func(t *testing.T) {
			if _, _, err := parser.ReadServiceDeclaration([]byte(entity("    lock_at_most: "+value+"\n")), "service.yaml"); err == nil {
				t.Fatal("the declaration was read")
			}
		})
	}
}

func TestChildLists(t *testing.T) {
	// Order has a resource type; Line is a child of Order that also refers to Note; Pair is a child of Order and
	// Store; Note has no authorization of its rows (security review of PR #416, M1).
	entities := []types.Entity{
		{Name: "Order", EntityAuthorization: types.EntityAuthorization{ResourceType: "order"}, Fields: []types.Field{{Name: "status", Type: types.FieldTypeString}, {Name: "store_id", Type: types.FieldTypeRef, To: "Store"}}},
		{Name: "Store", EntityAuthorization: types.EntityAuthorization{ResourceType: "store"}, Fields: []types.Field{{Name: "city", Type: types.FieldTypeString}}},
		{Name: "Line", Fields: []types.Field{{Name: "order_id", Type: types.FieldTypeRef, To: "Order"}, {Name: "sku", Type: types.FieldTypeString}, {Name: "note_id", Type: types.FieldTypeRef, To: "Note"}}},
		{Name: "Pair", Fields: []types.Field{{Name: "order_id", Type: types.FieldTypeRef, To: "Order"}, {Name: "store_id", Type: types.FieldTypeRef, To: "Store"}}},
		{Name: "Note", Fields: []types.Field{{Name: "body", Type: types.FieldTypeString}}},
	}
	parent := types.ListTerm{Field: "order_id", Op: types.ListEquals, Parent: true}
	list := func(entity string, terms ...types.ListTerm) []types.WorkerList {
		return []types.WorkerList{{Name: "rows", Entity: entity, Filter: terms}}
	}
	writes := []string{"Order", "Store", "Line", "Pair", "Note"}
	for name, lists := range map[string][]types.WorkerList{
		"a child list with its parent term":          list("Line", parent),
		"a child list with its parent term and more": list("Line", parent, types.ListTerm{Field: "sku", Op: types.ListEquals, Param: "sku"}),
		"a list of an entity with a resource type":   list("Order", types.ListTerm{Field: "status", Op: types.ListEquals, Param: "status"}),
		"a list of an entity with no authorization":  list("Note", types.ListTerm{Field: "body", Op: types.ListIsSet}),
	} {
		t.Run("ST-DECL-INV94-T "+name+" is accepted", func(t *testing.T) {
			wantNoFault(t, validateLists("command", "place-order", "commands[0]", writes, nil, lists, entities))
		})
	}
	for name, c := range map[string]struct {
		lists []types.WorkerList
		want  string
	}{
		"a child list with no parent term":              {list("Line", types.ListTerm{Field: "order_id", Op: types.ListEquals, Param: "order_id"}), "exactly one term"},
		"a child list with two parent terms":            {list("Line", parent, parent), "exactly one term"},
		"a parent term on an entity with a type":        {list("Order", types.ListTerm{Field: "store_id", Op: types.ListEquals, Parent: true}), "fits only a list of a child entity"},
		"a parent term with another operator":           {list("Line", types.ListTerm{Field: "order_id", Op: types.ListNotEquals, Parent: true}), "only the operator equals"},
		"a parent term with a parameter":                {list("Line", types.ListTerm{Field: "order_id", Op: types.ListEquals, Parent: true, Param: "order_id"}), "only the operator equals"},
		"a parent term on a field that is no reference": {list("Line", types.ListTerm{Field: "sku", Op: types.ListEquals, Parent: true}), "needs a reference field"},
		"a child of two parents":                        {list("Pair", parent), "more than one entity"},
		"a parent term on a reference to an entity with no resource type": {list("Line", types.ListTerm{Field: "note_id", Op: types.ListEquals, Parent: true}),
			"needs a reference to an entity with a resource type, and Note has none"},
	} {
		t.Run("ST-DECL-INV94-T "+name+" is refused", func(t *testing.T) {
			wantFault(t, validateLists("command", "place-order", "commands[0]", writes, nil, c.lists, entities), `command "place-order"`, `list "rows"`, c.want)
		})
	}
	t.Run("ST-DECL-INV94-T A child list of a worker that reads Line and Order is accepted", func(t *testing.T) {
		wantNoFault(t, validateLists("worker", "report-orders", "workers[0]", nil, []string{"Order", "Line"}, list("Line", parent), entities))
	})
	t.Run("ST-DECL-INV94-T A child list whose parent the worker does not write or read is refused", func(t *testing.T) {
		wantFault(t, validateLists("worker", "report-orders", "workers[0]", nil, []string{"Line"}, list("Line", parent), entities), `worker "report-orders"`, `list "rows"`, "does not write or read")
	})
}

// TestWorkerRootCreate proves ST-SUP-RP10-LC17-T03: checking warns for each entity that a worker can create with no
// parent when the type tenant of the schema has no permission of the worker, which generation refuses
// (ST-SUP-RC35-OC17), and gives no warning when the schema has it or the entity always has a parent.
func TestWorkerRootCreate(t *testing.T) {
	ref := func(name, to string, optional bool) types.Field {
		return types.Field{Name: name, Type: types.FieldTypeRef, To: to, Optional: optional}
	}
	service := func(writes ...string) *types.ServiceDeclaration {
		return &types.ServiceDeclaration{
			Authorization: &types.Authorization{Provider: "spicedb"},
			Entities: []types.Entity{
				{Name: "Roster", EntityAuthorization: types.EntityAuthorization{ResourceType: "roster"}, Fields: []types.Field{{Name: "label", Type: types.FieldTypeString}}},
				{Name: "PatientCard", EntityAuthorization: types.EntityAuthorization{ResourceType: "patient_card"}, Fields: []types.Field{ref("roster_id", "Roster", true), {Name: "patient_ref", Type: types.FieldTypeString}}},
				{Name: "Visit", EntityAuthorization: types.EntityAuthorization{ResourceType: "visit"}, Fields: []types.Field{ref("roster_id", "Roster", false)}},
				{Name: "Note", Fields: []types.Field{ref("visit_id", "Visit", false)}},
			},
			Workers: []types.Worker{{Name: "track-patients", Trigger: "consumes patient-changes", Writes: writes, ResourceType: "patient_card", Permission: "track"}},
		}
	}
	schema := func(tenant string) *AuthzSchema {
		t.Helper()
		compiled, err := CompileAuthzSchema("authz/schema.zed", []byte("definition user {}\ndefinition service_account {}\ndefinition tenant {\n\trelation worker_track_patients: service_account\n"+tenant+"}\n"))
		if err != nil {
			t.Fatal(err)
		}
		return compiled
	}
	without, with := schema(""), schema("\tpermission track = worker_track_patients\n")
	t.Run("ST-SUP-RP10-LC17-T03 a worker that writes a root entity and lacks its permission on tenant is warned, with the worker, the entity and the permission", func(t *testing.T) {
		warnings := RootCreateWarnings(service("Roster", "PatientCard"), without)
		if len(warnings) != 2 || warnings[0].Rule != "worker-root-create" || warnings[0].Path != "workers[0].writes" || warnings[0].Found != "Roster" || warnings[1].Found != "PatientCard" {
			t.Fatalf("warnings = %+v, want one worker-root-create warning for Roster and one for PatientCard", warnings)
		}
		for _, want := range []string{`worker "track-patients"`, "Roster", "permission track", "tenant", "ST-SUP-RP10-LC17", "ST-SUP-RC35-OC17", "track = worker_track_patients"} {
			if !strings.Contains(warnings[0].Message, want) {
				t.Errorf("message %q does not hold %q", warnings[0].Message, want)
			}
		}
	})
	t.Run("ST-SUP-RP10-LC17-T03 a schema that gives the permission on tenant, an entity with a required reference, a child entity and no schema are not warned", func(t *testing.T) {
		if warnings := RootCreateWarnings(service("Roster", "PatientCard"), with); len(warnings) != 0 {
			t.Fatalf("warnings with the permission = %+v, want none", warnings)
		}
		if warnings := RootCreateWarnings(service("Visit", "Note"), without); len(warnings) != 0 {
			t.Fatalf("warnings = %+v, want none", warnings)
		}
		if warnings := RootCreateWarnings(service("Roster"), nil); len(warnings) != 0 {
			t.Fatalf("warnings with no schema = %+v, want none", warnings)
		}
		open := service("Roster")
		open.Authorization = nil
		if roots := RootCreates(open); len(roots) != 0 {
			t.Fatalf("a service with no authorization gave the root creates %+v, want none", roots)
		}
		if roots := RootCreates(service("Roster", "PatientCard", "Visit", "Note")); len(roots) != 2 || roots[0].Entity != "Roster" || roots[1].Entity != "PatientCard" {
			t.Fatalf("the root creates are %+v, want Roster and PatientCard", roots)
		}
	})
}
