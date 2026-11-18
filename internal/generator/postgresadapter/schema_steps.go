package postgresadapter

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// The schema steps of the runtime data path (ST-SUP-RC01-OC22): 001_initial is the baseline, the tables of the
// first release, and each step of the schema history is one SQL migration after the component's own migrations,
// named step_<position>_<change ID>_<phase>, from the declaration alone. A database that the migrations of each
// release made in turn and a new one that the migrations of the last release made get the same schema
// (ST-GEN-INV35). Generation carries out the add, rename, retype, require, relax, within, drop and seal kinds (the
// last three in schema_relax.go, schema_drop.go and schema_seal.go): an add at expand creates the table of a new entity, with its row-level security in a pooled service, or adds the column of a new field, and changes
// nothing that is there (ST-GEN-UC12-R02); a rename and a retype add a new column with a sync trigger and a backfill,
// and a require adds its column with the fill value as its default (decision on ST-GEN-Q22 (a), orchestrator under the
// owner's delegation, 2026-11-14); each but the add has a contract step (ST-GEN-UC15).

// maxSchemaSteps is the most steps that the four digits of a step position name, so the byte order of the names
// is the order of the steps.
const maxSchemaSteps = 9999

// schemaStep is one migration of a step of the schema history: its ledger name, its change, its phase and its SQL
// body. A step on a table with rows has more than one: its own migration, which adds each CHECK and foreign key NOT
// VALID, so that it scans no row under its strong lock; then, for an index, Index, the qualified name of the index,
// which the migration builds CONCURRENTLY outside a transaction (ST-SUP-RC01-OC38); then, when Validate, the
// migration that validates those constraints in its own transaction (ST-SUP-RC01-OC22, decision on finding F2,
// orchestrator under the owner's delegation, 2026-11-13).
type schemaStep struct {
	Name       string
	Change     types.SchemaChange
	Phase      string
	Body       string
	Index      string
	Definition string
	Validate   bool
	// Validations are the constraints that the migration of a step that validates, Validate, validates in the order
	// of its body: a foreign key through the helper of the tenant runtime, past row-level security (ST-SUP-RP04-LC10).
	Validations []validation
	// Contract is the contract step of a rename, which runs each statement in its own short transaction after its
	// check (ST-SUP-RC01-OC24, ST-SUP-RC01-OC39); Expand is the migration of the expand step of its change.
	Contract *contractStep
	Expand   string
}

// contractStep is what the contract migration of a rename checks and runs: the change, the qualified table, and its
// schema and its name with no quotes, the completeness CHECK, its definition as pg_get_constraintdef gives it and the
// sync trigger of the expand step, the name of the old column, the old and the new column as quoted identifiers, the
// tenant column of an owned table and the schema of the tenant list stego_tenants, and its statements in their order.
type contractStep struct {
	change, table, schema, tableName, check, definition, trigger, from, old, new, tenant, tenantSchema string
	statements                                                                                         []contractStatement
	// drop tells that the step is the contract of a drop, whose check is the running releases alone, while the old
	// column from, or with no from the table, is there (ST-SUP-RC01-OC44, ST-SUP-RC01-OC47).
	drop bool
	// seal tells that the step is the contract of a seal, whose check is its seal checkpoint, the rows left and the
	// running releases while the clear column from is there (ST-SUP-RC01-OC50).
	seal bool
}

// contractStatement is one statement of a contract step: its SQL; when, a query that gives whether the statement
// still has to run, or empty; long, for the long statement timeout of a VALIDATE; guard, for the check of the step
// again in its transaction, before the first statement after which the release before can no longer write; and
// also, the statements that run after it in the same transaction.
type contractStatement struct {
	sql, when   string
	long, guard bool
	also        []string
}

// stepAddition is what an add step gives: the SQL of its own migration, each index that it builds on a table with
// rows, the SQL of the migration after those indexes that adds the sync trigger of a rename, and the constraints that
// it validates.
type stepAddition struct {
	body    strings.Builder
	indexes []stepIndex
	// keys adds, NOT VALID, each foreign key onto a unique key that the step builds CONCURRENTLY, after its index
	// migrations, so the key it refers to exists (ST-SUP-RC01-OC46).
	keys      strings.Builder
	sync      strings.Builder
	validates []validation
	// deferred tells whether a foreign key refers to a unique key that the step builds, so it goes to keys.
	deferred func(fk foreignModel) bool
}

// addKey adds the foreign key fk of table NOT VALID in the keys migration, and its validation.
func (out *stepAddition) addKey(table tableModel, fk foreignModel) {
	qualified := "public." + sqlIdentifier(table.name)
	fmt.Fprintf(&out.keys, "ALTER TABLE %s ADD CONSTRAINT %s %s NOT VALID;\n", qualified, sqlIdentifier(fk.name), foreignSQL(fk))
	out.validates = append(out.validates, validation{table: qualified, schema: "public", tableName: table.name, name: fk.name, foreign: true})
}

// validation is one constraint that the validate migration of a step validates: the qualified table as SQL text, its
// schema and its name with no quotes, the name of the constraint with no quotes, and foreign for a foreign key.
type validation struct {
	table, schema, tableName, name string
	foreign                        bool
}

// validateBody gives the body of the validate migration of the constraints validates, one ALTER TABLE ... VALIDATE
// CONSTRAINT for each, which the ledger digest covers.
func validateBody(validates []validation) string {
	var b strings.Builder
	for _, v := range validates {
		fmt.Fprintf(&b, "ALTER TABLE %s VALIDATE CONSTRAINT %s;\n", v.table, sqlIdentifier(v.name))
	}
	return b.String()
}

// stepIndex is one index of a step on a table with rows: its qualified name, its CREATE INDEX CONCURRENTLY and its
// definition as pg_get_indexdef writes it.
type stepIndex struct{ name, statement, definition string }

// IndexMigrationName and ValidateMigrationName give the ledger names of the migrations after the migration name of
// a step: the build of its index number n from 1, in two digits, and the validation of its constraints. They sort
// after name and before the next step.
func IndexMigrationName(name string, n int) string { return fmt.Sprintf("%s_index_%02d", name, n) }

// KeysMigrationName gives the ledger name of the migration that adds the foreign keys of the step name onto the
// unique keys that its index migrations build, after them (ST-SUP-RC01-OC46).
func KeysMigrationName(name string) string { return name + "_keys" }

// ValidateMigrationName gives the ledger name of the migration that validates the constraints of the step name.
func ValidateMigrationName(name string) string { return name + "_validate" }

// SyncMigrationName gives the ledger name of the migration that adds the sync trigger of the step name, after its
// indexes and before the validation of its constraints.
func SyncMigrationName(name string) string { return name + "_sync" }

// maxStepIndexes is the most indexes that the two digits of an index migration name number.
const maxStepIndexes = 99

// StepMigrationName gives the ledger name of the migration of a step: step_<position>_<change ID>_<phase>, with the
// position from 1 in four digits (ST-SUP-RC01-OC22).
func StepMigrationName(position int, change, phase string) string {
	return fmt.Sprintf("step_%04d_%s_%s", position, change, phase)
}

// stepName is the grammar of the ledger name of a step migration.
var stepName = regexp.MustCompile(`^step_[0-9]{4}_[a-z0-9]+(-[a-z0-9]+)*_(expand|contract)(_index_[0-9]{2}|_keys|_sync|_validate|_validate_finish)?$`)

// CheckLedgerName refuses a ledgered migration whose name is not 001_initial, 100_tenancy_row_security or the name of
// a schema step (ST-SUP-RC01-OC43): the ledger applies and checks migrations in the byte order of their names, so a
// component migration that a later release adds, such as 101_x, would sort before the step rows that an upgraded
// database holds, and that database would fail the ledger check. Such a migration waits for its own schema step.
func CheckLedgerName(name string) error {
	if name == "001_initial" || name == tenancyMigration || stepName.MatchString(name) {
		return nil
	}
	return fmt.Errorf("the ledgered migration %q is not 001_initial, %s or a schema step; a component migration of a later release would sort before the step rows of an upgraded ledger (ST-SUP-RC01-OC43)", name, tenancyMigration)
}

// tenancyMigration is the ledger name of the row-level security of the tenancy component.
const tenancyMigration = "100_tenancy_row_security"

// StepMigrationFile gives the path, in the output namespace of the adapter, of the SQL file of a step migration.
func StepMigrationFile(ns, name string) string { return path.Join(ns, "migrations", name+".sql") }

// schemaSteps gives the migration of each step of the schema history, in its order: the SQL that takes the tables
// of the shape before the step to the shape after it (gen.Context.AtSchemaStep). It refuses a step that is not the
// expand step of an add, an add that changes or removes what is there, a column that is NOT NULL and has no
// default, and a foreign key to a table that a later step adds.
func schemaSteps(ctx gen.Context) ([]schemaStep, error) {
	if len(ctx.SchemaHistory) > maxSchemaSteps {
		return nil, fmt.Errorf("the schema history has %d steps; at most %d steps are allowed", len(ctx.SchemaHistory), maxSchemaSteps)
	}
	changes := map[string]types.SchemaChange{}
	for _, change := range ctx.SchemaChanges {
		changes[change.ID] = change
	}
	var at func(steps int) ([]tableModel, error)
	at = func(steps int) ([]tableModel, error) {
		shaped, err := ctx.AtSchemaStep(steps)
		if err != nil {
			return nil, err
		}
		models, err := tableModels(shaped)
		if err != nil {
			return nil, err
		}
		if models, err = requiredAt(ctx, models, steps); err != nil {
			return nil, err
		}
		// The old column of a rename or a retype stays in the table from its expand step until its contract step, so
		// the models after each step between keep it, with its CHECK and its indexes.
		expand, contract := map[string]int{}, map[string]int{}
		for i, step := range ctx.SchemaHistory {
			if step.Phase == "expand" {
				expand[step.Change] = i + 1
			} else {
				contract[step.Change] = i + 1
			}
		}
		for _, change := range ctx.SchemaChanges {
			e, c := expand[change.ID], contract[change.ID]
			if (change.Kind != "rename" && change.Kind != "retype") || e == 0 || e > steps || (c != 0 && c <= steps) {
				continue
			}
			pair, err := pairOf(ctx, change)
			if err != nil {
				return nil, fmt.Errorf("schema change %s: %w", change.ID, err)
			}
			earlier, err := at(e - 1)
			if err != nil {
				return nil, err
			}
			models = withColumnOf(models, earlier, change, pair.k)
		}
		// The clear column of a seal stays in the table from its expand step until its contract step, and its sealed
		// column is nullable until then (ST-SUP-RC01-OC49).
		for _, change := range ctx.SchemaChanges {
			e, c := expand[change.ID], contract[change.ID]
			if change.Kind != "seal" || e == 0 || e > steps || (c != 0 && c <= steps) {
				continue
			}
			earlier, err := at(e - 1)
			if err != nil {
				return nil, err
			}
			if models, err = withSealing(ctx, models, earlier, change); err != nil {
				return nil, fmt.Errorf("schema change %s: %w", change.ID, err)
			}
		}
		// The old column or table of a drop stays in the database from its expand step until its contract step, so
		// the models after each step between keep it, as the expand step leaves it (ST-SUP-RC01-OC34).
		for _, change := range ctx.SchemaChanges {
			e, c := expand[change.ID], contract[change.ID]
			if change.Kind != "drop" || e == 0 || e > steps || (c != 0 && c <= steps) {
				continue
			}
			earlier, err := at(e - 1)
			if err != nil {
				return nil, err
			}
			if models, err = withDropped(models, earlier, change); err != nil {
				return nil, fmt.Errorf("schema change %s: %w", change.ID, err)
			}
		}
		return models, nil
	}
	before, err := at(0)
	if err != nil {
		return nil, err
	}
	key := tenantColumn(ctx)
	security, err := rowSecurityImport(ctx)
	if err != nil {
		return nil, err
	}
	var steps []schemaStep
	for i, step := range ctx.SchemaHistory {
		change, ok := changes[step.Change]
		if !ok {
			return nil, fmt.Errorf("schema_history[%d] names the schema change %q, which schema_changes does not declare", i, step.Change)
		}
		built := change.Kind == "rename" || change.Kind == "retype" || change.Kind == "require" || change.Kind == "drop" || change.Kind == "seal" || ((change.Kind == "add" || change.Kind == "relax" || change.Kind == "within") && step.Phase == "expand")
		if !built || (step.Phase != "expand" && step.Phase != "contract") {
			return nil, fmt.Errorf("schema_history[%d]: the step %s of the schema change %s (%s) is not built; generation carries out only the expand step of an add, a relax and a within and the expand and contract steps of a rename, a retype, a require, a drop and a seal", i, step.Phase, change.ID, change.Kind)
		}
		after, err := at(i + 1)
		if err != nil {
			return nil, err
		}
		if change.Kind == "drop" && step.Phase == "expand" {
			// A drop at expand keeps the old column or table and only takes from it what the release of the change
			// does not write (ST-SUP-RC01-OC34).
			body, err := dropExpandSQL(ctx, i+1, before, after, change)
			if err != nil {
				return nil, fmt.Errorf("schema change %s: %w", change.ID, err)
			}
			steps = append(steps, schemaStep{Name: StepMigrationName(i+1, change.ID, step.Phase), Change: change, Phase: step.Phase, Body: body})
			before = after
			continue
		}
		if change.Kind == "seal" && step.Phase == "expand" {
			// A seal at expand adds the sealed column beside the clear one (ST-SUP-RC01-OC49).
			body, err := sealExpandSQL(ctx, i+1, before, after, change)
			if err != nil {
				return nil, fmt.Errorf("schema change %s: %w", change.ID, err)
			}
			steps = append(steps, schemaStep{Name: StepMigrationName(i+1, change.ID, step.Phase), Change: change, Phase: step.Phase, Body: body})
			before = after
			continue
		}
		if change.Kind == "relax" {
			relaxed, err := relaxSteps(i+1, before, after, change)
			if err != nil {
				return nil, fmt.Errorf("schema change %s: %w", change.ID, err)
			}
			steps = append(steps, relaxed...)
			before = after
			continue
		}
		if step.Phase == "contract" {
			expand := ""
			for j, earlier := range ctx.SchemaHistory[:i] {
				if earlier.Change == change.ID && earlier.Phase == "expand" {
					expand = StepMigrationName(j+1, change.ID, earlier.Phase)
				}
			}
			if expand == "" {
				return nil, fmt.Errorf("schema_history[%d]: the contract step of %s has no expand step before it", i, change.ID)
			}
			var contract *contractStep
			switch change.Kind {
			case "require":
				contract, err = requireContractSQL(before, after, change)
			case "drop":
				contract, err = dropContractSQL(ctx, before, after, change)
			case "seal":
				contract, err = sealContractSQL(ctx, before, after, change)
			default:
				contract, err = contractSQL(ctx, before, after, change, expand)
			}
			if err != nil {
				return nil, fmt.Errorf("schema change %s: %w", change.ID, err)
			}
			var body strings.Builder
			for _, statement := range contract.statements {
				for _, text := range append([]string{statement.sql}, statement.also...) {
					body.WriteString(text + ";\n")
				}
			}
			steps = append(steps, schemaStep{Name: StepMigrationName(i+1, change.ID, step.Phase), Change: change, Phase: step.Phase,
				Body: body.String(), Contract: contract, Expand: expand})
			before = after
			continue
		}
		if change.Kind == "rename" || change.Kind == "retype" {
			// The schema check of the release before accepts at most one sync trigger on a table (ST-SUP-RP15-LC02
			// rule 1), so a second rename or retype on the table waits for the contract step of the first.
			for j, earlier := range ctx.SchemaHistory[:i] {
				other := changes[earlier.Change]
				if earlier.Phase != "expand" || other.ID == change.ID || other.Entity != change.Entity || (other.Kind != "rename" && other.Kind != "retype") {
					continue
				}
				if !slices.ContainsFunc(ctx.SchemaHistory[j+1:i], func(s types.SchemaStep) bool { return s.Change == other.ID && s.Phase == "contract" }) {
					return nil, fmt.Errorf("schema change %s: the %s %s on the entity %s is still at expand; the schema check of the release before accepts one sync trigger on a table, so put the contract step of %s first", change.ID, other.Kind, other.ID, change.Entity, other.ID)
				}
			}
		}
		if change.Kind == "require" {
			// Each row gets the fill value, so a unique index of the field could not be built on a table with two rows
			// (security review of PR #335, L3, orchestrator under the owner's delegation, 2026-11-14).
			if table, ok := findTable(after, tableName(change.Entity)); ok && slices.ContainsFunc(table.unique, func(columns []string) bool { return slices.Contains(columns, change.Field) }) {
				return nil, fmt.Errorf("schema change %s: the require of %s.%s is of a unique field, whose rows would all get the fill value; it is not built", change.ID, change.Entity, change.Field)
			}
		}
		var added *stepAddition
		if change.Kind == "rename" || change.Kind == "retype" {
			added, err = renameSQL(ctx, before, after, change)
		} else {
			added, err = addedSQL(before, after, key, security != "")
		}
		if err != nil {
			return nil, fmt.Errorf("schema change %s: %w", change.ID, err)
		}
		if added.body.Len() == 0 && len(added.indexes) == 0 && added.keys.Len() == 0 {
			return nil, fmt.Errorf("schema change %s adds nothing to the tables", change.ID)
		}
		if len(added.indexes) > maxStepIndexes {
			return nil, fmt.Errorf("schema change %s builds %d indexes; at most %d are allowed", change.ID, len(added.indexes), maxStepIndexes)
		}
		name := StepMigrationName(i+1, change.ID, step.Phase)
		steps = append(steps, schemaStep{Name: name, Change: change, Phase: step.Phase, Body: added.body.String()})
		for n, index := range added.indexes {
			steps = append(steps, schemaStep{Name: IndexMigrationName(name, n+1), Change: change, Phase: step.Phase, Body: index.statement, Index: index.name, Definition: index.definition})
		}
		if added.keys.Len() > 0 {
			steps = append(steps, schemaStep{Name: KeysMigrationName(name), Change: change, Phase: step.Phase, Body: added.keys.String()})
		}
		if added.sync.Len() > 0 {
			steps = append(steps, schemaStep{Name: SyncMigrationName(name), Change: change, Phase: step.Phase, Body: added.sync.String()})
		}
		if len(added.validates) > 0 {
			steps = append(steps, schemaStep{Name: ValidateMigrationName(name), Change: change, Phase: step.Phase, Body: validateBody(added.validates), Validate: true, Validations: added.validates})
		}
		before = after
	}
	return steps, nil
}

// addedSQL gives the SQL that takes the tables before to the tables after, which may only add: a new table with its
// indexes and, when rowSecurity, the row-level security of an owned table; and on a table that is there, a column
// that is nullable or has a default, a CHECK, a foreign key and an index. It refuses every other difference.
func addedSQL(before, after []tableModel, key string, rowSecurity bool) (*stepAddition, error) {
	present := map[string]tableModel{}
	for _, table := range before {
		present[table.name] = table
	}
	known := map[string]bool{}
	for _, table := range after {
		known[table.name] = true
	}
	for _, table := range before {
		if !known[table.name] {
			return nil, fmt.Errorf("the step removes the table %s, and an add step only adds", table.name)
		}
	}
	out := &stepAddition{}
	// A unique key that the step adds to a table with rows is built CONCURRENTLY after the step's own migration, so
	// a foreign key onto it waits for the keys migration (ST-SUP-RC01-OC46).
	built := map[string]bool{}
	for _, table := range after {
		old, there := present[table.name]
		if !there {
			continue
		}
		for _, columns := range table.unique {
			if !slices.ContainsFunc(old.unique, func(u []string) bool { return slices.Equal(u, columns) }) {
				built[table.name+"\x00"+strings.Join(append(append([]string{}, table.lead...), columns...), "\x00")] = true
			}
		}
	}
	out.deferred = func(fk foreignModel) bool { return built[fk.table+"\x00"+strings.Join(fk.to, "\x00")] }
	for _, table := range after {
		for _, fk := range table.foreign {
			if !known[fk.table] {
				return nil, fmt.Errorf("the table %s refers to the table %s, which a later step adds; put the step that adds %s first", table.name, fk.table, fk.table)
			}
		}
		old, there := present[table.name]
		if !there {
			created := table
			created.foreign = slices.DeleteFunc(slices.Clone(table.foreign), out.deferred)
			out.body.WriteString(createTableSQL(created))
			for _, fk := range table.foreign {
				if out.deferred(fk) {
					out.addKey(table, fk)
				}
			}
			if rowSecurity && table.owned {
				out.body.WriteString(TenantRowSecurity(table.entity, key))
			}
			continue
		}
		if err := addedToTable(old, table, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// addedToTable adds to out what takes the table old, which has rows, to the table next: each new column, then each
// new CHECK and foreign key NOT VALID, so that the step scans no row while it holds its lock, and a statement that
// validates each of them later in its own transaction; and each new search index and unique index, built
// CONCURRENTLY by a migration of its own (decision on finding F2). It refuses a change or a removal of what old
// holds, and a new column that is NOT NULL and has no default, which the rows there cannot take.
func addedToTable(old, next tableModel, out *stepAddition) error {
	if old.owned != next.owned || !slices.Equal(old.lead, next.lead) {
		return fmt.Errorf("the step changes the tenant column of the table %s, and an add step only adds", old.name)
	}
	table := "public." + sqlIdentifier(next.name)
	columns := map[string]columnModel{}
	for _, column := range next.columns {
		columns[column.name] = column
	}
	for _, column := range old.columns {
		if got, ok := columns[column.name]; !ok || got != column {
			return fmt.Errorf("the step changes or removes the column %s.%s, and an add step only adds", old.name, column.name)
		}
	}
	for _, column := range next.columns {
		if slices.ContainsFunc(old.columns, func(c columnModel) bool { return c.name == column.name }) {
			continue
		}
		if column.notNull && column.defaultClause == "" {
			return fmt.Errorf("the step adds the column %s.%s as NOT NULL with no default, which the rows there cannot take; make the field optional or give it a default", next.name, column.name)
		}
		fmt.Fprintf(&out.body, "ALTER TABLE %s ADD COLUMN %s;\n", table, columnSQL(column))
	}
	if !subset(old.checks, next.checks) || !subset(old.foreign, next.foreign) || !subset(old.search, next.search) || !subset(old.unique, next.unique) {
		return fmt.Errorf("the step changes or removes a constraint or an index of the table %s, and an add step only adds", old.name)
	}
	validate := func(name string, foreign bool) {
		out.validates = append(out.validates, validation{table: table, schema: "public", tableName: next.name, name: name, foreign: foreign})
	}
	for _, check := range next.checks {
		if !slices.Contains(old.checks, check) {
			fmt.Fprintf(&out.body, "ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s) NOT VALID;\n", table, sqlIdentifier(check.name), check.expression)
			validate(check.name, false)
		}
	}
	for _, fk := range next.foreign {
		if !slices.ContainsFunc(old.foreign, func(f foreignModel) bool { return reflect.DeepEqual(f, fk) }) {
			if out.deferred != nil && out.deferred(fk) {
				out.addKey(next, fk)
				continue
			}
			fmt.Fprintf(&out.body, "ALTER TABLE %s ADD CONSTRAINT %s %s NOT VALID;\n", table, sqlIdentifier(fk.name), foreignSQL(fk))
			validate(fk.name, true)
		}
	}
	for _, column := range next.search {
		if !slices.Contains(old.search, column) {
			out.indexes = append(out.indexes, stepIndex{name: "public." + sqlIdentifier(searchName(next.name, column)), statement: concurrently(searchIndexSQL(next, column)),
				definition: searchIndexDefinition(next, column)})
		}
	}
	for _, columns := range next.unique {
		if !slices.ContainsFunc(old.unique, func(u []string) bool { return slices.Equal(u, columns) }) {
			out.indexes = append(out.indexes, stepIndex{name: "public." + sqlIdentifier(uniqueName(next.name, columns)), statement: concurrently(uniqueIndexSQL(next, columns)),
				definition: uniqueIndexDefinition(next, columns)})
		}
	}
	return nil
}

// concurrently gives the CREATE INDEX or CREATE UNIQUE INDEX statement as CREATE INDEX CONCURRENTLY IF NOT EXISTS, so
// that a rerun after a build that succeeded and wrote no ledger row recovers.
func concurrently(statement string) string {
	for _, head := range []string{"CREATE INDEX ", "CREATE UNIQUE INDEX "} {
		if strings.HasPrefix(statement, head) {
			return head + "CONCURRENTLY IF NOT EXISTS " + strings.TrimPrefix(statement, head)
		}
	}
	return statement
}

// SyncTriggerName gives the name of the sync trigger, and of its function in the schema StepSchema, of the expand
// step of a rename, a retype or a require that adds the column column to the table of the entity:
// <table>_<column>_sync, cut as sqlName cuts a name (ST-SUP-RC01-OC23).
func SyncTriggerName(entity, column string) string { return sqlName(tableName(entity), column, "sync") }

// BackfillLease gives the lease of the backfill of a schema change, which the controller replica that moves its
// rows holds (ST-GEN-UC13-R01).
func BackfillLease(change string) string { return "stego.backfill." + change }

// StepTrigger and StepBackfill give the sync trigger and the backfill lease that the step of a change at expand
// adds, or empty when the kind adds none: a rename and a retype add them, and an add and a require add none
// (ST-GEN-UC12-R01, decision on ST-GEN-Q22 (a), orchestrator under the owner's delegation, 2026-11-14).
func StepTrigger(change types.SchemaChange, phase string) string {
	if (change.Kind == "rename" || change.Kind == "retype") && phase == "expand" {
		return SyncTriggerName(change.Entity, change.Field)
	}
	return ""
}

// StepBackfill gives the backfill lease of the step, or empty.
func StepBackfill(change types.SchemaChange, phase string) string {
	if (change.Kind == "rename" || change.Kind == "retype") && phase == "expand" {
		return BackfillLease(change.ID)
	}
	return ""
}

// syncTemplate is version 2 of the sync function template of the runtime library
// (registry/runtime/manifest.SyncSource, ST-SUP-RP15-LC02): %[1]s is K and %[2]s is U, as quote_ident gives them,
// %[3]s the conversion of NEW.K to the type of U and %[4]s the conversion of NEW.U to the type of K, %[5]s the change
// ID and %[6]s the backfill role, each as a string literal. The compiler does not depend on the runtime module, so
// this is a copy: the release before a step accepts the trigger only when its source is byte for byte the runtime's
// text. TestSyncTemplateCopy compares the two texts, and the generated test of a rename proves the release before
// accepts the trigger with the runtime's Check (ST-GEN-UC12-R03).
const syncTemplate = `
BEGIN
  IF pg_catalog.current_setting('stego.backfill_change', true) = %[5]s AND CURRENT_USER = %[6]s THEN
    RETURN NEW;
  END IF;
  IF pg_catalog.strpos(',' || pg_catalog.current_setting('stego.schema_step', true) || ',', ',' || %[5]s || ',') > 0 THEN
    IF TG_OP = 'INSERT' THEN
      NEW.%[1]s := %[4]s;
    ELSIF NEW.%[1]s IS DISTINCT FROM OLD.%[1]s OR NEW.%[2]s IS DISTINCT FROM OLD.%[2]s THEN
      IF NEW.%[1]s IS DISTINCT FROM OLD.%[1]s AND NEW.%[2]s IS DISTINCT FROM OLD.%[2]s AND NEW.%[1]s IS DISTINCT FROM %[4]s THEN
        RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = 'stego sync trigger: the write sets the old and the new column to values that differ';
      END IF;
      NEW.%[1]s := %[4]s;
    ELSIF NEW.%[2]s IS NULL THEN
      NEW.%[2]s := %[3]s;
    END IF;
  ELSIF TG_OP = 'INSERT' THEN
    IF NEW.%[2]s IS NOT NULL AND NEW.%[2]s IS DISTINCT FROM %[3]s THEN
      RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = 'stego sync trigger: the write sets the old and the new column to values that differ';
    END IF;
    NEW.%[2]s := %[3]s;
  ELSIF NEW.%[1]s IS DISTINCT FROM OLD.%[1]s OR NEW.%[2]s IS DISTINCT FROM OLD.%[2]s OR NEW.%[2]s IS NULL THEN
    IF NEW.%[1]s IS DISTINCT FROM OLD.%[1]s AND NEW.%[2]s IS DISTINCT FROM OLD.%[2]s AND NEW.%[2]s IS DISTINCT FROM %[3]s THEN
      RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = 'stego sync trigger: the write sets the old and the new column to values that differ';
    END IF;
    NEW.%[2]s := %[3]s;
  END IF;
  RETURN NEW;
END;
`

// syncSource gives the source of the sync function of a rename of k to u, with the identity conversion, for the
// change and the backfill role owner, as registry/runtime/manifest.SyncSource gives it for version 2.
func syncSource(change, owner, k, u string) string {
	qk, qu := deparsedName(k), deparsedName(u)
	return fmt.Sprintf(syncTemplate, qk, qu, "NEW."+qk, "NEW."+qu, sqlString(change), sqlString(owner))
}

// syncFunction gives the CREATE FUNCTION statement of the sync function StepSchema.<function>, as
// registry/runtime/manifest.SyncFunction gives it.
func syncFunction(function, source string) string {
	return "CREATE FUNCTION " + deparsedName(StepSchema) + "." + deparsedName(function) +
		"() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER VOLATILE SET search_path = pg_catalog, pg_temp AS $stego_sync$" +
		source + "$stego_sync$"
}

// backfillsTable is the table of the backfill checkpoints of the runtime library
// (registry/runtime/lease.BackfillsTable, ST-SUP-RC01-OC25), one row for each schema change, and backfillFunctions
// are its two functions (registry/runtime/lease.BackfillFunctions); copies, as the template is, which
// TestSyncTemplateCopy compares with the runtime's text.
const backfillsTable = `CREATE TABLE IF NOT EXISTS stego_schema.backfills (
	change_id text PRIMARY KEY,
	last_key bytea,
	last_key_type text,
	rows_moved bigint NOT NULL DEFAULT 0,
	failed_rows bigint NOT NULL DEFAULT 0,
	duplicate_groups bigint NOT NULL DEFAULT 0,
	fill_digest text NOT NULL DEFAULT '',
	token bigint NOT NULL DEFAULT 0,
	completed_at timestamptz
)`

const backfillFunctions = `CREATE OR REPLACE FUNCTION stego_schema.backfill_read(text)
 RETURNS TABLE (last_key bytea, last_key_type text, rows_moved bigint, failed_rows bigint, duplicate_groups bigint, fill_digest text, token bigint, completed boolean)
 LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
 AS $stego_backfill$SELECT b.last_key, b.last_key_type, b.rows_moved, b.failed_rows, b.duplicate_groups, b.fill_digest, b.token, b.completed_at IS NOT NULL
  FROM stego_schema.backfills b WHERE b.change_id = $1 FOR UPDATE$stego_backfill$;
CREATE OR REPLACE FUNCTION stego_schema.backfill_checkpoint(text, bytea, bigint, bigint, bigint, text, boolean, bigint, text, text, text)
 RETURNS bigint LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
 AS $stego_backfill$DECLARE n bigint;
BEGIN
  PERFORM 1 FROM public.leases l WHERE l.name = $10 AND $10 = 'stego.backfill.' || $1 AND l.holder = $11 AND l.token = $8 AND l.expires_at > pg_catalog.clock_timestamp() FOR SHARE;
  IF NOT FOUND THEN
    RETURN 0;
  END IF;
  WITH changed AS (UPDATE stego_schema.backfills b SET last_key = $2, last_key_type = $9, rows_moved = $3, failed_rows = $4,
   duplicate_groups = $5, fill_digest = $6, completed_at = CASE WHEN $7 THEN coalesce(b.completed_at, pg_catalog.now()) END, token = $8
   WHERE b.change_id = $1 AND b.token <= $8 RETURNING 1) SELECT pg_catalog.count(*) INTO n FROM changed;
  RETURN n;
END$stego_backfill$;
REVOKE ALL ON FUNCTION stego_schema.backfill_read(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION stego_schema.backfill_checkpoint(text, bytea, bigint, bigint, bigint, text, boolean, bigint, text, text, text) FROM PUBLIC;
`

// releasesText is the text of the table of the release reports and its two functions of the runtime library
// (registry/runtime/lease.Releases, ST-SUP-RP09-LC14), with %[1]s the runtime role, quoted; a copy, as the template
// is, which TestSyncTemplateCopy compares with the runtime's text. The migration of a rename at expand runs it, so a
// release before the step reports itself from then on, and the contract step can refuse while it runs (decision on
// finding F8 of the schema-change research, owner, 2026-11-13).
const releasesText = `CREATE TABLE IF NOT EXISTS stego_schema.releases (
	holder text PRIMARY KEY,
	release text NOT NULL,
	version text NOT NULL,
	every_ms integer NOT NULL,
	seen_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS releases_seen_at ON stego_schema.releases (seen_at);
REVOKE ALL ON TABLE stego_schema.releases FROM PUBLIC;
CREATE OR REPLACE FUNCTION stego_schema.release_beat(text, text, text, integer)
 RETURNS void LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_release$BEGIN
  IF $1 IS NULL OR $2 IS NULL OR $3 IS NULL OR $4 IS NULL OR $1 !~ '^[0-9a-f]{32}$' OR $2 !~ '^[0-9a-z_-]{1,200}$'
   OR $3 !~ '^[ -~]{0,64}$' OR $4 < 1000 OR $4 > 60000 THEN
    RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'stego release report: an argument is out of range';
  END IF;
  INSERT INTO stego_schema.releases AS r (holder, release, version, every_ms, seen_at)
   VALUES ($1, $2, $3, $4, pg_catalog.clock_timestamp())
   ON CONFLICT (holder) DO UPDATE SET release = EXCLUDED.release, version = EXCLUDED.version, every_ms = EXCLUDED.every_ms,
   seen_at = EXCLUDED.seen_at;
END$stego_release$;
CREATE OR REPLACE FUNCTION stego_schema.release_stop(text)
 RETURNS void LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_release$DELETE FROM stego_schema.releases r WHERE r.holder = $1$stego_release$;
REVOKE ALL ON FUNCTION stego_schema.release_beat(text, text, text, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION stego_schema.release_stop(text) FROM PUBLIC;
GRANT USAGE ON SCHEMA stego_schema TO %[1]s;
GRANT EXECUTE ON FUNCTION stego_schema.release_beat(text, text, text, integer) TO %[1]s;
GRANT EXECUTE ON FUNCTION stego_schema.release_stop(text) TO %[1]s;
`

// renameSQL gives the migrations of the expand step of a rename, and of a retype by the pair of pairOf with its
// conversion (ST-GEN-UC12-R02, ST-GEN-UC12-R03,
// ST-SUP-RC01-OC23, ST-SUP-RC01-OC25): its own migration adds the new column U nullable and with no default, and the
// CHECK of its field after the change NOT VALID, which a later migration validates; it changes nothing that is
// there, so the old column K keeps its type, NOT NULL, CHECK and indexes. It makes the schema StepSchema, the sync
// function of the fixed template with the identity, which the release before accepts by its text
// (ST-SUP-RP15-LC02), and the BEFORE row trigger on INSERT OR UPDATE that runs it. Then it makes the table of the
// backfill checkpoints, its two SECURITY DEFINER functions, through which the backfill alone reads and changes a
// row, with EXECUTE for no role other than the owner, and the row of the change. The search index and the unique indexes of U are built CONCURRENTLY by
// migrations of their own (decision on finding F2, orchestrator under the owner's delegation, 2026-11-13). The
// migration job runs it as the owner role, which owns each object it makes.
func renameSQL(ctx gen.Context, before, after []tableModel, change types.SchemaChange) (*stepAddition, error) {
	runtime, owner, err := serviceRoles(ctx)
	if err != nil {
		return nil, err
	}
	pair, err := pairOf(ctx, change)
	if err != nil {
		return nil, err
	}
	name := tableName(change.Entity)
	old, okOld := findTable(before, name)
	next, okNext := findTable(after, name)
	if !okOld || !okNext {
		return nil, fmt.Errorf("the %s names the entity %s, whose table is not there", change.Kind, change.Entity)
	}
	k, okK := findColumn(old, pair.k)
	u, okU := findColumn(next, pair.u)
	if !okK || !okU {
		return nil, fmt.Errorf("the %s of %s.%s finds no column %s before the step or %s after it", change.Kind, change.Entity, change.Field, pair.k, pair.u)
	}
	if _, clash := findColumn(old, pair.u); clash {
		return nil, fmt.Errorf("the table %s has a column %s already, so the %s cannot add it", name, pair.u, change.Kind)
	}
	// The schema check of the release before accepts the sync trigger for a column K in a unique key only when the
	// key is K alone after the tenant column, and the step gives U the same unique key by CREATE UNIQUE INDEX
	// CONCURRENTLY before the trigger (ST-SUP-RP15-LC02 rule 2, decision on ST-GEN-Q18 (a), orchestrator under the
	// owner's delegation, 2026-11-13). A retype of a field in a unique key is not built: two values of the old type
	// that differ can convert to one value of the new type.
	for _, columns := range old.unique {
		if slices.Contains(columns, pair.k) && (len(columns) != 1 || change.Kind == "retype") {
			return nil, fmt.Errorf("the %s of %s.%s is of a field in a unique key that its sync trigger cannot carry (ST-SUP-RP15-LC02); it is not built", change.Kind, change.Entity, change.Field)
		}
	}
	// A retype that also makes its field unique has no unique change, and two old values can convert to one new value,
	// so its unique index could not be built (security review of PR #335, L2, orchestrator under the owner's
	// delegation, 2026-11-14).
	for _, columns := range next.unique {
		if change.Kind == "retype" && slices.Contains(columns, pair.u) {
			return nil, fmt.Errorf("the retype of %s.%s is of a field that is unique after it, which a retype does not carry; it is not built", change.Entity, change.Field)
		}
	}
	for _, fk := range old.foreign {
		if slices.Contains(fk.from, pair.k) {
			return nil, fmt.Errorf("the %s of %s.%s is of a reference, whose foreign key a %s does not carry yet", change.Kind, change.Entity, change.Field, change.Kind)
		}
	}
	if change.Kind == "rename" && k.typ != u.typ {
		return nil, fmt.Errorf("the rename of %s.%s changes the column type from %s to %s; a rename keeps the type, and a retype changes it", change.Entity, change.From, k.typ, u.typ)
	}
	trigger := SyncTriggerName(change.Entity, change.Field)
	complete := CompleteCheckName(change.Entity, pair.u)
	if !validTriggerName(trigger) || !validTriggerName(complete) {
		return nil, fmt.Errorf("the sync trigger name %s is not a name the schema check accepts", trigger)
	}
	table := "public." + sqlIdentifier(name)
	out := &stepAddition{}
	fmt.Fprintf(&out.body, "ALTER TABLE %s ADD COLUMN %s %s;\n", table, sqlIdentifier(u.name), u.typ)
	for _, check := range next.checks {
		if check.name == sqlName(name, pair.u, "check") {
			fmt.Fprintf(&out.body, "ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s) NOT VALID;\n", table, sqlIdentifier(check.name), check.expression)
			out.validates = append(out.validates, validation{table: table, schema: "public", tableName: name, name: check.name})
		}
	}
	// The sync function, its trigger and the backfill row come in a migration of their own after the indexes, so
	// that U has its unique key, valid, before the trigger moves K into it (ST-SUP-RP15-LC02 rule 2).
	fmt.Fprintf(&out.sync, "CREATE SCHEMA IF NOT EXISTS %s;\n", StepSchema)
	out.sync.WriteString(syncFunction(trigger, syncSourceOf(change.ID, owner, pair)) + ";\n")
	// The completeness CHECK comes NOT VALID in the transaction of the trigger, and before it, so that its ACCESS
	// EXCLUSIVE lock is the first lock of the transaction on the table and no write of the release before lands
	// between the two; it checks no row that is there, and each write after it meets it, which the trigger of
	// version 2 fills. The backfill validates it before it marks the change complete (decision on ST-SUP-Q120 (c),
	// orchestrator under the owner's delegation, 2026-11-14).
	fmt.Fprintf(&out.sync, "ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s) NOT VALID;\n", table, sqlIdentifier(complete), completeExpressionOf(pair))
	fmt.Fprintf(&out.sync, "CREATE TRIGGER %s BEFORE INSERT OR UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION %s.%s();\n",
		sqlIdentifier(trigger), table, StepSchema, sqlIdentifier(trigger))
	// The functions revoke EXECUTE from PUBLIC and grant it to no role: only the owner role, which runs the backfill
	// in the migration job, reads and changes the row (decision on finding M1 of the security review of PR #291,
	// orchestrator under the owner's delegation, 2026-11-14).
	out.sync.WriteString(backfillsTable + ";\n" + backfillFunctions)
	fmt.Fprintf(&out.sync, "INSERT INTO stego_schema.backfills (change_id) VALUES (%s) ON CONFLICT (change_id) DO NOTHING;\n", sqlString(change.ID))
	// The table of the release reports, with EXECUTE on its two functions for the runtime role (ST-SUP-RP09-LC14).
	fmt.Fprintf(&out.sync, releasesText, sqlIdentifier(runtime))
	out.sync.WriteString(sealsAgain(ctx, change.ID, runtime))
	if slices.Contains(next.search, pair.u) {
		out.indexes = append(out.indexes, stepIndex{name: "public." + sqlIdentifier(searchName(next.name, pair.u)),
			statement: concurrently(searchIndexSQL(next, pair.u)), definition: searchIndexDefinition(next, pair.u)})
	}
	for _, columns := range next.unique {
		if slices.Contains(columns, pair.u) {
			out.indexes = append(out.indexes, stepIndex{name: "public." + sqlIdentifier(uniqueName(next.name, columns)),
				statement: concurrently(uniqueIndexSQL(next, columns)), definition: uniqueIndexDefinition(next, columns)})
		}
	}
	return out, nil
}

// contractSQL gives the contract step of a rename or a retype, by the pair of pairOf (ST-GEN-UC15-R02, ST-SUP-RC01-OC24, ST-SUP-RC01-OC39): before is
// the tables of the expand step, with the old column K, and after the tables of the declaration after the step, with
// the new column U alone. A required field makes U NOT NULL with a CHECK (U IS NOT NULL) NOT VALID, which a later
// statement validates, then SET NOT NULL and the drop of the CHECK; then K becomes nullable, so a write of the
// release of the rename, which leaves K out, meets no NOT NULL once the trigger stops filling K; then the step drops
// the sync trigger, after its check again, the completeness CHECK and K with its CHECK and its indexes, all in that
// one transaction, so that no write of the release before changes K alone after the trigger is gone (security review
// of PR #316, H1); then the function of the trigger. Each statement is IF EXISTS or has a query that tells whether it
// still has to run, so a rerun after a failure part way recovers. For a field with a default, the first statement
// gives U the default, which the expand step did not give it so that the backfill finds each row that it has to
// fill, and in the same transaction the comment of a relaxed form that states the default and names the expand
// migration expand: the manifest of the release of the change states no default for U, and its schema check accepts
// the default of such a comment while that release still runs (ST-SUP-RC01-OC39, ST-SUP-RP15-LC04, walkthrough 14
// W14-B3).
func contractSQL(ctx gen.Context, before, after []tableModel, change types.SchemaChange, expand string) (*contractStep, error) {
	pair, err := pairOf(ctx, change)
	if err != nil {
		return nil, err
	}
	name := tableName(change.Entity)
	old, okOld := findTable(before, name)
	next, okNext := findTable(after, name)
	if !okOld || !okNext {
		return nil, fmt.Errorf("the %s names the entity %s, whose table is not there", change.Kind, change.Entity)
	}
	k, okK := findColumn(old, pair.k)
	u, okU := findColumn(next, pair.u)
	if !okK || !okU {
		return nil, fmt.Errorf("the contract of the %s finds no column %s before the step or %s after it", changeText(change), pair.k, pair.u)
	}
	if _, kept := findColumn(next, pair.k); kept {
		return nil, fmt.Errorf("the table %s keeps the column %s after the contract of its %s", name, pair.k, change.Kind)
	}
	if u.defaultClause != "" {
		if !strings.HasPrefix(u.manifestDefault, "'") {
			return nil, fmt.Errorf("the contract of the %s is of a field whose default %s is no constant, which the schema check of the release of the change cannot accept on the new column; it is not built", changeText(change), u.manifestDefault)
		}
		if expand == "" {
			return nil, fmt.Errorf("the contract of the %s has no expand step before it", changeText(change))
		}
	}
	table := "public." + sqlIdentifier(name)
	regclass := sqlString(table) + "::pg_catalog.regclass"
	trigger := SyncTriggerName(change.Entity, change.Field)
	complete := CompleteCheckName(change.Entity, pair.u)
	notNull := NotNullCheckName(change.Entity, pair.u)
	attribute := func(column, test string) string {
		return "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid = " + regclass + " AND a.attname = " + sqlString(column) +
			" AND a.attnum > 0 AND NOT a.attisdropped" + test + ")"
	}
	constraint := "SELECT 1 FROM pg_catalog.pg_constraint c WHERE c.conrelid = " + regclass + " AND c.conname = " + sqlString(notNull)
	out := &contractStep{change: change.ID, table: table, schema: "public", tableName: name, check: complete, definition: completeDefinitionOf(pair), trigger: trigger,
		from: k.name, old: sqlIdentifier(k.name), new: sqlIdentifier(u.name)}
	if old.owned {
		out.tenant, out.tenantSchema = sqlIdentifier(tenantColumn(ctx)), "public"
	}
	add := func(s contractStatement) { out.statements = append(out.statements, s) }
	if u.defaultClause != "" {
		// The comment comes first in the transaction, so no session of the release of the change sees the default
		// without it.
		comment := RelaxComment(expand, "", false, u.manifestDefault, nil)
		add(contractStatement{sql: fmt.Sprintf("COMMENT ON COLUMN %s.%s IS %s", table, sqlIdentifier(u.name), sqlString(comment)),
			when: "SELECT NOT EXISTS (SELECT 1 FROM pg_catalog.pg_attrdef d JOIN pg_catalog.pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum WHERE d.adrelid = " +
				regclass + " AND a.attname = " + sqlString(u.name) + " AND NOT a.attisdropped)",
			also: []string{fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET%s", table, sqlIdentifier(u.name), u.defaultClause)}})
	}
	if u.notNull {
		add(contractStatement{sql: fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IS NOT NULL) NOT VALID", table, sqlIdentifier(notNull), sqlIdentifier(u.name)),
			when: attribute(u.name, " AND NOT a.attnotnull") + " AND NOT EXISTS (" + constraint + ")"})
		add(contractStatement{sql: fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", table, sqlIdentifier(notNull)), when: "SELECT EXISTS (" + constraint + ")", long: true})
		add(contractStatement{sql: fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", table, sqlIdentifier(u.name)), when: attribute(u.name, " AND NOT a.attnotnull")})
		add(contractStatement{sql: fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s", table, sqlIdentifier(notNull))})
	}
	if k.notNull {
		add(contractStatement{sql: fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL", table, sqlIdentifier(k.name)), when: attribute(k.name, " AND a.attnotnull")})
	}
	add(contractStatement{sql: fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s", sqlIdentifier(trigger), table), guard: true,
		also: []string{fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s", table, sqlIdentifier(complete)),
			fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s", table, sqlIdentifier(k.name))}})
	add(contractStatement{sql: fmt.Sprintf("DROP FUNCTION IF EXISTS %s.%s()", StepSchema, sqlIdentifier(trigger))})
	return out, nil
}

// changeText names a rename or a retype as a fault text names it: the rename of Board.title to heading, or the retype
// of Board.revision from string.
func changeText(change types.SchemaChange) string {
	if change.Kind == "retype" {
		return fmt.Sprintf("retype of %s.%s from %s", change.Entity, change.Field, change.From)
	}
	return fmt.Sprintf("%s of %s.%s to %s", change.Kind, change.Entity, change.From, change.Field)
}

// NotNullCheckName gives the name of the CHECK (U IS NOT NULL) that the contract step of a rename of a required field
// adds NOT VALID, validates and drops after SET NOT NULL: <table>_<column>_not_null, cut as sqlName cuts a name.
func NotNullCheckName(entity, column string) string {
	return sqlName(tableName(entity), column, "not_null")
}

// CompleteCheckName gives the name of the completeness CHECK of the expand step of a rename that adds the column
// column to the table of the entity: <table>_<column>_complete, cut as sqlName cuts a name
// (registry/runtime/manifest.CompleteName).
func CompleteCheckName(entity, column string) string {
	return sqlName(tableName(entity), column, "complete")
}

// completeExpression gives the expression of the completeness CHECK of a rename of k to u, with the identity
// conversion, as registry/runtime/manifest.CompleteExpression gives it: u is set wherever k is set.
func completeExpression(k, u string) string {
	return "(" + deparsedName(u) + " IS NOT NULL) OR (" + deparsedName(k) + " IS NULL)"
}

// completeDefinition gives the completeness CHECK of a rename of k to u as pg_get_constraintdef writes it, as
// registry/runtime/manifest.CompleteDefinition gives it.
func completeDefinition(k, u string) string { return "CHECK ((" + completeExpression(k, u) + "))" }

// validTriggerName tells whether a trigger name has 63 bytes at most and no control character.
func validTriggerName(name string) bool {
	return len(name) > 0 && len(name) <= 63 && !strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 })
}

// withColumnOf gives the tables after a rename with the old column of the change, its CHECK and its indexes from
// the tables before, which stay in the database until the contract step.
func withColumnOf(after, before []tableModel, change types.SchemaChange, k string) []tableModel {
	name := tableName(change.Entity)
	old, _ := findTable(before, name)
	out := slices.Clone(after)
	for i := range out {
		if out[i].name != name {
			continue
		}
		t := out[i]
		t.columns = slices.Clone(t.columns)
		if k, ok := findColumn(old, k); ok {
			t.columns = append(t.columns, k)
		}
		for _, check := range old.checks {
			if check.name == sqlName(name, k, "check") {
				t.checks = append(slices.Clone(t.checks), check)
			}
		}
		if slices.Contains(old.search, k) {
			t.search = append(slices.Clone(t.search), k)
		}
		for _, columns := range old.unique {
			if slices.Contains(columns, k) {
				t.unique = append(slices.Clone(t.unique), columns)
			}
		}
		out[i] = t
	}
	return out
}

// findTable gives the table of the name among tables.
func findTable(tables []tableModel, name string) (tableModel, bool) {
	i := slices.IndexFunc(tables, func(t tableModel) bool { return t.name == name })
	if i < 0 {
		return tableModel{}, false
	}
	return tables[i], true
}

// findColumn gives the column of the name in the table.
func findColumn(table tableModel, name string) (columnModel, bool) {
	i := slices.IndexFunc(table.columns, func(c columnModel) bool { return c.name == name })
	if i < 0 {
		return columnModel{}, false
	}
	return table.columns[i], true
}

// subset tells whether each item of old is in new.
func subset[T any](old, new []T) bool {
	for _, item := range old {
		if !slices.ContainsFunc(new, func(n T) bool { return reflect.DeepEqual(n, item) }) {
			return false
		}
	}
	return true
}

// generateSteps gives the SQL file of each step migration, for a reader and for an operator who runs the migrations
// outside the service; migrate.go registers the same text.
func generateSteps(ns string, steps []schemaStep) []gen.File {
	var files []gen.File
	for _, step := range steps {
		files = append(files, gen.File{Path: StepMigrationFile(ns, step.Name), Content: []byte(step.Body)})
	}
	return files
}

// syncConversion is one conversion of the sync template of the runtime library
// (registry/runtime/manifest.SyncConversions): its name, the column types of K and U, each direction with %s for its
// argument, and, for a conversion that can fail, its guard and the guard as pg_get_constraintdef writes it, with
// %[1]s for the column. A copy, as the template is, which TestSyncTemplateCopy compares with the runtime's list.
type syncConversion struct{ name, from, to, forward, back, guard, deparsed string }

// syncConversions is the copy of registry/runtime/manifest.SyncConversions.
var syncConversions = func() []syncConversion {
	list := []syncConversion{
		{name: "identity", forward: "%s", back: "%s"},
		{name: "integer-bigint", from: "integer", to: "bigint", forward: "CAST(%s AS pg_catalog.int8)", back: "CAST(%s AS pg_catalog.int4)"},
		{name: "bigint-integer", from: "bigint", to: "integer", forward: "CAST(%s AS pg_catalog.int4)", back: "CAST(%s AS pg_catalog.int8)",
			guard: "%s BETWEEN -2147483648 AND 2147483647", deparsed: "((%[1]s >= '-2147483648'::integer) AND (%[1]s <= 2147483647))"},
	}
	for _, t := range []struct{ format, catalog string }{
		{"integer", "int4"}, {"bigint", "int8"}, {"numeric", "numeric"}, {"boolean", "bool"}, {"uuid", "uuid"},
	} {
		list = append(list,
			syncConversion{name: t.catalog + "-text", from: t.format, to: "text", forward: "CAST(%s AS pg_catalog.text)", back: "CAST(%s AS pg_catalog." + t.catalog + ")"},
			syncConversion{name: "text-" + t.catalog, from: "text", to: t.format, forward: "CAST(%s AS pg_catalog." + t.catalog + ")", back: "CAST(%s AS pg_catalog.text)",
				guard: "pg_catalog.pg_input_is_valid(%s, 'pg_catalog." + t.catalog + "')", deparsed: "pg_input_is_valid(%[1]s, 'pg_catalog." + t.catalog + "'::text)"})
	}
	return list
}()

// stepPair is what the sync trigger of a step moves: the old column K and the new column U, with the conversion of K
// to U and back, and, for a conversion that can fail, the fill value of the change as the text of a constant, or
// none (NULL).
type stepPair struct {
	k, u    string
	conv    syncConversion
	fill    string
	hasFill bool
}

// retypeColumns gives the old and the new column of a retype: the column of the field before the change, which is
// the column of the retype before it on the field or the field's name, and the column of the type after it, which is
// the type From of the next retype on the field or the field's declared type (decision on ST-GEN-Q23 (a),
// orchestrator under the owner's delegation, 2026-11-14), with the types before and after.
func retypeColumns(ctx gen.Context, change types.SchemaChange) (k, u string, before, after types.FieldType, err error) {
	position := map[string]int{}
	for i, step := range ctx.SchemaHistory {
		if _, seen := position[step.Change]; !seen {
			position[step.Change] = i
		}
	}
	k, before = change.Field, types.FieldType(change.From)
	after = ""
	for _, entity := range ctx.Entities {
		if entity.Name == change.Entity {
			for _, field := range entity.Fields {
				if field.Name == change.Field {
					after = field.Type
				}
			}
		}
	}
	next := -1
	for _, other := range ctx.SchemaChanges {
		if other.ID == change.ID || other.Kind != "retype" || other.Entity != change.Entity || other.Field != change.Field {
			continue
		}
		switch at := position[other.ID]; {
		case at < position[change.ID]:
			k = gen.RetypeColumn(change.Field, types.FieldType(change.From))
		case next < 0 || at < next:
			next, after = at, types.FieldType(other.From)
		}
	}
	if after == "" {
		return "", "", "", "", fmt.Errorf("the retype names the field %s.%s, which the declaration does not have", change.Entity, change.Field)
	}
	return k, gen.RetypeColumn(change.Field, after), before, after, nil
}

// pairOf gives the pair of a rename or a retype: for a rename, K is From and U the declared name, with the identity;
// for a retype, the columns of retypeColumns and the conversion of the template from the column type of K to that of
// U, with the change's fill value where the conversion can fail. It refuses a retype of a column type that the
// template has no conversion for, such as a string with a max_length, and a fill value that the sync function cannot
// hold as a constant.
func pairOf(ctx gen.Context, change types.SchemaChange) (stepPair, error) {
	if change.Kind == "rename" {
		return stepPair{k: change.From, u: change.Field, conv: syncConversions[0]}, nil
	}
	if change.Field == "generation" {
		// The field generation is the version counter that the server keeps (restapi), whose type stays an integer.
		return stepPair{}, fmt.Errorf("the retype of %s.generation is of the version counter that the server keeps; it is not built", change.Entity)
	}
	k, u, beforeType, afterType, err := retypeColumns(ctx, change)
	if err != nil {
		return stepPair{}, err
	}
	before := gen.RetypeField(change)
	before.Type = beforeType
	kType, err := columnType(before)
	if err != nil {
		return stepPair{}, err
	}
	afterField := types.Field{Name: change.Field, Type: afterType}
	for _, entity := range ctx.Entities {
		for _, field := range entity.Fields {
			if entity.Name == change.Entity && field.Name == change.Field && field.Type == afterType {
				afterField = field
			}
		}
	}
	uType, err := columnType(afterField)
	if err != nil {
		return stepPair{}, err
	}
	i := slices.IndexFunc(syncConversions, func(c syncConversion) bool { return c.from == kType && c.to == uType })
	if i < 0 {
		return stepPair{}, fmt.Errorf("the retype of %s.%s from the column type %s to %s has no conversion in the sync template; it is not built", change.Entity, change.Field, kType, uType)
	}
	p := stepPair{k: k, u: u, conv: syncConversions[i]}
	if p.conv.guard != "" && change.FillValue != nil {
		p.fill, p.hasFill = fmt.Sprint(change.FillValue), true
		if !utf8.ValidString(p.fill) || strings.ContainsAny(p.fill, "\\$") || strings.ContainsFunc(p.fill, unicode.IsControl) {
			return stepPair{}, fmt.Errorf("the fill value of the retype %s holds a backslash, a dollar sign or a control character, which the sync function does not take", change.ID)
		}
	}
	return p, nil
}

// forward gives the conversion of the value of K, written as arg, to the type of U, guarded with the fill value or
// NULL where it can fail, as registry/runtime/manifest.SyncSource writes it.
func (p stepPair) forward(arg string) string {
	text := fmt.Sprintf(p.conv.forward, arg)
	if p.conv.guard == "" {
		return text
	}
	fill := "NULL"
	if p.hasFill {
		fill = sqlString(p.fill)
	}
	return "(CASE WHEN " + arg + " IS NULL THEN NULL WHEN " + fmt.Sprintf(p.conv.guard, arg) + " THEN " + text + " ELSE " + fill + " END)"
}

// failGuard gives the guard of a conversion that can fail with no fill value, with arg for K, or empty: a row whose
// K fails it keeps U unset and counts as failed.
func (p stepPair) failGuard(arg string) string {
	if p.conv.guard == "" || p.hasFill {
		return ""
	}
	return fmt.Sprintf(p.conv.guard, arg)
}

// syncSourceOf gives the source of the sync function of the pair, for the change and the backfill role owner, as
// registry/runtime/manifest.SyncSource gives it for version 2.
func syncSourceOf(change, owner string, p stepPair) string {
	qk, qu := deparsedName(p.k), deparsedName(p.u)
	return fmt.Sprintf(syncTemplate, qk, qu, p.forward("NEW."+qk), fmt.Sprintf(p.conv.back, "NEW."+qu), sqlString(change), sqlString(owner))
}

// completeExpressionOf and completeDefinitionOf give the completeness CHECK of the pair, as
// registry/runtime/manifest.CompleteExpression and CompleteDefinition give it.
func completeExpressionOf(p stepPair) string {
	expression := "(" + deparsedName(p.u) + " IS NOT NULL) OR (" + deparsedName(p.k) + " IS NULL)"
	if guard := p.failGuard(deparsedName(p.k)); guard != "" {
		expression += " OR (NOT " + guard + ")"
	}
	return expression
}

func completeDefinitionOf(p stepPair) string {
	definition := "(" + deparsedName(p.u) + " IS NOT NULL) OR (" + deparsedName(p.k) + " IS NULL)"
	if p.failGuard("x") != "" {
		definition += " OR (NOT " + fmt.Sprintf(p.conv.deparsed, deparsedName(p.k)) + ")"
	}
	return "CHECK ((" + definition + "))"
}

// fillDigest gives the digest of the fill value of the pair, which the backfill keeps in its checkpoint row, or empty.
func (p stepPair) fillDigest() string {
	if !p.hasFill {
		return ""
	}
	sum := sha256.Sum256([]byte(p.fill))
	return hex.EncodeToString(sum[:])
}

// requiredAt gives the models of the tables after the first steps steps of the schema history with the column of each
// require whose expand step is among them as the steps leave it (decision on ST-GEN-Q22 (a), orchestrator under the
// owner's delegation, 2026-11-14): with the fill value as its constant DEFAULT, which the step adds with no rewrite of
// the table, so each row there and each insert of the release before, which leaves the column out, reads it; and
// nullable until the contract step of the require is among them, which keeps the default.
func requiredAt(ctx gen.Context, models []tableModel, steps int) ([]tableModel, error) {
	expand, contract := map[string]int{}, map[string]int{}
	for i, step := range ctx.SchemaHistory {
		if step.Phase == "expand" {
			expand[step.Change] = i + 1
		} else {
			contract[step.Change] = i + 1
		}
	}
	out := slices.Clone(models)
	for _, change := range ctx.SchemaChanges {
		if change.Kind != "require" || expand[change.ID] == 0 || expand[change.ID] > steps {
			continue
		}
		for i := range out {
			if out[i].name != tableName(change.Entity) {
				continue
			}
			out[i].columns = slices.Clone(out[i].columns)
			for j := range out[i].columns {
				column := &out[i].columns[j]
				if column.name != change.Field {
					continue
				}
				clause, err := columnDefault(types.Field{Name: change.Field, Default: change.FillValue})
				if err != nil {
					return nil, fmt.Errorf("schema change %s: %w", change.ID, err)
				}
				column.defaultClause, column.manifestDefault = clause, literalDefault(change.FillValue, column.typ)
				if at := contract[change.ID]; at == 0 || at > steps {
					column.notNull = false
				}
			}
		}
	}
	return out, nil
}

// requireContractSQL gives the contract step of a require (ST-GEN-UC15-R02, ST-SUP-RC01-OC39): before is the tables
// of the expand step, with the new column U nullable and with the fill value as its default, and after the tables
// after the step, with U NOT NULL and the default kept. It adds CHECK (U IS NOT NULL) NOT VALID, validates it, runs
// SET NOT NULL, which then scans no row, and drops the CHECK, each statement in its own short transaction and each
// with a query that tells whether it still has to run. A require has no sync trigger and no backfill, so its check
// has nothing to wait for: the default gave each row the fill value (decision on ST-GEN-Q22 (a), orchestrator under
// the owner's delegation, 2026-11-14).
func requireContractSQL(before, after []tableModel, change types.SchemaChange) (*contractStep, error) {
	name := tableName(change.Entity)
	old, okOld := findTable(before, name)
	next, okNext := findTable(after, name)
	if !okOld || !okNext {
		return nil, fmt.Errorf("the require names the entity %s, whose table is not there", change.Entity)
	}
	was, okWas := findColumn(old, change.Field)
	u, okU := findColumn(next, change.Field)
	if !okWas || !okU || was.notNull || !u.notNull || was.defaultClause == "" || u.defaultClause != was.defaultClause {
		return nil, fmt.Errorf("the contract of the require of %s.%s finds no nullable column %s with its fill value before the step, NOT NULL with it after", change.Entity, change.Field, change.Field)
	}
	table := "public." + sqlIdentifier(name)
	regclass := sqlString(table) + "::pg_catalog.regclass"
	notNull := NotNullCheckName(change.Entity, change.Field)
	nullable := "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid = " + regclass + " AND a.attname = " + sqlString(u.name) +
		" AND a.attnum > 0 AND NOT a.attisdropped AND NOT a.attnotnull)"
	constraint := "SELECT 1 FROM pg_catalog.pg_constraint c WHERE c.conrelid = " + regclass + " AND c.conname = " + sqlString(notNull)
	out := &contractStep{change: change.ID, table: table, schema: "public", tableName: name}
	out.statements = []contractStatement{
		{sql: fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IS NOT NULL) NOT VALID", table, sqlIdentifier(notNull), sqlIdentifier(u.name)),
			when: nullable + " AND NOT EXISTS (" + constraint + ")"},
		{sql: fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", table, sqlIdentifier(notNull)), when: "SELECT EXISTS (" + constraint + ")", long: true},
		{sql: fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", table, sqlIdentifier(u.name)), when: nullable},
		{sql: fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s", table, sqlIdentifier(notNull))},
	}
	return out, nil
}
