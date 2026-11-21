package declaration

import (
	"fmt"

	"github.com/jsell-rh/stego/internal/types"
)

// maxAttemptsLimit is the highest limit of wrong checks: the most failed attempts that a verifier of a memorized
// secret may allow on one account (NIST SP 800-63B, 5.2.2).
const maxAttemptsLimit = 100

// validateAttempts enforces ST-DECL-INV104 (decision ST-SUP-Q185 (a), security review of PR #451, H1): attempts is
// only on a field of sensitivity secret that is compare-only, with a limit from 1 to 100. Each fault names the field.
func validateAttempts(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	for i, entity := range service.Entities {
		for j, f := range entity.Fields {
			if f.Attempts == nil {
				continue
			}
			path := fmt.Sprintf("entities[%d].fields[%d].attempts", i, j)
			fault := func(why string) {
				faults = append(faults, ValidationError{Category: "field-type", Rule: "attempts-limit", Path: path, Found: f.Name,
					Message: fmt.Sprintf("the field %q of the entity %q %s (ST-DECL-INV104)", f.Name, entity.Name, why)})
			}
			if !f.Secret() || !f.CompareOnly {
				fault("declares attempts and is not a compare-only secret; only the check of a compare-only secret has wrong attempts")
			}
			if a := f.Attempts.Limit; a < 1 || a > maxAttemptsLimit {
				fault(fmt.Sprintf("has the attempts limit %d; give a whole number from 1 to %d", a, maxAttemptsLimit))
			}
		}
	}
	return faults
}
