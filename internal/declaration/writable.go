package declaration

import (
	"fmt"
	"slices"

	"github.com/910-software-solutions/stego/internal/types"
)

// validateWritable holds the declaration to ST-DECL-INV100. The writable setting of a field, when it is given, is
// always, create or never. A computed field and a timestamp field that is not client_set do not set create or never,
// because no collection write sets them (ST-SUP-RC20-OC17). A field with writable never is optional or has a
// default, because a create stores its default. A patchable list names no field with writable create or never. Each
// field gets one fault at most, at the path of its setting or of the patchable entry, that names the field and the
// rule.
func validateWritable(service *types.ServiceDeclaration) []ValidationError {
	var errs []ValidationError
	kept := map[string]map[string]string{}
	for ei, e := range service.Entities {
		for fi, f := range e.Fields {
			if f.Writable == "" {
				continue
			}
			path := fieldPath(ei, fi) + ".writable"
			reason := ""
			switch {
			case !slices.Contains(types.WritableValues, f.Writable):
				errs = append(errs, ValidationError{
					Category: "field-type", Rule: "writable", Path: path, Found: f.Writable, Allowed: types.WritableValues,
					Message: fmt.Sprintf("entity %q field %q has writable %q, which is not always, create or never", e.Name, f.Name, f.Writable),
				})
				continue
			case f.Writable == types.WritableAlways:
				continue
			case f.Computed:
				reason = "a computed field is filled by its fill, and no collection write sets it"
			case f.Type == types.FieldTypeTimestamp && !f.ClientSet:
				reason = "a timestamp that is not client_set is set by the server, and no collection write sets it"
			case f.Writable == types.WritableNever && !f.Optional && f.Default == nil:
				reason = "a create stores the default of a never field, so the field must be optional or have a default"
			}
			if reason != "" {
				errs = append(errs, ValidationError{
					Category: "field-type", Rule: "writable", Path: path, Found: f.Writable,
					Message: fmt.Sprintf("entity %q field %q has writable %s, but %s", e.Name, f.Name, f.Writable, reason),
				})
				continue
			}
			if kept[e.Name] == nil {
				kept[e.Name] = map[string]string{}
			}
			kept[e.Name][f.Name] = f.Writable
		}
	}
	for _, c := range service.Collections {
		reported := map[string]bool{}
		for pi, field := range c.Patchable {
			writable, ok := kept[c.Entity][field]
			if !ok || reported[field] {
				continue
			}
			reported[field] = true
			errs = append(errs, ValidationError{
				Category: "collection", Rule: "writable", Path: fmt.Sprintf("%s.patchable[%d]", collectionPath(c.Name), pi), Found: field,
				Message: fmt.Sprintf("collection %q: patchable field %q has writable %s — a collection write cannot change it, so a patch cannot name it", c.Name, field, writable),
			})
		}
	}
	return errs
}
