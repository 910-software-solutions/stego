package registry

import (
	"strings"
	"testing"
)

// apiA requires two ports and offers two slots, so a check that reads only the first port or the first slot fails
// the test; storeA provides a port, so a check that reads only the required ports fails it.
func apiA() *Component {
	return &Component{Name: "api-a", Requires: []Port{{Name: "store-port"}, {Name: "auth-port"}},
		Slots: []SlotDefinition{
			{Name: "before_create", Phase: "before_store", Operations: []string{"create"}},
			{Name: "after_create", Phase: "after_store", Operations: []string{"create", "update"}},
		},
		Architecture: &ArchitectureRole{Layer: "handler", Flow: "Decodes the request and calls the command.",
			Ports: map[string]string{"store-port": "outbound", "auth-port": "outbound"}}}
}

func storeA() *Component {
	return &Component{Name: "store-a", Provides: []Port{{Name: "store-port"}},
		Architecture: &ArchitectureRole{Layer: "repository", Flow: "Writes the row.", Ports: map[string]string{"store-port": "inbound"}}}
}

func TestStatesPlace(t *testing.T) {
	t.Run("ST-SUP-SP05-T A component states its place only with one layer, a flow part, a side for every port and a phase and operations for every slot", func(t *testing.T) {
		for _, full := range []*Component{apiA(), storeA()} {
			if fault := statesPlace(full); fault != nil {
				t.Fatalf("the full role of %s is refused: %v", full.Name, fault)
			}
		}
		cases := []struct {
			name      string
			change    func(*Component)
			base      func() *Component
			statement string
		}{
			{"no role at all", func(c *Component) { c.Architecture = nil }, apiA, "architecture"},
			{"no layer", func(c *Component) { c.Architecture.Layer = "" }, apiA, "architecture.layer"},
			{"two layers", func(c *Component) { c.Architecture.Layer = "edge, handler" }, apiA, "architecture.layer"},
			{"the layer outbound, which is now outside call", func(c *Component) { c.Architecture.Layer = "outbound" }, apiA, "architecture.layer"},
			{"no flow part", func(c *Component) { c.Architecture.Flow = "" }, apiA, "architecture.flow"},
			{"a flow part of 101 characters", func(c *Component) { c.Architecture.Flow = strings.Repeat("a", 101) }, apiA, "architecture.flow"},
			{"a flow part on two lines", func(c *Component) { c.Architecture.Flow = "Decodes.\nCalls." }, apiA, "architecture.flow"},
			{"no side for the second required port", func(c *Component) { delete(c.Architecture.Ports, "auth-port") }, apiA, "architecture.ports.auth-port"},
			{"a side that is not inbound or outbound", func(c *Component) { c.Architecture.Ports["auth-port"] = "left" }, apiA, "architecture.ports.auth-port"},
			{"a side for a port it has not", func(c *Component) { c.Architecture.Ports["other-port"] = "inbound" }, apiA, "architecture.ports.other-port"},
			{"no phase for the second slot", func(c *Component) { c.Slots[1].Phase = "" }, apiA, "slots[after_create].phase"},
			{"a phase that is not before_store or after_store", func(c *Component) { c.Slots[1].Phase = "during" }, apiA, "slots[after_create].phase"},
			{"no operations for the second slot", func(c *Component) { c.Slots[1].Operations = nil }, apiA, "slots[after_create].operations"},
			{"an operation that is not a collection operation", func(c *Component) { c.Slots[1].Operations = []string{"create", "merge"} }, apiA, "slots[after_create].operations"},
			{"an operation twice", func(c *Component) { c.Slots[1].Operations = []string{"create", "create"} }, apiA, "slots[after_create].operations"},
			{"no side for its provided port", func(c *Component) { delete(c.Architecture.Ports, "store-port") }, storeA, "architecture.ports.store-port"},
			// The kinds of operation a component serves and the store of a repository (ST-GEN-Q44 (A)).
			{"a kind of operation that is not served", func(c *Component) { c.Architecture.Serves = []string{"collection operations", "events"} }, apiA, "architecture.serves"},
			{"a kind of operation twice", func(c *Component) { c.Architecture.Serves = []string{"commands", "commands"} }, apiA, "architecture.serves"},
			{"a store on a handler", func(c *Component) { c.Architecture.Store = "PostgreSQL" }, apiA, "architecture.store"},
			{"serves on a repository", func(c *Component) { c.Architecture.Serves = []string{"queries"} }, storeA, "architecture.serves"},
			{"a store of 41 characters", func(c *Component) { c.Architecture.Store = strings.Repeat("a", 41) }, storeA, "architecture.store"},
			{"a store on two lines", func(c *Component) { c.Architecture.Store = "Postgre\nSQL" }, storeA, "architecture.store"},
			{"a store with a control character", func(c *Component) { c.Architecture.Store = "Postgre\x1bSQL" }, storeA, "architecture.store"},
		}
		served, stored := apiA(), storeA()
		served.Architecture.Serves = []string{"collection operations", "commands", "queries", "workers"}
		stored.Architecture.Store = "PostgreSQL"
		for _, full := range []*Component{served, stored} {
			if fault := statesPlace(full); fault != nil {
				t.Fatalf("the role of %s with serves and store is refused: %v", full.Name, fault)
			}
		}
		for _, tc := range cases {
			component := tc.base()
			tc.change(component)
			fault := statesPlace(component)
			if fault == nil {
				t.Fatalf("%s: %s is accepted", tc.name, component.Name)
			}
			if fault.Statement != tc.statement || !strings.Contains(fault.Error(), "component "+component.Name) || !strings.Contains(fault.Error(), tc.statement) {
				t.Fatalf("%s: the refusal names %q, want the component %s and the statement %s: %v", tc.name, fault.Statement, component.Name, tc.statement, fault)
			}
		}
	})
}

func TestParseRefusesUnplacedParts(t *testing.T) {
	t.Run("ST-SUP-UC01-R05-T03 The registry decoder refuses a component or a mixin slot that does not state its place, naming it and the statement", func(t *testing.T) {
		full := "kind: component\nenvironment: []\nname: api-a\nversion: 1.0.0\nstage: ga\nrequires: [store-port]\nprovides: []\n" +
			"slots:\n  - name: before_create\n    proto: x.BeforeCreate\n    phase: before_store\n    operations: [create]\n" +
			"architecture:\n  layer: handler\n  flow: Decodes the request.\n  ports:\n    store-port: outbound\n"
		component, err := ParseComponentFromBytes([]byte(full), "components/api-a/component.yaml")
		if err != nil {
			t.Fatalf("the full api-a is refused: %v", err)
		}
		if component.Architecture.Layer != "handler" || component.Architecture.Ports["store-port"] != "outbound" || component.Slots[0].Phase != "before_store" || component.Slots[0].Operations[0] != "create" {
			t.Fatalf("the decoder lost a statement: %+v %+v", component.Architecture, component.Slots)
		}
		for _, tc := range []struct{ name, from, to, want string }{
			{"no layer", "  layer: handler\n", "", "architecture.layer"},
			{"no flow part", "  flow: Decodes the request.\n", "", "architecture.flow"},
			{"no side of store-port", "    store-port: outbound\n", "", "architecture.ports.store-port"},
			{"the layer middleware", "layer: handler", "layer: middleware", "architecture.layer"},
			{"no phase", "    phase: before_store\n", "", "slots[before_create].phase"},
			{"no operations", "    operations: [create]\n", "", "slots[before_create].operations"},
		} {
			_, err := ParseComponentFromBytes([]byte(strings.Replace(full, tc.from, tc.to, 1)), "components/api-a/component.yaml")
			if err == nil || !strings.Contains(err.Error(), "component api-a") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: got %v, want a refusal that names api-a and %s", tc.name, err, tc.want)
			}
		}
		mixin := "kind: mixin\nname: extra-mixin\nversion: 1.0.0\nadds_components: []\nadds_slots:\n  - name: on_change\n    proto: x.OnChange\n    operations: [create]\noverrides: none\n"
		if _, err := ParseMixinFromBytes([]byte(mixin), "mixins/extra-mixin/mixin.yaml"); err == nil || !strings.Contains(err.Error(), "mixin extra-mixin") || !strings.Contains(err.Error(), "slots[on_change].phase") {
			t.Fatalf("a mixin slot with no phase: got %v", err)
		}
	})
}

func TestShippedRegistryStatesPlace(t *testing.T) {
	t.Run("ST-SUP-UC01-R05-T04 Every component and mixin of the shipped registry states its place", func(t *testing.T) {
		reg, err := Load("../../registry")
		if err != nil {
			t.Fatalf("the shipped registry does not load: %v", err)
		}
		components := reg.Components()
		if len(components) < 30 {
			t.Fatalf("the shipped registry holds %d components, want each shipped component", len(components))
		}
		for _, component := range components {
			if fault := statesPlace(component); fault != nil {
				t.Fatalf("shipped %v", fault)
			}
		}
	})
}
