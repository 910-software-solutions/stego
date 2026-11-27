package compiler

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/910-software-solutions/stego/internal/registry"
	"github.com/910-software-solutions/stego/internal/types"
)

// selectionRegistry writes a registry whose archetype plain holds the ga component api, with a runtime block, and
// the five infrastructure components at stage alpha with the sunset 2099-12-31, each at its own version. audit-log
// requires audit-sink, which no component provides.
func selectionRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"archetypes/plain/archetype.yaml": "kind: archetype\nruntime: {module: github.com/910-software-solutions/stego/registry/runtime, version: v0.1.0, sum: 'h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='}\n" +
			"name: plain\nlanguage: go\nversion: 1.0.0\ncomponents: [api]\nconventions:\n  layout: flat\n",
		"components/api/component.yaml": "kind: component\nenvironment: []\narchitecture:\n  layer: handler\n  flow: Serves the test.\n  ports:\n    http-api: inbound\nname: api\nversion: 1.0.0\nstage: ga\noutput_namespace: internal/api\nrequires: []\nprovides: [http-api]\nslots: []\n",
	}
	versions := map[string]string{"tenancy": "0.2.0", "audit-log": "0.1.1", "spicedb-authz": "0.2.2", "idempotency": "0.1.3", "ledger": "0.1.4", "kafka-consumer": "0.1.5"}
	for name, version := range versions {
		requires := "[]"
		if name == "audit-log" {
			requires = "[audit-sink]"
		}
		files["components/"+name+"/component.yaml"] = "kind: component\nenvironment: []\nname: " + name + "\nversion: " + version +
			"\nstage: alpha\nsunset: 2099-12-31\noutput_namespace: internal/" + strings.ReplaceAll(name, "-", "") +
			"\nrequires: " + requires + "\nprovides: []\nslots: []\narchitecture:\n  layer: handler\n  flow: Serves the test.\n  ports: {" + strings.Trim(requires, "[]") + map[bool]string{true: ": outbound", false: ""}[requires != "[]"] + "}\n"
	}
	for name, data := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := registry.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// setting is one declaration that sets one setting, with the component it implies and the version the registry
// pins for it.
type setting struct {
	name, component, version string
	set                      func(*types.ServiceDeclaration)
}

var settings = []setting{
	{"tenancy", "tenancy", "0.2.0", func(s *types.ServiceDeclaration) {
		s.Tenancy = &types.Tenancy{Kind: "pooled", Key: "tenant_id", Claim: "tenant_id"}
	}},
	{"audit", "audit-log", "0.1.1", func(s *types.ServiceDeclaration) { s.Audit = &types.Audit{Level: "change"} }},
	{"authorization", "spicedb-authz", "0.2.2", func(s *types.ServiceDeclaration) {
		s.Authorization = &types.Authorization{Provider: "spicedb"}
	}},
	{"idempotency", "idempotency", "0.1.3", func(s *types.ServiceDeclaration) {
		s.Idempotency = &types.Idempotency{RequiredOn: []types.Operation{"create"}}
	}},
	{"ledger", "ledger", "0.1.4", func(s *types.ServiceDeclaration) {
		s.Entities = append(s.Entities, types.Entity{Name: "Wallet", Kind: "ledger"})
	}},
	// A declared subscription selects kafka-consumer (decision on ST-SUP-Q158).
	{"subscriptions", "kafka-consumer", "0.1.5", func(s *types.ServiceDeclaration) {
		s.Subscriptions = []types.Subscription{{Name: "order-changes", Source: "orders", Events: []string{"orders.order.created"}}}
	}},
}

func declare(st setting, acceptances ...types.ExperimentalAcceptance) *types.ServiceDeclaration {
	s := &types.ServiceDeclaration{Kind: "service", Name: "svc", Archetype: "plain", Language: "go",
		Entities: []types.Entity{{Name: "Item"}}, ExperimentalAcceptances: acceptances}
	st.set(s)
	return s
}

// atClock sets the clock that the expiry of an acceptance reads.
func atClock(t *testing.T, at string) {
	t.Helper()
	now, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatal(err)
	}
	saved := acceptanceClock
	acceptanceClock = func() time.Time { return now }
	t.Cleanup(func() { acceptanceClock = saved })
}

func TestComponentSelection(t *testing.T) {
	t.Run("ST-GEN-INV37-T Each setting selects its component, and a component that is not ga needs an acceptance", func(t *testing.T) {
		reg := selectionRegistry(t)
		archetype := reg.Archetype("plain")
		atClock(t, "2099-06-01T23:59:59Z")
		for _, st := range settings {
			accepted := types.ExperimentalAcceptance{Component: st.component, AcceptedUntil: "2099-06-01", Reason: "pilot"}

			// With an acceptance that names it, the component is in the active set at its pinned version.
			svc := declare(st, accepted)
			_, active, faults := activeSet(archetype, svc, reg)
			faults = append(faults, validateSelectedComponents(svc, reg)...)
			if len(faults) != 0 {
				t.Fatalf("%s with an acceptance: faults %v", st.name, faults)
			}
			if c := active[st.component]; c == nil || c.Version != st.version {
				t.Fatalf("%s with an acceptance: the active set holds %v, want %s at %s", st.name, c, st.component, st.version)
			}
			if active["api"] == nil {
				t.Fatalf("%s: the archetype's component api left the active set", st.name)
			}

			// With no acceptance, and with one for another component, one fault names the component, its stage and
			// the acceptance to add.
			for _, svc := range []*types.ServiceDeclaration{declare(st),
				declare(st, types.ExperimentalAcceptance{Component: "other", AcceptedUntil: "2099-06-01", Reason: "pilot"})} {
				faults := validateSelectedComponents(svc, reg)
				if len(faults) != 1 {
					t.Fatalf("%s with no acceptance: %d faults %v, want one", st.name, len(faults), faults)
				}
				for _, want := range []string{st.component, "alpha", "experimental_acceptances", "component: " + st.component} {
					if !strings.Contains(faults[0].Message, want) {
						t.Fatalf("%s with no acceptance: the fault %q does not name %q", st.name, faults[0].Message, want)
					}
				}
			}
		}

		// From the start of the next day in UTC, each acceptance has run out.
		atClock(t, "2099-06-02T00:00:00Z")
		for _, st := range settings {
			faults := validateSelectedComponents(declare(st, types.ExperimentalAcceptance{Component: st.component, AcceptedUntil: "2099-06-01", Reason: "pilot"}), reg)
			if len(faults) != 1 || !strings.Contains(faults[0].Message, st.component) || !strings.Contains(faults[0].Message, "alpha") ||
				!strings.Contains(faults[0].Message, "run out") || !strings.Contains(faults[0].Message, "experimental_acceptances") {
				t.Fatalf("%s with an acceptance that has run out: faults %v, want one that names the component, its stage and the acceptance to add", st.name, faults)
			}
		}

		// Audit on, with no component that provides audit-sink: the platform binds it.
		svc := declare(settings[1], types.ExperimentalAcceptance{Component: "audit-log", AcceptedUntil: "2099-12-31", Reason: "pilot"})
		_, active, faults := activeSet(archetype, svc, reg)
		if len(faults) != 0 || active["audit-log"] == nil {
			t.Fatalf("audit with audit-sink left to the platform: faults %v, active %v", faults, slices.Sorted(maps.Keys(active)))
		}

		// A setting the declaration omits selects nothing.
		_, active, _ = activeSet(archetype, &types.ServiceDeclaration{Kind: "service", Name: "svc", Archetype: "plain", Language: "go"}, reg)
		if len(active) != 1 {
			t.Fatalf("a declaration with no setting: the active set is %v, want api only", slices.Sorted(maps.Keys(active)))
		}
	})
}

// betaComponent is a beta component with the sunset given, for a unit test of the acceptance rule.
func betaComponent(sunset string) *registry.Component {
	return &registry.Component{Name: "local-a", Stage: types.StageBeta, Sunset: sunset}
}

func TestAcceptanceRule(t *testing.T) {
	t.Run("ST-GEN-INV37-T Each setting selects its component, and a component that is not ga needs an acceptance", func(t *testing.T) {
		// The boundary day and the form of the date of an acceptance, which one rule holds for a component that a
		// setting selects and for one that an override brings (acceptanceFault; sec-501 I1).
		atClock(t, "2099-06-01T23:59:59Z")
		component := betaComponent("2099-12-31")
		accept := func(until string) *types.ServiceDeclaration {
			return &types.ServiceDeclaration{Overrides: map[string]any{"local-port": "local-a"},
				ExperimentalAcceptances: []types.ExperimentalAcceptance{{Component: "local-a", AcceptedUntil: until, Reason: "pilot"}}}
		}
		faults := func(svc *types.ServiceDeclaration) []*ValidationError {
			var all []*ValidationError
			if fault := acceptanceFault(svc, "local-a", component, "tenancy", "the tenancy setting selects the component local-a"); fault != nil {
				all = append(all, fault)
			}
			for _, fault := range validateAcceptedComponents(svc, map[string]*registry.Component{"local-a": component}) {
				all = append(all, &fault)
			}
			return all
		}
		for until, refused := range map[string]bool{"2099-06-01": false, "2099-05-31": true, "2099-6-1": true, "": true, "tomorrow": true, "2099-12-31": false} {
			got := faults(accept(until))
			if (refused && len(got) != 2) || (!refused && len(got) != 0) {
				t.Fatalf("the acceptance until %q gave the faults %v, want refused %v by both routes", until, got, refused)
			}
			for _, fault := range got {
				if fault.Path != "experimental_acceptances" || !strings.Contains(fault.Message, "has run out or is not a date") {
					t.Fatalf("the acceptance until %q was refused with %v", until, fault)
				}
			}
		}
		// From the start of the next day in UTC, the acceptance of the day before has run out.
		atClock(t, "2099-06-02T00:00:00Z")
		if got := faults(accept("2099-06-01")); len(got) != 2 {
			t.Fatalf("the acceptance of the day before passed: %v", got)
		}
	})
}

func TestAcceptanceNotLaterThanSunset(t *testing.T) {
	t.Run("ST-DECL-INV15-T An acceptance dated later than its component's sunset is refused", func(t *testing.T) {
		atClock(t, "2099-01-01T12:00:00Z")
		component := betaComponent("2099-12-31")
		for until, refused := range map[string]bool{"2100-01-01": true, "2099-12-31": false, "2099-12-30": false} {
			svc := &types.ServiceDeclaration{ExperimentalAcceptances: []types.ExperimentalAcceptance{{Component: "local-a", AcceptedUntil: until, Reason: "pilot"}}}
			// The rule holds for a component that a setting selects and for one that an override brings.
			for name, fault := range map[string]*ValidationError{
				"selected": acceptanceFault(svc, "local-a", component, "tenancy", "the tenancy setting selects the component local-a"),
				"overridden": func() *ValidationError {
					f := validateAcceptedComponents(svc, map[string]*registry.Component{"local-a": component})
					if len(f) == 0 {
						return nil
					}
					return &f[0]
				}(),
			} {
				if refused != (fault != nil) {
					t.Fatalf("%s, the acceptance until %s: fault %v, want refused %v", name, until, fault, refused)
				}
				if refused && (!strings.Contains(fault.Message, "local-a") || !strings.Contains(fault.Message, until) || !strings.Contains(fault.Message, "2099-12-31")) {
					t.Fatalf("%s: the fault does not name the component, the acceptance date and the sunset: %v", name, fault)
				}
			}
		}
	})
}
