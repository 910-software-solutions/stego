package compiler

import (
	"slices"

	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

// databaseConversions gives the conversions that the database component among components declares, by name, and
// whether one of them declares conversions: the component that provides the storage-adapter port.
func databaseConversions(components map[string]*registry.Component) (map[string]types.Conversion, bool) {
	for _, name := range sortedKeys(components) {
		component := components[name]
		if !slices.ContainsFunc(component.Provides, func(p registry.Port) bool { return p.Name == "storage-adapter" }) {
			continue
		}
		out := map[string]types.Conversion{}
		for _, c := range component.Conversions {
			out[c.Name] = c
		}
		return out, true
	}
	return nil, false
}

// validateSchemaConversions checks the conversions of each retype against the database component among components
// (declaration.ValidateSchemaConversions, ST-DECL-INV62, ST-DECL-INV63).
func validateSchemaConversions(service *types.ServiceDeclaration, components map[string]*registry.Component) []ValidationError {
	conversions, declared := databaseConversions(components)
	return declaration.ValidateSchemaConversions(service, conversions, declared)
}
