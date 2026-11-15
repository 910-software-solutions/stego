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
// before keeps running on it. In the transaction of its ALTER TABLE, the step gives the column a comment that states
// the exact form of each attribute that it relaxes (relaxForm): RelaxMarker and the step migration on the first
// line, then type T, nullable, default D and check C or check none, one on each line. On a plain table of a declared
// entity, for a column that the manifest lists, that has such a comment and that is no column of the primary key
// and none of the fixed columns, Check accepts for each attribute only the form of the manifest or the exact form of
// the comment:
//
//   - the type of the comment, a longer character varying than the manifest's;
//   - no NOT NULL, when the comment says nullable;
//   - the default of the comment, compared by its value, where the manifest states none;
//   - the CHECK of the comment, validated, in place of the CHECK of the column that the manifest names (CheckName),
//     or that CHECK missing when the comment says check none; and, while the step runs, the manifest's CHECK
//     missing beside the CHECK RelaxCheckName of the column in the form of the comment, or of the manifest when the
//     comment states no check;
//   - the CHECK RelaxCheckName of the column, NOT VALID or validated, in that same form.
//
// Every other form stays a fault: a longer type, a dropped NOT NULL, a default or a CHECK that the comment does not
// state is refused on a marked column as on any other, and a comment that does not read as a relaxed form marks
// nothing. A release whose manifest states the relaxed form accepts only that form, so for it the comment no longer
// counts. A role that owns the table can still write such a comment by hand, as it can change the table itself.

// RelaxMarker is the first word of the comment that the relax step gives the column that it changes.
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

// relaxForm is the exact form after a relax step of each attribute that the step relaxes, as its comment states it:
// the column type, or empty; whether the column is nullable; the default as a manifest states one, or empty; and
// the CHECK definition, with none for no CHECK, or nil when the step changes no CHECK.
type relaxForm struct {
	typ, defaultValue string
	nullable          bool
	check             *string
}

// relaxStep is the grammar of the step migration that the comment names.
var relaxStep = regexp.MustCompile(`^step_[0-9]{4}_[a-z0-9]+(-[a-z0-9]+)*_expand$`)

// parseRelax reads the comment of a column as a relaxed form, and gives false for a comment that is not one: each
// line once, in the order of RelaxComment, a type of character varying, a default that is a constant of a built-in
// type, and a CHECK definition or none.
func parseRelax(comment string) (relaxForm, bool) {
	lines := strings.Split(comment, "\n")
	var form relaxForm
	if !strings.HasPrefix(lines[0], RelaxMarker+" ") || !relaxStep.MatchString(strings.TrimPrefix(lines[0], RelaxMarker+" ")) {
		return form, false
	}
	order := 0
	for _, line := range lines[1:] {
		key, value, _ := strings.Cut(line, " ")
		switch {
		case key == "type" && order < 1 && varcharType.MatchString(value):
			form.typ, order = value, 1
		case line == "nullable" && order < 2:
			form.nullable, order = true, 2
		case key == "default" && order < 3 && value != "":
			if _, typ, ok := literal(value); !ok || !builtinType.MatchString(typ) {
				return form, false
			}
			form.defaultValue, order = value, 3
		case key == "check" && order < 4 && (value == "none" || strings.HasPrefix(value, "CHECK (")):
			check := value
			if value == "none" {
				check = ""
			}
			form.check, order = &check, 4
		default:
			return form, false
		}
	}
	if order == 0 {
		return form, false
	}
	return form, true
}

// relaxedColumns reads the columns of the table rel whose comment is a relaxed form, that are no column of its
// primary key and none of the fixed columns, with their forms.
func relaxedColumns(ctx context.Context, tx *sql.Tx, rel uint32) (map[string]relaxForm, error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.attname, pg_catalog.col_description(a.attrelid, a.attnum) FROM pg_catalog.pg_attribute a
  WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped
    AND pg_catalog.starts_with(pg_catalog.col_description(a.attrelid, a.attnum), $2)
    AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_index i WHERE i.indrelid = a.attrelid AND i.indisprimary AND a.attnum = ANY (i.indkey))`,
		rel, RelaxMarker+" ")
	if err != nil {
		return nil, err
	}
	relaxed := map[string]relaxForm{}
	for rows.Next() {
		var n, comment string
		if err := rows.Scan(&n, &comment); err != nil {
			rows.Close()
			return nil, err
		}
		if form, ok := parseRelax(comment); ok && !fixedColumns[n] {
			relaxed[n] = form
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

// relaxedCheck tells whether the CHECK definition got of the column holds each enum value of the CHECK want.
func relaxedCheck(want, got, column string) bool {
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

// relaxState is what checkTable knows of the relaxed columns of one table: the forms of the marked columns and the
// CHECKs of the manifest by name.
type relaxState struct {
	table   string
	columns map[string]relaxForm
	named   map[string]Constraint
}

// acceptType tells whether the type got of the listed column want is the type of its relaxed form, a longer
// character varying than the manifest's.
func (r *relaxState) acceptType(want Column, got string) bool {
	if r == nil {
		return false
	}
	form, ok := r.columns[want.Name]
	return ok && form.typ != "" && got == form.typ && relaxedType(want.Type, form.typ)
}

// acceptNullable tells whether the relaxed form of the column makes it nullable.
func (r *relaxState) acceptNullable(column string) bool {
	if r == nil {
		return false
	}
	form, ok := r.columns[column]
	return ok && form.nullable
}

// defaults gives the default of the relaxed form of each marked column that has one, or nil.
func (r *relaxState) defaults() map[string]string {
	if r == nil {
		return nil
	}
	out := map[string]string{}
	for column, form := range r.columns {
		if form.defaultValue != "" {
			out[column] = form.defaultValue
		}
	}
	return out
}

// checkColumn gives the column whose CHECK the manifest's constraint name is, when that column has a relaxed form.
func (r *relaxState) checkColumn(name string) (string, relaxForm, bool) {
	if r == nil {
		return "", relaxForm{}, false
	}
	for column, form := range r.columns {
		if CheckName(r.table, column) == name {
			return column, form, true
		}
	}
	return "", relaxForm{}, false
}

// relaxCheck gives the CHECK definition that the relaxed form of the column states for the CHECK of the column,
// given the manifest's: the CHECK of the form, when it states a CHECK that holds each enum value of the manifest's,
// "" for no CHECK when the manifest's is a CHECK of enum values, or the manifest's when the form changes no CHECK; and
// false when the form states a CHECK that is no relax of the manifest's.
func relaxCheck(want Constraint, form relaxForm, column string) (string, bool) {
	switch {
	case form.check == nil:
		return want.Definition, true
	case *form.check == "":
		_, values := checkValues(want.Definition, column)
		return "", values
	}
	return *form.check, relaxedCheck(want.Definition, *form.check, column)
}

// acceptNamed tells whether the CHECK want of the manifest may be as the table has it: got, or missing when ok is
// false, with the CHECKs of the table checks.
func (r *relaxState) acceptNamed(want Constraint, got constraintRow, ok bool, checks map[string]constraintRow) bool {
	column, form, marked := r.checkColumn(want.Name)
	if !marked || want.Kind != KindCheck {
		return false
	}
	definition, relaxes := relaxCheck(want, form, column)
	if !relaxes {
		return false
	}
	if ok {
		return definition != "" && got.kind == KindCheck && got.validated && got.definition == definition
	}
	if relax, there := checks[RelaxCheckName(r.table, column)]; there {
		return definition != "" && relax.kind == KindCheck && strings.TrimSuffix(relax.definition, " NOT VALID") == definition
	}
	return definition == ""
}

// acceptRelax tells whether the CHECK n that the manifest does not name is the CHECK RelaxCheckName of a marked
// column in the form that its relaxed form states for the CHECK of the column.
func (r *relaxState) acceptRelax(n string, got constraintRow) bool {
	if r == nil || got.kind != KindCheck {
		return false
	}
	for column, form := range r.columns {
		if RelaxCheckName(r.table, column) != n {
			continue
		}
		want, named := r.named[CheckName(r.table, column)]
		if !named || want.Kind != KindCheck {
			return false
		}
		definition, relaxes := relaxCheck(want, form, column)
		return relaxes && definition != "" && strings.TrimSuffix(got.definition, " NOT VALID") == definition
	}
	return false
}

// sameLiteral tells whether the default expr of a column of the type typ has the value of the constant want, as a
// default that the manifest states is compared.
func sameLiteral(ctx context.Context, tx *sql.Tx, expr, want, typ string) (bool, error) {
	if expr == want {
		return true, nil
	}
	if !literalPair(expr, want) || !builtinType.MatchString(typ) {
		return false, nil
	}
	gotText, gotType, _ := literal(expr)
	wantText, wantType, _ := literal(want)
	var same bool
	err := tx.QueryRowContext(ctx, "SELECT ($1::"+gotType+")::"+typ+" IS NOT DISTINCT FROM ($2::"+wantType+")::"+typ, gotText, wantText).Scan(&same)
	return same, err
}

// constraintRow is one constraint of a table as the catalog gives it.
type constraintRow struct {
	kind, definition string
	validated        bool
}
