package declaration

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// conversionTotal tells whether the conversion is total over the constraints of the field: it is declared total, or
// one of its limits holds the field, by a pattern that is the field's pattern byte for byte, or by bounds that the
// field's min and max lie within.
func conversionTotal(c types.Conversion, field types.Field) bool {
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

// ValidateSchemaConversions checks the conversions of each retype against the database component (ST-DECL-INV62,
// ST-DECL-INV63): the conversion and the inverse conversion are ones that it declares from From to the type of the
// field as the change's own step leaves it, and back; the conversion is total over the constraints of the field before the change, or the change declares a
// fill value; and the inverse conversion is total over the constraints of the field after the change.
func ValidateSchemaConversions(service *types.ServiceDeclaration, conversions map[string]types.Conversion, declared bool) []ValidationError {
	var faults []ValidationError
	for i, change := range service.SchemaChanges {
		if change.Kind != "retype" || change.Conversion == "" || change.InverseConversion == "" {
			continue
		}
		path := fmt.Sprintf("schema_changes[%d]", i)
		fault := func(rule, where, found, message string) {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: rule, Path: path + where, Found: found, Message: fmt.Sprintf("schema change %q: %s", change.ID, message)})
		}
		// The field after the change is the field as its own step leaves it, so that each retype of a chain is checked
		// from its own From to the type that its step gives (decision on ST-DECL-Q17 (a), orchestrator under the
		// owner's delegation, 2026-11-14). A change whose shape is not known is refused by the check of the later
		// change that cannot be undone.
		shape := shapeAtStep(service, change)
		after, found := shape.field(change.Field)
		if shape.reason != "" || !found {
			continue
		}
		before, at := gen.RetypeField(change), shape.of(change.Entity, after)
		// In a widening between two integer types, such as int32 to int64, each side holds only values of the range
		// of the narrower type while the change is open: the column type of the field before holds the one side, and
		// the API of the release of the change holds the other (gen.HeldBounds). So the inverse conversion is checked,
		// and the bounds are compared, within that range; the messages name the declared bounds (ST-DECL-Q48;
		// walkthrough 26, P3-RT).
		heldBefore, heldAfter := gen.WithinNarrower(before, after)
		if !declared {
			fault("schema-change-value", ".conversion", change.Conversion, "no database component of the service declares conversions, so the retype has none to use")
			continue
		}
		forward, okForward := conversions[change.Conversion]
		inverse, okInverse := conversions[change.InverseConversion]
		switch {
		case !okForward || forward.From != before.Type || forward.To != after.Type:
			fault("schema-change-value", ".conversion", change.Conversion, fmt.Sprintf("the conversion %s is not one that the database component declares from %s to %s%s", change.Conversion, before.Type, after.Type, at))
		case !conversionTotal(forward, before) && change.FillValue == nil:
			fault("schema-change-total", ".conversion", change.Conversion, fmt.Sprintf("the conversion %s is not total over the field %s.%s before the change, so a value can fail it; declare a fill value, or give the field before the change, in its Definition, a constraint under which the conversion is total", change.Conversion, change.Entity, change.Field))
		}
		switch {
		case !okInverse || inverse.From != after.Type || inverse.To != before.Type:
			fault("schema-change-value", ".inverse_conversion", change.InverseConversion, fmt.Sprintf("the inverse conversion %s is not one that the database component declares from %s to %s%s", change.InverseConversion, after.Type, before.Type, at))
		case !conversionTotal(inverse, heldAfter):
			fault("schema-change-total", ".inverse_conversion", change.InverseConversion, fmt.Sprintf("the inverse conversion %s is not total over the field %s.%s after the change, so a write of the release of the change could fail it; give the field a constraint under which it is total%s", change.InverseConversion, change.Entity, change.Field, at))
		}
		if reason := checksKept(heldBefore, heldAfter); reason != "" && okForward && okInverse {
			// A retype with no Definition takes the field before the change as its From type alone, with no
			// constraint, so the author who left the Definition out is told first to give it, and not that the
			// bounds differ, which they do not in the author's view (walkthrough 7, K12; walkthrough 8 and 9, B9).
			integer := func(t types.FieldType) bool { return t == types.FieldTypeInt32 || t == types.FieldTypeInt64 }
			if strings.TrimSpace(change.Definition) == "" && integer(before.Type) && integer(after.Type) {
				fault("schema-change-total", ".definition", "", fmt.Sprintf("the retype of %s.%s gives no definition, so the field before the change is taken as %s with no min and no max, and the field after the change has %s: give the field before the change in definition, such as definition: \"%s\", so that the CHECK of each column holds the values of the other%s", change.Entity, change.Field, change.From, bounds(after), sideDefinition(change.From, after), at))
				continue
			}
			fault("schema-change-total", ".field", change.Field, fmt.Sprintf("the retype of %s.%s: %s, so a converted value of one release could break the CHECK of the column of the other, and its write would be refused%s", change.Entity, change.Field, reason, at))
		}
	}
	return faults
}

// bounds names the bounds min and max of an integer field, or says that it has none.
func bounds(field types.Field) string {
	var items []string
	if field.Min != nil {
		items = append(items, "min "+strconv.FormatFloat(*field.Min, 'f', -1, 64))
	}
	if field.Max != nil {
		items = append(items, "max "+strconv.FormatFloat(*field.Max, 'f', -1, 64))
	}
	if len(items) == 0 {
		return "no min and no max"
	}
	return strings.Join(items, " and ")
}

// sideDefinition gives an example Definition of a field of the type from with the bounds min and max of field, the
// form that a retype of two integer types needs.
func sideDefinition(from string, field types.Field) string {
	items := []string{from}
	if field.Min != nil {
		items = append(items, "min "+strconv.FormatFloat(*field.Min, 'f', -1, 64))
	}
	if field.Max != nil {
		items = append(items, "max "+strconv.FormatFloat(*field.Max, 'f', -1, 64))
	}
	return strings.Join(items, ", ")
}

// checksKept tells why a value that one side of a retype holds could break the CHECK of the column of the other
// side after its conversion, or gives empty: each side's CHECK holds every value of the other (security review of
// PR #335, M1, orchestrator under the owner's delegation, 2026-11-14). Between two integer types the bounds min and
// max of the field before and after the change must be the same, as gen.WithinNarrower gives them for a widening; between a number or a boolean and a text, neither
// field may have a min, a max, a min_length, a max_length or values, which the conversion does not keep. A pattern
// is not a CHECK of the column, so it is not compared.
func checksKept(before, after types.Field) string {
	integer := func(t types.FieldType) bool { return t == types.FieldTypeInt32 || t == types.FieldTypeInt64 }
	same := func(a, b *float64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
	if integer(before.Type) && integer(after.Type) {
		if !same(before.Min, after.Min) || !same(before.Max, after.Max) {
			return "the bounds min and max of the field before and after the change are not the same"
		}
		return ""
	}
	for _, f := range []types.Field{before, after} {
		if f.Min != nil || f.Max != nil || f.MinLength != nil || f.MaxLength != nil || len(f.Values) > 0 {
			return "a field before or after the change has a min, a max, a min_length, a max_length or values, which the conversion between a number and a text does not keep"
		}
	}
	return ""
}
