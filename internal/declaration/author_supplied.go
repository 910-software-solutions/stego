package declaration

import (
	"fmt"

	"github.com/910-software-solutions/stego/internal/types"
)

// AuthorSuppliedSettings names each setting that a config schema marks as author supplied (ST-SUP-EN09 field Config
// schema), as overrides.<component>.<setting>, in the order of the component names and then of the setting names.
// The schemas are the config schemas of the active components, by component name. An empty scaffold prints these
// names (ST-DECL-UC01-R05), and ValidateAuthorSupplied names the same paths.
func AuthorSuppliedSettings(schemas map[string]map[string]types.ConfigField) []string {
	var names []string
	for _, component := range sortedKeys(schemas) {
		for _, setting := range sortedKeys(schemas[component]) {
			if schemas[component][setting].AuthorSupplied {
				names = append(names, "overrides."+component+"."+setting)
			}
		}
	}
	return names
}

// ValidateAuthorSupplied gives one fault of the author-supplied rule (ST-DECL-INV90) for each setting that a config
// schema of an active component marks as author supplied and the declaration gives no value for, or an empty
// value: an empty text, list or map. Each fault is at
// overrides.<component>.<setting> and names the component and the setting. The faults come in the order of
// AuthorSuppliedSettings.
func ValidateAuthorSupplied(service *types.ServiceDeclaration, schemas map[string]map[string]types.ConfigField) []ValidationError {
	var faults []ValidationError
	for _, component := range sortedKeys(schemas) {
		values, _ := service.Overrides[component].(map[string]any)
		for _, setting := range sortedKeys(schemas[component]) {
			if !schemas[component][setting].AuthorSupplied {
				continue
			}
			if value, given := values[setting]; given && !emptyValue(value) {
				continue
			}
			faults = append(faults, ValidationError{Category: "config", Rule: "author-supplied", Path: "overrides." + component + "." + setting,
				Message: fmt.Sprintf("the component %s needs the setting %s, which its schema marks as author supplied; give it under overrides.%s.%s", component, setting, component, setting)})
		}
	}
	return faults
}

// emptyValue tells whether a setting's value gives nothing: no value, an empty text, an empty list or an empty map.
func emptyValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	}
	return false
}
