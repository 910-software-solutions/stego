package compiler

import (
	"fmt"
	"slices"
	"strings"

	core "github.com/authzed/spicedb/pkg/proto/core/v1"

	"github.com/jsell-rh/stego/internal/declaration"
)

// The grant paths of the access matrix (ST-GEN-UC08-R13): each permission that an operation checks is expanded
// statically from the declared authorization schema, never from a relationship, which is data. A path goes from the
// permission through each relation, arrow and userset to a subject type and its relation, and states each kind that
// limits it: an arrow to a parent, a caveat, an intersection with another relation, an exclusion, a wildcard. A
// recursion or the depth limit stops a path, and the path says so; the expansion never guesses past it.

// schemaSource is the source of each grant path and role: the declared schema.
var schemaSource = ArchSource{Declaration: "authorization.schema_path"}

// maxGrantDepth is the most hops a grant path follows before it stops at the depth limit.
const maxGrantDepth = 12

// maxGrantPaths is the most grant paths one permission gets; the expansion stops with a stated limit after it.
const maxGrantPaths = 512

// grantPath is one path as the expansion finds it.
type grantPath struct {
	role     *ArchRole
	kinds    []string
	hops     []string
	caveat   string
	requires []string
	except   []string
	stops    string
}

// grantExpander expands the permissions of one schema.
type grantExpander struct {
	schema *declaration.AuthzSchema
	paths  []grantPath
	full   bool
}

// grantPaths gives the grant paths of the permission of the resource type, sorted by role and hops, each with its
// ID under the permission's ID, and the roles they grant.
func grantPaths(schema *declaration.AuthzSchema, resourceType, permission string) ([]ArchGrant, []ArchRole) {
	e := &grantExpander{schema: schema}
	e.expand(resourceType, permission, nil, nil, nil, nil, nil)
	slices.SortFunc(e.paths, func(a, b grantPath) int {
		if c := strings.Compare(roleKey(a.role), roleKey(b.role)); c != 0 {
			return c
		}
		return strings.Compare(strings.Join(a.hops, " "), strings.Join(b.hops, " "))
	})
	grants := make([]ArchGrant, 0, len(e.paths))
	var roles []ArchRole
	for i, path := range e.paths {
		grant := ArchGrant{ID: fmt.Sprintf("grant:%s#%s.%03d", resourceType, permission, i+1), Kinds: path.kinds, Hops: path.hops,
			Caveat: path.caveat, Requires: path.requires, Except: path.except, Stops: path.stops, Source: schemaSource}
		if path.role != nil {
			grant.Role = path.role.ID
			roles = append(roles, *path.role)
		}
		grants = append(grants, grant)
	}
	return grants, roles
}

// roleKey orders paths by their role; a path with no role, which a recursion or a limit stops, sorts last.
func roleKey(role *ArchRole) string {
	if role == nil {
		return "~"
	}
	return role.ID
}

// newRole gives the role of a subject type that holds the relation on the resource type.
func newRole(on, relation string, subject declaration.SchemaSubject) *ArchRole {
	name := subject.Type
	if subject.Wildcard {
		name += ":*"
	}
	return &ArchRole{ID: "role:" + on + "#" + relation + "@" + name, SubjectType: subject.Type, Relation: relation, On: on, Wildcard: subject.Wildcard,
		Source: schemaSource}
}

// with gives a copy of the list with the items added, so that sibling branches never share a backing array.
func with(list []string, items ...string) []string {
	return append(slices.Clip(list), items...)
}

// add records one path, with its kinds sorted and given once, and direct when nothing else limits it.
func (e *grantExpander) add(path grantPath, kinds []string) {
	if len(e.paths) >= maxGrantPaths {
		if !e.full {
			e.full = true
			e.paths = append(e.paths, grantPath{kinds: []string{"depth limit"}, hops: path.hops,
				stops: fmt.Sprintf("the expansion stops at %d paths of the permission and does not expand the rest", maxGrantPaths)})
		}
		return
	}
	kinds = slices.Clone(kinds)
	if len(kinds) == 0 {
		kinds = []string{"direct"}
	}
	slices.Sort(kinds)
	path.kinds = slices.Compact(kinds)
	e.paths = append(e.paths, path)
}

// expand follows the relation or permission name of the type. stack holds the type#name of each node on the way
// from the permission, so that a node met again is a recursion.
func (e *grantExpander) expand(typeName, name string, stack, hops, kinds, requires, except []string) {
	node := typeName + "#" + name
	hops = with(hops, node)
	if slices.Contains(stack, node) {
		e.add(grantPath{hops: hops, requires: requires, except: except, stops: "recursion: " + node + " grants through itself, and the path is not expanded again"}, with(kinds, "recursion"))
		return
	}
	if len(hops) > maxGrantDepth {
		e.add(grantPath{hops: hops, requires: requires, except: except, stops: fmt.Sprintf("the depth limit of %d hops at %s", maxGrantDepth, node)}, with(kinds, "depth limit"))
		return
	}
	stack = with(stack, node)
	if rewrite, isPermission := e.schema.PermissionRewrite(typeName, name); isPermission {
		e.rewrite(typeName, rewrite, stack, hops, kinds, requires, except)
		return
	}
	subjects, _ := e.schema.RelationSubjects(typeName, name)
	for _, subject := range subjects {
		subjectKinds := kinds
		if subject.Wildcard {
			subjectKinds = with(subjectKinds, "wildcard")
		}
		if subject.Caveat != "" {
			subjectKinds = with(subjectKinds, "caveat")
		}
		// The schema compiler gives a plain subject type the relation "...".
		if subject.Relation != "" && subject.Relation != "..." && !subject.Wildcard {
			// A userset subject such as group#member: the holders of that relation get the relation.
			e.expand(subject.Type, subject.Relation, stack, hops, with(subjectKinds, "userset"), requires, except)
			continue
		}
		e.add(grantPath{role: newRole(typeName, name, subject), hops: hops, caveat: subject.Caveat, requires: requires, except: except}, subjectKinds)
	}
}

// rewrite follows the expression of a permission of the type.
func (e *grantExpander) rewrite(typeName string, rewrite *core.UsersetRewrite, stack, hops, kinds, requires, except []string) {
	switch {
	case rewrite.GetUnion() != nil:
		for _, child := range rewrite.GetUnion().GetChild() {
			e.child(typeName, child, stack, hops, kinds, requires, except)
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
			e.child(typeName, child, stack, hops, with(kinds, "intersection"), with(requires, others...), except)
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
		e.child(typeName, children[0], stack, hops, with(kinds, "exclusion"), requires, with(except, removed...))
	}
}

// child follows one part of an expression.
func (e *grantExpander) child(typeName string, child *core.SetOperation_Child, stack, hops, kinds, requires, except []string) {
	switch {
	case child.GetComputedUserset() != nil:
		e.expand(typeName, child.GetComputedUserset().GetRelation(), stack, hops, kinds, requires, except)
	case child.GetTupleToUserset() != nil || child.GetFunctionedTupleToUserset() != nil:
		tupleset, computed := child.GetTupleToUserset().GetTupleset().GetRelation(), child.GetTupleToUserset().GetComputedUserset().GetRelation()
		arrowKinds := with(kinds, "arrow")
		if f := child.GetFunctionedTupleToUserset(); f != nil {
			tupleset, computed = f.GetTupleset().GetRelation(), f.GetComputedUserset().GetRelation()
			if f.GetFunction() == core.FunctionedTupleToUserset_FUNCTION_ALL {
				// all() needs the permission on every parent, as an intersection does.
				arrowKinds = with(arrowKinds, "intersection")
				requires = with(requires, typeName+"#"+tupleset+".all("+computed+") on every "+tupleset)
			}
		}
		arrow := typeName + "#" + tupleset + "->" + computed
		subjects, _ := e.schema.RelationSubjects(typeName, tupleset)
		for _, subject := range subjects {
			subjectKinds := arrowKinds
			if subject.Caveat != "" {
				subjectKinds = with(subjectKinds, "caveat")
			}
			e.expand(subject.Type, computed, stack, with(hops, arrow), subjectKinds, requires, except)
		}
	case child.GetUsersetRewrite() != nil:
		e.rewrite(typeName, child.GetUsersetRewrite(), stack, hops, kinds, requires, except)
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
