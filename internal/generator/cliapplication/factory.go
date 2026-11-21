package cliapplication

import (
	"bytes"
	"context"
	"fmt"
	"go/format"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jsell-rh/stego/internal/gen"
)

// factoryNamespace is the folder under the CLI namespace of the definitions that the generator makes from the
// service's OpenAPI document when the declaration names no factory_package (ST-SUP-RC04-OC32).
const factoryNamespace = "app"

// The forms that the command runtime accepts (runtime.go.tmpl): a command word or a flag, and a request member.
var (
	cliWord     = regexp.MustCompile(`^[a-z][a-zA-Z0-9-]{0,63}$`)
	cliProperty = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,63}$`)
	cliPathPart = regexp.MustCompile(`^[a-zA-Z0-9._~!$&'()*+,;=:@-]+$`)
)

// The limits of the command runtime: commands of an application, fields of a command and apply resources.
const (
	maxCommands  = 128
	maxFields    = 64
	maxResources = 64
)

// factoryCommand is one command of the generated definitions, in the form of command.Command.
type factoryCommand struct {
	Name                                 string
	Method, Path                         string
	ID, Query, IdempotentWrites, Confirm bool
	PathParameters                       [][2]string
	Fields                               []factoryField
	Success, EmptyResponses              []int
	// BodyFields are the secret members, which have no flag and which only --body FILE gives (ST-SUP-RC04-OC32).
	BodyFields []factoryField
	// Sensitive tells that a success answer holds a secret, so the CLI writes it only to --output-file
	// (ST-SUP-RC04-OC33).
	Sensitive bool
}

// factoryField is one request member of a command, in the form of command.Field.
type factoryField struct {
	Flag, Key, Type string
	Required        bool
}

// factoryResource is one apply resource, in the form of command.ApplyResource.
type factoryResource struct {
	Kind, APIVersion, Path    string
	CreateFields, PatchFields []factoryField
	IdempotentWrites          bool
	// ConditionalPatch states that the patch operation takes the header If-Match (ST-SUP-RC04-OC38).
	ConditionalPatch bool
}

// factorySource gives the Go source of the package app of the CLI: Commands, which returns the definitions of the
// commands and the apply resources that the service's OpenAPI document gives (ST-SUP-RC04-OC32). Each operation is
// one command named by the words of its operationId, such as discharge-patient for dischargePatient; each path
// parameter is a required flag, {id} the flag --id; each member of a JSON body or query parameter of type string,
// integer, boolean or a list of strings is a flag, and the other members go in --body FILE. A collection with a create,
// a list and a patch at a path with no parameter is an apply resource, of the kind of the schema of its create request. An
// operation with more members than the runtime takes, and the document after the most commands, are left out. A
// write states IdempotentWrites when its operation carries the Idempotency-Key header parameter, as
// command.IdempotentWritesFrom sets it (ST-SUP-RC04-OC31): a command, and a create and a patch of a service with the
// idempotency component.
func factorySource(ctx gen.Context, runtimeImport string) ([]byte, error) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(ctx.ServiceOpenAPI)
	if err != nil {
		return nil, fmt.Errorf("CLI: the OpenAPI document of the service does not load: %w", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("CLI: the OpenAPI document of the service is not valid: %w", err)
	}
	var commands []factoryCommand
	var resources []factoryResource
	paths := doc.Paths.InMatchingOrder()
	sort.Strings(paths)
	for _, p := range paths {
		item := doc.Paths.Value(p)
		methods := make([]string, 0, 5)
		for method := range item.Operations() {
			methods = append(methods, method)
		}
		sort.Strings(methods)
		for _, method := range methods {
			op := item.GetOperation(method)
			c, ok := operationCommand(p, method, op)
			c.IdempotentWrites = (method == "POST" || method == "PATCH") && keyed(item, op)
			if ok && len(commands) < maxCommands {
				commands = append(commands, c)
			}
		}
		if r, ok := applyResource(doc, p, item, ctx.ServiceName); ok && len(resources) < maxResources {
			resources = append(resources, r)
		}
	}
	if len(commands) == 0 && len(resources) == 0 {
		return nil, fmt.Errorf("CLI: the OpenAPI document of the service gives no operation that the CLI can call")
	}
	configName := ctx.ServiceName
	if !cliWord.MatchString(configName) {
		configName = "apply"
	}
	var b bytes.Buffer
	b.WriteString("// Package app holds the definitions of the commands of the CLI, made from the OpenAPI document of the service\n")
	b.WriteString("// (ST-SUP-RC04-OC32).\npackage app\n\n")
	fmt.Fprintf(&b, "import command %q\n\n", runtimeImport)
	b.WriteString("// Commands gives the definitions of the commands and the apply resources of the CLI.\n")
	b.WriteString("func Commands() command.Application {\n\treturn command.Application{\n")
	fmt.Fprintf(&b, "\t\tConfigEnv: %q,\n\t\tConfigName: %q,\n\t\tVersionCommand: true,\n", "APPLY_CONFIG", configName)
	b.WriteString("\t\tCommands: []command.Command{\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "\t\t\t{Name: []string{%q}, Method: %q, Path: %q, ID: %t, Query: %t, IdempotentWrites: %t, Confirm: %t,", c.Name, c.Method, c.Path, c.ID, c.Query, c.IdempotentWrites, c.Confirm)
		if c.Sensitive {
			b.WriteString(" Sensitive: true,")
		}
		if len(c.PathParameters) > 0 {
			b.WriteString(" PathParameters: []command.PathParameter{")
			for _, p := range c.PathParameters {
				fmt.Fprintf(&b, "{Flag: %q, Key: %q}, ", p[0], p[1])
			}
			b.WriteString("},")
		}
		writeFields(&b, "Fields", c.Fields)
		writeFields(&b, "BodyFields", c.BodyFields)
		fmt.Fprintf(&b, " Success: %s,", intList(c.Success))
		if len(c.EmptyResponses) > 0 {
			fmt.Fprintf(&b, " EmptyResponses: %s,", intList(c.EmptyResponses))
		}
		b.WriteString("},\n")
	}
	b.WriteString("\t\t},\n")
	if len(resources) > 0 {
		b.WriteString("\t\tResources: []command.ApplyResource{\n")
		for _, r := range resources {
			fmt.Fprintf(&b, "\t\t\t{Kind: %q, APIVersion: %q, Path: %q, IdempotentWrites: %t,", r.Kind, r.APIVersion, r.Path, r.IdempotentWrites)
			if r.ConditionalPatch {
				b.WriteString(" ConditionalPatch: true,")
			}
			writeFields(&b, "CreateFields", r.CreateFields)
			writeFields(&b, "PatchFields", r.PatchFields)
			b.WriteString("},\n")
		}
		b.WriteString("\t\t},\n")
	}
	b.WriteString("\t}\n}\n")
	return format.Source(b.Bytes())
}

// writeFields writes a list of fields of the name name, or nothing for no field.
func writeFields(b *bytes.Buffer, name string, fields []factoryField) {
	if len(fields) == 0 {
		return
	}
	fmt.Fprintf(b, " %s: []command.Field{", name)
	for _, f := range fields {
		fmt.Fprintf(b, "{Flag: %q, Key: %q, Type: %q, Required: %t}, ", f.Flag, f.Key, f.Type, f.Required)
	}
	b.WriteString("},")
}

// intList writes a list of status codes as Go source.
func intList(codes []int) string {
	parts := make([]string, len(codes))
	for i, c := range codes {
		parts[i] = strconv.Itoa(c)
	}
	return "[]int{" + strings.Join(parts, ", ") + "}"
}

// operationCommand gives the command of one operation, and false for an operation that the command runtime cannot
// call: a name, a route or a member that it does not take, or more members than it takes.
func operationCommand(p, method string, op *openapi3.Operation) (factoryCommand, bool) {
	// A delete asks for --yes (security review of PR #454, L1).
	c := factoryCommand{Name: commandName(op.OperationID), Method: method, Path: p, Confirm: method == "DELETE"}
	if !cliWord.MatchString(c.Name) || reservedName(c.Name) {
		return c, false
	}
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
	default:
		return c, false
	}
	flags := map[string]bool{}
	for _, ref := range op.Parameters {
		param := ref.Value
		if param == nil {
			return c, false
		}
		switch param.In {
		case "path":
			if param.Name == "id" {
				c.ID = true
				continue
			}
			flag := flagName(param.Name)
			if !cliProperty.MatchString(param.Name) || !cliWord.MatchString(flag) || flags[flag] {
				return c, false
			}
			flags[flag] = true
			c.PathParameters = append(c.PathParameters, [2]string{flag, param.Name})
		case "query":
			t, ok := fieldType(param.Schema)
			flag := flagName(param.Name)
			if !ok || !cliProperty.MatchString(param.Name) || !cliWord.MatchString(flag) || flags[flag] || reservedFlag(flag) {
				continue
			}
			flags[flag] = true
			c.Query = true
			c.Fields = append(c.Fields, factoryField{Flag: flag, Key: param.Name, Type: t, Required: param.Required})
		}
	}
	for _, part := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			continue
		}
		if part == "." || part == ".." || !cliPathPart.MatchString(part) {
			return c, false
		}
	}
	if op.RequestBody != nil && op.RequestBody.Value != nil {
		if c.Query {
			return c, false
		}
		if media := op.RequestBody.Value.Content.Get("application/json"); media != nil && media.Schema != nil && media.Schema.Value != nil {
			c.Fields = append(c.Fields, bodyFields(media.Schema.Value, flags)...)
			c.BodyFields = secretFields(media.Schema.Value)
		}
	}
	if len(c.Fields)+len(c.BodyFields) > maxFields {
		return c, false
	}
	for code, ref := range op.Responses.Map() {
		n, err := strconv.Atoi(code)
		if err != nil || n < 200 || n > 299 {
			continue
		}
		c.Success = append(c.Success, n)
		if ref.Value != nil && len(ref.Value.Content) == 0 {
			c.EmptyResponses = append(c.EmptyResponses, n)
		}
		if ref.Value != nil {
			for _, media := range ref.Value.Content {
				c.Sensitive = c.Sensitive || (media != nil && holdsSecret(media.Schema, map[*openapi3.Schema]bool{}))
			}
		}
	}
	sort.Ints(c.Success)
	sort.Ints(c.EmptyResponses)
	if len(c.Success) == 0 || len(c.Success) > 4 {
		return c, false
	}
	return c, true
}

// bodyFields gives a flag for each member of a body schema that the runtime takes, leaving out each member whose
// flag another field already has, a readOnly member, a secret member (writeOnly or x-sensitive) and a member of
// another type, which --body FILE gives.
func bodyFields(schema *openapi3.Schema, flags map[string]bool) []factoryField {
	required := map[string]bool{}
	for _, name := range schema.Required {
		required[name] = true
	}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	var fields []factoryField
	for _, name := range names {
		ref := schema.Properties[name]
		// A secret member, writeOnly or x-sensitive, goes only in --body FILE: a flag would put it in the shell history
		// and the process list (security review of PR #454, M1).
		if ref == nil || ref.Value == nil || ref.Value.ReadOnly || ref.Value.WriteOnly || ref.Value.Extensions["x-sensitive"] == true {
			continue
		}
		// The member kind of one value is the name of the entity, which a body may give and the CLI never needs; an
		// x-kept member is one that a write cannot change, and an x-output-only member one that the server sets
		// (ST-SUP-RC20-OC15, ST-SUP-RC20-OC46, ST-SUP-RC20-OC55).
		if (name == "kind" && len(ref.Value.Enum) == 1) || ref.Value.Extensions["x-kept"] == true || ref.Value.Extensions["x-output-only"] == true {
			continue
		}
		t, ok := fieldType(ref)
		flag := flagName(name)
		if !ok || !cliProperty.MatchString(name) || !cliWord.MatchString(flag) || flags[flag] || reservedFlag(flag) {
			continue
		}
		flags[flag] = true
		fields = append(fields, factoryField{Flag: flag, Key: name, Type: t, Required: required[name]})
	}
	return fields
}

// secretFields gives a body field, with no flag, for each secret member of a body schema (writeOnly or
// x-sensitive) of a type that the runtime takes, so --body FILE can give it while no flag takes it.
func secretFields(schema *openapi3.Schema) []factoryField {
	required := map[string]bool{}
	for _, name := range schema.Required {
		required[name] = true
	}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	var fields []factoryField
	for _, name := range names {
		ref := schema.Properties[name]
		if ref == nil || ref.Value == nil || ref.Value.ReadOnly || !(ref.Value.WriteOnly || ref.Value.Extensions["x-sensitive"] == true) {
			continue
		}
		if t, ok := fieldType(ref); ok && cliProperty.MatchString(name) {
			fields = append(fields, factoryField{Key: name, Type: t, Required: required[name]})
		}
	}
	return fields
}

// holdsSecret tells whether a schema, or a schema of a member, an item, a map value or a part of it at any depth, is
// marked x-sensitive, or x-stego-opaque for an answer whose content the document cannot describe, which can hold a
// secret (ST-SUP-RC04-OC33, decision ST-SUP-Q194 (a)).
func holdsSecret(ref *openapi3.SchemaRef, seen map[*openapi3.Schema]bool) bool {
	if ref == nil || ref.Value == nil || seen[ref.Value] {
		return false
	}
	s := ref.Value
	seen[s] = true
	if s.Extensions["x-sensitive"] == true || s.Extensions["x-stego-opaque"] == true {
		return true
	}
	parts := []*openapi3.SchemaRef{s.Items, s.Not, s.AdditionalProperties.Schema}
	for _, p := range s.Properties {
		parts = append(parts, p)
	}
	for _, list := range []openapi3.SchemaRefs{s.AllOf, s.AnyOf, s.OneOf} {
		parts = append(parts, list...)
	}
	for _, p := range parts {
		if holdsSecret(p, seen) {
			return true
		}
	}
	return false
}

// fieldType gives the type of a field of the runtime for a schema: string, integer, boolean or string-list.
func fieldType(ref *openapi3.SchemaRef) (string, bool) {
	if ref == nil || ref.Value == nil || ref.Value.Type == nil {
		return "", false
	}
	s := ref.Value
	switch {
	case s.Type.Is("string"):
		return "string", true
	case s.Type.Is("integer"):
		return "integer", true
	case s.Type.Is("boolean"):
		return "boolean", true
	case s.Type.Is("array") && s.Items != nil && s.Items.Value != nil && s.Items.Value.Type != nil && s.Items.Value.Type.Is("string"):
		return "string-list", true
	}
	return "", false
}

// applyResource gives the apply resource of a collection path: a path with no parameter, with a create that takes
// a schema of the components and a list, and a patch at <path>/{id}; both take a string name. Its kind is the name of
// the schema of the create without CreateRequest.
func applyResource(doc *openapi3.T, p string, item *openapi3.PathItem, service string) (factoryResource, bool) {
	r := factoryResource{Path: p, APIVersion: service + "/v1"}
	if strings.Contains(p, "{") || item.Post == nil || item.Get == nil || item.Post.RequestBody == nil || item.Post.RequestBody.Value == nil {
		return r, false
	}
	media := item.Post.RequestBody.Value.Content.Get("application/json")
	if media == nil || media.Schema == nil || media.Schema.Value == nil {
		return r, false
	}
	kind, ok := strings.CutSuffix(strings.TrimPrefix(media.Schema.Ref, "#/components/schemas/"), "CreateRequest")
	if !ok || !cliProperty.MatchString(kind) {
		return r, false
	}
	r.Kind = kind
	// A secret member, writeOnly or x-sensitive, is a field with no flag, which only a private apply file gives, so
	// apply can give it at create and in a patch, as --body FILE gives it to a command (ST-SUP-RC04-OC32).
	r.CreateFields = append(bodyFields(media.Schema.Value, map[string]bool{}), secretFields(media.Schema.Value)...)
	if !slicesContainsKey(r.CreateFields, "name") {
		return r, false
	}
	one := doc.Paths.Value(p + "/{id}")
	if one != nil && one.Patch != nil && one.Patch.RequestBody != nil && one.Patch.RequestBody.Value != nil {
		if patch := one.Patch.RequestBody.Value.Content.Get("application/json"); patch != nil && patch.Schema != nil && patch.Schema.Value != nil {
			r.PatchFields = append(bodyFields(patch.Schema.Value, map[string]bool{}), secretFields(patch.Schema.Value)...)
		}
	}
	r.IdempotentWrites = keyed(item, item.Post) && one != nil && one.Patch != nil && keyed(one, one.Patch)
	r.ConditionalPatch = one != nil && one.Patch != nil && headerParameter(one, one.Patch, "If-Match")
	// The runtime needs the name among the fields of the create and of the patch, so a collection with no patch
	// that takes the name is no apply resource: apply cannot change a resource that it finds.
	if !slicesContainsKey(r.PatchFields, "name") || len(r.CreateFields) > maxFields || len(r.PatchFields) > maxFields {
		return r, false
	}
	return r, true
}

// keyed tells whether an operation, with the parameters of its path item, carries the Idempotency-Key header
// parameter. A parameter given by $ref is not followed, as command.IdempotentWritesFrom does not follow it.
func keyed(item *openapi3.PathItem, op *openapi3.Operation) bool {
	return headerParameter(item, op, "Idempotency-Key")
}

// headerParameter tells whether an operation, with the parameters of its path item, carries the header parameter
// name. A parameter given by $ref is not followed.
func headerParameter(item *openapi3.PathItem, op *openapi3.Operation, name string) bool {
	for _, params := range []openapi3.Parameters{item.Parameters, op.Parameters} {
		for _, ref := range params {
			if ref != nil && ref.Ref == "" && ref.Value != nil && ref.Value.In == "header" && strings.EqualFold(ref.Value.Name, name) {
				return true
			}
		}
	}
	return false
}

// slicesContainsKey tells whether one of the fields is a string field of the key.
func slicesContainsKey(fields []factoryField, key string) bool {
	for _, f := range fields {
		if f.Key == key && f.Type == "string" {
			return true
		}
	}
	return false
}

// commandName gives the command word of an operationId: its words in lower case, joined with hyphens, such as
// discharge-patient for dischargePatient and read-clinic-patients for readClinicPatients.
func commandName(operationID string) string {
	var b strings.Builder
	runes := []rune(operationID)
	for i, r := range runes {
		if unicode.IsUpper(r) {
			if i > 0 && (unicode.IsLower(runes[i-1]) || (i+1 < len(runes) && unicode.IsLower(runes[i+1]))) {
				b.WriteByte('-')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		if r == '_' {
			r = '-'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// flagName gives the flag of a member or a parameter: its name in lower case with hyphens, such as clinic-id for
// clinic_id and page-size for pageSize.
func flagName(name string) string { return commandName(name) }

// reservedName tells whether a command name is one that the runtime keeps for itself.
func reservedName(name string) bool {
	switch name {
	case "apply", "login", "logout", "help", "version", "whoami":
		return true
	}
	return false
}

// reservedFlag tells whether a flag is one that the runtime keeps for itself.
func reservedFlag(flag string) bool {
	switch flag {
	case "body", "yes", "help", "output-file", "no-browser", "id":
		return true
	}
	return false
}
