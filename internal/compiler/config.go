package compiler

import (
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

// validateComponentConfig checks supplied values and registry defaults before
// generators consume them. Missing runtime settings are checked at startup.
func validateComponentConfig(service *types.ServiceDeclaration, components map[string]*registry.Component) []ValidationError {
	var errors []ValidationError
	names := sortedKeys(service.Overrides)
	for _, name := range names {
		if declaration.IsConventionOverrideKey(name) {
			continue
		}
		value := service.Overrides[name]
		if _, binding := value.(string); binding {
			continue // Port resolution checks string-valued bindings.
		}
		config, ok := value.(map[string]any)
		if !ok {
			errors = append(errors, configError("overrides."+name, "must be a component configuration map or a port binding"))
			continue
		}
		component := components[name]
		if component == nil {
			errors = append(errors, configError("overrides."+name, "component is not active"))
			continue
		}
		errors = append(errors, declaration.ValidateSettings("overrides."+name, config, component.Config, false)...)
	}
	// No setting holds a value for a setting that its component's schema marks as a bypass (ST-DECL-INV24).
	schemas := make(map[string]map[string]types.ConfigField, len(components))
	for name, component := range components {
		schemas[name] = component.Config
	}
	errors = append(errors, declaration.ValidateNoBypass(service, schemas)...)
	errors = append(errors, declaration.ValidateAuthorSupplied(service, schemas)...)
	// A bypass setting states no default, and no environment variable takes its default from one, since either would
	// hold the flag when the declaration gives no value (#394 security review L1).
	errors = append(errors, declaration.ValidateBypassDefaults(schemas, settingDefaults(components))...)
	for _, name := range sortedKeys(components) {
		for _, key := range sortedKeys(components[name].Config) {
			field := components[name].Config[key]
			if field.Default != nil {
				errors = append(errors, declaration.ValidateSettingValue("components."+name+".config."+key+".default", field.Default, field)...)
			}
		}
	}
	return errors
}

// settingDefaults gives, by component and then by environment variable, the setting key that the variable's
// default {settings.<key>} names.
func settingDefaults(components map[string]*registry.Component) map[string]map[string]string {
	defaults := map[string]map[string]string{}
	for name, component := range components {
		for _, variable := range component.Variables() {
			if variable.Default == nil {
				continue
			}
			key, ok := strings.CutPrefix(*variable.Default, "{settings.")
			if !ok || !strings.HasSuffix(key, "}") {
				continue
			}
			if defaults[name] == nil {
				defaults[name] = map[string]string{}
			}
			defaults[name][variable.Name] = strings.TrimSuffix(key, "}")
		}
	}
	return defaults
}

func configError(path, message string) ValidationError {
	return ValidationError{Category: "config", Rule: "config", Path: path, Message: path + ": " + message}
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
