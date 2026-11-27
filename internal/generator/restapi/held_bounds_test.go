package restapi

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	legacyrouter "github.com/getkin/kin-openapi/routers/legacy"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/types"
)

// widenedContext gives the crud context with Meeting.seats retyped from int32 to an int64 with min 0 and no max, at
// the expand step, as gen.HeldBounds gives it to the generators: the field holds the int32 range until the contract
// step (ST-DECL-Q48). contracted gives it after the contract step, with no held bound.
func widenedContext(contracted bool) gen.Context {
	ctx := crudContext()
	zero := 0.0
	changes := []types.SchemaChange{{ID: "meeting-seats-retype", Kind: "retype", Entity: "Meeting", Field: "seats", From: "int32",
		Definition: "int32, min 0, default 8", Conversion: "integer_to_int64", InverseConversion: "int64_to_integer"}}
	history := []types.SchemaStep{{Change: "meeting-seats-retype", Phase: "expand"}}
	if contracted {
		history = append(history, types.SchemaStep{Change: "meeting-seats-retype", Phase: "contract"})
	}
	for i := range ctx.Entities {
		if ctx.Entities[i].Name != "Meeting" {
			continue
		}
		fields := append([]types.Field(nil), ctx.Entities[i].Fields...)
		for j := range fields {
			if fields[j].Name == "seats" {
				fields[j].Type, fields[j].Min = types.FieldTypeInt64, &zero
			}
		}
		ctx.Entities[i].Fields = fields
	}
	ctx.Entities = gen.HeldBounds(ctx.Entities, changes, history)
	return ctx
}

func TestHeldBoundsDocument(t *testing.T) {
	t.Run("ST-GEN-UC12-R03-T06 The OpenAPI document of a release with an open widening holds the int32 maximum, and POST and PUT above it are 400", func(t *testing.T) {
		// Security review of PR #499, I2: only PATCH had a test of the 400 above int32.
		const item = "/api/v1/meetings/0b9c6a5e-6f7a-4c1e-9a52-2f1d3c4b5a69"
		body := func(seats string) string {
			return `{"title":"t","starts_at":"2026-12-01T08:00:00Z","seats":` + seats + `}`
		}
		for _, contracted := range []bool{false, true} {
			data := crudFiles(t, widenedContext(contracted))["api/openapi.json"]
			var document struct {
				Components struct {
					Schemas map[string]struct {
						Properties map[string]struct {
							Format  string   `json:"format"`
							Minimum *float64 `json:"minimum"`
							Maximum *float64 `json:"maximum"`
						} `json:"properties"`
					} `json:"schemas"`
				} `json:"components"`
			}
			if err := json.Unmarshal(data, &document); err != nil {
				t.Fatal(err)
			}
			for _, schema := range []string{"Meeting", "MeetingCreateRequest"} {
				seats, ok := document.Components.Schemas[schema].Properties["seats"]
				if !ok || seats.Format != "int64" || seats.Minimum == nil || *seats.Minimum != 0 {
					t.Fatalf("contract %t: the schema %s gives seats %+v, want an int64 with minimum 0", contracted, schema, seats)
				}
				if held := seats.Maximum != nil && *seats.Maximum == math.MaxInt32; held == contracted || (contracted && seats.Maximum != nil) {
					t.Fatalf("contract %t: the schema %s gives seats the maximum %v, want 2147483647 only until the contract", contracted, schema, seats.Maximum)
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
			for _, request := range []struct{ method, path string }{{http.MethodPost, "/api/v1/meetings"}, {http.MethodPut, item}} {
				if err := check(request.method, request.path, body("2147483647")); err != nil {
					t.Errorf("contract %t: %s %s with seats 2147483647 is refused: %v", contracted, request.method, request.path, err)
				}
				for _, seats := range []string{"2147483648", "9223372036854775807"} {
					if err := check(request.method, request.path, body(seats)); (err == nil) != contracted {
						t.Errorf("contract %t: %s %s with seats %s gave %v, want a refusal only until the contract", contracted, request.method, request.path, seats, err)
					}
				}
			}
		}
	})
}
