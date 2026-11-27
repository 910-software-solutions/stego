package declaration

import (
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/parser"
)

// basePathFaults reads ST-DECL-TD02 with the base path given and gives its base-path faults.
func basePathFaults(t *testing.T, basePath string) []ValidationError {
	t.Helper()
	data := strings.Replace(td02Fields, "%s", "      - {name: title, type: string, min_length: 1, max_length: 255}", 1)
	service, undeclared, err := parser.ReadServiceDeclaration([]byte(data), "service.yaml")
	if err != nil || len(undeclared) != 0 {
		t.Fatalf("read the declaration: %v %v", err, undeclared)
	}
	service.BasePath = basePath
	var faults []ValidationError
	for _, fault := range Validate(service).All() {
		if fault.Rule == "base-path" {
			faults = append(faults, fault)
		}
	}
	return faults
}

func TestBasePathForm(t *testing.T) {
	t.Run("ST-DECL-INV32-T A base path that is not empty or literal and absolute is refused", func(t *testing.T) {
		for _, accepted := range []string{"", "/api/v1", "/a-b.c_d~e/0"} {
			if faults := basePathFaults(t, accepted); len(faults) != 0 {
				t.Errorf("the base path %q got faults: %+v", accepted, faults)
			}
		}
		for _, refused := range []string{"api/v1", "/", "/api//v1", "/api/", "/api/..", "/api/.", "/api/%2e", "/api/{id}", "/api/v 1", "/api/café"} {
			faults := basePathFaults(t, refused)
			if len(faults) != 1 || faults[0].Path != "base_path" || faults[0].Found != refused || faults[0].Message == "" {
				t.Errorf("the base path %q got %+v, want one base-path fault at base_path", refused, faults)
			}
		}
	})
}
