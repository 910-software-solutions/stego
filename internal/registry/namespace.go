package registry

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/910-software-solutions/stego/internal/gen"
)

// checkNamespace is the rule of ST-SUP-INV13 that the registry holds: a component's output namespace, as the
// component states it, is a valid Go import path, so that no generator renders a file under a folder that Go
// cannot import. It is checked when the component is decoded, before any generator runs, and the namespace is
// never rewritten. A component that states no namespace generates no file (ST-GEN-INV07). The package name at the
// end of a library namespace is checked by the preflight of the library's generator, before it renders.
func checkNamespace(component *Component) error {
	namespace := component.OutputNamespace
	if namespace == "" {
		return nil
	}
	if err := gen.ValidateGoImportNamespace(namespace); err != nil {
		return fmt.Errorf("component %s: the output namespace %q is not a valid Go import path: %w", component.Name, namespace, err)
	}
	// A path element that the go command skips, or that names a vendored or test-only folder, holds no package
	// that a service can import (sec-500 L2).
	for _, element := range strings.Split(namespace, "/") {
		if strings.HasPrefix(element, ".") || strings.HasPrefix(element, "_") || element == "testdata" || element == "vendor" {
			return fmt.Errorf("component %s: the output namespace %q holds the path element %q, which the go command ignores or treats as special", component.Name, namespace, element)
		}
	}
	// The paths that STEGO itself writes: the module files, the slot contracts, the runtime wiring and the files
	// that apply places in the project folder (sec-500 L1). A namespace may not be one of them or hold one.
	lower := strings.ToLower(namespace)
	for _, reserved := range reservedNamespaces {
		if owned, ok := ReservedNamespaceOwner[component.Name]; ok && owned == namespace {
			continue
		}
		if lower == reserved || strings.HasPrefix(lower, reserved+"/") || strings.HasPrefix(reserved, lower+"/") {
			return fmt.Errorf("component %s: the output namespace %q overlaps %s, which STEGO writes", component.Name, namespace, reserved)
		}
	}
	return nil
}

// reservedNamespaces are the paths, in lower case, that STEGO writes beside the namespaces of the components: the
// module files, the slot contracts, the runtime wiring, and the paths that apply writes in the project folder (the
// platform requirements and the Kubernetes base under deploy/, the architecture view, the CI files and the
// Containerfile). The component kubernetes-service writes its renderer under deploy, which the compiler places in
// the generated output; it is the one component whose namespace is deploy (ReservedNamespaceOwner).
var reservedNamespaces = []string{"go.mod", "go.sum", "slots", "internal/stegoruntime", "deploy", "architecture", ".github", ".tekton", "containerfile"}

// ReservedNamespaceOwner is the one component that may state a reserved namespace, and the namespace: the
// kubernetes-service component writes its renderer into out/deploy, never into the project's deploy folder.
var ReservedNamespaceOwner = map[string]string{"kubernetes-service": "deploy"}

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
	if component == nil {
		return fmt.Errorf("component %s is not in the active component set, so its files have no namespace", name)
	}
	if len(files) == 0 {
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
