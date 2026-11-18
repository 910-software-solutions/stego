package declaration

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/jsell-rh/stego/internal/types"
)

// MethodKind is the kind of a gRPC method of a proto file: unary, a server stream, a client stream or a stream both
// ways.
type MethodKind string

// The kinds of a gRPC method.
const (
	MethodUnary        MethodKind = "unary"
	MethodServerStream MethodKind = "server stream"
	MethodClientStream MethodKind = "client stream"
	MethodBidiStream   MethodKind = "bidirectional stream"
)

// methodName is the form of a Method: package.Service/Method, with a package of one or more dotted names.
var methodName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)+/[A-Za-z_][A-Za-z0-9_]*$`)

// validateGRPCMethods checks the gRPC method of each command and query (ST-DECL-INV73): a command's Method names a
// unary method, and a query's Method a unary method or a server stream, of a service in a proto file that the
// grpc-application component declares; a Method is given only when that component is active (facts.GRPCMethods is
// not nil), and no two commands or queries name the same Method. Each fault names the method. When the component set
// did not resolve (facts.ComponentsUnresolved), it gives no fault for a missing component: the resolution fault names
// the cause. A query whose Method is a server stream declares no route (ST-DECL-INV73).
func validateGRPCMethods(service *types.ServiceDeclaration, facts Facts) []ValidationError {
	type operation struct {
		kind, name, path, method string
		allowed                  []MethodKind
	}
	var operations []operation
	for i, c := range service.Commands {
		operations = append(operations, operation{"command", c.Name, commandPath(i) + ".method", c.Method, []MethodKind{MethodUnary}})
	}
	for i, q := range service.Queries {
		operations = append(operations, operation{"query", q.Name, queryPath(i) + ".method", q.Method, []MethodKind{MethodUnary, MethodServerStream}})
	}
	var faults []ValidationError
	first := map[string]string{}
	for _, op := range operations {
		if op.method == "" {
			continue
		}
		fault := func(format string, args ...any) {
			faults = append(faults, ValidationError{Category: op.kind, Rule: op.kind + "-method", Path: op.path, Found: op.method,
				Message: fmt.Sprintf("%s %q has the gRPC method %s, which ", op.kind, op.name, op.method) + fmt.Sprintf(format, args...)})
		}
		kind, declared := facts.GRPCMethods[op.method]
		switch {
		case facts.GRPCMethods == nil && facts.ComponentsUnresolved:
			// The component set did not resolve, so it is not known whether grpc-application is active.
			continue
		case facts.GRPCMethods == nil:
			fault("needs the grpc-application component; add the component or remove the method")
			continue
		case !methodName.MatchString(op.method):
			fault("is not package.Service/Method")
			continue
		case !declared:
			fault("no service of the proto files of the grpc-application component declares; declare it there or name a declared method (known: %v)", knownMethods(facts.GRPCMethods))
			continue
		}
		allowed := false
		for _, k := range op.allowed {
			allowed = allowed || k == kind
		}
		if !allowed {
			fault("is a %s; a %s is served by %s", kind, op.kind, allowedKinds(op.allowed))
		}
		if other, used := first[op.method]; used {
			fault("is already the method of %s; give each command and query its own method", other)
			continue
		}
		first[op.method] = fmt.Sprintf("%s %q", op.kind, op.name)
	}
	// A stream query is served over gRPC only (ST-SUP-RC33-OC08), so a route of it would be served by no handler
	// (walkthrough 12, W12-B2): it declares none.
	for i, q := range service.Queries {
		if q.Route == "" || q.Method == "" || facts.GRPCMethods[q.Method] != MethodServerStream {
			continue
		}
		faults = append(faults, ValidationError{Category: "query", Rule: "query-stream-route", Path: queryPath(i) + ".route", Found: q.Route,
			Message: fmt.Sprintf("query %q has the route %q and the gRPC method %s, which is a server stream; a stream is served over gRPC only, so remove the route. To list over HTTP, declare a second query with a route and a unary method or no method", q.Name, q.Route, q.Method)})
	}
	return faults
}

// validateGRPCServed checks the methods of the proto files of the grpc-application component when it is active
// (ST-DECL-INV75): each unary method and each server stream is the Method of a command or a query, and no service has
// a client stream or a bidirectional stream. Each fault names the method; the fault of a stream both ways or from the
// client says that such streams are not supported yet. A Method named twice is the fault of validateGRPCMethods.
func validateGRPCServed(service *types.ServiceDeclaration, facts Facts) []ValidationError {
	if facts.GRPCMethods == nil {
		return nil
	}
	served := map[string]bool{}
	for _, c := range service.Commands {
		served[c.Method] = true
	}
	for _, q := range service.Queries {
		served[q.Method] = true
	}
	names := make([]string, 0, len(facts.GRPCMethods))
	for name := range facts.GRPCMethods {
		names = append(names, name)
	}
	sort.Strings(names)
	const path = "overrides.grpc-application.proto_files"
	var faults []ValidationError
	for _, name := range names {
		switch kind := facts.GRPCMethods[name]; {
		case kind == MethodClientStream || kind == MethodBidiStream:
			faults = append(faults, ValidationError{Category: "grpc", Rule: "grpc-method-stream", Path: path, Found: name,
				Message: fmt.Sprintf("the gRPC method %s is a %s, and client streams and bidirectional streams are not supported yet; remove it from the proto file, or make it a unary method for a command or query, or a server stream for a query", name, kind)})
		case !served[name]:
			faults = append(faults, ValidationError{Category: "grpc", Rule: "grpc-method-unserved", Path: path, Found: name,
				Message: fmt.Sprintf("the gRPC method %s is a %s that no command or query serves; give it as the method of a command (a unary method) or of a query (a unary method or a server stream), or remove it from the proto file", name, kind)})
		}
	}
	return faults
}

// QueryStreams gives the names of the queries whose gRPC method is a server stream of the proto files of the
// grpc-application component (ST-DECL-INV73, ST-SUP-RC33-OC08). The compiler marks each as a stream query in
// gen.Context.QueryStreams.
func QueryStreams(service *types.ServiceDeclaration, facts Facts) map[string]bool {
	streams := map[string]bool{}
	for _, q := range service.Queries {
		if q.Method != "" && facts.GRPCMethods[q.Method] == MethodServerStream {
			streams[q.Name] = true
		}
	}
	return streams
}

// knownMethods lists the declared methods in order, at most ten, for a fault.
func knownMethods(methods map[string]MethodKind) []string {
	names := make([]string, 0, len(methods))
	for name := range methods {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > 10 {
		names = append(names[:10], "...")
	}
	return names
}

// allowedKinds names the kinds of method that an operation may have.
func allowedKinds(kinds []MethodKind) string {
	if len(kinds) == 1 {
		return "a " + string(kinds[0]) + " method"
	}
	return "a " + string(kinds[0]) + " method or a " + string(kinds[1])
}
