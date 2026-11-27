package compiler

import (
	"fmt"
	"slices"

	"github.com/910-software-solutions/stego/internal/types"
)

// EventFieldsRule is the rule that a fault of the base check names when the head breaks the event fields that the
// consumers of the change events of the base read (ST-GEN-INV39, decision ST-SUP-Q184 (c)).
const EventFieldsRule = "ST-GEN-INV39"

// eventFieldFaults refuses a head that breaks the event fields of an entity of the base at the same version
// (ST-GEN-INV39): no Events, a lower version, or at the same version a field of the base list that the head leaves
// out, gives another type, or gives an enum with a value of the base removed. A field that the head adds, and each
// change at a higher version, is accepted. An entity that the head does not declare is held by the rules of its drop.
func eventFieldFaults(base, head *types.ServiceDeclaration) []BaseFault {
	heads := map[string]types.Entity{}
	for _, e := range head.Entities {
		heads[e.Name] = e
	}
	var faults []BaseFault
	for _, b := range base.Entities {
		if b.Events == nil {
			continue
		}
		h, ok := heads[b.Name]
		if !ok {
			continue
		}
		version := b.Events.EventVersion()
		fault := func(field, why string) {
			faults = append(faults, BaseFault{Rule: EventFieldsRule, Entity: b.Name, Field: field,
				Message: fmt.Sprintf("the change events of %s carry the event fields of version %d in the base, which a consumer reads as a contract, and %s; keep the field with its type, or raise events.version to %d, which tells each consumer of a breaking change (ST-GEN-INV39)", b.Name, version, why, version+1)})
		}
		if h.Events == nil {
			fault("", "the head declares no events")
			continue
		}
		switch hv := h.Events.EventVersion(); {
		case hv > version:
			continue
		case hv < version:
			fault("", fmt.Sprintf("the head gives the lower version %d", hv))
			continue
		}
		baseFields, headFields := fieldsByName(b), fieldsByName(h)
		for _, name := range b.Events.Fields {
			bf := baseFields[name]
			hf, declared := headFields[name]
			switch {
			case !slices.Contains(h.Events.Fields, name) || !declared:
				fault(name, fmt.Sprintf("the head leaves out the field %s", name))
			case hf.Type != bf.Type:
				fault(name, fmt.Sprintf("the head gives the field %s the type %s in place of %s", name, hf.Type, bf.Type))
			case bf.Type == types.FieldTypeEnum && slices.ContainsFunc(bf.Values, func(v string) bool { return !slices.Contains(hf.Values, v) }):
				fault(name, fmt.Sprintf("the head removes a value of the enum field %s", name))
			}
		}
	}
	return faults
}

// fieldsByName gives the declared fields of an entity by name.
func fieldsByName(e types.Entity) map[string]types.Field {
	out := make(map[string]types.Field, len(e.Fields))
	for _, f := range e.Fields {
		out[f.Name] = f
	}
	return out
}
