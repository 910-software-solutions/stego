package declaration

import (
	"strings"
	"testing"
	"testing/fstest"
)

// operationFacts are the facts of the TD03 operations: a fill of the command slot, of the query slot and of the
// worker slot, and the outbox destination billing that an active component declares.
func operationFacts() Facts {
	return Facts{
		FillSlots:    map[string]string{"reserve-and-charge": CommandSlot, "open-reservations": QuerySlot, "sync": WorkerSlot, "apply": WorkerSlot},
		Destinations: map[string]bool{"billing": true},
	}
}

// checkOperations runs checking in process on a declaration of ST-DECL-TD03 as stego validate does: the
// declaration's own checks, its composition with the facts given, and its authorization against the fixture
// schema. It gives every fault.
func checkOperations(t *testing.T, service string, facts Facts) []ValidationError {
	t.Helper()
	_, schema := td03Files(t)
	decl := readTD03(t, service)
	faults := Validate(decl).All()
	faults = append(faults, ValidateComposition(decl, facts)...)
	return append(faults, CheckAuthorization(decl, fstest.MapFS{"authz/schema.zed": {Data: []byte(schema)}})...)
}

// expectNamed expects exactly one fault of the rule, and that its value found, path or message names each text.
func expectNamed(t *testing.T, faults []ValidationError, rule string, texts ...string) {
	t.Helper()
	expectOneFault(t, faults, rule, texts...)
}

// td03Command is ST-DECL-TD03 with one command that writes Todo, bound to the fill reserve-and-charge, with the
// lines given.
func td03Command(t *testing.T, lines ...string) string {
	service, _ := td03Files(t)
	return service + "commands:\n" + standardCommand("approve", lines...)
}

func TestCommandAuthorization(t *testing.T) {
	full := []string{"resource_type: document", "permission: edit", "resource_member: documentId"}
	without := func(drop string) []string {
		var kept []string
		for _, line := range full {
			if !strings.HasPrefix(line, drop+":") {
				kept = append(kept, line)
			}
		}
		return kept
	}
	t.Run("ST-DECL-INV40-T A command on document with edit and a resource member is accepted", func(t *testing.T) {
		expectAccepted(t, checkOperations(t, td03Command(t, full...), operationFacts()))
	})
	t.Run("ST-DECL-INV40-T A command with no permission is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Command(t, without("permission")...), operationFacts()), "command-authorization", "approve", "no permission")
	})
	t.Run("ST-DECL-INV40-T A command with no resource type is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Command(t, without("resource_type")...), operationFacts()), "command-authorization", "approve", "no resource type")
	})
	t.Run("ST-DECL-INV40-T A command with no resource member is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Command(t, without("resource_member")...), operationFacts()), "command-authorization", "approve", "no resource member")
	})
	t.Run("ST-DECL-INV40-T A command with the permission approve, which the schema lacks, is refused", func(t *testing.T) {
		lines := append(without("permission"), "permission: approve")
		expectNamed(t, checkOperations(t, td03Command(t, lines...), operationFacts()), "command-authorization", "approve", "permission \"approve\"")
	})

	// A command on the resource type of the tenant provisioning is decided on the service-level resource in the
	// tenant of the request (ST-SUP-RC32-OC15), so it names no resource member.
	serviceLevel := func(t *testing.T, lines ...string) []ValidationError {
		t.Helper()
		_, schema := td03Files(t)
		schema = change(t, schema, "permission retire = operator", "permission retire = operator\n\tpermission record = operator")
		decl := readTD03(t, td03Command(t, lines...))
		faults := Validate(decl).All()
		faults = append(faults, ValidateComposition(decl, operationFacts())...)
		return append(faults, CheckAuthorization(decl, fstest.MapFS{"authz/schema.zed": {Data: []byte(schema)}})...)
	}
	t.Run("ST-DECL-INV40-T A command on the service-level resource with no resource member is accepted", func(t *testing.T) {
		expectAccepted(t, serviceLevel(t, "resource_type: service", "permission: record"))
	})
	t.Run("ST-DECL-INV40-T A command on the service-level resource with a resource member is refused", func(t *testing.T) {
		expectNamed(t, serviceLevel(t, "resource_type: service", "permission: record", "resource_member: documentId"), "command-authorization", "approve", "documentId", "service-level resource", "remove resource_member")
	})
	t.Run("ST-DECL-INV40-T A command on the service-level resource with the provision permission is refused", func(t *testing.T) {
		expectNamed(t, serviceLevel(t, "resource_type: service", "permission: provision"), "command-authorization", "approve", "tenancy.provisioning.provision", "permission of its own")
	})
	t.Run("ST-DECL-INV40-T A command on the service-level resource with the retire permission is refused", func(t *testing.T) {
		expectNamed(t, serviceLevel(t, "resource_type: service", "permission: retire"), "command-authorization", "approve", "tenancy.provisioning.retire", "permission of its own")
	})
	t.Run("ST-DECL-INV40-T A command on service with no tenant provisioning names a resource member", func(t *testing.T) {
		service, schema := td03Files(t)
		service = change(t, service, "  provisioning: {resource_type: service, provision: provision, retire: retire}\n", "")
		schema = change(t, schema, "permission retire = operator", "permission retire = operator\n\tpermission record = operator")
		decl := readTD03(t, service+"commands:\n"+standardCommand("approve", "resource_type: service", "permission: record"))
		faults := CheckAuthorization(decl, fstest.MapFS{"authz/schema.zed": {Data: []byte(schema)}})
		var named bool
		for _, f := range faults {
			named = named || (f.Rule == "command-authorization" && strings.Contains(f.Message, "no resource member"))
		}
		if !named {
			t.Fatalf("want the fault command-authorization for no resource member, got %+v", faults)
		}
	})

	relations := func(relation string) string {
		return td03Command(t, append(full, "relationships_written: ["+relation+"]")...)
	}
	t.Run("ST-DECL-INV41-T A command that writes document#viewer is accepted", func(t *testing.T) {
		expectAccepted(t, checkOperations(t, relations("document#viewer"), operationFacts()))
	})
	t.Run("ST-DECL-INV41-T A command that writes document#approver, which the schema lacks, is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, relations("document#approver"), operationFacts()), "command-relations", "approve", "document#approver", "lacks the relation approver")
	})
	t.Run("ST-DECL-INV41-T A command that writes folder#viewer, of no entity the command writes, is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, relations("folder#viewer"), operationFacts()), "command-relations", "approve", "folder#viewer", "no entity the command writes")
	})
	// The runtime writes the relation of a declared relationship and of a reference alone: a command that could relate
	// or unrelate it could move an owner after create, or relate a row to a parent with no permission on the parent
	// (ST-SUP-Q118).
	t.Run("ST-DECL-INV41-T A command that writes document#owner, the caller relationship of Todo, is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, relations("document#owner"), operationFacts()), "command-relations", "approve", "document#owner", "relationship owner of the entity Todo", "runtime writes it alone")
	})
	t.Run("ST-DECL-INV41-T A command that writes document#viewer, a field relationship of Todo, is refused", func(t *testing.T) {
		service := change(t, relations("document#viewer"), "      - {relation: owner, subject_source: caller, subject_type: user}\n",
			"      - {relation: owner, subject_source: caller, subject_type: user}\n      - {relation: viewer, subject_source: field, subject_field: owner_id, subject_type: user}\n")
		expectNamed(t, checkOperations(t, service, operationFacts()), "command-relations", "approve", "document#viewer", "relationship viewer of the entity Todo, with the subject source field", "runtime writes it alone")
	})
	t.Run("ST-DECL-INV41-T A command that writes order#customer, the relation of a reference of Order, is refused", func(t *testing.T) {
		decl := readTD03(t, referencesService+"commands:\n"+command("move", "writes: [Order]", "relationships_written: [order#customer, order#owner]"))
		faults := validateCommandRelations(decl, compileTD03(t, referencesSchema))
		expectNamed(t, faults, "command-relations", "move", "order#customer", "reference customer_id of the entity Order", "runtime writes it alone")
	})
}

// td03Query is ST-DECL-TD03 with one query that reads Todo, bound to the fill open-reservations, with the lines
// given.
func td03Query(t *testing.T, lines ...string) string {
	service, _ := td03Files(t)
	return service + "queries:\n" + standardQuery("open-documents", lines...)
}

func TestQueryAuthorization(t *testing.T) {
	t.Run("ST-DECL-INV46-T A query on document with view is accepted", func(t *testing.T) {
		expectAccepted(t, checkOperations(t, td03Query(t, "resource_type: document", "permission: view", "resource_member: documentId"), operationFacts()))
	})
	t.Run("ST-DECL-INV46-T A query with no permission is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Query(t, "resource_type: document"), operationFacts()), "query-authorization", "open-documents", "no permission")
	})
	t.Run("ST-DECL-INV46-T A query with no resource type is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Query(t, "permission: view"), operationFacts()), "query-authorization", "open-documents", "no resource type")
	})
	t.Run("ST-DECL-INV46-T A query with the permission audit, which the schema lacks, is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Query(t, "resource_type: document", "permission: audit"), operationFacts()), "query-authorization", "open-documents", "permission \"audit\"")
	})
	t.Run("ST-DECL-INV46-T A list query with view and no resource member is accepted", func(t *testing.T) {
		expectAccepted(t, checkOperations(t, td03Query(t, "resource_type: document", "permission: view"), operationFacts()))
	})
}

// td03Worker is ST-DECL-TD03 with the modes api and worker and the worker sync-documents with the trigger every
// 1m that writes Todo, bound to the fill sync, with the lines given.
func td03Worker(t *testing.T, lines ...string) string {
	service, _ := td03Files(t)
	return service + "modes: [api, worker]\nworkers:\n" + worker("sync-documents", append([]string{"trigger: every 1m", "writes: [Todo]", "fill: sync"}, lines...)...)
}

func TestWorkerAuthorization(t *testing.T) {
	t.Run("ST-DECL-INV69-T The worker sync-documents on document with view is accepted", func(t *testing.T) {
		expectAccepted(t, checkOperations(t, td03Worker(t, "resource_type: document", "permission: view"), operationFacts()))
	})
	t.Run("ST-DECL-INV69-T A worker with no permission is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Worker(t, "resource_type: document"), operationFacts()), "worker-authorization", "sync-documents", "no permission")
	})
	t.Run("ST-DECL-INV69-T A worker with no resource type is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Worker(t, "permission: view"), operationFacts()), "worker-authorization", "sync-documents", "no resource type")
	})
	t.Run("ST-DECL-INV69-T A worker with the permission approve, which the schema lacks, is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Worker(t, "resource_type: document", "permission: approve"), operationFacts()), "worker-authorization", "sync-documents", "permission \"approve\"")
	})
	t.Run("ST-DECL-INV69-T A worker with the resource type invoice, which the schema lacks, is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Worker(t, "resource_type: invoice", "permission: view"), operationFacts()), "worker-authorization", "sync-documents", "resource type \"invoice\"")
	})
	// A worker needs its permission on each entity it declares it writes, on the resource type of the entity (docs1
	// review of PR #396; security review of PR #402, M1).
	noteService := func(workers, commands string) string {
		service, _ := td03Files(t)
		service = change(t, service, "collections:\n", "  - name: Note\n    fields:\n      - {name: text, type: string}\n    resource_type: note\n"+
			"    public_reasons: {create: a new note has no resource to decide on before it exists}\n"+
			"    permissions: {create: public, read: edit, update: edit, delete: edit, list: edit}\n"+
			"    relationships:\n      - {relation: owner, subject_source: caller, subject_type: user}\n"+
			"collections:\n  notes:\n    entity: Note\n    operations: [create, read, list]\n")
		service += "modes: [api, worker]\nworkers:\n" + workers
		if commands != "" {
			service += "commands:\n" + commands
		}
		return service
	}
	withNote := func(service, permissions string) []ValidationError {
		_, schema := td03Files(t)
		schema += "\ndefinition note {\n\trelation owner: user | service_account\n" + permissions + "}\n"
		decl := readTD03(t, service)
		faults := Validate(decl).All()
		faults = append(faults, ValidateComposition(decl, operationFacts())...)
		return append(faults, CheckAuthorization(decl, fstest.MapFS{"authz/schema.zed": {Data: []byte(schema)}})...)
	}
	editOnly := "\tpermission edit = owner\n"
	written := func(permission string) []ValidationError {
		return withNote(noteService(worker("sync-documents", "trigger: every 1m", "writes: [Todo, Note]", "fill: sync", "resource_type: document", "permission: "+permission), ""), editOnly)
	}
	onlyAt := func(t *testing.T, faults []ValidationError, path string) {
		t.Helper()
		for _, fault := range faults {
			if fault.Rule == "worker-authorization" && fault.Path != path {
				t.Errorf("the fault is at %s, want %s", fault.Path, path)
			}
		}
	}
	t.Run("ST-DECL-INV69-T A worker that writes Note, whose resource type note lacks its permission view, is refused", func(t *testing.T) {
		faults := written("view")
		expectNamed(t, faults, "worker-authorization", "sync-documents", "Note", "note", "\"view\"", "needs its permission on each entity it declares it writes")
		onlyAt(t, faults, "workers[0].writes[1]")
		for _, fault := range faults {
			if strings.Contains(fault.Message, "refuse") {
				t.Errorf("the fault says the runtime refuses each row, which is not so for a create: %s", fault.Message)
			}
		}
	})
	t.Run("ST-DECL-INV69-T A worker that writes Note with the permission edit, which document and note hold, is accepted", func(t *testing.T) {
		expectAccepted(t, written("edit"))
	})
	// A consuming worker also decides the stager's permission on each row it touches (security review of PR #402, L1).
	consumer := worker("apply-document-replies", "trigger: consumes document-replies", "writes: [Todo, Note]", "fill: apply", "resource_type: document", "permission: edit")
	stager := standardCommand("reply", "resource_type: document", "permission: view", "resource_member: documentId", "effects:\n      - {destination: document-replies, name: reply}")
	t.Run("ST-DECL-INV69-T A consuming worker that writes Note, whose resource type note lacks the permission view of its stager reply, is refused", func(t *testing.T) {
		faults := withNote(noteService(consumer, stager), editOnly)
		expectNamed(t, faults, "worker-authorization", "apply-document-replies", "Note", "note", "\"view\"", "\"reply\"", "document-replies")
		onlyAt(t, faults, "workers[0].writes[1]")
	})
	t.Run("ST-DECL-INV69-T A consuming worker that writes Note, whose resource type note lacks the permission view of its stager worker sync-documents, is refused", func(t *testing.T) {
		stagingWorker := worker("sync-documents", "trigger: every 1m", "writes: [Todo]", "fill: sync", "resource_type: document", "permission: view", "effects:\n      - {destination: document-replies, name: reply}")
		faults := withNote(noteService(consumer+stagingWorker, ""), editOnly)
		expectNamed(t, faults, "worker-authorization", "apply-document-replies", "Note", "note", "\"view\"", "\"sync-documents\"", "document-replies")
		onlyAt(t, faults, "workers[0].writes[1]")
	})
	t.Run("ST-DECL-INV69-T A consuming worker that writes Note, whose resource type note holds its own permission and its stager's, is accepted", func(t *testing.T) {
		expectAccepted(t, withNote(noteService(consumer, stager), editOnly+"\tpermission view = owner\n"))
	})
}

// replyWorker is the worker apply-document-replies with the trigger consumes document-replies that writes Todo on
// the resource type document with the permission edit, bound to the fill apply.
func replyWorker() string {
	return worker("apply-document-replies", "trigger: consumes document-replies", "writes: [Todo]", "fill: apply", "resource_type: document", "permission: edit")
}

// td03Staging is ST-DECL-TD03 with the modes api and worker, the worker apply-document-replies and the items given.
func td03Staging(t *testing.T, workers string, commands string) string {
	service, _ := td03Files(t)
	service += "modes: [api, worker]\nworkers:\n" + replyWorker() + workers
	if commands != "" {
		service += "commands:\n" + commands
	}
	return service
}

func TestTenantResourceReserved(t *testing.T) {
	t.Run("ST-SUP-RC35-OC13-T04 No entity takes the resource type tenant, and no command or slot writes the tenant relation or a worker relation", func(t *testing.T) {
		service, schema := td03Files(t)
		// An entity of the resource type tenant is refused, also when the schema defines it.
		withTenant := schema + "\ndefinition tenant {\n\trelation worker_sync: service_account\n\tpermission view = worker_sync\n\tpermission edit = worker_sync\n}\n"
		decl := readTD03(t, change(t, service, "    resource_type: document", "    resource_type: tenant"))
		var reserved []ValidationError
		for _, fault := range validateSchemaNames(decl, compileTD03(t, withTenant)) {
			if strings.Contains(fault.Message, "reserved for the tenant resource") {
				reserved = append(reserved, fault)
			}
		}
		if len(reserved) != 1 || reserved[0].Path != "entities[0].resource_type" || reserved[0].Found != "tenant" {
			t.Fatalf("an entity of the resource type tenant gave %+v, want one fault on entities[0].resource_type", reserved)
		}
		// A command may write neither the relation tenant of its rows, nor a worker relation, nor a relation of the
		// tenant resource.
		relations := func(relation string) string {
			return td03Command(t, "resource_type: document", "permission: edit", "resource_member: documentId", "relationships_written: ["+relation+"]")
		}
		for relation, text := range map[string]string{
			"document#tenant":      "relation of each row to its tenant",
			"document#worker_sync": "holds the grant of a worker",
			"tenant#worker_sync":   "is the tenant resource",
		} {
			expectNamed(t, checkOperations(t, relations(relation), operationFacts()), "command-relations", "approve", relation, text)
		}
		// The same check holds for a slot binding, and RuntimeRelations names the relation tenant of each entity
		// with a resource type, so the command generator refuses it too.
		declared := map[string]bool{"document": true, "tenant": true}
		compiled := compileTD03(t, withTenant)
		for _, relation := range []string{"document#tenant", "document#worker_sync", "tenant#worker_sync"} {
			if writtenRelationUnknown(relation, declared, compiled, "the service declares") == "" {
				t.Errorf("a slot binding may write %s", relation)
			}
		}
		if why := RuntimeRelations(decl.Entities[0])["tenant"]; why == "" {
			t.Error("RuntimeRelations does not name the relation tenant of an entity with a resource type")
		}
	})
}
