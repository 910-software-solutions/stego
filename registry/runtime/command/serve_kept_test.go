package command

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
)

func TestServeKeptHeader(t *testing.T) {
	t.Run("ST-SUP-RC32-OC27-T01 Each answer to a command request with a key carries Idempotency-Kept: 1, and an answer to a request with no key does not", func(t *testing.T) {
		e := newEnv(t)
		e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o7', 'open')`)
		cmd := e.command("reserve", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource = "reserve", orderResource()
		}, e.reservationE)
		shortStock := NewRefusal("reserve", "short_stock", http.StatusUnprocessableEntity, "reserve.short_stock")
		var current func(ctx context.Context, h *Handle[testProbe]) error
		spec := func(required bool) ServeSpec[*structpb.Struct, *structpb.Struct, testProbe] {
			return ServeSpec[*structpb.Struct, *structpb.Struct, testProbe]{
				Command: cmd, ResourceType: "order", SubjectType: "user", Status: http.StatusCreated,
				NewRequest:  func() *structpb.Struct { return &structpb.Struct{} },
				NewResponse: func() *structpb.Struct { return &structpb.Struct{} },
				Resource:    func(r *structpb.Struct) string { return r.GetFields()["order_id"].GetStringValue() },
				Fill: func(ctx context.Context, r *structpb.Struct, h *Handle[testProbe]) (*structpb.Struct, error) {
					if err := current(ctx, h); err != nil {
						return nil, err
					}
					return structpb.NewStruct(map[string]any{"done": true})
				},
				Refusals: []*Refusal{shortStock}, KeyRequired: required,
			}
		}
		required, err := Serve(e.rt, spec(true))
		if err != nil {
			t.Fatal(err)
		}
		optional, err := Serve(e.rt, spec(false))
		if err != nil {
			t.Fatal(err)
		}
		key := authntest.NewKey(t, "k1")
		set, err := authn.ParseKeys(authntest.Document(key.JWK()))
		if err != nil {
			t.Fatal(err)
		}
		verifier, err := authn.New(authn.Settings{Issuer: "https://issuer.example", Audience: "orders", ProblemCode: "ORDERS",
			Keys: set, TenantClaim: "tenant"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(verifier.Stop)
		claims := authntest.Claims("https://issuer.example", "orders", "u-8", time.Now())
		claims["tenant"] = "t1"
		token := authntest.Mint(t, authntest.Header("k1"), claims, key)
		identity, err := verifier.Verify(context.Background(), token)
		if err != nil {
			t.Fatal(err)
		}
		caller, err := authz.Person("user", identity)
		if err != nil {
			t.Fatal(err)
		}
		e.decision.setRelations(relation(t, caller.Subject().String(), "reserve", "t1", "order", "o7"))

		commit := func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-kept-" + NewID(), OrderID: "o7", SKU: "s"}, nil)
			return err
		}
		cases := []struct {
			name     string
			handler  http.Handler
			key      string
			body     string
			fill     func(ctx context.Context, h *Handle[testProbe]) error
			status   int
			replayed bool
			kept     bool
		}{
			{name: "a success", handler: required, key: `"k-kept-1"`, fill: commit, status: http.StatusCreated, kept: true},
			{name: "the stored answer given again", handler: required, key: `"k-kept-1"`, fill: commit, status: http.StatusCreated, replayed: true, kept: true},
			{name: "a declared refusal", handler: required, key: `"k-kept-2"`, status: http.StatusUnprocessableEntity, kept: true,
				fill: func(context.Context, *Handle[testProbe]) error { return shortStock }},
			{name: "a 5xx answer", handler: required, key: `"k-kept-3"`, status: http.StatusInternalServerError, kept: true,
				fill: func(context.Context, *Handle[testProbe]) error { return errors.New("the stock is gone") }},
			{name: "a 503 answer", handler: required, key: `"k-kept-4"`, status: http.StatusServiceUnavailable, kept: true,
				fill: func(context.Context, *Handle[testProbe]) error { return unavailablef("the statement failed") }},
			{name: "a body that is not valid", handler: required, key: `"k-kept-5"`, body: `{"order_id":`, status: http.StatusBadRequest, kept: true,
				fill: commit},
			{name: "a key used with another body", handler: required, key: `"k-kept-1"`, body: `{"order_id":"o7","note":"other"}`,
				status: http.StatusUnprocessableEntity, kept: true, fill: commit},
			{name: "a required key that is missing", handler: required, fill: commit, status: http.StatusBadRequest},
			{name: "a malformed key", handler: required, key: `k-kept-6`, fill: commit, status: http.StatusBadRequest},
			{name: "a success with no key", handler: optional, fill: commit, status: http.StatusCreated},
			{name: "a 5xx answer with no key", handler: optional, status: http.StatusInternalServerError,
				fill: func(context.Context, *Handle[testProbe]) error { return errors.New("the stock is gone") }},
			{name: "a success with a key that is optional", handler: optional, key: `"k-kept-7"`, fill: commit, status: http.StatusCreated, kept: true},
		}
		for _, c := range cases {
			current = c.fill
			body := c.body
			if body == "" {
				body = `{"order_id":"o7"}`
			}
			r := httptest.NewRequest(http.MethodPost, "/commands/reserve", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+token)
			if c.key != "" {
				r.Header.Set(idempotency.KeyHeader, c.key)
			}
			w := httptest.NewRecorder()
			verifier.Middleware(c.handler).ServeHTTP(w, r)
			if w.Code != c.status {
				t.Fatalf("%s gave %d %s, want %d", c.name, w.Code, w.Body.String(), c.status)
			}
			if replayed := w.Header().Get(idempotency.ReplayedHeader) == "true"; replayed != c.replayed {
				t.Errorf("%s gave Idempotent-Replayed %t, want %t", c.name, replayed, c.replayed)
			}
			if got := w.Header().Values(idempotency.KeptHeader); (c.kept && (len(got) != 1 || got[0] != "1")) || (!c.kept && len(got) != 0) {
				t.Errorf("%s gave Idempotency-Kept %q, want it %t", c.name, got, c.kept)
			}
		}
	})
}
