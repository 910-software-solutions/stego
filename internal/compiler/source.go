package compiler

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

// compilationSource holds one read of the declaration and registry. Validation,
// generation, and the state hash must use the same declaration bytes.
type compilationSource struct {
	ServiceData []byte
	Service     *types.ServiceDeclaration
	// Undeclared holds the fields that the declaration format does not declare; validation reports each.
	Undeclared []parser.UndeclaredField
	Registry   *registry.Registry
	Resolved   *resolvedCompilation
	// Active is the active component set, once validation resolves it.
	Active map[string]*registry.Component
}

func loadCompilationSource(input ReconcilerInput) (*compilationSource, error) {
	path := filepath.Join(input.ProjectDir, "service.yaml")
	data, err := parser.ReadDocument(path)
	if err != nil {
		return nil, fmt.Errorf("reading service.yaml: %w", err)
	}
	service, undeclared, err := parser.ReadServiceDeclaration(data, path)
	if err != nil {
		return nil, &declarationReadError{fmt.Errorf("parsing service.yaml: %w", err)}
	}
	dirs := input.RegistryDirs
	if len(dirs) == 0 {
		dirs = []string{input.RegistryDir}
	}
	reg, err := registry.LoadDirectories(dirs...)
	if err != nil {
		// A need is named as the service sees it, with the service name in place of {service} (ST-DLV-UC01-R02).
		var refused *registry.NeedError
		if errors.As(err, &refused) {
			named := *refused
			named.Need = strings.ReplaceAll(named.Need, "{service}", service.Name)
			return nil, &RegistryLoadError{err: &named}
		}
		return nil, &RegistryLoadError{err: err}
	}
	return &compilationSource{ServiceData: data, Service: service, Undeclared: undeclared, Registry: reg}, nil
}

// RegistryLoadError is a registry that the compiler could not load from the folders that the resolver gave, such as a
// folder that does not exist. It comes from outside the declaration, so stego compat, which checks the declaration
// against its base, can still give the result of that check (ST-GEN-CLI07).
type RegistryLoadError struct{ err error }

func (e *RegistryLoadError) Error() string { return "loading registry: " + e.err.Error() }
func (e *RegistryLoadError) Unwrap() error { return e.err }

// FillError is a fill of the project that the compiler could not read or check, such as a fill manifest that does
// not parse, or a qualification of the bound fills that could not run. The declaration checks did not run to their
// end, so no caller may take their result as a pass (security review of PR #499, L2).
type FillError struct{ err error }

func (e *FillError) Error() string { return e.err.Error() }
func (e *FillError) Unwrap() error { return e.err }

// declarationReadError is a declaration that cannot be read as one document. Validation reports it as the one
// fault of the declaration (ST-DECL-UC04-R06); plan and apply stop with it.
type declarationReadError struct{ err error }

func (e *declarationReadError) Error() string { return e.err.Error() }
func (e *declarationReadError) Unwrap() error { return e.err }
