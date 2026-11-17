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

// ValidateBypassDefaults gives one fault of the no-bypass rule (ST-DECL-INV24) for each setting of an active
// component's schema that is marked as a bypass and states a default, also in a list at any depth, and for each
// environment variable of a component whose default is {settings.<key>} of a setting marked as a bypass. The
// generator resolves a schema default when the declaration gives no value, and a variable default takes the
// setting's value, so either would hold the flag that a declaration may not give (#394 security review L1). The
// schemas are the config schemas of the active components, by component name; settingDefaults gives, by component
// and then by variable name, the setting key that the variable's default names. The faults come in the order of
// the component names, then of the setting or variable names, and never hold the value.
func ValidateBypassDefaults(schemas map[string]map[string]types.ConfigField, settingDefaults map[string]map[string]string) []ValidationError {
	var faults []ValidationError
	for _, name := range sortedKeys(schemas) {
		faults = append(faults, schemaBypassDefaults(name, "components."+name+".config", schemas[name])...)
		variables := settingDefaults[name]
		for _, variable := range sortedKeys(variables) {
			key := variables[variable]
			if field, declared := schemas[name][key]; declared && field.Bypass {
				faults = append(faults, ValidationError{Category: "config", Rule: "no-bypass", Path: "components." + name + ".environment." + variable + ".default",
					Message: fmt.Sprintf("the environment variable %s of the component %s takes its default from the setting %s, which the component's schema marks as a bypass; a bypass setting gives no default to a variable", variable, name, key)})
			}
		}
	}
	return faults
}

// schemaBypassDefaults checks one config schema: a field marked as a bypass states no default, and the fields of a
// list's objects and of a list of lists are checked at their own paths.
func schemaBypassDefaults(component, path string, schema map[string]types.ConfigField) []ValidationError {
	var faults []ValidationError
	for _, key := range sortedKeys(schema) {
		faults = append(faults, fieldBypassDefaults(component, key, path+"."+key, schema[key])...)
	}
	return faults
}

// fieldBypassDefaults checks one schema field and the schema of its items.
func fieldBypassDefaults(component, key, place string, field types.ConfigField) []ValidationError {
	var faults []ValidationError
	if field.Bypass && field.Default != nil {
		faults = append(faults, ValidationError{Category: "config", Rule: "no-bypass", Path: place + ".default",
			Message: fmt.Sprintf("the setting %s of the component %s is marked as a bypass and states a default in the component's schema; a bypass setting has no default, so remove the default", key, component)})
	}
	if field.Items == nil {
		return faults
	}
	if field.Items.Fields != nil {
		faults = append(faults, schemaBypassDefaults(component, place+".items", field.Items.Fields)...)
	}
	if field.Items.Inline != nil {
		faults = append(faults, fieldBypassDefaults(component, key, place+".items", *field.Items.Inline)...)
	}
	return faults
}
