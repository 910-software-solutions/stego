package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/modes"
)

// parkedService is a service of the fixture with SpiceDB, with the parked-message routes on a router behind its
// middleware, and a token for each subject of the tenant t1 or t2.
type parkedService struct {
	f       *fixture
	svc     *Service
	handler http.Handler
	signing *authntest.Key
}

const parkedBase = "/api/todo/v1"

func newParkedService(t *testing.T, selection modes.Selection) *parkedService {
	t.Helper()
	endpoint, key := startSpiceDB(t)
	f := newFixture(t)
	spice := SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}
	if err := Migrate(context.Background(), MigrationSettings{DatabaseURL: f.database, SpiceDB: spice}, f.migration(nil)); err != nil {
		t.Fatal(err)
	}
	// user:ops may resolve the messages of billing in the tenant t1 only (ST-SUP-RP01-LC17).
	relate(t, endpoint, key, "outbox_destination", "billing", "operator", personOf(t, "ops"))
	if _, err := f.admin.Exec(`INSERT INTO public.stego_tenants (tenant_id) VALUES ('t1'), ('t2')`); err != nil {
		t.Fatal(err)
	}
	signing := authntest.NewKey(t, "k1")
	settings := Settings{DatabaseURL: f.runtimeURL(), Issuer: "https://issuer.example", Audience: "todo",
		KeysFile: writeFile(t, "keys.json", authntest.Document(signing.JWK())), SpiceDB: spice,
		CursorKey: []byte(strings.Repeat("k", 32)), ServiceVersion: "1.0.0"}
	svc, err := New(context.Background(), settings, parts(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	router := http.NewServeMux()
	svc.RegisterParked(router, parkedBase+"/", selection)
	return &parkedService{f: f, svc: svc, handler: svc.RoutedMiddleware(router, router), signing: signing}
}

// personOf is the subject of the person sub of the test issuer, as the authorization runtime maps a verified
// identity (ST-SUP-RP06-LC07).
func personOf(t *testing.T, sub string) string {
	t.Helper()
	subject, err := authz.PersonSubject("user", "https://issuer.example", sub)
	if err != nil {
		t.Fatal(err)
	}
	return subject.String()
}

// park writes one parked message of destination in tenant as the relay leaves it, and gives its ID.
func (s *parkedService) park(t *testing.T, tenantID, destination string, mustLand bool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	if _, err := s.f.admin.Exec(`INSERT INTO stego_outbox.messages (id, tenant_id, destination, resource_key, kind, payload,
		outgoing_key, state, created_at, available_at, attempts, must_land, failure_code, parked_from, parked_at)
		VALUES ($1::uuid, $2, $3, 'invoice:i-1', 'invoice.sent', '{"secret":"payload-value"}'::bytea, $5, 'parked',
		now() - interval '1 hour', now(), 5, $4, 'refused', 'dispatching', now())`, id, tenantID, destination, mustLand, id.String()); err != nil {
		t.Fatal(err)
	}
	return id
}

// call sends one request of subject in tenant and gives the status and the body.
func (s *parkedService) call(t *testing.T, tenantID, subject, method, target, body string) (int, []byte) {
	t.Helper()
	claims := authntest.Claims("https://issuer.example", "todo", subject, time.Now())
	claims["tenant"] = tenantID
	token := authntest.Mint(t, authntest.Header("k1"), claims, s.signing.Private)
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.handler.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

// noLeak fails when an answer holds the payload or an error text of the database or the outbox.
func noLeak(t *testing.T, body []byte) {
	t.Helper()
	for _, text := range []string{"payload-value", "secret", "outbox:", "pq:", "ERROR", "invoice:i-1"} {
		if bytes.Contains(body, []byte(text)) {
			t.Fatalf("an answer holds %q: %s", text, body)
		}
	}
}

func TestParkedRoutes(t *testing.T) {
	t.Run("ST-SUP-RP14-LC09-T01 The parked list shows only the fixed fields to an allowed caller, pages, and refuses others alike", func(t *testing.T) {
		s := newParkedService(t, modes.Selection{modes.API})
		var own []string
		for range 3 {
			own = append(own, s.park(t, "t1", "billing", true).String())
		}
		s.park(t, "t2", "billing", true)
		target := parkedBase + "/stego/outbox/billing/parked?limit=2"
		var got []string
		for page := 0; ; page++ {
			status, body := s.call(t, "t1", "ops", http.MethodGet, target, "")
			if status != http.StatusOK {
				t.Fatalf("page %d: status %d: %s", page, status, body)
			}
			noLeak(t, body)
			var decoded struct {
				Items []map[string]any `json:"items"`
				Next  string           `json:"next"`
			}
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatal(err)
			}
			for _, item := range decoded.Items {
				keys := make([]string, 0, len(item))
				for key := range item {
					keys = append(keys, key)
				}
				slices.Sort(keys)
				if !slices.Equal(keys, []string{"attempts", "created_at", "destination", "failure_code", "id", "parked_at"}) ||
					item["destination"] != "billing" || item["attempts"] != float64(5) || item["failure_code"] != "refused" {
					t.Fatalf("an item is %v", item)
				}
				got = append(got, item["id"].(string))
			}
			if decoded.Next == "" {
				if page != 1 {
					t.Fatalf("the list ended after %d pages, want 2", page+1)
				}
				break
			}
			if len(decoded.Items) != 2 {
				t.Fatalf("page %d holds %d items, want 2", page, len(decoded.Items))
			}
			target = parkedBase + "/stego/outbox/billing/parked?limit=2&cursor=" + url.QueryEscape(decoded.Next)
		}
		if !slices.Equal(got, own) {
			t.Fatalf("the list gave %v, want the three messages of t1 %v", got, own)
		}
		// A caller with no grant gets 403, the same for a destination with parked messages and for one with none.
		denied, deniedBody := s.call(t, "t1", "nobody", http.MethodGet, parkedBase+"/stego/outbox/billing/parked", "")
		empty, emptyBody := s.call(t, "t1", "nobody", http.MethodGet, parkedBase+"/stego/outbox/shipping/parked", "")
		if denied != http.StatusForbidden || empty != http.StatusForbidden || !bytes.Equal(deniedBody, emptyBody) {
			t.Fatalf("a caller with no grant got %d %s and %d %s, want 403 alike", denied, deniedBody, empty, emptyBody)
		}
		// The grant of t1 serves no other tenant.
		if status, _ := s.call(t, "t2", "ops", http.MethodGet, parkedBase+"/stego/outbox/billing/parked", ""); status != http.StatusForbidden {
			t.Fatalf("the operator of t1 in t2 got %d, want 403", status)
		}
		// A cursor that was changed is refused with 400.
		if status, _ := s.call(t, "t1", "ops", http.MethodGet, parkedBase+"/stego/outbox/billing/parked?cursor=AAAA", ""); status != http.StatusBadRequest {
			t.Fatalf("a changed cursor got %d, want 400", status)
		}
	})
	t.Run("ST-SUP-RP14-LC09-T01 A process of the worker mode registers no parked-message route", func(t *testing.T) {
		s := newParkedService(t, modes.Selection{modes.Worker})
		s.park(t, "t1", "billing", true)
		if status, _ := s.call(t, "t1", "ops", http.MethodGet, parkedBase+"/stego/outbox/billing/parked", ""); status != http.StatusNotFound {
			t.Fatalf("the worker mode answered %d, want 404 for a route it does not serve", status)
		}
	})
	t.Run("ST-SUP-RP14-LC10-T01 A resolve through the route changes the message and writes its audit row in one transaction", func(t *testing.T) {
		s := newParkedService(t, modes.Selection{modes.API})
		first := s.park(t, "t1", "billing", true)
		other := s.park(t, "t1", "billing", true)
		foreign := s.park(t, "t2", "billing", true)
		resolve := func(id uuid.UUID) string {
			return parkedBase + "/stego/outbox/billing/parked/" + id.String() + ":resolve"
		}

		status, body := s.call(t, "t1", "ops", http.MethodPost, resolve(first), `{"resolution":"not_acted"}`)
		if status != http.StatusNoContent || len(body) != 0 {
			t.Fatalf("the resolution got %d %s, want 204", status, body)
		}
		var state, messageTx string
		if err := s.f.admin.QueryRow(`SELECT state, xmin::text FROM stego_outbox.messages WHERE id = $1`, first).Scan(&state, &messageTx); err != nil || state != "pending" {
			t.Fatalf("the message is %q after not_acted, want pending with the same identity: %v", state, err)
		}
		var action, actor, auditTx string
		var rows int
		if err := s.f.admin.QueryRow(`SELECT count(*) OVER (), action, actor_id, xmin::text FROM `+audit.RecordSchema+`.`+audit.EventsTable+
			` WHERE resource_type = $1 AND resource_id = $2`, ParkedResourceType, first.String()).Scan(&rows, &action, &actor, &auditTx); err != nil {
			t.Fatalf("no audit row of the resolution: %v", err)
		}
		if rows != 1 || action != ResolveAction || actor != personOf(t, "ops") || auditTx != messageTx {
			t.Fatalf("the audit row is %d rows, %q by %q in transaction %s, the message in %s; want one row in the same commit",
				rows, action, actor, auditTx, messageTx)
		}

		answers := map[string][]byte{}
		check := func(name string, want, status int, body []byte) {
			t.Helper()
			if status != want {
				t.Fatalf("%s got %d %s, want %d", name, status, body, want)
			}
			noLeak(t, body)
			answers[name] = body
		}
		status, body = s.call(t, "t1", "ops", http.MethodPost, resolve(first), `{"resolution":"not_acted"}`)
		check("a second resolve", http.StatusConflict, status, body)
		status, body = s.call(t, "t1", "nobody", http.MethodPost, resolve(other), `{"resolution":"not_acted"}`)
		check("a caller with no grant", http.StatusForbidden, status, body)
		status, body = s.call(t, "t1", "nobody", http.MethodPost, resolve(uuid.Must(uuid.NewV7())), `{"resolution":"not_acted"}`)
		check("a caller with no grant, a message that is not there", http.StatusForbidden, status, body)
		if !bytes.Equal(answers["a caller with no grant"], answers["a caller with no grant, a message that is not there"]) {
			t.Fatal("a deny differs between messages")
		}
		if err := s.f.admin.QueryRow(`SELECT state FROM stego_outbox.messages WHERE id = $1`, other).Scan(&state); err != nil || state != "parked" {
			t.Fatalf("the message of the denied resolve is %q, want parked: %v", state, err)
		}
		status, body = s.call(t, "t1", "ops", http.MethodPost, resolve(foreign), `{"resolution":"not_acted"}`)
		check("a message of another tenant", http.StatusNotFound, status, body)
		for _, bad := range []string{`{"resolution":"retry"}`, `{"resolution":"not_acted","extra":1}`, `{}`, `not json`, `{"resolution":"acted"} {}`} {
			status, body = s.call(t, "t1", "ops", http.MethodPost, resolve(other), bad)
			check("the body "+bad, http.StatusBadRequest, status, body)
		}
		// An outcome that the destination refuses: discarded for a message that need not land.
		loose := s.park(t, "t1", "billing", false)
		status, body = s.call(t, "t1", "ops", http.MethodPost, resolve(loose), `{"resolution":"discarded"}`)
		check("a refused outcome", http.StatusConflict, status, body)
		if err := s.f.admin.QueryRow(`SELECT state FROM stego_outbox.messages WHERE id = $1`, loose).Scan(&state); err != nil || state != "parked" {
			t.Fatalf("the message of the refused outcome is %q, want parked: %v", state, err)
		}
	})
}
