package declaration

import (
	"fmt"

	"github.com/jsell-rh/stego/internal/types"
)

// The authorization of the service's own operations (ST-DECL-UC09): each command, query and worker decides one
// permission of one resource type of the schema, and a command writes only the relations it declares. Each check
// takes the compiled schema, or nil when the schema did not compile; with nil it reports only what is missing.

// operationAuthz is what a command, a query or a worker states for authorization, with its place.
type operationAuthz struct {
	kind, name, path         string
	resourceType, permission string
}

// operationFaults checks that an operation names a resource type and a permission, and that the schema holds both.
func operationFaults(rule string, op operationAuthz, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	missing := func(key, what string) {
		faults = append(faults, ValidationError{Category: "authorization", Rule: rule, Path: op.path + "." + key, Found: op.name,
			Message: fmt.Sprintf("%s %q names no %s; with authorization bound, each %s decides a permission of a resource type of the schema", op.kind, op.name, what, op.kind)})
	}
	if op.resourceType == "" {
		missing("resource_type", "resource type")
	}
	if op.permission == "" {
		missing("permission", "permission")
	}
	if schema == nil {
		return faults
	}
	definition, defined := schema.definitions[op.resourceType]
	switch {
	case op.resourceType != "" && !defined:
		faults = append(faults, ValidationError{Category: "authorization", Rule: rule, Path: op.path + ".resource_type", Found: op.resourceType, Allowed: schema.typeNames(),
			Message: fmt.Sprintf("%s %q names the resource type %q, which the schema %s lacks", op.kind, op.name, op.resourceType, schema.Path)})
	case defined && op.permission != "" && !definition.permissions[op.permission]:
		faults = append(faults, ValidationError{Category: "authorization", Rule: rule, Path: op.path + ".permission", Found: op.permission, Allowed: sortedKeys(definition.permissions),
			Message: fmt.Sprintf("%s %q names the permission %q, which the resource type %s of the schema %s lacks", op.kind, op.name, op.permission, op.resourceType, schema.Path)})
	}
	return faults
}

// validateCommandAuthorization checks that each command names a resource type and a permission that the schema
// holds and a resource member of its request (ST-DECL-INV40). A command whose resource type is the resource type of
// the tenant provisioning is decided on the service-level resource in the tenant of the request (ST-SUP-RC32-OC15):
// it names no resource member, and not the provision or the retire permission, which are decided on that resource
// with no tenant and so are platform-wide (ST-SUP-Q127).
func validateCommandAuthorization(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	var provisioning *types.TenantProvisioning
	if service.Tenancy != nil && service.Tenancy.Provisioning != nil && service.Tenancy.Provisioning.ResourceType != "" {
		provisioning = service.Tenancy.Provisioning
	}
	for i, command := range service.Commands {
		faults = append(faults, operationFaults("command-authorization", operationAuthz{"command", command.Name, commandPath(i), command.ResourceType, command.Permission}, schema)...)
		serviceLevel := provisioning != nil && command.ResourceType == provisioning.ResourceType
		switch {
		case serviceLevel && command.ResourceMember != "":
			faults = append(faults, ValidationError{Category: "authorization", Rule: "command-authorization", Path: commandPath(i) + ".resource_member", Found: command.ResourceMember,
				Message: fmt.Sprintf("command %q names the resource member %q, but its resource type %s is the resource type of the tenant provisioning, so it is decided on the service-level resource %s:%s in the tenant of the request (ST-SUP-RC32-OC15); remove resource_member", command.Name, command.ResourceMember, command.ResourceType, command.ResourceType, service.Name)})
		case !serviceLevel && command.ResourceMember == "":
			faults = append(faults, ValidationError{Category: "authorization", Rule: "command-authorization", Path: commandPath(i) + ".resource_member", Found: command.Name,
				Message: fmt.Sprintf("command %q names no resource member; name the member of its request that holds the ID of the resource the permission is decided on, or, for a command that creates rows with no parent, name the resource type of the tenant provisioning and no resource member", command.Name)})
		}
		if !serviceLevel {
			continue
		}
		faults = append(faults, serviceLevelWrites(service, i, command)...)
		if command.Permission == "" {
			continue
		}
		for _, platform := range []struct{ name, permission string }{{"provision", provisioning.Provision}, {"retire", provisioning.Retire}} {
			if platform.permission != "" && command.Permission == platform.permission {
				faults = append(faults, ValidationError{Category: "authorization", Rule: "command-authorization", Path: commandPath(i) + ".permission", Found: command.Permission,
					Message: fmt.Sprintf("command %q names the permission %q, which tenancy.provisioning.%s also names: a %s is decided on the service-level resource %s:%s with no tenant, but the command is decided on that resource in the tenant of the request (ST-SUP-RC32-OC15), so it needs a permission of its own; declare another permission on %s for the command", command.Name, command.Permission, platform.name, platform.name, command.ResourceType, service.Name, command.ResourceType)})
			}
		}
	}
	return faults
}

// validateCommandRelations checks that each relation in a command's Relationships written is a relation of the
// schema under the resource type of an entity the command writes, and is no relation that the runtime writes alone
// for that entity: the relation of a declared relationship or of a reference. A fill that could relate or unrelate
// such a relation could move an owner after create, or relate a row to a parent with no permission on the parent
// (ST-DECL-INV41, ST-SUP-Q118).
func validateCommandRelations(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	for i, command := range service.Commands {
		written := map[string]bool{}
		runtimeWritten := map[string]string{}
		for _, name := range command.Writes {
			if _, entity := entityIndex(service.Entities, name); entity != nil && entity.ResourceType != "" {
				written[entity.ResourceType] = true
				for relation, why := range RuntimeRelations(*entity) {
					runtimeWritten[entity.ResourceType+"#"+relation] = why
				}
			}
		}
		for ri, relation := range command.RelationshipsWritten {
			message := writtenRelationUnknown(relation, written, schema, "the command writes")
			if why, ok := runtimeWritten[relation]; ok && message == "" {
				message = fmt.Sprintf("which is %s; the runtime writes it alone, so a command may not relate or unrelate it", why)
			}
			if message == "" {
				continue
			}
			faults = append(faults, ValidationError{Category: "authorization", Rule: "command-relations", Path: fmt.Sprintf("%s.relationships_written[%d]", commandPath(i), ri), Found: relation,
				Allowed: sortedKeys(written), Message: fmt.Sprintf("command %q lists %q, %s", command.Name, relation, message)})
		}
	}
	return faults
}

// validateQueryAuthorization checks that each query names a resource type and a permission that the schema holds
// (ST-DECL-INV46). A query with no resource member is a list query, decided per row.
func validateQueryAuthorization(queries []types.Query, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	for i, query := range queries {
		faults = append(faults, operationFaults("query-authorization", operationAuthz{"query", query.Name, queryPath(i), query.ResourceType, query.Permission}, schema)...)
	}
	return faults
}

// validateWorkerAuthorization checks that each worker names a resource type and a permission that the schema holds
// (ST-DECL-INV69). A worker needs its permission on each entity it declares it writes: its handle decides the
// permission on the rows it reads and changes, on the resource type of the row's entity, so the resource type of
// each entity in its writes must hold it too, also for an entity that the worker only creates (least privilege,
// stated in the declaration). A worker that consumes a destination also decides, on each row it touches, the
// permission of each command and worker that stages for that destination, as the staging principal
// (ST-SUP-RP10-LC17), so the resource type of each entity in its writes must hold each such permission too.
func validateWorkerAuthorization(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	var faults []ValidationError
	for i, worker := range service.Workers {
		faults = append(faults, operationFaults("worker-authorization", operationAuthz{"worker", worker.Name, workerPath(i), worker.ResourceType, worker.Permission}, schema)...)
		if schema == nil {
			continue
		}
		needs := workerPermissions(service, worker)
		// A worker decides its permission on each row that it reads too, also of an entity in its reads
		// (decision ST-SUP-Q186 (a)).
		named := append(append([]string(nil), worker.Writes...), worker.Reads...)
		for wi, name := range named {
			list, verb, at := "writes", "writes", wi
			if wi >= len(worker.Writes) {
				list, verb, at = "reads", "reads", wi-len(worker.Writes)
			}
			_, entity := entityIndex(service.Entities, name)
			if entity == nil || entity.ResourceType == "" || entity.ResourceType == worker.ResourceType {
				continue
			}
			definition, defined := schema.definitions[entity.ResourceType]
			if !defined {
				continue
			}
			for _, need := range needs {
				if definition.permissions[need.permission] {
					continue
				}
				faults = append(faults, ValidationError{Category: "authorization", Rule: "worker-authorization", Path: fmt.Sprintf("%s.%s[%d]", workerPath(i), list, at), Found: name, Allowed: sortedKeys(definition.permissions),
					Message: fmt.Sprintf("worker %q declares that it %s the entity %s, whose resource type %s lacks %s %q in the schema %s; a worker needs %s on each entity it declares it %s", worker.Name, verb, name, entity.ResourceType, need.whose, need.permission, schema.Path, need.what, verb)})
			}
		}
	}
	return faults
}

// workerPermission is one permission that a worker decides on each row it touches: its own, or that of a stager of
// the destination it consumes, with the words that name it in a fault.
type workerPermission struct {
	permission, whose, what string
}

// workerPermissions gives the permissions that a worker decides on each row it touches: its own permission, and,
// when it consumes a destination, the permission of each other command and worker that stages an effect for that
// destination (ST-SUP-RP10-LC17), each once, in declaration order.
func workerPermissions(service *types.ServiceDeclaration, worker types.Worker) []workerPermission {
	var needs []workerPermission
	seen := map[string]bool{}
	add := func(need workerPermission) {
		if need.permission != "" && !seen[need.permission] {
			seen[need.permission] = true
			needs = append(needs, need)
		}
	}
	add(workerPermission{worker.Permission, "the worker's permission", "its permission"})
	trigger, reason := parseTrigger(worker.Trigger)
	if reason != "" || trigger.consumes == "" {
		return needs
	}
	stages := func(effects []types.Effect) bool {
		for _, effect := range effects {
			if effect.Destination == trigger.consumes {
				return true
			}
		}
		return false
	}
	stager := func(kind, name, permission string) workerPermission {
		return workerPermission{permission, fmt.Sprintf("the permission of the %s %q, which stages for %s", kind, name, trigger.consumes),
			"the permission of each stager of the destination it consumes, which it decides for the staging principal,"}
	}
	for _, command := range service.Commands {
		if stages(command.Effects) {
			add(stager("command", command.Name, command.Permission))
		}
	}
	for _, other := range service.Workers {
		if other.Name != worker.Name && stages(other.Effects) {
			add(stager("worker", other.Name, other.Permission))
		}
	}
	return needs
}

// operationsAuthorization runs the authorization checks of the commands, queries and workers.
func operationsAuthorization(service *types.ServiceDeclaration, schema *AuthzSchema) []ValidationError {
	faults := validateCommandAuthorization(service, schema)
	if schema != nil {
		faults = append(faults, validateCommandRelations(service, schema)...)
	}
	faults = append(faults, validateQueryAuthorization(service.Queries, schema)...)
	faults = append(faults, validateReferences(service.Entities, schema)...)
	faults = append(faults, validateWithin(service.Entities, service.Collections)...)
	return append(faults, validateWorkerAuthorization(service, schema)...)
}

// TenantResourceType is the resource type of the tenant resource, on which provisioning writes the tuple of each
// worker's relation (ST-SUP-RC35-OC13); TenantRelation is the relation of a row to its tenant, which the runtime
// writes on create; and WorkerRelationPrefix starts the relation of each worker on the tenant resource
// (ST-SUP-Q132). No entity takes the resource type, and no command or slot binding writes these relations.
const (
	TenantResourceType   = "tenant"
	TenantRelation       = "tenant"
	WorkerRelationPrefix = "worker_"
)

// RuntimeRelations gives the relations that the runtime writes alone for the rows of the entity, each with what
// declares it: the relation of each declared relationship, of the subject source field or caller, the relation
// of each reference (ST-DECL-EN02, ST-DECL-EN12), and the relation tenant of the row to its tenant (ST-SUP-Q132). No command lists one of them in Relationships written
// (ST-DECL-INV41).
func RuntimeRelations(e types.Entity) map[string]string {
	relations := map[string]string{}
	// The relation of each row to its tenant, which the runtime writes on create when the schema gives the resource
	// type the relation tenant of the subject type tenant (ST-SUP-RP10-LC05, ST-SUP-Q132).
	if e.ResourceType != "" {
		relations[TenantRelation] = fmt.Sprintf("the relation of each row of the entity %s to its tenant", e.Name)
	}
	for _, r := range e.Relationships {
		if r.Relation != "" && relations[r.Relation] == "" {
			relations[r.Relation] = fmt.Sprintf("the relationship %s of the entity %s, with the subject source %s", r.Relation, e.Name, r.SubjectSource)
		}
	}
	for _, field := range sortedKeys(e.References) {
		if ref := e.References[field]; ref.Relation != "" && relations[ref.Relation] == "" {
			relations[ref.Relation] = fmt.Sprintf("the relation of the reference %s of the entity %s", field, e.Name)
		}
	}
	return relations
}

// serviceLevelWrites refuses what a command on the service-level resource cannot do, as the generator does
// (ST-SUP-RC32-OC15): its handle offers only the create and the read, so it writes no relation of its own, and it
// creates only rows with no parent, so it writes no entity that takes its authorization from a referenced row and no
// entity that a scoped collection puts under a parent.
func serviceLevelWrites(service *types.ServiceDeclaration, i int, command types.Command) []ValidationError {
	var faults []ValidationError
	if len(command.RelationshipsWritten) > 0 {
		faults = append(faults, ValidationError{Category: "authorization", Rule: "command-authorization", Path: commandPath(i) + ".relationships_written", Found: command.RelationshipsWritten[0],
			Message: fmt.Sprintf("command %q is decided on the service-level resource, and its handle offers only the create and the read, so it writes no relation of its own (ST-SUP-RC32-OC15); remove relationships_written", command.Name)})
	}
	for wi, name := range command.Writes {
		_, entity := entityIndex(service.Entities, name)
		if entity == nil {
			continue
		}
		parent := ""
		if entity.ResourceType == "" {
			for _, field := range entity.Fields {
				if field.Type != types.FieldTypeRef {
					continue
				}
				if _, to := entityIndex(service.Entities, field.To); to != nil && to.ResourceType != "" {
					parent = "it has no resource type and takes its authorization from the " + to.Name + " that its field " + field.Name + " names"
					break
				}
			}
		}
		for _, collection := range service.Collections {
			if parent == "" && collection.Entity == name && len(collection.Scope) > 0 {
				parent = "the collection " + collection.Name + " puts it under a parent"
			}
		}
		if parent != "" {
			faults = append(faults, ValidationError{Category: "authorization", Rule: "command-authorization", Path: fmt.Sprintf("%s.writes[%d]", commandPath(i), wi), Found: name,
				Message: fmt.Sprintf("command %q is decided on the service-level resource and writes %s, but %s; such a command creates only rows with no parent (ST-SUP-RC32-OC15)", command.Name, name, parent)})
		}
	}
	return faults
}
