package query

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
	"github.com/jsell-rh/stego/registry/runtime/internal/staging"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// testCursorKey is the cursor key of the tests: 32 fixed bytes.
var testCursorKey = []byte("0123456789abcdef0123456789abcdef")

// servedRuntime is the query runtime of the env with the decision port provider and the cursor key.
func (e *env) servedRuntime(t *testing.T, provider authz.Decision) *Runtime {
	t.Helper()
	az, err := authz.New(authz.Config{Provider: provider, DB: e.db, Schema: "authz", Outbox: e.queue})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(Config{DB: e.db, AuditDB: e.auditDB, Audit: e.writer, AuditSettings: audit.Settings{Level: audit.LevelChange},
		Authz: az, Service: "shop", CursorKey: testCursorKey})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// headerBind is the Bind of the tests: the tenant and the subject come from two headers, as the generated code takes
// them from the verified identity, and the probe call is bound to them and to the query name.
func headerBind(q *Query) func(r *http.Request) (Bound[noProbe], error) {
	return func(r *http.Request) (Bound[noProbe], error) {
		id := r.Header.Get("X-Tenant")
		tn, err := tenant.New(tenant.Requested(tenant.ProvisionAction, id, decided{seal.NewDecisionMark(id, "provision")}))
		if err != nil {
			return Bound[noProbe]{}, err
		}
		p, err := authz.StagingPrincipal(staging.NewStaging(), r.Header.Get("X-Subject"))
		if err != nil {
			return Bound[noProbe]{}, err
		}
		call := probe.NewRecorder(nil).Bind(r.Context(), tn.ID(), p.Subject().String(), q.Name())
		return Bound[noProbe]{Tenant: tn, Principal: p, Call: call, Probe: newNoProbe}, nil
	}
}

// orderPage is the response of the list query of the tests.
type orderPage struct {
	IDs  []string
	next Cursor
}

// listOrders serves the list query q over orders, two rows a page; fills counts the calls of the fill.
func listOrders(t *testing.T, rt *Runtime, q *Query, orders *Entity[Order], fills *atomic.Int64) http.Handler {
	t.Helper()
	h, err := Serve(rt, HTTP[ListRequest, orderPage, noProbe]{
		Query: q,
		Bind:  headerBind(q),
		Decode: func(_ *http.Request, after Cursor) (ListRequest, error) {
			return ListRequest{Limit: 2, After: after}, nil
		},
		Fill: func(ctx context.Context, req ListRequest, h *Handle[noProbe]) (orderPage, error) {
			fills.Add(1)
			repo, err := Repo(h, orders)
			if err != nil {
				return orderPage{}, err
			}
			page, err := repo.List(ctx, req)
			if err != nil {
				return orderPage{}, err
			}
			out := orderPage{IDs: []string{}}
			for _, o := range page.Items {
				out.IDs = append(out.IDs, o.Value().ID)
			}
			if page.More {
				out.next = page.Next
			}
			return out, nil
		},
		Next: func(res orderPage) Cursor { return res.next },
		Encode: func(res orderPage, next string) ([]byte, error) {
			return json.Marshal(map[string]any{"ids": res.IDs, "next": next})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type served struct {
	status      int
	cache       string
	retryAfter  string
	contentType string
	body        string
	IDs         []string `json:"ids"`
	Next        string   `json:"next"`
}

// get sends a GET of path to h as the tenant and the subject.
func get(t *testing.T, h http.Handler, path, tenantID, subject string) served {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("X-Tenant", tenantID)
	r.Header.Set("X-Subject", subject)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	body, _ := io.ReadAll(w.Result().Body)
	s := served{status: w.Code, cache: w.Header().Get("Cache-Control"), retryAfter: w.Header().Get("Retry-After"), contentType: w.Header().Get("Content-Type"), body: string(body)}
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(body, &s); err != nil {
			t.Fatalf("the body %q does not decode: %v", body, err)
		}
	}
	return s
}

// wantProblem checks that s is the problem response of status, with the fixed fields only.
func wantProblem(t *testing.T, s served, status int, forbidden ...string) {
	t.Helper()
	if s.status != status || s.contentType != "application/problem+json" || s.cache != "no-store" {
		t.Fatalf("the response is %d %s %s, want the problem response %d", s.status, s.contentType, s.body, status)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(s.body), &p); err != nil {
		t.Fatalf("the problem %q does not decode: %v", s.body, err)
	}
	if len(p) != 3 || p["type"] != "about:blank" || p["status"] != float64(status) || p["title"] == "" {
		t.Fatalf("the problem is %v, want only type, title and status", p)
	}
	for _, f := range forbidden {
		if strings.Contains(s.body, f) {
			t.Fatalf("the problem %q holds %q", s.body, f)
		}
	}
}

func cursorPath(text string) string {
	return "/orders?" + CursorParameter + "=" + url.QueryEscape(text)
}

// wantSealedOnly decodes the cursor text and checks, with no search in random bytes, that it is exactly the cursor
// version, a nonce of the AEAD's size, and the AEAD seal of position under that nonce with the associated data of
// the query, the tenant and the principal. So the text holds no other byte, and the position shows only as its
// seal. It gives the decoded bytes.
func wantSealedOnly(t *testing.T, rt *Runtime, q *Query, tn, subject, text string, position []byte) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil {
		t.Fatalf("the cursor %q does not decode: %v", text, err)
	}
	nonceSize := rt.cursorAEAD.NonceSize()
	size := 1 + nonceSize + len(position) + rt.cursorAEAD.Overhead()
	if len(raw) != size || raw[0] != cursorVersion {
		t.Fatalf("the cursor %q has %d bytes, want version %d and %d bytes", text, len(raw), cursorVersion, size)
	}
	nonce := raw[1 : 1+nonceSize]
	want := rt.cursorAEAD.Seal(nil, nonce, position, rt.cursorData(q, mustTenant(t, tn), mustPrincipal(t, subject)))
	if !bytes.Equal(raw[1+nonceSize:], want) {
		t.Fatalf("the cursor %q is not the seal of its position %q", text, position)
	}
	return raw
}

func TestCursor(t *testing.T) {
	t.Run("ST-SUP-RP11-LC15-T01 A changed or moved cursor is refused", func(t *testing.T) {
		e := newEnv(t)
		decision := &fakeDecision{decide: allowAll}
		rt := e.servedRuntime(t, decision)
		orders := orderEntity(t, false)
		for _, id := range []string{"o1", "o2", "o3", "o4", "o5"} {
			e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t1', $1, 10), ('t2', $1, 20)", id)
		}
		q := mustListQuery(t, "list-orders", orders, "view")
		other := mustListQuery(t, "list-orders-again", orders, "view")
		var fills atomic.Int64
		h := listOrders(t, rt, q, orders, &fills)
		hOther := listOrders(t, rt, other, orders, &fills)

		first := get(t, h, "/orders", "t1", "user:u-7")
		if first.status != http.StatusOK || strings.Join(first.IDs, ",") != "o1,o2" || first.Next == "" {
			t.Fatalf("the first page is %d %v %q, want o1 and o2 with a cursor", first.status, first.IDs, first.Next)
		}
		// The position of the first page is the key o2, with no filter. The text holds only the version, the nonce and the
		// AEAD seal of that position, so it shows no key, query or principal.
		raw := wantSealedOnly(t, rt, q, "t1", "user:u-7", first.Next, appendText(appendText([]byte{cursorVersion, cursorTable}, "o2"), ""))
		second := get(t, h, cursorPath(first.Next), "t1", "user:u-7")
		if second.status != http.StatusOK || strings.Join(second.IDs, ",") != "o3,o4" {
			t.Fatalf("the unchanged cursor gave %d %v, want o3 and o4", second.status, second.IDs)
		}

		// One byte of the sealed position changed, and one byte of the tag.
		changed := append([]byte(nil), raw...)
		changed[len(changed)-33] ^= 0x01
		flipped := base64.RawURLEncoding.EncodeToString(changed)
		macChanged := append([]byte(nil), raw...)
		macChanged[len(macChanged)-1] ^= 0x80
		fillsBefore, callsBefore := fills.Load(), len(decision.recorded())
		for name, c := range map[string]struct {
			h                 http.Handler
			text, tn, subject string
		}{
			"a byte of the position changed": {h, flipped, "t1", "user:u-7"},
			"a byte of the tag changed":      {h, base64.RawURLEncoding.EncodeToString(macChanged), "t1", "user:u-7"},
			"another query":                  {hOther, first.Next, "t1", "user:u-7"},
			"another tenant":                 {h, first.Next, "t2", "user:u-7"},
			"another principal":              {h, first.Next, "t1", "user:u-9"},
			"text that does not decode":      {h, "not a cursor!", "t1", "user:u-7"},
		} {
			wantProblem(t, get(t, c.h, cursorPath(c.text), c.tn, c.subject), http.StatusBadRequest, "o2", "o3")
			if fills.Load() != fillsBefore || len(decision.recorded()) != callsBefore {
				t.Fatalf("%s reached the fill or the decision port: no row may be read", name)
			}
		}
		if _, err := rt.ParseCursor(q, mustTenant(t, "t2"), mustPrincipal(t, "user:u-7"), first.Next); !errors.Is(err, ErrBadCursor) {
			t.Fatalf("ParseCursor for another tenant gave %v, want ErrBadCursor", err)
		}

		// A search cursor keeps the provider's page token and the first page's consistency.
		token, _ := authz.NewToken("zed-7")
		search := Cursor{page: authz.NewPageToken("spicedb", "page-2", authz.AtLeast(token))}
		text, err := rt.CursorText(q, mustTenant(t, "t1"), mustPrincipal(t, "user:u-7"), search)
		if err != nil {
			t.Fatal(err)
		}
		// The search position holds the provider spicedb, its cursor page-2 and the token zed-7; the text holds
		// only their seal.
		searchPosition := appendText([]byte{cursorVersion, cursorSearch}, "spicedb")
		searchPosition = appendText(searchPosition, "page-2")
		searchPosition = appendText(append(searchPosition, consistencyAtLeast), token.Text())
		wantSealedOnly(t, rt, q, "t1", "user:u-7", text, searchPosition)
		// The same key in another service opens no cursor of this one.
		otherService, err := New(Config{DB: e.db, Service: "billing", CursorKey: testCursorKey})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := otherService.ParseCursor(q, mustTenant(t, "t1"), mustPrincipal(t, "user:u-7"), text); !errors.Is(err, ErrBadCursor) {
			t.Fatalf("another service opened the cursor: %v", err)
		}
		back, err := rt.ParseCursor(q, mustTenant(t, "t1"), mustPrincipal(t, "user:u-7"), text)
		if err != nil || back != search {
			t.Fatalf("the search cursor came back as %+v, %v", back, err)
		}
		if _, err := New(Config{DB: e.db, Service: "shop", CursorKey: []byte("short")}); err == nil {
			t.Fatal("a cursor key of 5 bytes was accepted")
		}
		if _, err := New(Config{DB: e.db, CursorKey: testCursorKey}); err == nil {
			t.Fatal("a cursor key with no service name was accepted")
		}
	})
}

func TestServe(t *testing.T) {
	t.Run("ST-SUP-RP11-LC17-T01 One helper serves get and list", func(t *testing.T) {
		e := newEnv(t)
		decision := &fakeDecision{decide: func(r authz.EvaluationRequest) bool { return keyOf(r.Resource) != "o2" }}
		rt := e.servedRuntime(t, decision)
		orders := orderEntity(t, false)
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t1', 'o1', 4711), ('t1', 'o2', 4712), ('t1', 'o3', 4713)")
		getOrder := mustQuery(t, "get-order", orders)
		h, err := Serve(rt, HTTP[string, Order, noProbe]{
			Query: getOrder,
			Bind:  headerBind(getOrder),
			Decode: func(r *http.Request, _ Cursor) (string, error) {
				id := r.URL.Query().Get("id")
				if id == "" {
					return "", ErrBadRequest
				}
				return id, nil
			},
			Fill: func(ctx context.Context, id string, h *Handle[noProbe]) (Order, error) {
				repo, err := Repo(h, orders)
				if err != nil {
					return Order{}, err
				}
				o, err := repo.Get(ctx, id)
				if err != nil {
					return Order{}, err
				}
				return o.Value(), nil
			},
			Encode: func(o Order, _ string) ([]byte, error) {
				return json.Marshal(map[string]any{"id": o.ID, "total": o.Total})
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		ok := get(t, h, "/order?id=o1", "t1", "user:u-7")
		if ok.status != http.StatusOK || ok.contentType != "application/json" || ok.body != `{"id":"o1","total":4711}` {
			t.Fatalf("the get gave %d %s %s, want 200 with the row", ok.status, ok.contentType, ok.body)
		}
		wantProblem(t, get(t, h, "/order?id=o2", "t1", "user:u-7"), http.StatusNotFound, "o2", "4712")
		wantProblem(t, get(t, h, "/order?id=o9", "t1", "user:u-7"), http.StatusNotFound, "o9")
		wantProblem(t, get(t, h, "/order", "t1", "user:u-7"), http.StatusBadRequest)
		wantProblem(t, get(t, h, "/order?id=o1", "", ""), http.StatusUnauthorized)

		var fills atomic.Int64
		list := listOrders(t, rt, mustListQuery(t, "list-orders", orders, "view"), orders, &fills)
		page := get(t, list, "/orders", "t1", "user:u-7")
		if page.status != http.StatusOK || strings.Join(page.IDs, ",") != "o1,o3" {
			t.Fatalf("the list gave %d %v, want the allowed rows o1 and o3", page.status, page.IDs)
		}
		wantProblem(t, get(t, list, cursorPath("garbage"), "t1", "user:u-7"), http.StatusBadRequest)
		if fills.Load() != 1 {
			t.Fatalf("the bad cursor reached the fill")
		}
		if page.cache != "no-store" {
			t.Fatalf("the list response has Cache-Control %q, want no-store", page.cache)
		}
		// A provider that fails is a read that could not be decided: 503, not 500.
		decision.mu.Lock()
		decision.down = true
		decision.mu.Unlock()
		undecided := get(t, list, "/orders", "t1", "user:u-7")
		wantProblem(t, undecided, http.StatusServiceUnavailable, "the provider is down")
		if !slices.Contains([]string{"1", "2", "3"}, undecided.retryAfter) {
			t.Errorf("the 503 of an undecided list has Retry-After %q, want 1 to 3 s (ST-SUP-Q140)", undecided.retryAfter)
		}
		// A provider that refuses the call, such as for a bad key, is no retry case: 500 with no Retry-After.
		decision.mu.Lock()
		decision.fault = providerError(status.Error(codes.PermissionDenied, "bad key"))
		decision.mu.Unlock()
		refused := get(t, list, "/orders", "t1", "user:u-7")
		if refused.status != http.StatusInternalServerError || refused.retryAfter != "" || strings.Contains(refused.body, "bad key") {
			t.Errorf("a refused provider answered %d with Retry-After %q: %s, want 500 with none and no provider text", refused.status, refused.retryAfter, refused.body)
		}
		decision.mu.Lock()
		decision.fault = nil
		decision.mu.Unlock()
		if page.retryAfter != "" {
			t.Errorf("the 200 of the list has Retry-After %q", page.retryAfter)
		}
		decision.mu.Lock()
		decision.down = false
		decision.mu.Unlock()

		// Decode reads at most MaxRequestBody bytes of the body.
		var read atomic.Int64
		bodyQuery := mustQuery(t, "body-order", orders)
		withBody, err := Serve(rt, HTTP[string, Order, noProbe]{Query: bodyQuery, Bind: headerBind(bodyQuery),
			Decode: func(r *http.Request, _ Cursor) (string, error) {
				n, err := io.Copy(io.Discard, r.Body)
				read.Store(n)
				if err != nil {
					return "", ErrBadRequest
				}
				return "o1", nil
			},
			Fill:   func(context.Context, string, *Handle[noProbe]) (Order, error) { return Order{ID: "o1"}, nil },
			Encode: func(Order, string) ([]byte, error) { return []byte(`{}`), nil }})
		if err != nil {
			t.Fatal(err)
		}
		big := httptest.NewRequest(http.MethodGet, "/order", strings.NewReader(strings.Repeat("x", MaxRequestBody+1)))
		big.Header.Set("X-Tenant", "t1")
		big.Header.Set("X-Subject", "user:u-7")
		w := httptest.NewRecorder()
		withBody.ServeHTTP(w, big)
		if w.Code != http.StatusBadRequest || read.Load() > MaxRequestBody {
			t.Fatalf("a body above the limit gave %d after %d bytes read, want 400 within the limit", w.Code, read.Load())
		}

		r := httptest.NewRequest(http.MethodPost, "/orders", nil)
		w = httptest.NewRecorder()
		list.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("a POST gave %d, want 405", w.Code)
		}
		noKey := e.runtimeOf(t, decision)
		if _, err := Serve(noKey, HTTP[ListRequest, orderPage, noProbe]{Query: getOrder, Bind: headerBind(getOrder),
			Decode: func(*http.Request, Cursor) (ListRequest, error) { return ListRequest{}, nil },
			Fill:   func(context.Context, ListRequest, *Handle[noProbe]) (orderPage, error) { return orderPage{}, nil },
			Next:   func(orderPage) Cursor { return Cursor{} },
			Encode: func(orderPage, string) ([]byte, error) { return nil, nil }}); !errors.Is(err, ErrNoCursorKey) {
			t.Fatalf("a list helper with no cursor key gave %v, want ErrNoCursorKey", err)
		}
	})
}

func TestUnavailableAnswer(t *testing.T) {
	t.Run("ST-SUP-RP10-LC43-T02 A query read that the database cannot serve now is 503 unavailable with Retry-After, and Unavailable over gRPC", func(t *testing.T) {
		refused := fmt.Errorf("tenant: begin the transaction of tenant t1: %w", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED})
		shutdown := fmt.Errorf("query: read order: %w", sqlState("57P01"))
		for name, err := range map[string]error{"a refused connection": refused, "a server that shuts down": shutdown} {
			if status, code := answerOf(err, nil); status != http.StatusServiceUnavailable || code != ErrorCodeUnavailable || !Transient(err) {
				t.Fatalf("%s gave %d %q (transient %v), want 503 unavailable", name, status, code, Transient(err))
			}
			grpcErr := rpcError(context.Background(), err, nil)
			if status.Code(grpcErr) != codes.Unavailable || status.Convert(grpcErr).Message() != ErrorCodeUnavailable {
				t.Fatalf("%s over gRPC gave %v, want Unavailable with the code unavailable", name, grpcErr)
			}
			w := httptest.NewRecorder()
			status, code := answerOf(err, nil)
			(*Runtime)(nil).writeCodedProblem(w, status, code)
			if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" || strings.Contains(w.Body.String(), "refused") {
				t.Fatalf("%s over HTTP gave %d %v %s, want 503 with Retry-After and no error text", name, w.Code, w.Header(), w.Body)
			}
		}
		// A database error during the wait of a gRPC query is Unavailable too (security review of PR #434, I4).
		if st := status.Convert(awaitError(context.Background(), refused)); st.Code() != codes.Unavailable || st.Message() != ErrorCodeUnavailable {
			t.Fatalf("a refused connection during the gRPC wait gave %v, want Unavailable", st)
		}
		// An undecided read keeps its own message, and a statement that the server refuses stays 500 internal.
		if status, code := answerOf(&undecidedError{transient: true}, nil); status != http.StatusServiceUnavailable || code != "" {
			t.Fatalf("an undecided read gave %d %q, want 503 with no code", status, code)
		}
		if status, code := answerOf(fmt.Errorf("query: read: %w", sqlState("42703")), nil); status != http.StatusInternalServerError || code != ErrorCodeInternal {
			t.Fatalf("an undefined column gave %d %q, want 500 internal", status, code)
		}
	})
}

// sqlState is a database error with a SQLSTATE, as the driver gives it.
type sqlState string

func (s sqlState) Error() string    { return "sqlstate " + string(s) }
func (s sqlState) SQLState() string { return string(s) }

func TestCursorKeyRing(t *testing.T) {
	t.Run("ST-SUP-RP11-LC23-T01 Each key of the ring opens a cursor, and only the first seals", func(t *testing.T) {
		e := newEnv(t)
		keyA, keyB, keyC := bytes.Repeat([]byte{'a'}, MinCursorKey), bytes.Repeat([]byte{'b'}, MinCursorKey), bytes.Repeat([]byte{'c'}, MinCursorKey)
		runtime := func(key []byte, extra ...[]byte) *Runtime {
			t.Helper()
			rt, err := New(Config{DB: e.db, Service: "orders", CursorKey: key, ExtraCursorKeys: extra})
			if err != nil {
				t.Fatal(err)
			}
			return rt
		}
		q, err := NewQuery("list-orders")
		if err != nil {
			t.Fatal(err)
		}
		tn, p := mustTenant(t, "t1"), mustPrincipal(t, "user:u-7")
		seal := func(rt *Runtime) string {
			t.Helper()
			text, err := rt.CursorText(q, tn, p, KeyCursor("o2"))
			if err != nil {
				t.Fatal(err)
			}
			return text
		}
		opens := func(rt *Runtime, text string) bool {
			c, err := rt.ParseCursor(q, tn, p, text)
			if err != nil {
				if !errors.Is(err, ErrBadCursor) {
					t.Fatalf("ParseCursor gave %v, want nil or ErrBadCursor", err)
				}
				return false
			}
			after, ok := c.Key()
			return ok && after == "o2"
		}
		// The three steps of a rotation from key A to key B: B as the extra key; B as the key and A as the extra
		// key; B alone.
		onlyA, step1, step2, onlyB := runtime(keyA), runtime(keyA, keyB), runtime(keyB, keyA), runtime(keyB)
		fromA, fromStep1, fromStep2 := seal(onlyA), seal(step1), seal(step2)
		for name, c := range map[string]struct {
			rt   *Runtime
			text string
			want bool
		}{
			"step 1 opens a cursor of A":        {step1, fromA, true},
			"A opens a cursor of step 1":        {onlyA, fromStep1, true},
			"step 2 opens a cursor of step 1":   {step2, fromStep1, true},
			"step 1 opens a cursor of step 2":   {step1, fromStep2, true},
			"step 2 opens a cursor of A":        {step2, fromA, true},
			"B opens a cursor of step 2":        {onlyB, fromStep2, true},
			"A opens no cursor of step 2":       {onlyA, fromStep2, false},
			"B opens no cursor of A":            {onlyB, fromA, false},
			"step 2 opens no cursor of a key C": {step2, seal(runtime(keyC)), false},
			"the ring opens no changed cursor":  {step2, fromA[:len(fromA)-2] + "AA", false},
		} {
			if got := opens(c.rt, c.text); got != c.want {
				t.Errorf("%s: opened %v, want %v", name, got, c.want)
			}
		}
		for name, cfg := range map[string]Config{
			"an extra key that is short":   {DB: e.db, Service: "orders", CursorKey: keyA, ExtraCursorKeys: [][]byte{keyB[:MinCursorKey-1]}},
			"an extra key that is the key": {DB: e.db, Service: "orders", CursorKey: keyA, ExtraCursorKeys: [][]byte{keyA}},
			"an extra key with no key":     {DB: e.db, Service: "orders", ExtraCursorKeys: [][]byte{keyB}},
		} {
			if _, err := New(cfg); err == nil {
				t.Errorf("New took %s", name)
			}
		}
	})
}

func TestProblemForm(t *testing.T) {
	t.Run("ST-SUP-RC33-OC12-T01 A query problem has the type of its code under the problem base, and about:blank with no base", func(t *testing.T) {
		const base = "https://errors.example.test/orders/"
		e := newEnv(t)
		rt, err := New(Config{DB: e.db, Service: "orders", CursorKey: testCursorKey, ProblemBase: base})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			status int
			code   string
			want   string
		}{
			{http.StatusNotFound, "", base + "not-found"},
			{http.StatusBadRequest, "", base + "bad-request"},
			{http.StatusInternalServerError, ErrorCodeInternal, base + "internal"},
			{http.StatusServiceUnavailable, ErrorCodeUnavailable, base + "unavailable"},
			{http.StatusConflict, "order.closed", base + "order-closed"},
		} {
			w := httptest.NewRecorder()
			rt.writeCodedProblem(w, c.status, c.code)
			var p map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			if p["type"] != c.want || w.Code != c.status {
				t.Errorf("the problem %d %q has %v, want the type %s", c.status, c.code, p, c.want)
			}
			for member := range p {
				if member != strings.ToLower(member) {
					t.Errorf("the problem member %q is not snake_case", member)
				}
			}
		}
		w := httptest.NewRecorder()
		(*Runtime)(nil).writeProblem(w, http.StatusNotFound)
		if !strings.Contains(w.Body.String(), `"type":"about:blank"`) {
			t.Errorf("with no base the problem is %s, want the type about:blank", w.Body)
		}
	})
}
