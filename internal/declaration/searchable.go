package declaration

import (
	"fmt"

	"github.com/910-software-solutions/stego/internal/types"
)

// validateSearchable holds the declaration to ST-DECL-INV107 (decision ST-DECL-Q50). The searchable setting of a
// field, when it is given, is on a string field. searchable: false is valid on each string field. searchable: true
// is valid only on a public string field whose max_length cannot pass the size of a search index entry
// (types.SearchIndexBytes in UTF-8, so at most 500 characters): a longer value would fail the write of its index
// entry, and a pii or secret field is never searched. Each field gets one fault at most, at the path of the setting,
// that names the field and the rule.
func validateSearchable(service *types.ServiceDeclaration) []ValidationError {
	var errs []ValidationError
	for ei, e := range service.Entities {
		for fi, f := range e.Fields {
			if f.Searchable == nil {
				continue
			}
			found := fmt.Sprintf("%t", *f.Searchable)
			reason := ""
			switch {
			case f.Type != types.FieldTypeString:
				reason = fmt.Sprintf("searchable is a setting of a string field, and the field is of the type %s", f.Type)
			case !*f.Searchable:
			case !f.Public():
				reason = fmt.Sprintf("a %s field is never searched, so searchable: true does not suit it; leave searchable out", f.Sensitivity)
			case f.LongText():
				reason = fmt.Sprintf("its max_length %d can pass the %d bytes of a search index entry in UTF-8 (%d bytes a character), so the write of a long value would fail; leave searchable out, or set a max_length of at most %d",
					*f.MaxLength, types.SearchIndexBytes, types.UTF8Bytes, types.SearchIndexBytes/types.UTF8Bytes)
			}
			if reason != "" {
				errs = append(errs, ValidationError{
					Category: "field-type", Rule: "searchable", Path: fieldPath(ei, fi) + ".searchable", Found: found,
					Message: fmt.Sprintf("entity %q field %q has searchable: %s, but %s", e.Name, f.Name, found, reason),
				})
			}
		}
	}
	return errs
}
