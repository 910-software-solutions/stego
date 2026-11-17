package declaration

import (
	"fmt"

	"github.com/jsell-rh/stego/internal/types"
)

// BypassFlagPresent is the predicate of ST-DECL-SP07: a component setting of the declaration holds a value for a
// setting that its component's schema marks as a bypass. The schemas are the config schemas of the active
// components, by component name. A slot input is not part of it: no slot declares an input, so no binding can give
// one a value (decision on ST-DECL-Q40, reading a).
func BypassFlagPresent(service *types.ServiceDeclaration, schemas map[string]map[string]types.ConfigField) bool {
	return len(ValidateNoBypass(service, schemas)) > 0
}

// ValidateNoBypass gives one fault of the no-bypass rule (ST-DECL-INV24) for each value the declaration gives to a
// setting that its component's schema marks as a bypass, also in a list at any depth. Each fault names the component
// and the setting. The faults come in the order of the component names and then of the setting names.
func ValidateNoBypass(service *types.ServiceDeclaration, schemas map[string]map[string]types.ConfigField) []ValidationError {
	var faults []ValidationError
	for _, name := range sortedKeys(service.Overrides) {
		values, ok := service.Overrides[name].(map[string]any)
		if !ok {
			continue // A port binding or a convention is no setting.
		}
		schema, active := schemas[name]
		if !active {
			continue // Configuration checking names a component that is not active.
		}
		faults = append(faults, bypassFaults(name, "overrides."+name, values, schema)...)
	}
	return faults
}

// bypassFaults checks the values of one settings map against its schema, at any depth of lists.
func bypassFaults(component, path string, values map[string]any, schema map[string]types.ConfigField) []ValidationError {
	var faults []ValidationError
	for _, key := range sortedKeys(values) {
		field, declared := schema[key]
		if !declared {
			continue // Configuration checking refuses an undeclared setting.
		}
		faults = append(faults, valueBypassFaults(component, key, path+"."+key, values[key], field)...)
	}
	return faults
}

// valueBypassFaults checks one value against its schema field: a value of a field marked as a bypass is a fault,
// and a list is checked item by item, through a list of objects (Items.Fields) and through a list of lists or of
// values (Items.Inline). The fault names the setting and its path, and never holds the value.
func valueBypassFaults(component, key, place string, value any, field types.ConfigField) []ValidationError {
	if field.Bypass {
		return []ValidationError{{Category: "config", Rule: "no-bypass", Path: place,
			Message: fmt.Sprintf("the setting %s of the component %s is marked as a bypass in the component's schema, so a declaration may not give it a value; remove it from the declaration", key, component)}}
	}
	list, isList := value.([]any)
	if !isList || field.Items == nil {
		return nil
	}
	var faults []ValidationError
	for i, item := range list {
		itemPlace := fmt.Sprintf("%s[%d]", place, i)
		switch {
		case field.Items.Fields != nil:
			if object, isObject := item.(map[string]any); isObject {
				faults = append(faults, bypassFaults(component, itemPlace, object, field.Items.Fields)...)
			}
		case field.Items.Inline != nil:
			faults = append(faults, valueBypassFaults(component, key, itemPlace, item, *field.Items.Inline)...)
		}
	}
	return faults
}
