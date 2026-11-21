// Package check runs the example user-management on the runtime data path as an operator runs it: the migration
// job and then the service binary, built from the generated main.go, against a local PostgreSQL server
// (STEGO_TEST_POSTGRES_DSN) and a local SpiceDB container (STEGO_TEST_SPICEDB), and calls its collections over HTTP.
// The example binds the event-publisher mixin, so its worker mode sends the change events to the local Kafka broker
// of kafkatest (ST-SUP-RC20-OC40).
package check

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/kafkatest"
	"github.com/jsell-rh/stego/registry/runtime/outbox/kafkaoutbox"
	"github.com/jsell-rh/stego/registry/runtime/tenant"

	"github.com/example/service/out/internal/stegoruntime"
	"github.com/example/service/out/internal/tenancy"
)

const (
	issuer   = "https://issuer.example.test/realms/users"
	audience = "user-management"
	password = "example-check"
	base     = "/api/user-mgmt/v1"
)

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func execAll(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%.100s: %v", statement, err)
		}
	}
}

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func writeFile(t *testing.T, name string, data []byte) string {
	file := filepath.Join(t.TempDir(), name)
	must(t, os.WriteFile(file, data, 0o600))
	return file
}

// service is the running example: its database, its SpiceDB and its address.
type service struct {
	t     *testing.T
	ctx   context.Context
	admin *sql.DB
	spice *grpc.ClientConn
	key   *authntest.Key
	base  string
	log   *bytes.Buffer
	// cursor is the cursor key of the service, and processEnv the environment of the service binary on a port.
	cursor     []byte
	processEnv func(port string) []string
	// migrateJob runs the migration job of the binary once on the database.
	migrateJob func() ([]byte, error)
}

// TestMain removes the Kafka container of kafkatest when the tests end.
func TestMain(m *testing.M) { os.Exit(kafkatest.Wrap(m.Run)) }

// changeTopic is the declared topic of the change events of the example, which the first test that starts the
// service makes once.
const changeTopic = "user-management.changes"

var topicOnce sync.Once

// start makes a database of its own with the roles of the tenancy component, runs the migration job, and starts the
// service until it answers. The database is on the loopback address, which the loopback test mode of the adapter
// takes with no TLS.
func start(t *testing.T) *service {
	s := migrated(t)
	s.serve()
	return s
}

// migrated makes a database of its own with the roles of the tenancy component and runs the migration job on it.
func migrated(t *testing.T) *service {
	dsn, endpoint, binary := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_TEST_SPICEDB"), os.Getenv("STEGO_EXAMPLE_BINARY")
	if dsn == "" || endpoint == "" || binary == "" {
		t.Fatal("the check needs STEGO_TEST_POSTGRES_DSN, STEGO_TEST_SPICEDB and STEGO_EXAMPLE_BINARY: a missing result is not a pass")
	}
	server, err := url.Parse(dsn)
	must(t, err)
	root, err := sql.Open("pgx", dsn)
	must(t, err)
	t.Cleanup(func() { root.Close() })
	t.Cleanup(func() {
		for _, role := range []string{stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole, stegoruntime.OwnerRole} {
			_, _ = root.Exec("DROP ROLE IF EXISTS " + quote(role))
		}
	})
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "example_" + hex.EncodeToString(suffix)
	execAll(t, root, "CREATE DATABASE "+name)
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)") })
	u := *server
	u.Path = "/" + name
	s := &service{t: t, ctx: context.Background(), log: &bytes.Buffer{}}
	s.admin, err = sql.Open("pgx", u.String())
	must(t, err)
	t.Cleanup(func() { s.admin.Close() })
	execAll(t, s.admin, tenancy.Roles)
	for _, role := range []string{stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole, stegoruntime.OwnerRole} {
		execAll(t, s.admin, "ALTER ROLE "+quote(role)+" LOGIN PASSWORD '"+password+"'")
	}
	as := func(role string) string {
		v := u
		v.User = url.UserPassword(role, password)
		q := v.Query()
		q.Set("sslmode", "disable")
		v.RawQuery = q.Encode()
		return v.String()
	}
	spiceKey := os.Getenv("STEGO_TEST_SPICEDB_KEY")
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(bearer(spiceKey)))
	must(t, err)
	t.Cleanup(func() { conn.Close() })
	s.spice = conn
	env := []string{"STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK=1", "STEGO_SPICEDB_ENDPOINT=" + endpoint,
		"STEGO_SPICEDB_KEY_FILE=" + writeFile(t, "spicedb.key", []byte(spiceKey)), "STEGO_SPICEDB_INSECURE_LOOPBACK=1"}
	s.migrateJob = func() ([]byte, error) {
		ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
		defer cancel()
		migrate := exec.CommandContext(ctx, binary, "migrate")
		migrate.Env = append(append(os.Environ(), env...), "STEGO_MIGRATION_DATABASE_URL="+as(stegoruntime.OwnerRole))
		return migrate.CombinedOutput()
	}
	if output, err := s.migrateJob(); err != nil {
		t.Fatalf("the migration job failed: %v\n%s", err, output)
	}
	broker := kafkatest.Start(t)
	topicOnce.Do(func() { broker.CreateTopic(t, changeTopic, 3) })
	env = append(env, kafkaoutbox.BrokersSetting+"="+broker.Plain.Addr(), kafkaoutbox.InsecureLoopbackSetting+"=1",
		kafkaoutbox.TopicSetting+"="+changeTopic)
	s.processEnv = func(port string) []string {
		return append(append(os.Environ(), env...), "PORT="+port, "DATABASE_URL="+as(stegoruntime.RuntimeRole),
			"STEGO_WORKER_DATABASE_URL="+as(stegoruntime.WorkerRole), "STEGO_RELAY_DATABASE_URL="+as(stegoruntime.RelayRole),
			"STEGO_AUTHN_KEYS_FILE="+writeFile(t, "keys.json", authntest.Document(s.key.JWK())),
			"STEGO_AUTHN_ISSUER="+issuer, "STEGO_AUTHN_AUDIENCE="+audience,
			"STEGO_QUERY_CURSOR_KEY_FILE="+writeFile(t, "cursor.key", s.cursor), "STEGO_SERVICE_VERSION=1.0.0")
	}
	s.key = authntest.NewKey(t, "k1")
	s.cursor = make([]byte, 32)
	_, _ = rand.Read(s.cursor)
	return s
}

// freePort gives a free port of the loopback address.
func freePort(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer listener.Close()
	return fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
}

// serve starts the service binary on the migrated database until it answers /livez.
func (s *service) serve() {
	t := s.t
	port := freePort(t)
	s.base = "http://127.0.0.1:" + port
	process := exec.Command(os.Getenv("STEGO_EXAMPLE_BINARY"))
	process.Env = s.processEnv(port)
	process.Stdout, process.Stderr = s.log, s.log
	must(t, process.Start())
	exited := make(chan struct{})
	go func() { _ = process.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = process.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = process.Process.Kill()
		}
		if t.Failed() {
			t.Logf("service log:\n%s", s.log)
		}
	})
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		response, err := http.Get(s.base + "/livez")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-exited:
			t.Fatalf("the service stopped before it answered /livez:\n%s", s.log)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the service does not answer /livez: %v\n%s", err, s.log)
		}
	}
}

// bearer sends the preshared key of the local SpiceDB server on each call.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return false }

// caller is one caller of a tenant, with a token that holds its roles.
type caller struct {
	tenant  tenant.Tenant
	subject string
	token   string
}

func (s *service) as(subject, tenantID string, roles ...string) caller {
	c := s.mint(subject, tenantID, roles...)
	// The tenant of the caller is provisioned, so the tenant check of the generated service lets its requests through
	// (ST-SUP-RC27-OC11); the checks of this file prove other contracts, so the fixture adds it to the tenant list as
	// the administrator, not through Provision. TestProvisionRetire provisions through Provision.
	if _, err := s.admin.Exec(`INSERT INTO stego_tenants (tenant_id) VALUES ($1) ON CONFLICT DO NOTHING`, tenantID); err != nil {
		s.t.Fatalf("provision the tenant %s of the check: %v", tenantID, err)
	}
	return c
}

// mint mints a token for subject in tenantID with the roles given, and does not provision the tenant.
func (s *service) mint(subject, tenantID string, roles ...string) caller {
	t := s.t
	claims := authntest.Claims(issuer, audience, subject, time.Now())
	claims["tenant"] = tenantID
	if len(roles) > 0 {
		claims["roles"] = roles
	}
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	token := authntest.Mint(t, authntest.Header("k1"), claims, s.key)
	keys, err := authn.ParseKeys(authntest.Document(s.key.JWK()))
	must(t, err)
	verifier, err := authn.New(authn.Settings{Issuer: issuer, Audience: audience, TenantClaim: "tenant", ProblemCode: "CHECK", Keys: keys})
	must(t, err)
	defer verifier.Stop()
	id, err := verifier.Verify(s.ctx, token)
	must(t, err)
	c := caller{token: token}
	c.tenant, err = tenant.New(tenant.Claim(id))
	must(t, err)
	principal, err := authz.Person("user", id)
	must(t, err)
	c.subject = principal.Subject().ID
	return c
}

// relate writes the relation of a resource of the caller's tenant in SpiceDB to the caller.
func (s *service) relate(c caller, resourceType, id, relation string) {
	resourceID, err := authz.ResourceID(c.tenant, id)
	must(s.t, err)
	_, err = v1.NewPermissionsServiceClient(s.spice).WriteRelationships(s.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID}, Relation: relation,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject}}}}}})
	must(s.t, err)
}

type reply struct {
	status int
	body   []byte
}

func (r reply) member(t *testing.T, name string) string {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(r.body, &document); err != nil {
		t.Fatalf("the answer %s is not a JSON object: %v", r.body, err)
	}
	value, _ := document[name].(string)
	return value
}

func (r reply) items(t *testing.T) []map[string]any {
	t.Helper()
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(r.body, &page); err != nil {
		t.Fatalf("the answer %s is not a page: %v", r.body, err)
	}
	return page.Items
}

// send sends one request as the caller, with the token in the header that the example declares for jwt-auth.
func (s *service) send(c caller, method, path, body string) reply {
	s.t.Helper()
	return s.sendKeyed(c, method, path, "", body)
}

// sendKeyed sends one request as send does, with an Idempotency-Key when key is not empty.
func (s *service) sendKeyed(c caller, method, path, key, body string) reply {
	t := s.t
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, s.base+base+path, reader)
	must(t, err)
	request.Header.Set("X-Internal-Token", "Bearer "+c.token)
	if key != "" {
		request.Header.Set("Idempotency-Key", `"`+key+`"`)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	must(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	must(t, err)
	return reply{status: response.StatusCode, body: data}
}

func (s *service) count(query string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.admin.QueryRow(query, args...).Scan(&n); err != nil {
		s.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestExample(t *testing.T) {
	s := start(t)
	t.Run("ST-SUP-RC20-OC39-T02 The example user-management runs on the runtime data path with its scoped collections, upsert, implicit fields and fills", func(t *testing.T) {
		s.t = t
		owner := s.as("u-owner", "t-acme", "member")
		// The chain of organizations.before_create refuses a reserved name, and creates the organization.
		if r := s.send(owner, http.MethodPost, "/organizations", `{"name":"system org"}`); r.status != http.StatusBadRequest {
			t.Errorf("a reserved organization name answered %d %s, want 400 from org-name-validator", r.status, r.body)
		}
		created := s.send(owner, http.MethodPost, "/organizations", `{"name":"acme"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create of an organization answered %d %s, want 201", created.status, created.body)
		}
		org := created.member(t, "id")
		s.relate(owner, "organization", org, "owner")
		// A duplicate unique name is a conflict.
		if r := s.send(owner, http.MethodPost, "/organizations", `{"name":"acme"}`); r.status != http.StatusConflict {
			t.Errorf("a duplicate organization name answered %d %s, want 409", r.status, r.body)
		}
		// The gates of org-users.before_create: a caller with no role is refused; a member may not create an admin.
		noRole := s.as("u-norole", "t-acme")
		s.relate(noRole, "organization", org, "owner")
		if r := s.send(noRole, http.MethodPost, "/organizations/"+org+"/users", `{"email":"a@acme.test","display_name":"A","role":"member"}`); r.status != http.StatusForbidden {
			t.Errorf("a create with no role answered %d %s, want 403 from rbac-policy", r.status, r.body)
		}
		if r := s.send(owner, http.MethodPost, "/organizations/"+org+"/users", `{"email":"b@acme.test","display_name":"B","role":"admin"}`); r.status != http.StatusForbidden {
			t.Errorf("a member that creates an admin answered %d %s, want 403 from admin-creation-policy", r.status, r.body)
		}
		user := s.send(owner, http.MethodPost, "/organizations/"+org+"/users", `{"email":"c@acme.test","display_name":"C","role":"viewer","metadata":{"team":"core"}}`)
		if user.status != http.StatusCreated {
			t.Fatalf("the create of a user answered %d %s, want 201", user.status, user.body)
		}
		userID := user.member(t, "id")
		if n := s.count(`SELECT count(*) FROM users WHERE id = $1::uuid AND org_id = $2::uuid AND tenant_id = 't-acme' AND metadata = '{"team":"core"}'::jsonb`, userID, org); n != 1 {
			t.Errorf("%d user rows of the organization and the tenant with the jsonb metadata, want 1", n)
		}
		s.relate(owner, "member", userID, "owner")
		// The view and the edit of a member need the view of its organization too, through the relation that the relay
		// writes after the commit (ST-SUP-RC20-OC24, ST-SUP-Q156): the patch waits for it.
		memberID, err := authz.ResourceID(owner.tenant, userID)
		must(t, err)
		s.awaitAllowed(owner, "member", memberID, "edit")
		if r := s.send(owner, http.MethodPatch, "/organizations/"+org+"/users/"+userID, `{"display_name":"Cee"}`); r.status != http.StatusOK || r.member(t, "display_name") != "Cee" {
			t.Errorf("the patch of the user answered %d %s, want 200 with the new name", r.status, r.body)
		}
		if items := s.send(owner, http.MethodGet, "/users?search="+url.QueryEscape("display_name = 'Cee'"), "").items(t); len(items) != 1 {
			t.Errorf("the search of all users gave %d users, want 1", len(items))
		}
		// An owner of the member who may not view its organization gets 404 for each route of the member: the routes
		// decide only on the member (ST-SUP-Q156), and the view and the edit of member are intersections with
		// organization->view, which keep the access of an earlier STEGO (security review of PR #409, M2).
		memberOwner := s.as("u-member-owner", "t-acme", "member")
		s.relate(memberOwner, "member", userID, "owner")
		item := "/organizations/" + org + "/users/" + userID
		for _, w := range []struct{ method, body string }{
			{http.MethodGet, ""},
			{http.MethodPut, `{"email":"c@acme.test","display_name":"Owned","role":"viewer"}`},
			{http.MethodPatch, `{"display_name":"Owned"}`},
			{http.MethodDelete, ""},
		} {
			if r := s.send(memberOwner, w.method, item, w.body); r.status != http.StatusNotFound {
				t.Errorf("the %s of the member by its owner with no view of the organization answered %d %s, want 404", w.method, r.status, r.body)
			}
		}
		if n := s.count(`SELECT count(*) FROM public.users WHERE id = $1::uuid AND display_name = 'Cee'`, userID); n != 1 {
			t.Errorf("the member changed after the refused writes of its owner with no view of the organization")
		}
		// The upsert of org-settings creates the row of its key and then updates it.
		first := s.send(owner, http.MethodPut, "/organizations/"+org+"/orgsettings", `{"key":"theme","value":{"dark":true},"generation":1}`)
		// The relay writes the relation of the new setting to its organization after the commit; the setting is
		// seen, and found by its key, once SpiceDB holds it.
		for deadline := time.Now().Add(30 * time.Second); len(s.send(owner, http.MethodGet, "/organizations/"+org+"/orgsettings", "").items(t)) == 0; time.Sleep(200 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the relay did not write the relation of the setting to its organization:\n%s", s.log)
			}
		}
		second := s.send(owner, http.MethodPut, "/organizations/"+org+"/orgsettings", `{"key":"theme","value":{"dark":false},"generation":2}`)
		if first.status != http.StatusCreated || second.status != http.StatusOK || first.member(t, "id") != second.member(t, "id") {
			t.Errorf("the upserts answered %d %s and %d %s, want 201 and 200 for one row", first.status, first.body, second.status, second.body)
		}
		// The activity entries of a user carry the implicit source type, under the path prefix of their collection. A
		// body that gives another source type is refused, and one that leaves it out gets it (ST-SUP-RC20-OC23).
		if r := s.send(owner, http.MethodPost, "/user-activity", `{"source_type":"Organization","source_id":"`+userID+`","action":"login"}`); r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"field":"source_type"`) {
			t.Errorf("a user activity entry with the source type Organization answered %d %s, want 400 that names source_type", r.status, r.body)
		}
		event := s.send(owner, http.MethodPost, "/user-activity", `{"source_id":"`+userID+`","action":"login"}`)
		if event.status != http.StatusCreated || event.member(t, "source_type") != "User" {
			t.Errorf("the create of a user activity entry answered %d %s, want 201 with the source type User", event.status, event.body)
		}
		s.relate(owner, "activity_entry", event.member(t, "id"), "owner")
		if items := s.send(owner, http.MethodGet, "/org-activity", "").items(t); len(items) != 0 {
			t.Errorf("the list of organization activity holds %d entries, want none", len(items))
		}
		// Another tenant sees none of it.
		stranger := s.as("u-owner", "t-other", "admin")
		if r := s.send(stranger, http.MethodGet, "/organizations/"+org, ""); r.status != http.StatusNotFound {
			t.Errorf("the read from another tenant answered %d %s, want 404", r.status, r.body)
		}
		// Each write left its audit row in the tenant.
		if n := s.count(`SELECT count(*) FROM audit_event WHERE tenant_id = 't-acme' AND outcome = 'ok' AND action IN ('create', 'patch', 'upsert')`); n < 5 {
			t.Errorf("%d ok audit rows of the tenant, want at least 5", n)
		}
	})
}

func TestSchemaManifest(t *testing.T) {
	t.Run("ST-SUP-RC01-OC12-T08 A start on a database that does not satisfy the schema manifest alerts, and the migration job refuses it", func(t *testing.T) {
		// Each change makes the database that the migration job made differ from the schema manifest of the code
		// once, as a change by hand after a release does; the service then restarts on it and alerts, and the
		// migration job run again refuses it, unless a migration text of the runtime makes the object again
		// (decision on finding F10, owner, 2026-11-13).
		changes := []struct {
			name, statement, fault string
			healed                 bool
		}{
			{"a dropped unique index", `DROP INDEX public.organizations_name_key`,
				"table public.organizations: index organizations_name_key is missing", false},
			{"an upsert key index on other columns", `DROP INDEX public.org_settings_org_id_key_key;
CREATE UNIQUE INDEX org_settings_org_id_key_key ON public.org_settings (tenant_id, key)`,
				"table public.org_settings: index org_settings_org_id_key_key does not have the columns (tenant_id, org_id, key) of the manifest", false},
			{"an upsert key index with a predicate", `DROP INDEX public.org_settings_org_id_key_key;
CREATE UNIQUE INDEX org_settings_org_id_key_key ON public.org_settings (tenant_id, org_id, key) WHERE generation > 0`,
				"table public.org_settings: index org_settings_org_id_key_key does not have the predicate of the manifest", false},
			{"a dropped trigger of the audit record", `DROP TRIGGER audit_event_reject_row ON public.audit_event`,
				"table public.audit_event: trigger audit_event_reject_row is missing", true},
			{"disabled row-level security", `ALTER TABLE public.users DISABLE ROW LEVEL SECURITY`,
				"table public.users: row-level security is not enabled and forced", false},
			{"a changed column type", `ALTER TABLE public.users ALTER COLUMN email TYPE text`,
				"table public.users: column email has the type text, and the manifest needs character varying(255)", false},
			{"a dropped check constraint", `ALTER TABLE public.users DROP CONSTRAINT users_role_check`,
				"table public.users: constraint users_role_check is missing", false},
		}
		for _, change := range changes {
			t.Run(change.name, func(t *testing.T) {
				s := migrated(t)
				execAll(t, s.admin, change.statement)
				s.serve()
				// serve waits for /livez; the alert is written before the service answers.
				log := s.log.String()
				if !strings.Contains(log, "the database does not satisfy the schema manifest") || !strings.Contains(log, change.fault) ||
					!strings.Contains(log, `"event.name":"schema.manifest.mismatch"`) {
					t.Fatalf("the start does not alert %q with schema.manifest.mismatch:\n%s", change.fault, log)
				}
				if strings.Contains(log, password) || strings.Contains(log, "postgres://") {
					t.Fatalf("the log of the start holds a credential:\n%s", log)
				}
				output, err := s.migrateJob()
				switch {
				case change.healed && err != nil:
					t.Fatalf("the migration job, whose texts make the object again, failed: %v\n%s", err, output)
				case !change.healed && (err == nil || !strings.Contains(string(output), `"stage":"migrate.manifest_before_backfill"`)):
					t.Fatalf("the migration job gave %v, want a refusal at the stage migrate.manifest_before_backfill:\n%s", err, output)
				}
			})
		}
		t.Run("the database of the migration job starts", func(t *testing.T) {
			s := migrated(t)
			s.serve()
		})
	})
}

func TestStartFailure(t *testing.T) {
	t.Run("ST-SUP-RP14-LC05-T04 The service binary of the example names the missing setting or the database in its failure record, with no value", func(t *testing.T) {
		s := migrated(t)
		run := func(env []string) string {
			process := exec.Command(os.Getenv("STEGO_EXAMPLE_BINARY"))
			process.Env = env
			output, err := process.CombinedOutput()
			if err == nil {
				t.Fatalf("the service started:\n%s", output)
			}
			if strings.Contains(string(output), password) || strings.Contains(string(output), "postgres://") {
				t.Fatalf("the failure of the start shows a credential:\n%s", output)
			}
			return string(output)
		}
		without := func(env []string, name string) []string {
			var kept []string
			for _, entry := range env {
				if !strings.HasPrefix(entry, name+"=") {
					kept = append(kept, entry)
				}
			}
			return kept
		}
		env := without(without(s.processEnv(freePort(t)), "DATABASE_URL"), "DATABASE_URL_FILE")
		if output := run(env); !strings.Contains(output, `"stage":"service.start.setting.postgres-adapter.DATABASE_URL"`) {
			t.Fatalf("a start with no DATABASE_URL does not name the setting in its failure record:\n%s", output)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		must(t, err)
		closed := listener.Addr().String()
		must(t, listener.Close())
		unreachable := append(env, "DATABASE_URL=postgres://"+stegoruntime.RuntimeRole+":"+password+"@"+closed+"/app?sslmode=disable")
		if output := run(unreachable); !strings.Contains(output, `"stage":"service.start.database"`) {
			t.Fatalf("a start on a closed database port does not name the database in its failure record:\n%s", output)
		}
	})
}

// lockedBuffer is the log of a service process that the check stops and starts again: the process writes it while
// the check reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// rawCodec passes the bytes of each message through the proxy as they are.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) { return *(v.(*[]byte)), nil }
func (rawCodec) Unmarshal(data []byte, v any) error {
	*(v.(*[]byte)) = append([]byte(nil), data...)
	return nil
}
func (rawCodec) Name() string { return "proto" }

const (
	reflectSchemaMethod   = "/authzed.api.v1.SchemaService/ReflectSchema"
	checkPermissionMethod = "/authzed.api.v1.PermissionsService/CheckPermission"
)

// schemaReads is a gRPC proxy in front of the local SpiceDB server, which the service reaches SpiceDB through. It
// forwards each call as it is, and counts the calls of each method: only the worker-tuples job and Retire read the
// worker relations of the schema with ReflectSchema (authz.SpiceDB.RelationsOf), and a service whose schema has no
// definition tenant does neither. The counts of the other methods show that the service's calls pass the proxy.
type schemaReads struct {
	addr  string
	mu    sync.Mutex
	calls map[string]int
}

// startSchemaReads starts the proxy to the SpiceDB server at target, and stops it when the test ends.
func startSchemaReads(t *testing.T, target string) *schemaReads {
	backend, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	must(t, err)
	t.Cleanup(func() { backend.Close() })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	r := &schemaReads{addr: listener.Addr().String(), calls: map[string]int{}}
	server := grpc.NewServer(grpc.ForceServerCodec(rawCodec{}), grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(stream)
		r.mu.Lock()
		r.calls[method]++
		r.mu.Unlock()
		incoming, _ := metadata.FromIncomingContext(stream.Context())
		ctx := metadata.NewOutgoingContext(stream.Context(), metadata.MD{"authorization": incoming.Get("authorization")})
		client, err := backend.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, method, grpc.ForceCodec(rawCodec{}))
		if err != nil {
			return err
		}
		go func() {
			for {
				var message []byte
				if err := stream.RecvMsg(&message); err != nil {
					_ = client.CloseSend()
					return
				}
				if err := client.SendMsg(&message); err != nil {
					return
				}
			}
		}()
		for first := true; ; first = false {
			var message []byte
			if err := client.RecvMsg(&message); err != nil {
				stream.SetTrailer(client.Trailer())
				if err == io.EOF {
					return nil
				}
				return err
			}
			if first {
				if header, err := client.Header(); err == nil {
					_ = stream.SendHeader(header)
				}
			}
			if err := stream.SendMsg(&message); err != nil {
				return err
			}
		}
	}))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return r
}

// count gives the number of calls of method through the proxy.
func (r *schemaReads) count(method string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[method]
}

// rollout is the service process of TestProvisionRetire, which the check stops and starts again as a rollout does.
type rollout struct {
	s      *service
	env    []string
	log    *lockedBuffer
	exited chan struct{}
	stop   func()
}

// start starts the service process in all modes and waits until it answers /livez; the wait has no deadline of its
// own.
func (r *rollout) start(t *testing.T) {
	t.Helper()
	process := exec.Command(os.Getenv("STEGO_EXAMPLE_BINARY"))
	process.Env = r.env
	process.Stdout, process.Stderr = r.log, r.log
	must(t, process.Start())
	exited := make(chan struct{})
	go func() { _ = process.Wait(); close(exited) }()
	r.exited = exited
	r.stop = func() {
		_ = process.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(30 * time.Second):
			_ = process.Process.Kill()
			<-exited
		}
	}
	for !r.probe(t, "/livez") {
	}
}

// probe gets the probe path once and tells whether it answered 200. It fails the test when the process exited, and
// waits a little before it gives false, so a caller may call it again at once.
func (r *rollout) probe(t *testing.T, path string) bool {
	t.Helper()
	select {
	case <-r.exited:
		t.Fatalf("the service stopped before %s answered 200:\n%s", path, r.log)
	default:
	}
	response, err := http.Get(r.s.base + path)
	if err == nil {
		response.Body.Close()
		if response.StatusCode == http.StatusOK {
			return true
		}
	}
	time.Sleep(100 * time.Millisecond)
	return false
}

// awaitReady waits until /readyz and /readyz/modes both answer 200: the workers of the worker and the controller
// modes start after the HTTP server.
func (r *rollout) awaitReady(t *testing.T) {
	t.Helper()
	for !r.probe(t, "/readyz") || !r.probe(t, "/readyz/modes") {
	}
}

// healthy fails the test unless /readyz and /readyz/modes answer 200 now with one line only: the line that names a
// failed or degraded job, such as worker-tuples (ST-SUP-RC35-OC13), is refused.
func (r *rollout) healthy(t *testing.T, when string) {
	t.Helper()
	for _, path := range []string{"/readyz", "/readyz/modes"} {
		response, err := http.Get(r.s.base + path)
		must(t, err)
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		must(t, err)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s, %s answered %d %s, want 200", when, path, response.StatusCode, body)
		}
		if text := strings.TrimSpace(string(body)); text == "" || strings.Contains(text, "\n") || strings.Contains(text, "job") {
			t.Errorf("%s, %s answered 200 with %q, want one line that names no job", when, path, body)
		}
	}
}

// controllerHolder waits until a controller other than not holds the controller lease, and gives it. A controller
// runs the first run of each periodic job when it takes the lease.
func (r *rollout) controllerHolder(t *testing.T, not string) string {
	t.Helper()
	for {
		var holder sql.NullString
		err := r.s.admin.QueryRow(`SELECT holder FROM public.leases WHERE name = $1 AND expires_at > pg_catalog.now()`,
			"stego.controller").Scan(&holder)
		if err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		if holder.Valid && holder.String != "" && holder.String != not {
			return holder.String
		}
		select {
		case <-r.exited:
			t.Fatalf("the service stopped before a controller held the lease:\n%s", r.log)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// awaitAllowed waits until SpiceDB answers, at its quantized revision, that c has the permission on the resource, as
// the service asks it.
func (s *service) awaitAllowed(c caller, resourceType, resourceID, permission string) {
	for {
		answer, err := v1.NewPermissionsServiceClient(s.spice).CheckPermission(s.ctx, &v1.CheckPermissionRequest{
			Consistency: &v1.Consistency{Requirement: &v1.Consistency_MinimizeLatency{MinimizeLatency: true}},
			Resource:    &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID}, Permission: permission,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject}}})
		if err == nil && answer.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// grantOperator writes the relation operator of c on the service-level resource service:user-management, whose ID is
// not tenant-qualified (ST-SUP-RP06-LC22), as the operator of the platform does.
func (s *service) grantOperator(c caller) {
	_, err := v1.NewPermissionsServiceClient(s.spice).WriteRelationships(s.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: "service", ObjectId: stegoruntime.Service}, Relation: "operator",
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject}}}}}})
	must(s.t, err)
}

// short gives a new random suffix of 8 hex digits.
func short() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// TestProvisionRetire runs the tenant provisioning of the example through its generated routes: the schema of the
// example has no definition tenant, so the service has no worker-tuples job and Retire reads no worker relation of
// the schema (ST-SUP-RC35-OC13). Each wait has no deadline of its own: only the timeout of go test bounds it.
func TestProvisionRetire(t *testing.T) {
	t.Run("ST-SUP-RC35-OC13-T13 The generated user-management example provisions a tenant, creates and reads an organization, retires the tenant, and stays ready", func(t *testing.T) {
		s := migrated(t)
		reads := startSchemaReads(t, os.Getenv("STEGO_TEST_SPICEDB"))
		port := freePort(t)
		s.base = "http://127.0.0.1:" + port
		// The service reaches SpiceDB through the proxy: the last value of a variable wins.
		r := &rollout{s: s, log: &lockedBuffer{}, env: append(s.processEnv(port), "STEGO_SPICEDB_ENDPOINT="+reads.addr)}
		t.Cleanup(func() {
			if r.stop != nil {
				r.stop()
			}
			if t.Failed() {
				t.Logf("service log:\n%s", r.log)
			}
		})
		r.start(t)
		r.awaitReady(t)
		first := r.controllerHolder(t, "")
		r.healthy(t, "after the start")

		// The operator holds provision and retire on service:user-management, which is of no tenant; the operator's
		// own tenant is never provisioned.
		operator := s.mint("op-"+short(), "ops-"+short())
		s.grantOperator(operator)
		s.awaitAllowed(operator, "service", stegoruntime.Service, "provision")
		id := "acme-" + short()
		if got := s.sendKeyed(operator, http.MethodPost, "/stego/tenants", "k-"+short(), `{"tenant_id":"`+id+`"}`); got.status != http.StatusCreated || got.member(t, "tenant_id") != id {
			t.Fatalf("the provision of %s answered %d %s, want 201", id, got.status, got.body)
		}

		user := s.mint("u-"+short(), id)
		created := s.sendKeyed(user, http.MethodPost, "/organizations", "k-"+short(), `{"name":"Acme `+short()+`"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create of an organization answered %d %s, want 201", created.status, created.body)
		}
		org, name := created.member(t, "id"), created.member(t, "name")
		// The create of a public operation writes no owner tuple; the owner relation is written as an application
		// does it, and the read is decided on view.
		s.relate(user, "organization", org, "owner")
		resourceID, err := authz.ResourceID(user.tenant, org)
		must(t, err)
		s.awaitAllowed(user, "organization", resourceID, "view")
		decisions := reads.count(checkPermissionMethod)
		read := s.send(user, http.MethodGet, "/organizations/"+org, "")
		// The read is decided in SpiceDB through the proxy: the proxy carries the service's calls, so the count of
		// ReflectSchema at the end is a count of real traffic.
		if n := reads.count(checkPermissionMethod); n <= decisions {
			t.Errorf("the read of the organization made no CheckPermission call through the proxy")
		}
		if read.status != http.StatusOK || read.member(t, "id") != org || read.member(t, "name") != name {
			t.Fatalf("the read of the organization answered %d %s, want 200 with the id %s and the name %s", read.status, read.body, org, name)
		}
		r.healthy(t, "after the create and the read")

		// With no definition tenant, Retire stages no worker tuple and still retires (ST-SUP-RC35-OC13).
		if got := s.sendKeyed(operator, http.MethodPost, "/stego/tenants/"+url.PathEscape(id)+"/retire", "k-"+short(), ""); got.status != http.StatusOK {
			t.Fatalf("the retire of %s answered %d %s, want 200", id, got.status, got.body)
		}
		var retired sql.NullTime
		must(t, s.admin.QueryRow(`SELECT retired_at FROM stego_tenants WHERE tenant_id = $1`, id).Scan(&retired))
		if !retired.Valid {
			t.Errorf("the tenant list holds %s with no retire time after the retire", id)
		}
		// The tenant check refuses a request of a retired tenant before any handler (ST-SUP-RC27-OC11).
		if got := s.send(user, http.MethodGet, "/organizations/"+org, ""); got.status != http.StatusForbidden || got.member(t, "code") != "tenant.not_provisioned" {
			t.Errorf("a read in the retired tenant %s answered %d %s, want 403 with the code tenant.not_provisioned", id, got.status, got.body)
		}
		r.healthy(t, "after the retire")

		// A start after the retire, as a rollout does: the new controller takes the lease over a tenant list with a
		// retired tenant, and the service is ready again.
		r.stop()
		r.start(t)
		r.controllerHolder(t, first)
		r.awaitReady(t)
		r.healthy(t, "after the start that followed the retire")
		if n := reads.count(reflectSchemaMethod); n != 0 {
			t.Errorf("the service read the relations of the schema %d times, want none for a schema with no definition tenant", n)
		}
	})
}
