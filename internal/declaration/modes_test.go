package declaration

import (
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/types"
)

func TestNeededModes(t *testing.T) {
	t.Run("ST-DECL-INV89-T Declared modes that leave out a mode the declared features need are refused", func(t *testing.T) {
		// The declaration of ST-DECL-TD02: the collection todos of Todo, with the modes given.
		declared := func(modes ...string) *types.ServiceDeclaration {
			return &types.ServiceDeclaration{Name: "todo-svc", Modes: modes,
				Entities:    []types.Entity{{Name: "Todo"}},
				Collections: []types.Collection{{Name: "todos", Entity: "Todo"}}}
		}
		faults := func(svc *types.ServiceDeclaration, runtimeData bool) map[string]string {
			t.Helper()
			got := map[string]string{}
			for _, f := range validateNeededModes(svc, Facts{RuntimeData: runtimeData}) {
				if f.Rule != "mode-needed" || f.Path != "modes" {
					t.Fatalf("unexpected fault %+v", f)
				}
				for _, mode := range knownModes {
					if strings.Contains(f.Message, "do not hold "+mode+",") {
						got[mode] = f.Message
					}
				}
			}
			return got
		}
		for _, modes := range [][]string{nil, {}, {"api", "worker", "controller"}, {"controller", "api", "worker"}} {
			if got := faults(declared(modes...), true); len(got) != 0 {
				t.Errorf("the modes %v got %v, want them accepted", modes, got)
			}
		}
		got := faults(declared("api"), true)
		if len(got) != 2 || !strings.Contains(got["worker"], "outbox relay and the audit sink") || !strings.Contains(got["controller"], "audit anchor and maintenance jobs") {
			t.Errorf("the modes [api] on the runtime data path got %v, want the faults of worker and controller", got)
		}
		got = faults(declared("worker", "controller"), true)
		if len(got) != 1 || !strings.Contains(got["api"], "the collections todos") {
			t.Errorf("the modes [worker controller] got %v, want the fault of api for the collection todos", got)
		}
		svc := declared("api", "worker")
		svc.Commands = []types.Command{{Name: "add-note"}}
		svc.Queries = []types.Query{{Name: "order-summary"}}
		svc.Workers = []types.Worker{{Name: "sync"}}
		svc.Authorization = &types.Authorization{}
		got = faults(svc, true)
		if len(got) != 1 || !strings.Contains(got["controller"], "reconcile of the worker tuples") {
			t.Errorf("the modes [api worker] with workers and authorization got %v, want the fault of controller with the reconcile", got)
		}
		if got := faults(declared("api"), false); len(got) != 0 {
			t.Errorf("the modes [api] off the runtime data path got %v, want them accepted", got)
		}
		svc = declared("worker", "controller")
		svc.Collections = nil
		svc.Commands = []types.Command{{Name: "add-note"}}
		if got := faults(svc, true); len(got) != 1 || !strings.Contains(got["api"], "the commands add-note") {
			t.Errorf("a command with no api mode got %v, want the fault of api", got)
		}
	})
}
