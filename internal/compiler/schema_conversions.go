package compiler

import (
	"fmt"
	"slices"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

// databaseConversions gives the conversions that the database component among components declares, by name, and
// whether one of them declares conversions: the component that provides the storage-adapter port.
func databaseConversions(components map[string]*registry.Component) (map[string]registry.Conversion, bool) {
	for _, name := range sortedKeys(components) {
		component := components[name]
		if !slices.ContainsFunc(component.Provides, func(p registry.Port) bool { return p.Name == "storage-adapter" }) {
			continue
		}
		out := map[string]registry.Conversion{}
		for _, c := range component.Conversions {
			out[c.Name] = c
		}
		return out, true
	}
	return nil, false
}

// conversionTotal tells whether the conversion is total over the constraints of the field: it is declared total, or
// one of its limits holds the field, by a pattern that is the field's pattern byte for byte, or by bounds that the
// field's min and max lie within.
func conversionTotal(c registry.Conversion, field types.Field) bool {
	if c.Total {
		return true
	}
	for _, limit := range c.TotalWhen {
		if limit.Pattern != "" && limit.Pattern != field.Pattern {
			continue
		}
		if limit.Min != nil && (field.Min == nil || *field.Min < *limit.Min) {
			continue
		}
		if limit.Max != nil && (field.Max == nil || *field.Max > *limit.Max) {
			continue
		}
		if limit.Pattern == "" && limit.Min == nil && limit.Max == nil {
			continue
		}
		return true
	}
	return false
}

// validateSchemaConversions checks the conversions of each retype against the database component (ST-DECL-INV62,
// ST-DECL-INV63): the conversion and the inverse conversion are ones that it declares from From to the field's type
// and back; the conversion is total over the constraints of the field before the change, or the change declares a
// fill value; and the inverse conversion is total over the constraints of the field after the change.
func validateSchemaConversions(service *types.ServiceDeclaration, components map[string]*registry.Component) []ValidationError {
	conversions, declared := databaseConversions(components)
	var faults []ValidationError
	for i, change := range service.SchemaChanges {
		if change.Kind != "retype" || change.Conversion == "" || change.InverseConversion == "" {
			continue
		}
		path := fmt.Sprintf("schema_changes[%d]", i)
		fault := func(rule, where, found, message string) {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: rule, Path: path + where, Found: found, Message: fmt.Sprintf("schema change %q: %s", change.ID, message)})
		}
		entity := slices.IndexFunc(service.Entities, func(e types.Entity) bool { return e.Name == change.Entity })
		if entity < 0 {
			continue
		}
		at := slices.IndexFunc(service.Entities[entity].Fields, func(f types.Field) bool { return f.Name == change.Field })
		if at < 0 {
			continue
		}
		after, before := service.Entities[entity].Fields[at], gen.RetypeField(change)
		if !declared {
			fault("schema-change-value", ".conversion", change.Conversion, "no database component of the service declares conversions, so the retype has none to use")
			continue
		}
		forward, okForward := conversions[change.Conversion]
		inverse, okInverse := conversions[change.InverseConversion]
		switch {
		case !okForward || forward.From != before.Type || forward.To != after.Type:
			fault("schema-change-value", ".conversion", change.Conversion, fmt.Sprintf("the conversion %s is not one that the database component declares from %s to %s", change.Conversion, before.Type, after.Type))
		case !conversionTotal(forward, before) && change.FillValue == nil:
			fault("schema-change-total", ".conversion", change.Conversion, fmt.Sprintf("the conversion %s is not total over the field %s.%s before the change, so a value can fail it; declare a fill value, or give the field before the change, in its Definition, a constraint under which the conversion is total", change.Conversion, change.Entity, change.Field))
		}
		switch {
		case !okInverse || inverse.From != after.Type || inverse.To != before.Type:
			fault("schema-change-value", ".inverse_conversion", change.InverseConversion, fmt.Sprintf("the inverse conversion %s is not one that the database component declares from %s to %s", change.InverseConversion, after.Type, before.Type))
		case !conversionTotal(inverse, after):
			fault("schema-change-total", ".inverse_conversion", change.InverseConversion, fmt.Sprintf("the inverse conversion %s is not total over the field %s.%s after the change, so a write of the release of the change could fail it; give the field a constraint under which it is total", change.InverseConversion, change.Entity, change.Field))
		}
	}
	return faults
}
