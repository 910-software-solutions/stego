package declaration

import (
	"fmt"

	"github.com/jsell-rh/stego/internal/types"
)

// maxAttemptsLimit is the highest limit of a counter of wrong attempts: the most failed attempts that a verifier of a
// memorized secret may allow on one account (NIST SP 800-63B, 5.2.2).
const maxAttemptsLimit = 100

// validateAttempts enforces ST-DECL-INV104 (decision ST-SUP-Q185 (a)): a field that declares attempts counts another
// field of the same entity that is a compare-only secret, with a limit from 1 to 100; the counter is a required int32
// with the default 0, the minimum 0 and no maximum, writable never, public and not in a key; and a compare-only field
// has at most one counter. Each fault names the counter field.
func validateAttempts(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	for i, entity := range service.Entities {
		byName := map[string]types.Field{}
		for _, f := range entity.Fields {
			byName[f.Name] = f
		}
		counted := map[string]string{}
		for j, f := range entity.Fields {
			if f.Attempts == nil {
				continue
			}
			path := fmt.Sprintf("entities[%d].fields[%d].attempts", i, j)
			fault := func(why string) {
				faults = append(faults, ValidationError{Category: "field-type", Rule: "attempts-counter", Path: path, Found: f.Name,
					Message: fmt.Sprintf("the counter field %q of the entity %q %s (ST-DECL-INV104)", f.Name, entity.Name, why)})
			}
			a := f.Attempts
			target, ok := byName[a.Counts]
			switch {
			case a.Counts == f.Name:
				fault("counts itself; name the compare-only secret field whose wrong attempts it counts")
			case !ok:
				fault(fmt.Sprintf("counts %q, which the entity does not declare", a.Counts))
			case !target.Secret() || !target.CompareOnly:
				fault(fmt.Sprintf("counts %q, which is not a compare-only secret field; only the check of a compare-only secret has wrong attempts", a.Counts))
			case counted[a.Counts] != "":
				fault(fmt.Sprintf("counts %q, which the field %q counts already; a compare-only field has one counter", a.Counts, counted[a.Counts]))
			default:
				counted[a.Counts] = f.Name
			}
			if a.Limit < 1 || a.Limit > maxAttemptsLimit {
				fault(fmt.Sprintf("has the limit %d; give a whole number from 1 to %d", a.Limit, maxAttemptsLimit))
			}
			if f.Type != types.FieldTypeInt32 {
				fault(fmt.Sprintf("has the type %s; a counter is an int32", f.Type))
			}
			if f.Optional {
				fault("is optional; a counter always has a count")
			}
			if !zeroDefault(f.Default) {
				fault("needs the default 0, the count of a new row")
			}
			if f.Min == nil || *f.Min != 0 {
				fault("needs the minimum 0")
			}
			if f.Max != nil {
				fault("has a maximum; the limit bounds the count")
			}
			if f.Writable != types.WritableNever {
				fault("needs writable: never; only a command sets the count")
			}
			if f.Sensitivity != "" && f.Sensitivity != "public" {
				fault("has the sensitivity " + f.Sensitivity + "; a counter is public")
			}
			if f.Unique || len(f.UniqueComposite) > 0 || f.UniqueWhenLive {
				fault("is unique or in a unique key; a counter is no key")
			}
		}
	}
	return faults
}

// zeroDefault tells whether a default, as YAML reads it, is the number 0.
func zeroDefault(value any) bool {
	switch v := value.(type) {
	case int:
		return v == 0
	case float64:
		return v == 0
	case string:
		return v == "0"
	}
	return false
}
