package declaration

import (
	"fmt"
	"strings"

	"github.com/910-software-solutions/stego/internal/types"
)

// validateFillModules faults each fill_modules entry whose module path has no dot in its first element, such as
// workflow. The fill check takes an import path whose first element has no dot as a package of the standard library,
// so it refused the fill only with "may not import workflow", and no fault named the entry (spike-localfill,
// 2026-11-27). Go module paths outside the standard library start with a domain name, such as example.com/erp/workflow.
func validateFillModules(modules []types.FillModule) []ValidationError {
	var faults []ValidationError
	for i, module := range modules {
		first, _, _ := strings.Cut(module.Module, "/")
		if strings.Contains(first, ".") {
			continue
		}
		faults = append(faults, ValidationError{Category: "fill", Rule: "fill-module-path", Path: fmt.Sprintf("fill_modules[%d].module", i), Found: module.Module,
			Message: fmt.Sprintf("the fill module %q has no dot in the first element of its path; a fill imports only a module whose path starts with a domain name, such as example.com/%s, as the go command takes a path with no dot there as the standard library; change the module line of its go.mod and this entry", module.Module, strings.TrimPrefix(module.Module, "/"))})
	}
	return faults
}
