// Package command generates the per-service code of the command capability (ST-SUP-RC32) on the command runtime
// github.com/jsell-rh/stego/registry/runtime/command (ST-SUP-RP10). For each declared command it emits the command slot contract (the typed
// request and response messages, the fill type and the command handle), the typed refusals, the repository
// declarations of the entities that the command writes, the command declaration with its permission and resource,
// the probe wiring through the domain probes (ST-SUP-RC34), and one HTTP handler and one gRPC handler that call the
// one fill through Run. The handle, Run, the idempotency wrapper, the audit and outbox writes and the decisions live
// in the runtime library; the generated code holds what varies with the declaration (ST-STD-RUNTIME-R05).
package command

import (
	"bytes"
	"fmt"
	"go/format"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	authzschema "github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// Generator is the generator of the command component.
type Generator struct{}

// The slot that each command fill implements, and the components whose output the generated code uses.
const (
	commandSlot    = "command.command"
	probeComponent = "domain-probe"
	runtimeModule  = "github.com/jsell-rh/stego/registry/runtime"
	protobufModule = "google.golang.org/protobuf"
	protobufPin    = "v1.36.12"
	grpcModule     = "google.golang.org/grpc"
	grpcPin        = "v1.83.2"
)

// MinimumGoVersion is the Go version of the runtime module that the generated code imports.
func (*Generator) MinimumGoVersion() string { return "1.26.11" }

// ConsumedSlots is empty: a command fill is bound by the command's fill, not by a slot binding.
func (*Generator) ConsumedSlots() []string { return []string{} }

// InputFiles names the proto files of the command messages, which the compiler reads for the generator.
func (*Generator) InputFiles(config map[string]any) ([]gen.InputFile, error) {
	if _, set := config["proto_files"]; !set {
		// A service that declares no command has no command messages (ST-SUP-RC20-OC39).
		return nil, nil
	}
	items, err := protoSources(config)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(items))
	for i, item := range items {
		names[i] = item.File
	}
	return gen.SourceInputs(names), nil
}

// ValidateContext checks the declared commands before any code is rendered.
func (*Generator) ValidateContext(ctx gen.Context) error {
	_, err := plan(ctx)
	return err
}

// Generate emits the command contract package and its handlers package in the component's output namespace.
func (*Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	m, err := plan(ctx)
	if err != nil {
		return nil, nil, err
	}
	if m == nil {
		return routeDocument(ctx, nil), &gen.Wiring{}, nil
	}
	files, err := m.messageFiles()
	if err != nil {
		return nil, nil, err
	}
	contract, err := format.Source(renderContract(m))
	if err != nil {
		return nil, nil, fmt.Errorf("command: formatting the contract package: %w", err)
	}
	handlers, err := format.Source(renderHandlers(m))
	if err != nil {
		return nil, nil, fmt.Errorf("command: formatting the handlers package: %w", err)
	}
	bound, err := format.Source(renderFills(m))
	if err != nil {
		return nil, nil, fmt.Errorf("command: formatting the fill binding: %w", err)
	}
	files = append(files,
		gen.File{Path: path.Join(ctx.OutputNamespace, "commands.go"), Content: contract},
		gen.File{Path: path.Join(ctx.OutputNamespace, handlersFolder, "fills.go"), Content: bound},
		gen.File{Path: path.Join(ctx.OutputNamespace, handlersFolder, "handlers.go"), Content: handlers},
	)
	files = routeDocument(ctx, files)
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	wiring := &gen.Wiring{GoModRequires: map[string]string{protobufModule: protobufPin, grpcModule: grpcPin}}
	return files, wiring, nil
}

// routeDocument adds openapi.json to files when the compiler gives the component the OpenAPI document of the command
// and query routes of a service with no rest-api component (ST-SUP-RC32-OC29); the client components are made from
// the same document.
func routeDocument(ctx gen.Context, files []gen.File) []gen.File {
	if ctx.ServiceOpenAPI == nil {
		return files
	}
	return append(files, gen.File{Path: path.Join(ctx.OutputNamespace, "openapi.json"), Content: ctx.ServiceOpenAPI})
}

// handlersFolder is the folder of the handlers package under the output namespace. Its code holds the runtime; the
// contract package, which a fill imports, does not.
const handlersFolder = "handlers"

// model is what the generator renders.
type model struct {
	ctx            gen.Context
	pkg            string // the contract package name
	contractImport string
	handlersImport string
	probeImport    string // the domain probes package, or empty when the service has none
	tenantKey      string
	// relations are the schema relations of each resource type (SchemaRelations).
	relations   Relations
	subjectType string
	basePath    string
	messages    *messages
	entities    []*entity // the entities that a command writes, in declaration order
	commands    []*command
	names       map[string]string
	// destinations are the declared outbox destinations by name, with their delivery (ST-SUP-RC32-OC12).
	destinations map[string]delivery
}

// ConsumedComponent is the component of a destination that a declared worker consumes (ST-SUP-RC35-OC06).
const ConsumedComponent = "worker"

// delivery is the declared delivery of one outbox destination.
type delivery struct{ mustLand, idempotent, consumed bool }

// declaredDeliveries reads the destinations that the components declare, and refuses one that leaves must_land or
// idempotent out, naming the destination and the property (ST-SUP-RC32-OC12).
func declaredDeliveries(destinations []gen.Destination) (map[string]delivery, error) {
	out := map[string]delivery{}
	for _, d := range destinations {
		for property, value := range map[string]*bool{"must_land": d.MustLand, "idempotent": d.Idempotent} {
			if value == nil {
				return nil, fmt.Errorf("command: the destination %s of the component %s does not declare %s; the setting destinations declares must_land and idempotent for each destination (ST-SUP-RC32-OC12)", d.Name, d.Component, property)
			}
		}
		// A destination that a worker consumes stores the staging principal and the stager of each message
		// (ST-SUP-RP10-LC16).
		out[d.Name] = delivery{mustLand: *d.MustLand, idempotent: *d.Idempotent, consumed: d.Component == ConsumedComponent}
	}
	return out, nil
}

// command is one checked command.
type command struct {
	decl      types.Command
	goName    string
	writes    []*entity
	resource  *resource
	request   *message
	response  *message
	params    []param
	member    *messageField // the resource member, or nil for a command without a permission
	refusals  []refusal
	creates   bool
	pattern   string
	relations []string
	// helpers are the typed relation helpers of the declared relations (ST-SUP-RC32-OC13).
	helpers []relationHelper
	effects []string
	// effectNames are the declared effect names of each effect destination, which Stage holds the fill to
	// (ST-SUP-RP10-LC07).
	effectNames map[string][]string
	retry       int
	lock        string // a Go duration expression, or empty for the default

	// service is the service name of the service-level resource of a command whose resource type is the resource
	// type of the tenant provisioning, and empty for every other command (ST-SUP-RC32-OC15).
	service string
	// lists are the declared lists of the command, which the contract emits from the entity of the release
	// (ST-SUP-RC32-OC16).
	lists []*commandList
	// resets are the declared step-up resets of the attempts of counted fields (ST-SUP-RC32-OC24, ST-DECL-INV104), and
	// resetGuards the permissions of the other commands on its resource type that write their entities, which a caller
	// of the reset may not hold on the row (ST-SUP-RP10-LC54).
	resets      []resetHelper
	resetGuards []string
}

// resetHelper is the typed helper of one declared reset of attempts: the written entity, its counted field, and the
// name of the helper in the contract.
type resetHelper struct {
	entity *entity
	field  *entityField
	name   string
}

// commandList is one declared list of a command with its generated plan.
type commandList struct {
	decl   types.WorkerList
	goName string // the exported name in the contract, such as PlaceOrderOfOrder
	plan   ListPlan
}

// relationHelper is the pair of typed helpers of one relation that a command declares in relationships_written: the
// relation, and the written entity whose rows are the relation's resources.
type relationHelper struct {
	resourceType, relation string
	entity                 *entity
	relate, unrelate       string
}

// resource is the resource of a command's permission: its type and the table of the entity that has it.
type resource struct {
	resourceType string
	table        string
	keyCast      string
}

// param is one path parameter of a command's route and the request field that holds it.
type param struct {
	name  string
	field *messageField
}

// refusal is one declared refusal of a command.
type refusal struct {
	name, goName, code string
	status             int
}

// plan checks the context and builds the model. It refuses, naming the command, each declaration that the
// generated code or the runtime's checks would refuse, so the emitted code never fails at start. It gives no model
// for a service that declares no command: the component is then in the service only because its archetype holds it,
// and it emits nothing (ST-SUP-RC20-OC39).
func plan(ctx gen.Context) (*model, error) {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return nil, err
	}
	for key := range ctx.ComponentConfig {
		if key != "proto_files" {
			return nil, fmt.Errorf("command: the component has no setting %s", key)
		}
	}
	m := &model{
		ctx:            ctx,
		pkg:            path.Base(ctx.OutputNamespace),
		contractImport: path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace),
		basePath:       strings.TrimSuffix(ctx.BasePath, "/"),
		names:          map[string]string{},
	}
	m.handlersImport = path.Join(m.contractImport, handlersFolder)
	if ns := ctx.PeerNamespaces[probeComponent]; ns != "" {
		m.probeImport = path.Join(ctx.ModuleName, ctx.OutDirName, ns)
	}
	if len(ctx.Commands) == 0 {
		if _, set := ctx.ComponentConfig["proto_files"]; set {
			return nil, fmt.Errorf("command: the setting proto_files holds the messages of the declared commands, and the declaration declares no command; remove the setting or declare a command")
		}
		return nil, nil
	}
	if ctx.Tenancy == nil || ctx.Tenancy.Key != "tenant_id" {
		return nil, fmt.Errorf("command: a command runs in the transaction of one tenant, so the service needs pooled tenancy with the tenant column tenant_id")
	}
	m.tenantKey = ctx.Tenancy.Key
	if ctx.Authorization == nil || ctx.Authorization.SubjectFromIdentity == "" {
		return nil, fmt.Errorf("command: a command decides its permission before the fill, so the service needs authorization with subject_from_identity")
	}
	m.subjectType = ctx.Authorization.SubjectFromIdentity
	deliveries, err := declaredDeliveries(ctx.Destinations)
	if err != nil {
		return nil, err
	}
	m.destinations = deliveries
	msgs, err := compileMessages(ctx)
	if err != nil {
		return nil, err
	}
	m.messages = msgs
	if m.relations, err = SchemaRelations(ctx); err != nil {
		return nil, err
	}
	byName := map[string]types.Entity{}
	for _, e := range ctx.Entities {
		byName[e.Name] = e
	}
	planned := map[string]*entity{}
	for _, c := range ctx.Commands {
		cmd, err := m.planCommand(c, byName, planned)
		if err != nil {
			return nil, err
		}
		m.commands = append(m.commands, cmd)
	}
	m.planResetGuards()
	for _, e := range ctx.Entities {
		if p := planned[e.Name]; p != nil {
			m.entities = append(m.entities, p)
		}
	}
	if err := m.checkChangeEvents(); err != nil {
		return nil, err
	}
	if err := m.takeNames(); err != nil {
		return nil, err
	}
	return m, nil
}

// checkChangeEvents refuses, for a service with the kafka-producer component, a service without the outbox
// component, a written entity whose change event name Kafka refuses, and a declared effect named as a change event of
// the service, which only the handle stages (ST-SUP-RC32-OC14, ST-SUP-RP10-LC36).
func (m *model) checkChangeEvents() error {
	if !ChangeEvents(m.ctx) {
		return nil
	}
	var names []string
	for _, e := range m.entities {
		names = append(names, e.decl.Name)
	}
	if err := CheckChangeEvents(m.ctx, "command", "ST-SUP-RC32-OC14", names); err != nil {
		return err
	}
	events := map[string]bool{}
	for _, e := range m.ctx.Entities {
		for _, action := range []string{EventCreated, EventUpdated, EventDeleted} {
			events[ChangeEventType(m.ctx.ServiceName, e.Name, action)] = true
		}
	}
	for _, c := range m.commands {
		for _, d := range c.effects {
			for _, name := range c.effectNames[d] {
				if events[name] {
					return fmt.Errorf("command: the command %s declares the effect %s, which is the name of a change event that the handle stages for each write (ST-SUP-RC32-OC14)", c.decl.Name, name)
				}
			}
		}
	}
	return nil
}

// planCommand checks one command.
func (m *model) planCommand(c gen.Command, byName map[string]types.Entity, planned map[string]*entity) (*command, error) {
	decl := c.Command
	if !operationName.MatchString(decl.Name) {
		return nil, fmt.Errorf("command: the command name %q is not a lower-case name of letters, digits and hyphens", decl.Name)
	}
	cmd := &command{decl: decl, goName: mixedCase(decl.Name)}
	// One handler calls the one fill that the command names, and that fill must implement the command slot
	// (ST-SUP-RC32-OC02).
	if len(decl.Fill) != 1 {
		return nil, fmt.Errorf("command: the command %s names %d fills; it needs exactly one fill of the slot %s", decl.Name, len(decl.Fill), commandSlot)
	}
	if c.FillSlot != commandSlot {
		found := c.FillSlot
		if found == "" {
			found = "no slot"
		}
		return nil, fmt.Errorf("command: the command %s names the fill %s, which implements %s, not the command slot %s", decl.Name, decl.Fill[0], found, commandSlot)
	}
	if !fillName.MatchString(decl.Fill[0]) {
		return nil, fmt.Errorf("command: the fill %q of the command %s is not a valid fill folder name", decl.Fill[0], decl.Name)
	}
	if len(decl.Writes) == 0 {
		return nil, fmt.Errorf("command: the command %s writes no entity", decl.Name)
	}
	for _, name := range decl.Writes {
		e, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("command: the command %s writes the entity %s, which the declaration does not declare", decl.Name, name)
		}
		p := planned[name]
		if p == nil {
			var err error
			if p, err = planEntity(e, byName, m.tenantKey, m.relations); err != nil {
				return nil, fmt.Errorf("command: the command %s writes %s: %w", decl.Name, name, err)
			}
			planned[name] = p
		}
		cmd.writes = append(cmd.writes, p)
	}
	if err := m.planRoute(cmd); err != nil {
		return nil, err
	}
	if err := m.planPermission(cmd, byName); err != nil {
		return nil, err
	}
	for _, r := range decl.Refusals {
		if !refusalName.MatchString(r.Name) || !refusalCode.MatchString(r.Code) || r.Code == "internal" || r.Code == "conflict" || !refusalStatus[r.Status] {
			return nil, fmt.Errorf("command: the command %s declares the refusal %s: %d %s, which is not a lower snake case name with a status of 400, 403, 404, 409, 412 or 422 and a code other than internal and conflict", decl.Name, r.Name, r.Status, r.Code)
		}
		cmd.refusals = append(cmd.refusals, refusal{name: r.Name, goName: cmd.goName + mixedCase(r.Name), code: r.Code, status: r.Status})
	}
	for _, text := range decl.RelationshipsWritten {
		if !relationText.MatchString(text) {
			return nil, fmt.Errorf("command: the command %s writes the relation %q, which is not resource_type#relation", decl.Name, text)
		}
		cmd.relations = append(cmd.relations, text)
		resourceType, relation, _ := strings.Cut(text, "#")
		// The tenant resource and the worker relations on it are written by the tenant provisioning alone
		// (ST-SUP-RC35-OC13, ST-SUP-Q132).
		if resourceType == authzschema.TenantResourceType || strings.HasPrefix(relation, authzschema.WorkerRelationPrefix) {
			return nil, fmt.Errorf("command: the command %s writes the relation %s, which only the tenant provisioning writes: the resource type %s and each relation that starts with %s hold the grants of the workers", decl.Name, text, authzschema.TenantResourceType, authzschema.WorkerRelationPrefix)
		}
		h := relationHelper{resourceType: resourceType, relation: relation,
			relate: cmd.goName + "Relate" + mixedCase(relation), unrelate: cmd.goName + "Unrelate" + mixedCase(relation)}
		for _, e := range cmd.writes {
			if e.resourceType == resourceType {
				h.entity = e
				break
			}
		}
		if h.entity == nil {
			return nil, fmt.Errorf("command: the command %s writes the relation %s, but it writes no entity of the resource type %s, whose rows the relation's resources are", decl.Name, text, resourceType)
		}
		// The relation of a declared relationship or of a reference is the runtime's own: a helper for it would let a
		// fill move an owner after create, or relate a row to a parent with no permission on the parent
		// (ST-SUP-RC32-OC13, ST-SUP-Q118).
		for _, e := range cmd.writes {
			if why, ok := authzschema.RuntimeRelations(e.decl)[relation]; ok && e.resourceType == resourceType {
				return nil, fmt.Errorf("command: the command %s writes the relation %s, which is %s; the runtime writes it alone, so a command may not relate or unrelate it", decl.Name, text, why)
			}
		}
		cmd.helpers = append(cmd.helpers, h)
	}
	seen, named := map[string]bool{}, map[string]bool{}
	for _, effect := range decl.Effects {
		if !effectName.MatchString(effect.Name) || named[effect.Name] {
			return nil, fmt.Errorf("command: the command %s declares the effect %q, whose name is missing, not a lower-case name or given twice", decl.Name, effect.String())
		}
		named[effect.Name] = true
		if effect.Destination == "" || effect.Destination == "authz" || effect.Destination == "audit" || effect.Destination == "audit-sink" {
			return nil, fmt.Errorf("command: the command %s stages for the destination %q, which is not an effect destination", decl.Name, effect.Destination)
		}
		if _, ok := m.destinations[effect.Destination]; !ok {
			return nil, fmt.Errorf("command: the command %s stages for the destination %q, which no component declares in its setting destinations (ST-SUP-RC32-OC12)", decl.Name, effect.Destination)
		}
		if !seen[effect.Destination] {
			seen[effect.Destination] = true
			cmd.effects = append(cmd.effects, effect.Destination)
		}
		if cmd.effectNames == nil {
			cmd.effectNames = map[string][]string{}
		}
		cmd.effectNames[effect.Destination] = append(cmd.effectNames[effect.Destination], effect.Name)
	}
	retry, lock, err := retryAndLock(decl)
	if err != nil {
		return nil, err
	}
	cmd.retry, cmd.lock = retry, lock
	if err := m.planLists(cmd, byName); err != nil {
		return nil, err
	}
	if err := planResets(cmd); err != nil {
		return nil, err
	}
	return cmd, nil
}

// planResetGuards gives each command that declares a reset of attempts the permissions of the other commands on its
// resource type that write an entity of its resets, and so may check the field: the runtime refuses the reset of a
// caller who holds one on the row (security review of PR #489, second review, M2b).
func (m *model) planResetGuards() {
	for _, c := range m.commands {
		if len(c.resets) == 0 {
			continue
		}
		reset := map[string]bool{}
		for _, r := range c.resets {
			reset[r.entity.decl.Name] = true
		}
		seen := map[string]bool{}
		for _, other := range m.commands {
			p := other.decl.Permission
			if other == c || p == "" || p == c.decl.Permission || other.decl.ResourceType != c.decl.ResourceType || seen[p] {
				continue
			}
			for _, e := range other.writes {
				if reset[e.decl.Name] {
					seen[p] = true
					c.resetGuards = append(c.resetGuards, p)
					break
				}
			}
		}
		sort.Strings(c.resetGuards)
	}
}

// planResets plans the declared resets of attempts of the command (ST-DECL-INV104, decision ST-SUP-Q205 (c)): each
// names <Entity>.<field> of a field with a counter of an entity that the command writes, once, and the command is
// decided with a permission on a row of that entity, so the reset is decided on the row it resets.
func planResets(cmd *command) error {
	seen := map[string]bool{}
	for _, text := range cmd.decl.ResetsAttempts {
		name, field, _ := strings.Cut(text, ".")
		var h resetHelper
		for _, e := range cmd.writes {
			if e.decl.Name != name {
				continue
			}
			for _, f := range e.fields {
				if f.decl.Name == field && f.limit > 0 {
					h = resetHelper{entity: e, field: f, name: cmd.goName + "Reset" + e.goName + f.goName + "Attempts"}
				}
			}
		}
		if h.entity == nil || seen[text] {
			return fmt.Errorf("command: the command %s resets the attempts of %q, which is not <Entity>.<field> of a field with attempts of an entity that it writes, or is named twice (ST-DECL-INV104)", cmd.decl.Name, text)
		}
		if cmd.service != "" || cmd.member == nil || cmd.decl.Permission == "" || h.entity.resourceType != cmd.decl.ResourceType {
			return fmt.Errorf("command: the command %s resets the attempts of %s and is not decided with a permission on a row of %s (ST-DECL-INV104)", cmd.decl.Name, text, name)
		}
		seen[text] = true
		cmd.resets = append(cmd.resets, h)
	}
	return nil
}

// planLists builds the plan of each declared list of the command from the entity of the release (ST-SUP-RC32-OC16),
// and refuses a list that checking refuses (ST-DECL-INV94) and a list of a command on the service-level resource,
// whose handle offers no list (ST-SUP-RC32-OC15), naming the command and the list.
func (m *model) planLists(cmd *command, byName map[string]types.Entity) error {
	seen := map[string]bool{}
	for _, list := range cmd.decl.Lists {
		if cmd.service != "" {
			return fmt.Errorf("command: the command %s on the service-level resource declares the list %s; its handle offers no list (ST-SUP-RC32-OC15)", cmd.decl.Name, list.Name)
		}
		if !operationName.MatchString(list.Name) || seen[list.Name] {
			return fmt.Errorf("command: the command %s declares the list %q, whose name is not a lower-case name or is given twice", cmd.decl.Name, list.Name)
		}
		seen[list.Name] = true
		if !slices.Contains(cmd.decl.Writes, list.Entity) {
			return fmt.Errorf("command: the list %s of the command %s reads %s, which the command does not write (ST-DECL-INV94)", list.Name, cmd.decl.Name, list.Entity)
		}
		if _, ok := authzschema.LockAtMost(byName[list.Entity]); list.Lock && !ok {
			return fmt.Errorf("command: the list %s of the command %s locks rows of %s, which declares no Lock at most (ST-DECL-INV81)", list.Name, cmd.decl.Name, list.Entity)
		}
		plan, err := ListStatements(list, m.ctx.Entities, m.tenantKey)
		if err != nil {
			return fmt.Errorf("command: the command %s: %w", cmd.decl.Name, err)
		}
		if err := plan.CheckChild("command: the command "+cmd.decl.Name, list.Name, list.Entity, cmd.decl.Writes); err != nil {
			return err
		}
		cmd.lists = append(cmd.lists, &commandList{decl: list, goName: cmd.goName + mixedCase(list.Name), plan: plan})
	}
	return nil
}

// commandRoutePath is the form of a command route's path: segments of letters, digits and the URL characters
// - . _ ~ : @, or a {name} path parameter. The path is written into Go comments and strings, so it holds no space,
// quote, backslash, control character or other character that could end them.
var commandRoutePath = regexp.MustCompile(`^(/([A-Za-z0-9._~:@-]+|\{[A-Za-z_][A-Za-z0-9_]*\}))+$`)

// planRoute checks the command's route, finds its messages and binds each path parameter to a string field of the
// request message (ST-SUP-RC32-OC11).
func (m *model) planRoute(cmd *command) error {
	decl := cmd.decl
	verb, routePath, ok := strings.Cut(decl.Route, " ")
	if !ok || verb != "POST" || !commandRoutePath.MatchString(routePath) {
		return fmt.Errorf("command: the command %s has the route %q; a command route is POST and a path", decl.Name, decl.Route)
	}
	cmd.pattern = "POST " + m.basePath + routePath
	var err error
	if cmd.request, err = m.messages.find(cmd.goName + "Request"); err != nil {
		return fmt.Errorf("command: the command %s: %w", decl.Name, err)
	}
	if cmd.response, err = m.messages.find(cmd.goName + "Response"); err != nil {
		return fmt.Errorf("command: the command %s: %w", decl.Name, err)
	}
	segments := strings.Split(strings.Trim(routePath, "/"), "/")
	for _, segment := range segments {
		if !strings.HasPrefix(segment, "{") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}")
		f := cmd.request.field(name)
		if f == nil || !f.isString() {
			return fmt.Errorf("command: the route of %s has the path parameter %s, which is not a string field of the message %s", decl.Name, name, cmd.request.name)
		}
		// A path parameter goes in the URL, the access logs and a CLI flag, so it cannot hold a secret; this holds
		// for each archetype that mounts the command routes, also one with no rest-api (ST-SUP-RC20-OC54, security
		// review of PR #462, L2).
		if f.secret {
			return fmt.Errorf("command: the route of %s has the path parameter %s, and the proto file marks the member %s of %s debug_redact: a secret member cannot be a path parameter, because the URL goes in access logs and a CLI flag; give it in the body", decl.Name, name, f.protoName, cmd.request.name)
		}
		cmd.params = append(cmd.params, param{name: name, field: f})
	}
	// A POST to a collection path creates a resource and answers 201; a POST to one resource answers 200.
	last := segments[len(segments)-1]
	cmd.creates = !strings.HasPrefix(last, "{") && !strings.Contains(last, ":")
	return nil
}

// planPermission checks the command's permission and finds its resource: the entity whose resource type the
// command names, and the request field that the resource member names (ST-SUP-RC32-OC06, ST-SUP-RP10-LC31).
func (m *model) planPermission(cmd *command, byName map[string]types.Entity) error {
	decl := cmd.decl
	if decl.Permission == "" || decl.ResourceType == "" {
		return fmt.Errorf("command: the command %s needs its permission and its resource type, which the authorization of the service makes required", decl.Name)
	}
	if !identifier.MatchString(decl.Permission) || !identifier.MatchString(decl.ResourceType) {
		return fmt.Errorf("command: the command %s has the permission %q on %q, which are not names of the schema", decl.Name, decl.Permission, decl.ResourceType)
	}
	if p := m.ctx.Tenancy.Provisioning; p != nil && p.ResourceType != "" && decl.ResourceType == p.ResourceType {
		return m.planServiceLevel(cmd, *p)
	}
	if decl.ResourceMember == "" {
		return fmt.Errorf("command: the command %s needs its resource member, which the authorization of the service makes required", decl.Name)
	}
	var owner *types.Entity
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if e := byName[name]; e.ResourceType == decl.ResourceType {
			if owner != nil {
				return fmt.Errorf("command: the resource type %s of the command %s is the resource type of two entities, %s and %s", decl.ResourceType, decl.Name, owner.Name, e.Name)
			}
			e := e
			owner = &e
		}
	}
	if owner == nil {
		return fmt.Errorf("command: no entity has the resource type %s of the command %s, so its row token cannot be read", decl.ResourceType, decl.Name)
	}
	if owner.Tenant != "" && owner.Tenant != "owned" {
		return fmt.Errorf("command: the resource of the command %s is the entity %s, which is not owned by a tenant", decl.Name, owner.Name)
	}
	cmd.resource = &resource{resourceType: decl.ResourceType, table: tableName(owner.Name), keyCast: "uuid"}
	// The runtime checks the token statement at start against one form of plain identifiers (ST-SUP-RP10-LC31).
	if !sqlName.MatchString(cmd.resource.table) {
		return fmt.Errorf("command: the resource table %q of the command %s is not a plain SQL name", cmd.resource.table, decl.Name)
	}
	f := cmd.request.field(decl.ResourceMember)
	if f == nil || !f.isString() {
		return fmt.Errorf("command: the resource member %s of the command %s is not a string field of the message %s", decl.ResourceMember, decl.Name, cmd.request.name)
	}
	cmd.member = f
	return nil
}

// planServiceLevel checks a command whose resource type is the resource type of the tenant provisioning
// (ST-SUP-RC32-OC15): it names no resource member, and Run decides its permission on the service-level resource
// <resource type>:<service name> in the tenant of the request, with no row to read, as a CRUD create of an entity
// with no parent is decided (ST-SUP-RC29-OC01). Its handle offers each written entity only the create and the read,
// so it is refused when an entity that it writes has a parent, whose permission the decision does not cover, and
// when it writes a relation, which Relate decides on a row with the command's permission of another resource type.
// A permission of Provision or Retire is decided with no tenant, so it is refused here (ST-SUP-Q127).
func (m *model) planServiceLevel(cmd *command, p types.TenantProvisioning) error {
	decl := cmd.decl
	if decl.ResourceMember != "" {
		return fmt.Errorf("command: the command %s names the resource member %s, but its resource type %s is the resource type of the tenant provisioning, so it is decided on the service-level resource and names no resource member (ST-SUP-RC32-OC15)", decl.Name, decl.ResourceMember, decl.ResourceType)
	}
	if decl.Permission == p.Provision || decl.Permission == p.Retire {
		return fmt.Errorf("command: the command %s names the permission %s, which the tenant provisioning names for Provision or Retire, which are decided with no tenant; declare a permission of its own (ST-SUP-RC32-OC15)", decl.Name, decl.Permission)
	}
	if len(decl.RelationshipsWritten) > 0 {
		return fmt.Errorf("command: the command %s is decided on the service-level resource and writes the relation %s; its handle offers only the create, so it writes no relation of its own (ST-SUP-RC32-OC15)", decl.Name, decl.RelationshipsWritten[0])
	}
	for _, e := range cmd.writes {
		if e.resourceType == "" && len(e.references) > 0 {
			return fmt.Errorf("command: the command %s is decided on the service-level resource and writes %s, which has no resource type and takes its authorization from a parent; such a command creates only rows with no parent (ST-SUP-RC32-OC15)", decl.Name, e.decl.Name)
		}
		for _, c := range m.ctx.Collections {
			if c.Entity == e.decl.Name && len(c.Scope) > 0 {
				return fmt.Errorf("command: the command %s is decided on the service-level resource and writes %s, which the collection %s puts under a parent; such a command creates only rows with no parent (ST-SUP-RC32-OC15)", decl.Name, e.decl.Name, c.Name)
			}
		}
	}
	if !serviceName.MatchString(m.ctx.ServiceName) {
		return fmt.Errorf("command: the service name %q of the command %s is not a plain name for the service-level resource", m.ctx.ServiceName, decl.Name)
	}
	cmd.resource = &resource{resourceType: decl.ResourceType}
	cmd.service = m.ctx.ServiceName
	return nil
}

// serviceName is the form of a service name, the ID of the service-level resource: lower-case letters, digits and
// hyphens.
var serviceName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// takeNames refuses two exported names of the contract package that are the same.
func (m *model) takeNames() error {
	take := func(owner string, names ...string) error {
		for _, name := range names {
			if other, ok := m.names[name]; ok {
				return fmt.Errorf("command: %s and %s both need the Go name %s", other, owner, name)
			}
			m.names[name] = owner
		}
		return nil
	}
	if err := take("the refusal type", "Refusal"); err != nil {
		return err
	}
	for _, name := range m.messages.goNames() {
		if err := take("the message "+name, name); err != nil {
			return err
		}
	}
	for _, e := range m.entities {
		if err := take("the entity "+e.decl.Name, e.goName, e.goName+"Entity", e.goName+"Repository", "Loaded"+e.goName); err != nil {
			return err
		}
	}
	for _, c := range m.commands {
		if err := take("the command "+c.decl.Name, c.goName+"Handle", "New"+c.goName+"Handle", c.goName+"Fill"); err != nil {
			return err
		}
		for _, r := range c.refusals {
			if err := take("the refusal "+r.name+" of "+c.decl.Name, r.goName); err != nil {
				return err
			}
		}
		for _, h := range c.helpers {
			if err := take("the relation "+h.resourceType+"#"+h.relation+" of "+c.decl.Name, h.relate, h.unrelate); err != nil {
				return err
			}
		}
		for _, l := range c.lists {
			if err := take("the list "+l.decl.Name+" of "+c.decl.Name, l.goName, l.goName+"With"); err != nil {
				return err
			}
		}
		for _, r := range c.resets {
			if err := take("the reset of "+r.entity.decl.Name+"."+r.field.decl.Name+" of "+c.decl.Name, r.name); err != nil {
				return err
			}
		}
	}
	return nil
}

// writer is a source buffer.
type writer struct{ bytes.Buffer }

func (w *writer) f(format string, args ...any) { fmt.Fprintf(&w.Buffer, format, args...) }
