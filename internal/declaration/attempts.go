package declaration

import (
	"fmt"
	"sort"
	"strings"
	"time"

	core "github.com/authzed/spicedb/pkg/proto/core/v1"

	"github.com/jsell-rh/stego/internal/types"
)

// maxAttemptsLimit is the highest limit of wrong checks: the most failed attempts that a verifier of a memorized
// secret may allow on one account (NIST SP 800-63B, 5.2.2).
const maxAttemptsLimit = 100

// The shortest and the longest window of the decay of a count of wrong checks (ST-DECL-INV104, decisions ST-SUP-Q205
// (c) and ST-SUP-Q207 (a)). guessableLength is the longest min_length of a secret for which a window shorter than
// maxUnlockAfter gives a warning.
const (
	minUnlockAfter  = time.Minute
	maxUnlockAfter  = 24 * time.Hour
	guessableLength = 6
)

// validateAttempts enforces ST-DECL-INV104 (decision ST-SUP-Q185 (a), security review of PR #451, H1; decision
// ST-SUP-Q205 (c) and ST-SUP-Q207 (a)): attempts is only on a field of sensitivity secret that is compare-only, with a
// limit from 1 to 100 and no unlock_after or one from 1m to 24h, and each reset of attempts of a command passes validateResets. Each fault names
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
			// The window of the decay is opt-in (decisions ST-SUP-Q205 (c) and ST-SUP-Q207 (a)): a row at its limit
			// opens again after it, and not so soon that it lets many guesses in a day.
			if u, err := f.Attempts.UnlockAfterDuration(); f.Attempts.UnlockAfter != "" && (err != nil || u < minUnlockAfter || u > maxUnlockAfter || u%time.Second != 0) {
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

// AttemptsWarnings warns of each field with attempts whose unlock_after is shorter than 24h while its min_length is at
// most 6, such as a PIN of 4 digits (decision ST-SUP-Q207 (a)): the decay lets limit checks again in each window, up to
// the cap of the failures, so a short secret with a short window is open to a guesser. The warning names the field
// and the window, and no value.
func AttemptsWarnings(service *types.ServiceDeclaration) []ValidationError {
	var warnings []ValidationError
	for i, entity := range service.Entities {
		for j, f := range entity.Fields {
			if f.Attempts == nil || f.Attempts.UnlockAfter == "" {
				continue
			}
			u, err := f.Attempts.UnlockAfterDuration()
			if err != nil || u >= maxUnlockAfter || (f.MinLength != nil && *f.MinLength > guessableLength) {
				continue
			}
			warnings = append(warnings, ValidationError{Category: "field-type", Rule: "attempts-guessable",
				Path: fmt.Sprintf("entities[%d].fields[%d].attempts.unlock_after", i, j), Found: f.Attempts.UnlockAfter,
				Message: fmt.Sprintf("the field %q of the entity %q may be %d characters or fewer, and its count of wrong checks decays after %s: a guesser gets %d checks in each window, up to %d; give unlock_after: 24h, no unlock_after, or a min_length above %d (ST-DECL-INV104)",
					f.Name, entity.Name, guessableLength, f.Attempts.UnlockAfter, f.Attempts.Limit, min(10*f.Attempts.Limit, maxAttemptsLimit), guessableLength)})
		}
	}
	return warnings
}

// validateResetSchema enforces the permission of its own of ST-DECL-INV104 on the compiled schema (security review of
// PR #489, M2 and second review, M2b). It expands the permission of a command that declares resets_attempts, and each
// permission that another operation on its resource type checks, into their leaves: each relation, and each arrow
// with the relations and arrows that its computed name expands to on each subject type of its tupleset, one level
// deep; a permission that an expression names is expanded in its place, once, so a cycle ends. It refuses the reset
// permission when its expansion names a checking permission, when a checking permission's expansion names the reset
// permission, and when the two expansions share a leaf. Thus unlock_pin = customer->edit beside operate =
// customer->edit, an alias of a part of operate, and operate = parent->viewer + unlock_pin are all refused.
func validateResetSchema(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	if schema == nil {
		return nil
	}
	var faults []ValidationError
	for i, c := range service.Commands {
		if len(c.ResetsAttempts) == 0 || c.Permission == "" || c.ResourceType == "" {
			continue
		}
		if _, ok := schema.PermissionRewrite(c.ResourceType, c.Permission); !ok {
			continue
		}
		own := schema.expandPermission(c.ResourceType, c.Permission)
		for _, other := range checkingPermissions(service, i, c.ResourceType) {
			if other == c.Permission {
				continue
			}
			theirs := schema.expandPermission(c.ResourceType, other)
			shared := ""
			switch {
			case own.permissions[other]:
				shared = "names " + other + ","
			case theirs.permissions[c.Permission]:
				shared = "is named by " + other + ","
			default:
				for _, leaf := range sortedKeys(own.leaves) {
					if theirs.leaves[leaf] {
						shared = "shares " + leaf + " with " + other + ","
						break
					}
				}
			}
			if shared != "" {
				faults = append(faults, ValidationError{Category: "command", Rule: "resets-attempts", Path: commandPath(i) + ".permission", Found: c.Permission,
					Message: fmt.Sprintf("the command %q resets attempts with the permission %s, which in the schema %s a permission that another operation on %s checks; give the reset a permission with relations of its own, such as a support relation (ST-DECL-INV104)", c.Name, c.Permission, shared, c.ResourceType)})
				break
			}
		}
	}
	return faults
}

// expansion is what a permission expands to: its leaves, each relation as <type>#<relation> and each arrow as
// <type>#<tupleset>-><computed>, and the permissions of its own type that it names on the way.
type expansion struct {
	leaves      map[string]bool
	permissions map[string]bool
}

// expandPermission expands the permission of the type into its leaves (validateResetSchema): through each permission
// of the type that it names, once each, and through each arrow one level into each subject type of its tupleset. The
// arms of an intersection and of an exclusion count too, so the check refuses rather than misses a shared leaf.
func (s *AuthzSchema) expandPermission(typeName, permission string) expansion {
	x := expansion{leaves: map[string]bool{}, permissions: map[string]bool{}}
	var walk func(typeName, name string, follow bool, visited map[string]bool)
	walk = func(typeName, name string, follow bool, visited map[string]bool) {
		definition, ok := s.definitions[typeName]
		if !ok {
			return
		}
		if !definition.permissions[name] {
			x.leaves[typeName+"#"+name] = true
			return
		}
		if visited[typeName+"#"+name] {
			return
		}
		visited[typeName+"#"+name] = true
		var rewrite func(r *core.UsersetRewrite)
		rewrite = func(r *core.UsersetRewrite) {
			var children []*core.SetOperation_Child
			switch {
			case r.GetUnion() != nil:
				children = r.GetUnion().GetChild()
			case r.GetIntersection() != nil:
				children = r.GetIntersection().GetChild()
			case r.GetExclusion() != nil:
				children = r.GetExclusion().GetChild()
			}
			for _, c := range children {
				var tupleset, computed string
				switch {
				case c.GetComputedUserset() != nil:
					computed := c.GetComputedUserset().GetRelation()
					if follow && definition.permissions[computed] {
						x.permissions[computed] = true
					}
					walk(typeName, computed, follow, visited)
					continue
				case c.GetTupleToUserset() != nil:
					tupleset, computed = c.GetTupleToUserset().GetTupleset().GetRelation(), c.GetTupleToUserset().GetComputedUserset().GetRelation()
				case c.GetFunctionedTupleToUserset() != nil:
					f := c.GetFunctionedTupleToUserset()
					tupleset, computed = f.GetTupleset().GetRelation(), f.GetComputedUserset().GetRelation()
				case c.GetUsersetRewrite() != nil:
					rewrite(c.GetUsersetRewrite())
					continue
				default:
					continue
				}
				x.leaves[typeName+"#"+tupleset+"->"+computed] = true
				if follow {
					for _, subject := range definition.subjects[tupleset] {
						walk(subject.namespace, computed, false, map[string]bool{})
					}
				}
			}
		}
		rewrite(definition.rewrites[name])
	}
	walk(typeName, permission, true, map[string]bool{})
	return x
}

// checkingPermissions gives the permissions on resourceType that another operation than the command at index self
// checks: the operations of each entity of that resource type, and each other command, query and worker on it, in
// order and once each.
func checkingPermissions(service *types.ServiceDeclaration, self int, resourceType string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && p != "public" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, e := range service.Entities {
		if e.ResourceType != resourceType {
			continue
		}
		ops := make([]string, 0, len(e.Permissions))
		for op := range e.Permissions {
			ops = append(ops, string(op))
		}
		sort.Strings(ops)
		for _, op := range ops {
			add(e.Permissions[types.Operation(op)])
		}
	}
	for i, c := range service.Commands {
		if i != self && c.ResourceType == resourceType {
			add(c.Permission)
		}
	}
	for _, q := range service.Queries {
		if q.ResourceType == resourceType {
			add(q.Permission)
		}
	}
	for _, w := range service.Workers {
		if w.ResourceType == resourceType {
			add(w.Permission)
		}
	}
	return out
}
