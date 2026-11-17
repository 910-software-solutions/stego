package declaration

import (
	"strings"
	"testing"
)

func TestServerFactories(t *testing.T) {
	t.Run("ST-DECL-INV72-T A declaration with an HTTP, gRPC or process factory package is refused with the way to move its work", func(t *testing.T) {
		if faults := td02RuleFaults(t, "overrides:\n  grpc-application:\n    proto_files: []\n", "server-factory"); len(faults) != 0 {
			t.Errorf("grpc-application with no factory_package got %+v, want it accepted", faults)
		}
		if faults := td02RuleFaults(t, "overrides:\n  cli-application:\n    factory_package: internal/cli\n", "server-factory"); len(faults) != 0 {
			t.Errorf("the cli-application factory got %+v, want it accepted: it is the stated exception", faults)
		}
		faults := td02RuleFaults(t, "overrides:\n  grpc-application:\n    factory_package: internal/rpc\n", "server-factory")
		if len(faults) != 1 || faults[0].Path != "overrides.grpc-application.factory_package" || faults[0].Found != "internal/rpc" {
			t.Fatalf("the grpc-application factory got %+v, want one server-factory fault at its setting", faults)
		}
		for _, word := range []string{"grpc-application", "factory_package", "internal/rpc", "worker", "command", "query", "collection", "method", "server stream", "process method", "observation", "client streams and bidirectional streams are not supported yet"} {
			if !strings.Contains(faults[0].Message, word) {
				t.Errorf("the fault does not name %q: %s", word, faults[0].Message)
			}
		}
	})
}
