// Package examplecheck runs the generated service of examples/user-management as an operator runs it: the role
// migration, the migration job and then the service binary in all modes, against a local PostgreSQL server
// (STEGO_TEST_POSTGRES_DSN), a local SpiceDB container (STEGO_TEST_SPICEDB) and the local Kafka broker of kafkatest,
// which the event-publisher mixin of the example needs. It provisions one tenant, creates and reads one
// organization in it, retires the tenant and starts the service again; the readiness of every mode stays healthy.
// The schema of the example has no definition tenant: the service then has no worker-tuples job, and Retire reads no
// worker relation of the schema (ST-SUP-RC35-OC13).
//
// TestExampleWiring of internal/compiler copies the example, writes this file into its module and runs it. Each wait
// of the check has no deadline of its own: only the timeout of go test bounds it.
package examplecheck

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
	"github.com/google/uuid"
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
	issuer   = "https://issuer.example.test/realms/r1"
	audience = "user-management"
	password = "example-check"
	// basePath is the declared base path of the example.
	basePath = "/api/user-mgmt/v1"
	// tokenHeader is the header of the bearer token that the example declares for its auth provider.
	tokenHeader = "X-Internal-Token"
	// changeTopic is the topic of the change events of the example.
	changeTopic = "user-management.changes"
)

// TestMain removes the Kafka container of kafkatest when the tests end.
func TestMain(m *testing.M) { os.Exit(kafkatest.Wrap(m.Run)) }

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func short() string { return uuid.NewString()[:8] }

// writeFile writes data to a new file of the test and gives its path.
func writeFile(t *testing.T, name string, data []byte) string {
	file := filepath.Join(t.TempDir(), name)
	must(t, os.WriteFile(file, data, 0o600))
	return file
}

// lockedBuffer is the log of the service process, which the process writes and the check reads.
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

// example is the running example service.
type example struct {
	t     *testing.T
	ctx   context.Context
	admin *sql.DB
	spice *grpc.ClientConn
	key   *authntest.Key
	base  string
	log   *lockedBuffer
	// reads counts the reads of the schema that the service makes through its proxy to SpiceDB.
	reads *schemaReads
	// env is the environment of the service process, and binary its executable.
	env    []string
	binary string
	// stop stops the service process and waits until it exited; exited is closed when it exited.
	stop   func()
	exited chan struct{}
}

// caller is one caller of a tenant, with a token.
type caller struct {
	tenant  tenant.Tenant
	subject string
	token   string
}

// reply is one answer of the service.
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

// setUp makes a database of its own on the server, runs the role migration of the example and the migration job,
// and starts the service.
func setUp(t *testing.T) *example {
	dsn, endpoint, binary := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_TEST_SPICEDB"), os.Getenv("STEGO_EXAMPLE_BINARY")
	if dsn == "" || endpoint == "" || binary == "" {
		t.Fatal("the check needs STEGO_TEST_POSTGRES_DSN, STEGO_TEST_SPICEDB and STEGO_EXAMPLE_BINARY: a missing result is not a pass")
	}
	server, err := url.Parse(dsn)
	must(t, err)
	root, err := sql.Open("pgx", dsn)
	must(t, err)
	t.Cleanup(func() { root.Close() })
	e := &example{t: t, ctx: context.Background(), log: &lockedBuffer{}, binary: binary}
	// The roles belong to the server: they are dropped after the database, whose cleanup runs first.
	t.Cleanup(func() {
		for _, role := range []string{stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole,
			stegoruntime.OwnerRole, stegoruntime.PurgeRole, stegoruntime.OutboxRelayRole} {
			if _, err := root.Exec("DROP ROLE IF EXISTS " + quote(role)); err != nil {
				t.Errorf("drop the role %s: %v", role, err)
			}
		}
	})
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "example_" + hex.EncodeToString(suffix)
	_, err = root.Exec("CREATE DATABASE " + name)
	must(t, err)
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)") })
	u := *server
	u.Path = "/" + name
	e.admin, err = sql.Open("pgx", u.String())
	must(t, err)
	t.Cleanup(func() { e.admin.Close() })
	// What the database administrator runs before the first migration job: the role migration of the tenancy
	// component, and a login of each role, which a deployment gives.
	_, err = e.admin.Exec(tenancy.Roles)
	must(t, err)
	for _, role := range []string{stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole, stegoruntime.OwnerRole} {
		_, err = e.admin.Exec("ALTER ROLE " + quote(role) + " LOGIN PASSWORD '" + password + "'")
		must(t, err)
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
	e.spice, err = grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(bearer(spiceKey)))
	must(t, err)
	t.Cleanup(func() { e.spice.Close() })
	keyFile := writeFile(t, "spicedb.key", []byte(spiceKey))
	// The PostgreSQL server of the check is on the loopback address without TLS: the loopback test mode of the
	// adapter lets the service and the job connect to it. The migration job reaches SpiceDB directly, and the
	// service through the proxy that counts its reads of the schema.
	common := []string{"STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK=1", "STEGO_SPICEDB_KEY_FILE=" + keyFile, "STEGO_SPICEDB_INSECURE_LOOPBACK=1"}
	e.reads = startSchemaReads(t, endpoint)

	migrate := exec.Command(binary, "migrate")
	migrate.Env = append(append(os.Environ(), common...), "STEGO_SPICEDB_ENDPOINT="+endpoint,
		"STEGO_MIGRATION_DATABASE_URL="+as(stegoruntime.OwnerRole))
	if output, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("the migration job of the example failed: %v\n%s", err, output)
	}

	e.key = authntest.NewKey(t, "k1")
	cursor := make([]byte, 32)
	_, _ = rand.Read(cursor)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	must(t, listener.Close())
	e.base = "http://127.0.0.1:" + port
	broker := kafkatest.Start(t)
	broker.CreateTopic(t, changeTopic, 1)
	e.env = append(append([]string{}, common...), "STEGO_SPICEDB_ENDPOINT="+e.reads.addr,
		"PORT="+port, "DATABASE_URL="+as(stegoruntime.RuntimeRole),
		"STEGO_WORKER_DATABASE_URL="+as(stegoruntime.WorkerRole), "STEGO_RELAY_DATABASE_URL="+as(stegoruntime.RelayRole),
		"STEGO_AUTHN_ISSUER="+issuer, "STEGO_AUTHN_AUDIENCE="+audience,
		"STEGO_AUTHN_KEYS_FILE="+writeFile(t, "keys.json", authntest.Document(e.key.JWK())),
		"STEGO_QUERY_CURSOR_KEY_FILE="+writeFile(t, "cursor.key", cursor), "STEGO_SERVICE_VERSION=1.0.0",
		kafkaoutbox.BrokersSetting+"="+broker.Plain.Addr(), kafkaoutbox.InsecureLoopbackSetting+"=1",
		kafkaoutbox.TopicSetting+"="+changeTopic)
	t.Cleanup(func() {
		if e.stop != nil {
			e.stop()
		}
		if t.Failed() {
			t.Logf("service log:\n%s", e.log)
		}
	})
	e.start(t)
	return e
}

// start starts the service process in all modes and waits until it answers /livez.
func (e *example) start(t *testing.T) {
	t.Helper()
	process := exec.Command(e.binary)
	process.Env = append(os.Environ(), e.env...)
	process.Stdout, process.Stderr = e.log, e.log
	must(t, process.Start())
	exited := make(chan struct{})
	go func() { _ = process.Wait(); close(exited) }()
	e.exited = exited
	e.stop = func() {
		_ = process.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(30 * time.Second):
			_ = process.Process.Kill()
			<-exited
		}
	}
	for !e.probe(t, "/livez") {
	}
}

// probe gets the probe path once and tells whether it answered 200. It fails the test when the process exited, and
// waits a little before it gives false, so a caller may call it again at once.
func (e *example) probe(t *testing.T, path string) bool {
	t.Helper()
	select {
	case <-e.exited:
		t.Fatalf("the service stopped before %s answered 200:\n%s", path, e.log)
	default:
	}
	response, err := http.Get(e.base + path)
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
func (e *example) awaitReady(t *testing.T) {
	t.Helper()
	for !e.probe(t, "/readyz") || !e.probe(t, "/readyz/modes") {
	}
}

// healthy fails the test unless /readyz and /readyz/modes answer 200 now, and the readiness of the modes names no
// job: a failed or degraded worker-tuples job would be named there (ST-SUP-RC35-OC13).
func (e *example) healthy(t *testing.T, when string) {
	t.Helper()
	for _, path := range []string{"/readyz", "/readyz/modes"} {
		response, err := http.Get(e.base + path)
		must(t, err)
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		must(t, err)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s, %s answered %d %s, want 200", when, path, response.StatusCode, body)
		}
		if strings.Contains(string(body), "worker-tuples") || strings.Contains(string(body), "degraded") {
			t.Errorf("%s, %s answered 200 and names a job that is not healthy: %s", when, path, body)
		}
	}
	if strings.Contains(e.log.String(), "worker_tuples") {
		t.Errorf("%s, the service log has a line of the worker-tuples job:\n%s", when, e.log)
	}
}

// schemaReads is a gRPC proxy in front of the local SpiceDB server, which the service reaches SpiceDB through. It
// forwards each call as it is, and counts the calls of ReflectSchema: only the worker-tuples job and Retire read the
// worker relations of the schema that way (authz.SpiceDB.RelationsOf), and a service whose schema has no definition
// tenant does neither.
type schemaReads struct {
	addr string
	mu   sync.Mutex
	n    int
}

// rawCodec passes the bytes of each message through the proxy as they are.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) { return *(v.(*[]byte)), nil }
func (rawCodec) Unmarshal(data []byte, v any) error {
	*(v.(*[]byte)) = append([]byte(nil), data...)
	return nil
}
func (rawCodec) Name() string { return "proto" }

const reflectSchemaMethod = "/authzed.api.v1.SchemaService/ReflectSchema"

// startSchemaReads starts the proxy to the SpiceDB server at target, and stops it when the test ends.
func startSchemaReads(t *testing.T, target string) *schemaReads {
	backend, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	must(t, err)
	t.Cleanup(func() { backend.Close() })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	r := &schemaReads{addr: listener.Addr().String()}
	server := grpc.NewServer(grpc.ForceServerCodec(rawCodec{}), grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(stream)
		if method == reflectSchemaMethod {
			r.mu.Lock()
			r.n++
			r.mu.Unlock()
		}
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

func (r *schemaReads) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// controllerHolder waits until a controller other than not holds the controller lease, and gives it. A controller
// runs the first run of each periodic job when it takes the lease.
func (e *example) controllerHolder(t *testing.T, not string) string {
	t.Helper()
	for {
		var holder sql.NullString
		err := e.admin.QueryRow(`SELECT holder FROM public.leases WHERE name = $1 AND expires_at > pg_catalog.now()`,
			"stego.controller").Scan(&holder)
		if err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		if holder.Valid && holder.String != "" && holder.String != not {
			return holder.String
		}
		select {
		case <-e.exited:
			t.Fatalf("the service stopped before a controller held the lease:\n%s", e.log)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// bearer sends the preshared key of the local SpiceDB server on each call.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return false }

// as mints a token for subject in tenantID.
func (e *example) as(subject, tenantID string) caller {
	t := e.t
	claims := authntest.Claims(issuer, audience, subject, time.Now())
	claims["tenant"] = tenantID
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	token := authntest.Mint(t, authntest.Header("k1"), claims, e.key)
	keys, err := authn.ParseKeys(authntest.Document(e.key.JWK()))
	must(t, err)
	verifier, err := authn.New(authn.Settings{Issuer: issuer, Audience: audience, TenantClaim: "tenant", ProblemCode: "CHECK", Keys: keys})
	must(t, err)
	defer verifier.Stop()
	id, err := verifier.Verify(e.ctx, token)
	must(t, err)
	c := caller{token: token}
	c.tenant, err = tenant.New(tenant.Claim(id))
	must(t, err)
	principal, err := authz.Person("user", id)
	must(t, err)
	c.subject = principal.Subject().ID
	return c
}

// touch writes the relation of the user c on the resource of type resourceType and ID resourceID in SpiceDB.
func (e *example) touch(c caller, resourceType, resourceID, relation string) {
	_, err := v1.NewPermissionsServiceClient(e.spice).WriteRelationships(e.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID}, Relation: relation,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject}}}}}})
	must(e.t, err)
}

// awaitAllowed waits until SpiceDB answers, at its quantized revision, that c has the permission on the resource, as
// the service asks it.
func (e *example) awaitAllowed(t *testing.T, c caller, resourceType, resourceID, permission string) {
	t.Helper()
	for {
		answer, err := v1.NewPermissionsServiceClient(e.spice).CheckPermission(e.ctx, &v1.CheckPermissionRequest{
			Consistency: &v1.Consistency{Requirement: &v1.Consistency_MinimizeLatency{MinimizeLatency: true}},
			Resource:    &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID}, Permission: permission,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject}}})
		if err == nil && answer.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// send sends one request as the caller, with an Idempotency-Key when key is not empty.
func (e *example) send(c *caller, method, path, key, body string) reply {
	t := e.t
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, e.base+basePath+path, reader)
	must(t, err)
	if c != nil {
		request.Header.Set(tokenHeader, "Bearer "+c.token)
	}
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

func TestExample(t *testing.T) {
	t.Run("ST-SUP-RC35-OC13-T13 The generated user-management example provisions a tenant, creates and reads an organization, retires the tenant, and stays ready", func(t *testing.T) {
		e := setUp(t)
		e.awaitReady(t)
		first := e.controllerHolder(t, "")
		e.healthy(t, "after the start")

		// The operator holds provision and retire on the service-level resource service:user-management, which is
		// of no tenant; the operator's own tenant is never provisioned.
		operator := e.as("op-"+short(), "ops-"+short())
		e.touch(operator, "service", stegoruntime.Service, "operator")
		e.awaitAllowed(t, operator, "service", stegoruntime.Service, "provision")
		id := "acme-" + short()
		if got := e.send(&operator, http.MethodPost, "/stego/tenants", "k-"+uuid.NewString(), `{"tenant_id":"`+id+`"}`); got.status != http.StatusCreated || got.member(t, "tenant_id") != id {
			t.Fatalf("the provision of %s answered %d %s, want 201", id, got.status, got.body)
		}

		user := e.as("u-"+short(), id)
		created := e.send(&user, http.MethodPost, "/organizations", "k-"+uuid.NewString(), `{"name":"Acme `+short()+`"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create of an organization answered %d %s, want 201", created.status, created.body)
		}
		org, name := created.member(t, "id"), created.member(t, "name")
		resourceID, err := authz.ResourceID(user.tenant, org)
		must(t, err)
		// The create of a public operation writes no owner tuple; the owner relation is written as an
		// application does it, and the read is decided on view.
		e.touch(user, "organization", resourceID, "owner")
		e.awaitAllowed(t, user, "organization", resourceID, "view")
		read := e.send(&user, http.MethodGet, "/organizations/"+org, "", "")
		if read.status != http.StatusOK || read.member(t, "id") != org || read.member(t, "name") != name {
			t.Fatalf("the read of the organization answered %d %s, want 200 with the id %s and the name %s", read.status, read.body, org, name)
		}
		e.healthy(t, "after the create and the read")

		// The schema has no definition tenant, so Retire reads no worker relation, stages no worker tuple and still
		// retires (ST-SUP-RC35-OC13).
		if got := e.send(&operator, http.MethodPost, "/stego/tenants/"+url.PathEscape(id)+"/retire", "k-"+uuid.NewString(), ""); got.status != http.StatusOK {
			t.Fatalf("the retire of %s answered %d %s, want 200", id, got.status, got.body)
		}
		var retired sql.NullTime
		must(t, e.admin.QueryRow(`SELECT retired_at FROM stego_tenants WHERE tenant_id = $1`, id).Scan(&retired))
		if !retired.Valid {
			t.Errorf("the tenant list holds %s with no retire time after the retire", id)
		}
		if got := e.send(&user, http.MethodGet, "/organizations/"+org, "", ""); got.status == http.StatusOK {
			t.Errorf("a read in the retired tenant %s answered 200 %s", id, got.body)
		}
		e.healthy(t, "after the retire")

		// A start after the retire, as a rollout does: the new controller takes the lease, over a tenant list with a
		// retired tenant, and the service is ready again.
		e.stop()
		e.start(t)
		e.controllerHolder(t, first)
		e.awaitReady(t)
		e.healthy(t, "after the start that followed the retire")
		// With no definition tenant, the service has no worker-tuples job and Retire reads no worker relation: the
		// service never read the relations of the schema.
		if n := e.reads.count(); n != 0 {
			t.Errorf("the service read the relations of the schema %d times, want none for a schema with no definition tenant", n)
		}
	})
}
