package declaration

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/types"
)

// maxAttemptsLimit is the highest limit of wrong checks: the most failed attempts that a verifier of a memorized
// secret may allow on one account (NIST SP 800-63B, 5.2.2).
const maxAttemptsLimit = 100

// The shortest and the longest window of the decay of a count of wrong checks (ST-DECL-INV104, decision ST-SUP-Q205
// (c)).
const (
	minUnlockAfter = time.Minute
	maxUnlockAfter = 24 * time.Hour
)

// validateAttempts enforces ST-DECL-INV104 (decision ST-SUP-Q185 (a), security review of PR #451, H1; decision
// ST-SUP-Q205 (c)): attempts is only on a field of sensitivity secret that is compare-only, with a limit from 1 to 100
// and an unlock_after from 1m to 24h, and each reset of attempts of a command passes validateResets. Each fault names
// the field or the command.
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
			// The window of the decay (decision ST-SUP-Q205 (c)): a row at its limit opens again after it, so it is
			// never locked for ever by another principal, and not so short that it lets many guesses in a day.
			if u, err := f.Attempts.UnlockAfterDuration(); err != nil || u < minUnlockAfter || u > maxUnlockAfter || u%time.Second != 0 {
				fault(fmt.Sprintf("has the attempts unlock_after %q; give a duration in whole seconds from 1m to 24h, such as 15m", f.Attempts.UnlockAfter))
			}
		}
	}
	return append(faults, validateResets(service)...)
}

// validateResets enforces ST-DECL-INV104 (decision ST-SUP-Q205 (c)): each entry of resets_attempts of a command names
// <Entity>.<field> of a field with attempts of an entity that the command writes, only once; the command is decided on
// a row of the resource type of that entity, with a resource member; and its permission is its own: no operation of
// the entity and no other command, query or worker on that resource type checks the same permission. Thus a caller
// who may check the secret, such as with operate, cannot also reset its count, and the schema can give the reset to
// a support role only.
func validateResets(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	entities := map[string]types.Entity{}
	for _, e := range service.Entities {
		entities[e.Name] = e
	}
	for i, c := range service.Commands {
		if len(c.ResetsAttempts) == 0 {
			continue
		}
		path := commandPath(i) + ".resets_attempts"
		fault := func(found, why string) {
			faults = append(faults, ValidationError{Category: "command", Rule: "resets-attempts", Path: path, Found: found,
				Message: fmt.Sprintf("the command %q %s (ST-DECL-INV104)", c.Name, why)})
		}
		writes := map[string]bool{}
		for _, w := range c.Writes {
			writes[w] = true
		}
		seen := map[string]bool{}
		for _, r := range c.ResetsAttempts {
			name, field, _ := strings.Cut(r, ".")
			e, ok := entities[name]
			var attempts bool
			for _, f := range e.Fields {
				if f.Name == field && f.Attempts != nil {
					attempts = true
				}
			}
			switch {
			case seen[r]:
				fault(r, fmt.Sprintf("names %s twice in resets_attempts", r))
			case !ok || !attempts:
				fault(r, fmt.Sprintf("resets the attempts of %s, which is not <Entity>.<field> of a field with attempts", r))
			case !writes[name]:
				fault(r, fmt.Sprintf("resets the attempts of %s and does not write %s; add it to writes", r, name))
			case c.ResourceType == "" || c.ResourceType != e.ResourceType || c.ResourceMember == "" || c.Permission == "":
				fault(r, fmt.Sprintf("resets the attempts of %s and is not decided with a permission on a row of %s; give resource_type %q, a permission and a resource_member", r, name, e.ResourceType))
			}
			seen[r] = true
		}
		if c.Permission == "" || c.ResourceType == "" {
			continue
		}
		if other := sharedPermission(service, i, c.ResourceType, c.Permission); other != "" {
			fault(c.Permission, fmt.Sprintf("resets attempts with the permission %s, which %s also checks; give the reset a permission of its own", c.Permission, other))
		}
	}
	return faults
}

// sharedPermission names an operation other than the command at index self that checks permission on resourceType:
// an operation of an entity of that resource type, a command, a query or a worker; or empty for none.
func sharedPermission(service *types.ServiceDeclaration, self int, resourceType, permission string) string {
	for _, e := range service.Entities {
		if e.ResourceType != resourceType {
			continue
		}
		ops := make([]string, 0, len(e.Permissions))
		for op, p := range e.Permissions {
			if p == permission {
				ops = append(ops, string(op))
			}
		}
		if len(ops) > 0 {
			sort.Strings(ops)
			return fmt.Sprintf("the %s of %s", ops[0], e.Name)
		}
	}
	for i, c := range service.Commands {
		if i != self && c.ResourceType == resourceType && c.Permission == permission {
			return "the command " + c.Name
		}
	}
	for _, q := range service.Queries {
		if q.ResourceType == resourceType && q.Permission == permission {
			return "the query " + q.Name
		}
	}
	for _, w := range service.Workers {
		if w.ResourceType == resourceType && w.Permission == permission {
			return "the worker " + w.Name
		}
	}
	return ""
}
