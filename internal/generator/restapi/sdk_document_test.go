package restapi

import (
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/generator/gosdk"
	"github.com/910-software-solutions/stego/internal/generator/typescriptsdk"
	"github.com/910-software-solutions/stego/internal/types"
)

// markedDocument has a patch member marked x-kept and a command answer marked x-stego-opaque, as the rest-api
// component writes them.
const markedDocument = `openapi: 3.0.3
info: {title: Todo, version: '1'}
paths:
  /api/todos/{id}:
    patch:
      operationId: patchTodo
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              properties:
                title: {type: string}
                code: {type: string, x-kept: true}
      responses:
        '200': {description: Patched}
  /api/todos/{id}/state:close:
    post:
      operationId: closeTodo
      x-stego-command: true
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
      requestBody:
        required: true
        content:
          application/json:
            schema: {type: object, properties: {reason: {type: string}}}
      responses:
        '200':
          description: Closed
          content:
            application/json:
              schema: {type: object, x-stego-opaque: true, properties: {data: {type: object}}}
`

func TestSDKsOfTheServiceDocument(t *testing.T) {
	t.Run("ST-SUP-RC06-OC09-T02 The Go and TypeScript SDKs are made from a rest-api document with a kept patch member and an opaque command answer", func(t *testing.T) {
		ctx := routesContext()
		ctx.Inputs = map[string][]byte{"proto/todo.proto": []byte(opaqueProto("google.protobuf.Struct data = 1;"))}
		ctx.Entities[0].Fields = append(ctx.Entities[0].Fields, types.Field{Name: "code", Type: types.FieldTypeString, Writable: types.WritableCreate})
		document, err := new(Generator).OpenAPIDocument(onRuntime(ctx))
		if err != nil {
			t.Fatal(err)
		}
		for _, mark := range []string{`"x-kept": true`, `"x-stego-opaque": true`, `"x-stego-command": true`} {
			if !strings.Contains(string(document), mark) {
				t.Fatalf("the document has no %s:\n%s", mark, document)
			}
		}
		sdk := gen.Context{ModuleName: "example.com/todo", OutDirName: "out", OutputNamespace: "sdk", ServiceName: "todo", ComponentConfig: map[string]any{}, ServiceOpenAPI: document}
		if files, _, err := new(gosdk.Generator).Generate(sdk); err != nil || len(files) == 0 {
			t.Errorf("the Go SDK of the document gave %d files and %v, want files", len(files), err)
		}
		// The TypeScript SDK takes no header parameter, so it gets a document with the same marks and no header, as
		// its setting document.
		web := gen.Context{ModuleName: "example.com/todo", OutDirName: "out", OutputNamespace: "web/sdk", ComponentConfig: map[string]any{"document": "api.yaml"},
			Inputs: map[string][]byte{"api.yaml": []byte(markedDocument)}}
		if files, _, err := new(typescriptsdk.Generator).Generate(web); err != nil || len(files) == 0 {
			t.Errorf("the TypeScript SDK of a document with x-kept and x-stego-opaque gave %d files and %v, want files", len(files), err)
		}
	})
}
