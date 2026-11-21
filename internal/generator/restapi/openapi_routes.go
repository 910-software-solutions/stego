package restapi

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/command"
	"github.com/jsell-rh/stego/internal/types"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// commandMarker is the OpenAPI extension that marks the operation of a command. The request validation of the
// emitted middleware does not check its body against the document: the command runtime decodes it, and accepts both
// protobuf JSON names of a member (ST-SUP-RC20-OC53).
const commandMarker = "x-stego-command"

// maxMessageDepth is how deep the schema of a command message follows nested messages; a deeper or a recursive
// message is an object with no fixed members.
const maxMessageDepth = 8

// OpenAPIDocument gives the OpenAPI document that this component emits for the service of ctx, with the command and
// query routes, as Generate writes it in openapi.json. The compiler gives it to the client components that name no
// document of their own (ST-SUP-RC06-OC09, ST-SUP-RC04-OC32).
func (g *Generator) OpenAPIDocument(ctx gen.Context) ([]byte, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, err
	}
	file, err := serviceOpenAPIFile(ctx)
	if err != nil {
		return nil, err
	}
	return file.Content, nil
}

// RouteOpenAPIDocument gives the OpenAPI document of the command and query routes of a service with no rest-api
// component, from the context of the command component (ST-SUP-RC32-OC29): each command and each get or list query
// that the service serves over HTTP, in the form of ST-SUP-RC20-OC53, and no collection route. It gives nil when the
// service serves none.
func (g *Generator) RouteOpenAPIDocument(command gen.Context) ([]byte, error) {
	ctx := command
	ctx.PeerConfigs = maps.Clone(command.PeerConfigs)
	ctx.PeerNamespaces = maps.Clone(command.PeerNamespaces)
	if ctx.PeerConfigs == nil {
		ctx.PeerConfigs = map[string]map[string]any{}
	}
	if ctx.PeerNamespaces == nil {
		ctx.PeerNamespaces = map[string]string{}
	}
	ctx.PeerConfigs["command"], ctx.PeerNamespaces["command"] = command.ComponentConfig, command.OutputNamespace
	routes, err := routeOperations(ctx)
	if err != nil {
		return nil, err
	}
	if len(routes) == 0 {
		return nil, nil
	}
	file, err := generateOpenAPI(command.OutputNamespace, nil, nil, nil, ctx.BasePath, ctx.Conventions.ResponseFormat, nil, routes)
	if err != nil {
		return nil, err
	}
	return file.Content, nil
}

// serviceOpenAPIFile gives openapi.json of the service of ctx: the collection routes and the command and query routes.
func serviceOpenAPIFile(ctx gen.Context) (gen.File, error) {
	collectionMap := make(map[string]types.Collection, len(ctx.Collections))
	for _, collection := range ctx.Collections {
		if _, exists := collectionMap[collection.Entity]; !exists {
			collectionMap[collection.Entity] = collection
		}
	}
	routes, err := routeOperations(ctx)
	if err != nil {
		return gen.File{}, err
	}
	return generateOpenAPI(ctx.OutputNamespace, ctx.Entities, ctx.Collections, collectionMap, ctx.BasePath, ctx.Conventions.ResponseFormat, idempotencyKeys(ctx), routes)
}

// routeOperations gives the operation of each command and each get or list query that the service serves over HTTP,
// by path and method (ST-SUP-RC20-OC53). A command's messages come from the proto files of the command component.
func routeOperations(ctx gen.Context) (map[string]map[string]openAPIOperation, error) {
	out := map[string]map[string]openAPIOperation{}
	add := func(route, method string, op openAPIOperation) error {
		if out[route] == nil {
			out[route] = map[string]openAPIOperation{}
		}
		if _, clash := out[route][method]; clash {
			return fmt.Errorf("two commands or queries have the route %s %s", strings.ToUpper(method), route)
		}
		out[route][method] = op
		return nil
	}
	var descriptors map[string]protoreflect.MessageDescriptor
	if len(ctx.Commands) > 0 {
		config, active := ctx.PeerConfigs["command"]
		if !active {
			return nil, fmt.Errorf("the service declares commands, and the command component is not active")
		}
		peer := ctx
		peer.ComponentConfig, peer.OutputNamespace = config, ctx.PeerNamespaces["command"]
		var err error
		if descriptors, err = command.MessageDescriptors(peer); err != nil {
			return nil, err
		}
	}
	for _, c := range ctx.Commands {
		verb, routePath, ok := strings.Cut(c.Route, " ")
		if !ok || verb != "POST" {
			continue
		}
		goName := command.GoName(c.Name)
		request, response := descriptors[goName+"Request"], descriptors[goName+"Response"]
		if request == nil || response == nil {
			return nil, fmt.Errorf("the command %s has no message %sRequest or %sResponse in the proto files of the command component", c.Name, goName, goName)
		}
		params := pathParameters(routePath)
		body := messageSchema(request, 0, map[protoreflect.FullName]bool{})
		writeOnlySecrets(&body)
		for _, p := range params {
			delete(body.Properties, p)
			f := request.Fields().ByName(protoreflect.Name(p))
			if f == nil {
				f = request.Fields().ByJSONName(p)
			}
			if f == nil {
				continue
			}
			// A path parameter goes in the URL, the access logs and a CLI flag, so it cannot hold a secret
			// (security review of PR #460, L2).
			if command.SecretMember(f) {
				return nil, fmt.Errorf("the command %s has the path parameter %s, and the proto file marks the member %s of %sRequest debug_redact: a secret member cannot be a path parameter, because the URL goes in access logs and a CLI flag; give it in the body", c.Name, p, f.Name(), goName)
			}
			delete(body.Properties, f.JSONName())
		}
		status := "200"
		last := routePath[strings.LastIndex(routePath, "/")+1:]
		if !strings.HasPrefix(last, "{") && !strings.Contains(last, ":") {
			status = "201"
		}
		op := openAPIOperation{
			Summary:     "Run the command " + c.Name,
			OperationID: lowerCamel(c.Name),
			Tags:        []string{"commands"},
			Command:     true,
			Parameters: append(stringPathParams(params), openAPIParam{Name: "Idempotency-Key", In: "header", Required: true,
				Description: "A structured string that holds a key, such as a quoted UUID; a second request with the same key gets the stored answer", Schema: openAPISchema{Type: "string"}}),
			RequestBody: &openAPIRequestBody{Required: true, Content: jsonContent(body)},
			Responses: map[string]openAPIResponse{
				status: {Description: "The response of the command", Content: jsonContent(responseSchema(response))},
				"400":  {Description: "Bad request: the body, a path parameter or the Idempotency-Key header is not valid"},
				"401":  {Description: "Unauthorized"},
				"404":  {Description: "Not found: the resource is not there, or the caller may not run the command on it"},
				"409":  {Description: "Conflict"},
				"500":  {Description: "Internal error"},
			},
		}
		op.Parameters = append(op.Parameters, revocationParameters()...)
		refusals(op.Responses, c.Refusals)
		op = withKeptHeader(op)
		if err := add(ctx.BasePath+routePath, "post", op); err != nil {
			return nil, err
		}
	}
	for _, q := range ctx.Queries {
		verb, routePath, ok := strings.Cut(q.Route, " ")
		if !ok || verb != "GET" || ctx.QueryStreams[q.Name] {
			// A stream query is served over gRPC only (ST-SUP-RC33-OC08).
			continue
		}
		params := pathParameters(routePath)
		op := openAPIOperation{
			Summary:     "Run the query " + q.Name,
			OperationID: lowerCamel(q.Name),
			Tags:        []string{"queries"},
			Parameters:  stringPathParams(params),
			Responses: map[string]openAPIResponse{
				"400": {Description: "Bad request"},
				"401": {Description: "Unauthorized"},
				"404": {Description: "Not found: the resource is not there, or the caller may not read it"},
				"500": {Description: "Internal error"},
			},
		}
		if q.ResourceMember == "" {
			op.Parameters = append(op.Parameters,
				openAPIParam{Name: "cursor", In: "query", Description: "The cursor of the next page, as the page before gave it", Schema: openAPISchema{Type: "string"}},
				openAPIParam{Name: "limit", In: "query", Description: "The most values of the page, from 1 to 1000", Schema: openAPISchema{Type: "integer", Format: "int32"}})
			op.Responses["200"] = openAPIResponse{Description: "One page of the query", Content: jsonContent(openAPISchema{Type: "object", Required: []string{"items"},
				Properties: map[string]openAPISchema{"items": {Type: "array", Items: &openAPISchema{}}, "next": {Type: "string", Description: "The cursor of the next page; absent on the last page"}}})}
		} else {
			if !slices.Contains(params, q.ResourceMember) {
				op.Parameters = append(op.Parameters, openAPIParam{Name: q.ResourceMember, In: "query", Required: true, Description: "The ID of the resource of the query", Schema: openAPISchema{Type: "string"}})
			}
			// The fill of the query gives the form of its answer, so the schema is any JSON value.
			op.Responses["200"] = openAPIResponse{Description: "The answer of the query", Content: jsonContent(openAPISchema{})}
		}
		op.Parameters = append(op.Parameters, revocationParameters()...)
		refusals(op.Responses, q.Refusals)
		if err := add(ctx.BasePath+routePath, "get", op); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// refusals adds the problem response of each declared refusal to responses, by its status.
func refusals(responses map[string]openAPIResponse, declared []types.Refusal) {
	byStatus := map[string][]string{}
	for _, r := range declared {
		key := strconv.Itoa(r.Status)
		byStatus[key] = append(byStatus[key], r.Code)
	}
	for _, key := range sortedStrings(byStatus) {
		codes := byStatus[key]
		sort.Strings(codes)
		description := "Problem " + strings.Join(codes, ", ")
		if r, ok := responses[key]; ok {
			description = r.Description + "; or problem " + strings.Join(codes, ", ")
		}
		responses[key] = openAPIResponse{Description: description}
	}
}

// sortedStrings gives the keys of m in order.
func sortedStrings[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// revocationParameters are the optional headers by which a command or a query waits for the caller's own write
// (ST-SUP-RC20-OC49, ST-SUP-RC33-OC10).
func revocationParameters() []openAPIParam {
	return []openAPIParam{
		{Name: "Authz-Revocation-After", In: "header", Description: "The Authz-Revocation-Location of the caller's own write, which the request waits for", Schema: openAPISchema{Type: "string"}},
		{Name: "Prefer", In: "header", Description: "wait=N: wait at most N seconds for the revocation (RFC 7240)", Schema: openAPISchema{Type: "string"}},
	}
}

// pathParameters gives the names of the {name} segments of a route path, in order.
func pathParameters(routePath string) []string {
	var names []string
	for _, segment := range strings.Split(routePath, "/") {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			names = append(names, segment[1:len(segment)-1])
		}
	}
	return names
}

// stringPathParams gives a required string path parameter for each name.
func stringPathParams(names []string) []openAPIParam {
	params := make([]openAPIParam, 0, len(names))
	for _, name := range names {
		params = append(params, openAPIParam{Name: name, In: "path", Required: true, Schema: openAPISchema{Type: "string"}})
	}
	return params
}

// lowerCamel gives the operationId of a command or a query name, such as dischargePatient for discharge-patient.
func lowerCamel(name string) string {
	upper := command.GoName(name)
	if upper == "" {
		return upper
	}
	if strings.HasPrefix(upper, "ID") {
		return "id" + upper[2:]
	}
	return strings.ToLower(upper[:1]) + upper[1:]
}

// messageSchema gives the schema of the protobuf JSON form of a message (protojson): each member by its JSON name; a
// 64-bit integer as a string, as protojson writes it; an enum as its value names; bytes as base64; a list as an
// array; a map as an object of its values; a nested message as an object, and a recursive or too deep one as an
// object with no fixed members. A proto3 member is never required. Well-known types have their JSON form.
func messageSchema(m protoreflect.MessageDescriptor, depth int, seen map[protoreflect.FullName]bool) openAPISchema {
	switch m.FullName() {
	case "google.protobuf.Timestamp":
		return openAPISchema{Type: "string", Format: "date-time"}
	case "google.protobuf.Duration":
		return openAPISchema{Type: "string", Description: "A duration in seconds with the suffix s, such as 1.5s"}
	case "google.protobuf.Empty":
		return openAPISchema{Type: "object"}
	case "google.protobuf.StringValue":
		return openAPISchema{Type: "string"}
	case "google.protobuf.BoolValue":
		return openAPISchema{Type: "boolean"}
	case "google.protobuf.Int32Value", "google.protobuf.UInt32Value":
		return openAPISchema{Type: "integer"}
	case "google.protobuf.Int64Value", "google.protobuf.UInt64Value":
		return openAPISchema{Type: "string", Format: "int64"}
	case "google.protobuf.FloatValue", "google.protobuf.DoubleValue":
		return openAPISchema{Type: "number"}
	}
	if m.FullName().Parent() == "google.protobuf" || seen[m.FullName()] || depth >= maxMessageDepth {
		return openAPISchema{Type: "object"}
	}
	seen[m.FullName()] = true
	defer delete(seen, m.FullName())
	schema := openAPISchema{Type: "object", Properties: map[string]openAPISchema{}}
	fields := m.Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		schema.Properties[f.JSONName()] = fieldSchema(f, depth, seen)
	}
	return schema
}

// responseSchema gives the schema of the response message of a command. When the message holds a member that the
// proto marks debug_redact at any depth, also deeper than the schema follows or past a recursive message, the root
// carries x-sensitive, so the CLI still makes the command Sensitive (ST-SUP-RC20-OC54, security review of PR #460,
// L1). When the message holds a google.protobuf.Any, Struct, Value or ListValue member at any depth, the root carries
// x-stego-opaque, so the CLI makes the command Sensitive too; the service needs no key ring for it (decision
// ST-SUP-Q194 (a)).
func responseSchema(m protoreflect.MessageDescriptor) openAPISchema {
	schema := messageSchema(m, 0, map[protoreflect.FullName]bool{})
	schema.Sensitive = command.HoldsSecret(m)
	schema.Opaque = command.HoldsOpaque(m)
	return schema
}

// fieldSchema gives the schema of one member of a message in its protobuf JSON form. A member that the proto file
// marks debug_redact is a secret: x-sensitive, and the format password for a string (ST-SUP-RC20-OC54).
func fieldSchema(f protoreflect.FieldDescriptor, depth int, seen map[protoreflect.FullName]bool) openAPISchema {
	var schema openAPISchema
	switch {
	case f.IsMap():
		value := singleSchema(f.MapValue(), depth, seen)
		schema = openAPISchema{Type: "object", AdditionalProperties: &value}
	case f.IsList():
		item := singleSchema(f, depth, seen)
		schema = openAPISchema{Type: "array", Items: &item}
	default:
		schema = singleSchema(f, depth, seen)
	}
	if command.SecretMember(f) {
		schema.Sensitive = true
		if schema.Type == "string" && schema.Format == "" {
			schema.Format = "password"
		}
	}
	return schema
}

// writeOnlySecrets marks each secret member of a request body writeOnly, at any depth: a request member is never in
// an answer (ST-SUP-RC20-OC54).
func writeOnlySecrets(s *openAPISchema) {
	if s.Sensitive {
		s.WriteOnly = true
	}
	for name, p := range s.Properties {
		writeOnlySecrets(&p)
		s.Properties[name] = p
	}
	if s.Items != nil {
		writeOnlySecrets(s.Items)
	}
	if s.AdditionalProperties != nil {
		writeOnlySecrets(s.AdditionalProperties)
	}
}

// singleSchema gives the schema of one value of the kind of f.
func singleSchema(f protoreflect.FieldDescriptor, depth int, seen map[protoreflect.FullName]bool) openAPISchema {
	switch f.Kind() {
	case protoreflect.BoolKind:
		return openAPISchema{Type: "boolean"}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return openAPISchema{Type: "integer", Format: "int32"}
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return openAPISchema{Type: "integer", Format: "int64"}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return openAPISchema{Type: "string", Format: "int64"}
	case protoreflect.FloatKind:
		return openAPISchema{Type: "number", Format: "float"}
	case protoreflect.DoubleKind:
		return openAPISchema{Type: "number", Format: "double"}
	case protoreflect.StringKind:
		return openAPISchema{Type: "string"}
	case protoreflect.BytesKind:
		return openAPISchema{Type: "string", Format: "byte"}
	case protoreflect.EnumKind:
		values := f.Enum().Values()
		names := make([]string, 0, values.Len())
		for i := 0; i < values.Len(); i++ {
			names = append(names, string(values.Get(i).Name()))
		}
		return openAPISchema{Type: "string", Enum: names}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return messageSchema(f.Message(), depth+1, seen)
	}
	return openAPISchema{}
}

// mergeRoutes adds the command and query operations to the paths of spec, and refuses a path and method that a
// collection already serves, or an operationId that another operation has.
func mergeRoutes(spec *openAPISpec, routes map[string]map[string]openAPIOperation) error {
	ids := map[string]string{}
	for p, item := range spec.Paths {
		for m, op := range item.Operations {
			ids[op.OperationID] = strings.ToUpper(m) + " " + p
		}
	}
	for _, p := range sortedStrings(routes) {
		item := spec.Paths[p]
		if item.Operations == nil {
			item.Operations = map[string]openAPIOperation{}
		}
		for _, m := range sortedStrings(routes[p]) {
			op := routes[p][m]
			if _, clash := item.Operations[m]; clash {
				return fmt.Errorf("the route %s %s of a command or a query is also a collection route", strings.ToUpper(m), p)
			}
			if other, clash := ids[op.OperationID]; clash {
				return fmt.Errorf("the operationId %s of %s %s is also the operationId of %s", op.OperationID, strings.ToUpper(m), p, other)
			}
			ids[op.OperationID] = strings.ToUpper(m) + " " + p
			item.Operations[m] = op
		}
		spec.Paths[p] = item
	}
	return nil
}
