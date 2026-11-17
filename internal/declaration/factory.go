package declaration

import (
	"fmt"

	"github.com/jsell-rh/stego/internal/types"
)

// grpcApplication is the server-side gRPC component, which serves the declared commands and queries and gives no
// factory from 2.0.0 (ST-SUP-RC07-OC19). Every release of it in the registry is 2.0.0 or later.
const grpcApplication = "grpc-application"

// factoryMoves says where each part of the work of a factory goes now (ST-DECL-INV72).
const factoryMoves = "a background task or an event source goes to a worker (workers), a write to a command (commands), " +
	"a read to a query (queries), a route to a command, a query or a collection (collections), a registered gRPC method or " +
	"server stream to the method of a command or a query, a process's method to a process method, initialization and " +
	"cleanup to the generated runtime, and a log or a domain signal to an observation (observations); client streams and " +
	"bidirectional streams are not supported yet"

// validateServerFactories refuses the setting factory_package of the grpc-application component (ST-DECL-INV72): no
// hand-written code runs on the server outside a fill, so the fault names the component, the setting and the package,
// and says where each part of the factory's work goes now. The cli-application factory is the stated exception
// (ST-DECL-INV17), and http-application and grpc-processes still take factory_package in the releases that the
// registry holds.
func validateServerFactories(service *types.ServiceDeclaration) []ValidationError {
	if service == nil {
		return nil
	}
	config, _ := service.Overrides[grpcApplication].(map[string]any)
	value, set := config["factory_package"]
	if !set {
		return nil
	}
	pkg := fmt.Sprint(value)
	return []ValidationError{{
		Category: "config",
		Rule:     "server-factory",
		Path:     "overrides." + grpcApplication + ".factory_package",
		Found:    pkg,
		Message: fmt.Sprintf("the component %s gives no factory, so its setting factory_package (the package %s) is refused: the generated server runs no hand-written code outside a fill; move the work of the package: %s",
			grpcApplication, pkg, factoryMoves),
	}}
}
