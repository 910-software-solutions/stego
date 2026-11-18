// Package postgresadapter implements the postgres-adapter component Generator. It gives the database opener of a
// service and, for a service with entities, the tables of the runtime data path as SQL from the declaration, the
// migration ledger that applies them and the schema generation marker (tables.go, ST-SUP-RC01-OC41). It emits no
// store and no entity model: the generated commands and queries carry all data access, on the only data path
// (ST-SUP-RC20-OC39).
package postgresadapter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// validFieldNamePattern defines the safe character set for field names across
// all target systems (Go identifiers, SQL identifiers, URL segments).
var validFieldNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Generator produces the postgres-adapter component's generated code.
type Generator struct{}

// MinimumGoVersion covers the compiler minimum for golang.org/x/text.
func (*Generator) MinimumGoVersion() string { return "1.25.0" }

// ValidateContext checks storage names, constraints, and migration settings, with the checks of Generate.
func (*Generator) ValidateContext(ctx gen.Context) error {
	if err := validateTenantColumn(ctx); err != nil {
		return err
	}
	if _, err := schemaGeneration(ctx); err != nil {
		return err
	}
	if err := validateWriterLease(ctx); err != nil {
		return err
	}
	if peer := ctx.PeerNamespaces["otel-tracing"]; peer != "" {
		if err := gen.ValidateGoPackageNamespace(peer); err != nil {
			return err
		}
	}
	if errs := types.ValidateVersioned(ctx.Entities, ctx.Collections); len(errs) > 0 {
		return errs[0]
	}
	if errs := types.ValidateLiveUnique(ctx.Entities, ctx.Collections); len(errs) > 0 {
		return errs[0]
	}
	migrations := "startup"
	if value, present := ctx.ComponentConfig["migrations"]; present {
		var ok bool
		migrations, ok = value.(string)
		if !ok || (migrations != "startup" && migrations != "external") {
			return fmt.Errorf("migrations must be startup or external")
		}
	}
	if len(ctx.Entities) == 0 {
		return gen.ValidateGoPackageNamespace(ctx.OutputNamespace)
	}
	// Validate no duplicate entity names.
	if err := validateEntityUniqueness(ctx.Entities); err != nil {
		return err
	}

	// Validate no case-insensitive entity name collisions (would produce
	// the same table name).
	if err := validateCaseInsensitiveUniqueness(ctx.Entities); err != nil {
		return err
	}

	// Validate entity names don't collide with generator-internal identifiers.
	if err := validateReservedNames(ctx.Entities); err != nil {
		return err
	}

	// Validate that ref fields' "to" attributes reference existing entities.
	if err := validateRefTargets(ctx.Entities); err != nil {
		return err
	}

	// Validate no duplicate field names within any entity.
	if err := validateFieldUniqueness(ctx.Entities); err != nil {
		return err
	}

	// Validate entity names use a safe character set for all target systems.
	if err := validateEntityNameCharset(ctx.Entities); err != nil {
		return err
	}

	// Validate field names use a safe character set for all target systems.
	if err := validateFieldNameCharset(ctx.Entities); err != nil {
		return err
	}

	// Validate no field name collides with the implicit "id" primary key.
	if err := validateNoImplicitIDCollision(ctx.Entities); err != nil {
		return err
	}

	// Validate derived PascalCase field names are unique within each entity.
	if err := validateDerivedFieldUniqueness(ctx.Entities); err != nil {
		return err
	}

	// Validate derived entity names are valid, usable Go type names.
	if err := validateDerivedEntityValidity(ctx.Entities); err != nil {
		return err
	}

	// Validate derived PascalCase field names are valid Go identifiers.
	if err := validateDerivedFieldValidity(ctx.Entities); err != nil {
		return err
	}

	// Validate that enum fields have non-empty values.
	if err := validateEnumValues(ctx.Entities); err != nil {
		return err
	}

	for _, entity := range ctx.Entities {
		for _, field := range entity.Fields {
			if field.Min == nil && field.Max == nil {
				continue
			}
			switch field.Type {
			case types.FieldTypeInt32, types.FieldTypeInt64, types.FieldTypeFloat, types.FieldTypeDouble:
			default:
				return fmt.Errorf("entity %s field %s: numeric bounds require a numeric type", entity.Name, field.Name)
			}
			for _, bound := range []*float64{field.Min, field.Max} {
				if bound != nil && (math.IsNaN(*bound) || math.IsInf(*bound, 0)) {
					return fmt.Errorf("entity %s field %s: numeric bounds must be finite", entity.Name, field.Name)
				}
			}
			if field.Min != nil && field.Max != nil && *field.Min > *field.Max {
				return fmt.Errorf("entity %s field %s: numeric range is empty", entity.Name, field.Name)
			}
		}
	}
	if _, err := tableOrder(ctx.Entities); err != nil {
		return err
	}
	if err := validateRuntimeData(ctx); err != nil {
		return err
	}

	return gen.ValidateGoPackageNamespace(ctx.OutputNamespace)
}

// Generate gives the files and the wiring of the component. A service with entities gets the tables of the runtime
// data path (generateRuntimeData, ST-SUP-RC01-OC41), which is the only data path (ST-SUP-RC20-OC39); a service with
// no entity has no data access and gets the database opener alone.
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	if len(ctx.Entities) == 0 {
		file, err := generateDatabaseOpener(ctx)
		if err != nil {
			return nil, nil, err
		}
		wiring := &gen.Wiring{NeedsDB: true, Imports: []string{ctx.OutputNamespace}, DatabaseOpener: &gen.DatabaseOpenerSpec{Namespace: ctx.OutputNamespace, Function: "OpenDatabase"}, GoModRequires: map[string]string{"github.com/jackc/pgx/v5": "v5.11.0"}}
		return []gen.File{file}, wiring, nil
	}
	return generateRuntimeData(ctx)
}

// entityAccess adds the table of each entity to the database access that the runtime role is granted and to the
// backup manifest. The runtime role holds only the privileges of ST-SUP-RC27-OC07 and ST-STD-TXN-R03.
func entityAccess(ctx gen.Context, wiring *gen.Wiring) {
	for _, entity := range ctx.Entities {
		wiring.DatabaseAccess = append(wiring.DatabaseAccess, gen.DatabaseObject{
			Schema: "public", Name: tableName(entity.Name), Kind: "table",
			Privileges: runtimePrivileges(ctx, entity),
		})
		wiring.BackupObjects = append(wiring.BackupObjects, gen.BackupObject{
			Schema: "public", Name: tableName(entity.Name), Kind: "table",
		})
	}
}

// schemaGenerationAccess adds the objects of the schema generation marker and of the migration ledger to the
// database access and to the backup manifest, when the service declares schema_generation: SELECT on the marker and
// on the ledger, which the runtime role reads at start.
func schemaGenerationAccess(ctx gen.Context, wiring *gen.Wiring) {
	if ctx.ComponentConfig["schema_generation"] == nil {
		return
	}
	wiring.DatabaseAccess = append(wiring.DatabaseAccess, gen.DatabaseObject{
		Schema: "stego_schema", Name: "generation", Kind: "table", Privileges: []string{"SELECT"},
	})
	wiring.DatabaseAccess = append(wiring.DatabaseAccess, gen.DatabaseObject{
		Schema: "stego_schema", Name: "identity", Kind: "table", Privileges: []string{"SELECT"},
	})
	wiring.DatabaseAccess = append(wiring.DatabaseAccess, gen.DatabaseObject{
		Schema: "stego_schema", Name: "epoch_seq", Kind: "sequence", Privileges: []string{"SELECT", "USAGE"},
	})
	// The runtime role reads the migration ledger at start, and refuses a database that does not hold the last
	// migration of the release (ST-SUP-RP14-LC07). The ledger shape allows SELECT for a role other than its owner.
	wiring.DatabaseAccess = append(wiring.DatabaseAccess, gen.DatabaseObject{
		Schema: "stego_schema", Name: "migrations", Kind: "table", Privileges: []string{"SELECT"},
	})
	wiring.BackupObjects = append(wiring.BackupObjects,
		gen.BackupObject{Schema: "stego_schema", Name: "generation", Kind: "table"},
		gen.BackupObject{Schema: "stego_schema", Name: "identity", Kind: "table"},
		gen.BackupObject{Schema: "stego_schema", Name: "epoch_seq", Kind: "sequence"},
		gen.BackupObject{Schema: "stego_schema", Name: "migrations", Kind: "table"},
	)
}

// reservedTypeNames is the union of: (1) Go keywords, (2) Go predeclared
// identifiers, and (3) generator-internal identifiers. Entity names that
// match any of these produce uncompilable or shadowed generated code.
var reservedTypeNames = map[string]bool{
	"SchemaGeneration": true, "SchemaDefinition": true, "schemaDefinitionBefore": true, "ErrSchemaGeneration": true, "VerifySchema": true, "BootstrapSchema": true, "readSchemaGeneration": true,
	"databaseUUID":       true,
	"ErrMigrationLedger": true, "AppliedMigrations": true, "ApplyMigration": true, "RegisterSQL": true,
	"Database": true, "inTransaction": true, "lockLedger": true, "recordMigration": true,
	"refreshMarker": true, "RegisterStep": true, "RegisterValidateStep": true, "ValidateConstraint": true, "runtimetenant": true, "RegisterIndexStep": true, "applyPending": true,
	"applyIndex": true, "stepTimeouts": true, "lockTimedOut": true, "pause": true, "sessionOf": true, "dropInvalid": true,
	"dropAfterFailure": true, "indexBuilt": true, "dropAttempts": true,
	"ContractStep": true, "ContractStatement": true, "RegisterContractStep": true, "applyContract": true, "checkContract": true, "checkSeal": true,
	"contractStatementRun": true, "violatesCheck": true, "contractRefusal": true, "keyRefusal": true, "digestRefusal": true, "MigrationDigests": true, "rowsLeft": true, "runningReleases": true, "releaseWindow": true,
	"SchemaRelease": true, "SchemaSteps": true, "BackfillCopies": true,
	// Generator-internal identifiers.
	"stegotenancy":             true,
	"OpenDatabase":             true,
	"databaseConnector":        true,
	"databaseLoopbackTestMode": true,
	"databaseTransport":        true,
	"tls":                      true,
	"net":                      true,
	"driver":                   true,
	"databasePoolSettings":     true,
	"readDatabasePoolSettings": true,
	"databasePoolInteger":      true,
	"databasePoolDuration":     true,
	"os":                       true,
	"strconv":                  true,
	"databaseConfiguration":    true,
	"databaseWithoutHomeFiles": true,
	"strings":                  true,
	"databaseTracer":           true,
	"databaseCallKey":          true,
	"databaseCall":             true,
	"beginDatabaseCall":        true,
	"endDatabaseCall":          true,
	"pgx":                      true,
	"stdlib":                   true,
	"telemetry":                true,
	"context":                  true,
	"errors":                   true,

	"Migrate": true,
	// Go keywords.
	"break": true, "case": true, "chan": true, "const": true,
	"continue": true, "default": true, "defer": true, "else": true,
	"fallthrough": true, "for": true, "func": true, "go": true,
	"goto": true, "if": true, "import": true, "interface": true,
	"map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true,
	"var": true,
	// Go predeclared identifiers.
	"any": true, "bool": true, "byte": true, "comparable": true,
	"complex64": true, "complex128": true, "error": true, "float32": true,
	"float64": true, "int": true, "int8": true, "int16": true,
	"int32": true, "int64": true, "rune": true, "string": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true,
	"uint64": true, "uintptr": true,
	"true": true, "false": true, "iota": true, "nil": true,
	"append": true, "cap": true, "clear": true, "close": true,
	"complex": true, "copy": true, "delete": true, "imag": true,
	"len": true, "make": true, "max": true, "min": true,
	"new": true, "panic": true, "print": true, "println": true,
	"real": true, "recover": true,
	// Import aliases used in generated files.
	"fmt":  true,
	"sql":  true,
	"time": true,
	"uuid": true,
}

// --- Migration ---

// migrationLedger is the source of the migration ledger in migrate.go, after its imports. It runs on database/sql
// (Database of schema_generation.go): each migration and its ledger row commit in one transaction, under the ledger
// lock, after a read of the ledger in that transaction, so a migration applies once and in the order of its name
// (ST-SUP-RC01-OC11).
const migrationLedger = `// MigrationFunc is a function that performs a database migration. It runs in the
// transaction that records the migration in the ledger, and must not commit it.
type MigrationFunc func(ctx context.Context, tx *sql.Tx) error

// Migration represents a named database migration. The digest covers the
// migration body; an edited migration changes the digest and is rejected.
// A migration of a schema step records its compatible_with set: the
// migrations that code must register to run on a schema that holds its row
// (ST-SUP-RC01-OC27). Step tells that the set is recorded; a row with no set
// is an exact match.
//
// A migration of a schema step runs under the lock timeout of the step, and
// under its statement timeout: the short one of a step that takes a strong
// lock, or the long one of a VALIDATE or an index build, which take only a
// weak lock (ST-SUP-RC01-OC22). Index, for the build of one index of a step
// on a table with rows, is the qualified name of the index: the migration
// runs CREATE INDEX CONCURRENTLY outside a transaction (ST-SUP-RC01-OC38), and
// Definition is the definition of that index as pg_get_indexdef gives it.
//
// Contract, for the contract step of a schema change, is what the step
// checks and runs, each statement in its own short transaction outside the
// transaction of the ledger row (applyContract, ST-SUP-RC01-OC24,
// ST-SUP-RC01-OC39).
type Migration struct {
	Name             string
	Digest           string
	Func             MigrationFunc
	Step             bool
	CompatibleWith   []string
	StatementTimeout string
	Index            string
	Definition       string
	Body             string
	Contract         *ContractStep
}

// ContractStep is what the contract step of a rename checks before it
// changes the schema, and its statements in order: the change, the schema
// and the name of the table with no quotes, the completeness CHECK, its
// definition as pg_get_constraintdef gives it and the sync trigger of its
// expand step, the expand migration, the name of the old column, the old and
// the new column as quoted identifiers, and, for an owned table, the tenant
// column and the schema of the tenant list stego_tenants with no quotes, by
// which the check counts the rows left of each tenant. Each statement names
// the table and the tenant list with their schema (ST-STD-TXN-R19).
//
// Drop, for the contract step of a drop, tells that the check is the
// running releases alone while the old column From, or with no From the
// table, is there: a drop has no sync trigger and no backfill
// (ST-SUP-RC01-OC44, ST-SUP-RC01-OC47).
//
// Seal, for the contract step of a seal, tells that the check, while the
// clear column From is there, is the seal checkpoint of the change, the rows
// of the listed tenants with Old set and New empty, and the running releases
// (ST-SUP-RC01-OC50).
type ContractStep struct {
	Change, Schema, Table, Check, Definition, Trigger, Expand string
	From, Old, New, TenantColumn, TenantSchema                string
	Statements                                                []ContractStatement
	Drop, Seal                                                bool
}

// ContractStatement is one statement of a contract step: SQL; When, a query
// that gives whether the statement still has to run, or empty; Long for the
// long statement timeout of a VALIDATE; Guard for the check of the step again
// in the same transaction before the statement; and Also, the statements that
// run after SQL in the same transaction, so that the drop of the sync trigger
// and the drop of the old column commit together (security review of PR #316,
// H1).
type ContractStatement struct {
	SQL, When   string
	Long, Guard bool
	Also        []string
}

// releaseWindow is how many report intervals a release report stays
// current (registry/runtime/lease.ReleaseWindow).
const releaseWindow = 3

// The limits of a migration of a schema step (ST-SUP-RC01-OC22): it waits at
// most stepLockTimeout for a lock, so the statements that queue behind it wait
// no longer, and it runs again after a pause when the wait ends with SQLSTATE
// 55P03, at most stepAttempts times. A step that takes a strong lock runs for
// at most stepStatementTimeout; a VALIDATE and an index build, which take a
// lock that blocks no read and no write, for at most longStatementTimeout.
// CREATE INDEX CONCURRENTLY waits for each transaction older than its
// snapshots, so it runs with no lock timeout (applyIndex). The drop of the
// invalid index that a failed build left runs again after a pause, at most
// dropAttempts times.
const (
	stepLockTimeout      = "1s"
	stepStatementTimeout = "15s"
	longStatementTimeout = "1h"
	stepAttempts         = 10
	dropAttempts         = 5
)

var migrations []Migration

// registered makes registerGenerated run once, at the first read of the migrations, so the generated migrations
// are registered by a call in a fixed order and not by package initialization (ST-GEN-INV14).
var registered sync.Once

// Digest computes the ledger digest of a migration body. The body is the
// SQL text of a SQL migration and the generated source text of a function
// migration.
func Digest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// Register adds a function migration to the ordered migration list. The
// digest is Digest of the migration's source text; STEGO computes it for
// each migration it generates. Migration names must sort in apply order;
// use zero-padded number prefixes.
func Register(name, digest string, fn MigrationFunc) {
	migrations = append(migrations, Migration{Name: name, Digest: digest, Func: fn})
}

// RegisterSQL adds a SQL migration to the ordered migration list. The
// digest covers the SQL text, so an edit after application is detected.
func RegisterSQL(name, body string) {
	migrations = append(migrations, Migration{Name: name, Digest: Digest(body), Func: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, body)
		return err
	}})
}

// RegisterStep adds the SQL migration of a schema step with its compatible_with
// set: none for a step at expand, and the migration of the expand step of its
// change for a step at contract (ST-SUP-RC01-OC27). It runs under the short
// statement timeout of a step that takes a strong lock.
func RegisterStep(name, body string, compatibleWith ...string) {
	migrations = append(migrations, Migration{Name: name, Digest: Digest(body), Step: true, CompatibleWith: append([]string{}, compatibleWith...), StatementTimeout: stepStatementTimeout, Body: body, Func: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, body)
		return err
	}})
}

// ValidateConstraint is one constraint that a validate migration validates:
// the schema and the name of the table and the name of the constraint, each
// with no quotes, and Foreign for a foreign key.
type ValidateConstraint struct {
	Schema, Table, Name string
	Foreign             bool
}

// quoteIdentifier quotes one SQL name, so a statement names a table with its
// schema as "schema"."table" (ST-STD-TXN-R19).
func quoteIdentifier(name string) string {
	return "\"" + strings.ReplaceAll(name, "\"", "\"\"") + "\""
}

// RegisterValidateStep adds the migration that validates the constraints that
// the step before it added NOT VALID, in its own transaction, under the long
// statement timeout: VALIDATE CONSTRAINT takes a lock that blocks no read and
// no write (ST-SUP-RC01-OC22). body is the text of the migration, one ALTER
// TABLE ... VALIDATE CONSTRAINT for each of constraints in their order, which
// the ledger digest covers. The migration validates a foreign key with the
// helper of the tenant runtime, which checks each row past row-level security
// (ST-SUP-RP04-LC10; security review of PR #320, info), and each other
// constraint with its statement of body.
func RegisterValidateStep(name, body string, constraints ...ValidateConstraint) {
	migrations = append(migrations, Migration{Name: name, Digest: Digest(body), Step: true, CompatibleWith: []string{}, StatementTimeout: longStatementTimeout, Body: body, Func: func(ctx context.Context, tx *sql.Tx) error {
		for _, c := range constraints {
			table := quoteIdentifier(c.Schema) + "." + quoteIdentifier(c.Table)
			if c.Foreign {
				if err := runtimetenant.ValidateForeignKey(ctx, tx, table, c.Name); err != nil {
					// A row that the key refuses stops the migration job with a refusal that names the step, the
					// table and the key, and no row value (ST-SUP-RC01-OC46).
					var coded interface{ SQLState() string }
					if errors.As(err, &coded) && coded.SQLState() == "23503" {
						return &keyRefusal{text: fmt.Sprintf("the step %s does not validate the foreign key %s of %s.%s: a row of the table breaks it; correct or remove the rows that break it, then run the migration job again", name, c.Name, c.Schema, c.Table), err: err}
					}
					return err
				}
				continue
			}
			if _, err := tx.ExecContext(ctx, "ALTER TABLE "+table+" VALIDATE CONSTRAINT "+quoteIdentifier(c.Name)); err != nil {
				return err
			}
		}
		return nil
	}})
}

// RegisterIndexStep adds the migration that builds the index index of a step on
// a table with rows by its body, CREATE INDEX CONCURRENTLY, outside a
// transaction (ST-SUP-RC01-OC38). A new database applies every migration in
// one transaction on tables with no row, so there it builds the index by the
// same statement without CONCURRENTLY. Definition is the definition of the
// index as pg_get_indexdef gives it: IF NOT EXISTS builds nothing when a
// relation of the name is there, so each path checks the index before its
// ledger row (indexBuilt).
func RegisterIndexStep(name, index, body, definition string) {
	plain := strings.Replace(body, " CONCURRENTLY", "", 1)
	migrations = append(migrations, Migration{Name: name, Digest: Digest(body), Step: true, CompatibleWith: []string{}, StatementTimeout: longStatementTimeout, Index: index, Definition: definition, Body: body, Func: func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, plain); err != nil {
			return err
		}
		return indexBuilt(ctx, tx, index, definition)
	}})
}

// RegisterContractStep adds the contract migration of a schema change, whose
// compatible_with set is the expand migration of its change
// (ST-SUP-RC01-OC27). A new database applies every migration in one
// transaction on tables with no row and with no release running, so there it
// runs each statement that still has to run, with no check.
func RegisterContractStep(name, body string, step ContractStep) {
	migrations = append(migrations, Migration{Name: name, Digest: Digest(body), Step: true, CompatibleWith: []string{step.Expand}, StatementTimeout: stepStatementTimeout, Body: body, Contract: &step, Func: func(ctx context.Context, tx *sql.Tx) error {
		for _, s := range step.Statements {
			if s.When != "" {
				var run bool
				if err := tx.QueryRowContext(ctx, s.When).Scan(&run); err != nil {
					return err
				}
				if !run {
					continue
				}
			}
			for _, statement := range append([]string{s.SQL}, s.Also...) {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
		}
		return nil
	}})
}

// indexBuilt checks that the index index is there, valid and ready, and has
// the definition definition as pg_get_indexdef gives it. CREATE INDEX IF NOT
// EXISTS builds nothing when a relation of the name is there, so a table, or
// an index of another definition, of that name fails the migration before its
// ledger row (ST-SUP-RC01-OC38).
func indexBuilt(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, index, definition string) error {
	var got string
	var valid bool
	err := q.QueryRowContext(ctx, "SELECT pg_catalog.pg_get_indexdef(i.indexrelid), i.indisvalid AND i.indisready FROM pg_catalog.pg_index i WHERE i.indexrelid = pg_catalog.to_regclass($1)", index).Scan(&got, &valid)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("the index %s is not there after its build: a relation of that name that is no index holds the name", index)
	case err != nil:
		return err
	case !valid:
		return fmt.Errorf("the index %s is not valid and ready after its build", index)
	case got != definition:
		return fmt.Errorf("the index %s is %q, and the step builds %q", index, got, definition)
	}
	return nil
}

// refreshMarker rewrites, in the transaction of the migration job that
// holds the ledger lock and whose ledger check passed, the marker digest that
// a release before ST-SUP-RC01-OC42 wrote, the digest of the entities and the
// collections of the baseline, to the digest of the stored shape. The marker
// check of this release accepts both; a later release whose collections or
// permissions differ accepts only the digest of the stored shape
// (ST-SUP-RC01-OC42). A job that fails its ledger check leaves the marker as
// it is.
func refreshMarker(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, "UPDATE stego_schema.generation SET definition = $1 WHERE singleton AND generation = $2 AND definition = $3", SchemaDefinition, SchemaGeneration, schemaDefinitionBefore)
	return err
}

// addCompatibleWith adds the nullable column compatible_with to the ledger
// under the ledger lock, before the migration job reads the ledger, and
// writes no value into the rows already there; it writes no ledger row
// (ST-SUP-RC01-OC29).
func addCompatibleWith(ctx context.Context, db Database) error {
	return inTransaction(ctx, db, &sql.TxOptions{Isolation: sql.LevelReadCommitted}, func(tx *sql.Tx) error {
		if err := lockLedger(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "ALTER TABLE stego_schema.migrations ADD COLUMN IF NOT EXISTS compatible_with pg_catalog.text[]")
		return err
	})
}

// Migrate applies every registered migration and records each one in the
// stego_schema.migrations ledger. Applied versions must match the
// registered history exactly: a re-application, a version gap, an edited
// migration, and an unreadable ledger are all rejected. A fresh database
// applies all migrations inside the bootstrap transaction; afterwards
// each pending migration commits with its ledger row in one transaction.
func Migrate(ctx context.Context, db Database) error {
	if ctx == nil || db == nil {
		return fmt.Errorf("migration requires an initialized database")
	}
	ordered := orderedMigrations()
	if err := BootstrapSchema(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		for _, m := range ordered {
			if err := m.Func(ctx, tx); err != nil {
				return fmt.Errorf("migration %s: %w", m.Name, err)
			}
			if err := recordMigration(ctx, tx, m); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := addCompatibleWith(ctx, db); err != nil {
		return err
	}
	return applyPending(ctx, db, ordered)
}

// ApplyMigration applies one SQL migration by body and records it in the
// ledger, for databases whose migrations run outside the application. The
// same continuity rules hold: no re-application, no gap, no digest change.
// The body runs with the ledger row in one transaction, so it must hold no
// transaction control (BEGIN, COMMIT, ROLLBACK, SAVEPOINT): a COMMIT ends the
// transaction early and leaves a partial migration with no ledger row. This
// is a stated limit; the body is not checked. A release must register each
// external migration before the next one runs: the ledger then holds one row
// past the registered history, so a later ApplyMigration and Migrate are
// refused until a release registers it (ST-SUP-Q113).
func ApplyMigration(ctx context.Context, db Database, name, body string) error {
	if ctx == nil || db == nil {
		return ErrMigrationLedger
	}
	ordered := orderedMigrations()
	for _, m := range ordered {
		if m.Name == name {
			return ErrMigrationLedger
		}
	}
	return inTransaction(ctx, db, &sql.TxOptions{Isolation: sql.LevelReadCommitted}, func(tx *sql.Tx) error {
		if err := lockLedger(ctx, tx); err != nil {
			return err
		}
		applied, err := appliedMigrations(ctx, tx)
		if err != nil {
			return err
		}
		if err := checkLedger(applied, ordered); err != nil {
			return err
		}
		if len(applied) != len(ordered) {
			return ErrMigrationLedger
		}
		if len(applied) > 0 && name <= applied[len(applied)-1].Name {
			return ErrMigrationLedger
		}
		if _, err := tx.ExecContext(ctx, body); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		return recordMigration(ctx, tx, Migration{Name: name, Digest: Digest(body)})
	})
}

// orderedMigrations returns the registered migrations sorted by name,
// after it registers the generated migrations once. The apply order is
// the name order, so names must sort in dependency order.
func orderedMigrations() []Migration {
	registered.Do(registerGenerated)
	ordered := make([]Migration, len(migrations))
	copy(ordered, migrations)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	return ordered
}

// MigrationDigests gives the ledger digest of each migration of this release
// by name, which the service runtime compares with the ledger at each start
// (ST-SUP-RP14-LC13).
func MigrationDigests() map[string]string {
	digests := map[string]string{}
	for _, m := range orderedMigrations() {
		digests[m.Name] = m.Digest
	}
	return digests
}

// recordMigration writes the ledger row of one migration in its transaction,
// with the compatible_with set of a schema step, as a text array literal, and
// no set for every other migration (ST-SUP-RC01-OC27).
func recordMigration(ctx context.Context, tx *sql.Tx, m Migration) error {
	var set any
	if m.Step {
		set = arrayLiteral(m.CompatibleWith)
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO stego_schema.migrations(version,digest,compatible_with) VALUES($1,$2,$3::pg_catalog.text[])", m.Name, m.Digest, set)
	return err
}

// arrayLiteral writes migration names as a PostgreSQL text array literal.
// A migration name holds no quote, backslash, comma or brace.
func arrayLiteral(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = "\"" + name + "\""
	}
	return "{" + strings.Join(quoted, ",") + "}"
}

// appliedMigrations reads the ledger in apply order: the byte order of the
// version, as orderedMigrations sorts the names, whatever the collation of
// the database. It fails closed when the ledger is missing or unreadable.
// A ledger that has no column compatible_with yet, which a release before
// it wrote, holds rows with no set (ST-SUP-RC01-OC29).
func appliedMigrations(ctx context.Context, tx *sql.Tx) ([]Migration, error) {
	var column bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_attribute WHERE attrelid = 'stego_schema.migrations'::pg_catalog.regclass AND attname = 'compatible_with' AND attnum > 0 AND NOT attisdropped)").Scan(&column); err != nil {
		return nil, ErrMigrationLedger
	}
	query := "SELECT version, digest, NULL::pg_catalog.text[] FROM stego_schema.migrations ORDER BY version COLLATE \"C\""
	if column {
		query = "SELECT version, digest, compatible_with FROM stego_schema.migrations ORDER BY version COLLATE \"C\""
	}
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, ErrMigrationLedger
	}
	defer rows.Close()
	var applied []Migration
	for rows.Next() {
		var row Migration
		var set sql.NullString
		if err := rows.Scan(&row.Name, &row.Digest, &set); err != nil {
			return nil, ErrMigrationLedger
		}
		if set.Valid {
			row.Step = true
			names, err := parseArray(set.String)
			if err != nil {
				return nil, ErrMigrationLedger
			}
			row.CompatibleWith = names
		}
		applied = append(applied, row)
	}
	if rows.Err() != nil {
		return nil, ErrMigrationLedger
	}
	return applied, nil
}

// parseArray reads a text array that recordMigration wrote: {} or
// {"a","b"}, each name in double quotes with no quote, backslash or comma.
func parseArray(text string) ([]string, error) {
	if text == "{}" {
		return []string{}, nil
	}
	if len(text) < 2 || text[0] != '{' || text[len(text)-1] != '}' {
		return nil, ErrMigrationLedger
	}
	var names []string
	for _, part := range strings.Split(text[1:len(text)-1], ",") {
		name := strings.Trim(part, "\"")
		if name == "" || strings.ContainsAny(name, "\"\\{} ") {
			return nil, ErrMigrationLedger
		}
		names = append(names, name)
	}
	return names, nil
}

// checkLedger rejects any mismatch between the applied versions and the
// registered history. Applied versions must start with the registered list,
// or be a prefix of it, in order, with matching digests: no gaps and no
// edits. A row after the registered history is accepted only when it has a
// compatible_with set and this code registers each migration that the set
// names (ST-SUP-RC01-OC28).
func checkLedger(applied, registered []Migration) error {
	known := map[string]bool{}
	for _, m := range registered {
		known[m.Name] = true
	}
	for i, row := range applied {
		if i >= len(registered) {
			if !row.Step {
				return ErrMigrationLedger
			}
			for _, name := range row.CompatibleWith {
				if !known[name] {
					return ErrMigrationLedger
				}
			}
			continue
		}
		if row.Name != registered[i].Name {
			return ErrMigrationLedger
		}
		if row.Digest != registered[i].Digest {
			return &digestRefusal{text: fmt.Sprintf("the ledger row of the migration %s holds the digest %q, and this release registers the digest %q for its body: "+
				"the body of an applied migration never changes, so a change comes as a new schema step. Deploy the release that applied it, "+
				"or a release that keeps its body and brings the change in a new schema step; the job changed nothing, and nothing repairs the digest (ST-SUP-RC01-OC51)",
				row.Name, row.Digest, registered[i].Digest)}
		}
	}
	return nil
}

// digestRefusal is the refusal of a ledger row whose digest is not the body
// digest of the registered migration of the same name: its text names the
// migration, the two digests and the remedy, and no part of a body, so the
// migration job may log it (Refusal). It matches ErrMigrationLedger.
type digestRefusal struct{ text string }

func (r *digestRefusal) Error() string        { return r.text }
func (r *digestRefusal) Refusal() string      { return r.text }
func (r *digestRefusal) Is(target error) bool { return target == ErrMigrationLedger }

// applyPending applies the migrations after the recorded history, one
// transaction each, at read committed whatever the default isolation of the
// database is, so each statement takes its snapshot after the ledger lock is
// granted: it takes the ledger lock, reads the ledger again,
// refuses a ledger that is not a prefix of the registered history, and
// applies the next migration with its ledger row. A migration that another
// job applied in the meantime is not applied again. A migration of a schema
// step runs under the limits of a step (stepTimeouts), and when it waits too
// long for a lock it runs again after a pause, at most stepAttempts times
// (ST-SUP-RC01-OC22). The build of an index of a step runs outside a
// transaction (applyIndex).
func applyPending(ctx context.Context, db Database, ordered []Migration) error {
	tries := 0
	for {
		done := false
		var index *Migration
		err := inTransaction(ctx, db, &sql.TxOptions{Isolation: sql.LevelReadCommitted}, func(tx *sql.Tx) error {
			if err := lockLedger(ctx, tx); err != nil {
				return err
			}
			applied, err := appliedMigrations(ctx, tx)
			if err != nil {
				return err
			}
			if err := checkLedger(applied, ordered); err != nil {
				return err
			}
			if err := refreshMarker(ctx, tx); err != nil {
				return err
			}
			if len(applied) >= len(ordered) {
				done = true
				return nil
			}
			m := ordered[len(applied)]
			if m.Index != "" || m.Contract != nil {
				index = &m
				return nil
			}
			if m.Step {
				if err := stepTimeouts(ctx, tx, m); err != nil {
					return err
				}
			}
			if err := m.Func(ctx, tx); err != nil {
				return fmt.Errorf("migration %s: %w", m.Name, err)
			}
			return recordMigration(ctx, tx, m)
		})
		if err == nil && index != nil && index.Contract != nil {
			err = applyContract(ctx, db, ordered, *index)
		} else if err == nil && index != nil {
			err = applyIndex(ctx, db, ordered, *index)
		}
		if err != nil {
			tries++
			if !lockTimedOut(err) || tries >= stepAttempts {
				return err
			}
			if err := pause(ctx, tries); err != nil {
				return err
			}
			continue
		}
		tries = 0
		if done {
			return nil
		}
	}
}

// stepTimeouts sets, in the transaction of a migration of a schema step and
// after the ledger lock, the lock timeout of a step and the statement timeout
// of the migration (ST-SUP-RC01-OC22).
func stepTimeouts(ctx context.Context, tx *sql.Tx, m Migration) error {
	_, err := tx.ExecContext(ctx, "SELECT pg_catalog.set_config('lock_timeout', $1, true), pg_catalog.set_config('statement_timeout', $2, true)", stepLockTimeout, m.StatementTimeout)
	return err
}

// lockTimedOut tells whether err ends a wait for a lock at the lock timeout:
// SQLSTATE 55P03, lock_not_available.
func lockTimedOut(err error) bool {
	var state interface{ SQLState() string }
	return errors.As(err, &state) && state.SQLState() == "55P03"
}

// pause waits before the next attempt of a migration that waited too long for
// a lock: a time that doubles with each attempt from 100ms to at most 5s, of
// which a random part, so that two jobs do not try again at once.
func pause(ctx context.Context, attempt int) error {
	wait := 100 * time.Millisecond << min(attempt-1, 6)
	wait = min(wait, 5*time.Second)
	wait = wait/2 + rand.N(wait/2+1)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ledgerLockKey is the key of the ledger lock: lockLedger takes it for a
// transaction, and applyIndex for its session.
const ledgerLockKey = "1937007983,1935894637"

// applyIndex builds the index of the migration m of a schema step on a table
// with rows (ST-SUP-RC01-OC38). It holds the ledger lock for its session, on
// one connection, and no transaction is open on it while the index builds, so
// CREATE INDEX CONCURRENTLY waits for no transaction of its own job. It reads
// the ledger again, and builds nothing when another job has applied m. It
// first drops an invalid index of the same name, which a build that stopped
// part way left, by DROP INDEX CONCURRENTLY, and when the build fails it drops
// the invalid index that the build left at once, so that no half-built index
// stays (decision on finding F9); when that drop fails, it tries again
// (dropAfterFailure). The build waits for each transaction older than its
// snapshots, so it runs with no lock timeout and under the statement timeout
// of m; the drops run under the lock timeout of a step. It writes the ledger
// row only after the build succeeds and the index has the definition of m
// (indexBuilt).
func applyIndex(ctx context.Context, db Database, ordered []Migration, m Migration) error {
	conn, release, err := sessionOf(ctx, db)
	if err != nil {
		return err
	}
	defer release()
	if _, err := conn.ExecContext(ctx, "SELECT pg_catalog.pg_advisory_lock("+ledgerLockKey+")"); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_catalog.pg_advisory_unlock("+ledgerLockKey+")")
	}()
	position := -1
	for i := range ordered {
		if ordered[i].Name == m.Name {
			position = i
		}
	}
	pending := func(tx *sql.Tx) (bool, error) {
		applied, err := appliedMigrations(ctx, tx)
		if err != nil {
			return false, err
		}
		if err := checkLedger(applied, ordered); err != nil {
			return false, err
		}
		return len(applied) == position, nil
	}
	next := false
	if err := inTransaction(ctx, conn, &sql.TxOptions{Isolation: sql.LevelReadCommitted, ReadOnly: true}, func(tx *sql.Tx) error {
		var err error
		next, err = pending(tx)
		return err
	}); err != nil || !next {
		return err
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_catalog.set_config('lock_timeout', $1, false), pg_catalog.set_config('statement_timeout', $2, false)", stepLockTimeout, m.StatementTimeout); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "RESET lock_timeout; RESET statement_timeout")
	}()
	if err := dropInvalid(ctx, conn, m.Index); err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_catalog.set_config('lock_timeout', '0', false)"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, m.Body); err != nil {
		build := fmt.Errorf("migration %s: %w", m.Name, err)
		if err := dropAfterFailure(context.WithoutCancel(ctx), conn, m.Index); err != nil {
			return fmt.Errorf("%w; %v", build, err)
		}
		return build
	}
	if err := indexBuilt(ctx, conn, m.Index, m.Definition); err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}
	return inTransaction(ctx, conn, &sql.TxOptions{Isolation: sql.LevelReadCommitted}, func(tx *sql.Tx) error {
		next, err := pending(tx)
		if err != nil || !next {
			return err
		}
		return recordMigration(ctx, tx, m)
	})
}

// applyContract runs the contract migration m of a schema change
// (ST-SUP-RC01-OC24, ST-SUP-RC01-OC39). It holds the ledger lock for its
// session on one connection, reads the ledger again and runs nothing when
// another job has applied m. It first checks the step in its own transaction
// (checkContract), and refuses with the change and the count of rows left,
// changes no schema and writes no ledger row when the check fails. Then it
// runs each statement in its own short transaction under the lock timeout and
// the statement timeout of a step, again after a pause when the lock wait ends
// at the timeout, at most stepAttempts times; the statement that drops the
// sync trigger checks the step again in its transaction, and drops the
// completeness CHECK and the old column in that same transaction, so no write
// of the release before can change the old column alone after the trigger is
// gone (security review of PR #316, H1). It writes the ledger row last.
func applyContract(ctx context.Context, db Database, ordered []Migration, m Migration) error {
	conn, release, err := sessionOf(ctx, db)
	if err != nil {
		return err
	}
	defer release()
	if _, err := conn.ExecContext(ctx, "SELECT pg_catalog.pg_advisory_lock("+ledgerLockKey+")"); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_catalog.pg_advisory_unlock("+ledgerLockKey+")")
	}()
	position := -1
	for i := range ordered {
		if ordered[i].Name == m.Name {
			position = i
		}
	}
	pending := func(tx *sql.Tx) (bool, error) {
		applied, err := appliedMigrations(ctx, tx)
		if err != nil {
			return false, err
		}
		if err := checkLedger(applied, ordered); err != nil {
			return false, err
		}
		return len(applied) == position, nil
	}
	next := false
	if err := inTransaction(ctx, conn, &sql.TxOptions{Isolation: sql.LevelReadCommitted, ReadOnly: true}, func(tx *sql.Tx) error {
		var err error
		next, err = pending(tx)
		return err
	}); err != nil || !next {
		return err
	}
	c := *m.Contract
	if err := inTransaction(ctx, conn, &sql.TxOptions{Isolation: sql.LevelReadCommitted}, func(tx *sql.Tx) error {
		if err := stepTimeouts(ctx, tx, m); err != nil {
			return err
		}
		return checkContract(ctx, tx, c)
	}); err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}
	for _, s := range c.Statements {
		if err := contractStatementRun(ctx, conn, m, c, s); err != nil {
			return fmt.Errorf("migration %s: %w", m.Name, err)
		}
	}
	return inTransaction(ctx, conn, &sql.TxOptions{Isolation: sql.LevelReadCommitted}, func(tx *sql.Tx) error {
		next, err := pending(tx)
		if err != nil || !next {
			return err
		}
		return recordMigration(ctx, tx, m)
	})
}

// contractStatementRun runs one statement s of the contract step c of the
// migration m, with its statements Also, in its own transaction, when its
// query When gives that it still has to run, after the check of the step when
// it is a Guard, under the lock
// timeout of a step and the statement timeout of m, or the long one; it runs
// it again after a pause when the lock wait ends at the timeout, at most
// stepAttempts times. The last failure is wrapped as text, so the caller does
// not take it for one more lock wait to retry.
func contractStatementRun(ctx context.Context, conn *sql.Conn, m Migration, c ContractStep, s ContractStatement) error {
	timeout := m.StatementTimeout
	if s.Long {
		timeout = longStatementTimeout
	}
	for attempt := 1; ; attempt++ {
		err := inTransaction(ctx, conn, &sql.TxOptions{Isolation: sql.LevelReadCommitted}, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, "SELECT pg_catalog.set_config('lock_timeout', $1, true), pg_catalog.set_config('statement_timeout', $2, true)", stepLockTimeout, timeout); err != nil {
				return err
			}
			if s.When != "" {
				var run bool
				if err := tx.QueryRowContext(ctx, s.When).Scan(&run); err != nil {
					return err
				}
				if !run {
					return nil
				}
			}
			if s.Guard && (c.Drop || c.Seal) {
				// A statement of a drop or a seal takes the lock of its table first and checks the step after it, in
				// the same transaction: a process of the release before that reports itself after the check can no
				// longer write the table before the statement commits (security review of PR #423, M1 and I2; of PR
				// #443, M1).
				table := quoteIdentifier(c.Schema) + "." + quoteIdentifier(c.Table)
				var there bool
				if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.to_regclass($1) IS NOT NULL", table).Scan(&there); err != nil {
					return err
				}
				if there {
					if _, err := tx.ExecContext(ctx, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
						return err
					}
				}
			}
			if s.Guard {
				if err := checkContract(ctx, tx, c); err != nil {
					return err
				}
			}
			for _, statement := range append([]string{s.SQL}, s.Also...) {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			return nil
		})
		if err == nil {
			return nil
		}
		if c.Seal && s.Long && violatesCheck(err) && strings.Contains(s.SQL, "VALIDATE CONSTRAINT "+quoteIdentifier(c.Check)) {
			// The VALIDATE of the completeness CHECK, Check, reads every row, also a row of a tenant that the tenant
			// list does not hold, which the seal job does not seal (security review of PR #443, I4; re-review N5).
			return &contractRefusal{text: fmt.Sprintf("the contract step of the schema change %s waits for its seal: a row has a clear value and no sealed value, and the seal job has sealed each row of the listed tenants, so the row is of a tenant that the tenant list stego_tenants does not hold; provision that tenant and run the release of the expand step %s until the seal job completes a pass, or remove the rows of that tenant", c.Change, c.Expand)}
		}
		if !lockTimedOut(err) {
			return err
		}
		if attempt >= stepAttempts {
			return fmt.Errorf("%s waited for its lock %d times, and the last wait ended with: %v", s.SQL, attempt, err)
		}
		if err := pause(ctx, attempt); err != nil {
			return err
		}
	}
}

// violatesCheck tells whether err is a row that breaks a CHECK: SQLSTATE 23514, check_violation.
func violatesCheck(err error) bool {
	var state interface{ SQLState() string }
	return errors.As(err, &state) && state.SQLState() == "23514"
}

// keyRefusal is the refusal of a validate migration whose foreign key a row
// breaks: its text names the step, the table and the key and no row value, so
// the migration job may log it (Refusal). It wraps the database error, whose
// SQLSTATE 23503 the job keeps.
type keyRefusal struct {
	text string
	err  error
}

func (r *keyRefusal) Error() string   { return r.text }
func (r *keyRefusal) Refusal() string { return r.text }
func (r *keyRefusal) Unwrap() error   { return r.err }

// contractRefusal is the refusal of a contract step: its text names the
// change, the counts and the labels of the running releases, and no row
// value, so the migration job may log it (Refusal).
type contractRefusal struct{ text string }

func (r *contractRefusal) Error() string   { return r.text }
func (r *contractRefusal) Refusal() string { return r.text }

// checkContract checks the contract step c in the transaction tx
// (ST-SUP-RC01-OC24): while the sync trigger of the change or the old column
// is there, the backfill row of the change must be complete with no failed
// row, the completeness CHECK of the expand step must be there with its exact
// definition, as inherited, and validated, which proves that no row of any
// tenant has the old column set and the new one empty (decision on
// ST-SUP-Q120 (c); security review of PR #316, L1), and no release that does
// not know the change may run (runningReleases). A refusal gives the rows left
// of each listed tenant. When the trigger and the old column are both gone, a
// run before passed this check and dropped them in one transaction, so the
// check passes and the rerun goes on; when only the trigger is gone, the check
// runs in full again (security review of PR #316, H1). A trigger of the name
// that is disabled, fires only on a replica, or does not run the sync function
// stego_steps.<trigger>() does not copy the writes of the release before, so
// the check refuses it (security review of PR #316, L2); so does a trigger
// whose definition, as pg_get_triggerdef gives it, is not the one that the
// step made, CREATE TRIGGER <trigger> BEFORE INSERT OR UPDATE ON <table> FOR
// EACH ROW EXECUTE FUNCTION stego_steps.<trigger>(), as one that fires only on
// an INSERT, on an UPDATE OF another column, with a WHEN condition, for each
// statement or after the row (security review of PR #320, L1). It reads the
// catalog and the tables of the step with the search path pg_catalog,
// pg_temp, so pg_get_triggerdef names the schema of the sync function
// whatever the search path of the session, and only the operators of
// pg_catalog run in each compare, up to its last read (review of PR #320,
// info; security review of PR #332, INFO-3). When the check passes, it sets
// the search path of tx back for the statements of the step; a refusal or an
// error rolls tx back, and the path with it.
func checkContract(ctx context.Context, tx *sql.Tx, c ContractStep) (err error) {
	table := quoteIdentifier(c.Schema) + "." + quoteIdentifier(c.Table)
	var path string
	if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.current_setting('search_path')").Scan(&path); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "SELECT pg_catalog.set_config('search_path', 'pg_catalog, pg_temp', true)"); err != nil {
		return err
	}
	defer func() {
		if err == nil {
			_, err = tx.ExecContext(ctx, "SELECT pg_catalog.set_config('search_path', $1, true)", path)
		}
	}()
	if c.Seal {
		return checkSeal(ctx, tx, c, table)
	}
	if c.Drop {
		// A drop has no sync trigger and no backfill: while its old column or table is there, the check refuses
		// while a release that still reads it runs, and when it is gone a run before dropped it.
		var there bool
		query := "SELECT pg_catalog.to_regclass($1) IS NOT NULL"
		args := []any{table}
		if c.From != "" {
			query = "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid = pg_catalog.to_regclass($1) AND a.attname = $2 AND a.attnum > 0 AND NOT a.attisdropped)"
			args = append(args, c.From)
		}
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&there); err != nil {
			return err
		}
		if !there {
			return nil
		}
		return runningReleases(ctx, tx, c)
	}
	var trigger, syncing, column bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_trigger t WHERE t.tgrelid = pg_catalog.to_regclass($1) AND t.tgname = $2 AND NOT t.tgisinternal), EXISTS (SELECT 1 FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_proc p ON p.oid = t.tgfoid JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace JOIN pg_catalog.pg_class r ON r.oid = t.tgrelid JOIN pg_catalog.pg_namespace rn ON rn.oid = r.relnamespace WHERE t.tgrelid = pg_catalog.to_regclass($1) AND t.tgname = $2 AND NOT t.tgisinternal AND t.tgenabled IN ('O', 'A') AND n.nspname = 'stego_steps' AND p.proname = $2 AND p.pronargs = 0 AND pg_catalog.pg_get_triggerdef(t.oid) = 'CREATE TRIGGER ' || pg_catalog.quote_ident($2) || ' BEFORE INSERT OR UPDATE ON ' || pg_catalog.quote_ident(rn.nspname) || '.' || pg_catalog.quote_ident(r.relname) || ' FOR EACH ROW EXECUTE FUNCTION stego_steps.' || pg_catalog.quote_ident($2) || '()'), EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid = pg_catalog.to_regclass($1) AND a.attname = $3 AND a.attnum > 0 AND NOT a.attisdropped)", table, c.Trigger, c.From).Scan(&trigger, &syncing, &column); err != nil {
		return err
	}
	if !trigger && !column {
		return nil
	}
	var complete bool
	var failed int64
	err = tx.QueryRowContext(ctx, "SELECT b.completed_at IS NOT NULL, b.failed_rows FROM stego_schema.backfills b WHERE b.change_id = $1", c.Change).Scan(&complete, &failed)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var definition string
	var noInherit, validated bool
	err = tx.QueryRowContext(ctx, "SELECT pg_catalog.pg_get_constraintdef(k.oid), k.contype <> 'c' OR k.connoinherit, k.convalidated FROM pg_catalog.pg_constraint k WHERE k.conrelid = pg_catalog.to_regclass($1) AND k.conname = $2", table, c.Check).Scan(&definition, &noInherit, &validated)
	present := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	exact := present && !noInherit && strings.TrimSuffix(definition, " NOT VALID") == c.Definition
	state := ""
	switch {
	case trigger && !syncing:
		state = "the sync trigger " + c.Trigger + " is disabled or does not run its sync function before each write of a row"
	case !found:
		state = "the backfill has no checkpoint row"
	case failed > 0:
		state = fmt.Sprintf("the backfill has %d failed rows", failed)
	case !complete:
		state = "the backfill is not complete"
	case !present:
		state = "the completeness CHECK " + c.Check + " is not there"
	case !exact:
		state = "the constraint " + c.Check + " is not the completeness CHECK of the change"
	case !validated:
		state = "the completeness CHECK " + c.Check + " is not validated"
	}
	if state != "" {
		left, err := rowsLeft(ctx, tx, c)
		count := "unknown"
		if err == nil {
			count = fmt.Sprint(left)
		}
		return &contractRefusal{text: fmt.Sprintf("the contract step of the schema change %s waits for its backfill: %s; rows of the listed tenants with the old column set and the new one empty: %s; run the migration job of the release before until the backfill completes", c.Change, state, count)}
	}
	// A last count of the rows with the old column set and the new one empty: a write that the completeness CHECK
	// let through, as a value that does not convert with no fill value, would lose its value when the old column
	// goes (security review of PR #335, L5, orchestrator under the owner's delegation, 2026-11-14).
	left, err := rowsLeft(ctx, tx, c)
	if err != nil {
		return err
	}
	if left > 0 {
		return &contractRefusal{text: fmt.Sprintf("the contract step of the schema change %s waits for its backfill: rows of the listed tenants with the old column set and the new one empty: %d; run the migration job of the release before until the backfill completes", c.Change, left)}
	}
	return runningReleases(ctx, tx, c)
}

// checkSeal checks the contract step c of a seal in tx (ST-SUP-RC01-OC50):
// while the clear column is there, the seal checkpoint of the change must
// hold a completed pass, no release before the expand migration may have
// reported itself within its window after the start of that pass, since such a
// release writes the clear column alone and its write would be lost with the
// column (ST-SUP-RP09-LC15), no row of a listed tenant may have a clear value
// and no sealed value, and no release before may run (runningReleases). When
// the clear column is gone, a run before dropped it and the check passes.
func checkSeal(ctx context.Context, tx *sql.Tx, c ContractStep, table string) error {
	var there bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid = pg_catalog.to_regclass($1) AND a.attname = $2 AND a.attnum > 0 AND NOT a.attisdropped)", table, c.From).Scan(&there); err != nil {
		return err
	}
	if !there {
		return nil
	}
	var seals bool
	if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.to_regclass('stego_schema.seals') IS NOT NULL AND pg_catalog.to_regclass('stego_schema.releases') IS NOT NULL").Scan(&seals); err != nil {
		return err
	}
	state := "the seal checkpoint of the change is not there"
	if seals {
		var complete, stale bool
		err := tx.QueryRowContext(ctx, "SELECT s.completed_at IS NOT NULL, EXISTS (SELECT 1 FROM stego_schema.releases r WHERE r.release COLLATE \"C\" < s.expand COLLATE \"C\" AND r.seen_at + r.every_ms * $2::integer * interval '1 millisecond' > s.pass_started) FROM stego_schema.seals s WHERE s.change_id = $1", c.Change, releaseWindow).Scan(&complete, &stale)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case !complete:
			state = "the seal job of the controller has not completed a pass over the rows"
		case stale:
			state = "a release before the step ran after the start of the last pass of the seal job, so a sealed value can be older than its clear value"
		default:
			state = ""
		}
	}
	if state != "" {
		return &contractRefusal{text: fmt.Sprintf("the contract step of the schema change %s waits for its seal: %s; run the release of the expand step %s, with its controller, until the seal job completes a pass", c.Change, state, c.Expand)}
	}
	left, err := rowsLeft(ctx, tx, c)
	if err != nil {
		return err
	}
	if left > 0 {
		return &contractRefusal{text: fmt.Sprintf("the contract step of the schema change %s waits for its seal: rows of the listed tenants with a clear value and no sealed value: %d; run the release of the expand step %s, with its controller, until the seal job completes a pass", c.Change, left, c.Expand)}
	}
	return runningReleases(ctx, tx, c)
}

// rowsLeft counts the rows of the table of c whose old column is set and
// whose new column is empty: on an owned table, which has row-level security,
// for each tenant of the tenant list in turn under its tenant setting.
func rowsLeft(ctx context.Context, tx *sql.Tx, c ContractStep) (int64, error) {
	table := quoteIdentifier(c.Schema) + "." + quoteIdentifier(c.Table)
	count := "SELECT pg_catalog.count(*) FROM " + table + " WHERE " + c.Old + " IS NOT NULL AND " + c.New + " IS NULL"
	if c.TenantColumn == "" {
		var n int64
		err := tx.QueryRowContext(ctx, count).Scan(&n)
		return n, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT t.tenant_id FROM "+quoteIdentifier(c.TenantSchema)+".stego_tenants t ORDER BY t.tenant_id COLLATE \"C\"")
	if err != nil {
		return 0, err
	}
	var tenants []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		tenants = append(tenants, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var total int64
	for _, id := range tenants {
		var n int64
		if _, err := tx.ExecContext(ctx, "SELECT pg_catalog.set_config('app.tenant', $1, true)", id); err != nil {
			return 0, err
		}
		if err := tx.QueryRowContext(ctx, count).Scan(&n); err != nil {
			return 0, err
		}
		total += n
	}
	_, err = tx.ExecContext(ctx, "SELECT pg_catalog.set_config('app.tenant', '', true)")
	return total, err
}

// runningReleases refuses while a process of a release that does not know
// the change reports itself in stego_schema.releases (decision on finding F8
// of the schema-change research, owner, 2026-11-13; ST-SUP-RP09-LC14): a
// release whose last migration sorts before the expand migration of the
// change, with a report younger than releaseWindow of its intervals. It
// refuses when the table is missing, as the expand migration makes it.
func runningReleases(ctx context.Context, tx *sql.Tx, c ContractStep) error {
	var table bool
	if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.to_regclass('stego_schema.releases') IS NOT NULL").Scan(&table); err != nil {
		return err
	}
	if !table {
		return &contractRefusal{text: fmt.Sprintf("the contract step of the schema change %s finds no table stego_schema.releases of the release reports, which the expand migration of the change makes", c.Change)}
	}
	rows, err := tx.QueryContext(ctx, "SELECT r.version, pg_catalog.count(*) FROM stego_schema.releases r WHERE r.release COLLATE \"C\" < $1 COLLATE \"C\" AND r.seen_at > pg_catalog.clock_timestamp() - r.every_ms * $2::integer * interval '1 millisecond' GROUP BY r.version ORDER BY r.version COLLATE \"C\"", c.Expand, releaseWindow)
	if err != nil {
		return err
	}
	defer rows.Close()
	var running []string
	for rows.Next() {
		var version string
		var n int64
		if err := rows.Scan(&version, &n); err != nil {
			return err
		}
		if version == "" {
			version = "with no version"
		}
		running = append(running, fmt.Sprintf("%s (%d processes)", version, n))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(running) > 0 {
		// The contract of a drop of an entity drops a table, and every other contract step an old column.
		old := "the old column"
		if c.Drop && c.From == "" {
			old = "the old table"
		}
		return &contractRefusal{text: fmt.Sprintf("the contract step of the schema change %s waits for the release before to stop: the releases %s run and do not know the migration %s, so they still need %s; stop them, and run the migration job again", c.Change, strings.Join(running, ", "), c.Expand, old)}
	}
	return nil
}

// dropAfterFailure drops the invalid index index that a failed build left,
// under the lock timeout of a step, and when the drop fails it runs it again
// after a pause, at most dropAttempts times. An invalid unique index that is
// ready goes on enforcing its key on the writes of the release before, so a
// drop that still fails gives an error that names the index, and the job
// fails with it; it never stays silent (ST-SUP-RC01-OC38).
func dropAfterFailure(ctx context.Context, conn *sql.Conn, index string) error {
	var err error
	for attempt := 1; attempt <= dropAttempts; attempt++ {
		if attempt > 1 {
			if err := pause(ctx, attempt-1); err != nil {
				return err
			}
		}
		if _, err = conn.ExecContext(ctx, "SELECT pg_catalog.set_config('lock_timeout', $1, false)", stepLockTimeout); err == nil {
			if err = dropInvalid(ctx, conn, index); err == nil {
				return nil
			}
		}
	}
	return fmt.Errorf("the failed build left the invalid index %s, and %d drops of it failed, the last with: %v", index, dropAttempts, err)
}

// sessionOf gives one connection of db for a session setting, and the function
// that gives it back: db itself when it is one connection, else a connection of
// its pool.
func sessionOf(ctx context.Context, db Database) (*sql.Conn, func(), error) {
	switch d := db.(type) {
	case *sql.Conn:
		return d, func() {}, nil
	case interface {
		Conn(context.Context) (*sql.Conn, error)
	}:
		conn, err := d.Conn(ctx)
		if err != nil {
			return nil, nil, err
		}
		return conn, func() { _ = conn.Close() }, nil
	}
	return nil, nil, ErrMigrationLedger
}

// dropInvalid drops the index index, outside a transaction, when it is there
// and not valid.
func dropInvalid(ctx context.Context, conn *sql.Conn, index string) error {
	var invalid bool
	if err := conn.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_index WHERE indexrelid = pg_catalog.to_regclass($1) AND NOT indisvalid)", index).Scan(&invalid); err != nil {
		return err
	}
	if !invalid {
		return nil
	}
	_, err := conn.ExecContext(ctx, "DROP INDEX CONCURRENTLY IF EXISTS "+index)
	return err
}

`

// generateMigrate produces migrate.go: the migration ledger in stego_schema.migrations, with 001_initial as the SQL
// migration of InitialTables (ST-SUP-RC01-OC41), in a pooled service the row-level security of the tenancy
// component after it, and then the migration of each schema step (ST-SUP-RC01-OC22). The ledger refuses every row that is not the registered history, in order and with the body
// digest (ST-SUP-RC01-OC11).
func generateMigrate(ns string, rowSecurity string, steps []schemaStep) (gen.File, error) {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "package %s\n\n", path.Base(ns))
	fmt.Fprintf(&buf, "import (\n")
	fmt.Fprintf(&buf, "\t\"context\"\n")
	fmt.Fprintf(&buf, "\t\"crypto/sha256\"\n")
	fmt.Fprintf(&buf, "\t\"database/sql\"\n")
	fmt.Fprintf(&buf, "\t\"encoding/hex\"\n")
	fmt.Fprintf(&buf, "\t\"errors\"\n")
	fmt.Fprintf(&buf, "\t\"fmt\"\n")
	fmt.Fprintf(&buf, "\t\"math/rand/v2\"\n")
	fmt.Fprintf(&buf, "\t\"sort\"\n")
	fmt.Fprintf(&buf, "\t\"strings\"\n")
	fmt.Fprintf(&buf, "\t\"sync\"\n")
	fmt.Fprintf(&buf, "\t\"time\"\n")
	fmt.Fprintf(&buf, "\n\truntimetenant %q\n", runtimeTenantImport)
	if rowSecurity != "" {
		fmt.Fprintf(&buf, "\tstegotenancy %q\n", rowSecurity)
	}
	fmt.Fprintf(&buf, ")\n\n")
	buf.WriteString(migrationLedger)

	// Initial migration registration.
	// Generation registers no ledgered migration but 001_initial, the tenancy row-level security and the steps
	// (ST-SUP-RC01-OC43). It checks the names that it registers, not the text of the files, so no form of a name
	// escapes the check.
	names := []string{"001_initial"}
	if rowSecurity != "" {
		names = append(names, tenancyMigration)
	}
	for _, step := range steps {
		names = append(names, step.Name)
	}
	for _, name := range names {
		if err := CheckLedgerName(name); err != nil {
			return gen.File{}, err
		}
	}
	// The generated migrations are registered by a function that orderedMigrations calls once, never by func init,
	// so nothing in the service registers code at package initialization (ST-GEN-INV14).
	fmt.Fprintf(&buf, "// registerGenerated registers the generated migrations in their fixed order.\n")
	fmt.Fprintf(&buf, "func registerGenerated() {\n")
	fmt.Fprintf(&buf, "\tRegisterSQL(\"001_initial\", %s)\n", tablesConstant)
	if rowSecurity != "" {
		// The row-level security of the tenancy component comes after every table it covers (ST-SUP-RC27-OC03).
		fmt.Fprintf(&buf, "\tRegisterSQL(%q, stegotenancy.RowSecurity)\n", tenancyMigration)
	}
	// Each schema step comes after the component's own migrations, in the order of the schema history, with the
	// empty compatible_with set of a step at expand (ST-SUP-RC01-OC22, ST-SUP-RC01-OC27).
	// A step on a table with rows is its migration, with its constraints NOT VALID, then one migration for each
	// index that it builds CONCURRENTLY, then, for a rename, the migration of its sync trigger, then the migration
	// that validates its constraints (ST-SUP-RC01-OC22, ST-SUP-RC01-OC38).
	for _, step := range steps {
		switch {
		case step.Index != "":
			fmt.Fprintf(&buf, "\tRegisterIndexStep(%q, %q, %q, %q)\n", step.Name, step.Index, step.Body, step.Definition)
		case step.Validate:
			fmt.Fprintf(&buf, "\tRegisterValidateStep(%q, %q", step.Name, step.Body)
			for _, v := range step.Validations {
				fmt.Fprintf(&buf, ", ValidateConstraint{Schema: %q, Table: %q, Name: %q, Foreign: %t}", v.schema, v.tableName, v.name, v.foreign)
			}
			fmt.Fprintf(&buf, ")\n")
		case step.Contract != nil:
			c := step.Contract
			fmt.Fprintf(&buf, "\tRegisterContractStep(%q, %q, ContractStep{Change: %q, Schema: %q, Table: %q, Check: %q, Definition: %q, Trigger: %q, Expand: %q, From: %q, Old: %q, New: %q, TenantColumn: %q, TenantSchema: %q, Statements: []ContractStatement{\n",
				step.Name, step.Body, c.change, c.schema, c.tableName, c.check, c.definition, c.trigger, step.Expand, c.from, c.old, c.new, c.tenant, c.tenantSchema)
			for _, s := range c.statements {
				also := ""
				if len(s.also) > 0 {
					quoted := make([]string, len(s.also))
					for i, a := range s.also {
						quoted[i] = strconv.Quote(a)
					}
					also = ", Also: []string{" + strings.Join(quoted, ", ") + "}"
				}
				fmt.Fprintf(&buf, "\t\t{SQL: %q, When: %q, Long: %t, Guard: %t%s},\n", s.sql, s.when, s.long, s.guard, also)
			}
			drop := ""
			if c.drop {
				drop = ", Drop: true"
			}
			if c.seal {
				drop = ", Seal: true"
			}
			fmt.Fprintf(&buf, "\t}%s})\n", drop)
		default:
			fmt.Fprintf(&buf, "\tRegisterStep(%q, %q)\n", step.Name, step.Body)
		}
	}
	fmt.Fprintf(&buf, "}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting migrate: %w", err)
	}

	return gen.File{
		Path:    path.Join(ns, "migrate.go"),
		Content: formatted,
	}, nil
}

// migrationCall is one Register call of a function migration in a generated
// file: the offset where its digest argument goes, the digest it holds, and
// the digest of its body.
type migrationCall struct {
	insert int
	digest string
	sum    string
}

// sealMigrations writes the digest of each function migration into its
// Register call. The digest covers the generated source text of the function
// literal and of each package-level function, constant and variable that the
// literal reaches, so an edit to the migration changes its digest. Types are
// not covered: a model migration follows its models by design.
func sealMigrations(ns string, files []gen.File) error {
	calls, err := migrationCalls(ns, files)
	if err != nil {
		return err
	}
	for i, list := range calls {
		content := files[i].Content
		for j := len(list) - 1; j >= 0; j-- {
			if list[j].digest != "" {
				return fmt.Errorf("%s: migration digest is already set", files[i].Path)
			}
			at := list[j].insert
			content = append(content[:at:at], append([]byte(strconv.Quote(list[j].sum)+", "), content[at:]...)...)
		}
		formatted, err := format.Source(content)
		if err != nil {
			return fmt.Errorf("formatting %s: %w", files[i].Path, err)
		}
		files[i].Content = formatted
	}
	// Formatting must not change a sealed body.
	calls, err = migrationCalls(ns, files)
	if err != nil {
		return err
	}
	for i, list := range calls {
		for _, call := range list {
			if call.digest != call.sum {
				return fmt.Errorf("%s: migration digest does not cover its body", files[i].Path)
			}
		}
	}
	return nil
}

// migrationCalls finds the Register calls of function migrations in the Go
// files of the namespace, by file index, and computes the digest of each body.
func migrationCalls(ns string, files []gen.File) (map[int][]migrationCall, error) {
	type declaration struct {
		node ast.Node
		text string
	}
	fset := token.NewFileSet()
	parsed := map[int]*ast.File{}
	declarations := map[string]declaration{}
	for i, file := range files {
		if path.Dir(file.Path) != ns || path.Ext(file.Path) != ".go" {
			continue
		}
		tree, err := parser.ParseFile(fset, file.Path, file.Content, 0)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", file.Path, err)
		}
		parsed[i] = tree
		text := func(node ast.Node) string {
			return string(file.Content[fset.Position(node.Pos()).Offset:fset.Position(node.End()).Offset])
		}
		for _, decl := range tree.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Recv == nil && decl.Name.Name != "init" {
					declarations[decl.Name.Name] = declaration{decl, text(decl)}
				}
			case *ast.GenDecl:
				if decl.Tok != token.CONST && decl.Tok != token.VAR {
					continue
				}
				for _, spec := range decl.Specs {
					for _, name := range spec.(*ast.ValueSpec).Names {
						declarations[name.Name] = declaration{spec, text(spec)}
					}
				}
			}
		}
	}
	references := func(node ast.Node) []string {
		var names []string
		var visit func(ast.Node) bool
		visit = func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				ast.Inspect(n.X, visit)
				return false
			case *ast.Ident:
				names = append(names, n.Name)
			}
			return true
		}
		ast.Inspect(node, visit)
		return names
	}
	calls := map[int][]migrationCall{}
	indexes := make([]int, 0, len(parsed))
	for i := range parsed {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		content := files[i].Content
		ast.Inspect(parsed[i], func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name, ok := call.Fun.(*ast.Ident); !ok || name.Name != "Register" || len(call.Args) < 2 || len(call.Args) > 3 {
				return true
			}
			literal, ok := call.Args[len(call.Args)-1].(*ast.FuncLit)
			if !ok {
				return true
			}
			var body strings.Builder
			body.Write(content[fset.Position(literal.Pos()).Offset:fset.Position(literal.End()).Offset])
			reached := map[string]bool{}
			queue := references(literal)
			for len(queue) > 0 {
				name := queue[0]
				queue = queue[1:]
				if reached[name] {
					continue
				}
				if found, ok := declarations[name]; ok {
					reached[name] = true
					queue = append(queue, references(found.node)...)
				}
			}
			names := make([]string, 0, len(reached))
			for name := range reached {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				body.WriteString("\n")
				body.WriteString(declarations[name].text)
			}
			sum := sha256.Sum256([]byte(body.String()))
			sealed := migrationCall{insert: fset.Position(literal.Pos()).Offset, sum: hex.EncodeToString(sum[:])}
			if len(call.Args) == 3 {
				if digest, ok := call.Args[1].(*ast.BasicLit); ok && digest.Kind == token.STRING {
					sealed.digest, _ = strconv.Unquote(digest.Value)
				} else {
					sealed.digest = "?"
				}
			}
			calls[i] = append(calls[i], sealed)
			return true
		})
	}
	return calls, nil
}

// tableName converts an entity name to a PostgreSQL table name:
// PascalCase to snake_case, then pluralize using English rules.
func tableName(entityName string) string {
	return pluralize(toSnakeCase(entityName))
}

// Plural gives the plural of a word by the rule of the table names (pluralize), such as Entries of Entry and Boxes of
// Box: the query generator names the repository of an entity with it (walkthrough 12, W12-B5).
func Plural(word string) string { return pluralize(word) }

// pluralize applies basic English pluralization rules to a snake_case word.
func pluralize(s string) string {
	if s == "" {
		return s
	}
	if strings.HasSuffix(s, "s") || strings.HasSuffix(s, "x") || strings.HasSuffix(s, "z") ||
		strings.HasSuffix(s, "sh") || strings.HasSuffix(s, "ch") {
		return s + "es"
	}
	if strings.HasSuffix(s, "y") && len(s) >= 2 {
		preceding := s[len(s)-2]
		if !strings.ContainsRune("aeiou", rune(preceding)) {
			return s[:len(s)-1] + "ies"
		}
	}
	return s + "s"
}

// toSnakeCase converts PascalCase to snake_case, correctly handling
// consecutive uppercase letters (acronyms) like HTTP, API, ID.
func toSnakeCase(s string) string {
	runes := []rune(s)
	var result []rune
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prev := runes[i-1]
			if unicode.IsLower(prev) {
				result = append(result, '_')
			} else if unicode.IsUpper(prev) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
				result = append(result, '_')
			}
		}
		result = append(result, unicode.ToLower(r))
	}
	return string(result)
}

// toPascalCase converts a snake_case string to PascalCase, treating "id" as
// the acronym "ID" per Go conventions.
func toPascalCase(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if strings.EqualFold(p, "id") {
			parts[i] = "ID"
		} else if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// --- Topological Sort ---

// topoSortEntities topologically sorts entities by ref dependencies so that
// referenced tables are created before referencing tables.
func topoSortEntities(entities []types.Entity) ([]types.Entity, error) {
	entityIndex := make(map[string]int, len(entities))
	for i, e := range entities {
		entityIndex[e.Name] = i
	}

	deps := make(map[string]map[string]bool, len(entities))
	for _, e := range entities {
		d := make(map[string]bool)
		for _, f := range e.Fields {
			if f.Type == types.FieldTypeRef && f.To != "" {
				if _, ok := entityIndex[f.To]; ok && f.To != e.Name {
					d[f.To] = true
				}
			}
		}
		deps[e.Name] = d
	}

	var sorted []types.Entity
	visited := make(map[string]bool, len(entities))

	for len(sorted) < len(entities) {
		var ready []string
		for _, e := range entities {
			if visited[e.Name] {
				continue
			}
			allSatisfied := true
			for dep := range deps[e.Name] {
				if !visited[dep] {
					allSatisfied = false
					break
				}
			}
			if allSatisfied {
				ready = append(ready, e.Name)
			}
		}
		if len(ready) == 0 {
			var cycle []string
			for _, e := range entities {
				if !visited[e.Name] {
					cycle = append(cycle, e.Name)
				}
			}
			return nil, fmt.Errorf("circular ref dependency among entities: %s",
				strings.Join(cycle, " → "))
		}
		for _, name := range ready {
			visited[name] = true
			sorted = append(sorted, entities[entityIndex[name]])
		}
	}

	return sorted, nil
}

// --- Validation ---

func validateEntityUniqueness(entities []types.Entity) error {
	seen := make(map[string]int, len(entities))
	var dupes []string
	for _, e := range entities {
		seen[e.Name]++
		if seen[e.Name] == 2 {
			dupes = append(dupes, e.Name)
		}
	}
	if len(dupes) > 0 {
		return fmt.Errorf("duplicate entity names: %s; each entity must have a unique name",
			strings.Join(dupes, ", "))
	}
	return nil
}

func validateCaseInsensitiveUniqueness(entities []types.Entity) error {
	seen := make(map[string]string, len(entities))
	var errs []string
	for _, e := range entities {
		tbl := tableName(e.Name)
		if existing, ok := seen[tbl]; ok {
			errs = append(errs, fmt.Sprintf(
				"entities %q and %q both produce table name %q",
				existing, e.Name, tbl))
		} else {
			seen[tbl] = e.Name
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("derived table name collisions:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateReservedNames(entities []types.Entity) error {
	for _, e := range entities {
		if reservedTypeNames[e.Name] {
			return fmt.Errorf(
				"entity name %q collides with postgres-adapter generator internal identifier %q; rename the entity to avoid a redeclaration error in generated code",
				e.Name, e.Name)
		}
	}
	return nil
}

func validateRefTargets(entities []types.Entity) error {
	entityNames := make(map[string]bool, len(entities))
	for _, e := range entities {
		entityNames[e.Name] = true
	}

	var errs []string
	for _, e := range entities {
		for _, f := range e.Fields {
			if f.Type == types.FieldTypeRef {
				if f.To == "" {
					errs = append(errs, fmt.Sprintf(
						"entity %s: field %q has type ref but no 'to' attribute — ref fields must specify the target entity",
						e.Name, f.Name))
				} else if !entityNames[f.To] {
					errs = append(errs, fmt.Sprintf(
						"entity %s: field %q has type ref with to: %q, but entity %q does not exist",
						e.Name, f.Name, f.To, f.To))
				}
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("unresolved ref targets:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func validateFieldUniqueness(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		seen := make(map[string]bool, len(e.Fields))
		for _, f := range e.Fields {
			if seen[f.Name] {
				errs = append(errs, fmt.Sprintf(
					"entity %s: duplicate field name %q",
					e.Name, f.Name))
			}
			seen[f.Name] = true
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("field name uniqueness violations:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateFieldNameCharset(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		for _, f := range e.Fields {
			if !validFieldNamePattern.MatchString(f.Name) {
				errs = append(errs, fmt.Sprintf(
					"entity %s: field name %q contains invalid characters; field names must match %s",
					e.Name, f.Name, validFieldNamePattern.String()))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid field name characters:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateEntityNameCharset(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		if !validFieldNamePattern.MatchString(e.Name) {
			errs = append(errs, fmt.Sprintf(
				"entity name %q contains invalid characters; entity names must match %s",
				e.Name, validFieldNamePattern.String()))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid entity name characters:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateNoImplicitIDCollision(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		for _, f := range e.Fields {
			if toPascalCase(f.Name) == "ID" {
				errs = append(errs, fmt.Sprintf(
					"entity %s: field %q collides with the implicit primary key column — entities cannot declare a field named %q",
					e.Name, f.Name, f.Name))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("implicit ID collisions:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

var validGoIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func validateDerivedEntityValidity(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		if e.Name == "_" {
			errs = append(errs, fmt.Sprintf(
				"entity name %q is the Go blank identifier and cannot be used as a type name",
				e.Name))
			continue
		}
		if !validGoIdentifier.MatchString(e.Name) {
			errs = append(errs, fmt.Sprintf(
				"entity name %q is not a valid Go identifier",
				e.Name))
			continue
		}
		allUnderscore := true
		for _, r := range e.Name {
			if r != '_' {
				allUnderscore = false
				break
			}
		}
		if allUnderscore {
			errs = append(errs, fmt.Sprintf(
				"entity name %q consists only of underscores and cannot be used as a Go type name",
				e.Name))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid derived entity identifiers:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateDerivedFieldValidity(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		for _, f := range e.Fields {
			pascal := toPascalCase(f.Name)
			if !validGoIdentifier.MatchString(pascal) {
				errs = append(errs, fmt.Sprintf(
					"entity %s: field %q transforms to %q via toPascalCase, which is not a valid Go identifier",
					e.Name, f.Name, pascal))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid derived field identifiers:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateDerivedFieldUniqueness(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		seen := make(map[string]string, len(e.Fields))
		for _, f := range e.Fields {
			pascal := toPascalCase(f.Name)
			if existing, ok := seen[pascal]; ok && existing != f.Name {
				errs = append(errs, fmt.Sprintf(
					"entity %s: fields %q and %q both produce Go struct field name %q",
					e.Name, existing, f.Name, pascal))
			} else {
				seen[pascal] = f.Name
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("derived field name collisions:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateEnumValues(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		for _, f := range e.Fields {
			if f.Type != types.FieldTypeEnum {
				continue
			}
			if len(f.Values) == 0 {
				errs = append(errs, fmt.Sprintf(
					"entity %s: field %q has type enum but no values — enum fields must specify at least one value",
					e.Name, f.Name))
				continue
			}
			seen := make(map[string]bool, len(f.Values))
			for _, v := range f.Values {
				if seen[v] {
					errs = append(errs, fmt.Sprintf(
						"entity %s: field %q has duplicate enum value %q",
						e.Name, f.Name, v))
				}
				seen[v] = true
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid enum fields:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}
