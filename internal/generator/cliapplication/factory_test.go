package cliapplication_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/generator/cliapplication"
	"github.com/910-software-solutions/stego/internal/localtest"
	"github.com/910-software-solutions/stego/internal/types"
)

// TestMain fills the checked module cache of localtest first and runs no test without it (ST-STD-LOCAL-R03).
func TestMain(m *testing.M) { os.Exit(localtest.MainModules(m)) }

// noteEntities gives the entity Note of serviceDocument with the field name as given.
func noteEntities(name types.Field) []types.Entity {
	return []types.Entity{{Name: "Note", Fields: []types.Field{name, {Name: "size", Type: "int64"}, {Name: "owner", Type: "string"}}}}
}

// factoryApp gives the definitions of the package app that the generator makes from document for ctx.
func factoryApp(t *testing.T, ctx gen.Context, document string) string {
	t.Helper()
	ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace, ctx.ServiceName, ctx.ComponentConfig, ctx.ServiceOpenAPI = "example.com/notes-cli", "out", "cli", "notes", map[string]any{}, []byte(document)
	files, _, err := new(cliapplication.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.Path == "cli/app/commands.go" {
			return string(file.Bytes())
		}
	}
	t.Fatal("no definitions")
	return ""
}

func TestFactoryApplyIdentity(t *testing.T) {
	t.Run("ST-SUP-RC04-OC36-T01 A collection gets its apply resource whether or not its entity holds the name unique", func(t *testing.T) {
		resource := `{Kind: "Note", APIVersion: "notes/v1", Path: "/api/notes/v1/notes"`
		for _, c := range []struct {
			about     string
			name      types.Field
			composite []string
		}{
			{"a unique name", types.Field{Name: "name", Type: "string", Unique: true}, nil},
			{"a name that is not unique", types.Field{Name: "name", Type: "string"}, nil},
			{"a name unique with another field", types.Field{Name: "name", Type: "string"}, []string{"owner", "name"}},
		} {
			entities := noteEntities(c.name)
			entities[0].Fields[2].UniqueComposite = c.composite
			app := factoryApp(t, gen.Context{Entities: entities, Collections: []types.Collection{{Name: "notes", Entity: "Note"}}}, serviceDocument)
			if !strings.Contains(app, resource) {
				t.Errorf("%s: no apply resource:\n%s", c.about, app)
			}
		}
	})
}

func TestFactoryConditionalPatch(t *testing.T) {
	t.Run("ST-SUP-RC04-OC38-T01 An apply resource whose patch takes If-Match states it, and one whose patch does not take it does not", func(t *testing.T) {
		ctx := gen.Context{Entities: noteEntities(types.Field{Name: "name", Type: "string", Unique: true})}
		if app := factoryApp(t, ctx, serviceDocument); strings.Contains(app, "ConditionalPatch") {
			t.Errorf("a patch with no If-Match states a conditional patch:\n%s", app)
		}
		conditional := strings.Replace(serviceDocument, `"patch": {"operationId": "patchNotes", "parameters": [`, `"patch": {"operationId": "patchNotes", "parameters": [{"name": "If-Match", "in": "header", "schema": {"type": "string"}}, `, 1)
		if conditional == serviceDocument {
			t.Fatal("the document did not change")
		}
		if app := factoryApp(t, ctx, conditional); !strings.Contains(app, "IdempotentWrites: false, ConditionalPatch: true,") {
			t.Errorf("a patch with If-Match does not state a conditional patch:\n%s", app)
		}
	})
}

// serviceDocument is an OpenAPI document as the rest-api component emits it: a collection of notes with a create, a
// list, a read and a patch, and a command with a path parameter.
const serviceDocument = `{"openapi": "3.0.3", "info": {"title": "Generated API", "version": "1.0.0"},
 "paths": {
  "/api/notes/v1/notes": {
   "get": {"operationId": "listNotes", "parameters": [{"name": "pageSize", "in": "query", "schema": {"type": "integer"}}, {"name": "cursor", "in": "query", "schema": {"type": "string"}}],
    "responses": {"200": {"description": "A page", "content": {"application/json": {"schema": {"type": "object"}}}}}},
   "post": {"operationId": "createNotes", "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/NoteCreateRequest"}}}},
    "responses": {"201": {"description": "Created", "content": {"application/json": {"schema": {"type": "object"}}}}}}},
  "/api/notes/v1/notes/{id}": {
   "get": {"operationId": "readNotes", "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
    "responses": {"200": {"description": "A note", "content": {"application/json": {"schema": {"type": "object"}}}}}},
   "patch": {"operationId": "patchNotes", "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
    "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/NotePatchRequest"}}}},
    "responses": {"200": {"description": "A note", "content": {"application/json": {"schema": {"type": "object"}}}}}}},
  "/api/notes/v1/notes/{note_id}/state:archive": {
   "post": {"operationId": "archiveNote", "x-stego-command": true, "parameters": [{"name": "note_id", "in": "path", "required": true, "schema": {"type": "string"}},
     {"name": "Idempotency-Key", "in": "header", "required": true, "schema": {"type": "string"}}],
    "requestBody": {"required": true, "content": {"application/json": {"schema": {"type": "object", "properties": {"reason": {"type": "string"}, "tags": {"type": "array", "items": {"type": "string"}}, "extra": {"type": "object"}}}}}},
    "responses": {"200": {"description": "Archived", "content": {"application/json": {"schema": {"type": "object"}}}}, "409": {"description": "Problem note.archived"}}}}},
 "components": {"schemas": {
  "NoteCreateRequest": {"type": "object", "required": ["name"], "properties": {"name": {"type": "string"}, "size": {"type": "integer"}}},
  "NotePatchRequest": {"type": "object", "properties": {"name": {"type": "string"}, "pinned": {"type": "boolean"}}}}}}`

func TestFactoryFromDocument(t *testing.T) {
	t.Run("ST-SUP-RC04-OC32-T01 With no factory the commands and the apply resources come from the service's OpenAPI document, and with neither the CLI is refused", func(t *testing.T) {
		ctx := gen.Context{ModuleName: "example.com/notes-cli", OutDirName: "out", OutputNamespace: "cli", ServiceName: "notes", ComponentConfig: map[string]any{}, ServiceOpenAPI: []byte(serviceDocument), Entities: noteEntities(types.Field{Name: "name", Type: "string", Unique: true})}
		files, wiring, err := new(cliapplication.Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		project := t.TempDir()
		var app string
		for _, file := range files {
			if file.Path == "cli/app/commands.go" {
				app = string(file.Bytes())
			}
			path := filepath.Join(project, "out", file.Path)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, file.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, want := range []string{`{Name: []string{"archive-note"}, Method: "POST", Path: "/api/notes/v1/notes/{note_id}/state:archive", ID: false, Query: false, IdempotentWrites: true`,
			`PathParameters: []command.PathParameter{{Flag: "note-id", Key: "note_id"}}`, `{Flag: "reason", Key: "reason", Type: "string", Required: false}`,
			`{Flag: "tags", Key: "tags", Type: "string-list", Required: false}`, `{Name: []string{"read-notes"}, Method: "GET", Path: "/api/notes/v1/notes/{id}", ID: true`,
			`{Name: []string{"list-notes"}, Method: "GET", Path: "/api/notes/v1/notes", ID: false, Query: true, IdempotentWrites: false`, `{Flag: "page-size", Key: "pageSize", Type: "integer", Required: false}`,
			`{Kind: "Note", APIVersion: "notes/v1", Path: "/api/notes/v1/notes", IdempotentWrites: false, CreateFields: []command.Field{{Flag: "name", Key: "name", Type: "string", Required: true}, {Flag: "size", Key: "size", Type: "integer", Required: false}}, PatchFields:`,
			`ConfigEnv:      "APPLY_CONFIG"`} {
			if !strings.Contains(app, want) {
				t.Errorf("the definitions do not hold %s:\n%s", want, app)
			}
		}
		if strings.Contains(app, `"extra"`) {
			t.Errorf("the definitions give a flag to an object member, which --body FILE gives")
		}
		// The generated definitions pass the checks of the command runtime: help lists each command and apply.
		var module strings.Builder
		fmt.Fprintf(&module, "module example.com/notes-cli\ngo 1.25.0\nrequire (\n")
		names := []string{}
		for name := range wiring.GoModRequires {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(&module, "%s %s\n", name, wiring.GoModRequires[name])
		}
		module.WriteString(")\n")
		help := "package app_test\nimport (\"bytes\";\"context\";\"strings\";\"testing\";\"example.com/notes-cli/out/cli/app\";command \"example.com/notes-cli/out/cli/command\")\n" +
			"func TestHelp(t *testing.T){var out bytes.Buffer;if err:=command.Run(context.Background(),app.Commands(),[]string{\"--help\"},&out);err!=nil{t.Fatal(err)}\n" +
			"for _,w:=range []string{\"archive-note\",\"create-notes\",\"list-notes\",\"read-notes\",\"patch-notes\",\"apply -f FILE\"}{if !strings.Contains(out.String(),w){t.Errorf(\"help lacks %s:\\n%s\",w,out.String())}}}\n"
		for name, data := range map[string]string{"go.mod": module.String(), "out/cli/app/help_test.go": help} {
			if err := os.WriteFile(filepath.Join(project, name), []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, args := range [][]string{{"mod", "tidy"}, {"test", "-count=1", "-mod=readonly", "./out/cli/app"}} {
			cmd := exec.Command("go", args...)
			cmd.Dir, cmd.Env = project, localtest.GoEnv()
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, output)
			}
		}
		localtest.CheckSums(t, project)
		ctx.ServiceOpenAPI = nil
		if err := new(cliapplication.Generator).ValidateContext(ctx); err == nil || !strings.Contains(err.Error(), "the setting factory_package, or the rest-api component") {
			t.Errorf("with no factory and no service document the CLI gives %v, want a refusal that names the setting", err)
		}
	})
}

func TestFactoryStrictRequest(t *testing.T) {
	t.Run("ST-SUP-RC20-OC55-T05 The CLI gives no flag for kind or for an x-kept or x-output-only member of a closed create request", func(t *testing.T) {
		document := strings.Replace(serviceDocument, `"NoteCreateRequest": {"type": "object", "required": ["name"], "properties": {"name": {"type": "string"}, "size": {"type": "integer"}}}`,
			`"NoteCreateRequest": {"type": "object", "additionalProperties": false, "required": ["name"], "properties": {"name": {"type": "string"}, "size": {"type": "integer"},
			 "kind": {"type": "string", "enum": ["Note"]}, "state": {"type": "string", "x-kept": true}, "created_by": {"type": "string", "nullable": true, "x-output-only": true},
			 "id": {"nullable": true, "x-output-only": true}}}`, 1)
		if document == serviceDocument {
			t.Fatal("the document has no create request to close")
		}
		ctx := gen.Context{ModuleName: "example.com/notes-cli", OutDirName: "out", OutputNamespace: "cli", ServiceName: "notes", ComponentConfig: map[string]any{}, ServiceOpenAPI: []byte(document), Entities: noteEntities(types.Field{Name: "name", Type: "string", Unique: true})}
		files, _, err := new(cliapplication.Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var app string
		for _, file := range files {
			if file.Path == "cli/app/commands.go" {
				app = string(file.Bytes())
			}
		}
		if want := `CreateFields: []command.Field{{Flag: "name", Key: "name", Type: "string", Required: true}, {Flag: "size", Key: "size", Type: "integer", Required: false}}, PatchFields:`; !strings.Contains(app, want) {
			t.Errorf("the definitions do not hold %s:\n%s", want, app)
		}
		for _, member := range []string{`"kind"`, `"state"`, `"created_by"`, `"id"`} {
			if strings.Contains(app, "Key: "+member) {
				t.Errorf("the definitions give a flag to the member %s", member)
			}
		}
	})
}

// secretDocument is a document with a create whose body has a writeOnly member, a member marked x-sensitive and a
// plain member, and a delete.
const secretDocument = `{"openapi": "3.0.3", "info": {"title": "Generated API", "version": "1.0.0"},
 "paths": {
  "/api/keys/v1/keys": {
   "post": {"operationId": "createKeys", "requestBody": {"required": true, "content": {"application/json": {"schema": {"type": "object", "required": ["pin"],
     "properties": {"label": {"type": "string"}, "pin": {"type": "string", "writeOnly": true}, "token": {"type": "string", "x-sensitive": true}}}}}},
    "responses": {"201": {"description": "Created", "content": {"application/json": {"schema": {"type": "object"}}}}}}},
  "/api/keys/v1/keys/{id}": {
   "delete": {"operationId": "deleteKeys", "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
    "responses": {"204": {"description": "Deleted"}}}}}}`

func TestFactorySecrets(t *testing.T) {
	t.Run("ST-SUP-RC04-OC32-T02 A secret member has no flag, and a delete asks for --yes", func(t *testing.T) {
		ctx := gen.Context{ModuleName: "example.com/keys-cli", OutDirName: "out", OutputNamespace: "cli", ServiceName: "keys", ComponentConfig: map[string]any{}, ServiceOpenAPI: []byte(secretDocument)}
		files, _, err := new(cliapplication.Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var app string
		for _, file := range files {
			if file.Path == "cli/app/commands.go" {
				app = string(file.Bytes())
			}
		}
		if !strings.Contains(app, `{Flag: "label", Key: "label", Type: "string", Required: false}`) {
			t.Errorf("the plain member has no flag:\n%s", app)
		}
		for _, secret := range []string{`Flag: "pin"`, `Flag: "token"`} {
			if strings.Contains(app, secret) {
				t.Errorf("the secret member %s has a flag, which puts it in the shell history and the process list:\n%s", secret, app)
			}
		}
		// Only --body FILE gives a secret member.
		if !strings.Contains(app, `BodyFields: []command.Field{{Flag: "", Key: "pin", Type: "string", Required: true}, {Flag: "", Key: "token", Type: "string", Required: false}}`) {
			t.Errorf("the secret members are not body fields:\n%s", app)
		}
		if !strings.Contains(app, `{Name: []string{"delete-keys"}, Method: "DELETE", Path: "/api/keys/v1/keys/{id}", ID: true, Query: false, IdempotentWrites: false, Confirm: true,`) {
			t.Errorf("the delete does not ask for --yes:\n%s", app)
		}
		if !strings.Contains(app, `{Name: []string{"create-keys"}, Method: "POST", Path: "/api/keys/v1/keys", ID: false, Query: false, IdempotentWrites: false, Confirm: false,`) {
			t.Errorf("the create asks for --yes:\n%s", app)
		}
	})
}

func TestFactoryApplySecrets(t *testing.T) {
	t.Run("ST-SUP-RC04-OC32-T07 An apply resource takes a secret member of its create and its patch as a field with no flag", func(t *testing.T) {
		document := strings.Replace(strings.Replace(serviceDocument,
			`"NoteCreateRequest": {"type": "object", "required": ["name"], "properties": {"name": {"type": "string"}, "size": {"type": "integer"}}}`,
			`"NoteCreateRequest": {"type": "object", "required": ["name", "pin"], "properties": {"name": {"type": "string"}, "size": {"type": "integer"}, "pin": {"type": "string", "writeOnly": true}}}`, 1),
			`"NotePatchRequest": {"type": "object", "properties": {"name": {"type": "string"}, "pinned": {"type": "boolean"}}}`,
			`"NotePatchRequest": {"type": "object", "properties": {"name": {"type": "string"}, "pinned": {"type": "boolean"}, "pin": {"type": "string", "x-sensitive": true}}}`, 1)
		if strings.Count(document, `"pin"`) != 3 {
			t.Fatal("the document has no secret member of the create and the patch")
		}
		ctx := gen.Context{Entities: noteEntities(types.Field{Name: "name", Type: "string", Unique: true})}
		app := factoryApp(t, ctx, document)
		for _, want := range []string{
			`CreateFields: []command.Field{{Flag: "name", Key: "name", Type: "string", Required: true}, {Flag: "size", Key: "size", Type: "integer", Required: false}, {Flag: "", Key: "pin", Type: "string", Required: true}}`,
			`PatchFields: []command.Field{{Flag: "name", Key: "name", Type: "string", Required: false}, {Flag: "pinned", Key: "pinned", Type: "boolean", Required: false}, {Flag: "", Key: "pin", Type: "string", Required: false}}`,
		} {
			if !strings.Contains(app, want) {
				t.Errorf("the apply resource does not hold %s:\n%s", want, app)
			}
		}
		if strings.Contains(app, `Flag: "pin"`) {
			t.Errorf("the secret member has a flag:\n%s", app)
		}
	})
}

// sensitiveDocument has a command whose answer holds a member marked x-sensitive in an item of a list of a nested
// object, and a command whose answer holds none.
const sensitiveDocument = `{"openapi": "3.0.3", "info": {"title": "Generated API", "version": "1.0.0"},
 "paths": {
  "/api/keys/v1/keys:issue": {
   "post": {"operationId": "issueKey", "x-stego-command": true, "requestBody": {"required": true, "content": {"application/json": {"schema": {"type": "object", "properties": {"label": {"type": "string"}}}}}},
    "responses": {"201": {"description": "Issued", "content": {"application/json": {"schema": {"type": "object",
     "properties": {"issued": {"type": "object", "properties": {"keys": {"type": "array", "items": {"type": "object",
      "properties": {"value": {"type": "string", "format": "password", "x-sensitive": true}}}}}}}}}}}}}},
  "/api/keys/v1/keys:count": {
   "post": {"operationId": "countKeys", "x-stego-command": true, "requestBody": {"required": true, "content": {"application/json": {"schema": {"type": "object",
     "properties": {"pin": {"type": "string", "writeOnly": true, "x-sensitive": true}}}}}},
    "responses": {"200": {"description": "Counted", "content": {"application/json": {"schema": {"type": "object", "properties": {"count": {"type": "integer"}}}}}}}}}}}`

func TestFactorySensitive(t *testing.T) {
	t.Run("ST-SUP-RC04-OC33-T01 A command whose answer holds a secret is Sensitive, and a command whose answer holds none is not", func(t *testing.T) {
		ctx := gen.Context{ModuleName: "example.com/keys-cli", OutDirName: "out", OutputNamespace: "cli", ServiceName: "keys", ComponentConfig: map[string]any{}, ServiceOpenAPI: []byte(sensitiveDocument)}
		files, _, err := new(cliapplication.Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var app string
		for _, file := range files {
			if file.Path == "cli/app/commands.go" {
				app = string(file.Bytes())
			}
		}
		line := func(name string) string {
			for _, l := range strings.Split(app, "\n") {
				if strings.Contains(l, `{Name: []string{"`+name+`"}`) {
					return l
				}
			}
			t.Fatalf("the definitions have no command %s:\n%s", name, app)
			return ""
		}
		if issue := line("issue-key"); !strings.Contains(issue, "Sensitive: true,") {
			t.Errorf("the command whose answer holds a secret is not Sensitive: %s", issue)
		}
		// A secret request member alone does not make the answer Sensitive.
		if count := line("count-keys"); strings.Contains(count, "Sensitive") {
			t.Errorf("the command whose answer holds no secret is Sensitive: %s", count)
		}
	})
}

// opaqueDocument holds the command describeKey, whose 200 answer the rest-api component marks x-stego-opaque at its
// root (it holds a google.protobuf.Struct), and the command listKeys, whose answer is an object with no fixed members
// and no mark.
const opaqueDocument = `{"openapi": "3.0.3", "info": {"title": "keys", "version": "1"}, "paths": {
 "/api/keys/v1/keys/{key_id}/state:describe": {"post": {"operationId": "describeKey", "x-stego-command": true,
   "parameters": [{"name": "key_id", "in": "path", "required": true, "schema": {"type": "string"}}],
   "requestBody": {"required": true, "content": {"application/json": {"schema": {"type": "object"}}}},
   "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "object", "x-stego-opaque": true,
     "properties": {"data": {"type": "object"}}}}}}}}},
 "/api/keys/v1/keys:list": {"post": {"operationId": "listKeys", "x-stego-command": true,
   "requestBody": {"required": true, "content": {"application/json": {"schema": {"type": "object"}}}},
   "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "object", "properties": {"data": {"type": "object"}}}}}}}}}}}`

func TestFactoryOpaqueSensitive(t *testing.T) {
	t.Run("ST-SUP-RC04-OC33-T02 A command whose answer is marked x-stego-opaque is Sensitive, and a command whose answer has no mark is not", func(t *testing.T) {
		ctx := gen.Context{ModuleName: "example.com/keys-cli", OutDirName: "out", OutputNamespace: "cli", ServiceName: "keys", ComponentConfig: map[string]any{}, ServiceOpenAPI: []byte(opaqueDocument)}
		files, _, err := new(cliapplication.Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var app string
		for _, file := range files {
			if file.Path == "cli/app/commands.go" {
				app = string(file.Bytes())
			}
		}
		line := func(name string) string {
			for _, l := range strings.Split(app, "\n") {
				if strings.Contains(l, `{Name: []string{"`+name+`"}`) {
					return l
				}
			}
			t.Fatalf("the definitions have no command %s:\n%s", name, app)
			return ""
		}
		if describe := line("describe-key"); !strings.Contains(describe, "Sensitive: true,") {
			t.Errorf("the command whose answer is opaque is not Sensitive: %s", describe)
		}
		if list := line("list-keys"); strings.Contains(list, "Sensitive") {
			t.Errorf("the command whose answer has no mark is Sensitive: %s", list)
		}
	})
}
