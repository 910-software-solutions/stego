package compiler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/gen"
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

// archFixture gives the inputs of a small service: a handler api that serves the collection docs, an auth provider,
// a store, an outbox and an event publisher, the slots before_create and after_create, a command, and the schema
// above.
func archFixture(t *testing.T) ArchitectureInput {
	t.Helper()
	api := archComponent("api", "handler", []string{"auth-provider", "storage-adapter"}, []string{"http-server"},
		registry.SlotDefinition{Name: "before_create", Phase: "before_store", Operations: []string{"create"}},
		registry.SlotDefinition{Name: "after_create", Phase: "after_store", Operations: []string{"create"}},
		registry.SlotDefinition{Name: "validate", Phase: "before_store", Operations: []string{"create", "update"}})
	api.Config = map[string]types.ConfigField{"expose": {Type: "list"}}
	components := map[string]*registry.Component{
		"api":    api,
		"auth":   archComponent("auth", "authentication", nil, []string{"auth-provider"}),
		"store":  archComponent("store", "repository", nil, []string{"storage-adapter"}),
		"outbox": archComponent("outbox", "repository", nil, []string{"durable-outbox"}),
		"events": archComponent("events", "outside call", []string{"durable-outbox"}, []string{"event-publisher"}),
	}
	schema, err := declaration.CompileAuthzSchema("authz/schema.zed", []byte(archFixtureSchema))
	if err != nil {
		t.Fatal(err)
	}
	service := &types.ServiceDeclaration{Kind: "service", Name: "docs", Archetype: "arch", Language: "go", BasePath: "/v1",
		Authorization: &types.Authorization{Provider: "spicedb", SchemaPath: "authz/schema.zed", SubjectFromIdentity: "user"},
		Overrides:     map[string]any{"storage-adapter": "store"},
		Entities: []types.Entity{{Name: "Doc", Fields: []types.Field{{Name: "title", Type: "string"}, {Name: "email", Type: "string", Sensitivity: "pii"}},
			EntityAuthorization: types.EntityAuthorization{ResourceType: "doc", Permissions: map[types.Operation]string{"create": "public", "read": "read", "update": "write"},
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
		Components: components, Names: []string{"api", "auth", "store", "outbox", "events"}, Wirings: []ComponentWiring{{Name: "api", Wiring: wiring}}, Schema: schema}
}

func deriveFixture(t *testing.T, in ArchitectureInput) (*ArchitectureModel, []byte) {
	t.Helper()
	file, err := WriteArchitecture(in)
	if err != nil {
		t.Fatal(err)
	}
	var model ArchitectureModel
	if err := json.Unmarshal(file.Content, &model); err != nil {
		t.Fatal(err)
	}
	return &model, file.Content
}

func TestArchitectureDeterminism(t *testing.T) {
	t.Run("ST-GEN-DS04-T02 The same inputs always give the same architecture model", func(t *testing.T) {
		_, first := deriveFixture(t, archFixture(t))
		for range 20 {
			in := archFixture(t)
			// The maps of the declaration and of the registry parts are given anew, so they iterate in another
			// order; the registry parts are also given as new values.
			reordered := map[string]*registry.Component{}
			for _, name := range []string{"events", "outbox", "store", "auth", "api"} {
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
		want := []string{"operation operation:docs.create sync", "authenticate component:auth sync", "handle component:api sync",
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
	t.Run("ST-GEN-UC08-R05-T02 The derivation refuses a component with no layer, flow part or port side, and a slot with no phase, naming it and the statement", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			change func(ArchitectureInput)
			want   []string
		}{
			{"no role", func(in ArchitectureInput) { in.Components["store"].Architecture = nil }, []string{"component store", "architecture"}},
			{"no layer", func(in ArchitectureInput) { in.Components["store"].Architecture.Layer = "" }, []string{"component store", "architecture.layer"}},
			{"no flow part", func(in ArchitectureInput) { in.Components["auth"].Architecture.Flow = "" }, []string{"component auth", "architecture.flow"}},
			{"no port side", func(in ArchitectureInput) { delete(in.Components["api"].Architecture.Ports, "storage-adapter") }, []string{"component api", "architecture.ports.storage-adapter"}},
			{"no slot phase", func(in ArchitectureInput) { in.Components["api"].Slots[1].Phase = "" }, []string{"component api", "slots[after_create].phase"}},
			{"no mixin slot phase", func(in ArchitectureInput) {
				in.Mixins[0].AddsSlots[0].Phase = ""
			}, []string{"mixin extra", "slots[on_change].phase"}},
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
		in = archFixture(t)
		in.Components["api"].Config = nil
		model, _ = deriveFixture(t, in)
		if model.Aspects.RequestFlows != (ArchAspect{State: "empty", Reason: reasonNoServer}) {
			t.Fatalf("with no component that serves the collections the request flows are %+v", model.Aspects.RequestFlows)
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
			{"an empty aspect with no reason", `"reason": "No component of the active set states a platform need."`, `"x": 1`, "platform_edges"},
			{"an extra field", `"schema_version": 1,`, `"schema_version": 1, "notes": "hand-written",`, "notes"},
			{"a dangling ID", `"offered_by": "component:api"`, `"offered_by": "component:gone"`, "component:gone"},
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
		if walk.steps > maxGrantSteps+1 || len(walk.paths) != 1 || !strings.Contains(walk.paths[0].stops, "work limit") || walk.complete {
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
		if complete || !slices.Contains(last.Kinds, "work limit") || last.Role != "" || !strings.Contains(last.Stops, "limit") {
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
