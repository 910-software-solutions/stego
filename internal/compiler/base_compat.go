package compiler

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/types"
	"golang.org/x/mod/semver"
)

// BaseCompatRule is the rule that a fault of the base check names when the head changes a table of the base other
// than by a declared schema change (ST-GEN-INV33).
const BaseCompatRule = "ST-GEN-INV33"

// BaseRecordRule is the rule that a fault names when the declaration at the base ref is not the declaration that
// the record at that ref names (ST-GEN-UC14-R01).
const BaseRecordRule = "ST-GEN-UC14-R01"

// BaseSource is what the check reads at the base ref: the ref as the author or the change gate names it, the
// commit it resolves to, and the bytes of the declaration and of the record there. A nil Record means that the
// base ref holds no record, so no release of the service has run.
type BaseSource struct {
	Ref         string
	Commit      string
	Declaration []byte
	Record      []byte
}

// BaseCheck is the result of the check against the base (ST-GEN-QRY04): the base it took as the version that runs
// now, and every fault, in a fixed order. No fault means that the head can run beside the base.
type BaseCheck struct {
	BaseRef    string `json:"base_ref"`
	BaseCommit string `json:"base_commit"`
	// Released tells whether the base holds a record, so that a release of it may run. With no record the check
	// accepts any stored shape.
	Released bool        `json:"base_released"`
	Faults   []BaseFault `json:"faults"`
}

// BaseFault is one fault of the head against the base. It names the entity, the field, the rule that the change
// breaks, and the schema change and phase that make the change safe, so that an agent can correct it in one
// attempt (ST-GEN-UC14-R03, ST-BO05).
type BaseFault struct {
	Rule    string `json:"rule"`
	Entity  string `json:"entity,omitempty"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
	// Declare is each schema change that makes the change safe, with its phase; it is empty for a fault that no
	// schema change corrects.
	Declare []SuggestedChange `json:"declare,omitempty"`
}

// SuggestedChange is a schema change that a fault asks the author to declare: its ID, its kind, its entity and
// field, the values its kind needs, and the phase of the step to put at the end of the schema history.
type SuggestedChange struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Entity string `json:"entity"`
	Field  string `json:"field,omitempty"`
	From   string `json:"from,omitempty"`
	Needs  string `json:"needs,omitempty"`
	Phase  string `json:"phase"`
}

// String gives the change as a sentence part, such as rename note-title-rename (Note.heading from title) at expand.
func (c SuggestedChange) String() string {
	target := c.Entity
	if c.Field != "" {
		target += "." + c.Field
	}
	text := fmt.Sprintf("%s %s (%s", c.Kind, c.ID, target)
	if c.From != "" {
		text += " from " + c.From
	}
	text += ")"
	if c.Needs != "" {
		text += " with " + c.Needs
	}
	return text + " at " + c.Phase
}

// RecordsRelease tells whether the bytes of a record name a release of its service: an apply that wrote it.
func RecordsRelease(data []byte, name string) (bool, error) {
	state, err := decodeState(data, name)
	if err != nil {
		return false, err
	}
	return state.LastApplied != nil, nil
}

// HasFaults tells whether the check refused the head.
func (c BaseCheck) HasFaults() bool { return len(c.Faults) > 0 }

// CheckBase compares the head declaration with the declaration and the record at the base ref (ST-GEN-QRY04). With
// no record at the base it accepts any stored shape, because no release has run (ST-GEN-UC14-R02). It refuses a
// base declaration whose digest is not the declaration digest of the record (ST-GEN-UC14-R01), and otherwise gives
// the faults of ST-GEN-INV33. It writes nothing.
func CheckBase(base BaseSource, head *types.ServiceDeclaration) (BaseCheck, error) {
	check := BaseCheck{BaseRef: base.Ref, BaseCommit: base.Commit, Faults: []BaseFault{}}
	if base.Record == nil {
		return check, nil
	}
	state, err := decodeState(base.Record, ".stego/state.yaml at "+base.Ref)
	if err != nil {
		return check, err
	}
	if state.LastApplied == nil {
		return check, nil
	}
	check.Released = true
	if base.Declaration == nil {
		check.Faults = append(check.Faults, BaseFault{Rule: BaseRecordRule, Message: fmt.Sprintf("the base ref %s (commit %s) holds a record and no service.yaml; the record names the declaration digest %s", base.Ref, base.Commit, state.LastApplied.ServiceHash)})
		return check, nil
	}
	if digest := HashBytes(base.Declaration); digest != state.LastApplied.ServiceHash {
		check.Faults = append(check.Faults, BaseFault{Rule: BaseRecordRule, Message: fmt.Sprintf("the declaration at the base ref %s (commit %s) has the digest %s, and the record there names the declaration digest %s; the base was changed without stego apply, so it is not the version that runs. Run stego apply on the base and commit the record", base.Ref, base.Commit, digest, state.LastApplied.ServiceHash)})
		return check, nil
	}
	declaration, err := parser.ParseServiceDeclarationFromBytes(base.Declaration, "service.yaml at "+base.Ref)
	if err != nil {
		return check, err
	}
	check.Faults = append(check.Faults, validateBaseSchema(state.LastApplied, declaration, head)...)
	check.Faults = append(check.Faults, ledgerVersionFaults(state.LastApplied, declaration, head)...)
	check.Faults = append(check.Faults, syncRuntimeFaults(state.LastApplied, declaration, head)...)
	check.Faults = append(check.Faults, relaxRuntimeFaults(state.LastApplied, declaration, head)...)
	return check, nil
}

// StepLedgerVersion is the first postgres-adapter version that holds ST-SUP-RC01-OC28, as the introduced_in of that
// output contract names it: its ledger accepts the rows of a later schema step within their compatible_with sets.
const StepLedgerVersion = "6.1.0"

// StepLedgerRule is the rule that refuses a new schema step on a base whose postgres-adapter does not hold
// ST-SUP-RC01-OC28.
const StepLedgerRule = "ST-GEN-INV36"

// ledgerVersionFaults refuses a head with a new schema step, one after the steps of the base, when the record at
// the base pins a postgres-adapter version before StepLedgerVersion: that release refuses a ledger longer than its
// own, so it cannot restart after the step's migration. The fault names the version to deploy first (ST-GEN-INV36).
// A base with no postgres-adapter has no ledger to refuse.
func ledgerVersionFaults(applied *AppliedState, base, head *types.ServiceDeclaration) []BaseFault {
	if len(head.SchemaHistory) <= len(base.SchemaHistory) {
		return nil
	}
	adapter, pinned := applied.Components["postgres-adapter"]
	if !pinned {
		return nil
	}
	version := "v" + strings.TrimPrefix(adapter.Version, "v")
	if semver.IsValid(version) && semver.Compare(version, "v"+StepLedgerVersion) >= 0 {
		return nil
	}
	step := head.SchemaHistory[len(base.SchemaHistory)]
	return []BaseFault{{Rule: StepLedgerRule, Entity: changeEntity(head, step.Change), Message: fmt.Sprintf(
		"the base runs postgres-adapter %s, whose ledger refuses the row of a new schema step such as %s at %s, so its pods cannot restart after the step's migration; deploy a release with postgres-adapter %s or later, with no new schema step, first", adapter.Version, step.Change, step.Phase, StepLedgerVersion)}}
}

// SyncRuntimeVersion is the first runtime library version whose schema check accepts the sync trigger of template
// version 2 with the completeness CHECK of its step, NOT VALID or validated (decision on ST-SUP-Q120 (c),
// orchestrator under the owner's delegation, 2026-11-14), after template version 1 as the decisions on findings F3
// and F4 of the schema-change research fixed it, with the writer marker, the backfill bypass and the guarded
// conversions (orchestrator under the owner's delegation, 2026-11-13).
const SyncRuntimeVersion = "v0.95.0"

// syncKinds are the kinds of schema change whose step at expand adds a sync trigger.
var syncKinds = map[string]bool{"rename": true, "retype": true, "require": true}

// syncRuntimeFaults refuses a head with a new expand step of a rename, a retype or a require when the record at the
// base pins a runtime library before SyncRuntimeVersion, or none: the schema check of that release refuses the sync
// trigger of the step, so its pods cannot restart after the step's migration. The fault names the runtime version to
// deploy first (ST-GEN-INV36; decision on finding F11, orchestrator under the owner's delegation, 2026-11-13). A
// base with no postgres-adapter has no schema check of its tables.
func syncRuntimeFaults(applied *AppliedState, base, head *types.ServiceDeclaration) []BaseFault {
	if _, pinned := applied.Components["postgres-adapter"]; !pinned {
		return nil
	}
	version := ""
	if applied.Archetype != nil && applied.Archetype.Runtime != nil {
		version = applied.Archetype.Runtime.Version
	}
	if semver.IsValid(version) && semver.Compare(version, SyncRuntimeVersion) >= 0 {
		return nil
	}
	kinds := map[string]string{}
	for _, change := range head.SchemaChanges {
		kinds[change.ID] = change.Kind
	}
	for _, step := range head.SchemaHistory[min(len(base.SchemaHistory), len(head.SchemaHistory)):] {
		if step.Phase != "expand" || !syncKinds[kinds[step.Change]] {
			continue
		}
		pinned := version
		if pinned == "" {
			pinned = "no runtime library"
		}
		return []BaseFault{{Rule: StepLedgerRule, Entity: changeEntity(head, step.Change), Message: fmt.Sprintf(
			"the base runs %s, whose schema check refuses the sync trigger of the %s step %s at expand, so its pods cannot restart after the step's migration; deploy a release with the runtime library %s or later, with no new schema step, first", pinned, kinds[step.Change], step.Change, SyncRuntimeVersion)}}
	}
	return nil
}

// validateBaseSchema enforces ST-GEN-INV33 on a head declaration against the declaration of a base that has a
// record: the head's schema steps start with the base's steps in the same order and unchanged, the base's schema
// changes stay as they are, also the fill value of a retype (ST-GEN-Q24 (c)), no change gets its expand and its
// contract step in one release, and the baseline of the head, its stored shape with every schema step undone, is
// the baseline of the base. STEGO cannot count rows, so it takes each table of the base as a table with rows.
func validateBaseSchema(applied *AppliedState, base, head *types.ServiceDeclaration) []BaseFault {
	var faults []BaseFault
	faults = append(faults, baseStepFaults(base, head)...)
	faults = append(faults, baseChangeFaults(base, head)...)
	faults = append(faults, onePhaseFaults(base, head)...)
	faults = append(faults, uniqueAddFaults(base, head)...)
	faults = append(faults, relaxFaults(base, head)...)
	faults = append(faults, withinFaults(base, head)...)
	generated, ok := generatedBaselines(applied, base, head)
	if !ok {
		// Generation does not carry out each kind of the declarations yet, so the indexes are compared as the
		// collections describe them.
		faults = append(faults, collectionIndexFaults(base, head)...)
	}
	shapeFaults := baselineFaults(shapeAt(base, 0), shapeAt(head, 0))
	// The baselines leave out what a step of the base added, required or retyped, so the head with only its new
	// steps undone must also be the stored shape of the base as it runs now, with every field in full.
	named := map[[2]string]bool{}
	for _, fault := range shapeFaults {
		named[[2]string{fault.Entity, fault.Field}] = true
	}
	for _, fault := range baselineFaults(shapeAt(base, len(base.SchemaHistory)+1), shapeAt(head, len(base.SchemaHistory))) {
		if !named[[2]string{fault.Entity, fault.Field}] {
			shapeFaults = append(shapeFaults, fault)
		}
	}
	faults = append(faults, shapeFaults...)
	if ok {
		faults = append(faults, generated.faults(faults)...)
	}
	return faults
}

// baselineOutput is what the postgres-adapter generator gives for the baselines of the base and of the head: the
// tables of 001_initial and the digest of the schema marker (postgresadapter.Baseline).
type baselineOutput struct {
	base, head             []postgresadapter.BaselineTable
	baseMarker, headMarker string
}

// generatedBaselines gives what the postgres-adapter generator writes for the baselines of the base and of the head,
// each with its own tenancy: the base with the tenancy component when the record at the base names it (or, for a
// record that names no component, when its settings select it), and the head when its settings select it
// (selectComponents). No archetype or mixin holds the tenancy component, so the settings alone select it. It gives
// false, so that the check compares the indexes that the collections describe, when the record names components and
// no postgres-adapter, and when generation does not carry out a kind of schema change of either declaration yet.
func generatedBaselines(applied *AppliedState, base, head *types.ServiceDeclaration) (*baselineOutput, bool) {
	selected := func(service *types.ServiceDeclaration) []string {
		var names []string
		for _, s := range selectComponents(service) {
			names = append(names, s.Component)
		}
		return names
	}
	baseComponents := selected(base)
	if applied != nil && len(applied.Components) > 0 {
		if _, adapter := applied.Components["postgres-adapter"]; !adapter {
			return nil, false
		}
		baseComponents = nil
		if _, tenancy := applied.Components[gen.TenancyComponent]; tenancy {
			baseComponents = []string{gen.TenancyComponent}
		}
	}
	baseline := func(service *types.ServiceDeclaration, components []string) ([]postgresadapter.BaselineTable, string, error) {
		entities, err := gen.StoredNames(service.Entities, service.Invariants, service.Collections, service.SchemaChanges)
		if err != nil {
			return nil, "", err
		}
		return postgresadapter.Baseline(gen.Context{Entities: entities, Collections: service.Collections,
			SchemaChanges: service.SchemaChanges, SchemaHistory: service.SchemaHistory, Tenancy: pooledTenancy(service, components)})
	}
	out := &baselineOutput{}
	var err error
	if out.base, out.baseMarker, err = baseline(base, baseComponents); err != nil {
		return nil, false
	}
	if out.head, out.headMarker, err = baseline(head, selected(head)); err != nil {
		return nil, false
	}
	return out, true
}

// faults compares the generated baselines of the base and the head, table by table, and gives a fault for each
// table whose CREATE TABLE or indexes in 001_initial differ, for a new order of the tables, and for a new digest of
// the marker: a database with rows holds the 001_initial and the marker of the base, so the head's migration job or
// marker check would fail on it (ST-GEN-INV33, ST-SUP-RC01-OC41, ST-SUP-RC01-OC42). An entity that an earlier fault
// names gets no second fault here, and the order and the marker get one only when no other fault names their cause.
func (o *baselineOutput) faults(earlier []BaseFault) []BaseFault {
	if o == nil {
		return nil
	}
	named := map[string]bool{}
	for _, fault := range earlier {
		named[fault.Entity] = true
	}
	headTables := map[string]postgresadapter.BaselineTable{}
	for _, table := range o.head {
		headTables[table.Table] = table
	}
	var faults []BaseFault
	for _, before := range o.base {
		after, ok := headTables[before.Table]
		switch {
		case named[before.Entity]:
		case !ok:
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: before.Entity, Message: fmt.Sprintf(
				"the baseline of the head has no table %s, and 001_initial of the base creates it; keep the entity %s of the base, or declare the schema change that removes it", before.Table, before.Entity)})
		case before.Create != after.Create:
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: before.Entity, Message: fmt.Sprintf(
				"the head changes the table %s of 001_initial with no schema step: the base creates\n%sand the head creates\n%sA database with rows holds the table of the base, so the head's migration job would fail its ledger check; keep the fields of %s in the order and with the settings of the base, and declare each change as a schema change", before.Table, before.Create, after.Create, before.Entity)})
		case before.Indexes != after.Indexes:
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: before.Entity, Message: fmt.Sprintf(
				"the collections or the fields over %s change its indexes in 001_initial: the base gives\n%sand the head gives\n%sNo schema change builds such an index yet, so the next migration job of a database with rows would fail; keep the list, the scope and implicit fields, the upsert keys of the collections over %s and the sensitivity of its searched fields as the base has them", before.Entity, before.Indexes, after.Indexes, before.Entity)})
		}
	}
	if len(earlier) > 0 || len(faults) > 0 {
		return faults
	}
	baseOrder, headOrder := make([]string, len(o.base)), []string{}
	for i, table := range o.base {
		baseOrder[i] = table.Table
	}
	for _, table := range o.head {
		if slices.Contains(baseOrder, table.Table) {
			headOrder = append(headOrder, table.Table)
		}
	}
	switch {
	case len(o.head) != len(o.base) || !slices.Equal(baseOrder, headOrder):
		faults = append(faults, BaseFault{Rule: BaseCompatRule, Message: fmt.Sprintf(
			"the baseline of the head creates the tables (%s) and the base creates (%s) in 001_initial; a database with rows holds the 001_initial of the base, so keep the entities of the base and their references as the base has them, and add each new entity with a schema step", strings.Join(headOrder, ", "), strings.Join(baseOrder, ", "))})
	case o.baseMarker != o.headMarker:
		faults = append(faults, BaseFault{Rule: BaseCompatRule, Message: fmt.Sprintf(
			"the schema marker of the head has the digest %s and the base has %s; a database with rows holds the marker of the base, so the head would fail its marker check; keep the stored shape of the base", o.headMarker, o.baseMarker)})
	}
	return faults
}

// collectionIndexFaults refuses a head that changes, with no schema step, an index that the collections over an
// entity of the base give: whether a collection lists the entity, the scope and implicit fields of a collection that
// lists it, which have a search index (ST-SUP-RP11-LC20), or an upsert key, which has a unique index. No kind of
// schema change builds such an index yet, so a change of it would change the indexes of 001_initial and fail the
// migration job of a database with rows (decision on ST-GEN-Q13 part 1 (b), 2026-11-13). The field names of the
// head go back through its renames to the names of the base, so a rename alone changes no index here.
func collectionIndexFaults(base, head *types.ServiceDeclaration) []BaseFault {
	var faults []BaseFault
	for _, entity := range base.Entities {
		if _, kept := findStoredEntity(head, entity.Name); !kept {
			continue
		}
		before, after := collectionIndexes(base, entity.Name), collectionIndexes(head, entity.Name)
		if before != after {
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: entity.Name, Message: fmt.Sprintf(
				"the collections over %s change its indexes: the base gives %s and the head gives %s. No schema change builds an index of a collection yet, so the next migration job of a database with rows would fail; keep the list, the scope and implicit fields and the upsert keys of the collections over %s as the base has them",
				entity.Name, before, after, entity.Name)})
		}
	}
	return faults
}

// collectionIndexes describes the indexes that the collections of the declaration give the entity, with the field
// names of the declaration before its renames: whether a collection lists it, the fields with a search index when a
// collection lists it, and each upsert key, each part in a fixed order. The fields with a search index are those
// that 001_initial indexes, as the postgres-adapter's searchColumns gives them: the scope and implicit fields of each
// collection that lists the entity, and each field that the search takes (postgresadapter.Searchable) and that no
// add step adds, as the migration of that step builds its index. A field goes back through the renames of the entity
// in the order of the schema history, the last step first, so renames of one field one after the other give the
// name of the base whatever the order of their schema changes (review of PR #263, L11).
func collectionIndexes(service *types.ServiceDeclaration, entity string) string {
	position := map[string]int{}
	for i, step := range service.SchemaHistory {
		if _, seen := position[step.Change]; !seen {
			position[step.Change] = i
		}
	}
	// A rename with no step yet comes after each one with a step, in the order of the schema changes.
	at := func(change types.SchemaChange) int {
		if i, ok := position[change.ID]; ok {
			return i
		}
		return len(service.SchemaHistory)
	}
	var renames []types.SchemaChange
	added := map[string]bool{}
	// first is the first retype of each field in the order of the schema history: the baseline has the field of its
	// type From, so the search index of 001_initial follows that field, not the declared one (review of PR #314, L14).
	first := map[string]types.SchemaChange{}
	for _, change := range service.SchemaChanges {
		if change.Entity != entity {
			continue
		}
		switch change.Kind {
		case "rename":
			renames = append(renames, change)
		case "add", "require":
			if change.Field != "" {
				added[change.Field] = true
			}
		case "retype":
			if earlier, seen := first[change.Field]; !seen || at(change) < at(earlier) {
				first[change.Field] = change
			}
		}
	}
	slices.SortStableFunc(renames, func(a, b types.SchemaChange) int { return at(a) - at(b) })
	original := func(field string) string {
		for i := len(renames) - 1; i >= 0; i-- {
			if renames[i].Field == field {
				field = renames[i].From
			}
		}
		return field
	}
	listed := false
	filtered := map[string]bool{}
	var upserts []string
	for _, collection := range service.Collections {
		if collection.Entity != entity {
			continue
		}
		if slices.Contains(collection.Operations, types.OpList) {
			listed = true
			for field := range collection.Scope {
				filtered[original(field)] = true
			}
			for field := range collection.Implicit {
				filtered[original(field)] = true
			}
		}
		if len(collection.UpsertKey) > 0 {
			var key []string
			for _, field := range collection.UpsertLookup() {
				key = append(key, original(field))
			}
			upserts = append(upserts, "("+strings.Join(key, ", ")+")")
		}
	}
	if declared, ok := findStoredEntity(service, entity); ok && listed {
		for _, field := range declared.Fields {
			if retype, ok := first[field.Name]; ok {
				sensitivity := field.Sensitivity
				field = gen.RetypeField(retype)
				field.Sensitivity = sensitivity
			}
			if !added[field.Name] && postgresadapter.Searchable(field) {
				filtered[original(field.Name)] = true
			}
		}
	}
	fields := slices.Sorted(maps.Keys(filtered))
	slices.Sort(upserts)
	return fmt.Sprintf("listed %t, list fields [%s], upsert keys [%s]", listed, strings.Join(fields, ", "), strings.Join(upserts, ", "))
}

// uniqueAddFaults refuses a new add of a field that is unique and has a default, on an entity of the base: every
// row that the table holds gets the default, so the unique index cannot be built.
func uniqueAddFaults(base, head *types.ServiceDeclaration) []BaseFault {
	var faults []BaseFault
	for _, change := range head.SchemaChanges {
		if change.Kind != "add" || change.Field == "" || slices.ContainsFunc(base.SchemaChanges, func(c types.SchemaChange) bool { return c.ID == change.ID }) {
			continue
		}
		if _, declared := findStoredEntity(base, change.Entity); !declared {
			continue
		}
		entity, _ := findStoredEntity(head, change.Entity)
		for _, field := range entity.Fields {
			if field.Name == change.Field && (field.Unique || field.UniqueWhenLive || len(field.UniqueComposite) > 0) && field.Default != nil {
				faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: change.Entity, Field: change.Field, Message: fmt.Sprintf("the add %s gives %s.%s both unique and a default; every row of the table gets the default, so its unique index cannot be built: remove the default and make the field optional, or leave out unique", change.ID, change.Entity, change.Field)})
			}
		}
	}
	for _, change := range head.SchemaChanges {
		if change.Kind != "unique" || slices.ContainsFunc(base.SchemaChanges, func(c types.SchemaChange) bool { return c.ID == change.ID }) {
			continue
		}
		if slices.ContainsFunc(base.Invariants, func(invariant types.DeclaredInvariant) bool { return invariant.Name == change.Field }) {
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: change.Entity, Field: change.Field, Message: fmt.Sprintf("the unique change %s names %s, an invariant that the base declares; a unique change makes a new invariant, so give it a name that the base does not use", change.ID, change.Field)})
		}
	}
	return faults
}

func findStoredEntity(service *types.ServiceDeclaration, name string) (types.Entity, bool) {
	for _, entity := range service.Entities {
		if entity.Name == name {
			return entity, true
		}
	}
	return types.Entity{}, false
}

// baseStepFaults refuses a head whose schema history does not start with the base's steps in the same order.
func baseStepFaults(base, head *types.ServiceDeclaration) []BaseFault {
	for i, step := range base.SchemaHistory {
		if i < len(head.SchemaHistory) && head.SchemaHistory[i] == step {
			continue
		}
		found := "no step"
		if i < len(head.SchemaHistory) {
			found = fmt.Sprintf("the step %s %s", head.SchemaHistory[i].Change, head.SchemaHistory[i].Phase)
		}
		return []BaseFault{{Rule: BaseCompatRule, Entity: changeEntity(base, step.Change), Message: fmt.Sprintf("schema_history[%d] of the base is the step %s %s, and the head has %s there; the head's schema history must start with the base's steps in the same order and unchanged, and put each new step at the end", i, step.Change, step.Phase, found)}}
	}
	return nil
}

// baseChangeFaults refuses a head that removes a schema change of the base or changes it in any way.
func baseChangeFaults(base, head *types.ServiceDeclaration) []BaseFault {
	var faults []BaseFault
	for _, change := range base.SchemaChanges {
		index := slices.IndexFunc(head.SchemaChanges, func(c types.SchemaChange) bool { return c.ID == change.ID })
		if index < 0 {
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: change.Entity, Field: change.Field, Message: fmt.Sprintf("the head removes the schema change %s of the base; the declaration keeps every schema change, also a finished one, until a new baseline folds it (ST-GEN-UC15-R03)", change.ID)})
			continue
		}
		// A fill value of a retype is in the text of its expand step, which the ledger digest covers, so it does not
		// change either (decision on ST-GEN-Q24 (c), orchestrator under the owner's delegation, 2026-11-14).
		if now := head.SchemaChanges[index]; !reflect.DeepEqual(now, change) {
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: change.Entity, Field: change.Field, Message: fmt.Sprintf("the head changes the schema change %s of the base; a schema change that a release declared stays as it is, also its fill value, which its expand step holds", change.ID)})
		}
	}
	return faults
}

// onePhaseFaults refuses a head whose new steps give one schema change its expand and its contract step, because
// the release before the contract step must run on the expanded schema first.
func onePhaseFaults(base, head *types.ServiceDeclaration) []BaseFault {
	var faults []BaseFault
	if len(head.SchemaHistory) <= len(base.SchemaHistory) {
		return nil
	}
	newSteps := head.SchemaHistory[len(base.SchemaHistory):]
	for _, step := range newSteps {
		if step.Phase != "contract" {
			continue
		}
		if slices.Contains(newSteps, types.SchemaStep{Change: step.Change, Phase: "expand"}) {
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: changeEntity(head, step.Change), Message: fmt.Sprintf("the head gives the schema change %s its expand and its contract step in one release; a release moves each change one phase, so remove the contract step and put it at the end of the schema history of the next release", step.Change), Declare: []SuggestedChange{suggestion(head, step.Change, "expand")}})
		}
	}
	return faults
}

func suggestion(service *types.ServiceDeclaration, id, phase string) SuggestedChange {
	for _, change := range service.SchemaChanges {
		if change.ID == id {
			return SuggestedChange{ID: id, Kind: change.Kind, Entity: change.Entity, Field: change.Field, From: change.From, Phase: phase}
		}
	}
	return SuggestedChange{ID: id, Phase: phase}
}

func changeEntity(service *types.ServiceDeclaration, id string) string {
	for _, change := range service.SchemaChanges {
		if change.ID == id {
			return change.Entity
		}
	}
	return ""
}

// phaseOf gives the phase of a schema change, the phase of its last step, or "" when it has no step.
func phaseOf(service *types.ServiceDeclaration, id string) string {
	phase := ""
	for _, step := range service.SchemaHistory {
		if step.Change == id {
			phase = step.Phase
		}
	}
	return phase
}

// storedField is the part of a declared field that the table holds: the column's type, its length and numeric
// bounds, its values, whether it can be NULL, its default, its uniqueness and its reference. A pattern stays with
// request validation, so it is not part of it. A field whose old shape a schema change does not state, the old
// column of a retype or of a drop, is loose: only its type is known.
type storedField struct {
	Name  string
	Shape fieldShape
	Loose bool
}

type fieldShape struct {
	Type            types.FieldType
	MinLength       *int
	MaxLength       *int
	Min             *float64
	Max             *float64
	Unique          bool
	UniqueComposite []string
	UniqueWhenLive  bool
	Optional        bool
	Default         any
	Values          []string
	To              string
}

func shapeOf(field types.Field) fieldShape {
	return fieldShape{Type: field.Type, MinLength: field.MinLength, MaxLength: field.MaxLength, Min: field.Min, Max: field.Max,
		Unique: field.Unique, UniqueComposite: field.UniqueComposite, UniqueWhenLive: field.UniqueWhenLive,
		Optional: field.Optional, Default: field.Default, Values: field.Values, To: field.To}
}

// storedEntity is the stored shape of one entity: its fields in the order of the declaration, the declared
// invariants of kind unique on it, and the settings of the entity that change its table: its tenant, versioned,
// kind and generation fields. A loose entity is one that a drop removed: only its name is known.
type storedEntity struct {
	Name     string
	Fields   []storedField
	Uniques  []storedUnique
	Settings string
	Loose    bool
}

// storedUnique is a declared invariant of kind unique: its name, its columns and its condition.
type storedUnique struct {
	Name, By, Where string
}

// entitySettings gives the settings of an entity that change its table, with the defaults written out.
func entitySettings(entity types.Entity) string {
	tenant, kind := entity.Tenant, entity.Kind
	if tenant == "" {
		tenant = "owned"
	}
	if kind == "" {
		kind = "data"
	}
	return fmt.Sprintf("tenant %s, kind %s, versioned %t, generation_fields [%s]", tenant, kind, entity.Versioned, strings.Join(entity.GenerationFields, ", "))
}

func (e *storedEntity) field(name string) int {
	return slices.IndexFunc(e.Fields, func(f storedField) bool { return f.Name == name })
}

// shapeAt gives the stored shape of a declaration with each schema change undone whose expand step is at the
// position from of the schema history or after it, last step first: from 0 gives the baseline, with every step
// undone. An add removes its field or entity, a rename gives the field its From name, a retype gives it its From
// type, a require removes its field, a drop brings back its field or entity, a unique removes its invariant, and a
// relax gives its field what it was before the change (types.SchemaChange.RelaxedFrom). A relax that does not relax
// its field is not undone, so the shape before it differs from the base, and relaxFaults names the reason.
func shapeAt(service *types.ServiceDeclaration, from int) []*storedEntity {
	var entities []*storedEntity
	for _, entity := range service.Entities {
		stored := &storedEntity{Name: entity.Name, Settings: entitySettings(entity)}
		for _, field := range entity.Fields {
			stored.Fields = append(stored.Fields, storedField{Name: field.Name, Shape: shapeOf(field)})
		}
		for _, invariant := range service.Invariants {
			if invariant.Kind == "unique" && invariant.On == entity.Name {
				stored.Uniques = append(stored.Uniques, storedUnique{Name: invariant.Name, By: strings.Join(invariant.By, ", "), Where: invariant.Where})
			}
		}
		entities = append(entities, stored)
	}
	find := func(name string) int {
		return slices.IndexFunc(entities, func(e *storedEntity) bool { return e.Name == name })
	}
	for _, change := range undoOrder(service, from) {
		at := find(change.Entity)
		if change.Kind == "drop" && change.Field == "" {
			if at < 0 {
				entities = append(entities, &storedEntity{Name: change.Entity, Loose: true})
			}
			continue
		}
		if change.Kind == "add" && change.Field == "" {
			if at >= 0 {
				entities = slices.Delete(entities, at, at+1)
			}
			continue
		}
		if at < 0 {
			continue
		}
		entity := entities[at]
		field := entity.field(change.Field)
		switch change.Kind {
		case "add", "require":
			if field >= 0 {
				entity.Fields = slices.Delete(entity.Fields, field, field+1)
			}
		case "rename":
			if field >= 0 {
				entity.Fields[field].Name = change.From
			}
		case "retype":
			if field >= 0 {
				entity.Fields[field].Shape = fieldShape{Type: types.FieldType(change.From)}
				entity.Fields[field].Loose = true
			}
		case "drop":
			if field < 0 {
				entity.Fields = append(entity.Fields, storedField{Name: change.Field, Shape: fieldShape{Type: definitionType(change.Definition)}, Loose: true})
			}
		case "unique":
			entity.Uniques = slices.DeleteFunc(entity.Uniques, func(unique storedUnique) bool { return unique.Name == change.Field })
		case "relax":
			if field >= 0 && !entity.Fields[field].Loose {
				if before, reason := change.RelaxedFrom(fieldOf(entity.Fields[field])); reason == "" {
					entity.Fields[field].Shape = shapeOf(before)
				}
			}
		}
	}
	return entities
}

// definitionType reads the type of a dropped field from its Definition, [name:] type[, constraint value]....
func definitionType(definition string) types.FieldType {
	head, _, _ := strings.Cut(definition, ",")
	if _, typ, named := strings.Cut(head, ":"); named {
		head = typ
	}
	return types.FieldType(strings.TrimSpace(head))
}

// undoOrder gives the schema changes to undo, in the order that undoes them: each change whose expand step is at
// the position from or after it, the change whose expand step is last first. A change with no step, which checking
// refuses, is undone before all, unless from is past the end of the schema history.
func undoOrder(service *types.ServiceDeclaration, from int) []types.SchemaChange {
	position := map[string]int{}
	for i, step := range service.SchemaHistory {
		if _, seen := position[step.Change]; !seen {
			position[step.Change] = i
		}
	}
	var changes []types.SchemaChange
	for _, change := range service.SchemaChanges {
		at, stepped := position[change.ID]
		if (stepped && at >= from) || (!stepped && from <= len(service.SchemaHistory)) {
			changes = append(changes, change)
		}
	}
	slices.SortStableFunc(changes, func(a, b types.SchemaChange) int {
		pa, oka := position[a.ID]
		pb, okb := position[b.ID]
		switch {
		case !oka && !okb:
			return 0
		case !oka:
			return -1
		case !okb:
			return 1
		}
		return pb - pa
	})
	return changes
}

// kebab gives a name in lower kebab case, as a schema change ID takes it.
func kebab(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), "_", "-")
}

// baselineFaults compares a stored shape of the head with one of the base, entity by entity and field by field,
// and gives one fault for each change that no schema step declares, with the schema change to declare. A change to
// the settings of an entity's table, or to a unique invariant under its name, has no schema change that makes it
// safe.
func baselineFaults(base, head []*storedEntity) []BaseFault {
	var faults []BaseFault
	byName := func(entities []*storedEntity, name string) *storedEntity {
		for _, entity := range entities {
			if entity.Name == name {
				return entity
			}
		}
		return nil
	}
	for _, entity := range head {
		old := byName(base, entity.Name)
		if old == nil {
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: entity.Name, Message: fmt.Sprintf("the head adds the entity %s, which no schema step declares; every change after the first release is a declared schema step, so declare it, or run stego apply, which writes the add step", entity.Name),
				Declare: []SuggestedChange{{ID: "add-" + kebab(entity.Name), Kind: "add", Entity: entity.Name, Phase: "expand"}}})
			continue
		}
		if old.Loose || entity.Loose {
			continue
		}
		if old.Settings != entity.Settings {
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: entity.Name, Message: fmt.Sprintf("the head changes the table settings of %s from %s to %s; the release that runs now reads and writes the table as it is, and no schema change moves these settings, so keep them, or add a new entity", entity.Name, old.Settings, entity.Settings)})
		}
		faults = append(faults, fieldFaults(old, entity)...)
		for _, unique := range entity.Uniques {
			at := slices.IndexFunc(old.Uniques, func(u storedUnique) bool { return u.Name == unique.Name })
			switch {
			case at < 0:
				faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: entity.Name, Field: unique.Name, Message: fmt.Sprintf("the head declares the invariant %s of kind unique on %s, which no schema step declares; rows of the base can break it, so declare a unique change, whose backfill counts the duplicate groups", unique.Name, entity.Name),
					Declare: []SuggestedChange{{ID: kebab(entity.Name) + "-" + kebab(unique.Name) + "-unique", Kind: "unique", Entity: entity.Name, Field: unique.Name, Phase: "expand"}}})
			case old.Uniques[at] != unique:
				faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: entity.Name, Field: unique.Name, Message: fmt.Sprintf("the head changes the invariant %s of kind unique on %s from by [%s] where %q to by [%s] where %q; a unique invariant of the base stays as it is, so keep it and declare a new invariant with its own unique change", unique.Name, entity.Name, old.Uniques[at].By, old.Uniques[at].Where, unique.By, unique.Where)})
			}
		}
	}
	for _, entity := range base {
		now := byName(head, entity.Name)
		if now != nil && !now.Loose && !entity.Loose {
			for _, unique := range entity.Uniques {
				if !slices.ContainsFunc(now.Uniques, func(u storedUnique) bool { return u.Name == unique.Name }) {
					faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: entity.Name, Field: unique.Name, Message: fmt.Sprintf("the head removes or changes the invariant %s of kind unique on %s, which the base declares; a unique invariant of the base stays as it is, so keep it", unique.Name, entity.Name)})
				}
			}
		}
		if now == nil {
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: entity.Name, Message: fmt.Sprintf("the head removes the entity %s, which no schema step declares; the release that runs now still reads and writes its table", entity.Name),
				Declare: []SuggestedChange{{ID: kebab(entity.Name) + "-drop", Kind: "drop", Entity: entity.Name, Needs: "the Definition of its fields", Phase: "expand"}}})
		}
	}
	return faults
}

// fieldFaults compares the fields of one entity in the base's and the head's baselines. A field of the base that
// the head does not have, with a new field of the same shape, is a rename; with none, it is a drop, and the fault
// also names the add or require of each new field. A field whose shape changes is a retype. A new field that is
// optional or has a default is an add, and a new required field with no default is a require.
func fieldFaults(base, head *storedEntity) []BaseFault {
	var faults []BaseFault
	var added []storedField
	for _, field := range head.Fields {
		if base.field(field.Name) < 0 {
			added = append(added, field)
		}
	}
	var dropped []storedField
	for _, field := range base.Fields {
		at := head.field(field.Name)
		if at < 0 {
			pair := slices.IndexFunc(added, func(a storedField) bool { return !field.Loose && !a.Loose && reflect.DeepEqual(a.Shape, field.Shape) })
			if pair < 0 {
				dropped = append(dropped, field)
				continue
			}
			to := added[pair].Name
			added = slices.Delete(added, pair, pair+1)
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: head.Name, Field: field.Name, Message: fmt.Sprintf("the head renames %s.%s to %s, which no schema step declares; the release that runs now still reads and writes the column %s", head.Name, field.Name, to, field.Name),
				Declare: []SuggestedChange{{ID: kebab(head.Name) + "-" + kebab(field.Name) + "-rename", Kind: "rename", Entity: head.Name, Field: to, From: field.Name, Phase: "expand"}}})
			continue
		}
		now := head.Fields[at]
		if field.Loose || now.Loose {
			if field.Shape.Type == now.Shape.Type {
				continue
			}
		} else if reflect.DeepEqual(field.Shape, now.Shape) {
			continue
		}
		if definition, ok := relaxDefinition(field.Shape, now.Shape); ok && !field.Loose && !now.Loose {
			// A relax changes the field in place (decision on finding F12, owner, 2026-11-13).
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: head.Name, Field: field.Name, Message: fmt.Sprintf("the head relaxes %s.%s from %s in place, which no schema step declares; declare a relax with the definition %s", head.Name, field.Name, definitionOf(field.Shape), definition),
				Declare: []SuggestedChange{{ID: kebab(head.Name) + "-" + kebab(field.Name) + "-relax", Kind: "relax", Entity: head.Name, Field: field.Name, Needs: "the definition " + definition, Phase: "expand"}}})
			continue
		}
		if field.Shape.Type == now.Shape.Type {
			// Only a relax changes the constraints of a field that keeps its type: a longer max_length, more enum
			// values, a new default or an optional field. A narrowing is no schema change.
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: head.Name, Field: field.Name, Message: fmt.Sprintf("the head changes the stored shape of %s.%s from %s and keeps its type, which no schema step declares; a relax only makes a max_length longer, adds enum values, gives a default to a field with none or makes a field optional, so keep the other constraints, or add a new field with the new constraints and drop this one", head.Name, field.Name, definitionOf(field.Shape))})
			continue
		}
		faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: head.Name, Field: field.Name, Message: fmt.Sprintf("the head changes the stored shape of %s.%s from %s, which no schema step declares; a new column takes the new shape, and the release that runs now keeps the old one", head.Name, field.Name, definitionOf(field.Shape)),
			Declare: []SuggestedChange{{ID: kebab(head.Name) + "-" + kebab(field.Name) + "-retype", Kind: "retype", Entity: head.Name, Field: field.Name, From: string(field.Shape.Type), Needs: "a conversion and an inverse conversion", Phase: "expand"}}})
	}
	addition := func(field storedField) SuggestedChange {
		if field.Shape.Optional || field.Shape.Default != nil {
			return SuggestedChange{ID: "add-" + kebab(head.Name) + "-" + kebab(field.Name), Kind: "add", Entity: head.Name, Field: field.Name, Phase: "expand"}
		}
		return SuggestedChange{ID: kebab(head.Name) + "-" + kebab(field.Name) + "-require", Kind: "require", Entity: head.Name, Field: field.Name, Needs: "a fill value", Phase: "expand"}
	}
	for i, field := range dropped {
		declare := []SuggestedChange{{ID: kebab(head.Name) + "-" + kebab(field.Name) + "-drop", Kind: "drop", Entity: head.Name, Field: field.Name, Needs: "the Definition " + definitionOf(field.Shape), Phase: "expand"}}
		message := fmt.Sprintf("the head removes %s.%s, which no schema step declares; the release that runs now still reads and writes the column %s", head.Name, field.Name, field.Name)
		if i == 0 && len(added) > 0 {
			var names []string
			for _, field := range added {
				declare = append(declare, addition(field))
				names = append(names, field.Name)
			}
			message += fmt.Sprintf(", and adds %s, which no schema step declares either", strings.Join(names, ", "))
			added = nil
		}
		faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: head.Name, Field: field.Name, Message: message, Declare: declare})
	}
	for _, field := range added {
		suggested := addition(field)
		message := fmt.Sprintf("the head adds %s.%s, which no schema step declares; every change after the first release is a declared schema step, so declare it, or run stego apply, which writes the add step", head.Name, field.Name)
		if suggested.Kind == "require" {
			message = fmt.Sprintf("the head adds the required field %s.%s with no default, which no schema step declares; the rows of the base have no value for it and the release that runs now writes none, so declare a require with a fill value", head.Name, field.Name)
		}
		faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: head.Name, Field: field.Name, Message: message, Declare: []SuggestedChange{suggested}})
	}
	return faults
}

// definitionOf gives a field's shape as the Definition of a drop takes it, such as string, max_length 255.
func definitionOf(shape fieldShape) string {
	return string(shape.Type) + definitionConstraints(shape)
}

func definitionConstraints(shape fieldShape) string {
	var parts []string
	if shape.MinLength != nil {
		parts = append(parts, fmt.Sprintf("min_length %d", *shape.MinLength))
	}
	if shape.MaxLength != nil {
		parts = append(parts, fmt.Sprintf("max_length %d", *shape.MaxLength))
	}
	if shape.Min != nil {
		parts = append(parts, fmt.Sprintf("min %v", *shape.Min))
	}
	if shape.Max != nil {
		parts = append(parts, fmt.Sprintf("max %v", *shape.Max))
	}
	if shape.Unique {
		parts = append(parts, "unique")
	}
	if shape.Optional {
		parts = append(parts, "optional")
	}
	if shape.Default != nil {
		parts = append(parts, fmt.Sprintf("default %v", shape.Default))
	}
	if len(parts) == 0 {
		return ""
	}
	return ", " + strings.Join(parts, ", ")
}

// RelaxRuntimeVersion is the first runtime library version whose schema check accepts the relaxed forms of a column
// that the relax step marks (ST-SUP-RP15-LC04; decision on finding F12, owner, 2026-11-13).
const RelaxRuntimeVersion = "v0.108.0"

// relaxRuntimeFaults refuses a head with a new relax step when the record at the base pins a runtime library before
// RelaxRuntimeVersion, or none: the schema check of that release refuses the relaxed column, so its migration job
// refuses the database after the step. The fault names the runtime version to deploy first (ST-GEN-INV36; decision
// on finding F11, orchestrator under the owner's delegation, 2026-11-13). A base with no postgres-adapter has no
// schema check of its tables.
func relaxRuntimeFaults(applied *AppliedState, base, head *types.ServiceDeclaration) []BaseFault {
	if _, pinned := applied.Components["postgres-adapter"]; !pinned {
		return nil
	}
	version := ""
	if applied.Archetype != nil && applied.Archetype.Runtime != nil {
		version = applied.Archetype.Runtime.Version
	}
	if semver.IsValid(version) && semver.Compare(version, RelaxRuntimeVersion) >= 0 {
		return nil
	}
	kinds := map[string]string{}
	for _, change := range head.SchemaChanges {
		kinds[change.ID] = change.Kind
	}
	for _, step := range head.SchemaHistory[min(len(base.SchemaHistory), len(head.SchemaHistory)):] {
		if kinds[step.Change] != "relax" {
			continue
		}
		pinned := version
		if pinned == "" {
			pinned = "no runtime library"
		}
		return []BaseFault{{Rule: StepLedgerRule, Entity: changeEntity(head, step.Change), Message: fmt.Sprintf(
			"the base runs %s, whose schema check refuses the column that the relax %s changes in place, so its migration job refuses the database after the step; deploy a release with the runtime library %s or later, with no new schema step, first", pinned, step.Change, RelaxRuntimeVersion)}}
	}
	return nil
}

// relaxFaults refuses each new relax of a field of the base that does not relax it in place
// (types.SchemaChange.RelaxedFrom): a shorter max_length, an enum value taken away, a field made required, or a
// retype that PostgreSQL cannot make with no rewrite. The baselines then differ too, as shapeAt does not undo it.
func relaxFaults(base, head *types.ServiceDeclaration) []BaseFault {
	var faults []BaseFault
	for _, change := range head.SchemaChanges {
		if change.Kind != "relax" || slices.ContainsFunc(base.SchemaChanges, func(c types.SchemaChange) bool { return c.ID == change.ID }) {
			continue
		}
		// The relax is checked against its field as its own step leaves it, each later step undone (decision on
		// ST-DECL-Q17 (a), orchestrator under the owner's delegation, 2026-11-14).
		after, found, reason := declaration.FieldAtStep(head, change)
		if !found && reason == "" {
			continue
		}
		if reason == "" {
			_, reason = change.RelaxedFrom(after)
		}
		if reason != "" {
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: change.Entity, Field: change.Field, Message: fmt.Sprintf("the relax %s of %s.%s does not relax the field in place: %s", change.ID, change.Entity, change.Field, reason)})
		}
	}
	return faults
}

// withinFaults refuses a head that holds a reference of an entity of the base within another field than the base
// does, with no new schema change of kind within on the reference (ST-SUP-RC01-OC46, ST-DECL-Q42): the tables of the
// base have rows, so a composite key that the head adds is a within step, which builds the unique key, adds the key
// NOT VALID and validates it over the rows. A key that the head drops or moves has no step yet, so it is refused
// with no suggestion. A reference that the head adds with its field is a new column, which an add step adds.
func withinFaults(base, head *types.ServiceDeclaration) []BaseFault {
	var faults []BaseFault
	for _, now := range head.Entities {
		at := slices.IndexFunc(base.Entities, func(e types.Entity) bool { return e.Name == now.Name })
		if at < 0 {
			continue
		}
		was := base.Entities[at]
		for _, name := range slices.Sorted(maps.Keys(now.References)) {
			before, after := was.References[name].WithinField(), now.References[name].WithinField()
			if before == after || !slices.ContainsFunc(was.Fields, func(f types.Field) bool { return f.Name == name }) {
				continue
			}
			stepped := slices.ContainsFunc(head.SchemaChanges, func(c types.SchemaChange) bool {
				return c.Kind == "within" && c.Entity == now.Name && c.Field == name && !slices.ContainsFunc(base.SchemaChanges, func(b types.SchemaChange) bool { return b.ID == c.ID })
			})
			switch {
			case before == "" && stepped:
			case before == "":
				faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: now.Name, Field: name, Message: fmt.Sprintf("the head holds the reference %s.%s within %s, which no schema step declares; the tables of the release that runs now have rows, so declare a within step, which adds the key and checks the rows", now.Name, name, after),
					Declare: []SuggestedChange{{ID: kebab(now.Name) + "-" + kebab(name) + "-within", Kind: "within", Entity: now.Name, Field: name, Phase: "expand"}}})
			default:
				faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: now.Name, Field: name, Message: fmt.Sprintf("the head changes the within of the reference %s.%s from %s, and no schema step drops a composite key yet; keep within: %s", now.Name, name, before, before)})
			}
		}
	}
	return faults
}

// fieldOf gives a stored field back as a declared field, for types.SchemaChange.RelaxedFrom.
func fieldOf(field storedField) types.Field {
	s := field.Shape
	return types.Field{Name: field.Name, Type: s.Type, MinLength: s.MinLength, MaxLength: s.MaxLength, Min: s.Min, Max: s.Max,
		Unique: s.Unique, UniqueComposite: s.UniqueComposite, UniqueWhenLive: s.UniqueWhenLive, Optional: s.Optional,
		Default: s.Default, Values: s.Values, To: s.To}
}

// relaxDefinition gives the Definition of the relax that takes a field from the shape before to the shape after,
// and false when no relax does: the change is not only a longer max_length, more enum values, a default where there
// was none, an optional field, or an enum made a string with no max_length and no min_length.
func relaxDefinition(before, after fieldShape) (string, bool) {
	var items []string
	rest, restAfter := before, after
	switch {
	case before.Type == types.FieldTypeEnum && after.Type == types.FieldTypeString:
		if after.MaxLength != nil || after.MinLength != nil {
			return "", false
		}
		items = append(items, "type enum", "values "+strings.Join(before.Values, " "))
		rest.Type, rest.Values = after.Type, nil
	case before.Type != after.Type:
		return "", false
	}
	if !reflect.DeepEqual(before.MaxLength, after.MaxLength) && rest.Type == before.Type {
		if before.MaxLength == nil || after.MaxLength == nil || *after.MaxLength <= *before.MaxLength {
			return "", false
		}
		items = append(items, fmt.Sprintf("max_length %d", *before.MaxLength))
		rest.MaxLength = after.MaxLength
	}
	if !slices.Equal(before.Values, after.Values) && rest.Type == before.Type {
		for _, v := range before.Values {
			if !slices.Contains(after.Values, v) {
				return "", false
			}
		}
		items = append(items, "values "+strings.Join(before.Values, " "))
		rest.Values = after.Values
	}
	if before.Optional != after.Optional {
		if !after.Optional {
			return "", false
		}
		items = append(items, "required")
		rest.Optional = true
	}
	if !reflect.DeepEqual(before.Default, after.Default) {
		if before.Default != nil {
			return "", false
		}
		items = append(items, "no default")
		rest.Default = after.Default
	}
	if len(items) == 0 || !reflect.DeepEqual(rest, restAfter) {
		return "", false
	}
	return strings.Join(items, ", "), true
}
