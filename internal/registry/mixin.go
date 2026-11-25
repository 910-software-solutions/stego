package registry

import (
	"fmt"
	"slices"
	"strings"
)

// CheckExtension is the rule of ST-SUP-INV04: a mixin only adds components and slots to the archetype it extends,
// and replaces no part of it. It refuses a mixin that states overrides other than none, that adds a slot whose name
// a component of the archetype already offers, or that adds a component the archetype does not hold which provides
// a port that a component of the archetype provides, or that the archetype binds. Each fault names the mixin, the
// part it replaces and the archetype. A component that the archetype already holds adds nothing, so a mixin may
// name it. The decoder already refuses a mixin that states a field a mixin does not have, such as bindings or
// conventions.
func (r *Registry) CheckExtension(archetype *Archetype, mixin *Mixin) error {
	if archetype == nil || mixin == nil {
		return nil
	}
	var faults []string
	if mixin.Overrides != "" && mixin.Overrides != "none" {
		faults = append(faults, fmt.Sprintf("it states overrides %q; a mixin overrides none", mixin.Overrides))
	}
	held := slices.Clone(archetype.Components)
	if archetype.DefaultAuth != "" && !slices.Contains(held, archetype.DefaultAuth) {
		held = append(held, archetype.DefaultAuth)
	}
	slotOwner := map[string]string{}
	portOwner := map[string]string{}
	for _, name := range held {
		component := r.components[name]
		if component == nil {
			continue
		}
		for _, slot := range component.Slots {
			slotOwner[slot.Name] = name
		}
		for _, port := range component.Provides {
			portOwner[port.Name] = name
		}
	}
	for port, bound := range archetype.Bindings {
		if _, owned := portOwner[port]; !owned {
			portOwner[port] = bound
		}
	}
	for _, slot := range mixin.AddsSlots {
		if owner, offered := slotOwner[slot.Name]; offered {
			faults = append(faults, fmt.Sprintf("it adds the slot %s, which the component %s of the archetype offers", slot.Name, owner))
		}
	}
	for _, name := range mixin.AddsComponents {
		component := r.components[name]
		if component == nil || slices.Contains(held, name) {
			continue
		}
		for _, port := range component.Provides {
			if owner, owned := portOwner[port.Name]; owned {
				faults = append(faults, fmt.Sprintf("it adds the component %s, which provides the port %s that %s answers in the archetype", name, port.Name, owner))
			}
		}
	}
	if len(faults) > 0 {
		return fmt.Errorf("mixin %s replaces a part of the archetype %s, and a mixin only adds: %s", mixin.Name, archetype.Name, strings.Join(faults, "; "))
	}
	return nil
}

// RequiredMixins is the rule of ST-SUP-INV16: each mixin that a declaration binds has every mixin it requires bound
// too. It refuses the first bound mixin whose required mixin is not bound, naming both. The bound mixins are the
// mixins after each alias is replaced by the mixins it names; a name the registry does not hold is refused by the
// caller.
func (r *Registry) RequiredMixins(bound []string) error {
	for _, name := range bound {
		mixin := r.mixins[name]
		if mixin == nil {
			continue
		}
		for _, required := range mixin.RequiresMixins {
			if !slices.Contains(bound, required) {
				return fmt.Errorf("mixin %q requires the mixin %q, which the declaration does not bind", name, required)
			}
		}
	}
	return nil
}
