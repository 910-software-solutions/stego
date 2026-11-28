package declaration

import (
	"fmt"
	"slices"

	core "github.com/authzed/spicedb/pkg/proto/core/v1"

	"github.com/910-software-solutions/stego/internal/types"
)

// The opt-in creates_with_children of a command or a worker (ST-DECL-INV108, decision ST-SUP-Q221 (b)): checking
// computes, from the compiled schema, the witnesses of each decision that a create of an entity in writes makes on a
// referenced entity in writes, by the union-subset rule, and the generator gives them to the runtime
// (ST-SUP-RP10-LC59).

// The subject kinds and witness kinds of a witness, as the runtime names them.
const (
	SubjectCaller       = "caller"
	SubjectWorker       = "worker"
	SubjectStager       = "stager"
	WitnessRelationship = "relationship"
	WitnessReference    = "reference"
	WitnessTenant       = "tenant"
)

// ChildWitness is one witness of one decision of one reference: the decision of Permission with the subject kind
// Subject on the row that the reference Field of the entity Entity names is proved when the create of that row made
// the witness of the kind Kind through Relation, with the decided permission Decided for a reference or a tenant
// witness.
type ChildWitness struct {
	Entity, Field, Subject, Permission, Kind, Relation, Decided string
}

// childDecision is one decision of a reference that the create of an entity makes on a referenced entity in the mode
// of the command or the worker, with its witnesses.
type childDecision struct {
	entity, field, subject, permission string
	witnesses                          []ChildWitness
}

// childOwner is a command or a worker as the rule reads it.
type childOwner struct {
	kind, name, path, permission string
	command                      bool
	stagers                      []string
	writes                       []string
}

// ChildWitnesses gives the witnesses that checking proves for the command or the worker name that declares
// creates_with_children, in a fixed order, for the generator (ST-DECL-INV108). It gives none for a declaration that
// does not opt in or a schema that is nil.
func ChildWitnesses(service *types.ServiceDeclaration, schema *AuthzSchema, name string) []ChildWitness {
	var out []ChildWitness
	for _, o := range childOwners(service) {
		if o.name != name || schema == nil {
			continue
		}
		for _, d := range childDecisions(service, schema, o) {
			out = append(out, d.witnesses...)
		}
	}
	return out
}

// childOwners gives each command and worker of the declaration that declares creates_with_children.
func childOwners(service *types.ServiceDeclaration) []childOwner {
	if service == nil {
		return nil
	}
	var owners []childOwner
	for i, c := range service.Commands {
		if c.CreatesWithChildren {
			owners = append(owners, childOwner{kind: "command", name: c.Name, path: fmt.Sprintf("commands[%d].creates_with_children", i),
				permission: c.Permission, command: true, writes: c.Writes})
		}
	}
	consumed := consumedDestinations(service.Workers)
	for i, w := range service.Workers {
		if !w.CreatesWithChildren {
			continue
		}
		o := childOwner{kind: "worker", name: w.Name, path: fmt.Sprintf("workers[%d].creates_with_children", i), permission: w.Permission, writes: w.Writes}
		if destination := consumed[w.Name]; destination != "" {
			o.stagers = stagerPermissions(service, w, destination)
		}
		owners = append(owners, o)
	}
	return owners
}

// stagerPermissions gives the permissions that a consuming worker decides as the staging principal: the declared
// permission of each command and worker that stages for its destination, and its own for a subscription, as the
// worker generator gives its Stagers.
func stagerPermissions(service *types.ServiceDeclaration, w types.Worker, destination string) []string {
	var out []string
	add := func(p string) {
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for _, c := range service.Commands {
		for _, e := range c.Effects {
			if e.Destination == destination {
				add(c.Permission)
			}
		}
	}
	for _, other := range service.Workers {
		for _, e := range other.Effects {
			if e.Destination == destination {
				add(other.Permission)
			}
		}
	}
	for _, s := range service.Subscriptions {
		if s.Name == destination {
			add(w.Permission)
		}
	}
	slices.Sort(out)
	return out
}

// modeDecisions gives the decisions, as subject kind and permission, that a create in the mode of o makes on a
// referenced row through a reference that names named, by the rule of decideReference (ST-SUP-RP10-LC05,
// ST-SUP-RP10-LC17): a command the named permission, or its own when it names none, and nothing with neither; a worker
// its own; a consumer also each stager permission.
func modeDecisions(o childOwner, named string) [][2]string {
	if o.command {
		switch {
		case named != "":
			return [][2]string{{SubjectCaller, named}}
		case o.permission != "":
			return [][2]string{{SubjectCaller, o.permission}}
		}
		return nil
	}
	out := [][2]string{{SubjectWorker, o.permission}}
	for _, s := range o.stagers {
		out = append(out, [2]string{SubjectStager, s})
	}
	return out
}

// decidedBy gives the permission that a create in the mode of o decides with the subject kind on a referenced row
// through a reference that names named, and false when it decides none.
func decidedBy(o childOwner, subject, stager, named string) (string, bool) {
	switch subject {
	case SubjectCaller:
		d := modeDecisions(o, named)
		if len(d) == 0 {
			return "", false
		}
		return d[0][1], true
	case SubjectWorker:
		return o.permission, true
	case SubjectStager:
		return stager, true
	}
	return "", false
}

// childDecisions gives each decision of each reference of an entity in the writes of o to an entity in its writes
// that has a resource type, with the witnesses that the rule proves.
func childDecisions(service *types.ServiceDeclaration, schema *AuthzSchema, o childOwner) []childDecision {
	byName := map[string]types.Entity{}
	for _, e := range service.Entities {
		byName[e.Name] = e
	}
	var out []childDecision
	for _, cName := range o.writes {
		c, ok := byName[cName]
		if !ok {
			continue
		}
		for _, f := range c.Fields {
			p, ok := byName[f.To]
			if f.Type != types.FieldTypeRef || !ok || p.ResourceType == "" || !slices.Contains(o.writes, p.Name) {
				continue
			}
			ref := c.References[f.Name]
			for _, d := range modeDecisions(o, ref.Permission) {
				out = append(out, childDecision{entity: c.Name, field: f.Name, subject: d[0], permission: d[1],
					witnesses: witnessesOf(service, schema, o, c.Name, f.Name, p, d[0], d[1], byName)})
			}
		}
	}
	return out
}

// witnessesOf gives the witnesses of the decision (subject, permission) on the entity p for the reference field of
// entity.
func witnessesOf(service *types.ServiceDeclaration, schema *AuthzSchema, o childOwner, entity, field string, p types.Entity,
	subject, permission string, byName map[string]types.Entity) []ChildWitness {
	var out []ChildWitness
	w := ChildWitness{Entity: entity, Field: field, Subject: subject, Permission: permission}
	// (1) A caller relationship of P, for a caller decision only.
	if subject == SubjectCaller {
		for _, rel := range p.Relationships {
			if rel.SubjectSource != types.SubjectSourceCaller || !schema.cleanRelation(p.ResourceType, rel.Relation, rel.SubjectType) {
				continue
			}
			if schema.proves(p.ResourceType, permission, func(child *core.SetOperation_Child) bool {
				return child.GetComputedUserset() != nil && child.GetComputedUserset().GetRelation() == rel.Relation
			}) {
				x := w
				x.Kind, x.Relation = WitnessRelationship, rel.Relation
				out = append(out, x)
			}
		}
	}
	// (2) A required reference g of P to an entity G with a resource type, which the create of P decides in the mode.
	for _, g := range p.Fields {
		gEntity, ok := byName[g.To]
		if g.Type != types.FieldTypeRef || g.Optional || !ok || gEntity.ResourceType == "" {
			continue
		}
		gRef := p.References[g.Name]
		decided, ok := decidedBy(o, subject, permission, gRef.Permission)
		if !ok || gRef.Relation == "" || !schema.cleanRelation(p.ResourceType, gRef.Relation, gEntity.ResourceType) {
			continue
		}
		if schema.proves(p.ResourceType, permission, schema.arrowTo(gRef.Relation, gEntity.ResourceType, decided)) {
			x := w
			x.Kind, x.Relation, x.Decided = WitnessReference, gRef.Relation, decided
			out = append(out, x)
		}
	}
	// (3) The relation tenant of P, for a worker or a stager, when the create of P with no parent is decided on the
	// tenant resource.
	if subject != SubjectCaller && rootCreate(p, byName) && schema.cleanRelation(p.ResourceType, tenantRelationName, tenantType) {
		decided, _ := decidedBy(o, subject, permission, "")
		if schema.proves(p.ResourceType, permission, schema.arrowTo(tenantRelationName, tenantType, decided)) {
			x := w
			x.Kind, x.Relation, x.Decided = WitnessTenant, tenantRelationName, decided
			out = append(out, x)
		}
	}
	return out
}

// tenantRelationName is the relation of a resource type to the tenant of its rows (ST-SUP-Q132).
const tenantRelationName = "tenant"

// rootCreate tells whether a create of the entity can have no parent: it has no required reference to an entity with
// a resource type, so a worker decides it on the tenant resource (ST-SUP-RP10-LC17).
func rootCreate(e types.Entity, byName map[string]types.Entity) bool {
	for _, f := range e.Fields {
		if to, ok := byName[f.To]; f.Type == types.FieldTypeRef && ok && to.ResourceType != "" && !f.Optional {
			return false
		}
	}
	return true
}

// cleanRelation tells whether the relation of the type admits only the subject type, with no caveat, no expiration,
// no wildcard and no subject relation: a relation on a proven path.
func (s *AuthzSchema) cleanRelation(typeName, relation, subjectType string) bool {
	subjects, ok := s.definitions[typeName].subjects[relation]
	if !ok || len(subjects) == 0 {
		return false
	}
	for _, subject := range subjects {
		if subject.namespace != subjectType || subject.wildcard || subject.caveat != "" || subject.expiration ||
			(subject.relation != "" && subject.relation != "...") {
			return false
		}
	}
	return true
}

// arrowTo gives the target of a proof through an arrow: a plain arrow, or .any(), whose tupleset relation is relation,
// whose permission X on the type target the decided permission proves.
func (s *AuthzSchema) arrowTo(relation, target, decided string) func(child *core.SetOperation_Child) bool {
	return func(child *core.SetOperation_Child) bool {
		var tupleset, computed string
		switch {
		case child.GetTupleToUserset() != nil:
			tupleset, computed = child.GetTupleToUserset().GetTupleset().GetRelation(), child.GetTupleToUserset().GetComputedUserset().GetRelation()
		case child.GetFunctionedTupleToUserset() != nil && child.GetFunctionedTupleToUserset().GetFunction() == core.FunctionedTupleToUserset_FUNCTION_ANY:
			arrow := child.GetFunctionedTupleToUserset()
			tupleset, computed = arrow.GetTupleset().GetRelation(), arrow.GetComputedUserset().GetRelation()
		default:
			return false
		}
		return tupleset == relation && s.HoldsTerm(target, decided, computed)
	}
}

// proves tells whether the permission name of the type holds a term that target accepts, through union terms only
// (ST-DECL-INV108): from name, each step is a term of a union, a nested union, or another permission of the type whose
// expression is a union. An intersection, an exclusion or an arrow .all() on the path, a name that the schema does
// not define and a walk deeper than maxGuardDepth give no proof. A branch off the path does not count.
func (s *AuthzSchema) proves(typeName, name string, target func(*core.SetOperation_Child) bool) bool {
	rewrite, ok := s.PermissionRewrite(typeName, name)
	if !ok {
		return false
	}
	return s.provesUnion(typeName, rewrite, target, 0)
}

func (s *AuthzSchema) provesUnion(typeName string, rewrite *core.UsersetRewrite, target func(*core.SetOperation_Child) bool, depth int) bool {
	if rewrite == nil || rewrite.GetUnion() == nil || depth > maxGuardDepth {
		return false
	}
	for _, child := range rewrite.GetUnion().GetChild() {
		if target(child) {
			return true
		}
		switch {
		case child.GetUsersetRewrite() != nil:
			if s.provesUnion(typeName, child.GetUsersetRewrite(), target, depth+1) {
				return true
			}
		case child.GetComputedUserset() != nil:
			if next, ok := s.PermissionRewrite(typeName, child.GetComputedUserset().GetRelation()); ok && s.provesUnion(typeName, next, target, depth+1) {
				return true
			}
		}
	}
	return false
}

// HoldsTerm tells whether holding the permission decided of the type proves the permission x of the type by the
// union-subset rule of ST-DECL-INV108: decided is x, or a term of the union of x, directly or through more unions
// only, with no step into an arrow. It is not a subset rule, and it is a check of its own: it does not reuse implies,
// which is one.
func (s *AuthzSchema) HoldsTerm(typeName, decided, x string) bool {
	if decided == "" || x == "" {
		return false
	}
	if decided == x {
		return s.definitions[typeName].has(x)
	}
	return s.proves(typeName, x, func(child *core.SetOperation_Child) bool {
		return child.GetComputedUserset() != nil && child.GetComputedUserset().GetRelation() == decided
	})
}

// CreatesWithChildrenFaults refuses creates_with_children on a query, and on a command or a worker when no decision
// of a reference among the entities in its writes has a witness, with a fault that names it and ST-DECL-INV108.
func CreatesWithChildrenFaults(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	if service == nil {
		return nil
	}
	var faults []ValidationError
	for i, q := range service.Queries {
		if q.CreatesWithChildren {
			faults = append(faults, ValidationError{Category: "authorization", Rule: "creates-with-children", Path: fmt.Sprintf("queries[%d].creates_with_children", i), Found: q.Name,
				Message: fmt.Sprintf("query %q declares creates_with_children, which only a command or a worker may declare: a query creates no row (ST-DECL-INV108)", q.Name)})
		}
	}
	for _, o := range childOwners(service) {
		proved := false
		if schema != nil {
			for _, d := range childDecisions(service, schema, o) {
				proved = proved || len(d.witnesses) > 0
			}
		}
		if !proved {
			faults = append(faults, ValidationError{Category: "authorization", Rule: "creates-with-children", Path: o.path, Found: o.name,
				Message: fmt.Sprintf("%s %q declares creates_with_children, and the schema proves no decision of a reference among the entities in its writes, so each reference to a row of the same call would still be decided at the provider (ST-DECL-INV108); remove the key, or give the referenced entity a caller relationship or a required reference whose decided permission is a term of the union of the permission of the reference", o.kind, o.name)})
		}
	}
	return faults
}

// NewReferenceWarnings gives one warning new-reference-decided for each decision of a reference of a command or a
// worker that declares creates_with_children that the schema does not prove: the runtime decides it at the provider,
// so a reference to a row of the same call is not found (ST-DECL-INV108). A declaration whose every decision has no
// witness gets the fault of CreatesWithChildrenFaults and no warning.
func NewReferenceWarnings(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	if schema == nil {
		return nil
	}
	var warnings []ValidationError
	for _, o := range childOwners(service) {
		decisions := childDecisions(service, schema, o)
		if !slices.ContainsFunc(decisions, func(d childDecision) bool { return len(d.witnesses) > 0 }) {
			continue
		}
		for _, d := range decisions {
			if len(d.witnesses) > 0 {
				continue
			}
			warnings = append(warnings, ValidationError{Category: "authorization", Rule: "new-reference-decided", Path: o.path, Found: d.entity + "." + d.field,
				Message: fmt.Sprintf("%s %q declares creates_with_children, and the schema proves no witness of the decision of %s with the subject kind %s for the reference %s.%s: the runtime decides it at the provider, so a reference to a row that the same call created is not found (ST-DECL-INV108)",
					o.kind, o.name, d.permission, d.subject, d.entity, d.field)})
		}
	}
	return warnings
}
