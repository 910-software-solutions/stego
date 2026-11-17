package compiler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The checks of an architecture model: the schema, and the rules that the schema cannot state (ST-GEN-DS04): every
// ID is unique, every ID that an element names is an element of the model, the steps of a flow are numbered 1 to n
// in the layer order with each slot at its phase, and an aspect is empty exactly when its elements are.

var (
	architectureSchemaOnce sync.Once
	architectureSchema     *jsonschema.Schema
	architectureSchemaErr  error
)

// compiledArchitectureSchema compiles the shipped schema once.
func compiledArchitectureSchema() (*jsonschema.Schema, error) {
	architectureSchemaOnce.Do(func() {
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(ArchitectureSchema))
		if err != nil {
			architectureSchemaErr = err
			return
		}
		compiler := jsonschema.NewCompiler()
		compiler.DefaultDraft(jsonschema.Draft2020)
		if err := compiler.AddResource(ArchitectureSchemaID, document); err != nil {
			architectureSchemaErr = err
			return
		}
		architectureSchema, architectureSchemaErr = compiler.Compile(ArchitectureSchemaID)
	})
	return architectureSchema, architectureSchemaErr
}

// ValidateArchitecture checks the bytes of a model against the schema and the rules between its elements, and names
// the first fault.
func ValidateArchitecture(data []byte) error {
	schema, err := compiledArchitectureSchema()
	if err != nil {
		return fmt.Errorf("the architecture schema does not compile: %w", err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("the model is not JSON: %w", err)
	}
	if err := checkArchitectureVersion(instance); err != nil {
		return err
	}
	if err := schema.Validate(instance); err != nil {
		return fmt.Errorf("the model does not match the schema %s: %w", ArchitectureSchemaID, err)
	}
	var model ArchitectureModel
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&model); err != nil {
		return err
	}
	return checkArchitectureRules(&model)
}

// ArchitectureVersionError is a model of a schema version that this STEGO does not read (decision on ST-GEN-Q43). An
// older model is derived output, so apply writes it again; a newer model needs the STEGO that wrote it.
type ArchitectureVersionError struct {
	Found int
}

// Error says which version the model has, which version this STEGO reads, and what to do.
func (e *ArchitectureVersionError) Error() string {
	if e.Found < ArchitectureSchemaVersion {
		return fmt.Sprintf("the model is of the architecture schema version %d, and this STEGO reads version %d only; run stego apply to write the model again", e.Found, ArchitectureSchemaVersion)
	}
	return fmt.Sprintf("the model is of the architecture schema version %d, which is newer than version %d that this STEGO reads; use the STEGO that wrote the model", e.Found, ArchitectureSchemaVersion)
}

// checkArchitectureVersion refuses, before the schema, a model whose schema_version is a whole number other than the
// version of this STEGO, so that the refusal says why and does not list each part of the schema that differs. A
// model with no schema_version or one that is not a whole number goes on to the schema, which refuses it.
func checkArchitectureVersion(instance any) error {
	object, isObject := instance.(map[string]any)
	if !isObject {
		return nil
	}
	number, isNumber := object["schema_version"].(json.Number)
	if !isNumber {
		return nil
	}
	found, err := number.Int64()
	if err != nil || found == ArchitectureSchemaVersion || found < 0 || found > 1<<20 {
		return nil
	}
	return &ArchitectureVersionError{Found: int(found)}
}

// checkArchitectureRules checks the rules between the elements of a model.
func checkArchitectureRules(m *ArchitectureModel) error {
	ids := map[string]bool{m.System.ID: true}
	var fault error
	declare := func(id string) {
		if ids[id] && fault == nil {
			fault = fmt.Errorf("the ID %s is given twice", id)
		}
		ids[id] = true
	}
	for _, e := range m.Containers {
		declare(e.ID)
	}
	declareContext(m, declare)
	for _, e := range m.Components {
		declare(e.ID)
	}
	for _, e := range m.Mixins {
		declare(e.ID)
	}
	for _, e := range m.Ports {
		declare(e.ID)
	}
	for _, e := range m.Bindings {
		declare(e.ID)
	}
	for _, e := range m.Slots {
		declare(e.ID)
	}
	for _, e := range m.Fills {
		declare(e.ID)
	}
	for _, e := range m.Entities {
		declare(e.ID)
		for _, f := range e.Fields {
			declare(f.ID)
		}
	}
	for _, e := range m.References {
		declare(e.ID)
	}
	for _, e := range m.Collections {
		declare(e.ID)
	}
	for _, e := range m.Operations {
		declare(e.ID)
	}
	for _, e := range m.Actions {
		declare(e.ID)
	}
	for _, e := range m.Flows {
		declare(e.ID)
	}
	for _, e := range m.Needs {
		declare(e.ID)
	}
	for _, e := range m.Variables {
		declare(e.ID)
	}
	for _, e := range m.Boundaries {
		declare(e.ID)
	}
	for _, e := range m.Settings {
		declare(e.ID)
	}
	for _, e := range m.AuthnPoints {
		declare(e.ID)
	}
	for _, e := range m.AuthzPoints {
		declare(e.ID)
	}
	for _, e := range m.Access.Checks {
		declare(e.ID)
		for _, a := range e.Ancestors {
			declare(a.ID)
		}
	}
	for _, e := range m.Access.Permissions {
		declare(e.ID)
		for _, g := range e.Paths {
			declare(g.ID)
		}
	}
	for _, e := range m.Access.Roles {
		declare(e.ID)
	}
	for _, t := range m.Compliance {
		declare(t.ID)
		for _, f := range t.Facts {
			declare(f.ID)
		}
	}
	if fault != nil {
		return fault
	}
	refer := func(owner string, refs ...string) {
		for _, ref := range refs {
			if ref != "" && !ids[ref] && fault == nil {
				fault = fmt.Errorf("%s names %s, which is no element of the model", owner, ref)
			}
		}
	}
	// referKind checks that each reference names an element of the model of its kind, so a grant that names a role
	// or a field in place of a permission is not valid (security review of PR #374, L1).
	referKind := func(owner, kind string, refs ...string) {
		refer(owner, refs...)
		for _, ref := range refs {
			if ref != "" && !strings.HasPrefix(ref, kind+":") && fault == nil {
				fault = fmt.Errorf("%s names %s, which is no %s of the model", owner, ref, kind)
			}
		}
	}
	for _, c := range m.Components {
		refer(c.ID, c.Container)
		for _, p := range c.Ports {
			refer(c.ID, p.Port)
		}
	}
	for _, p := range m.Ports {
		refer(p.ID, p.ProvidedBy...)
		refer(p.ID, p.RequiredBy...)
	}
	for _, b := range m.Bindings {
		refer(b.ID, b.Port, b.Provider, b.Consumer)
	}
	for _, s := range m.Slots {
		refer(s.ID, s.Collection, s.OfferedBy)
		refer(s.ID, s.Fills...)
	}
	for _, f := range m.Fills {
		refer(f.ID, f.Implements...)
	}
	for _, e := range m.Entities {
		for _, f := range e.Fields {
			refer(f.ID, f.Ref)
		}
	}
	for _, r := range m.References {
		refer(r.ID, r.From, r.To, r.Field)
	}
	for _, c := range m.Collections {
		refer(c.ID, c.Entity, c.Scope)
		refer(c.ID, c.Operations...)
	}
	for _, o := range m.Operations {
		refer(o.ID, o.Collection)
		if len(m.Flows) > 0 {
			refer(o.ID, o.Flow)
		}
	}
	for _, a := range m.Actions {
		refer(a.ID, a.Fills...)
	}
	for _, f := range m.Flows {
		refer(f.ID, f.Operation)
		if err := checkFlowOrder(f, m); err != nil && fault == nil {
			fault = err
		}
		for _, s := range f.Steps {
			refer(f.ID, s.Element, s.ViaPort)
		}
	}
	for _, n := range m.Needs {
		refer(n.ID, n.StatedBy...)
	}
	for _, b := range m.Boundaries {
		refer(b.ID, b.Contains...)
		refer(b.ID, b.CrossedBy...)
	}
	for _, p := range append(slices.Clone(m.AuthnPoints), m.AuthzPoints...) {
		refer(p.ID, p.Element)
	}
	for _, c := range m.Access.Checks {
		refer(c.ID, c.Subject)
		referKind(c.ID, "permission", c.Grants)
		// Each ancestor check names an entity of the model, the field that links it up the path, and the
		// permission whose grant paths the model holds (ST-SUP-RC20-OC24).
		for i, a := range c.Ancestors {
			referKind(a.ID, "entity", a.Entity)
			referKind(a.ID, "field", a.Link)
			referKind(a.ID, "permission", a.Grants)
			if err := checkAncestorDecision(c, i, a); err != nil && fault == nil {
				fault = err
			}
		}
	}
	for _, p := range m.Access.Permissions {
		for _, g := range p.Paths {
			refer(g.ID, g.Role)
		}
	}
	for _, t := range m.Compliance {
		for _, f := range t.Facts {
			refer(f.ID, f.About)
		}
	}
	if err := checkContext(m, refer); err != nil && fault == nil {
		fault = err
	}
	if fault != nil {
		return fault
	}
	return checkAspects(m)
}

// stepLayer gives the place of a step kind in the order of a flow; a slot step takes its place from its phase.
var stepLayer = map[string]int{"operation": 0, "authenticate": 1, "authorize": 2, "handle": 3, "fill": 4, "repository": 5, "store": 6, "outbox": 7, "event": 9, "outside call": 10}

// checkFlowOrder checks that the steps of a flow are numbered 1 to n and follow the layer order, each slot at its
// phase.
func checkFlowOrder(f ArchFlow, m *ArchitectureModel) error {
	phases := map[string]string{}
	for _, s := range m.Slots {
		phases[s.ID] = s.Phase
	}
	last := -1
	for i, s := range f.Steps {
		if s.N != i+1 {
			return fmt.Errorf("%s: step %d is numbered %d", f.ID, i+1, s.N)
		}
		place, known := stepLayer[s.Kind]
		if s.Kind == "slot" {
			place, known = 4, true
			if phases[s.Element] == "after_store" {
				place = 8
			}
		}
		if !known || place < last {
			return fmt.Errorf("%s: step %d (%s %s) is out of the layer order", f.ID, s.N, s.Kind, s.Element)
		}
		last = place
	}
	return nil
}

// checkAspects checks that an aspect is empty exactly when it has no element.
func checkAspects(m *ArchitectureModel) error {
	for _, a := range []struct {
		name   string
		aspect ArchAspect
		count  int
	}{
		{"structure", m.Aspects.Structure, len(m.Components)},
		{"data_model", m.Aspects.DataModel, len(m.Entities)},
		{"request_flows", m.Aspects.RequestFlows, len(m.Flows)},
		{"platform_edges", m.Aspects.PlatformEdges, len(m.Needs) + len(m.Variables)},
		{"trust_and_tenancy", m.Aspects.TrustAndTenancy, len(m.Boundaries) + len(m.Settings)},
	} {
		if (a.aspect.State == "empty") != (a.count == 0) {
			return fmt.Errorf("the aspect %s is %s but holds %d elements", a.name, a.aspect.State, a.count)
		}
	}
	if !slices.Equal(m.Layers, []string{"edge", "authentication", "handler", "slot", "repository", "store", "outside call"}) {
		return fmt.Errorf("the layers are %s, not the layer order", strings.Join(m.Layers, ", "))
	}
	return nil
}

// checkAncestorDecision checks what the model says that the handler decides on one ancestor (ST-SUP-RC20-OC24,
// ST-SUP-Q156): only the parent, the last ancestor, of a list, a create or an upsert is decided, and with a bound
// authorization it names its read permission; an ancestor that is not decided names no permission, no resource type
// and no grants, so the view never shows a read that no handler decides, and never hides one that it decides. A
// decided parent with no permission must say that no authorization is bound (ST-DECL-UC06-R01).
func checkAncestorDecision(c ArchCheck, i int, a ArchAncestor) error {
	op := c.Subject[strings.LastIndex(c.Subject, ".")+1:]
	parent := i == len(c.Ancestors)-1
	decides := strings.HasPrefix(c.Subject, "operation:") && parent && (op == "list" || op == "create" || op == "upsert")
	switch {
	case a.Decided != decides:
		return fmt.Errorf("%s: decided is %t, and the handler decides a permission only on the parent of a list, a create or an upsert", a.ID, a.Decided)
	case !a.Decided && (a.Permission != "" || a.ResourceType != "" || a.Grants != "" || a.Public):
		return fmt.Errorf("%s: an ancestor that is not decided names a permission, a resource type or grants", a.ID)
	case a.Decided && !a.Public && (a.Permission == "") != (a.Grants == ""):
		return fmt.Errorf("%s: the decided parent names a read permission without its grants, or grants without the permission", a.ID)
	case a.Decided && !a.Public && a.Permission == "" && a.Source.Rule != "ST-DECL-UC06-R01":
		return fmt.Errorf("%s: the decided parent names no read permission, and the model does not say that no authorization is bound", a.ID)
	}
	return nil
}
