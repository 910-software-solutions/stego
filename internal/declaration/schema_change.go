package declaration

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

func schemaChangePath(i int) string { return fmt.Sprintf("schema_changes[%d]", i) }
func schemaStepPath(i int) string   { return fmt.Sprintf("schema_history[%d]", i) }

// schemaChangeKinds are the kinds of schema change, in the order the specification gives them.
var schemaChangeKinds = []string{"add", "rename", "retype", "require", "drop", "unique", "relax", "within"}

// backfillKinds are the kinds of schema change whose expand step starts a backfill, which runs only in controller
// mode.
var backfillKinds = map[string]bool{"rename": true, "retype": true, "require": true, "unique": true}

// maxOpenSchemaChanges is the most schema changes that may be open at one time, because the backfill observations
// carry the change as an enum (ST-SUP-RP12-LC23).
const maxOpenSchemaChanges = 127

// schemaValue says whether a kind of schema change takes a value, and whether it needs it.
type schemaValue int

const (
	valueRefused schemaValue = iota
	valueOptional
	valueRequired
)

// schemaValues gives, for each value of a schema change, what each kind takes (ST-DECL-INV57). Field is optional
// for add and drop, which may change a whole entity. A relax names its field and gives the field before the change
// as its Definition (decision on finding F12, owner, 2026-11-13).
var schemaValues = map[string]map[string]schemaValue{
	"field":              {"add": valueOptional, "rename": valueRequired, "retype": valueRequired, "require": valueRequired, "drop": valueOptional, "unique": valueRequired, "relax": valueRequired, "within": valueRequired},
	"from":               {"rename": valueRequired, "retype": valueRequired},
	"conversion":         {"retype": valueRequired},
	"inverse_conversion": {"retype": valueRequired},
	"fill_value":         {"require": valueRequired, "retype": valueOptional, "drop": valueOptional},
	"definition":         {"drop": valueRequired, "retype": valueOptional, "relax": valueRequired},
}

func schemaValueOf(change types.SchemaChange, value string) bool {
	switch value {
	case "field":
		return change.Field != ""
	case "from":
		return change.From != ""
	case "conversion":
		return change.Conversion != ""
	case "inverse_conversion":
		return change.InverseConversion != ""
	case "fill_value":
		return change.FillValue != nil
	case "definition":
		return change.Definition != ""
	}
	return false
}

// validateSchemaChangeKind checks that each schema change has an ID in lower kebab case that is unique in the
// declaration, a known kind, and only the values its kind takes, with each value it needs (ST-DECL-INV57).
func validateSchemaChangeKind(changes []types.SchemaChange) []ValidationError {
	var faults []ValidationError
	seen := map[string]bool{}
	for i, change := range changes {
		path := schemaChangePath(i)
		switch {
		case !kebabName.MatchString(change.ID):
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-kind", Path: path + ".id", Found: change.ID, Message: fmt.Sprintf("the schema change ID %q must match %s: lower kebab case", change.ID, kebabName)})
		case seen[change.ID]:
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-kind", Path: path + ".id", Found: change.ID, Message: fmt.Sprintf("the schema change ID %q is used twice; no change can use the ID of another", change.ID)})
		}
		seen[change.ID] = true
		if !contains(schemaChangeKinds, change.Kind) {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-kind", Path: path + ".kind", Found: change.Kind, Allowed: schemaChangeKinds, Message: fmt.Sprintf("schema change %q has the kind %q; its kind must be add, rename, retype, require, drop, unique or relax", change.ID, change.Kind)})
			continue
		}
		for _, value := range []string{"field", "from", "conversion", "inverse_conversion", "fill_value", "definition"} {
			takes, has := schemaValues[value][change.Kind], schemaValueOf(change, value)
			if change.Kind == "drop" && change.Field == "" && (value == "definition" || value == "fill_value") {
				// A drop of a whole entity takes no Definition and no fill value: the entity stays declared with
				// dropped_by (ST-DECL-INV57, ST-GEN-Q47).
				if has {
					faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-kind", Path: path + "." + value, Found: schemaValueText(change, value), Message: fmt.Sprintf("schema change %q drops the whole entity %s, which takes no %s: the entity stays declared with dropped_by: %s; remove the %s", change.ID, change.Entity, value, change.ID, value)})
				}
				continue
			}
			switch {
			case takes == valueRequired && !has:
				faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-kind", Path: path + "." + value, Message: fmt.Sprintf("schema change %q of kind %s has no %s; a %s needs it", change.ID, change.Kind, value, change.Kind)})
			case takes == valueRefused && has:
				faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-kind", Path: path + "." + value, Found: schemaValueText(change, value), Message: fmt.Sprintf("schema change %q of kind %s has a %s, which a %s does not take; remove it", change.ID, change.Kind, value, change.Kind)})
			}
		}
	}
	return faults
}

func schemaValueText(change types.SchemaChange, value string) string {
	switch value {
	case "field":
		return change.Field
	case "from":
		return change.From
	case "conversion":
		return change.Conversion
	case "inverse_conversion":
		return change.InverseConversion
	case "fill_value":
		return fmt.Sprint(change.FillValue)
	}
	return change.Definition
}

func findEntity(entities []types.Entity, name string) (types.Entity, bool) {
	for _, entity := range entities {
		if entity.Name == name {
			return entity, true
		}
	}
	return types.Entity{}, false
}

func findField(entity types.Entity, name string) (types.Field, bool) {
	for _, field := range entity.Fields {
		if field.Name == name {
			return field, true
		}
	}
	return types.Field{}, false
}

// firstSteps gives the position in the schema history of the first step of each schema change.
func firstSteps(history []types.SchemaStep) map[string]int {
	first := map[string]int{}
	for i, step := range history {
		if _, seen := first[step.Change]; !seen {
			first[step.Change] = i
		}
	}
	return first
}

// fieldShape gives the type and the constraints of a field as text, for a fault that names the shape of a field at
// a step.
func fieldShape(field types.Field) string {
	items := []string{string(field.Type)}
	if field.MinLength != nil {
		items = append(items, fmt.Sprintf("min_length %d", *field.MinLength))
	}
	if field.MaxLength != nil {
		items = append(items, fmt.Sprintf("max_length %d", *field.MaxLength))
	}
	if field.Min != nil {
		items = append(items, fmt.Sprintf("min %v", *field.Min))
	}
	if field.Max != nil {
		items = append(items, fmt.Sprintf("max %v", *field.Max))
	}
	if len(field.Values) > 0 {
		items = append(items, "values "+strings.Join(field.Values, " "))
	}
	if field.Optional {
		items = append(items, "optional")
	}
	if field.Default != nil {
		items = append(items, fmt.Sprintf("default %v", field.Default))
	}
	if field.Pattern != "" {
		items = append(items, "pattern "+field.Pattern)
	}
	return strings.Join(items, ", ")
}

// stepShape is the shape of an entity as the step of one schema change leaves it.
type stepShape struct {
	// entity is the entity after the step; declared is false when the entity does not exist after it.
	entity   types.Entity
	declared bool
	// later is true when a schema change on the entity has its step after the step of the change, so that the
	// shape after the step is not the shape that the declaration declares.
	later bool
	// step is the path of the first step of the change, empty when it has no step.
	step string
	// reason is why a later schema change cannot be undone, and so why the shape is not known; empty when it is.
	reason string
}

// field gives the field of the entity after the step, by its name at that step.
func (s stepShape) field(name string) (types.Field, bool) {
	if !s.declared {
		return types.Field{}, false
	}
	return findField(s.entity, name)
}

// of names, for a fault, the shape of the field after the step when a later step changes the entity, or gives
// empty.
func (s stepShape) of(entity string, field types.Field) string {
	if !s.later {
		return ""
	}
	return fmt.Sprintf(" (the field after the step %s is %s.%s: %s, with each later step undone)", s.step, entity, field.Name, fieldShape(field))
}

// shapeAtStep gives the entity of a schema change as the change's own step leaves it: the entity that the
// declaration declares, with each schema change on the entity whose first step comes after the first step of the
// change undone, the last first, as gen.Context.AtSchemaStep undoes them (decision on ST-DECL-Q17 (a), orchestrator
// under the owner's delegation, 2026-11-14). So each change is checked against the shape of its field at its own
// step, and a chain such as a retype from A to B and then a retype from B to C is checked step by step. An add or a
// require is undone by removing its field, or its entity; a rename by giving the field the name From; a retype by
// giving the field its type and constraints before the change (gen.RetypeField); a relax by giving the field what it
// was before the change (types.SchemaChange.RelaxedFrom); and a drop of a field by giving the entity the field of
// its Definition. A unique changes no field. A change with no step is checked against the declared shape, and is
// not undone. When a later change cannot be undone, the reason names it, its step and the shape of the field after
// that step; that later change is also refused by its own check.
func shapeAtStep(service *types.ServiceDeclaration, change types.SchemaChange) stepShape {
	entity, declared := findEntity(service.Entities, change.Entity)
	if !declared && !(change.Kind == "drop" && change.Field == "") {
		// An entity that a drop removes stays declared with dropped_by, as it was before the drop (ST-GEN-Q47).
		entity, declared = findEntity(service.DroppedEntities, change.Entity)
	}
	shape := stepShape{declared: declared}
	if declared {
		entity.Fields = append([]types.Field(nil), entity.Fields...)
		shape.entity = entity
	}
	first := firstSteps(service.SchemaHistory)
	at, stepped := first[change.ID]
	if !stepped {
		return shape
	}
	shape.step = schemaStepPath(at)
	var later []types.SchemaChange
	for _, other := range service.SchemaChanges {
		if position, ok := first[other.ID]; ok && other.Entity == change.Entity && position > at && other.Kind != "unique" && other.Kind != "within" {
			later = append(later, other)
		}
	}
	slices.SortFunc(later, func(a, b types.SchemaChange) int { return first[b.ID] - first[a.ID] })
	for _, other := range later {
		shape.later = true
		if !shape.declared {
			// An entity that a later step drops whole is not built again from its Definition here, and a change on an
			// entity that the shape does not have is refused by its own check.
			return shape
		}
		fields := shape.entity.Fields
		found := slices.IndexFunc(fields, func(f types.Field) bool { return f.Name == other.Field })
		cannot := func(message string) stepShape {
			shape.reason = fmt.Sprintf("the later %s %q at %s cannot be undone: %s", other.Kind, other.ID, schemaStepPath(first[other.ID]), message)
			return shape
		}
		// A rename, a retype and a relax need their field: one with no field cannot be undone, and its own check
		// (ST-DECL-INV57) refuses it (security review of PR #372, M1). The index of a field is never read when the
		// shape has no such field.
		if other.Field == "" && (other.Kind == "rename" || other.Kind == "retype" || other.Kind == "relax") {
			return cannot("it names no field")
		}
		if found < 0 && other.Field != "" && other.Kind != "drop" {
			return cannot(fmt.Sprintf("the entity %s after that step has no field %s", other.Entity, other.Field))
		}
		switch other.Kind {
		case "add", "require":
			if other.Field == "" {
				shape.declared = false
				continue
			}
			shape.entity.Fields = slices.Delete(fields, found, found+1)
		case "rename":
			if _, taken := findField(shape.entity, other.From); taken {
				return cannot(fmt.Sprintf("the entity %s after that step still has the field %s, the name before the rename", other.Entity, other.From))
			}
			fields[found].Name = other.From
		case "retype":
			before := gen.RetypeField(other)
			before.Name, before.Optional, before.Sensitivity = fields[found].Name, fields[found].Optional, fields[found].Sensitivity
			if other.Definition == "" {
				before.Default = nil
			}
			fields[found] = before
		case "relax":
			before, reason := other.RelaxedFrom(fields[found])
			if reason != "" {
				return cannot(fmt.Sprintf("it does not relax the field %s.%s after that step, %s: %s", other.Entity, other.Field, fieldShape(fields[found]), reason))
			}
			fields[found] = before
		case "drop":
			// A drop is undone as gen.Context.AtSchemaStep undoes it: a drop of an entity by the entity that the
			// declaration keeps with dropped_by, which the shape is already, and a drop of a field by giving the
			// entity back the field of its Definition in its place (security review of PR #372, L2).
			if other.Field == "" {
				continue
			}
			if found >= 0 {
				return cannot(fmt.Sprintf("the entity %s after that step still has the field %s", other.Entity, other.Field))
			}
			dropped, reason := gen.DropDefinition(other.Definition)
			if reason != "" {
				return cannot(fmt.Sprintf("its definition %q cannot be read: %s", other.Definition, reason))
			}
			dropped.Field.Name = other.Field
			place := 0
			if !dropped.First {
				after := slices.IndexFunc(fields, func(f types.Field) bool { return f.Name == dropped.After })
				if after < 0 {
					return cannot(fmt.Sprintf("its definition puts the field after %s, which the entity %s after that step does not have", dropped.After, other.Entity))
				}
				place = after + 1
			}
			shape.entity.Fields = slices.Insert(fields, place, dropped.Field)
		default:
			return cannot("its kind is not one that checking knows")
		}
	}
	return shape
}

// FieldAtStep gives the field of a schema change as the change's own step leaves it (shapeAtStep): the declared
// field with each later schema change on its entity undone. It tells false when the field does not exist after the
// step, and gives the reason when a later change cannot be undone.
func FieldAtStep(service *types.ServiceDeclaration, change types.SchemaChange) (types.Field, bool, string) {
	shape := shapeAtStep(service, change)
	if shape.reason != "" {
		return types.Field{}, false, shape.reason
	}
	field, found := shape.field(change.Field)
	return field, found, ""
}

// validateSchemaChangeEntity checks that the entity and the field of each schema change agree with the declaration
// (ST-DECL-INV58), as the change's own step leaves the entity (shapeAtStep, decision on ST-DECL-Q17 (a)): for add,
// rename, retype and require the entity declares the field, for rename it does not declare From, for retype From is
// not the type of the field, for drop it does not declare the field, a drop with no field names an entity that the
// declaration does not declare, and for unique the field names a declared invariant of kind unique on the entity.
// A change whose shape is not known, because a later change cannot be undone, is not checked here: the check of the
// later change refuses it.
func validateSchemaChangeEntity(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	for i, change := range service.SchemaChanges {
		path := schemaChangePath(i)
		shape := shapeAtStep(service, change)
		after := ""
		if shape.later {
			after = fmt.Sprintf(" after its step %s, with each later step undone", shape.step)
			if field, found := shape.field(change.Field); found {
				after = fmt.Sprintf(" after its step %s (%s.%s: %s, with each later step undone)", shape.step, change.Entity, field.Name, fieldShape(field))
			}
		}
		fault := func(where, message string) {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-entity", Path: path + where, Found: change.Entity + "." + change.Field, Message: fmt.Sprintf("schema change %q on the entity %q and the field %q%s: %s", change.ID, change.Entity, change.Field, after, message)})
		}
		if shape.reason != "" {
			continue
		}
		entity, declared := shape.entity, shape.declared
		if change.Kind == "drop" && change.Field == "" {
			// The entity that a drop removes stays declared, with dropped_by naming the drop (ST-DECL-INV58,
			// ST-GEN-Q47).
			dropped, kept := findEntity(service.DroppedEntities, change.Entity)
			switch {
			case declared:
				fault(".entity", fmt.Sprintf("the declaration still declares the entity that the change drops as a live entity; keep it as it is and add dropped_by: %s to it", change.ID))
			case !kept:
				fault(".entity", fmt.Sprintf("the declaration declares no entity %s with dropped_by: %s; the entity that a drop removes stays declared as it was, with dropped_by naming the drop", change.Entity, change.ID))
			case dropped.DroppedBy != change.ID:
				fault(".entity", fmt.Sprintf("the entity %s names the drop %q in dropped_by, not this drop", change.Entity, dropped.DroppedBy))
			}
			continue
		}
		if !declared {
			if contains(schemaChangeKinds, change.Kind) {
				fault(".entity", "the declaration declares no such entity")
			}
			continue
		}
		_, hasField := findField(entity, change.Field)
		switch change.Kind {
		case "add":
			if change.Field != "" && !hasField {
				fault(".field", "the entity does not declare the field that the change adds")
			}
		case "rename", "retype", "require", "relax":
			if !hasField {
				fault(".field", fmt.Sprintf("the entity does not declare the field, which a %s names by its name after the change", change.Kind))
			}
			if field, _ := findField(entity, change.Field); change.Kind == "retype" && hasField && change.From == string(field.Type) {
				fault(".from", fmt.Sprintf("the retype is from %s, the type that the field has now; a retype changes the type, so name the type before the change", change.From))
			}
			if _, hasFrom := findField(entity, change.From); change.Kind == "rename" && change.From != "" && hasFrom {
				fault(".from", fmt.Sprintf("the entity still declares %q, the name before the rename; remove it", change.From))
			}
		case "drop":
			if hasField {
				fault(".field", "the entity still declares the field that the change drops; remove the field")
			}
		case "within":
			// The reference of a within holds its key within a field (ST-DECL-INV58, ST-DECL-Q42).
			switch field, _ := findField(entity, change.Field); {
			case !hasField || field.Type != types.FieldTypeRef:
				fault(".field", "the entity does not declare the reference whose key a within adds")
			case entity.References[change.Field].WithinField() == "":
				fault(".field", "the reference declares no within or within: none; a within change adds the key of a reference that declares within a field")
			}
		case "unique":
			found := false
			for _, invariant := range service.Invariants {
				if invariant.Name == change.Field && invariant.Kind == "unique" && invariant.On == change.Entity {
					found = true
				}
			}
			if !found {
				fault(".field", "the field names no declared invariant of kind unique on the entity")
			}
		}
	}
	return faults
}

// validateDroppedEntities checks that each entity with dropped_by names a schema change of kind drop with no field
// on that entity, and that the drop has a step: the entity stays declared only to give back the stored shape before
// its drop (ST-DECL-INV58, ST-GEN-Q47). Every other part of the declaration names only live entities, which the
// checks of each part hold, as the declaration keeps a dropped entity apart from its entities.
func validateDroppedEntities(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	first := firstSteps(service.SchemaHistory)
	for _, entity := range service.DroppedEntities {
		at := slices.IndexFunc(service.SchemaChanges, func(c types.SchemaChange) bool { return c.ID == entity.DroppedBy })
		fault := func(message string) {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-entity", Path: "entities." + entity.Name + ".dropped_by", Found: entity.DroppedBy, Message: fmt.Sprintf("the entity %s is dropped by %q: %s", entity.Name, entity.DroppedBy, message)})
		}
		switch {
		case at < 0:
			fault("schema_changes declares no such change; declare a drop with no field of the entity, or remove dropped_by")
		case service.SchemaChanges[at].Kind != "drop" || service.SchemaChanges[at].Field != "" || service.SchemaChanges[at].Entity != entity.Name:
			fault(fmt.Sprintf("the change is not a drop with no field of %s; dropped_by names the drop that removes the whole entity", entity.Name))
		default:
			if _, stepped := first[entity.DroppedBy]; !stepped {
				fault("the drop has no step in schema_history; give it its expand step")
			}
		}
	}
	return faults
}

// schemaPhases gives the phases of each schema change in the order of the schema history.
func schemaPhases(steps []types.SchemaStep) map[string][]string {
	phases := map[string][]string{}
	for _, step := range steps {
		phases[step.Change] = append(phases[step.Change], step.Phase)
	}
	return phases
}

// openSchemaChanges gives the schema changes whose last step is expand, in the order of the declaration. A relax
// finishes at its expand step, in place, so it is never open (decision on finding F12, owner, 2026-11-13).
func openSchemaChanges(service *types.ServiceDeclaration) []types.SchemaChange {
	phases := schemaPhases(service.SchemaHistory)
	var open []types.SchemaChange
	for _, change := range service.SchemaChanges {
		if change.Kind == "relax" || change.Kind == "within" {
			continue
		}
		if steps := phases[change.ID]; len(steps) > 0 && steps[len(steps)-1] == "expand" {
			open = append(open, change)
		}
	}
	return open
}

// validateSchemaSteps checks that the schema history moves each change in order (ST-DECL-INV59): each step names a
// declared change, each change has exactly one expand step and at most one contract step after it, a change of kind
// add has no contract step, and no change has no step.
func validateSchemaSteps(service *types.ServiceDeclaration) []ValidationError {
	declared := map[string]string{}
	for _, change := range service.SchemaChanges {
		declared[change.ID] = change.Kind
	}
	var faults []ValidationError
	for i, step := range service.SchemaHistory {
		if _, known := declared[step.Change]; !known {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-step", Path: schemaStepPath(i) + ".change", Found: step.Change, Message: fmt.Sprintf("the schema step for %q names no declared schema change", step.Change)})
		}
		if step.Phase != "expand" && step.Phase != "contract" {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-step", Path: schemaStepPath(i) + ".phase", Found: step.Phase, Allowed: []string{"expand", "contract"}, Message: fmt.Sprintf("the schema step for %q has the phase %q; a phase is expand or contract", step.Change, step.Phase)})
		}
	}
	phases := schemaPhases(service.SchemaHistory)
	for i, change := range service.SchemaChanges {
		path := schemaChangePath(i)
		fault := func(message string) {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-step", Path: path, Found: strings.Join(phases[change.ID], ", "), Message: fmt.Sprintf("schema change %q has the steps %q: %s", change.ID, strings.Join(phases[change.ID], ", "), message)})
		}
		steps := phases[change.ID]
		switch {
		case len(steps) == 0:
			fault("it has no step; put its expand step at the end of the schema history")
		case steps[0] != "expand":
			fault("its first step must be expand")
		case len(steps) > 2 || (len(steps) == 2 && steps[1] != "contract"):
			fault("it has exactly one expand step and at most one contract step after it")
		case len(steps) == 2 && change.Kind == "add":
			fault("a change of kind add has an expand step only")
		case len(steps) == 2 && change.Kind == "relax":
			fault("a change of kind relax has an expand step only, which finishes it in place")
		case len(steps) == 2 && change.Kind == "within":
			fault("a change of kind within has an expand step only, which adds and validates the key")
		}
	}
	return faults
}

// openOnField gives the open schema changes that hold a field for ST-DECL-INV60: an add, as a relax, finishes at
// its expand step, as it has no contract step (decision on ST-DECL-Q17 (a), orchestrator under the owner's
// delegation, 2026-11-14), so a later change of the field that an add added is not refused for ever.
func openOnField(service *types.ServiceDeclaration) []types.SchemaChange {
	var open []types.SchemaChange
	for _, change := range openSchemaChanges(service) {
		if change.Kind != "add" {
			open = append(open, change)
		}
	}
	return open
}

// validateSchemaChangeOpen checks that at most one open schema change names a given field of an entity, as Field or
// as From, and that no relax has its step after the expand step of an open change that names its field: the two
// columns of that change hold the field until its contract step (ST-DECL-INV60). An add is not open here
// (openOnField).
func validateSchemaChangeOpen(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	faults = append(faults, relaxOpenFaults(service)...)
	owner := map[[2]string]string{}
	for _, change := range openOnField(service) {
		for _, field := range []string{change.Field, change.From} {
			if field == "" || (change.Kind == "retype" && field == change.From) {
				continue
			}
			key := [2]string{change.Entity, field}
			if first, taken := owner[key]; taken && first != change.ID {
				faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-open", Path: "schema_changes", Found: field, Message: fmt.Sprintf("the schema changes %q and %q are both open on the field %s.%s; finish %q with its contract step before %q moves the same column", first, change.ID, change.Entity, field, first, change.ID)})
				continue
			}
			owner[key] = change.ID
		}
	}
	return faults
}

// validateSchemaChangeMode checks that a declaration with an open schema change whose expand step starts a backfill
// runs the controller mode (ST-DECL-INV61). Checking does not add the mode.
func validateSchemaChangeMode(service *types.ServiceDeclaration) []ValidationError {
	if RunsMode(service, "controller") {
		return nil
	}
	var faults []ValidationError
	for _, change := range openSchemaChanges(service) {
		if backfillKinds[change.Kind] {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-mode", Path: "modes", Found: strings.Join(service.Modes, ", "), Allowed: []string{"controller"}, Message: fmt.Sprintf("schema change %q is at expand and its backfill runs only in the mode controller, which the modes %s do not hold; add controller to modes, or leave modes out to run all", change.ID, strings.Join(service.Modes, ", "))})
		}
	}
	return faults
}

// ValueSuits tells whether a value suits the type and the constraints of a field, as valueSuits does.
func ValueSuits(field types.Field, value any) (bool, string) { return valueSuits(field, value) }

// definitionField reads the Definition of a dropped or a retyped field (gen.DefinitionField).
// definitionDefaultFaults refuses the default of a Definition that does not suit the field that the Definition gives:
// its type, its min_length, max_length and pattern, and its min and max (ST-DECL-INV62). Generation writes the default
// into the DEFAULT clause of the column before the change, so a default such as abc of an int32 field gives DDL that
// PostgreSQL refuses (security review of PR #408).
func definitionDefaultFaults(change types.SchemaChange, field types.Field, fault func(path, found, message string)) {
	if field.Default == nil {
		return
	}
	if ok, reason := valueSuits(field, field.Default); !ok {
		fault(".definition", change.Definition, fmt.Sprintf("the default %v of the definition %q does not suit the field it gives: %s", field.Default, change.Definition, reason))
	}
}

func definitionField(definition string) (types.Field, string) { return gen.DefinitionField(definition) }

// zeroValue gives the zero value of a field type that has one: the empty string, 0 or false (ST-DECL-INV62).
func zeroValue(typ types.FieldType) (any, bool) {
	switch typ {
	case types.FieldTypeString, types.FieldTypeEnum:
		return "", true
	case types.FieldTypeInt32, types.FieldTypeInt64, types.FieldTypeFloat, types.FieldTypeDouble:
		return 0, true
	case types.FieldTypeBool:
		return false, true
	}
	return nil, false
}

// valueSuits tells whether a value suits the type and the constraints of a field, and gives the reason when it
// does not.
func valueSuits(field types.Field, value any) (bool, string) {
	probe := field
	probe.Default = value
	if !defaultSuits(probe) {
		return false, fmt.Sprintf("it does not suit the type %s", field.Type)
	}
	if text, ok := value.(string); ok {
		length := utf8.RuneCountInString(text)
		if field.MinLength != nil && length < *field.MinLength {
			return false, fmt.Sprintf("it is shorter than the min_length %d", *field.MinLength)
		}
		if field.MaxLength != nil && length > *field.MaxLength {
			return false, fmt.Sprintf("it is longer than the max_length %d", *field.MaxLength)
		}
		if field.Pattern != "" {
			if pattern, err := regexp.Compile(field.Pattern); err == nil && !pattern.MatchString(text) {
				return false, fmt.Sprintf("it does not match the pattern %s", field.Pattern)
			}
		}
	}
	var number float64
	switch n := value.(type) {
	case int:
		number = float64(n)
	case int64:
		number = float64(n)
	case uint64:
		number = float64(n)
	case float64:
		number = n
	default:
		return true, ""
	}
	if field.Min != nil && number < *field.Min {
		return false, fmt.Sprintf("it is less than the min %v", *field.Min)
	}
	if field.Max != nil && number > *field.Max {
		return false, fmt.Sprintf("it is more than the max %v", *field.Max)
	}
	return true, ""
}

// validateSchemaChangeValues checks that the values of each schema change suit its field (ST-DECL-INV62), as the
// change's own step leaves the field (shapeAtStep, decision on ST-DECL-Q17 (a)): the field of a require is required
// with no default, a fill value suits the type and the constraints of the field, or for a drop those of its
// Definition, and the Definition of a relax relaxes the field in place. A change whose shape is not known, because
// a later change cannot be undone, is not checked here: the check of the later change refuses it. Checking holds the conversions of a retype against the database component in
// ValidateSchemaConversions.
func validateSchemaChangeValues(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	for i, change := range service.SchemaChanges {
		path := schemaChangePath(i)
		fault := func(where, found, message string) {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-value", Path: path + where, Found: found, Message: fmt.Sprintf("schema change %q: %s", change.ID, message)})
		}
		var field types.Field
		var at string
		switch change.Kind {
		case "relax":
			if change.Definition == "" {
				continue
			}
			shape := shapeAtStep(service, change)
			after, found := shape.field(change.Field)
			if shape.reason != "" || !found {
				continue
			}
			if _, reason := change.RelaxedFrom(after); reason != "" {
				fault(".definition", change.Definition, fmt.Sprintf("the relax of %s.%s cannot be carried out in place: %s%s", change.Entity, change.Field, reason, shape.of(change.Entity, after)))
			}
			continue
		case "require", "retype":
			shape := shapeAtStep(service, change)
			found := false
			if shape.reason == "" {
				field, found = shape.field(change.Field)
			}
			if !found {
				continue
			}
			at = shape.of(change.Entity, field)
			if change.Kind == "retype" && change.Definition != "" {
				// The Definition of a retype is the field before the change, whose type is From (decision on
				// ST-GEN-Q07 (a), orchestrator under the owner's delegation, 2026-11-13).
				old, reason := definitionField(change.Definition)
				switch {
				case reason != "":
					fault(".definition", change.Definition, fmt.Sprintf("the definition %q cannot be read: %s", change.Definition, reason))
				case string(old.Type) != change.From:
					fault(".definition", change.Definition, fmt.Sprintf("the definition %q is of the type %s, and the retype is from %s; the Definition gives the field before the change", change.Definition, old.Type, change.From))
				default:
					definitionDefaultFaults(change, old, fault)
				}
			}
			if change.Kind == "require" && (field.Optional || field.Default != nil) {
				fault(".field", change.Field, fmt.Sprintf("the field %s.%s of a require must be required with no default; a field that is optional or has a default is an add%s", change.Entity, change.Field, at))
			}
		case "drop":
			if change.Field == "" || change.Definition == "" {
				continue
			}
			dropped, reason := gen.DropDefinition(change.Definition)
			if reason != "" {
				fault(".definition", change.Definition, fmt.Sprintf("the definition %q cannot be read: %s", change.Definition, reason))
				continue
			}
			field = dropped.Field
			field.Name = change.Field
			definitionDefaultFaults(change, field, fault)
			if dropped.After != "" {
				// The place of the field is a field of the entity as the step of the drop leaves it (ST-DECL-INV62).
				shape := shapeAtStep(service, change)
				if _, found := shape.field(dropped.After); shape.reason == "" && shape.declared && !found {
					fault(".definition", change.Definition, fmt.Sprintf("the definition puts the field after %s, which the entity %s does not have at the step of the drop; name the field that %s came after, or first", dropped.After, change.Entity, change.Field))
				}
			}
			if change.FillValue == nil && !field.Optional && field.Default == nil {
				// The old column of a required field takes the zero value of its type as its DEFAULT at expand, so
				// the zero value must suit the field (ST-DECL-INV62, ST-SUP-RC01-OC34).
				zero, ok := zeroValue(field.Type)
				if !ok {
					fault(".fill_value", "", fmt.Sprintf("the required field %s.%s of the type %s has no zero value, which its old column would take as its default; give the drop a fill_value", change.Entity, change.Field, field.Type))
				} else if suits, why := valueSuits(field, zero); !suits {
					fault(".fill_value", "", fmt.Sprintf("the zero value %q of the required field %s.%s does not suit its definition: %s; its old column takes the default at expand, so give the drop a fill_value that suits", fmt.Sprint(zero), change.Entity, change.Field, why))
				}
			}
		default:
			continue
		}
		if change.FillValue == nil {
			continue
		}
		if ok, reason := valueSuits(field, change.FillValue); !ok {
			fault(".fill_value", fmt.Sprint(change.FillValue), fmt.Sprintf("the fill value %v does not suit the field %s: %s%s", change.FillValue, change.Field, reason, at))
		}
	}
	return faults
}

// relaxOpenFaults refuses a relax or an add whose step comes after the expand step of an open schema change that
// names the same field, as Field or, for a rename, as From (ST-DECL-INV60): the two columns of that change hold the field until its
// contract step. An add is never open itself (openOnField), but an add of the field that an open rename renames
// from, or that an open retype retypes, meets the old column, which still exists (security review of PR #372, L1).
func relaxOpenFaults(service *types.ServiceDeclaration) []ValidationError {
	first := firstSteps(service.SchemaHistory)
	open := openOnField(service)
	var faults []ValidationError
	for _, change := range service.SchemaChanges {
		at, stepped := first[change.ID]
		if (change.Kind != "relax" && change.Kind != "add") || change.Field == "" || !stepped {
			continue
		}
		for _, other := range open {
			if other.Entity != change.Entity || (other.Field != change.Field && (other.Kind == "retype" || other.From != change.Field)) || first[other.ID] > at {
				continue
			}
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-open", Path: "schema_changes", Found: change.Field, Message: fmt.Sprintf("the %s %q of %s.%s comes after the expand step of %q, which is open on the same field; finish %q with its contract step first", change.Kind, change.ID, change.Entity, change.Field, other.ID, other.ID)})
		}
	}
	return faults
}

// validateSchemaChangeCount checks that at most 127 schema changes are open (ST-DECL-INV64).
func validateSchemaChangeCount(service *types.ServiceDeclaration) []ValidationError {
	open := len(openSchemaChanges(service))
	if open <= maxOpenSchemaChanges {
		return nil
	}
	return []ValidationError{{Category: "schema-change", Rule: "schema-change-count", Path: "schema_history", Found: strconv.Itoa(open), Message: fmt.Sprintf("%d schema changes are open at expand; at most %d may be open, so finish some with their contract steps first", open, maxOpenSchemaChanges)}}
}

// schemaChangeSettings gives the phase of each schema change, which is the phase of its last step, so that the
// author sees which changes are open at expand and which are finished at contract. A change with no step has no
// phase.
func schemaChangeSettings(service *types.ServiceDeclaration) []Setting {
	phases := schemaPhases(service.SchemaHistory)
	var settings []Setting
	for i, change := range service.SchemaChanges {
		if steps := phases[change.ID]; len(steps) > 0 {
			settings = append(settings, Setting{Path: schemaChangePath(i) + ".phase", Value: steps[len(steps)-1]})
		}
	}
	return settings
}
