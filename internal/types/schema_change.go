package types

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// SchemaChange is one change to the stored shape of an entity after the service's first release (ST-DECL-EN21):
// its ID, its kind, the entity and the field it changes, and what its kind needs to move the rows. The declaration
// keeps every schema change, also a finished one.
type SchemaChange struct {
	// ID is the change's name in lower kebab case, unique in the declaration.
	ID string `yaml:"id"`
	// Kind is add, rename, retype, require, drop, unique or relax.
	Kind string `yaml:"kind"`
	// Entity is the entity whose table the change changes.
	Entity string `yaml:"entity"`
	// Field is the field the change is about, by its name after the change; for unique, the name of the declared
	// invariant of kind unique.
	Field string `yaml:"field,omitempty"`
	// From is, for rename, the field's name before the change, and for retype, the field's type before it.
	From string `yaml:"from,omitempty"`
	// Conversion is, for retype, the conversion from the old type to the new one, such as text_to_integer.
	Conversion string `yaml:"conversion,omitempty"`
	// InverseConversion is, for retype, the conversion from the new type back to the old one.
	InverseConversion string `yaml:"inverse_conversion,omitempty"`
	// FillValue is, for require, the value each row that exists gets; for retype, the value of a row whose old value
	// does not convert; for drop, the default of the old column at expand.
	FillValue any `yaml:"fill_value,omitempty"`
	// Definition is, for drop, the removed field's type, constraints and default, such as string, max_length 64, or
	// the removed entity's fields; for relax, what the field was before the change, item by item (RelaxedFrom), such
	// as max_length 64, required.
	Definition string `yaml:"definition,omitempty"`
}

// SchemaStep is one entry of the declaration's ordered schema history (ST-DECL-EN22): a schema change and the phase
// it moves to, expand or contract. The phase of a change is the phase of its last step.
type SchemaStep struct {
	Change string `yaml:"change"`
	Phase  string `yaml:"phase"`
}

// A relax changes a field of a table with rows in place, in one release, so that every value that the release
// before writes stays valid and every value of the table stays readable by it: a longer max_length, more enum
// values, a default where there was none, a required field made optional, and an enum made a string with no
// max_length, which keeps the column type text (decision on finding F12 of the schema-change research, owner,
// 2026-11-13). Its Definition gives the field before the change, one item for each attribute that the change
// relaxes, separated by commas: max_length N, values V1 V2 ..., required, no default and type enum.

// RelaxItems are the items of the Definition of a relax, in the order that RelaxedFrom reads them.
var RelaxItems = []string{"type enum", "max_length", "values", "required", "no default"}

// RelaxedFrom gives the field before a relax, from the field after it and the items of the change's Definition. It
// gives a reason, and no field, when the Definition cannot be read or an item does not relax the field: a shorter or
// equal max_length, enum values that the field does not keep, required for a field that is still required, no
// default for a field with no default, or type enum for a field that is not a string with no max_length and no
// min_length. A relax changes nothing else of the field, so the other attributes before are those after.
func (c SchemaChange) RelaxedFrom(after Field) (Field, string) {
	before := after
	before.Values = slices.Clone(after.Values)
	seen := map[string]bool{}
	items := strings.Split(c.Definition, ",")
	if strings.TrimSpace(c.Definition) == "" {
		return Field{}, "the definition of a relax names no item; name what the field was, such as max_length 64"
	}
	toEnum, values := false, []string(nil)
	for _, raw := range items {
		item := strings.Join(strings.Fields(raw), " ")
		key, value, _ := strings.Cut(item, " ")
		if item == "no default" || item == "type enum" {
			key, value = item, ""
		}
		if !slices.Contains(RelaxItems, key) {
			return Field{}, fmt.Sprintf("the item %q is not one a relax takes; a relax takes max_length N, values V1 V2 ..., required, no default and type enum", item)
		}
		if seen[key] {
			return Field{}, fmt.Sprintf("the item %s is given twice", key)
		}
		seen[key] = true
		switch key {
		case "max_length":
			length, err := strconv.Atoi(value)
			if err != nil || length < 1 {
				return Field{}, fmt.Sprintf("the max_length %q is not a whole number from 1", value)
			}
			if after.Type != FieldTypeString || after.MaxLength == nil || *after.MaxLength <= length {
				return Field{}, fmt.Sprintf("max_length %d before is not shorter than the max_length of the field now; a relax only makes a max_length longer, and a column with no max_length is a new type, which needs a retype", length)
			}
			before.MaxLength = &length
		case "values":
			values = strings.Fields(value)
			if len(values) == 0 {
				return Field{}, "the item values names no value"
			}
			for i, v := range values {
				if slices.Contains(values[:i], v) {
					return Field{}, fmt.Sprintf("the value %q is given twice", v)
				}
			}
		case "required":
			if !after.Optional {
				return Field{}, "the field is required now, so required before relaxes nothing; a field made required needs a require"
			}
			before.Optional = false
		case "no default":
			if after.Default == nil {
				return Field{}, "the field has no default now, so no default before relaxes nothing; a relax only gives a default to a field with none"
			}
			before.Default = nil
		case "type enum":
			toEnum = true
		}
	}
	switch {
	case toEnum:
		if after.Type != FieldTypeString || after.MaxLength != nil || after.MinLength != nil || values == nil {
			return Field{}, "type enum before needs the values before and a field that is now a string with no max_length and no min_length, which keeps the column type text and gets no CHECK"
		}
		before.Type, before.Values = FieldTypeEnum, values
	case values != nil:
		if after.Type != FieldTypeEnum {
			return Field{}, "values before need a field that is an enum now, or type enum"
		}
		for _, v := range values {
			if !slices.Contains(after.Values, v) {
				return Field{}, fmt.Sprintf("the value %q before is not a value of the field now; a relax only adds enum values, and a value taken away needs a new field", v)
			}
		}
		if len(values) == len(after.Values) {
			return Field{}, "the values before are the values now, so they relax nothing"
		}
		before.Values = values
	}
	return before, ""
}

// FieldAfterRelax gives the field as the relax change left it: the field now, with each relax of the same field
// whose step comes after the step of change undone, the last first, as the schema history orders them. It gives the
// reason when one of those later relaxes cannot be undone.
func FieldAfterRelax(changes []SchemaChange, history []SchemaStep, change SchemaChange, now Field) (Field, string) {
	first := map[string]int{}
	for i, step := range history {
		if _, seen := first[step.Change]; !seen {
			first[step.Change] = i
		}
	}
	at, stepped := first[change.ID]
	var later []SchemaChange
	for _, other := range changes {
		if position, ok := first[other.ID]; ok && stepped && other.Kind == "relax" && other.Entity == change.Entity && other.Field == change.Field && position > at {
			later = append(later, other)
		}
	}
	slices.SortFunc(later, func(a, b SchemaChange) int { return first[b.ID] - first[a.ID] })
	field := now
	for _, other := range later {
		before, reason := other.RelaxedFrom(field)
		if reason != "" {
			return Field{}, fmt.Sprintf("the later relax %s cannot be undone: %s", other.ID, reason)
		}
		field = before
	}
	return field, ""
}
