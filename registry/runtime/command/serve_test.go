package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/otel"
)

// serveCase is one fill outcome of the Serve test: what the fill does, and the answer and audit row it gives.
type serveCase struct {
	name     string
	resource string
	fill     func(ctx context.Context, h *Handle[testProbe]) error
	status   int
	code     string
	rpc      codes.Code
	outcome  string
	auditErr string
}

func TestServe(t *testing.T) {
	t.Run("ST-SUP-RP10-LC33-T01 Serve and ServeRPC map each outcome of Run to its answer and audit row", func(t *testing.T) {
		e := newEnv(t)
		e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o5', 'open'), ('t1', 'o6', 'open')`)
		cmd := e.command("reserve", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource = "reserve", orderResource()
		}, e.reservationE)
		shortStock := NewRefusal("reserve", "short_stock", http.StatusUnprocessableEntity, "reserve.short_stock")
		const secret = "jane.doe@example.com"
		var current func(ctx context.Context, h *Handle[testProbe]) error
		spec := ServeSpec[*structpb.Struct, *structpb.Struct, testProbe]{
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
			Refusals: []*Refusal{shortStock}, KeyRequired: true,
		}
		h, err := Serve(e.rt, spec)
		if err != nil {
			t.Fatal(err)
		}
		rpc, err := ServeRPC(e.rt, spec)
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
		claims := authntest.Claims("https://issuer.example", "orders", "u-7", time.Now())
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
		// The caller may reserve on o5 only.
		e.decision.setRelations(relation(t, caller.Subject().String(), "reserve", "t1", "order", "o5"))

		cases := []serveCase{
			{name: "commit", resource: "o5", status: http.StatusCreated, rpc: codes.OK, outcome: "ok",
				fill: func(ctx context.Context, h *Handle[testProbe]) error {
					_, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-serve-" + NewID(), OrderID: "o5", SKU: "s"}, nil)
					return err
				}},
			{name: "declared refusal", resource: "o5", status: http.StatusUnprocessableEntity, code: "reserve.short_stock",
				rpc: codes.InvalidArgument, outcome: "failed", auditErr: "reserve.short_stock",
				fill: func(context.Context, *Handle[testProbe]) error { return shortStock }},
			{name: "deny", resource: "o6", status: http.StatusNotFound, code: "denied", rpc: codes.NotFound, outcome: "denied",
				auditErr: "denied", fill: func(context.Context, *Handle[testProbe]) error { return nil }},
			{name: "conflict", resource: "o5", status: http.StatusConflict, code: "conflict", rpc: codes.Aborted,
				outcome: "failed", auditErr: "conflict",
				fill: func(context.Context, *Handle[testProbe]) error { return &conflictError{text: "moved"} }},
			{name: "commit of unknown outcome", resource: "o5", status: http.StatusInternalServerError, code: "internal",
				rpc: codes.Internal, outcome: "failed", auditErr: "commit_outcome_unknown",
				fill: func(context.Context, *Handle[testProbe]) error { return ErrCommitUnknown }},
			{name: "another error", resource: "o5", status: http.StatusInternalServerError, code: "internal",
				rpc: codes.Internal, outcome: "failed", auditErr: "internal",
				fill: func(context.Context, *Handle[testProbe]) error { return errors.New("no stock for " + secret) }},
			// A permission that the provider could not decide now, such as past the decision time limit, and a
			// database that cannot serve now are 503 with Retry-After and Unavailable over gRPC (walkthrough 10, B5
			// and B7).
			{name: "a permission that could not be decided now", resource: "o5", status: http.StatusServiceUnavailable,
				code: "undecided", rpc: codes.Unavailable, outcome: "failed", auditErr: "undecided",
				fill: func(context.Context, *Handle[testProbe]) error { return ErrUndecided }},
			{name: "a database that cannot serve now", resource: "o5", status: http.StatusServiceUnavailable,
				code: "unavailable", rpc: codes.Unavailable, outcome: "failed", auditErr: "unavailable",
				fill: func(context.Context, *Handle[testProbe]) error { return unavailablef("the statement failed") }},
			{name: "a refusal that the fill made itself", resource: "o5", status: http.StatusInternalServerError,
				code: "internal", rpc: codes.Internal, outcome: "failed", auditErr: "internal",
				fill: func(context.Context, *Handle[testProbe]) error {
					return NewRefusal("reserve", "short_stock", http.StatusUnprocessableEntity, "reserve.short_stock")
				}},
		}
		lastAudit := func() (outcome, code string) {
			t.Helper()
			if err := e.admin.QueryRowContext(e.ctx, `SELECT outcome, coalesce(error_code, '') FROM audit_event
 WHERE tenant_id = 't1' AND action = 'reserve' ORDER BY seq DESC LIMIT 1`).Scan(&outcome, &code); err != nil {
				t.Fatal(err)
			}
			return outcome, code
		}
		for i, c := range cases {
			current = c.fill
			before := e.auditRows("t1", "reserve")
			// HTTP, through the verifier, with a place for the code of the completion log.
			body := `{"order_id":"` + c.resource + `"}`
			r := httptest.NewRequest(http.MethodPost, "/commands/reserve", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Idempotency-Key", `"k-serve-http-`+string(rune('a'+i))+`"`)
			w := httptest.NewRecorder()
			var logged string
			verifier.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := otel.WithErrorCode(r.Context())
				h.ServeHTTP(w, r.WithContext(ctx))
				logged, _ = otel.ErrorCode(ctx)
			})).ServeHTTP(w, r)
			answer, _ := io.ReadAll(w.Result().Body)
			if w.Code != c.status || strings.Contains(string(answer), secret) {
				t.Fatalf("%s over HTTP gave %d %s, want %d with no error text", c.name, w.Code, answer, c.status)
			}
			if retry := w.Header().Get("Retry-After"); (c.status == http.StatusServiceUnavailable) != (retry != "") {
				t.Fatalf("%s over HTTP gave Retry-After %q with %d, want it only with 503", c.name, retry, w.Code)
			}
			if c.code != "" {
				var p map[string]any
				if err := json.Unmarshal(answer, &p); err != nil || p["code"] != c.code || len(p) != 4 || logged != c.code {
					t.Fatalf("%s over HTTP gave the problem %s and the log code %q, want the code %s", c.name, answer, logged, c.code)
				}
			} else if logged != "" {
				t.Fatalf("%s over HTTP gave the log code %q, want none", c.name, logged)
			}
			if outcome, code := lastAudit(); e.auditRows("t1", "reserve") != before+1 || outcome != c.outcome || code != c.auditErr {
				t.Fatalf("%s over HTTP left the audit row %s %q (%d rows), want one row %s %q", c.name, outcome, code,
					e.auditRows("t1", "reserve")-before, c.outcome, c.auditErr)
			}

			// gRPC, with the identity of authn.Authenticate and the key in the request metadata.
			ctx, err := verifier.Authenticate(context.Background(), "Bearer "+token)
			if err != nil {
				t.Fatal(err)
			}
			ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(RPCKeyMetadata, `"k-serve-rpc-`+string(rune('a'+i))+`"`))
			request, _ := structpb.NewStruct(map[string]any{"order_id": c.resource})
			before = e.auditRows("t1", "reserve")
			response, err := rpc(ctx, request)
			if status.Code(err) != c.rpc || (err != nil && strings.Contains(err.Error(), secret)) {
				t.Fatalf("%s over gRPC gave %v, want %s with no error text", c.name, err, c.rpc)
			}
			if err == nil && !response.GetFields()["done"].GetBoolValue() {
				t.Fatalf("%s over gRPC gave the response %v", c.name, response)
			}
			if c.code != "" && status.Convert(err).Message() != c.code {
				t.Fatalf("%s over gRPC gave the message %q, want the code %s", c.name, status.Convert(err).Message(), c.code)
			}
			if outcome, code := lastAudit(); e.auditRows("t1", "reserve") != before+1 || outcome != c.outcome || code != c.auditErr {
				t.Fatalf("%s over gRPC left the audit row %s %q, want %s %q", c.name, outcome, code, c.outcome, c.auditErr)
			}
		}

		// A request with no token never reaches Run.
		calls := 0
		current = func(context.Context, *Handle[testProbe]) error { calls++; return nil }
		before := e.auditRows("t1", "")
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/commands/reserve", strings.NewReader(`{"order_id":"o5"}`))
		r.Header.Set("Idempotency-Key", `"k-serve-none"`)
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("a request with no token gave %d, want 401", w.Code)
		}
		if _, err := rpc(context.Background(), &structpb.Struct{}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("a call with no token gave %v, want UNAUTHENTICATED", err)
		}
		if calls != 0 || e.auditRows("t1", "") != before {
			t.Fatal("a request with no token reached the fill or wrote an audit row")
		}
		// A required key that is missing is 400, and a spec that lacks a part is refused.
		r = httptest.NewRequest(http.MethodPost, "/commands/reserve", strings.NewReader(`{"order_id":"o5"}`))
		r.Header.Set("Authorization", "Bearer "+token)
		w = httptest.NewRecorder()
		verifier.Middleware(h).ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || calls != 0 {
			t.Fatalf("a request with no key gave %d, want 400 with no fill call", w.Code)
		}
		// The problem type of a missing key sits under the problem base of the service (ST-SUP-RC30-OC02).
		e.rt.cfg.ProblemBase = "https://errors.example.com/orders/"
		r = httptest.NewRequest(http.MethodPost, "/commands/reserve", strings.NewReader(`{"order_id":"o5"}`))
		r.Header.Set("Authorization", "Bearer "+token)
		w = httptest.NewRecorder()
		verifier.Middleware(h).ServeHTTP(w, r)
		if !strings.Contains(w.Body.String(), `"type":"https://errors.example.com/orders/idempotency-key-missing"`) {
			t.Fatalf("a request with no key gave the problem %s, want the type under the problem base", w.Body.String())
		}
		t.Run("ST-SUP-RC30-OC02-T02 A command whose Idempotency-Key is not a structured string gets idempotency-key-invalid over HTTP and gRPC", func(t *testing.T) {
			for _, value := range []string{`abc`, `""`, `"` + strings.Repeat("k", 256) + `"`} {
				r := httptest.NewRequest(http.MethodPost, "/commands/reserve", strings.NewReader(`{"order_id":"o5"}`))
				r.Header.Set("Authorization", "Bearer "+token)
				r.Header.Set(idempotency.KeyHeader, value)
				w := httptest.NewRecorder()
				verifier.Middleware(h).ServeHTTP(w, r)
				if w.Code != http.StatusBadRequest || calls != 0 || !strings.Contains(w.Body.String(), `"type":"https://errors.example.com/orders/idempotency-key-invalid"`) {
					t.Fatalf("the key %.20s gave %d %s, want 400 idempotency-key-invalid with no fill call", value, w.Code, w.Body.String())
				}
				ctx, err := verifier.Authenticate(context.Background(), "Bearer "+token)
				if err != nil {
					t.Fatal(err)
				}
				ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(RPCKeyMetadata, value))
				request, _ := structpb.NewStruct(map[string]any{"order_id": "o5"})
				if _, err := rpc(ctx, request); status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != idempotency.ProblemInvalid || calls != 0 {
					t.Fatalf("the key %.20s over gRPC gave %v, want INVALID_ARGUMENT %s with no fill call", value, err, idempotency.ProblemInvalid)
				}
			}
			ctx, err := verifier.Authenticate(context.Background(), "Bearer "+token)
			if err != nil {
				t.Fatal(err)
			}
			request, _ := structpb.NewStruct(map[string]any{"order_id": "o5"})
			if _, err := rpc(ctx, request); status.Convert(err).Message() != idempotency.ProblemMissing {
				t.Fatalf("a call with no key over gRPC gave %v, want %s", err, idempotency.ProblemMissing)
			}
		})
		e.rt.cfg.ProblemBase = ""
		broken := spec
		broken.Fill = nil
		if _, err := Serve(e.rt, broken); err == nil {
			t.Fatal("Serve took a spec with no fill")
		}
	})

	t.Run("ST-SUP-RP10-LC33-T01 The contract helpers that never change", func(t *testing.T) {
		seen := map[string]bool{}
		for range 100 {
			id := NewID()
			if !ValidUUID(id) || seen[id] || id[14] != '4' {
				t.Fatalf("NewID gave %q", id)
			}
			seen[id] = true
		}
		for _, bad := range []string{"", "o5", "0a1b2c3d-0a1b-4c3d-8e9f-0a1b2c3d4e5", "0a1b2c3d_0a1b-4c3d-8e9f-0a1b2c3d4e5f", "0a1b2c3d-0a1b-4c3d-8e9f-0a1b2c3d4e5g"} {
			if ValidUUID(bad) {
				t.Fatalf("ValidUUID took %q", bad)
			}
		}
		n := 5
		p := ClonePointer(&n)
		*p = 6
		if n != 5 || ClonePointer[int](nil) != nil || StateOf(&n) != any(5) || StateOf[int](nil) != nil {
			t.Fatal("ClonePointer or StateOf is wrong")
		}
		m := 5
		if !EqualPointer(&n, &m) || EqualPointer(&n, nil) || !EqualPointer[int](nil, nil) {
			t.Fatal("EqualPointer is wrong")
		}
		var field string
		if SetPath(&field, "o5") != nil || field != "o5" || !errors.Is(SetPath(&field, "o6"), ErrBadRequest) || !errors.Is(SetPath(&field, ""), ErrBadRequest) {
			t.Fatal("SetPath is wrong")
		}
		defer func() {
			if recover() == nil {
				t.Fatal("Must did not panic on an error")
			}
		}()
		Must(0, errors.New("refused"))
	})
}

func TestServeHelpers(t *testing.T) {
	t.Run("ST-SUP-RP10-LC34-T01 The bind, the outcome map and the outcome row serve a handler that decodes its own body", func(t *testing.T) {
		e := newEnv(t)
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
		mint := func(tenantID string) string {
			claims := authntest.Claims("https://issuer.example", "orders", "u-7", time.Now())
			if tenantID != "" {
				claims["tenant"] = tenantID
			}
			return "Bearer " + authntest.Mint(t, authntest.Header("k1"), claims, key)
		}

		// The bind gives the tenant of the verified claim and the principal of the subject type, and refuses a
		// context with no verified identity and an identity with no tenant claim.
		ctx, err := verifier.Authenticate(context.Background(), mint("t1"))
		if err != nil {
			t.Fatal(err)
		}
		identity, _ := authn.FromContext(ctx)
		caller, err := authz.Person("user", identity)
		if err != nil {
			t.Fatal(err)
		}
		b, err := BindIdentity(ctx, "user")
		if err != nil || b.Tenant.ID() != "t1" || b.Principal.Subject() != caller.Subject() {
			t.Fatalf("BindIdentity gave %+v, %v; want the tenant t1 and the caller's principal of the type user", b, err)
		}
		if _, err := BindIdentity(context.Background(), "user"); err == nil {
			t.Fatal("BindIdentity bound a context with no verified identity")
		}
		noTenant, err := verifier.Authenticate(context.Background(), mint(""))
		if err == nil {
			if _, err := BindIdentity(noTenant, "user"); err == nil {
				t.Fatal("BindIdentity bound an identity with no tenant claim")
			}
		}

		// The outcome map gives the status, the fixed code and the audit outcome of each error of Run, and no error
		// text; a refusal that is not declared is an other error.
		const secret = "jane.doe@example.com"
		declared := NewRefusal("reserve", "short_stock", http.StatusUnprocessableEntity, "reserve.short_stock")
		for _, c := range []struct {
			name   string
			err    error
			want   Outcome
			answer string
		}{
			{"declared refusal", fmt.Errorf("wrapped: %w", declared), Outcome{http.StatusUnprocessableEntity, "reserve.short_stock", audit.OutcomeFailed, "", ""}, "reserve.short_stock"},
			{"refusal not declared", NewRefusal("reserve", "short_stock", http.StatusUnprocessableEntity, "reserve.short_stock"), Outcome{http.StatusInternalServerError, ErrorCodeInternal, audit.OutcomeFailed, "", ""}, ErrorCodeInternal},
			{"deny", ErrDenied, Outcome{http.StatusNotFound, ErrorCodeDenied, audit.OutcomeDenied, "", ""}, ErrorCodeDenied},
			{"conflict", &conflictError{text: "moved"}, Outcome{http.StatusConflict, audit.ErrorCodeConflict, audit.OutcomeFailed, "", ""}, audit.ErrorCodeConflict},
			{"commit of unknown outcome", ErrCommitUnknown, Outcome{http.StatusInternalServerError, audit.ErrorCodeCommitOutcomeUnknown, audit.OutcomeFailed, "", ""}, ErrorCodeInternal},
			{"another error", errors.New("no stock for " + secret), Outcome{http.StatusInternalServerError, ErrorCodeInternal, audit.OutcomeFailed, "", ""}, ErrorCodeInternal},
			{"invalid value", fmt.Errorf("wrapped: %w", &invalidError{field: "owner_id", reason: reasonCreateOnly}), Outcome{http.StatusBadRequest, ErrorCodeInvalid, audit.OutcomeFailed, "owner_id", reasonCreateOnly}, ErrorCodeInvalid},
		} {
			got := OutcomeOf(c.err, declared)
			if got != c.want || got.AnswerCode() != c.answer || strings.Contains(fmt.Sprint(got), secret) {
				t.Fatalf("%s: OutcomeOf gave %+v with the answer code %q, want %+v and %q", c.name, got, got.AnswerCode(), c.want, c.answer)
			}
		}

		// The outcome row is written in its own transaction with the outcome and the fixed code, also when the
		// request's context has ended, and the code goes to the completion log.
		req := e.request("t1", u7(t), "k-helpers", `{}`)
		req.Resource = "o9"
		cancelled, cancel := context.WithCancel(otel.WithErrorCode(context.Background()))
		cancel()
		before := e.auditRows("t1", "reserve")
		if err := e.rt.RecordFailure(cancelled, req, "reserve", "order", OutcomeOf(ErrCommitUnknown)); err != nil {
			t.Fatalf("RecordFailure gave %v", err)
		}
		var outcome, code, resource string
		if err := e.admin.QueryRowContext(e.ctx, `SELECT outcome, coalesce(error_code, ''), resource_type || ':' || resource_id
 FROM audit_event WHERE tenant_id = 't1' AND action = 'reserve' ORDER BY seq DESC LIMIT 1`).Scan(&outcome, &code, &resource); err != nil {
			t.Fatal(err)
		}
		if e.auditRows("t1", "reserve") != before+1 || outcome != "failed" || code != audit.ErrorCodeCommitOutcomeUnknown || resource != "order:o9" {
			t.Fatalf("RecordFailure wrote %s %q on %s, want one row failed commit_outcome_unknown on order:o9", outcome, code, resource)
		}
		if logged, _ := otel.ErrorCode(cancelled); logged != ErrorCodeInternal {
			t.Fatalf("the completion log has the code %q, want %s", logged, ErrorCodeInternal)
		}
	})
}

// failingConn is a SpiceDB connection whose every call fails with err.
type failingConn struct{ err error }

func (c failingConn) Invoke(context.Context, string, any, any, ...grpc.CallOption) error {
	return c.err
}

func (c failingConn) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, c.err
}

func TestOutcomeTransient(t *testing.T) {
	t.Run("ST-SUP-RP10-LC34-T02 A decision that the provider cannot make now maps to 503 undecided, and a refusal of the provider stays 500", func(t *testing.T) {
		notServed, err := status.New(codes.FailedPrecondition, "object definition `comment` not found").WithDetails(
			&errdetails.ErrorInfo{Reason: v1.ErrorReason_ERROR_REASON_UNKNOWN_DEFINITION.String(), Domain: "authzed.com"})
		if err != nil {
			t.Fatal(err)
		}
		decide := func(cause error) error {
			provider, err := authz.NewSpiceDB(failingConn{err: cause})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.Evaluate(context.Background(), authz.EvaluationRequest{Subject: authz.Subject{Type: "user", ID: "u-7"},
				Action: authz.Action{Name: "view"}, Resource: authz.Resource{Type: "comment", ID: "t1/c1"}, Consistency: authz.Fresh()})
			if err == nil {
				t.Fatal("the failing provider decided")
			}
			// The command runtime wraps a provider error so (decisionError).
			return fmt.Errorf("command: decide: %w", err)
		}
		undecided := Outcome{Status: http.StatusServiceUnavailable, Code: audit.ErrorCodeUndecided, Audit: audit.OutcomeFailed}
		internal := Outcome{Status: http.StatusInternalServerError, Code: ErrorCodeInternal, Audit: audit.OutcomeFailed}
		unavailable := Outcome{Status: http.StatusServiceUnavailable, Code: ErrorCodeUnavailable, Audit: audit.OutcomeFailed}
		notFound := Outcome{Status: http.StatusNotFound, Code: ErrorCodeNotFound, Audit: audit.OutcomeFailed}
		denied := Outcome{Status: http.StatusNotFound, Code: ErrorCodeDenied, Audit: audit.OutcomeDenied}
		for _, c := range []struct {
			name string
			err  error
			want Outcome
		}{
			{"a command permission that could not be decided now", fmt.Errorf("wrapped: %w", ErrUndecided), undecided},
			{"a definition that SpiceDB does not serve yet", decide(notServed.Err()), undecided},
			{"a SpiceDB that does not answer", decide(status.Error(codes.Unavailable, "connection refused")), undecided},
			{"a SpiceDB call that ran out of time", decide(status.Error(codes.DeadlineExceeded, "deadline")), undecided},
			{"a refusal of SpiceDB", decide(status.Error(codes.FailedPrecondition, "the relation is not in the schema")), internal},
			{"a key that SpiceDB does not take", decide(status.Error(codes.PermissionDenied, "bad key")), internal},
			// A not-found error that also holds a cause of a 503 answer is 503, so that the caller tries again
			// (security review of PR 448, I2).
			{"a not-found error with a decision that could not be made now", errors.Join(fmt.Errorf("fill: %w", ErrNotFound), ErrUndecided), undecided},
			{"a not-found error with a SpiceDB that does not answer", errors.Join(ErrNotFound, decide(status.Error(codes.Unavailable, "connection refused"))), undecided},
			{"a not-found error of a database that cannot serve now", errors.Join(fmt.Errorf("fill: %w", ErrNotFound), ErrUnavailable), unavailable},
			{"a not-found error alone", fmt.Errorf("fill: %w", ErrNotFound), notFound},
			// A denial with the same causes gets the same answer as a not-found error (security review of PR 449,
			// INFO-1).
			{"a denial with a decision that could not be made now", errors.Join(fmt.Errorf("fill: %w", ErrDenied), ErrUndecided), undecided},
			{"a denial with a SpiceDB that does not answer", errors.Join(ErrDenied, decide(status.Error(codes.Unavailable, "connection refused"))), undecided},
			{"a denial of a database that cannot serve now", errors.Join(fmt.Errorf("fill: %w", ErrDenied), ErrUnavailable), unavailable},
			{"a denial with a refusal of SpiceDB", errors.Join(ErrDenied, decide(status.Error(codes.FailedPrecondition, "no relation"))), denied},
			{"a denial alone", fmt.Errorf("fill: %w", ErrDenied), denied},
		} {
			if got := OutcomeOf(c.err); got != c.want {
				t.Errorf("%s: OutcomeOf gave %+v, want %+v", c.name, got, c.want)
			}
		}
		// Serve writes the 503 with Retry-After, and no other answer has it.
		for status, want := range map[int][]string{http.StatusServiceUnavailable: {"1", "2", "3"}, http.StatusInternalServerError: {""}, http.StatusNotFound: {""}} {
			w := httptest.NewRecorder()
			(*Runtime)(nil).writeProblem(w, status, "", "code")
			if got := w.Header().Get("Retry-After"); !slices.Contains(want, got) || w.Code != status {
				t.Errorf("the answer %d has Retry-After %q (status %d), want one of %q", status, got, w.Code, want)
			}
			// A browser does not sniff the problem as another type (security review of PR 445, I-2).
			if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("the answer %d has X-Content-Type-Options %q, want nosniff", status, got)
			}
		}
	})
}

func TestInvalidRPC(t *testing.T) {
	t.Run("ST-SUP-RP10-LC42-T03 An invalid value over gRPC is InvalidArgument with the field and the reason in a BadRequest detail, and no value", func(t *testing.T) {
		err := OutcomeOf(fmt.Errorf("fill: %w", &invalidError{field: "patient_ref", reason: "shorter than min_length 1"})).RPCStatus()
		st := status.Convert(err)
		if st.Code() != codes.InvalidArgument || st.Message() != ErrorCodeInvalid || len(st.Details()) != 1 {
			t.Fatalf("the invalid value gave %v with %d details, want InvalidArgument invalid with one detail", st, len(st.Details()))
		}
		bad, ok := st.Details()[0].(*errdetails.BadRequest)
		if !ok || len(bad.GetFieldViolations()) != 1 || bad.GetFieldViolations()[0].GetField() != "patient_ref" ||
			bad.GetFieldViolations()[0].GetDescription() != "shorter than min_length 1" {
			t.Fatalf("the detail is %v, want the field patient_ref and its reason", st.Details()[0])
		}
		// An outcome with no field has no detail.
		if st := status.Convert(OutcomeOf(ErrDenied).RPCStatus()); st.Code() != codes.NotFound || len(st.Details()) != 0 {
			t.Fatalf("a deny gave %v with %d details, want NotFound with none", st, len(st.Details()))
		}
	})
}

func TestTransientEndsRun(t *testing.T) {
	t.Run("ST-SUP-RP06-LC25-T03 A decision that the provider cannot make now ends the command run: no later decision is tried, and the answer is 503 undecided", func(t *testing.T) {
		e := newEnv(t)
		e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o-a', 'new'), ('t1', 'o-b', 'new')`)
		cmd := e.command("read-two", nil, e.orders)
		calls := 0
		e.decision.setFail(func(string) error {
			calls++
			// The error of a provider call that ran past the decision time limit.
			return context.DeadlineExceeded
		})
		var second error
		_, err := e.run(cmd, e.request("t1", u7(t), "k-lc25-t03", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, e.orders)
			_, first := r.Get(ctx, "o-a")
			_, second = r.Get(ctx, "o-b")
			return first
		}))
		if calls != 1 {
			t.Fatalf("the provider was called %d times, want once: the run ends at the first transient error", calls)
		}
		if second == nil || !errors.Is(second, ErrInternal) {
			t.Fatalf("the second read gave %v, want the broken call", second)
		}
		if o := OutcomeOf(err); o.Status != http.StatusServiceUnavailable || o.Code != audit.ErrorCodeUndecided || RPCCode(o.Status) != codes.Unavailable {
			t.Fatalf("the run gave %v and the outcome %+v, want 503 undecided", err, o)
		}
	})
}

func TestFinite(t *testing.T) {
	t.Run("ST-SUP-RP10-LC42-T04 Finite refuses NaN and both infinities, which the generated Check of a bounded float uses", func(t *testing.T) {
		for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			if Finite(v) {
				t.Fatalf("Finite(%v) is true", v)
			}
		}
		if !Finite(0) || !Finite(-1.5) || !Finite(math.MaxFloat64) {
			t.Fatal("Finite refuses a finite number")
		}
	})
}

func TestProblemForm(t *testing.T) {
	t.Run("ST-SUP-RC32-OC19-T02 A command problem has the type of its code under the problem base, and about:blank with no base", func(t *testing.T) {
		const base = "https://errors.example.test/orders/"
		read := func(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
			t.Helper()
			var p map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			for member := range p {
				if member != strings.ToLower(member) {
					t.Errorf("the problem member %q is not snake_case", member)
				}
			}
			return p
		}
		rt := &Runtime{cfg: Config{ProblemBase: base}}
		for _, c := range []struct {
			status            int
			problemType, code string
			want              string
		}{
			{http.StatusNotFound, "", ErrorCodeDenied, base + "denied"},
			{http.StatusInternalServerError, "", ErrorCodeInternal, base + "internal"},
			{http.StatusMethodNotAllowed, "", "", base + "method-not-allowed"},
			{http.StatusBadRequest, "", "", base + "bad-request"},
			{http.StatusConflict, "", "appointment.not_booked", base + "appointment-not-booked"},
			{http.StatusBadRequest, rt.problemType("idempotency-key-missing"), "", base + "idempotency-key-missing"},
		} {
			w := httptest.NewRecorder()
			rt.writeProblem(w, c.status, c.problemType, c.code)
			if p := read(t, w); p["type"] != c.want || (c.code != "" && p["code"] != c.code) {
				t.Errorf("the problem %d %q has %v, want the type %s", c.status, c.code, p, c.want)
			}
		}
		w := httptest.NewRecorder()
		rt.writeFieldProblem(w, http.StatusBadRequest, "", ErrorCodeInvalid, "patient_ref")
		if p := read(t, w); p["type"] != base+"invalid" || p["field"] != "patient_ref" {
			t.Errorf("the invalid problem is %v", p)
		}
		bare := &Runtime{}
		w = httptest.NewRecorder()
		bare.writeProblem(w, http.StatusBadRequest, bare.problemType("idempotency-key-missing"), "")
		if p := read(t, w); p["type"] != "about:blank" {
			t.Errorf("with no base the idempotency problem has the type %v, want about:blank, never a bare word", p["type"])
		}
	})
}

// secretMessages gives a request message whose nested member pin the proto marks debug_redact when marked is true,
// and a plain response message.
func secretMessages(t *testing.T, marked bool) (func() *dynamicpb.Message, func() *dynamicpb.Message) {
	t.Helper()
	str, msg, opt := descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	file := &descriptorpb.FileDescriptorProto{Name: proto.String("secret.proto"), Package: proto.String("secret.v1"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Pin"), Field: []*descriptorpb.FieldDescriptorProto{{Name: proto.String("pin"), JsonName: proto.String("pin"), Number: proto.Int32(1), Type: &str, Label: &opt,
				Options: &descriptorpb.FieldOptions{DebugRedact: proto.Bool(marked)}}}},
			{Name: proto.String("ResetRequest"), Field: []*descriptorpb.FieldDescriptorProto{{Name: proto.String("pin"), JsonName: proto.String("pin"), Number: proto.Int32(1), Type: &msg, Label: &opt, TypeName: proto.String(".secret.v1.Pin")}}},
			{Name: proto.String("ResetResponse")},
		}}
	fd, err := protodesc.NewFile(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, response := fd.Messages().ByName("ResetRequest"), fd.Messages().ByName("ResetResponse")
	return func() *dynamicpb.Message { return dynamicpb.NewMessage(request) }, func() *dynamicpb.Message { return dynamicpb.NewMessage(response) }
}

func TestServeSecretNeedsRing(t *testing.T) {
	t.Run("ST-SUP-RP14-LC12-T04 Serve and ServeRPC refuse a command whose message holds a debug_redact member when the runtime has no key ring", func(t *testing.T) {
		cmd := &Command[testProbe]{spec: CommandSpec[testProbe]{Name: "reset-pin"}}
		spec := func(marked bool) ServeSpec[*dynamicpb.Message, *dynamicpb.Message, testProbe] {
			newRequest, newResponse := secretMessages(t, marked)
			return ServeSpec[*dynamicpb.Message, *dynamicpb.Message, testProbe]{Command: cmd, ResourceType: "pin", SubjectType: "user", Status: http.StatusOK,
				NewRequest: newRequest, NewResponse: newResponse, Resource: func(*dynamicpb.Message) string { return "p1" },
				Fill: func(context.Context, *dynamicpb.Message, *Handle[testProbe]) (*dynamicpb.Message, error) {
					return nil, nil
				}}
		}
		bare, ringed := &Runtime{}, &Runtime{cfg: Config{Ring: vaultRing(t, "1 "+throwawayKey(t))}}
		for _, c := range []struct {
			name    string
			rt      *Runtime
			marked  bool
			refused bool
		}{{"a marked member and no ring", bare, true, true}, {"a marked member and a ring", ringed, true, false}, {"no marked member and no ring", bare, false, false}} {
			_, serveErr := Serve(c.rt, spec(c.marked))
			_, rpcErr := ServeRPC(c.rt, spec(c.marked))
			for name, err := range map[string]error{"Serve": serveErr, "ServeRPC": rpcErr} {
				if refused := err != nil && strings.Contains(err.Error(), "debug_redact"); refused != c.refused {
					t.Errorf("%s of %s gave %v, want refused %t", name, c.name, err, c.refused)
				}
			}
		}
	})
}

func TestNewStruct(t *testing.T) {
	t.Run("ST-SUP-RP10-LC57-T01 NewStruct gives a Struct of the JSON values of a fill, and refuses another value, invalid UTF-8 and a value nested too deep", func(t *testing.T) {
		fields := map[string]any{"site": "s-1", "count": 3, "ratio": 0.5, "open": true, "none": nil,
			"sites": []any{"a", map[string]any{"n": int64(2)}}, "raw": []byte{1, 2}}
		s, err := NewStruct(fields)
		if err != nil {
			t.Fatalf("NewStruct of JSON values: %v", err)
		}
		got, err := protojson.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var back map[string]any
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"site": "s-1", "count": 3.0, "ratio": 0.5, "open": true, "none": nil,
			"sites": []any{"a", map[string]any{"n": 2.0}}, "raw": "AQI="}
		if !reflect.DeepEqual(back, want) {
			t.Fatalf("NewStruct gave %s, want %v", got, want)
		}
		// The deepest values that it takes, of objects alone and with a list, and one level more.
		deep := func(levels int) map[string]any {
			m := map[string]any{"leaf": 1}
			for i := 1; i < levels; i++ {
				m = map[string]any{"next": m}
			}
			return m
		}
		for _, value := range []map[string]any{deep(MaxStructDepth), {"list": []any{deep(MaxStructDepth - 2)}}} {
			if _, err := NewStruct(value); err != nil {
				t.Fatalf("NewStruct refuses a value of %d levels: %v", MaxStructDepth, err)
			}
		}
		loop := map[string]any{}
		loop["self"] = loop
		for name, value := range map[string]map[string]any{
			"a value of another type":        {"when": struct{}{}},
			"a string that is not UTF-8":     {"name": "\xff"},
			"a key that is not UTF-8":        {"\xff": 1},
			"one level of objects too many":  deep(MaxStructDepth + 1),
			"one level with a list too many": {"list": []any{deep(MaxStructDepth - 1)}},
			"a map that holds itself":        loop,
		} {
			if s, err := NewStruct(value); err == nil || s != nil {
				t.Errorf("NewStruct of %s gave %v, %v; want an error and no Struct", name, s, err)
			}
		}
	})
}

func TestNewToken(t *testing.T) {
	t.Run("ST-SUP-RP10-LC56-T01 NewToken gives new random bytes of each size from 16 to 64, and refuses each other size", func(t *testing.T) {
		seen := map[string]bool{}
		for size := MinTokenSize; size <= MaxTokenSize; size++ {
			a, err := NewToken(size)
			if err != nil || len(a) != size {
				t.Fatalf("NewToken(%d) gives %d bytes and %v", size, len(a), err)
			}
			b, _ := NewToken(size)
			if string(a) == string(b) || seen[string(a)] {
				t.Fatalf("NewToken(%d) gives the same bytes twice", size)
			}
			seen[string(a)] = true
			if string(a) == string(make([]byte, size)) {
				t.Fatalf("NewToken(%d) gives only zero bytes", size)
			}
		}
		for _, size := range []int{-1, 0, 1, MinTokenSize - 1, MaxTokenSize + 1, 1 << 20} {
			if b, err := NewToken(size); err == nil || b != nil {
				t.Errorf("NewToken(%d) gives %d bytes and %v, want a refusal and no bytes", size, len(b), err)
			}
		}
	})
}

func TestServeUnknownMember(t *testing.T) {
	t.Run("ST-SUP-RP10-LC33-T02 A command body with a member that the request message does not take gets 400 bad-request that names the member and no value", func(t *testing.T) {
		str, msg, opt := descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		file := &descriptorpb.FileDescriptorProto{Name: proto.String("ship.proto"), Package: proto.String("ship.v1"), Syntax: proto.String("proto3"),
			Dependency: []string{"google/protobuf/struct.proto"},
			MessageType: []*descriptorpb.DescriptorProto{
				{Name: proto.String("Address"), Field: []*descriptorpb.FieldDescriptorProto{{Name: proto.String("postal_code"), JsonName: proto.String("postalCode"), Number: proto.Int32(1), Type: &str, Label: &opt}}},
				{Name: proto.String("ShipRequest"), Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("shipment_id"), JsonName: proto.String("shipmentId"), Number: proto.Int32(1), Type: &str, Label: &opt},
					{Name: proto.String("address"), JsonName: proto.String("address"), Number: proto.Int32(2), Type: &msg, Label: &opt, TypeName: proto.String(".ship.v1.Address")},
					{Name: proto.String("extra"), JsonName: proto.String("extra"), Number: proto.Int32(3), Type: &msg, Label: &opt, TypeName: proto.String(".google.protobuf.Struct")},
				}},
				{Name: proto.String("ShipResponse")},
			}}
		fd, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
		if err != nil {
			t.Fatal(err)
		}
		request, response := fd.Messages().ByName("ShipRequest"), fd.Messages().ByName("ShipResponse")
		calls := 0
		h, err := Serve(&Runtime{cfg: Config{ProblemBase: "https://errors.example.com/ship/"}}, ServeSpec[*dynamicpb.Message, *dynamicpb.Message, testProbe]{
			Command: &Command[testProbe]{spec: CommandSpec[testProbe]{Name: "ship"}}, ResourceType: "shipment", SubjectType: "user", Status: http.StatusOK,
			NewRequest:  func() *dynamicpb.Message { return dynamicpb.NewMessage(request) },
			NewResponse: func() *dynamicpb.Message { return dynamicpb.NewMessage(response) },
			Resource:    func(*dynamicpb.Message) string { return "s1" },
			Fill: func(context.Context, *dynamicpb.Message, *Handle[testProbe]) (*dynamicpb.Message, error) {
				calls++
				return nil, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		key := authntest.NewKey(t, "k1")
		set, err := authn.ParseKeys(authntest.Document(key.JWK()))
		if err != nil {
			t.Fatal(err)
		}
		verifier, err := authn.New(authn.Settings{Issuer: "https://issuer.example", Audience: "ship", ProblemCode: "SHIP", Keys: set, TenantClaim: "tenant"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(verifier.Stop)
		claims := authntest.Claims("https://issuer.example", "ship", "u-7", time.Now())
		claims["tenant"] = "t1"
		token := authntest.Mint(t, authntest.Header("k1"), claims, key)
		long := strings.Repeat("é", 100)
		for _, c := range []struct{ body, member string }{
			{`{"shipmentId":"s1","carrier":"jane.doe@example.com"}`, "carrier"},
			{`{"shipment_id":"s1","ShipmentId":"x"}`, "ShipmentId"},
			{`{"address":{"postalCode":"1","zip":"jane.doe@example.com"}}`, "address.zip"},
			{`{"address":{"postal_code":"1"},"` + long + `":1}`, strings.Repeat("é", 64)},
			{`{"extra":{"any":"thing"},"shipmentId":7}`, ""},
			{`[1]`, ""},
		} {
			r := httptest.NewRequest(http.MethodPost, "/commands/ship", strings.NewReader(c.body))
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			verifier.Middleware(h).ServeHTTP(w, r)
			var problem map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil || w.Code != http.StatusBadRequest || calls != 0 {
				t.Fatalf("the body %.40s gave %d %s, want 400 with no fill call", c.body, w.Code, w.Body.String())
			}
			field, named := problem["field"]
			if problem["type"] != "https://errors.example.com/ship/bad-request" || (c.member == "") == named || (named && field != c.member) {
				t.Errorf("the body %.40s gave the problem %s, want bad-request with the field %q", c.body, w.Body.String(), c.member)
			}
			if strings.Contains(w.Body.String(), "jane.doe") || strings.Contains(w.Body.String(), "thing") {
				t.Errorf("the problem of the body %.40s holds a value: %s", c.body, w.Body.String())
			}
		}
	})
}
