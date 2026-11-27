package declaration

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"slices"
	"sort"
	"strings"

	core "github.com/authzed/spicedb/pkg/proto/core/v1"
	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/schemadsl/input"

	"github.com/910-software-solutions/stego/internal/types"
)

// The checks of the authorization that a declaration binds (ST-DECL-UC06): the authorization setting, the .zed
// schema in the service's own source, which checking compiles with the schema compiler of the SpiceDB provider, and
// every name the declaration takes from it.

// Providers are the authorization providers a declaration may bind.
var Providers = []string{"spicedb"}

// MaxSchemaBytes limits the size of the .zed schema that checking reads.
const MaxSchemaBytes = 1 << 20

// AuthzSchema holds what checking reads from a compiled .zed schema: each definition with its relations and
// permissions, and the parameters of its caveats.
type AuthzSchema struct {
	// Path is the schema's path in the project folder.
	Path string
	// definitions maps each definition name to its relations and permissions.
	definitions map[string]schemaDefinition
	// caveatParameters holds the parameter names of every caveat.
	caveatParameters map[string]bool
	// wildcards holds each type#name of a relation or permission that can grant a wildcard subject
	// (wildcardReach).
	wildcards map[string]bool
}

// schemaDefinition is one definition of the schema: each relation with the subject types it admits, and the names
// of its permissions.
type schemaDefinition struct {
	relations   map[string][]string
	permissions map[string]bool
	// subjects maps each relation to the subjects it admits, each with its relation and whether it is a wildcard
	// such as user:*.
	subjects map[string][]schemaSubject
	// rewrites maps each permission to its expression.
	rewrites map[string]*core.UsersetRewrite
}

// schemaSubject is one subject that a relation admits: a type, the relation of a userset such as group#member, and
// whether it is the wildcard of the type, such as user:*.
type schemaSubject struct {
	namespace string
	relation  string
	wildcard  bool
	// caveat is the caveat the relation requires of the subject, such as with ip_allowed, or "".
	caveat string
	// expiration tells whether the relation requires an expiration of the subject (with expiration).
	expiration bool
}

// SchemaError is a .zed schema that does not compile: its path, the line where compiling stopped and why.
type SchemaError struct {
	Path    string
	Line    int
	Message string
}

func (e *SchemaError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s line %d: %s", e.Path, e.Line, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Path, e.Message)
}

// CompileAuthzSchema compiles the .zed schema source at the path with the SpiceDB schema compiler, and then checks
// that every type, relation and caveat the schema refers to is one it defines. It returns a *SchemaError, which
// names the line, for a schema that does not compile. Before it compiles, it refuses a schema beyond the shape bounds
// of checkSchemaShape with a *SchemaError that names the limit (ST-DECL-INV88).
func CompileAuthzSchema(schemaPath string, source []byte) (*AuthzSchema, error) {
	return shapedCompile(schemaPath, source, compileSchema)
}

// shapedCompile refuses a schema beyond the shape bounds of checkSchemaShape, and gives only a schema in the bounds to
// compile. CompileAuthzSchema gives it compileSchema; a test gives it a compile that fails the test, to show that a
// refused schema never reaches the compiler.
func shapedCompile(schemaPath string, source []byte, compile func(string, []byte) (*AuthzSchema, error)) (*AuthzSchema, error) {
	if err := checkSchemaShape(schemaPath, source); err != nil {
		return nil, err
	}
	return compile(schemaPath, source)
}

// compileSchema compiles and reads the .zed schema as CompileAuthzSchema does, with no shape bound. Only
// CompileAuthzSchema calls it outside the tests, and the tests call it to show that the checks after the compiler stay
// linear on a schema beyond the bounds.
func compileSchema(schemaPath string, source []byte) (*AuthzSchema, error) {
	compiled, err := compiler.Compile(compiler.InputSchema{Source: input.Source(schemaPath), SchemaString: string(source)},
		compiler.AllowUnprefixedObjectType(), compiler.DisallowImportFlag())
	if err != nil {
		fault := &SchemaError{Path: schemaPath, Message: err.Error()}
		var located compiler.WithContextError
		if errors.As(err, &located) {
			if line, _, lineErr := located.SourceRange.Start().LineAndColumn(); lineErr == nil {
				fault.Line = line + 1
			}
			fault.Message = located.BaseMessage
		}
		return nil, fault
	}
	schema := &AuthzSchema{Path: schemaPath, definitions: map[string]schemaDefinition{}, caveatParameters: map[string]bool{}}
	caveats := map[string]bool{}
	for _, caveat := range compiled.CaveatDefinitions {
		caveats[caveat.GetName()] = true
		for parameter := range caveat.GetParameterTypes() {
			schema.caveatParameters[parameter] = true
		}
	}
	for _, definition := range compiled.ObjectDefinitions {
		entry := schemaDefinition{relations: map[string][]string{}, permissions: map[string]bool{}, subjects: map[string][]schemaSubject{}, rewrites: map[string]*core.UsersetRewrite{}}
		for _, relation := range definition.GetRelation() {
			if relation.GetUsersetRewrite() != nil {
				entry.permissions[relation.GetName()] = true
				entry.rewrites[relation.GetName()] = relation.GetUsersetRewrite()
				continue
			}
			var subjects []string
			for _, allowed := range relation.GetTypeInformation().GetAllowedDirectRelations() {
				subjects = append(subjects, allowed.GetNamespace())
				entry.subjects[relation.GetName()] = append(entry.subjects[relation.GetName()], schemaSubject{namespace: allowed.GetNamespace(), relation: allowed.GetRelation(), wildcard: allowed.GetPublicWildcard() != nil, caveat: allowed.GetRequiredCaveat().GetCaveatName(), expiration: allowed.GetRequiredExpiration() != nil})
			}
			entry.relations[relation.GetName()] = subjects
		}
		schema.definitions[definition.GetName()] = entry
	}
	if err := schema.checkReferences(compiled.ObjectDefinitions, caveats); err != nil {
		return nil, err
	}
	schema.wildcards = schema.wildcardReach()
	return schema, nil
}

// checkReferences checks that each subject type a relation admits is a definition of the schema, with the relation
// it names, that each caveat a relation requires is defined, and that each relation a permission computes is one of
// its own definition. The compiler parses the schema but leaves these to the provider, which would refuse the
// schema only when the service writes it.
func (s *AuthzSchema) checkReferences(definitions []*core.NamespaceDefinition, caveats map[string]bool) error {
	for _, definition := range definitions {
		own := s.definitions[definition.GetName()]
		for _, relation := range definition.GetRelation() {
			for _, allowed := range relation.GetTypeInformation().GetAllowedDirectRelations() {
				subject, defined := s.definitions[allowed.GetNamespace()]
				switch {
				case !defined:
					return s.referenceError(allowed.GetSourcePosition(), fmt.Sprintf("relation %s of %s admits the type %s, which the schema does not define", relation.GetName(), definition.GetName(), allowed.GetNamespace()))
				case allowed.GetRelation() != "" && allowed.GetRelation() != "..." && !subject.has(allowed.GetRelation()):
					return s.referenceError(allowed.GetSourcePosition(), fmt.Sprintf("relation %s of %s admits %s#%s, which the schema does not define", relation.GetName(), definition.GetName(), allowed.GetNamespace(), allowed.GetRelation()))
				case allowed.GetRequiredCaveat() != nil && !caveats[allowed.GetRequiredCaveat().GetCaveatName()]:
					return s.referenceError(allowed.GetSourcePosition(), fmt.Sprintf("relation %s of %s requires the caveat %s, which the schema does not define", relation.GetName(), definition.GetName(), allowed.GetRequiredCaveat().GetCaveatName()))
				}
			}
			for _, name := range computedRelations(relation.GetUsersetRewrite()) {
				if !own.has(name.relation) {
					return s.referenceError(name.position, fmt.Sprintf("permission %s of %s uses %s, which %s does not define", relation.GetName(), definition.GetName(), name.relation, definition.GetName()))
				}
			}
		}
	}
	return nil
}

// referenceError is the schema error at a position of the compiled schema.
func (s *AuthzSchema) referenceError(position *core.SourcePosition, message string) error {
	fault := &SchemaError{Path: s.Path, Message: message}
	if position != nil {
		fault.Line = int(position.GetZeroIndexedLineNumber()) + 1
	}
	return fault
}

// computedName is a relation that a permission computes in its own definition, with where the schema names it.
type computedName struct {
	relation string
	position *core.SourcePosition
}

// computedRelations gives the relations of its own definition that a permission's expression names: each computed
// relation and the left side of each arrow.
func computedRelations(rewrite *core.UsersetRewrite) []computedName {
	if rewrite == nil {
		return nil
	}
	var operation *core.SetOperation
	switch {
	case rewrite.GetUnion() != nil:
		operation = rewrite.GetUnion()
	case rewrite.GetIntersection() != nil:
		operation = rewrite.GetIntersection()
	default:
		operation = rewrite.GetExclusion()
	}
	var names []computedName
	for _, child := range operation.GetChild() {
		switch {
		case child.GetComputedUserset() != nil:
			names = append(names, computedName{child.GetComputedUserset().GetRelation(), child.GetSourcePosition()})
		case child.GetTupleToUserset() != nil:
			names = append(names, computedName{child.GetTupleToUserset().GetTupleset().GetRelation(), child.GetSourcePosition()})
		case child.GetFunctionedTupleToUserset() != nil:
			names = append(names, computedName{child.GetFunctionedTupleToUserset().GetTupleset().GetRelation(), child.GetSourcePosition()})
		case child.GetUsersetRewrite() != nil:
			names = append(names, computedRelations(child.GetUsersetRewrite())...)
		}
	}
	return names
}

// has tells whether the definition has a relation or a permission of the name.
func (d schemaDefinition) has(name string) bool {
	_, relation := d.relations[name]
	return relation || d.permissions[name]
}

// SchemaSubject is one subject that a relation of the schema admits: its type, the relation of a userset such as
// group#member or "", the caveat that the relation requires of it or "", whether the relation requires an expiration
// of it, and whether it is the wildcard of the type.
type SchemaSubject struct {
	Type       string
	Relation   string
	Caveat     string
	Expiration bool
	Wildcard   bool
}

// RelationSubjects gives the subjects that a relation of the type admits, in the order of the schema, and false when
// the type has no relation of the name. The architecture model expands the grant paths from it (ST-GEN-UC08-R13).
func (s *AuthzSchema) RelationSubjects(typeName, relation string) ([]SchemaSubject, bool) {
	definition, ok := s.definitions[typeName]
	if !ok {
		return nil, false
	}
	if _, isRelation := definition.relations[relation]; !isRelation {
		return nil, false
	}
	subjects := make([]SchemaSubject, 0, len(definition.subjects[relation]))
	for _, subject := range definition.subjects[relation] {
		subjects = append(subjects, SchemaSubject{Type: subject.namespace, Relation: subject.relation, Caveat: subject.caveat, Expiration: subject.expiration, Wildcard: subject.wildcard})
	}
	return subjects, true
}

// PermissionRewrite gives the expression of a permission of the type, and false when the type has no permission of
// the name.
func (s *AuthzSchema) PermissionRewrite(typeName, permission string) (*core.UsersetRewrite, bool) {
	definition, ok := s.definitions[typeName]
	if !ok || !definition.permissions[permission] {
		return nil, false
	}
	return definition.rewrites[permission], true
}

// ReadAuthzSchema reads and compiles the .zed schema that the declaration binds, from the project folder, with the
// same checks of the path and the size as checking makes; it gives nil and no error when the declaration binds no
// schema.
func ReadAuthzSchema(service *types.ServiceDeclaration, project fs.FS) (*AuthzSchema, error) {
	if service.Authorization == nil || service.Authorization.SchemaPath == "" {
		return nil, nil
	}
	schema, fault := readAuthzSchema(service.Authorization.SchemaPath, project)
	if fault != nil {
		return nil, fmt.Errorf("%s: %s", fault.Path, fault.Message)
	}
	return schema, nil
}

// Defines tells whether the schema has a definition of the type.
func (s *AuthzSchema) Defines(typeName string) bool {
	_, ok := s.definitions[typeName]
	return ok
}

// Relations gives the names of the relations that the schema declares on the type, in sorted order, and none for a
// type that the schema does not define. The command generator gives them to the runtime, which clears each on the
// resource of a deleted row (ST-SUP-Q118).
func (s *AuthzSchema) Relations(typeName string) []string {
	definition, ok := s.definitions[typeName]
	if !ok {
		return nil
	}
	names := make([]string, 0, len(definition.relations))
	for name := range definition.relations {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// RelationAdmits tells whether the type of the schema has the relation and the relation admits the subject type,
// such as tenant#worker_sync_partner_orders and service_account (ST-SUP-RC35-OC13).
func (s *AuthzSchema) RelationAdmits(typeName, relation, subjectType string) bool {
	for _, subject := range s.definitions[typeName].relations[relation] {
		if subject == subjectType {
			return true
		}
	}
	return false
}

// grants tells whether a relation of the schema admits the subject type.
func (s *AuthzSchema) grants(subjectType string) bool { return s.grantsBut(subjectType, "", "") }

// grantsBut tells whether a relation of the schema, other than the relation relation of the type typeName, admits the
// subject type.
func (s *AuthzSchema) grantsBut(subjectType, typeName, relation string) bool {
	for name, definition := range s.definitions {
		for own, subjects := range definition.relations {
			if name == typeName && own == relation {
				continue
			}
			for _, subject := range subjects {
				if subject == subjectType {
					return true
				}
			}
		}
	}
	return false
}

// CheckAuthorization runs the checks of the authorization that the declaration binds, reading the schema from the
// project folder: the authorization setting (ST-DECL-UC06-R01), the schema path and its compile, every name the
// declaration takes from the schema (ST-DECL-INV22), the relations a slot binding may write (ST-DECL-INV23), the
// system principal (ST-DECL-INV24), the relationships of each entity (ST-DECL-UC06-R04) and the coverage of every
// operation (ST-DECL-INV31). A declaration that binds no authorization and names none of it gets no fault. project
// is the project folder; a schema path that leaves it is refused.
func CheckAuthorization(service *types.ServiceDeclaration, project fs.FS) []ValidationError {
	if service.Authorization == nil {
		return authorizationUnbound(service)
	}
	faults := validateAuthorizationSetting(service.Authorization)
	faults = append(faults, validateAuthorizationTenancy(service)...)
	faults = append(faults, validateSystemEntities(service.Entities)...)
	faults = append(faults, validateRelationshipParts(service.Entities)...)
	faults = append(faults, validateAuthorizationCoverage(service)...)
	schema, fault := readAuthzSchema(service.Authorization.SchemaPath, project)
	if fault != nil {
		faults = append(faults, operationsAuthorization(service, nil)...)
		return append(faults, *fault)
	}
	if schema == nil {
		return append(faults, operationsAuthorization(service, nil)...)
	}
	faults = append(faults, operationsAuthorization(service, schema)...)
	faults = append(faults, validateResetSchema(service, schema)...)
	faults = append(faults, validateSchemaNames(service, schema)...)
	faults = append(faults, validateWrittenRelations(service, schema)...)
	faults = append(faults, validateSystemPrincipal(service, schema)...)
	faults = append(faults, checkProvisioningSchema(service, schema)...)
	faults = append(faults, checkTenantServiceRelation(service, schema)...)
	faults = append(faults, checkOutboxDestination(schema)...)
	faults = append(faults, droppedSchemaNames(service, schema)...)
	faults = append(faults, relateSchema(service, schema)...)
	return faults
}

// relateSchema refuses a relate step whose relation the schema does not give the resource type of its entity
// (decision ST-SUP-Q212 (a)): the relation tenant of the subject type tenant, which the runtime writes for each row,
// or the relation of a relationship or a reference, which the schema names already.
func relateSchema(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	for i, change := range service.SchemaChanges {
		if change.Kind != "relate" {
			continue
		}
		entity, ok := findEntity(service.Entities, change.Entity)
		if !ok || entity.ResourceType == "" {
			continue
		}
		subject := ""
		for _, r := range entity.Relationships {
			if r.Relation == change.Relation {
				subject = r.SubjectType
			}
		}
		if subject == "" && change.Relation == "tenant" {
			subject = "tenant"
		}
		if subject != "" && !schema.RelationAdmits(entity.ResourceType, change.Relation, subject) {
			faults = append(faults, ValidationError{Category: "schema-change", Rule: "schema-change-entity", Path: schemaChangePath(i) + ".relation", Found: change.Relation,
				Message: fmt.Sprintf("schema change %q relates the rows of %s through %s#%s, which the schema %s does not give the subject type %s; add relation %s: %s to the definition %s", change.ID, change.Entity, entity.ResourceType, change.Relation, schema.Path, subject, change.Relation, subject, entity.ResourceType)})
		}
	}
	return faults
}

// droppedSchemaNames refuses a schema that does not define the resource type of an entity with dropped_by, with each
// relation of its references: the release before the drop still writes the relationships of that type, and the
// provider refuses a schema that removes a definition or a relation that has relationships, so the schema keeps the
// definition while the entity stays declared (ST-SUP-RC29-OC25, ST-GEN-Q47).
func droppedSchemaNames(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	for _, entity := range service.DroppedEntities {
		a := entity.EntityAuthorization
		if a.ResourceType == "" {
			continue
		}
		// The relationships of the dropped type stay at the provider, so a live entity with the same resource type
		// would take them for a row that gets an old ID (security review of PR #423, L3).
		for i, live := range service.Entities {
			if live.ResourceType == a.ResourceType {
				faults = append(faults, ValidationError{Category: "authorization", Rule: "schema-names", Path: fmt.Sprintf("entities[%d].resource_type", i), Found: a.ResourceType,
					Message: fmt.Sprintf("the entity %s has the resource type %q of the dropped entity %s, whose relationships stay at the provider while its definition stays; give %s another resource type", live.Name, a.ResourceType, entity.Name, live.Name)})
			}
		}
		fault := func(found, message string) {
			faults = append(faults, ValidationError{Category: "authorization", Rule: "schema-names", Path: "entities." + entity.Name + ".resource_type", Found: found,
				Message: fmt.Sprintf("the dropped entity %s %s, which the schema %s lacks; the release before the drop still writes its relationships, and the provider refuses a schema that removes them, so keep its definition in the schema", entity.Name, message, schema.Path)})
		}
		if !schema.Defines(a.ResourceType) {
			fault(a.ResourceType, fmt.Sprintf("has the resource type %q", a.ResourceType))
			continue
		}
		relations := schema.Relations(a.ResourceType)
		for _, field := range slices.Sorted(maps.Keys(a.References)) {
			if relation := a.References[field].Relation; relation != "" && !slices.Contains(relations, relation) {
				fault(relation, fmt.Sprintf("writes the relation %s#%s for its reference %s", a.ResourceType, relation, field))
			}
		}
	}
	return faults
}

// OutboxDestinationType and OutboxResolvePermission are the resource type and the permission on which the outbox
// decides the resolve of a parked message: resolve on outbox_destination:<destination> (ST-SUP-RC29-OC20).
const (
	OutboxDestinationType   = "outbox_destination"
	OutboxResolvePermission = "resolve"
)

// checkOutboxDestination checks that the schema of a service that binds authorization declares the resource type
// outbox_destination with the permission resolve (ST-SUP-RC29-OC20). The runtime wiring of each such service
// registers the parked-message routes, and its authz relay delivers the relationship writes and revocations through
// the outbox destination authz, also with no mixin bound (security review of PR #425, M1). Without the type the
// routes decide on a type the provider does not know, and answer 500. A resolve that reaches a wildcard subject is
// refused too: anyone could then send a parked message again or discard it, also a revocation of the destination
// authz.
func checkOutboxDestination(schema *AuthzSchema) []ValidationError {
	fault := ValidationError{Category: "authorization", Rule: "outbox-resolve", Path: "authorization.schema_path", Found: schema.Path}
	switch {
	case !schema.Defines(OutboxDestinationType):
		fault.Allowed = []string{OutboxDestinationType}
		fault.Message = fmt.Sprintf("the declaration binds authorization, but the schema %s does not define the resource type %s; the outbox decides the resolve of a parked message on the permission %s of %s:<destination>, so define %s with the permission %s",
			schema.Path, OutboxDestinationType, OutboxResolvePermission, OutboxDestinationType, OutboxDestinationType, OutboxResolvePermission)
	case !schema.declaresPermission(OutboxDestinationType, OutboxResolvePermission):
		fault.Allowed = []string{OutboxResolvePermission}
		fault.Message = fmt.Sprintf("the declaration binds authorization, but the resource type %s of the schema %s has no permission %s; the outbox decides the resolve of a parked message on it, so add the permission %s",
			OutboxDestinationType, schema.Path, OutboxResolvePermission, OutboxResolvePermission)
	case schema.reachesWildcard(OutboxDestinationType, OutboxResolvePermission):
		fault.Message = fmt.Sprintf("the permission %s of the resource type %s reaches a wildcard subject of the schema %s, so anyone could resolve a parked message; give it to the operators only",
			OutboxResolvePermission, OutboxDestinationType, schema.Path)
	default:
		return nil
	}
	return []ValidationError{fault}
}

// readAuthzSchema reads and compiles the schema at the path in the project folder. It gives no schema and no fault
// when the path is empty, which validateAuthorizationSetting reports.
func readAuthzSchema(schemaPath string, project fs.FS) (*AuthzSchema, *ValidationError) {
	if schemaPath == "" {
		return nil, nil
	}
	fault := func(message string) *ValidationError {
		return &ValidationError{Category: "authorization", Rule: "schema-path", Path: "authorization.schema_path", Found: schemaPath, Message: message}
	}
	if !fs.ValidPath(schemaPath) || path.Clean(schemaPath) != schemaPath {
		return nil, fault(fmt.Sprintf("the schema path %q is not a clean relative path inside the service's own source; give the path of the .zed schema relative to the project folder, such as authz/schema.zed", schemaPath))
	}
	file, err := project.Open(schemaPath)
	if err != nil {
		return nil, fault(fmt.Sprintf("the schema %q cannot be read inside the service's own source: %v", schemaPath, err))
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, fault(fmt.Sprintf("the schema %q is not a regular file inside the service's own source", schemaPath))
	}
	source, err := io.ReadAll(io.LimitReader(file, MaxSchemaBytes+1))
	if err != nil {
		return nil, fault(fmt.Sprintf("the schema %q cannot be read: %v", schemaPath, err))
	}
	if len(source) > MaxSchemaBytes {
		return nil, fault(fmt.Sprintf("the schema %q is larger than %d bytes", schemaPath, MaxSchemaBytes))
	}
	schema, err := CompileAuthzSchema(schemaPath, source)
	var compileFault *SchemaError
	if errors.As(err, &compileFault) {
		found := schemaPath
		if compileFault.Line > 0 {
			found = fmt.Sprintf("%s:%d", schemaPath, compileFault.Line)
		}
		return nil, &ValidationError{Category: "authorization", Rule: "schema-compile", Path: "authorization.schema_path", Found: found,
			Message: fmt.Sprintf("the schema does not compile: %s", compileFault.Error())}
	}
	if err != nil {
		return nil, fault(err.Error())
	}
	return schema, nil
}

// validateAuthorizationTenancy checks that a declaration that binds authorization declares pooled tenancy
// (ST-DECL-INV84): every resource ID at the provider is tenant-qualified (ST-SUP-RP06-LC22), and only a stated
// pooled tenancy selects the tenancy component that gives each table its tenant column (ST-GEN-INV37), so an
// omitted tenancy, which relies on the default, is refused as well as kind single.
func validateAuthorizationTenancy(service *types.ServiceDeclaration) []ValidationError {
	tenancy := service.Tenancy
	switch {
	case tenancy == nil:
		return []ValidationError{{Category: "authorization", Rule: "authorization-tenancy", Path: "tenancy",
			Message: "the declaration binds authorization but does not declare tenancy; every resource ID at the provider is tenant-qualified, so declare tenancy with kind pooled, its key and its claim"}}
	case tenancy.Kind != "" && tenancy.Kind != "pooled":
		return []ValidationError{{Category: "authorization", Rule: "authorization-tenancy", Path: "tenancy.kind", Found: tenancy.Kind,
			Allowed: []string{"pooled"}, Message: fmt.Sprintf("the declaration binds authorization with tenancy kind %q; every resource ID at the provider is tenant-qualified, so declare tenancy with kind pooled", tenancy.Kind)}}
	}
	return nil
}

// validateAuthorizationSetting checks that the authorization setting names a known provider, the schema path and
// the subject type of a request's identity (ST-DECL-UC06-R01).
func validateAuthorizationSetting(setting *types.Authorization) []ValidationError {
	var faults []ValidationError
	missing := func(key, name, hint string) {
		faults = append(faults, ValidationError{Category: "authorization", Rule: "authorization-setting", Path: "authorization." + key,
			Message: fmt.Sprintf("the authorization setting %s is missing; %s", name, hint)})
	}
	switch {
	case setting.Provider == "":
		missing("provider", "provider", "name the provider of the decision and relationship ports, such as spicedb")
		faults[len(faults)-1].Allowed = append([]string(nil), Providers...)
	case !contains(Providers, setting.Provider):
		faults = append(faults, ValidationError{Category: "authorization", Rule: "authorization-setting", Path: "authorization.provider", Found: setting.Provider,
			Allowed: append([]string(nil), Providers...), Message: fmt.Sprintf("the authorization provider %q is not one that STEGO binds", setting.Provider)})
	}
	if setting.SchemaPath == "" {
		missing("schema_path", "schema path", "give the path of the .zed schema in the service's own source, such as authz/schema.zed")
	}
	if setting.SubjectFromIdentity == "" {
		missing("subject_from_identity", "subject from identity", "name the subject type of the schema that a request's identity maps to, such as user")
	}
	return faults
}

// authorizationUnbound gives a fault for each place that names authorization when the declaration binds none: an
// entity's resource type, permissions, context fields or relationships, and a slot binding's relationships written.
// With no schema, none of these names can exist in it (ST-DECL-INV22).
func authorizationUnbound(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	fault := func(place, message string) {
		faults = append(faults, ValidationError{Category: "authorization", Rule: "schema-names", Path: place, Found: place,
			Message: message + ", but the declaration binds no authorization; add the authorization setting with its provider, schema path and subject from identity, or remove it"})
	}
	for ei, entity := range service.Entities {
		a := entity.EntityAuthorization
		if a.ResourceType != "" {
			fault(fmt.Sprintf("entities[%d].resource_type", ei), fmt.Sprintf("entity %q names the resource type %q", entity.Name, a.ResourceType))
		}
		if len(a.Permissions) > 0 {
			fault(fmt.Sprintf("entities[%d].permissions", ei), fmt.Sprintf("entity %q names permissions", entity.Name))
		}
		if len(a.ContextFields) > 0 {
			fault(fmt.Sprintf("entities[%d].context_fields", ei), fmt.Sprintf("entity %q names context fields", entity.Name))
		}
		if len(a.Relationships) > 0 {
			fault(fmt.Sprintf("entities[%d].relationships", ei), fmt.Sprintf("entity %q declares relationships", entity.Name))
		}
	}
	operations := func(kind, path, resourceType, permission string, relations []string) {
		if resourceType != "" || permission != "" || len(relations) > 0 {
			fault(path, fmt.Sprintf("the %s at %s names a resource type, a permission or relationships written", kind, path))
		}
	}
	for i, command := range service.Commands {
		operations("command", commandPath(i), command.ResourceType, command.Permission, command.RelationshipsWritten)
	}
	for i, query := range service.Queries {
		operations("query", queryPath(i), query.ResourceType, query.Permission, nil)
	}
	for i, worker := range service.Workers {
		operations("worker", workerPath(i), worker.ResourceType, worker.Permission, nil)
	}
	for si, slot := range service.Slots {
		if len(slot.RelationshipsWritten) > 0 {
			fault(fmt.Sprintf("slots[%d].relationships_written", si), fmt.Sprintf("the binding of %s at %s lists relationships written", slot.Collection, slot.Slot))
		}
	}
	return faults
}

// validateRelationshipParts checks that each relationship of an entity names its relation, its subject source, the
// subject field when the source is field, and its subject type (ST-DECL-UC06-R04).
func validateRelationshipParts(entities []types.Entity) []ValidationError {
	var faults []ValidationError
	for ei, entity := range entities {
		for ri, relationship := range entity.Relationships {
			place := fmt.Sprintf("entities[%d].relationships[%d]", ei, ri)
			name := relationshipName(entity, relationship)
			missing := func(key, part, hint string, allowed ...string) {
				faults = append(faults, ValidationError{Category: "authorization", Rule: "relationship", Path: place + "." + key, Found: name, Allowed: allowed,
					Message: fmt.Sprintf("the relationship %s of entity %q has no %s; %s", name, entity.Name, part, hint)})
			}
			if relationship.Relation == "" {
				missing("relation", "relation", "name the relation of the schema under the entity's resource type")
			}
			switch relationship.SubjectSource {
			case "":
				missing("subject_source", "subject source", "say where the subject comes from", types.SubjectSourceField, types.SubjectSourceCaller)
			case types.SubjectSourceField:
				if relationship.SubjectField == "" {
					missing("subject_field", "subject field", "name the field of the entity that holds the subject")
				} else if !hasField(entity, relationship.SubjectField) {
					faults = append(faults, ValidationError{Category: "authorization", Rule: "relationship", Path: place + ".subject_field", Found: relationship.SubjectField,
						Allowed: fieldNames(entity), Message: fmt.Sprintf("the relationship %s of entity %q takes its subject from the field %q, which the entity does not declare", name, entity.Name, relationship.SubjectField)})
				}
			case types.SubjectSourceCaller:
			default:
				faults = append(faults, ValidationError{Category: "authorization", Rule: "relationship", Path: place + ".subject_source", Found: relationship.SubjectSource,
					Allowed: []string{types.SubjectSourceField, types.SubjectSourceCaller}, Message: fmt.Sprintf("the relationship %s of entity %q has the subject source %q, which is not field or caller", name, entity.Name, relationship.SubjectSource)})
			}
			if relationship.SubjectType == "" {
				missing("subject_type", "subject type", "name the schema type of the subject, such as user")
			}
		}
	}
	return faults
}

// relationshipName names a relationship in a fault: its resource type and relation, as far as it states them.
func relationshipName(entity types.Entity, relationship types.Relationship) string {
	relation := relationship.Relation
	if relation == "" {
		relation = "(no relation)"
	}
	if entity.ResourceType == "" {
		return relation
	}
	return entity.ResourceType + "#" + relation
}

// topLevelCreate checks the permission of the create of an entity with no parent, which a collection with no scope
// serves: no row names the new row, so the create is decided on the service-level resource of the tenant
// provisioning, and its permission is one that the schema declares on the provisioning's resource type
// (ST-SUP-RC29-OC01, ST-SUP-RC27-OC09). A declaration with no provisioning has no such resource, so only a public
// create is accepted. An undefined resource type of the provisioning is the fault of checkProvisioningSchema.
func topLevelCreate(service *types.ServiceDeclaration, schema *AuthzSchema, ei int, entity types.Entity, permission string) []ValidationError {
	place := fmt.Sprintf("entities[%d].permissions.%s", ei, types.OpCreate)
	if service.Tenancy == nil || service.Tenancy.Provisioning == nil || service.Tenancy.Provisioning.ResourceType == "" {
		return []ValidationError{{Category: "authorization", Rule: "schema-names", Path: place, Found: permission,
			Message: fmt.Sprintf("entity %q has no parent, so its create is decided on the service-level resource of the tenant provisioning (ST-SUP-RC29-OC01), which the declaration does not declare; declare tenancy.provisioning, or make the create public with a reason", entity.Name)}}
	}
	provisioning := service.Tenancy.Provisioning
	resourceType := provisioning.ResourceType
	// Provision and Retire are decided on the service-level resource with no tenant in its ID, so a grant of them
	// is platform-wide; a create is decided on the tenant-qualified ID of that resource (ST-SUP-Q127 (a)). One name
	// with two meanings would let an operator who grants one of them in a tenant think that it grants nothing else.
	for _, platform := range []struct{ name, permission string }{{"provision", provisioning.Provision}, {"retire", provisioning.Retire}} {
		if platform.permission != "" && permission == platform.permission {
			return []ValidationError{{Category: "authorization", Rule: "schema-names", Path: place, Found: permission,
				Message: fmt.Sprintf("entity %q for the operation create names the permission %q, which tenancy.provisioning.%s also names: a %s is decided on the service-level resource %s:%s with no tenant, but a create is decided on that resource in the tenant of the request (ST-SUP-RC29-OC01), so it needs a permission of its own; declare another permission on %s for the create", entity.Name, permission, platform.name, platform.name, resourceType, service.Name, resourceType)}}
		}
	}
	definition, defined := schema.definitions[resourceType]
	if !defined || definition.permissions[permission] {
		return nil
	}
	return []ValidationError{{Category: "authorization", Rule: "schema-names", Path: place, Found: permission, Allowed: sortedKeys(definition.permissions),
		Message: fmt.Sprintf("entity %q for the operation create names the permission %q, which the resource type %s of the schema %s lacks; an entity with no parent is created on the service-level resource %s:%s of the tenant provisioning (ST-SUP-RC29-OC01)", entity.Name, permission, resourceType, schema.Path, resourceType, service.Name)}}
}

// validateSchemaNames checks that every name the declaration takes from the schema is in it (ST-DECL-INV22): the
// subject type of a request's identity, each entity's resource type, the permission of each operation, the
// relation and subject type of each relationship, and the caveat parameter of each context field.
func validateSchemaNames(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	lacks := func(place, kind, name, where string, allowed []string) {
		faults = append(faults, ValidationError{Category: "authorization", Rule: "schema-names", Path: place, Found: name, Allowed: allowed,
			Message: fmt.Sprintf("%s names the %s %q, which the schema %s lacks", where, kind, name, schema.Path)})
	}
	if schema.Defines(types.FloorMarkerType) {
		faults = append(faults, ValidationError{Category: "authorization", Rule: "schema-names", Path: "authorization.schema_path", Found: types.FloorMarkerType,
			Message: fmt.Sprintf("the schema %s defines %s, the floor marker that STEGO adds to the schema it writes to the provider; remove the definition %s from the schema", schema.Path, types.FloorMarkerType, types.FloorMarkerType)})
	}
	if subject := service.Authorization.SubjectFromIdentity; subject != "" && !schema.Defines(subject) {
		lacks("authorization.subject_from_identity", "subject type", subject, "the authorization setting", schema.typeNames())
	}
	for ei, entity := range service.Entities {
		a := entity.EntityAuthorization
		where := fmt.Sprintf("entity %q", entity.Name)
		definition, defined := schema.definitions[a.ResourceType]
		if a.ResourceType == OutboxDestinationType {
			// The outbox decides the resolve of a parked message on outbox_destination:<destination>; a row of the
			// type could take the ID of a destination such as authz, and its relationships would grant the resolve of
			// that destination (security review of PR #425, L1).
			faults = append(faults, ValidationError{Category: "authorization", Rule: "schema-names", Path: fmt.Sprintf("entities[%d].resource_type", ei), Found: a.ResourceType,
				Message: fmt.Sprintf("%s names the resource type %q, which is reserved for the outbox destinations on which the resolve of a parked message is decided (ST-SUP-RC29-OC20); a row of it could take the ID of a destination, and its relationships would grant the resolve of that destination; give the entity another resource type", where, a.ResourceType)})
		}
		if a.ResourceType == TenantResourceType {
			faults = append(faults, ValidationError{Category: "authorization", Rule: "schema-names", Path: fmt.Sprintf("entities[%d].resource_type", ei), Found: a.ResourceType,
				Message: fmt.Sprintf("%s names the resource type %q, which is reserved for the tenant resource that holds the tuple of each worker (ST-SUP-RC35-OC13, ST-SUP-Q132); a row of it could take the ID of its tenant's resource, and its delete would clear the worker tuples; give the entity another resource type", where, a.ResourceType)})
		}
		if a.ResourceType != "" && !defined {
			lacks(fmt.Sprintf("entities[%d].resource_type", ei), "resource type", a.ResourceType, where, schema.typeNames())
		}
		if defined {
			for _, operation := range sortedOperations(a.Permissions) {
				permission := a.Permissions[operation]
				if operation == types.OpCreate && permission != types.PermissionPublic && types.CreatesTopLevel(service.Collections, entity.Name) {
					faults = append(faults, topLevelCreate(service, schema, ei, entity, permission)...)
					continue
				}
				if permission != types.PermissionPublic && !definition.permissions[permission] {
					lacks(fmt.Sprintf("entities[%d].permissions.%s", ei, operation), "permission", permission,
						fmt.Sprintf("%s for the operation %s", where, operation), sortedKeys(definition.permissions))
				}
			}
		}
		for ri, relationship := range a.Relationships {
			place := fmt.Sprintf("entities[%d].relationships[%d]", ei, ri)
			subjects, isRelation := definition.relations[relationship.Relation]
			if defined && relationship.Relation != "" && !isRelation {
				lacks(place+".relation", "relation", relationship.Relation, fmt.Sprintf("%s in a relationship of the resource type %s", where, a.ResourceType), sortedKeys(definition.relations))
				continue
			}
			if relationship.SubjectType == "" {
				continue
			}
			if !schema.Defines(relationship.SubjectType) {
				lacks(place+".subject_type", "subject type", relationship.SubjectType, where+" in a relationship", schema.typeNames())
				continue
			}
			if isRelation && !contains(subjects, relationship.SubjectType) {
				faults = append(faults, ValidationError{Category: "authorization", Rule: "schema-names", Path: place + ".subject_type", Found: relationship.SubjectType,
					Allowed: unique(subjects), Message: fmt.Sprintf("%s relates the subject type %q by %s#%s, which the schema %s does not admit for that relation", where, relationship.SubjectType, a.ResourceType, relationship.Relation, schema.Path)})
			}
		}
		for ci, field := range a.ContextFields {
			parameter := strings.TrimPrefix(field, "request.")
			if !schema.caveatParameters[parameter] {
				lacks(fmt.Sprintf("entities[%d].context_fields[%d]", ei, ci), "caveat parameter", field, where+" as a context field", prefixed("request.", sortedKeys(schema.caveatParameters)))
			}
		}
	}
	return faults
}

// SchemaLacksName is the predicate of ST-DECL-SP05: an entity, a collection's operation or a slot binding names a
// resource type, a permission, a relation or a caveat parameter that the compiled schema does not declare.
func SchemaLacksName(service *types.ServiceDeclaration, schema *AuthzSchema) bool {
	if service.Authorization == nil {
		service = withAuthorization(service)
	}
	for _, fault := range validateSchemaNames(service, schema) {
		if fault.Path != "authorization.subject_from_identity" && fault.Path != "authorization.schema_path" && !strings.HasSuffix(fault.Path, ".subject_type") {
			return true
		}
	}
	for _, slot := range service.Slots {
		for _, written := range slot.RelationshipsWritten {
			resourceType, relation, ok := strings.Cut(written, "#")
			if !ok {
				continue
			}
			if _, isRelation := schema.definitions[resourceType].relations[relation]; !isRelation {
				return true
			}
		}
	}
	return false
}

// withAuthorization is a copy of the declaration with an empty authorization setting, so that the name checks read
// a declaration that binds none.
func withAuthorization(service *types.ServiceDeclaration) *types.ServiceDeclaration {
	copied := *service
	copied.Authorization = &types.Authorization{}
	return &copied
}

// validateWrittenRelations checks that each relation a slot binding may write is resource_type#relation, that the
// resource type is the type of an entity the service declares, and that the relation is a relation of that type in
// the schema (ST-DECL-INV23).
func validateWrittenRelations(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	declared := declaredResourceTypes(service.Entities)
	for si, slot := range service.Slots {
		binding := fmt.Sprintf("the binding of %s at %s", slot.Collection, slot.Slot)
		for wi, written := range slot.RelationshipsWritten {
			place := fmt.Sprintf("slots[%d].relationships_written[%d]", si, wi)
			if message := writtenRelationUnknown(written, declared, schema, "the service declares"); message != "" {
				fault := ValidationError{Category: "authorization", Rule: "written-relations", Path: place, Found: written, Message: fmt.Sprintf("%s lists %q, %s", binding, written, message)}
				if resourceType, _, ok := strings.Cut(written, "#"); ok && declared[resourceType] {
					fault.Allowed = prefixed(resourceType+"#", sortedKeys(schema.definitions[resourceType].relations))
				} else {
					fault.Allowed = sortedKeys(declared)
				}
				faults = append(faults, fault)
			}
		}
	}
	return faults
}

// writtenRelationUnknown says why a relation a slot binding may write is unknown, or gives "" when it is a relation
// of the schema under the resource type of a declared entity.
func writtenRelationUnknown(written string, declared map[string]bool, schema *AuthzSchema, owner string) string {
	resourceType, relation, ok := strings.Cut(written, "#")
	switch {
	case !ok || resourceType == "" || relation == "" || strings.Contains(relation, "#"):
		return "which is not of the form resource_type#relation"
	case resourceType == TenantResourceType:
		return fmt.Sprintf("but %s is the tenant resource, whose worker relations only the tenant provisioning writes (ST-SUP-RC35-OC13)", TenantResourceType)
	case relation == TenantRelation:
		return fmt.Sprintf("but %s is the relation of each row to its tenant, which the runtime writes alone (ST-SUP-RP10-LC05, ST-SUP-Q132)", TenantRelation)
	case strings.HasPrefix(relation, WorkerRelationPrefix):
		return fmt.Sprintf("but a relation that starts with %s holds the grant of a worker, which only the tenant provisioning writes (ST-SUP-RC35-OC13)", WorkerRelationPrefix)
	case !schema.Defines(resourceType):
		return fmt.Sprintf("but the schema %s lacks the resource type %s", schema.Path, resourceType)
	case !declared[resourceType]:
		return fmt.Sprintf("but %s is the resource type of no entity %s", resourceType, owner)
	}
	if _, isRelation := schema.definitions[resourceType].relations[relation]; !isRelation {
		return fmt.Sprintf("but the schema %s lacks the relation %s of %s", schema.Path, relation, resourceType)
	}
	return ""
}

// SlotRelationUnknown is the predicate of ST-DECL-SP06: a slot binding lists a relation in Relationships written
// that the schema lacks, or that belongs to no resource type of a declared entity.
func SlotRelationUnknown(service *types.ServiceDeclaration, schema *AuthzSchema) bool {
	declared := declaredResourceTypes(service.Entities)
	for _, slot := range service.Slots {
		for _, written := range slot.RelationshipsWritten {
			if writtenRelationUnknown(written, declared, schema, "the service declares") != "" {
				return true
			}
		}
	}
	return false
}

// validateSystemPrincipal checks the system principal half of ST-DECL-INV24: the system subject type is
// service_account, the one subject type the authorization runtime gives a system principal, and the schema defines
// it and grants it relations; and when a slot binding runs as system, the authorization setting names it. So the
// system principal is a subject the schema grants relations to, and never a bypass.
func validateSystemPrincipal(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	// The subject of a request's identity is never service_account: a person with the subject service_account:<name>
	// would get the grants of the worker or the controller named name (security review of PR #337, I7; decision on
	// ST-SUP-Q141).
	if subject := service.Authorization.SubjectFromIdentity; subject == types.SystemSubjectType {
		faults = append(faults, ValidationError{Category: "authorization", Rule: "no-bypass", Path: "authorization.subject_from_identity", Found: subject,
			Message: fmt.Sprintf("the subject type of a request's identity is %s, the subject type of the system principals; a caller would act as the worker or controller of the same name, so name the subject type of the people, such as user", types.SystemSubjectType)})
	}
	subjectType := service.Authorization.SystemSubjectType
	if subjectType != "" {
		fault := ValidationError{Category: "authorization", Rule: "no-bypass", Path: "authorization.system_subject_type", Found: subjectType}
		switch {
		case !schema.Defines(subjectType):
			fault.Allowed = []string{types.SystemSubjectType}
			fault.Message = fmt.Sprintf("the system subject type %q is a type that the schema %s does not define; a system principal acts as %s", subjectType, schema.Path, types.SystemSubjectType)
		case !schema.grants(subjectType):
			fault.Allowed = []string{types.SystemSubjectType}
			fault.Message = fmt.Sprintf("the system subject type %q is a type that no relation of the schema %s grants; a system principal is a subject the schema grants relations to", subjectType, schema.Path)
		case subjectType != types.SystemSubjectType:
			fault.Allowed = []string{types.SystemSubjectType}
			fault.Message = fmt.Sprintf("the system subject type %q is not %s, the one subject type that the authorization runtime gives a system principal", subjectType, types.SystemSubjectType)
		default:
			return nil
		}
		var bindings []string
		for _, slot := range service.Slots {
			if slot.RunsAs == types.RunsAsSystem {
				bindings = append(bindings, fmt.Sprintf("the binding of %s at %s", slot.Collection, slot.Slot))
			}
		}
		if len(bindings) > 0 {
			fault.Message += "; it is the principal of " + strings.Join(bindings, " and ") + ", which runs as system"
		}
		return append(faults, fault)
	}
	for si, slot := range service.Slots {
		if slot.RunsAs != types.RunsAsSystem {
			continue
		}
		faults = append(faults, ValidationError{Category: "authorization", Rule: "no-bypass", Path: fmt.Sprintf("slots[%d].runs_as", si), Found: types.RunsAsSystem, Allowed: []string{types.SystemSubjectType},
			Message: fmt.Sprintf("the binding of %s at %s runs as system, but the authorization setting names no system subject type; name %s, the subject type of the schema that a system principal acts as", slot.Collection, slot.Slot, types.SystemSubjectType)})
	}
	return faults
}

// validateSystemEntities checks that no entity whose tenant is system names a resource type or permissions: a
// resource ID at the provider is tenant-qualified (ST-SUP-RP06-LC22), and an entity with no tenant has no tenant to
// qualify it with. Such an entity is never exposed through a tenant request, so it needs no permission.
func validateSystemEntities(entities []types.Entity) []ValidationError {
	var faults []ValidationError
	for ei, entity := range entities {
		if entity.Tenant != types.TenantSystem {
			continue
		}
		if entity.ResourceType != "" {
			faults = append(faults, systemEntityFault(ei, entity, "resource_type", entity.ResourceType, "the resource type"))
		}
		if len(entity.Permissions) > 0 {
			faults = append(faults, systemEntityFault(ei, entity, "permissions", entity.Name, "permissions"))
		}
	}
	return faults
}

func systemEntityFault(ei int, entity types.Entity, key, found, what string) ValidationError {
	return ValidationError{Category: "authorization", Rule: "system-entity-protected", Path: fmt.Sprintf("entities[%d].%s", ei, key), Found: found,
		Message: fmt.Sprintf("entity %q has the tenant system and names %s; every resource ID at the provider is tenant-qualified, and an entity with no tenant has none to qualify it with, so remove %s or give the entity the tenant owned or shared", entity.Name, what, what)}
}

// declaredResourceTypes gives the resource types of the declared entities.
func declaredResourceTypes(entities []types.Entity) map[string]bool {
	declared := map[string]bool{}
	for _, entity := range entities {
		if entity.ResourceType != "" {
			declared[entity.ResourceType] = true
		}
	}
	return declared
}

// typeNames gives the names of the schema's definitions, in order.
func (s *AuthzSchema) typeNames() []string { return sortedKeys(s.definitions) }

// TypeNames gives the names of the definitions of the schema, sorted.
func (s *AuthzSchema) TypeNames() []string { return s.typeNames() }

func sortedOperations(m map[types.Operation]string) []types.Operation {
	operations := make([]types.Operation, 0, len(m))
	for operation := range m {
		operations = append(operations, operation)
	}
	sort.Slice(operations, func(i, j int) bool { return operations[i] < operations[j] })
	return operations
}

func prefixed(prefix string, names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = prefix + name
	}
	return out
}

func unique(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range names {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func hasField(entity types.Entity, name string) bool {
	for _, field := range entity.Fields {
		if field.Name == name {
			return true
		}
	}
	return false
}

func fieldNames(entity types.Entity) []string {
	names := make([]string, 0, len(entity.Fields))
	for _, field := range entity.Fields {
		names = append(names, field.Name)
	}
	return names
}

// slotSettings resolves the principal that the fills of each slot binding run as: tenant when the binding omits it
// (ST-DECL-EN05). A binding that runs as system states it.
func slotSettings(service *types.ServiceDeclaration) []Setting {
	var settings []Setting
	for i, slot := range service.Slots {
		settings = append(settings, resolved(fmt.Sprintf("slots[%d].runs_as", i), slot.RunsAs, types.RunsAsTenant))
	}
	return settings
}

// reachesWildcard tells whether the relation or permission name of the type can grant a wildcard subject, such as
// user:*, so that every subject of that type holds it (wildcardReach).
func (s *AuthzSchema) reachesWildcard(typeName, name string) bool {
	return s.wildcards[typeName+"#"+name]
}

// wildcardReach computes, for every relation and permission of the schema, whether it can grant a wildcard subject:
// a relation that admits a wildcard; a relation that admits a userset whose relation reaches one; or a permission
// whose expression reaches one through its relations and arrows. A union reaches one when any part does, an
// intersection when every part does, and an exclusion when its base does. Each rule is monotone, so the answer is
// the least fixed point: every node starts at no wildcard, and a node is set when its rule holds by the nodes set so
// far. A cycle with no wildcard stays at no wildcard.
//
// It is computed on an AND/OR graph (security reviews of PR 253, L3 and L4, and of PR 294, M1): each relation and
// permission, each expression and each arrow is a gate, its inputs are the gates its rule reads, once each, and each
// gate counts its inputs that are not set yet. An OR gate is set by its first set input, an AND gate by its last.
// Each edge is followed once, so the work grows with the size of the schema, also on a wide intersection whose parts
// repeat, and no schema makes checking slow.
func (s *AuthzSchema) wildcardReach() map[string]bool {
	reach, _ := s.wildcardReachWork()
	return reach
}

// wildcardReachWork is wildcardReach with its work: each input that the graph is given, and each edge that run
// follows. A test holds the work to the size of the schema, so that it counts the work and does not time it.
func (s *AuthzSchema) wildcardReachWork() (map[string]bool, int) {
	g := &reachGraph{named: map[string]int{}, arrows: map[string]int{}, edges: map[[2]int]bool{}}
	for typeName, definition := range s.definitions {
		for name, subjects := range definition.subjects {
			node := g.name(typeName + "#" + name)
			for _, subject := range subjects {
				switch {
				case subject.wildcard:
					g.wildcard(node)
				case subject.relation != "" && subject.relation != "...":
					g.input(node, g.name(subject.namespace+"#"+subject.relation))
				}
			}
		}
		for name, rewrite := range definition.rewrites {
			g.input(g.name(typeName+"#"+name), s.rewriteGate(g, typeName, rewrite))
		}
	}
	return g.run(), g.work
}

// rewriteGate gives the gate of a permission expression of the type: an OR gate for a union, an AND gate for an
// intersection, and for an exclusion an OR gate whose only input is its base, which alone decides it.
func (s *AuthzSchema) rewriteGate(g *reachGraph, typeName string, rewrite *core.UsersetRewrite) int {
	var children []*core.SetOperation_Child
	gate := g.gate(false)
	switch {
	case rewrite.GetUnion() != nil:
		children = rewrite.GetUnion().GetChild()
	case rewrite.GetIntersection() != nil:
		children = rewrite.GetIntersection().GetChild()
		gate = g.gate(true)
	case rewrite.GetExclusion() != nil:
		children = rewrite.GetExclusion().GetChild()
		if len(children) > 1 {
			children = children[:1]
		}
	}
	for _, c := range children {
		switch {
		case c.GetComputedUserset() != nil:
			g.input(gate, g.name(typeName+"#"+c.GetComputedUserset().GetRelation()))
		case c.GetTupleToUserset() != nil || c.GetFunctionedTupleToUserset() != nil:
			tupleset, computed := c.GetTupleToUserset().GetTupleset().GetRelation(), c.GetTupleToUserset().GetComputedUserset().GetRelation()
			if f := c.GetFunctionedTupleToUserset(); f != nil {
				tupleset, computed = f.GetTupleset().GetRelation(), f.GetComputedUserset().GetRelation()
			}
			g.input(gate, s.arrowGate(g, typeName, tupleset, computed))
		case c.GetUsersetRewrite() != nil:
			g.input(gate, s.rewriteGate(g, typeName, c.GetUsersetRewrite()))
		default:
			g.input(gate, g.gate(false)) // a part that never reaches a wildcard, such as nil
		}
	}
	return gate
}

// arrowGate gives the OR gate of the arrow tupleset->computed of the type, one for each arrow: it is set when a
// subject type of the tupleset is a wildcard, or when the computed relation of a subject type reaches one.
func (s *AuthzSchema) arrowGate(g *reachGraph, typeName, tupleset, computed string) int {
	key := typeName + "#" + tupleset + "->" + computed
	if gate, ok := g.arrows[key]; ok {
		return gate
	}
	gate := g.gate(false)
	g.arrows[key] = gate
	for _, subject := range s.definitions[typeName].subjects[tupleset] {
		if subject.wildcard {
			g.wildcard(gate)
		} else {
			g.input(gate, g.name(subject.namespace+"#"+computed))
		}
	}
	return gate
}

// reachGraph is the AND/OR graph of wildcardReach.
type reachGraph struct {
	and     []bool         // whether each gate is an AND gate
	unset   []int          // the inputs of each gate that are not set yet
	outputs [][]int        // the gates that read each gate
	set     []bool         // whether each gate is set
	wild    []int          // the gates that a wildcard sets at the start
	named   map[string]int // the gate of each relation and permission, by type#name
	arrows  map[string]int // the gate of each arrow, by type#tupleset->computed
	edges   map[[2]int]bool
	work    int // the inputs given and the edges followed (wildcardReachWork)
}

func (g *reachGraph) gate(and bool) int {
	g.and, g.unset, g.outputs, g.set = append(g.and, and), append(g.unset, 0), append(g.outputs, nil), append(g.set, false)
	return len(g.and) - 1
}

// name gives the gate of a relation or permission. A name the schema does not define gets an OR gate with no input,
// which is never set.
func (g *reachGraph) name(node string) int {
	if gate, ok := g.named[node]; ok {
		return gate
	}
	gate := g.gate(false)
	g.named[node] = gate
	return gate
}

// input adds from as an input of the gate, once: a part that an expression repeats is one input.
func (g *reachGraph) input(gate, from int) {
	g.work++
	if g.edges[[2]int{from, gate}] {
		return
	}
	g.edges[[2]int{from, gate}] = true
	g.unset[gate]++
	g.outputs[from] = append(g.outputs[from], gate)
}

func (g *reachGraph) wildcard(gate int) { g.wild = append(g.wild, gate) }

// run sets the gates that a wildcard sets, and then each gate whose rule holds, following each edge once. An OR
// gate is set by one set input, and an AND gate when it has inputs and all of them are set. It gives the relations
// and permissions that are set.
func (g *reachGraph) run() map[string]bool {
	var work []int
	mark := func(gate int) {
		if !g.set[gate] {
			g.set[gate] = true
			work = append(work, gate)
		}
	}
	for _, gate := range g.wild {
		if !g.and[gate] {
			mark(gate)
		}
	}
	for len(work) > 0 {
		gate := work[len(work)-1]
		work = work[:len(work)-1]
		for _, out := range g.outputs[gate] {
			g.work++
			g.unset[out]--
			if !g.and[out] || g.unset[out] == 0 {
				mark(out)
			}
		}
	}
	reach := map[string]bool{}
	for node, gate := range g.named {
		if g.set[gate] {
			reach[node] = true
		}
	}
	return reach
}

// ScopedAccessWarnings gives one warning for each permission that a read, an update, a patch or a delete of a scoped
// collection decides on its row, when the permission does not need the read permission of the parent through the
// relation of the scope reference: an arrow such as organization->view, or an intersection with one. The handler of
// a scoped collection decides only on the deepest resource of the path (ST-SUP-RC20-OC24, ST-SUP-Q156), so such a
// permission opens the row through the scoped routes to a grant on the row alone; an earlier STEGO also decided the
// read of each ancestor. The warning names the entity, the permission and the parent, and gives the intersection
// that keeps the earlier access (security review of PR #409, M1). A schema that does not compile gives none: the
// checks of the schema refuse it.
func ScopedAccessWarnings(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	if service == nil || schema == nil {
		return nil
	}
	index := map[string]int{}
	for i, e := range service.Entities {
		index[e.Name] = i
	}
	var warnings []ValidationError
	seen := map[string]bool{}
	first := types.FirstCollections(service.Collections)
	for _, c := range service.Collections {
		scope := c.ScopeField()
		ei, ok := index[c.Entity]
		pi, parentOK := index[c.ParentEntity()]
		if scope == "" || !ok || !parentOK {
			continue
		}
		e, p := service.Entities[ei], service.Entities[pi]
		relation := e.References[scope].Relation
		read := p.Permissions[types.OpRead]
		if e.ResourceType == "" || p.ResourceType == "" || relation == "" || read == "" || read == types.PermissionPublic {
			continue
		}
		for _, op := range []types.Operation{types.OpRead, types.OpUpdate, types.OpPatch, types.OpDelete} {
			permission := e.Permissions[op]
			// A permission is checked once for each scope: another scoped collection of the entity may reach it
			// through another relation, from another parent.
			key := strings.Join([]string{e.Name, permission, relation, p.ResourceType, read}, "#")
			if !slices.Contains(c.Operations, op) || permission == "" || permission == types.PermissionPublic || seen[key] {
				continue
			}
			seen[key] = true
			rewrite, isPermission := schema.PermissionRewrite(e.ResourceType, permission)
			g := scopeGuard{schema: schema, resourceType: e.ResourceType, relation: relation, parentType: p.ResourceType, read: read}
			if isPermission && g.guarded(rewrite, 0) {
				continue
			}
			expression := permission
			if isPermission {
				expression = rewriteText(rewrite)
			}
			warnings = append(warnings, ValidationError{Category: "authorization", Rule: "scoped-access", Path: fmt.Sprintf("entities[%d].permissions.%s", ei, op), Found: e.ResourceType + "#" + permission,
				Message: fmt.Sprintf("the permission %s of %s (the %s of collections.%s) does not need the read %s of its parent %s through the relation %s. The routes of collections.%s decide only on the %s (ST-SUP-Q156), so a grant on a %s alone opens it there, also to a caller that may not read the %s; an earlier STEGO also decided the read of the %s. Write it as an intersection, which keeps the access of an earlier STEGO: permission %s = (%s) & %s->%s",
					permission, e.ResourceType, op, c.Name, read, p.Name, relation, c.Name, e.Name, e.ResourceType, p.Name, p.Name, permission, expression, relation, read)})
		}
		warnings = append(warnings, ancestorWarnings(service, schema, c, index, first, seen)...)
	}
	return warnings
}

// ancestorWarnings gives one warning for each ancestor above the parent in the path of the scoped collection c whose
// read does not need the read of the ancestor above it through the relation of its own scope reference. The routes
// of c decide only on the row (ST-SUP-Q156), so the read of the parent stands for the reads of the ancestors above it
// only when each ancestor's read needs the read of the next one: else a grant on the parent alone opens the row to
// a caller that may not read the grandparent, which an earlier STEGO also decided (security review of PR #409). It
// warns only when c serves a route that decides on its row, and once for each pair of ancestors.
func ancestorWarnings(service *types.ServiceDeclaration, schema *AuthzSchema, c types.Collection, index map[string]int,
	first map[string]types.Collection, seen map[string]bool) []ValidationError {
	decides := false
	if ei, ok := index[c.Entity]; ok && service.Entities[ei].ResourceType != "" {
		for _, op := range []types.Operation{types.OpRead, types.OpUpdate, types.OpPatch, types.OpDelete} {
			permission := service.Entities[ei].Permissions[op]
			decides = decides || (slices.Contains(c.Operations, op) && permission != "" && permission != types.PermissionPublic)
		}
	}
	if !decides {
		return nil
	}
	var warnings []ValidationError
	chain := types.ParentChain(c, first)
	for i := 0; i+1 < len(chain); i++ {
		ai, aOK := index[chain[i].Entity]
		gi, gOK := index[chain[i+1].Entity]
		if !aOK || !gOK {
			continue
		}
		a, g := service.Entities[ai], service.Entities[gi]
		relation := a.References[chain[i].Link].Relation
		read, above := a.Permissions[types.OpRead], g.Permissions[types.OpRead]
		if a.ResourceType == "" || g.ResourceType == "" || relation == "" || read == "" || read == types.PermissionPublic || above == "" || above == types.PermissionPublic {
			continue
		}
		key := strings.Join([]string{"ancestor", a.Name, read, relation, g.ResourceType, above}, "#")
		if seen[key] {
			continue
		}
		seen[key] = true
		rewrite, isPermission := schema.PermissionRewrite(a.ResourceType, read)
		guard := scopeGuard{schema: schema, resourceType: a.ResourceType, relation: relation, parentType: g.ResourceType, read: above}
		if isPermission && guard.guarded(rewrite, 0) {
			continue
		}
		expression := read
		if isPermission {
			expression = rewriteText(rewrite)
		}
		warnings = append(warnings, ValidationError{Category: "authorization", Rule: "scoped-access", Path: fmt.Sprintf("entities[%d].permissions.%s", ai, types.OpRead), Found: a.ResourceType + "#" + read,
			Message: fmt.Sprintf("the read %s of %s, an ancestor in the path of collections.%s, does not need the read %s of its parent %s through the relation %s. The routes of collections.%s decide only on the %s (ST-SUP-Q156), so a grant on a %s alone opens the %s there, also to a caller that may not read the %s; an earlier STEGO also decided the read of each ancestor. Write it as an intersection, which keeps the access of an earlier STEGO: permission %s = (%s) & %s->%s",
				read, a.ResourceType, c.Name, above, g.Name, relation, c.Name, c.Entity, a.ResourceType, c.Entity, g.Name, read, expression, relation, above)})
	}
	return warnings
}

// scopeGuard decides whether a permission of resourceType needs the permission read of its parent of parentType
// through the relation relation.
type scopeGuard struct {
	schema                                   *AuthzSchema
	resourceType, relation, parentType, read string
}

// maxGuardDepth bounds the walk through the permissions of the schema; a deeper permission counts as not guarded.
const maxGuardDepth = 16

// guarded tells whether each way of the expression to grant the permission needs the read of the parent: every
// branch of a union, one branch of an intersection, and the base of an exclusion.
func (g scopeGuard) guarded(rewrite *core.UsersetRewrite, depth int) bool {
	if rewrite == nil || depth > maxGuardDepth {
		return false
	}
	switch {
	case rewrite.GetUnion() != nil:
		children := rewrite.GetUnion().GetChild()
		for _, child := range children {
			if !g.child(child, depth) {
				return false
			}
		}
		return len(children) > 0
	case rewrite.GetIntersection() != nil:
		for _, child := range rewrite.GetIntersection().GetChild() {
			if g.child(child, depth) {
				return true
			}
		}
		return false
	case rewrite.GetExclusion() != nil:
		children := rewrite.GetExclusion().GetChild()
		return len(children) > 0 && g.child(children[0], depth)
	}
	return false
}

// child tells whether one branch needs the read of the parent: an arrow through the relation to a permission of the
// parent that needs its read, another permission of the type that is guarded, or a nested expression that is.
func (g scopeGuard) child(child *core.SetOperation_Child, depth int) bool {
	switch {
	case child.GetTupleToUserset() != nil:
		arrow := child.GetTupleToUserset()
		return arrow.GetTupleset().GetRelation() == g.relation && g.onlyParent() && g.schema.implies(g.parentType, arrow.GetComputedUserset().GetRelation(), g.read, depth+1)
	case child.GetComputedUserset() != nil:
		rewrite, ok := g.schema.PermissionRewrite(g.resourceType, child.GetComputedUserset().GetRelation())
		return ok && g.guarded(rewrite, depth+1)
	case child.GetUsersetRewrite() != nil:
		return g.guarded(child.GetUsersetRewrite(), depth+1)
	}
	return false
}

// onlyParent tells whether the relation admits subjects of the parent type only. An arrow through a relation that
// also admits another type grants through a subject of that type too, which needs no read of the parent.
func (g scopeGuard) onlyParent() bool {
	subjects, ok := g.schema.RelationSubjects(g.resourceType, g.relation)
	if !ok || len(subjects) == 0 {
		return false
	}
	for _, subject := range subjects {
		if subject.Type != g.parentType {
			return false
		}
	}
	return true
}

// implies tells whether each way to hold the permission name of the type also gives the permission read: the same
// name, a name that read takes in a union, or an expression whose every branch read gives too. It is a check of
// the shape of the schema, and it says no when it cannot tell.
func (s *AuthzSchema) implies(typeName, name, read string, depth int) bool {
	if name == read {
		return true
	}
	if depth > maxGuardDepth {
		return false
	}
	readRewrite, ok := s.PermissionRewrite(typeName, read)
	if !ok || readRewrite.GetUnion() == nil {
		return false
	}
	readChildren := readRewrite.GetUnion().GetChild()
	for _, child := range readChildren {
		if child.GetComputedUserset() != nil && child.GetComputedUserset().GetRelation() == name {
			return true
		}
	}
	rewrite, ok := s.PermissionRewrite(typeName, name)
	if !ok || rewrite.GetUnion() == nil {
		return false
	}
	for _, child := range rewrite.GetUnion().GetChild() {
		if !s.readGives(typeName, child, readChildren, depth+1) {
			return false
		}
	}
	return true
}

// readGives tells whether the union readChildren of a read gives the branch child: a relation or a permission that it
// names too, or an arrow of the same relation to a permission that the arrow of the read implies.
func (s *AuthzSchema) readGives(typeName string, child *core.SetOperation_Child, readChildren []*core.SetOperation_Child, depth int) bool {
	switch {
	case child.GetComputedUserset() != nil:
		name := child.GetComputedUserset().GetRelation()
		for _, r := range readChildren {
			if r.GetComputedUserset() != nil && r.GetComputedUserset().GetRelation() == name {
				return true
			}
		}
	case child.GetTupleToUserset() != nil:
		arrow := child.GetTupleToUserset()
		subjects, ok := s.RelationSubjects(typeName, arrow.GetTupleset().GetRelation())
		if !ok || len(subjects) != 1 {
			return false
		}
		for _, r := range readChildren {
			if other := r.GetTupleToUserset(); other != nil && other.GetTupleset().GetRelation() == arrow.GetTupleset().GetRelation() &&
				s.implies(subjects[0].Type, arrow.GetComputedUserset().GetRelation(), other.GetComputedUserset().GetRelation(), depth+1) {
				return true
			}
		}
	}
	return false
}

// rewriteText gives the expression of a permission as the schema writes it, such as owner + organization->edit.
func rewriteText(rewrite *core.UsersetRewrite) string {
	var operator string
	var operation *core.SetOperation
	switch {
	case rewrite.GetUnion() != nil:
		operator, operation = " + ", rewrite.GetUnion()
	case rewrite.GetIntersection() != nil:
		operator, operation = " & ", rewrite.GetIntersection()
	case rewrite.GetExclusion() != nil:
		operator, operation = " - ", rewrite.GetExclusion()
	default:
		return ""
	}
	var parts []string
	for _, child := range operation.GetChild() {
		switch {
		case child.GetComputedUserset() != nil:
			parts = append(parts, child.GetComputedUserset().GetRelation())
		case child.GetTupleToUserset() != nil:
			parts = append(parts, child.GetTupleToUserset().GetTupleset().GetRelation()+"->"+child.GetTupleToUserset().GetComputedUserset().GetRelation())
		case child.GetFunctionedTupleToUserset() != nil:
			arrow, function := child.GetFunctionedTupleToUserset(), "any"
			if arrow.GetFunction() == core.FunctionedTupleToUserset_FUNCTION_ALL {
				function = "all"
			}
			parts = append(parts, arrow.GetTupleset().GetRelation()+"."+function+"("+arrow.GetComputedUserset().GetRelation()+")")
		case child.GetUsersetRewrite() != nil:
			parts = append(parts, "("+rewriteText(child.GetUsersetRewrite())+")")
		case child.GetXNil() != nil:
			parts = append(parts, "nil")
		}
	}
	return strings.Join(parts, operator)
}
