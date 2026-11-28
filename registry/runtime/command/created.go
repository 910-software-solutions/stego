package command

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/910-software-solutions/stego/registry/runtime/authz"
	"github.com/910-software-solutions/stego/registry/runtime/otel"
)

// The decision of a reference to a row that this attempt created (ST-SUP-RP10-LC59, decision ST-SUP-Q221 (b)): a
// command or a worker that declares creates_with_children decides a reference of a new row C to a row P that its own
// Create wrote in this attempt with no provider call, only when the frozen witness table of the declaration lists a
// witness of each decision of the reference, the created-by-Create record of P holds a witness record that matches
// it, and SpiceDB serves the schema of the release (authz.SchemaGate). Every other reference is decided at the
// provider as before.

// The subject kinds of a decision: the principal of a command, the service account of a worker, and the staging
// principal of a consumer.
const (
	SubjectCaller = "caller"
	SubjectWorker = "worker"
	SubjectStager = "stager"
)

// The witness kinds (ST-DECL-INV108): a caller relationship of P, a required reference of P that the create of P
// decided, and the relation tenant of P with the decision of the create of P on the tenant resource.
const (
	WitnessRelationship = "relationship"
	WitnessReference    = "reference"
	WitnessTenant       = "tenant"
)

// Witness is one witness that checking computed for one decision of one reference (ST-DECL-INV108), which the
// generator gives for a command or a worker that declares creates_with_children: the decision of Permission with the
// subject kind Subject on the row P that the reference Field of the entity Entity names is proved when the create of
// P made the witness. For a reference witness, Relation is the relation of the reference g of P and Decided the
// permission D that the create of P decided on the row of g; for a tenant witness, Relation is tenant and Decided the
// permission that the create of P decided on the tenant resource; for a relationship witness, Relation is the
// relation of a caller relationship of P and Decided is empty.
type Witness struct {
	Entity, Field       string
	Subject, Permission string
	Kind, Relation      string
	Decided             string
}

// witnessKey names the decisions of one reference: the entity C, its reference field, the subject kind and the
// permission that the runtime decides.
type witnessKey struct {
	entity, field, subject, permission string
}

// witnessTable is the frozen copy of the witnesses of a declaration, which a handle reads and no fill reaches.
type witnessTable map[witnessKey][]Witness

// witnessEntity is what the witness check of a bind reads of an entity: its name, its resource type, its references
// and whether each is required, the relations of its caller relationships, and whether a create relates it to the
// tenant.
type witnessEntity struct {
	name, resourceType string
	references         []witnessReference
	callerRelations    []string
	tenant             bool
}

// witnessReference is one declared reference of an entity, as the witness check reads it.
type witnessReference struct {
	field, entity, resourceType, relation, permission string
	required                                          bool
}

func (e *Entity[T]) witnessEntity() witnessEntity {
	w := witnessEntity{name: e.spec.Name, resourceType: e.spec.ResourceType, tenant: e.spec.Tenant}
	for _, r := range e.spec.Parent {
		w.references = append(w.references, witnessReference{field: r.Field, entity: r.Entity, resourceType: r.ResourceType,
			relation: r.Relation, permission: r.Permission, required: r.Required})
	}
	for _, rel := range e.spec.Relationships {
		if rel.Field == nil {
			w.callerRelations = append(w.callerRelations, rel.Relation)
		}
	}
	return w
}

// bindMode is what the witness check of a bind knows of the command or the worker: the subject kinds that it has and
// the permissions that its handle decides.
type bindMode struct {
	owner string
	// command is true for a command, whose decisions are of the caller; a worker decides with its service account,
	// and a consumer also with the staging principal, whose permissions are stagers.
	command           bool
	commandPermission string
	workerPermission  string
	consumer          bool
	stagers           []string
}

// decides gives the permission that a create in the mode decides with the subject kind on a referenced row through a
// reference that names named, and false when the mode has no such decision.
func (m bindMode) decides(subject, named string) ([]string, bool) {
	switch {
	case subject == SubjectCaller && m.command:
		if named != "" {
			return []string{named}, true
		}
		return []string{m.commandPermission}, m.commandPermission != ""
	case subject == SubjectWorker && !m.command:
		return []string{m.workerPermission}, true
	case subject == SubjectStager && m.consumer:
		return m.stagers, len(m.stagers) > 0
	}
	return nil, false
}

// freezeWitnesses checks the witnesses of a declaration that declares creates_with_children against the entities that
// it writes and its mode, and gives their frozen copy (ST-SUP-RP10-LC59). It refuses, with an error that names the
// entity and the witness, a witness of an entity or a reference that the declaration does not write, a witness whose
// relation is not a caller relationship relation of P, the relation of a required reference of P or tenant, one whose
// subject kind the mode does not have, and one whose permission is not the one that the runtime decides there in the
// mode. A declaration that does not declare creates_with_children takes no witness.
func freezeWitnesses(m bindMode, opted bool, witnesses []Witness, writes []Declared) (witnessTable, error) {
	if !opted {
		if len(witnesses) > 0 {
			return nil, fmt.Errorf("command: %s gives witnesses but does not declare creates_with_children", m.owner)
		}
		return nil, nil
	}
	entities := map[string]witnessEntity{}
	for _, d := range writes {
		w := d.witnessEntity()
		entities[w.name] = w
	}
	table := witnessTable{}
	for _, w := range witnesses {
		refuse := func(why string) error {
			return fmt.Errorf("command: %s: the witness %s of %s.%s for %s %s is refused: %s", m.owner, w.Kind+" "+w.Relation, w.Entity, w.Field, w.Subject, w.Permission, why)
		}
		c, ok := entities[w.Entity]
		if !ok {
			return nil, refuse("the declaration does not write the entity")
		}
		i := slices.IndexFunc(c.references, func(r witnessReference) bool { return r.field == w.Field })
		if i < 0 {
			return nil, refuse("the entity has no such reference")
		}
		r := c.references[i]
		p, ok := entities[r.entity]
		if !ok || p.resourceType == "" || p.resourceType != r.resourceType {
			return nil, refuse("the referenced entity is not written by the declaration or has no resource type")
		}
		permissions, ok := m.decides(w.Subject, r.permission)
		if !ok {
			return nil, refuse("the mode has no decision of this subject kind")
		}
		if !slices.Contains(permissions, w.Permission) {
			return nil, refuse("the permission is not the one that the runtime decides on the reference")
		}
		switch w.Kind {
		case WitnessRelationship:
			if w.Subject != SubjectCaller || w.Decided != "" || !slices.Contains(p.callerRelations, w.Relation) {
				return nil, refuse("the relation is not a caller relationship relation of " + p.name)
			}
		case WitnessReference:
			j := slices.IndexFunc(p.references, func(g witnessReference) bool { return g.relation == w.Relation && g.relation != "" })
			if j < 0 || !p.references[j].required || p.references[j].resourceType == "" {
				return nil, refuse("the relation is not the relation of a required reference of " + p.name)
			}
			decided, ok := m.decides(w.Subject, p.references[j].permission)
			if !ok || !slices.Contains(decided, w.Decided) || (w.Subject == SubjectStager && w.Decided != w.Permission) {
				return nil, refuse("the decided permission is not the one that the create of " + p.name + " decides")
			}
		case WitnessTenant:
			if w.Relation != authz.TenantRelation || !p.tenant || w.Subject == SubjectCaller {
				return nil, refuse("the relation is not the relation tenant of " + p.name)
			}
			decided, _ := m.decides(w.Subject, "")
			if !slices.Contains(decided, w.Decided) || (w.Subject == SubjectStager && w.Decided != w.Permission) {
				return nil, refuse("the decided permission is not the one that the create of " + p.name + " decides on the tenant resource")
			}
		default:
			return nil, refuse("the witness kind is not relationship, reference or tenant")
		}
		key := witnessKey{entity: w.Entity, field: w.Field, subject: w.Subject, permission: w.Permission}
		table[key] = append(table[key], w)
	}
	return table, nil
}

// witnessRecord is one fact of a create that a proof may use (ST-SUP-RP10-LC59): an allow that the runtime checked,
// with its subject kind, its permission, its resource and the reference field of the created row, or tenant, that it
// was made for; or a tuple of a caller relationship of the created row that the runtime enqueued, with its relation
// and its subject.
type witnessRecord struct {
	relationship bool
	subject      string
	permission   string
	resourceType string
	resourceID   string
	field        string
	relation     string
}

// createdRecord is the created-by-Create record of one row that the Create of this attempt wrote: the create token,
// which the loaded value that the Create returned carries, whether a later Update or Delete of the row closed it, the
// references of the row, and the witness records of its create.
type createdRecord struct {
	token      uint64
	open       bool
	references []reference
	records    []witnessRecord
}

// decisionAllow is one allow that a decision of a reference returned, with its subject kind and permission.
type decisionAllow struct {
	subject, permission string
}

// nextWrite gives the write token of the next write of the state.
func (s *state) nextWrite() uint64 {
	s.writeSeq++
	return s.writeSeq
}

// closeCreated closes the created-by-Create record of a row that an Update or a Delete of this attempt wrote.
func (s *state) closeCreated(entity, id string) {
	if rec := s.created[rowKey{entity, id}]; rec != nil {
		rec.open = false
	}
}

// subjectKind is the subject kind of the handle's principal: the caller of a command, else the worker.
func (s *state) subjectKind() string {
	if s.mode == modeCommand {
		return SubjectCaller
	}
	return SubjectWorker
}

// decideCreateReference decides one set reference ref of a new row of the entity entity on its loaded referenced row
// p, for a Create (ST-SUP-RP10-LC05, ST-SUP-RP10-LC17, ST-SUP-RP10-LC59). It gives the allows that the decision made,
// for the created-by-Create record of the new row. A deny is not found.
func (s *state) decideCreateReference(ctx context.Context, entity string, ref reference, p rowRef) ([]decisionAllow, error) {
	if s.witnesses != nil && p.st == s && p.write != 0 && s.createTokens[p.write] {
		rec := s.created[rowKey{p.entity, p.id}]
		if rec == nil || !rec.open || rec.token != p.write {
			// The value of a Create whose row a later write changed or deleted, or of an earlier Create of the same ID,
			// is stale: the row version is not the test, as it starts at 1 again (ST-SUP-RP10-LC59).
			return nil, s.breakWith(internalf("a create of %s takes a loaded value of %s whose row a later write of the call changed or deleted", entity, p.entity))
		}
		if allows, ok := s.proveReference(ctx, entity, ref, p, rec); ok {
			return allows, nil
		}
	}
	return s.decideReferenceAllows(ctx, ref, p)
}

// decideReferenceAllows is decideReference that gives the allows that it made, each from an evaluate that returned
// allowed: none for a call that returned with no evaluate.
func (s *state) decideReferenceAllows(ctx context.Context, ref reference, p rowRef) ([]decisionAllow, error) {
	if err := s.decideReference(ctx, ref, p); err != nil {
		return nil, err
	}
	return s.allowsOf(ref.permission, p.resourceType), nil
}

// allowsOf gives the allows that a decision of a reference that names named on a row of resourceType made, once it
// returned with no error: a command evaluates the named permission, or its own when it names none, and nothing with
// neither; a worker evaluates its own permission, and a consumer also the stager's.
func (s *state) allowsOf(named, resourceType string) []decisionAllow {
	if resourceType == "" {
		return nil
	}
	switch s.mode {
	case modeCommand:
		if named != "" {
			return []decisionAllow{{SubjectCaller, named}}
		}
		if s.commandPermission != "" {
			return []decisionAllow{{SubjectCaller, s.commandPermission}}
		}
		return nil
	case modeConsumer:
		return []decisionAllow{{SubjectWorker, s.permission}, {SubjectStager, s.stagingPermission}}
	}
	return []decisionAllow{{SubjectWorker, s.permission}}
}

// neededDecisions gives the decisions that a create in the mode makes on a referenced row through a reference that
// names named: the same as allowsOf.
func (s *state) neededDecisions(named, resourceType string) []decisionAllow {
	return s.allowsOf(named, resourceType)
}

// proveReference decides the reference ref of a new row of entity on p, a row of the open created-by-Create record
// rec of this attempt, with no provider call, when the schema digest holds and rec holds a witness record of each
// decision that the reference needs in the mode. It gives the allows that it made, which count as witness records of
// the new row, and false when it cannot prove one decision, so the caller decides at the provider.
func (s *state) proveReference(ctx context.Context, entity string, ref reference, p rowRef, rec *createdRecord) ([]decisionAllow, bool) {
	needed := s.neededDecisions(ref.permission, p.resourceType)
	if len(needed) == 0 {
		return nil, false
	}
	kinds := make([]string, len(needed))
	for i, d := range needed {
		kind, ok := s.witnessOf(entity, ref.field, d, rec)
		if !ok {
			return nil, false
		}
		kinds[i] = kind
	}
	if !s.rt.cfg.SchemaGate.Holds(ctx) {
		return nil, false
	}
	for i, d := range needed {
		// The decision that skips the provider is seen in production; the line holds no row ID and no value.
		slog.InfoContext(ctx, "command: a reference to a row of this call is decided by its create", otel.SlogEventKey, "command.reference.decided_by_create",
			"action", s.action, "entity", entity, "field", ref.field, "subject_kind", d.subject, "witness_kind", kinds[i])
	}
	return needed, true
}

// witnessOf finds, for one decision d of the reference field of entity, a witness of the frozen table that a record of
// rec matches, and gives its kind.
func (s *state) witnessOf(entity, field string, d decisionAllow, rec *createdRecord) (string, bool) {
	for _, w := range s.witnesses[witnessKey{entity: entity, field: field, subject: d.subject, permission: d.permission}] {
		for _, r := range rec.records {
			if s.matches(w, r, rec) {
				return w.Kind, true
			}
		}
	}
	return "", false
}

// matches tells whether the witness record r of the created row of rec is the witness w: the same subject kind and
// permission, and for a reference witness the relation of the reference g of w and the ID that the row holds in g;
// for a tenant witness the tenant resource of the state; for a relationship witness the relation of w and the
// principal of the state. A record on another object or for another reference is no witness.
func (s *state) matches(w Witness, r witnessRecord, rec *createdRecord) bool {
	switch w.Kind {
	case WitnessRelationship:
		return r.relationship && w.Subject == SubjectCaller && r.subject == s.principal.Subject().String() && r.relation == w.Relation
	case WitnessReference:
		if r.relationship || r.subject != w.Subject || r.permission != w.Decided || r.relation != w.Relation {
			return false
		}
		for _, g := range rec.references {
			if g.relation == w.Relation && g.field == r.field && g.resourceType == r.resourceType && g.id == r.resourceID {
				return true
			}
		}
		return false
	case WitnessTenant:
		return !r.relationship && r.subject == w.Subject && r.permission == w.Decided && r.relation == authz.TenantRelation &&
			r.resourceType == authz.TenantType && r.resourceID == s.tenant.ID()
	}
	return false
}

// recordCreated keeps the created-by-Create record of a row that a Create of this attempt wrote, with its create token,
// its references and its witness records; a witnessRecordHook of a test may change the records first.
func (s *state) recordCreated(entity, id string, token uint64, refs []reference, records []witnessRecord) {
	if s.created == nil {
		s.created = map[rowKey]*createdRecord{}
		s.createTokens = map[uint64]bool{}
	}
	if hook := s.rt.witnessRecordHook; hook != nil {
		records = hook(entity, records)
	}
	s.created[rowKey{entity, id}] = &createdRecord{token: token, open: true, references: slices.Clone(refs), records: records}
	s.createTokens[token] = true
}
