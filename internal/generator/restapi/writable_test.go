package restapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	legacyrouter "github.com/getkin/kin-openapi/routers/legacy"

	"github.com/jsell-rh/stego/internal/types"
)

// keptSchema is what the test reads of one schema of the OpenAPI document.
type keptSchema struct {
	Required   []string `json:"required"`
	Properties map[string]struct {
		ReadOnly    bool   `json:"readOnly"`
		Description string `json:"description"`
		Default     any    `json:"default"`
	} `json:"properties"`
}

func TestKeptDocument(t *testing.T) {
	t.Run("ST-SUP-RC20-OC47-T01 The OpenAPI document marks the kept fields readOnly in the entity schema only, and a read row sent back passes the update-request check", func(t *testing.T) {
		data := crudFiles(t, crudContext())["api/openapi.json"]
		var document struct {
			Paths map[string]map[string]struct {
				RequestBody struct {
					Content map[string]struct {
						Schema struct {
							Ref string `json:"$ref"`
						} `json:"schema"`
					} `json:"content"`
				} `json:"requestBody"`
			} `json:"paths"`
			Components struct {
				Schemas map[string]keptSchema `json:"schemas"`
			} `json:"components"`
		}
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		schemas := document.Components.Schemas
		ticket, create, update := schemas["Ticket"], schemas["TicketCreateRequest"], schemas["TicketUpdateRequest"]
		kept := []string{"sku", "state", "meta", "due_at", "rank"}
		if p := update.Properties["pin"]; !ticket.Properties["pin"].ReadOnly || p.ReadOnly || !strings.Contains(p.Description, "must leave it out") {
			t.Errorf("the secret kept field pin is %+v in the entity schema and %+v in the update-request schema, want readOnly there and a description that the body must leave it out here", ticket.Properties["pin"], p)
		}
		for _, name := range kept {
			p := ticket.Properties[name]
			when := "A create sets this field"
			if name == "state" || name == "rank" {
				when = "Only a command or a worker sets this field"
			}
			if !p.ReadOnly || !strings.Contains(p.Description, when) {
				t.Errorf("the entity schema gives %s %+v, want readOnly and the description %q", name, p, when)
			}
			u, ok := update.Properties[name]
			if !ok || u.ReadOnly || u.Default != nil || slices.Contains(update.Required, name) || !strings.Contains(u.Description, "may leave it out or give its stored value") {
				t.Errorf("the update-request schema gives %s %+v (present %v, required %v), want an optional member, not readOnly, with no default, that says it may only repeat the stored value", name, u, ok, update.Required)
			}
		}
		for _, name := range []string{"title", "code", "folder_id"} {
			if ticket.Properties[name].ReadOnly || !slices.Contains(update.Required, name) {
				t.Errorf("%s is readOnly in the entity schema or optional in the update-request schema", name)
			}
		}
		for _, name := range []string{"state", "rank"} {
			if _, ok := create.Properties[name]; ok {
				t.Errorf("the create-request schema holds the never field %s", name)
			}
		}
		if !slices.Contains(create.Required, "sku") || create.Properties["sku"].ReadOnly || create.Properties["meta"].ReadOnly {
			t.Errorf("the create-request schema gives sku %+v and required %v, want the create field sku required and no readOnly member", create.Properties["sku"], create.Required)
		}
		for name, schema := range schemas {
			for property, p := range schema.Properties {
				if p.ReadOnly && name != "Ticket" && property != "id" {
					t.Errorf("the schema %s marks %s readOnly", name, property)
				}
			}
		}
		ref := func(path, method string) string {
			return document.Paths[path][method].RequestBody.Content["application/json"].Schema.Ref
		}
		if got := ref("/api/v1/tickets/{id}", "put"); got != "#/components/schemas/TicketUpdateRequest" {
			t.Errorf("the update of tickets takes %s, want TicketUpdateRequest", got)
		}
		if got := ref("/api/v1/tickets", "put"); got != "#/components/schemas/TicketCreateRequest" {
			t.Errorf("the upsert of tickets takes %s, want TicketCreateRequest", got)
		}
		// An entity with no kept field keeps one request schema for the create and the update.
		if _, ok := schemas["TodoUpdateRequest"]; ok || ref("/api/v1/todos/{id}", "put") != "#/components/schemas/TodoCreateRequest" {
			t.Error("the todos, with no kept field, have an update-request schema")
		}

		// The request check of the service, as validation.go runs it, passes a read row sent back and a body that
		// leaves out each kept field, and writes no default of a kept field into the body.
		loader := openapi3.NewLoader()
		doc, err := loader.LoadFromData(data)
		if err != nil {
			t.Fatal(err)
		}
		doc.Servers = nil
		if err := doc.Validate(context.Background()); err != nil {
			t.Fatal(err)
		}
		router, err := legacyrouter.NewRouter(doc)
		if err != nil {
			t.Fatal(err)
		}
		check := func(method, path, body string) ([]byte, error) {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			route, params, err := router.FindRoute(r)
			if err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
			err = openapi3filter.ValidateRequestBody(context.Background(), &openapi3filter.RequestValidationInput{Request: r, PathParams: params, Route: route,
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, MultiError: true}}, route.Operation.RequestBody.Value)
			checked, _ := io.ReadAll(r.Body)
			return checked, err
		}
		const item = "/api/v1/tickets/0b9c6a5e-6f7a-4c1e-9a52-2f1d3c4b5a69"
		row := `{"kind":"Ticket","id":"0b9c6a5e-6f7a-4c1e-9a52-2f1d3c4b5a69","href":"/api/v1/tickets/0b9c6a5e-6f7a-4c1e-9a52-2f1d3c4b5a69","folder_id":"f","code":"c1","title":"t","sku":"s1","state":"closed","meta":{"a":1},"due_at":"2026-12-01T08:00:00Z","rank":3}`
		if _, err := check(http.MethodPut, item, row); err != nil {
			t.Errorf("the update-request check refuses a read row sent back: %v", err)
		}
		checked, err := check(http.MethodPut, item, `{"folder_id":"f","code":"c1","title":"t"}`)
		if err != nil {
			t.Errorf("the update-request check refuses a body that leaves out each kept field: %v", err)
		}
		if bytes.Contains(checked, []byte("state")) {
			t.Errorf("the update-request check wrote a kept field into the body: %s", checked)
		}
		if _, err := check(http.MethodPut, item, `{"folder_id":"f","code":"c1","title":"t","state":"done"}`); err == nil {
			t.Error("the update-request check passes a state that is not of its values")
		}
		if _, err := check(http.MethodPost, "/api/v1/tickets", `{"folder_id":"f","code":"c1","title":"t"}`); err == nil {
			t.Error("the create-request check passes a create that leaves out the create field sku")
		}
	})
	t.Run("ST-SUP-RC20-OC45-T02 Generation refuses a never field whose default it cannot write", func(t *testing.T) {
		ctx := crudContext()
		for i, e := range ctx.Entities {
			if e.Name == "Ticket" {
				ctx.Entities[i].Fields = append(ctx.Entities[i].Fields, types.Field{Name: "extra", Type: types.FieldTypeJsonb, Optional: true, Default: "{}", Writable: types.WritableNever})
			}
		}
		if files, _, err := new(Generator).Generate(ctx); err == nil || files != nil || !strings.Contains(err.Error(), "extra") {
			t.Errorf("generation gave %d files and %v, want a refusal that names the field extra", len(files), err)
		}
	})
}
