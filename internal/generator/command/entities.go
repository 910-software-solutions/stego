package command

import (
	"fmt"
	"go/token"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	authzschema "github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/types"
)

// ContractRuntimeImports gives the packages of the runtime library that the command slot contract out/commands
// imports for the declared entities and commands: the audit package when an entity that a command writes has a pii
// or secret field (model.usesAudit), the authz package when a command writes relations (model.hasRelations), and the
// command runtime. A fill of the command slot may import them (ST-EXT-INV05, ST-EXT-UC01-R03), so the fill check
// admits them also before out/commands is generated, and admits no package that the contract does not import.
func ContractRuntimeImports(entities []types.Entity, commands []types.Command) []string {
	var writes []string
	relations := false
	for _, c := range commands {
		writes = append(writes, c.Writes...)
		relations = relations || len(c.RelationshipsWritten) > 0
	}
	var out []string
	if WritesSensitive(entities, writes) {
		out = append(out, "github.com/jsell-rh/stego/registry/runtime/audit")
	}
	if relations {
		out = append(out, "github.com/jsell-rh/stego/registry/runtime/authz")
	}
	return append(out, "github.com/jsell-rh/stego/registry/runtime/command")
}

// WritesSensitive tells whether one of the entities named in names has a pii or secret field, so that its generated
// repository declaration imports the audit package (model.usesAudit).
func WritesSensitive(entities []types.Entity, names []string) bool {
	for _, e := range entities {
		if !slices.Contains(names, e.Name) {
			continue
		}
		for _, f := range e.Fields {
			if f.Sensitivity == "pii" || f.Sensitivity == "secret" {
				return true
			}
		}
	}
	return false
}

// entity is one entity that a command writes, with its repository declaration (ST-SUP-RP10-LC02).
type entity struct {
	decl         types.Entity
	goName       string
	table        string
	fields       []*entityField
	resourceType string
	read, list   string
	pii          bool
	deep         bool // a field is a pointer or a slice, so Clone copies it
	// references are the references of the entity to entities with a resource type, in field order, which the
	// generated Parent list declares (ST-SUP-RP10-LC05, ST-DECL-EN02).
	references []reference
	keys       []*entityField // the fields of a unique key besides the ID (ST-SUP-RP10-LC24)
	// within are the fields that a reference of this entity, or of another entity to this one, is held within, which
	// the generated Within map reads (ST-SUP-RP10-LC37, ST-DECL-Q42).
	within []*entityField
	// referrers are the composite keys of the references of other entities to this one that are held within a field
	// (ST-SUP-RP10-LC37): a write of this entity that a row of theirs refuses is invalid, never internal.
	referrers []referrer
	// relationships are the declared relationships of the entity, which the generated Relationships list declares
	// and the runtime writes (ST-DECL-UC06-R04, ST-SUP-Q118).
	relationships []relationship
	// schemaRelations are the relations that the schema declares on the entity's resource type, which the generated
	// Relations list holds; a delete of a row clears each of them (ST-SUP-Q118).
	schemaRelations []string
	// tenant is true when the schema gives the entity's resource type the relation tenant of the subject type
	// tenant, which the runtime writes on each create (ST-SUP-RP10-LC05, ST-SUP-Q132).
	tenant bool
}

// relationship is one declared relationship: the relation, the subject type, the field that holds the subject or
// nil for the caller, and whether the subject type is a resource type of this service.
type relationship struct {
	relation, subjectType string
	field                 *entityField
	resource              bool
}

// referrer is the composite key of a reference of another entity to this one: the key name, the reference as
// Entity.field, and the within field.
type referrer struct{ key, field, within string }

// reference is one reference of an entity to an entity with a resource type: the field, the referenced entity and
// its resource type, and the relation and the permission that the declaration names for it.
type reference struct {
	field                *entityField
	entity, resourceType string
	relation, permission string
	// within is the field that the row and the referenced row must have equal, or empty (ST-DECL-INV93).
	within string
}

// entityField is one declared field of an entity.
type entityField struct {
	decl        types.Field
	goName      string
	goType      string
	pointer     bool
	slice       bool
	sensitivity string
}

// goTypes are the Go types of the declared field types, as the PostgreSQL columns of ST-SUP-RC01-OC41 scan.
var goTypes = map[types.FieldType]string{
	types.FieldTypeString: "string", types.FieldTypeEnum: "string", types.FieldTypeRef: "string",
	types.FieldTypeInt32: "int32", types.FieldTypeInt64: "int64", types.FieldTypeFloat: "float32",
	types.FieldTypeDouble: "float64", types.FieldTypeBool: "bool", types.FieldTypeTimestamp: "time.Time",
	types.FieldTypeBytes: "[]byte", types.FieldTypeJsonb: "[]byte",
}

// Relations are what the service's schema declares on the resource type of each entity: the names of its relations
// (ST-SUP-Q118), and whether it has the relation tenant of the subject type tenant (ST-SUP-Q132). The zero value is
// a service with no authorization.
type Relations struct {
	names  map[string][]string
	tenant map[string]bool
}

// Of gives the names of the relations that the schema declares on the resource type.
func (r Relations) Of(resourceType string) []string { return r.names[resourceType] }

// Tenant tells whether the schema gives the resource type the relation tenant of the subject type tenant.
func (r Relations) Tenant(resourceType string) bool { return r.tenant[resourceType] }

// tenantRelation is the relation of a resource type to the tenant of its rows, and tenantSubject its subject type
// (ST-SUP-Q132).
const (
	tenantRelation = "tenant"
	tenantSubject  = "tenant"
)

// SchemaRelations gives, for each resource type of an entity of the service, the relations that the schema declares
// on it (ST-SUP-Q118): the runtime clears each of them on the resource of a deleted row, and it writes the relation
// tenant on each create when the schema gives it the subject type tenant (ST-SUP-Q132). It reads the schema that the
// authorization setting names among the generator's inputs, and gives none for a service with no authorization.
func SchemaRelations(ctx gen.Context) (Relations, error) {
	setting := ctx.Authorization
	if setting == nil || setting.SchemaPath == "" {
		return Relations{}, nil
	}
	source, ok := ctx.Inputs[setting.SchemaPath]
	if !ok {
		return Relations{}, fmt.Errorf("command: the schema %q that the authorization setting names is not among the inputs of the generator", setting.SchemaPath)
	}
	schema, err := authzschema.CompileAuthzSchema(setting.SchemaPath, source)
	if err != nil {
		return Relations{}, fmt.Errorf("command: the schema %q does not compile: %w", setting.SchemaPath, err)
	}
	relations := Relations{names: map[string][]string{}, tenant: map[string]bool{}}
	for _, e := range ctx.Entities {
		if e.ResourceType != "" {
			relations.names[e.ResourceType] = schema.Relations(e.ResourceType)
			relations.tenant[e.ResourceType] = schema.RelationAdmits(e.ResourceType, tenantRelation, tenantSubject)
		}
	}
	return relations, nil
}

// withinHeld tells whether the field name of the entity e is the within of a reference: held when a reference of e
// or of another entity to e is held within it, and referenced when a reference of another entity to e is, so that
// the field is in the unique key that the composite key of that reference refers to (ST-SUP-RC01-OC46).
func withinHeld(e types.Entity, byName map[string]types.Entity, name string) (held, referenced bool) {
	for _, ref := range e.References {
		if ref.WithinField() == name {
			held = true
		}
	}
	for _, other := range byName {
		for _, f := range other.Fields {
			if f.Type == types.FieldTypeRef && f.To == e.Name && other.References[f.Name].WithinField() == name {
				held, referenced = true, true
			}
		}
	}
	return held, referenced
}

// planEntity checks one entity that a command writes and builds its repository declaration; relations are the
// schema relations of each resource type (SchemaRelations).
func planEntity(e types.Entity, byName map[string]types.Entity, tenantKey string, relations Relations) (*entity, error) {
	if e.Tenant != "" && e.Tenant != "owned" {
		return nil, fmt.Errorf("the entity is %s; a command writes only entities that a tenant owns", e.Tenant)
	}
	if e.Kind != "" && e.Kind != "data" {
		return nil, fmt.Errorf("the entity is of kind %s; a command writes only data entities", e.Kind)
	}
	p := &entity{decl: e, goName: e.Name, table: tableName(e.Name), resourceType: e.ResourceType,
		schemaRelations: relations.Of(e.ResourceType), tenant: e.ResourceType != "" && relations.Tenant(e.ResourceType)}
	if !sqlName.MatchString(p.table) || !token.IsIdentifier(e.Name) || !token.IsExported(e.Name) {
		return nil, fmt.Errorf("the entity name %s does not give a plain table name and an exported Go name", e.Name)
	}
	if e.ResourceType != "" {
		p.read, p.list = e.Permissions[types.OpRead], e.Permissions[types.OpList]
		if p.read == "" || p.read == types.PermissionPublic {
			// The message names the declared read exactly: none, or public (security review of PR #383, L4).
			read := "no read permission"
			if p.read == types.PermissionPublic {
				read = "the read permission public"
			}
			return nil, fmt.Errorf("the entity has the resource type %s and %s; the command runtime decides the read permission of each row that it loads (ST-SUP-RP10-LC13), so declare permissions.read as a permission of %s", e.ResourceType, read, e.ResourceType)
		}
		if p.list == "public" {
			p.list = ""
		}
	}
	var refs []*entityField
	for _, f := range e.Fields {
		if c := f.ColumnName(); !sqlName.MatchString(c) || !sqlName.MatchString(f.Name) || c == "id" || c == tenantKey || c == "version" || c == "authz_token" || c == "created_time" || c == "updated_time" {
			return nil, fmt.Errorf("the field name %s is not a plain column name of its own", f.Name)
		}
		goType, ok := goTypes[f.Type]
		if !ok {
			return nil, fmt.Errorf("the field %s has the type %s, which a command cannot write", f.Name, f.Type)
		}
		if f.Computed {
			return nil, fmt.Errorf("the field %s is computed, which a command cannot write", f.Name)
		}
		// The generated Check compiles the declared pattern once, with the RE2 syntax of Go (walkthrough 10, B4).
		if _, err := regexp.Compile(f.Pattern); f.Pattern != "" && err != nil {
			return nil, fmt.Errorf("the field %s has a pattern that is not an RE2 expression", f.Name)
		}
		ef := &entityField{decl: f, goName: mixedCase(f.Name), goType: goType, sensitivity: f.Sensitivity}
		ef.slice = strings.HasPrefix(goType, "[]")
		if f.Optional && !ef.slice {
			ef.pointer = true
			ef.goType = "*" + goType
		}
		if ef.pointer || ef.slice {
			p.deep = true
		}
		switch f.Sensitivity {
		case "", "none":
		case "pii":
			p.pii = true
		case "secret":
		default:
			return nil, fmt.Errorf("the field %s has the sensitivity %s, which is not pii or secret", f.Name, f.Sensitivity)
		}
		// A within field of another entity's reference to this one is in the unique key that the composite key of
		// that reference refers to (ST-SUP-RC01-OC46), and a within field of this entity's own reference is read too.
		held, referenced := withinHeld(e, byName, f.Name)
		if f.Unique || len(f.UniqueComposite) > 0 || referenced {
			p.keys = append(p.keys, ef)
		}
		if held {
			p.within = append(p.within, ef)
		}
		if f.Type == types.FieldTypeRef && byName[f.To].ResourceType != "" {
			refs = append(refs, ef)
		}
		p.fields = append(p.fields, ef)
	}
	// Each reference to an entity with a resource type is a reference of the generated Parent list: a create and a
	// move decide the permission that the declaration names for it on the referenced row, and the runtime writes its
	// relation (ST-SUP-RP10-LC05, ST-DECL-EN02). The check of ST-DECL-INV85 holds every one to its References; a
	// reference with none decides the command's own permission and writes no relation.
	for _, f := range refs {
		to := byName[f.decl.To]
		declared := e.References[f.decl.Name]
		relation := declared.Relation
		if p.resourceType == "" {
			relation = ""
		}
		p.references = append(p.references, reference{field: f, entity: to.Name, resourceType: to.ResourceType,
			relation: relation, permission: declared.Permission, within: declared.WithinField()})
	}
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		other := byName[name]
		for _, f := range other.Fields {
			if within := other.References[f.Name].WithinField(); f.Type == types.FieldTypeRef && f.To == e.Name && within != "" {
				p.referrers = append(p.referrers, referrer{key: postgresadapter.WithinKeyName(other.Name, f.ColumnName()), field: other.Name + "." + f.Name, within: within})
			}
		}
	}
	resourceTypes := map[string]bool{}
	for _, other := range byName {
		if other.ResourceType != "" {
			resourceTypes[other.ResourceType] = true
		}
	}
	for _, r := range e.Relationships {
		if p.resourceType == "" {
			return nil, fmt.Errorf("the relationship %s needs a resource type on the entity", r.Relation)
		}
		rel := relationship{relation: r.Relation, subjectType: r.SubjectType, resource: resourceTypes[r.SubjectType]}
		if r.SubjectSource == types.SubjectSourceField {
			for _, f := range p.fields {
				if f.decl.Name == r.SubjectField {
					rel.field = f
				}
			}
			if rel.field == nil || (rel.field.decl.Type != types.FieldTypeString && rel.field.decl.Type != types.FieldTypeRef) {
				return nil, fmt.Errorf("the relationship %s takes its subject from the field %q, which is not a string or ref field of the entity", r.Relation, r.SubjectField)
			}
		} else {
			rel.resource = false
		}
		p.relationships = append(p.relationships, rel)
	}
	return p, nil
}

// columns is the column list that each statement of the entity returns, in the order that Scan reads: the ID as
// text, the fields, the resource version and the authorization token, which an entity with no resource type does
// not have.
func (e *entity) columns() string {
	cols := []string{`"id"::text`}
	for _, f := range e.fields {
		cols = append(cols, quote(f.decl.ColumnName()))
	}
	cols = append(cols, `"version"`)
	if e.resourceType != "" {
		cols = append(cols, `"authz_token"`)
	} else {
		cols = append(cols, `''::text`)
	}
	return strings.Join(cols, ", ")
}

// statements gives the statements of the entity's repository (ST-SUP-RP10-LC23, ST-SUP-RP10-LC24): each takes the
// tenant as $1.
func (e *entity) statements(tenantKey string) (selectRow, forUpdate, forDelete, insert, update, remove string) {
	table := "public." + quote(e.table)
	where := fmt.Sprintf("%s = $1 AND \"id\" = $2::uuid", quote(tenantKey))
	selectRow = fmt.Sprintf("SELECT %s FROM %s WHERE %s", e.columns(), table, where)
	forUpdate = selectRow + " FOR NO KEY UPDATE"
	forDelete = selectRow + " FOR UPDATE"
	names := []string{quote(tenantKey), `"id"`}
	params := []string{"$1", "$2::uuid"}
	sets := make([]string, 0, len(e.fields)+2)
	for i, f := range e.fields {
		names = append(names, quote(f.decl.ColumnName()))
		params = append(params, fmt.Sprintf("$%d", i+3))
		sets = append(sets, fmt.Sprintf("%s = $%d", quote(f.decl.ColumnName()), i+5))
	}
	sets = append(sets, `"version" = "version" + 1`, `"updated_time" = now()`)
	insert = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) RETURNING %s", table, strings.Join(names, ", "), strings.Join(params, ", "), e.columns())
	// $4 is the ID of the values, which an update does not change.
	update = fmt.Sprintf("UPDATE %s SET %s WHERE %s AND \"version\" = $3 AND \"id\" = $4::uuid RETURNING %s", table, strings.Join(sets, ", "), where, e.columns())
	remove = fmt.Sprintf("DELETE FROM %s WHERE %s AND \"version\" = $3", table, where)
	return
}

func quote(name string) string { return `"` + name + `"` }

// patternVar is the name of the package-level variable of the compiled pattern of field f of entity e.
func patternVar(e *entity, f *entityField) string {
	return "pattern" + e.goName + f.goName
}

// renderCheck writes the Check of an entity, the declared constraints of its fields in field order: min_length,
// max_length and pattern of a string, min and max of a number and the values of an enum, each of an optional field
// only when it is set; and Constraints, the declared field of each CHECK and foreign key of the table by name and of
// each column, so a value that the table refuses names its field (ST-SUP-RP10-LC02; walkthrough 10, B4). The reason
// names the constraint and its declared bound, never the value.
func renderCheck(w *writer, e *entity) {
	type rule struct{ test, reason string }
	var fields []string
	rules := map[string][]rule{}
	for _, f := range e.fields {
		d := f.decl
		value := "v." + f.goName
		if f.pointer {
			value = "*v." + f.goName
		}
		base := strings.TrimPrefix(f.goType, "*")
		var rs []rule
		if base == "string" {
			if d.MinLength != nil {
				rs = append(rs, rule{fmt.Sprintf("!command.MinLength(%s, %d)", value, *d.MinLength), fmt.Sprintf("shorter than min_length %d", *d.MinLength)})
			}
			if d.MaxLength != nil {
				rs = append(rs, rule{fmt.Sprintf("!command.MaxLength(%s, %d)", value, *d.MaxLength), fmt.Sprintf("longer than max_length %d", *d.MaxLength)})
			}
			if d.Pattern != "" {
				rs = append(rs, rule{fmt.Sprintf("!%s.MatchString(%s)", patternVar(e, f), value), "does not match the declared pattern"})
			}
			if d.Type == types.FieldTypeEnum && len(d.Values) > 0 {
				quoted := make([]string, len(d.Values))
				for i, v := range d.Values {
					quoted[i] = fmt.Sprintf("%q", v)
				}
				rs = append(rs, rule{fmt.Sprintf("!command.OneOf(%s, %s)", value, strings.Join(quoted, ", ")), "is not one of the declared values"})
			}
		}
		switch base {
		case "int32", "int64", "float32", "float64":
			if (base == "float32" || base == "float64") && (d.Min != nil || d.Max != nil) {
				// A bounded float is finite, as the table CHECK holds (security review of PR #434, I3).
				rs = append(rs, rule{fmt.Sprintf("!command.Finite(float64(%s))", value), "is not a finite number"})
			}
			if d.Min != nil {
				rs = append(rs, rule{fmt.Sprintf("float64(%s) < %s", value, strconv.FormatFloat(*d.Min, 'g', -1, 64)), "less than min " + strconv.FormatFloat(*d.Min, 'g', -1, 64)})
			}
			if d.Max != nil {
				rs = append(rs, rule{fmt.Sprintf("float64(%s) > %s", value, strconv.FormatFloat(*d.Max, 'g', -1, 64)), "more than max " + strconv.FormatFloat(*d.Max, 'g', -1, 64)})
			}
		}
		if len(rs) > 0 {
			fields = append(fields, f.goName)
			rules[f.goName] = rs
		}
	}
	if len(fields) > 0 {
		w.f("\tCheck: func(v %s) (string, string, bool) {\n", e.goName)
		for _, f := range e.fields {
			rs := rules[f.goName]
			if len(rs) == 0 {
				continue
			}
			indent := "\t\t"
			if f.pointer {
				w.f("\t\tif v.%s != nil {\n", f.goName)
				indent = "\t\t\t"
			}
			for _, r := range rs {
				w.f("%sif %s {\n%s\treturn %q, %q, false\n%s}\n", indent, r.test, indent, f.decl.Name, r.reason, indent)
			}
			if f.pointer {
				w.f("\t\t}\n")
			}
		}
		w.f("\t\treturn \"\", \"\", true\n\t},\n")
	}
	w.f("\tConstraints: map[string]string{\n")
	seen := map[string]bool{}
	put := func(name, field string) {
		if !seen[name] {
			seen[name] = true
			w.f("\t\t%q: %q,\n", name, field)
		}
	}
	for _, f := range e.fields {
		column := f.decl.ColumnName()
		if len(rules[f.goName]) > 0 {
			put(postgresadapter.CheckName(e.decl.Name, column), f.decl.Name)
		}
		if f.decl.Type == types.FieldTypeRef && f.decl.To != "" {
			put(postgresadapter.ForeignKeyName(e.decl.Name, column), f.decl.Name)
		}
	}
	for _, f := range e.fields {
		put(f.decl.ColumnName(), f.decl.Name)
	}
	w.f("\t},\n")
	renderUnique(w, e)
}

// renderUnique writes Unique of the entity declaration: the declared fields of the key of each unique index of its
// table, by the name that the postgres adapter gives the index, so a unique violation names its key and never a value
// (ST-SUP-RC32-OC22, ST-SUP-RP10-LC48). An index whose columns are not all declared fields is not given.
func renderUnique(w *writer, e *entity) {
	fields := map[string]string{}
	for _, f := range e.fields {
		fields[f.decl.ColumnName()] = f.decl.Name
	}
	indexes := postgresadapter.UniqueIndexes(e.decl)
	var lines []string
	for _, name := range slices.Sorted(maps.Keys(indexes)) {
		var key []string
		for _, column := range indexes[name] {
			if fields[column] == "" {
				key = nil
				break
			}
			key = append(key, fields[column])
		}
		if len(key) > 0 {
			lines = append(lines, fmt.Sprintf("\t\t%q: %q,\n", name, strings.Join(key, ", ")))
		}
	}
	if len(lines) == 0 {
		return
	}
	w.f("\tUnique: map[string]string{\n%s\t},\n", strings.Join(lines, ""))
}

// renderEntity writes the Go type, the repository declaration and the repository accessor of one entity.
func renderEntity(w *writer, e *entity, tenantKey string) {
	w.f("// %s is a row of the entity %s, as its command handle reads and writes it.\ntype %s struct {\n\tID string\n", e.goName, e.decl.Name, e.goName)
	for _, f := range e.fields {
		w.f("\t%s %s\n", f.goName, f.goType)
	}
	w.f("}\n\n")
	w.f("// Loaded%s is a loaded value of %s: only a read or a write through a command handle makes one.\ntype Loaded%s = command.Loaded[%s]\n\n", e.goName, e.decl.Name, e.goName, e.goName)
	selectRow, forUpdate, forDelete, insert, update, remove := e.statements(tenantKey)
	for _, f := range e.fields {
		if f.decl.Pattern != "" && f.goType != "" && strings.TrimPrefix(f.goType, "*") == "string" {
			w.f("// %s is the declared pattern of %s.%s.\nvar %s = command.MustPattern(%q)\n\n", patternVar(e, f), e.decl.Name, f.decl.Name, patternVar(e, f), f.decl.Pattern)
		}
	}
	w.f("// %sEntity is the repository declaration of %s (ST-SUP-RP10-LC02).\n", e.goName, e.decl.Name)
	w.f("var %sEntity = command.Must(command.NewEntity(command.EntitySpec[%s]{\n", e.goName, e.goName)
	w.f("\tName: %q,\n", e.decl.Name)
	if e.resourceType != "" {
		w.f("\tResourceType: %q,\n\tReadPermission: %q,\n", e.resourceType, e.read)
		if e.list != "" {
			w.f("\tListPermission: %q,\n", e.list)
		}
	}
	var sensitive []string
	for _, f := range e.fields {
		switch f.sensitivity {
		case "pii":
			sensitive = append(sensitive, fmt.Sprintf("%q: audit.PII", f.decl.ColumnName()))
		case "secret":
			sensitive = append(sensitive, fmt.Sprintf("%q: audit.Secret", f.decl.ColumnName()))
		}
	}
	if len(sensitive) > 0 {
		w.f("\tFields: audit.Fields{%s},\n", strings.Join(sensitive, ", "))
	}
	if e.pii {
		w.f("\tPII: true,\n")
	}
	if bound, ok := authzschema.LockAtMost(e.decl); ok {
		// The most rows that one locked list of the entity locks (ST-DECL-EN02, ST-SUP-RP10-LC27).
		w.f("\tLockAtMost: %d,\n", bound)
	}
	w.f("\tSelect: %q,\n\tSelectForUpdate: %q,\n\tSelectForDelete: %q,\n", selectRow, forUpdate, forDelete)
	w.f("\tInsert: %q,\n\tUpdate: %q,\n\tDelete: %q,\n", insert, update, remove)
	// Scan reads the columns of the statements.
	w.f("\tScan: func(row command.Scanner) (%s, command.Meta, error) {\n\t\tvar v %s\n\t\tvar m command.Meta\n", e.goName, e.goName)
	targets := []string{"&v.ID"}
	for _, f := range e.fields {
		targets = append(targets, "&v."+f.goName)
	}
	targets = append(targets, "&m.Version", "&m.Token")
	w.f("\t\terr := row.Scan(%s)\n\t\tm.ID = v.ID\n\t\treturn v, m, err\n\t},\n", strings.Join(targets, ", "))
	// Values gives the ID and the field values; a new row with no ID gets a new one.
	values := []string{"id"}
	for _, f := range e.fields {
		values = append(values, "v."+f.goName)
	}
	w.f("\tValues: func(v %s) []any {\n\t\tid := v.ID\n\t\tif id == \"\" {\n\t\t\tid = command.NewID()\n\t\t}\n\t\treturn []any{%s}\n\t},\n", e.goName, strings.Join(values, ", "))
	state := []string{`"id": v.ID`}
	for _, f := range e.fields {
		// The audit runtime encodes plain values only, so an optional field gives its value, or nil when it is not set.
		if f.pointer {
			state = append(state, fmt.Sprintf("%q: command.StateOf(v.%s)", f.decl.ColumnName(), f.goName))
			continue
		}
		// A jsonb column holds JSON text: its state is that JSON value, which the audit runtime decodes and redacts
		// at any depth (ST-SUP-RP05-LC03). A NULL column is nil.
		if f.decl.Type == types.FieldTypeJsonb {
			state = append(state, fmt.Sprintf("%q: json.RawMessage(v.%s)", f.decl.ColumnName(), f.goName))
			continue
		}
		state = append(state, fmt.Sprintf("%q: v.%s", f.decl.ColumnName(), f.goName))
	}
	w.f("\tState: func(v %s) map[string]any {\n\t\treturn map[string]any{%s}\n\t},\n", e.goName, strings.Join(state, ", "))
	changed := []string{"before.ID != after.ID"}
	for _, f := range e.keys {
		if f.slice {
			changed = append(changed, fmt.Sprintf("!bytes.Equal(before.%s, after.%s)", f.goName, f.goName))
			continue
		}
		if f.pointer {
			changed = append(changed, fmt.Sprintf("!command.EqualPointer(before.%s, after.%s)", f.goName, f.goName))
			continue
		}
		changed = append(changed, fmt.Sprintf("before.%s != after.%s", f.goName, f.goName))
	}
	w.f("\tKeyChanged: func(before, after %s) bool {\n\t\treturn %s\n\t},\n", e.goName, strings.Join(changed, " || "))
	if !e.deep {
		w.f("\tClone: func(v %s) %s { return v },\n", e.goName, e.goName)
	} else {
		w.f("\tClone: func(v %s) %s {\n", e.goName, e.goName)
		for _, f := range e.fields {
			switch {
			case f.slice:
				w.f("\t\tv.%s = bytes.Clone(v.%s)\n", f.goName, f.goName)
			case f.pointer:
				w.f("\t\tv.%s = command.ClonePointer(v.%s)\n", f.goName, f.goName)
			}
		}
		w.f("\t\treturn v\n\t},\n")
	}
	w.f("\tValidID: command.ValidUUID,\n")
	renderCheck(w, e)
	if len(e.references) > 0 {
		w.f("\tParent: []command.Reference[%s]{\n", e.goName)
		for _, r := range e.references {
			id := fmt.Sprintf("func(v %s) string { return v.%s }", e.goName, r.field.goName)
			if r.field.pointer {
				id = fmt.Sprintf("func(v %s) string {\n\t\tif v.%s == nil {\n\t\t\treturn \"\"\n\t\t}\n\t\treturn *v.%s\n\t}", e.goName, r.field.goName, r.field.goName)
			}
			w.f("\t\t{Entity: %q, ResourceType: %q, ID: %s", r.entity, r.resourceType, id)
			if r.permission != "" {
				w.f(", Permission: %q", r.permission)
			}
			if r.relation != "" {
				w.f(", Relation: %q", r.relation)
			}
			if r.within != "" {
				// The runtime compares the within field of the row and of the referenced row (ST-SUP-RP10-LC37).
				// A CHECK of an optional within field refuses a set reference with no within field; its
				// violation answers 400 as the key's does (ST-SUP-RC01-OC46).
				w.f(", Field: %q, Within: %q, Key: %q, Check: %q", r.field.decl.Name, r.within, postgresadapter.WithinKeyName(e.decl.Name, r.field.decl.ColumnName()),
					postgresadapter.WithinCheckName(e.decl.Name, r.field.decl.ColumnName()))
			}
			w.f("},\n")
		}
		w.f("\t},\n")
	}
	if len(e.within) > 0 {
		w.f("\tWithin: map[string]func(v %s) string{\n", e.goName)
		for _, f := range e.within {
			read := fmt.Sprintf("func(v %s) string { return v.%s }", e.goName, f.goName)
			if f.pointer {
				read = fmt.Sprintf("func(v %s) string {\n\t\tif v.%s == nil {\n\t\t\treturn \"\"\n\t\t}\n\t\treturn *v.%s\n\t}", e.goName, f.goName, f.goName)
			}
			w.f("\t\t%q: %s,\n", f.decl.Name, read)
		}
		w.f("\t},\n")
	}
	if len(e.referrers) > 0 {
		w.f("\tReferrers: []command.Referrer{\n")
		for _, k := range e.referrers {
			w.f("\t\t{Key: %q, Field: %q, Within: %q},\n", k.key, k.field, k.within)
		}
		w.f("\t},\n")
	}
	if len(e.schemaRelations) > 0 {
		quoted := make([]string, len(e.schemaRelations))
		for i, name := range e.schemaRelations {
			quoted[i] = fmt.Sprintf("%q", name)
		}
		w.f("\tRelations: []string{%s},\n", strings.Join(quoted, ", "))
	}
	if e.tenant {
		// The runtime relates each new row to its tenant (ST-SUP-RP10-LC05, ST-SUP-Q132).
		w.f("\tTenant: true,\n")
	}
	if len(e.relationships) > 0 {
		w.f("\tRelationships: []command.Relationship[%s]{\n", e.goName)
		for _, r := range e.relationships {
			w.f("\t\t{Relation: %q, SubjectType: %q", r.relation, r.subjectType)
			if f := r.field; f != nil {
				read := fmt.Sprintf("func(v %s) string { return v.%s }", e.goName, f.goName)
				if f.pointer {
					read = fmt.Sprintf("func(v %s) string {\n\t\tif v.%s == nil {\n\t\t\treturn \"\"\n\t\t}\n\t\treturn *v.%s\n\t}", e.goName, f.goName, f.goName)
				}
				w.f(", Field: %s, FieldName: %q", read, f.decl.Name)
				if r.resource {
					w.f(", Resource: true")
				}
			}
			w.f("},\n")
		}
		w.f("\t},\n")
	}
	w.f("}))\n\n")
	w.f("// %sRepository gives the repository of %s of a command handle; the handle refuses an entity that its command does\n// not declare it writes.\n", e.goName, e.decl.Name)
	w.f("func %sRepository[P any](h *command.Handle[P]) (*command.Repository[%s], error) {\n\treturn command.Repo(h, %sEntity)\n}\n\n", e.goName, e.goName, e.goName)
}
