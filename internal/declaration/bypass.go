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
// setting that its component's schema marks as a bypass, also in a setting of an object in a list. Each fault names the component
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

// bypassFaults checks the values of one settings map, and of each object they hold, against its schema.
func bypassFaults(component, path string, values map[string]any, schema map[string]types.ConfigField) []ValidationError {
	var faults []ValidationError
	for _, key := range sortedKeys(values) {
		field, declared := schema[key]
		if !declared {
			continue // Configuration checking refuses an undeclared setting.
		}
		place := path + "." + key
		if field.Bypass {
			faults = append(faults, ValidationError{Category: "config", Rule: "no-bypass", Path: place, Found: fmt.Sprint(values[key]),
				Message: fmt.Sprintf("the setting %s of the component %s is marked as a bypass in the component's schema, so a declaration may not give it a value; remove it from the declaration", key, component)})
			continue
		}
		// A list of objects holds settings of its own (ValidateSettingValue).
		list, isList := values[key].([]any)
		if !isList || field.Items == nil || field.Items.Fields == nil {
			continue
		}
		for i, item := range list {
			if object, isObject := item.(map[string]any); isObject {
				faults = append(faults, bypassFaults(component, fmt.Sprintf("%s[%d]", place, i), object, field.Items.Fields)...)
			}
		}
	}
	return faults
}
