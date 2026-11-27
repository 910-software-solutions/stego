package declaration

import (
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/types"
)

func TestFillModulePath(t *testing.T) {
	t.Run("ST-DECL-INV108-T Validate faults a fill_modules entry whose module path has no dot in its first element, at the entry", func(t *testing.T) {
		// spike-localfill (2026-11-27): module workflow was refused only as "may not import workflow", with no fault
		// at the entry and no word of the dot.
		decl := &types.ServiceDeclaration{FillModules: []types.FillModule{
			{Module: "example.com/erp/workflow", Reason: "the workflow rules"},
			{Module: "workflow", Reason: "a path with no dot"},
			{Module: "github.com/google/uuid", Reason: "IDs"},
			{Module: "erp/workflow.v2", Reason: "a dot after the first element only"},
		}}
		var faults []ValidationError
		for _, fault := range ValidateComposition(decl, Facts{}) {
			if fault.Rule == "fill-module-path" {
				faults = append(faults, fault)
			}
		}
		if len(faults) != 2 || faults[0].Path != "fill_modules[1].module" || faults[0].Found != "workflow" ||
			faults[1].Path != "fill_modules[3].module" || faults[1].Found != "erp/workflow.v2" {
			t.Fatalf("the faults of the fill modules are %+v, want one at fill_modules[1].module and one at fill_modules[3].module", faults)
		}
		if !strings.Contains(faults[0].Message, "no dot in the first element") || !strings.Contains(faults[0].Message, "example.com/workflow") {
			t.Errorf("the fault says %q, want the missing dot and an example path", faults[0].Message)
		}
	})
}
