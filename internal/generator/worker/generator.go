// Package worker generates the per-service code of the worker component (ST-SUP-RC35) on the command runtime
// github.com/jsell-rh/stego/registry/runtime/command (ST-SUP-RP10). For each declared worker it emits the worker slot
// contract (the fill type, which gets its input and one command handle only, and the handle), the repository
// declarations of the entities that the workers write, and one runner bound to the worker's one fill: a ticker of
// the command runtime for a worker with an every trigger, under the lease stego.worker.<name> for a single worker,
// and the one destination handler of the consumed destination, which calls RunConsumer, for a worker with a
// consumes trigger. The service runtime starts the runners in the worker mode and stops them at shutdown. The
// handle, the transactions, the decisions, the deadlines and the schedule live in the runtime library; the
// generated code holds what varies with the declaration (ST-STD-RUNTIME-R05).
package worker

import (
	"bytes"
	"fmt"
	"go/format"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/command"
	"github.com/jsell-rh/stego/internal/types"
)

// Generator is the generator of the worker component.
type Generator struct{}

// The slot that each worker fill implements, the components whose output the generated code uses, and the subject
// type of a worker's service account (ST-SUP-RP06-LC07).
const (
	workerSlot     = declaration.WorkerSlot
	probeComponent = "domain-probe"
	runnersFolder  = "runners"
	serviceAccount = "service_account"
	tenantType     = "tenant"
)

// ContractRuntimeImports gives the packages of the runtime library that the worker slot contract out/workers imports
// for the declared entities and workers: the audit package when an entity that a worker writes has a pii or secret
// field (the repository declarations of command.EntityFile), and the command runtime. A fill of the worker slot may
// import them (ST-EXT-INV05, ST-EXT-UC01-R03), so the fill check admits them also before out/workers is generated,
// and admits no package that the contract does not import.
func ContractRuntimeImports(entities []types.Entity, workers []types.Worker) []string {
	var writes []string
	for _, w := range workers {
		writes = append(writes, w.Writes...)
	}
	if command.WritesSensitive(entities, writes) {
		return []string{"github.com/jsell-rh/stego/registry/runtime/audit", "github.com/jsell-rh/stego/registry/runtime/command"}
	}
	return []string{"github.com/jsell-rh/stego/registry/runtime/command"}
}

// The grammars of the declared names that the generated code holds.
var (
	workerName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	fillName   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	effectName = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,62}$`)
	every      = regexp.MustCompile(`^every ([0-9]+)(s|m|h)$`)
)

// MinimumGoVersion is the Go version of the runtime module that the generated code imports.
func (*Generator) MinimumGoVersion() string { return "1.26.11" }

// ConsumedSlots is empty: a worker fill is bound by the worker's fill, not by a slot binding.
func (*Generator) ConsumedSlots() []string { return []string{} }

// ValidateContext checks the declared workers before any code is rendered.
func (*Generator) ValidateContext(ctx gen.Context) error {
	_, err := plan(ctx)
	return err
}

// Generate emits the worker contract package and its runners package in the component's output namespace.
func (*Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	m, err := plan(ctx)
	if err != nil {
		return nil, nil, err
	}
	if m == nil {
		return nil, &gen.Wiring{}, nil
	}
	entities, err := command.EntityFile(m.pkg, m.entityNames, ctx.Entities, m.tenantKey, m.relations)
	if err != nil {
		return nil, nil, fmt.Errorf("worker: %w", err)
	}
	var files []gen.File
	for _, part := range []struct {
		file   string
		render func(*model) []byte
	}{
		{"workers.go", renderContract},
		{path.Join(runnersFolder, "runners.go"), renderRunners},
		{path.Join(runnersFolder, "fills.go"), renderFills},
	} {
		source, err := format.Source(part.render(m))
		if err != nil {
			return nil, nil, fmt.Errorf("worker: formatting %s: %w", part.file, err)
		}
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, part.file), Content: source})
	}
	files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, "entities.go"), Content: entities})
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	return files, &gen.Wiring{}, nil
}

// model is what the generator renders.
type model struct {
	ctx            gen.Context
	pkg            string // the contract package name
	contractImport string
	probeImport    string // the domain probes package, or empty when the service has none
	tenantKey      string
	relations      command.Relations
	entityNames    []string // the entities that a worker writes, in declaration order
	workers        []*worker
	destinations   map[string]delivery
}

// delivery is the declared delivery of one outbox destination.
type delivery struct{ mustLand, idempotent, consumed bool }

// worker is one checked worker.
type worker struct {
	decl     types.Worker
	goName   string
	interval time.Duration // the interval of an every trigger, or zero
	consumes string        // the consumed destination of a consumes trigger, or empty
	single   bool
	leaseTTL time.Duration
	// effects are the destinations of the declared effects in declaration order, and effectNames the effect names
	// of each.
	effects     []string
	effectNames map[string][]string
	// stagers maps each command or worker that stages for the consumed destination to its own declared permission
	// (ST-SUP-RP10-LC17).
	stagers map[string]string
	// lists are the declared lists of the worker, which the contract emits from the entity of the release
	// (ST-SUP-RC35-OC15).
	lists []*workerList
}

// workerList is one declared list of a worker with its generated statements and values.
type workerList struct {
	decl   types.WorkerList
	goName string // the exported name in the contract, such as StaleScanOpenTickets
	plan   command.ListPlan
}

// plan checks the context and builds the model. It refuses, naming the worker, each declaration that the generated
// code or the runtime's checks would refuse, so the emitted code never fails at start. It gives no model for a
// service that declares no worker.
func plan(ctx gen.Context) (*model, error) {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return nil, err
	}
	for key := range ctx.ComponentConfig {
		return nil, fmt.Errorf("worker: the component has no setting %s", key)
	}
	if len(ctx.Workers) == 0 {
		return nil, nil
	}
	m := &model{ctx: ctx, pkg: path.Base(ctx.OutputNamespace),
		contractImport: path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace), destinations: map[string]delivery{}}
	if ns := ctx.PeerNamespaces[probeComponent]; ns != "" {
		m.probeImport = path.Join(ctx.ModuleName, ctx.OutDirName, ns)
	}
	if ctx.Tenancy == nil || ctx.Tenancy.Key != "tenant_id" {
		return nil, fmt.Errorf("worker: a worker runs in the transaction of one tenant, so the service needs pooled tenancy with the tenant column tenant_id")
	}
	m.tenantKey = ctx.Tenancy.Key
	if ctx.Authorization == nil || ctx.Authorization.SubjectFromIdentity == "" {
		return nil, fmt.Errorf("worker: a worker decides its permission on each resource it touches, so the service needs authorization")
	}
	for _, d := range ctx.Destinations {
		if d.MustLand == nil || d.Idempotent == nil {
			return nil, fmt.Errorf("worker: the destination %s of the component %s does not declare must_land and idempotent (ST-SUP-RC32-OC12)", d.Name, d.Component)
		}
		m.destinations[d.Name] = delivery{mustLand: *d.MustLand, idempotent: *d.Idempotent, consumed: d.Component == command.ConsumedComponent}
	}
	var err error
	if m.relations, err = command.SchemaRelations(ctx); err != nil {
		return nil, fmt.Errorf("worker: %w", err)
	}
	declared := map[string]bool{}
	for _, e := range ctx.Entities {
		declared[e.Name] = true
	}
	writes := map[string]bool{}
	for _, w := range ctx.Workers {
		p, err := m.planWorker(w, declared)
		if err != nil {
			return nil, err
		}
		for _, name := range w.Writes {
			writes[name] = true
		}
		m.workers = append(m.workers, p)
	}
	for _, e := range ctx.Entities {
		if writes[e.Name] {
			m.entityNames = append(m.entityNames, e.Name)
		}
	}
	if err := m.planStagers(); err != nil {
		return nil, err
	}
	if err := m.checkChangeEvents(); err != nil {
		return nil, err
	}
	if err := m.checkTenantRelations(); err != nil {
		return nil, err
	}
	return m, m.checkNames()
}

// checkChangeEvents refuses, for a service with the kafka-producer component, a service without the outbox
// component, a written entity whose change event name Kafka refuses, and a declared effect named as a change event of
// the service, which only the handle stages (ST-SUP-RC35-OC14, ST-SUP-RP10-LC36).
func (m *model) checkChangeEvents() error {
	if !command.ChangeEvents(m.ctx) {
		return nil
	}
	if err := command.CheckChangeEvents(m.ctx, "worker", "ST-SUP-RC35-OC14", m.entityNames); err != nil {
		return err
	}
	events := map[string]bool{}
	for _, e := range m.ctx.Entities {
		for _, action := range []string{command.EventCreated, command.EventUpdated, command.EventDeleted} {
			events[command.ChangeEventType(m.ctx.ServiceName, e.Name, action)] = true
		}
	}
	for _, wk := range m.workers {
		for _, d := range wk.effects {
			for _, name := range wk.effectNames[d] {
				if events[name] {
					return fmt.Errorf("worker: the worker %s declares the effect %s, which is the name of a change event that the handle stages for each write (ST-SUP-RC35-OC14)", wk.decl.Name, name)
				}
			}
		}
	}
	return nil
}

// planWorker checks one worker: its one fill of the worker slot (ST-SUP-RC35-OC02), its trigger, its writes, its
// permission and its effects.
func (m *model) planWorker(w gen.Worker, declared map[string]bool) (*worker, error) {
	decl := w.Worker
	if !workerName.MatchString(decl.Name) {
		return nil, fmt.Errorf("worker: the worker name %q is not a lower-case name of letters, digits and hyphens", decl.Name)
	}
	p := &worker{decl: decl, goName: mixedCase(decl.Name), effectNames: map[string][]string{}}
	if len(decl.Fill) != 1 {
		return nil, fmt.Errorf("worker: the worker %s names %d fills; it needs exactly one fill of the slot %s", decl.Name, len(decl.Fill), workerSlot)
	}
	if w.FillSlot != workerSlot {
		found := w.FillSlot
		if found == "" {
			found = "no slot"
		}
		return nil, fmt.Errorf("worker: the worker %s names the fill %s, which implements %s, not the worker slot %s", decl.Name, decl.Fill[0], found, workerSlot)
	}
	if !fillName.MatchString(decl.Fill[0]) {
		return nil, fmt.Errorf("worker: the fill %q of the worker %s is not a valid fill folder name", decl.Fill[0], decl.Name)
	}
	if match := every.FindStringSubmatch(decl.Trigger); match != nil {
		count, err := strconv.Atoi(match[1])
		unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour}[match[2]]
		if err != nil || count <= 0 || count > int(24*time.Hour/unit) {
			return nil, fmt.Errorf("worker: the worker %s has the trigger %q, whose interval is not from 1s to 24h", decl.Name, decl.Trigger)
		}
		p.interval = time.Duration(count) * unit
		p.single = declaration.WorkerSingle(decl)
		if p.single {
			if p.leaseTTL, err = time.ParseDuration(declaration.WorkerLeaseTTL(decl)); err != nil || p.leaseTTL < 15*time.Second || p.leaseTTL > 5*time.Minute || p.leaseTTL%time.Second != 0 {
				return nil, fmt.Errorf("worker: the worker %s has the lease TTL %q, which is not whole seconds from 15s to 5m", decl.Name, decl.LeaseTTL)
			}
		}
	} else if destination, ok := strings.CutPrefix(decl.Trigger, "consumes "); ok && workerName.MatchString(destination) {
		p.consumes = destination
	} else {
		return nil, fmt.Errorf("worker: the worker %s has the trigger %q; a trigger is every <interval> or consumes <destination>", decl.Name, decl.Trigger)
	}
	if len(decl.Writes) == 0 {
		return nil, fmt.Errorf("worker: the worker %s writes no entity", decl.Name)
	}
	for _, name := range decl.Writes {
		if !declared[name] {
			return nil, fmt.Errorf("worker: the worker %s writes the entity %s, which the declaration does not declare", decl.Name, name)
		}
	}
	if !identifier.MatchString(decl.Permission) || !identifier.MatchString(decl.ResourceType) {
		return nil, fmt.Errorf("worker: the worker %s needs its permission and its resource type, as names of the schema", decl.Name)
	}
	named := map[string]bool{}
	for _, effect := range decl.Effects {
		if !effectName.MatchString(effect.Name) || named[effect.Destination+"/"+effect.Name] {
			return nil, fmt.Errorf("worker: the worker %s declares the effect %q, whose name is missing, not a lower-case name or given twice", decl.Name, effect.String())
		}
		named[effect.Destination+"/"+effect.Name] = true
		if _, ok := m.destinations[effect.Destination]; !ok || effect.Destination == "authz" || effect.Destination == "audit" {
			return nil, fmt.Errorf("worker: the worker %s stages for the destination %q, which no component declares and no worker consumes", decl.Name, effect.Destination)
		}
		if len(p.effectNames[effect.Destination]) == 0 {
			p.effects = append(p.effects, effect.Destination)
		}
		p.effectNames[effect.Destination] = append(p.effectNames[effect.Destination], effect.Name)
	}
	if err := m.planLists(p); err != nil {
		return nil, err
	}
	return p, nil
}

// planLists builds the statements of each declared list of the worker from the entity of the release
// (ST-SUP-RC35-OC15), and refuses a list that checking refuses (ST-DECL-INV94), naming the worker and the list.
func (m *model) planLists(p *worker) error {
	byName := map[string]types.Entity{}
	for _, e := range m.ctx.Entities {
		byName[e.Name] = e
	}
	seen := map[string]bool{}
	for _, list := range p.decl.Lists {
		if !workerName.MatchString(list.Name) || seen[list.Name] {
			return fmt.Errorf("worker: the worker %s declares the list %q, whose name is not a lower-case name or is given twice", p.decl.Name, list.Name)
		}
		seen[list.Name] = true
		if !slices.Contains(p.decl.Writes, list.Entity) {
			return fmt.Errorf("worker: the list %s of the worker %s reads %s, which the worker does not write (ST-DECL-INV94)", list.Name, p.decl.Name, list.Entity)
		}
		if _, ok := declaration.LockAtMost(byName[list.Entity]); list.Lock && !ok {
			return fmt.Errorf("worker: the list %s of the worker %s locks rows of %s, which declares no Lock at most (ST-DECL-INV81)", list.Name, p.decl.Name, list.Entity)
		}
		plan, err := command.ListStatements(list, m.ctx.Entities, m.ctx.Tenancy.Key)
		if err != nil {
			return fmt.Errorf("worker: the worker %s: %w", p.decl.Name, err)
		}
		p.lists = append(p.lists, &workerList{decl: list, goName: p.goName + mixedCase(list.Name), plan: plan})
	}
	return nil
}

// planStagers finds, for each consuming worker, each command and worker that stages for its destination with its
// own declared permission, which RunConsumer decides as the staging principal (ST-SUP-RP10-LC17).
func (m *model) planStagers() error {
	for _, w := range m.workers {
		if w.consumes == "" {
			continue
		}
		w.stagers = map[string]string{}
		for _, c := range m.ctx.Commands {
			for _, effect := range c.Effects {
				if effect.Destination == w.consumes && c.Permission != "" {
					w.stagers[c.Name] = c.Permission
				}
			}
		}
		for _, other := range m.workers {
			for _, effect := range other.decl.Effects {
				if effect.Destination == w.consumes {
					w.stagers[other.decl.Name] = other.decl.Permission
				}
			}
		}
	}
	return nil
}

// checkTenantRelations refuses a schema whose type tenant lacks the relation worker_<name in snake case> of the
// subject type service_account for a declared worker, naming the relation (ST-SUP-RC35-OC13).
func (m *model) checkTenantRelations() error {
	setting := m.ctx.Authorization
	if setting.SchemaPath == "" {
		return nil
	}
	source, ok := m.ctx.Inputs[setting.SchemaPath]
	if !ok {
		return fmt.Errorf("worker: the schema %q that the authorization setting names is not among the inputs of the generator", setting.SchemaPath)
	}
	schema, err := declaration.CompileAuthzSchema(setting.SchemaPath, source)
	if err != nil {
		return fmt.Errorf("worker: the schema %q does not compile: %w", setting.SchemaPath, err)
	}
	for _, w := range m.workers {
		relation := TenantRelation(w.decl.Name)
		if !schema.RelationAdmits(tenantType, relation, serviceAccount) {
			return fmt.Errorf("worker: the schema %s has no relation %s#%s of the subject type %s, which holds the tuple of the worker %s in each tenant (ST-SUP-RC35-OC13)", setting.SchemaPath, tenantType, relation, serviceAccount, w.decl.Name)
		}
	}
	return nil
}

// TenantRelation is the relation of the type tenant that holds the tuple of the worker name in each tenant:
// worker_<name in snake case> (ST-SUP-RC35-OC13).
func TenantRelation(name string) string { return "worker_" + strings.ReplaceAll(name, "-", "_") }

// checkNames refuses two exported names of the contract package that are the same.
func (m *model) checkNames() error {
	taken := map[string]string{}
	for _, name := range m.entityNames {
		for _, n := range []string{name, name + "Entity", name + "Repository", "Loaded" + name} {
			taken[n] = "the entity " + name
		}
	}
	taken["Refused"] = "the refusal helper"
	for _, w := range m.workers {
		for _, n := range []string{w.goName + "Handle", w.goName + "Fill"} {
			if other, ok := taken[n]; ok {
				return fmt.Errorf("worker: %s and the worker %s both need the Go name %s", other, w.decl.Name, n)
			}
			taken[n] = "the worker " + w.decl.Name
		}
	}
	for _, w := range m.workers {
		for _, l := range w.lists {
			for _, n := range []string{l.goName, l.goName + "With"} {
				if other, ok := taken[n]; ok {
					return fmt.Errorf("worker: %s and the list %s of the worker %s both need the Go name %s", other, l.decl.Name, w.decl.Name, n)
				}
				taken[n] = "the list " + l.decl.Name + " of the worker " + w.decl.Name
			}
		}
	}
	return nil
}

// mixedCase gives the exported Go name of a lower-case name with hyphens or underscores, as the command and
// domain-probe generators do, such as SyncPartnerOrders for sync-partner-orders.
func mixedCase(name string) string {
	var b strings.Builder
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
		if part == "id" {
			b.WriteString("ID")
			continue
		}
		runes := []rune(part)
		runes[0] = unicode.ToUpper(runes[0])
		b.WriteString(string(runes))
	}
	return b.String()
}

// probeType is the typed probe of a worker: the domain probe of the worker when the service has the domain-probe
// component (ST-SUP-RC34-OC04), and struct{} otherwise.
func (m *model) probeType(w *worker, qualifier string) string {
	if m.probeImport == "" {
		return "struct{}"
	}
	return qualifier + w.goName + "Probe"
}

func (m *model) has(every bool) bool {
	for _, w := range m.workers {
		if (w.interval > 0) == every {
			return true
		}
	}
	return false
}

// timeParams tells whether a declared list takes a parameter of a timestamp field, whose With function takes a
// time.Time.
func (m *model) timeParams() bool {
	for _, wk := range m.workers {
		for _, l := range wk.lists {
			for _, p := range l.plan.Params {
				if p.GoType == "time.Time" {
					return true
				}
			}
		}
	}
	return false
}

// writer is a source buffer.
type writer struct{ bytes.Buffer }

func (w *writer) f(format string, args ...any) { fmt.Fprintf(&w.Buffer, format, args...) }

// renderContract writes the worker slot contract package, which a worker fill imports (ST-SUP-RC35-OC01): for each
// worker its handle and its fill type. A fill of a worker with an every trigger gets the tick time and one command
// handle; a fill of a worker with a consumes trigger gets the message as JSON, its effect name and one command
// handle; neither gets anything else that reaches storage, authorization, audit, the outbox or the network.
func renderContract(m *model) []byte {
	w := &writer{}
	w.f("// Package %s is the worker slot contract of the service (ST-SUP-RC35-OC01): for each declared worker the fill\n", m.pkg)
	w.f("// type, which takes its input and one command handle only, and the handle, with the repositories of the entities\n// that the workers write. A worker fill imports this package only.\n")
	w.f("package %s\n\nimport (\n\t\"context\"\n", m.pkg)
	if m.has(false) {
		w.f("\t\"encoding/json\"\n")
	}
	if m.has(true) || m.timeParams() {
		w.f("\t\"time\"\n")
	}
	w.f("\n\t\"github.com/jsell-rh/stego/registry/runtime/command\"\n")
	if m.probeImport != "" {
		w.f("\n\tprobes %q\n", m.probeImport)
	}
	w.f(")\n\n")
	for _, wk := range m.workers {
		w.f("// %sHandle is the command handle that the fill of %s gets: it is bound to the service account\n// service_account:%s and reaches only the entities and effect destinations that the worker declares.\n", wk.goName, wk.decl.Name, wk.decl.Name)
		w.f("type %sHandle = command.Handle[%s]\n\n", wk.goName, m.probeType(wk, "probes."))
		if wk.interval > 0 {
			w.f("// %sFill is the worker slot of %s, which runs every %s: the fill gets the scheduled tick time and one\n// command handle, once for each tenant of the tick in a transaction of that tenant.\n", wk.goName, wk.decl.Name, wk.interval)
			w.f("type %sFill func(ctx context.Context, tick time.Time, h *%sHandle) error\n\n", wk.goName, wk.goName)
		} else {
			w.f("// %sFill is the worker slot of %s, which consumes %s: the fill gets the message as JSON, its effect name\n// and one command handle, once for each message, in the transaction of the message's tenant.\n", wk.goName, wk.decl.Name, wk.consumes)
			w.f("type %sFill func(ctx context.Context, message json.RawMessage, effect string, h *%sHandle) error\n\n", wk.goName, wk.goName)
		}
	}
	for _, wk := range m.workers {
		for _, l := range wk.lists {
			command.RenderList(&w.Buffer, l.goName, l.decl.Name, "the worker "+wk.decl.Name, l.decl.Entity, l.plan)
		}
	}
	w.f("// Refused marks err as a declared refusal of a consuming worker's fill: the message is then parked, since a\n// delivery again gives the same refusal (ST-SUP-RP10-LC26).\nfunc Refused(err error) error { return command.Refused(err) }\n")
	return w.Bytes()
}

// renderRunners writes the runners package: the worker declarations with their permission, interval, writes and
// effect destinations, the ticker of each worker with an every trigger (ST-SUP-RC35-OC04, ST-SUP-RC35-OC05) and the
// one destination handler of each worker with a consumes trigger (ST-SUP-RC35-OC06), which the service runtime starts
// in the worker mode and stops once (ST-SUP-RC35-OC11).
func renderRunners(m *model) []byte {
	w := &writer{}
	w.f("// Package %s holds the generated runners of the workers of the service (ST-SUP-RC35-OC02): one runner for each\n", runnersFolder)
	w.f("// declared worker, bound to the one fill that the worker names, which the service runtime starts in the worker\n// mode and stops at shutdown.\n")
	w.f("package %s\n\nimport (\n", runnersFolder)
	if m.has(false) {
		w.f("\t\"context\"\n\t\"database/sql\"\n\t\"encoding/json\"\n\t\"errors\"\n")
	}
	// Only a worker with an every trigger needs time and lease: its interval and its lease TTL (ST-SUP-RC35-OC04).
	if m.has(true) {
		w.f("\t\"time\"\n")
	}
	w.f("\n\t\"github.com/jsell-rh/stego/registry/runtime/command\"\n")
	if m.has(true) {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/lease\"\n")
	}
	w.f("\t\"github.com/jsell-rh/stego/registry/runtime/outbox\"\n")
	if m.probeImport == "" {
		w.f("\t\"github.com/jsell-rh/stego/registry/runtime/probe\"\n")
	}
	w.f("\t\"github.com/jsell-rh/stego/registry/runtime/service\"\n\n\t%s %q\n", m.pkg, m.contractImport)
	if m.probeImport != "" {
		w.f("\tprobes %q\n", m.probeImport)
	}
	w.f(")\n\n")
	w.f("// Entities are the entities that the workers write, for the command runtime of the workers.\nfunc Entities() []command.Declared {\n\treturn []command.Declared{")
	for i, name := range m.entityNames {
		if i > 0 {
			w.f(", ")
		}
		w.f("%s.%sEntity", m.pkg, name)
	}
	w.f("}\n}\n\n")
	w.f("// Names are the names of the declared workers, the values of the field worker of the built-in worker\n// observations and the subjects of the worker tuples of each tenant (ST-SUP-RC35-OC13).\nfunc Names() []string {\n\treturn []string{")
	for i, wk := range m.workers {
		if i > 0 {
			w.f(", ")
		}
		w.f("%q", wk.decl.Name)
	}
	w.f("}\n}\n\n")
	w.f("// Leases are the names of the single workers, whose lease rows stego.worker.<name> the migration job makes.\nfunc Leases() []string {\n\treturn []string{")
	first := true
	for _, wk := range m.workers {
		if wk.single {
			if !first {
				w.f(", ")
			}
			first = false
			w.f("%q", wk.decl.Name)
		}
	}
	w.f("}\n}\n\n")
	w.f("// ConsumerTables are the tables of the entities that the workers with a consumes trigger write, which the migration\n// job grants the worker role SELECT, INSERT and UPDATE on (decision on ST-SUP-Q141).\nfunc ConsumerTables() []string {\n\treturn []string{")
	seen := map[string]bool{}
	for _, name := range m.entityNames {
		for _, wk := range m.workers {
			if wk.consumes != "" && slices.Contains(wk.decl.Writes, name) && !seen[name] {
				if len(seen) > 0 {
					w.f(", ")
				}
				seen[name] = true
				w.f("%q", command.TableName(name))
			}
		}
	}
	w.f("}\n}\n\n")
	w.f("// Fills holds the one fill of each worker.\ntype Fills struct {\n")
	for _, wk := range m.workers {
		w.f("\t// %s is the fill %s of the worker %s.\n\t%s %s.%sFill\n", wk.goName, wk.decl.Fill[0], wk.decl.Name, wk.goName, m.pkg, wk.goName)
	}
	w.f("}\n\n")
	if m.probeImport == "" {
		w.f("// noProbe is the typed probe of a service that has no domain probes.\nfunc noProbe(*probe.Probe) struct{} { return struct{}{} }\n\n")
	}
	for _, wk := range m.workers {
		renderWorker(w, m, wk)
	}
	w.f("// Tickers gives the ticker of each worker with an every trigger, bound to its fill of fills, which the worker mode\n// runs (ST-SUP-RC35-OC04).\nfunc Tickers(fills Fills) func(env service.TickerEnv) ([]*command.Ticker, error) {\n")
	w.f("\treturn func(env service.TickerEnv) ([]*command.Ticker, error) {\n\t\tvar tickers []*command.Ticker\n")
	for _, wk := range m.workers {
		if wk.interval == 0 {
			continue
		}
		lower := lowerFirst(wk.goName)
		probe := m.probeType(wk, "probes.")
		w.f("\t\t{\n\t\t\tvar client *lease.Client\n")
		if wk.single {
			w.f("\t\t\t// One instance runs %s at a time, under the lease %s%s (ST-SUP-RC35-OC05).\n", wk.decl.Name, "stego.worker.", wk.decl.Name)
			w.f("\t\t\tvar err error\n\t\t\tif client, err = env.Lease(%q, %d*time.Second); err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n", wk.decl.Name, int(wk.leaseTTL/time.Second))
		}
		w.f("\t\t\tt, err := command.NewTicker(env.Runtime, command.TickerSpec[%s]{Worker: %sWorker, Fill: command.TickFill[%s](fills.%s),\n", probe, lower, probe, wk.goName)
		w.f("\t\t\t\tTenants: env.Tenants, Lease: client, Observe: env.Observe(%q), Clock: env.Clock})\n", wk.decl.Name)
		w.f("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n\t\t\ttickers = append(tickers, t)\n\t\t}\n")
	}
	w.f("\t\treturn tickers, nil\n\t}\n}\n\n")
	w.f("// Consumers registers on the outbox worker the one destination handler of each worker with a consumes trigger,\n// bound to its fill of fills (ST-SUP-RC35-OC06).\nfunc Consumers(fills Fills) func(rt *command.Runtime, worker *outbox.Worker, observe func(name string) command.WorkerObserver) error {\n")
	w.f("\treturn func(rt *command.Runtime, worker *outbox.Worker, observe func(name string) command.WorkerObserver) error {\n")
	for _, wk := range m.workers {
		if wk.consumes == "" {
			continue
		}
		lower := lowerFirst(wk.goName)
		probe := m.probeType(wk, "probes.")
		w.f("\t\t{\n\t\t\tfill := command.MessageFill[json.RawMessage, %s](fills.%s)\n", probe, wk.goName)
		w.f("\t\t\t// %s is the one handler of %s: idempotent, must land, and only for an active tenant, so the message\n\t\t\t// of a retired tenant is parked before the fill (ST-SUP-RC27-OC12).\n", wk.decl.Name, wk.consumes)
		w.f("\t\t\tif err := worker.Register(outbox.Destination{Name: %q, Idempotent: true, MustLand: true, ActiveTenants: true,\n", wk.consumes)
		w.f("\t\t\t\tHandler: func(ctx context.Context, tx *sql.Tx, d outbox.Delivery) error {\n")
		w.f("\t\t\t\t\t// Each message has its own deadline (ST-SUP-RC35-OC07).\n\t\t\t\t\tctx, cancel := context.WithTimeout(ctx, command.RunBound)\n\t\t\t\t\tdefer cancel()\n")
		w.f("\t\t\t\t\t// A run that the database refuses for a missing privilege is reported as not_granted (ST-SUP-Q141).\n")
		w.f("\t\t\t\t\treturn command.ObserveConsumer(ctx, observe(%q), command.RunConsumer(ctx, rt, %sWorker, tx, d, decodeMessage, fill))\n\t\t\t\t}}); err != nil {\n\t\t\t\treturn err\n\t\t\t}\n\t\t}\n", wk.decl.Name, lower)
	}
	w.f("\t\treturn nil\n\t}\n}\n")
	if m.has(false) {
		w.f("\n// decodeMessage gives the payload of a consumed message as JSON, and refuses a payload that is not one JSON value.\nfunc decodeMessage(payload []byte) (json.RawMessage, error) {\n\tif !json.Valid(payload) {\n\t\treturn nil, errors.New(\"the payload is not one JSON value\")\n\t}\n\treturn json.RawMessage(payload), nil\n}\n")
	}
	return w.Bytes()
}

// renderWorker writes the declaration of one worker (ST-SUP-RP10-LC16, ST-SUP-RP10-LC17).
func renderWorker(w *writer, m *model, wk *worker) {
	lower := lowerFirst(wk.goName)
	w.f("// %sWorker is the declaration of the worker %s.\n", lower, wk.decl.Name)
	w.f("var %sWorker = command.Must(command.NewWorker(command.WorkerSpec[%s]{\n", lower, m.probeType(wk, "probes."))
	w.f("\tName: %q,\n\tPermission: %q,\n", wk.decl.Name, wk.decl.Permission)
	if wk.interval > 0 {
		w.f("\tInterval: %d * time.Second,\n", int(wk.interval/time.Second))
	}
	w.f("\tWrites: []command.Declared{")
	for i, name := range wk.decl.Writes {
		if i > 0 {
			w.f(", ")
		}
		w.f("%s.%sEntity", m.pkg, name)
	}
	w.f("},\n")
	// With kafka-producer, the handle stages the change event of each write in RunWorker and RunConsumer
	// (ST-SUP-RC35-OC14).
	w.f("%s", command.ChangeEventsSpec(m.ctx, wk.decl.Writes))
	if len(wk.lists) > 0 {
		// The handle takes only the declared lists (ST-SUP-RP10-LC38).
		w.f("\tLists: []command.AnyList{")
		for i, l := range wk.lists {
			if i > 0 {
				w.f(", ")
			}
			w.f("%s.%s", m.pkg, l.goName)
		}
		w.f("},\n")
	}
	if len(wk.effects) > 0 {
		w.f("\tDestinations: []command.Destination{")
		for i, d := range wk.effects {
			if i > 0 {
				w.f(", ")
			}
			declared := m.destinations[d]
			w.f("{Name: %q, MustLand: %t, Idempotent: %t, ", d, declared.mustLand, declared.idempotent)
			if declared.consumed {
				w.f("Consumed: true, ")
			}
			w.f("Effects: []string{")
			for j, name := range wk.effectNames[d] {
				if j > 0 {
					w.f(", ")
				}
				w.f("%q", name)
			}
			w.f("}}")
		}
		w.f("},\n")
	}
	if wk.consumes != "" {
		w.f("\tStagers: map[string]string{")
		names := make([]string, 0, len(wk.stagers))
		for name := range wk.stagers {
			names = append(names, name)
		}
		sort.Strings(names)
		for i, name := range names {
			if i > 0 {
				w.f(", ")
			}
			w.f("%q: %q", name, wk.stagers[name])
		}
		w.f("},\n")
	}
	if m.probeImport != "" {
		w.f("\tProbe: probes.New%sProbe,\n", wk.goName)
	} else {
		w.f("\tProbe: noProbe,\n")
	}
	w.f("}))\n\n")
}

// renderFills writes the binding of each worker to the one fill that it names (ST-SUP-RC35-OC02): the fill folder
// fills/<fill> of the project is a Go package whose New gives the worker's fill type, so a fill of another slot
// does not compile here.
func renderFills(m *model) []byte {
	w := &writer{}
	w.f("package %s\n\nimport (\n", runnersFolder)
	seen := map[string]bool{}
	for _, wk := range m.workers {
		name := wk.decl.Fill[0]
		if !seen[name] {
			seen[name] = true
			w.f("\t%s %q\n", "fill"+mixedCase(name), m.ctx.ModuleName+"/fills/"+name)
		}
	}
	w.f(")\n\n// BoundFills gives the one fill that each worker names, from its fill folder.\nfunc BoundFills() Fills {\n\treturn Fills{\n")
	for _, wk := range m.workers {
		w.f("\t\t%s: fill%s.New(),\n", wk.goName, mixedCase(wk.decl.Fill[0]))
	}
	w.f("\t}\n}\n")
	return w.Bytes()
}

func lowerFirst(name string) string { return strings.ToLower(name[:1]) + name[1:] }
