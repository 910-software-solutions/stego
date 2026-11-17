// Package check runs the generated service of the gRPC runtime test as an operator runs it: the migration job and
// then the service binary, built from the generated main.go, against a local PostgreSQL server
// (STEGO_TEST_POSTGRES_DSN) with verified TLS and a local SpiceDB container (STEGO_TEST_SPICEDB). It calls one unary
// command, one unary query and one stream query over gRPC with verified TLS 1.3, and the same command and query over
// HTTP, and compares their answers and their audit rows.
package check

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/tenant"

	"example.com/shop/out/commands"
	shop "example.com/shop/out/grpcapi/pb/shop/v1"
	"example.com/shop/out/internal/stegoruntime"
	"example.com/shop/out/internal/tenancy"
)

const (
	issuer   = "https://issuer.grpc.test/realms/r1"
	audience = "grpc-shop"
	password = "grpc-check"
)

// service is the running service: its database, its SpiceDB, its HTTP base and its gRPC client.
type service struct {
	t     *testing.T
	ctx   context.Context
	admin *sql.DB
	spice *grpc.ClientConn
	key   *authntest.Key
	base  string
	shop  shop.ShopClient
	// conn is the connection of shop, on which a check calls a method that no proto file declares.
	conn *grpc.ClientConn
	log  *bytes.Buffer
}

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

// authority is a CA and a certificate for localhost that it signed, as PEM.
type authority struct{ ca, cert, key []byte }

// newAuthority makes a CA and a server certificate for localhost and 127.0.0.1.
func newAuthority(t *testing.T, name string) authority {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name + " CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	must(t, err)
	ca, err := x509.ParseCertificate(caDER)
	must(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	serverDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(2),
		Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, ca, &key.PublicKey, caKey)
	must(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	must(t, err)
	return authority{
		ca:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		key:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}
}

// serverTLS turns TLS on in the PostgreSQL server with a certificate of a new CA, written into its data folder with
// COPY TO PROGRAM, and gives the PEM file of the CA.
func serverTLS(t *testing.T, root *sql.DB) string {
	a := newAuthority(t, "grpc test database")
	var dataDir string
	must(t, root.QueryRow("SHOW data_directory").Scan(&dataDir))
	write := func(name string, text []byte) {
		lines := strings.Split(strings.TrimSpace(string(text)), "\n")
		quoted := make([]string, len(lines))
		for i, l := range lines {
			quoted[i] = "('" + l + "')"
		}
		execAll(t, root, "COPY (SELECT line FROM (VALUES "+strings.Join(quoted, ", ")+") AS v(line)) TO PROGRAM 'rm -f "+
			dataDir+"/"+name+" && umask 077 && cat > "+dataDir+"/"+name+"'")
	}
	write("stego_grpc_server.crt", a.cert)
	write("stego_grpc_server.key", a.key)
	execAll(t, root, "ALTER SYSTEM SET ssl_cert_file = 'stego_grpc_server.crt'", "ALTER SYSTEM SET ssl_key_file = 'stego_grpc_server.key'",
		"ALTER SYSTEM SET ssl = on", "SELECT pg_reload_conf()")
	t.Cleanup(func() {
		for _, statement := range []string{"ALTER SYSTEM RESET ssl", "ALTER SYSTEM RESET ssl_cert_file", "ALTER SYSTEM RESET ssl_key_file", "SELECT pg_reload_conf()"} {
			_, _ = root.Exec(statement)
		}
	})
	time.Sleep(300 * time.Millisecond)
	return writeFile(t, "ca.pem", a.ca)
}

// withTLS gives the URL of role on u, on the host localhost, with verified TLS.
func withTLS(u *url.URL, role, caFile string) string {
	v := *u
	v.User = url.UserPassword(role, password)
	if _, port, err := net.SplitHostPort(v.Host); err == nil {
		v.Host = net.JoinHostPort("localhost", port)
	}
	q := v.Query()
	q.Set("sslmode", "verify-full")
	q.Set("sslrootcert", caFile)
	v.RawQuery = q.Encode()
	return v.String()
}

func freePort(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer listener.Close()
	return fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
}

// bearer sends a bearer token on each call.
type bearer struct {
	token  string
	secure bool
}

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}
func (b bearer) RequireTransportSecurity() bool { return b.secure }

// start prepares the database, runs the migration job and starts the service with its HTTP and gRPC servers, and
// waits until both answer.
func start(t *testing.T) *service {
	dsn, endpoint, binary := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_TEST_SPICEDB"), os.Getenv("STEGO_GRPC_BINARY")
	if dsn == "" || endpoint == "" || binary == "" {
		t.Fatal("the check needs STEGO_TEST_POSTGRES_DSN, STEGO_TEST_SPICEDB and STEGO_GRPC_BINARY: a missing result is not a pass")
	}
	server, err := url.Parse(dsn)
	must(t, err)
	root, err := sql.Open("pgx", dsn)
	must(t, err)
	t.Cleanup(func() { root.Close() })
	s := &service{t: t, ctx: context.Background(), log: &bytes.Buffer{}}
	roles := []string{stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole, stegoruntime.OwnerRole}
	t.Cleanup(func() {
		for _, role := range roles {
			if _, err := root.Exec("DROP ROLE IF EXISTS " + quote(role)); err != nil {
				t.Errorf("drop the role %s: %v", role, err)
			}
		}
	})
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "grpc_" + hex.EncodeToString(suffix)
	execAll(t, root, "CREATE DATABASE "+name)
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)") })
	u := *server
	u.Path = "/" + name
	s.admin, err = sql.Open("pgx", u.String())
	must(t, err)
	t.Cleanup(func() { s.admin.Close() })
	// What the database administrator runs before the first migration job: the role migration and a login of each
	// role (ST-SUP-RC27-OC04).
	execAll(t, s.admin, tenancy.Roles)
	for _, role := range roles {
		execAll(t, s.admin, "ALTER ROLE "+quote(role)+" LOGIN PASSWORD '"+password+"'")
	}
	ca := serverTLS(t, root)

	spiceKey := os.Getenv("STEGO_TEST_SPICEDB_KEY")
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(bearer{token: spiceKey}))
	must(t, err)
	t.Cleanup(func() { conn.Close() })
	s.spice = conn
	spice := []string{"STEGO_SPICEDB_ENDPOINT=" + endpoint, "STEGO_SPICEDB_KEY_FILE=" + writeFile(t, "spicedb.key", []byte(spiceKey)),
		"STEGO_SPICEDB_INSECURE_LOOPBACK=1"}
	migrate := exec.Command(binary, "migrate")
	migrate.Env = append(append(os.Environ(), spice...), "STEGO_MIGRATION_DATABASE_URL="+withTLS(&u, stegoruntime.OwnerRole, ca),
		"STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK=0")
	if output, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("the migration job failed: %v\n%s", err, output)
	}

	s.key = authntest.NewKey(t, "k1")
	cursor := make([]byte, 32)
	_, _ = rand.Read(cursor)
	grpcTLS := newAuthority(t, "grpc test service")
	httpPort, grpcPort := freePort(t), freePort(t)
	s.base = "http://127.0.0.1:" + httpPort
	process := exec.Command(binary, "--mode", "api")
	process.Env = append(append(os.Environ(), spice...),
		"PORT="+httpPort, "DATABASE_URL="+withTLS(&u, stegoruntime.RuntimeRole, ca),
		"STEGO_AUTHN_KEYS_FILE="+writeFile(t, "keys.json", authntest.Document(s.key.JWK())),
		"STEGO_QUERY_CURSOR_KEY_FILE="+writeFile(t, "cursor.key", cursor), "STEGO_SERVICE_VERSION=1.0.0",
		"STEGO_GRPC_ADDR=127.0.0.1:"+grpcPort, "STEGO_GRPC_TLS_CERT="+writeFile(t, "grpc.crt", grpcTLS.cert),
		"STEGO_GRPC_TLS_KEY="+writeFile(t, "grpc.key", grpcTLS.key), "STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK=0")
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
				break
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
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(grpcTLS.ca)
	client, err := grpc.NewClient("127.0.0.1:"+grpcPort, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13})))
	must(t, err)
	t.Cleanup(func() { client.Close() })
	s.shop = shop.NewShopClient(client)
	s.conn = client
	return s
}

// caller is one caller of a tenant, with a token.
type caller struct {
	tenant  tenant.Tenant
	subject string
	token   string
}

// as mints a token for subject in tenantID.
func (s *service) as(subject, tenantID string) caller {
	t := s.t
	claims := authntest.Claims(issuer, audience, subject, time.Now())
	claims["tenant"] = tenantID
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

// order stores an order of the caller's tenant, with the caller as its owner when owned is true.
func (s *service) order(c caller, sku string, owned bool) string {
	id := uuid.NewString()
	execAll(s.t, s.admin, fmt.Sprintf("INSERT INTO orders (tenant_id, id, sku, qty, email) VALUES ('%s', '%s', '%s', 2, 'buyer@example.com')", c.tenant.ID(), id, sku))
	if owned {
		resourceID, err := authz.ResourceID(c.tenant, id)
		must(s.t, err)
		_, err = v1.NewPermissionsServiceClient(s.spice).WriteRelationships(s.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: "order", ObjectId: resourceID}, Relation: "owner",
				Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject}}}}}})
		must(s.t, err)
	}
	return id
}

// rpc gives the context of one gRPC call as the caller, with an idempotency key and a request ID when they are given.
func (s *service) rpc(c *caller, key, requestID string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	var pairs []string
	if c != nil {
		pairs = append(pairs, "authorization", "Bearer "+c.token)
	}
	if key != "" {
		pairs = append(pairs, "idempotency-key", `"`+key+`"`)
	}
	if requestID != "" {
		pairs = append(pairs, "x-request-id", requestID)
	}
	return metadata.NewOutgoingContext(ctx, metadata.Pairs(pairs...)), cancel
}

// reply is one answer of the HTTP server.
type reply struct {
	status int
	body   []byte
}

// send sends one HTTP request as the caller, with an Idempotency-Key and a request ID when they are given.
func (s *service) send(c *caller, method, path, key, requestID, body string) reply {
	t := s.t
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, s.base+"/api/shop/v1"+path, reader)
	must(t, err)
	if c != nil {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", `"`+key+`"`)
	}
	if requestID != "" {
		request.Header.Set("X-Request-Id", requestID)
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

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// auditRows gives the audit rows whose column equals value, each as the fields that do not depend on the transport
// or the time, in order: the action, the operation, the resource type, the outcome, the error code and whether a
// state is stored.
func (s *service) auditRows(column, value string) []string {
	t := s.t
	t.Helper()
	rows, err := s.admin.Query(`SELECT action, operation, resource_type, outcome, error_code, after IS NOT NULL FROM audit_event WHERE `+column+` = $1`, value)
	must(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var action, operation, resourceType, outcome, code string
		var state bool
		must(t, rows.Scan(&action, &operation, &resourceType, &outcome, &code, &state))
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%s|%t", action, operation, resourceType, outcome, code, state))
	}
	must(t, rows.Err())
	sort.Strings(out)
	return out
}

// readMark gives the IDs of the read audit rows so far, so newReads gives the rows of the calls after it. The server
// makes the request ID of a query call, so a read row cannot be found by a request ID of the client
// (ST-SUP-RP11-LC19).
func (s *service) readMark() map[string]bool {
	t := s.t
	t.Helper()
	rows, err := s.admin.Query(`SELECT id::text FROM audit_event WHERE action = 'read'`)
	must(t, err)
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		must(t, rows.Scan(&id))
		seen[id] = true
	}
	must(t, rows.Err())
	return seen
}

// newReads gives the read audit rows that are not in mark, in the form of auditRows, and checks that none holds the
// request ID of the client.
func (s *service) newReads(mark map[string]bool, clientIDs ...string) []string {
	t := s.t
	t.Helper()
	rows, err := s.admin.Query(`SELECT id::text, request_id, action, operation, resource_type, outcome, error_code, after IS NOT NULL FROM audit_event WHERE action = 'read'`)
	must(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, requestID, action, operation, resourceType, outcome, code string
		var state bool
		must(t, rows.Scan(&id, &requestID, &action, &operation, &resourceType, &outcome, &code, &state))
		if mark[id] {
			continue
		}
		for _, client := range clientIDs {
			if requestID == client {
				t.Fatalf("the read audit row %s holds the request ID %s of the client", id, client)
			}
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%s|%t", action, operation, resourceType, outcome, code, state))
	}
	must(t, rows.Err())
	sort.Strings(out)
	return out
}

// wantCode checks the gRPC status code of err.
func wantCode(t *testing.T, err error, code codes.Code, what string) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("%s gave %v, want %s", what, err, code)
	}
}

// provisioned provisions the tenant of c through the provisioning route, as an operator of a tenant that is never
// provisioned, who holds provision on the service-level resource service:grpc-shop, whose ID is not tenant-qualified
// (ST-SUP-RC27-OC09, ST-SUP-RP06-LC22).
func (s *service) provisioned(c caller) {
	t := s.t
	t.Helper()
	operator := s.as("op-"+uuid.NewString()[:8], "ops-"+uuid.NewString()[:8])
	const resourceID = "grpc-shop"
	_, err := v1.NewPermissionsServiceClient(s.spice).WriteRelationships(s.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: "service", ObjectId: resourceID}, Relation: "operator",
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: operator.subject}}}}}})
	must(t, err)
	body, err := json.Marshal(map[string]string{"tenant_id": c.tenant.ID()})
	must(t, err)
	if got := s.send(&operator, http.MethodPost, "/stego/tenants", "k-"+uuid.NewString(), "", string(body)); got.status != http.StatusCreated {
		t.Fatalf("the provision of the tenant %s answered %d %s, want 201", c.tenant.ID(), got.status, got.body)
	}
}

// relate writes the tuple resourceType:resourceID#relation@user:<subject of c> to SpiceDB.
func (s *service) relate(resourceType, resourceID, relation string, c caller) {
	_, err := v1.NewPermissionsServiceClient(s.spice).WriteRelationships(s.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID}, Relation: relation,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject}}}}}})
	must(s.t, err)
}

// granted waits until SpiceDB answers, at its quantized revision, that c has permission on resourceType:resourceID,
// as the service asks it at the revocation floor.
func (s *service) granted(resourceType, resourceID, permission string, c caller) {
	t := s.t
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		answer, err := v1.NewPermissionsServiceClient(s.spice).CheckPermission(s.ctx, &v1.CheckPermissionRequest{
			Consistency: &v1.Consistency{Requirement: &v1.Consistency_MinimizeLatency{MinimizeLatency: true}},
			Resource:    &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID}, Permission: permission,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject}}})
		if err == nil && answer.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION {
			return
		}
	}
	t.Fatalf("after 60 s, SpiceDB does not answer that the caller has %s on %s:%s", permission, resourceType, resourceID)
}

func TestGRPC(t *testing.T) {
	s := start(t)
	owner := s.as("u-"+uuid.NewString()[:8], "t-"+uuid.NewString()[:8])
	other := s.as("u-"+uuid.NewString()[:8], owner.tenant.ID())
	s.provisioned(owner)
	mine := s.order(owner, "a1", true)
	theirs := s.order(owner, "b2", false)
	// placer may place orders in the tenant of owner: its grant is on the tenant-qualified ID of the service-level
	// resource (ST-SUP-RC32-OC15). platform holds the same relation on the ID with no tenant, which is the ID of
	// Provision and Retire, and so may place no order in a tenant.
	placer := s.as("u-"+uuid.NewString()[:8], owner.tenant.ID())
	platform := s.as("u-"+uuid.NewString()[:8], owner.tenant.ID())
	qualified, err := authz.ResourceID(owner.tenant, "grpc-shop")
	must(t, err)
	s.relate("service", qualified, "placer", placer)
	s.relate("service", "grpc-shop", "placer", platform)
	// stranger, a caller of another tenant, holds placer on the service-level resource of the tenant of owner, which
	// grants nothing in its own tenant. placer may view theirs and not edit it.
	stranger := s.as("u-"+uuid.NewString()[:8], "t-"+uuid.NewString()[:8])
	s.provisioned(stranger)
	s.relate("service", qualified, "placer", stranger)
	theirsID, err := authz.ResourceID(owner.tenant, theirs)
	must(t, err)
	s.relate("order", theirsID, "viewer", placer)

	t.Run("ST-SUP-RC27-OC11-T01 A gRPC call of a tenant that is not provisioned is refused before any handler", func(t *testing.T) {
		s.t = t
		unknown := s.as("u-"+uuid.NewString()[:8], "org-z-"+uuid.NewString()[:8])
		ctx, cancel := s.rpc(&unknown, "k-"+uuid.NewString(), "")
		_, err := s.shop.AddNote(ctx, &commands.AddNoteRequest{OrderId: mine, Text: "unprovisioned"})
		cancel()
		if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "tenant.not_provisioned" {
			t.Errorf("the call of a tenant that is not provisioned gave %v, want PERMISSION_DENIED with the code tenant.not_provisioned", err)
		}
		if n := count(t, s.admin, `SELECT (SELECT count(*) FROM notes WHERE tenant_id = $1) + (SELECT count(*) FROM audit_event WHERE tenant_id = $1)`, unknown.tenant.ID()); n != 0 {
			t.Errorf("the refused call left %d notes and audit rows", n)
		}
	})

	t.Run("ST-SUP-RC07-OC19-T01 A gRPC call runs its command or query fill with the same handling as HTTP", func(t *testing.T) {
		s.t = t
		// The command: two calls with one idempotency key run the fill once and replay its response.
		key := "k-" + uuid.NewString()
		ctx, cancel := s.rpc(&owner, key, "")
		first, err := s.shop.AddNote(ctx, &commands.AddNoteRequest{OrderId: mine, Text: "over grpc"})
		cancel()
		must(t, err)
		ctx, cancel = s.rpc(&owner, key, "")
		second, err := s.shop.AddNote(ctx, &commands.AddNoteRequest{OrderId: mine, Text: "over grpc"})
		cancel()
		must(t, err)
		if first.GetNoteId() == "" || second.GetNoteId() != first.GetNoteId() {
			t.Fatalf("the second call with the same key gave the note %q, want the replay of %q", second.GetNoteId(), first.GetNoteId())
		}
		if n := count(t, s.admin, `SELECT count(*) FROM notes WHERE text = 'over grpc' AND tenant_id = $1`, owner.tenant.ID()); n != 1 {
			t.Fatalf("the two calls with one key wrote %d notes, want 1", n)
		}
		// The same command over HTTP writes the same audit rows as over gRPC.
		httpKey := "k-" + uuid.NewString()
		if got := s.send(&owner, http.MethodPost, "/orders/"+mine+"/notes", httpKey, "", `{"text":"over http"}`); got.status != http.StatusCreated {
			t.Fatalf("the command over HTTP answered %d %s, want 201", got.status, got.body)
		}
		overGRPC, overHTTP := s.auditRows("idempotency_key", key), s.auditRows("idempotency_key", httpKey)
		if len(overGRPC) == 0 || strings.Join(overGRPC, ";") != strings.Join(overHTTP, ";") {
			t.Fatalf("the audit rows of the command over gRPC are %v and over HTTP %v, want the same rows", overGRPC, overHTTP)
		}
		if !strings.Contains(strings.Join(overGRPC, ";"), "add-note") {
			t.Fatalf("the audit rows of the command %v name no action add-note", overGRPC)
		}
		// A call with no key is refused, as the HTTP handler refuses it.
		ctx, cancel = s.rpc(&owner, "", "")
		_, err = s.shop.AddNote(ctx, &commands.AddNoteRequest{OrderId: mine, Text: "no key"})
		cancel()
		wantCode(t, err, codes.InvalidArgument, "the command with no idempotency key")
		// A caller who may not edit the order: NotFound over gRPC and 404 over HTTP, with the same audit rows.
		deniedKey, deniedHTTPKey := "k-"+uuid.NewString(), "k-"+uuid.NewString()
		ctx, cancel = s.rpc(&other, deniedKey, "")
		_, err = s.shop.AddNote(ctx, &commands.AddNoteRequest{OrderId: mine, Text: "denied"})
		cancel()
		wantCode(t, err, codes.NotFound, "the command of a caller who may not edit the order")
		if got := s.send(&other, http.MethodPost, "/orders/"+mine+"/notes", deniedHTTPKey, "", `{"text":"denied"}`); got.status != http.StatusNotFound {
			t.Fatalf("the denied command over HTTP answered %d %s, want 404", got.status, got.body)
		}
		deniedGRPC, deniedHTTP := s.auditRows("idempotency_key", deniedKey), s.auditRows("idempotency_key", deniedHTTPKey)
		if len(deniedGRPC) == 0 || strings.Join(deniedGRPC, ";") != strings.Join(deniedHTTP, ";") || !strings.Contains(deniedGRPC[0], "|denied|") {
			t.Fatalf("the audit rows of the denied command over gRPC are %v and over HTTP %v, want the same denied rows", deniedGRPC, deniedHTTP)
		}

		// The query: the allowed order with its read audit row, as over HTTP.
		readID := uuid.NewString()
		mark := s.readMark()
		ctx, cancel = s.rpc(&owner, "", readID)
		summary, err := s.shop.GetOrderSummary(ctx, &shop.GetOrderSummaryRequest{OrderId: mine})
		cancel()
		must(t, err)
		if summary.GetId() != mine || summary.GetSku() != "a1" || summary.GetQty() != 2 {
			t.Fatalf("the query gave %v, want the order %s with the sku a1 and the qty 2", summary, mine)
		}
		readGRPC := s.newReads(mark, readID)
		httpReadID := uuid.NewString()
		mark = s.readMark()
		if got := s.send(&owner, http.MethodGet, "/orders/"+mine+"/summary", "", httpReadID, ""); got.status != http.StatusOK || !bytes.Contains(got.body, []byte(`"a1"`)) {
			t.Fatalf("the query over HTTP answered %d %s, want 200 with the sku a1", got.status, got.body)
		}
		readHTTP := s.newReads(mark, httpReadID)
		if len(readGRPC) == 0 || strings.Join(readGRPC, ";") != strings.Join(readHTTP, ";") {
			t.Fatalf("the read audit rows over gRPC are %v and over HTTP %v, want the same rows", readGRPC, readHTTP)
		}
		// The order that the caller may not view: NotFound over gRPC and 404 over HTTP, with the same audit rows.
		deniedRead, deniedHTTPRead := uuid.NewString(), uuid.NewString()
		mark = s.readMark()
		ctx, cancel = s.rpc(&owner, "", deniedRead)
		_, err = s.shop.GetOrderSummary(ctx, &shop.GetOrderSummaryRequest{OrderId: theirs})
		cancel()
		wantCode(t, err, codes.NotFound, "the query of an order that the caller may not view")
		if status.Convert(err).Message() != "not_found" {
			t.Fatalf("the denied query has the message %q, want the fixed code not_found", status.Convert(err).Message())
		}
		deniedGRPCRows := s.newReads(mark, deniedRead)
		mark = s.readMark()
		if got := s.send(&owner, http.MethodGet, "/orders/"+theirs+"/summary", "", deniedHTTPRead, ""); got.status != http.StatusNotFound {
			t.Fatalf("the denied query over HTTP answered %d %s, want 404", got.status, got.body)
		}
		if a, b := deniedGRPCRows, s.newReads(mark, deniedHTTPRead); strings.Join(a, ";") != strings.Join(b, ";") {
			t.Fatalf("the audit rows of the denied query over gRPC are %v and over HTTP %v, want the same rows", a, b)
		}
		// No token is Unauthenticated. A method that no command or query names is refused by checking
		// (ST-DECL-INV75), so the proto file of the service holds none; a call of a method that the server does not
		// register, such as shop.v1.Shop/CancelOrder, is still Unimplemented and runs no fill.
		ctx, cancel = s.rpc(nil, "", "")
		_, err = s.shop.GetOrderSummary(ctx, &shop.GetOrderSummaryRequest{OrderId: mine})
		cancel()
		wantCode(t, err, codes.Unauthenticated, "the query with no token")
		ctx, cancel = s.rpc(&owner, "", "")
		err = s.conn.Invoke(ctx, "/shop.v1.Shop/CancelOrder", &shop.GetOrderSummaryRequest{OrderId: mine}, &shop.OrderSummary{})
		cancel()
		wantCode(t, err, codes.Unimplemented, "the method that the server does not register")
	})

	t.Run("ST-SUP-RC33-OC08-T01 A stream query gets a gRPC stream and no HTTP route", func(t *testing.T) {
		s.t = t
		ctx, cancel := s.rpc(&owner, "", "")
		defer cancel()
		stream, err := s.shop.WatchOrders(ctx, &shop.WatchOrdersRequest{})
		must(t, err)
		var got []string
		for {
			message, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			must(t, err)
			got = append(got, message.GetId()+"="+message.GetSku())
		}
		if strings.Join(got, ",") != mine+"=a1" {
			t.Fatalf("the stream sent %v, want only the order %s that the caller may view", got, mine)
		}
		// The stream query has no HTTP route; the unary query keeps its route.
		if r := s.send(&owner, http.MethodGet, "/orders/watch", "", "", ""); r.status != http.StatusNotFound {
			t.Fatalf("GET /orders/watch answered %d %s, want 404: the stream query has no HTTP route", r.status, r.body)
		}
		var document map[string]any
		if r := s.send(&owner, http.MethodGet, "/orders/"+mine+"/summary", "", "", ""); r.status != http.StatusOK || json.Unmarshal(r.body, &document) != nil {
			t.Fatalf("the route of get-order-summary answered %d %s, want 200", r.status, r.body)
		}
	})
	t.Run("ST-SUP-RC32-OC15-T03 A command on the service-level resource creates a row with no parent over gRPC and HTTP", func(t *testing.T) {
		s.t = t
		s.granted("service", qualified, "place_order", placer)
		s.granted("service", "grpc-shop", "place_order", platform)
		// A grant on the service-level resource with no tenant is not a grant in the tenant: NotFound, and no row.
		ctx, cancel := s.rpc(&platform, "k-"+uuid.NewString(), "")
		_, err := s.shop.PlaceOrder(ctx, &commands.PlaceOrderRequest{Sku: "p0", Qty: 1, Email: "p0@example.com"})
		cancel()
		wantCode(t, err, codes.NotFound, "the command of a caller with a grant on the service-level resource with no tenant")
		if got := s.send(&other, http.MethodPost, "/orders", "k-"+uuid.NewString(), "", `{"sku":"p0","qty":1,"email":"p0@example.com"}`); got.status != http.StatusNotFound {
			t.Fatalf("the command of a caller with no grant over HTTP answered %d %s, want 404", got.status, got.body)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM orders WHERE sku = 'p0'`); n != 0 {
			t.Fatalf("the denied commands wrote %d orders", n)
		}
		// The caller with the grant in the tenant creates an order over gRPC, and the replay gives the same order.
		key := "k-" + uuid.NewString()
		ctx, cancel = s.rpc(&placer, key, "")
		placed, err := s.shop.PlaceOrder(ctx, &commands.PlaceOrderRequest{Sku: "p1", Qty: 3, Email: "p1@example.com"})
		cancel()
		must(t, err)
		ctx, cancel = s.rpc(&placer, key, "")
		replayed, err := s.shop.PlaceOrder(ctx, &commands.PlaceOrderRequest{Sku: "p1", Qty: 3, Email: "p1@example.com"})
		cancel()
		must(t, err)
		if placed.GetOrderId() == "" || replayed.GetOrderId() != placed.GetOrderId() {
			t.Fatalf("the replay gave the order %q, want %q", replayed.GetOrderId(), placed.GetOrderId())
		}
		if n := count(t, s.admin, `SELECT count(*) FROM orders WHERE id = $1 AND tenant_id = $2 AND sku = 'p1' AND qty = 3`, placed.GetOrderId(), placer.tenant.ID()); n != 1 {
			t.Fatalf("the orders table holds %d rows of the placed order in the tenant of the caller, want 1", n)
		}
		// The same command over HTTP, at the route POST /orders.
		got := s.send(&placer, http.MethodPost, "/orders", "k-"+uuid.NewString(), "", `{"sku":"p2","qty":4,"email":"p2@example.com"}`)
		var answer map[string]any
		if got.status != http.StatusCreated || json.Unmarshal(got.body, &answer) != nil || answer["orderId"] == nil {
			t.Fatalf("the command over HTTP answered %d %s, want 201 with the order ID", got.status, got.body)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM orders WHERE sku = 'p2' AND tenant_id = $1`, placer.tenant.ID()); n != 1 {
			t.Fatalf("the command over HTTP wrote %d orders, want 1", n)
		}
		// The audit rows: the command on the service-level resource and the create of the order.
		if rows := strings.Join(s.auditRows("idempotency_key", key), ";"); !strings.Contains(rows, "place-order|place-order|order|ok") {
			t.Fatalf("the audit rows of the command %s hold no create of the order", rows)
		}
		// The runtime enqueues the owner tuple of the new row, the caller, for the relay (ST-SUP-RP10-LC05).
		resourceID, err := authz.ResourceID(placer.tenant, placed.GetOrderId())
		must(t, err)
		if n := count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE tenant_id = $1 AND destination = 'authz'
 AND convert_from(payload, 'UTF8')::jsonb #>> '{tuple,resource_type}' = 'order' AND convert_from(payload, 'UTF8')::jsonb #>> '{tuple,resource_id}' = $2
 AND convert_from(payload, 'UTF8')::jsonb #>> '{tuple,relation}' = 'owner' AND convert_from(payload, 'UTF8')::jsonb #>> '{tuple,subject_id}' = $3`,
			placer.tenant.ID(), resourceID, placer.subject); n != 1 {
			t.Fatalf("the outbox holds %d owner tuples of the placed order for the relay, want 1", n)
		}
		// The owner reads the order back through the query once the relay has written its owner tuple; the check
		// writes it as the relay does, since the service runs the api mode only.
		s.relate("order", resourceID, "owner", placer)
		s.granted("order", resourceID, "view", placer)
		ctx, cancel = s.rpc(&placer, "", "")
		summary, err := s.shop.GetOrderSummary(ctx, &shop.GetOrderSummaryRequest{OrderId: placed.GetOrderId()})
		cancel()
		must(t, err)
		if summary.GetSku() != "p1" || summary.GetQty() != 3 {
			t.Fatalf("the query gave %v, want the placed order with the sku p1 and the qty 3", summary)
		}
		// The grant is on the ID of one tenant: a caller of another tenant with it places no order.
		s.granted("service", qualified, "place_order", stranger)
		ctx, cancel = s.rpc(&stranger, "k-"+uuid.NewString(), "")
		_, err = s.shop.PlaceOrder(ctx, &commands.PlaceOrderRequest{Sku: "px", Qty: 1, Email: "px@example.com"})
		cancel()
		wantCode(t, err, codes.NotFound, "the command of a caller of another tenant with the grant of the tenant of owner")
		// The handle reads with Get, decided by the read permission, and a create decides its reference: a note on
		// an order that placer may not view, and one on an order that placer may view and not edit, are refused and
		// leave no row; a note on the order that placer owns is written.
		s.granted("order", theirsID, "view", placer)
		s.granted("order", resourceID, "edit", placer)
		for _, refused := range []struct{ sku, on, what string }{{"p3", mine, "an order that placer may not view"}, {"p4", theirs, "an order that placer may view and not edit"}} {
			ctx, cancel = s.rpc(&placer, "k-"+uuid.NewString(), "")
			_, err = s.shop.PlaceOrder(ctx, &commands.PlaceOrderRequest{Sku: refused.sku, Qty: 1, Email: "n@example.com", NoteOn: refused.on, Note: "refused"})
			cancel()
			wantCode(t, err, codes.NotFound, "the command with a note on "+refused.what)
		}
		ctx, cancel = s.rpc(&placer, "k-"+uuid.NewString(), "")
		_, err = s.shop.PlaceOrder(ctx, &commands.PlaceOrderRequest{Sku: "p5", Qty: 1, Email: "n@example.com", NoteOn: placed.GetOrderId(), Note: "on my order"})
		cancel()
		must(t, err)
		if n := count(t, s.admin, `SELECT (SELECT count(*) FROM orders WHERE sku IN ('px', 'p3', 'p4')) + (SELECT count(*) FROM notes WHERE text = 'refused')`); n != 0 {
			t.Fatalf("the refused commands left %d rows", n)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM notes WHERE order_id = $1 AND text = 'on my order' AND tenant_id = $2`, placed.GetOrderId(), placer.tenant.ID()); n != 1 {
			t.Fatalf("the notes table holds %d notes on the placed order, want 1", n)
		}
	})
}
