package compiler

import (
	"fmt"

	"github.com/910-software-solutions/stego/internal/types"
)

// ResourceTypeRule is the rule that a fault of the base check names when the head gives a resource type of the base
// to another entity, or changes the resource type of an entity of the base (ST-GEN-INV41).
const ResourceTypeRule = "ST-GEN-INV41"

// resourceTypeFaults refuses a head that gives an entity a resource type that another entity of the base has, also an
// entity of the base with dropped_by, and a head that changes or removes the resource type of an entity of the base
// (ST-GEN-INV41, security review of PR #21, M1). The SpiceDB object of a row is its resource type and its
// tenant-qualified ID, with no part that names the entity. The relationships of the rows of the base stay on the
// objects of the base type after a release, because no schema step deletes them. Thus a row of another entity that
// takes the type, created with the ID of such a row in the same tenant, would take its relationships and grants, and
// an entity that leaves the type leaves its relationships there for the next entity that takes it.
func resourceTypeFaults(base, head *types.ServiceDeclaration) []BaseFault {
	owners := map[string]string{}
	before := map[string]types.Entity{}
	for _, e := range base.AllEntities() {
		before[e.Name] = e
		if e.ResourceType != "" {
			if _, taken := owners[e.ResourceType]; !taken {
				owners[e.ResourceType] = e.Name
			}
		}
	}
	var faults []BaseFault
	for _, e := range head.AllEntities() {
		if prior, ok := before[e.Name]; ok && prior.ResourceType != "" && prior.ResourceType != e.ResourceType {
			now := fmt.Sprintf("the resource type %q", e.ResourceType)
			if e.ResourceType == "" {
				now = "no resource type"
			}
			faults = append(faults, BaseFault{Rule: ResourceTypeRule, Entity: e.Name, Message: fmt.Sprintf(
				"the head gives %s %s, and the base gives it the resource type %q: the relationships of the rows that the base made stay on the objects %s:<row ID>, because no schema step deletes them, and an entity that later takes %q, or a row of an entity that has it, with the ID of such a row in the same tenant takes them and their grants. Keep the resource type %q (ST-GEN-INV41)",
				e.Name, now, prior.ResourceType, prior.ResourceType, prior.ResourceType, prior.ResourceType)})
			continue
		}
		if prior, ok := before[e.Name]; e.ResourceType == "" || ok && prior.ResourceType == e.ResourceType {
			// An entity that keeps its type of the base: a type that two entities of the base share is refused by
			// the declaration check (ST-DECL-INV107).
			continue
		}
		if owner, taken := owners[e.ResourceType]; taken && owner != e.Name {
			faults = append(faults, BaseFault{Rule: ResourceTypeRule, Entity: e.Name, Message: fmt.Sprintf(
				"the head gives %s the resource type %q, which the entity %s has in the base: the relationships of the rows of %s that the base made stay on the objects %s:<row ID>, so a row of %s with the ID of such a row in the same tenant would take them and their grants. Give %s a resource type that no entity of the base has (ST-GEN-INV41)",
				e.Name, e.ResourceType, owner, owner, e.ResourceType, e.Name, e.Name)})
		}
	}
	return faults
}
