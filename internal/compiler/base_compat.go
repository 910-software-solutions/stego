package compiler

import (
	"fmt"
	"maps"
	pathpkg "path"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/910-software-solutions/stego/internal/declaration"
	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/generator/postgresadapter"
	"github.com/910-software-solutions/stego/internal/parser"
	"github.com/910-software-solutions/stego/internal/types"
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
	// Schema is the .zed schema that the base declaration binds, at its schema path in the service folder of the
	// base, or nil when the base binds none or the commit does not hold it. RelateFaults reads it.
	Schema []byte
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
	declared := declaration
	if ClearSecrets(state.LastApplied) {
		// The base declares a secret field that its generator stored in clear (walkthrough 13, W13-B1): its stored
		// shape is the field in clear, which a seal from secret moves (ST-SUP-RC01-OC49, ST-DECL-INV102).
		declaration = withClearSecrets(declaration)
	}
	faults := validateBaseSchema(state.LastApplied, declaration, head)
	for i := range faults {
		for j, change := range faults[i].Declare {
			if field, ok := findStoredField(declared, change.Entity, change.Field); ok && change.Kind == "seal" && field.Secret() {
				faults[i].Declare[j].Needs = "from: secret, because the generator of the base stored the secret field in clear"
			}
		}
	}
	check.Faults = append(check.Faults, faults...)
	check.Faults = append(check.Faults, ledgerVersionFaults(state.LastApplied, declaration, head)...)
	check.Faults = append(check.Faults, syncRuntimeFaults(state.LastApplied, declaration, head)...)
	check.Faults = append(check.Faults, relaxRuntimeFaults(state.LastApplied, declaration, head)...)
	check.Faults = append(check.Faults, uniqueRuntimeFaults(state.LastApplied, declaration, head)...)
	check.Faults = append(check.Faults, sealRuntimeFaults(state.LastApplied, declaration, head)...)
	check.Faults = append(check.Faults, sealContractFaults(state.LastApplied, declaration, head)...)
	check.Faults = append(check.Faults, sealTogetherFaults(declaration, head)...)
	return check, nil
}

// SecretStorageVersion is the first postgres-adapter version that stores a secret field sealed or hashed, as the
// introduced_in of ST-SUP-RC01-OC48 names it; SecretRuntimeVersion is the first runtime library version that seals
// it (decision ST-DECL-Q44). An adapter before it stored a field that its declaration named secret in clear.
const (
	SecretStorageVersion = "6.13.0"
	SecretRuntimeVersion = "v0.135.0"
)

// ClearSecrets tells whether the record of a release names a postgres-adapter before SecretStorageVersion, or, with
// no postgres-adapter in the record, a runtime library before SecretRuntimeVersion: that release stores each field
// of sensitivity secret in clear, so its stored shape is the field in clear, and the head moves it to its sealed
// column with a seal from secret (walkthrough 13, W13-B1; ST-SUP-RC01-OC49). A version that does not parse, or a
// record with neither pin, counts as such a release, so the check fails closed (security review of PR #443, I3).
func ClearSecrets(applied *AppliedState) bool {
	return before(applied, SecretStorageVersion, SecretRuntimeVersion)
}

// before tells whether the record of a release names a postgres-adapter before adapter, or, with no postgres-adapter
// in the record, a runtime library before runtime; a version that does not parse, or no pin, counts as before.
func before(applied *AppliedState, adapter, runtime string) bool {
	if applied == nil {
		return true
	}
	if pin, pinned := applied.Components["postgres-adapter"]; pinned {
		version := "v" + strings.TrimPrefix(pin.Version, "v")
		return !semver.IsValid(version) || semver.Compare(version, "v"+adapter) < 0
	}
	if applied.Archetype != nil && applied.Archetype.Runtime != nil {
		version := applied.Archetype.Runtime.Version
		return !semver.IsValid(version) || semver.Compare(version, runtime) < 0
	}
	return true
}

// ReportAdapterVersion and ReportRuntimeVersion are the first postgres-adapter and runtime library versions whose
// processes report their release in stego_schema.releases (ST-SUP-RP09-LC14). The seal job and the contract of a seal
// see a release before the step only through those reports, so a seal needs a base that reports itself.
const (
	ReportAdapterVersion = "6.4.0"
	ReportRuntimeVersion = "v0.120.0"
)

// sealRuntimeFaults refuses a head with a new expand step of a seal when the record at the base names a
// postgres-adapter before ReportAdapterVersion, or, with no postgres-adapter, a runtime library before
// ReportRuntimeVersion, or neither: the processes of that release do not report themselves, so a write that they make
// to the clear column after the seal job passed its row is not seen, and the contract would drop it (security review
// of PR #443, H1; ST-SUP-RC01-OC49).
func sealRuntimeFaults(applied *AppliedState, base, head *types.ServiceDeclaration) []BaseFault {
	if !before(applied, ReportAdapterVersion, ReportRuntimeVersion) {
		return nil
	}
	kinds := map[string]string{}
	for _, change := range head.SchemaChanges {
		kinds[change.ID] = change.Kind
	}
	for _, step := range head.SchemaHistory[min(len(base.SchemaHistory), len(head.SchemaHistory)):] {
		if step.Phase != "expand" || kinds[step.Change] != "seal" {
			continue
		}
		return []BaseFault{{Rule: StepLedgerRule, Entity: changeEntity(head, step.Change), Message: fmt.Sprintf(
			"the base runs a release whose processes do not report themselves (postgres-adapter before %s or runtime library before %s, or no pin), so the seal job and the contract of the seal %s cannot see its writes to the clear column; deploy a release with the runtime library %s or later, with no new schema step, first, and then declare the seal", ReportAdapterVersion, ReportRuntimeVersion, step.Change, ReportRuntimeVersion)}}
	}
	return nil
}

// SealSwitchRuntimeVersion is the first runtime library version whose release of the expand step of a seal goes on
// after the contract step drops the clear column: it then reads and writes the sealed column alone
// (ST-SUP-RP10-LC49; walkthrough 14, W14-B6).
const SealSwitchRuntimeVersion = "v0.150.0"

// sealContractFaults refuses a head with a new contract step of a seal when the record at the base names a runtime
// library before SealSwitchRuntimeVersion, or none: the processes of that release, the release of the expand step,
// need the clear column, so each of their writes and reads of the entity fails once the contract drops it
// (ST-SUP-RC01-OC49). A pin that does not parse counts as such a release.
func sealContractFaults(applied *AppliedState, base, head *types.ServiceDeclaration) []BaseFault {
	if applied != nil && applied.Archetype != nil && applied.Archetype.Runtime != nil {
		version := applied.Archetype.Runtime.Version
		if semver.IsValid(version) && semver.Compare(version, SealSwitchRuntimeVersion) >= 0 {
			return nil
		}
	}
	kinds := map[string]string{}
	for _, change := range head.SchemaChanges {
		kinds[change.ID] = change.Kind
	}
	for _, step := range head.SchemaHistory[min(len(base.SchemaHistory), len(head.SchemaHistory)):] {
		if step.Phase != "contract" || kinds[step.Change] != "seal" {
			continue
		}
		return []BaseFault{{Rule: StepLedgerRule, Entity: changeEntity(head, step.Change), Message: fmt.Sprintf(
			"the base runs a release with the runtime library before %s, or no pin, whose processes need the clear column of the seal %s, so each of their writes and reads of the entity would fail after its contract step drops the column; deploy a release with the runtime library %s or later, with the seal still at expand, first, and then declare the contract step", SealSwitchRuntimeVersion, step.Change, SealSwitchRuntimeVersion)}}
	}
	return nil
}

// sealTogetherFaults refuses a head with a new contract step of a seal of an entity while the base holds another seal
// of the same entity open, at expand, that the head does not contract too: the release of the base switches the entity
// to its statements after the contract only when every clear column of its open seals is gone (AfterSeal), so with one
// clear column dropped and another there each of its writes and reads of the entity would fail (security review of
// PR #452, L3; ST-SUP-RC01-OC49). The remedy is to contract the open seals of an entity in one release.
func sealTogetherFaults(base, head *types.ServiceDeclaration) []BaseFault {
	changes := map[string]types.SchemaChange{}
	for _, change := range head.SchemaChanges {
		changes[change.ID] = change
	}
	open := map[string][]string{}
	for _, change := range base.SchemaChanges {
		if change.Kind == "seal" && phaseOf(base, change.ID) == "expand" {
			open[change.Entity] = append(open[change.Entity], change.ID)
		}
	}
	var faults []BaseFault
	named := map[string]bool{}
	for _, step := range head.SchemaHistory[min(len(base.SchemaHistory), len(head.SchemaHistory)):] {
		change, ok := changes[step.Change]
		if step.Phase != "contract" || !ok || change.Kind != "seal" || named[change.Entity] {
			continue
		}
		var left []string
		for _, id := range open[change.Entity] {
			if phaseOf(head, id) != "contract" {
				left = append(left, id)
			}
		}
		if len(left) == 0 {
			continue
		}
		named[change.Entity] = true
		faults = append(faults, BaseFault{Rule: StepLedgerRule, Entity: change.Entity, Message: fmt.Sprintf(
			"the head contracts the seal %s of %s while the base holds the seal %s of the same entity open; the release of the base reads and writes the sealed columns alone only when the clear column of each open seal of the entity is gone, so after this contract each of its writes and reads of %s would fail; declare the contract step of each open seal of %s in the same release",
			change.ID, change.Entity, strings.Join(left, ", "), change.Entity, change.Entity)})
	}
	return faults
}

// findStoredField gives the field of the entity of the declaration, live or dropped.
func findStoredField(service *types.ServiceDeclaration, entity, name string) (types.Field, bool) {
	for _, e := range append(slices.Clone(service.Entities), service.DroppedEntities...) {
		if e.Name != entity {
			continue
		}
		for _, f := range e.Fields {
			if f.Name == name {
				return f, true
			}
		}
	}
	return types.Field{}, false
}

// withClearSecrets gives a copy of the declaration in which each field of sensitivity secret, of a live or a dropped
// entity, is marked as stored in clear (types.Field.ClearStored): the stored shape of a release whose generator stored
// it in clear, with its CHECKs and no search index.
func withClearSecrets(service *types.ServiceDeclaration) *types.ServiceDeclaration {
	out := *service
	clearOf := func(entities []types.Entity) []types.Entity {
		copied := make([]types.Entity, len(entities))
		for i, entity := range entities {
			entity.Fields = append([]types.Field(nil), entity.Fields...)
			for j := range entity.Fields {
				if entity.Fields[j].Secret() {
					entity.Fields[j].ClearStored, entity.Fields[j].CompareOnly = true, false
				}
			}
			copied[i] = entity
		}
		return copied
	}
	out.Entities, out.DroppedEntities = clearOf(service.Entities), clearOf(service.DroppedEntities)
	return &out
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
	faults = append(faults, droppedEntityFaults(base, head)...)
	faults = append(faults, eventFieldFaults(base, head)...)
	generated, ok := generatedBaselines(applied, base, head)
	if !ok {
		// Generation does not carry out each kind of the declarations yet, so the indexes are compared as the
		// collections describe them.
		faults = append(faults, collectionIndexFaults(base, head)...)
	}
	// A drop of a field whose seal is open has a fault that names the seal, in place of the fault of its stored form.
	sealDrops := openSealDropFaults(base, head)
	named := map[[2]string]bool{}
	for _, fault := range sealDrops {
		named[[2]string{fault.Entity, fault.Field}] = true
	}
	shapeFaults := sealDrops
	for _, fault := range baselineFaults(shapeAt(base, 0), shapeAt(head, 0)) {
		if !named[[2]string{fault.Entity, fault.Field}] {
			shapeFaults = append(shapeFaults, fault)
		}
	}
	// The baselines leave out what a step of the base added, required or retyped, so the head with only its new
	// steps undone must also be the stored shape of the base as it runs now, with every field in full.
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

// openSealDropFaults refuses a drop that the head declares of a field whose seal the base holds open, or of the
// entity of such a field: the base has the expand step of the seal and not its contract step, so the release that
// runs now reads and writes the clear and the sealed column of the field. The fault names the seal and tells to
// contract it first (ST-SUP-RC01-OC49-T10, ST-SUP-RC01-OC49-T11). A drop of a field that the base stores secret,
// whose definition gives the field in clear, has a fault that says to add the item secret.
func openSealDropFaults(base, head *types.ServiceDeclaration) []BaseFault {
	var faults []BaseFault
	for _, drop := range head.SchemaChanges {
		if drop.Kind != "drop" || slices.ContainsFunc(base.SchemaChanges, func(c types.SchemaChange) bool { return c.ID == drop.ID }) {
			continue
		}
		sealed := false
		for _, seal := range base.SchemaChanges {
			if seal.Kind != "seal" || seal.Entity != drop.Entity || (drop.Field != "" && seal.Field != drop.Field) ||
				!slices.Contains(base.SchemaHistory, types.SchemaStep{Change: seal.ID, Phase: "expand"}) ||
				slices.Contains(base.SchemaHistory, types.SchemaStep{Change: seal.ID, Phase: "contract"}) {
				continue
			}
			sealed = true
			removes := fmt.Sprintf("%s.%s", drop.Entity, drop.Field)
			if drop.Field == "" {
				removes = fmt.Sprintf("the entity %s with %s.%s", drop.Entity, drop.Entity, seal.Field)
			}
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: drop.Entity, Field: drop.Field, Message: fmt.Sprintf(
				"the drop %s removes %s, whose seal %s is open in the base: the release that runs now reads and writes the clear and the sealed column of %s; "+
					"contract the seal first, with the step {change: %s, phase: contract} in a release of its own, then drop it in a later release, with the item secret in the definition of a sealed field",
				drop.ID, removes, seal.ID, seal.Field, seal.ID),
				Declare: []SuggestedChange{suggestion(base, seal.ID, "contract")}})
		}
		// A drop of a field that the base stores secret gives it in its Definition with the item secret, so the
		// field that the drop brings back has the stored form of the base (security review of PR #468, I2).
		if entity, found := findStoredEntity(base, drop.Entity); found && drop.Field != "" && !sealed {
			at := slices.IndexFunc(entity.Fields, func(f types.Field) bool { return f.Name == drop.Field })
			if dropped, reason := gen.DropDefinition(drop.Definition); at >= 0 && reason == "" && entity.Fields[at].Secret() && !entity.Fields[at].ClearStored && !dropped.Field.Secret() {
				faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: drop.Entity, Field: drop.Field, Message: fmt.Sprintf(
					"the definition of the drop %s gives %s.%s in clear, but the base stores it as a secret; add the item secret to the definition, before a pattern",
					drop.ID, drop.Entity, drop.Field)})
			}
		}
	}
	return faults
}

// droppedEntityFaults refuses a drop of an entity of the base that the head does not keep declared with dropped_by:
// the baseline and the contract come from the declaration alone, so the entity and the collections over it stay as
// the base has them (ST-DECL-INV58, ST-GEN-Q47).
func droppedEntityFaults(base, head *types.ServiceDeclaration) []BaseFault {
	var faults []BaseFault
	for _, change := range head.SchemaChanges {
		if change.Kind != "drop" || change.Field != "" {
			continue
		}
		_, inBase := findStoredEntity(base, change.Entity)
		kept := slices.ContainsFunc(head.DroppedEntities, func(e types.Entity) bool { return e.Name == change.Entity && e.DroppedBy == change.ID })
		if inBase && !kept {
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: change.Entity, Message: fmt.Sprintf("the drop %s removes the entity %s from the head; the release that runs now still reads and writes its table, and the migrations come from the declaration alone, so keep the entity and its collections as the base has them, with dropped_by: %s", change.ID, change.Entity, change.ID)})
		}
	}
	return faults
}

// baselineOutput is what the postgres-adapter generator gives for the baselines of the base and of the head: the
// tables of 001_initial and the digest of the schema marker (postgresadapter.Baseline).
type baselineOutput struct {
	base, head             []postgresadapter.BaselineTable
	baseMarker, headMarker string
	// headChanges are the schema changes of the head, by which a fault of a changed table names its cause.
	headChanges []types.SchemaChange
	// built is the baseline of the base as the generator of its release built it, when that is a postgres-adapter
	// before PublicSearchVersion and the base writes sensitivity: public on a field: that generator gave no search
	// index on such a field (decision ST-DECL-Q46 (a)).
	built []postgresadapter.BaselineTable
}

// PublicSearchVersion and PublicSearchRuntimeVersion are the first postgres-adapter and runtime library versions
// whose generator reads a field written sensitivity: public as a field with no sensitivity, so a list searches it and
// 001_initial gives its search index (decision ST-DECL-Q46 (a)).
const (
	PublicSearchVersion        = "6.19.0"
	PublicSearchRuntimeVersion = "v0.158.0"
)

// writtenPublic gives a copy of the entities with each field written sensitivity: public marked pii, which, as the
// generator before PublicSearchVersion read it, has no search index; and whether any field was written so.
func writtenPublic(entities []types.Entity) ([]types.Entity, bool) {
	out, any := slices.Clone(entities), false
	for i := range out {
		out[i].Fields = slices.Clone(out[i].Fields)
		for j := range out[i].Fields {
			if out[i].Fields[j].Sensitivity == "public" {
				out[i].Fields[j].Sensitivity, any = "pii", true
			}
		}
	}
	return out, any
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
	baseline := func(service *types.ServiceDeclaration, components []string, older bool) ([]postgresadapter.BaselineTable, string, error) {
		entities, dropped, err := storedEntities(service, service.SchemaChanges)
		if err != nil {
			return nil, "", err
		}
		if older {
			var written bool
			if entities, written = writtenPublic(entities); !written {
				return nil, "", nil
			}
		}
		return postgresadapter.Baseline(gen.Context{Entities: entities, Collections: gen.StoredCollections(service.Collections, service.SchemaChanges),
			SchemaChanges: service.SchemaChanges, SchemaHistory: service.SchemaHistory, Tenancy: pooledTenancy(service, components),
			DroppedEntities: dropped, DroppedCollections: gen.StoredCollections(service.DroppedCollections, service.SchemaChanges), EntityOrder: service.EntityOrder, CollectionOrder: service.CollectionOrder})
	}
	out := &baselineOutput{headChanges: head.SchemaChanges}
	var err error
	if out.base, out.baseMarker, err = baseline(base, baseComponents, false); err != nil {
		return nil, false
	}
	if out.head, out.headMarker, err = baseline(head, selected(head), false); err != nil {
		return nil, false
	}
	if before(applied, PublicSearchVersion, PublicSearchRuntimeVersion) {
		if out.built, _, err = baseline(base, baseComponents, true); err != nil {
			return nil, false
		}
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
	builtTables := map[string]postgresadapter.BaselineTable{}
	for _, table := range o.built {
		builtTables[table.Table] = table
	}
	for _, now := range o.base {
		if built, ok := builtTables[now.Table]; ok && !named[now.Entity] && built.Indexes != now.Indexes {
			// The database holds the indexes that the generator of the base built, and from PublicSearchVersion
			// 001_initial gives a search index on a field written public (decision ST-DECL-Q46 (a)).
			named[now.Entity] = true
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: now.Entity, Message: fmt.Sprintf(
				"the base writes sensitivity: public on a field of %s that a list searches, and its release (postgres-adapter before %s) built no search index on it; from postgres-adapter %s a field written public is searched as one with no sensitivity, so 001_initial gives\n%sand the database holds\n%sNo schema change builds such an index yet, so the next migration job of a database with rows would fail; keep the release of the base until a schema change can build the index (ST-DECL-Q46)", now.Entity, PublicSearchVersion, PublicSearchVersion, now.Indexes, built.Indexes)})
		}
	}
	for _, before := range o.base {
		after, ok := headTables[before.Table]
		switch {
		case named[before.Entity]:
		case !ok:
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: before.Entity, Message: fmt.Sprintf(
				"the baseline of the head has no table %s, and 001_initial of the base creates it; keep the entity %s of the base, or declare the schema change that removes it", before.Table, before.Entity)})
		case before.Create != after.Create:
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: before.Entity, Message: fmt.Sprintf(
				"the head changes the table %s of 001_initial with no schema step: %sA database with rows holds the table of the base, so the head's migration job would fail its ledger check; keep the fields of %s in the order and with the settings of the base, and declare each change as a schema change", before.Table, createDifference(before, after, o.headChanges), before.Entity)})
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
	// sealedFrom is the sensitivity before its seal of each field that a seal names: 001_initial creates the clear
	// column with the index of that sensitivity, so a seal from public keeps the search index of the field, also
	// when another change of the same release, such as a rename, keeps generation from comparing the baselines
	// (walkthrough 17, W17-B2).
	sealedFrom := map[string]string{}
	for _, change := range service.SchemaChanges {
		if change.Entity != entity {
			continue
		}
		switch change.Kind {
		case "seal":
			sealedFrom[change.Field] = change.From
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
		// A dropped field is in 001_initial with its search index, as its Definition gives it (ST-DECL-EN21).
		fields := slices.Clone(declared.Fields)
		for _, change := range service.SchemaChanges {
			if change.Kind != "drop" || change.Entity != entity || change.Field == "" {
				continue
			}
			if dropped, reason := gen.DropDefinition(change.Definition); reason == "" && !slices.ContainsFunc(fields, func(f types.Field) bool { return f.Name == change.Field }) {
				dropped.Field.Name = change.Field
				fields = append(fields, dropped.Field)
			}
		}
		for _, field := range fields {
			if retype, ok := first[field.Name]; ok {
				sensitivity := field.Sensitivity
				field = gen.RetypeField(retype)
				field.Sensitivity = sensitivity
			}
			if from, sealed := sealedFrom[field.Name]; sealed {
				// No from is the same as from: public (docs/schema-changes.md, Seal steps).
				field.Sensitivity = from
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
	// Secret and CompareOnly are the stored form of a secret field: sealed bytes, or an Argon2id hash (decision
	// ST-DECL-Q44), so a change of either changes what the column holds.
	Secret, CompareOnly bool
}

func shapeOf(field types.Field) fieldShape {
	return fieldShape{Type: field.Type, MinLength: field.MinLength, MaxLength: field.MaxLength, Min: field.Min, Max: field.Max,
		Unique: field.Unique, UniqueComposite: field.UniqueComposite, UniqueWhenLive: field.UniqueWhenLive,
		Optional: field.Optional, Default: field.Default, Values: field.Values, To: field.To,
		Secret: field.Secret() && !field.ClearStored, CompareOnly: field.Secret() && !field.ClearStored && field.CompareOnly}
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
// type, a require removes its field, a drop brings back its field or entity, a unique removes its invariant or takes
// the unique off its field (types.WithoutUnique), and a
// relax gives its field what it was before the change (types.SchemaChange.RelaxedFrom). A relax that does not relax
// its field is not undone, so the shape before it differs from the base, and relaxFaults names the reason.
func shapeAt(service *types.ServiceDeclaration, from int) []*storedEntity {
	var entities []*storedEntity
	stored := func(entity types.Entity) *storedEntity {
		stored := &storedEntity{Name: entity.Name, Settings: entitySettings(entity)}
		for _, field := range entity.Fields {
			stored.Fields = append(stored.Fields, storedField{Name: field.Name, Shape: shapeOf(field)})
		}
		for _, invariant := range service.Invariants {
			if invariant.Kind == "unique" && invariant.On == entity.Name {
				stored.Uniques = append(stored.Uniques, storedUnique{Name: invariant.Name, By: strings.Join(invariant.By, ", "), Where: invariant.Where})
			}
		}
		return stored
	}
	for _, entity := range service.Entities {
		entities = append(entities, stored(entity))
	}
	find := func(name string) int {
		return slices.IndexFunc(entities, func(e *storedEntity) bool { return e.Name == name })
	}
	for _, change := range undoOrder(service, from) {
		at := find(change.Entity)
		if change.Kind == "drop" && change.Field == "" {
			// The entity that a drop removes stays declared with dropped_by, as it was (ST-GEN-Q47).
			if at < 0 {
				if dropped := slices.IndexFunc(service.DroppedEntities, func(e types.Entity) bool { return e.Name == change.Entity }); dropped >= 0 {
					entities = append(entities, stored(service.DroppedEntities[dropped]))
				} else {
					entities = append(entities, &storedEntity{Name: change.Entity, Loose: true})
				}
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
				// The loose shape keeps the secret form of the field, so a retype never moves a value between the
				// clear, sealed and hashed forms unseen (security re-review of PR #441, N3).
				now := entity.Fields[field].Shape
				entity.Fields[field].Shape = fieldShape{Type: types.FieldType(change.From), Secret: now.Secret, CompareOnly: now.CompareOnly}
				entity.Fields[field].Loose = true
			}
		case "drop":
			if field >= 0 {
				break
			}
			// The Definition of a drop gives the field in full and its place (ST-DECL-EN21); one that cannot be read
			// gives a loose field of its type, which checking refuses.
			dropped, reason := gen.DropDefinition(change.Definition)
			if reason != "" {
				entity.Fields = append(entity.Fields, storedField{Name: change.Field, Shape: fieldShape{Type: definitionType(change.Definition)}, Loose: true})
				break
			}
			place := 0
			if !dropped.First {
				place = len(entity.Fields)
				if after := entity.field(dropped.After); after >= 0 {
					place = after + 1
				}
			}
			entity.Fields = slices.Insert(entity.Fields, place, storedField{Name: change.Field, Shape: shapeOf(dropped.Field)})
		case "seal":
			// A seal is undone by giving the field its clear form (ST-SUP-RC01-OC49, ST-DECL-INV102).
			if field >= 0 {
				entity.Fields[field].Shape.Secret, entity.Fields[field].Shape.CompareOnly = false, false
			}
		case "unique":
			if change.UniqueOfField(service.Entities) {
				// A unique of a field is undone by taking its unique off the field and its group (ST-GEN-INV33,
				// ST-GEN-Q48).
				if field >= 0 && !entity.Fields[field].Loose {
					group := entity.Fields[field].Shape.UniqueComposite
					for i := range entity.Fields {
						if i == field || (len(group) > 0 && slices.Equal(entity.Fields[i].Shape.UniqueComposite, group)) {
							if i == field {
								entity.Fields[i].Shape.Unique = false
							}
							entity.Fields[i].Shape.UniqueComposite, entity.Fields[i].Shape.UniqueWhenLive = nil, false
						}
					}
				}
				break
			}
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
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: entity.Name, Message: fmt.Sprintf("the head removes the entity %s, which no schema step declares; the release that runs now still reads and writes its table, so keep the entity and its collections as they are, with dropped_by: %s-drop", entity.Name, kebab(entity.Name)),
				Declare: []SuggestedChange{{ID: kebab(entity.Name) + "-drop", Kind: "drop", Entity: entity.Name, Needs: "the entity kept with dropped_by", Phase: "expand"}}})
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
			// A secret field is never renamed, because its sealed values are bound to its column (ST-DECL-INV101): its
			// new name is a drop and an add.
			pair := slices.IndexFunc(added, func(a storedField) bool {
				return !field.Loose && !a.Loose && !field.Shape.Secret && reflect.DeepEqual(a.Shape, field.Shape)
			})
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
		if field.Shape.Secret != now.Shape.Secret || field.Shape.CompareOnly != now.Shape.CompareOnly {
			// The column of a secret holds its sealed bytes or its hash: a move between the clear, sealed and hashed
			// forms rewrites each row. A seal moves a field in clear, with no other change, to its sealed column
			// (ST-SUP-RC01-OC49, ST-DECL-INV102); no schema change moves a field the other way, or to a hash (security
			// review of PR #441, L1).
			clearForm := field.Shape
			clearForm.Secret, clearForm.CompareOnly = true, false
			if !field.Shape.Secret && now.Shape.Secret && !now.Shape.CompareOnly && !field.Loose && !now.Loose && reflect.DeepEqual(clearForm, now.Shape) {
				faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: head.Name, Field: field.Name, Message: fmt.Sprintf("the head stores %s.%s sealed, which the base stores in clear, and no schema step declares it; the release that runs now reads and writes the clear column, so declare a seal step for %s, which adds the sealed column, seals each row and drops the clear column at its contract step", head.Name, field.Name, field.Name),
					Declare: []SuggestedChange{{ID: kebab(head.Name) + "-" + kebab(field.Name) + "-seal", Kind: "seal", Entity: head.Name, Field: field.Name, Needs: "from: the sensitivity of the field in the base, public or pii, or secret when the generator of the base stored the secret field in clear", Phase: "expand"}}})
				continue
			}
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: head.Name, Field: field.Name, Message: fmt.Sprintf("the head changes the secret form of %s.%s (sensitivity secret or compare_only), which changes what its column holds; only a seal moves a field, with no other change, from the clear form to a readable secret (ST-SUP-RC01-OC49), so keep the form that the base stores, or add a new secret field and drop this one", head.Name, field.Name)})
			continue
		}
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
		if made := field.Shape; !field.Loose && !now.Loose && !madeUnique(field.Shape) && madeUnique(now.Shape) {
			// A field of the base that the head makes unique, or puts in a unique_composite, with no other change, is
			// a unique of a field (ST-GEN-Q48).
			made.Unique, made.UniqueComposite, made.UniqueWhenLive = now.Shape.Unique, now.Shape.UniqueComposite, now.Shape.UniqueWhenLive
			if reflect.DeepEqual(made, now.Shape) {
				faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: head.Name, Field: field.Name, Message: fmt.Sprintf("the head makes %s.%s unique, which no schema step declares; the rows of the base can hold one value twice, so declare a unique of the field, whose index migration builds the unique index and stops the migration job while two rows hold one value", head.Name, field.Name),
					Declare: []SuggestedChange{{ID: kebab(head.Name) + "-" + kebab(field.Name) + "-unique", Kind: "unique", Entity: head.Name, Field: field.Name, Phase: "expand"}}})
				continue
			}
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
		if field.Shape.Secret {
			// A fill value of a secret would be clear text in the declaration and raw bytes in its column: a new
			// secret field is optional and comes with an add (ST-DECL-INV101, security re-review of PR #441, N2).
			return SuggestedChange{ID: "add-" + kebab(head.Name) + "-" + kebab(field.Name), Kind: "add", Entity: head.Name, Field: field.Name, Needs: "the field optional, because a secret field takes no fill value", Phase: "expand"}
		}
		if field.Shape.Optional || field.Shape.Default != nil {
			return SuggestedChange{ID: "add-" + kebab(head.Name) + "-" + kebab(field.Name), Kind: "add", Entity: head.Name, Field: field.Name, Phase: "expand"}
		}
		return SuggestedChange{ID: kebab(head.Name) + "-" + kebab(field.Name) + "-require", Kind: "require", Entity: head.Name, Field: field.Name, Needs: "a fill value", Phase: "expand"}
	}
	for i, field := range dropped {
		// The Definition of a drop also gives the place of the field, as the entity is at the step of the drop
		// (ST-DECL-EN21): after the nearest field before it in the base that the head keeps, or first. So the drops
		// of one entity, declared in the order of these faults, each give back their field in its place.
		place := ", first"
		for at := base.field(field.Name) - 1; at >= 0; at-- {
			if head.field(base.Fields[at].Name) >= 0 {
				place = ", after " + base.Fields[at].Name
				break
			}
		}
		declare := []SuggestedChange{{ID: kebab(head.Name) + "-" + kebab(field.Name) + "-drop", Kind: "drop", Entity: head.Name, Field: field.Name, Needs: "the Definition " + definitionOf(field.Shape) + place, Phase: "expand"}}
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

// madeUnique tells whether a stored field is unique or in a unique_composite.
func madeUnique(shape fieldShape) bool { return shape.Unique || len(shape.UniqueComposite) > 0 }

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
	if len(shape.Values) > 0 {
		parts = append(parts, "values "+strings.Join(shape.Values, " "))
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
		kind := kinds[step.Change]
		if kind != "relax" && (kind != "drop" || step.Phase != "expand") {
			continue
		}
		pinned := version
		if pinned == "" {
			pinned = "no runtime library"
		}
		// A drop at expand drops the NOT NULL of the old column and gives it a default in the relaxed form, and the
		// release before reports itself for its contract step (ST-SUP-RC01-OC34), which a runtime from
		// RelaxRuntimeVersion accepts.
		return []BaseFault{{Rule: StepLedgerRule, Entity: changeEntity(head, step.Change), Message: fmt.Sprintf(
			"the base runs %s, whose schema check refuses the column that the %s %s changes in place, so its migration job refuses the database after the step; deploy a release with the runtime library %s or later, with no new schema step, first", pinned, kind, step.Change, RelaxRuntimeVersion)}}
	}
	return nil
}

// UniqueRuntimeVersion is the first runtime library version whose schema check accepts the unique index of a unique
// of a field on columns that its manifest lists (ST-SUP-RP15-LC01, ST-GEN-Q48).
const UniqueRuntimeVersion = "v0.183.0"

// uniqueRuntimeFaults refuses a head with a new unique step of a field when the record at the base pins a runtime
// library before UniqueRuntimeVersion, or none: the schema check of that release refuses the unique index of the
// step, so its pods cannot restart after the step's index migration. The fault names the runtime version to deploy
// first (ST-GEN-INV36, ST-GEN-Q48). A base with no postgres-adapter has no schema check of its tables.
func uniqueRuntimeFaults(applied *AppliedState, base, head *types.ServiceDeclaration) []BaseFault {
	if _, pinned := applied.Components["postgres-adapter"]; !pinned {
		return nil
	}
	version := ""
	if applied.Archetype != nil && applied.Archetype.Runtime != nil {
		version = applied.Archetype.Runtime.Version
	}
	if semver.IsValid(version) && semver.Compare(version, UniqueRuntimeVersion) >= 0 {
		return nil
	}
	for _, step := range head.SchemaHistory[min(len(base.SchemaHistory), len(head.SchemaHistory)):] {
		at := slices.IndexFunc(head.SchemaChanges, func(c types.SchemaChange) bool { return c.ID == step.Change })
		if at < 0 || !head.SchemaChanges[at].UniqueOfField(head.Entities) {
			continue
		}
		pinned := version
		if pinned == "" {
			pinned = "no runtime library"
		}
		return []BaseFault{{Rule: StepLedgerRule, Entity: changeEntity(head, step.Change), Message: fmt.Sprintf(
			"the base runs %s, whose schema check refuses the unique index that the unique %s builds on columns that it has, so its pods cannot restart after the step's index migration; deploy a release with the runtime library %s or later, with no new schema step, first", pinned, step.Change, UniqueRuntimeVersion)}}
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

// MigrationBodyRule is the rule of a fault of a changed body of a released migration (ST-GEN-INV38).
const MigrationBodyRule = "ST-GEN-INV38"

// migrationFile is the path of a generated SQL file of a migration, in the output namespace of its component: the
// group is the name of the file without .sql.
var migrationFile = regexp.MustCompile(`^(?:[^/]+/)*migrations/([^/]+)\.sql$`)

// tenancyRowSecurity is the name of the file of the row-level security of the tenancy component, whose migration the
// ledger names 100_tenancy_row_security (ST-SUP-RC27-OC03).
const tenancyRowSecurity = "row_security"

// ledgeredMigration gives the ledger name of the migration whose generated SQL file is at path, and whether the file
// is that of a ledgered migration: 001_initial or a schema step of the PostgreSQL adapter, or the row-level security
// of the tenancy component. Each such file holds exactly the body whose digest the ledger records.
func ledgeredMigration(path string) (string, bool) {
	match := migrationFile.FindStringSubmatch(path)
	if match == nil {
		return "", false
	}
	name := match[1]
	if name == tenancyRowSecurity {
		name = "100_tenancy_row_security"
	}
	return name, postgresadapter.CheckLedgerName(name) == nil
}

// MigrationBodyFaults refuses a head whose generated SQL file of a ledgered migration that the record at the base
// lists has another content digest than that record lists (ST-GEN-INV38): a database of that release applied the
// body, and the ledger check of its migration job refuses another one (ST-SUP-RC01-OC51). head gives the SHA-256 of
// each generated file of the head by its path in the output folder: the plan of stego plan and stego apply, or the
// record of the head for stego compat. A migration whose file the record at the base does not list is new, and a
// base with no record, or no head, gives no fault. Each fault names the migration, the file and the remedy.
//
// The one other body that the check accepts is a body whose digest at the base the head registers as an earlier
// digest of the same migration (ST-SUP-RC01-OC51): the body that an earlier version of the adapter wrote with the
// text of the seal checkpoints in it, which the ledger check of the head accepts too (walkthrough 19, W19-U1).
// migrate gives the content of each generated migrate.go of the adapter at the head by its path in the output
// folder, beside the migrations folder of the migration; with no such file, the check accepts no earlier digest.
func MigrationBodyFaults(base BaseSource, head map[string]string, migrate map[string][]byte) ([]BaseFault, error) {
	if base.Record == nil || head == nil {
		return nil, nil
	}
	state, err := decodeState(base.Record, ".stego/state.yaml at "+base.Ref)
	if err != nil {
		return nil, err
	}
	if state.LastApplied == nil {
		return nil, nil
	}
	var faults []BaseFault
	earlier := map[string]map[string][]string{}
	for _, path := range slices.Sorted(maps.Keys(state.LastApplied.Files)) {
		name, ledgered := ledgeredMigration(path)
		if !ledgered {
			continue
		}
		want := state.LastApplied.Files[path]
		got, generated := head[path]
		if !generated || got == want {
			continue
		}
		accepted, err := earlierBody(migrate, earlier, path, name, want)
		if err != nil {
			return nil, err
		}
		if accepted {
			continue
		}
		faults = append(faults, BaseFault{Rule: MigrationBodyRule, Message: fmt.Sprintf(
			"the migration %s has another body at the head: its generated file %s has the digest %s, and the record at the base ref %s (commit %s) lists %s. "+
				"A database of that release applied the body, and the body of an applied migration never changes: keep the body, and bring the change in a new schema step",
			name, path, got, base.Ref, base.Commit, want)})
	}
	return faults, nil
}

// earlierBody tells whether the head registers digest as an earlier digest of the migration name, whose generated SQL
// file is at file: the migrate.go of the adapter beside the migrations folder of the file calls
// RegisterEarlierDigests with the name and the digest (postgresadapter.EarlierDigests). read keeps the earlier
// digests of each migrate.go that it read, by its path.
func earlierBody(migrate map[string][]byte, read map[string]map[string][]string, file, name, digest string) (bool, error) {
	source := pathpkg.Join(pathpkg.Dir(pathpkg.Dir(file)), postgresadapter.MigrateFile)
	digests, done := read[source]
	if !done {
		content, there := migrate[source]
		if !there {
			return false, nil
		}
		var err error
		if digests, err = postgresadapter.EarlierDigests(content); err != nil {
			return false, fmt.Errorf("the generated file %s at the head: %w", source, err)
		}
		read[source] = digests
	}
	return slices.Contains(digests[name], digest), nil
}

// placeDifference tells which columns of a table are out of place in the CREATE TABLE of the head, when the base and the
// head have the same columns in another order: each such column with the column that it comes after in the head and in
// the base, and, for a drop of the head whose Definition gives the field another place, the place to give (walkthrough
// 20, W20-P-B). The columns out of place are those that a longest common order of the two leaves out; a column of a
// drop that alone leaves the two orders equal is the one out of place. It gives "" when the columns of the two differ.
func placeDifference(before postgresadapter.BaselineTable, baseKeys, headKeys []string, changes []types.SchemaChange) string {
	columns := func(keys []string) []string {
		var out []string
		for _, key := range keys {
			if name, ok := strings.CutPrefix(key, "column "); ok {
				out = append(out, name)
			}
		}
		return out
	}
	base, head := columns(baseKeys), columns(headKeys)
	if len(base) != len(head) || !slices.Equal(slices.Sorted(slices.Values(base)), slices.Sorted(slices.Values(head))) {
		return ""
	}
	var drops []types.SchemaChange
	for _, change := range changes {
		if change.Entity == before.Entity && change.Kind == "drop" && change.Field != "" {
			drops = append(drops, change)
		}
	}
	without := func(list []string, name string) []string {
		return slices.DeleteFunc(slices.Clone(list), func(n string) bool { return n == name })
	}
	var moved []string
	for _, drop := range drops {
		if slices.Contains(base, drop.Field) && slices.Equal(without(base, drop.Field), without(head, drop.Field)) {
			moved = []string{drop.Field}
			break
		}
	}
	if moved == nil {
		kept := commonOrder(base, head)
		for _, name := range head {
			if !kept[name] {
				moved = append(moved, name)
			}
		}
	}
	if len(moved) == 0 {
		return ""
	}
	previous := func(list []string, name string) string {
		at := slices.Index(list, name)
		if at <= 0 {
			return "first"
		}
		return "after " + list[at-1]
	}
	var text strings.Builder
	for _, name := range moved {
		fmt.Fprintf(&text, "the column %s is out of place: it comes %s in the head, and %s in the base.\n", name, previous(head, name), previous(base, name))
	}
	for _, drop := range drops {
		if !slices.Contains(moved, drop.Field) {
			continue
		}
		want := previous(base, drop.Field)
		dropped, reason := gen.DropDefinition(drop.Definition)
		given := "after " + dropped.After
		if dropped.First {
			given = "first"
		}
		if reason != "" || given == want {
			continue
		}
		fmt.Fprintf(&text, "Cause: the definition %q of the drop %s gives the field %s the place %q. Give it the place of the column in the base: end the definition with %q.\n",
			drop.Definition, drop.ID, drop.Field, given, want)
	}
	return text.String()
}

// commonOrder gives the names of a longest order that base and head share, so that each name it leaves out is one
// that the head puts in another place. A tie keeps the names of the base that come first.
func commonOrder(base, head []string) map[string]bool {
	n, m := len(base), len(head)
	length := make([][]int, n+1)
	for i := range length {
		length[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if base[i] == head[j] {
				length[i][j] = length[i+1][j+1] + 1
			} else {
				length[i][j] = max(length[i+1][j], length[i][j+1])
			}
		}
	}
	kept := map[string]bool{}
	for i, j := 0, 0; i < n && j < m; {
		switch {
		case base[i] == head[j]:
			kept[base[i]] = true
			i, j = i+1, j+1
		case length[i+1][j] >= length[i][j+1]:
			i++
		default:
			j++
		}
	}
	return kept
}

// createDifference tells how the CREATE TABLE of a table in the baseline of the head differs from the one of the base:
// each column or constraint that differs, is missing or is new, with its line in the base and in the head, and the
// schema change of the head that gives the field of the base, which a rename, a retype or a drop takes from its
// definition (walkthrough 14, W14-B3). When the two tables have the same columns in another order, it names each
// column out of place, its place in the base and the place that a drop gives it (placeDifference); else it gives the two
// texts.
func createDifference(before, after postgresadapter.BaselineTable, changes []types.SchemaChange) string {
	type item struct{ key, line string }
	items := func(create string) []item {
		var out []item
		for _, line := range strings.Split(create, "\n") {
			line = strings.TrimSuffix(strings.TrimSpace(line), ",")
			key := ""
			switch {
			case strings.HasPrefix(line, `"`):
				name, _, _ := strings.Cut(line[1:], `"`)
				key = "column " + name
			case strings.HasPrefix(line, `CONSTRAINT "`):
				name, _, _ := strings.Cut(strings.TrimPrefix(line, `CONSTRAINT "`), `"`)
				key = "constraint " + name
			default:
				continue
			}
			out = append(out, item{key, line})
		}
		return out
	}
	baseItems, headItems := items(before.Create), items(after.Create)
	headLine, baseLine := map[string]string{}, map[string]string{}
	for _, it := range headItems {
		headLine[it.key] = it.line
	}
	for _, it := range baseItems {
		baseLine[it.key] = it.line
	}
	var shared []string
	for _, it := range baseItems {
		if _, ok := headLine[it.key]; ok {
			shared = append(shared, it.key)
		}
	}
	var headOrder []string
	for _, it := range headItems {
		if _, ok := baseLine[it.key]; ok {
			headOrder = append(headOrder, it.key)
		}
	}
	whole := "the base creates\n" + before.Create + "and the head creates\n" + after.Create
	if !slices.Equal(shared, headOrder) {
		keys := func(list []item) []string {
			out := make([]string, len(list))
			for i, it := range list {
				out[i] = it.key
			}
			return out
		}
		if text := placeDifference(before, keys(baseItems), keys(headItems), changes); text != "" {
			return text
		}
		return whole
	}
	var text strings.Builder
	fields := map[string]bool{}
	for _, it := range baseItems {
		switch got, ok := headLine[it.key]; {
		case !ok:
			fmt.Fprintf(&text, "the %s\n  %s\nof the base is not in the head.\n", it.key, it.line)
		case got != it.line:
			fmt.Fprintf(&text, "the %s is\n  %s\nin the base and\n  %s\nin the head.\n", it.key, it.line, got)
		default:
			continue
		}
		fields[it.key] = true
	}
	for _, it := range headItems {
		if _, ok := baseLine[it.key]; !ok {
			fmt.Fprintf(&text, "the %s\n  %s\nof the head is not in the base.\n", it.key, it.line)
			fields[it.key] = true
		}
	}
	if text.Len() == 0 {
		return whole
	}
	table := strings.TrimPrefix(before.Table, "public.")
	for _, change := range changes {
		if change.Entity != before.Entity || (change.Kind != "rename" && change.Kind != "retype" && change.Kind != "drop") {
			continue
		}
		name := change.Field
		if change.Kind == "rename" {
			name = change.From
		}
		named := false
		for key := range fields {
			for _, suffix := range []string{"check", "fkey", "key", "not_null", "complete"} {
				named = named || key == "constraint "+table+"_"+name+"_"+suffix
			}
			named = named || key == "column "+name
		}
		if !named {
			continue
		}
		if change.Definition == "" {
			fmt.Fprintf(&text, "Cause: the %s %s gives no definition, so the head takes the field %s before the change with its type alone. Give in definition the field as the base declares it, with each of its settings, such as its default and its limits.\n", change.Kind, change.ID, name)
			continue
		}
		fmt.Fprintf(&text, "Cause: the head takes the field %s before the change from the definition %q of the %s %s. Give in it each setting of the field as the base declares it, such as its default and its limits.\n", name, change.Definition, change.Kind, change.ID)
	}
	return text.String()
}

// RelateFaults gives the faults of ST-GEN-INV33 for the relations of the authorization schema (decision ST-SUP-Q212
// (a)): for each entity of the base with rows, each relation that the head writes for each row and the base does not
// write. The runtime writes the tuple of a relation only for the rows that a release that writes it makes, so the rows
// that the base made, and the rows that it makes during the rollout, get no tuple, and a permission through the
// relation denies them. The base writes a relation as the head does: the relation tenant when its schema gives the
// resource type of the entity the relation tenant of the subject type tenant, the relation of a relationship whose
// subject is a field or the caller, and the relation of a reference to an entity with a resource type. A relation
// whose subject is a field that the base entity does not store has no value on the rows that are there, so it needs
// no relate step. A base that binds no schema writes no relation, so a head that adds the schema gets a fault for each
// relation that it writes for the rows of the base (security review of PR #495, I1).
//
// The fault of a relation of an entity that a tenant owns suggests the relate step at expand, and a relate step of the
// entity and the relation corrects it. No relate step can correct two other faults, so they suggest no schema change
// and tell the author to give the permission another path (security review of PR #495, L1 and L2): a relation whose
// subject is the caller of the create, which no row holds, and a relation of an entity that is shared or system, whose
// rows the relate job does not read. The relation tenant of a shared or system entity is not checked: such a row has
// no tenant.
//
// It gives no fault when the check refused the base record, no release ran, or the head binds no schema. headSchema
// is the schema that the head binds, or nil.
func RelateFaults(check BaseCheck, base BaseSource, head *types.ServiceDeclaration, headSchema *declaration.AuthzSchema) ([]BaseFault, error) {
	if !check.Released || headSchema == nil || base.Declaration == nil || slices.ContainsFunc(check.Faults, func(f BaseFault) bool { return f.Rule == BaseRecordRule }) {
		return nil, nil
	}
	declared, err := parser.ParseServiceDeclarationFromBytes(base.Declaration, "service.yaml at "+base.Ref)
	if err != nil {
		return nil, err
	}
	var baseSchema *declaration.AuthzSchema
	if declared.Authorization != nil && declared.Authorization.SchemaPath != "" {
		schemaPath := declared.Authorization.SchemaPath
		if base.Schema == nil {
			return nil, fmt.Errorf("the base ref %s (commit %s) binds the authorization schema %s, which the commit does not hold in the service folder", base.Ref, base.Commit, schemaPath)
		}
		if len(base.Schema) > declaration.MaxSchemaBytes {
			return nil, fmt.Errorf("the authorization schema %s at the base ref %s (commit %s) is larger than %d bytes", schemaPath, base.Ref, base.Commit, declaration.MaxSchemaBytes)
		}
		if baseSchema, err = declaration.CompileAuthzSchema(schemaPath, base.Schema); err != nil {
			return nil, fmt.Errorf("the authorization schema %s at the base ref %s (commit %s): %w", schemaPath, base.Ref, base.Commit, err)
		}
	}
	var faults []BaseFault
	for _, entity := range head.Entities {
		before, ok := findStoredEntity(declared, entity.Name)
		if !ok || entity.ResourceType == "" {
			continue
		}
		owned := entity.Tenant == "" || entity.Tenant == "owned"
		written := writtenRelations(head, entity, headSchema)
		if !owned {
			delete(written, declaration.TenantRelation)
		}
		callers := callerRelations(entity)
		// baseName gives the name in the base of a field of the head, back through the renames that the base does not
		// declare, so a new relation on a renamed field is found (security review of PR #497, L2).
		baseName := newRenames(declared, head, entity.Name)
		// The relations that the base writes for its rows, each with its source: none when it binds no schema or gives
		// the entity another resource type. The caller relations of the base are a set of their own, which only the
		// check of the caller relations of the head reads (security review of PR #497, L1).
		was := map[string]writtenRelation{}
		var wasCallers []string
		if baseSchema != nil && before.ResourceType == entity.ResourceType {
			was = writtenRelations(declared, before, baseSchema)
			wasCallers = callerRelations(before)
		}
		for _, relation := range slices.Sorted(maps.Keys(written)) {
			source := written[relation]
			// The base writes the relation already when it writes it from the same source: tenant, the same field or
			// the same reference, by its name in the base.
			if prior, ok := was[relation]; slices.Contains(callers, relation) || ok && prior.kind == source.kind && prior.field == baseName(source.field) {
				continue
			}
			if source.field != "" && !slices.ContainsFunc(before.Fields, func(f types.Field) bool { return f.Name == baseName(source.field) }) {
				continue
			}
			if !owned {
				faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: entity.Name, Field: source.field,
					Message: fmt.Sprintf("the head writes the relation %s#%s for each row of %s, and the base does not write it: the rows that the base made get no tuple of it, and a permission through %s denies them. %s is %s, and a relate step reads only the rows that a tenant owns, so no schema change gives them the tuple. Give the permission another path, through a relation that the base writes for the rows of %s", entity.ResourceType, relation, entity.Name, relation, entity.Name, entity.Tenant, entity.Name)})
				continue
			}
			if slices.ContainsFunc(head.SchemaChanges, func(c types.SchemaChange) bool {
				return c.Kind == "relate" && c.Entity == entity.Name && c.Relation == relation
			}) {
				continue
			}
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: entity.Name, Field: source.field,
				Message: fmt.Sprintf("the head writes the relation %s#%s for each row of %s, and the base does not write it: the runtime writes its tuple only for the rows that the head makes, so the rows that the base made, and the rows that it makes during the rollout, get none, and a permission through %s denies them. Declare a schema change of kind relate for %s and the relation %s, with its expand step at the end of the schema history; its relate job gives each row the tuple", entity.ResourceType, relation, entity.Name, relation, entity.Name, relation),
				Declare: []SuggestedChange{{ID: kebab(entity.Name) + "-" + kebab(relation) + "-relate", Kind: "relate", Entity: entity.Name, Needs: "relation: " + relation, Phase: "expand"}}})
		}
		for _, relation := range callers {
			if slices.Contains(wasCallers, relation) {
				continue
			}
			faults = append(faults, BaseFault{Rule: BaseCompatRule, Entity: entity.Name,
				Message: fmt.Sprintf("the head writes the relation %s#%s from the caller of each create of %s, and the base does not write it: the rows that the base made, and the rows that it makes during the rollout, get no tuple of it, and a permission through %s denies them. No row holds the caller of its create, so no relate step gives them the tuple. Give the permission another path, through a relation that the rows of %s get: the relation of a relationship whose subject is a field that the rows hold, of a reference, or tenant, each with its relate step", entity.ResourceType, relation, entity.Name, relation, entity.Name)})
		}
	}
	return faults, nil
}

// callerRelations gives, in their declared order, the relation of each relationship of the entity whose subject is the
// caller of the create, which the runtime writes for each row that a create makes and which no row holds.
func callerRelations(entity types.Entity) []string {
	var relations []string
	for _, r := range entity.Relationships {
		if r.SubjectSource == types.SubjectSourceCaller && r.Relation != "" && !slices.Contains(relations, r.Relation) {
			relations = append(relations, r.Relation)
		}
	}
	return relations
}

// writtenRelation is the source of a relation that the runtime writes for each row: its kind (field, reference or
// tenant) and the field that holds its subject, or empty for tenant.
type writtenRelation struct{ kind, field string }

// writtenRelations gives each relation that the runtime writes for each row of the entity, as the command generator
// writes the tuples of a row, with its source: the relation of a relationship whose subject is a field, the relation
// of a reference to an entity with a resource type, and the relation tenant when the schema gives the resource type of
// the entity the relation tenant of the subject type tenant.
func writtenRelations(service *types.ServiceDeclaration, entity types.Entity, schema *declaration.AuthzSchema) map[string]writtenRelation {
	written := map[string]writtenRelation{}
	for _, r := range entity.Relationships {
		if r.SubjectSource == types.SubjectSourceField && r.Relation != "" {
			written[r.Relation] = writtenRelation{kind: "field", field: r.SubjectField}
		}
	}
	for _, f := range entity.Fields {
		ref, ok := entity.References[f.Name]
		if f.Type != types.FieldTypeRef || !ok || ref.Relation == "" {
			continue
		}
		if to, ok := findStoredEntity(service, f.To); ok && to.ResourceType != "" {
			if _, taken := written[ref.Relation]; !taken {
				written[ref.Relation] = writtenRelation{kind: "reference", field: f.Name}
			}
		}
	}
	if _, taken := written["tenant"]; !taken && schema.RelationAdmits(entity.ResourceType, "tenant", "tenant") {
		written["tenant"] = writtenRelation{kind: "tenant"}
	}
	return written
}

// newRenames gives the function that maps the name of a field of the entity in the head to its name in the base: back
// through each rename of the entity that the head declares and the base does not, the last in the schema history
// first, as collectionIndexes undoes renames. A rename with no step yet comes after each one with a step. Empty stays
// empty.
func newRenames(base, head *types.ServiceDeclaration, entity string) func(string) string {
	known := map[string]bool{}
	for _, change := range base.SchemaChanges {
		known[change.ID] = true
	}
	position := map[string]int{}
	for i, step := range head.SchemaHistory {
		if _, seen := position[step.Change]; !seen {
			position[step.Change] = i
		}
	}
	at := func(change types.SchemaChange) int {
		if i, ok := position[change.ID]; ok {
			return i
		}
		return len(head.SchemaHistory)
	}
	var renames []types.SchemaChange
	for _, change := range head.SchemaChanges {
		if change.Kind == "rename" && change.Entity == entity && !known[change.ID] {
			renames = append(renames, change)
		}
	}
	slices.SortStableFunc(renames, func(a, b types.SchemaChange) int { return at(a) - at(b) })
	return func(field string) string {
		for i := len(renames) - 1; i >= 0 && field != ""; i-- {
			if renames[i].Field == field {
				field = renames[i].From
			}
		}
		return field
	}
}
