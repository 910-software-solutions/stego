package declaration

import (
	"fmt"
	"slices"

	"github.com/jsell-rh/stego/internal/types"
)

// validateSecretFields enforces ST-DECL-INV101 (decision ST-DECL-Q44): a field of sensitivity secret is of type
// string or jsonb (a compare-only one a string), and is not unique, in a unique_composite, in a declared invariant of kind unique, in an upsert key, the
// within field of a reference, the subject field of a relationship, computed, or given a default; compare_only is only
// on a field of sensitivity secret. A key or an index on a secret needs a deterministic form, which tells which rows
// hold the same value, and a default puts a secret in clear in the declaration. Each fault names the field and never a
// value.
func validateSecretFields(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	fault := func(path, field, entity, why string) {
		faults = append(faults, ValidationError{Category: "field-type", Rule: "secret-field", Path: path,
			Message: fmt.Sprintf("the secret field %q of the entity %q %s (ST-DECL-INV101)", field, entity, why)})
	}
	for i, entity := range service.Entities {
		within := map[string]bool{}
		for _, ref := range entity.References {
			if w := ref.WithinField(); w != "" {
				within[w] = true
			}
		}
		subjects := map[string]bool{}
		for _, r := range entity.Relationships {
			if r.SubjectSource == types.SubjectSourceField {
				subjects[r.SubjectField] = true
			}
		}
		for j, field := range entity.Fields {
			path := fieldPath(i, j)
			if field.CompareOnly && !field.Secret() {
				faults = append(faults, ValidationError{Category: "field-type", Rule: "secret-field", Path: path + ".compare_only",
					Message: fmt.Sprintf("the field %q of the entity %q states compare_only, which is only for a field of sensitivity secret (ST-DECL-INV101)", field.Name, entity.Name)})
			}
			if !field.Secret() {
				continue
			}
			switch {
			case field.Type != types.FieldTypeString && field.Type != types.FieldTypeJsonb:
				fault(path+".type", field.Name, entity.Name, fmt.Sprintf("has the type %s; a secret field is a string or a jsonb", field.Type))
			case field.CompareOnly && field.Type != types.FieldTypeString:
				fault(path+".compare_only", field.Name, entity.Name, "is compare_only and not a string; a compare-only secret is a string")
			case field.Unique || field.UniqueWhenLive:
				fault(path+".unique", field.Name, entity.Name, "is unique; a unique key on a secret tells which rows hold the same value")
			case len(field.UniqueComposite) > 0:
				fault(path+".unique_composite", field.Name, entity.Name, "has a unique_composite; a unique key on a secret tells which rows hold the same value")
			case field.Computed:
				fault(path+".computed", field.Name, entity.Name, "is computed; a secret is given by a write")
			case field.Default != nil:
				fault(path+".default", field.Name, entity.Name, "has a default, which puts a secret in clear in the declaration")
			case within[field.Name]:
				fault(path, field.Name, entity.Name, "holds a reference within it; a key on a secret tells which rows hold the same value")
			case subjects[field.Name]:
				fault(path, field.Name, entity.Name, "is the subject of a relationship, which SpiceDB stores in clear")
			}
			for _, other := range entity.Fields {
				if slices.Contains(other.UniqueComposite, field.Name) {
					fault(path, field.Name, entity.Name, fmt.Sprintf("is in the unique_composite of the field %q; a unique key on a secret tells which rows hold the same value", other.Name))
				}
			}
		}
	}
	for k, inv := range service.Invariants {
		if inv.Kind != "unique" {
			continue
		}
		for _, name := range inv.By {
			if f, ok := secretField(service, inv.On, name); ok {
				faults = append(faults, ValidationError{Category: "field-type", Rule: "secret-field", Path: fmt.Sprintf("invariants[%d].by", k),
					Message: fmt.Sprintf("the invariant %q of kind unique is by the secret field %q of the entity %q; a unique key on a secret tells which rows hold the same value (ST-DECL-INV101)", inv.Name, f.Name, inv.On)})
			}
		}
	}
	for _, c := range service.Collections {
		for _, key := range c.UpsertKey {
			if f, ok := secretField(service, c.Entity, key); ok {
				faults = append(faults, ValidationError{Category: "field-type", Rule: "secret-field", Path: "collections." + c.Name + ".upsert_key",
					Message: fmt.Sprintf("the upsert key of the collection %q holds the secret field %q of the entity %q; a key on a secret tells which rows hold the same value (ST-DECL-INV101)", c.Name, f.Name, c.Entity)})
			}
		}
	}
	return faults
}

// secretField gives the field name of the entity when it is of the sensitivity secret.
func secretField(service *types.ServiceDeclaration, entity, name string) (types.Field, bool) {
	for _, e := range service.Entities {
		if e.Name != entity {
			continue
		}
		for _, f := range e.Fields {
			if f.Name == name && f.Secret() {
				return f, true
			}
		}
	}
	return types.Field{}, false
}
