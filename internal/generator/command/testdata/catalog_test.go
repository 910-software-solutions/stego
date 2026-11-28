// Package check runs the generated command handlers of the catalog test service over HTTP, against a local
// PostgreSQL server (STEGO_TEST_POSTGRES_DSN) and a local SpiceDB server (STEGO_TEST_SPICEDB), with the runtime
// library wired as the service wires it: the schema gate of the release on the command runtime, and the
// authorization relay on an outbox worker.
package check

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/910-software-solutions/stego/registry/runtime/audit"
	"github.com/910-software-solutions/stego/registry/runtime/authn"
	"github.com/910-software-solutions/stego/registry/runtime/authn/authntest"
	"github.com/910-software-solutions/stego/registry/runtime/authz"
	"github.com/910-software-solutions/stego/registry/runtime/command"
	"github.com/910-software-solutions/stego/registry/runtime/idempotency"
	"github.com/910-software-solutions/stego/registry/runtime/outbox"
	"github.com/910-software-solutions/stego/registry/runtime/probe"
	"github.com/910-software-solutions/stego/registry/runtime/tenant"

	"example.com/orders/out/commands/handlers"
)

// tablesSQL is the migration 001_initial of the PostgreSQL adapter for the declaration (ST-SUP-RC01-OC41).
//
//go:embed tables.sql
var tablesSQL string

// schema is the schema of the catalog service. The test of the generator puts the schema text of the declaration
// here, so the check writes to SpiceDB and gives the schema gate the same text.
const schema = ``

const (
	issuer   = "https://issuer.authn.test/realms/r1"
	audience = "orders"
	password = "catalog-check"
)

var tables = []string{"orgs", "cost_items", "price_histories"}

// env is the running catalog service: its database with the runtime, worker and relay roles, its command runtime
// with the schema gate, the relay on an outbox worker, and its HTTP server.
type env struct {
	t        *testing.T
	ctx      context.Context
	admin    *sql.DB
	workerDB *sql.DB
	relayDB  *sql.DB
	rt       *command.Runtime
	az       *authz.Runtime
	server   *httptest.Server
	verifier *authn.Verifier
	key      *authntest.Key
	spice    *grpc.ClientConn
	provider *authz.SpiceDB
	decision *recorder
	rtConfig command.Config
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// service starts the catalog service for the test.
func service(t *testing.T) *env {
	t.Helper()
	dsn, endpoint := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_TEST_SPICEDB")
	if dsn == "" || endpoint == "" {
		t.Fatal("the check needs STEGO_TEST_POSTGRES_DSN and STEGO_TEST_SPICEDB: a missing result is not a pass")
	}
	if !strings.Contains(schema, "definition price_history") {
		t.Fatal("the test of the generator did not put the schema of the catalog service in the check")
	}
	e := &env{t: t, ctx: context.Background()}
	e.database(dsn)
	e.authorization(endpoint, os.Getenv("STEGO_TEST_SPICEDB_KEY"))
	e.start()
	return e
}

// database makes a database of its own with the runtime, outbox worker and relay roles, the runtime tables and
// the entity tables with row-level security on the tenant setting.
func (e *env) database(dsn string) {
	t := e.t
	root, err := sql.Open("pgx", dsn)
	must(t, err)
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	tag := hex.EncodeToString(suffix)
	name := "cat_" + tag
	roles := [3]string{"cat_rt_" + tag, "cat_worker_" + tag, "cat_relay_" + tag}
	_, err = root.ExecContext(e.ctx, "CREATE DATABASE "+name)
	must(t, err)
	for _, role := range roles {
		_, err := root.ExecContext(e.ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '"+password+"'")
		must(t, err)
	}
	u, err := url.Parse(dsn)
	must(t, err)
	u.Path = "/" + name
	e.admin, err = sql.Open("pgx", u.String())
	must(t, err)
	queueDDL, err := outbox.Migration("cat_check"+outbox.RelaySuffix, roles[0], roles[1])
	must(t, err)
	authzDDL, err := authz.Migration("authz", roles[0], roles[2], "cat_check"+outbox.RelaySuffix, roles[1])
	must(t, err)
	auditDDL, err := audit.DDL(audit.Config{RuntimeRole: roles[0], AnchorPeriod: time.Hour, RetentionMonths: 12})
	must(t, err)
	keysDDL, err := idempotency.Migration("cat_check_idempotency_purge", roles[0])
	must(t, err)
	setup := []string{
		queueDDL,
		"CREATE SCHEMA authz",
		"GRANT USAGE ON SCHEMA authz TO " + roles[0] + ", " + roles[1] + ", " + roles[2],
		authzDDL,
		auditDDL,
		keysDDL,
		"GRANT SELECT, INSERT, UPDATE ON idempotency_keys TO " + roles[0],
		tablesSQL,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON " + strings.Join(tables, ", ") + " TO " + roles[0],
		"GRANT " + roles[0] + " TO " + roles[1],
	}
	for _, table := range tables {
		setup = append(setup,
			"ALTER TABLE "+table+" ENABLE ROW LEVEL SECURITY",
			"ALTER TABLE "+table+" FORCE ROW LEVEL SECURITY",
			"CREATE POLICY "+table+"_tenant ON "+table+" USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))")
	}
	for _, statement := range setup {
		if _, err := e.admin.ExecContext(e.ctx, statement); err != nil {
			t.Fatalf("%.80s: %v", statement, err)
		}
	}
	dsnOf := func(role string) string {
		v := *u
		v.User = url.UserPassword(role, password)
		return v.String()
	}
	open := func(role string) *sql.DB {
		db, err := tenant.Open(e.ctx, tenant.Config{InsecureLoopback: true, DSN: dsnOf(role)})
		must(t, err)
		return db
	}
	pool, auditDB := open(roles[0]), open(roles[0])
	// The outbox worker and the relay are roles of the worker mode, which the pool of the runtime refuses.
	e.workerDB, err = sql.Open("pgx", dsnOf(roles[1]))
	must(t, err)
	e.relayDB, err = sql.Open("pgx", dsnOf(roles[2]))
	must(t, err)
	maintainAudit(t, e.ctx, pool)
	queue, err := outbox.New(pool, outbox.Options{})
	must(t, err)
	e.rtConfig = command.Config{DB: pool, AuditDB: auditDB, Outbox: queue, Service: "orders", ServiceVersion: "1.0.0",
		DeclarationVersion: "d1", Entities: handlers.Entities(), Probes: probe.NewRecorder(nil)}
	writer, err := audit.NewWriter(auditOutbox{q: queue}, nil)
	must(t, err)
	e.rtConfig.Audit = writer
}

// authorization connects the decision port to the local SpiceDB server, writes the schema and makes the schema gate
// of the command runtime from the same text, as service.New does from the schema text of the release
// (ST-SUP-RP10-LC59).
func (e *env) authorization(endpoint, key string) {
	t := e.t
	if key == "" {
		key = "catalog-check"
	}
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(bearer(key)))
	must(t, err)
	e.spice = conn
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	for {
		_, err = v1.NewSchemaServiceClient(conn).WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: schema})
		if err == nil || ctx.Err() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	must(t, err)
	e.provider, err = authz.NewSpiceDB(conn)
	must(t, err)
	e.decision = &recorder{inner: e.provider}
	e.az, err = authz.New(authz.Config{Provider: e.decision, DB: e.rtConfig.DB, Schema: "authz", Outbox: e.rtConfig.Outbox})
	must(t, err)
	e.rtConfig.Authz = e.az
	e.rtConfig.SchemaGate, err = authz.NewSchemaGate(schema, e.provider, 0)
	must(t, err)
}

// start starts the command runtime, the relay on an outbox worker of the worker role, and the HTTP server with the
// generated handlers behind the verifier.
func (e *env) start() {
	t := e.t
	var err error
	e.rt, err = command.New(e.ctx, e.rtConfig)
	must(t, err)
	relay, err := authz.NewRelay(authz.RelayConfig{Name: "authz-relay", Relationships: e.provider, DB: e.relayDB, Schema: "authz",
		Relations: []authz.Relation{{ResourceType: "cost_item", Name: "org"}, {ResourceType: "price_history", Name: "cost_item"}},
		Owners: map[string]authz.Owner{"cost_item": {Schema: "public", Table: "cost_items", Key: "id"},
			"price_history": {Schema: "public", Table: "price_histories", Key: "id"}}})
	must(t, err)
	wqueue, err := outbox.New(e.workerDB, outbox.Options{})
	must(t, err)
	config := outbox.DefaultWorkerConfig()
	config.PollInterval = 10 * time.Millisecond
	config.RetryMin = 10 * time.Millisecond
	config.RetryMax = 50 * time.Millisecond
	config.AttemptTimeout = 2 * time.Second
	config.LeaseDuration = 15 * time.Second
	config.PurgeEvery = time.Second
	worker, err := outbox.NewWorker(wqueue, config)
	must(t, err)
	must(t, worker.Register(relay.Destination()))
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = worker.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	e.key = authntest.NewKey(t, "k1")
	keys, err := authn.ParseKeys(authntest.Document(e.key.JWK()))
	must(t, err)
	e.verifier, err = authn.New(authn.Settings{Issuer: issuer, Audience: audience, TenantClaim: "tenant", ProblemCode: "ORD", Keys: keys})
	must(t, err)
	mux := http.NewServeMux()
	must(t, handlers.Register(mux, e.rt, handlers.BoundFills(), handlers.Options{}))
	e.server = httptest.NewServer(e.verifier.Middleware(mux))
	t.Cleanup(e.server.Close)
}

// bearer sends the preshared key of the local SpiceDB server on each call.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return false }

// recorder records each request of the decision port and passes it to SpiceDB.
type recorder struct {
	inner authz.Decision
	mu    sync.Mutex
	calls []authz.EvaluationRequest
}

func (r *recorder) Evaluate(ctx context.Context, request authz.EvaluationRequest) (authz.EvaluationResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, request)
	r.mu.Unlock()
	return r.inner.Evaluate(ctx, request)
}

func (r *recorder) EvaluateBatch(ctx context.Context, request authz.BatchRequest) ([]authz.EvaluationResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, request.Evaluations...)
	r.mu.Unlock()
	return r.inner.EvaluateBatch(ctx, request)
}

func (r *recorder) Search(ctx context.Context, request authz.SearchRequest) (authz.SearchResult, error) {
	return r.inner.Search(ctx, request)
}

func (r *recorder) Searches() bool { return r.inner.Searches() }

// taken gives the requests since the last call and forgets them.
func (r *recorder) taken() []authz.EvaluationRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	calls := r.calls
	r.calls = nil
	return calls
}

// auditOutbox is the audit writer's outbox: it enqueues each sink message on the queue.
type auditOutbox struct{ q *outbox.Queue }

func (o auditOutbox) Enqueue(ctx context.Context, tx audit.Tx, _, destination, key string, payload []byte) error {
	sqlTx, ok := tx.(*sql.Tx)
	if !ok {
		return errors.New("the audit outbox needs a *sql.Tx")
	}
	return o.q.Enqueue(ctx, sqlTx, outbox.Message{ID: uuid.New(), Destination: destination, ResourceKey: key,
		Kind: "audit.event", Payload: payload, Idempotent: true, MustLand: true})
}

// maintainAudit makes the audit partitions of this month and the next, as the controller's maintenance does.
func maintainAudit(t *testing.T, ctx context.Context, pool *sql.DB) {
	t.Helper()
	for range 64 {
		var more bool
		err := func() error {
			tx, err := pool.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if err := tx.QueryRowContext(ctx, `SELECT step_left FROM `+audit.MaintainFunction+`()`).Scan(&more); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err != nil {
			t.Fatalf("audit_maintain: %v", err)
		}
		if !more {
			return
		}
	}
	t.Fatal("audit_maintain did not finish")
}

// caller is one verified caller of a tenant, with a token.
type caller struct {
	tenant    tenant.Tenant
	principal authz.Principal
	token     string
}

// as mints a token for subject in tenantID and verifies it.
func (e *env) as(subject, tenantID string) caller {
	t := e.t
	t.Helper()
	claims := authntest.Claims(issuer, audience, subject, time.Now())
	claims["tenant"] = tenantID
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	c := caller{token: authntest.Mint(t, authntest.Header("k1"), claims, e.key)}
	id, err := e.verifier.Verify(e.ctx, c.token)
	must(t, err)
	c.tenant, err = tenant.New(tenant.Claim(id))
	must(t, err)
	c.principal, err = authz.Person("user", id)
	must(t, err)
	return c
}

// org seeds an Org row of the caller's tenant on which the caller has the relation editor in SpiceDB, and gives
// its ID.
func (e *env) org(c caller) string {
	t := e.t
	t.Helper()
	id := uuid.NewString()
	_, err := e.admin.ExecContext(e.ctx, `INSERT INTO orgs (tenant_id, id, name) VALUES ($1, $2, 'org-1')`, c.tenant.ID(), id)
	must(t, err)
	resourceID, err := authz.ResourceID(c.tenant, id)
	must(t, err)
	_, err = v1.NewPermissionsServiceClient(e.spice).WriteRelationships(e.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: "org", ObjectId: resourceID}, Relation: "editor",
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.principal.Subject().ID}}}}}})
	must(t, err)
	return id
}

// reply is one HTTP answer.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// post sends body to the path with the token, the idempotency key and the headers.
func (e *env) post(token, key, path, body string, headers ...string) reply {
	t := e.t
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, e.server.URL+"/api/v1"+path, strings.NewReader(body))
	must(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", `"`+key+`"`)
	request.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response, err := http.DefaultClient.Do(request)
	must(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	must(t, err)
	return reply{status: response.StatusCode, header: response.Header, body: data}
}

// complete waits until the relay landed every tuple of the revocation id of the caller, and fails the test when
// the revocation is not complete within one minute.
func (e *env) complete(c caller, id uuid.UUID) {
	t := e.t
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		status, _, err := e.az.WaitFor(e.ctx, c.tenant, c.principal, id, 5*time.Second)
		must(t, err)
		if status.State == authz.RevocationComplete {
			return
		}
		if status.State != authz.RevocationPending || time.Now().After(deadline) {
			t.Fatalf("the revocation %s of the create is %s, want complete", id, status.State)
		}
	}
}

func TestCatalog(t *testing.T) {
	t.Run("ST-SUP-RP10-LC59-T09 The generated command add-cost-item answers 201 for a CostItem with its first PriceHistory", func(t *testing.T) {
		e := service(t)
		a, outsider := e.as("u-7", "tA"), e.as("u-9", "tA")
		org := e.org(a)
		e.decision.taken()
		got := e.post(a.token, "lc59-t09-add", "/orgs/"+org+"/cost-items", `{"name":"bolt","priceCents":125}`)
		if got.status != http.StatusCreated {
			t.Fatalf("add-cost-item answered %d %s, want 201", got.status, got.body)
		}
		// The reference of the PriceHistory to the CostItem that the same attempt created is decided by the witness,
		// with no call to SpiceDB: no decision of the command is on a cost_item.
		for _, call := range e.decision.taken() {
			if call.Resource.Type == "cost_item" {
				t.Errorf("add-cost-item asked SpiceDB for %s on the new CostItem %s, want the decision of the witness", call.Action.Name, call.Resource.ID)
			}
		}
		var created struct {
			CostItemID     string `json:"costItemId"`
			PriceHistoryID string `json:"priceHistoryId"`
		}
		must(t, json.Unmarshal(got.body, &created))
		if created.CostItemID == "" || created.PriceHistoryID == "" {
			t.Fatalf("add-cost-item answered %s, want the IDs of the CostItem and of the PriceHistory", got.body)
		}
		if state := got.header.Get(authz.RevocationHeader); state != string(authz.RevocationPending) {
			t.Errorf("%s is %q, want pending", authz.RevocationHeader, state)
		}
		location := got.header.Get(authz.RevocationLocationHeader)
		id, err := uuid.Parse(strings.TrimPrefix(location, authz.RevocationPath))
		if !strings.HasPrefix(location, authz.RevocationPath) || err != nil {
			t.Fatalf("%s is %q, want the path of a revocation", authz.RevocationLocationHeader, location)
		}
		e.complete(a, id)

		show := "/cost-items/" + created.CostItemID + "/prices:show"
		body := `{"priceHistoryId":"` + created.PriceHistoryID + `"}`
		read := e.post(a.token, "lc59-t09-show", show, body, authz.RevocationAfterHeader, location)
		if read.status != http.StatusOK {
			t.Fatalf("show-cost-item of the caller answered %d %s, want 200", read.status, read.body)
		}
		var shown struct {
			Name       string          `json:"name"`
			PriceCents json.RawMessage `json:"priceCents"`
			CostItemID string          `json:"costItemId"`
		}
		must(t, json.Unmarshal(read.body, &shown))
		if shown.Name != "bolt" || strings.Trim(string(shown.PriceCents), `"`) != "125" || shown.CostItemID != created.CostItemID {
			t.Errorf("show-cost-item answered %s, want the CostItem bolt and its PriceHistory of 125", read.body)
		}
		denied := e.post(outsider.token, "lc59-t09-outsider", show, body, authz.RevocationAfterHeader, location)
		if denied.status != http.StatusNotFound {
			t.Errorf("show-cost-item of a user with no catalog_view on the Org answered %d %s, want 404", denied.status, denied.body)
		}
	})
}
