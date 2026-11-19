package restapi

import (
	"context"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// routesProto holds the messages of the command close-todo: a path member, an int64, an enum, a list, a map and a
// nested message, so the document gives each in its protobuf JSON form.
const routesProto = `syntax = "proto3";
package todo.v1;
enum Reason {
  REASON_UNSPECIFIED = 0;
  REASON_DONE = 1;
}
message Note { string text = 1; }
message CloseTodoRequest {
  string todo_id = 1;
  Reason reason = 2;
  repeated string labels = 3;
  map<string, int32> counts = 4;
  Note note = 5;
}
message CloseTodoResponse {
  string todo_id = 1;
  int64 closed_count = 2;
}
`

// routesContext is the todo service with the command close-todo at POST /todos/{todo_id}/state:close with one refusal,
// the get query todo-summary at GET /todos/{todoId}/summary, and the list query open-todos at GET /todos/open.
func routesContext() gen.Context {
	ctx := todoIdempotencyContext()
	ctx.Idempotency = nil
	ctx.Conventions.RequestValidation = "openapi-schema"
	ctx.Commands = []gen.Command{{Command: types.Command{Name: "close-todo", Writes: []string{"Todo"}, ResourceType: "todo", Permission: "edit",
		ResourceMember: "todo_id", Route: "POST /todos/{todo_id}/state:close", Fill: types.FillNames{"close-todo"},
		Refusals: []types.Refusal{{Name: "closed", Status: 409, Code: "todo.closed"}}}}}
	ctx.Queries = []types.Query{
		{Name: "todo-summary", Reads: []string{"Todo"}, ResourceType: "todo", Permission: "view", ResourceMember: "todoId", Route: "GET /todos/{todoId}/summary", Fill: types.FillNames{"todo-summary"}},
		{Name: "open-todos", Reads: []string{"Todo"}, ResourceType: "todo", Permission: "view", Route: "GET /todos/open", Fill: types.FillNames{"open-todos"}},
	}
	ctx.ModuleName, ctx.OutDirName = "example.com/todo", "out"
	ctx.PeerNamespaces = map[string]string{"command": "commands"}
	ctx.PeerConfigs = map[string]map[string]any{"command": {"proto_files": []any{map[string]any{"path": "proto/todo.proto", "import_path": "todo/v1/todo.proto"}}}}
	ctx.Inputs = map[string][]byte{"proto/todo.proto": []byte(routesProto)}
	return ctx
}

func TestCommandAndQueryRoutes(t *testing.T) {
	t.Run("ST-SUP-RC20-OC53-T01 The OpenAPI document describes each command and query route, and the request validation does not check a command body", func(t *testing.T) {
		files, _, err := new(Generator).Generate(onRuntime(routesContext()))
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		var document, validation []byte
		for _, f := range files {
			switch f.Path {
			case "internal/api/openapi.json":
				document = f.Bytes()
			case "internal/api/validation.go":
				validation = f.Bytes()
			}
		}
		doc, err := openapi3.NewLoader().LoadFromData(document)
		if err != nil {
			t.Fatal(err)
		}
		if err := doc.Validate(context.Background()); err != nil {
			t.Fatalf("the document is not valid OpenAPI: %v", err)
		}
		command := doc.Paths.Value("/api/todo/v1/todos/{todo_id}/state:close")
		if command == nil || command.Post == nil {
			t.Fatalf("the document has no operation of the command close-todo:\n%s", document)
		}
		op := command.Post
		if op.OperationID != "closeTodo" || op.Extensions[commandMarker] != true {
			t.Errorf("the command operation is %q with the marker %v, want closeTodo marked", op.OperationID, op.Extensions[commandMarker])
		}
		if key := op.Parameters.GetByInAndName("header", "Idempotency-Key"); key == nil || !key.Required {
			t.Errorf("the command does not require the Idempotency-Key header")
		}
		if path := op.Parameters.GetByInAndName("path", "todo_id"); path == nil || !path.Required {
			t.Errorf("the command has no required path parameter todo_id")
		}
		body := op.RequestBody.Value.Content.Get("application/json").Schema.Value
		if _, there := body.Properties["todoId"]; there {
			t.Errorf("the body holds the member that the path gives")
		}
		for name, want := range map[string]string{"reason": "string", "labels": "array", "counts": "object", "note": "object"} {
			if p := body.Properties[name]; p == nil || !p.Value.Type.Is(want) {
				t.Errorf("the body member %s is not of type %s", name, want)
			}
		}
		if enum := body.Properties["reason"].Value.Enum; len(enum) != 2 || enum[1] != "REASON_DONE" {
			t.Errorf("the enum member gives the values %v", enum)
		}
		if counts := body.Properties["counts"].Value.AdditionalProperties.Schema; counts == nil || !counts.Value.Type.Is("integer") {
			t.Errorf("the map member does not give its value schema")
		}
		response := op.Responses.Value("200").Value.Content.Get("application/json").Schema.Value
		if count := response.Properties["closedCount"]; count == nil || !count.Value.Type.Is("string") || count.Value.Format != "int64" {
			t.Errorf("the int64 member of the response is not a string of the format int64, as protojson writes it")
		}
		if conflict := op.Responses.Value("409"); conflict == nil || !strings.Contains(*conflict.Value.Description, "todo.closed") {
			t.Errorf("the 409 response does not name the refusal todo.closed")
		}
		get := doc.Paths.Value("/api/todo/v1/todos/{todoId}/summary")
		if get == nil || get.Get == nil || get.Get.OperationID != "todoSummary" || get.Get.Parameters.GetByInAndName("path", "todoId") == nil {
			t.Fatalf("the document has no operation of the get query todo-summary")
		}
		list := doc.Paths.Value("/api/todo/v1/todos/open")
		if list == nil || list.Get == nil || list.Get.Parameters.GetByInAndName("query", "cursor") == nil || list.Get.Parameters.GetByInAndName("query", "limit") == nil {
			t.Fatalf("the list query open-todos has no cursor and limit")
		}
		page := list.Get.Responses.Value("200").Value.Content.Get("application/json").Schema.Value
		if page.Properties["items"] == nil || page.Properties["next"] == nil {
			t.Errorf("the page of the list query has no items and next")
		}
		if !strings.Contains(string(validation), `route.Operation.Extensions["x-stego-command"] != nil`) {
			t.Errorf("the request validation checks the body of a command:\n%s", validation)
		}
		// OpenAPIDocument gives the same document, which the client components read.
		same, err := new(Generator).OpenAPIDocument(onRuntime(routesContext()))
		if err != nil || string(same) != string(document) {
			t.Errorf("OpenAPIDocument gives another document: %v", err)
		}
	})
}
