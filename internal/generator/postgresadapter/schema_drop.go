package postgresadapter

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/types"
)

// The drop step of the runtime data path (ST-SUP-RC01-OC34, ST-SUP-RC01-OC47; ST-GEN-Q47). A drop removes a field or
// an entity across two releases, as Rails ignored_columns and the two-phase removal of Django do:
//
//  1. At expand the release of the change stops reading and writing the old column or table, and the migration keeps
//     it. For a required field it drops the NOT NULL and gives the column a constant DEFAULT, the fill value or the
//     zero value of the type, so that the release before never reads NULL from a row of the new release; it marks
//     the column with that relaxed form, which the schema check of the release before accepts (ST-SUP-RP15-LC04).
//     It makes the table of the release reports, so the release before reports itself from then on.
//  2. At contract, in a later release, the migration refuses while a release before the expand step reports itself
//     (ST-SUP-RC01-OC44), and then drops the column, or the foreign keys of the table one by one and then the table,
//     each statement in its own short transaction under the lock timeout of a step. So a DROP never waits long in
//     the lock queue of a busy table, and no statement holds the locks of the dropped table and of a table that it
//     refers to at one time.

// withDropped gives the models after a step with the old column or table of the drop change from the models
// earlier, before its expand step, as the expand step leaves it (dropColumn).
func withDropped(models, earlier []tableModel, change types.SchemaChange) ([]tableModel, error) {
	name := tableName(change.Entity)
	old, ok := findTable(earlier, name)
	if !ok {
		return nil, fmt.Errorf("the drop names the entity %s, whose table is not there before its expand step", change.Entity)
	}
	if change.Field == "" {
		if _, there := findTable(models, name); there {
			return nil, fmt.Errorf("the entity %s that the drop removes still has its table after the expand step", change.Entity)
		}
		return append(slices.Clone(models), old), nil
	}
	if fixedColumn(old, change.Field) {
		return nil, fmt.Errorf("the drop of %s.%s names a column that STEGO adds to each table itself; a drop removes only a declared field", change.Entity, change.Field)
	}
	column, ok := findColumn(old, change.Field)
	if !ok {
		return nil, fmt.Errorf("the drop of %s.%s finds no column %s before its expand step", change.Entity, change.Field, change.Field)
	}
	for _, fk := range old.foreign {
		if slices.Contains(fk.from, column.name) {
			return nil, fmt.Errorf("the drop of %s.%s is of a reference, whose foreign key and relation a drop does not carry yet; it is not built", change.Entity, change.Field)
		}
	}
	kept, err := dropColumn(change, column)
	if err != nil {
		return nil, err
	}
	for _, columns := range old.unique {
		if slices.Contains(columns, column.name) && (column.notNull || kept.defaultClause != "") {
			return nil, fmt.Errorf("the drop of %s.%s is of a unique field that would get one default in each row that the new release writes; it is not built", change.Entity, change.Field)
		}
	}
	out := withColumnOf(models, earlier, change, change.Field)
	for i := range out {
		if out[i].name != name {
			continue
		}
		out[i].columns = slices.Clone(out[i].columns)
		for j := range out[i].columns {
			if out[i].columns[j].name == change.Field {
				out[i].columns[j] = kept
			}
		}
	}
	return out, nil
}

// dropColumn gives the old column of a drop of a field as its expand step leaves it (ST-SUP-RC01-OC34): nullable, and
// for a required field with no default, with the fill value as its DEFAULT, else the zero value of the type; an
// optional field gets a DEFAULT only from a fill value, because NULL is a value of the old field.
func dropColumn(change types.SchemaChange, column columnModel) (columnModel, error) {
	dropped, reason := gen.DropDefinition(change.Definition)
	if reason != "" {
		return columnModel{}, fmt.Errorf("its definition %q cannot be read: %s", change.Definition, reason)
	}
	if dropped.Field.Type == types.FieldTypeRef {
		return columnModel{}, fmt.Errorf("the drop of %s.%s is of a reference, whose foreign key and relation a drop does not carry yet; it is not built", change.Entity, change.Field)
	}
	required := column.notNull
	column.notNull = false
	if column.defaultClause != "" {
		return column, nil
	}
	value := change.FillValue
	if value == nil && required {
		zero, ok := zeroValue(dropped.Field.Type)
		if !ok {
			return columnModel{}, fmt.Errorf("the required field %s.%s of the type %s has no zero value; give the drop a fill value", change.Entity, change.Field, dropped.Field.Type)
		}
		value = zero
	}
	if value == nil {
		return column, nil
	}
	clause, err := columnDefault(types.Field{Name: change.Field, Default: value})
	if err != nil {
		return columnModel{}, err
	}
	column.defaultClause, column.manifestDefault = clause, literalDefault(value, column.typ)
	return column, nil
}

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

// dropExpandSQL gives the migration of the expand step of a drop at the position of the schema history, from the
// tables before and after it. For a field it drops the NOT NULL and sets the DEFAULT that dropColumn gives, in one
// ALTER TABLE, with the relaxed form of the column as its comment in the same transaction (RelaxComment); for an
// entity it changes no table. In both cases it then makes the table of the release reports. It refuses a step that
// changes any other table or column, and a drop of a reference or of a unique field, which it does not carry yet.
func dropExpandSQL(ctx gen.Context, position int, before, after []tableModel, change types.SchemaChange) (string, error) {
	runtime, _, err := serviceRoles(ctx)
	if err != nil {
		return "", fmt.Errorf("a drop needs the release reports of the tenancy component: %w", err)
	}
	name := tableName(change.Entity)
	old, okOld := findTable(before, name)
	next, okNext := findTable(after, name)
	if !okOld || !okNext {
		return "", fmt.Errorf("the drop names the entity %s, whose table is not there", change.Entity)
	}
	for _, table := range before {
		if table.name == name {
			continue
		}
		if other, ok := findTable(after, table.name); !ok || !reflect.DeepEqual(other, table) {
			return "", fmt.Errorf("the drop of %s changes the table %s", change.Entity, table.name)
		}
	}
	if len(before) != len(after) {
		return "", fmt.Errorf("the drop of %s adds or removes a table at its expand step", change.Entity)
	}
	sql := ""
	if change.Field == "" {
		if !reflect.DeepEqual(old, next) {
			return "", fmt.Errorf("the drop of the entity %s changes its table at its expand step", change.Entity)
		}
		for _, table := range after {
			for _, fk := range table.foreign {
				if fk.table == name && table.name != name {
					return "", fmt.Errorf("the table %s refers to the table %s of the dropped entity; drop %s first", table.name, name, table.name)
				}
			}
		}
	} else {
		if fixedColumn(old, change.Field) {
			return "", fmt.Errorf("the drop of %s.%s names a column that STEGO adds to each table itself; a drop removes only a declared field", change.Entity, change.Field)
		}
		k, ok := findColumn(old, change.Field)
		kept, okKept := findColumn(next, change.Field)
		if !ok || !okKept {
			return "", fmt.Errorf("the drop of %s.%s finds no column %s", change.Entity, change.Field, change.Field)
		}
		for _, fk := range old.foreign {
			if slices.Contains(fk.from, k.name) {
				return "", fmt.Errorf("the drop of %s.%s is of a reference, whose foreign key and relation a drop does not carry yet; it is not built", change.Entity, change.Field)
			}
		}
		for _, columns := range old.unique {
			if slices.Contains(columns, k.name) && (k.notNull || kept.defaultClause != "") {
				return "", fmt.Errorf("the drop of %s.%s is of a unique field that would get one default in each row that the new release writes; it is not built", change.Entity, change.Field)
			}
		}
		same := next
		same.columns = slices.Clone(next.columns)
		same.columns[slices.IndexFunc(same.columns, func(c columnModel) bool { return c.name == k.name })] = k
		if !reflect.DeepEqual(normalized(same), normalized(old)) {
			return "", fmt.Errorf("the drop of %s.%s changes more of the table than its column", change.Entity, change.Field)
		}
		var parts []string
		nullable := k.notNull && !kept.notNull
		if nullable {
			parts = append(parts, fmt.Sprintf("ALTER COLUMN %s DROP NOT NULL", sqlIdentifier(k.name)))
		}
		defaultValue := ""
		if kept.defaultClause != k.defaultClause {
			parts = append(parts, fmt.Sprintf("ALTER COLUMN %s SET%s", sqlIdentifier(k.name), kept.defaultClause))
			defaultValue = kept.manifestDefault
		}
		if len(parts) > 0 {
			table := "public." + sqlIdentifier(name)
			comment := RelaxComment(StepMigrationName(position, change.ID, "expand"), "", nullable, defaultValue, nil)
			sql = fmt.Sprintf("ALTER TABLE %s %s;\nCOMMENT ON COLUMN %s.%s IS %s;\n", table, strings.Join(parts, ", "), table, sqlIdentifier(k.name), sqlString(comment))
		}
	}
	return sql + fmt.Sprintf(releasesText, sqlIdentifier(runtime)), nil
}

// dropContractSQL gives the contract step of a drop (ST-SUP-RC01-OC47): before is the tables of the expand step, with
// the old column or table, and after the tables after the step, without it. For a field it is one statement, DROP
// COLUMN IF EXISTS, which drops the CHECK and the indexes of the column; for an entity it drops each foreign key of
// the table, the tenant key of an owned table too, each in its own statement, and then DROP TABLE IF EXISTS. Each
// statement takes the lock of the table first and then checks the running releases again in its transaction, so a
// refused check leaves the table with all its keys for the release before (security review of PR #423, M1).
func dropContractSQL(ctx gen.Context, before, after []tableModel, change types.SchemaChange) (*contractStep, error) {
	name := tableName(change.Entity)
	old, okOld := findTable(before, name)
	if !okOld {
		return nil, fmt.Errorf("the contract of the drop finds no table %s", name)
	}
	table := "public." + sqlIdentifier(name)
	out := &contractStep{change: change.ID, table: table, schema: "public", tableName: name, drop: true}
	if change.Field != "" {
		next, okNext := findTable(after, name)
		if !okNext {
			return nil, fmt.Errorf("the contract of the drop of %s.%s finds no table %s after it", change.Entity, change.Field, name)
		}
		if _, ok := findColumn(old, change.Field); !ok {
			return nil, fmt.Errorf("the contract of the drop of %s.%s finds no column %s", change.Entity, change.Field, change.Field)
		}
		if _, kept := findColumn(next, change.Field); kept {
			return nil, fmt.Errorf("the table %s keeps the column %s after the contract of its drop", name, change.Field)
		}
		out.from = change.Field
		out.statements = []contractStatement{{sql: fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s", table, sqlIdentifier(change.Field)), guard: true},
			{sql: purgeCountsSQL("counter = " + sqlString(change.Entity+"."+change.Field))}}
		return out, nil
	}
	if _, kept := findTable(after, name); kept {
		return nil, fmt.Errorf("the table %s is still there after the contract of the drop of %s", name, change.Entity)
	}
	for _, other := range after {
		for _, fk := range other.foreign {
			if fk.table == name {
				return nil, fmt.Errorf("the table %s refers to the table %s of the dropped entity", other.name, name)
			}
		}
	}
	for _, fk := range old.foreign {
		out.statements = append(out.statements, contractStatement{sql: fmt.Sprintf("ALTER TABLE IF EXISTS %s DROP CONSTRAINT IF EXISTS %s", table, sqlIdentifier(fk.name)), guard: true})
	}
	if old.owned {
		out.statements = append(out.statements, contractStatement{sql: fmt.Sprintf("ALTER TABLE IF EXISTS %s DROP CONSTRAINT IF EXISTS %s", table, sqlIdentifier(TenantKeyName(name, tenantColumn(ctx)))), guard: true})
	}
	out.statements = append(out.statements, contractStatement{sql: "DROP TABLE IF EXISTS " + table, guard: true},
		contractStatement{sql: purgeCountsSQL("pg_catalog.split_part(counter, '.', 1) = " + sqlString(change.Entity))})
	return out, nil
}

// purgeCountsSQL gives the last statement of the contract step of a drop: it deletes the counts of wrong attempts
// (attempt_counts, ST-SUP-RP10-LC54) of the dropped field, or of each field of the dropped entity, whose counter is
// <entity>.<field>, so no count stays of a field that is gone (decision ST-SUP-Q216 (a)). The table forces its
// tenant policy also for the owner role, so the statement deletes the counts of each tenant of the tenant list under
// the tenant setting of that tenant, as rowsLeft reads the rows; a database with no table of the counts or no tenant
// list has no count. It runs again with no error after a run that deleted the counts.
func purgeCountsSQL(where string) string {
	return "DO $purge$ DECLARE t text; BEGIN IF pg_catalog.to_regclass('public.attempt_counts') IS NULL OR " +
		"pg_catalog.to_regclass('public.stego_tenants') IS NULL THEN RETURN; END IF; " +
		"FOR t IN SELECT s.tenant_id FROM public.stego_tenants s ORDER BY s.tenant_id COLLATE \"C\" LOOP " +
		"PERFORM pg_catalog.set_config('app.tenant', t, true); DELETE FROM public.attempt_counts WHERE tenant_id = t AND " + where + "; " +
		"END LOOP; PERFORM pg_catalog.set_config('app.tenant', '', true); END $purge$"
}

// dropBodyBefore gives the body of the contract step of a drop as versions of the adapter before the delete of the
// counts wrote it (decision ST-SUP-Q216 (a)): a database that ran the step then holds the digest of that body, which
// the ledger accepts as an earlier digest of the step.
func dropBodyBefore(c *contractStep) string {
	var body strings.Builder
	for _, statement := range c.statements {
		if strings.HasPrefix(statement.sql, "DO $purge$") {
			continue
		}
		for _, text := range append([]string{statement.sql}, statement.also...) {
			body.WriteString(text + ";\n")
		}
	}
	return body.String()
}

// normalized gives a copy of the table with its columns, checks, foreign keys and indexes in the order of their
// names, so that two models of one table compare by what they hold: the models after a step keep an old column at
// the end of the table, where the database has it in its place.
func normalized(table tableModel) tableModel {
	table.columns = slices.SortedFunc(slices.Values(table.columns), func(a, b columnModel) int { return strings.Compare(a.name, b.name) })
	table.checks = slices.SortedFunc(slices.Values(table.checks), func(a, b checkModel) int { return strings.Compare(a.name, b.name) })
	table.foreign = slices.SortedFunc(slices.Values(table.foreign), func(a, b foreignModel) int { return strings.Compare(a.name, b.name) })
	table.search = slices.Sorted(slices.Values(table.search))
	table.unique = slices.SortedFunc(slices.Values(table.unique), func(a, b []string) int { return slices.Compare(a, b) })
	// Which columns hold a secret is a mark of the manifest, not of the schema that a step changes.
	table.secret = nil
	return table
}

// fixedColumn tells whether the column is one that STEGO adds to each table itself, which no schema change may name:
// the key, the tenant column, the times of the row, its version and its authorization token (security review of
// PR #423, L1).
func fixedColumn(table tableModel, column string) bool {
	return slices.Contains([]string{keyColumn, createdColumn, updatedColumn, versionColumn, tokenColumn}, column) || slices.Contains(table.lead, column)
}
