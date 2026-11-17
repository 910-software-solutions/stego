package registry

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The architecture role of a component and the phase and operations of each slot (ST-SUP-SP05, ST-SUP-UC01-R05,
// ST-SUP-Q130 (A), ST-SUP-Q134 (A)): the registry refuses a part that leaves one out, so the architecture model
// places every element from the registry alone and never guesses.

// Layers are the layers of a service in the order of a request flow (ST-SUP-EN09 field Architecture role).
var Layers = []string{"edge", "authentication", "handler", "slot", "repository", "store", "outside call"}

// Sides are the sides of a port: inbound, the flow comes into the component through the port; outbound, the
// component calls on through the port.
var Sides = []string{"inbound", "outbound"}

// Phases are the phases of a slot, relative to the write to the store (ST-SUP-EN08 field Phase).
var Phases = []string{"before_store", "after_store"}

// SlotOperations are the collection operations a slot may state that it is called by.
var SlotOperations = []string{"create", "read", "update", "delete", "list", "upsert", "patch"}

// MaxFlowPart is the most characters a flow part may have.
const MaxFlowPart = 100

// ServedKinds are the kinds of operation whose request flows start at the component that serves them
// (ST-SUP-EN09 field Architecture role, ST-GEN-Q44 (A)).
var ServedKinds = []string{"collection operations", "commands", "queries", "workers"}

// MaxStoreName is the most characters the store of a repository may have.
const MaxStoreName = 40

// PlaceError is a component or a mixin that does not state its place: the part, the slot when the fault is a
// slot's, the statement it leaves out or gets wrong, and why.
type PlaceError struct {
	// Part is "component <name>" or "mixin <name>".
	Part string
	// Slot is the slot at fault, or "".
	Slot string
	// Statement is the statement, such as architecture.layer or slots[before_create].phase.
	Statement string
	Reason    string
}

func (e *PlaceError) Error() string {
	return fmt.Sprintf("%s states no valid %s: %s", e.Part, e.Statement, e.Reason)
}

// statesPlace is the predicate of ST-SUP-SP05: it gives nil when the component names one layer, its part in a
// request flow, a side for each port it provides or requires and for no other port, and a phase and the operations
// of each slot it offers; else it gives the first fault, with the part that is missing or extra.
func statesPlace(component *Component) *PlaceError {
	part := "component " + component.Name
	fault := func(statement, format string, args ...any) *PlaceError {
		return &PlaceError{Part: part, Statement: statement, Reason: fmt.Sprintf(format, args...)}
	}
	role := component.Architecture
	if role == nil {
		return fault("architecture", "the component states no architecture role; give architecture.layer, architecture.flow and architecture.ports")
	}
	if !slices.Contains(Layers, role.Layer) {
		if role.Layer == "" {
			return fault("architecture.layer", "the component states no layer; give one of %s", strings.Join(Layers, ", "))
		}
		return fault("architecture.layer", "the layer %q is not one of %s", role.Layer, strings.Join(Layers, ", "))
	}
	flow := strings.TrimSpace(role.Flow)
	switch {
	case flow == "":
		return fault("architecture.flow", "the component states no part in a request flow; give one plain sentence of at most %d characters", MaxFlowPart)
	case utf8.RuneCountInString(role.Flow) > MaxFlowPart:
		return fault("architecture.flow", "the flow part has %d characters, more than %d", utf8.RuneCountInString(role.Flow), MaxFlowPart)
	case flow != role.Flow || strings.ContainsAny(role.Flow, "\n\r\t"):
		return fault("architecture.flow", "the flow part is not one plain sentence on one line")
	}
	ports := map[string]bool{}
	for _, list := range [][]Port{component.Requires, component.Provides} {
		for _, port := range list {
			ports[port.Name] = true
			side, stated := role.Ports[port.Name]
			if !stated {
				return fault("architecture.ports."+port.Name, "the component states no side for the port %s; give inbound or outbound", port.Name)
			}
			if !slices.Contains(Sides, side) {
				return fault("architecture.ports."+port.Name, "the side %q of the port %s is not inbound or outbound", side, port.Name)
			}
		}
	}
	for _, name := range sortedNames(role.Ports) {
		if !ports[name] {
			return fault("architecture.ports."+name, "the component neither provides nor requires the port %s", name)
		}
	}
	if len(role.Serves) > 0 && role.Layer != "handler" {
		return fault("architecture.serves", "only a component of the handler layer serves a kind of operation; the component is of the layer %s", role.Layer)
	}
	for i, kind := range role.Serves {
		if !slices.Contains(ServedKinds, kind) {
			return fault("architecture.serves", "the kind %q is not one of %s", kind, strings.Join(ServedKinds, ", "))
		}
		if slices.Contains(role.Serves[:i], kind) {
			return fault("architecture.serves", "the kind %s is stated twice", kind)
		}
	}
	if role.Store != "" {
		store := strings.TrimSpace(role.Store)
		switch {
		case role.Layer != "repository":
			return fault("architecture.store", "only a component of the repository layer states its store")
		case utf8.RuneCountInString(role.Store) > MaxStoreName:
			return fault("architecture.store", "the store name has %d characters, more than %d", utf8.RuneCountInString(role.Store), MaxStoreName)
		case store != role.Store || strings.ContainsAny(role.Store, "\n\r\t") || strings.ContainsFunc(role.Store, unicode.IsControl):
			return fault("architecture.store", "the store name is not plain text on one line")
		}
	}
	return slotsStatePlace(part, component.Slots)
}

// slotsStatePlace holds each slot to a phase and to the operations that call it (ST-SUP-EN08 fields Phase and
// Operations).
func slotsStatePlace(part string, slots []SlotDefinition) *PlaceError {
	for _, slot := range slots {
		fault := func(field, format string, args ...any) *PlaceError {
			return &PlaceError{Part: part, Slot: slot.Name, Statement: fmt.Sprintf("slots[%s].%s", slot.Name, field), Reason: fmt.Sprintf(format, args...)}
		}
		if !slices.Contains(Phases, slot.Phase) {
			if slot.Phase == "" {
				return fault("phase", "the slot %s states no phase; give before_store or after_store", slot.Name)
			}
			return fault("phase", "the phase %q of the slot %s is not before_store or after_store", slot.Phase, slot.Name)
		}
		if len(slot.Operations) == 0 {
			return fault("operations", "the slot %s states no operation that calls it; give one or more of %s", slot.Name, strings.Join(SlotOperations, ", "))
		}
		for i, operation := range slot.Operations {
			if !slices.Contains(SlotOperations, operation) {
				return fault("operations", "the operation %q of the slot %s is not one of %s", operation, slot.Name, strings.Join(SlotOperations, ", "))
			}
			if slices.Contains(slot.Operations[:i], operation) {
				return fault("operations", "the slot %s states the operation %s twice", slot.Name, operation)
			}
		}
	}
	return nil
}

// sortedNames gives the keys of a map in byte order.
func sortedNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// CheckPlace gives the fault of a component that does not state its place, or nil. The architecture model calls it
// again before it derives, which catches a component that came from no decoder (ST-GEN-UC08-R05).
func CheckPlace(component *Component) error {
	if fault := statesPlace(component); fault != nil {
		return fault
	}
	return nil
}

// CheckMixinPlace gives the fault of a mixin slot that states no phase or no operations, or nil.
func CheckMixinPlace(mixin *Mixin) error {
	if fault := slotsStatePlace("mixin "+mixin.Name, mixin.AddsSlots); fault != nil {
		return fault
	}
	return nil
}
