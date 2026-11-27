package compiler

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/token"
	"path"
	"slices"

	"github.com/910-software-solutions/stego/internal/registry"
)

// declaredModes gives the modes that a declaration states, in the order api, worker, controller, each once; all three
// when it states none (ST-DECL-EN01). Checking refuses any other mode (ST-DECL-INV28).
func declaredModes(stated []string) []string {
	all := []string{"api", "worker", "controller"}
	if len(stated) == 0 {
		return all
	}
	var modes []string
	for _, m := range all {
		if slices.Contains(stated, m) {
			modes = append(modes, m)
		}
	}
	return modes
}

// modeSelectionExpression gives the call of the function that gives the modes the process runs, which one consumed
// component supplies (ST-GEN-INV26), or empty when no component supplies it.
func modeSelectionExpression(input AssemblerInput, imports importResult, consumed map[int]bool) (string, error) {
	call := ""
	for i, component := range input.Wirings {
		if component.Wiring == nil || component.Wiring.ModeSelection == nil {
			continue
		}
		selection := component.Wiring.ModeSelection
		if call != "" {
			return "", fmt.Errorf("multiple components declare the mode selection")
		}
		if !consumed[i] || !slices.Contains(component.Wiring.Imports, selection.Namespace) || !token.IsIdentifier(selection.Function) || !ast.IsExported(selection.Function) {
			return "", fmt.Errorf("component %q has an invalid mode selection", component.Name)
		}
		name := path.Base(selection.Namespace)
		if renamed := imports.Renames[i][name]; renamed != "" {
			name = renamed
		}
		call = name + "." + selection.Function + "()"
	}
	return call, nil
}

// writeModeSelection writes the first step of run: it reads the modes the process runs at the stage modes, before
// the database and every constructor, so that an unknown mode, a mode that the declaration does not state or an
// argument that is not a mode stops the start at that stage, and the failure record of every later failure names
// the selected modes (ST-GEN-INV26, ST-GEN-INV16).
func writeModeSelection(buf *bytes.Buffer, call string) {
	if call == "" {
		return
	}
	fmt.Fprintf(buf, "	stegoStage = \"modes\"\n\tif stegoModes, stegoErr = %s; stegoErr != nil {\n\t\treturn stegoErr\n\t}\n", call)
}

// modeVariable is the environment variable that selects the modes of a process when no mode option is given.
const modeVariable = "STEGO_MODE"

// declaredModeVariables gives the environment variables of a component with the allowed values of STEGO_MODE
// narrowed to the declared modes and all, so that stego env check refuses a value that names a mode the
// declaration does not state (ST-GEN-INV26). The registry's list is not changed.
func declaredModeVariables(variables []registry.EnvironmentVariable, stated []string) []registry.EnvironmentVariable {
	out := slices.Clone(variables)
	for i, variable := range out {
		if variable.Name != modeVariable || variable.Allowed == nil || len(variable.Allowed.Values) == 0 {
			continue
		}
		allowed := *variable.Allowed
		allowed.Values = append(declaredModes(stated), "all")
		out[i].Allowed = &allowed
	}
	return out
}
