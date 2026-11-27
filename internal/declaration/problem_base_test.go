package declaration

import (
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/parser"
)

// td02RuleFaults reads ST-DECL-TD02 with the top-level text given added and gives its faults of the rule.
func td02RuleFaults(t *testing.T, added, rule string) []ValidationError {
	t.Helper()
	data := strings.Replace(td02Fields, "%s", "      - {name: title, type: string, min_length: 1, max_length: 255}", 1) + added
	service, undeclared, err := parser.ReadServiceDeclaration([]byte(data), "service.yaml")
	if err != nil || len(undeclared) != 0 {
		t.Fatalf("read the declaration: %v %v", err, undeclared)
	}
	var faults []ValidationError
	for _, fault := range Validate(service).All() {
		if fault.Rule == rule {
			faults = append(faults, fault)
		}
	}
	return faults
}

func TestProblemBase(t *testing.T) {
	t.Run("ST-DECL-INV83-T Idempotency without a problem base is refused", func(t *testing.T) {
		const idempotency = "idempotency:\n  required_on: [create]\n"
		if faults := td02RuleFaults(t, idempotency+"error_type_base: https://errors.example.com/todo/\n", "problem-base"); len(faults) != 0 {
			t.Errorf("idempotency with error_type_base got %+v, want it accepted", faults)
		}
		if faults := td02RuleFaults(t, "", "problem-base"); len(faults) != 0 {
			t.Errorf("no idempotency and no error_type_base got %+v, want it accepted", faults)
		}
		faults := td02RuleFaults(t, idempotency, "problem-base")
		if len(faults) != 1 || faults[0].Path != "error_type_base" || !strings.Contains(faults[0].Message, "idempotency") || !strings.Contains(faults[0].Message, "error_type_base") {
			t.Errorf("idempotency with no error_type_base got %+v, want one fault at error_type_base that names idempotency", faults)
		}
	})
}
