package compiler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jsell-rh/stego/internal/buildidentity"
	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/command"
	"github.com/jsell-rh/stego/internal/generator/query"
	"github.com/jsell-rh/stego/internal/generator/restapi"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

// archFixtureSchema holds every kind of grant path: an arrow, a userset, a caveat, an intersection, an exclusion, a
// wildcard, and two recursions (group#member through itself, and folder#view through parent->view).
const archFixtureSchema = `definition user {}

caveat ip_allowed(ip ipaddress, cidr string) {
	ip.in_cidr(cidr)
}

definition group {
	relation member: user | group#member
}

definition folder {
	relation parent: folder
	relation viewer: user | user:* | group#member
	relation banned: user
	relation editor: user with ip_allowed
	relation auditor: user
	permission view = (viewer + parent->view) - banned
	permission edit = editor & auditor
}

definition doc {
	relation folder: folder
	relation owner: user
	permission read = folder->view
	permission write = folder->edit + owner
}
`

// archComponent builds a component with its role; slots are name:phase:operations.
func archComponent(name, layer string, requires, provides []string, slots ...registry.SlotDefinition) *registry.Component {
	c := &registry.Component{Kind: "component", Name: name, Version: "1.0.0", Stage: "ga", OutputNamespace: "internal/" + name, Slots: slots,
		Architecture: &registry.ArchitectureRole{Layer: layer, Flow: "The part of " + name + ".", Ports: map[string]string{}}}
	for _, p := range requires {
		c.Requires = append(c.Requires, registry.Port{Name: p})
		c.Architecture.Ports[p] = "outbound"
	}
	for _, p := range provides {
		c.Provides = append(c.Provides, registry.Port{Name: p})
		c.Architecture.Ports[p] = "inbound"
	}
	return c
}

// archFixture gives the inputs of a small service: a handler rest-api that serves the collection docs, an auth provider,
// a store, an outbox and an event publisher, the slots before_create and after_create, a command, and the schema
// above.
func archFixture(t *testing.T) ArchitectureInput {
	t.Helper()
	api := archComponent("rest-api", "handler", []string{"auth-provider", "storage-adapter"}, []string{"http-server"},
		registry.SlotDefinition{Name: "before_create", Phase: "before_store", Operations: []string{"create"}},
		registry.SlotDefinition{Name: "after_create", Phase: "after_store", Operations: []string{"create"}},
		registry.SlotDefinition{Name: "validate", Phase: "before_store", Operations: []string{"create", "update"}})
	api.Config = map[string]types.ConfigField{"expose": {Type: "list"}}
	// rest-api serves the collection operations, and the capability components serve the commands, queries and
	// workers, so a test can add a query or a worker (ST-GEN-Q44 (A), ST-GEN-Q45 (b)).
	api.Architecture.Serves = []string{"collection operations"}
	capability := func(name, kind string) *registry.Component {
		c := archComponent(name, "handler", []string{"storage-adapter"}, nil)
		c.Architecture.Serves = []string{kind}
		return c
	}
	store := archComponent("store", "repository", nil, []string{"storage-adapter"})
	store.Architecture.Store = "DocsDB"
	components := map[string]*registry.Component{
		"rest-api": api,
		"auth":     archComponent("auth", "authentication", nil, []string{"auth-provider"}),
		"store":    store,
		"command":  capability("command", "commands"),
		"query":    capability("query", "queries"),
		"worker":   capability("worker", "workers"),
		"outbox":   archComponent("outbox", "repository", nil, []string{"durable-outbox"}),
		"events":   archComponent("events", "outside call", []string{"durable-outbox"}, []string{"event-publisher"}),
	}
	schema, err := declaration.CompileAuthzSchema("authz/schema.zed", []byte(archFixtureSchema))
	if err != nil {
		t.Fatal(err)
	}
	service := &types.ServiceDeclaration{Kind: "service", Name: "docs", Archetype: "arch", Language: "go", BasePath: "/v1",
		Authorization: &types.Authorization{Provider: "spicedb", SchemaPath: "authz/schema.zed", SubjectFromIdentity: "user"},
		Overrides:     map[string]any{"storage-adapter": "store"},
		Entities: []types.Entity{{Name: "Doc", Fields: []types.Field{{Name: "title", Type: "string"}, {Name: "email", Type: "string", Sensitivity: "pii"}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "doc", Permissions: map[types.Operation]string{"create": "public", "read": "read", "list": "read", "update": "write"},
				PublicReasons: map[types.Operation]string{"create": "a new doc has no resource before it exists"}}}},
		Collections: []types.Collection{{Name: "docs", Entity: "Doc", Operations: []types.Operation{"create", "read", "update"}}},
		Slots: []types.SlotDeclaration{
			{Slot: "after_create", Collection: "docs", FanOut: []string{"notify"}},
			{Slot: "before_create", Collection: "docs", Gate: []string{"policy-a", "policy-b"}},
		},
		Commands: []types.Command{{Name: "archive", ResourceType: "doc", Permission: "write", Fill: types.FillNames{"archiver"}}},
	}
	wiring := &gen.Wiring{Constructors: []string{"api.NewDocsHandler(store)"}, ConstructorCollections: map[int]string{0: "docs"},
		Routes: []string{`mux.HandleFunc("POST /v1/docs", docsHandler.Create)`, `mux.HandleFunc("GET /v1/docs/{id}", docsHandler.Read)`}}
	return ArchitectureInput{Service: service, Archetype: &registry.Archetype{Name: "arch", Version: "2.0.0", Bindings: map[string]string{"auth-provider": "auth"}},
		Components: components, Names: []string{"rest-api", "auth", "store", "outbox", "events", "command", "query", "worker"}, Wirings: []ComponentWiring{{Name: "rest-api", Wiring: wiring}}, Schema: schema}
}

// nestedFixtureSchema is the schema of archFixture with a shelf, whose keeper may view it.
const nestedFixtureSchema = archFixtureSchema + `
definition shelf {
	relation keeper: user
	permission view = keeper
}
`

// nestedFixture gives the inputs of archFixture with a chain of three levels: the shelves, whose read permission is
// view, the folders of a shelf, whose read permission is view, and the docs of a folder (ST-SUP-RC20-OC24).
func nestedFixture(t *testing.T) ArchitectureInput {
	t.Helper()
	in := archFixture(t)
	schema, err := declaration.CompileAuthzSchema("authz/schema.zed", []byte(nestedFixtureSchema))
	if err != nil {
		t.Fatal(err)
	}
	in.Schema = schema
	svc := in.Service
	svc.Entities = append(svc.Entities[:1:1],
		types.Entity{Name: "Folder", Fields: []types.Field{{Name: "name", Type: "string"}, {Name: "shelf_id", Type: types.FieldTypeRef, To: "Shelf"}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "folder", Permissions: map[types.Operation]string{"read": "view", "list": "view"}}},
		types.Entity{Name: "Shelf", Fields: []types.Field{{Name: "name", Type: "string"}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "shelf", Permissions: map[types.Operation]string{"read": "view", "list": "view"}}})
	svc.Entities[0].Fields = append(slices.Clone(svc.Entities[0].Fields), types.Field{Name: "folder_id", Type: types.FieldTypeRef, To: "Folder"})
	svc.Collections = []types.Collection{
		{Name: "docs", Entity: "Doc", Operations: []types.Operation{"create", "read", "update"}},
		{Name: "shelves", Entity: "Shelf", Operations: []types.Operation{"read"}},
		{Name: "shelf-folders", Entity: "Folder", Scope: map[string]string{"shelf_id": "Shelf"}, Operations: []types.Operation{"read", "list"}},
		{Name: "folder-docs", Entity: "Doc", Scope: map[string]string{"folder_id": "Folder"}, Operations: []types.Operation{"read", "update", "list"}},
	}
	return in
}

// chainSchema gives a schema whose doc permissions read and write each pass a chain of n permission rewrites before
// they reach the relation owner, or grant nothing (nil) when n is 0.
func chainSchema(n int) string {
	var b strings.Builder
	b.WriteString("definition user {}\n\ndefinition doc {\n\trelation owner: user\n")
	if n == 0 {
		b.WriteString("\tpermission read = nil\n\tpermission write = nil\n}\n")
		return b.String()
	}
	b.WriteString("\tpermission read = step1\n\tpermission write = step1\n")
	for i := 1; i < n; i++ {
		fmt.Fprintf(&b, "\tpermission step%d = step%d\n", i, i+1)
	}
	fmt.Fprintf(&b, "\tpermission step%d = owner\n}\n", n)
	return b.String()
}

// chainFixture gives the inputs of archFixture with the schema chainSchema(n): with n above the depth limit, every
// grant path of every checked permission stops at the limit before it reaches a role (security review of PR #384,
// L1).
func chainFixture(t *testing.T, n int) ArchitectureInput {
	t.Helper()
	in := archFixture(t)
	schema, err := declaration.CompileAuthzSchema("authz/schema.zed", []byte(chainSchema(n)))
	if err != nil {
		t.Fatal(err)
	}
	in.Schema = schema
	return in
}

// workFixture gives the inputs of archFixture with the schema wideSchema(40, "nil") and a doc whose permissions read
// and write each pass an arrow to node0#view: the work limit stops the expansion of each before a path reaches a
// role, and no depth limit is reached (security review of PR #385, L2).
func workFixture(t *testing.T) ArchitectureInput {
	t.Helper()
	in := archFixture(t)
	schema, err := declaration.CompileAuthzSchema("authz/schema.zed", []byte(wideSchema(40, "nil")+
		"\ndefinition doc {\n\trelation root: node0\n\tpermission read = root->view\n\tpermission write = root->view\n}\n"))
	if err != nil {
		t.Fatal(err)
	}
	in.Schema = schema
	return in
}

// pathFixture gives the inputs of archFixture with a doc whose permissions read and write each pass one of 30
// arrows to hub#mid, and hub#mid one of 30 arrows back to doc#read: each of the 900 paths is a recursion, a path with
// no role, so the path limit of 512 paths stops the expansion of each before a path reaches a role and before the
// work limit (security review of PR #388, L1).
func pathFixture(t *testing.T) ArchitectureInput {
	t.Helper()
	var hub, doc strings.Builder
	var toHub, toDoc []string
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&hub, "\trelation back%d: doc\n", i)
		fmt.Fprintf(&doc, "\trelation hub%d: hub\n", i)
		toDoc = append(toDoc, fmt.Sprintf("back%d->read", i))
		toHub = append(toHub, fmt.Sprintf("hub%d->mid", i))
	}
	schemaText := "definition user {}\n\ndefinition hub {\n" + hub.String() + "\tpermission mid = " + strings.Join(toDoc, " + ") + "\n}\n\n" +
		"definition doc {\n" + doc.String() + "\tpermission read = " + strings.Join(toHub, " + ") + "\n\tpermission write = " + strings.Join(toHub, " + ") + "\n}\n"
	in := archFixture(t)
	schema, err := declaration.CompileAuthzSchema("authz/schema.zed", []byte(schemaText))
	if err != nil {
		t.Fatal(err)
	}
	in.Schema = schema
	return in
}

// withoutPaths gives the model data with no grant path in each named permission, which keeps its key complete false:
// a model, as a reader can edit it, whose limit names no path that stops.
func withoutPaths(t *testing.T, data []byte, ids ...string) []byte {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, perm := range raw["access"].(map[string]any)["permissions"].([]any) {
		obj := perm.(map[string]any)
		if slices.Contains(ids, obj["id"].(string)) {
			obj["paths"] = []any{}
		}
	}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

func deriveFixture(t *testing.T, in ArchitectureInput) (*ArchitectureModel, []byte) {
	t.Helper()
	files, err := WriteArchitecture(in)
	if err != nil {
		t.Fatal(err)
	}
	var model ArchitectureModel
	if err := json.Unmarshal(files[0].Content, &model); err != nil {
		t.Fatal(err)
	}
	return &model, files[0].Content
}

func TestArchitectureDeterminism(t *testing.T) {
	t.Run("ST-GEN-DS04-T02 The same inputs always give the same architecture model", func(t *testing.T) {
		_, first := deriveFixture(t, archFixture(t))
		for range 20 {
			in := archFixture(t)
			// The maps of the declaration and of the registry parts are given anew, so they iterate in another
			// order; the registry parts are also given as new values.
			reordered := map[string]*registry.Component{}
			for _, name := range []string{"worker", "events", "query", "outbox", "store", "command", "auth", "rest-api"} {
				reordered[name] = in.Components[name]
			}
			in.Components = reordered
			_, again := deriveFixture(t, in)
			if !bytes.Equal(first, again) {
				t.Fatalf("two derivations gave different bytes:\n%s\n---\n%s", first, again)
			}
		}
		if !bytes.HasSuffix(first, []byte("}\n")) || bytes.Contains(first, []byte("\r")) || bytes.Contains(first, []byte("\t")) {
			t.Fatal("the model is not two-space indented with LF line ends and one trailing newline")
		}
	})
}

func TestArchitectureFlows(t *testing.T) {
	t.Run("ST-GEN-UC08-R04-T03 A flow runs in the layer order with each slot at its phase, and states its exits", func(t *testing.T) {
		model, _ := deriveFixture(t, archFixture(t))
		var create ArchFlow
		for _, f := range model.Flows {
			if f.ID == "flow:docs.create" {
				create = f
			}
		}
		var got []string
		for _, s := range create.Steps {
			got = append(got, s.Kind+" "+s.Element+" "+s.Mode)
		}
		want := []string{"operation operation:docs.create sync", "authenticate component:auth sync", "handle component:rest-api sync",
			"slot slot:docs.before_create sync", "repository component:store sync", "store component:store sync", "outbox component:outbox sync",
			"slot slot:docs.after_create async", "event component:events async"}
		if !slices.Equal(got, want) {
			t.Fatalf("the create flow is\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if create.Steps[0].Text != "POST /v1/docs" {
			t.Fatalf("the operation step does not take the route of the wiring: %q", create.Steps[0].Text)
		}
		var exits []string
		for _, e := range create.Exits {
			exits = append(exits, e.Result)
		}
		if len(create.Exits) != 3 || create.Exits[0].AfterStep != 2 || !strings.HasPrefix(exits[0], "401") || create.Exits[2].AfterStep != 4 {
			t.Fatalf("the exits are %+v", create.Exits)
		}
		for _, f := range model.Flows {
			if f.ID == "flow:docs.read" {
				for _, s := range f.Steps {
					if s.Kind == "slot" || s.Kind == "outbox" || s.Kind == "event" {
						t.Fatalf("the read flow holds the step %s %s", s.Kind, s.Element)
					}
				}
				if f.Steps[1].Kind != "authenticate" || f.Steps[2].Kind != "authorize" || len(f.Exits) != 2 || !strings.HasPrefix(f.Exits[1].Result, "403") {
					t.Fatalf("the read flow does not decide read on the doc: %+v %+v", f.Steps, f.Exits)
				}
			}
		}
	})
}

// pathsOf gives the grant paths of a permission, each as role, kinds and the conditions.
func pathsOf(model *ArchitectureModel, id string) []string {
	var out []string
	for _, p := range model.Access.Permissions {
		if p.ID != id {
			continue
		}
		for _, g := range p.Paths {
			line := g.Role + " " + strings.Join(g.Kinds, ",")
			if g.Caveat != "" {
				line += " caveat=" + g.Caveat
			}
			if len(g.Requires) > 0 {
				line += " requires=" + strings.Join(g.Requires, ",")
			}
			if len(g.Except) > 0 {
				line += " except=" + strings.Join(g.Except, ",")
			}
			if g.Stops != "" {
				line += " stops=" + strings.SplitN(g.Stops, ":", 2)[0]
			}
			out = append(out, line)
		}
	}
	return out
}

func TestArchitectureAccessMatrix(t *testing.T) {
	t.Run("ST-GEN-UC08-R12-T01 The model holds what each operation and command checks, traced to the declaration", func(t *testing.T) {
		model, _ := deriveFixture(t, archFixture(t))
		got := map[string]string{}
		for _, c := range model.Access.Checks {
			got[c.Subject] = c.ResourceType + "#" + c.Permission + " public=" + map[bool]string{true: "yes", false: "no"}[c.Public] + " " + c.Source.Declaration
		}
		want := map[string]string{
			"operation:docs.create": "doc# public=yes entities[0].permissions.create",
			"operation:docs.read":   "doc#read public=no entities[0].permissions.read",
			"operation:docs.update": "doc#write public=no entities[0].permissions.update",
			"command:archive":       "doc#write public=no commands[0].permission",
		}
		if len(got) != len(want) {
			t.Fatalf("the checks are %v", got)
		}
		for subject, line := range want {
			if got[subject] != line {
				t.Fatalf("%s checks %q, want %q", subject, got[subject], line)
			}
		}
	})

	t.Run("ST-GEN-UC08-R12-T02 The model holds the check of each ancestor in the path of a nested operation, the top one first: the decided parent of a list with its read permission, and each other ancestor with its existence and link only, traced to the declaration", func(t *testing.T) {
		model, _ := deriveFixture(t, nestedFixture(t))
		checks := map[string]ArchCheck{}
		for _, c := range model.Access.Checks {
			checks[c.Subject] = c
		}
		line := func(a ArchAncestor) string {
			return fmt.Sprintf("%s param=%s link=%s %s#%s public=%v decided=%v grants=%s source=%s%s scope=%s", a.Entity, a.Param, a.Link, a.ResourceType, a.Permission,
				a.Public, a.Decided, a.Grants, a.Source.Declaration, a.Source.Rule, a.ScopeSource.Declaration)
		}
		ancestors := func(subject string) []string {
			var got []string
			for _, a := range checks[subject].Ancestors {
				got = append(got, line(a))
			}
			return got
		}
		// The top ancestor first. An update is decided on the row: the shelf and the folder are checked for their
		// existence and their link only, with no permission (ST-SUP-RC20-OC24, ST-SUP-Q156).
		want := []string{
			"entity:Shelf param=shelf_id link= # public=false decided=false grants= source=ST-SUP-RC20-OC24 scope=collections.shelf-folders.scope",
			"entity:Folder param=folder_id link=field:Folder.shelf_id # public=false decided=false grants= source=ST-SUP-RC20-OC24 scope=collections.folder-docs.scope",
		}
		for _, subject := range []string{"operation:folder-docs.update", "operation:folder-docs.read"} {
			if got := ancestors(subject); !slices.Equal(got, want) {
				t.Fatalf("the ancestors of %s are\n%s\nwant\n%s", subject, strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		}
		// A list is decided on its parent, the deepest resource that the request names: the folder has its read
		// permission view, and the shelf above it has its existence and its link only.
		wantList := []string{
			"entity:Shelf param=shelf_id link= # public=false decided=false grants= source=ST-SUP-RC20-OC24 scope=collections.shelf-folders.scope",
			"entity:Folder param=folder_id link=field:Folder.shelf_id folder#view public=false decided=true grants=permission:folder#view source=entities[1].permissions.read scope=collections.folder-docs.scope",
		}
		if got := ancestors("operation:folder-docs.list"); !slices.Equal(got, wantList) {
			t.Fatalf("the ancestors of folder-docs.list are\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantList, "\n"))
		}
		if a := checks["operation:folder-docs.update"].Ancestors[0]; a.ID != "ancestor:folder-docs.update/Shelf" || a.Reason == "" {
			t.Fatalf("the shelf check has the wrong ID or no reason: %+v", a)
		}
		if n := len(checks["operation:shelf-folders.list"].Ancestors); n != 1 || !checks["operation:shelf-folders.list"].Ancestors[0].Decided {
			t.Fatalf("shelf-folders.list has %d ancestor checks, want 1 decided", n)
		}
		if n := len(checks["operation:docs.read"].Ancestors); n != 0 {
			t.Fatalf("a collection with no scope has %d ancestor checks", n)
		}
		// The read permission of the decided folder has its grant paths.
		if paths := pathsOf(model, "permission:folder#view"); len(paths) == 0 {
			t.Fatal("the model holds no grant path of the ancestor permission folder#view")
		}
		points := map[string]string{}
		for _, p := range model.AuthzPoints {
			points[p.Element] = p.Text
		}
		if point := points["operation:folder-docs.update"]; strings.Contains(point, "folder#view") || strings.Contains(point, "shelf#view") || !strings.Contains(point, ancestorRule) {
			t.Fatalf("the authorization point of the update names an ancestor read, or not the existence check: %q", point)
		}
		if point := points["operation:folder-docs.list"]; !strings.Contains(point, "First it decides the read permission folder#view on the parent Folder.") || strings.Contains(point, "shelf#view") || !strings.Contains(point, ancestorRule) {
			t.Fatalf("the authorization point of the list does not name the parent read only: %q", point)
		}
		// With no bound authorization, no ancestor gets a permission: the decided parent says why, and each other
		// ancestor has its existence check.
		in := nestedFixture(t)
		in.Service.Authorization, in.Schema = nil, nil
		unbound, _ := deriveFixture(t, in)
		for _, c := range unbound.Access.Checks {
			for _, a := range c.Ancestors {
				rule := "ST-SUP-RC20-OC24"
				if a.Decided {
					rule = "ST-DECL-UC06-R01"
				}
				if a.Permission != "" || a.Source.Rule != rule {
					t.Fatalf("with no authorization, %s checks %+v", c.Subject, a)
				}
			}
		}
		// A model that claims a permission on an ancestor that is not decided, or that marks a decided ancestor that
		// no handler decides, or a decided parent with no permission, is not valid.
		for _, mutate := range []func(m *ArchitectureModel){
			func(m *ArchitectureModel) {
				for i := range m.Access.Checks {
					if m.Access.Checks[i].Subject == "operation:folder-docs.update" {
						a := &m.Access.Checks[i].Ancestors[1]
						a.ResourceType, a.Permission, a.Grants = "folder", "view", "permission:folder#view"
					}
				}
			},
			func(m *ArchitectureModel) {
				for i := range m.Access.Checks {
					if m.Access.Checks[i].Subject == "operation:folder-docs.read" {
						m.Access.Checks[i].Ancestors[1].Decided = true
					}
				}
			},
			func(m *ArchitectureModel) {
				for i := range m.Access.Checks {
					if m.Access.Checks[i].Subject == "operation:folder-docs.list" {
						a := &m.Access.Checks[i].Ancestors[1]
						a.ResourceType, a.Permission, a.Grants = "", "", ""
					}
				}
			},
		} {
			fresh, _ := deriveFixture(t, nestedFixture(t))
			mutate(fresh)
			if err := checkArchitectureRules(fresh); err == nil || !strings.Contains(err.Error(), "ancestor:folder-docs.") {
				t.Fatalf("a model with a wrong ancestor decision is valid, or fails for another reason: %v", err)
			}
		}
		// A model whose ancestor check names an entity that is not in it is not valid.
		for i := range model.Access.Checks {
			if len(model.Access.Checks[i].Ancestors) > 0 {
				model.Access.Checks[i].Ancestors[0].Entity = "entity:Nowhere"
				break
			}
		}
		if err := checkArchitectureRules(model); err == nil || !strings.Contains(err.Error(), "entity:Nowhere") {
			t.Fatalf("a model with an ancestor of no entity is valid: %v", err)
		}
		// The grants of an ancestor check and of a check must name a permission of the model, not another element
		// such as a role or a field (security review of PR #374, L1).
		for _, mutate := range []func(m *ArchitectureModel) string{
			func(m *ArchitectureModel) string {
				for i := range m.Access.Checks {
					if len(m.Access.Checks[i].Ancestors) > 1 && m.Access.Checks[i].Ancestors[1].Decided {
						m.Access.Checks[i].Ancestors[1].Grants = m.Access.Roles[0].ID
						return m.Access.Roles[0].ID
					}
				}
				return ""
			},
			func(m *ArchitectureModel) string {
				m.Access.Checks[0].Grants = "field:Doc.title"
				return "field:Doc.title"
			},
		} {
			fresh, _ := deriveFixture(t, nestedFixture(t))
			ref := mutate(fresh)
			if err := checkArchitectureRules(fresh); ref == "" || err == nil || !strings.Contains(err.Error(), ref+", which is no permission") {
				t.Fatalf("a model whose grants name %q is valid, or fails for another reason: %v", ref, err)
			}
		}
	})

	t.Run("ST-GEN-UC08-R13-T01 The grant paths are expanded from the schema: an arrow, a userset, a caveat, an intersection, an exclusion, a wildcard and each recursion stated", func(t *testing.T) {
		model, _ := deriveFixture(t, archFixture(t))
		read := pathsOf(model, "permission:doc#read")
		wantRead := []string{
			"role:folder#viewer@user arrow,exclusion except=folder#banned",
			"role:folder#viewer@user:* arrow,exclusion,wildcard except=folder#banned",
			"role:group#member@user arrow,exclusion,userset except=folder#banned",
			" arrow,exclusion,recursion except=folder#banned stops=recursion",
			" arrow,exclusion,recursion,userset except=folder#banned stops=recursion",
		}
		if !slices.Equal(read, wantRead) {
			t.Fatalf("the paths of doc#read are\n%s\nwant\n%s", strings.Join(read, "\n"), strings.Join(wantRead, "\n"))
		}
		write := pathsOf(model, "permission:doc#write")
		wantWrite := []string{
			"role:doc#owner@user direct",
			"role:folder#auditor@user arrow,intersection requires=folder#editor",
			"role:folder#editor@user(ip_allowed) arrow,caveat,intersection caveat=ip_allowed requires=folder#auditor",
		}
		if !slices.Equal(write, wantWrite) {
			t.Fatalf("the paths of doc#write are\n%s\nwant\n%s", strings.Join(write, "\n"), strings.Join(wantWrite, "\n"))
		}
		var roles []string
		for _, r := range model.Access.Roles {
			roles = append(roles, r.ID)
		}
		if !slices.Equal(roles, []string{"role:doc#owner@user", "role:folder#auditor@user", "role:folder#editor@user(ip_allowed)", "role:folder#viewer@user", "role:folder#viewer@user:*", "role:group#member@user"}) {
			t.Fatalf("the roles are %v", roles)
		}
	})
}

func TestArchitectureRefusals(t *testing.T) {
	t.Run("ST-GEN-UC08-R05-T02 The derivation refuses a component with no layer, flow part or port side, a slot with no phase, a kind of operation with no or two serving components, a serving component with no repository and a repository with no store, naming it and the statement", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			change func(ArchitectureInput)
			want   []string
		}{
			{"no role", func(in ArchitectureInput) { in.Components["store"].Architecture = nil }, []string{"component store", "architecture"}},
			{"no layer", func(in ArchitectureInput) { in.Components["store"].Architecture.Layer = "" }, []string{"component store", "architecture.layer"}},
			{"no flow part", func(in ArchitectureInput) { in.Components["auth"].Architecture.Flow = "" }, []string{"component auth", "architecture.flow"}},
			{"no port side", func(in ArchitectureInput) { delete(in.Components["rest-api"].Architecture.Ports, "storage-adapter") }, []string{"component rest-api", "architecture.ports.storage-adapter"}},
			{"no slot phase", func(in ArchitectureInput) { in.Components["rest-api"].Slots[1].Phase = "" }, []string{"component rest-api", "slots[after_create].phase"}},
			{"no mixin slot phase", func(in ArchitectureInput) {
				in.Mixins[0].AddsSlots[0].Phase = ""
			}, []string{"mixin extra", "slots[on_change].phase"}},
			// The kinds of operation and the store (ST-GEN-Q44 (A)).
			{"no component that serves the command", func(in ArchitectureInput) {
				in.Components["command"].Architecture.Serves = nil
			}, []string{"the service has commands", "architecture.serves"}},
			// Only command, query and worker serve their kinds: the derivation knows no other request path
			// (security review of #404, M1).
			{"another component that serves the commands", func(in ArchitectureInput) {
				in.Components["command"].Architecture.Serves = nil
				in.Components["rest-api"].Architecture.Serves = []string{"collection operations", "commands"}
			}, []string{"the component rest-api states in architecture.serves that the commands start at it", "the component command alone"}},
			{"another component that serves the workers", func(in ArchitectureInput) {
				in.Components["worker"].Architecture.Serves = nil
				in.Components["auth"].Architecture.Layer = "handler"
				in.Components["auth"].Architecture.Serves = []string{"workers"}
			}, []string{"the component auth states in architecture.serves that the workers start at it"}},
			{"two components that serve the collections", func(in ArchitectureInput) {
				in.Components["auth"].Architecture.Layer = "handler"
				in.Components["auth"].Architecture.Serves = []string{"collection operations"}
			}, []string{"the components rest-api and auth", "collection operations", "one component serves each kind"}},
			{"a repository with no store", func(in ArchitectureInput) { in.Components["store"].Architecture.Store = "" }, []string{"component store", "architecture.store"}},
			{"a serving component bound to no repository", func(in ArchitectureInput) {
				in.Components["store"].Architecture.Layer, in.Components["store"].Architecture.Store = "handler", ""
			}, []string{"the component rest-api serves the collection operations", "bound to no component of the repository layer"}},
		} {
			in := archFixture(t)
			in.Mixins = []*registry.Mixin{{Kind: "mixin", Name: "extra", Version: "1.0.0", AddsSlots: []registry.SlotDefinition{{Name: "on_change", Phase: "after_store", Operations: []string{"create"}}}}}
			tc.change(in)
			_, err := WriteArchitecture(in)
			var refusal *ArchitectureRefusal
			if !errors.As(err, &refusal) {
				t.Fatalf("%s: got %v, want a refusal", tc.name, err)
			}
			for _, part := range tc.want {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("%s: the refusal does not name %s: %v", tc.name, part, err)
				}
			}
		}
	})
}

func TestArchitectureAncestorRefusals(t *testing.T) {
	t.Run("ST-GEN-UC08-R12-T03 The derivation and the generators refuse the same declarations: each planned entity with a resource type and a public read or no read permission, and each ancestor with no resource type", func(t *testing.T) {
		// planned gives the entities whose rows the command runtime plans, as the generators do: for the rest-api data
		// path (generateRuntimeHandlers) the entity of each collection and each entity with a resource type that a ref
		// field of one names; for the command and the worker generators each entity that a command or a worker writes.
		planned := func(svc *types.ServiceDeclaration) [][]string {
			byName := map[string]types.Entity{}
			for _, e := range svc.Entities {
				byName[e.Name] = e
			}
			var names []string
			for _, c := range svc.Collections {
				if !slices.Contains(names, c.Entity) {
					names = append(names, c.Entity)
				}
			}
			for _, c := range svc.Collections {
				for _, f := range byName[c.Entity].Fields {
					if f.Type == types.FieldTypeRef && byName[f.To].ResourceType != "" && !slices.Contains(names, f.To) {
						names = append(names, f.To)
					}
				}
			}
			sets := [][]string{names}
			for _, c := range svc.Commands {
				sets = append(sets, c.Writes)
			}
			for _, w := range svc.Workers {
				sets = append(sets, w.Writes)
			}
			return sets
		}
		// generate runs the entity planning of the command runtime that the rest-api, the command and the worker
		// generators share (command.EntityFile, whose planEntity refuses the read), on each planned set.
		generate := func(svc *types.ServiceDeclaration) error {
			for _, names := range planned(svc) {
				if _, err := command.EntityFile("data", names, svc.Entities, "tenant_id", command.Relations{}); err != nil {
					return err
				}
			}
			return nil
		}
		folder := func(read string) func(in *ArchitectureInput) {
			return func(in *ArchitectureInput) {
				e := &in.Service.Entities[1]
				e.Permissions = maps.Clone(e.Permissions)
				if read == "" {
					delete(e.Permissions, types.OpRead)
					return
				}
				e.Permissions[types.OpRead] = read
				e.PublicReasons = map[types.Operation]string{types.OpRead: "Anyone may see a folder."}
			}
		}
		// extra adds an entity Log with the resource type log and the read permission (none when empty).
		extra := func(in *ArchitectureInput, read string) {
			permissions := map[types.Operation]string{}
			if read != "" {
				permissions[types.OpRead] = read
			}
			in.Service.Entities = append(in.Service.Entities, types.Entity{Name: "Log", Fields: []types.Field{{Name: "line", Type: "string"}},
				EntityAuthorization: types.EntityAuthorization{ResourceType: "log", Permissions: permissions, PublicReasons: map[types.Operation]string{types.OpRead: "Anyone may see a log."}}})
		}
		parts := func(more ...string) []string {
			return append([]string{"has the resource type", "ST-SUP-RP10-LC13", "permissions.read as a permission of"}, more...)
		}
		for _, tc := range []struct {
			name   string
			change func(in *ArchitectureInput)
			want   []string // the parts of the refusal of the derivation, or none when both accept
		}{
			{"the nested fixture", func(in *ArchitectureInput) {}, nil},
			{"an ancestor with the read public", folder(types.PermissionPublic), []string{"collections.folder-docs.scope", "entity Folder", "entities[1]", "resource type folder", "the read permission public", "entities[1].permissions.read", "ST-SUP-RC20-OC24"}},
			{"an ancestor with no read", folder(""), []string{"collections.folder-docs.scope", "entity Folder", "entities[1]", "resource type folder", "no read permission", "entities[1].permissions.read", "ST-SUP-RC20-OC24"}},
			{"the entity of a collection with the read public", func(in *ArchitectureInput) {
				in.Service.Entities[0].Permissions = maps.Clone(in.Service.Entities[0].Permissions)
				in.Service.Entities[0].Permissions[types.OpRead] = types.PermissionPublic
			}, parts("entities[0] (Doc)", "the read permission public", "collections[0] (docs) serves it")},
			{"the entity of a collection with no read and no authorization", func(in *ArchitectureInput) {
				in.Service.Authorization, in.Schema = nil, nil
				in.Service.Entities[0].Permissions = maps.Clone(in.Service.Entities[0].Permissions)
				delete(in.Service.Entities[0].Permissions, types.OpRead)
			}, parts("entities[0] (Doc)", "no read permission", "collections[0] (docs) serves it")},
			{"the entity that a ref field names with no read", func(in *ArchitectureInput) {
				extra(in, "")
				in.Service.Entities[0].Fields = append(slices.Clone(in.Service.Entities[0].Fields), types.Field{Name: "log_id", Type: types.FieldTypeRef, To: "Log"})
			}, parts("entities[3] (Log)", "no read permission", "the ref field entities[0].fields[3] of Doc, which collections[0] serves, names it")},
			{"an entity that a command writes with the read public", func(in *ArchitectureInput) {
				extra(in, types.PermissionPublic)
				in.Service.Commands[0].Writes = []string{"Log"}
			}, parts("entities[3] (Log)", "the read permission public", "commands[0] (archive) writes it")},
			{"an entity that a worker writes with no read", func(in *ArchitectureInput) {
				extra(in, "")
				in.Service.Workers = []types.Worker{{Name: "sweeper", Writes: []string{"Log"}, ResourceType: "log", Permission: "view"}}
			}, parts("entities[3] (Log)", "no read permission", "workers[0] (sweeper) writes it")},
			{"an entity with no read that nothing plans", func(in *ArchitectureInput) { extra(in, "") }, nil},
		} {
			in := nestedFixture(t)
			tc.change(&in)
			genErr := generate(in.Service)
			_, err := WriteArchitecture(in)
			if tc.want == nil {
				if genErr != nil || err != nil {
					t.Errorf("%s: the generators give %v and the derivation %v, want both to accept", tc.name, genErr, err)
				}
				continue
			}
			if genErr == nil {
				t.Errorf("%s: the generators accept the declaration", tc.name)
			} else {
				read := "no read permission"
				if slices.Contains(tc.want, "the read permission public") {
					read = "the read permission public"
				}
				if !strings.Contains(genErr.Error(), read) {
					t.Errorf("%s: the generators do not name %q: %v", tc.name, read, genErr)
				}
			}
			var refusal *ArchitectureRefusal
			if !errors.As(err, &refusal) {
				t.Errorf("%s: the derivation gives %v, want a refusal", tc.name, err)
				continue
			}
			for _, part := range tc.want {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("%s: the refusal does not name %q: %v", tc.name, part, err)
				}
			}
		}
		// An ancestor with no resource type: the rest-api generator refuses it, because each ancestor is the entity of
		// a collection (it refuses a parent that no collection exposes), and the data path decides each operation of a
		// collection; the declaration check refuses it too when authorization is bound. So the derivation refuses it
		// (security review of PR #383).
		restContext := func(in ArchitectureInput) gen.Context {
			return gen.Context{ModuleName: "example.com/docs", OutDirName: "out", OutputNamespace: "api", ServiceName: "docs", BasePath: "/v1",
				ErrorTypeBase: "https://errors.example.com/docs/", Entities: in.Service.Entities, Collections: in.Service.Collections, Authorization: in.Service.Authorization,
				PeerNamespaces: map[string]string{"command": "command", "query": "query"}, Tenancy: &gen.Tenancy{Key: "tenant_id"},
				Inputs: map[string][]byte{"authz/schema.zed": []byte(nestedFixtureSchema)}}
		}
		if _, _, err := new(restapi.Generator).Generate(restContext(nestedFixture(t))); err != nil {
			t.Fatalf("the rest-api generator refuses the nested fixture: %v", err)
		}
		in := nestedFixture(t)
		in.Service.Entities[2].EntityAuthorization = types.EntityAuthorization{}
		ctx := restContext(in)
		if _, _, err := new(restapi.Generator).Generate(ctx); err == nil || !strings.Contains(err.Error(), "collection shelves reads the entity Shelf, which has no resource type") {
			t.Errorf("the rest-api generator does not refuse an ancestor with no resource type: %v", err)
		}
		_, err := WriteArchitecture(in)
		var refusal *ArchitectureRefusal
		if !errors.As(err, &refusal) {
			t.Fatalf("an ancestor with no resource type: the derivation gives %v, want a refusal", err)
		}
		for _, part := range []string{"collections.shelf-folders.scope", "entity Shelf", "entities[2] has no resource type", "ST-SUP-RC20-OC24", "entities[2].resource_type"} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("an ancestor with no resource type: the refusal does not name %q: %v", part, err)
			}
		}
	})
}

func TestArchitectureViewRefusals(t *testing.T) {
	t.Run("ST-GEN-UC08-R12-T04 The derivation and the query runtime refuse the same declarations: each entity of the rest-api data path with a resource type and a public or no list permission", func(t *testing.T) {
		// generate gives the refusal of the generators: the read-only values of the query runtime on the rest-api data
		// path (query.ViewFile in generateRuntimeHandlers: the entity of each collection and each entity with a
		// resource type that a ref field of one names), and the entity planning of the command runtime on each set
		// that the rest-api, the command and the worker generators plan.
		generate := func(svc *types.ServiceDeclaration) error {
			byName := map[string]types.Entity{}
			for _, e := range svc.Entities {
				byName[e.Name] = e
			}
			var names []string
			for _, c := range svc.Collections {
				if !slices.Contains(names, c.Entity) {
					names = append(names, c.Entity)
				}
			}
			for _, c := range svc.Collections {
				for _, f := range byName[c.Entity].Fields {
					if f.Type == types.FieldTypeRef && byName[f.To].ResourceType != "" && !slices.Contains(names, f.To) {
						names = append(names, f.To)
					}
				}
			}
			if _, err := query.ViewFile("data", names, svc.Entities); err != nil {
				return err
			}
			sets := [][]string{names}
			for _, c := range svc.Commands {
				sets = append(sets, c.Writes)
			}
			for _, w := range svc.Workers {
				sets = append(sets, w.Writes)
			}
			for _, set := range sets {
				if _, err := command.EntityFile("data", set, svc.Entities, "tenant_id", command.Relations{}); err != nil {
					return err
				}
			}
			return nil
		}
		// list sets the list permission of the entity at i, or deletes it when it is empty.
		list := func(i int, permission string) func(in *ArchitectureInput) {
			return func(in *ArchitectureInput) {
				e := &in.Service.Entities[i]
				e.Permissions = maps.Clone(e.Permissions)
				if permission == "" {
					delete(e.Permissions, types.OpList)
					return
				}
				e.Permissions[types.OpList] = permission
				e.PublicReasons = map[types.Operation]string{types.OpList: "Anyone may list them."}
			}
		}
		// extra adds an entity Log with the resource type log, the read permission view and no list permission.
		extra := func(in *ArchitectureInput) {
			in.Service.Entities = append(in.Service.Entities, types.Entity{Name: "Log", Fields: []types.Field{{Name: "line", Type: "string"}},
				EntityAuthorization: types.EntityAuthorization{ResourceType: "log", Permissions: map[types.Operation]string{types.OpRead: "view"}}})
		}
		parts := func(more ...string) []string {
			return append([]string{"has the resource type", "ST-SUP-RC33-OC01", "permissions.list as a permission of"}, more...)
		}
		for _, tc := range []struct {
			name   string
			change func(in *ArchitectureInput)
			want   []string // the parts of the refusal of the derivation, or none when both accept
		}{
			{"the nested fixture", func(in *ArchitectureInput) {}, nil},
			{"the entity of a collection with the list public", list(0, types.PermissionPublic), parts("entities[0] (Doc)", "the list permission public", "collections[0] (docs) serves it", "entities[0].permissions.list")},
			{"the entity of a collection with no list", list(2, ""), parts("entities[2] (Shelf)", "no list permission", "collections[1] (shelves) serves it", "entities[2].permissions.list")},
			{"the entity of a collection with no list and no authorization", func(in *ArchitectureInput) {
				in.Service.Authorization, in.Schema = nil, nil
				list(0, "")(in)
			}, parts("entities[0] (Doc)", "no list permission", "collections[0] (docs) serves it")},
			{"the entity that a ref field names with no list", func(in *ArchitectureInput) {
				extra(in)
				in.Service.Entities[0].Fields = append(slices.Clone(in.Service.Entities[0].Fields), types.Field{Name: "log_id", Type: types.FieldTypeRef, To: "Log"})
			}, parts("entities[3] (Log)", "no list permission", "the ref field entities[0].fields[3] of Doc, which collections[0] serves, names it")},
			{"an entity with no list that only a command writes", func(in *ArchitectureInput) {
				extra(in)
				in.Service.Commands[0].Writes = []string{"Log"}
			}, nil},
			{"an entity with no list that only a worker writes", func(in *ArchitectureInput) {
				extra(in)
				in.Service.Workers = []types.Worker{{Name: "sweeper", Writes: []string{"Log"}, ResourceType: "log", Permission: "view"}}
			}, nil},
		} {
			in := nestedFixture(t)
			tc.change(&in)
			genErr := generate(in.Service)
			_, err := WriteArchitecture(in)
			if tc.want == nil {
				if genErr != nil || err != nil {
					t.Errorf("%s: the generators give %v and the derivation %v, want both to accept", tc.name, genErr, err)
				}
				continue
			}
			if genErr == nil || !strings.Contains(genErr.Error(), "its list permission is") {
				t.Errorf("%s: the query runtime does not refuse the list permission: %v", tc.name, genErr)
			}
			var refusal *ArchitectureRefusal
			if !errors.As(err, &refusal) {
				t.Errorf("%s: the derivation gives %v, want a refusal", tc.name, err)
				continue
			}
			for _, part := range tc.want {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("%s: the refusal does not name %q: %v", tc.name, part, err)
				}
			}
		}
	})

	t.Run("ST-GEN-UC08-R12-T05 The derivation refuses a bound authorization with no compiled schema, an ancestor that the declaration does not state, and collections that a component other than rest-api serves", func(t *testing.T) {
		refused := func(name string, in ArchitectureInput, parts ...string) {
			t.Helper()
			_, err := WriteArchitecture(in)
			var refusal *ArchitectureRefusal
			if !errors.As(err, &refusal) {
				t.Errorf("%s: the derivation gives %v, want a refusal", name, err)
				return
			}
			for _, part := range parts {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("%s: the refusal does not name %q: %v", name, part, err)
				}
			}
		}
		// A bound authorization with no compiled schema: no grant path is known, so an empty role list would read as
		// a definite no (security review of PR #384, L2).
		in := archFixture(t)
		in.Schema = nil
		refused("no compiled schema", in, "authorization is bound", "authz/schema.zed", "no compiled schema", "ST-GEN-UC08-R13")
		// An ancestor that the declaration does not state: the rest-api generator refuses it too (security review of
		// PR #384, L3).
		in = nestedFixture(t)
		in.Service.Collections[3].Scope = map[string]string{"folder_id": "Binder"}
		ctx := gen.Context{ModuleName: "example.com/docs", OutDirName: "out", OutputNamespace: "api", ServiceName: "docs", BasePath: "/v1",
			ErrorTypeBase: "https://errors.example.com/docs/", Entities: in.Service.Entities, Collections: in.Service.Collections, Authorization: in.Service.Authorization,
			PeerNamespaces: map[string]string{"command": "command", "query": "query"}, Tenancy: &gen.Tenancy{Key: "tenant_id"},
			Inputs: map[string][]byte{"authz/schema.zed": []byte(nestedFixtureSchema)}}
		if _, _, err := new(restapi.Generator).Generate(ctx); err == nil {
			t.Error("the rest-api generator accepts an ancestor that the declaration does not state")
		}
		refused("an undeclared ancestor", in, "collections.folder-docs.scope", "entity Binder", "the declaration states no entity Binder", "ST-SUP-RC20-OC24")
		// Collections that a component other than rest-api serves: the derivation does not know what it plans.
		in = archFixture(t)
		api := in.Components["rest-api"]
		delete(in.Components, "rest-api")
		api.Name = "other-api"
		in.Components["other-api"] = api
		in.Names[slices.Index(in.Names, "rest-api")] = "other-api"
		in.Wirings[0].Name = "other-api"
		refused("another component", in, "the component other-api serves the collections", "the component rest-api alone")
		// With no component that serves the collections, the derivation refuses to guess where their flows start
		// (ST-GEN-UC08-R05, ST-GEN-Q44 (A)).
		in = archFixture(t)
		in.Components["rest-api"].Architecture.Serves = []string{"commands", "queries", "workers"}
		refused("no component that serves the collections", in, "the service has collection operations", "architecture.serves")
	})
}

func TestArchitectureEmptyAspects(t *testing.T) {
	t.Run("ST-GEN-UC08-R06-T02 An aspect with no element is empty with its reason, and the others are present", func(t *testing.T) {
		model, _ := deriveFixture(t, archFixture(t))
		if model.Aspects.PlatformEdges != (ArchAspect{State: "empty", Reason: reasonNoNeed}) {
			t.Fatalf("the platform edges are %+v", model.Aspects.PlatformEdges)
		}
		for name, aspect := range map[string]ArchAspect{"structure": model.Aspects.Structure, "data_model": model.Aspects.DataModel,
			"request_flows": model.Aspects.RequestFlows, "trust_and_tenancy": model.Aspects.TrustAndTenancy} {
			if aspect.State != "present" || aspect.Reason != "" {
				t.Fatalf("%s is %+v", name, aspect)
			}
		}
		in := archFixture(t)
		in.Requirements = []byte("version: 1\nservice: docs\nrequirements:\n  - {kind: backing store, name: docs-db, engine: postgres, components: [store]}\n")
		in.Service.Entities, in.Service.Collections, in.Service.Slots, in.Service.Commands = nil, nil, nil, nil
		model, _ = deriveFixture(t, in)
		if model.Aspects.PlatformEdges.State != "present" || len(model.Needs) != 1 || model.Needs[0].StatedBy[0] != "component:store" {
			t.Fatalf("the need of the requirements is not a platform edge: %+v %+v", model.Aspects.PlatformEdges, model.Needs)
		}
		if model.Aspects.DataModel != (ArchAspect{State: "empty", Reason: reasonNoEntity}) || model.Aspects.RequestFlows != (ArchAspect{State: "empty", Reason: reasonNoOperation}) {
			t.Fatalf("with no entity and no operation the aspects are %+v %+v", model.Aspects.DataModel, model.Aspects.RequestFlows)
		}
		// A service whose collections no component serves is refused (ST-GEN-UC08-R05), so its flows are never an
		// empty aspect.
		in = archFixture(t)
		in.Components["rest-api"].Architecture.Serves = nil
		var refusal *ArchitectureRefusal
		if _, err := DeriveArchitecture(in); !errors.As(err, &refusal) || !strings.Contains(err.Error(), "collection operations") {
			t.Fatalf("with no component that serves the collections the derivation gives %v, want a refusal", err)
		}
	})
}

func TestArchitectureValidation(t *testing.T) {
	t.Run("ST-GEN-DS04-T03 A model that breaks the schema or the rules between its elements is not valid, and the fault is named", func(t *testing.T) {
		_, data := deriveFixture(t, archFixture(t))
		if err := ValidateArchitecture(data); err != nil {
			t.Fatalf("the derived model is not valid: %v", err)
		}
		for _, tc := range []struct{ name, from, to, want string }{
			{"no layer", `"layer": "handler",`, ``, "layer"},
			{"a bad side", `"side": "inbound"`, `"side": "left"`, "side"},
			{"no phase", `"phase": "before_store",`, ``, "phase"},
			{"an empty aspect with no reason", `"reason": "No component of the active set states a platform need or an environment variable."`, `"x": 1`, "platform_edges"},
			{"an extra field", `"schema_version": 2,`, `"schema_version": 2, "notes": "hand-written",`, "notes"},
			{"a dangling ID", `"offered_by": "component:rest-api"`, `"offered_by": "component:gone"`, "component:gone"},
			{"a step out of order", `"n": 2,`, `"n": 3,`, "numbered"},
		} {
			changed := strings.Replace(string(data), tc.from, tc.to, 1)
			if changed == string(data) {
				t.Fatalf("%s: the model holds no %s", tc.name, tc.from)
			}
			if err := ValidateArchitecture([]byte(changed)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: got %v, want a fault that names %s", tc.name, err, tc.want)
			}
		}
	})
}

// sourceOf reads the source of an element of the decoded model.
func sourcesIn(t *testing.T, value any, path string, found func(path string, source map[string]any)) {
	switch v := value.(type) {
	case map[string]any:
		// A compliance topic is a heading of facts, not an element.
		if id, isElement := v["id"].(string); isElement && !strings.HasPrefix(id, "topic:") {
			source, ok := v["source"].(map[string]any)
			if !ok {
				t.Fatalf("the element %v at %s names no source", id, path)
			}
			if ok {
				found(path, source)
			}
		}
		for key, item := range v {
			if key != "source" {
				sourcesIn(t, item, path+"."+key, found)
			}
		}
	case []any:
		for _, item := range v {
			sourcesIn(t, item, path, found)
		}
	}
}

func TestExampleArchitecture(t *testing.T) {
	t.Run("ST-GEN-DS04-T01 The architecture model of the example service is valid and holds every part of the five aspects", func(t *testing.T) {
		for _, example := range []string{"user-management", "user-management-rhsso"} {
			dir := filepath.Join("..", "..", "examples", example)
			data, err := os.ReadFile(filepath.Join(dir, ArchitectureFile))
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateArchitecture(data); err != nil {
				t.Fatalf("%s: the committed model is not valid: %v", example, err)
			}
			var model ArchitectureModel
			if err := json.Unmarshal(data, &model); err != nil {
				t.Fatal(err)
			}
			state, err := LoadState(filepath.Join(dir, ".stego", "state.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if state.LastApplied.ProjectFiles[ArchitectureFile] != HashBytes(data) {
				t.Fatalf("%s: the record does not hold the hash of the model", example)
			}
			ids := map[string]bool{}
			for _, c := range model.Components {
				ids[c.ID] = true
			}
			for name := range state.LastApplied.Components {
				if !ids["component:"+name] {
					t.Fatalf("%s: the component %s of the record is not in the model", example, name)
				}
			}
			serviceData, err := os.ReadFile(filepath.Join(dir, "service.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			var service types.ServiceDeclaration
			if err := yaml.Unmarshal(serviceData, &service); err != nil {
				t.Fatal(err)
			}
			fields := map[string]bool{}
			for _, e := range model.Entities {
				for _, f := range e.Fields {
					fields[f.ID] = true
				}
			}
			for _, e := range service.Entities {
				for _, f := range e.Fields {
					if !fields["field:"+e.Name+"."+f.Name] {
						t.Fatalf("%s: the field %s.%s is not in the model", example, e.Name, f.Name)
					}
				}
			}
			flows := map[string]bool{}
			for _, f := range model.Flows {
				flows[f.Operation] = true
			}
			operations := 0
			for _, c := range service.Collections {
				for _, op := range c.Operations {
					operations++
					if !flows["operation:"+c.Name+"."+string(op)] {
						t.Fatalf("%s: the operation %s.%s has no flow", example, c.Name, op)
					}
				}
			}
			if operations != len(model.Flows) || len(model.Slots) != len(service.Slots) {
				t.Fatalf("%s: %d flows for %d operations, %d slots for %d bindings", example, len(model.Flows), operations, len(model.Slots), len(service.Slots))
			}
			var decoded any
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			count := 0
			sourcesIn(t, decoded, "", func(string, map[string]any) { count++ })
			if count < 100 {
				t.Fatalf("%s: only %d elements name a source", example, count)
			}
		}
	})
}

func TestReadArchitecture(t *testing.T) {
	t.Run("ST-GEN-UC16-R06-T02 The reader of the model refuses a missing, changed, linked, too large or not valid model, naming the file and the cause", func(t *testing.T) {
		_, data := deriveFixture(t, archFixture(t))
		project := t.TempDir()
		write := func(model []byte, recorded []byte) {
			t.Helper()
			if err := os.MkdirAll(filepath.Join(project, "architecture"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, ArchitectureFile), model, 0o644); err != nil {
				t.Fatal(err)
			}
			state := &State{FormatVersion: StateFormatVersion, LastApplied: &AppliedState{ProjectFiles: map[string]string{ArchitectureFile: HashBytes(recorded)}}}
			if err := SaveState(filepath.Join(project, ".stego", "state.yaml"), state); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := ReadArchitecture(project); err == nil || !strings.Contains(err.Error(), ArchitectureFile) || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("a missing model: %v", err)
		}
		write(data, data)
		if got, err := ReadArchitecture(project); err != nil || !bytes.Equal(got.Data, data) {
			t.Fatalf("the model as apply wrote it: %v", err)
		}
		changed := bytes.Replace(data, []byte(`"name": "docs"`), []byte(`"name": "other"`), 1)
		write(changed, data)
		if _, err := ReadArchitecture(project); err == nil || !strings.Contains(err.Error(), "does not match the record") {
			t.Fatalf("a changed model: %v", err)
		}
		model := filepath.Join(project, ArchitectureFile)
		outside := filepath.Join(t.TempDir(), "elsewhere.json")
		if err := os.WriteFile(outside, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(model); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, model); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadArchitecture(project); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("a model that is a symbolic link: %v", err)
		}
		if err := os.Remove(model); err != nil {
			t.Fatal(err)
		}
		large := append(bytes.Repeat([]byte(" "), MaxArchitectureBytes), data...)
		write(large, large)
		if _, err := ReadArchitecture(project); err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Fatalf("a model beyond the size bound: %v", err)
		}
		invalid := bytes.Replace(data, []byte(`"layer": "handler",`), nil, 1)
		write(invalid, invalid)
		if _, err := ReadArchitecture(project); err == nil || !strings.Contains(err.Error(), ArchitectureFile) || !strings.Contains(err.Error(), "schema") {
			t.Fatalf("a model that is not valid: %v", err)
		}
		// A cause can quote a value of the model, and the message goes to a terminal (security review of #338).
		if message := (&ArchitectureReadError{File: ArchitectureFile, Cause: "the value \x1b[2J\u202e is not valid"}).Error(); strings.ContainsAny(message, "\x1b\u202e") || !strings.Contains(message, `\u001b[2J\u202e`) {
			t.Fatalf("the refusal prints the characters of the model as they are: %q", message)
		}
	})
	t.Run("ST-GEN-UC08-R17-T01 A reader refuses a model of another schema version before the schema, naming both versions and the remedy", func(t *testing.T) {
		// Each version pins the digest of its schema, so a change of the schema that keeps the version fails here.
		// When this fails, raise ArchitectureSchemaVersion and the $id and schema_version of the schema, and add
		// the digest of the new version.
		schemaDigests := map[int]string{2: "22b8267f9bb511066c6f9d3c1edd5f1d3c4631427f0d874acb27ad80c214da21"}
		if got := HashBytes(ArchitectureSchema); got != schemaDigests[ArchitectureSchemaVersion] {
			t.Fatalf("the architecture schema has the digest %s, not the digest of version %d: a change of the schema must raise its version", got, ArchitectureSchemaVersion)
		}
		_, data := deriveFixture(t, archFixture(t))
		current := fmt.Sprintf(`"schema_version": %d,`, ArchitectureSchemaVersion)
		if !bytes.Contains(data, []byte(current)) || !bytes.Contains(ArchitectureSchema, []byte(`"$id": "`+ArchitectureSchemaID+`"`)) {
			t.Fatalf("the derived model or the schema does not state version %d", ArchitectureSchemaVersion)
		}
		project := t.TempDir()
		write := func(model []byte, build *buildidentity.Record) {
			t.Helper()
			if err := os.MkdirAll(filepath.Join(project, "architecture"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, ArchitectureFile), model, 0o644); err != nil {
				t.Fatal(err)
			}
			state := &State{FormatVersion: StateFormatVersion, LastApplied: &AppliedState{CompilerBuild: build, ProjectFiles: map[string]string{ArchitectureFile: HashBytes(model)}}}
			if err := SaveState(filepath.Join(project, ".stego", "state.yaml"), state); err != nil {
				t.Fatal(err)
			}
		}
		older := bytes.Replace(data, []byte(current), []byte(`"schema_version": 1,`), 1)
		write(older, nil)
		_, err := ReadArchitecture(project)
		if err == nil || !strings.Contains(err.Error(), ArchitectureFile) || !strings.Contains(err.Error(), "version 1") || !strings.Contains(err.Error(), "run stego apply") {
			t.Fatalf("an older model is not refused with its version and the step to take: %v", err)
		}
		if strings.Contains(err.Error(), "does not match the schema") {
			t.Errorf("the refusal of an older model lists schema faults in place of the version: %v", err)
		}
		newer := bytes.Replace(data, []byte(current), []byte(fmt.Sprintf(`"schema_version": %d,`, ArchitectureSchemaVersion+1)), 1)
		build := func(version string) *buildidentity.Record {
			return &buildidentity.Record{GoVersion: "go1.26.11", GOOS: "linux", GOARCH: "amd64", ModuleVersion: version, VCS: "unknown", Revision: "unknown", SourceState: "unknown"}
		}
		write(newer, build("v9.9.9"))
		_, err = ReadArchitecture(project)
		if err == nil || !strings.Contains(err.Error(), "newer") || !strings.Contains(err.Error(), "the record names STEGO v9.9.9") {
			t.Fatalf("a newer model is not refused with the STEGO that the record names: %v", err)
		}
		write(newer, build("unknown"))
		if _, err = ReadArchitecture(project); err == nil || !strings.Contains(err.Error(), "use the STEGO that wrote the model") || strings.Contains(err.Error(), "record names") {
			t.Fatalf("a newer model with no known STEGO version: %v", err)
		}
		// A version that is not a whole number goes to the schema, which refuses it.
		for _, odd := range []string{`"schema_version": "2",`, `"schema_version": 2.5,`, `"schema_version": -1,`} {
			write(bytes.Replace(data, []byte(current), []byte(odd), 1), nil)
			if _, err := ReadArchitecture(project); err == nil || !strings.Contains(err.Error(), "schema") {
				t.Errorf("a model with %s is accepted: %v", odd, err)
			}
		}
	})
}

// wideSchema gives a schema of seven types in a chain, each of whose permission p is the union of an arrow through
// each of its width relations to the next type, which would cost width to the power of six nodes to expand whole.
func wideSchema(width int, leaf string) string {
	var b strings.Builder
	b.WriteString("definition user {}\n\ndefinition node6 {\n\trelation owner: user\n\tpermission view = " + leaf + "\n}\n")
	for i := 5; i >= 0; i-- {
		fmt.Fprintf(&b, "\ndefinition node%d {\n", i)
		var arrows []string
		for r := 1; r <= width; r++ {
			fmt.Fprintf(&b, "\trelation rel%d: node%d\n", r, i+1)
			arrows = append(arrows, fmt.Sprintf("rel%d->view", r))
		}
		fmt.Fprintf(&b, "\tpermission view = %s\n}\n", strings.Join(arrows, " + "))
	}
	return b.String()
}

func TestArchitectureGrantWork(t *testing.T) {
	t.Run("ST-GEN-UC08-R13-T02 The expansion of a wide and deep schema stops at a stated work limit, and the model says that its roles are not complete", func(t *testing.T) {
		// With a leaf that grants nothing, no path is found, so only the work limit can stop the expansion.
		empty, err := declaration.CompileAuthzSchema("authz/schema.zed", []byte(wideSchema(40, "nil")))
		if err != nil {
			t.Fatal(err)
		}
		walk := &grantExpander{schema: empty, complete: true}
		walk.expand("node0", "view", nil, pathState{})
		if walk.steps > maxGrantSteps+1 || len(walk.paths) != 1 || !strings.Contains(walk.paths[0].stops, "work limit") || walk.complete ||
			!slices.Contains(walk.paths[0].state.kinds, "work limit") || slices.Contains(walk.paths[0].state.kinds, "path limit") {
			t.Fatalf("the expansion of a schema that grants nothing took %d steps and gave %+v", walk.steps, walk.paths)
		}
		schema, err := declaration.CompileAuthzSchema("authz/schema.zed", []byte(wideSchema(40, "owner")))
		if err != nil {
			t.Fatal(err)
		}
		e := &grantExpander{schema: schema, complete: true}
		e.expand("node0", "view", nil, pathState{})
		if e.steps > maxGrantSteps+1 {
			t.Fatalf("the expansion took %d steps, more than the bound %d", e.steps, maxGrantSteps)
		}
		grants, _, complete := grantPaths(schema, "node0", "view")
		last := grants[len(grants)-1]
		// The paths to the owner reach the path limit before the work limit; the kind of the last path names the
		// same limit as its stop text (security review of PR #388, L1).
		if complete || !slices.Contains(last.Kinds, "path limit") || slices.Contains(last.Kinds, "work limit") || last.Role != "" ||
			!strings.HasPrefix(last.Stops, fmt.Sprintf("the path limit of %d paths", maxGrantPaths)) {
			t.Fatalf("the cut expansion does not state its limit: complete %v, last path %+v", complete, last)
		}
		in := archFixture(t)
		in.Schema = schema
		in.Service.Commands = []types.Command{{Name: "walk", ResourceType: "node0", Permission: "view"}}
		model, _ := deriveFixture(t, in)
		if model.Access.RolesComplete {
			t.Fatal("the role list does not say that it is not complete")
		}
		for _, p := range model.Access.Permissions {
			if p.ID == "permission:node0#view" && p.Complete {
				t.Fatal("the cut permission says that it is complete")
			}
		}
		full, _ := deriveFixture(t, archFixture(t))
		if !full.Access.RolesComplete {
			t.Fatal("the role list of a small schema says that it is not complete")
		}
	})

	t.Run("ST-GEN-UC08-R13-T04 The expansion that reaches the path limit stops with one path of the kind path limit, whose stop text names that limit, and not the work limit", func(t *testing.T) {
		const limit = "the path limit of 512 paths"
		model, _ := deriveFixture(t, pathFixture(t))
		if len(model.Access.Roles) != 0 || model.Access.RolesComplete {
			t.Fatalf("the path fixture proves too little: %d roles, roles complete %v", len(model.Access.Roles), model.Access.RolesComplete)
		}
		n := 0
		for _, p := range model.Access.Permissions {
			if p.ID != "permission:doc#read" && p.ID != "permission:doc#write" {
				continue
			}
			n++
			if p.Complete || len(p.Paths) != maxGrantPaths+1 {
				t.Fatalf("%s: complete %v with %d paths, want not complete with %d", p.ID, p.Complete, len(p.Paths), maxGrantPaths+1)
			}
			stopped := 0
			for _, g := range p.Paths {
				if slices.Contains(g.Kinds, "work limit") {
					t.Errorf("%s: the path %s has the kind work limit: %+v", p.ID, g.ID, g)
				}
				if !slices.Contains(g.Kinds, "path limit") {
					continue
				}
				stopped++
				if g.Role != "" || !strings.HasPrefix(g.Stops, limit+";") || limitName(g) != limit {
					t.Errorf("%s: the path of the path limit does not name it: %+v", p.ID, g)
				}
			}
			if stopped != 1 {
				t.Errorf("%s: %d paths of the kind path limit, want 1", p.ID, stopped)
			}
		}
		if n != 2 {
			t.Fatalf("the model holds %d of the permissions doc#read and doc#write", n)
		}
		// A path of the kind path limit with no stop text, as a reader can edit it, still names the path limit.
		if got := limitName(ArchGrant{Kinds: []string{"arrow", "path limit"}}); got != "the path limit" {
			t.Errorf("a path limit with no stop text is named %q", got)
		}
	})

	t.Run("ST-GEN-UC08-R13-T03 A caveat or an expiration on a relation, a userset or an arrow makes a conditional grant and its own role", func(t *testing.T) {
		const source = `use expiration

definition user {}

caveat in_hours(hour int) {
	hour < 18
}

definition team {
	relation member: user
}

definition box {
	relation team: team with in_hours
	relation reader: user | user with expiration
	relation viewer: team#member with in_hours
	permission read = reader + viewer + team->member
}
`
		schema, err := declaration.CompileAuthzSchema("authz/schema.zed", []byte(source))
		if err != nil {
			t.Fatal(err)
		}
		grants, roles, complete := grantPaths(schema, "box", "read")
		var got []string
		for _, g := range grants {
			got = append(got, g.Role+" "+strings.Join(g.Kinds, ",")+" caveat="+g.Caveat)
		}
		want := []string{
			"role:box#reader@user direct caveat=",
			"role:box#reader@user(expiration) expiration caveat=",
			"role:team#member@user arrow,caveat caveat=in_hours",
			"role:team#member@user caveat,userset caveat=in_hours",
		}
		if !slices.Equal(got, want) || !complete {
			t.Fatalf("the paths of box#read are\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		for _, r := range roles {
			if r.ID == "role:box#reader@user(expiration)" && !r.Expiration {
				t.Fatalf("the role %s does not state its expiration", r.ID)
			}
		}
	})
}

// complianceFact gives the fact of the model with the ID.
func complianceFact(model *ArchitectureModel, id string) (ArchFact, bool) {
	for _, topic := range model.Compliance {
		for _, f := range topic.Facts {
			if f.ID == id {
				return f, true
			}
		}
	}
	return ArchFact{}, false
}

func TestArchitectureComplianceFacts(t *testing.T) {
	t.Run("ST-GEN-UC16-R01-T02 The audit and tenancy facts follow the settings and the active set", func(t *testing.T) {
		model, _ := deriveFixture(t, archFixture(t))
		if f, ok := complianceFact(model, "fact:audit/entity:Doc/reads-audited"); !ok || f.Value != true || f.State != "default" {
			t.Fatalf("with audit on, the reads of Doc, which has a pii field, are %+v", f)
		}
		off := false
		in := archFixture(t)
		in.Service.Audit = &types.Audit{Enabled: &off, Reason: "a test service"}
		model, _ = deriveFixture(t, in)
		if f, ok := complianceFact(model, "fact:audit/entity:Doc/reads-audited"); !ok || f.Value != false || f.State != "stated" || f.Source.Declaration != "audit.enabled" {
			t.Fatalf("with audit off, the reads of Doc are %+v", f)
		}
		in = archFixture(t)
		in.Service.Tenancy = &types.Tenancy{Kind: "pooled", Key: "org_id", Claim: "org"}
		model, _ = deriveFixture(t, in)
		if f, _ := complianceFact(model, "fact:tenant-isolation/entity:Doc/row-level-security-policy"); f.State != "unstated" {
			t.Fatalf("with no tenancy component, the row-level security of Doc is %+v", f)
		}
		in = archFixture(t)
		in.Service.Tenancy = &types.Tenancy{Kind: "pooled", Key: "org_id", Claim: "org"}
		in.Components["tenancy"] = archComponent("tenancy", "authentication", nil, []string{"tenant-isolation"})
		in.Names = append(in.Names, "tenancy")
		model, _ = deriveFixture(t, in)
		policy, _ := complianceFact(model, "fact:tenant-isolation/entity:Doc/row-level-security-policy")
		key, _ := complianceFact(model, "fact:tenant-isolation/entity:Doc/tenant-key-column")
		if policy.State != "stated" || policy.Source.Registry == nil || policy.Source.Registry.Name != "tenancy" {
			t.Fatalf("with the tenancy component, the row-level security of Doc is %+v", policy)
		}
		if key.Value != "org_id" || key.State != "stated" || key.Source.Declaration != "tenancy.key" {
			t.Fatalf("with a stated tenancy key, the tenant key column of Doc is %+v", key)
		}
	})
}

// archVariables are platform requirements with three environment variables: an optional one with a default that
// two components read, a secret one, and a field of a declaration group.
const archVariables = `version: 2
service: docs
requirements: []
environment:
  - name: DATABASE_URL
    read_by: [store]
    type: string
    required: false
    description: The connection string.
    secret: true
  - name: STORE_TIMEOUT
    read_by: [rest-api, store]
    type: duration
    required: false
    default: 5m
    description: The time of a call.
    secret: false
    allowed: 1s to 30m
  - name: WIDGET_LIMIT
    read_by: [store (Worker)]
    type: integer
    required: true
    description: The limit.
    secret: false
`

func TestArchitectureVariables(t *testing.T) {
	t.Run("ST-GEN-UC08-R16-T01 The model holds each environment variable of the list with its readers, type, required flag, default and secret flag, and no default of a secret one", func(t *testing.T) {
		in := archFixture(t)
		in.Requirements = []byte(archVariables)
		model, data := deriveFixture(t, in)
		if len(model.Variables) != 3 || model.Aspects.PlatformEdges.State != "present" {
			t.Fatalf("the model holds the variables %+v with the platform edges %+v", model.Variables, model.Aspects.PlatformEdges)
		}
		secret, timeout, limit := model.Variables[0], model.Variables[1], model.Variables[2]
		if secret.ID != "variable:DATABASE_URL" || !secret.Secret || secret.Default != nil || secret.Type != "string" {
			t.Fatalf("DATABASE_URL is %+v", secret)
		}
		if !slices.Equal(timeout.ReadBy, []string{"component:rest-api", "component:store"}) || timeout.Type != "duration" || timeout.Required ||
			timeout.Default == nil || *timeout.Default != "5m" || timeout.Source.Registry == nil || timeout.Source.Registry.Name != "rest-api" {
			t.Fatalf("STORE_TIMEOUT is %+v", timeout)
		}
		if !slices.Equal(limit.ReadBy, []string{"component:store/Worker"}) || !limit.Required || limit.Source.Declaration != "overrides.store" {
			t.Fatalf("WIDGET_LIMIT is %+v", limit)
		}
		if bytes.Contains(data, []byte("The connection string")) || bytes.Contains(data, []byte("1s to 30m")) {
			t.Fatalf("the model holds more of a variable than its facts:\n%s", data)
		}
		in.Requirements = []byte(strings.Replace(archVariables, "    secret: true\n", "    secret: true\n    default: CANARY-SECRET-VALUE\n", 1))
		_, data = deriveFixture(t, in)
		if bytes.Contains(data, []byte("CANARY")) {
			t.Fatal("the model holds the default of a secret variable")
		}
		in.Requirements = []byte(strings.Replace(archVariables, "read_by: [store]", "read_by: [other]", 1))
		if _, err := WriteArchitecture(in); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
			t.Fatalf("a variable of a reader that is no component of the active set is accepted: %v", err)
		}
	})
}

func TestArchitectureConfigurationFacts(t *testing.T) {
	t.Run("ST-GEN-UC16-R07-T01 The configuration topic gives each variable's readers, required flag, default and secret flag with their source", func(t *testing.T) {
		in := archFixture(t)
		in.Requirements = []byte(archVariables)
		model, _ := deriveFixture(t, in)
		readBy, _ := complianceFact(model, "fact:configuration/variable:STORE_TIMEOUT/read-by")
		required, _ := complianceFact(model, "fact:configuration/variable:WIDGET_LIMIT/required")
		defaults, _ := complianceFact(model, "fact:configuration/variable:STORE_TIMEOUT/default")
		secret, _ := complianceFact(model, "fact:configuration/variable:DATABASE_URL/secret")
		if readBy.Value != "component:rest-api, component:store" || readBy.State != "stated" || readBy.Source.Registry == nil {
			t.Fatalf("the readers of STORE_TIMEOUT are %+v", readBy)
		}
		if required.Value != true || defaults.Value != "5m" || secret.Value != true {
			t.Fatalf("the facts are %+v, %+v, %+v", required, defaults, secret)
		}
		if _, ok := complianceFact(model, "fact:configuration/variable:DATABASE_URL/default"); ok {
			t.Fatal("the configuration topic gives a default of a secret variable")
		}
	})
}

func TestArchitectureContext(t *testing.T) {
	summary := func(m *ArchitectureModel) (actors, relations []string) {
		for _, a := range m.Actors {
			actors = append(actors, a.ID+" "+a.Kind+" "+a.Name+" <"+sourceText(a.Source)+">")
		}
		for _, r := range m.Relations {
			relations = append(relations, r.From+" "+r.Text+" "+r.To+" through "+r.Through+" "+r.Mode+" <"+sourceText(r.Source)+">")
		}
		return actors, relations
	}
	t.Run("ST-GEN-UC08-R01-T04 The model holds each outside actor that an input states and each relation of the service with it, each with its source, and no other actor", func(t *testing.T) {
		model, _ := deriveFixture(t, archFixture(t))
		actors, relations := summary(model)
		want := []string{
			"actor:authorization.spicedb authorization service spicedb <service.yaml authorization.provider>",
			"actor:caller caller Caller <service.yaml collections>",
			"actor:outside.events outside system System that events calls <registry component events 1.0.0, field architecture.layer>",
			"actor:store.store store DocsDB <registry component store 1.0.0, field architecture.store>",
		}
		if !slices.Equal(actors, want) {
			t.Fatalf("the actors are\n%s\nwant\n%s", strings.Join(actors, "\n"), strings.Join(want, "\n"))
		}
		wantRelations := []string{
			"actor:caller calls 3 operations container:docs through component:rest-api sync <service.yaml collections>",
			"container:docs asks for permission decisions actor:authorization.spicedb through  sync <service.yaml authorization.provider>",
			"container:docs sends change messages, later actor:outside.events through component:events async <registry component events 1.0.0, field architecture.layer>",
			"container:docs keeps rows actor:store.store through component:store sync <registry component store 1.0.0, field architecture.store>",
		}
		if !slices.Equal(relations, wantRelations) {
			t.Fatalf("the relations are\n%s\nwant\n%s", strings.Join(relations, "\n"), strings.Join(wantRelations, "\n"))
		}

		// The issuer of the tokens is an actor only when the part of an authentication component takes one, and the
		// declaration names it when it sets it.
		in := archFixture(t)
		in.Components["auth"].Config = map[string]types.ConfigField{"issuer": {Type: "string"}}
		model, _ = deriveFixture(t, in)
		actors, relations = summary(model)
		if !slices.Contains(actors, "actor:token-issuer.auth token issuer Token issuer of auth <registry component auth 1.0.0, field config.issuer>") ||
			!slices.Contains(relations, "container:docs checks tokens actor:token-issuer.auth through component:auth sync <registry component auth 1.0.0, field config.issuer>") {
			t.Fatalf("a part that takes an issuer gives no token issuer:\n%s\n%s", strings.Join(actors, "\n"), strings.Join(relations, "\n"))
		}
		in.Service.Overrides["auth"] = map[string]any{"issuer": "https://id.example/realm"}
		model, _ = deriveFixture(t, in)
		actors, _ = summary(model)
		if !slices.Contains(actors, "actor:token-issuer.auth token issuer https://id.example/realm <service.yaml overrides.auth.issuer>") {
			t.Fatalf("the declared issuer does not name the actor:\n%s", strings.Join(actors, "\n"))
		}
		// The model shows the scheme, host and path of the issuer, and no query or fragment; a value that is not an
		// https URL with a host, or that holds '@' at any position, is not shown. The last four values put a '?', a
		// '#' or a '/' in the user info, which the parse would read as a part of the host or the path.
		for issuer, want := range map[string]string{
			"https://id.example/realm?token=t0#f":   "https://id.example/realm",
			"https://id.example/realm?":             "https://id.example/realm",
			"https://id.example/realm#":             "https://id.example/realm",
			"https://user:secret@id.example/realm":  "Token issuer of auth",
			"user:secret@id.example/realm":          "Token issuer of auth",
			"id.example/realm":                      "Token issuer of auth",
			"https://u:123/secret@id.example/realm": "Token issuer of auth",
			"https://id.example/realm%40secret":     "Token issuer of auth",
			"https://id.example/realm?by=a@b":       "Token issuer of auth",
			"http://id.example/realm":               "Token issuer of auth",
			"HTTPS://id.example/realm":              "https://id.example/realm",
			"https://tok?en@id.example/realm":       "Token issuer of auth",
			"https://tok#en@id.example/realm":       "Token issuer of auth",
			"https://u:12#secret@id.example/realm":  "Token issuer of auth",
			"https://u:1/x?y@id.example/realm":      "Token issuer of auth",
		} {
			in.Service.Overrides["auth"] = map[string]any{"issuer": issuer}
			model, _ = deriveFixture(t, in)
			actors, _ = summary(model)
			if !slices.Contains(actors, "actor:token-issuer.auth token issuer "+want+" <service.yaml overrides.auth.issuer>") {
				t.Errorf("the declared issuer %q does not name the actor %q:\n%s", issuer, want, strings.Join(actors, "\n"))
			}
		}

		// A need of a backing store is a container that the service keeps its rows in, and then the repository's
		// store is no actor; with no authorization and no collection, there is no authorization service and no caller.
		in = archFixture(t)
		in.Requirements = []byte("requirements:\n  - {kind: backing store, name: docs-db, engine: postgres, components: [store]}\n")
		in.Service.Authorization = nil
		in.Service.Collections, in.Service.Slots = nil, nil
		model, _ = deriveFixture(t, in)
		actors, relations = summary(model)
		if len(actors) != 1 || !strings.HasPrefix(actors[0], "actor:outside.events ") {
			t.Fatalf("the actors with no collection, no authorization and a backing store are\n%s", strings.Join(actors, "\n"))
		}
		if !slices.Contains(relations, "container:docs keeps rows container:docs-db through component:store sync <registry component store 1.0.0, field needs>") {
			t.Fatalf("the service does not keep its rows in the backing store:\n%s", strings.Join(relations, "\n"))
		}
	})

	t.Run("ST-GEN-UC08-R04-T04 The deriver sets the layer of each step of a request flow, and a model with a step in another layer is not valid", func(t *testing.T) {
		model, data := deriveFixture(t, archFixture(t))
		var got []string
		for _, f := range model.Flows {
			if f.ID != "flow:docs.create" {
				continue
			}
			for _, s := range f.Steps {
				got = append(got, s.Kind+" "+s.Layer)
			}
		}
		want := []string{"operation edge", "authenticate authentication", "handle handler", "slot slot", "repository repository", "store store",
			"outbox repository", "slot slot", "event outside call"}
		if !slices.Equal(got, want) {
			t.Fatalf("the layers of the create flow are\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		for _, f := range model.Flows {
			for _, s := range f.Steps {
				if s.Layer == "" {
					t.Errorf("%s step %d has no layer", f.ID, s.N)
				}
			}
		}
		moved := bytes.Replace(data, []byte(`"mode": "sync",
          "layer": "handler"`), []byte(`"mode": "sync",
          "layer": "slot"`), 1)
		if bytes.Equal(moved, data) {
			t.Fatal("the model has no step of the handler layer to move")
		}
		if err := ValidateArchitecture(moved); err == nil || !strings.Contains(err.Error(), "is in the layer slot, not handler") {
			t.Fatalf("a step in another layer is valid: %v", err)
		}
		loose := bytes.Replace(data, []byte(`"to": "container:docs"`), []byte(`"to": "component:rest-api"`), 1)
		if err := ValidateArchitecture(loose); err == nil || !strings.Contains(err.Error(), "joins only containers and actors") {
			t.Fatalf("a relation to a component is valid: %v", err)
		}
	})
}

func TestArchitectureActionFlows(t *testing.T) {
	t.Run("ST-GEN-UC08-R18-T01 Each command, query and worker has a request flow from the component that serves its kind through its fill, the repository and the store to its events and effects", func(t *testing.T) {
		in := archFixture(t)
		in.Service.Commands[0].Writes = []string{"Doc"}
		in.Service.Commands[0].Effects = []types.Effect{{Destination: "mail", Name: "notify"}}
		in.Service.Overrides["events"] = map[string]any{"destinations": []any{map[string]any{"name": "mail", "must_land": true, "idempotent": true}}}
		in.Service.Queries = []types.Query{{Name: "recent", Reads: []string{"Doc"}, ResourceType: "doc", Permission: "read", Fill: types.FillNames{"reader"}}}
		in.Service.Workers = []types.Worker{{Name: "sweeper", Trigger: "every 1m", Writes: []string{"Doc"}, ResourceType: "doc", Permission: "write", Fill: types.FillNames{"sweep"}}}
		model, data := deriveFixture(t, in)
		// The text form of the view titles each action flow by its kind and its name, and says of no declared
		// action that it has no flow.
		view, err := RenderArchitectureView(data)
		if err != nil {
			t.Fatal(err)
		}
		page := parseView(t, view)
		form := textOf(page.byID("form-flows"))
		for _, title := range []string{"command archive", "query recent", "worker sweeper", "The fill sweep carries out the worker."} {
			if !strings.Contains(form, title) {
				t.Errorf("the text form of the flows does not hold %q", title)
			}
		}
		if note := page.byID("form-no-flow-actions"); note != nil {
			t.Errorf("the view says that an action has no flow: %s", textOf(note))
		}
		kinds := map[string]string{}
		for _, f := range model.Flows {
			var parts []string
			for _, s := range f.Steps {
				parts = append(parts, s.Kind+" "+s.Element)
			}
			kinds[f.ID] = strings.Join(parts, ", ")
		}
		for id, want := range map[string]string{
			"flow:command.archive": "operation command:archive, authenticate component:auth, authorize check:command:archive, handle component:command, fill fill:archiver, repository component:store, store component:store, outbox component:outbox, event component:events, outside call component:events",
			"flow:query.recent":    "operation query:recent, authenticate component:auth, authorize check:query:recent, handle component:query, fill fill:reader, repository component:store, store component:store",
			"flow:worker.sweeper":  "operation worker:sweeper, authorize check:worker:sweeper, handle component:worker, fill fill:sweep, repository component:store, store component:store, outbox component:outbox, event component:events",
		} {
			if kinds[id] != want {
				t.Errorf("the flow %s is\n%s\nwant\n%s", id, kinds[id], want)
			}
		}
		for _, f := range model.Flows {
			if f.ID != "flow:worker.sweeper" {
				continue
			}
			if f.Steps[0].Text != "The worker sweeper runs: every 1m." || f.Steps[0].Source.Declaration != "workers[0]" || f.Steps[3].Text != "The fill sweep carries out the worker." {
				t.Errorf("the worker flow does not state its trigger and its fill with their sources: %+v", f.Steps)
			}
		}
		// The fill step is in the slot layer, between the handler and the repository.
		for _, f := range model.Flows {
			for _, s := range f.Steps {
				if s.Kind == "fill" && s.Layer != "slot" {
					t.Errorf("the fill step of %s is in the layer %s", f.ID, s.Layer)
				}
			}
		}
	})
}

func TestArchitectureUnknownHandler(t *testing.T) {
	t.Run("ST-GEN-UC08-R05-T03 For a collection handler other than rest-api the flows state no authentication step, and the registry refuses serves outside the handler layer", func(t *testing.T) {
		// A service with no resource type may have its collections served by another handler (plannedReads); the
		// derivation does not know how it authenticates, so it states no step for it (security review of #404, L1).
		in := archFixture(t)
		in.Service.Authorization = nil
		in.Service.Entities[0].ResourceType, in.Service.Entities[0].Permissions, in.Service.Entities[0].PublicReasons = "", nil, nil
		in.Service.Commands = nil
		api := in.Components["rest-api"]
		delete(in.Components, "rest-api")
		api.Name = "other-api"
		in.Components["other-api"] = api
		in.Names[slices.Index(in.Names, "rest-api")] = "other-api"
		in.Wirings[0].Name = "other-api"
		model, _ := deriveFixture(t, in)
		if len(model.Flows) == 0 {
			t.Fatal("the model holds no flow")
		}
		for _, f := range model.Flows {
			for _, s := range f.Steps {
				if s.Kind == "authenticate" {
					t.Errorf("the flow %s of the handler other-api states the authentication step %s", f.ID, s.Element)
				}
			}
		}
		// The same service with rest-api states the authentication of its bound provider.
		in = archFixture(t)
		model, _ = deriveFixture(t, in)
		if !slices.ContainsFunc(model.Flows[0].Steps, func(s ArchStep) bool { return s.Kind == "authenticate" }) {
			t.Error("the flows of rest-api state no authentication step")
		}
		// serves outside the handler layer is refused (security review of #404, L2).
		in = archFixture(t)
		in.Components["store"].Architecture.Serves = []string{"queries"}
		var refusal *ArchitectureRefusal
		if _, err := WriteArchitecture(in); !errors.As(err, &refusal) || !strings.Contains(err.Error(), "only a component of the handler layer serves") {
			t.Errorf("serves on a repository is not refused: %v", err)
		}
	})
}
