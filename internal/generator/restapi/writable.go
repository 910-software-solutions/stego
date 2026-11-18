package restapi

import (
	"fmt"

	"github.com/jsell-rh/stego/internal/types"
)

// The writable setting of a field on the runtime data path (ST-DECL-EN03, ST-DECL-Q45 (a)): a field with writable
// create or never keeps its stored value on each collection write after its create. An update, an upsert of a stored
// row and a patch refuse a body that gives such a field another value than the stored one with a 400 validation
// problem that names the field, and change nothing; a body that leaves the field out, or gives the stored value,
// keeps the stored value (ST-SUP-RC20-OC44 to ST-SUP-RC20-OC46). A create, and an upsert that inserts, store the
// declared default of a field with writable never, and refuse another value. Each check reads the stored row in the
// transaction of the command, after the locked read, so the compare and the write see one row.

// keptFields gives the fields of the collection's entity that a collection write cannot change.
func (w *runtimeHandlerWriter) keptFields() []types.Field {
	var fields []types.Field
	for _, f := range w.c.entity.Fields {
		if keptField(f) {
			fields = append(fields, f)
		}
	}
	return fields
}

// checksKept tells whether a write of the collection checks a field that a collection write cannot change.
func (w *runtimeHandlerWriter) checksKept() bool {
	c := w.c
	return len(w.keptFields()) > 0 && (c.ops[types.OpCreate] || c.ops[types.OpUpdate] || c.ops[types.OpUpsert] || c.ops[types.OpPatch])
}

// keptJSON tells whether the entity has a jsonb field that a collection write cannot change, which the check
// compares as JSON values.
func keptJSON(e types.Entity) bool {
	for _, f := range e.Fields {
		if keptField(f) && f.Type == types.FieldTypeJsonb {
			return true
		}
	}
	return false
}

// keptCheck writes <collection>Kept, the check of a body against kept, the value that the row keeps: the stored row,
// or on a create the new row with the default of each field with writable never, whose fields with writable create it
// does not check. The JSON decoder matches a member name to a field with no regard to case, so each member whose name
// is the field's name in any case is checked. A null member is a value: it keeps only an optional field that holds
// none. The problem names each refused field once, in the order of the declaration, and quotes no value.
func (w *runtimeHandlerWriter) keptCheck() {
	if !w.checksKept() {
		return
	}
	c, e := w.c, w.c.entity
	w.f("// %sKept refuses a body that gives a field of %s that a collection write cannot change another value than kept\n", c.camel, e.Name)
	w.f("// holds: the stored row, or on a create (create) the new row, whose fields with writable create it does not\n")
	w.f("// check. It names each such field in a 400 validation problem (ST-SUP-RC20-OC44 to ST-SUP-RC20-OC46).\n")
	w.f("func %sKept(body []byte, kept %s, create bool) error {\n", c.camel, e.Name)
	w.f("\tvar rawMap map[string]json.RawMessage\n\tif err := json.Unmarshal(body, &rawMap); err != nil {\n\t\treturn &invalidRequest{problem: BadRequest(\"the body is not a JSON object\")}\n\t}\n")
	w.f("\tmessage := %q\n\tif create {\n\t\tmessage = %q\n\t}\n",
		"a collection write cannot change this field: leave it out or give its stored value",
		"a collection write cannot set this field: leave it out or give its default")
	w.f("\tvar refused []ValidationError\n")
	for _, f := range w.keptFields() {
		name := toPascalCase(f.Name)
		indent := "\t"
		if f.Writable == types.WritableCreate {
			w.f("\tif !create {\n")
			indent = "\t\t"
		}
		w.f("%sfor member, raw := range rawMap {\n%s\tif !strings.EqualFold(member, %q) {\n%s\t\tcontinue\n%s\t}\n", indent, indent, f.Name, indent, indent)
		var differs string
		if f.Type == types.FieldTypeJsonb {
			if f.Optional {
				w.f("%s\tvar stored []byte\n%s\tif kept.%s != nil {\n%s\t\tstored = *kept.%s\n%s\t}\n", indent, indent, name, indent, name, indent)
			} else {
				w.f("%s\tstored := []byte(kept.%s)\n", indent, name)
			}
			differs = fmt.Sprintf("!%sSameJSON(raw, stored)", c.camel)
		} else {
			w.f("%s\tvar given *%s\n", indent, fieldTypeToGo(f.Type))
			same := func(a, b string) string {
				switch f.Type {
				case types.FieldTypeRef:
					return fmt.Sprintf("canonicalID(%s) != %s", a, b)
				case types.FieldTypeTimestamp:
					return fmt.Sprintf("!(%s).Equal(%s)", a, b)
				}
				return fmt.Sprintf("%s != %s", a, b)
			}
			if f.Optional {
				differs = fmt.Sprintf("json.Unmarshal(raw, &given) != nil || (given == nil) != (kept.%s == nil) || given != nil && %s", name, same("*given", "*kept."+name))
			} else {
				differs = fmt.Sprintf("json.Unmarshal(raw, &given) != nil || given == nil || %s", same("*given", "kept."+name))
			}
		}
		w.f("%s\tif %s {\n%s\t\trefused = append(refused, ValidationError{Field: %q, Message: message})\n%s\t\tbreak\n%s\t}\n%s}\n", indent, differs, indent, f.Name, indent, indent, indent)
		if f.Writable == types.WritableCreate {
			w.f("\t}\n")
		}
	}
	w.f("\tif len(refused) > 0 {\n\t\treturn &invalidRequest{problem: Validation(refused)}\n\t}\n\treturn nil\n}\n\n")
	if keptJSON(e) {
		w.f("// %sSameJSON tells whether the JSON member raw holds the same JSON value as stored; a nil stored is null.\n", c.camel)
		w.f("func %sSameJSON(raw, stored []byte) bool {\n\tif stored == nil {\n\t\treturn strings.TrimSpace(string(raw)) == \"null\"\n\t}\n", c.camel)
		w.f("\tvar given, held any\n\tif json.Unmarshal(raw, &given) != nil || json.Unmarshal(stored, &held) != nil {\n\t\treturn false\n\t}\n\treturn reflect.DeepEqual(given, held)\n}\n\n")
	}
}

// keptCreate writes, in the fill of a create or of an upsert that inserts, the value of each field that a collection
// write cannot change: a field with writable never takes its declared default, or none, and the body may give only
// that value; a required client-set timestamp with writable create must be given, as the decode checks it for each
// other such field (ST-SUP-RC20-OC45).
func (w *runtimeHandlerWriter) keptCreate() {
	if !w.checksKept() {
		return
	}
	c := w.c
	never := false
	for _, f := range w.keptFields() {
		name := toPascalCase(f.Name)
		switch {
		case f.Writable == types.WritableCreate:
			if f.ClientSetTime() && !f.Optional {
				w.f("\t\tif %s.%s.IsZero() {\n\t\t\treturn idempotency.Response{}, &invalidRequest{problem: BadRequest(%q)}\n\t\t}\n", c.lower, name,
					f.Name+" is required: an RFC 3339 time after the zero time, such as "+clientTimeExample)
			}
		case f.Default != nil:
			never = true
			// ValidateContext refused a default that has no Go literal (generationDefaults).
			literal, _ := defaultLiteral(f)
			if f.Optional {
				w.f("\t\t%s.%s = ptrOf(%s(%s))\n", c.lower, name, fieldTypeToGo(f.Type), literal)
			} else {
				w.f("\t\t%s.%s = %s\n", c.lower, name, literal)
			}
		default:
			never = true
			// A never field with no default is optional (ST-DECL-INV100): a create stores none.
			w.f("\t\t%s.%s = nil\n", c.lower, name)
		}
	}
	if never {
		w.f("\t\tif err := %sKept(body, %s, true); err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", c.camel, c.lower)
	}
}

// keptUpdate writes, in the fill of an update or of an upsert of a stored row, after the locked read of stored, the
// check of the body against the stored row and the stored value of each field that a collection write cannot change
// (ST-SUP-RC20-OC44, ST-SUP-RC20-OC45).
func (w *runtimeHandlerWriter) keptUpdate() {
	if !w.checksKept() {
		return
	}
	c := w.c
	w.f("\t\tif err := %sKept(body, stored, false); err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", c.camel)
	for _, f := range w.keptFields() {
		name := toPascalCase(f.Name)
		w.f("\t\t%s.%s = stored.%s\n", c.lower, name, name)
	}
}
