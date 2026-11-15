package manifest

import (
	"context"
	"database/sql"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The relaxed forms of a column (ST-SUP-RP15-LC04; decision on finding F12 of the schema-change research, owner,
// 2026-11-13): the relax step of a later release changes one column of an entity table in place, and the release
// before keeps running on it. The step first marks the column with the comment RelaxMarker. On a plain table of a
// declared entity (Table.Entity), Check then accepts, for a column that the manifest lists, that has the marker, and
// that is no column of the primary key and none of the fixed columns:
//
//   - the type character varying(m) where the manifest states character varying(n) and m > n;
//   - no NOT NULL where the manifest states NOT NULL;
//   - a default that is a constant of a built-in type where the manifest states none;
//   - in place of the CHECK of the column that the manifest names (CheckName), a validated CHECK of enum values
//     that holds each value of the manifest's and more; and, when RelaxCheckName names a CHECK of the column, the
//     CHECK of the manifest missing;
//   - the CHECK RelaxCheckName of the column, NOT VALID or validated, when it is the manifest's CHECK of the
//     column or a CHECK of enum values that holds each of its values; or, with no such CHECK in the manifest, missing
//     where the manifest's CHECK of the column is a CHECK of enum values alone, as when an enum becomes a string.
//
// Every other form, and each of these on a column with no marker, stays a fault, so a column that someone changed by
// hand is still refused.

// RelaxMarker is the comment that the relax step gives the column that it changes.
const RelaxMarker = "stego relax"

// RelaxCheckName gives the name of the new CHECK of a relax until the step gives it the name of the column's CHECK:
// <table>_<column>_relax, or, when that is longer than 63 bytes, its first 46 bytes, an underscore and the first 16
// hexadecimal digits of its SHA-256 digest, as the generator's sqlName gives it.
func RelaxCheckName(table, column string) string {
	n := table + "_" + column + "_relax"
	if len(n) <= 63 {
		return n
	}
	return n[:46] + "_" + digestPrefix(n)
}

// relaxedColumns reads the columns of the table rel that have the comment RelaxMarker, that are no column of its
// primary key and none of the fixed columns.
func relaxedColumns(ctx context.Context, tx *sql.Tx, rel uint32) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.attname FROM pg_catalog.pg_attribute a
  WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped
    AND pg_catalog.col_description(a.attrelid, a.attnum) = $2
    AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_index i WHERE i.indrelid = a.attrelid AND i.indisprimary AND a.attnum = ANY (i.indkey))`,
		rel, RelaxMarker)
	if err != nil {
		return nil, err
	}
	relaxed := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		if !fixedColumns[n] {
			relaxed[n] = true
		}
	}
	return relaxed, closed(rows)
}

// varcharType reads the length of the type character varying(n).
var varcharType = regexp.MustCompile(`^character varying\(([1-9][0-9]*)\)$`)

// relaxedType tells whether the type got is a longer character varying than the type want.
func relaxedType(want, got string) bool {
	w, g := varcharType.FindStringSubmatch(want), varcharType.FindStringSubmatch(got)
	if w == nil || g == nil {
		return false
	}
	wn, errW := strconv.Atoi(w[1])
	gn, errG := strconv.Atoi(g[1])
	return errW == nil && errG == nil && gn > wn
}

// Enum values as pg_get_constraintdef writes their CHECK: one value, or an array of more.
var (
	valuesCheck = regexp.MustCompile(`^CHECK \(\((.+) = ANY \(ARRAY\[(.*)\]\)\)\)$`)
	valueCheck  = regexp.MustCompile(`^CHECK \(\((.+) = ('(?:[^']|'')*'::text)\)\)$`)
	textValue   = regexp.MustCompile(`'((?:[^']|'')*)'::text`)
)

// checkValues reads a CHECK of enum values on the column: its values, and false when the definition is not such a
// CHECK of that column. The values must make up the whole array, so no other expression can stand among them.
func checkValues(definition, column string) ([]string, bool) {
	definition = strings.TrimSuffix(definition, " NOT VALID")
	ref, list := "", ""
	if m := valuesCheck.FindStringSubmatch(definition); m != nil {
		ref, list = m[1], m[2]
	} else if m := valueCheck.FindStringSubmatch(definition); m != nil {
		ref, list = m[1], m[2]
	} else {
		return nil, false
	}
	if ref != QuoteIdent(column) {
		return nil, false
	}
	var values, parts []string
	for _, m := range textValue.FindAllStringSubmatch(list, -1) {
		values = append(values, strings.ReplaceAll(m[1], "''", "'"))
		parts = append(parts, m[0])
	}
	if len(values) == 0 || strings.Join(parts, ", ") != list {
		return nil, false
	}
	return values, true
}

// relaxedCheck tells whether the CHECK definition got of the column is the definition want, or, when both are CHECKs
// of enum values of the column, one that holds each value of want.
func relaxedCheck(want, got, column string) bool {
	if strings.TrimSuffix(got, " NOT VALID") == want {
		return true
	}
	wantValues, ok := checkValues(want, column)
	gotValues, found := checkValues(got, column)
	if !ok || !found {
		return false
	}
	for _, v := range wantValues {
		if !slices.Contains(gotValues, v) {
			return false
		}
	}
	return true
}

// relaxState is what checkTable knows of the relaxed columns of one table: the marked columns, the CHECKs of the
// manifest by name, and the CHECKs of the table by name with their definitions and whether they are validated.
type relaxState struct {
	table   string
	columns map[string]bool
	named   map[string]Constraint
}

// marked tells whether the column has the marker of a relax.
func (r *relaxState) marked(column string) bool { return r != nil && r.columns[column] }

// markedColumns gives the columns with the marker of a relax, or nil.
func (r *relaxState) markedColumns() map[string]bool {
	if r == nil {
		return nil
	}
	return r.columns
}

// checkColumn gives the column whose CHECK the manifest's constraint name is, when that column has the marker.
func (r *relaxState) checkColumn(name string) (string, bool) {
	if r == nil {
		return "", false
	}
	for column := range r.columns {
		if CheckName(r.table, column) == name {
			return column, true
		}
	}
	return "", false
}

// acceptNamed tells whether the CHECK want of the manifest may be as the table has it: got, or missing when ok is
// false, with the CHECKs of the table checks.
func (r *relaxState) acceptNamed(want Constraint, got constraintRow, ok bool, checks map[string]constraintRow) bool {
	column, marked := r.checkColumn(want.Name)
	if !marked || want.Kind != KindCheck {
		return false
	}
	if ok {
		return got.kind == KindCheck && got.validated && !strings.HasSuffix(got.definition, " NOT VALID") && relaxedCheck(want.Definition, got.definition, column)
	}
	if relax, there := checks[RelaxCheckName(r.table, column)]; there {
		return relax.kind == KindCheck && relaxedCheck(want.Definition, relax.definition, column)
	}
	_, values := checkValues(want.Definition, column)
	return values
}

// acceptRelax tells whether the CHECK n that the manifest does not name is the CHECK RelaxCheckName of a marked
// column in a form that the manifest's CHECK of that column accepts.
func (r *relaxState) acceptRelax(n string, got constraintRow) bool {
	if r == nil || got.kind != KindCheck {
		return false
	}
	for column := range r.columns {
		if RelaxCheckName(r.table, column) != n {
			continue
		}
		want, named := r.named[CheckName(r.table, column)]
		return named && want.Kind == KindCheck && relaxedCheck(want.Definition, got.definition, column)
	}
	return false
}

// constraintRow is one constraint of a table as the catalog gives it.
type constraintRow struct {
	kind, definition string
	validated        bool
}
