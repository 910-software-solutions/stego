package command

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/jsell-rh/stego/registry/runtime/authz"
)

// amendServe gives the HTTP and the gRPC handler of the command amend, which decides the permission amend on the
// order that the request member order_id names: the deliveredPort allows it only after the relay wrote a tuple of
// that order, as SpiceDB allows a child create only after the tuples of its parent.
func (r *revocationEnv) amendServe() (http.Handler, func(context.Context, *structpb.Struct) (*structpb.Struct, error)) {
	r.t.Helper()
	amend := r.command("amend", func(s *CommandSpec[testProbe]) {
		s.Permission, s.Resource = "amend", &Resource{Type: "order",
			Token:   `SELECT authz_token FROM placed_orders WHERE tenant_id = $1 AND id = $2 FOR SHARE`,
			ValidID: func(id string) bool { return id != "" }}
	}, r.orders)
	spec := ServeSpec[*structpb.Struct, *structpb.Struct, testProbe]{
		Command: amend, ResourceType: "order", SubjectType: "user", Status: http.StatusOK,
		NewRequest:  func() *structpb.Struct { return &structpb.Struct{} },
		NewResponse: func() *structpb.Struct { return &structpb.Struct{} },
		Resource:    func(s *structpb.Struct) string { return s.GetFields()["order_id"].GetStringValue() },
		Fill: func(context.Context, *structpb.Struct, *Handle[testProbe]) (*structpb.Struct, error) {
			return structpb.NewStruct(map[string]any{"done": true})
		},
	}
	h, err := Serve(r.rt, spec)
	if err != nil {
		r.t.Fatal(err)
	}
	rpc, err := ServeRPC(r.rt, spec)
	if err != nil {
		r.t.Fatal(err)
	}
	return h, rpc
}

// amendHTTP sends amend on order as who with the Authz-Revocation-After values after.
func (r *revocationEnv) amendHTTP(ctx context.Context, h http.Handler, order string, after ...string) *httptest.ResponseRecorder {
	r.t.Helper()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/orders/"+order+"/amend", strings.NewReader(`{"order_id":"`+order+`"}`))
	for _, value := range after {
		request.Header.Add(authz.RevocationAfterHeader, value)
	}
	answer := httptest.NewRecorder()
	h.ServeHTTP(answer, request)
	return answer
}

// placed creates the order id as who with no relay running, and gives the location of its revocation.
func (r *revocationEnv) placed(who, id string) string {
	r.t.Helper()
	location := r.create(who, "k-"+id, id, nil).Response.Header.Get(authz.RevocationLocationHeader)
	if location == "" {
		r.t.Fatalf("the create of %s answered no revocation", id)
	}
	return location
}

func TestAwaitRevocation(t *testing.T) {
	t.Run("ST-SUP-RP10-LC47-T01 A command on a row that the caller just created waits for the revocation of the create and is allowed", func(t *testing.T) {
		r := newRevocationEnv(t, 5*time.Second, 0)
		h, rpc := r.amendServe()
		first, second := r.placed("u-7@t1", "aw1"), r.placed("u-7@t1", "aw2")
		// Before the relay runs, a command on the new row with no header is denied: this is the gap of W12-B1.
		if answer := r.amendHTTP(r.identity["u-7@t1"], h, "aw1"); answer.Code != http.StatusNotFound {
			t.Fatalf("an amend before the delivery answered %d %s, want 404", answer.Code, answer.Body)
		}
		go func() { time.Sleep(300 * time.Millisecond); r.startRelay() }()
		answer := r.amendHTTP(r.identity["u-7@t1"], h, "aw1", first)
		if answer.Code != http.StatusOK {
			t.Fatalf("an amend with %s answered %d %s, want 200", authz.RevocationAfterHeader, answer.Code, answer.Body)
		}
		// The wait sets no revocation header: the command enqueued no tuple, so the answer names none.
		if got := answer.Header().Get(authz.RevocationHeader); got != "" {
			t.Fatalf("the amend answered %s: %q, want none from the wait", authz.RevocationHeader, got)
		}
		ctx, stream := r.rpcCall(r.identity["u-7@t1"], authz.RevocationAfterMetadata, second)
		if _, err := rpc(ctx, mustStruct(t, map[string]any{"order_id": "aw2"})); err != nil {
			t.Fatalf("a gRPC amend with %s gave %v, want OK", authz.RevocationAfterMetadata, err)
		}
		if got := stream.header(authz.RevocationMetadata); len(got) != 0 {
			t.Fatalf("the gRPC amend sent %s %v, want none from the wait", authz.RevocationMetadata, got)
		}
	})

	t.Run("ST-SUP-RP10-LC47-T02 A bad value is a bad request, another caller's revocation does not fail the command, and a cancel gets no internal code", func(t *testing.T) {
		r := newRevocationEnv(t, 5*time.Second, 0)
		h, rpc := r.amendServe()
		location := r.placed("u-7@t1", "aw3")
		for _, values := range [][]string{{"not-an-id"}, {location, location}} {
			answer := r.amendHTTP(r.identity["u-7@t1"], h, "aw3", values...)
			if answer.Code != http.StatusBadRequest || strings.Contains(answer.Body.String(), "not-an-id") {
				t.Fatalf("an amend with %v answered %d %s, want 400 that holds no value", values, answer.Code, answer.Body)
			}
		}
		ctx, _ := r.rpcCall(r.identity["u-7@t1"], authz.RevocationAfterMetadata, "not-an-id")
		if _, err := rpc(ctx, mustStruct(t, map[string]any{"order_id": "aw3"})); status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "bad_request" {
			t.Fatalf("a gRPC amend with a bad value gave %v, want InvalidArgument bad_request", err)
		}
		// The revocation of u-7 is unknown to u-8 and to another tenant: their command decides as with no wait, and
		// is denied (404), never a bad request or an internal error.
		for _, who := range []string{"u-8@t1", "u-7@t2"} {
			if answer := r.amendHTTP(r.identity[who], h, "aw3", location); answer.Code != http.StatusNotFound {
				t.Fatalf("an amend of %s with the revocation of u-7 answered %d %s, want 404", who, answer.Code, answer.Body)
			}
		}
		if answer := r.amendHTTP(r.identity["u-7@t1"], h, "aw3", authz.RevocationPath+uuid.NewString()); answer.Code != http.StatusNotFound {
			t.Fatalf("an amend with an unknown revocation answered %d %s, want 404", answer.Code, answer.Body)
		}
		// A client that goes away during the wait gets no answer, and a gRPC call the status of its context.
		ctx, cancel := context.WithCancel(r.identity["u-7@t1"])
		go func() { time.Sleep(200 * time.Millisecond); cancel() }()
		answer := r.amendHTTP(ctx, h, "aw3", location)
		if answer.Body.Len() != 0 || answer.Header().Get("Content-Type") != "" {
			t.Fatalf("a cancelled amend answered %d %s, want no answer", answer.Code, answer.Body)
		}
		rctx, rcancel := context.WithCancel(r.identity["u-7@t1"])
		go func() { time.Sleep(200 * time.Millisecond); rcancel() }()
		call, _ := r.rpcCall(rctx, authz.RevocationAfterMetadata, location)
		if _, err := rpc(call, mustStruct(t, map[string]any{"order_id": "aw3"})); status.Code(err) != codes.Canceled {
			t.Fatalf("a cancelled gRPC amend gave %v, want Canceled and no internal code", err)
		}
		if r.count(`SELECT count(*) FROM audit_event WHERE action = 'amend' AND error_code = 'internal'`) != 0 {
			t.Fatal("a cancelled wait wrote an internal audit row")
		}
	})
}

// mustStruct makes a request message of fields.
func mustStruct(t *testing.T, fields map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(fields)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
