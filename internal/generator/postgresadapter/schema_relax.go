package postgresadapter

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/910-software-solutions/stego/internal/types"
)

// The relax step of the runtime data path (ST-SUP-RC01-OC45; decision on finding F12 of the schema-change
// research, owner, 2026-11-13): a relax changes one column in place, in one release, and the release before keeps
// running on the result. Each of its migrations takes the ACCESS EXCLUSIVE lock of the table for one short
// statement that reads no row, under the lock timeout of a step:
//
//  1. Its own migration runs one ALTER TABLE: the CHECK of the field after the change NOT VALID under
//     RelaxCheckName, the drop of the old CHECK when the type changes or the field keeps no CHECK, the longer
//     character varying, DROP NOT NULL and SET DEFAULT. Then, in the same transaction, it marks the column with the
//     exact relaxed form (RelaxComment); COMMENT ON COLUMN then needs no lock beyond the one that the ALTER TABLE
//     holds (security review of PR #334). ALTER COLUMN TYPE checks each validated CHECK on the column again with a scan of the table
//     under its lock (PostgreSQL 17.7), so the old CHECK goes before the type changes; the new CHECK holds each new
//     write, and the old CHECK held each row there.
//  2. The validate migration validates the new CHECK, which takes SHARE UPDATE EXCLUSIVE and blocks no write.
//  3. The finish migration drops the old CHECK, when it is still there, and gives the new CHECK the name of the
//     field's CHECK.
//
// The schema check of the release before accepts each state between these migrations on a column with the marker
// (registry/runtime/manifest, ST-SUP-RP15-LC04).

// RelaxMarker is the first word of the comment that the relax step gives the column, before the name of the step
// migration. Its text is the runtime library's manifest.RelaxMarker (ST-SUP-RC01-OC45-T01).
const RelaxMarker = "stego relax"

// RelaxComment gives the comment of the column after the relax step: RelaxMarker and the step migration on the first
// line, then one line for each attribute that the step relaxes, with its exact form after the step as the catalog
// gives it: type T, nullable, default D (as the schema manifest states a default) and check C (the definition as
// pg_get_constraintdef gives it) or check none. The schema check of every release accepts on the column only the form
// of its own manifest or this form (registry/runtime/manifest, ST-SUP-RP15-LC04).
func RelaxComment(step string, typ string, nullable bool, defaultValue string, check *string) string {
	lines := []string{RelaxMarker + " " + step}
	if typ != "" {
		lines = append(lines, "type "+typ)
	}
	if nullable {
		lines = append(lines, "nullable")
	}
	if defaultValue != "" {
		lines = append(lines, "default "+defaultValue)
	}
	if check != nil {
		form := "none"
		if *check != "" {
			form = *check
		}
		lines = append(lines, "check "+form)
	}
	return strings.Join(lines, "\n")
}

// FinishMigrationName gives the ledger name of the migration that ends the relax step name, after its validate
// migration.
func FinishMigrationName(name string) string { return ValidateMigrationName(name) + "_finish" }

// RelaxCheckName gives the name of the new CHECK of a relax until the finish migration renames it:
// <table>_<column>_relax, cut as sqlName cuts a name.
func RelaxCheckName(entity, column string) string { return sqlName(tableName(entity), column, "relax") }

// varchar reads the length of a character varying(n) column type.
var varchar = regexp.MustCompile(`^character varying\(([0-9]+)\)$`)

// relaxSteps gives the migrations of the relax step at the position of the schema history from the tables before
// and after it. It refuses a step that changes anything other than the column of its field and that column's CHECK,
// and a change of the column that PostgreSQL cannot make in place with no scan under its strong lock: a type other
// than a longer character varying, NOT NULL added, a default taken away or changed, or a CHECK of the field that is
// not the CHECK of its enum values.
func relaxSteps(position int, before, after []tableModel, change types.SchemaChange) ([]schemaStep, error) {
	if len(before) != len(after) {
		return nil, fmt.Errorf("a relax changes one column, and the step adds or removes a table")
	}
	name := tableName(change.Entity)
	old, ok := findTable(before, name)
	next, found := findTable(after, name)
	if !ok || !found {
		return nil, fmt.Errorf("a relax changes the table %s, which the declaration does not have", name)
	}
	oldColumn, ok := findColumn(old, change.Field)
	newColumn, found := findColumn(next, change.Field)
	if !ok || !found {
		return nil, fmt.Errorf("a relax changes the column %s.%s, which the table does not have", name, change.Field)
	}
	checkName := sqlName(name, change.Field, "check")
	oldCheck, hadCheck := findCheck(old, checkName)
	newCheck, hasCheck := findCheck(next, checkName)
	// Only the column and its CHECK may change, and every other table must stay as it is.
	same := next
	same.columns = slices.Clone(next.columns)
	same.columns[slices.IndexFunc(same.columns, func(c columnModel) bool { return c.name == change.Field })] = oldColumn
	same.checks = slices.DeleteFunc(slices.Clone(next.checks), func(c checkModel) bool { return c.name == checkName })
	keptChecks := slices.DeleteFunc(slices.Clone(old.checks), func(c checkModel) bool { return c.name == checkName })
	if !slices.Equal(same.checks, keptChecks) {
		return nil, fmt.Errorf("a relax of %s.%s changes another CHECK of the table", name, change.Field)
	}
	same.checks = old.checks
	if !reflect.DeepEqual(same, old) {
		return nil, fmt.Errorf("a relax of %s.%s changes more of the table than the column", name, change.Field)
	}
	for i := range before {
		if before[i].name != name && !reflect.DeepEqual(before[i], after[i]) {
			return nil, fmt.Errorf("a relax of %s.%s changes the table %s", name, change.Field, before[i].name)
		}
	}
	table := "public." + sqlIdentifier(name)
	column := sqlIdentifier(change.Field)
	typeChange := oldColumn.typ != newColumn.typ
	if typeChange {
		from, to := varchar.FindStringSubmatch(oldColumn.typ), varchar.FindStringSubmatch(newColumn.typ)
		if from == nil || to == nil || atoi(to[1]) <= atoi(from[1]) {
			return nil, fmt.Errorf("a relax of %s.%s changes the column type from %s to %s; only a longer character varying keeps every value in place with no scan", name, change.Field, oldColumn.typ, newColumn.typ)
		}
	}
	if newColumn.notNull && !oldColumn.notNull {
		return nil, fmt.Errorf("a relax of %s.%s makes the column NOT NULL, which the rows and the release before may not hold", name, change.Field)
	}
	if oldColumn.defaultClause != "" && newColumn.defaultClause != oldColumn.defaultClause {
		return nil, fmt.Errorf("a relax of %s.%s takes away or changes the default of the column; a relax only gives a default to a column with none", name, change.Field)
	}
	checkChange := hadCheck != hasCheck || (hadCheck && oldCheck.definition != newCheck.definition)
	if checkChange && (!hadCheck || !valuesOnly(oldCheck.definition) || (hasCheck && !valuesOnly(newCheck.definition))) {
		return nil, fmt.Errorf("a relax of %s.%s changes a CHECK of the field other than the CHECK of its enum values", name, change.Field)
	}
	if checkChange && hasCheck {
		kept := definitionValues(newCheck.definition)
		for _, value := range definitionValues(oldCheck.definition) {
			if !slices.Contains(kept, value) {
				return nil, fmt.Errorf("a relax of %s.%s takes the enum value %s away, which rows and the release before may hold", name, change.Field, value)
			}
		}
	}
	relaxCheck := hasCheck && (checkChange || (typeChange && hadCheck))
	dropFirst := hadCheck && (typeChange || !hasCheck)
	var parts []string
	relax := RelaxCheckName(change.Entity, change.Field)
	if relaxCheck {
		parts = append(parts, fmt.Sprintf("ADD CONSTRAINT %s CHECK (%s) NOT VALID", sqlIdentifier(relax), newCheck.expression))
	}
	if dropFirst {
		parts = append(parts, "DROP CONSTRAINT "+sqlIdentifier(checkName))
	}
	if typeChange {
		parts = append(parts, fmt.Sprintf("ALTER COLUMN %s TYPE %s", column, newColumn.typ))
	}
	if oldColumn.notNull && !newColumn.notNull {
		parts = append(parts, fmt.Sprintf("ALTER COLUMN %s DROP NOT NULL", column))
	}
	if newColumn.defaultClause != oldColumn.defaultClause {
		parts = append(parts, fmt.Sprintf("ALTER COLUMN %s SET%s", column, newColumn.defaultClause))
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("a relax of %s.%s changes nothing of the column", name, change.Field)
	}
	step := StepMigrationName(position, change.ID, "expand")
	form := struct {
		typ, defaultValue string
		check             *string
	}{}
	if typeChange {
		form.typ = newColumn.typ
	}
	if newColumn.defaultClause != oldColumn.defaultClause {
		form.defaultValue = newColumn.manifestDefault
	}
	if checkChange {
		definition := ""
		if hasCheck {
			definition = newCheck.definition
		}
		form.check = &definition
	}
	comment := RelaxComment(step, form.typ, oldColumn.notNull && !newColumn.notNull, form.defaultValue, form.check)
	body := fmt.Sprintf("ALTER TABLE %s %s;\nCOMMENT ON COLUMN %s.%s IS %s;\n", table, strings.Join(parts, ", "), table, column, sqlString(comment))
	steps := []schemaStep{{Name: step, Change: change, Phase: "expand", Body: body}}
	if relaxCheck {
		validates := []validation{{table: table, schema: "public", tableName: name, name: relax}}
		steps = append(steps, schemaStep{Name: ValidateMigrationName(step), Change: change, Phase: "expand", Body: validateBody(validates), Validate: true, Validations: validates})
		finish := ""
		if !dropFirst {
			finish = fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s;\n", table, sqlIdentifier(checkName))
		}
		finish += fmt.Sprintf("ALTER TABLE %s RENAME CONSTRAINT %s TO %s;\n", table, sqlIdentifier(relax), sqlIdentifier(checkName))
		steps = append(steps, schemaStep{Name: FinishMigrationName(step), Change: change, Phase: "expand", Body: finish})
	}
	return steps, nil
}

// valuesOnly tells whether a CHECK definition, as pg_get_constraintdef gives it, is the CHECK of enum values alone.
func valuesOnly(definition string) bool {
	return strings.HasPrefix(definition, "CHECK ((") && !strings.Contains(definition, " AND ") &&
		(strings.Contains(definition, " = ANY (ARRAY['") || strings.HasSuffix(definition, "'::text))"))
}

// textValue is one value of a CHECK of enum values, as pg_get_constraintdef writes it.
var textValue = regexp.MustCompile(`'((?:[^']|'')*)'::text`)

// definitionValues gives the values of a CHECK of enum values.
func definitionValues(definition string) []string {
	var values []string
	for _, m := range textValue.FindAllStringSubmatch(definition, -1) {
		values = append(values, strings.ReplaceAll(m[1], "''", "'"))
	}
	return values
}

func findCheck(table tableModel, name string) (checkModel, bool) {
	for _, c := range table.checks {
		if c.name == name {
			return c, true
		}
	}
	return checkModel{}, false
}

func atoi(text string) int {
	n, _ := strconv.Atoi(text)
	return n
}
