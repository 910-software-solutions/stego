package command

import (
	"bytes"
	"fmt"
	"go/format"
	"go/token"
	"slices"
	"strings"

	"github.com/jsell-rh/stego/internal/types"
)

// probeType is the typed probe of a command: the domain probe of the command when the service has the
// domain-probe component (ST-SUP-RC34-OC04), and struct{} otherwise.
func (m *model) probeType(c *command) string {
	if m.probeImport == "" {
		return "struct{}"
	}
	return "probes." + c.goName + "Probe"
}

// usesTime tells whether an entity field is a timestamp.
func (m *model) usesTime() bool {
	for _, e := range m.entities {
		for _, f := range e.fields {
			if strings.Contains(f.goType, "time.Time") {
				return true
			}
		}
	}
	return false
}

func (m *model) usesBytes() bool {
	for _, e := range m.entities {
		for _, f := range e.fields {
			if f.slice {
				return true
			}
		}
	}
	return false
}

// usesJSON tells whether an entity has a jsonb field, whose audit state is its JSON value.
func (m *model) usesJSON() bool {
	for _, e := range m.entities {
		for _, f := range e.fields {
			if f.decl.Type == types.FieldTypeJsonb {
				return true
			}
		}
	}
	return false
}

func (m *model) usesAudit() bool {
	for _, e := range m.entities {
		for _, f := range e.fields {
			if f.sensitivity == "pii" || f.sensitivity == "secret" {
				return true
			}
		}
	}
	return false
}

// renderContract writes the command slot contract package, which a command fill imports (ST-SUP-RC32-OC01): the
// entity types and their repositories, the command handles, the fill types and the typed refusals. The request and
// the response messages are in the same package, in the files that protoc-gen-go writes.
func renderContract(m *model) []byte {
	w := &writer{}
	w.f("// Package %s is the command slot contract of the service (ST-SUP-RC32-OC01): for each declared command, the fill\n", m.pkg)
	w.f("// type, which takes the decoded request message and one command handle only, the command handle, the typed\n")
	w.f("// refusals and the repositories of the entities that the command writes. A command fill imports this package\n")
	w.f("// only; the handlers that call the fills are in the package %s.\n", m.handlersImport)
	w.f("package %s\n\nimport (\n", m.pkg)
	if m.usesBytes() {
		w.f("\t\"bytes\"\n")
	}
	w.f("\t\"context\"\n")
	if m.usesJSON() {
		w.f("\t\"encoding/json\"\n")
	}
	if m.usesTime() {
		w.f("\t\"time\"\n")
	}
	w.f("\n")
	if m.usesAudit() {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/audit\"\n")
	}
	if m.hasRelations() || m.hasServiceLevel() {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/authz\"\n")
	}
	w.f("\t\"github.com/jsell-rh/stego/registry/runtime/command\"\n")
	if m.hasServiceLevel() {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/tenant\"\n")
	}
	if m.probeImport != "" {
		w.f("\n\tprobes %q\n", m.probeImport)
	}
	w.f(")\n\n")
	for _, e := range m.entities {
		renderEntity(w, e, m.tenantKey)
	}
	for _, c := range m.commands {
		if c.service != "" {
			renderServiceLevelHandle(w, m, c)
		} else {
			w.f("// %sHandle is the command handle that the fill of %s gets: it reaches only the entities, relations and\n// effect destinations that the command declares.\n", c.goName, c.decl.Name)
			w.f("type %sHandle = command.Handle[%s]\n\n", c.goName, m.probeType(c))
		}
		w.f("// %sFill is the command slot of %s: the fill gets the decoded request message and one command handle, and\n", c.goName, c.decl.Name)
		w.f("// nothing else that reaches storage, authorization, audit, the outbox or the network (ST-SUP-RC32-OC01). It\n// returns the response message, or an error: one of the declared refusals of the command, or any other error,\n// which the handler answers with 500 and the code internal.\n")
		w.f("type %sFill func(ctx context.Context, request *%s, h *%sHandle) (*%s, error)\n\n", c.goName, c.request.name, c.goName, c.response.name)
		for _, r := range c.refusals {
			w.f("// %s is the refusal %s of %s: the handler answers it with %d and the code %s.\n", r.goName, r.name, c.decl.Name, r.status, r.code)
			w.f("var %s = command.NewRefusal(%q, %q, %d, %q)\n\n", r.goName, c.decl.Name, r.name, r.status, r.code)
		}
		for _, h := range c.helpers {
			renderRelationHelper(w, c, h)
		}
		names := make([]string, len(c.lists))
		for i, l := range c.lists {
			RenderList(&w.Buffer, l.goName, l.decl.Name, "the command "+c.decl.Name, l.decl.Entity, l.plan)
			names[i] = l.goName
		}
		RenderDeclaredLists(&w.Buffer, c.goName, "the command "+c.decl.Name, names)
	}
	w.f("%s", refusalHelpers)
	return w.Bytes()
}

// renderServiceLevelHandle writes the command handle of a command on the service-level resource
// (ST-SUP-RC32-OC15): Run decides its permission on that resource and on no row, so the handle offers for each
// written entity only the create and the read, with no locked read, update or delete, and the fill cannot change a
// row that the decision does not cover. A create still decides each reference of the new row (ST-SUP-RP10-LC05).
func renderServiceLevelHandle(w *writer, m *model, c *command) {
	probe := m.probeType(c)
	w.f("// %sHandle is the command handle that the fill of %s gets. The command is decided on the service-level\n", c.goName, c.decl.Name)
	w.f("// resource %s:%s in the tenant of the request, so the handle offers only the create and the read of each entity\n", c.resource.resourceType, c.service)
	w.f("// that the command writes, and the effect destinations that it declares (ST-SUP-RC32-OC15).\n")
	w.f("type %sHandle struct{ h *command.Handle[%s] }\n\n", c.goName, probe)
	w.f("// New%sHandle gives the handle of %s over the command handle of its run.\n", c.goName, c.decl.Name)
	w.f("func New%sHandle(h *command.Handle[%s]) *%sHandle { return &%sHandle{h: h} }\n\n", c.goName, probe, c.goName, c.goName)
	w.f("// Tenant is the tenant of the request.\nfunc (h *%sHandle) Tenant() tenant.Tenant { return h.h.Tenant() }\n\n", c.goName)
	w.f("// Principal is the principal of the request.\nfunc (h *%sHandle) Principal() authz.Principal { return h.h.Principal() }\n\n", c.goName)
	w.f("// Probe is the domain probe of the command.\nfunc (h *%sHandle) Probe() %s { return h.h.Probe() }\n\n", c.goName, probe)
	w.f("// Effects stages the declared effects of the command.\nfunc (h *%sHandle) Effects() *command.Effects { return h.h.Effects() }\n\n", c.goName)
	for _, e := range c.writes {
		w.f("// Create%s creates a row of %s. parents are the loaded values of the rows that its references name, each\n// decided with the permission that the declaration names for the reference (ST-SUP-RP10-LC05).\n", e.goName, e.decl.Name)
		w.f("func (h *%sHandle) Create%s(ctx context.Context, value %s, parents ...command.Ref) (Loaded%s, error) {\n", c.goName, e.goName, e.goName, e.goName)
		w.f("\tr, err := %sRepository(h.h)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\treturn r.Create(ctx, value, parents...)\n}\n\n", e.goName)
		w.f("// Get%s reads the row of %s with the ID id, which the read permission of the entity decides.\n", e.goName, e.decl.Name)
		w.f("func (h *%sHandle) Get%s(ctx context.Context, id string) (Loaded%s, error) {\n", c.goName, e.goName, e.goName)
		w.f("\tr, err := %sRepository(h.h)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\treturn r.Get(ctx, id)\n}\n\n", e.goName)
	}
}

// renderRelationHelper writes the typed helpers of one relation that the command declares (ST-SUP-RC32-OC13): a
// fill relates or unrelates a subject to the row of a loaded value through them, and never builds a resource ID or
// names a relation that the command does not declare. The tuple's resource is the tenant-qualified resource of the
// row in the handle's tenant (ST-SUP-RC29-OC22); the command runtime decides the command's permission on the row
// first, and the relay writes the tuple after the commit, in the revocation of the transaction (ST-SUP-RP10-LC05).
func renderRelationHelper(w *writer, c *command, h relationHelper) {
	for _, op := range []struct{ name, verb, call string }{{h.relate, "relates", "Relate"}, {h.unrelate, "unrelates", "Unrelate"}} {
		w.f("// %s %s the subject subjectType:subjectID to the row of on through the relation %s#%s that %s declares:\n", op.name, op.verb, h.resourceType, h.relation, c.decl.Name)
		w.f("// it enqueues the tuple for the relay in the command's transaction. on is a loaded %s of this handle.\n", h.entity.decl.Name)
		w.f("func %s(ctx context.Context, h *%sHandle, on Loaded%s, subjectType, subjectID string) error {\n", op.name, c.goName, h.entity.goName)
		w.f("\tif on == nil {\n\t\treturn command.ErrNoResource\n\t}\n")
		w.f("\tresource, err := authz.NewResource(h.Tenant(), %q, on.ID())\n\tif err != nil {\n\t\treturn err\n\t}\n", h.resourceType)
		w.f("\t_, err = h.Authz().%s(ctx, on, authz.Tuple{Resource: resource, Relation: %q, Subject: authz.Subject{Type: subjectType, ID: subjectID}})\n\treturn err\n}\n\n", op.call, h.relation)
	}
}

// EntityFile gives a Go file of the package pkg that holds the repository declarations on the command runtime of the
// named entities, as the command contract holds them (ST-SUP-RP10-LC02): for each entity its row type, its loaded
// type, its command.Entity declaration with the statements of ST-SUP-RP10-LC23 and ST-SUP-RP10-LC24, and its
// repository accessor, with the helpers that they use. Each statement takes the tenant column tenantKey as $1. It is
// for a generator that declares commands of its own, such as the rest-api generator for generated CRUD
// (ST-SUP-RC20-OC37), so the entity code has one source. It refuses an entity that a command cannot write.
func EntityFile(pkg string, names []string, entities []types.Entity, tenantKey string, relations Relations) ([]byte, error) {
	byName := map[string]types.Entity{}
	for _, e := range entities {
		byName[e.Name] = e
	}
	m := &model{tenantKey: tenantKey}
	for _, name := range names {
		e, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("command: the entity %s is not declared", name)
		}
		p, err := planEntity(e, byName, tenantKey, relations)
		if err != nil {
			return nil, fmt.Errorf("command: the entity %s: %w", name, err)
		}
		m.entities = append(m.entities, p)
	}
	w := &writer{}
	w.f("package %s\n\nimport (\n", pkg)
	if m.usesBytes() {
		w.f("\t\"bytes\"\n")
	}
	if m.usesJSON() {
		w.f("\t\"encoding/json\"\n")
	}
	if m.usesTime() {
		w.f("\t\"time\"\n")
	}
	w.f("\n")
	if m.usesAudit() {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/audit\"\n")
	}
	w.f("\t\"github.com/jsell-rh/stego/registry/runtime/command\"\n)\n\n")
	for _, e := range m.entities {
		renderEntity(w, e, tenantKey)
	}
	source, err := format.Source(w.Bytes())
	if err != nil {
		return nil, fmt.Errorf("command: formatting the entity declarations: %w", err)
	}
	return source, nil
}

// refusalHelpers name the refusal type of the runtime in the contract package (ST-SUP-RP10-LC33): a fill returns
// one of the declared values, which Serve knows by identity.
const refusalHelpers = `// Refusal is a declared refusal of a command (ST-SUP-RC32-OC09): the runtime's type. Only the values of this
// package are declared: a fill returns one of them, and the handler answers it with its status and code.
type Refusal = command.Refusal
`

// renderHandlers writes the handlers package: the command declarations with their permission and resource, the
// entities of the runtime, and one ServeSpec for each command, which command.Serve and command.ServeRPC serve
// (ST-SUP-RP10-LC33). Only what varies with the declaration is emitted (ST-STD-RUNTIME-R05).
func renderHandlers(m *model) []byte {
	w := &writer{}
	w.f("// Package %s holds the generated handlers of the commands of the service (ST-SUP-RC32-OC02): one HTTP handler\n", handlersFolder)
	w.f("// and one gRPC handler for each declared command, each bound to the one fill that the command names, served by\n// command.Serve and command.ServeRPC of the runtime library.\n")
	w.f("package %s\n\nimport (\n\t\"context\"\n\t\"net/http\"\n\t\"time\"\n\n", handlersFolder)
	if m.hasRelations() {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/authz\"\n")
	}
	w.f("\t\"github.com/jsell-rh/stego/registry/runtime/command\"\n")
	if m.probeImport == "" {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/probe\"\n")
	}
	w.f("\n\tcommands %q\n", m.contractImport)
	if m.probeImport != "" {
		w.f("\tprobes %q\n", m.probeImport)
	}
	w.f(")\n\n")
	w.f("// subjectType is the subject type of the schema that a verified identity maps to.\nconst subjectType = %q\n\n", m.subjectType)
	w.f("// Options are the settings of the command handlers.\ntype Options struct {\n\t// Retention is how long a completed idempotency key is kept; zero is the runtime's default.\n\tRetention time.Duration\n}\n\n")
	w.f("// Entities are the entities that the commands write, for the Entities of the command runtime's Config.\nfunc Entities() []command.Declared {\n\treturn []command.Declared{")
	for i, e := range m.entities {
		if i > 0 {
			w.f(", ")
		}
		w.f("commands.%sEntity", e.goName)
	}
	w.f("}\n}\n\n")
	w.f("// Fills holds the one fill of each command.\ntype Fills struct {\n")
	for _, c := range m.commands {
		w.f("\t// %s is the fill %s of the command %s.\n\t%s commands.%sFill\n", c.goName, c.decl.Fill[0], c.decl.Name, c.goName, c.goName)
	}
	w.f("}\n\n")
	w.f("// Register registers the HTTP handler of each command on mux at the command's route.\nfunc Register(mux *http.ServeMux, rt *command.Runtime, fills Fills, options Options) error {\n")
	for _, c := range m.commands {
		w.f("\t{\n\t\th, err := %s(rt, fills.%s, options)\n\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n\t\tmux.Handle(%q, h)\n\t}\n", c.goName, c.goName, c.pattern)
	}
	w.f("\treturn nil\n}\n\n")
	if m.probeImport == "" {
		w.f("// noProbe is the typed probe of a service that has no domain probes.\nfunc noProbe(*probe.Probe) struct{} { return struct{}{} }\n\n")
	}
	for _, c := range m.commands {
		renderCommandHandlers(w, m, c)
	}
	return w.Bytes()
}

func (m *model) hasLockTimeout() bool {
	for _, c := range m.commands {
		if c.lock != "" {
			return true
		}
	}
	return false
}

// hasServiceLevel tells whether a command is decided on the service-level resource (ST-SUP-RC32-OC15).
func (m *model) hasServiceLevel() bool {
	for _, c := range m.commands {
		if c.service != "" {
			return true
		}
	}
	return false
}

func (m *model) hasRelations() bool {
	for _, c := range m.commands {
		if len(c.relations) > 0 {
			return true
		}
	}
	return false
}

// renderCommandHandlers writes the command declaration, the ServeSpec and the handlers of one command.
func renderCommandHandlers(w *writer, m *model, c *command) {
	lower := strings.ToLower(c.goName[:1]) + c.goName[1:]
	probe := m.probeType(c)
	w.f("// %sCommand is the declaration of the command %s (ST-SUP-RP10-LC31, ST-SUP-RC32-OC10).\n", lower, c.decl.Name)
	w.f("var %sCommand = command.Must(command.NewCommand(command.CommandSpec[%s]{\n", lower, probe)
	w.f("\tName: %q,\n\tWrites: []command.Declared{", c.decl.Name)
	for i, e := range c.writes {
		if i > 0 {
			w.f(", ")
		}
		w.f("commands.%sEntity", e.goName)
	}
	w.f("},\n")
	// With kafka-producer, the handle stages the change event of each write (ST-SUP-RC32-OC14).
	written := make([]string, len(c.writes))
	for i, e := range c.writes {
		written[i] = e.decl.Name
	}
	w.f("%s", ChangeEventsSpec(m.ctx, written))
	if len(c.lists) > 0 {
		// The handle takes only the declared lists (ST-SUP-RP10-LC38).
		// The lists that the contract package took when it started, before a fill could change a list variable
		// (security review of PR #416, L3).
		w.f("\tLists: commands.%s(),\n", DeclaredListsName(c.goName))
	}
	if len(c.effects) > 0 {
		w.f("\tDestinations: []command.Destination{")
		for i, d := range c.effects {
			if i > 0 {
				w.f(", ")
			}
			declared := m.destinations[d]
			w.f("{Name: %q, MustLand: %t, Idempotent: %t, ", d, declared.mustLand, declared.idempotent)
			if declared.consumed {
				w.f("Consumed: true, ")
			}
			w.f("Effects: []string{")
			for j, name := range c.effectNames[d] {
				if j > 0 {
					w.f(", ")
				}
				w.f("%q", name)
			}
			w.f("}}")
		}
		w.f("},\n")
	}
	if len(c.relations) > 0 {
		w.f("\tRelations: []authz.Relation{")
		for i, r := range c.relations {
			if i > 0 {
				w.f(", ")
			}
			w.f("command.Must(authz.ParseRelation(%q))", r)
		}
		w.f("},\n")
	}
	w.f("\tRetryOnConflict: %d,\n", c.retry)
	if c.lock != "" {
		w.f("\tLockTimeout: %s,\n", c.lock)
	}
	if m.probeImport != "" {
		w.f("\tProbe: probes.New%sProbe,\n", c.goName)
	} else {
		w.f("\tProbe: noProbe,\n")
	}
	r := c.resource
	if c.service != "" {
		// A command on the service-level resource names no row: Run decides its permission on
		// <resource type>:<service name> in the tenant of the request, with no row to read (ST-SUP-RC32-OC15).
		w.f("\tPermission: %q,\n\tResource: &command.Resource{Type: %q, Service: %q},\n}))\n\n", c.decl.Permission, r.resourceType, c.service)
	} else {
		// The table is named with its schema, so no relation of the same name earlier on the search path stands in
		// for it.
		token := fmt.Sprintf("SELECT authz_token FROM public.%s WHERE %s = $1 AND id = $2::%s FOR SHARE", r.table, m.tenantKey, r.keyCast)
		w.f("\tPermission: %q,\n\tResource: &command.Resource{Type: %q, Token: %q, ValidID: command.ValidUUID},\n}))\n\n", c.decl.Permission, r.resourceType, token)
	}
	spec := fmt.Sprintf("command.ServeSpec[*commands.%s, *commands.%s, %s]", c.request.name, c.response.name, probe)
	w.f("// %sSpec is what varies with the declaration of %s.\nfunc %sSpec(fill commands.%sFill, options Options) %s {\n", lower, c.decl.Name, lower, c.goName, spec)
	w.f("\tspec := %s{\n\t\tCommand: %sCommand,\n\t\tResourceType: %q,\n\t\tSubjectType: subjectType,\n", spec, lower, r.resourceType)
	status := "http.StatusOK"
	if c.creates {
		status = "http.StatusCreated"
	}
	w.f("\t\tStatus: %s,\n", status)
	w.f("\t\tNewRequest: func() *commands.%s { return &commands.%s{} },\n", c.request.name, c.request.name)
	w.f("\t\tNewResponse: func() *commands.%s { return &commands.%s{} },\n", c.response.name, c.response.name)
	if len(c.params) > 0 {
		w.f("\t\tPath: func(r *http.Request, request *commands.%s) error {\n", c.request.name)
		for _, p := range c.params {
			w.f("\t\t\tif err := command.SetPath(&request.%s, r.PathValue(%q)); err != nil {\n\t\t\t\treturn err\n\t\t\t}\n", p.field.goName, p.name)
		}
		w.f("\t\t\treturn nil\n\t\t},\n")
	}
	if c.service != "" {
		w.f("\t\tResource: func(*commands.%s) string { return %q },\n", c.request.name, c.service)
	} else {
		w.f("\t\tResource: func(request *commands.%s) string { return request.%s },\n", c.request.name, c.member.goName)
	}
	w.f("\t\tRefusals: []*command.Refusal{")
	for i, ref := range c.refusals {
		if i > 0 {
			w.f(", ")
		}
		w.f("commands.%s", ref.goName)
	}
	w.f("},\n\t\tKeyRequired: true,\n\t\tRetention: options.Retention,\n\t}\n")
	handle := "h"
	if c.service != "" {
		handle = "commands.New" + c.goName + "Handle(h)"
	}
	w.f("\tif fill != nil {\n\t\tspec.Fill = func(ctx context.Context, request *commands.%s, h *command.Handle[%s]) (*commands.%s, error) {\n\t\t\treturn fill(ctx, request, %s)\n\t\t}\n\t}\n\treturn spec\n}\n\n", c.request.name, probe, c.response.name, handle)
	w.f("// %s gives the HTTP handler of the command %s at %s: it calls the one fill %s through command.Serve.\n", c.goName, c.decl.Name, c.pattern, c.decl.Fill[0])
	w.f("func %s(rt *command.Runtime, fill commands.%sFill, options Options) (http.Handler, error) {\n\treturn command.Serve(rt, %sSpec(fill, options))\n}\n\n", c.goName, c.goName, lower)
	w.f("// %sRPC gives the gRPC handler of the command %s: the same fill, handle, transaction, permission, audit and\n// idempotency as its HTTP handler, with the idempotency key from the idempotency-key request metadata.\n", c.goName, c.decl.Name)
	w.f("func %sRPC(rt *command.Runtime, fill commands.%sFill, options Options) (func(context.Context, *commands.%s) (*commands.%s, error), error) {\n\treturn command.ServeRPC(rt, %sSpec(fill, options))\n}\n\n", c.goName, c.goName, c.request.name, c.response.name, lower)
}

// renderFills writes the binding of each command to the one fill that it names (ST-SUP-RC32-OC02): the fill folder
// fills/<fill> of the project is a Go package whose New gives the command's fill type, so a fill of another slot
// does not compile here.
func renderFills(m *model) []byte {
	w := &writer{}
	w.f("package %s\n\nimport (\n", handlersFolder)
	seen := map[string]bool{}
	for _, c := range m.commands {
		name := c.decl.Fill[0]
		if !seen[name] {
			seen[name] = true
			w.f("\t%s %q\n", fillAlias(name), m.ctx.ModuleName+"/fills/"+name)
		}
	}
	w.f(")\n\n// BoundFills gives the one fill that each command names, from its fill folder.\nfunc BoundFills() Fills {\n\treturn Fills{\n")
	for _, c := range m.commands {
		w.f("\t\t%s: %s.New(),\n", c.goName, fillAlias(c.decl.Fill[0]))
	}
	w.f("\t}\n}\n")
	return w.Bytes()
}

// fillAlias is the import alias of a fill package.
func fillAlias(name string) string {
	alias := mixedCase(name)
	return "fill" + alias
}

// listOperators are the SQL operators of the list term operators (ST-DECL-INV94).
var listOperators = map[string]string{types.ListEquals: "=", types.ListNotEquals: "<>", types.ListLessThan: "<",
	types.ListLessThanOrEquals: "<=", types.ListGreaterThan: ">", types.ListGreaterThanOrEquals: ">="}

// ListPlan is the generated form of one declared list of a command or a worker (ST-SUP-RC32-OC16,
// ST-SUP-RC35-OC15): its Read and Lock statements over the entity of the release, the Go source of its
// command.ListValue values in the order of their parameters, and its parameters, which the generated With function
// takes as typed Go arguments.
type ListPlan struct {
	Read, Lock string
	Values     []string
	Params     []ListParam
	// Child tells that the entity is a child: it has no resource type and References references to entities with a
	// resource type; Parents are the referenced entities of the parent terms (ST-DECL-INV94).
	Child      bool
	References int
	Parents    []string
}

// ListParam is one declared parameter of a list: its name, the Go name of its argument and the Go type of its field.
type ListParam struct {
	Name, GoName, GoType string
}

// ListStatements gives the plan of the declared list of a command or a worker over the entity of the release
// (ST-SUP-RC35-OC15, ST-SUP-RC32-OC16). Both statements return the column list of the entity's generated Scan and
// take the tenant as $1. Read takes the most rows as $2; Lock takes the IDs to lock as the text array $2, applies the
// filter again, and locks FOR NO KEY UPDATE. Each term with a value binds it as a parameter from $3: a constant, the
// time of the run for now, or a declared parameter, which the fill gives through the generated With function; is_set
// binds none. Both order by the key, so a schema step that adds or renames a column changes the list with the release
// and no fill gives SQL text. Lock is empty for a list that does not lock.
func ListStatements(list types.WorkerList, entities []types.Entity, tenantKey string) (ListPlan, error) {
	byName := map[string]types.Entity{}
	for _, e := range entities {
		byName[e.Name] = e
	}
	decl, ok := byName[list.Entity]
	if !ok {
		return ListPlan{}, fmt.Errorf("command: the list %s reads the entity %s, which is not declared", list.Name, list.Entity)
	}
	e, err := planEntity(decl, byName, tenantKey, Relations{})
	if err != nil {
		return ListPlan{}, fmt.Errorf("command: the entity %s: %w", list.Entity, err)
	}
	if len(list.Filter) == 0 {
		return ListPlan{}, fmt.Errorf("command: the list %s has no filter term", list.Name)
	}
	var plan ListPlan
	var terms []string
	seen := map[string]bool{}
	for _, term := range list.Filter {
		var field *entityField
		for _, f := range e.fields {
			if f.decl.Name == term.Field {
				field = f
			}
		}
		if field == nil {
			return ListPlan{}, fmt.Errorf("command: the list %s names the field %s, which %s does not declare", list.Name, term.Field, list.Entity)
		}
		column := quote(field.decl.ColumnName())
		if term.Op == types.ListIsSet {
			terms = append(terms, column+" IS NOT NULL")
			continue
		}
		op, ok := listOperators[term.Op]
		if !ok {
			return ListPlan{}, fmt.Errorf("command: the list %s has the operator %q", list.Name, term.Op)
		}
		var value string
		if term.Parent {
			if term.Op != types.ListEquals || field.decl.Type != types.FieldTypeRef || term.Param != "" || term.Value != nil || term.Now {
				return ListPlan{}, fmt.Errorf("command: the list %s, the term on %s: a parent term needs a reference field and equals, and no value", list.Name, term.Field)
			}
			if byName[field.decl.To].ResourceType == "" {
				// The decided parent row is a row of an entity with a resource type (ST-DECL-INV94); the handle
				// cannot decide a row of another entity.
				return ListPlan{}, fmt.Errorf("command: the list %s, the term on %s: a parent term needs a reference to an entity with a resource type, and %s has none (ST-DECL-INV94)", list.Name, term.Field, field.decl.To)
			}
			value = fmt.Sprintf("command.ListValue{Parent: %q}", field.decl.To)
			plan.Parents = append(plan.Parents, field.decl.To)
			plan.Params = append(plan.Params, ListParam{Name: term.Field, GoName: paramName(strings.TrimSuffix(term.Field, "_id")), GoType: "Loaded" + field.decl.To})
		} else if term.Param != "" {
			goType, ok := paramTypes[field.decl.Type]
			if !ok || seen[term.Param] || term.Value != nil || term.Now {
				return ListPlan{}, fmt.Errorf("command: the list %s, the term on %s: the parameter %s is given twice, with a value, or on a field of the type %s", list.Name, term.Field, term.Param, field.decl.Type)
			}
			seen[term.Param] = true
			if field.decl.Type == types.FieldTypeRef {
				// A reference holds the key of a row of the referenced entity: the handle checks it with the key
				// check of that entity, whose keys are UUIDs (ST-SUP-RP10-LC38, sec-416b).
				value = fmt.Sprintf("command.ListValue{Param: %q, Key: true, KeyOf: command.KeyUUID}", term.Param)
			} else {
				value = fmt.Sprintf("command.ListValue{Param: %q}", term.Param)
			}
			plan.Params = append(plan.Params, ListParam{Name: term.Param, GoName: paramName(term.Param), GoType: goType})
		} else if value, err = listValue(field.decl, term); err != nil {
			return ListPlan{}, fmt.Errorf("command: the list %s, the term on %s: %w", list.Name, term.Field, err)
		}
		plan.Values = append(plan.Values, value)
		terms = append(terms, fmt.Sprintf("%s %s $%d", column, op, len(plan.Values)+2))
	}
	plan.Child, plan.References = e.resourceType == "" && len(e.references) > 0, len(e.references)
	from := fmt.Sprintf("SELECT %s FROM public.%s WHERE %s = $1", e.columns(), quote(e.table), quote(tenantKey))
	filter := " AND " + strings.Join(terms, " AND ")
	plan.Read = from + filter + ` ORDER BY "id" LIMIT $2`
	if list.Lock {
		plan.Lock = from + ` AND "id" = ANY($2::text[]::uuid[])` + filter + ` ORDER BY "id" FOR NO KEY UPDATE`
	}
	return plan, nil
}

// CheckChild refuses a plan of a declared list of owner that does not fit its entity (ST-DECL-INV94,
// ST-SUP-RC32-OC16): a list of a child entity needs exactly one parent term, of an entity in writes, and the child
// one reference to an entity with a resource type; a list of any other entity takes no parent term.
func (p ListPlan) CheckChild(owner, list, entity string, writes []string) error {
	switch {
	case p.Child && p.References > 1:
		return fmt.Errorf("%s: the list %s reads the child entity %s, which refers to more than one entity with a resource type, so it has no list (ST-DECL-INV94)", owner, list, entity)
	case p.Child && len(p.Parents) != 1:
		return fmt.Errorf("%s: the list %s reads the child entity %s and needs exactly one parent term, which takes the decided parent row (ST-DECL-INV94)", owner, list, entity)
	case !p.Child && len(p.Parents) > 0:
		return fmt.Errorf("%s: the list %s has a parent term, and %s is not a child entity (ST-DECL-INV94)", owner, list, entity)
	}
	for _, parent := range p.Parents {
		if !slices.Contains(writes, parent) {
			return fmt.Errorf("%s: the list %s takes a parent row of %s, which it does not write, so its fill cannot load one", owner, list, parent)
		}
	}
	return nil
}

// paramTypes are the Go types of a list parameter by the type of its field: the Go type of the entity's field, which
// command.List.With takes. A field of the type bytes or jsonb takes no parameter (ST-DECL-INV94).
var paramTypes = map[types.FieldType]string{types.FieldTypeString: "string", types.FieldTypeEnum: "string",
	types.FieldTypeRef: "string", types.FieldTypeInt32: "int32", types.FieldTypeInt64: "int64",
	types.FieldTypeFloat: "float32", types.FieldTypeDouble: "float64", types.FieldTypeBool: "bool",
	types.FieldTypeTimestamp: "time.Time"}

// paramName gives the Go name of the argument of the parameter name, such as orderID for order_id; a name that is a
// Go keyword or a predeclared name takes the suffix Value.
func paramName(name string) string {
	mixed := mixedCase(name)
	lower := strings.ToLower(mixed[:1]) + mixed[1:]
	if token.IsKeyword(lower) || reservedParams[lower] {
		return lower + "Value"
	}
	return lower
}

// reservedParams are the predeclared Go names and the package names of a contract that an argument must not hide.
var reservedParams = map[string]bool{"any": true, "append": true, "bool": true, "byte": true, "cap": true, "clear": true,
	"close": true, "complex": true, "copy": true, "delete": true, "error": true, "false": true, "float32": true,
	"float64": true, "imag": true, "int": true, "int32": true, "int64": true, "iota": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "nil": true, "panic": true, "print": true, "println": true, "real": true,
	"recover": true, "rune": true, "string": true, "true": true, "uint": true, "command": true, "context": true,
	"time": true, "json": true, "bytes": true, "audit": true, "authz": true, "tenant": true, "probes": true}

// RenderList writes one declared list of a command or a worker, named goName, over the entity entity of the same
// package: the typed command.List with its statements and its values and, for a list with parameters, the function
// <goName>With that takes each parameter as a typed Go argument and gives the list with them bound
// (ST-SUP-RP10-LC38). owner names the command or the worker, as "the command place-order".
func RenderList(w *bytes.Buffer, goName, name, owner, entity string, plan ListPlan) {
	fmt.Fprintf(w, "// %s is the list %s of %s over %s, which its fill reads with List or ListForUpdate. The\n// generator builds its statements from the entity of this release, so a fill gives no SQL text (ST-SUP-RC32-OC16,\n// ST-SUP-RC35-OC15, ST-SUP-RP10-LC38).", goName, name, owner, entity)
	if len(plan.Params) > 0 {
		fmt.Fprintf(w, " Its fill gives the parameters through %sWith.", goName)
	}
	fmt.Fprintf(w, "\nvar %s = command.Must(command.NewList(%sEntity, command.ListSpec{Name: %q,\n\tRead: %q,\n", goName, entity, name, plan.Read)
	if plan.Lock != "" {
		fmt.Fprintf(w, "\tLock: %q,\n", plan.Lock)
	}
	fmt.Fprintf(w, "\tValues: command.DeclaredValues(%s)}))\n\n", strings.Join(plan.Values, ", "))
	if len(plan.Params) == 0 {
		return
	}
	args := make([]string, len(plan.Params))
	names := make([]string, len(plan.Params))
	for i, p := range plan.Params {
		args[i] = p.GoName + " " + p.GoType
		names[i] = p.GoName
	}
	fmt.Fprintf(w, "// %sWith gives the list %s of %s with its parameters bound, which the handle binds as SQL\n// parameters (ST-SUP-RP10-LC38).\nfunc %sWith(%s) *command.List[%s] {\n\treturn %s.With(%s)\n}\n\n",
		goName, name, owner, goName, strings.Join(args, ", "), entity, goName, strings.Join(names, ", "))
}

// DeclaredListsName is the name of the function that gives the declared lists of the command or worker whose Go
// name is owner, for its CommandSpec.Lists or WorkerSpec.Lists.
func DeclaredListsName(owner string) string { return owner + "DeclaredLists" }

// RenderDeclaredLists writes, for the command or worker whose Go name is owner, an unexported slice of its declared
// lists, which the contract package fills when it starts, and the function DeclaredListsName(owner) that gives a
// copy of it. A fill imports the contract package, so the slice holds the generated lists, with a copy of each
// declaration (command.Snapshot), before the init of a fill runs: a fill that sets a list variable to a list of its own,
// or overwrites the list through its pointer, still gets a list that its handle refuses (ST-SUP-RP10-LC38, security
// reviews of PR #416, L3, and of PR #421, L1). It writes nothing for no list.
func RenderDeclaredLists(w *bytes.Buffer, owner, ownerText string, lists []string) {
	if len(lists) == 0 {
		return
	}
	held := strings.ToLower(owner[:1]) + owner[1:] + "DeclaredLists"
	fmt.Fprintf(w, "// %s are the declared lists of %s, each with a copy of its declaration that this package takes when\n// it starts, so the handle refuses a list that a fill overwrites through its pointer.\nvar %s = command.Snapshot(%s)\n\n", held, ownerText, held, strings.Join(lists, ", "))
	fmt.Fprintf(w, "// %s gives the declared lists of %s for its spec: the lists that this package made, also when a fill set a\n// list variable to another list (ST-SUP-RP10-LC38).\nfunc %s() []command.AnyList { return append([]command.AnyList(nil), %s...) }\n\n",
		DeclaredListsName(owner), ownerText, DeclaredListsName(owner), held)
}

// listValue gives the Go source of the command.ListValue of one term: the time of the run for now, or the constant
// as a value of the Go type of the field. A timestamp constant is RFC 3339 text, which PostgreSQL reads as the
// column's type.
func listValue(field types.Field, term types.ListTerm) (string, error) {
	if term.Now {
		if field.Type != types.FieldTypeTimestamp {
			return "", fmt.Errorf("now fits only a timestamp field")
		}
		return "command.ListValue{RunTime: true}", nil
	}
	switch v := term.Value.(type) {
	case string:
		if field.Type == types.FieldTypeString || field.Type == types.FieldTypeEnum || field.Type == types.FieldTypeRef || field.Type == types.FieldTypeTimestamp {
			return fmt.Sprintf("command.ListValue{Constant: %q}", v), nil
		}
	case bool:
		if field.Type == types.FieldTypeBool {
			return fmt.Sprintf("command.ListValue{Constant: %t}", v), nil
		}
	case int:
		switch field.Type {
		case types.FieldTypeInt32:
			return fmt.Sprintf("command.ListValue{Constant: int32(%d)}", v), nil
		case types.FieldTypeInt64:
			return fmt.Sprintf("command.ListValue{Constant: int64(%d)}", v), nil
		case types.FieldTypeFloat, types.FieldTypeDouble:
			return fmt.Sprintf("command.ListValue{Constant: float64(%d)}", v), nil
		}
	case float64:
		if field.Type == types.FieldTypeFloat || field.Type == types.FieldTypeDouble {
			return fmt.Sprintf("command.ListValue{Constant: float64(%v)}", v), nil
		}
	}
	return "", fmt.Errorf("the value %v is not a constant of the field type %s", term.Value, field.Type)
}
