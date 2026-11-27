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

	"github.com/910-software-solutions/stego/internal/types"
)

// keptSchema is what the test reads of one schema of the OpenAPI document.
type keptSchema struct {
	Required   []string `json:"required"`
	Properties map[string]struct {
		ReadOnly    bool   `json:"readOnly"`
		WriteOnly   bool   `json:"writeOnly"`
		Description string `json:"description"`
		Default     any    `json:"default"`
		Kept        bool   `json:"x-kept"`
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
		// A secret is writeOnly, and never also readOnly; a kept secret says that an update must leave it out.
		if e, p := ticket.Properties["pin"], update.Properties["pin"]; e.ReadOnly || !e.WriteOnly || !strings.Contains(e.Description, "An update must leave it out") ||
			p.ReadOnly || !p.WriteOnly || !strings.Contains(p.Description, "must leave it out") || strings.Contains(p.Description, "empty") {
			t.Errorf("the secret kept field pin is %+v in the entity schema and %+v in the update-request schema, want writeOnly and not readOnly in each, and a description that an update must leave it out", e, p)
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
			if !ok || u.ReadOnly || !u.Kept || u.Default != nil || slices.Contains(update.Required, name) || !strings.Contains(u.Description, "may leave it out or give its stored value") {
				t.Errorf("the update-request schema gives %s %+v (present %v, required %v), want an optional member, not readOnly, with no default, that says it may only repeat the stored value", name, u, ok, update.Required)
			}
		}
		for _, name := range []string{"title", "code", "folder_id"} {
			if ticket.Properties[name].ReadOnly || !slices.Contains(update.Required, name) {
				t.Errorf("%s is readOnly in the entity schema or optional in the update-request schema", name)
			}
		}
		// A create stores the default of a never field: the body may leave it out or give the default
		// (ST-SUP-RC20-OC45, ST-SUP-Q195).
		for _, name := range []string{"state", "rank"} {
			if c, ok := create.Properties[name]; !ok || !c.Kept || c.ReadOnly || c.Default != nil || slices.Contains(create.Required, name) || !strings.Contains(c.Description, "give its default") {
				t.Errorf("the create-request schema gives the never field %s %+v (present %v), want an optional x-kept member with no default", name, c, ok)
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

func TestDefaultedCreateField(t *testing.T) {
	t.Run("ST-SUP-RC20-OC47-T02 The create-request schema does not require a field with a declared default, and a create that leaves it out passes the request check", func(t *testing.T) {
		ctx := crudContext()
		for i, e := range ctx.Entities {
			if e.Name == "Order" {
				// Walkthrough 18, step 38: Listing.stock, an int32 with min 0 and default 0.
				ctx.Entities[i].Fields = append(ctx.Entities[i].Fields, types.Field{Name: "stock", Type: types.FieldTypeInt32, Default: 0},
					types.Field{Name: "region", Type: types.FieldTypeString, Default: "eu"})
			}
		}
		data := crudFiles(t, ctx)["api/openapi.json"]
		var document struct {
			Components struct {
				Schemas map[string]keptSchema `json:"schemas"`
			} `json:"components"`
		}
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		order, create := document.Components.Schemas["Order"], document.Components.Schemas["OrderCreateRequest"]
		for name, want := range map[string]any{"stock": float64(0), "region": "eu"} {
			p, ok := create.Properties[name]
			if !ok || slices.Contains(create.Required, name) || p.Default != want || p.Kept || p.ReadOnly {
				t.Errorf("the create-request schema gives %s %+v (present %v, required %v), want an optional member with the default %v", name, p, ok, create.Required, want)
			}
			// Each row has the field, so the entity schema of the answers requires it.
			if !slices.Contains(order.Required, name) {
				t.Errorf("the entity schema does not require %s: %v", name, order.Required)
			}
		}
		for _, name := range []string{"sku", "qty"} {
			if !slices.Contains(create.Required, name) {
				t.Errorf("the create-request schema does not require %s, which has no default: %v", name, create.Required)
			}
		}
		doc, err := openapi3.NewLoader().LoadFromData(data)
		if err != nil {
			t.Fatal(err)
		}
		doc.Servers = nil
		router, err := legacyrouter.NewRouter(doc)
		if err != nil {
			t.Fatal(err)
		}
		check := func(body string) error {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/orders", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			route, params, err := router.FindRoute(r)
			if err != nil {
				t.Fatal(err)
			}
			return openapi3filter.ValidateRequestBody(context.Background(), &openapi3filter.RequestValidationInput{Request: r, PathParams: params, Route: route,
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, MultiError: true}}, route.Operation.RequestBody.Value)
		}
		if err := check(`{"sku":"s1","qty":1}`); err != nil {
			t.Errorf("the create-request check refuses a create that leaves out the defaulted fields: %v", err)
		}
		if err := check(`{"sku":"s1"}`); err == nil {
			t.Error("the create-request check passes a create that leaves out qty, which has no default")
		}
	})
}

func TestStrictBodies(t *testing.T) {
	t.Run("ST-SUP-RC20-OC55-T02 The OpenAPI document closes the create and update requests, and the request check and the decode refuse a member that they do not take", func(t *testing.T) {
		files := crudFiles(t, crudContext())
		data := files["api/openapi.json"]
		var document struct {
			Components struct {
				Schemas map[string]map[string]any `json:"schemas"`
			} `json:"components"`
		}
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		// Each member of the entity, kind, id and href, and no other: a client never writes a member that the server
		// sets (x-output-only) or that it may only repeat (x-kept).
		want := []string{"code", "due_at", "folder_id", "href", "id", "kind", "meta", "pin", "rank", "recovery", "sku", "state", "title"}
		for _, name := range []string{"TicketCreateRequest", "TicketUpdateRequest"} {
			request := document.Components.Schemas[name]
			properties, _ := request["properties"].(map[string]any)
			kind, _ := properties["kind"].(map[string]any)
			names := slices.Sorted(func(yield func(string) bool) {
				for name := range properties {
					if !yield(name) {
						return
					}
				}
			})
			if request["additionalProperties"] != false || !slices.Equal(names, want) || len(kind["enum"].([]any)) != 1 || kind["enum"].([]any)[0] != "Ticket" {
				t.Errorf("%s is %v, want the members %v, kind Ticket and additionalProperties false", name, request, want)
			}
			for _, member := range []string{"id", "href"} {
				notWritten(t, request, member, "x-output-only")
			}
			notWritten(t, request, "state", "x-kept")
		}

		// The request check of the service refuses an unknown member, also in another letter case, and names it; it
		// takes a read row sent back, with each member that the server sets, null too.
		loader := openapi3.NewLoader()
		doc, err := loader.LoadFromData(data)
		if err != nil {
			t.Fatal(err)
		}
		doc.Servers = nil
		router, err := legacyrouter.NewRouter(doc)
		if err != nil {
			t.Fatal(err)
		}
		check := func(method, path, body string) error {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			route, params, err := router.FindRoute(r)
			if err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
			return openapi3filter.ValidateRequestBody(context.Background(), &openapi3filter.RequestValidationInput{Request: r, PathParams: params, Route: route,
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, MultiError: true}}, route.Operation.RequestBody.Value)
		}
		const item = "/api/v1/tickets/0b9c6a5e-6f7a-4c1e-9a52-2f1d3c4b5a69"
		body := `{"folder_id":"f","code":"c1","title":"t","sku":"s1"`
		for _, c := range []struct{ method, path string }{{http.MethodPost, "/api/v1/tickets"}, {http.MethodPut, item}, {http.MethodPut, "/api/v1/tickets"}} {
			for _, member := range []string{"Title", "titel", "tenant_id"} {
				if err := check(c.method, c.path, body+`,"`+member+`":"x"}`); err == nil || !strings.Contains(err.Error(), member) {
					t.Errorf("the request check of %s %s passes the member %s (%v), want it refused by name", c.method, c.path, member, err)
				}
			}
			if err := check(c.method, c.path, body+`,"kind":"Ticket","id":"0b9c6a5e-6f7a-4c1e-9a52-2f1d3c4b5a69","href":null}`); err != nil {
				t.Errorf("the request check of %s %s refuses kind, id and href as a read gives them: %v", c.method, c.path, err)
			}
		}

		// The decode refuses each member that is not kind, id, href or a field, in its exact letter case, before it
		// decodes the body, and an implicit field with another value than that of the collection.
		inOrder(t, "decodeTickets", handlerFunc(t, files, "tickets", "decodeTickets"),
			`case "kind", "id", "href", "folder_id", "code", "title", "sku", "state", "meta", "due_at", "rank", "pin", "recovery":`,
			"if unknown != \"\" {", "Validation([]ValidationError{{Field: memberName(unknown),", "json.Unmarshal(bodyBytes, &ticket)")
		inOrder(t, "decodeRedEvents", handlerFunc(t, files, "red-events", "decodeRedEvents"),
			"duplicateMember(bodyBytes)",
			`if raw, ok := rawMap["source"]; ok && string(raw) != "null" {`, `value != "red"`, `Validation([]ValidationError{{Field: "source",`,
			`delete(rawMap, member)`, "json.Unmarshal(bodyBytes, &event)", `event.Source != "" && event.Source != "red"`)
		inOrder(t, "decodeTicketsPatch", handlerFunc(t, files, "tickets", "decodeTicketsPatch"), "duplicateMember(bodyBytes)", "if unknown != \"\" {")
	})
}
