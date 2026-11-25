package registry

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
)

// checkNamespace is the rule of ST-SUP-INV13 that the registry holds: a component's output namespace, as the
// component states it, is a valid Go import path, so that no generator renders a file under a folder that Go
// cannot import. It is checked when the component is decoded, before any generator runs, and the namespace is
// never rewritten. A component that states no namespace generates no file (ST-GEN-INV07). The package name at the
// end of a library namespace is checked by the preflight of the library's generator, before it renders.
func checkNamespace(component *Component) error {
	if component.OutputNamespace == "" {
		return nil
	}
	if err := gen.ValidateGoImportNamespace(component.OutputNamespace); err != nil {
		return fmt.Errorf("component %s: the output namespace %q is not a valid Go import path: %w", component.Name, component.OutputNamespace, err)
	}
	return nil
}

// insideNamespace tells whether a path is the namespace or under it, by whole path elements: internal/store-extra
// is not inside internal/store.
func insideNamespace(name, namespace string) bool {
	return namespace != "" && (name == namespace || strings.HasPrefix(name, namespace+"/"))
}

// WritesOwnNamespace is the rule of ST-SUP-INV02: every file that the component planned is inside the namespace it
// declares, so one component cannot overwrite the output of another. It refuses the component, naming it and each
// file that is outside its namespace; for a file inside the namespace of another component of the active set, it
// names that component and its namespace too. The caller runs it before any file is written.
func WritesOwnNamespace(name string, components map[string]*Component, files []string) error {
	component := components[name]
	if component == nil || len(files) == 0 {
		return nil
	}
	var faults []string
	for _, file := range files {
		if gen.ValidatePath(file) == nil && insideNamespace(file, component.OutputNamespace) {
			continue
		}
		owner := ""
		for _, other := range slices.Sorted(maps.Keys(components)) {
			if other != name && components[other] != nil && insideNamespace(file, components[other].OutputNamespace) {
				owner = other
				break
			}
		}
		if owner != "" {
			faults = append(faults, fmt.Sprintf("%s is in the namespace %s of the component %s", file, components[owner].OutputNamespace, owner))
		} else {
			faults = append(faults, fmt.Sprintf("%s is outside %s", file, component.OutputNamespace))
		}
	}
	if len(faults) > 0 {
		return fmt.Errorf("component %s writes only into its own namespace %q, and it planned files outside it: %s", name, component.OutputNamespace, strings.Join(faults, "; "))
	}
	return nil
}
