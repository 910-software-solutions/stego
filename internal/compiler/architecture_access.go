package compiler

import (
	"fmt"
	"slices"
	"strings"

	core "github.com/authzed/spicedb/pkg/proto/core/v1"

	"github.com/910-software-solutions/stego/internal/declaration"
)

// The grant paths of the access matrix (ST-GEN-UC08-R13): each permission that an operation checks is expanded
// statically from the declared authorization schema, never from a relationship, which is data. A path goes from the
// permission through each relation, arrow and userset to a subject type and its relation, and states each kind that
// limits it: an arrow to a parent, a caveat, an expiration, an intersection with another relation, an exclusion, a
// wildcard. A recursion, the depth limit, the work limit or the path limit stops a path, and the path says so; the
// expansion never guesses past it.

// schemaSource is the source of each grant path and role: the declared schema.
var schemaSource = ArchSource{Declaration: "authorization.schema_path"}

// maxGrantDepth is the most relations and permissions a grant path passes before it stops at the depth limit.
const maxGrantDepth = 12

// maxGrantPaths is the most grant paths one permission gets; the expansion stops with a stated limit after it.
const maxGrantPaths = 512

// maxGrantSteps bounds the work of the expansion of one permission: each node and expression part it visits is one
// step. A wide schema whose arrows fan out on every level would otherwise cost the fan-out to the power of the depth;
// at the bound the expansion stops with a stated work limit (security review of #341, M1).
const maxGrantSteps = 20000

// pathState is what a path carries from the permission to the node it has reached.
type pathState struct {
	hops, kinds, requires, except, caveats []string
}

// with gives a copy of the list with the items added, so that sibling branches never share a backing array.
func with(list []string, items ...string) []string {
	return append(slices.Clip(list), items...)
}

// grantPath is one path as the expansion finds it.
type grantPath struct {
	role  *ArchRole
	state pathState
	stops string
}

// grantExpander expands the permissions of one schema.
type grantExpander struct {
	schema *declaration.AuthzSchema
	paths  []grantPath
	// steps counts the work done; full is set when the path cap or the work limit stops the expansion, which then
	// returns at once from every node.
	steps int
	full  bool
	// complete is false when a limit cut a path short, so the permission may grant more roles than the paths name.
	complete bool
}

// grantPaths gives the grant paths of the permission of the resource type, sorted by role and hops, each with its
// ID under the permission's ID, the roles they grant, and whether no limit cut a path short.
func grantPaths(schema *declaration.AuthzSchema, resourceType, permission string) ([]ArchGrant, []ArchRole, bool) {
	e := &grantExpander{schema: schema, complete: true}
	e.expand(resourceType, permission, nil, pathState{})
	slices.SortStableFunc(e.paths, func(a, b grantPath) int {
		if c := strings.Compare(roleKey(a.role), roleKey(b.role)); c != 0 {
			return c
		}
		return strings.Compare(strings.Join(a.state.hops, " "), strings.Join(b.state.hops, " "))
	})
	grants := make([]ArchGrant, 0, len(e.paths))
	var roles []ArchRole
	for i, path := range e.paths {
		kinds := slices.Clone(path.state.kinds)
		if len(kinds) == 0 {
			kinds = []string{"direct"}
		}
		slices.Sort(kinds)
		grant := ArchGrant{ID: fmt.Sprintf("grant:%s#%s.%03d", resourceType, permission, i+1), Kinds: slices.Compact(kinds), Hops: path.state.hops,
			Caveat: strings.Join(path.state.caveats, ", "), Requires: path.state.requires, Except: path.state.except, Stops: path.stops, Source: schemaSource}
		if path.role != nil {
			grant.Role = path.role.ID
			roles = append(roles, *path.role)
		}
		grants = append(grants, grant)
	}
	return grants, roles, e.complete
}

// roleKey orders paths by their role; a path with no role, which a recursion or a limit stops, sorts last.
func roleKey(role *ArchRole) string {
	if role == nil {
		return "~"
	}
	return role.ID
}

// newRole gives the role of a subject type that holds the relation on the resource type, under the caveat and the
// expiration the relation requires of it; a subject with and without a caveat are two roles.
func newRole(on, relation string, subject declaration.SchemaSubject) *ArchRole {
	name := subject.Type
	if subject.Wildcard {
		name += ":*"
	}
	if subject.Caveat != "" {
		name += "(" + subject.Caveat + ")"
	}
	if subject.Expiration {
		name += "(expiration)"
	}
	return &ArchRole{ID: "role:" + on + "#" + relation + "@" + name, SubjectType: subject.Type, Relation: relation, On: on, Wildcard: subject.Wildcard,
		Caveat: subject.Caveat, Expiration: subject.Expiration, Source: schemaSource}
}

// step counts one unit of work, and stops the expansion with a stated work limit at the bound. It tells whether the
// expansion goes on.
func (e *grantExpander) step(state pathState) bool {
	if e.full {
		return false
	}
	e.steps++
	if e.steps > maxGrantSteps {
		e.stop(state, "work limit", fmt.Sprintf("the work limit of %d steps; the rest of the permission is not expanded", maxGrantSteps))
		return false
	}
	return true
}

// stop ends the expansion with one path that states the limit, of the kind of that limit: the work limit or the path
// limit, so the mark and the text of the path name the same limit (security review of PR #388, L1).
func (e *grantExpander) stop(state pathState, kind, why string) {
	e.full, e.complete = true, false
	state.kinds = with(state.kinds, kind)
	e.paths = append(e.paths, grantPath{state: state, stops: why})
}

// add records one path, or stops the expansion at the path cap.
func (e *grantExpander) add(path grantPath) {
	if e.full {
		return
	}
	if len(e.paths) >= maxGrantPaths {
		e.stop(path.state, "path limit", fmt.Sprintf("the path limit of %d paths; the rest of the permission is not expanded", maxGrantPaths))
		return
	}
	e.paths = append(e.paths, path)
}

// subjectState gives the state of a path past a subject: its wildcard, caveat and expiration. An expiration makes the
// grant conditional, as a caveat does.
func subjectState(state pathState, subject declaration.SchemaSubject) pathState {
	if subject.Wildcard {
		state.kinds = with(state.kinds, "wildcard")
	}
	if subject.Caveat != "" {
		state.kinds = with(state.kinds, "caveat")
		state.caveats = with(state.caveats, subject.Caveat)
	}
	if subject.Expiration {
		state.kinds = with(state.kinds, "expiration")
	}
	return state
}

// expand follows the relation or permission name of the type. stack holds the type#name of each node on the way
// from the permission, so that a node met again is a recursion.
func (e *grantExpander) expand(typeName, name string, stack []string, state pathState) {
	node := typeName + "#" + name
	state.hops = with(state.hops, node)
	if !e.step(state) {
		return
	}
	if slices.Contains(stack, node) {
		state.kinds = with(state.kinds, "recursion")
		e.add(grantPath{state: state, stops: "recursion: " + node + " grants through itself, and the path is not expanded again"})
		return
	}
	// The depth counts the nodes of the path; an arrow is a hop of the path but no node.
	if len(stack) >= maxGrantDepth {
		// A recursion meets a node whose roles the path already expands; the depth limit can hide roles.
		e.complete = false
		state.kinds = with(state.kinds, "depth limit")
		e.add(grantPath{state: state, stops: fmt.Sprintf("the depth limit of %d nodes at %s", maxGrantDepth, node)})
		return
	}
	stack = with(stack, node)
	if rewrite, isPermission := e.schema.PermissionRewrite(typeName, name); isPermission {
		e.rewrite(typeName, rewrite, stack, state)
		return
	}
	subjects, _ := e.schema.RelationSubjects(typeName, name)
	for _, subject := range subjects {
		if e.full {
			return
		}
		next := subjectState(state, subject)
		// The schema compiler gives a plain subject type the relation "...".
		if subject.Relation != "" && subject.Relation != "..." && !subject.Wildcard {
			// A userset subject such as group#member: the holders of that relation get the relation.
			next.kinds = with(next.kinds, "userset")
			e.expand(subject.Type, subject.Relation, stack, next)
			continue
		}
		e.add(grantPath{role: newRole(typeName, name, subject), state: next})
	}
}

// rewrite follows the expression of a permission of the type.
func (e *grantExpander) rewrite(typeName string, rewrite *core.UsersetRewrite, stack []string, state pathState) {
	switch {
	case rewrite.GetUnion() != nil:
		for _, child := range rewrite.GetUnion().GetChild() {
			e.child(typeName, child, stack, state)
		}
	case rewrite.GetIntersection() != nil:
		children := rewrite.GetIntersection().GetChild()
		for i, child := range children {
			var others []string
			for j, other := range children {
				if j != i {
					others = append(others, typeName+"#"+childText(other))
				}
			}
			next := state
			next.kinds, next.requires = with(state.kinds, "intersection"), with(state.requires, others...)
			e.child(typeName, child, stack, next)
		}
	case rewrite.GetExclusion() != nil:
		children := rewrite.GetExclusion().GetChild()
		if len(children) == 0 {
			return
		}
		var removed []string
		for _, other := range children[1:] {
			removed = append(removed, typeName+"#"+childText(other))
		}
		next := state
		next.kinds, next.except = with(state.kinds, "exclusion"), with(state.except, removed...)
		e.child(typeName, children[0], stack, next)
	}
}

// child follows one part of an expression.
func (e *grantExpander) child(typeName string, child *core.SetOperation_Child, stack []string, state pathState) {
	if !e.step(state) {
		return
	}
	switch {
	case child.GetComputedUserset() != nil:
		e.expand(typeName, child.GetComputedUserset().GetRelation(), stack, state)
	case child.GetTupleToUserset() != nil || child.GetFunctionedTupleToUserset() != nil:
		tupleset, computed := child.GetTupleToUserset().GetTupleset().GetRelation(), child.GetTupleToUserset().GetComputedUserset().GetRelation()
		next := state
		next.kinds = with(state.kinds, "arrow")
		if f := child.GetFunctionedTupleToUserset(); f != nil {
			tupleset, computed = f.GetTupleset().GetRelation(), f.GetComputedUserset().GetRelation()
			if f.GetFunction() == core.FunctionedTupleToUserset_FUNCTION_ALL {
				// all() needs the permission on every parent, as an intersection does.
				next.kinds = with(next.kinds, "intersection")
				next.requires = with(next.requires, typeName+"#"+tupleset+".all("+computed+") on every "+tupleset)
			}
		}
		next.hops = with(next.hops, typeName+"#"+tupleset+"->"+computed)
		subjects, _ := e.schema.RelationSubjects(typeName, tupleset)
		for _, subject := range subjects {
			if e.full {
				return
			}
			// The caveat or expiration of the tupleset subject limits each path through the arrow.
			e.expand(subject.Type, computed, stack, subjectState(next, subject))
		}
	case child.GetUsersetRewrite() != nil:
		e.rewrite(typeName, child.GetUsersetRewrite(), stack, state)
	}
}

// childText gives one part of an expression as the schema writes it, such as owner, organization->view or
// (owner & member).
func childText(child *core.SetOperation_Child) string {
	switch {
	case child.GetComputedUserset() != nil:
		return child.GetComputedUserset().GetRelation()
	case child.GetTupleToUserset() != nil:
		return child.GetTupleToUserset().GetTupleset().GetRelation() + "->" + child.GetTupleToUserset().GetComputedUserset().GetRelation()
	case child.GetFunctionedTupleToUserset() != nil:
		f := child.GetFunctionedTupleToUserset()
		function := "any"
		if f.GetFunction() == core.FunctionedTupleToUserset_FUNCTION_ALL {
			function = "all"
		}
		return f.GetTupleset().GetRelation() + "." + function + "(" + f.GetComputedUserset().GetRelation() + ")"
	case child.GetUsersetRewrite() != nil:
		rewrite := child.GetUsersetRewrite()
		operator, operation := " + ", rewrite.GetUnion()
		if rewrite.GetIntersection() != nil {
			operator, operation = " & ", rewrite.GetIntersection()
		} else if rewrite.GetExclusion() != nil {
			operator, operation = " - ", rewrite.GetExclusion()
		}
		var parts []string
		for _, part := range operation.GetChild() {
			parts = append(parts, childText(part))
		}
		return "(" + strings.Join(parts, operator) + ")"
	case child.GetXNil() != nil:
		return "nil"
	}
	return "self"
}
