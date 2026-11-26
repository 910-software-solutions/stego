package compiler

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/910-software-solutions/stego/internal/declaration"
	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/registry"
	"github.com/910-software-solutions/stego/internal/types"
)

// ArchitectureSchema is the JSON Schema 2020-12 of the architecture model, $id stego:architecture/2, shipped in the
// compiler and never fetched.
//
//go:embed architecture.schema.json
var ArchitectureSchema []byte

// ArchitectureInput is what the architecture model is derived from, and nothing else (ST-GEN-INV11): the
// declaration, the registry parts of the active set and the bound mixins, the assembler's wiring, the platform
// requirements of the same run and the declared authorization schema.
type ArchitectureInput struct {
	Service    *types.ServiceDeclaration
	Archetype  *registry.Archetype
	Components map[string]*registry.Component
	// Names are the components of the active set in the order of the active set.
	Names  []string
	Mixins []*registry.Mixin
	// Wirings are the wiring of each component and of the runtime, from which the routes come.
	Wirings []ComponentWiring
	// Requirements are the bytes of the platform requirements that the run writes, or nil.
	Requirements []byte
	// Schema is the compiled authorization schema the declaration binds, or nil.
	Schema *declaration.AuthzSchema
}

// ArchitectureRefusal is a model that cannot be derived without a guess: the part, the statement it leaves out and
// why (ST-GEN-UC08-R05). Apply then writes no file.
type ArchitectureRefusal struct {
	Err error
}

func (r *ArchitectureRefusal) Error() string {
	return "the architecture model is not derived, and apply writes nothing: " + r.Err.Error()
}

func (r *ArchitectureRefusal) Unwrap() error { return r.Err }

// The fixed reasons of an empty aspect (ST-GEN-UC08-R06).
const (
	reasonNoEntity     = "The declaration states no entity."
	reasonNoOperation  = "The declaration states no collection operation."
	reasonNoNeed       = "No component of the active set states a platform need or an environment variable."
	reasonNoComponents = "The active set holds no component."
)

// DeriveArchitecture builds the architecture model from its inputs alone, in a fixed order, and gives its bytes:
// keys in a fixed order, every list sorted by id except the steps of a flow and the parts of an element that keep
// the declaration's order, two-space indent, LF line ends and one trailing newline, so the same inputs always give
// the same bytes (ST-GEN-DS04). It refuses, naming the part and the statement, a component that states no
// architecture role or a slot that states no phase or operations (ST-GEN-UC08-R05).
func DeriveArchitecture(in ArchitectureInput) ([]byte, error) {
	model, err := deriveModel(in)
	if err != nil {
		return nil, err
	}
	return encodeArchitecture(model)
}

// encodeArchitecture gives the bytes of a model.
func encodeArchitecture(model *ArchitectureModel) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(model); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WriteArchitecture derives the model and checks it against the schema and the references between its elements,
// renders the architecture view from the model alone, and gives the two as the project files
// architecture/architecture.json and architecture/index.html, which the record covers (ST-GEN-CMD05). It runs in
// every apply.
func WriteArchitecture(in ArchitectureInput) ([]gen.File, error) {
	data, err := DeriveArchitecture(in)
	if err != nil {
		return nil, err
	}
	if err := ValidateArchitecture(data); err != nil {
		return nil, fmt.Errorf("the derived architecture model is not valid: %w", err)
	}
	view, err := RenderArchitectureView(data)
	if err != nil {
		return nil, err
	}
	return []gen.File{{Path: ArchitectureFile, Content: data}, {Path: ArchitectureViewFile, Content: view}}, nil
}

// deriver holds the model while it is built.
type deriver struct {
	in      ArchitectureInput
	model   *ArchitectureModel
	service string
	// offers maps each slot name to what offers it: the component or mixin ID, its definition and its place in the
	// order of the offering parts.
	offers map[string]slotOffer
	// handler is the component that serves the collections, or nil.
	handler *registry.Component
	// servers maps each kind of operation of registry.ServedKinds to the component that serves it.
	servers map[string]*registry.Component
	// bindings maps consumer#port to its binding.
	bindings map[string]ArchBinding
	// routes maps an operation ID to its method and path.
	routes map[string][2]string
	// checks maps an operation or action ID to its check.
	checks map[string]ArchCheck
}

// slotOffer is a slot that a component or a mixin offers.
type slotOffer struct {
	by    string
	def   registry.SlotDefinition
	order int
}

func deriveModel(in ArchitectureInput) (*ArchitectureModel, error) {
	svc := in.Service
	d := &deriver{in: in, service: svc.Name, offers: map[string]slotOffer{}, bindings: map[string]ArchBinding{}, routes: map[string][2]string{}, checks: map[string]ArchCheck{}}
	for _, name := range in.Names {
		component := in.Components[name]
		if component == nil {
			return nil, &ArchitectureRefusal{fmt.Errorf("the active set names the component %s, which the registry does not hold", name)}
		}
		if err := registry.CheckPlace(component); err != nil {
			return nil, &ArchitectureRefusal{err}
		}
	}
	for _, mixin := range in.Mixins {
		if err := registry.CheckMixinPlace(mixin); err != nil {
			return nil, &ArchitectureRefusal{err}
		}
	}
	archetypeName, archetypeVersion := svc.Archetype, ""
	if in.Archetype != nil {
		archetypeName, archetypeVersion = in.Archetype.Name, in.Archetype.Version
	}
	d.model = &ArchitectureModel{
		SchemaVersion: ArchitectureSchemaVersion,
		Record:        ArchitectureRecord{Path: ".stego/state.yaml"},
		System: ArchSystem{ID: "system:" + svc.Name, Name: svc.Name, Archetype: archetypeName, ArchetypeVersion: archetypeVersion,
			Language: svc.Language, BasePath: svc.BasePath, Source: ArchSource{Declaration: "name"}},
		Layers: slices.Clone(registry.Layers),
	}
	d.structure()
	if err := d.servedKinds(); err != nil {
		return nil, err
	}
	if err := d.slots(); err != nil {
		return nil, err
	}
	d.dataModel()
	d.routesFromWiring()
	if err := d.needs(); err != nil {
		return nil, err
	}
	if err := d.access(); err != nil {
		return nil, err
	}
	if err := d.plannedReads(); err != nil {
		return nil, err
	}
	d.flows()
	d.actionFlows()
	d.stepLayers()
	d.trust()
	d.compliance()
	d.context()
	d.aspects()
	sortModel(d.model)
	return d.model, nil
}

// componentID gives the ID of a component.
func componentID(name string) string { return "component:" + name }

// componentSource gives the registry part of a component and the field of it.
func componentSource(component *registry.Component, field string) ArchSource {
	return ArchSource{Registry: &ArchRegistry{Part: "component", Name: component.Name, Version: component.Version, Field: field}}
}

// structure builds the containers, components, mixins, ports and bindings.
func (d *deriver) structure() {
	m := d.model
	m.Containers = []ArchContainer{{ID: "container:" + d.service, Name: d.service, Kind: "service", Source: ArchSource{Declaration: "name"}}}
	ports := map[string]*ArchPort{}
	port := func(name string, component *registry.Component, field string) *ArchPort {
		if ports[name] == nil {
			ports[name] = &ArchPort{ID: "port:" + name, Name: name, ProvidedBy: []string{}, RequiredBy: []string{}, Source: componentSource(component, field)}
		}
		return ports[name]
	}
	for order, name := range d.in.Names {
		c := d.in.Components[name]
		ac := ArchComponent{ID: componentID(name), Name: name, Version: c.Version, Stage: string(c.Stage), Layer: c.Architecture.Layer,
			FlowPart: c.Architecture.Flow, Namespace: c.OutputNamespace, Container: "container:" + d.service, Ports: []ArchComponentPort{}, Offers: []ArchOffer{},
			Source: componentSource(c, "architecture")}
		for _, p := range c.Requires {
			ac.Ports = append(ac.Ports, ArchComponentPort{Port: "port:" + p.Name, Direction: "requires", Side: c.Architecture.Ports[p.Name]})
			entry := port(p.Name, c, "requires")
			entry.RequiredBy = append(entry.RequiredBy, ac.ID)
		}
		for _, p := range c.Provides {
			ac.Ports = append(ac.Ports, ArchComponentPort{Port: "port:" + p.Name, Direction: "provides", Side: c.Architecture.Ports[p.Name]})
			entry := port(p.Name, c, "provides")
			entry.ProvidedBy = append(entry.ProvidedBy, ac.ID)
		}
		slices.SortFunc(ac.Ports, func(a, b ArchComponentPort) int { return strings.Compare(a.Port+a.Direction, b.Port+b.Direction) })
		for i, slot := range c.Slots {
			ac.Offers = append(ac.Offers, ArchOffer{Name: slot.Name, Phase: slot.Phase, Operations: slices.Clone(slot.Operations)})
			if _, taken := d.offers[slot.Name]; !taken {
				d.offers[slot.Name] = slotOffer{by: ac.ID, def: slot, order: order*1000 + i}
			}
		}
		m.Components = append(m.Components, ac)
	}
	for order, mixin := range d.in.Mixins {
		am := ArchMixin{ID: "mixin:" + mixin.Name, Name: mixin.Name, Version: mixin.Version, Offers: []ArchOffer{},
			Source: ArchSource{Registry: &ArchRegistry{Part: "mixin", Name: mixin.Name, Version: mixin.Version, Field: "adds_slots"}}}
		for i, slot := range mixin.AddsSlots {
			am.Offers = append(am.Offers, ArchOffer{Name: slot.Name, Phase: slot.Phase, Operations: slices.Clone(slot.Operations)})
			if _, taken := d.offers[slot.Name]; !taken {
				d.offers[slot.Name] = slotOffer{by: am.ID, def: slot, order: (len(d.in.Names)+order)*1000 + i}
			}
		}
		m.Mixins = append(m.Mixins, am)
	}
	for _, entry := range ports {
		slices.Sort(entry.ProvidedBy)
		slices.Sort(entry.RequiredBy)
		m.Ports = append(m.Ports, *entry)
	}
	d.bind()
}

// servedKinds finds, for each kind of operation that the service has, the one active component that states in
// serves that its flows start at it (ST-GEN-UC08-R05, ST-GEN-Q44 (A)). It refuses a kind that the service has with
// no such component or with more than one, naming the kind and the components, and names nothing it guesses. The
// component that serves the collection operations is the handler of their flows.
func (d *deriver) servedKinds() error {
	svc := d.in.Service
	has := map[string]bool{"collection operations": len(svc.Collections) > 0, "commands": len(svc.Commands) > 0,
		"queries": len(svc.Queries) > 0, "workers": len(svc.Workers) > 0}
	d.servers = map[string]*registry.Component{}
	for _, kind := range registry.ServedKinds {
		var serving []string
		for _, name := range d.in.Names {
			if c := d.in.Components[name]; slices.Contains(c.Architecture.Serves, kind) {
				serving = append(serving, name)
			}
		}
		switch {
		case len(serving) > 1:
			return &ArchitectureRefusal{fmt.Errorf("the components %s each state in architecture.serves that the %s start at them; one component serves each kind",
				strings.Join(serving, " and "), kind)}
		case len(serving) == 1:
			d.servers[kind] = d.in.Components[serving[0]]
		case has[kind]:
			return &ArchitectureRefusal{fmt.Errorf("the service has %s, and no component of the active set states in architecture.serves that their request flows start at it", kind)}
		}
	}
	// The derivation knows the request path of the capability components alone, so a command, a query or a worker
	// flow starts only at command, query or worker; another component that states it serves them is refused, as
	// plannedReads refuses a collection handler other than rest-api (security review of #404, M1).
	for kind, name := range map[string]string{"commands": commandComponent, "queries": queryComponent, "workers": workerComponent} {
		if c := d.servers[kind]; c != nil && c.Name != name {
			return &ArchitectureRefusal{fmt.Errorf("the component %s states in architecture.serves that the %s start at it, and the derivation knows the request path of the component %s alone; it cannot state how %s authenticates and authorizes them, so it builds no model",
				c.Name, kind, name, c.Name)}
		}
	}
	d.handler = d.servers["collection operations"]
	// The component that serves a kind of operation of the service is bound to a repository, through which its
	// flows pass (ST-GEN-Q45 (b), ST-GEN-UC08-R05).
	for _, kind := range registry.ServedKinds {
		if c := d.servers[kind]; c != nil && has[kind] && len(d.repositories(c)) == 0 {
			return &ArchitectureRefusal{fmt.Errorf("the component %s serves the %s and is bound to no component of the repository layer; bind its port storage-adapter to the repository that keeps its rows",
				c.Name, kind)}
		}
	}
	// With no component of the store layer, the store step and the store actor come from the store that the
	// repository states, and a repository that states none is refused, as a missing layer is.
	if len(d.byLayer("store")) == 0 {
		for _, kind := range registry.ServedKinds {
			c := d.servers[kind]
			if c == nil || !has[kind] {
				continue
			}
			for _, r := range d.repositories(c) {
				if r.component.Architecture.Store == "" {
					return &ArchitectureRefusal{&registry.PlaceError{Part: "component " + r.component.Name, Statement: "architecture.store",
						Reason: "no component of the store layer is active, and the repository states no store that keeps its rows; give architecture.store, such as PostgreSQL"}}
				}
			}
		}
	}
	return nil
}

// boundRepository is a component of the repository layer that a component is bound to, and the port of the binding.
type boundRepository struct {
	component *registry.Component
	port      string
}

// repositories gives the components of the repository layer that the component is bound to, in the order of its
// required ports.
func (d *deriver) repositories(c *registry.Component) []boundRepository {
	var list []boundRepository
	for _, p := range c.Requires {
		if r, binding := d.provider(c.Name, p.Name); r != nil && r.Architecture.Layer == "repository" {
			list = append(list, boundRepository{r, binding.Port})
		}
	}
	return list
}

// bind gives each required port of each component the component that provides it: the one the declaration binds,
// else the one the archetype binds, else the only provider in the active set. A port with no such provider gets no
// binding.
func (d *deriver) bind() {
	providers := map[string][]string{}
	for _, name := range d.in.Names {
		for _, p := range d.in.Components[name].Provides {
			providers[p.Name] = append(providers[p.Name], name)
		}
	}
	active := func(name string) bool { return slices.Contains(d.in.Names, name) }
	for _, name := range d.in.Names {
		for _, p := range d.in.Components[name].Requires {
			var provider string
			var source ArchSource
			if value, ok := d.in.Service.Overrides[p.Name].(string); ok && active(value) {
				provider, source = value, ArchSource{Declaration: "overrides." + p.Name}
			} else if a := d.in.Archetype; a != nil && active(a.Bindings[p.Name]) {
				provider, source = a.Bindings[p.Name], ArchSource{Registry: &ArchRegistry{Part: "archetype", Name: a.Name, Version: a.Version, Field: "bindings." + p.Name}}
			} else if len(providers[p.Name]) == 1 {
				provider, source = providers[p.Name][0], ArchSource{Wiring: "the only provider of " + p.Name + " in the active set"}
			} else {
				continue
			}
			binding := ArchBinding{ID: "binding:" + name + "." + p.Name, Port: "port:" + p.Name, Provider: componentID(provider), Consumer: componentID(name), Source: source}
			d.bindings[name+"#"+p.Name] = binding
			d.model.Bindings = append(d.model.Bindings, binding)
		}
	}
}

// slots builds the slot bindings of the declaration and their fills.
func (d *deriver) slots() error {
	fills := map[string]*ArchFill{}
	fill := func(name, path, implements string) {
		if fills[name] == nil {
			fills[name] = &ArchFill{ID: "fill:" + name, Name: name, Implements: []string{}, Source: ArchSource{Declaration: path}}
		}
		if !slices.Contains(fills[name].Implements, implements) {
			fills[name].Implements = append(fills[name].Implements, implements)
		}
	}
	for i, binding := range d.in.Service.Slots {
		offer, ok := d.offers[binding.Slot]
		if !ok {
			return &ArchitectureRefusal{fmt.Errorf("slots[%d] binds the slot %s, which no component or mixin of the active set offers", i, binding.Slot)}
		}
		strategy, names := "gate", binding.Gate
		if len(binding.Chain) > 0 {
			strategy, names = "chain", binding.Chain
		} else if len(binding.FanOut) > 0 {
			strategy, names = "fan-out", binding.FanOut
		}
		slot := ArchSlot{ID: "slot:" + binding.Collection + "." + binding.Slot, Collection: "collection:" + binding.Collection, Name: binding.Slot,
			Phase: offer.def.Phase, Operations: slices.Clone(offer.def.Operations), Strategy: strategy, ShortCircuit: binding.ShortCircuit,
			OfferedBy: offer.by, Fills: []string{}, Source: ArchSource{Declaration: fmt.Sprintf("slots[%d]", i)}}
		for j, name := range names {
			slot.Fills = append(slot.Fills, "fill:"+name)
			fill(name, fmt.Sprintf("slots[%d].%s[%d]", i, strategy, j), slot.ID)
		}
		d.model.Slots = append(d.model.Slots, slot)
	}
	svc := d.in.Service
	for i, c := range svc.Commands {
		for j, name := range c.Fill {
			fill(name, fmt.Sprintf("commands[%d].fill[%d]", i, j), "command:"+c.Name)
		}
	}
	for i, q := range svc.Queries {
		for j, name := range q.Fill {
			fill(name, fmt.Sprintf("queries[%d].fill[%d]", i, j), "query:"+q.Name)
		}
	}
	for i, w := range svc.Workers {
		for j, name := range w.Fill {
			fill(name, fmt.Sprintf("workers[%d].fill[%d]", i, j), "worker:"+w.Name)
		}
	}
	for _, f := range fills {
		slices.Sort(f.Implements)
		d.model.Fills = append(d.model.Fills, *f)
	}
	// The fills come from a map; the later parts read them in order.
	sortByID(d.model.Fills, func(e ArchFill) string { return e.ID })
	return nil
}

// dataModel builds the entities, fields, references, collections, operations and actions.
func (d *deriver) dataModel() {
	svc, m := d.in.Service, d.model
	for i, e := range svc.Entities {
		tenant := e.Tenant
		if tenant == "" {
			tenant = "owned"
		}
		entity := ArchEntity{ID: "entity:" + e.Name, Name: e.Name, TenantKind: tenant, ResourceType: e.ResourceType, Fields: []ArchField{},
			Source: ArchSource{Declaration: fmt.Sprintf("entities[%d]", i)}}
		for j, f := range e.Fields {
			sensitivity := f.Sensitivity
			if sensitivity == "" {
				sensitivity = "public"
			}
			field := ArchField{ID: "field:" + e.Name + "." + f.Name, Name: f.Name, Type: string(f.Type), Required: !f.Optional, Sensitivity: sensitivity,
				Source: ArchSource{Declaration: fmt.Sprintf("entities[%d].fields[%d]", i, j)}}
			if f.Type == types.FieldTypeRef && f.To != "" {
				field.Ref = "entity:" + f.To
				m.References = append(m.References, ArchReference{ID: "reference:" + e.Name + "." + f.Name, From: entity.ID, To: field.Ref, Field: field.ID,
					Relation: e.References[f.Name].Relation, Source: ArchSource{Declaration: fmt.Sprintf("entities[%d].fields[%d].to", i, j)}})
			}
			entity.Fields = append(entity.Fields, field)
		}
		m.Entities = append(m.Entities, entity)
	}
	for _, c := range svc.Collections {
		collection := ArchCollection{ID: "collection:" + c.Name, Name: c.Name, Entity: "entity:" + c.Entity, Operations: []string{},
			Source: ArchSource{Declaration: "collections." + c.Name}}
		if scope := c.ScopeField(); scope != "" {
			collection.Scope = "field:" + c.Entity + "." + scope
		}
		for i, op := range c.Operations {
			id := "operation:" + c.Name + "." + string(op)
			collection.Operations = append(collection.Operations, id)
			m.Operations = append(m.Operations, ArchOperation{ID: id, Collection: collection.ID, Verb: string(op), Flow: "flow:" + c.Name + "." + string(op),
				Source: ArchSource{Declaration: fmt.Sprintf("collections.%s.operations[%d]", c.Name, i)}})
		}
		m.Collections = append(m.Collections, collection)
	}
	for i, c := range svc.Commands {
		m.Actions = append(m.Actions, ArchAction{ID: "command:" + c.Name, Kind: "command", Name: c.Name, Fills: fillIDs(c.Fill), Source: ArchSource{Declaration: fmt.Sprintf("commands[%d]", i)}})
	}
	for i, q := range svc.Queries {
		m.Actions = append(m.Actions, ArchAction{ID: "query:" + q.Name, Kind: "query", Name: q.Name, Fills: fillIDs(q.Fill), Source: ArchSource{Declaration: fmt.Sprintf("queries[%d]", i)}})
	}
	for i, w := range svc.Workers {
		m.Actions = append(m.Actions, ArchAction{ID: "worker:" + w.Name, Kind: "worker", Name: w.Name, Fills: fillIDs(w.Fill), Source: ArchSource{Declaration: fmt.Sprintf("workers[%d]", i)}})
	}
	if p := tenantProvisioning(svc); p != nil {
		for _, name := range []string{"provision", "retire"} {
			m.Actions = append(m.Actions, ArchAction{ID: "command:tenant-" + name, Kind: "command", Name: "tenant-" + name, Fills: []string{}, Source: ArchSource{Declaration: "tenancy.provisioning." + name}})
		}
	}
}

// tenantProvisioning gives the tenant provisioning of a pooled declaration, or nil.
func tenantProvisioning(svc *types.ServiceDeclaration) *types.TenantProvisioning {
	if svc.Tenancy == nil || svc.Tenancy.Provisioning == nil {
		return nil
	}
	return svc.Tenancy.Provisioning
}

// fillIDs gives the IDs of the fills, in their order.
func fillIDs(names []string) []string {
	ids := []string{}
	for _, name := range names {
		ids = append(ids, "fill:"+name)
	}
	return ids
}

// routesFromWiring reads the route of each operation from the wiring: a route statement mux.HandleFunc("<method>
// <path>", <handler>.<Operation>) whose handler is the value of a constructor that the wiring states handles the
// collection. An operation the wiring registers no route for has no method and path in the model.
func (d *deriver) routesFromWiring() {
	for _, w := range d.in.Wirings {
		if w.Wiring == nil {
			continue
		}
		handlers := map[string]string{}
		for index, collection := range w.Wiring.ConstructorCollections {
			if index >= 0 && index < len(w.Wiring.Constructors) {
				handlers[rawConstructorVarName(w.Wiring.Constructors[index])] = collection
			}
		}
		for _, statement := range w.Wiring.Routes {
			expression, err := parser.ParseExpr(statement)
			if err != nil {
				continue
			}
			call, ok := expression.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				continue
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			selector, isSelector := call.Args[1].(*ast.SelectorExpr)
			if !ok || !isSelector {
				continue
			}
			receiver, ok := selector.X.(*ast.Ident)
			collection, known := handlers[receiver.Name]
			pattern, err := strconv.Unquote(literal.Value)
			if !ok || !known || err != nil {
				continue
			}
			method, path, found := strings.Cut(pattern, " ")
			if !found {
				continue
			}
			op := strings.ToLower(selector.Sel.Name)
			d.routes["operation:"+collection+"."+op] = [2]string{method, path}
		}
	}
	for i := range d.model.Operations {
		if route, ok := d.routes[d.model.Operations[i].ID]; ok {
			d.model.Operations[i].Method, d.model.Operations[i].Path = route[0], route[1]
		}
	}
}

// platformRequirements is the part of deploy/platform-requirements.yaml that the model reads (ST-DLV-EN01).
type platformRequirements struct {
	Requirements []struct {
		Kind       string   `yaml:"kind"`
		Name       string   `yaml:"name"`
		Keys       []string `yaml:"keys"`
		Engine     string   `yaml:"engine"`
		Port       int      `yaml:"port"`
		APIGroup   string   `yaml:"api_group"`
		Resource   string   `yaml:"resource"`
		Verbs      []string `yaml:"verbs"`
		Level      string   `yaml:"level"`
		Components []string `yaml:"components"`
	} `yaml:"requirements"`
	Environment []struct {
		Name     string   `yaml:"name"`
		ReadBy   []string `yaml:"read_by"`
		Type     string   `yaml:"type"`
		Required bool     `yaml:"required"`
		Default  *string  `yaml:"default"`
		Secret   bool     `yaml:"secret"`
	} `yaml:"environment"`
}

// needs builds the platform edges from the platform requirements of the run, and a container for each backing
// store and topic.
func (d *deriver) needs() error {
	if len(d.in.Requirements) == 0 {
		return nil
	}
	var file platformRequirements
	if err := yaml.Unmarshal(d.in.Requirements, &file); err != nil {
		return fmt.Errorf("reading the platform requirements %s: %w", RequirementsFile, err)
	}
	for _, r := range file.Requirements {
		if len(r.Components) == 0 || d.in.Components[r.Components[0]] == nil {
			return &ArchitectureRefusal{fmt.Errorf("the platform requirement %s %s names no component of the active set that states it", r.Kind, r.Name)}
		}
		var shape []string
		if len(r.Keys) > 0 {
			shape = append(shape, "keys "+strings.Join(r.Keys, ", "))
		}
		if r.Engine != "" {
			shape = append(shape, "engine "+r.Engine)
		}
		if r.Port != 0 {
			shape = append(shape, "port "+strconv.Itoa(r.Port))
		}
		if r.Resource != "" {
			group := r.APIGroup
			if group == "" {
				group = "core"
			}
			shape = append(shape, fmt.Sprintf("%s %s: %s", group, r.Resource, strings.Join(r.Verbs, ", ")))
		}
		if r.Level != "" {
			shape = append(shape, "level "+r.Level)
		}
		need := ArchNeed{ID: "need:" + strings.ReplaceAll(r.Kind, " ", "-") + "." + r.Name, Kind: r.Kind, Name: r.Name, Shape: strings.Join(shape, "; "),
			Source: componentSource(d.in.Components[r.Components[0]], "needs")}
		for _, name := range r.Components {
			need.StatedBy = append(need.StatedBy, componentID(name))
		}
		slices.Sort(need.StatedBy)
		d.model.Needs = append(d.model.Needs, need)
		if r.Kind == "backing store" || r.Kind == "topic" {
			d.model.Containers = append(d.model.Containers, ArchContainer{ID: "container:" + r.Name, Name: r.Name, Kind: r.Kind, Source: need.Source})
		}
	}
	// Each variable of the list, copied unchanged; a secret one with no default (ST-GEN-UC08-R16).
	for _, v := range file.Environment {
		if len(v.ReadBy) == 0 {
			return &ArchitectureRefusal{fmt.Errorf("the environment variable %s of the platform requirements names no reader", v.Name)}
		}
		variable := ArchVariable{ID: "variable:" + v.Name, Name: v.Name, Type: v.Type, Required: v.Required, Default: v.Default, Secret: v.Secret}
		if v.Secret {
			variable.Default = nil
		}
		for _, reader := range v.ReadBy {
			name, group, grouped := strings.Cut(reader, " (")
			if d.in.Components[name] == nil {
				return &ArchitectureRefusal{fmt.Errorf("the environment variable %s names %s, which is no component of the active set", v.Name, reader)}
			}
			id := componentID(name)
			if grouped {
				id += "/" + strings.TrimSuffix(group, ")")
			}
			variable.ReadBy = append(variable.ReadBy, id)
			// The source is the statement of the first reader in name order: the component's environment list, or
			// the declaration's group of the component.
			if variable.Source == (ArchSource{}) {
				variable.Source = componentSource(d.in.Components[name], "environment")
				if grouped {
					variable.Source = ArchSource{Declaration: "overrides." + name}
				}
			}
		}
		slices.Sort(variable.ReadBy)
		d.model.Variables = append(d.model.Variables, variable)
	}
	return nil
}

// sortByID sorts a list of elements by their IDs.
func sortByID[T any](list []T, id func(T) string) {
	slices.SortFunc(list, func(a, b T) int { return strings.Compare(id(a), id(b)) })
}

// sortModel sorts every list of the model by id, except the steps of a flow, the layers, the fields of an entity,
// the operations of a collection, the fills of a slot and the offers of a part, which keep their order.
func sortModel(m *ArchitectureModel) {
	sortByID(m.Containers, func(e ArchContainer) string { return e.ID })
	sortByID(m.Components, func(e ArchComponent) string { return e.ID })
	sortByID(m.Mixins, func(e ArchMixin) string { return e.ID })
	sortByID(m.Ports, func(e ArchPort) string { return e.ID })
	sortByID(m.Bindings, func(e ArchBinding) string { return e.ID })
	sortByID(m.Slots, func(e ArchSlot) string { return e.ID })
	sortByID(m.Fills, func(e ArchFill) string { return e.ID })
	sortByID(m.Entities, func(e ArchEntity) string { return e.ID })
	sortByID(m.References, func(e ArchReference) string { return e.ID })
	sortByID(m.Collections, func(e ArchCollection) string { return e.ID })
	sortByID(m.Operations, func(e ArchOperation) string { return e.ID })
	sortByID(m.Actions, func(e ArchAction) string { return e.ID })
	sortByID(m.Flows, func(e ArchFlow) string { return e.ID })
	sortByID(m.Needs, func(e ArchNeed) string { return e.ID })
	sortByID(m.Variables, func(e ArchVariable) string { return e.ID })
	sortByID(m.Boundaries, func(e ArchBoundary) string { return e.ID })
	sortByID(m.Settings, func(e ArchSetting) string { return e.ID })
	sortByID(m.AuthnPoints, func(e ArchPoint) string { return e.ID })
	sortByID(m.AuthzPoints, func(e ArchPoint) string { return e.ID })
	sortByID(m.Access.Checks, func(e ArchCheck) string { return e.ID })
	sortByID(m.Access.Permissions, func(e ArchPermission) string { return e.ID })
	sortByID(m.Access.Roles, func(e ArchRole) string { return e.ID })
	sortByID(m.Compliance, func(e ArchTopic) string { return e.ID })
	for i := range m.Compliance {
		sortByID(m.Compliance[i].Facts, func(e ArchFact) string { return e.ID })
	}
	// A nil list is written as [], as the schema requires.
	for _, list := range []*[]string{&m.Layers} {
		if *list == nil {
			*list = []string{}
		}
	}
	emptyLists(m)
}
