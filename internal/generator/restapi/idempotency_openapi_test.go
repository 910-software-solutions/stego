package restapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/types"
)

const todoProblemBase = "https://errors.example.com/todo/"

// todoIdempotencyContext is the test declaration of the idempotency component's output contracts: the owned entity
// Todo in the collection todos with create, read, list, patch, update and delete, the problem base, and the
// idempotency component in the service with Required on as given.
func todoIdempotencyContext(requiredOn ...types.Operation) gen.Context {
	return gen.Context{
		Conventions:     types.Convention{ErrorHandling: "problem-details-rfc"},
		OutputNamespace: "internal/api",
		BasePath:        "/api/todo/v1",
		ErrorTypeBase:   todoProblemBase,
		Idempotency:     &types.Idempotency{RequiredOn: requiredOn, Retention: "24h", Fingerprint: "body"},
		PeerNamespaces:  map[string]string{"idempotency": "internal/idempotency"},
		Entities: []types.Entity{{Name: "Todo", Fields: []types.Field{
			{Name: "title", Type: types.FieldTypeString},
			{Name: "done", Type: types.FieldTypeBool, Optional: true},
		}}},
		Collections: []types.Collection{{Name: "todos", Entity: "Todo", Patchable: []string{"title", "done"},
			Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList, types.OpPatch, types.OpUpdate, types.OpDelete}}},
	}
}

type documentOperation struct {
	Parameters []struct {
		Name     string `json:"name"`
		In       string `json:"in"`
		Required bool   `json:"required"`
	} `json:"parameters"`
	Responses map[string]struct {
		Description string                     `json:"description"`
		Headers     map[string]json.RawMessage `json:"headers"`
	} `json:"responses"`
}

// todoOperations generates the rest-api component and reads the operations of its OpenAPI document by method and
// path.
func todoOperations(t *testing.T, ctx gen.Context) map[string]documentOperation {
	t.Helper()
	files, _, err := new(Generator).Generate(onRuntime(ctx))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, file := range files {
		if file.Path != "internal/api/openapi.json" {
			continue
		}
		var document struct {
			Paths map[string]map[string]documentOperation `json:"paths"`
		}
		if err := json.Unmarshal(file.Bytes(), &document); err != nil {
			t.Fatalf("read openapi.json: %v", err)
		}
		operations := map[string]documentOperation{}
		for path, item := range document.Paths {
			for method, operation := range item {
				operations[strings.ToUpper(method)+" "+path] = operation
			}
		}
		return operations
	}
	t.Fatal("the rest-api component emits no openapi.json")
	return nil
}

// keyHeader gives the Idempotency-Key header parameter of an operation, and whether it has one.
func keyHeader(operation documentOperation) (required, found bool) {
	for _, parameter := range operation.Parameters {
		if parameter.Name == "Idempotency-Key" {
			if parameter.In != "header" {
				return false, false
			}
			return parameter.Required, true
		}
	}
	return false, false
}

// checkProblems expects the 400, 409 and 422 responses of an operation to name the idempotency problem types: the
// 400 names an absent key and a key that is not valid (ST-SUP-Q202 (a)).
func checkProblems(t *testing.T, label string, operation documentOperation) {
	t.Helper()
	for _, c := range [][2]string{{"400", "idempotency-key-missing"}, {"400", "idempotency-key-invalid"}, {"409", "idempotency-key-in-progress"}, {"422", "idempotency-key-reused"}} {
		code, slug := c[0], c[1]
		if !strings.Contains(operation.Responses[code].Description, todoProblemBase+slug) {
			t.Errorf("%s: response %s is %q, want the problem type %s", label, code, operation.Responses[code].Description, todoProblemBase+slug)
		}
	}
}

func TestIdempotencyKeyHeader(t *testing.T) {
	t.Run("ST-SUP-RC30-OC01-T01 Create requires the Idempotency-Key header in the OpenAPI document", func(t *testing.T) {
		create := todoOperations(t, todoIdempotencyContext(types.OpCreate, types.OpPatch))["POST /api/todo/v1/todos"]
		if required, found := keyHeader(create); !found || !required {
			t.Fatalf("create: Idempotency-Key header found %t, required %t; want a required header", found, required)
		}
		checkProblems(t, "create", create)
	})

	t.Run("ST-SUP-RC30-OC01-T02 Patch accepts the Idempotency-Key header", func(t *testing.T) {
		patch := todoOperations(t, todoIdempotencyContext(types.OpCreate))["PATCH /api/todo/v1/todos/{id}"]
		if required, found := keyHeader(patch); !found || required {
			t.Fatalf("patch with Required on create only: header found %t, required %t; want an accepted header", found, required)
		}
		checkProblems(t, "patch", patch)
	})

	t.Run("ST-SUP-RC30-OC01-T03 Get, put and delete carry no Idempotency-Key header", func(t *testing.T) {
		operations := todoOperations(t, todoIdempotencyContext(types.OpCreate, types.OpPatch))
		for _, name := range []string{"GET /api/todo/v1/todos", "GET /api/todo/v1/todos/{id}", "PUT /api/todo/v1/todos/{id}", "DELETE /api/todo/v1/todos/{id}"} {
			operation, ok := operations[name]
			if !ok {
				t.Fatalf("the document has no operation %s", name)
			}
			if _, found := keyHeader(operation); found {
				t.Errorf("%s carries the Idempotency-Key header", name)
			}
		}
	})

	t.Run("ST-SUP-RC20-OC56-T01 With no idempotency component, a create and a patch take the key, not required, and no other operation does", func(t *testing.T) {
		ctx := todoIdempotencyContext(types.OpCreate, types.OpPatch)
		ctx.PeerNamespaces = nil
		for name, operation := range todoOperations(t, ctx) {
			required, found := keyHeader(operation)
			takes := name == "POST /api/todo/v1/todos" || name == "PATCH /api/todo/v1/todos/{id}"
			if found != takes || required {
				t.Errorf("without the idempotency component, %s: Idempotency-Key found %t, required %t; want found %t, not required", name, found, required, takes)
			}
			if _, refused := operation.Responses["422"]; takes && !refused {
				t.Errorf("without the idempotency component, %s has no 422 response for a key used with another request", name)
			}
			// The handler keeps the key, so each response of the create and the patch, and no other, describes
			// Idempotency-Kept.
			for code, response := range operation.Responses {
				if _, kept := response.Headers["Idempotency-Kept"]; kept != takes {
					t.Errorf("without the idempotency component, the response %s of %s describes Idempotency-Kept %t, want %t", code, name, kept, takes)
				}
			}
		}
	})
}

func TestKeptKeyWithNoComponent(t *testing.T) {
	t.Run("ST-SUP-RC20-OC56-T02 With no idempotency component, the create and patch handlers mark each answer to a request with a key Idempotency-Kept", func(t *testing.T) {
		ctx := todoIdempotencyContext(types.OpCreate, types.OpPatch)
		ctx.PeerNamespaces = nil
		files, _, err := new(Generator).Generate(onRuntime(ctx))
		if err != nil {
			t.Fatal(err)
		}
		const mark = "if key != \"\" {\n\t\tw.Header().Set(idempotency.KeptHeader, idempotency.KeptValue)\n\t}\n"
		marks := 0
		for _, file := range files {
			marks += strings.Count(string(file.Bytes()), mark)
		}
		// One mark for the create and one for the patch, each right after the read of the key.
		if marks != 2 {
			t.Errorf("the handlers write Idempotency-Kept %d times, want 2: the create and the patch", marks)
		}
	})
	t.Run("ST-SUP-RC20-OC56-T03 With no idempotency component, a refused key of a create or a patch takes the problem type under the problem base, as a command does", func(t *testing.T) {
		keyTypes := func(t *testing.T, ctx gen.Context) string {
			t.Helper()
			files, _, err := new(Generator).Generate(onRuntime(ctx))
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				if file.Path != "internal/api/runtime.go" {
					continue
				}
				text := string(file.Bytes())
				start := strings.Index(text, "const (\n\tkeyMissingType")
				if start < 0 {
					t.Fatal("runtime.go declares no problem types of the idempotency key")
				}
				// keyMissing answers a malformed key with keyInvalidType and an absent one with keyMissingType.
				if !strings.Contains(text, "problem.Type = keyInvalidType") || !strings.Contains(text, "problem.Type = keyMissingType") {
					t.Fatal("the refusal of a key does not take the problem types of the key")
				}
				return strings.Join(strings.Fields(text[start:start+strings.Index(text[start:], ")")]), " ")
			}
			t.Fatal("no runtime.go is generated")
			return ""
		}
		ctx := todoIdempotencyContext(types.OpCreate, types.OpPatch)
		ctx.Idempotency, ctx.PeerNamespaces = nil, nil
		got := keyTypes(t, ctx)
		for name, slug := range map[string]string{"keyMissingType": "idempotency-key-missing", "keyInvalidType": "idempotency-key-invalid",
			"keyBusyType": "idempotency-key-in-progress", "keyReusedType": "idempotency-key-reused"} {
			if !strings.Contains(got, name+" = \""+todoProblemBase+slug+"\"") {
				t.Errorf("with no idempotency component, %s is not %s%s: %s", name, todoProblemBase, slug, got)
			}
		}
		// With no problem base, the types stay about:blank.
		ctx.ErrorTypeBase = ""
		if got := keyTypes(t, ctx); strings.Count(got, `"about:blank"`) != 4 {
			t.Errorf("with no problem base, the problem types of the key are not about:blank: %s", got)
		}
	})
}

func TestIdempotencyKeptHeaderDocument(t *testing.T) {
	t.Run("ST-SUP-RC30-OC08-T01 The OpenAPI document describes Idempotency-Kept on each response of a create and a patch, and on no other operation", func(t *testing.T) {
		operations := todoOperations(t, todoIdempotencyContext(types.OpCreate))
		for _, name := range []string{"POST /api/todo/v1/todos", "PATCH /api/todo/v1/todos/{id}"} {
			operation := operations[name]
			if len(operation.Responses) < 4 {
				t.Fatalf("%s has the responses %v, want a success and the idempotency problems", name, operation.Responses)
			}
			for code, response := range operation.Responses {
				if _, ok := response.Headers["Idempotency-Kept"]; !ok {
					t.Errorf("the response %s of %s does not describe Idempotency-Kept", code, name)
				}
			}
		}
		for _, name := range []string{"GET /api/todo/v1/todos", "GET /api/todo/v1/todos/{id}", "PUT /api/todo/v1/todos/{id}", "DELETE /api/todo/v1/todos/{id}"} {
			for code, response := range operations[name].Responses {
				if _, ok := response.Headers["Idempotency-Kept"]; ok {
					t.Errorf("the response %s of %s describes Idempotency-Kept", code, name)
				}
			}
		}
		// Without the idempotency component, ST-SUP-RC20-OC56-T01 proves the header on the create and the patch.
	})
}
