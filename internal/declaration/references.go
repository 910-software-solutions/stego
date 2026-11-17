package declaration

import (
	"fmt"
	"slices"

	"github.com/jsell-rh/stego/internal/types"
)

// validateReferences checks the References of each entity when the service binds authorization (ST-DECL-INV85):
// each field that refers to an entity with a resource type names a relation that the schema declares on the
// entity's own resource type for the referenced type, and a permission that the schema declares on the referenced
// resource type. A reference that leaves either out, names one that the schema does not declare, or is given for a
// field that is no such reference is refused, naming the field.
func validateReferences(entities []types.Entity, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	byName := map[string]*types.Entity{}
	for i := range entities {
		byName[entities[i].Name] = &entities[i]
	}
	for ei, e := range entities {
		path := fmt.Sprintf("entities[%d]", ei)
		references := map[string]bool{}
		for _, f := range e.Fields {
			to := byName[f.To]
			if f.Type != types.FieldTypeRef || to == nil || to.ResourceType == "" {
				continue
			}
			references[f.Name] = true
			fault := func(key, found, message string, allowed []string) {
				faults = append(faults, ValidationError{Category: "authorization", Rule: "entity-references",
					Path: path + ".references." + f.Name + key, Found: found, Allowed: allowed,
					Message: fmt.Sprintf("entity %q field %q refers to %s, which has a resource type: %s", e.Name, f.Name, to.Name, message)})
			}
			ref, ok := e.References[f.Name]
			switch {
			case !ok:
				fault("", f.Name, "name its relation and its permission in references", nil)
				continue
			case ref.Relation == "":
				fault(".relation", f.Name, "name the relation that the runtime writes to the referenced row", nil)
			case schema != nil && !schema.relationAdmits(e.ResourceType, ref.Relation, to.ResourceType):
				fault(".relation", ref.Relation, fmt.Sprintf("the schema %s declares no relation %q on %q for %q", schema.Path, ref.Relation, e.ResourceType, to.ResourceType), nil)
			}
			switch {
			case ref.Permission == "":
				fault(".permission", f.Name, "name the permission that a create and a move decide on the referenced row", nil)
			case schema != nil && !schema.declaresPermission(to.ResourceType, ref.Permission):
				fault(".permission", ref.Permission, fmt.Sprintf("the schema %s declares no permission %q on %q", schema.Path, ref.Permission, to.ResourceType), schema.permissionsOf(to.ResourceType))
			}
		}
		for _, name := range sortedKeys(e.References) {
			if !references[name] {
				faults = append(faults, ValidationError{Category: "authorization", Rule: "entity-references", Path: path + ".references." + name, Found: name,
					Allowed: sortedKeys(references), Message: fmt.Sprintf("entity %q gives references for %q, which is no field that refers to an entity with a resource type", e.Name, name)})
			}
		}
	}
	return faults
}

// relationAdmits tells whether the definition resourceType declares relation with the subject type subjectType.
func (s *AuthzSchema) relationAdmits(resourceType, relation, subjectType string) bool {
	definition, ok := s.definitions[resourceType]
	return ok && slices.Contains(definition.relations[relation], subjectType)
}

// declaresPermission tells whether the definition resourceType declares the permission.
func (s *AuthzSchema) declaresPermission(resourceType, permission string) bool {
	definition, ok := s.definitions[resourceType]
	return ok && definition.permissions[permission]
}

// permissionsOf gives the permission names of the definition resourceType.
func (s *AuthzSchema) permissionsOf(resourceType string) []string {
	return sortedKeys(s.definitions[resourceType].permissions)
}

// validateWithin checks the within of each reference (ST-DECL-INV93, ST-DECL-Q42): within is none, or a field that
// the entity and the referenced entity both declare, each a reference to the same entity that is required or is the
// scope field of a collection over its entity, and not the reference itself. Any other within is refused, naming the
// entity, the reference and the field.
func validateWithin(entities []types.Entity, collections []types.Collection) []ValidationError {
	var faults []ValidationError
	byName := map[string]*types.Entity{}
	for i := range entities {
		byName[entities[i].Name] = &entities[i]
	}
	for ei, e := range entities {
		for _, name := range sortedKeys(e.References) {
			ref := e.References[name]
			if ref.Within == "" || ref.Within == types.WithinNone {
				continue
			}
			fault := func(message string) {
				faults = append(faults, ValidationError{Category: "authorization", Rule: "reference-within",
					Path: fmt.Sprintf("entities[%d].references.%s.within", ei, name), Found: ref.Within,
					Message: fmt.Sprintf("entity %q reference %q is held within %q, but %s; name a field that both rows declare as a required reference to the same entity, or within: none", e.Name, name, ref.Within, message)})
			}
			reference, ok := findField(e, name)
			to := byName[reference.To]
			switch {
			case ref.Within == name:
				fault("a reference cannot be held within itself")
				continue
			case !ok || reference.Type != types.FieldTypeRef || to == nil:
				// validateReferences refuses References of a field that is no reference.
				continue
			}
			own, ok := withinField(e, ref.Within, collections)
			if !ok {
				fault(fmt.Sprintf("entity %q declares no required reference %q", e.Name, ref.Within))
				continue
			}
			theirs, ok := withinField(*to, ref.Within, collections)
			if !ok {
				fault(fmt.Sprintf("the referenced entity %q declares no required reference %q", to.Name, ref.Within))
				continue
			}
			if own.To != theirs.To {
				fault(fmt.Sprintf("%q refers to %s on %q and to %s on %q", ref.Within, own.To, e.Name, theirs.To, to.Name))
			}
		}
	}
	return faults
}

// withinField gives the field name of the entity e when it is a reference that is required, or that is the scope
// field of a collection over e, which a row of the collection always holds (ST-DECL-INV93).
func withinField(e types.Entity, name string, collections []types.Collection) (types.Field, bool) {
	f, ok := findField(e, name)
	if !ok || f.Type != types.FieldTypeRef || f.To == "" {
		return types.Field{}, false
	}
	if !f.Optional {
		return f, true
	}
	for _, c := range collections {
		if c.Entity == e.Name && c.ScopeField() == name {
			return f, true
		}
	}
	return types.Field{}, false
}

// WithinWarnings gives one warning for each reference with no within when the entity and the referenced entity both
// declare a reference to the same third entity (ST-DECL-INV93): such a reference may join the rows of two
// ancestors. The warning names the entity, the reference and the shared field, and tells how to declare within or
// within: none. It is a warning, not a fault, because a link across an ancestor can be intended.
func WithinWarnings(service *types.ServiceDeclaration) []ValidationError {
	if service == nil {
		return nil
	}
	var warnings []ValidationError
	byName := map[string]*types.Entity{}
	for i := range service.Entities {
		byName[service.Entities[i].Name] = &service.Entities[i]
	}
	for ei, e := range service.Entities {
		for _, name := range sortedKeys(e.References) {
			if e.References[name].Within != "" {
				continue
			}
			reference, ok := findField(e, name)
			to := byName[reference.To]
			if !ok || reference.Type != types.FieldTypeRef || to == nil {
				continue
			}
			for _, f := range e.Fields {
				if f.Name == name || f.Type != types.FieldTypeRef || f.To == "" || f.To == to.Name {
					continue
				}
				if !slices.ContainsFunc(to.Fields, func(g types.Field) bool { return g.Type == types.FieldTypeRef && g.To == f.To }) {
					continue
				}
				warnings = append(warnings, ValidationError{Category: "authorization", Rule: "reference-within",
					Path: fmt.Sprintf("entities[%d].references.%s", ei, name), Found: name, Allowed: []string{f.Name, types.WithinNone},
					Message: fmt.Sprintf("entity %q reference %q refers to %s, and both %s and %s refer to %s; a row may refer to a row under another %s. Add within: %s to the reference to hold both rows under the same %s, or within: none when the reference crosses it on purpose", e.Name, name, to.Name, e.Name, to.Name, f.To, f.To, f.Name, f.To)})
				break
			}
		}
	}
	return warnings
}
