package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/910-software-solutions/stego/registry/runtime/authn"
	"github.com/910-software-solutions/stego/registry/runtime/authn/authntest"
	"github.com/910-software-solutions/stego/registry/runtime/authz"
	"github.com/910-software-solutions/stego/registry/runtime/idempotency"
	"github.com/910-software-solutions/stego/registry/runtime/sealed"
)

func TestFillOutcome(t *testing.T) {
	t.Run("ST-SUP-RP10-LC51-T01 A fill's not-found error is 404, and a 500 of a fill logs its cause class with no text", func(t *testing.T) {
		const secret = "SECRET-TEXT"
		var logs bytes.Buffer
		previous, held := slog.Default(), fillLog
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		// The clock of the log stays at one time, so each later failure of the same class is in the same ten seconds.
		fillLog = authz.NewCauseLog(func() time.Time { return tick })
		t.Cleanup(func() { slog.SetDefault(previous); fillLog = held })

		e := newEnv(t)
		e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o5', 'open'), ('t1', 'o6', 'open')`)
		cmd := e.command("transfer", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource = "reserve", orderResource()
		}, e.orders)
		noTarget := NewRefusal("transfer", "no_target", http.StatusUnprocessableEntity, "transfer.no_target")
		var current func(ctx context.Context, h *Handle[testProbe]) error
		spec := ServeSpec[*structpb.Struct, *structpb.Struct, testProbe]{
			Command: cmd, ResourceType: "order", SubjectType: "user", Status: http.StatusOK,
			NewRequest:  func() *structpb.Struct { return &structpb.Struct{} },
			NewResponse: func() *structpb.Struct { return &structpb.Struct{} },
			Resource:    func(r *structpb.Struct) string { return r.GetFields()["order_id"].GetStringValue() },
			Fill: func(ctx context.Context, r *structpb.Struct, h *Handle[testProbe]) (*structpb.Struct, error) {
				if err := current(ctx, h); err != nil {
					return nil, err
				}
				return structpb.NewStruct(map[string]any{"done": true})
			},
			Refusals: []*Refusal{noTarget}, KeyRequired: true,
		}
		serve, err := Serve(e.rt, spec)
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
		// The caller may transfer from o5, and may not read o6; o7 is not there.
		e.decision.setRelations(relation(t, caller.Subject().String(), "reserve", "t1", "order", "o5"))

		sent := 0
		call := func(fill func(ctx context.Context, h *Handle[testProbe]) error) (int, []byte) {
			t.Helper()
			current = fill
			sent++
			r := httptest.NewRequest(http.MethodPost, "/commands/transfer", strings.NewReader(`{"order_id":"o5"}`))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Idempotency-Key", `"k-lc51-`+NewID()+`"`)
			w := httptest.NewRecorder()
			verifier.Middleware(serve).ServeHTTP(w, r)
			answer, _ := io.ReadAll(w.Result().Body)
			return w.Code, answer
		}
		lastAudit := func() (outcome, code string) {
			t.Helper()
			if err := e.admin.QueryRowContext(e.ctx, `SELECT outcome, coalesce(error_code, '') FROM audit_event
 WHERE tenant_id = 't1' AND action = 'transfer' ORDER BY seq DESC LIMIT 1`).Scan(&outcome, &code); err != nil {
				t.Fatal(err)
			}
			return outcome, code
		}
		readOther := func(id string) func(ctx context.Context, h *Handle[testProbe]) error {
			return func(ctx context.Context, h *Handle[testProbe]) error {
				_, err := repo(t, h, e.orders).GetForUpdate(ctx, id)
				return err
			}
		}

		// A row that is not there, and a row that the caller may not read, give one answer: 404 not_found.
		var answers [][]byte
		for _, id := range []string{"o7", "o6"} {
			code, answer := call(readOther(id))
			var p map[string]any
			if err := json.Unmarshal(answer, &p); err != nil || code != http.StatusNotFound || p["code"] != ErrorCodeNotFound || len(p) != 4 {
				t.Fatalf("a fill that returns the not-found error of %s gave %d %s, want 404 not_found", id, code, answer)
			}
			if strings.Contains(string(answer), id) || strings.Contains(string(answer), "Order") || strings.Contains(string(answer), "order") {
				t.Fatalf("the answer for %s names the entity or the ID: %s", id, answer)
			}
			if outcome, auditCode := lastAudit(); outcome != "failed" || auditCode != ErrorCodeNotFound {
				t.Fatalf("the audit row for %s is %s %q, want failed not_found", id, outcome, auditCode)
			}
			answers = append(answers, answer)
		}
		if !bytes.Equal(answers[0], answers[1]) {
			t.Fatalf("a missing row gave %s and a denied row %s, want the same answer", answers[0], answers[1])
		}
		// Over gRPC, the same fill is NotFound with the fixed code alone.
		ctx, err := verifier.Authenticate(context.Background(), "Bearer "+token)
		if err != nil {
			t.Fatal(err)
		}
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(RPCKeyMetadata, `"k-lc51-rpc"`))
		current = readOther("o6")
		request, _ := structpb.NewStruct(map[string]any{"order_id": "o5"})
		if _, err := rpc(ctx, request); status.Code(err) != codes.NotFound || status.Convert(err).Message() != ErrorCodeNotFound {
			t.Fatalf("a fill that returns the not-found error over gRPC gave %v, want NotFound not_found", err)
		}

		// A fill that maps the not-found error to a declared refusal keeps its refusal.
		code, answer := call(func(ctx context.Context, h *Handle[testProbe]) error {
			if err := readOther("o7")(ctx, h); errors.Is(err, ErrNotFound) {
				return noTarget
			}
			return nil
		})
		if code != http.StatusUnprocessableEntity || !strings.Contains(string(answer), "transfer.no_target") {
			t.Fatalf("a fill that maps the not-found error to its refusal gave %d %s", code, answer)
		}
		if strings.Contains(logs.String(), "command.fill.failed") {
			t.Fatalf("a 404 or a declared refusal wrote the line command.fill.failed:\n%s", logs.String())
		}

		// A plain error, a handle used after it closed, each twice: 500 internal and one line for each cause class.
		var stale *Handle[testProbe]
		if code, _ := call(func(_ context.Context, h *Handle[testProbe]) error { stale = h; return nil }); code != http.StatusOK {
			t.Fatalf("the command that keeps its handle gave %d", code)
		}
		failing := []struct {
			cause string
			fill  func(ctx context.Context, h *Handle[testProbe]) error
		}{
			{fillCauseError, func(context.Context, *Handle[testProbe]) error { return errors.New("no money in " + secret) }},
			{fillCauseClosed, func(ctx context.Context, _ *Handle[testProbe]) error {
				r, err := Repo(stale, e.orders)
				if err != nil {
					return err
				}
				_, err = r.Get(ctx, "o5")
				return err
			}},
		}
		for _, f := range failing {
			for range 2 {
				code, answer := call(f.fill)
				if code != http.StatusInternalServerError || strings.Contains(string(answer), secret) {
					t.Fatalf("the fill of the cause %s gave %d %s, want 500 internal with no error text", f.cause, code, answer)
				}
				if outcome, auditCode := lastAudit(); outcome != "failed" || auditCode != ErrorCodeInternal {
					t.Fatalf("the audit row of the cause %s is %s %q, want failed internal", f.cause, outcome, auditCode)
				}
			}
			want := `level=ERROR msg=command.fill.failed action=transfer cause=` + f.cause + ` held_back=0`
			if got := strings.Count(logs.String(), want); got != 1 {
				t.Fatalf("the log holds %d lines %q, want 1 (the second failure is held back):\n%s", got, want, logs.String())
			}
		}

		// A sealed value whose key the ring of the runtime does not hold: 500 internal and the cause class sealed.
		v := newVaultEnv(t, vaultRing(t, "1 "+throwawayKey(t)))
		if err := v.write("org-a", "lc51-seal", func(ctx context.Context, r *Repository[vault]) error {
			_, err := r.Create(ctx, vault{ID: "v1", Label: "x", Token: sealed.SecretOf(secret)})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		other := v.runtime(func(c *Config) { c.Ring = vaultRing(t, "2 "+throwawayKey(t)) })
		for range 2 {
			_, err := v.runOn(other, v.cmd, v.request("org-a", u7(t), "lc51-open-"+NewID(), `{}`),
				func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
					_, err := repo(t, h, v.vaults).Get(ctx, "v1")
					return ok, err
				})
			if o := OutcomeOf(err); o.Status != http.StatusInternalServerError || o.Code != ErrorCodeInternal {
				t.Fatalf("a sealed value that does not open gave %v, the outcome %+v; want 500 internal", err, o)
			}
		}
		want := `level=ERROR msg=command.fill.failed action=keep-vault cause=sealed held_back=0`
		if got := strings.Count(logs.String(), want); got != 1 {
			t.Fatalf("the log holds %d lines %q, want 1:\n%s", got, want, logs.String())
		}
		if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "o6") || strings.Contains(logs.String(), "o7") {
			t.Fatalf("the log holds error text, a value or an ID:\n%s", logs.String())
		}
	})
}
