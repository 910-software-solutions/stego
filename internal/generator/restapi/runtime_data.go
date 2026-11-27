package restapi

import (
	"bytes"
	"fmt"
	"go/format"
	"maps"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/generator/command"
	"github.com/910-software-solutions/stego/internal/generator/postgresadapter"
	"github.com/910-software-solutions/stego/internal/generator/query"
	"github.com/910-software-solutions/stego/internal/types"
)

// The runtime data path, the one data path of the rest-api component (ST-SUP-RC20-OC39): each declared collection operation is
// one generated command on github.com/910-software-solutions/stego/registry/runtime/command (create, update, patch, delete) or one query on
// github.com/910-software-solutions/stego/registry/runtime/query (read, list), and each handler calls that command's or query's Run. The handlers open no
// database connection and hold no store; the bound before slots run in the command's fill, before its write, and
// the after slots run after the command committed, so tenancy, idempotency, audit and authorization are applied once,
// by the runtime (ST-SUP-RC20-OC37).

// commandComponent and queryComponent are the components whose generated commands and queries the handlers run.
const (
	commandComponent = "command"
	queryComponent   = "query"
)

// dataFolder is the folder, under the component's namespace, of the entity declarations of the command and the
// query runtime.
const dataFolder = "data"

// runtimeSlots are the slots that the handlers on the runtime data path call.
var runtimeSlots = map[string]bool{
	"before_create": true, "before_patch": true, "before_delete": true, "validate": true,
	"after_create": true, "after_patch": true, "after_delete": true, "on_entity_changed": true,
}

// runtimeFieldTypes are the field types that a handler converts between the API type and the command runtime's row
// type.
var runtimeFieldTypes = map[types.FieldType]bool{
	types.FieldTypeString: true, types.FieldTypeEnum: true, types.FieldTypeRef: true, types.FieldTypeInt32: true,
	types.FieldTypeInt64: true, types.FieldTypeFloat: true, types.FieldTypeDouble: true, types.FieldTypeBool: true,
	types.FieldTypeTimestamp: true,
	// A jsonb field is json.RawMessage in the API type and []byte in the row type: the handler converts it.
	types.FieldTypeJsonb: true,
}

// runtimeCollection is one collection on the runtime data path.
type runtimeCollection struct {
	decl     types.Collection
	entity   types.Entity
	pascal   string // the Go name of the collection, such as Orders
	camel    string // its unexported Go name, such as orders
	lower    string // the variable of one entity value, such as order
	hrefBase string
	slots    []collectionSlotParam
	ops      map[types.Operation]bool
	// scope is the field of the parent of a scoped collection, whose value is the path parameter of the same name,
	// and empty for a collection with no parent (ST-SUP-RC20-OC24).
	scope string
	// chain are the ancestors of a scoped collection, the parent first: each one is a row that the path names, which
	// must exist in the tenant and be of the next ancestor of the path. Only the parent of a list, a create or an
	// upsert is decided, on the read permission of its entity (ST-SUP-RC20-OC24, ST-SUP-Q156).
	chain []ancestorLevel
	// rowLink is the Go name of the link of the entity of a scoped collection by its scope field, through which an
	// update, a patch and a delete check the parent of the row before they read it (ST-SUP-RC20-OC24).
	rowLink string
	// implicit are the implicit fields of the collection and their values, by field name (ST-SUP-RC20-OC23).
	implicit [][2]string
	// refs are the ref fields of the entity whose target has a resource type: a write that sets one loads the row it
	// names, on which the command runtime decides the reference (ST-SUP-RP10-LC05).
	refs []types.Field
}

// ancestorLevel is one ancestor of the parent chain of a scoped collection: the entity of the row that the path
// parameter param names, and link, the field of that row that names the next ancestor, or empty for the top
// ancestor.
type ancestorLevel struct {
	entity, param, link string
	// linkType is the Go name of the link of the entity in the data package, which holds its key and link only
	// (renderChainLinks).
	linkType string
}

// parentChain gives the ancestors of the collection c, the parent first, by the one rule types.ParentChain, from
// which the architecture model states the checks too. Each ancestor follows the first collection of its entity, as
// the path does (collectionBasePath): its scope field links it to the ancestor above it.
func parentChain(c types.Collection, collectionMap map[string]types.Collection) []ancestorLevel {
	var chain []ancestorLevel
	for _, level := range types.ParentChain(c, collectionMap) {
		chain = append(chain, ancestorLevel{entity: level.Entity, param: level.Param, link: level.Link,
			linkType: linkTypeName(level.Entity, level.Link, collectionMap)})
	}
	return chain
}

// linkTypeName gives the Go name of the link of entity by its field field in the data package: <Entity>Link for the
// scope field of the first collection of the entity, which the parent chain follows, and <Entity><Field>Link for the
// scope field of another collection of the entity.
func linkTypeName(entity, field string, first map[string]types.Collection) string {
	if first[entity].ScopeField() == field {
		return entity + "Link"
	}
	return entity + toPascalCase(field) + "Link"
}

// permission gives the permission that the operation decides, and empty for a public operation.
func (c *runtimeCollection) permission(op types.Operation) string {
	p := c.entity.Permissions[op]
	if p == types.PermissionPublic {
		return ""
	}
	return p
}

// hrefExpr gives the expression of the href of the API value v: the path of the collection with the scope field of v
// in place of its path parameter.
func (c *runtimeCollection) hrefExpr(v string) string {
	base := c.hrefBase
	if c.scope != "" {
		base = strings.Replace(base, "{"+c.scope+"}", "\"+"+v+"."+toPascalCase(c.scope)+"+\"", 1)
	}
	// An ancestor above the parent is not a field of v: the path named it, and the parent chain check holds it.
	for i := 1; i < len(c.chain); i++ {
		base = strings.Replace(base, "{"+c.chain[i].param+"}", fmt.Sprintf("\"+chain[%d]+\"", i), 1)
	}
	return fmt.Sprintf("\"%s\" + \"/\" + %s.ID", base, v)
}

// searchKind gives the kind of the values of a field that a list search may name, or empty for a field that a
// search does not take: a pii or secret field, whose match would tell its value (ST-SUP-RP11-LC20), a string field
// out of the search (types.Field.Unsearched, ST-SUP-RC20-OC57), a ref, and a field of a jsonb, bytes or timestamp
// type.
func searchKind(f types.Field) string {
	if !f.Public() || f.Unsearched() {
		return ""
	}
	switch f.Type {
	case types.FieldTypeString, types.FieldTypeEnum:
		return "KindText"
	case types.FieldTypeInt32:
		return "KindInt32"
	case types.FieldTypeInt64:
		return "KindInt64"
	case types.FieldTypeFloat, types.FieldTypeDouble:
		return "KindFloat"
	case types.FieldTypeBool:
		return "KindBool"
	}
	return ""
}

// unsearchedFields gives the public string fields of the entity that are out of the search (types.Field.Unsearched),
// in the entity's order, but the implicit fields, which the list filters on already.
func unsearchedFields(e types.Entity, implicit map[string]bool) []string {
	var out []string
	for _, f := range e.Fields {
		if f.Public() && f.Unsearched() && !implicit[f.Name] {
			out = append(out, f.Name)
		}
	}
	return out
}

// patchFields gives the collection's patchable fields in the entity's order.
func (c *runtimeCollection) patchFields() []types.Field {
	set := map[string]bool{}
	for _, name := range c.decl.Patchable {
		set[name] = true
	}
	var fields []types.Field
	for _, f := range c.entity.Fields {
		if set[f.Name] {
			fields = append(fields, f)
		}
	}
	return fields
}

// validateRuntimeData refuses, with the reason, a service that the handlers on the runtime data path cannot serve.
// The command and the query runtime run in the transaction of one tenant and decide each permission for one
// principal, so the service needs pooled tenancy and authorization; the forms that the path does not build yet are
// refused, so no declared behaviour is lost without a word.
func validateRuntimeData(ctx gen.Context, entities map[string]types.Entity) error {
	// The runtime data path is the one data path (ST-SUP-RC20-OC39): the handlers run the generated commands and
	// queries, so a service with collections needs the command and the query components.
	if ctx.PeerNamespaces[commandComponent] == "" || ctx.PeerNamespaces[queryComponent] == "" {
		return fmt.Errorf("rest-api: the service declares collections, and rest-api serves them only on the runtime data path, which needs the command and the query components; add the components command and query to the service (ST-SUP-RC20-OC39)")
	}
	if ctx.Tenancy == nil || ctx.Tenancy.Key != "tenant_id" {
		return fmt.Errorf("rest-api: on the runtime data path each operation runs in the transaction of one tenant, so the service needs pooled tenancy with the tenant column tenant_id")
	}
	if ctx.Authorization == nil || ctx.Authorization.SubjectFromIdentity == "" {
		return fmt.Errorf("rest-api: on the runtime data path each operation decides its permission for the verified principal, so the service needs authorization with subject_from_identity")
	}
	for _, b := range ctx.SlotBindings {
		if b.Slot == "before_upsert" || b.Slot == "after_upsert" {
			return fmt.Errorf("rest-api: collection %s binds the slot %s, which only the removed store path called (ST-SUP-RC20-OC39); bind validate and on_entity_changed, which an upsert calls", b.Collection, b.Slot)
		}
		if !runtimeSlots[b.Slot] {
			return fmt.Errorf("rest-api: collection %s binds the slot %s, which the runtime data path does not call yet", b.Collection, b.Slot)
		}
	}
	// The handle stages the change event of each write, also of an entity that a collection only reads through the
	// handle, so each written entity needs a valid event name (ST-SUP-RC20-OC40, ST-SUP-RP10-LC36).
	written := make([]string, len(ctx.Entities))
	for i, e := range ctx.Entities {
		written[i] = e.Name
	}
	if err := command.CheckChangeEvents(ctx, "rest-api", "ST-SUP-RC20-OC40", written); err != nil {
		return err
	}
	for _, c := range ctx.Collections {
		e := entities[c.Entity]
		if e.ResourceType == "" {
			return fmt.Errorf("rest-api: collection %s reads the entity %s, which has no resource type; on the runtime data path every operation is decided", c.Name, e.Name)
		}
		if err := validateRuntimeScope(ctx, c, e, entities); err != nil {
			return err
		}
		for _, op := range c.Operations {
			switch op {
			case types.OpUpsert:
				if err := validateRuntimeUpsert(c, e, entities); err != nil {
					return err
				}
			case types.OpUpdate, types.OpPatch, types.OpDelete:
				// Every write of a row decides a permission on it before the fill; a write with none would run
				// undecided (ST-SUP-RC29-OC01).
				if p := e.Permissions[op]; p == "" || p == types.PermissionPublic {
					return fmt.Errorf("rest-api: collection %s declares %s with the permission %q; on the runtime data path a write of a row decides a permission of %s on it", c.Name, op, p, e.ResourceType)
				}
			case types.OpCreate:
				// A create of an entity with no parent, in a collection with no scope, is decided on the service-level
				// resource of the tenant provisioning (ST-SUP-RC29-OC01, ST-SUP-RC27-OC09). A create under a parent is
				// decided on the parent through its reference, so it declares no permission of its own.
				if e.Permissions[op] != types.PermissionPublic {
					if len(c.Scope) > 0 {
						return fmt.Errorf("rest-api: collection %s declares create with the permission %q; on the runtime data path a create under the parent %s is decided on the parent through its reference, so it must be public with a reason", c.Name, e.Permissions[op], c.ParentEntity())
					}
					if ctx.Tenancy == nil || ctx.Tenancy.Provisioning == nil || ctx.Tenancy.Provisioning.ResourceType == "" {
						return fmt.Errorf("rest-api: collection %s declares create with the permission %q; a create of an entity with no parent is decided on the service-level resource of the tenant provisioning (ST-SUP-RC29-OC01), which the declaration does not declare", c.Name, e.Permissions[op])
					}
					// A create that decides its permission may write a field-sourced relationship, as a declared
					// command with a permission may (ST-SUP-Q118).
					continue
				}
				// A command that declares no permission may not write a field-sourced relationship: the client would
				// choose the subject (ST-SUP-Q118).
				for _, r := range e.Relationships {
					if r.SubjectSource == types.SubjectSourceField {
						return fmt.Errorf("rest-api: collection %s declares a public create of %s, whose relationship %s takes its subject from the field %s; a command that declares no permission may not write a field-sourced relationship, since the client would choose the subject (ST-SUP-Q118): take the subject from the caller, or write the relationship in a declared command with a permission", c.Name, e.Name, r.Relation, r.SubjectField)
					}
				}
			}
		}
		for _, f := range e.Fields {
			if f.Type == types.FieldTypeMoney {
				return fmt.Errorf("rest-api: entity %s has the field %s of type money, which the runtime data path does not serve yet: the storage form (a bigint of minor units and a char(3) currency) and the API form {amount, currency} of a money field of a data entity are decided (ST-DECL-Q13) but not built", e.Name, f.Name)
			}
			if !runtimeFieldTypes[f.Type] {
				return fmt.Errorf("rest-api: entity %s has the field %s of type %s, which the runtime data path does not serve yet", e.Name, f.Name, f.Type)
			}
		}
	}
	return nil
}

// validateRuntimeScope refuses the forms of a scoped collection, of implicit fields and of a path prefix that the
// handlers on the runtime data path do not serve: a scope of more than one field, a scope field that is not a
// required ref to the parent or that a patch may change, an implicit field that is not a text or enum field, and a
// path parameter that is not a scope field of the parent chain, or the scope fields of the chain out of order
// (ST-SUP-RC20-OC23, ST-SUP-RC20-OC24).
func validateRuntimeScope(ctx gen.Context, c types.Collection, e types.Entity, entities map[string]types.Entity) error {
	field := func(name string) (types.Field, bool) {
		for _, f := range e.Fields {
			if f.Name == name {
				return f, true
			}
		}
		return types.Field{}, false
	}
	scope := ""
	if len(c.Scope) > 0 {
		if len(c.Scope) != 1 {
			return fmt.Errorf("rest-api: collection %s is scoped by more than one field, which the runtime data path does not serve", c.Name)
		}
		scope = c.ScopeField()
		f, ok := field(scope)
		if !ok || f.Type != types.FieldTypeRef || f.To != c.ParentEntity() || f.Optional {
			return fmt.Errorf("rest-api: collection %s is scoped by %s, which is not a required ref field of %s to %s", c.Name, scope, e.Name, c.ParentEntity())
		}
		if slices.Contains(c.Patchable, scope) {
			return fmt.Errorf("rest-api: collection %s lets a patch change its scope field %s, which moves a row to another parent", c.Name, scope)
		}
	}
	for name, value := range c.Implicit {
		if slices.Contains(c.Patchable, name) {
			return fmt.Errorf("rest-api: collection %s lets a patch change its implicit field %s, which moves a row to another collection", c.Name, name)
		}
		f, ok := field(name)
		if !ok || (f.Type != types.FieldTypeString && f.Type != types.FieldTypeEnum) || (f.Type == types.FieldTypeEnum && !slices.Contains(f.Values, value)) {
			return fmt.Errorf("rest-api: collection %s has the implicit field %s, which is not a text or enum field that takes its value", c.Name, name)
		}
	}
	// The path of a collection follows the first collection of each entity, as the router does.
	collectionMap := map[string]types.Collection{}
	for _, other := range ctx.Collections {
		if _, ok := collectionMap[other.Entity]; !ok {
			collectionMap[other.Entity] = other
		}
	}
	// A collection under a scoped collection is served at each level: the path names every ancestor, and the
	// handler checks the chain of them (ST-SUP-RC20-OC24).
	base, err := collectionBasePath(c, collectionMap)
	if err != nil {
		return err
	}
	chain := parentChain(c, collectionMap)
	var want []string
	for i := len(chain) - 1; i >= 0; i-- {
		want = append(want, chain[i].param)
	}
	// A parameter named twice or named id is refused with the route patterns (validateHTTPRoutePatterns).
	params := extractPathParams(base)
	if !slices.Equal(params, want) {
		for _, param := range params {
			if !slices.Contains(want, param) {
				return fmt.Errorf("rest-api: collection %s has the path parameter %s, which is not a scope field of its parent chain; the runtime data path checks only the scope fields of the chain", c.Name, param)
			}
		}
		return fmt.Errorf("rest-api: collection %s has the path parameters %s, and its parent chain needs %s in that order", c.Name, strings.Join(params, ", "), strings.Join(want, ", "))
	}
	return nil
}

// validateRuntimeUpsert refuses an upsert that the handlers on the runtime data path cannot serve: one with no
// upsert key or a key field that is not a required field of the entity, one with optimistic concurrency on an entity
// with no required integer generation (ST-SUP-RC20-OC14), one whose update of a stored row decides no
// permission, and one whose insert can decide nothing: an entity with no required reference to an entity with a
// resource type needs a public create, with its reason (ST-SUP-RC20-OC13). An optional reference that the body
// leaves out decides nothing.
func validateRuntimeUpsert(c types.Collection, e types.Entity, entities map[string]types.Entity) error {
	if len(c.UpsertKey) == 0 {
		return fmt.Errorf("rest-api: collection %s declares upsert with no upsert_key", c.Name)
	}
	for _, key := range c.UpsertKey {
		ok := false
		for _, f := range e.Fields {
			ok = ok || (f.Name == key && !f.Optional && f.Type != types.FieldTypeJsonb && f.Type != types.FieldTypeBytes)
		}
		if !ok {
			return fmt.Errorf("rest-api: collection %s has the upsert key field %s, which is not a required field of %s of a plain type", c.Name, key, e.Name)
		}
	}
	if c.Concurrency == types.ConcurrencyOptimistic {
		// The upsert compares the generation of an If-Match header with the stored one (ST-SUP-RC20-OC14), so the
		// entity needs a required generation counter.
		ok := false
		for _, f := range e.Fields {
			ok = ok || (f.Name == "generation" && !f.Optional && (f.Type == types.FieldTypeInt32 || f.Type == types.FieldTypeInt64))
		}
		if !ok {
			return fmt.Errorf("rest-api: collection %s declares optimistic concurrency on upsert, and %s has no required field generation of type int32 or int64 for the If-Match precondition to compare (ST-SUP-RC20-OC14)", c.Name, e.Name)
		}
		// An If-Match on a key with no stored row is decided on a required reference before its 412, so the header
		// tells no caller that a row exists; a public insert decides nothing.
		decided := false
		for _, f := range e.Fields {
			ref, has := e.References[f.Name]
			decided = decided || (f.Type == types.FieldTypeRef && !f.Optional && has && ref.Permission != "" && entities[f.To].ResourceType != "")
		}
		if !decided {
			return fmt.Errorf("rest-api: collection %s declares optimistic concurrency on upsert of %s, which has no required reference with a permission: an If-Match on a key with no stored row could not be decided, and its answer would tell that no row exists (ST-SUP-RC20-OC14)", c.Name, e.Name)
		}
	}
	if p := e.Permissions[types.OpUpsert]; p == "" || p == types.PermissionPublic {
		return fmt.Errorf("rest-api: collection %s declares upsert with the permission %q; on the runtime data path an upsert of a stored row decides a permission of %s on it", c.Name, p, e.ResourceType)
	}
	referred := false
	for _, f := range e.Fields {
		referred = referred || (f.Type == types.FieldTypeRef && !f.Optional && entities[f.To].ResourceType != "")
	}
	if !referred && e.Permissions[types.OpCreate] != types.PermissionPublic {
		return fmt.Errorf("rest-api: collection %s declares upsert of %s, which has no required reference to an entity with a resource type, so the insert of a new row can be decided on nothing: declare create public with a reason", c.Name, e.Name)
	}
	return nil
}

// RuntimeService is the variable of main.go that holds the runtime wiring of the service on the runtime data path,
// which the compiler builds; its method CollectionRuntime gives the Runtime of the collection handlers.
const RuntimeService = "service"

// runtimeWiring adds to wiring one constructor per collection, which takes the Runtime of the runtime wiring and the
// operators of the collection's bound slots, and one literal route per operation (ST-SUP-RC20-OC33).
func runtimeWiring(ctx gen.Context, collections []*runtimeCollection, wiring *gen.Wiring) error {
	if wiring.ConstructorCollections == nil {
		wiring.ConstructorCollections = map[int]string{}
	}
	if wiring.ConstructorDeps == nil {
		wiring.ConstructorDeps = map[int][]string{}
	}
	for _, c := range collections {
		index := len(wiring.Constructors)
		wiring.Constructors = append(wiring.Constructors,
			fmt.Sprintf("%s.New%sHandler(%s.CollectionRuntime())", path.Base(ctx.OutputNamespace), c.pascal, RuntimeService))
		wiring.ConstructorCollections[index] = c.decl.Name
		wiring.ConstructorDeps[index] = []string{RuntimeService}
		for _, op := range c.decl.Operations {
			route, err := collectionHTTPRoute(op, c.hrefBase)
			if err != nil {
				return err
			}
			wiring.Routes = append(wiring.Routes, fmt.Sprintf("mux.HandleFunc(%q, %sHandler.%s)", route.pattern(), c.camel, route.handler))
		}
	}
	return nil
}

// runtimeCollections gives the collections of the context on the runtime data path.
func runtimeCollections(ctx gen.Context, entities map[string]types.Entity, collectionMap map[string]types.Collection) ([]*runtimeCollection, error) {
	var out []*runtimeCollection
	for _, c := range ctx.Collections {
		base, err := collectionBasePath(c, collectionMap)
		if err != nil {
			return nil, err
		}
		e := entities[c.Entity]
		rc := &runtimeCollection{decl: c, entity: e, pascal: collectionToPascalCase(c.Name), camel: collectionToCamelCase(c.Name),
			lower: safeVarName(strings.ToLower(e.Name)), hrefBase: ctx.BasePath + base,
			slots: collectCollectionSlotParams(c.Name, ctx.SlotBindings), ops: map[types.Operation]bool{}}
		for _, op := range c.Operations {
			rc.ops[op] = true
		}
		rc.scope = c.ScopeField()
		rc.chain = parentChain(c, collectionMap)
		if rc.scope != "" {
			rc.rowLink = linkTypeName(e.Name, rc.scope, collectionMap)
		}
		for _, name := range slices.Sorted(maps.Keys(c.Implicit)) {
			rc.implicit = append(rc.implicit, [2]string{name, c.Implicit[name]})
		}
		for _, f := range e.Fields {
			if f.Type == types.FieldTypeRef && entities[f.To].ResourceType != "" {
				rc.refs = append(rc.refs, f)
			}
		}
		out = append(out, rc)
	}
	return out, nil
}

// generateRuntimeHandlers gives the files of the handlers on the runtime data path: the entity declarations of the
// command and the query runtime in the data package, the runtime helpers, and one handler file per collection.
func generateRuntimeHandlers(ctx gen.Context, collections []*runtimeCollection, slotsImportPath string) ([]gen.File, error) {
	var names []string
	seen := map[string]bool{}
	for _, c := range collections {
		if !seen[c.entity.Name] {
			seen[c.entity.Name] = true
			names = append(names, c.entity.Name)
		}
	}
	// The rows that a ref field names are loaded by the writes, so their entities are declared too.
	for _, c := range collections {
		for _, f := range c.refs {
			if !seen[f.To] {
				seen[f.To] = true
				names = append(names, f.To)
			}
		}
	}
	relations, err := command.SchemaRelations(ctx)
	if err != nil {
		return nil, fmt.Errorf("rest-api: %w", err)
	}
	entityFile, err := command.EntityFile(dataFolder, names, ctx.Entities, ctx.Tenancy.Key, relations)
	if err != nil {
		return nil, fmt.Errorf("rest-api: %w", err)
	}
	viewFile, err := query.ViewFile(dataFolder, names, ctx.Entities)
	if err != nil {
		return nil, fmt.Errorf("rest-api: %w", err)
	}
	dataImport := path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace, dataFolder)
	files := []gen.File{
		{Path: path.Join(ctx.OutputNamespace, dataFolder, "entities.go"), Content: entityFile},
		{Path: path.Join(ctx.OutputNamespace, dataFolder, "views.go"), Content: viewFile},
	}
	linkFile, err := renderChainLinks(ctx, collections)
	if err != nil {
		return nil, err
	}
	if linkFile != nil {
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, dataFolder, "links.go"), Content: linkFile})
	}
	hasSlots := false
	for _, c := range collections {
		hasSlots = hasSlots || len(c.slots) > 0
	}
	keys := idempotencyKeys(ctx)
	runtimeFile, err := renderRuntimeHelpers(ctx, names, dataImport, slotsImportPath, hasSlots, keys)
	if err != nil {
		return nil, err
	}
	files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, "runtime.go"), Content: runtimeFile})
	for _, c := range collections {
		source, err := renderRuntimeHandler(ctx, c, dataImport, slotsImportPath, keys)
		if err != nil {
			return nil, fmt.Errorf("rest-api: collection %s: %w", c.decl.Name, err)
		}
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, collectionToSnakeCase(c.decl.Name)+"_handler.go"), Content: source})
	}
	return files, nil
}

// renderRuntimeHelpers writes runtime.go: the Runtime that the handlers run on and the answers of the errors of Run.
// The bind, the outcome map and the outcome row of a write are the command runtime's (ST-SUP-RP10-LC34).
func renderRuntimeHelpers(ctx gen.Context, names []string, dataImport, slotsImportPath string, hasSlots bool, keys *idempotencyHeader) ([]byte, error) {
	var b bytes.Buffer
	pkg := path.Base(ctx.OutputNamespace)
	fmt.Fprintf(&b, "package %s\n\nimport (\n\t\"context\"\n\t\"encoding/json\"\n\t\"errors\"\n\t\"log/slog\"\n\t\"net\"\n\t\"net/http\"\n\t\"net/url\"\n\t\"slices\"\n\t\"strconv\"\n\t\"strings\"\n\t\"sync\"\n\t\"sync/atomic\"\n\t\"time\"\n\t\"unicode/utf8\"\n\n", pkg)
	fmt.Fprintf(&b, "\t\"go.opentelemetry.io/otel/attribute\"\n\t\"go.opentelemetry.io/otel/metric\"\n\n")
	fmt.Fprintf(&b, "\t\"github.com/910-software-solutions/stego/registry/runtime/audit\"\n\t\"github.com/910-software-solutions/stego/registry/runtime/authn\"\n\t\"github.com/910-software-solutions/stego/registry/runtime/command\"\n")
	fmt.Fprintf(&b, "\t\"github.com/910-software-solutions/stego/registry/runtime/idempotency\"\n\t\"github.com/910-software-solutions/stego/registry/runtime/outbox\"\n\t\"github.com/910-software-solutions/stego/registry/runtime/probe\"\n\t\"github.com/910-software-solutions/stego/registry/runtime/query\"\n\n")
	fmt.Fprintf(&b, "\tdata %q\n", dataImport)
	if hasSlots {
		fmt.Fprintf(&b, "\tslots %q\n", slotsImportPath)
	}
	fmt.Fprintf(&b, ")\n\n")
	fmt.Fprintf(&b, "// subjectType is the subject type of the schema that a verified identity maps to.\nconst subjectType = %q\n\n", ctx.Authorization.SubjectFromIdentity)
	// With no idempotency component the handler still keeps the key of a create and a patch (ST-SUP-RC20-OC56), so
	// its refusals take the same problem types under the problem base as a command, whose runtime joins the slugs of
	// the idempotency runtime to the base (walkthrough 19, W19-U3).
	missing, invalid, busy, reuse := "about:blank", "about:blank", "about:blank", "about:blank"
	switch {
	case keys != nil:
		missing, invalid, busy, reuse = keys.missing, keys.invalid, keys.busy, keys.reuse
	case ctx.ErrorTypeBase != "":
		missing, invalid, busy, reuse = ctx.ErrorTypeBase+"idempotency-key-missing", ctx.ErrorTypeBase+"idempotency-key-invalid",
			ctx.ErrorTypeBase+"idempotency-key-in-progress", ctx.ErrorTypeBase+"idempotency-key-reused"
	}
	fmt.Fprintf(&b, "// The problem types of the idempotency key (ST-SUP-RC30-OC02).\nconst (\n\tkeyMissingType = %q\n\tkeyInvalidType = %q\n\tkeyBusyType = %q\n\tkeyReusedType = %q\n)\n\n", missing, invalid, busy, reuse)
	fmt.Fprintf(&b, "// Entities are the entities that the generated commands write, for the Entities of the command runtime's Config.\nfunc Entities() []command.Declared {\n\treturn []command.Declared{")
	for i, name := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "data.%sEntity", name)
	}
	b.WriteString("}\n}\n\n")
	b.WriteString(runtimeHelpers)
	b.WriteString(listOrderHelper)
	if hasSlots {
		b.WriteString(slotCallerHelper)
	}
	formatted, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("rest-api: formatting runtime.go: %w\n%s", err, b.Bytes())
	}
	return formatted, nil
}

// listOrderHelper reads the query parameters of a list: it refuses one that the list does not read
// (ST-SUP-RC20-OC51), and reads orderBy and fields (ST-SUP-RC20-OC38, ST-SUP-RC20-OC12).
const listOrderHelper = `// listParameters are the query parameters that a list reads, by their exact names (ST-SUP-RC20-OC51).
var listParameters = map[string]bool{"pageSize": true, "cursor": true, "orderBy": true, "fields": true, "search": true}

// listQuery parses the query string of a list once, and gives its values and the detail of a 400, or "" when the list
// reads each parameter (ST-SUP-RC20-OC51). A query string that does not parse, such as one with a ';', a bad escape or
// more parameters than the url package takes, is refused: r.URL.Query() would drop the bad part or give no values,
// and the list would then read a page that the request did not ask for.
func listQuery(r *http.Request) (url.Values, string) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, "the query string is not valid"
	}
	return values, unreadParameters(values)
}

// unreadParameters gives the detail of the 400 of a list request whose query string holds a parameter that the list
// does not read, or "" when the list reads each one (ST-SUP-RC20-OC51). It names each such parameter once, in byte
// order, at most 10 of them, each cut to at most its first 64 bytes on a character boundary, and quotes no value.
// A name with no character boundary in those bytes, which is not valid UTF-8, is cut at 64 bytes, and strconv.Quote
// escapes its bytes. Two names that are the same after the cut are named once. For limit it says that pageSize sets
// the page size.
func unreadParameters(parameters map[string][]string) string {
	var names []string
	limit := false
	for name := range parameters {
		if listParameters[name] {
			continue
		}
		limit = limit || name == "limit"
		if len(name) > 64 {
			cut := 64
			for cut > 0 && !utf8.RuneStart(name[cut]) {
				cut--
			}
			if cut == 0 {
				cut = 64
			}
			name = name[:cut]
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return ""
	}
	slices.Sort(names)
	names = slices.Compact(names)
	more := max(len(names)-10, 0)
	names = names[:len(names)-more]
	for i, name := range names {
		names[i] = strconv.Quote(name)
	}
	detail := "the list does not read the query parameter " + strings.Join(names, ", ")
	if more > 0 {
		detail += " and " + strconv.Itoa(more) + " more"
	}
	detail += "; it reads only pageSize, cursor, orderBy, fields and search"
	if limit {
		detail += "; pageSize sets the page size"
	}
	return detail
}

// errOrder is the error of an orderBy that names a field that is not declared, or a direction that is not asc or
// desc.
var errOrder = errors.New("orderBy names a field that is not declared or a direction that is not asc or desc")

// listOrder reads the orderBy parameter of a list: a comma-separated list of declared fields, each with asc or desc,
// asc when it names none. With no orderBy the order is created_time descending (ST-SUP-RC20-OC11). The key is the last
// tie-breaker of every order, which the query runtime adds (ST-SUP-RP11-LC18).
func listOrder(text string, fields map[string]bool) ([]query.OrderBy, error) {
	if strings.TrimSpace(text) == "" {
		return []query.OrderBy{{Field: "created_time", Descending: true}}, nil
	}
	var order []query.OrderBy
	for _, part := range strings.Split(text, ",") {
		words := strings.Fields(part)
		if len(words) == 0 || len(words) > 2 || !fields[words[0]] {
			return nil, errOrder
		}
		o := query.OrderBy{Field: words[0]}
		if len(words) == 2 {
			switch strings.ToLower(words[1]) {
			case "asc":
			case "desc":
				o.Descending = true
			default:
				return nil, errOrder
			}
		}
		order = append(order, o)
	}
	return order, nil
}

// unsearchedOrder gives the first field of the orderBy text that is one of unsearched, the declared fields that a list
// neither searches nor orders, or "" when it names none (ST-SUP-RC20-OC57, decision ST-DECL-Q50).
func unsearchedOrder(text string, unsearched map[string]query.FieldKind) string {
	for _, part := range strings.Split(text, ",") {
		if words := strings.Fields(part); len(words) > 0 {
			if _, out := unsearched[words[0]]; out {
				return words[0]
			}
		}
	}
	return ""
}

// unsearchedSearch gives the first field of a search that is one of unsearched, when the search is well formed with
// those fields in it too, or "" (ST-SUP-RC20-OC57). A search that is not well formed stays the 400 that names no
// part of its text.
func unsearchedSearch(text string, searched, unsearched map[string]query.FieldKind, most int) string {
	all := map[string]query.FieldKind{}
	for name, kind := range searched {
		all[name] = kind
	}
	for name, kind := range unsearched {
		all[name] = kind
	}
	where, err := query.ParseSearch(text, all, most)
	if err != nil {
		return ""
	}
	for _, condition := range where {
		if _, out := unsearched[condition.Field]; out {
			return condition.Field
		}
	}
	return ""
}

// unsearchedDetail is the detail of the 400 of a list whose parameter names a field that a list neither searches nor
// orders (ST-SUP-RC20-OC57).
func unsearchedDetail(parameter, field string) string {
	return parameter + " names the field " + strconv.Quote(field) + ", which a list neither searches nor orders: the field is declared searchable: false, or its max_length is over 500 characters, so it has no search index"
}

// isBadCursor tells whether err is a cursor of another order, which a list answers with 400.
func isBadCursor(err error) bool { return errors.Is(err, query.ErrBadCursor) }

// errFields is the error of a fields parameter that names a field that is not declared.
var errFields = errors.New("fields names a field that is not declared")

// listFields reads the fields parameter of a list: a comma-separated list of declared fields. It gives nil when the
// parameter names none, and else the selected fields with id, which a selection always keeps (ST-SUP-RC20-OC12).
func listFields(text string, fields map[string]bool) (map[string]bool, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	selected := map[string]bool{"id": true}
	for _, name := range strings.Split(text, ",") {
		name = strings.TrimSpace(name)
		if name == "" || name == "id" {
			continue
		}
		if !fields[name] {
			return nil, errFields
		}
		selected[name] = true
	}
	return selected, nil
}

// selectFields gives the members of the presented value v that the selection names, with id, kind and href, and v
// itself when there is no selection (ST-SUP-RC20-OC12).
func selectFields(v any, selected map[string]bool) any {
	if selected == nil {
		return v
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return v
	}
	for name := range members {
		if !selected[name] && name != "kind" && name != "href" {
			delete(members, name)
		}
	}
	return members
}

`

// runtimeHelpers are the parts of runtime.go after its constants. They are the same for every declaration; ST-SUP-Q74
// and ST-SUP-Q84 ask whether such code moves into the runtime library (ST-STD-RUNTIME-R05).
const runtimeHelpers = `// Runtime is what the handlers of the collections run on (ST-SUP-RC20-OC37): the command runtime for each write, the
// query runtime for each read, and the probes that bind the probe call of each read. The handlers open no database
// connection and hold no store.
type Runtime struct {
	Commands *command.Runtime
	Queries  *query.Runtime
	Probes   command.Probes
	// Retention is how long a completed idempotency key is kept; zero is the runtime's default.
	Retention time.Duration
	// AfterSlots are the after slots of the collections, which the outbox worker runs (ST-SUP-RC20-OC34). Nil: the
	// handlers register none, and a staged after slot waits in the outbox.
	AfterSlots *AfterSlots
}

// MaxRequestBody is the most bytes of a request body that a handler reads.
const MaxRequestBody = 1 << 20

// noProbe is the typed probe of a collection operation, which emits no domain observation.
func noProbe(*probe.Probe) struct{} { return struct{}{} }

// mustCommand gives a checked command declaration. The generator checked each declaration, so it does not fail.
func mustCommand(c *command.Command[struct{}], err error) *command.Command[struct{}] {
	if err != nil {
		panic(err)
	}
	return c
}

// mustQuery gives a checked query declaration.
func mustQuery(q *query.Query, err error) *query.Query {
	if err != nil {
		panic(err)
	}
	return q
}

// slotRefusal is the refusal of a before slot fill: the handler answers it with the slot's status, and Run rolls the
// command back, so nothing of it is written (ST-SUP-RC20-OC30).
type slotRefusal struct {
	status int
	detail string
}

func (r *slotRefusal) Error() string { return "a slot fill refused the request" }

// haltHeader marks the response of a command that a before slot halted with no write. The key row stores it, so a
// replay of the request is known as halted too; writeResult takes it off before the response leaves the service.
const haltHeader = "Stego-Slot-Halted"

// slotHalt is the result of a before slot fill that accepted the request and stops it with a status: the command
// writes nothing and answers the status, and no after slot runs.
func slotHalt(status int) (idempotency.Response, error) {
	if status <= 0 {
		status = http.StatusOK
	}
	return idempotency.Response{Status: status, Header: http.Header{haltHeader: {"true"}}}, nil
}

// halted tells whether a before slot halted the command of result with no write, also in a replay.
func halted(result command.Result) bool { return result.Response.Header.Get(haltHeader) != "" }

// afterSlotFailures counts the after slots that failed after their command committed.
var afterSlotFailures atomic.Int64

// AfterSlotFailures gives how many after slots failed after their command committed.
func AfterSlotFailures() int64 { return afterSlotFailures.Load() }

// AfterSlotsDestination is the outbox destination of the after slots: a command stages each bound after slot in its
// transaction, so the slot runs only after the command committed, and the outbox worker runs it and retries a
// failure (ST-SUP-RC20-OC34).
const AfterSlotsDestination = "rest-api.after-slots"

// errAfterSlot is the fixed error of a failed after slot: the outbox runs it again, and no error text of the fill
// reaches the queue or the log.
var errAfterSlot = errors.New("an after slot failed")

// AfterSlots runs the after slots that the outbox worker delivers, one function for each collection, and makes
// each failure and each retry visible as a metric and a log line that name the slot and hold no error text.
type AfterSlots struct {
	mu          sync.Mutex
	collections map[string]func(ctx context.Context, slot string, payload []byte) error
	failures    metric.Int64Counter
	retries     metric.Int64Counter
}

// NewAfterSlots gives the after slots of the collections, with no metric until Meter sets one.
func NewAfterSlots() *AfterSlots {
	return &AfterSlots{collections: map[string]func(context.Context, string, []byte) error{}}
}

// Meter records the failure and the retry metrics of the after slots on meter, before the worker runs. A nil meter,
// such as the telemetry runtime's with no collector set, records no metric; the log lines stay.
func (a *AfterSlots) Meter(meter metric.MeterProvider) error {
	if meter == nil {
		return nil
	}
	m := meter.Meter("stego/rest-api")
	failures, err := m.Int64Counter("rest_api.after_slot.failures", metric.WithDescription("after slots that failed, by slot"))
	if err != nil {
		return err
	}
	retries, err := m.Int64Counter("rest_api.after_slot.retries", metric.WithDescription("after slots that run again, by slot"))
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures, a.retries = failures, retries
	return nil
}

// add registers the after slots of one collection.
func (a *AfterSlots) add(collection string, deliver func(ctx context.Context, slot string, payload []byte) error) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.collections[collection] = deliver
}

// Deliver runs one after slot that the outbox worker delivers on AfterSlotsDestination, which the wiring registers
// as idempotent, as a fill runs at least once, and as must land, so a message is retried and never parked for its
// retries. The message kind is <collection>.<slot>. A run that is not the first counts as a retry, and a failure is
// counted and logged and refused as transient, so the outbox runs it again.
func (a *AfterSlots) Deliver(ctx context.Context, d outbox.Delivery) error {
	collection, slot, _ := strings.Cut(d.Kind, ".")
	a.mu.Lock()
	run, failures, retries := a.collections[collection], a.failures, a.retries
	a.mu.Unlock()
	attr := metric.WithAttributes(attribute.String("slot", d.Kind))
	if d.Attempts > 1 {
		if retries != nil {
			retries.Add(ctx, 1, attr)
		}
		slog.WarnContext(ctx, "an after slot runs again through the outbox", "slot", d.Kind)
	}
	if run == nil || run(ctx, slot, d.Payload) != nil {
		afterSlotFailed(ctx, failures, d.Kind)
		return errAfterSlot
	}
	return nil
}

// afterSlotFailed records an after slot that failed after its command committed: a metric and a log line that name
// the slot and hold no error text (ST-SUP-RC20-OC34).
func afterSlotFailed(ctx context.Context, failures metric.Int64Counter, slot string) {
	afterSlotFailures.Add(1)
	if failures != nil {
		failures.Add(ctx, 1, metric.WithAttributes(attribute.String("slot", slot)))
	}
	slog.ErrorContext(ctx, "an after slot failed after its command committed; the outbox runs it again", "slot", slot)
}

// invalidRequest is a request body that does not decode into the collection's type.
type invalidRequest struct{ problem *ServiceError }

func (e *invalidRequest) Error() string { return "the request is not valid" }

// preconditionFailed is the answer of an upsert with optimistic concurrency whose If-Match precondition does not
// hold: 412 for a stale or malformed generation or a row that does not exist, 428 for a stored row and no If-Match
// (ST-SUP-RC20-OC14). The command writes nothing.
type preconditionFailed struct {
	status int
	detail string
}

func (e *preconditionFailed) Error() string { return "the precondition of the request does not hold" }

// ifMatchGeneration reads the If-Match header of an upsert with optimistic concurrency: one strong entity tag that
// holds the decimal generation the client last read, such as "3". It gives the generation, whether the request
// sends the header, and whether the header has that form; a weak tag, *, a list or any other text is not valid.
func ifMatchGeneration(r *http.Request) (int64, bool, bool) {
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		return 0, false, false
	}
	if len(values) != 1 {
		return 0, true, false
	}
	tag := strings.TrimSpace(values[0])
	if len(tag) < 3 || len(tag) > 21 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		return 0, true, false
	}
	var generation int64
	for _, digit := range tag[1 : len(tag)-1] {
		if digit < '0' || digit > '9' || generation > (1<<62)/10 {
			return 0, true, false
		}
		generation = generation*10 + int64(digit-'0')
	}
	return generation, true, true
}

// requestKey is the idempotency key of a write that takes no key from the client: a PUT, a DELETE, or a create or
// a patch without the header when the declaration does not require it. Run takes every write inside the
// idempotency wrapper, so the request gets a key of its own that no retry sends again (ST-SUP-Q86).
func requestKey(requestID string) string { return "request:" + requestID }

// commandRequest gives what Run knows of one write.
func (rt *Runtime) commandRequest(r *http.Request, b command.Bound, key, fingerprint, requestID string, body []byte, resource string) command.Request {
	return command.Request{Tenant: b.Tenant, Principal: b.Principal, Key: key, Fingerprint: fingerprint, Body: body,
		Resource: resource, RequestID: requestID, ActorIP: remoteIP(r.RemoteAddr), UserAgent: r.UserAgent(),
		Retention: rt.Retention}
}

// boundQuery gives the bound call of a read for the verified tenant and principal, with its probe call.
func (rt *Runtime) boundQuery(ctx context.Context, b command.Bound, q *query.Query) query.Bound[struct{}] {
	call := rt.Probes.Bind(ctx, b.Tenant.ID(), b.Principal.Subject().String(), q.Name())
	return query.Bound[struct{}]{Tenant: b.Tenant, Principal: b.Principal, Call: call, Probe: noProbe, RequestID: command.NewID()}
}

// answerWrite answers an error of Run and writes the audit row of the denied or failed write in its own short
// transaction (ST-SUP-RC28-OC03): a slot refusal is the slot's status and outcome denied, a deny is 404, the same
// answer as a missing row, a refused field value 400 naming the field (ST-SUP-Q118), a conflict 409 and every other
// error 500. A row that is not there is 404 and outcome
// denied for an operation that decides a permission, as a deny is, so the answer never tells that a row exists or
// once existed (ST-SUP-RC20-OC37, ST-SUP-RP10-LC31), and 404 not_found otherwise. No error text reaches the answer or
// the audit row.
func (rt *Runtime) answerWrite(w http.ResponseWriter, r *http.Request, req command.Request, action, resourceType, kind string, decided bool, err error) {
	var refused *slotRefusal
	var invalid *invalidRequest
	var precondition *preconditionFailed
	switch {
	case errors.As(err, &invalid):
		handleError(w, r, invalid.problem)
		return
	case errors.As(err, &precondition):
		_ = rt.Commands.RecordFailure(r.Context(), req, action, resourceType, command.Outcome{Status: precondition.status, Code: "precondition_failed", Audit: audit.OutcomeFailed})
		handleError(w, r, errorForStatus(precondition.status, precondition.detail))
		return
	case errors.As(err, &refused):
		status := refused.status
		if status <= 0 {
			status = http.StatusForbidden
		}
		_ = rt.Commands.RecordFailure(r.Context(), req, action, resourceType, command.Outcome{Status: status, Code: "slot_refused", Audit: audit.OutcomeDenied})
		handleError(w, r, errorForStatus(status, refused.detail))
		return
	case errors.Is(err, command.ErrNotFound) && decided:
		_ = rt.Commands.RecordFailure(r.Context(), req, action, resourceType, command.Outcome{Status: http.StatusNotFound, Code: "denied", Audit: audit.OutcomeDenied})
		handleError(w, r, missing(kind, req))
		return
	case errors.Is(err, command.ErrNotFound):
		_ = rt.Commands.RecordFailure(r.Context(), req, action, resourceType, command.Outcome{Status: http.StatusNotFound, Code: "not_found", Audit: audit.OutcomeFailed})
		handleError(w, r, missing(kind, req))
		return
	}
	o := command.OutcomeOf(err)
	_ = rt.Commands.RecordFailure(r.Context(), req, action, resourceType, o)
	switch o.Status {
	case http.StatusBadRequest:
		// The runtime refused a field's value (command.ErrInvalid): the answer names the field and the fixed reason.
		handleError(w, r, Validation([]ValidationError{{Field: o.Field, Message: o.Reason}}))
	case http.StatusNotFound:
		handleError(w, r, missing(kind, req))
	case http.StatusConflict:
		if o.Field != "" {
			// A unique violation names the fields of the key, never a value (ST-SUP-RC20-OC50, ST-SUP-RP10-LC48).
			handleError(w, r, Conflict("the "+kind+" was not written: another "+kind+" has the same "+o.Field+", which must be unique"))
			return
		}
		handleError(w, r, Conflict("the "+kind+" changed during the request"))
	case http.StatusServiceUnavailable:
		answerUndecided(w, r, o.Code == command.ErrorCodeUnavailable)
	default:
		handleError(w, r, InternalError("internal error"))
	}
}

// missing is the 404 of a write: of the row that the request names, or, for a write that names no row, such as a
// create, of the write, whose parent or referenced row is not there or may not be used, or which the caller may not
// make. It never gives an empty ID (walkthrough re-run, 2026-11-14).
func missing(kind string, req command.Request) *ServiceError {
	if req.Resource != "" {
		return NotFound(kind, req.Resource)
	}
	problem := NotFound(kind, "")
	problem.Detail = "the " + kind + " was not written: the caller may not write it, or its parent or a resource that it refers to is not there"
	return problem
}

// answerKey answers a result of the idempotency wrapper that did not call the fill: a key in progress is 409 and a
// key used with another request 422 (ST-SUP-RC30-OC02).
func answerKey(w http.ResponseWriter, r *http.Request, result command.Result) {
	problem := errorForStatus(result.Response.Status, "the idempotency key is in progress")
	problem.Type = keyBusyType
	if result.Problem == idempotency.ProblemReused {
		problem.Detail, problem.Type = "the idempotency key was used with another request", keyReusedType
	}
	handleError(w, r, problem)
}

// keyMissing answers a create or a patch that needs the Idempotency-Key header and has none, with the type
// idempotency-key-missing, or that has a malformed one, with the type idempotency-key-invalid and a detail that tells
// that the key is an RFC 9651 string in double quotes (ST-SUP-RC30-OC02, decision ST-SUP-Q202 (a)).
func keyMissing(w http.ResponseWriter, r *http.Request, err error) {
	problem := BadRequest("the Idempotency-Key header is absent")
	problem.Type = keyMissingType
	if errors.Is(err, idempotency.ErrKeyMalformed) {
		problem = BadRequest("the Idempotency-Key header is not one RFC 9651 string of 1 to 255 characters: put the key in double quotes, such as \"k-1\"")
		problem.Type = keyInvalidType
	}
	handleError(w, r, problem)
}

// canonicalID gives the lower-case form of a UUID, the form that the rows and the relations hold, so that an
// upper-case UUID in a path or a body names the same row (ST-SUP-RC20-OC24). Another text stays as it is.
func canonicalID(id string) string {
	if command.ValidUUID(id) {
		return strings.ToLower(id)
	}
	return id
}

// answerRead answers an error of a read: a row that is not there or that the caller may not read is 404, a read
// that the authorization provider could not decide now is 503 with Retry-After, and every other error, a refusal of
// the provider included, 500 (ST-SUP-RP11-LC06, ST-SUP-Q140).
func answerRead(w http.ResponseWriter, r *http.Request, kind, id string, err error) {
	if errors.Is(err, query.ErrNotFound) {
		handleError(w, r, NotFound(kind, id))
		return
	}
	if query.Transient(err) {
		answerUndecided(w, r, unavailableDatabase(err))
		return
	}
	handleError(w, r, InternalError("internal error"))
}

// answerUndecided answers a request that the service cannot serve now: 503 with Retry-After of 1 to 3 s, never a bare
// 500. The runtime logs the fixed cause (walkthrough 3, G4; ST-SUP-Q140). The detail names the cause: the database of
// the service, such as while its server restarts, when database is true, and else the authorization provider, such as
// for some seconds after a schema write (walkthrough 24, small items).
func answerUndecided(w http.ResponseWriter, r *http.Request, database bool) {
	w.Header().Set("Retry-After", query.RetryAfter())
	if database {
		handleError(w, r, errorForStatus(http.StatusServiceUnavailable, "the database of the service cannot serve the request now; try again"))
		return
	}
	handleError(w, r, errorForStatus(http.StatusServiceUnavailable, "the authorization provider cannot decide the request now; try again"))
}

// unavailableDatabase tells whether a read that a retry may pass (query.Transient) failed at the database, and not at
// the authorization provider.
func unavailableDatabase(err error) bool { return !errors.Is(err, query.ErrUndecided) }

// answerWaitFailure answers a wait for a revocation that failed: a database that cannot serve the wait now is 503
// with Retry-After, as command.Serve answers it, and every other failure 500 (security review of PR #440, I1).
func answerWaitFailure(w http.ResponseWriter, r *http.Request, err error) {
	if query.Transient(err) {
		w.Header().Set("Retry-After", query.RetryAfter())
		handleError(w, r, errorForStatus(http.StatusServiceUnavailable, "the service cannot read the state of the revocation now; try again"))
		return
	}
	handleError(w, r, InternalError("internal error"))
}

// writeResult writes the response that Run gives: the fill's response or the stored one of a replay.
func writeResult(w http.ResponseWriter, result command.Result) {
	for name, values := range result.Response.Header {
		w.Header()[name] = values
	}
	w.Header().Del(haltHeader)
	w.WriteHeader(result.Response.Status)
	_, _ = w.Write(result.Response.Body)
}

// jsonResponse is the response of a write with a JSON body.
func jsonResponse(status int, body []byte) idempotency.Response {
	return idempotency.Response{Status: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: body}
}

// callerName is the name of the verified caller that the created_by and updated_by fields hold: the email, or the
// user ID when the identity has none.
func callerName(ctx context.Context) string {
	identity, _ := authn.FromContext(ctx)
	if email := identity.Email(); email != "" {
		return email
	}
	return identity.UserID()
}

// ptrOf gives a pointer to a copy of v.
func ptrOf[T any](v T) *T { return &v }

// maxMemberName is the most bytes of a member name of the body that a problem repeats (security review of PR #464).
const maxMemberName = 128

// memberName gives a member name of the body as a problem repeats it: at most maxMemberName bytes, cut on a
// character boundary.
func memberName(name string) string {
	if len(name) <= maxMemberName {
		return name
	}
	cut := maxMemberName
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	return name[:cut]
}

// duplicateMember gives the first member name, after its escapes, that the top-level object of body gives more than
// once. The JSON decoder keeps the last value of such a member, so a check of one value and a decode of another
// could disagree: a write refuses the body (ST-SUP-RC20-OC15, ST-SUP-RC20-OC55, security review of PR #464).
func duplicateMember(body []byte) (string, bool) {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return "", false
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return "", false
		}
		name, _ := token.(string)
		if seen[name] {
			return name, true
		}
		seen[name] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return "", false
		}
	}
	return "", false
}

// float32Of gives the API value of an optional float field of a view: nil for NULL.
func float32Of(v *float64) *float32 {
	if v == nil {
		return nil
	}
	f := float32(*v)
	return &f
}

// remoteIP is the host part of a remote address.
func remoteIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return host
}

`

// slotCallerHelper maps the verified identity to the caller of a slot request.
const slotCallerHelper = `
// callerIdentity is the caller of a slot request: the verified identity of the request.
func callerIdentity(ctx context.Context) *slots.Identity {
	identity, _ := authn.FromContext(ctx)
	role := ""
	if roles := identity.Roles(); len(roles) > 0 {
		role = roles[0]
	}
	return &slots.Identity{UserID: identity.UserID(), Role: role, Attributes: identity.Attributes()}
}
`

// emitRuntimeSlotCheck writes the handling of a before slot's result inside a generated command's fill: an error is
// the fill's error, a refusal is a slotRefusal, which rolls the command back before any write, and a halt answers
// its status with no write.
func emitRuntimeSlotCheck(buf *bytes.Buffer, fieldName string) {
	fmt.Fprintf(buf, "\t\tslotResult, slotErr := h.%s.Evaluate(ctx, slotReq)\n", fieldName)
	fmt.Fprintf(buf, "\t\tif slotErr != nil {\n\t\t\treturn idempotency.Response{}, slotErr\n\t\t}\n")
	fmt.Fprintf(buf, "\t\tif !slotResult.Ok {\n\t\t\treturn idempotency.Response{}, &slotRefusal{status: int(slotResult.StatusCode), detail: slotResult.ErrorMessage}\n\t\t}\n")
	fmt.Fprintf(buf, "\t\tif slotResult.Halt {\n\t\t\treturn slotHalt(int(slotResult.StatusCode))\n\t\t}\n")
	fmt.Fprintf(buf, "\t}\n")
}

// emitRuntimeAfterFailure is the answer of a failed after slot on the runtime data path: the command committed and
// its response is stored, so the failure is logged with no error text and the handler answers the committed
// response (ST-SUP-Q89).
func emitRuntimeAfterFailure(buf *bytes.Buffer, param collectionSlotParam) {
	fmt.Fprintf(buf, "\t\t\t%s\n", afterCallMarker)
}

// afterCallMarker marks the call of an after slot that the runtime data path replaces with the staging of the slot
// in the command's transaction (ST-SUP-RC20-OC34).
const afterCallMarker = "/* stego: after slot */"

// afterCall matches the call of an after slot that the shared emitters write, with the marker of a failure.
var afterCall = regexp.MustCompile(`(?m)^(\t*)if _, slotErr := h\.(\w+)\.Evaluate\(r\.Context\(\), (.+?)\); slotErr != nil \{\n\t*` + regexp.QuoteMeta(afterCallMarker) + `\n\t*\}\n`)

// runtimeHandlerWriter writes one handler file.
type runtimeHandlerWriter struct {
	// upsertList is the generated list of the lookup of an upsert by its natural key.
	upsertList command.ListPlan
	bytes.Buffer
	c          *runtimeCollection
	ctx        gen.Context
	keys       *idempotencyHeader
	envelope   bool
	slotsAlias string
}

func (w *runtimeHandlerWriter) f(format string, args ...any) { fmt.Fprintf(&w.Buffer, format, args...) }

// renderRuntimeHandler writes the handler file of one collection: its command and query declarations, its handler
// type, and one method per declared operation that calls Run.
func renderRuntimeHandler(ctx gen.Context, c *runtimeCollection, dataImport, slotsImportPath string, keys *idempotencyHeader) ([]byte, error) {
	w := &runtimeHandlerWriter{c: c, ctx: ctx, keys: keys, envelope: ctx.Conventions.ResponseFormat == "envelope"}
	if c.ops[types.OpUpsert] {
		// The lookup of an upsert is a list of the upsert command over the entity of the release, with each field of
		// the lookup as a parameter that the handler binds from the decoded body (ST-SUP-RC20-OC13,
		// ST-SUP-RP10-LC38).
		list := types.WorkerList{Name: "upsert-key", Entity: c.entity.Name}
		for _, key := range c.decl.UpsertLookup() {
			list.Filter = append(list.Filter, types.ListTerm{Field: key, Op: types.ListEquals, Param: key})
		}
		plan, err := command.ListStatements(list, ctx.Entities, ctx.Tenancy.Key)
		if err != nil {
			return nil, err
		}
		if plan.Child {
			// The handle lists a child entity only under its decided parent row (ST-SUP-RP10-LC38).
			return nil, fmt.Errorf("rest-api: collection %s: an upsert of the child entity %s, which has no resource type, is not supported; give the entity a resource type", c.decl.Name, c.entity.Name)
		}
		w.upsertList = plan
	}
	if len(c.slots) > 0 {
		w.slotsAlias = "slots"
	}
	e := c.entity
	hasWrite := c.ops[types.OpCreate] || c.ops[types.OpUpdate] || c.ops[types.OpPatch] || c.ops[types.OpDelete] || c.ops[types.OpUpsert]
	hasBody := c.ops[types.OpCreate] || c.ops[types.OpUpdate] || c.ops[types.OpPatch] || c.ops[types.OpUpsert]
	needTime, needFmt := false, false
	for _, f := range e.Fields {
		if f.Type == types.FieldTypeTimestamp && (hasBody || f.Sensitivity == "secret") {
			needTime = true
		}
	}
	if len(c.slots) > 0 && needsFmtForSlotFields(e) {
		needFmt = true
	}
	needUUID := w.envelope && (c.ops[types.OpCreate] || c.ops[types.OpUpsert])
	w.f("package %s\n\nimport (\n", path.Base(ctx.OutputNamespace))
	w.f("\t\"context\"\n\t\"encoding/json\"\n")
	// Each read and list waits through AwaitWrite, and each write through AwaitRevocation, and each tests its error with
	// errors.Is (ST-SUP-RC20-OC41, ST-SUP-RC20-OC49).
	w.f("\t\"errors\"\n")
	if needFmt {
		w.f("\t\"fmt\"\n")
	}
	if hasBody {
		w.f("\t\"io\"\n")
	}
	w.f("\t\"net/http\"\n")
	if c.ops[types.OpList] || w.checksKept() && keptJSON(e) {
		w.f("\t\"strconv\"\n")
	}
	if w.checksKept() && keptJSON(e) {
		w.f("\t\"reflect\"\n")
	}
	if w.decodesClientTime() || w.checksKept() {
		w.f("\t\"strings\"\n")
	}
	if needTime {
		w.f("\t\"time\"\n")
	}
	w.f("\n")
	if needUUID {
		w.f("\t\"github.com/google/uuid\"\n\n")
	}
	// Each handler binds the verified identity through command.BindIdentity.
	w.f("\t\"github.com/910-software-solutions/stego/registry/runtime/command\"\n")
	if hasWrite {
		w.f("\t\"github.com/910-software-solutions/stego/registry/runtime/idempotency\"\n")
	}
	if c.ops[types.OpRead] || c.ops[types.OpList] {
		w.f("\t\"github.com/910-software-solutions/stego/registry/runtime/query\"\n")
	}
	if w.hasSecret() {
		w.f("\t\"github.com/910-software-solutions/stego/registry/runtime/sealed\"\n")
	}
	w.f("\n\tdata %q\n", dataImport)
	if len(c.slots) > 0 {
		w.f("\tslots %q\n", slotsImportPath)
	}
	w.f(")\n\n")
	w.declarations()
	w.f("// %sHandler serves the collection %s on the command and the query runtime (ST-SUP-RC20-OC37): each\n", c.pascal, c.decl.Name)
	w.f("// operation calls the Run of its generated command or query, and the handler holds no store.\n")
	w.f("type %sHandler struct {\n\trt *Runtime\n", c.pascal)
	for _, sp := range c.slots {
		w.f("\t%s slots.%s\n", sp.FieldName, sp.InterfaceType)
	}
	w.f("}\n\n")
	w.f("// New%sHandler gives the handler of %s on rt, with the operators of its bound slots.\n", c.pascal, c.decl.Name)
	w.f("func New%sHandler(rt *Runtime", c.pascal)
	for _, sp := range c.slots {
		w.f(", %s slots.%s", sp.FieldName, sp.InterfaceType)
	}
	w.f(") *%sHandler {\n\th := &%sHandler{rt: rt", c.pascal, c.pascal)
	for _, sp := range c.slots {
		w.f(", %s: %s", sp.FieldName, sp.FieldName)
	}
	w.f("}\n")
	var deliver runtimeHandlerWriter
	deliver.c = c
	if deliver.deliverAfter() {
		w.f("\t// The outbox worker runs the after slots of the collection (ST-SUP-RC20-OC34).\n\trt.AfterSlots.add(%q, h.deliverAfter)\n", c.decl.Name)
	}
	w.f("\treturn h\n}\n\n")
	w.Write(deliver.Bytes())
	w.helpers()
	for _, op := range c.decl.Operations {
		switch op {
		case types.OpCreate:
			w.create()
		case types.OpRead:
			w.read()
		case types.OpList:
			w.list()
		case types.OpUpdate:
			w.update()
		case types.OpPatch:
			w.patch()
		case types.OpDelete:
			w.remove()
		case types.OpUpsert:
			w.upsert()
		}
	}
	formatted, err := format.Source(w.Bytes())
	if err != nil {
		return nil, fmt.Errorf("formatting the handler: %w\n%s", err, w.Bytes())
	}
	return formatted, nil
}

// commandVar is the variable of an operation's command or query declaration.
func (w *runtimeHandlerWriter) commandVar(op types.Operation) string {
	switch op {
	case types.OpRead, types.OpList:
		return w.c.camel + collectionToPascalCase(string(op)) + "Query"
	}
	return w.c.camel + collectionToPascalCase(string(op)) + "Command"
}

// declarations writes the command of each write operation and the query of each read operation. A command is named
// by its operation, which its audit rows carry as the action (ST-SUP-RC28-OC02); an operation that decides a
// permission decides it on the row of the request's ID before the fill (ST-SUP-RP10-LC31).
func (w *runtimeHandlerWriter) declarations() {
	c, e := w.c, w.c.entity
	table := commandTableName(e.Name)
	for _, op := range c.decl.Operations {
		switch op {
		case types.OpRead:
			w.f("// %s is the query of a read of %s (ST-SUP-RP11).\nvar %s = mustQuery(query.NewQuery(%q, data.Entity%sView()%s))\n\n", w.commandVar(op), c.decl.Name, w.commandVar(op), string(op), e.Name, w.ancestorViews(false))
		case types.OpList:
			w.f("// %s is the query of a list of %s, on the list permission of %s (ST-SUP-RP11-LC07).\nvar %s = mustQuery(query.NewListQuery(%q, data.Entity%sView(), %q%s))\n\n", w.commandVar(op), c.decl.Name, e.Name, w.commandVar(op), string(op), e.Name, e.Permissions[types.OpList], w.ancestorViews(true))
		default:
			w.f("// %s is the generated command of a %s of %s (ST-SUP-RC20-OC37).\n", w.commandVar(op), op, c.decl.Name)
			writes := []string{"data." + e.Name + "Entity"}
			for _, f := range c.refs {
				if name := "data." + f.To + "Entity"; op != types.OpDelete && !slices.Contains(writes, name) {
					writes = append(writes, name)
				}
			}
			// The fill reads each ancestor of the path through the handle: the parent of a create or an upsert with the
			// read permission of its entity, which the refs hold already, and every other ancestor through its link,
			// with no decision (ST-SUP-RC20-OC24).
			for i, level := range c.chain {
				name := "data." + level.entity + "Entity"
				if i > 0 || !decidesParent(op) {
					name = "data." + level.linkType + "Entity"
				}
				if !slices.Contains(writes, name) {
					writes = append(writes, name)
				}
			}
			// An update, a patch and a delete check the parent of the row through its link too.
			if len(c.chain) > 0 && !decidesParent(op) {
				writes = append(writes, "data."+c.rowLink+"Entity")
			}
			w.f("var %s = mustCommand(command.NewCommand(command.CommandSpec[struct{}]{\n\tName: %q,\n\tWrites: []command.Declared{%s},\n\tProbe: noProbe,\n", w.commandVar(op), string(op), strings.Join(writes, ", "))
			// The handle stages the change event of each write of a written entity (ST-SUP-RC20-OC40, ST-SUP-RP10-LC36).
			// The command runtime needs the names of each declared entity, also of a link: no write goes through a link,
			// so the names of its events are never staged.
			names := make([]string, len(writes))
			for i, name := range writes {
				names[i] = strings.TrimSuffix(strings.TrimPrefix(name, "data."), "Entity")
			}
			w.f("%s", command.ChangeEventsSpec(w.ctx, names))
			if op == types.OpUpsert {
				// The handle takes only the declared lists: the lookup of the upsert (ST-SUP-RP10-LC38).
				w.f("\tLists: []command.AnyList{%sUpsertKey},\n", c.camel)
			}
			var destinations []string
			if effects := w.afterEffects(op); len(effects) > 0 {
				destinations = append(destinations, fmt.Sprintf("{Name: AfterSlotsDestination, Idempotent: true, MustLand: true, Effects: []string{%s}}", quoteAll(effects)))
			}
			if len(destinations) > 0 {
				w.f("\tDestinations: []command.Destination{%s},\n", strings.Join(destinations, ", "))
			}
			// A create of an entity with no parent names no row: Run decides its permission on the service-level
			// resource <resource type>:<service name> of the tenant provisioning (ST-SUP-RC29-OC01, ST-SUP-RP10-LC31).
			if p := c.permission(op); p != "" && op == types.OpCreate && c.scope == "" {
				w.f("\tPermission: %q,\n\tResource: &command.Resource{Type: %q, Service: %q},\n", p, w.ctx.Tenancy.Provisioning.ResourceType, w.ctx.ServiceName)
			}
			// An upsert has no row to decide on before the fill: it decides its permission on the stored row in the
			// fill.
			if p := c.permission(op); p != "" && op != types.OpCreate && op != types.OpUpsert {
				// The table is named with its schema, so no relation of the same name earlier on the search path
				// stands in for it.
				token := fmt.Sprintf("SELECT authz_token FROM public.%s WHERE tenant_id = $1 AND id = $2::uuid FOR SHARE", table)
				w.f("\tPermission: %q,\n\tResource: &command.Resource{Type: %q, Token: %q, ValidID: command.ValidUUID},\n", p, e.ResourceType, token)
			}
			w.f("}))\n\n")
		}
	}
	if c.ops[types.OpUpsert] {
		w.f("// %sUpsertKey finds the row of an upsert by %s: its natural key, with its scope and implicit fields\n// (ST-SUP-RC20-OC13). It is a list of the upsert command over the entity of this release, whose parameters the\n// handler binds (ST-SUP-RP10-LC38).\nvar %sUpsertKey = command.Must(command.NewList(data.%sEntity, command.ListSpec{Name: \"upsert-key\", Read: %q,\n\tValues: command.DeclaredValues(%s)}))\n\n", c.camel, strings.Join(c.decl.UpsertLookup(), ", "), c.camel, e.Name, w.upsertList.Read, strings.Join(w.upsertList.Values, ", "))
	}
	if c.ops[types.OpList] {
		w.f("// %sSearchFields are the fields that a list search may name, with the kind of their values: the declared\n// fields that are not pii, secret or implicit, of a text, number or bool type (ST-SUP-RP11-LC20).\nvar %sSearchFields = map[string]query.FieldKind{", c.camel, c.camel)
		implicit := map[string]bool{}
		for _, kv := range c.implicit {
			implicit[kv[0]] = true
		}
		for _, f := range e.Fields {
			// An implicit field has one value in the collection, which the list filters on already.
			if kind := searchKind(f); kind != "" && !implicit[f.Name] {
				w.f("%q: query.%s, ", f.Name, kind)
			}
		}
		w.f("}\n\n")
		// A public string field out of the search is named in the 400 of a search or an order that names it
		// (ST-SUP-RC20-OC57, decision ST-DECL-Q50).
		if out := unsearchedFields(e, implicit); len(out) > 0 {
			w.f("// %sUnsearchedFields are the public string fields that a list neither searches nor orders, as they have no\n// search index: each sets searchable: false or has a max_length over 500 (ST-SUP-RC20-OC57).\nvar %sUnsearchedFields = map[string]query.FieldKind{", c.camel, c.camel)
			for _, name := range out {
				w.f("%q: query.KindText, ", name)
			}
			w.f("}\n\n")
		}
	}
	if fields := w.fingerprintFields(types.OpCreate); c.ops[types.OpCreate] {
		w.f("// %sCreateFields are the body fields that the fingerprint of a create covers (ST-SUP-RC30-OC03).\nvar %sCreateFields = []string{%s}\n\n", c.camel, c.camel, quoteAll(fields))
	}
	if fields := w.fingerprintFields(types.OpPatch); c.ops[types.OpPatch] {
		w.f("// %sPatchFields are the body fields that the fingerprint of a patch covers (ST-SUP-RC30-OC03).\nvar %sPatchFields = []string{%s}\n\n", c.camel, c.camel, quoteAll(fields))
	}
}

// ancestorViews gives the views of the ancestors of the path, which a query of the collection reads too
// (ST-SUP-RC20-OC24), each after a comma: the view of the parent of a list, on which it decides the read permission,
// and the link of every other ancestor, which decides nothing.
func (w *runtimeHandlerWriter) ancestorViews(list bool) string {
	var views string
	for i, level := range w.c.chain {
		if i == 0 && list {
			views += ", data.Entity" + level.entity + "View()"
		} else {
			views += ", data.Entity" + level.linkType + "View()"
		}
	}
	return views
}

// decidesParent tells whether the parent of the path is the deepest resource that a request of the operation op
// names: a list, a create and an upsert name no row of the collection, so they decide on the parent, and a read, an
// update, a patch and a delete decide on the row (ST-SUP-RC20-OC24).
func decidesParent(op types.Operation) bool {
	return op == types.OpList || op == types.OpCreate || op == types.OpUpsert
}

// fingerprintFields are the body fields that the client gives for an operation: for patch the patchable fields, and
// for create the entity's fields that are not server-managed.
func (w *runtimeHandlerWriter) fingerprintFields(op types.Operation) []string {
	var fields []string
	if op == types.OpPatch {
		for _, f := range w.c.patchFields() {
			fields = append(fields, f.Name)
		}
		return fields
	}
	for _, f := range w.c.entity.Fields {
		if !isServerManaged(f) {
			fields = append(fields, f.Name)
		}
	}
	return fields
}

func quoteAll(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(quoted, ", ")
}

// commandTableName is the table of an entity on the runtime data path, as the PostgreSQL adapter creates it
// (ST-SUP-RC01-OC41).
func commandTableName(entity string) string { return postgresadapter.TableName(entity) }

// helpers writes the conversions of the collection's entity: from a stored row to the API type, from the
// read-only value to it, the redaction of its secret fields and its presentation.
func (w *runtimeHandlerWriter) helpers() {
	c, e := w.c, w.c.entity
	w.f("// %sFromView gives the API value of a read-only %s. A secret field is never read (ST-DECL-EN03).\n", c.camel, e.Name)
	w.f("func %sFromView(v data.%sView) %s {\n\treturn %s{ID: v.ID", c.camel, e.Name, e.Name, e.Name)
	for _, f := range e.Fields {
		if f.Sensitivity == "secret" {
			continue
		}
		name := toPascalCase(f.Name)
		expr := "v." + name
		if f.Type == types.FieldTypeFloat {
			expr = "float32(" + expr + ")"
		}
		// The view holds an optional field as a pointer, nil for NULL, except a jsonb or bytes field, which is
		// nil for NULL as it is (ST-SUP-Q128 (B)).
		switch {
		case f.Optional && f.Type == types.FieldTypeJsonb:
			expr = c.camel + "JSON(" + expr + ")"
		case f.Optional && f.Type == types.FieldTypeFloat:
			expr = "float32Of(v." + name + ")"
		case f.Optional && f.Type == types.FieldTypeBytes:
			expr = "ptrOf(" + expr + ")"
		}
		w.f(", %s: %s", name, expr)
	}
	w.f("}\n}\n\n")
	w.converters()
	w.f("// %sPresented gives the response value of one %s: its secret fields are cleared, so no response and no stored\n// response holds them.\n", c.camel, e.Name)
	if len(c.chain) > 1 {
		w.f("func %sPresented(v %s, chain [%d]string) any {\n", c.camel, e.Name, len(c.chain))
	} else {
		w.f("func %sPresented(v %s) any {\n", c.camel, e.Name)
	}
	// A timestamp is answered in UTC, whatever offset the database driver gave it.
	for _, f := range e.Fields {
		if f.Type != types.FieldTypeTimestamp || f.Sensitivity == "secret" {
			continue
		}
		if name := toPascalCase(f.Name); f.Optional {
			w.f("\tif v.%s != nil {\n\t\tutc := v.%s.UTC()\n\t\tv.%s = &utc\n\t}\n", name, name, name)
		} else {
			w.f("\tv.%s = v.%s.UTC()\n", name, name)
		}
	}
	for _, f := range e.Fields {
		if f.Sensitivity != "secret" {
			continue
		}
		if f.Optional {
			w.f("\tv.%s = nil\n", toPascalCase(f.Name))
		} else {
			w.f("\tv.%s = %s\n", toPascalCase(f.Name), zeroOf(fieldTypeToGo(f.Type)))
		}
	}
	if w.envelope {
		w.f("\treturn presentEntity(v, %q, v.ID, %s)\n}\n\n", e.Name, c.hrefExpr("v"))
	} else {
		w.f("\treturn v\n}\n\n")
	}
	if c.ops[types.OpCreate] || c.ops[types.OpUpdate] || c.ops[types.OpUpsert] {
		w.f("// decode%s gives the %s of a create, an update or an upsert body. Each member that is not kind, id, href or a\n// field of %s, in its exact letter case, is refused, and so is a kind member that is not %s and an implicit field\n// with another value than that of the collection; the client values of the server-managed fields are cleared\n// (ST-SUP-RC20-OC55, ST-SUP-RC20-OC17, ST-SUP-RC20-OC19, ST-SUP-RC20-OC23).\n", c.pascal, e.Name, e.Name, e.Name)
		w.f("func decode%s(bodyBytes []byte) (%s, error) {\n", c.pascal, e.Name)
		w.f("\tvar rawMap map[string]json.RawMessage\n\tif err := json.Unmarshal(bodyBytes, &rawMap); err != nil {\n\t\treturn %s{}, &invalidRequest{problem: BadRequest(\"the body is not a JSON object\")}\n\t}\n", e.Name)
		w.f("\tif member, twice := duplicateMember(bodyBytes); twice {\n\t\treturn %s, &invalidRequest{problem: Validation([]ValidationError{{Field: memberName(member), Message: \"the body gives this member more than once\"}})}\n\t}\n", e.Name+"{}")
		w.writeMembers()
		w.f("\tif kindRaw, ok := rawMap[\"kind\"]; ok {\n\t\tvar kind string\n\t\tif err := json.Unmarshal(kindRaw, &kind); err != nil || kind != %q {\n\t\t\treturn %s{}, &invalidRequest{problem: BadRequest(\"kind must be %s\")}\n\t\t}\n\t}\n", e.Name, e.Name, e.Name)
		w.implicitMembers()
		w.clientTimeText(e.Name+"{}", e.Fields)
		w.stripServerMembers()
		w.f("\tvar %s %s\n", c.lower, e.Name)
		w.presetDefaults(c.lower, e.Fields)
		w.f("\tif err := json.Unmarshal(bodyBytes, &%s); err != nil {\n\t\treturn %s{}, &invalidRequest{problem: BadRequest(\"the body does not decode into %s\")}\n\t}\n", c.lower, e.Name, e.Name)
		w.clientTimeValue(c.lower, e.Name+"{}", e.Fields, false)
		w.implicitValues()
		w.f("\t%s.ID = \"\"\n", c.lower)
		emitClearServerManagedFields(&w.Buffer, c.lower, e)
		w.canonicalRefs(c.lower, e.Fields, false)
		w.f("\treturn %s, nil\n}\n\n", c.lower)
	}
	if c.ops[types.OpPatch] {
		w.f("// %sPatchRequest holds the patchable fields of %s; a nil field is not changed (ST-SUP-RC20-OC15).\ntype %sPatchRequest struct {\n", c.pascal, c.decl.Name, c.pascal)
		for _, f := range c.patchFields() {
			w.f("\t%s *%s `json:%q`\n", toPascalCase(f.Name), fieldTypeToGo(f.Type), f.Name)
		}
		w.f("}\n\n")
		w.f("// decode%sPatch gives the patch of a patch body. A kind member that is not %s is refused, and so is each\n// member that is not kind or the API name of a patchable field, in its exact letter case (ST-SUP-RC20-OC15).\n", c.pascal, e.Name)
		w.f("func decode%sPatch(bodyBytes []byte) (%sPatchRequest, error) {\n", c.pascal, c.pascal)
		w.f("\tvar rawMap map[string]json.RawMessage\n\tif err := json.Unmarshal(bodyBytes, &rawMap); err != nil {\n\t\treturn %sPatchRequest{}, &invalidRequest{problem: BadRequest(\"the body is not a JSON object\")}\n\t}\n", c.pascal)
		w.f("\tif member, twice := duplicateMember(bodyBytes); twice {\n\t\treturn %s, &invalidRequest{problem: Validation([]ValidationError{{Field: memberName(member), Message: \"the body gives this member more than once\"}})}\n\t}\n", c.pascal+"PatchRequest{}")
		w.patchMembers()
		w.f("\tif kindRaw, ok := rawMap[\"kind\"]; ok {\n\t\tvar kind string\n\t\tif err := json.Unmarshal(kindRaw, &kind); err != nil || kind != %q {\n\t\t\treturn %sPatchRequest{}, &invalidRequest{problem: BadRequest(\"kind must be %s\")}\n\t\t}\n\t}\n", e.Name, c.pascal, e.Name)
		w.clientTimeText(c.pascal+"PatchRequest{}", c.patchFields())
		w.f("\tvar patch %sPatchRequest\n\tif err := json.Unmarshal(bodyBytes, &patch); err != nil {\n\t\treturn %sPatchRequest{}, &invalidRequest{problem: BadRequest(\"the body does not decode into the patch of %s\")}\n\t}\n", c.pascal, c.pascal, e.Name)
		w.clientTimeValue("patch", c.pascal+"PatchRequest{}", c.patchFields(), true)
		w.canonicalRefs("patch", c.patchFields(), true)
		w.f("\treturn patch, nil\n}\n\n")
	}
	w.keptCheck()
}

// patchMembers writes, in the decode of a patch body, the refusal of each member that is not kind or the API name of
// a patchable field. The JSON decoder ignores an unknown member and matches a member name to a field with no regard
// to case, so a patch that names a field it cannot change, or the declared name of a renamed field, would answer 200
// and change nothing. The problem names the first such member in byte order and holds no value (ST-SUP-RC20-OC15,
// security review of PR #461, M1).
func (w *runtimeHandlerWriter) patchMembers() {
	c := w.c
	names := []string{"kind"}
	for _, f := range c.patchFields() {
		if !slices.Contains(names, f.Name) {
			names = append(names, f.Name)
		}
	}
	allowed := slices.Clone(names)
	// A field that a collection write cannot change is checked by its own rule: the body may give its stored value
	// (ST-SUP-RC20-OC46).
	for _, f := range w.keptFields() {
		if _, scoped := c.decl.Scope[f.Name]; !scoped && !slices.Contains(allowed, f.Name) {
			allowed = append(allowed, f.Name)
		}
	}
	quoted := make([]string, len(allowed))
	for i, name := range allowed {
		quoted[i] = strconv.Quote(name)
	}
	message := "the member is not a patchable field of " + c.decl.Name + "; a patch takes only " + strings.Join(names[1:], ", ") + " and kind"
	w.unknownMembers(c.pascal+"PatchRequest{}", quoted, message)
}

// unknownMembers writes the refusal of each member of rawMap that is not one of the quoted names, compared in their
// exact letter case, with a 400 validation problem that names the first such member in byte order and holds no value
// (ST-SUP-RC20-OC15, ST-SUP-RC20-OC55).
func (w *runtimeHandlerWriter) unknownMembers(zero string, quoted []string, message string) {
	w.f("\tunknown := \"\"\n\tfor member := range rawMap {\n\t\tswitch member {\n\t\tcase %s:\n\t\tdefault:\n\t\t\tif unknown == \"\" || member < unknown {\n\t\t\t\tunknown = member\n\t\t\t}\n\t\t}\n\t}\n", strings.Join(quoted, ", "))
	w.f("\tif unknown != \"\" {\n\t\treturn %s, &invalidRequest{problem: Validation([]ValidationError{{Field: memberName(unknown), Message: %q}})}\n\t}\n", zero, message)
}

// writeMembers writes, in the decode of a create, an update or an upsert body, the refusal of each member that is not
// kind, id, href in the envelope format or the API name of a field of the entity. The JSON decoder ignores an unknown
// member and matches a member name to a field with no regard to case, so a body that gives the declared name of a
// renamed field, a typo, a tenant or a name in another letter case would answer 2xx and set nothing. A member that a
// read gives and the server sets is taken, and the decode clears its value (ST-SUP-RC20-OC55, ST-SUP-RC20-OC17); a
// field that a collection write cannot change, the scope field and each implicit field are taken and checked by their
// own rules (ST-SUP-RC20-OC44, ST-SUP-RC20-OC45, ST-SUP-RC20-OC23).
func (w *runtimeHandlerWriter) writeMembers() {
	e := w.c.entity
	names := []string{"kind", "id"}
	if w.envelope {
		names = append(names, "href")
	}
	for _, f := range e.Fields {
		if !slices.Contains(names, f.Name) {
			names = append(names, f.Name)
		}
	}
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = strconv.Quote(name)
	}
	message := "the member is not a field of " + e.Name + "; a create, an update or an upsert takes only the fields of " + e.Name + " and kind, in the letter case of the OpenAPI document"
	w.unknownMembers(e.Name+"{}", quoted, message)
}

// stripServerMembers writes, in the decode of a create, an update or an upsert body, the removal of each member that
// the server sets, id, href and each server-managed field, before the body is decoded into the entity: the server
// ignores their values, so a value of another JSON type is not a decode failure (ST-SUP-RC20-OC17, ST-SUP-RC20-OC55,
// security review of PR #464).
func (w *runtimeHandlerWriter) stripServerMembers() {
	names := []string{strconv.Quote("id"), strconv.Quote("href")}
	for _, f := range w.c.entity.Fields {
		if isServerManaged(f) {
			names = append(names, strconv.Quote(f.Name))
		}
	}
	w.f("\tfor _, member := range []string{%s} {\n\t\tdelete(rawMap, member)\n\t}\n", strings.Join(names, ", "))
	w.f("\tif stripped, err := json.Marshal(rawMap); err == nil {\n\t\tbodyBytes = stripped\n\t}\n")
}

// implicitValues writes, in the decode of a create, an update or an upsert body, the refusal of each implicit field
// whose decoded value is not the declared one, as the scope check reads the decoded value too (ST-SUP-RC20-OC23,
// security review of PR #464).
func (w *runtimeHandlerWriter) implicitValues() {
	c := w.c
	for _, kv := range c.implicit {
		message := "the collection " + c.decl.Name + " sets this field itself; leave it out or give the value of the collection"
		for _, f := range c.entity.Fields {
			if f.Name != kv[0] {
				continue
			}
			cond := fmt.Sprintf("%s.%s != \"\" && %s.%s != %q", c.lower, toPascalCase(f.Name), c.lower, toPascalCase(f.Name), kv[1])
			if f.Optional {
				cond = fmt.Sprintf("%s.%s != nil && *%s.%s != %q", c.lower, toPascalCase(f.Name), c.lower, toPascalCase(f.Name), kv[1])
			}
			w.f("\tif %s {\n\t\treturn %s{}, &invalidRequest{problem: Validation([]ValidationError{{Field: %q, Message: %q}})}\n\t}\n", cond, c.entity.Name, kv[0], message)
		}
	}
}

// implicitMembers writes, in the decode of a create, an update or an upsert body, the refusal of each implicit field
// of the collection that the body gives with another value than the declared one, with a 400 validation problem that
// names the field and holds no value. A body that leaves the field out, gives null or gives the declared value is
// taken, and the handler sets the declared value (ST-SUP-RC20-OC23). An implicit field is a text or an enum field
// (validateRuntimeScope).
func (w *runtimeHandlerWriter) implicitMembers() {
	c := w.c
	for _, kv := range c.implicit {
		message := "the collection " + c.decl.Name + " sets this field itself; leave it out or give the value of the collection"
		w.f("\tif raw, ok := rawMap[%q]; ok && string(raw) != \"null\" {\n\t\tvar value string\n\t\tif err := json.Unmarshal(raw, &value); err != nil || value != %q {\n", kv[0], kv[1])
		w.f("\t\t\treturn %s{}, &invalidRequest{problem: Validation([]ValidationError{{Field: %q, Message: %q}})}\n\t\t}\n\t}\n", c.entity.Name, kv[0], message)
	}
}

// clientTimeExample is the example of an RFC 3339 time in the problem of a client-set timestamp.
const clientTimeExample = "2026-11-14T09:30:00Z"

// decodesClientTime tells whether a decode of the collection reads a timestamp that the client sets (ST-DECL-Q16):
// the body of a create, an update or an upsert reads every field, and a patch body its patchable fields.
func (w *runtimeHandlerWriter) decodesClientTime() bool {
	c := w.c
	fields := c.patchFields()
	if c.ops[types.OpCreate] || c.ops[types.OpUpdate] || c.ops[types.OpUpsert] {
		fields = c.entity.Fields
	}
	for _, f := range fields {
		if f.ClientSetTime() && (c.ops[types.OpCreate] || c.ops[types.OpUpdate] || c.ops[types.OpUpsert] || c.ops[types.OpPatch]) {
			return true
		}
	}
	return false
}

// clientTimeText writes, in the decode of a body before the body is decoded, the check of each client-set timestamp
// of fields (ST-DECL-Q16): a member that is not null must be a JSON string that holds an RFC 3339 time, else the
// request is refused with a 400 problem that names the field. A time whose offset moves its UTC year outside 1 to
// 9999, such as 9999-12-31T23:59:59-14:00, is refused the same way, as the answer in UTC cannot encode it. The JSON decoder matches a member name to a field
// with no regard to case, so each member whose name is the field's name in any case is checked.
func (w *runtimeHandlerWriter) clientTimeText(zero string, fields []types.Field) {
	for _, f := range fields {
		if !f.ClientSetTime() {
			continue
		}
		invalid := fmt.Sprintf("&invalidRequest{problem: BadRequest(%q)}", f.Name+" must be an RFC 3339 time, such as "+clientTimeExample)
		w.f("\tfor member, raw := range rawMap {\n\t\tif !strings.EqualFold(member, %q) || string(raw) == \"null\" {\n\t\t\tcontinue\n\t\t}\n", f.Name)
		w.f("\t\tvar text string\n\t\tif err := json.Unmarshal(raw, &text); err != nil {\n\t\t\treturn %s, %s\n\t\t}\n", zero, invalid)
		w.f("\t\tparsed, err := time.Parse(time.RFC3339, text)\n\t\tif err != nil {\n\t\t\treturn %s, %s\n\t\t}\n", zero, invalid)
		year := fmt.Sprintf("&invalidRequest{problem: BadRequest(%q)}", f.Name+" must be an RFC 3339 time whose UTC year is from 1 to 9999, such as "+clientTimeExample)
		w.f("\t\tif utcYear := parsed.UTC().Year(); utcYear < 1 || utcYear > 9999 {\n\t\t\treturn %s, %s\n\t\t}\n\t}\n", zero, year)
	}
}

// clientTimeValue writes, after the decode of a body, the check of each client-set timestamp of the decoded value v
// (ST-DECL-Q16): the zero time is refused, as an answer cannot tell it from no value, and a required field that the
// body leaves out or sends as null is refused, except in a patch, where an absent member changes nothing. The column
// keeps the instant, and each answer gives it in UTC (the Presented function), whatever offset the client sent.
func (w *runtimeHandlerWriter) clientTimeValue(v, zero string, fields []types.Field, pointers bool) {
	for _, f := range fields {
		if !f.ClientSetTime() {
			continue
		}
		name := v + "." + toPascalCase(f.Name)
		if f.Optional || pointers {
			invalid := fmt.Sprintf("&invalidRequest{problem: BadRequest(%q)}", f.Name+" must be an RFC 3339 time after the zero time, such as "+clientTimeExample)
			w.f("\tif %s != nil && %s.IsZero() {\n\t\treturn %s, %s\n\t}\n", name, name, zero, invalid)
			continue
		}
		if keptField(f) {
			// An update keeps the stored value of a field that a collection write cannot change, so its body may
			// leave the field out: the create checks it (keptCreate).
			continue
		}
		required := fmt.Sprintf("&invalidRequest{problem: BadRequest(%q)}", f.Name+" is required: an RFC 3339 time after the zero time, such as "+clientTimeExample)
		w.f("\tif %s.IsZero() {\n\t\treturn %s, %s\n\t}\n", name, zero, required)
	}
}

// canonicalRefs writes, in the decode of a body, the lower-case form of each ref field of the decoded value v. So
// the before and validate slots, the scope check and the stored row all see the one form of a UUID, whatever case
// the request sent (ST-SUP-RC20-OC24). Each field of a patch is a pointer, and pointers tells so.
func (w *runtimeHandlerWriter) canonicalRefs(v string, fields []types.Field, pointers bool) {
	for _, f := range fields {
		if f.Type != types.FieldTypeRef {
			continue
		}
		name := v + "." + toPascalCase(f.Name)
		if pointers || f.Optional {
			w.f("\tif %s != nil {\n\t\t*%s = canonicalID(*%s)\n\t}\n", name, name, name)
		} else {
			w.f("\t%s = canonicalID(%s)\n", name, name)
		}
	}
}

// hasJSON tells whether the collection's entity has a jsonb field. Its API type holds the field as json.RawMessage
// (a pointer to one when the field is optional), and the command runtime's row type as []byte, so a struct
// conversion between the two does not compile and the handler converts field by field.
func (w *runtimeHandlerWriter) hasJSON() bool {
	for _, f := range w.c.entity.Fields {
		if f.Type == types.FieldTypeJsonb {
			return true
		}
	}
	return false
}

// hasSecret tells whether the entity has a field of sensitivity secret, whose row value is of the sealing runtime
// (ST-SUP-RC20-OC48).
func (w *runtimeHandlerWriter) hasSecret() bool {
	for _, f := range w.c.entity.Fields {
		if f.Secret() {
			return true
		}
	}
	return false
}

// converts tells whether the API value and the row value differ in a field, so they convert field by field.
func (w *runtimeHandlerWriter) converts() bool { return w.hasJSON() || w.hasSecret() }

// kept gives the row value of the API value expr of a write over the loaded row from: a secret field that the
// request leaves out or gives empty keeps its stored value, because no client can read it to send it back
// (ST-SUP-RC20-OC48).
func (w *runtimeHandlerWriter) kept(expr, from string) string {
	if !w.hasSecret() {
		return w.toData(expr)
	}
	return w.c.camel + "KeepSecrets(" + w.toData(expr) + ", " + from + ".Value())"
}

// toData gives the expression of the command runtime's row value of the API value expr.
func (w *runtimeHandlerWriter) toData(expr string) string {
	if w.converts() {
		return w.c.camel + "ToData(" + expr + ")"
	}
	return "data." + w.c.entity.Name + "(" + expr + ")"
}

// fromData gives the expression of the API value of the command runtime's row value expr.
func (w *runtimeHandlerWriter) fromData(expr string) string {
	if w.converts() {
		return w.c.camel + "FromData(" + expr + ")"
	}
	return w.c.entity.Name + "(" + expr + ")"
}

// converters writes, for an entity with a jsonb field, the field by field conversions between the API value and the
// command runtime's row value. An optional jsonb field that is not set is nil in both, so it is NULL in its column
// and absent from a response.
func (w *runtimeHandlerWriter) converters() {
	if !w.converts() {
		return
	}
	c, e := w.c, w.c.entity
	if w.hasSecret() {
		w.f("// %sText gives the text of an optional secret of a request, or empty when it is not set.\n", c.camel)
		w.f("func %sText(v *string) string {\n\tif v == nil {\n\t\treturn \"\"\n\t}\n\treturn *v\n}\n\n", c.camel)
		w.f("// %sJSONText gives the JSON text of an optional secret jsonb of a request, or empty when it is not set.\n", c.camel)
		w.f("func %sJSONText(v *json.RawMessage) string {\n\tif v == nil {\n\t\treturn \"\"\n\t}\n\treturn string(*v)\n}\n\n", c.camel)
		w.f("// %sKeepSecrets gives next with each secret field that it does not set taken from stored: a write keeps the\n// stored secret when the request leaves it out or gives it empty (ST-SUP-RC20-OC48).\n", c.camel)
		w.f("func %sKeepSecrets(next, stored data.%s) data.%s {\n", c.camel, e.Name, e.Name)
		for _, f := range e.Fields {
			if f.Secret() {
				name := toPascalCase(f.Name)
				w.f("\tif !next.%s.IsSet() {\n\t\tnext.%s = stored.%s\n\t}\n", name, name, name)
			}
		}
		w.f("\treturn next\n}\n\n")
	}
	for _, f := range e.Fields {
		if f.Type == types.FieldTypeJsonb && f.Optional {
			w.f("// %sJSON gives the API value of an optional jsonb column: nil when the column is NULL.\n", c.camel)
			w.f("func %sJSON(v []byte) *json.RawMessage {\n\tif v == nil {\n\t\treturn nil\n\t}\n\tr := json.RawMessage(v)\n\treturn &r\n}\n\n", c.camel)
			w.f("// %sJSONBytes gives the column value of an optional jsonb field: nil, which is NULL, when it is not set.\n", c.camel)
			w.f("func %sJSONBytes(v *json.RawMessage) []byte {\n\tif v == nil {\n\t\treturn nil\n\t}\n\treturn []byte(*v)\n}\n\n", c.camel)
			break
		}
	}
	field := func(f types.Field, to bool) string {
		name := toPascalCase(f.Name)
		switch {
		case f.Secret() && !to && (f.Optional || f.Type == types.FieldTypeJsonb):
			// No API value of a row holds a secret (ST-SUP-RC20-OC48).
			return "nil"
		case f.Secret() && !to:
			return `""`
		case f.Secret():
			text := "v." + name
			switch {
			case f.Type == types.FieldTypeJsonb && f.Optional:
				// A readable secret jsonb is sealed as its JSON text.
				text = c.camel + "JSONText(v." + name + ")"
			case f.Type == types.FieldTypeJsonb:
				text = "string(v." + name + ")"
			case f.Optional:
				text = c.camel + "Text(v." + name + ")"
			}
			if f.CompareOnly {
				return "sealed.ComparedOf(" + text + ")"
			}
			return "sealed.SecretOf(" + text + ")"
		case f.Type != types.FieldTypeJsonb:
			return "v." + name
		case f.Optional && to:
			return c.camel + "JSONBytes(v." + name + ")"
		case f.Optional:
			return c.camel + "JSON(v." + name + ")"
		case to:
			return "[]byte(v." + name + ")"
		}
		return "json.RawMessage(v." + name + ")"
	}
	w.f("// %sToData gives the command runtime's row value of an API value of %s.\n", c.camel, e.Name)
	w.f("func %sToData(v %s) data.%s {\n\treturn data.%s{ID: v.ID", c.camel, e.Name, e.Name, e.Name)
	for _, f := range e.Fields {
		w.f(", %s: %s", toPascalCase(f.Name), field(f, true))
	}
	w.f("}\n}\n\n")
	w.f("// %sFromData gives the API value of a command runtime's row value of %s.\n", c.camel, e.Name)
	w.f("func %sFromData(v data.%s) %s {\n\treturn %s{ID: v.ID", c.camel, e.Name, e.Name, e.Name)
	for _, f := range e.Fields {
		w.f(", %s: %s", toPascalCase(f.Name), field(f, false))
	}
	w.f("}\n}\n\n")
}

// zeroOf is the zero value of a Go type of a field.
func zeroOf(goType string) string {
	switch goType {
	case "string":
		return `""`
	case "bool":
		return "false"
	case "time.Time":
		return "time.Time{}"
	case "int32", "int64", "float32", "float64":
		return "0"
	}
	return "nil"
}

// defaultLiteral gives the Go literal of a field's default: strconv.Quote of a string default of a string field, and
// the decimal or boolean text of an integer or boolean default of an integer or boolean field. Any other default is
// refused, so no default text reaches the generated code as code.
func defaultLiteral(f types.Field) (string, error) {
	switch value := f.Default.(type) {
	case string:
		if f.Type == types.FieldTypeString || f.Type == types.FieldTypeEnum || f.Type == types.FieldTypeRef {
			return strconv.Quote(value), nil
		}
	case bool:
		if f.Type == types.FieldTypeBool {
			return strconv.FormatBool(value), nil
		}
	case int:
		if f.Type == types.FieldTypeInt32 || f.Type == types.FieldTypeInt64 {
			return strconv.FormatInt(int64(value), 10), nil
		}
	case int64:
		if f.Type == types.FieldTypeInt32 || f.Type == types.FieldTypeInt64 {
			return strconv.FormatInt(value, 10), nil
		}
	case uint64:
		if f.Type == types.FieldTypeInt64 {
			return strconv.FormatUint(value, 10), nil
		}
	case float64:
		if f.Type == types.FieldTypeDouble || f.Type == types.FieldTypeFloat && math.Abs(value) <= math.MaxFloat32 {
			return strconv.FormatFloat(value, 'g', -1, 64), nil
		}
	}
	if isFloatType(f.Type) {
		switch value := f.Default.(type) {
		case int:
			return strconv.FormatInt(int64(value), 10), nil
		case int64:
			return strconv.FormatInt(value, 10), nil
		case uint64:
			return strconv.FormatUint(value, 10), nil
		}
	}
	return "", fmt.Errorf("the field %s of type %s has the default %#v, which is not a string, an integer, a number or a boolean of its type", f.Name, f.Type, f.Default)
}

// isFloatType tells whether a field type is a float or a double.
func isFloatType(t types.FieldType) bool {
	return t == types.FieldTypeFloat || t == types.FieldTypeDouble
}

// presetsDefault tells whether the decode of a create or an update body writes the declared default of f into the
// value before it reads the body: a field with a default that the client sets, of a type whose default has a Go
// literal. A timestamp or a jsonb default stays with the column (ST-DECL-EN03).
func presetsDefault(f types.Field) bool {
	if f.Default == nil || isServerManaged(f) || f.Optional {
		return false
	}
	switch f.Type {
	case types.FieldTypeString, types.FieldTypeEnum, types.FieldTypeRef, types.FieldTypeBool, types.FieldTypeInt32,
		types.FieldTypeInt64, types.FieldTypeFloat, types.FieldTypeDouble:
		return true
	}
	return false
}

// presetDefaults writes, before the decode of a create or an update body into v, the declared default of each field
// of presetsDefault, so that a body that leaves out the field, or sends it as null, stores its default: the default
// is the value used when the field is absent (ST-DECL-EN03). Before this, an omitted enum field stored the empty
// text and the CHECK of its values failed the write with 500.
func (w *runtimeHandlerWriter) presetDefaults(v string, fields []types.Field) {
	for _, f := range fields {
		if !presetsDefault(f) {
			continue
		}
		// ValidateContext refused a default that has no Go literal (generationDefaults).
		literal, _ := defaultLiteral(f)
		w.f("\t%s.%s = %s\n", v, toPascalCase(f.Name), literal)
	}
}

// generationDefaults refuses an entity whose generation field, a field of presetsDefault or a field with writable
// never has a default with no Go literal (defaultLiteral), because the handlers write that default into the
// generated code.
func generationDefaults(entities []types.Entity) error {
	for _, e := range entities {
		for _, f := range e.Fields {
			// A create stores the default of a field with writable never, which the handler writes (keptDefaults).
			if f.Name == "generation" && f.Default != nil || presetsDefault(f) || keptField(f) && f.Writable == types.WritableNever && f.Default != nil {
				if _, err := defaultLiteral(f); err != nil {
					return fmt.Errorf("entity %s: %w", e.Name, err)
				}
			}
		}
	}
	return nil
}

// populateCreate writes the server values of a new row: the created_by and updated_by fields from the verified
// caller, each timestamp field at the time of the change and the generation from its default (ST-SUP-RC20-OC17).
func (w *runtimeHandlerWriter) populateCreate() {
	c := w.c
	for _, f := range c.entity.Fields {
		name := toPascalCase(f.Name)
		switch {
		case f.Name == "created_by" || f.Name == "updated_by":
			w.setField(f, "callerName(r.Context())")
		case f.Type == types.FieldTypeTimestamp && !f.ClientSetTime():
			w.setField(f, "time.Now().UTC()")
		case f.Name == "generation" && f.Default != nil:
			// ValidateContext refused a default that has no Go literal (generationDefaults).
			literal, _ := defaultLiteral(f)
			w.f("\t\t%s.%s = %s\n", c.lower, name, literal)
		}
	}
}

// clearServer writes the zero value into each server-managed field of the requested value, so that a slot that runs
// before the branch of an upsert is known sees none (ST-SUP-RC20-OC13, ST-SUP-RC20-OC17).
func (w *runtimeHandlerWriter) clearServer() {
	c := w.c
	first := true
	for _, f := range c.entity.Fields {
		if !isServerManaged(f) {
			continue
		}
		if first {
			w.f("\t\tvar serverBlank %s\n", c.entity.Name)
			first = false
		}
		name := toPascalCase(f.Name)
		w.f("\t\t%s.%s = serverBlank.%s\n", c.lower, name, name)
	}
}

// populateUpdate writes the server values of a changed row: the create-only values from the stored row, the
// generation raised by one, updated_by from the verified caller and updated_time at the time of the change
// (ST-SUP-RC20-OC16, ST-SUP-RC20-OC18).
func (w *runtimeHandlerWriter) populateUpdate() {
	c := w.c
	for _, f := range c.entity.Fields {
		if !isServerManaged(f) {
			continue
		}
		name := toPascalCase(f.Name)
		switch {
		case f.Name == "updated_by":
			w.setField(f, "callerName(r.Context())")
		case f.Name == "updated_time" && f.Type == types.FieldTypeTimestamp:
			w.setField(f, "time.Now().UTC()")
		case f.Name == "generation":
			if f.Optional {
				w.f("\t\tif stored.%s != nil {\n\t\t\t%s.%s = ptrOf(*stored.%s + 1)\n\t\t}\n", name, c.lower, name, name)
			} else {
				w.f("\t\t%s.%s = stored.%s + 1\n", c.lower, name, name)
			}
		default:
			w.f("\t\t%s.%s = stored.%s\n", c.lower, name, name)
		}
	}
}

func (w *runtimeHandlerWriter) setField(f types.Field, value string) {
	if f.Optional {
		w.f("\t\t%s.%s = ptrOf(%s)\n", w.c.lower, toPascalCase(f.Name), value)
		return
	}
	w.f("\t\t%s.%s = %s\n", w.c.lower, toPascalCase(f.Name), value)
}

// before writes the before slots of an operation inside its command's fill.
func (w *runtimeHandlerWriter) before(op types.Operation, patchFields []types.Field) {
	before, _ := slotsForOp(op, w.c.slots)
	for _, sp := range before {
		switch sp.SlotName {
		case "before_delete":
			emitBeforeDeleteSlot(&w.Buffer, w.slotsAlias, sp, w.c.entity.Name, emitRuntimeSlotCheck)
		case "before_patch":
			emitBeforePatchSlot(&w.Buffer, w.slotsAlias, sp, w.c.lower, w.c.entity, patchFields, emitRuntimeSlotCheck)
		default:
			emitBeforeSlot(&w.Buffer, w.slotsAlias, sp, w.c.lower, w.c.entity, emitRuntimeSlotCheck)
		}
	}
}

// after writes, inside the fill of an operation's command, the staging of each bound after slot in the command's
// transaction: the slot request is the slot's request of the operation, as JSON, and the effect is
// <collection>.<slot> on AfterSlotsDestination for the loaded value ref. So the slot runs only after the command
// committed, never after a replay or a before slot that halted the command, and the outbox worker runs it and
// retries a failure (ST-SUP-RC20-OC34).
func (w *runtimeHandlerWriter) after(op types.Operation, entityVar, ref string, patchFields []types.Field) {
	_, after := slotsForOp(op, w.c.slots)
	if len(after) == 0 {
		return
	}
	var buf bytes.Buffer
	for _, sp := range after {
		dispatchAfterSlot(&buf, w.slotsAlias, sp, entityVar, w.c.entity, op, patchFields, emitRuntimeAfterFailure)
	}
	text := afterCall.ReplaceAllStringFunc(buf.String(), func(call string) string {
		m := afterCall.FindStringSubmatch(call)
		indent, field, request := m[1], m[2], m[3]
		slot := ""
		for _, sp := range after {
			if sp.FieldName == field {
				slot = sp.SlotName
			}
		}
		return fmt.Sprintf("%[1]sslotPayload, err := json.Marshal(%[2]s)\n%[1]sif err != nil {\n%[1]s\treturn idempotency.Response{}, err\n%[1]s}\n"+
			"%[1]sif err := ch.Effects().Stage(ctx, AfterSlotsDestination, %[3]q, %[4]s, slotPayload); err != nil {\n%[1]s\treturn idempotency.Response{}, err\n%[1]s}\n",
			indent, request, w.c.decl.Name+"."+slot, ref)
	})
	if strings.Contains(text, afterCallMarker) {
		panic("rest-api: an after slot call was not replaced by its staging")
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		if line != "" {
			w.f("\t%s", line)
		}
	}
}

// afterEffects gives the effect names that the command of an operation stages for its after slots.
func (w *runtimeHandlerWriter) afterEffects(op types.Operation) []string {
	_, after := slotsForOp(op, w.c.slots)
	var names []string
	for _, sp := range after {
		names = append(names, w.c.decl.Name+"."+sp.SlotName)
	}
	return names
}

// deliverAfter writes the function that runs the after slots of the collection that the outbox worker delivers:
// it decodes the slot request and calls the bound operator, which a nil guard skips.
func (w *runtimeHandlerWriter) deliverAfter() bool {
	var after []collectionSlotParam
	seen := map[string]bool{}
	for _, op := range w.c.decl.Operations {
		_, a := slotsForOp(op, w.c.slots)
		for _, sp := range a {
			if !seen[sp.SlotName] {
				seen[sp.SlotName] = true
				after = append(after, sp)
			}
		}
	}
	if len(after) == 0 {
		return false
	}
	w.f("// deliverAfter runs one after slot of %s that the outbox worker delivers (ST-SUP-RC20-OC34).\n", w.c.decl.Name)
	w.f("func (h *%sHandler) deliverAfter(ctx context.Context, slot string, payload []byte) error {\n\tswitch slot {\n", w.c.pascal)
	for _, sp := range after {
		w.f("\tcase %q:\n\t\tif h.%s == nil {\n\t\t\treturn nil\n\t\t}\n\t\tvar request slots.%s\n", sp.SlotName, sp.FieldName, sp.RequestType)
		w.f("\t\tif err := json.Unmarshal(payload, &request); err != nil {\n\t\t\treturn err\n\t\t}\n\t\t_, err := h.%s.Evaluate(ctx, &request)\n\t\treturn err\n", sp.FieldName)
	}
	w.f("\t}\n\treturn errAfterSlot\n}\n\n")
	return true
}

// key writes the read of the idempotency key of a create or a patch: required when the declaration requires it,
// and the request's own key when the client sends none (ST-SUP-RC30-OC01). With or without the idempotency component, each
// answer from here on to a request with a key carries Idempotency-Kept: 1, a problem and a 5xx answer too
// (ST-SUP-RC30-OC08, ST-SUP-RC20-OC56).
func (w *runtimeHandlerWriter) key(op types.Operation) {
	required := w.keys != nil && w.keys.required[op]
	w.f("\tkey, err := idempotency.KeyFrom(r.Header, %t)\n\tif err != nil {\n\t\tkeyMissing(w, r, err)\n\t\treturn\n\t}\n", required)
	// The handler keeps the key that the request gives also with no idempotency component (ST-SUP-RC20-OC56).
	w.f("\tif key != \"\" {\n\t\tw.Header().Set(idempotency.KeptHeader, idempotency.KeptValue)\n\t}\n")
	w.f("\trequestID := command.NewID()\n\tif key == \"\" {\n\t\tkey = requestKey(requestID)\n\t}\n")
}

// start writes the binding of the verified caller and the read of the body.
func (w *runtimeHandlerWriter) start(body bool) {
	w.f("\tb, err := command.BindIdentity(r.Context(), subjectType)\n\tif err != nil {\n\t\thandleError(w, r, Unauthorized(\"the request is not authenticated\"))\n\t\treturn\n\t}\n")
	if w.c.scope != "" {
		// The parent of a scoped collection is the path parameter of its scope field (ST-SUP-RC20-OC24).
		w.f("\tscope := canonicalID(r.PathValue(%q))\n\tif !command.ValidUUID(scope) {\n\t\thandleError(w, r, NotFound(%q, scope))\n\t\treturn\n\t}\n", w.c.scope, w.c.decl.ParentEntity())
		// Each ancestor above the parent is a path parameter too, the parent first in chain (ST-SUP-RC20-OC24).
		w.f("\tchain := [%d]string{scope", len(w.c.chain))
		for _, level := range w.c.chain[1:] {
			w.f(", canonicalID(r.PathValue(%q))", level.param)
		}
		w.f("}\n")
		for i, level := range w.c.chain[1:] {
			w.f("\tif !command.ValidUUID(chain[%d]) {\n\t\thandleError(w, r, NotFound(%q, chain[%d]))\n\t\treturn\n\t}\n", i+1, level.entity, i+1)
		}
	}
	if body {
		w.readBody()
	}
}

// keyedStart writes the start of a create or a patch: the binding of the verified caller, the read of the
// idempotency key and then the read of the body, in the order of a command (Serve), so a malformed key gets
// idempotency-key-invalid also when the body cannot be read (ST-SUP-RC30-OC02; walkthrough 19, W19-U3).
func (w *runtimeHandlerWriter) keyedStart(op types.Operation) {
	w.start(false)
	w.key(op)
	w.readBody()
}

// readBody writes the read of the body, at most MaxRequestBody bytes.
func (w *runtimeHandlerWriter) readBody() {
	w.f("\tbody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBody))\n\tif err != nil {\n\t\thandleError(w, r, BadRequest(\"the body cannot be read\"))\n\t\treturn\n\t}\n")
}

// ancestors writes the parent chain check of a fill (ST-SUP-RC20-OC24, ST-SUP-Q156). It decides a permission only on
// the deepest resource that the request names. For a list, a create and an upsert (decideParent) that is the parent:
// the check reads it first with the read permission of its entity, which decides it, so a missing and a denied parent
// cost the same calls. For a read, an update, a patch and a delete it is the row, which the fill reads and decides
// before this check: a read loads it and checks it against the parent of the path first, and Run decides an update, a
// patch and a delete before the fill, whose check then reads the link of the row last and compares it with the parent
// of the path before the locked read. Each other ancestor is read through its link, with no decision: it must exist
// in the tenant and its scope field must name the ancestor above it in the path. The check reads these from the top
// ancestor down and stops at the first failure. A missing, denied or wrong ancestor is not
// found, the same answer for each, with the same calls to the authorization port, and no field of an ancestor is in
// a response. In a query (inQuery) the rows are read through the query handle qh, and in a command through the
// command handle ch; zero is the zero value that the fill returns with an error.
func (w *runtimeHandlerWriter) ancestors(inQuery bool, zero string, decideParent bool) {
	if len(w.c.chain) == 0 {
		return
	}
	notFound := "command.ErrNotFound"
	if inQuery {
		notFound = "query.ErrNotFound"
	}
	first := 0
	if decideParent {
		first = 1
		level := w.c.chain[0]
		w.f("\t\t// The parent is the deepest resource that the path names: it is read with the read permission of its entity,\n\t\t// which decides it (ST-SUP-RC20-OC24).\n\t\t{\n")
		if inQuery {
			w.f("\t\t\tparents, err := query.Repo(qh, data.Entity%sView())\n", level.entity)
		} else {
			w.f("\t\t\tparents, err := data.%sRepository(ch)\n", level.entity)
		}
		w.f("\t\t\tif err != nil {\n\t\t\t\treturn %s, err\n\t\t\t}\n", zero)
		w.f("\t\t\tparent, err := parents.Get(ctx, chain[0])\n\t\t\tif err != nil {\n\t\t\t\treturn %s, err\n\t\t\t}\n", zero)
		if level.link != "" {
			w.f("\t\t\tif parent.Value().%s != chain[1] {\n\t\t\t\treturn %s, %s\n\t\t\t}\n", toPascalCase(level.link), zero, notFound)
		} else {
			w.f("\t\t\t_ = parent\n")
		}
		w.f("\t\t}\n")
	}
	if first == len(w.c.chain) {
		return
	}
	w.f("\t\t// Each other ancestor of the path, from the top ancestor down: it exists in the tenant and is of the ancestor\n\t\t// above it. Its link holds only its key and that field, and decides no permission (ST-SUP-RC20-OC24).\n")
	for i := len(w.c.chain) - 1; i >= first; i-- {
		level := w.c.chain[i]
		w.f("\t\t{\n")
		if inQuery {
			w.f("\t\t\tlinks, err := query.Repo(qh, data.Entity%sView())\n", level.linkType)
		} else {
			w.f("\t\t\tlinks, err := data.%sRepository(ch)\n", level.linkType)
		}
		w.f("\t\t\tif err != nil {\n\t\t\t\treturn %s, err\n\t\t\t}\n", zero)
		w.f("\t\t\tlink, err := links.Get(ctx, chain[%d])\n\t\t\tif err != nil {\n\t\t\t\treturn %s, err\n\t\t\t}\n", i, zero)
		if level.link != "" {
			w.f("\t\t\tif link.Value().Link != chain[%d] {\n\t\t\t\treturn %s, %s\n\t\t\t}\n", i+1, zero, notFound)
		} else {
			w.f("\t\t\t_ = link\n")
		}
		w.f("\t\t}\n")
	}
	if !inQuery && !decideParent {
		// The row last: it is of the parent of the path. Its link is read before the row, so a row of another parent
		// costs no more calls than a missing ancestor; the locked read checks the stored row again.
		w.f("\t\t{\n\t\t\tlinks, err := data.%sRepository(ch)\n\t\t\tif err != nil {\n\t\t\t\treturn %s, err\n\t\t\t}\n", w.c.rowLink, zero)
		w.f("\t\t\tlink, err := links.Get(ctx, id)\n\t\t\tif err != nil {\n\t\t\t\treturn %s, err\n\t\t\t}\n\t\t\tif link.Value().Link != scope {\n\t\t\t\treturn %s, %s\n\t\t\t}\n\t\t}\n", zero, zero, notFound)
	}
}

// presentedArgs gives the arguments of the call of Presented on the value v: v, and the chain of a collection whose
// href names an ancestor above the parent.
func (w *runtimeHandlerWriter) presentedArgs(v string) string {
	if len(w.c.chain) > 1 {
		return v + ", chain"
	}
	return v
}

// respond writes the response of a fill.
func (w *runtimeHandlerWriter) respond(status, value string) {
	w.f("\t\tresponse, err := json.Marshal(%sPresented(%s))\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", w.c.camel, w.presentedArgs(value))
	w.f("\t\treturn jsonResponse(%s, response), nil\n", status)
}

// finish writes the answer of Run.
func (w *runtimeHandlerWriter) finish(op types.Operation, resource string) {
	w.f("\tif err != nil {\n\t\th.rt.answerWrite(w, r, req, %q, %q, %q, %t, err)\n\t\treturn\n\t}\n", string(op), w.c.entity.ResourceType, w.c.entity.Name, w.c.permission(op) != "")
	w.f("\tif result.Problem != \"\" {\n\t\tanswerKey(w, r, result)\n\t\treturn\n\t}\n")
	_ = resource
}

func (w *runtimeHandlerWriter) create() {
	c, e := w.c, w.c.entity
	w.f("// Create runs the generated command create of %s through Run: the before slots run in its fill, before the\n// write; the row, its audit row and the idempotency key commit together (ST-SUP-RC20-OC37).\n", c.decl.Name)
	w.f("func (h *%sHandler) Create(w http.ResponseWriter, r *http.Request) {\n", c.pascal)
	w.keyedStart(types.OpCreate)
	w.decodeBody("create")
	w.f("\tfingerprint, err := idempotency.FieldFingerprint(r.Method, r.URL.Path, body, %sCreateFields)\n\tif err != nil {\n\t\thandleError(w, r, BadRequest(\"the body is not a JSON object\"))\n\t\treturn\n\t}\n", c.camel)
	w.f("\treq := h.rt.commandRequest(r, b, key, fingerprint, requestID, body, \"\")\n")
	w.f("\tvar persisted %s\n", e.Name)
	w.awaitRevocation()
	w.f("\tresult, err := command.Run(r.Context(), h.rt.Commands, %s, req, decode%s, func(ctx context.Context, %s %s, ch *command.Handle[struct{}]) (idempotency.Response, error) {\n", w.commandVar(types.OpCreate), c.pascal, c.lower, e.Name)
	w.ancestors(false, "idempotency.Response{}", true)
	if w.envelope {
		w.f("\t\tid, err := uuid.NewV7()\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n\t\t%s.ID = id.String()\n", c.lower)
	} else {
		w.f("\t\t%s.ID = command.NewID()\n", c.lower)
	}
	w.populateCreate()
	w.keptCreate()
	w.forceScope("scope")
	w.before(types.OpCreate, nil)
	w.f("\t\trepo, err := data.%sRepository(ch)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", e.Name)
	w.parents("")
	w.f("\t\tcreated, err := repo.Create(ctx, %s, parents...)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", w.toData(c.lower))
	w.f("\t\tpersisted = %s\n", w.fromData("created.Value()"))
	w.after(types.OpCreate, "persisted", "created", nil)
	w.respond("http.StatusCreated", "persisted")
	w.f("\t})\n")
	w.finish(types.OpCreate, "")
	w.f("\twriteResult(w, result)\n}\n\n")
}

func (w *runtimeHandlerWriter) update() {
	c, e := w.c, w.c.entity
	w.f("// Update runs the generated command update of %s through Run, on the row of the path's ID.\n", c.decl.Name)
	w.f("func (h *%sHandler) Update(w http.ResponseWriter, r *http.Request) {\n\tid := canonicalID(r.PathValue(\"id\"))\n", c.pascal)
	w.start(true)
	w.decodeBody("update")
	w.f("\trequestID := command.NewID()\n\tfingerprint, err := idempotency.Fingerprint(r.Method, r.URL.Path, body)\n\tif err != nil {\n\t\thandleError(w, r, BadRequest(\"the body is not a JSON object\"))\n\t\treturn\n\t}\n")
	w.f("\treq := h.rt.commandRequest(r, b, requestKey(requestID), fingerprint, requestID, body, id)\n")
	w.f("\tvar persisted %s\n", e.Name)
	w.awaitRevocation()
	w.f("\tresult, err := command.Run(r.Context(), h.rt.Commands, %s, req, decode%s, func(ctx context.Context, %s %s, ch *command.Handle[struct{}]) (idempotency.Response, error) {\n", w.commandVar(types.OpUpdate), c.pascal, c.lower, e.Name)
	w.ancestors(false, "idempotency.Response{}", false)
	w.lockRow()
	w.f("\t\t%s.ID = id\n", c.lower)
	w.keptUpdate()
	w.populateUpdate()
	w.forceScope("stored." + toPascalCase(c.scope))
	w.before(types.OpUpdate, nil)
	w.parents("stored")
	w.f("\t\tupdated, err := %s(ctx, from, %s, parents...)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", updateCall(e), w.kept(c.lower, "from"))
	w.f("\t\tpersisted = %s\n", w.fromData("updated.Value()"))
	w.after(types.OpUpdate, "persisted", "updated", nil)
	w.respond("http.StatusOK", "persisted")
	w.f("\t})\n")
	w.finish(types.OpUpdate, "id")
	w.f("\twriteResult(w, result)\n}\n\n")
}

// storedValue gives the expression of the value of the implicit field name of the stored row, for an optional
// field the value it points to or the empty text.
func (w *runtimeHandlerWriter) storedValue(name string) string {
	for _, f := range w.c.entity.Fields {
		if f.Name == name && f.Optional {
			return fmt.Sprintf("command.StateOf(stored.%s)", toPascalCase(name))
		}
	}
	return "stored." + toPascalCase(name)
}

// viewDiffers gives the condition that the implicit field name of the loaded view does not hold value. The view
// holds an optional field as a pointer, nil for a NULL column (ST-SUP-Q128 (B)), and a NULL is another value: the
// row is not found in the collection.
func (w *runtimeHandlerWriter) viewDiffers(name, value string) string {
	field := "view." + toPascalCase(name)
	for _, f := range w.c.entity.Fields {
		if f.Name == name && f.Optional {
			return fmt.Sprintf("(%s == nil || *%s != %q)", field, field, value)
		}
	}
	return fmt.Sprintf("%s != %q", field, value)
}

// decodeBody writes the decode of the body of a create, an update or an upsert. On a scoped collection the path
// names the parent, so a body that gives the scope field another value is refused with a 400 problem that names the
// field, and not moved to the parent of the path in silence; a body that leaves the field out, or gives the value of
// the path, is taken. Kubernetes refuses an object whose namespace is not that of the request path in the same way,
// and AIP-203 refuses a changed value of an immutable field (walkthrough 8, step 30).
func (w *runtimeHandlerWriter) decodeBody(op string) {
	c, e := w.c, w.c.entity
	if c.scope == "" {
		w.f("\tif _, err := decode%s(body); err != nil {\n\t\th.rt.answerWrite(w, r, command.Request{}, %q, %q, %q, false, err)\n\t\treturn\n\t}\n", c.pascal, op, e.ResourceType, e.Name)
		return
	}
	w.f("\tgiven, err := decode%s(body)\n\tif err != nil {\n\t\th.rt.answerWrite(w, r, command.Request{}, %q, %q, %q, false, err)\n\t\treturn\n\t}\n", c.pascal, op, e.ResourceType, e.Name)
	w.f("\tif given.%s != \"\" && given.%s != scope {\n", toPascalCase(c.scope), toPascalCase(c.scope))
	w.f("\t\thandleError(w, r, Validation([]ValidationError{{Field: %q, Message: %q}}))\n\t\treturn\n\t}\n", c.scope,
		"the body names another "+c.decl.ParentEntity()+" than the path; leave the field out or give the value of the path")
}

// forceScope writes the server values of the scope field and the implicit fields of a written value: the scope
// field from scope, and each implicit field from its declared value (ST-SUP-RC20-OC23, ST-SUP-RC20-OC24).
func (w *runtimeHandlerWriter) forceScope(scope string) {
	c := w.c
	if c.scope != "" {
		w.f("\t\t%s.%s = %s\n", c.lower, toPascalCase(c.scope), scope)
	}
	for _, kv := range c.implicit {
		for _, f := range c.entity.Fields {
			if f.Name == kv[0] {
				w.setField(f, fmt.Sprintf("%q", kv[1]))
			}
		}
	}
}

// parents writes the load of the row that each ref field of the written value names, which the command runtime
// decides the reference on (ST-SUP-RP10-LC05). With stored, the value of a write of a stored row, it loads only a
// ref that the write changes, since a reference that stays needs no parent.
func (w *runtimeHandlerWriter) parents(stored string) {
	c := w.c
	w.f("\t\tvar parents []command.Ref\n")
	// Each ref already holds the lower-case form of its UUID, which the decode of the body gave (canonicalRefs).
	for _, f := range c.refs {
		name := toPascalCase(f.Name)
		v := c.lower + "." + name
		var cond, id string
		switch {
		case f.Optional && stored == "":
			cond, id = fmt.Sprintf("%s != nil && *%s != \"\"", v, v), "*"+v
		case f.Optional:
			cond, id = fmt.Sprintf("%s != nil && *%s != \"\" && (%s.%s == nil || *%s.%s != *%s)", v, v, stored, name, stored, name, v), "*"+v
		case stored == "":
			cond, id = "true", v
		default:
			cond, id = fmt.Sprintf("%s != %s.%s", v, stored, name), v
		}
		// A reference held within a field is compared with its row when that field moves too, so the write loads the
		// row then (ST-SUP-RP10-LC37).
		if within := c.entity.References[f.Name].WithinField(); within != "" && stored != "" {
			moved := w.withinMoved(within, stored)
			if f.Optional {
				cond = fmt.Sprintf("%s != nil && *%s != \"\" && (%s.%s == nil || *%s.%s != *%s || %s)", v, v, stored, name, stored, name, v, moved)
			} else {
				cond = fmt.Sprintf("%s != %s.%s || %s", v, stored, name, moved)
			}
		}
		if cond == "true" {
			w.f("\t\t{\n")
		} else {
			w.f("\t\tif %s {\n", cond)
		}
		w.f("\t\t\trefs, err := data.%sRepository(ch)\n\t\t\tif err != nil {\n\t\t\t\treturn idempotency.Response{}, err\n\t\t\t}\n", f.To)
		w.f("\t\t\tparent, err := refs.Get(ctx, %s)\n\t\t\tif err != nil {\n\t\t\t\treturn idempotency.Response{}, err\n\t\t\t}\n\t\t\tparents = append(parents, parent)\n\t\t}\n", id)
	}
}

// withinMoved gives the condition that the write of a stored row changes the field within of the entity.
func (w *runtimeHandlerWriter) withinMoved(within, stored string) string {
	c := w.c
	name := toPascalCase(within)
	v := c.lower + "." + name
	for _, f := range c.entity.Fields {
		if f.Name == within && f.Optional {
			return fmt.Sprintf("!command.EqualPointer(%s, %s.%s)", v, stored, name)
		}
	}
	return fmt.Sprintf("%s != %s.%s", v, stored, name)
}

// upsert writes the upsert of the collection: one generated command that finds the row of the natural key with no
// lock; a new row is created as a create is, and decided on the rows it refers to; a stored row is locked, decided on
// the upsert permission, and updated as an update is. It answers 201 for an insert and 200 for an update
// (ST-SUP-RC20-OC13). Two upserts of one new key that run at once meet the unique key: the later one runs again as
// a conflict and updates the row (ST-SUP-RP10-LC25).
func (w *runtimeHandlerWriter) upsert() {
	c, e := w.c, w.c.entity
	w.f("// Upsert runs the generated command upsert of %s through Run: it creates the row of the natural key %s, or\n// updates the stored one, and answers 201 or 200 (ST-SUP-RC20-OC13).\n", c.decl.Name, strings.Join(c.decl.UpsertKey, ", "))
	w.f("func (h *%sHandler) Upsert(w http.ResponseWriter, r *http.Request) {\n", c.pascal)
	w.start(true)
	w.decodeBody("upsert")
	w.f("\trequestID := command.NewID()\n\tfingerprint, err := idempotency.Fingerprint(r.Method, r.URL.Path, body)\n\tif err != nil {\n\t\thandleError(w, r, BadRequest(\"the body is not a JSON object\"))\n\t\treturn\n\t}\n")
	w.f("\treq := h.rt.commandRequest(r, b, requestKey(requestID), fingerprint, requestID, body, \"\")\n")
	optimistic := c.decl.Concurrency == types.ConcurrencyOptimistic
	if optimistic {
		w.f("\tifMatch, ifMatchSent, ifMatchValid := ifMatchGeneration(r)\n")
	}
	w.f("\tvar persisted %s\n", e.Name)
	w.awaitRevocation()
	w.f("\tresult, err := command.Run(r.Context(), h.rt.Commands, %s, req, decode%s, func(ctx context.Context, %s %s, ch *command.Handle[struct{}]) (idempotency.Response, error) {\n", w.commandVar(types.OpUpsert), c.pascal, c.lower, e.Name)
	w.ancestors(false, "idempotency.Response{}", true)
	// The before slots decide the requested value before the lookup, on both branches (ST-SUP-RC20-OC13). They see
	// no server-managed field: the branch is not known yet, so a create-time value would be wrong on the update
	// branch, and a client value is never taken (ST-SUP-RC20-OC17).
	w.clearServer()
	w.forceScope("scope")
	w.before(types.OpUpsert, nil)
	w.f("\t\trepo, err := data.%sRepository(ch)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", e.Name)
	var keys []string
	for _, key := range c.decl.UpsertLookup() {
		keys = append(keys, c.lower+"."+toPascalCase(key))
	}
	w.f("\t\tfound, err := repo.List(ctx, %sUpsertKey.With(%s), 1)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", c.camel, strings.Join(keys, ", "))
	w.f("\t\tif len(found) == 0 {\n")
	if optimistic {
		// An If-Match names a stored row, and there is none. The insert would be decided on edit of each required
		// parent through its reference; that decision comes first, so a caller who may not write under the parent gets
		// 404, the answer of a stored row it may not upsert, and the header tells no caller that a row exists
		// (ST-SUP-RC20-OC14, ST-SUP-RC20-OC37).
		w.f("\t\tif ifMatchSent {\n")
		for _, f := range c.refs {
			ref, ok := e.References[f.Name]
			if f.Optional || !ok || ref.Permission == "" {
				continue
			}
			v := c.lower + "." + toPascalCase(f.Name)
			w.f("\t\t\t{\n\t\t\t\trefs, err := data.%sRepository(ch)\n\t\t\t\tif err != nil {\n\t\t\t\t\treturn idempotency.Response{}, err\n\t\t\t\t}\n", f.To)
			w.f("\t\t\t\tparent, err := refs.Get(ctx, canonicalID(%s))\n\t\t\t\tif err != nil {\n\t\t\t\t\treturn idempotency.Response{}, err\n\t\t\t\t}\n", v)
			w.f("\t\t\t\tallowed, err := ch.Authz().Evaluate(ctx, %q, parent, nil)\n\t\t\t\tif err != nil {\n\t\t\t\t\treturn idempotency.Response{}, err\n\t\t\t\t}\n\t\t\t\tif !allowed {\n\t\t\t\t\treturn idempotency.Response{}, command.ErrNotFound\n\t\t\t\t}\n\t\t\t}\n", ref.Permission)
		}
		w.f("\t\t\treturn idempotency.Response{}, &preconditionFailed{status: http.StatusPreconditionFailed, detail: \"the If-Match precondition names a %s that does not exist\"}\n\t\t}\n", e.Name)
	}
	w.populateCreate()
	w.keptDefaults()
	if w.envelope {
		w.f("\t\tid, err := uuid.NewV7()\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n\t\t%s.ID = id.String()\n", c.lower)
	} else {
		w.f("\t\t%s.ID = command.NewID()\n", c.lower)
	}
	w.parents("")
	w.f("\t\tcreated, err := repo.Create(ctx, %s, parents...)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", w.toData(c.lower))
	// The check of the kept fields runs after the create decided the parent (keptCreateCheck).
	w.keptCreateCheck()
	w.f("\t\tpersisted = %s\n", w.fromData("created.Value()"))
	w.after(types.OpUpsert, "persisted", "created", nil)
	w.respond("http.StatusCreated", "persisted")
	w.f("\t\t}\n")
	w.f("\t\tfrom, err := repo.GetForUpdate(ctx, found[0].ID())\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n")
	w.f("\t\t// A stored row is decided on the upsert permission; a deny is not found (ST-SUP-RP11-LC06).\n")
	w.f("\t\tallowed, err := ch.Authz().Evaluate(ctx, %q, from, nil)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n\t\tif !allowed {\n\t\t\treturn idempotency.Response{}, command.ErrNotFound\n\t\t}\n", e.Permissions[types.OpUpsert])
	w.f("\t\tstored := %s\n\t\t%s.ID = stored.ID\n", w.fromData("from.Value()"), c.lower)
	w.notInCollection()
	if optimistic {
		// The generation that the client last read must be the stored one; the update raises it by one
		// (ST-SUP-RC20-OC14, ST-SUP-RC20-OC18).
		w.f("\t\tif !ifMatchSent {\n\t\t\treturn idempotency.Response{}, &preconditionFailed{status: http.StatusPreconditionRequired, detail: \"an upsert of a stored %s needs an If-Match precondition with its generation\"}\n\t\t}\n", e.Name)
		w.f("\t\tif !ifMatchValid || int64(stored.Generation) != ifMatch {\n\t\t\treturn idempotency.Response{}, &preconditionFailed{status: http.StatusPreconditionFailed, detail: \"the If-Match precondition does not name the stored generation of the %s\"}\n\t\t}\n", e.Name)
	}
	w.keptUpdate()
	w.populateUpdate()
	w.forceScope("scope")
	w.parents("stored")
	w.f("\t\tupdated, err := %s(ctx, from, %s, parents...)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", updateCall(e), w.kept(c.lower, "from"))
	w.f("\t\tpersisted = %s\n", w.fromData("updated.Value()"))
	w.after(types.OpUpsert, "persisted", "updated", nil)
	w.respond("http.StatusOK", "persisted")
	w.f("\t})\n")
	w.finish(types.OpUpsert, "")
	w.f("\twriteResult(w, result)\n}\n\n")
}

// lockRow writes the locked read of the row of the path's ID in a command's fill, as stored.
func (w *runtimeHandlerWriter) lockRow() {
	e := w.c.entity
	w.f("\t\trepo, err := data.%sRepository(ch)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", e.Name)
	w.f("\t\tfrom, err := repo.GetForDelete(ctx, id)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n")
	w.f("\t\tstored := %s\n\t\t_ = stored\n", w.fromData("from.Value()"))
	w.notInCollection()
}

// notInCollection writes the check of the stored row of a write: a row of another parent, or with other implicit
// values, is not found in this collection (ST-SUP-RC20-OC23, ST-SUP-RC20-OC24).
func (w *runtimeHandlerWriter) notInCollection() {
	var checks []string
	if w.c.scope != "" {
		checks = append(checks, fmt.Sprintf("stored.%s != scope", toPascalCase(w.c.scope)))
	}
	for _, kv := range w.c.implicit {
		checks = append(checks, fmt.Sprintf("%s != %q", w.storedValue(kv[0]), kv[1]))
	}
	if len(checks) > 0 {
		w.f("\t\tif %s {\n\t\t\treturn idempotency.Response{}, command.ErrNotFound\n\t\t}\n", strings.Join(checks, " || "))
	}
}

func (w *runtimeHandlerWriter) patch() {
	c, e := w.c, w.c.entity
	fields := c.patchFields()
	w.f("// Patch runs the generated command patch of %s through Run: it changes only the fields that the request\n// gives (ST-SUP-RC20-OC15).\n", c.decl.Name)
	w.f("func (h *%sHandler) Patch(w http.ResponseWriter, r *http.Request) {\n\tid := canonicalID(r.PathValue(\"id\"))\n", c.pascal)
	w.keyedStart(types.OpPatch)
	w.f("\tif _, err := decode%sPatch(body); err != nil {\n", c.pascal)
	w.f("\t\th.rt.answerWrite(w, r, command.Request{}, \"patch\", %q, %q, false, err)\n\t\treturn\n\t}\n", e.ResourceType, e.Name)
	w.f("\tfingerprint, err := idempotency.FieldFingerprint(r.Method, r.URL.Path, body, %sPatchFields)\n\tif err != nil {\n\t\thandleError(w, r, BadRequest(\"the body is not a JSON object\"))\n\t\treturn\n\t}\n", c.camel)
	w.f("\treq := h.rt.commandRequest(r, b, key, fingerprint, requestID, body, id)\n")
	w.f("\tvar persisted %s\n", e.Name)
	w.awaitRevocation()
	w.f("\tresult, err := command.Run(r.Context(), h.rt.Commands, %s, req, decode%sPatch, func(ctx context.Context, patch %sPatchRequest, ch *command.Handle[struct{}]) (idempotency.Response, error) {\n", w.commandVar(types.OpPatch), c.pascal, c.pascal)
	w.ancestors(false, "idempotency.Response{}", false)
	w.lockRow()
	w.f("\t\t%s := stored\n", c.lower)
	if w.checksKept() {
		// A patch keeps each field that a collection write cannot change: a body that names it with another value
		// is refused, not ignored in silence (ST-SUP-RC20-OC46).
		w.f("\t\tif err := %sKept(body, stored, false); err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", c.camel)
	}
	w.before(types.OpPatch, fields)
	for _, f := range fields {
		name := toPascalCase(f.Name)
		if f.Optional {
			w.f("\t\tif patch.%s != nil {\n\t\t\t%s.%s = ptrOf(*patch.%s)\n\t\t}\n", name, c.lower, name, name)
		} else {
			w.f("\t\tif patch.%s != nil {\n\t\t\t%s.%s = *patch.%s\n\t\t}\n", name, c.lower, name, name)
		}
	}
	w.forceScope("stored." + toPascalCase(c.scope))
	for _, f := range e.Fields {
		switch {
		case f.Name == "updated_by":
			w.setField(f, "callerName(r.Context())")
		case f.Name == "updated_time" && f.Type == types.FieldTypeTimestamp:
			w.setField(f, "time.Now().UTC()")
		}
	}
	w.parents("stored")
	w.f("\t\tupdated, err := %s(ctx, from, %s, parents...)\n\t\tif err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n", updateCall(e), w.kept(c.lower, "from"))
	w.f("\t\tpersisted = %s\n", w.fromData("updated.Value()"))
	w.after(types.OpPatch, "persisted", "updated", fields)
	w.respond("http.StatusOK", "persisted")
	w.f("\t})\n")
	w.finish(types.OpPatch, "id")
	w.f("\twriteResult(w, result)\n}\n\n")
}

func (w *runtimeHandlerWriter) remove() {
	c := w.c
	w.f("// Delete runs the generated command delete of %s through Run, and answers 204 with no body (ST-SUP-RC20-OC32).\n", c.decl.Name)
	w.f("func (h *%sHandler) Delete(w http.ResponseWriter, r *http.Request) {\n\tid := canonicalID(r.PathValue(\"id\"))\n", c.pascal)
	w.start(false)
	w.f("\trequestID := command.NewID()\n\tfingerprint, err := idempotency.Fingerprint(r.Method, r.URL.Path, nil)\n\tif err != nil {\n\t\thandleError(w, r, InternalError(\"internal error\"))\n\t\treturn\n\t}\n")
	w.f("\treq := h.rt.commandRequest(r, b, requestKey(requestID), fingerprint, requestID, nil, id)\n")
	w.awaitRevocation()
	w.f("\tresult, err := command.Run(r.Context(), h.rt.Commands, %s, req, func([]byte) (struct{}, error) { return struct{}{}, nil }, func(ctx context.Context, _ struct{}, ch *command.Handle[struct{}]) (idempotency.Response, error) {\n", w.commandVar(types.OpDelete))
	w.ancestors(false, "idempotency.Response{}", false)
	w.lockRow()
	w.before(types.OpDelete, nil)
	w.f("\t\tif err := repo.Delete(ctx, from); err != nil {\n\t\t\treturn idempotency.Response{}, err\n\t\t}\n")
	w.after(types.OpDelete, "", "from", nil)
	w.f("\t\treturn idempotency.Response{Status: http.StatusNoContent}, nil\n\t})\n")
	w.finish(types.OpDelete, "id")
	w.f("\twriteResult(w, result)\n}\n\n")
}

func (w *runtimeHandlerWriter) read() {
	c, e := w.c, w.c.entity
	w.f("// Read runs the query read of %s through query.Run: a row that is not there and a deny are both 404\n// (ST-SUP-RP11-LC06), and a read of a pii field has its audit row (ST-SUP-RC28-OC04).\n", c.decl.Name)
	w.f("func (h *%sHandler) Read(w http.ResponseWriter, r *http.Request) {\n\tid := canonicalID(r.PathValue(\"id\"))\n", c.pascal)
	w.start(false)
	w.f("\tif !command.ValidUUID(id) {\n\t\thandleError(w, r, NotFound(%q, id))\n\t\treturn\n\t}\n", e.Name)
	q := w.commandVar(types.OpRead)
	// A read that names the caller's own write in Authz-Revocation-After waits for that write's revocation before it
	// opens its transaction, as a list does, so a read right after a share sees it (ST-SUP-RC20-OC42, ST-SUP-Q119).
	w.awaitWrite()
	w.f("\tvalue, err := query.Run(r.Context(), h.rt.Queries, %s, h.rt.boundQuery(r.Context(), b, %s), id, func(ctx context.Context, id string, qh *query.Handle[struct{}]) (%s, error) {\n", q, q, e.Name)
	w.f("\t\trepo, err := query.Repo(qh, data.Entity%sView())\n\t\tif err != nil {\n\t\t\treturn %s{}, err\n\t\t}\n", e.Name, e.Name)
	w.f("\t\tloaded, err := repo.Get(ctx, id)\n\t\tif err != nil {\n\t\t\treturn %s{}, err\n\t\t}\n", e.Name)
	// A row of another parent, or with other implicit values, is not found in this collection (ST-SUP-RC20-OC23,
	// ST-SUP-RC20-OC24).
	var checks []string
	if c.scope != "" {
		checks = append(checks, fmt.Sprintf("view.%s != scope", toPascalCase(c.scope)))
	}
	for _, kv := range c.implicit {
		checks = append(checks, w.viewDiffers(kv[0], kv[1]))
	}
	if len(checks) > 0 {
		w.f("\t\tview := loaded.Value()\n\t\tif %s {\n\t\t\treturn %s{}, query.ErrNotFound\n\t\t}\n", strings.Join(checks, " || "), e.Name)
	}
	// The row is the deepest resource that the path names: it is decided first, then the chain (ST-SUP-RC20-OC24).
	w.ancestors(true, e.Name+"{}", false)
	w.f("\t\treturn %sFromView(loaded.Value()), nil\n\t})\n", c.camel)
	w.f("\tif err != nil {\n\t\tanswerRead(w, r, %q, id, err)\n\t\treturn\n\t}\n", e.Name)
	w.f("\tw.Header().Set(\"Content-Type\", \"application/json\")\n\t_ = json.NewEncoder(w).Encode(%sPresented(%s))\n}\n\n", c.camel, w.presentedArgs("value"))
}

// awaitRevocation emits the call of AwaitRevocation of a write, after the bind and before Run decides the write
// (ST-SUP-RC20-OC49, ST-SUP-RP10-LC47): a value that is not the identity of a revocation is 400, a client that went away
// during the wait gets no answer and no internal error code, a database that cannot serve the wait now 503, and any
// other failure 500. The wait sets no
// Authz-Revocation header: that header of a write names the revocation of the write itself. A wait that Prefer: wait=N
// asked for sets Preference-Applied (AwaitRevocationOn; walkthrough 23, small items).
func (w *runtimeHandlerWriter) awaitRevocation() {
	w.f("\tif err := h.rt.Commands.AwaitRevocationOn(w, r, b.Tenant, b.Principal); err != nil {\n\t\tif errors.Is(err, command.ErrBadRequest) {\n\t\t\thandleError(w, r, BadRequest(\"the Authz-Revocation-After header is not the identity of a revocation\"))\n\t\t\treturn\n\t\t}\n\t\tif r.Context().Err() != nil {\n\t\t\treturn\n\t\t}\n\t\tanswerWaitFailure(w, r, err)\n\t\treturn\n\t}\n")
}

// awaitWrite emits the call of AwaitWrite of a read or a list (ST-SUP-RP11-LC21): a value that is not the identity
// of a revocation is 400, a database that cannot serve the wait now 503, and any other failure 500.
// A client that went away during the wait gets no answer and no internal error code (review of PR #428, L1).
func (w *runtimeHandlerWriter) awaitWrite() {
	w.f("\tif err := h.rt.Queries.AwaitWrite(w, r, b.Tenant, b.Principal); err != nil {\n\t\tif errors.Is(err, query.ErrBadRequest) {\n\t\t\thandleError(w, r, BadRequest(\"the Authz-Revocation-After header is not the identity of a revocation\"))\n\t\t\treturn\n\t\t}\n\t\tif r.Context().Err() != nil {\n\t\t\treturn\n\t\t}\n\t\tanswerWaitFailure(w, r, err)\n\t\treturn\n\t}\n")
}

func (w *runtimeHandlerWriter) list() {
	c, e := w.c, w.c.entity
	q := w.commandVar(types.OpList)
	w.f("// %sPage is one page of a list of %s.\ntype %sPage struct {\n\titems []%s\n\tnext  query.Cursor\n\tmore  bool\n}\n\n", c.camel, c.decl.Name, c.camel, e.Name)
	w.f("// List runs the query list of %s through query.Run: it pages by the sealed cursor of the query runtime, at\n// most pageSize values a page, and keeps only the rows that the caller may see (ST-SUP-RP11-LC07, ST-SUP-RP11-LC15).\n", c.decl.Name)
	w.f("func (h *%sHandler) List(w http.ResponseWriter, r *http.Request) {\n", c.pascal)
	w.start(false)
	// A parameter that the list does not read is refused, not ignored, before the list reads a row (ST-SUP-RC20-OC51).
	// The query string is parsed once, and each parameter is read from that one parse.
	w.f("\tparameters, detail := listQuery(r)\n\tif detail != \"\" {\n\t\thandleError(w, r, BadRequest(detail))\n\t\treturn\n\t}\n")
	w.f("\tlimit := 100\n\tif text := parameters.Get(\"pageSize\"); text != \"\" {\n\t\tn, err := strconv.Atoi(text)\n\t\tif err != nil || n < 1 {\n\t\t\thandleError(w, r, BadRequest(\"pageSize must be a whole number of at least 1\"))\n\t\t\treturn\n\t\t}\n\t\tlimit = min(n, query.MaxLimit)\n\t}\n")
	var fields []string
	// A pii or secret field orders no list: the order would tell its value (ST-SUP-RP11-LC20). A string field out of
	// the search orders none either, and the 400 names it (ST-SUP-RC20-OC57).
	for _, f := range e.Fields {
		if f.Public() && !f.Unsearched() {
			fields = append(fields, fmt.Sprintf("%q: true", f.Name))
		}
	}
	implicit := map[string]bool{}
	for _, kv := range c.implicit {
		implicit[kv[0]] = true
	}
	unsearched := len(unsearchedFields(e, implicit)) > 0
	named := ""
	if unsearched {
		named = fmt.Sprintf("\t\tif name := unsearchedOrder(parameters.Get(\"orderBy\"), %sUnsearchedFields); name != \"\" {\n\t\t\thandleError(w, r, BadRequest(unsearchedDetail(\"orderBy\", name)))\n\t\t\treturn\n\t\t}\n", c.camel)
	}
	w.f("\torder, err := listOrder(parameters.Get(\"orderBy\"), map[string]bool{%s})\n\tif err != nil {\n%s\t\thandleError(w, r, BadRequest(err.Error()))\n\t\treturn\n\t}\n", strings.Join(fields, ", "), named)
	// A field selection names declared fields; a secret field is cleared in each presented value (ST-SUP-RC20-OC12).
	var declared []string
	for _, f := range e.Fields {
		declared = append(declared, fmt.Sprintf("%q: true", f.Name))
	}
	w.f("\tfields, err := listFields(parameters.Get(\"fields\"), map[string]bool{%s})\n\tif err != nil {\n\t\thandleError(w, r, BadRequest(err.Error()))\n\t\treturn\n\t}\n", strings.Join(declared, ", "))
	// The filter of the list: the conditions of the search, then the scope field and the implicit fields, which
	// the search may not name again (ST-SUP-RP11-LC20, ST-SUP-RC20-OC23, ST-SUP-RC20-OC24).
	own := len(c.implicit)
	if c.scope != "" {
		own++
	}
	named = ""
	if unsearched {
		named = fmt.Sprintf("\t\tif name := unsearchedSearch(parameters.Get(\"search\"), %sSearchFields, %sUnsearchedFields, query.MaxFilter-%d); name != \"\" {\n\t\t\thandleError(w, r, BadRequest(unsearchedDetail(\"search\", name)))\n\t\t\treturn\n\t\t}\n", c.camel, c.camel, own)
	}
	w.f("\twhere, err := query.ParseSearch(parameters.Get(\"search\"), %sSearchFields, query.MaxFilter-%d)\n\tif err != nil {\n%s\t\thandleError(w, r, BadRequest(\"the search is not valid\"))\n\t\treturn\n\t}\n", c.camel, own, named)
	// A renamed field is named in the API by its first name and stored in the column of its declared name
	// (ST-GEN-UC12-R04): the order and the search name it by the API name, and the query runtime reads columns.
	var renamed []string
	for _, f := range e.Fields {
		if f.Column != "" {
			renamed = append(renamed, fmt.Sprintf("%q: %q", f.Name, f.Column))
		}
	}
	if len(renamed) > 0 {
		w.f("\tcolumns := map[string]string{%s}\n\tfor i := range order {\n\t\tif column, ok := columns[order[i].Field]; ok {\n\t\t\torder[i].Field = column\n\t\t}\n\t}\n\tfor i := range where {\n\t\tif column, ok := columns[where[i].Field]; ok {\n\t\t\twhere[i].Field = column\n\t\t}\n\t}\n", strings.Join(renamed, ", "))
	}
	if c.scope != "" {
		w.f("\twhere = append(where, query.Equal{Field: %q, Value: scope})\n", c.scope)
	}
	for _, kv := range c.implicit {
		w.f("\twhere = append(where, query.Equal{Field: %q, Value: %q})\n", kv[0], kv[1])
	}
	w.f("\tvar after query.Cursor\n\tif text := parameters.Get(\"cursor\"); text != \"\" {\n\t\tif after, err = h.rt.Queries.ParseCursor(%s, b.Tenant, b.Principal, text); err != nil {\n\t\t\thandleError(w, r, BadRequest(\"the cursor is not valid\"))\n\t\t\treturn\n\t\t}\n\t}\n", q)
	// A list that names the caller's own write in Authz-Revocation-After waits for that write's revocation before it
	// opens its transaction, so it shows the write (ST-SUP-RC20-OC41, ST-SUP-RP11-LC21, ST-SUP-Q119).
	w.awaitWrite()
	w.f("\tpage, err := query.Run(r.Context(), h.rt.Queries, %s, h.rt.boundQuery(r.Context(), b, %s), query.ListRequest{Limit: limit, After: after, Order: order, Where: where}, func(ctx context.Context, request query.ListRequest, qh *query.Handle[struct{}]) (%sPage, error) {\n", q, q, c.camel)
	w.ancestors(true, c.camel+"Page{}", true)
	w.f("\t\trepo, err := query.Repo(qh, data.Entity%sView())\n\t\tif err != nil {\n\t\t\treturn %sPage{}, err\n\t\t}\n", e.Name, c.camel)
	w.f("\t\tresult, err := repo.List(ctx, request)\n\t\tif err != nil {\n\t\t\treturn %sPage{}, err\n\t\t}\n", c.camel)
	w.f("\t\tpage := %sPage{next: result.Next, more: result.More}\n\t\tfor _, item := range result.Items {\n\t\t\tpage.items = append(page.items, %sFromView(item.Value()))\n\t\t}\n\t\treturn page, nil\n\t})\n", c.camel, c.camel)
	w.f("\tif isBadCursor(err) {\n\t\thandleError(w, r, BadRequest(\"the cursor is not valid\"))\n\t\treturn\n\t}\n")
	if len(c.chain) > 0 {
		// A parent chain that the check refuses is 404 with the parent of the path, the same for an ancestor that is
		// not there, that the caller may not read and that is of another ancestor (ST-SUP-RC20-OC24).
		w.f("\tif errors.Is(err, query.ErrNotFound) {\n\t\thandleError(w, r, NotFound(%q, scope))\n\t\treturn\n\t}\n", c.decl.ParentEntity())
	}
	w.f("\tif errors.Is(err, query.ErrBadRequest) {\n\t\thandleError(w, r, BadRequest(\"the list request is not valid\"))\n\t\treturn\n\t}\n")
	w.f("\tif query.Transient(err) {\n\t\tanswerUndecided(w, r, unavailableDatabase(err))\n\t\treturn\n\t}\n")
	w.f("\tif err != nil {\n\t\thandleError(w, r, InternalError(\"internal error\"))\n\t\treturn\n\t}\n")
	w.f("\titems := make([]any, 0, len(page.items))\n\tfor _, item := range page.items {\n\t\titems = append(items, selectFields(%sPresented(%s), fields))\n\t}\n", c.camel, w.presentedArgs("item"))
	w.f("\tw.Header().Set(\"Content-Type\", \"application/json\")\n")
	if w.envelope {
		w.f("\tresult := map[string]any{\"kind\": %q, \"size\": len(items), \"items\": items}\n", e.Name+"List")
		w.f("\tif page.more {\n\t\tnext, err := h.rt.Queries.CursorText(%s, b.Tenant, b.Principal, page.next)\n\t\tif err != nil {\n\t\t\thandleError(w, r, InternalError(\"internal error\"))\n\t\t\treturn\n\t\t}\n\t\tresult[\"next\"] = next\n\t}\n", q)
		w.f("\t_ = json.NewEncoder(w).Encode(result)\n}\n\n")
	} else {
		w.f("\t_ = json.NewEncoder(w).Encode(items)\n}\n\n")
	}
}

// renderChainLinks writes links.go of the data package: for each entity of a parent chain, its link, which holds the
// key of a row and the field that names the ancestor above it in the path, and nothing else. The parent chain check
// reads an ancestor above the deepest resource of a request through its link, with no permission decision, so it
// checks only that the row exists in the tenant and is of the ancestor above it (ST-SUP-RC20-OC24, ST-SUP-Q156). The
// link has no resource type: the query runtime and the command runtime decide nothing on it. Its declaration for the
// command runtime has write statements that match no row and write nothing, so no write goes through a link. It gives
// nil when no collection has a parent chain.
func renderChainLinks(ctx gen.Context, collections []*runtimeCollection) ([]byte, error) {
	entities := map[string]types.Entity{}
	for _, e := range ctx.Entities {
		entities[e.Name] = e
	}
	// links gives the entity and the field of each link by its Go name: the link of each ancestor of a parent chain,
	// and the link of the entity of each scoped collection with an update, a patch or a delete by its scope field.
	links := map[string][2]string{}
	for _, c := range collections {
		var pairs [][3]string
		for _, level := range c.chain {
			pairs = append(pairs, [3]string{level.linkType, level.entity, level.link})
		}
		if len(c.chain) > 0 && (c.ops[types.OpUpdate] || c.ops[types.OpPatch] || c.ops[types.OpDelete]) {
			pairs = append(pairs, [3]string{c.rowLink, c.entity.Name, c.scope})
		}
		// Two links of one Go name, such as the link CardRackLink of the entity CardRack and the link of Card by its
		// field rack, would make one declaration hide the other: generation refuses them.
		for _, p := range pairs {
			if have, ok := links[p[0]]; ok && have != [2]string{p[1], p[2]} {
				return nil, fmt.Errorf("rest-api: the link %s of the entity %s by %q has the name of the link of the entity %s by %q; rename one of the entities or fields", p[0], p[1], p[2], have[0], have[1])
			}
			links[p[0]] = [2]string{p[1], p[2]}
		}
	}
	if len(links) == 0 {
		return nil, nil
	}
	names := slices.Sorted(maps.Keys(links))
	var b bytes.Buffer
	b.WriteString("package data\n\nimport (\n\t\"github.com/910-software-solutions/stego/registry/runtime/command\"\n\t\"github.com/910-software-solutions/stego/registry/runtime/query\"\n)\n\n")
	tenant := strconv.Quote(ctx.Tenancy.Key)
	for _, typeName := range names {
		entity, link := links[typeName][0], links[typeName][1]
		if _, ok := entities[typeName]; ok {
			return nil, fmt.Errorf("rest-api: the entity %s has the name of the link of the entity %s in a parent chain; rename it", typeName, entity)
		}
		column := ""
		for _, f := range entities[entity].Fields {
			if f.Name == link {
				column = f.ColumnName()
			}
		}
		if link != "" && column == "" {
			return nil, fmt.Errorf("rest-api: the entity %s has no field %s, which names its parent in the path", entity, link)
		}
		table := commandTableName(entity)
		name := strings.TrimSuffix(typeName, "Link")
		camel := strings.ToLower(name[:1]) + name[1:]
		columns, viewColumns, scan := `"id"::text`, `"id"`, "&v.ID"
		load := "\tif v.ID, err = r.Text(\"id\"); err != nil {\n\t\treturn " + name + "Link{}, err\n\t}\n"
		if column != "" {
			columns += ", " + strconv.Quote(column) + "::text"
			viewColumns += ", " + strconv.Quote(column)
			scan += ", &v.Link"
			load += "\tif v.Link, err = r.Text(" + strconv.Quote(column) + "); err != nil {\n\t\treturn " + name + "Link{}, err\n\t}\n"
		}
		from := fmt.Sprintf(`%s FROM public.%s WHERE %s = $1`, columns, strconv.Quote(table), tenant)
		row := from + ` AND "id" = $2::uuid`
		fmt.Fprintf(&b, "// %sLink is the key of one stored %s", name, entity)
		if column != "" {
			fmt.Fprintf(&b, " and its field %s, which names its parent in the path", link)
		}
		fmt.Fprintf(&b, ".\n// The parent chain check reads it with no permission decision, and reads no other field of the row\n// (ST-SUP-RC20-OC24).\ntype %sLink struct {\n\tID   string\n\tLink string\n}\n\n", name)
		fmt.Fprintf(&b, "// %sLinkView is the declaration of the link of %s for the query runtime. It has no resource type, so a read\n// through it decides no permission.\n", camel, entity)
		fmt.Fprintf(&b, "var %sLinkView = must(query.NewEntity(query.Spec{Name: %q, Schema: \"public\", Table: %q, Key: \"id\",\n\tColumns: []string{%s},\n}, func(r query.Row) (%sLink, error) {\n\tvar v %sLink\n\tvar err error\n%s\treturn v, nil\n}))\n\n",
			camel, name+"Link", table, viewColumns, name, name, load)
		fmt.Fprintf(&b, "// Entity%sLinkView is the declaration of the link of %s that the parent chain check of a query reads through.\nfunc Entity%sLinkView() *query.Entity[%sLink] { return %sLinkView }\n\n", name, entity, name, name, camel)
		fmt.Fprintf(&b, "// %sLinkEntity is the declaration of the link of %s for the command runtime. It has no resource type, so a read\n// through it decides no permission. Its write statements match no row, so no write goes through it.\n", name, entity)
		fmt.Fprintf(&b, "var %sLinkEntity = command.Must(command.NewEntity(command.EntitySpec[%sLink]{\n\tName: %q,\n", name, name, name+"Link")
		fmt.Fprintf(&b, "\tSelect: %q,\n\tSelectForUpdate: %q,\n\tSelectForDelete: %q,\n", "SELECT "+row, "SELECT "+row+" FOR NO KEY UPDATE", "SELECT "+row+" FOR UPDATE")
		fmt.Fprintf(&b, "\tInsert: %q,\n\tUpdate: %q,\n\tDelete: %q,\n", "SELECT "+from+" AND false", "SELECT "+row+" AND $3::bigint IS NULL AND false",
			fmt.Sprintf(`DELETE FROM public.%s WHERE %s = $1 AND "id" = $2::uuid AND "version" = $3 AND false`, strconv.Quote(table), tenant))
		fmt.Fprintf(&b, "\tScan: func(row command.Scanner) (%sLink, command.Meta, error) {\n\t\tvar v %sLink\n\t\terr := row.Scan(%s)\n\t\treturn v, command.Meta{ID: v.ID}, err\n\t},\n", name, name, scan)
		fmt.Fprintf(&b, "\tValues:     func(%sLink) []any { return nil },\n\tState:      func(v %sLink) map[string]any { return map[string]any{\"id\": v.ID} },\n\tKeyChanged: func(before, after %sLink) bool { return false },\n\tClone:      func(v %sLink) %sLink { return v },\n\tValidID:    command.ValidUUID,\n}))\n\n", name, name, name, name, name)
		fmt.Fprintf(&b, "// %sLinkRepository gives the repository of the link of %s of a command handle.\nfunc %sLinkRepository[P any](h *command.Handle[P]) (*command.Repository[%sLink], error) {\n\treturn command.Repo(h, %sLinkEntity)\n}\n\n", name, entity, name, name, name)
	}
	out, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("rest-api: format links.go: %w", err)
	}
	return out, nil
}

// updateCall gives the call of the update of a generated write of e: the repository of an entity with a resource type
// of its own takes no parent on Update, so the write calls the update of the runtime that it embeds, which takes the
// parents of a changed reference (ST-SUP-RC32-OC26).
func updateCall(e types.Entity) string {
	if e.ResourceType != "" {
		return "repo.Repository.Update"
	}
	return "repo.Update"
}
