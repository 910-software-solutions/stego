// Package check runs the generated service of the runtime wiring test as an operator runs it: the migration job and
// then the service binary, built from the generated main.go, against a local PostgreSQL server
// (STEGO_TEST_POSTGRES_DSN) with verified TLS and a local SpiceDB container (STEGO_TEST_SPICEDB), and sends it one
// collection call, one command and one query over HTTP. The service has the event-publisher mixin: its worker sends
// the change events of the collections to a local Kafka broker of kafkatest (ST-SUP-RC20-OC40, ST-SUP-RC18-OC31).
package check

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/kafkatest"
	"github.com/jsell-rh/stego/registry/runtime/outbox/kafkaoutbox"
	"github.com/jsell-rh/stego/registry/runtime/tenant"

	"example.com/shop/out/internal/stegoruntime"
	"example.com/shop/out/internal/storage"
	"example.com/shop/out/internal/tenancy"
)

const (
	issuer   = "https://issuer.wiring.test/realms/r1"
	audience = "wiring-shop"
	password = "wiring-check"
)

// changeTopic is the declared topic of the change events of the service, so the events of one entity go to one
// partition of one topic in their order.
const changeTopic = "wiring-shop.changes"

// TestMain removes the Kafka container of kafkatest when the tests end.
func TestMain(m *testing.M) { os.Exit(kafkatest.Wrap(m.Run)) }

// migratorRole is the login of the migration job: the owner role, as the outbox migration of the runtime library
// needs (ST-SUP-RP01-LC19), and never the runtime role.
var migratorRole = stegoruntime.OwnerRole

// service is the running service: its database, its SpiceDB and its address, and the results of the runs of the
// binary before the checks.
type service struct {
	t      *testing.T
	ctx    context.Context
	admin  *sql.DB
	dbName string
	spice  *grpc.ClientConn
	key    *authntest.Key
	base   string
	log    *bytes.Buffer

	// checks records each decision that the service asks of SpiceDB, and operator is the operator who may provision
	// and retire tenants on the service-level resource of its own tenant (ST-SUP-RC27-OC09).
	checks   *spiceRecorder
	operator caller

	weakMigration, weakStart, foreignMigration, legacyMigration, secondMigration, nextRelease, noKafka runResult
	// The refused starts of the modes (ST-GEN-INV26): an unknown mode, the worker mode with no connection string of
	// the worker role, and the binary of the next release, which declares the modes api and worker, with the mode
	// controller and with no mode option and no database setting.
	unknownMode, noWorkerURL, allModes, declaredDefault runResult
	broker                                              *kafkatest.Broker
	foreignDB, legacyDB                                 *sql.DB
	schemaBeforeNext                                    string
	// restart stops the service process and starts it again.
	restart func(t *testing.T)
	// restartWith stops the service process and starts it again with the arguments given, such as --mode api.
	restartWith func(t *testing.T, args ...string)
}

// runResult is the outcome of one run of the binary.
type runResult struct {
	err    error
	output string
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// execAll runs each statement on db.
func execAll(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%.100s: %v", statement, err)
		}
	}
}

// quote quotes one SQL name.
func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// serverTLS writes a server certificate for localhost, signed by a new CA, into the data folder of the server with
// COPY TO PROGRAM, and turns TLS on with ALTER SYSTEM and a reload, as the runtime's tenant tests do. It turns TLS off
// again when the test ends, and gives the PEM file of the CA.
func serverTLS(t *testing.T, root *sql.DB) string {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "wiring test CA"},
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
	var dataDir string
	must(t, root.QueryRow("SHOW data_directory").Scan(&dataDir))
	write := func(name, text string) {
		lines := strings.Split(strings.TrimSpace(text), "\n")
		quoted := make([]string, len(lines))
		for i, l := range lines {
			quoted[i] = "('" + l + "')"
		}
		// The server reads a key only when no group or other may read it, so the file is written with umask 077.
		execAll(t, root, "COPY (SELECT line FROM (VALUES "+strings.Join(quoted, ", ")+") AS v(line)) TO PROGRAM 'rm -f "+
			dataDir+"/"+name+" && umask 077 && cat > "+dataDir+"/"+name+"'")
	}
	write("stego_wiring_server.crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})))
	write("stego_wiring_server.key", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	execAll(t, root, "ALTER SYSTEM SET ssl_cert_file = 'stego_wiring_server.crt'", "ALTER SYSTEM SET ssl_key_file = 'stego_wiring_server.key'",
		"ALTER SYSTEM SET ssl = on", "SELECT pg_reload_conf()")
	t.Cleanup(func() {
		for _, statement := range []string{"ALTER SYSTEM RESET ssl", "ALTER SYSTEM RESET ssl_cert_file", "ALTER SYSTEM RESET ssl_key_file", "SELECT pg_reload_conf()"} {
			_, _ = root.Exec(statement)
		}
	})
	time.Sleep(300 * time.Millisecond)
	return writeFile(t, "ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
}

// newDatabase makes a database of its own on the server, and gives its admin pool and its URL as the admin.
func newDatabase(t *testing.T, root *sql.DB, server *url.URL) (*sql.DB, *url.URL) {
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "wiring_" + hex.EncodeToString(suffix)
	execAll(t, root, "CREATE DATABASE "+name)
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)") })
	u := *server
	u.Path = "/" + name
	admin, err := sql.Open("pgx", u.String())
	must(t, err)
	t.Cleanup(func() { admin.Close() })
	return admin, &u
}

// prepare runs what the database administrator runs before the first migration job: the role migration of the
// tenancy component, which makes the owner, runtime, worker and relay roles and the purge and outbox relay roles of
// the service, with the owner's SET membership in the last two (ST-SUP-RC27-OC04), and a login of each role, which a
// deployment gives. No other role or grant is made by hand.
func prepare(t *testing.T, admin *sql.DB) {
	execAll(t, admin, tenancy.Roles)
	for _, role := range []string{stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole, migratorRole} {
		execAll(t, admin, "ALTER ROLE "+quote(role)+" LOGIN PASSWORD '"+password+"'")
	}
}

// grantSchema prepares another database of the server as prepare does: the role migration runs on each database,
// since the grants on the database and its schema public are the database's, and it gives each role its login again.
func grantSchema(t *testing.T, admin *sql.DB) { prepare(t, admin) }

// withTLS gives the URL of role on u, on the host localhost, with sslmode mode; verify-full also names the CA file.
func withTLS(u *url.URL, role, mode, caFile string) string {
	v := *u
	v.User = url.UserPassword(role, password)
	if _, port, err := net.SplitHostPort(v.Host); err == nil {
		v.Host = net.JoinHostPort("localhost", port)
	}
	q := v.Query()
	q.Set("sslmode", mode)
	q.Del("sslrootcert")
	if mode == "verify-full" {
		q.Set("sslrootcert", caFile)
	}
	v.RawQuery = q.Encode()
	return v.String()
}

// freePort gives a free loopback TCP port.
func freePort(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer listener.Close()
	return fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
}

// writeFile writes data to a new file of the test and gives its path.
func writeFile(t *testing.T, name string, data []byte) string {
	file := filepath.Join(t.TempDir(), name)
	must(t, os.WriteFile(file, data, 0o600))
	return file
}

// noTestMode turns the loopback test mode of the adapter off, which the CI checks turn on for their fixtures: every
// run of the binary needs verified TLS to the database.
const noTestMode = "STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK=0"

// run runs binary with args and the environment env, and waits at most a minute.
func run(binary string, env []string, args ...string) runResult {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(append(os.Environ(), env...), noTestMode)
	output, err := cmd.CombinedOutput()
	return runResult{err: err, output: string(output)}
}

// start prepares the database and runs the binary: the migration job with TLS that does not verify the server, on
// a database that holds a foreign table, and twice on its own database; the service with TLS that does not verify
// the server, and then the service with verified TLS, until it answers. It then runs the migration job of the next
// release, whose declaration adds an entity.
func start(t *testing.T) *service {
	dsn, endpoint := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_TEST_SPICEDB")
	binary, next := os.Getenv("STEGO_WIRING_BINARY"), os.Getenv("STEGO_WIRING_NEXT_BINARY")
	if dsn == "" || endpoint == "" || binary == "" || next == "" {
		t.Fatal("the check needs STEGO_TEST_POSTGRES_DSN, STEGO_TEST_SPICEDB, STEGO_WIRING_BINARY and STEGO_WIRING_NEXT_BINARY: a missing result is not a pass")
	}
	server, err := url.Parse(dsn)
	must(t, err)
	root, err := sql.Open("pgx", dsn)
	must(t, err)
	t.Cleanup(func() { root.Close() })
	s := &service{t: t, ctx: context.Background(), log: &bytes.Buffer{}}
	// The roles belong to the server: they are dropped after the databases, whose cleanups run first.
	t.Cleanup(func() {
		for _, role := range []string{stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole, stegoruntime.OwnerRole} {
			if _, err := root.Exec("DROP ROLE IF EXISTS " + quote(role)); err != nil {
				t.Errorf("drop the role %s: %v", role, err)
			}
		}
	})
	var u *url.URL
	s.admin, u = newDatabase(t, root, server)
	s.dbName = strings.TrimPrefix(u.Path, "/")
	prepare(t, s.admin)
	ca := serverTLS(t, root)

	spiceKey := os.Getenv("STEGO_TEST_SPICEDB_KEY")
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(bearer(spiceKey)))
	must(t, err)
	t.Cleanup(func() { conn.Close() })
	s.spice = conn
	keyFile := writeFile(t, "spicedb.key", []byte(spiceKey))
	spice := []string{"STEGO_SPICEDB_ENDPOINT=" + endpoint, "STEGO_SPICEDB_KEY_FILE=" + keyFile, "STEGO_SPICEDB_INSECURE_LOOPBACK=1"}
	// The service reaches SpiceDB through the recording proxy, so a check can count its decisions, as the fake
	// provider of ST-DECL-TD05 records them.
	s.checks = startRecorder(t, endpoint)
	serviceSpice := []string{"STEGO_SPICEDB_ENDPOINT=" + s.checks.addr, "STEGO_SPICEDB_KEY_FILE=" + keyFile, "STEGO_SPICEDB_INSECURE_LOOPBACK=1"}
	migration := func(mode string, target *url.URL) []string {
		return append(append([]string{}, spice...), "STEGO_MIGRATION_DATABASE_URL="+withTLS(target, migratorRole, mode, ca))
	}

	// A migration job with TLS that does not verify the server writes nothing.
	s.weakMigration = run(binary, migration("prefer", u), "migrate")
	// A migration job on a database that holds a table of the declaration and no ledger writes nothing.
	var foreignURL *url.URL
	s.foreignDB, foreignURL = newDatabase(t, root, server)
	grantSchema(t, s.foreignDB)
	execAll(t, s.foreignDB, "CREATE TABLE orders (id uuid PRIMARY KEY, sku text)")
	s.foreignMigration = run(binary, migration("verify-full", foreignURL), "migrate")
	// A migration job on a database whose ledger holds the digest of the name alone for 001_initial, as a release
	// on the GORM store wrote it, writes nothing: the ledger never takes the GORM tables over as the SQL baseline.
	var legacyURL *url.URL
	s.legacyDB, legacyURL = newDatabase(t, root, server)
	grantSchema(t, s.legacyDB)
	execAll(t, s.legacyDB, "CREATE TABLE orders (id uuid PRIMARY KEY, sku text)", "CREATE SCHEMA stego_schema",
		"CREATE TABLE stego_schema.migrations (version text PRIMARY KEY, digest text NOT NULL)",
		"INSERT INTO stego_schema.migrations VALUES ('001_initial', '"+storage.Digest("001_initial")+"')",
		// The release on the GORM store made them as the owner role, as the migration job of the release does.
		"ALTER SCHEMA stego_schema OWNER TO "+quote(stegoruntime.OwnerRole), "ALTER TABLE stego_schema.migrations OWNER TO "+quote(stegoruntime.OwnerRole),
		"ALTER TABLE orders OWNER TO "+quote(stegoruntime.OwnerRole))
	s.legacyMigration = run(binary, migration("verify-full", legacyURL), "migrate")

	if r := run(binary, migration("verify-full", u), "migrate"); r.err != nil {
		t.Fatalf("the migration job failed: %v\n%s", r.err, r.output)
	}
	s.secondMigration = run(binary, migration("verify-full", u), "migrate")

	s.key = authntest.NewKey(t, "k1")
	// The operator op-1 of the tenant ops holds provision and retire on service:wiring-shop, which is of no tenant; the
	// tenant ops is never provisioned.
	s.operator = s.as("op-"+uuid.NewString()[:8], "ops-"+uuid.NewString()[:8])
	s.grantOperator(s.operator)
	cursor := make([]byte, 32)
	_, _ = rand.Read(cursor)
	keys := writeFile(t, "keys.json", authntest.Document(s.key.JWK()))
	cursorFile := writeFile(t, "cursor.key", cursor)
	port := freePort(t)
	s.base = "http://127.0.0.1:" + port
	s.broker = kafkatest.Start(t)
	s.broker.CreateTopic(t, changeTopic, 3)
	kafka := []string{kafkaoutbox.BrokersSetting + "=" + s.broker.Plain.Addr(), kafkaoutbox.InsecureLoopbackSetting + "=1",
		kafkaoutbox.TopicSetting + "=" + changeTopic}
	serviceEnv := func(mode string) []string {
		return append(append(append([]string{}, serviceSpice...), kafka...),
			"PORT="+port, "DATABASE_URL="+withTLS(u, stegoruntime.RuntimeRole, mode, ca), "STEGO_AUTHN_KEYS_FILE="+keys,
			"STEGO_WORKER_DATABASE_URL="+withTLS(u, stegoruntime.WorkerRole, mode, ca),
			"STEGO_RELAY_DATABASE_URL="+withTLS(u, stegoruntime.RelayRole, mode, ca),
			"STEGO_QUERY_CURSOR_KEY_FILE="+cursorFile, "STEGO_SERVICE_VERSION=1.0.0")
	}
	// A service with TLS that does not verify the server does not start.
	s.weakStart = run(binary, serviceEnv("disable"))
	// The worker mode of a service with no Kafka setting does not start: it names the setting (ST-SUP-RC18-OC31).
	var unset []string
	for _, v := range serviceEnv("verify-full") {
		if !strings.HasPrefix(v, "STEGO_KAFKA_") {
			unset = append(unset, v)
		}
	}
	s.noKafka = run(binary, append(unset, kafkaoutbox.BrokersSetting+"=", kafkaoutbox.TopicSetting+"=", kafkaoutbox.InsecureLoopbackSetting+"="), "--mode=worker")
	// The modes are read before the database: an unknown mode stops the start at the stage modes (ST-GEN-INV26).
	s.unknownMode = run(binary, append(serviceEnv("verify-full"), "STEGO_MODE="), "--mode", "bogus")
	// The worker mode reads the connection string of the worker role at the start, and names it when it is missing.
	var noWorker []string
	for _, v := range serviceEnv("verify-full") {
		if !strings.HasPrefix(v, "STEGO_WORKER_DATABASE_URL=") {
			noWorker = append(noWorker, v)
		}
	}
	s.noWorkerURL = run(binary, append(noWorker, "STEGO_WORKER_DATABASE_URL=", "STEGO_WORKER_DATABASE_URL_FILE=", "STEGO_MODE="), "--mode", "worker")
	// The next release declares its three modes out of order: with no mode option and with --mode all the binary
	// selects them in the order api, worker, controller, which the failure record of the missing database setting
	// names.
	noDatabase := []string{"DATABASE_URL=", "DATABASE_URL_FILE=", "STEGO_MODE="}
	s.allModes = run(next, noDatabase, "--mode", "all")
	s.declaredDefault = run(next, noDatabase)

	// start starts the service process with args, in all modes when args is empty, and waits until it answers
	// /livez; stop stops it and waits.
	var stop func()
	start := func(t *testing.T, args ...string) {
		t.Helper()
		process := exec.Command(binary, args...)
		process.Env = append(append(os.Environ(), serviceEnv("verify-full")...), noTestMode)
		process.Stdout, process.Stderr = s.log, s.log
		must(t, process.Start())
		exited := make(chan struct{})
		go func() { _ = process.Wait(); close(exited) }()
		stop = func() {
			_ = process.Process.Signal(os.Interrupt)
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				_ = process.Process.Kill()
				<-exited
			}
		}
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			response, err := http.Get(s.base + "/livez")
			status := 0
			if err == nil {
				response.Body.Close()
				if status = response.StatusCode; status == http.StatusOK {
					break
				}
			}
			select {
			case <-exited:
				t.Fatalf("the service stopped before it answered /livez:\n%s", s.log)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("the service does not answer /livez with 200: %d %v\n%s", status, err, s.log)
			}
		}
	}
	t.Cleanup(func() {
		stop()
		if t.Failed() {
			t.Logf("service log:\n%s", s.log)
		}
	})
	start(t)
	// restart stops the service and starts it again, as the rollout of a release does.
	s.restart = func(t *testing.T) {
		t.Helper()
		stop()
		start(t)
	}
	s.restartWith = func(t *testing.T, args ...string) {
		t.Helper()
		stop()
		start(t, args...)
	}
	schema, err := v1.NewSchemaServiceClient(s.spice).ReadSchema(s.ctx, &v1.ReadSchemaRequest{})
	must(t, err)
	s.schemaBeforeNext = schema.SchemaText
	// The next release adds an entity, whose table is not in the baseline of the ledger.
	s.nextRelease = run(next, migration("verify-full", u), "migrate")
	return s
}

// bearer sends the preshared key of the local SpiceDB server on each call.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return false }

// caller is one caller of a tenant, with a token.
type caller struct {
	tenant  tenant.Tenant
	subject string
	token   string
}

// as mints a token for subject in tenantID, for the declared audience or the audience aud.
func (s *service) as(subject, tenantID string, aud ...string) caller {
	t := s.t
	claims := authntest.Claims(issuer, audience, subject, time.Now())
	if len(aud) > 0 {
		claims["aud"] = aud[0]
	}
	claims["tenant"] = tenantID
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	token := authntest.Mint(t, authntest.Header("k1"), claims, s.key)
	keys, err := authn.ParseKeys(authntest.Document(s.key.JWK()))
	must(t, err)
	verifier, err := authn.New(authn.Settings{Issuer: issuer, Audience: claims["aud"].(string), TenantClaim: "tenant", ProblemCode: "CHECK", Keys: keys})
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

// own writes the owner relation of the caller on an order of the caller's tenant in SpiceDB.
func (s *service) own(c caller, id string) {
	resourceID, err := authz.ResourceID(c.tenant, id)
	must(s.t, err)
	_, err = v1.NewPermissionsServiceClient(s.spice).WriteRelationships(s.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: "order", ObjectId: resourceID}, Relation: "owner",
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject}}}}}})
	must(s.t, err)
}

// serviceName is the name of the service, the ID of its service-level resource (ST-SUP-RC27-OC09).
const serviceName = "wiring-shop"

// grantOperator writes the relation operator of c on the service-level resource service:wiring-shop in SpiceDB, whose
// ID is not tenant-qualified (ST-SUP-RP06-LC22), as the operator of the platform does.
func (s *service) grantOperator(c caller) { s.grantOn(c, serviceName) }

// grantTenantOperator writes the relation operator of c on service:<c's tenant>/wiring-shop, the tenant-qualified
// form in c's own tenant, which no provisioning decides on.
func (s *service) grantTenantOperator(c caller) {
	resourceID, err := authz.ResourceID(c.tenant, serviceName)
	must(s.t, err)
	s.grantOn(c, resourceID)
}

// grantOn writes the relation operator of c on the resource of type service and ID resourceID in SpiceDB.
func (s *service) grantOn(c caller, resourceID string) {
	_, err := v1.NewPermissionsServiceClient(s.spice).WriteRelationships(s.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: "service", ObjectId: resourceID}, Relation: "operator",
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject}}}}}})
	must(s.t, err)
}

// provision calls Provision for the tenant id as c, with the idempotency key given.
func (s *service) provision(c *caller, id, key string) reply {
	s.t.Helper()
	body, err := json.Marshal(map[string]string{"tenant_id": id})
	must(s.t, err)
	return s.send(c, http.MethodPost, "/stego/tenants", key, string(body))
}

// retire calls Retire for the tenant id as c, with the idempotency key given.
func (s *service) retire(c *caller, id, key string) reply {
	s.t.Helper()
	return s.send(c, http.MethodPost, "/stego/tenants/"+url.PathEscape(id)+"/retire", key, "")
}

// provisioned provisions the tenant id as the operator and fails the check when it is not provisioned.
func (s *service) provisioned(t *testing.T, id string) {
	t.Helper()
	if got := s.provision(&s.operator, id, "k-"+uuid.NewString()); got.status != http.StatusCreated {
		t.Fatalf("the provision of %s as the operator answered %d %s, want 201", id, got.status, got.body)
	}
}

// listed gives the tenant row of id: whether the tenant list holds it and whether it has a retire time.
func (s *service) listed(t *testing.T, id string) (held, retired bool) {
	t.Helper()
	var retiredAt sql.NullTime
	err := s.admin.QueryRow(`SELECT retired_at FROM stego_tenants WHERE tenant_id = $1`, id).Scan(&retiredAt)
	if err == sql.ErrNoRows {
		return false, false
	}
	must(t, err)
	return true, retiredAt.Valid
}

// spiceRecorder is a gRPC proxy in front of the local SpiceDB server. It forwards each call as it is, and records
// each decision of CheckPermission, so a check can tell which decisions a request made.
type spiceRecorder struct {
	addr   string
	mu     sync.Mutex
	checks []*v1.CheckPermissionRequest
}

// rawCodec passes the bytes of each message through the proxy as they are.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) { return *(v.(*[]byte)), nil }
func (rawCodec) Unmarshal(data []byte, v any) error {
	*(v.(*[]byte)) = append([]byte(nil), data...)
	return nil
}
func (rawCodec) Name() string { return "proto" }

const checkPermissionMethod = "/authzed.api.v1.PermissionsService/CheckPermission"

// startRecorder starts the proxy to the SpiceDB server at target, and stops it when the test ends.
func startRecorder(t *testing.T, target string) *spiceRecorder {
	backend, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	must(t, err)
	t.Cleanup(func() { backend.Close() })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	r := &spiceRecorder{addr: listener.Addr().String()}
	server := grpc.NewServer(grpc.ForceServerCodec(rawCodec{}), grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(stream)
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
				if method == checkPermissionMethod {
					request := &v1.CheckPermissionRequest{}
					if proto.Unmarshal(message, request) == nil {
						r.mu.Lock()
						r.checks = append(r.checks, request)
						r.mu.Unlock()
					}
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

// decisions counts the decisions of permission on the resource of type resourceType and ID resourceID, or of any ID
// when resourceID is empty.
func (r *spiceRecorder) decisions(resourceType, resourceID, permission string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.checks {
		if c.GetResource().GetObjectType() == resourceType && c.GetPermission() == permission &&
			(resourceID == "" || c.GetResource().GetObjectId() == resourceID) {
			n++
		}
	}
	return n
}

// total counts every decision.
func (r *spiceRecorder) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.checks)
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

// send sends one request as the caller, with an Idempotency-Key when key is not empty.
func (s *service) send(c *caller, method, path, key, body string) reply {
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

func TestWiring(t *testing.T) {
	s := start(t)
	owner := s.as("u-"+uuid.NewString()[:8], "t-"+uuid.NewString()[:8])
	var order string

	t.Run("ST-SUP-RC01-OC21-T03 The generated migration job runs as the owner role and the service as the runtime role", func(t *testing.T) {
		s.t = t
		if n := count(t, s.admin, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner <> $1`, stegoruntime.OwnerRole); n != 0 {
			t.Errorf("%d tables of public are not owned by the owner role %s", n, stegoruntime.OwnerRole)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename IN ('orders', 'notes', 'idempotency_keys')`); n != 3 {
			t.Errorf("the migration job made %d of the tables orders, notes and idempotency_keys", n)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM pg_class c JOIN pg_roles r ON r.oid = c.relowner WHERE r.rolname = $1`, stegoruntime.RuntimeRole); n != 0 {
			t.Errorf("the runtime role owns %d relations", n)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND usename = $2`, s.dbName, stegoruntime.RuntimeRole); n == 0 {
			t.Error("the service holds no connection as the runtime role")
		}
		// The worker mode connects as the worker role and the relay as the relay role (ST-SUP-RP14-LC04); the
		// request path holds no other role.
		if n := count(t, s.admin, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND backend_type = 'client backend' AND usename NOT IN ($2, $3, $4, current_user)`, s.dbName, stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole); n != 0 {
			t.Errorf("the service holds %d connections as another role than the runtime, worker and relay roles", n)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM pg_stat_activity WHERE usename = $1`, migratorRole); n != 0 {
			t.Errorf("%d connections use the login of the migration job after it ended", n)
		}
	})

	t.Run("ST-SUP-RC01-OC41-T04 The runtime pool is opened through tenant.Open with the adapter's verified TLS configuration", func(t *testing.T) {
		s.t = t
		if n := count(t, s.admin, `SELECT count(*) FROM pg_stat_activity a JOIN pg_stat_ssl l ON l.pid = a.pid WHERE a.datname = $1 AND a.usename IN ($2, $3, $4) AND NOT l.ssl`, s.dbName, stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole); n != 0 {
			t.Errorf("%d connections of the service do not use TLS", n)
		}
		if s.weakStart.err == nil {
			t.Errorf("the service started with sslmode disable:\n%s", s.weakStart.output)
		}
		if s.weakMigration.err == nil || !strings.Contains(s.weakMigration.output, `"stage":"migrate.settings"`) {
			t.Errorf("the migration job with sslmode prefer gave %v, want a refusal of its settings:\n%s", s.weakMigration.err, s.weakMigration.output)
		}
	})

	t.Run("ST-SUP-RC01-OC41-T07 The generated migration job runs again on its own database and refuses a database with tables it did not make", func(t *testing.T) {
		s.t = t
		if s.secondMigration.err != nil {
			t.Errorf("the second run of the migration job failed: %v\n%s", s.secondMigration.err, s.secondMigration.output)
		}
		if s.foreignMigration.err == nil || !strings.Contains(s.foreignMigration.output, `"stage":"migrate.ledger_check"`) {
			t.Errorf("the migration job on a database with a table of the declaration and no ledger gave %v, want the refusal of the ledger check:\n%s", s.foreignMigration.err, s.foreignMigration.output)
		}
		if n := count(t, s.foreignDB, `SELECT count(*) FROM pg_namespace WHERE nspname IN ('stego_schema', 'stego_outbox')`); n != 0 {
			t.Errorf("the refused migration job made %d schemas", n)
		}
		if s.legacyMigration.err == nil || !strings.Contains(s.legacyMigration.output, `"stage":"migrate.ledger_check"`) {
			t.Errorf("the migration job on a database whose ledger holds the name digest of a GORM release gave %v, want the refusal of the ledger check:\n%s", s.legacyMigration.err, s.legacyMigration.output)
		}
		if n := count(t, s.legacyDB, `SELECT count(*) FROM stego_schema.migrations WHERE digest = $1`, storage.Digest("001_initial")); n != 1 {
			t.Error("the refused migration job changed the ledger row of the GORM release")
		}
		if n := count(t, s.legacyDB, `SELECT count(*) FROM pg_namespace WHERE nspname = 'stego_outbox'`); n != 0 {
			t.Error("the refused migration job made the outbox schema")
		}
		if s.nextRelease.err == nil || !strings.Contains(s.nextRelease.output, `"stage":"migrate.ledger_check"`) {
			t.Errorf("the migration job of a release that adds an entity with no schema step gave %v, want the refusal of the ledger check:\n%s", s.nextRelease.err, s.nextRelease.output)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename = 'tags'`); n != 0 {
			t.Error("the refused migration job of the next release made the table tags")
		}
		schema, err := v1.NewSchemaServiceClient(s.spice).ReadSchema(s.ctx, &v1.ReadSchemaRequest{})
		must(t, err)
		if schema.SchemaText != s.schemaBeforeNext {
			t.Error("the refused migration job of the next release wrote the authorization schema")
		}
	})

	t.Run("ST-SUP-RC29-OC23-T02 The generated migration job writes the schema to SpiceDB before the service starts", func(t *testing.T) {
		s.t = t
		for _, want := range []string{"definition order", "definition note", "definition stego_floor"} {
			if !strings.Contains(s.schemaBeforeNext, want) {
				t.Errorf("SpiceDB holds no %q after the migration job:\n%s", want, s.schemaBeforeNext)
			}
		}
	})

	// The checks of the tenant provisioning (ST-SUP-RC27-OC09 to OC11, ST-DECL-UC10). The first two run while the
	// tenant list holds no active tenant.
	short := func() string { return uuid.NewString()[:8] }
	refusedTenant := func(t *testing.T, got reply, what string) {
		t.Helper()
		if got.status != http.StatusForbidden || got.member(t, "code") != "tenant.not_provisioned" {
			t.Errorf("%s answered %d %s, want 403 with the code tenant.not_provisioned", what, got.status, got.body)
		}
	}

	t.Run("ST-SUP-RC27-OC11-T02 The provisioning routes pass the tenant check on an empty service", func(t *testing.T) {
		s.t = t
		if n := count(t, s.admin, `SELECT count(*) FROM stego_tenants`); n != 0 {
			t.Fatalf("the tenant list holds %d tenants before the first provision, want none", n)
		}
		id, resource := "org-a-"+short(), serviceName
		provisions, retires := s.checks.decisions("service", resource, "provision"), s.checks.decisions("service", resource, "retire")
		if got := s.provision(&s.operator, id, "k-"+uuid.NewString()); got.status != http.StatusCreated || got.member(t, "tenant_id") != id {
			t.Fatalf("the provision of %s by the operator of a tenant that is not provisioned answered %d %s, want 201", id, got.status, got.body)
		}
		if n := s.checks.decisions("service", resource, "provision"); n != provisions+1 {
			t.Errorf("the provision made %d decisions of provision on service:%s, want one", n-provisions, serviceName)
		}
		if got := s.retire(&s.operator, id, "k-"+uuid.NewString()); got.status != http.StatusOK {
			t.Fatalf("the retire of %s answered %d %s, want 200", id, got.status, got.body)
		}
		if n := s.checks.decisions("service", resource, "retire"); n != retires+1 {
			t.Errorf("the retire made %d decisions of retire on service:%s, want one", n-retires, serviceName)
		}
		refusedTenant(t, s.send(&s.operator, http.MethodGet, "/orders", "", ""), "a list of orders by the operator, whose tenant is not provisioned,")
	})

	t.Run("ST-DECL-UC10-R04-T02 An operator provisions the first tenant of an empty service", func(t *testing.T) {
		s.t = t
		if n := count(t, s.admin, `SELECT count(*) FROM stego_tenants WHERE retired_at IS NULL`); n != 0 {
			t.Fatalf("the tenant list holds %d active tenants before the first provision, want none", n)
		}
		u1 := s.as("u1-"+short(), "org-a-"+short())
		resource := serviceName
		before := s.checks.decisions("service", resource, "provision")
		s.provisioned(t, u1.tenant.ID())
		if n := s.checks.decisions("service", resource, "provision"); n != before+1 {
			t.Errorf("the provision made %d decisions on service:%s, want one", n-before, serviceName)
		}
		if got := s.send(&u1, http.MethodPost, "/orders", "k-"+uuid.NewString(), `{"sku":"first","qty":1}`); got.status != http.StatusCreated {
			t.Errorf("the create of u1 in the new tenant answered %d %s, want 201", got.status, got.body)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM orders WHERE tenant_id = $1 AND sku = 'first'`, u1.tenant.ID()); n != 1 {
			t.Errorf("the new tenant holds %d orders of the create, want one", n)
		}
	})

	decided := func(t *testing.T) {
		u2 := s.as("u2-"+short(), "org-b-"+short())
		id, resource := "org-c-"+short(), serviceName
		before := s.checks.decisions("service", resource, "provision")
		key := "k-" + uuid.NewString()
		first := s.provision(&s.operator, id, key)
		if first.status != http.StatusCreated || first.member(t, "tenant_id") != id {
			t.Fatalf("the provision of %s by the operator answered %d %s, want 201", id, first.status, first.body)
		}
		if n := s.checks.decisions("service", resource, "provision"); n != before+1 {
			t.Errorf("the provision made %d decisions of provision for the operator, want one", n-before)
		}
		if held, retired := s.listed(t, id); !held || retired {
			t.Errorf("the tenant list holds %s: %t, retired %t; want it active", id, held, retired)
		}
		// The same request with its key gets the stored answer again; a request with no key is refused.
		if again := s.provision(&s.operator, id, key); again.status != http.StatusCreated || !bytes.Equal(again.body, first.body) {
			t.Errorf("the provision sent again with its key answered %d %s, want the first answer %s", again.status, again.body, first.body)
		}
		if got := s.provision(&s.operator, "org-k-"+short(), ""); got.status != http.StatusBadRequest {
			t.Errorf("a provision with no Idempotency-Key answered %d %s, want 400", got.status, got.body)
		}
		// A tenant ID that the rule of a tenant ID refuses reaches no decision.
		total := s.checks.total()
		if got := s.provision(&s.operator, "org\x01bad", "k-"+uuid.NewString()); got.status != http.StatusBadRequest || got.member(t, "field") != "tenant_id" {
			t.Errorf("a provision of a tenant ID with a control character answered %d %s, want 400 that names tenant_id", got.status, got.body)
		}
		if s.checks.total() != total {
			t.Error("the provision of a malformed tenant ID was decided")
		}
		other := "org-d-" + short()
		denied := s.checks.decisions("service", serviceName, "provision")
		if got := s.provision(&u2, other, "k-"+uuid.NewString()); got.status != http.StatusNotFound {
			t.Errorf("the provision by u2, whom the provider denies, answered %d %s, want 404", got.status, got.body)
		}
		if n := s.checks.decisions("service", serviceName, "provision"); n != denied+1 {
			t.Errorf("the provision by u2 made %d decisions, want one", n-denied)
		}
		if held, _ := s.listed(t, other); held {
			t.Errorf("the denied provision added %s to the tenant list", other)
		}
		total = s.checks.total()
		if got := s.send(nil, http.MethodPost, "/stego/tenants", "k-"+uuid.NewString(), `{"tenant_id":"`+other+`"}`); got.status != http.StatusUnauthorized {
			t.Errorf("a provision with no token answered %d %s, want 401", got.status, got.body)
		}
		if s.checks.total() != total {
			t.Error("the provision with no token was decided")
		}
		if got := s.retire(&u2, id, "k-"+uuid.NewString()); got.status != http.StatusNotFound {
			t.Errorf("the retire by u2 answered %d %s, want 404", got.status, got.body)
		}
		if held, retired := s.listed(t, id); !held || retired {
			t.Errorf("the denied retire changed %s: held %t, retired %t", id, held, retired)
		}
	}
	t.Run("ST-SUP-RC27-OC09-T01 A caller without the provision permission cannot provision a tenant", func(t *testing.T) {
		s.t = t
		decided(t)
	})
	t.Run("ST-DECL-UC10-R01-T01 A caller without the provision permission cannot provision a tenant", func(t *testing.T) {
		s.t = t
		decided(t)
	})

	t.Run("ST-SUP-RC27-OC09-T03 A grant on the service-level resource of one tenant provisions and retires no tenant", func(t *testing.T) {
		s.t = t
		// alice of the provisioned tenant org-a holds operator on service:<org-a>/wiring-shop, the resource of her own
		// tenant, in SpiceDB; org-b is a tenant of another customer.
		alice := s.as("alice-"+short(), "org-a-"+short())
		s.provisioned(t, alice.tenant.ID())
		victim := "org-b-" + short()
		s.provisioned(t, victim)
		s.grantTenantOperator(alice)
		perTenant, err := authz.ResourceID(alice.tenant, serviceName)
		must(t, err)
		decided := s.checks.decisions("service", serviceName, "provision") + s.checks.decisions("service", serviceName, "retire")
		if got := s.retire(&alice, victim, "k-"+uuid.NewString()); got.status != http.StatusNotFound {
			t.Errorf("the retire of %s by alice answered %d %s, want 404", victim, got.status, got.body)
		}
		if held, retired := s.listed(t, victim); !held || retired {
			t.Errorf("the retire by alice changed %s: held %t, retired %t", victim, held, retired)
		}
		if got := s.retire(&alice, alice.tenant.ID(), "k-"+uuid.NewString()); got.status != http.StatusNotFound {
			t.Errorf("the retire of her own tenant by alice answered %d %s, want 404", got.status, got.body)
		}
		squat := "org-future-" + short()
		if got := s.provision(&alice, squat, "k-"+uuid.NewString()); got.status != http.StatusNotFound {
			t.Errorf("the provision of %s by alice answered %d %s, want 404", squat, got.status, got.body)
		}
		if held, _ := s.listed(t, squat); held {
			t.Errorf("the provision by alice added %s to the tenant list", squat)
		}
		// Each of the three calls was decided once on service:wiring-shop, and none on the resource of her tenant.
		if n := s.checks.decisions("service", serviceName, "provision") + s.checks.decisions("service", serviceName, "retire"); n != decided+3 {
			t.Errorf("the calls of alice made %d decisions on service:%s, want three", n-decided, serviceName)
		}
		if n := s.checks.decisions("service", perTenant, "provision") + s.checks.decisions("service", perTenant, "retire"); n != 0 {
			t.Errorf("%d decisions named the tenant-qualified service:<org-a>/%s", n, serviceName)
		}
	})

	audited := func(t *testing.T) {
		id := "org-c-" + short()
		xmin := func(query string, args ...any) string {
			t.Helper()
			var x string
			must(t, s.admin.QueryRow(query, args...).Scan(&x))
			return x
		}
		const auditRow = `SELECT xmin::text FROM audit_event WHERE tenant_id = $1 AND action = $2 AND resource_type = 'tenant' AND resource_id = $1 AND outcome = 'ok'`
		s.provisioned(t, id)
		if n := count(t, s.admin, `SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'provision-tenant'`, id); n != 1 {
			t.Fatalf("the provision of %s left %d audit rows, want one", id, n)
		}
		if row, list := xmin(auditRow, id, "provision-tenant"), xmin(`SELECT xmin::text FROM stego_tenants WHERE tenant_id = $1`, id); row != list {
			t.Errorf("the audit row of the provision is of the transaction %s and the tenant row of %s, want one transaction", row, list)
		}
		if got := s.retire(&s.operator, id, "k-"+uuid.NewString()); got.status != http.StatusOK {
			t.Fatalf("the retire of %s answered %d %s, want 200", id, got.status, got.body)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'retire-tenant'`, id); n != 1 {
			t.Fatalf("the retire of %s left %d audit rows, want one", id, n)
		}
		if row, list := xmin(auditRow, id, "retire-tenant"), xmin(`SELECT xmin::text FROM stego_tenants WHERE tenant_id = $1`, id); row != list {
			t.Errorf("the audit row of the retire is of the transaction %s and the tenant row of %s, want one transaction", row, list)
		}
		// A provision whose hook fails: the audit write of its transaction is refused for org-d. The caller is an
		// operator of a provisioned tenant, so its outcome row goes into the chain of its tenant; a caller of a tenant
		// that the list does not hold, such as the operator of ops, writes no row (ST-SUP-RC27-OC10-T02).
		listedOp := s.as("op-"+short(), "org-ops-"+short())
		s.provisioned(t, listedOp.tenant.ID())
		s.grantOperator(listedOp)
		// The worker tuples of each provision move the revocation floor, so the decision sees the grant only when
		// SpiceDB serves it at its quantized revision (ST-SUP-RC35-OC13).
		s.canProvision(t, listedOp)
		failing := "org-d-" + short()
		execAll(t, s.admin, `CREATE FUNCTION public.wiring_fail_hook() RETURNS trigger LANGUAGE plpgsql AS $f$ BEGIN
	IF NEW.tenant_id = '`+failing+`' THEN RAISE EXCEPTION 'the hook of the check fails'; END IF; RETURN NEW; END $f$`,
			`CREATE TRIGGER wiring_fail_hook BEFORE INSERT ON public.audit_event FOR EACH ROW EXECUTE FUNCTION public.wiring_fail_hook()`)
		got := s.provision(&listedOp, failing, "k-"+uuid.NewString())
		execAll(t, s.admin, `DROP TRIGGER wiring_fail_hook ON public.audit_event`, `DROP FUNCTION public.wiring_fail_hook()`)
		if got.status != http.StatusInternalServerError {
			t.Errorf("the provision whose hook fails answered %d %s, want 500", got.status, got.body)
		}
		if held, _ := s.listed(t, failing); held {
			t.Errorf("the failed provision left %s in the tenant list", failing)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'provision-tenant' AND resource_id = $2 AND outcome = 'failed'`, listedOp.tenant.ID(), failing); n != 1 {
			t.Errorf("the failed provision left %d outcome rows of failure, want one", n)
		}
	}
	t.Run("ST-SUP-RC27-OC10-T01 A provision and a retire each leave one audit row in their transaction", func(t *testing.T) {
		s.t = t
		audited(t)
	})
	t.Run("ST-DECL-UC10-R02-T01 A provision and a retire each leave one audit row in their transaction", func(t *testing.T) {
		s.t = t
		audited(t)
	})

	gated := func(t *testing.T) {
		unknown := s.as("u-"+short(), "org-z-"+short())
		total := s.checks.total()
		refusedTenant(t, s.send(&unknown, http.MethodPost, "/orders/"+uuid.NewString()+"/notes", "k-"+uuid.NewString(), `{"text":"z"}`),
			"the command of a tenant that is not provisioned")
		if s.checks.total() != total {
			t.Error("the command of a tenant that is not provisioned was decided")
		}
		if n := count(t, s.admin, `SELECT (SELECT count(*) FROM notes WHERE tenant_id = $1) + (SELECT count(*) FROM audit_event WHERE tenant_id = $1)`, unknown.tenant.ID()); n != 0 {
			t.Errorf("the command of a tenant that is not provisioned left %d notes and audit rows", n)
		}
		u1 := s.as("u1-"+short(), "org-a-"+short())
		s.provisioned(t, u1.tenant.ID())
		created := s.send(&u1, http.MethodPost, "/orders", "k-"+uuid.NewString(), `{"sku":"kept","qty":1}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create of u1 answered %d %s, want 201", created.status, created.body)
		}
		order := created.member(t, "id")
		s.own(u1, order)
		if got := s.retire(&s.operator, u1.tenant.ID(), "k-"+uuid.NewString()); got.status != http.StatusOK {
			t.Fatalf("the retire of the tenant of u1 answered %d %s, want 200", got.status, got.body)
		}
		total = s.checks.total()
		refusedTenant(t, s.send(&u1, http.MethodPost, "/orders/"+order+"/notes", "k-"+uuid.NewString(), `{"text":"retired"}`),
			"the command of a retired tenant")
		if s.checks.total() != total {
			t.Error("the command of a retired tenant was decided")
		}
		// Retire keeps the rows of the tenant (ST-SUP-RC27-OC12).
		if n := count(t, s.admin, `SELECT count(*) FROM orders WHERE tenant_id = $1 AND id = $2`, u1.tenant.ID(), order); n != 1 {
			t.Errorf("the retired tenant holds %d of its orders, want its one order kept", n)
		}
		// The command of u2 of a provisioned tenant runs as usual: the create of an order, which stages the tuple of
		// the order to its tenant (ST-SUP-Q132).
		u2 := s.as("u2-"+short(), "org-b-"+short())
		s.provisioned(t, u2.tenant.ID())
		if got := s.send(&u2, http.MethodPost, "/orders", "k-"+uuid.NewString(), `{"sku":"b1","qty":1}`); got.status != http.StatusCreated {
			t.Errorf("the command of u2 of a provisioned tenant answered %d %s, want 201", got.status, got.body)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM orders WHERE tenant_id = $1 AND sku = 'b1'`, u2.tenant.ID()); n != 1 {
			t.Errorf("the tenant of u2 holds %d orders of its command, want one", n)
		}
	}
	t.Run("ST-SUP-RC27-OC11-T01 A request for an unknown or retired tenant is refused before any handler", func(t *testing.T) {
		s.t = t
		gated(t)
	})
	t.Run("ST-DECL-UC10-R04-T01 A request for an unknown or retired tenant is refused before any handler", func(t *testing.T) {
		s.t = t
		gated(t)
	})

	t.Run("ST-SUP-RC20-OC37-T07 A collection create and read reach the generated handlers through the generated main.go", func(t *testing.T) {
		s.t = t
		s.provisioned(t, owner.tenant.ID())
		if got := s.send(nil, http.MethodGet, "/orders", "", ""); got.status != http.StatusUnauthorized {
			t.Errorf("a list with no token answered %d %s, want 401", got.status, got.body)
		}
		stranger := s.as("u-"+uuid.NewString()[:8], owner.tenant.ID(), "another-service")
		if got := s.send(&stranger, http.MethodGet, "/orders", "", ""); got.status != http.StatusUnauthorized {
			t.Errorf("a list with a token for another audience than the declared one answered %d %s, want 401", got.status, got.body)
		}
		created := s.send(&owner, http.MethodPost, "/orders", "k-"+uuid.NewString(), `{"sku":"a1","qty":2}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		order = created.member(t, "id")
		s.own(owner, order)
		// The tuples of the provision and of the create to its tenant move the revocation floor: the read is decided
		// at least as fresh as it, so the check waits until SpiceDB serves the owner tuple (ST-SUP-RC35-OC13).
		s.canEdit(t, owner, order)
		read := s.send(&owner, http.MethodGet, "/orders/"+order, "", "")
		if read.status != http.StatusOK || read.member(t, "sku") != "a1" {
			t.Fatalf("the read answered %d %s, want 200 with sku a1", read.status, read.body)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM orders WHERE id = $1 AND tenant_id = $2`, order, owner.tenant.ID()); n != 1 {
			t.Errorf("the orders table holds %d rows of the order in the caller's tenant, want 1", n)
		}
	})

	t.Run("ST-SUP-RC32-OC02-T03 A declared command reaches its fill through the generated main.go", func(t *testing.T) {
		s.t = t
		if order == "" {
			t.Fatal("no order: the collection check did not pass")
		}
		got := s.send(&owner, http.MethodPost, "/orders/"+order+"/notes", "k-"+uuid.NewString(), `{"text":"hello"}`)
		if got.status != http.StatusCreated {
			t.Fatalf("the command answered %d %s, want 201", got.status, got.body)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM notes WHERE order_id = $1 AND text = 'hello' AND tenant_id = $2`, order, owner.tenant.ID()); n != 1 {
			t.Errorf("the notes table holds %d notes of the command, want 1", n)
		}
		other := s.as("u-"+uuid.NewString()[:8], owner.tenant.ID())
		if got := s.send(&other, http.MethodPost, "/orders/"+order+"/notes", "k-"+uuid.NewString(), `{"text":"denied"}`); got.status != http.StatusNotFound {
			t.Errorf("the command of a caller who may not edit the order answered %d %s, want 404", got.status, got.body)
		}
	})

	t.Run("ST-SUP-RP14-LC04-T02 The generated service runs its workers through the service runtime", func(t *testing.T) {
		s.t = t
		if order == "" {
			t.Fatal("no order: the collection check did not pass")
		}
		eventually := func(what string, ok func() bool) {
			t.Helper()
			for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
				if ok() {
					return
				}
			}
			t.Fatalf("after 60 s: %s\n%s", what, s.log)
		}
		// The after slot of the create is an effect of its command: the outbox worker of the worker mode runs it, as
		// the worker role, and the message is done (ST-SUP-RC20-OC34, ST-SUP-RC18-OC25).
		eventually("the after slot of the create is delivered", func() bool {
			return count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE tenant_id = $1 AND destination = 'rest-api.after-slots' AND state = 'done'`, owner.tenant.ID()) == 1
		})
		if n := count(t, s.admin, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND usename = $2`, s.dbName, stegoruntime.WorkerRole); n == 0 {
			t.Error("the service holds no connection as the worker role")
		}
		// The command add-note writes a Note that refers to the order: the relay, as the relay role, writes the
		// relation note#order of the reference to SpiceDB (ST-SUP-RP10-LC05, ST-SUP-RP06-LC06), so the owner of the
		// order may view the note.
		created := s.send(&owner, http.MethodPost, "/orders/"+order+"/notes", "k-"+uuid.NewString(), `{"text":"relayed"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the command answered %d %s, want 201", created.status, created.body)
		}
		var note string
		must(t, s.admin.QueryRow(`SELECT id::text FROM notes WHERE text = 'relayed' AND tenant_id = $1`, owner.tenant.ID()).Scan(&note))
		noteID, err := authz.ResourceID(owner.tenant, note)
		must(t, err)
		eventually("the relay writes the relation of the note to its order", func() bool {
			answer, err := v1.NewPermissionsServiceClient(s.spice).CheckPermission(s.ctx, &v1.CheckPermissionRequest{
				Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
				Resource:    &v1.ObjectReference{ObjectType: "note", ObjectId: noteID}, Permission: "view",
				Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: owner.subject}}})
			return err == nil && answer.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
		})
		// The anchor job of the controller mode anchors the chain of the tenant under the controller lease, at the
		// declared anchor period of 2 s: the boundary is stored and the anchor is in the outbox for the audit sink.
		eventually("the anchor job anchors the chain of the tenant", func() bool {
			return count(t, s.admin, `SELECT count(*) FROM audit_anchor_boundaries WHERE tenant_id = $1`, owner.tenant.ID()) == 1 &&
				count(t, s.admin, `SELECT (SELECT count(*) FROM stego_outbox.messages WHERE tenant_id = $1 AND destination = 'audit-sink')
 - (SELECT count(*) FROM audit_event WHERE tenant_id = $1)`, owner.tenant.ID()) >= 1
		})
		if n := count(t, s.admin, `SELECT count(*) FROM leases WHERE name = 'stego.controller' AND expires_at > now()`); n != 1 {
			t.Errorf("%d holders of the controller lease, want one", n)
		}
		// The readiness of the modes answers every mode ready.
		eventually("every mode is ready", func() bool {
			response, err := http.Get(s.base + "/readyz/modes")
			if err != nil {
				return false
			}
			defer response.Body.Close()
			return response.StatusCode == http.StatusOK
		})
	})

	t.Run("ST-SUP-RP14-LC04-T03 The generated worker delivers each audit entry and anchor to the default table", func(t *testing.T) {
		s.t = t
		if order == "" {
			t.Fatal("no order: the collection check did not pass")
		}
		// No STEGO_AUDIT_SINK_URL is set, so the worker mode binds the audit-sink port to the table of its own in the
		// service database: every audit-sink message of the tenant is done, and the table holds one row for each,
		// keyed by the event ID or the anchor ID, written as the worker role.
		var last string
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			pending := count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE tenant_id = $1 AND destination = 'audit-sink' AND state <> 'done'`, owner.tenant.ID())
			// The anchor job publishes an anchor at each period, also of a chain with no new event; the anchors of one
			// chain head are one message body and one entry of the table, so the done messages count once for each
			// body (ST-SUP-RP05-LC09).
			done := count(t, s.admin, `SELECT count(DISTINCT payload) FROM stego_outbox.messages WHERE tenant_id = $1 AND destination = 'audit-sink' AND state = 'done'`, owner.tenant.ID())
			events := count(t, s.admin, `SELECT count(*) FROM stego_audit_sink.entries e JOIN audit_event a ON a.id::text = e.entry_id WHERE e.tenant_id = $1 AND e.kind = 'event'`, owner.tenant.ID())
			anchors := count(t, s.admin, `SELECT count(*) FROM stego_audit_sink.entries WHERE tenant_id = $1 AND kind = 'anchor'`, owner.tenant.ID())
			audited := count(t, s.admin, `SELECT count(*) FROM audit_event WHERE tenant_id = $1`, owner.tenant.ID())
			last = fmt.Sprintf("%d pending, %d done, %d events of %d and %d anchors in the table", pending, done, events, audited, anchors)
			if pending == 0 && done > 0 && audited > 0 && events == audited && anchors >= 1 && done == events+anchors {
				return
			}
		}
		t.Fatalf("after 60 s the audit-sink messages did not all land in the default table: %s\n%s", last, s.log)
	})

	// The workers follow the tenant list (ST-SUP-RC35, ST-SUP-RC27-OC12): at each tick, every 10 s, the worker
	// sync-partner-orders adds one to the qty of the seeded order syncedOrder in each tenant of the list whose ID
	// starts with org-w, and the worker may sync that order only by the grant that seed writes.
	const syncedOrder = "5b7c2f0e-3a1d-4c8e-9f60-1d2e3f4a5b6c"
	ticks := func(t *testing.T, tenantID string) int {
		return count(t, s.admin, `SELECT coalesce(sum(qty), 0) FROM orders WHERE tenant_id = $1 AND id = $2`, tenantID, syncedOrder)
	}
	seed := func(t *testing.T, tenantID string) {
		t.Helper()
		_, err := s.admin.Exec(`INSERT INTO orders (tenant_id, id, sku, qty) VALUES ($1, $2, 'synced', 0)`, tenantID, syncedOrder)
		must(t, err)
		resourceID, err := authz.ResourceID(s.as("u-seed", tenantID).tenant, syncedOrder)
		must(t, err)
		_, err = v1.NewPermissionsServiceClient(s.spice).WriteRelationships(s.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: "order", ObjectId: resourceID}, Relation: "syncer",
				Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "service_account", ObjectId: "sync-partner-orders"}}}}}})
		must(t, err)
	}
	kept, retired := "org-wa-"+short(), "org-wr-"+short()
	t.Run("ST-SUP-RC35-OC02-T02 A generated service runs one runner for each declared worker in the worker mode", func(t *testing.T) {
		s.t = t
		s.provisioned(t, kept)
		s.provisioned(t, retired)
		seed(t, kept)
		seed(t, retired)
		s.eventually(t, "the ticker of sync-partner-orders runs for each provisioned tenant", func() bool {
			return ticks(t, kept) >= 1 && ticks(t, retired) >= 1
		})
		// A single worker runs under its lease, one instance at a time.
		if n := count(t, s.admin, `SELECT count(*) FROM leases WHERE name = 'stego.worker.sync-partner-orders'`); n != 1 {
			t.Errorf("%d rows of the lease of sync-partner-orders, want one", n)
		}
		// The consumer apply-note-replies is the one handler of note-replies, and runs as the worker role, which holds
		// SELECT, INSERT and UPDATE on idempotency_keys (ST-SUP-Q136): each message that add-note staged is done.
		for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(200 * time.Millisecond) {
			if count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE destination = 'note-replies' AND state = 'done'`) >= 1 &&
				count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE destination = 'note-replies' AND state <> 'done'`) == 0 {
				break
			}
			if time.Now().After(deadline) {
				var states string
				must(t, s.admin.QueryRow(`SELECT coalesce(string_agg(state || ' ' || attempts || ' ' || failure_code || ' ' || origin, '; '), '')
 FROM stego_outbox.messages WHERE destination = 'note-replies'`).Scan(&states))
				t.Fatalf("after 60 s the consumer did not run each message of note-replies: %s\n%s", states, s.log)
			}
		}
	})
	t.Run("ST-SUP-RC35-OC06-T04 A consuming worker that writes commits as the worker role, which may write nothing it did not declare", func(t *testing.T) {
		s.t = t
		writer := s.as("u-"+uuid.NewString()[:8], kept)
		created := s.send(&writer, http.MethodPost, "/orders", "k-"+uuid.NewString(), `{"sku":"consumed","qty":1}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		id := created.member(t, "id")
		s.own(writer, id)
		s.canEdit(t, writer, id)
		// The decisions of the worker may see the grant below some time after it is written: the fill fails the
		// message with the text consume until it may edit the order, so the outbox tries it again.
		// The worker may sync the order, its declared permission, by the relation syncer.
		resourceID, err := authz.ResourceID(writer.tenant, id)
		must(t, err)
		_, err = v1.NewPermissionsServiceClient(s.spice).WriteRelationships(s.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: "order", ObjectId: resourceID}, Relation: "syncer",
				Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "service_account", ObjectId: "apply-note-replies"}}}}}})
		must(t, err)
		// The decision of the command may see the owner tuple some time after canEdit does: a denied note writes
		// nothing and stages nothing, so the check sends it again until it passes.
		s.eventually(t, "the note of the owner passes", func() bool {
			return s.send(&writer, http.MethodPost, "/orders/"+id+"/notes", "k-"+uuid.NewString(), `{"text":"consume"}`).status/100 == 2
		})
		// The consumer adds 1000 to the qty in the transaction of the outbox worker, a session of the worker role.
		for deadline := time.Now().Add(60 * time.Second); count(t, s.admin, `SELECT coalesce(sum(qty), 0) FROM orders WHERE tenant_id = $1 AND id = $2`, kept, id) != 1001; time.Sleep(200 * time.Millisecond) {
			if time.Now().After(deadline) {
				var states string
				must(t, s.admin.QueryRow(`SELECT coalesce(string_agg(state || ' ' || attempts || ' ' || coalesce(failure_code, ''), '; '), '')
 FROM stego_outbox.messages WHERE destination = 'note-replies' AND tenant_id = $1`, kept).Scan(&states))
				t.Fatalf("after 60 s the consumer did not write the order (qty %d); the messages of note-replies: %s", count(t, s.admin, `SELECT coalesce(sum(qty), 0) FROM orders WHERE id = $1`, id), states)
			}
		}
		if n := count(t, s.admin, `SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND resource_id = $2 AND actor_id = 'service_account:apply-note-replies'`, kept, id); n != 1 {
			t.Errorf("%d audit rows of the consumer's write, want 1", n)
		}
		// The worker role holds the least grants of the declared writes: no DELETE on orders, and nothing on notes,
		// which no consumer writes.
		for _, c := range []struct {
			table, privilege string
			want             bool
		}{
			{"public.orders", "UPDATE", true}, {"public.orders", "INSERT", true}, {"public.orders", "DELETE", false},
			{"public.notes", "SELECT", false}, {"public.notes", "INSERT", false}, {"public.idempotency_keys", "INSERT", true},
		} {
			var has bool
			must(t, s.admin.QueryRow(`SELECT has_table_privilege($1, $2, $3)`, stegoruntime.WorkerRole, c.table, c.privilege).Scan(&has))
			if has != c.want {
				t.Errorf("the worker role has %s on %s: %t, want %t", c.privilege, c.table, has, c.want)
			}
		}
	})
	t.Run("ST-SUP-RC27-OC12-T05 A retired tenant gets no worker run from the next tick, and its rows stay", func(t *testing.T) {
		s.t = t
		if got := s.retire(&s.operator, retired, "k-"+uuid.NewString()); got.status != http.StatusOK {
			t.Fatalf("the retire of %s answered %d %s, want 200", retired, got.status, got.body)
		}
		// A run that read the list before the retire may still commit; from the next tick none runs.
		base := ticks(t, kept)
		s.eventually(t, "the next tick runs for the kept tenant", func() bool { return ticks(t, kept) >= base+1 })
		stopped := ticks(t, retired)
		s.eventually(t, "two more ticks run for the kept tenant", func() bool { return ticks(t, kept) >= base+3 })
		if n := ticks(t, retired); n != stopped || n == 0 {
			t.Errorf("the seeded order of the retired tenant holds the qty %d, want its qty %d kept and no new tick", n, stopped)
		}
	})

	t.Run("ST-SUP-RC33-OC02-T04 A declared query reaches its fill through the generated main.go", func(t *testing.T) {
		s.t = t
		if order == "" {
			t.Fatal("no order: the collection check did not pass")
		}
		got := s.send(&owner, http.MethodGet, "/orders/"+order+"/summary", "", "")
		if got.status != http.StatusOK || !bytes.Contains(got.body, []byte(`"a1"`)) {
			t.Fatalf("the query answered %d %s, want 200 with the sku a1", got.status, got.body)
		}
		other := s.as("u-"+uuid.NewString()[:8], owner.tenant.ID())
		if got := s.send(&other, http.MethodGet, "/orders/"+order+"/summary", "", ""); got.status != http.StatusNotFound {
			t.Errorf("the query of a caller who may not view the order answered %d %s, want 404", got.status, got.body)
		}
	})

	t.Run("ST-SUP-RC18-OC31-T02 The generated worker sends to Kafka with the settings of the process, and does not start without them", func(t *testing.T) {
		s.t = t
		// The run with no Kafka setting has the environment of the service of the check, which runs, without the
		// STEGO_KAFKA_ settings alone: its worker fails to start, and the failure record names the stage and the task,
		// with no error text (ST-SUP-RP14-LC05).
		if s.noKafka.err == nil || !strings.Contains(s.noKafka.output, `"stage":"service.run"`) || !strings.Contains(s.noKafka.output, `"stego-runtime[0]"`) {
			t.Fatalf("the worker mode with no Kafka setting gave %v, want a failed start of the runtime task:\n%s", s.noKafka.err, s.noKafka.output)
		}
		// The service of the check runs with the settings: its worker registered the destination kafka, and the
		// change event of the first create is done.
		if order == "" {
			t.Fatal("no order: the collection check did not pass")
		}
		s.eventDone(t, order, "wiring-shop.order.created")
	})

	t.Run("ST-SUP-RC20-OC40-T02 A change event of a generated write arrives once with its key and headers, waits out a broker outage in the outbox, and keeps the order of its entity", func(t *testing.T) {
		s.t = t
		created := s.send(&owner, http.MethodPost, "/orders", "k-"+uuid.NewString(), `{"sku":"k1","qty":1}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		id := created.member(t, "id")
		s.own(owner, id)
		s.canEdit(t, owner, id)
		first := s.eventDone(t, id, "wiring-shop.order.created")
		records := s.records(t, id, 1)
		if len(records) != 1 {
			t.Fatalf("the topic holds %d records of the order, want the one event", len(records))
		}
		r := records[0]
		if want := kafkaoutbox.Key(owner.tenant.ID(), first.resourceKey); string(r.Key) != want {
			t.Errorf("the record key is %q, want %q: the tenant, the separator and the resource key of the message", r.Key, want)
		}
		var payload map[string]string
		if err := json.Unmarshal(r.Value, &payload); err != nil || payload["entity"] != "Order" || payload["action"] != "created" || payload["id"] != id || len(payload) != 3 {
			t.Errorf("the record value is %s (%v), want the entity Order, the action created and the ID %s only", r.Value, err, id)
		}
		for name, value := range map[string]string{kafkaoutbox.HeaderEventID: first.id, kafkaoutbox.HeaderEventType: "wiring-shop.order.created",
			kafkaoutbox.HeaderEventAction:    "created",
			kafkaoutbox.HeaderIdempotencyKey: first.outgoing, kafkaoutbox.HeaderTenant: owner.tenant.ID(), kafkaoutbox.HeaderContentType: "application/json"} {
			if got, once := kafkatest.Header(r, name); !once || got != value {
				t.Errorf("the header %s is %q (once: %v), want %q", name, got, once, value)
			}
		}

		// The broker is down: the patch commits, and its event stays in the outbox with no record.
		s.broker.Plain.Down()
		patched := s.send(&owner, http.MethodPatch, "/orders/"+id, "k-"+uuid.NewString(), `{"qty":5}`)
		if patched.status != http.StatusOK {
			s.broker.Plain.Up()
			t.Fatalf("the patch during the outage answered %d %s, want 200", patched.status, patched.body)
		}
		deleted := s.send(&owner, http.MethodDelete, "/orders/"+id, "", "")
		if deleted.status != http.StatusNoContent {
			s.broker.Plain.Up()
			t.Fatalf("the delete during the outage answered %d %s, want 204", deleted.status, deleted.body)
		}
		s.eventually(t, "the event of the patch is tried during the outage", func() bool {
			return count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE destination = 'kafka' AND kind = 'wiring-shop.order.updated'
 AND resource_key = $1 AND state <> 'done' AND attempts > 0`, first.resourceKey) == 1
		})
		pending := count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE destination = 'kafka' AND resource_key = $1 AND state <> 'done'`, first.resourceKey)
		s.broker.Plain.Up()
		if pending != 2 {
			t.Errorf("%d events of the order wait in the outbox during the outage, want the patch and the delete", pending)
		}
		s.eventDone(t, id, "wiring-shop.order.updated")
		s.eventDone(t, id, "wiring-shop.order.deleted")

		// Each event arrives once, all on one partition, in the order of the writes.
		records = s.records(t, id, 3)
		var actions []string
		for _, r := range records {
			var payload map[string]string
			must(t, json.Unmarshal(r.Value, &payload))
			actions = append(actions, payload["action"])
			if r.Partition != records[0].Partition {
				t.Errorf("the events of one order are on the partitions %d and %d", records[0].Partition, r.Partition)
			}
		}
		if got := strings.Join(actions, ","); got != "created,updated,deleted" {
			t.Errorf("the events of the order arrived as %s, want created,updated,deleted, each once", got)
		}
	})

	t.Run("ST-SUP-RC20-OC40-T03 A replay of a write with its idempotency key stages no second event", func(t *testing.T) {
		s.t = t
		key := "k-" + uuid.NewString()
		first := s.send(&owner, http.MethodPost, "/orders", key, `{"sku":"replayed","qty":1}`)
		again := s.send(&owner, http.MethodPost, "/orders", key, `{"sku":"replayed","qty":1}`)
		if first.status != http.StatusCreated || again.status != http.StatusCreated || first.member(t, "id") != again.member(t, "id") {
			t.Fatalf("the create and its replay answered %d %s and %d %s, want 201 twice with one ID", first.status, first.body, again.status, again.body)
		}
		id := first.member(t, "id")
		s.eventDone(t, id, "wiring-shop.order.created")
		if n := count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE destination = 'kafka' AND resource_key LIKE '%:' || $1`, id); n != 1 {
			t.Errorf("the outbox holds %d events of the order, want the one of the create", n)
		}
		if records := s.records(t, id, 1); len(records) != 1 {
			t.Errorf("the topic holds %d records of the order, want one", len(records))
		}
	})

	t.Run("ST-SUP-RC20-OC40-T04 A write that rolls back after it staged its event leaves no outbox message and no record", func(t *testing.T) {
		s.t = t
		// A deferred trigger refuses the commit of an order with the sku rolled-back: the fill wrote the row and
		// staged the event, and the transaction rolls back at its end. The schema manifest check runs only at start,
		// so the trigger, which the test drops again, does not stop the running service.
		execAll(t, s.admin, `CREATE FUNCTION wiring_refuse_at_commit() RETURNS trigger LANGUAGE plpgsql AS $f$
BEGIN RAISE EXCEPTION 'the check refuses this commit'; END $f$`,
			`CREATE CONSTRAINT TRIGGER wiring_refuse_at_commit AFTER INSERT ON orders DEFERRABLE INITIALLY DEFERRED
 FOR EACH ROW WHEN (NEW.sku = 'rolled-back') EXECUTE FUNCTION wiring_refuse_at_commit()`)
		defer execAll(t, s.admin, "DROP TRIGGER wiring_refuse_at_commit ON orders", "DROP FUNCTION wiring_refuse_at_commit()")
		messages := s.kafkaMessages(t)
		before := len(s.broker.Records(t, changeTopic, 0, 10*time.Second, time.Second))
		got := s.send(&owner, http.MethodPost, "/orders", "k-"+uuid.NewString(), `{"sku":"rolled-back","qty":1}`)
		if got.status < http.StatusInternalServerError {
			t.Fatalf("the create whose commit is refused answered %d %s, want a server error", got.status, got.body)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM orders WHERE sku = 'rolled-back'`); n != 0 {
			t.Errorf("the refused create left %d rows", n)
		}
		if n := s.kafkaMessages(t); n != messages {
			t.Errorf("the outbox holds %d events of the destination kafka after the refused create, want the %d of before", n, messages)
		}
		if after := len(s.broker.Records(t, changeTopic, 0, 10*time.Second, 3*time.Second)); after != before {
			t.Errorf("the topic holds %d records after the refused create, want the %d of before", after, before)
		}
	})

	// The tuples of the workers in each tenant (ST-SUP-RC35-OC13, ST-SUP-Q132): Provision writes one tuple for each
	// worker on the tenant resource, the runtime relates each new order to its tenant, and Retire deletes the
	// tuples. These checks run last: each waits on the state of SpiceDB with no deadline of its own, so only the
	// timeout of go test bounds them.
	t.Run("ST-DECL-UC10-R03-T01 A provisioned tenant gives each worker its tuple", func(t *testing.T) {
		s.t = t
		id := "org-c-" + short()
		s.provisioned(t, id)
		resource := s.tenantResource(t, id)
		want := []string{
			"tenant:" + resource + "#worker_apply_note_replies@service_account:apply-note-replies",
			"tenant:" + resource + "#worker_sync_partner_orders@service_account:sync-partner-orders",
		}
		got := s.awaitTuples(t, resource, want)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("SpiceDB holds on the tenant resource of %s:\n%s\nwant each worker's own tuple and no other:\n%s", id, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
	t.Run("ST-SUP-RC35-OC13-T01 Provisioning a tenant writes one tuple for each worker in that tenant only", func(t *testing.T) {
		s.t = t
		first, second := "org-t1-"+short(), "org-t2-"+short()
		s.provisioned(t, first)
		s.provisioned(t, second)
		orderOf := func(tenantID string) (caller, string) {
			c := s.as("u-"+uuid.NewString()[:8], tenantID)
			created := s.send(&c, http.MethodPost, "/orders", "k-"+uuid.NewString(), `{"sku":"tenant","qty":1}`)
			if created.status != http.StatusCreated {
				t.Fatalf("the create in %s answered %d %s, want 201", tenantID, created.status, created.body)
			}
			return c, created.member(t, "id")
		}
		c1, o1 := orderOf(first)
		c2, o2 := orderOf(second)
		r1, err := authz.ResourceID(c1.tenant, o1)
		must(t, err)
		r2, err := authz.ResourceID(c2.tenant, o2)
		must(t, err)
		// The order relates to the tenant resource of its own tenant (ST-SUP-RP10-LC05).
		for _, o := range []struct{ resource, tenantID string }{{r1, first}, {r2, second}} {
			want := []string{"order:" + o.resource + "#tenant@tenant:" + s.tenantResource(t, o.tenantID)}
			if got := s.awaitRelation(t, "order", o.resource, "tenant", want); strings.Join(got, "\n") != want[0] {
				t.Fatalf("the order of %s relates to %v, want %v", o.tenantID, got, want)
			}
		}
		// Each worker may act on the orders of a provisioned tenant by its own relation only: the schema's
		// tenant_sync is tenant->worker_sync_partner_orders and tenant_apply is tenant->worker_apply_note_replies.
		cases := []struct {
			resource, permission, account string
			allowed                       bool
		}{
			{r1, "tenant_sync", "sync-partner-orders", true}, {r1, "tenant_apply", "apply-note-replies", true},
			{r1, "tenant_sync", "apply-note-replies", false}, {r1, "tenant_apply", "sync-partner-orders", false},
			{r2, "tenant_sync", "sync-partner-orders", true}, {r2, "tenant_apply", "apply-note-replies", true},
		}
		for _, c := range cases {
			s.awaitPermission(t, "order", c.resource, c.permission, c.account, c.allowed)
		}
		// Retire deletes the tuples of the retired tenant only: the same account is then denied on the order of the
		// retired tenant and still allowed in the other.
		if got := s.retire(&s.operator, second, "k-"+uuid.NewString()); got.status != http.StatusOK {
			t.Fatalf("the retire of %s answered %d %s, want 200", second, got.status, got.body)
		}
		if got := s.awaitTuples(t, s.tenantResource(t, second), nil); len(got) != 0 {
			t.Fatalf("SpiceDB still holds the tuples of the retired tenant: %v", got)
		}
		s.awaitPermission(t, "order", r2, "tenant_sync", "sync-partner-orders", false)
		s.awaitPermission(t, "order", r2, "tenant_apply", "apply-note-replies", false)
		s.awaitPermission(t, "order", r1, "tenant_sync", "sync-partner-orders", true)
		if got := s.awaitTuples(t, s.tenantResource(t, first), []string{
			"tenant:" + s.tenantResource(t, first) + "#worker_apply_note_replies@service_account:apply-note-replies",
			"tenant:" + s.tenantResource(t, first) + "#worker_sync_partner_orders@service_account:sync-partner-orders",
		}); len(got) != 2 {
			t.Fatalf("the retire of %s changed the tuples of %s: %v", second, first, got)
		}
		// No tuple of the workers has a wildcard subject or resource.
		for _, tenantID := range []string{first, second} {
			for _, line := range s.relations(t, "tenant", s.tenantResource(t, tenantID), "") {
				if strings.Contains(line, "*") {
					t.Fatalf("the tuple %s holds a wildcard", line)
				}
			}
		}
	})
	t.Run("ST-SUP-RC35-OC13-T06 The reconcile at start gives a tenant of an earlier release each worker's tuple and deletes the tuple of a removed worker", func(t *testing.T) {
		s.t = t
		// The tuples of the other active tenants once the relay delivered every message, which the reconcile must
		// not change.
		for polls := 1; ; polls++ {
			pending := count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE destination = 'authz' AND state <> 'done'`)
			if pending == 0 {
				break
			}
			if polls%50 == 0 {
				t.Logf("after %d reads, %d messages for authz are not done", polls, pending)
			}
			time.Sleep(200 * time.Millisecond)
		}
		others := map[string][]string{}
		rows, err := s.admin.Query(`SELECT tenant_id FROM stego_tenants WHERE retired_at IS NULL`)
		must(t, err)
		for rows.Next() {
			var id string
			must(t, rows.Scan(&id))
			others[id] = nil
		}
		must(t, rows.Err())
		rows.Close()
		for id := range others {
			others[id] = s.relations(t, "tenant", s.tenantResource(t, id), "")
		}
		// A tenant of an earlier release: listed with no tuple of the declared workers and with the tuple of the
		// removed worker retired-export. The service starts again, as at the rollout of a release, and the reconcile
		// of the controller mode, which runs at start, mends both. Its ID sorts after the IDs of the other tenants,
		// so the reconcile visits it last.
		preTenant := "zz-pre-" + uuid.NewString()[:8]
		resource := s.tenantResource(t, preTenant)
		restarted := time.Now()
		execAll(t, s.admin, "INSERT INTO stego_tenants (tenant_id) VALUES ('"+preTenant+"')")
		_, err = v1.NewPermissionsServiceClient(s.spice).WriteRelationships(s.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: "tenant", ObjectId: resource},
				Relation: "worker_retired_export",
				Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "service_account", ObjectId: "retired-export"}}}}}})
		must(t, err)
		s.restart(t)
		want := []string{
			"tenant:" + resource + "#worker_apply_note_replies@service_account:apply-note-replies",
			"tenant:" + resource + "#worker_sync_partner_orders@service_account:sync-partner-orders",
		}
		got := s.awaitTuples(t, resource, want)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("SpiceDB holds on the tenant resource of %s:\n%s\nwant each worker's own tuple and no tuple of retired-export:\n%s", preTenant, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		// One audit row in the tenant's own chain, of the actor kind system.
		if n := count(t, s.admin, `SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'reconcile-worker-tuples'
 AND actor_kind = 'system' AND resource_type = 'tenant' AND resource_id = $1 AND outcome = 'ok'`, preTenant); n != 1 {
			t.Fatalf("%d audit rows of the reconcile in %s, want 1", n, preTenant)
		}
		// The reconcile visited the other tenants first and changed none: no tuple write for them since the restart,
		// and SpiceDB holds their tuples as before.
		if n := count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE destination = 'authz' AND tenant_id <> $1 AND created_at >= $2`, preTenant, restarted); n != 0 {
			t.Fatalf("the reconcile staged %d tuple writes for other tenants, want none: their tuples were whole", n)
		}
		for id, before := range others {
			if after := s.relations(t, "tenant", s.tenantResource(t, id), ""); !slices.Equal(after, before) {
				t.Fatalf("the tuples of %s changed in the reconcile:\n%s\nwant:\n%s", id, strings.Join(after, "\n"), strings.Join(before, "\n"))
			}
		}
		// The job worker-tuples is healthy, so the modes become ready; a readiness that names the job fails the check
		// at once. The wait has no deadline of its own.
		for polls := 1; ; polls++ {
			response, err := http.Get(s.base + "/readyz/modes")
			must(t, err)
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
			if strings.Contains(string(body), "worker-tuples") {
				t.Fatalf("the readiness of the modes answered %d %s after the reconcile, want the job worker-tuples healthy", response.StatusCode, body)
			}
			if polls%50 == 0 {
				t.Logf("after %d reads, the readiness of the modes answers %d %s", polls, response.StatusCode, body)
			}
			time.Sleep(200 * time.Millisecond)
		}
	})

	t.Run("ST-GEN-INV26-T The assembler wires each declared mode as a named set in one binary that a repeatable mode flag selects", func(t *testing.T) {
		s.t = t
		if order == "" {
			t.Fatal("no order: the collection check did not pass")
		}
		// A refused start names its stage and the modes, with no error text and no value (ST-GEN-INV16).
		refused := func(what string, r runResult, stage, modes string) {
			t.Helper()
			if r.err == nil || !strings.Contains(r.output, `"stage":"`+stage+`"`) {
				t.Errorf("%s gave %v, want a refused start at the stage %s:\n%s", what, r.err, stage, r.output)
			}
			if modes != "" && !strings.Contains(r.output, `"modes":`+modes) {
				t.Errorf("%s gave a failure record with no modes %s:\n%s", what, modes, r.output)
			}
			if strings.Contains(r.output, "bogus") || strings.Contains(r.output, "postgres://") {
				t.Errorf("%s gave a failure record with an error text or a value:\n%s", what, r.output)
			}
		}
		refused("an unknown mode", s.unknownMode, "modes", "")
		refused("the worker mode with no connection string of the worker role", s.noWorkerURL, "service.start.setting.postgres-adapter.STEGO_WORKER_DATABASE_URL", `["worker"]`)
		refused("the next release with --mode all and no database setting", s.allModes, "service.start.setting.postgres-adapter.DATABASE_URL", `["api","worker","controller"]`)
		refused("the next release with no mode option and no database setting", s.declaredDefault, "service.start.setting.postgres-adapter.DATABASE_URL", `["api","worker","controller"]`)

		workerConnections := func() int {
			return count(t, s.admin, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND usename = $2`, s.dbName, stegoruntime.WorkerRole)
		}
		leaseHeld := func() bool {
			return count(t, s.admin, `SELECT count(*) FROM leases WHERE name = 'stego.controller' AND expires_at > now()`) == 1
		}
		ready := func() bool {
			response, err := http.Get(s.base + "/readyz/modes")
			if err != nil {
				return false
			}
			defer response.Body.Close()
			return response.StatusCode == http.StatusOK
		}
		serves := func(mode string) {
			t.Helper()
			if read := s.send(&owner, http.MethodGet, "/orders/"+order, "", ""); read.status != http.StatusOK {
				t.Errorf("%s: the read answered %d %s, want 200 from the api set", mode, read.status, read.body)
			}
		}
		delivered := func() int {
			return count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE tenant_id = $1 AND destination = 'rest-api.after-slots' AND state = 'done'`, owner.tenant.ID())
		}

		// With no mode option the binary runs all: the api set serves, the worker set holds connections as the worker
		// role, and the controller set holds the controller lease.
		s.eventually(t, "every mode is ready with no mode option", ready)
		serves("no mode option")
		s.eventually(t, "the worker set connects as the worker role with no mode option", func() bool { return workerConnections() > 0 })
		s.eventually(t, "the controller set holds the controller lease with no mode option", leaseHeld)

		// --mode api runs the api set alone: no worker pool is opened and the controller lease is not held.
		s.restartWith(t, "--mode", "api")
		s.eventually(t, "the api mode is ready", ready)
		serves("--mode api")
		s.eventually(t, "no connection as the worker role in the api mode", func() bool { return workerConnections() == 0 })
		s.eventually(t, "no holder of the controller lease in the api mode", func() bool { return !leaseHeld() })

		// --mode api --mode worker runs the two sets: the api set serves and the worker set delivers the after slot
		// of a create; the controller lease stays free.
		s.restartWith(t, "--mode", "api", "--mode", "worker")
		s.eventually(t, "the api and worker modes are ready", ready)
		serves("--mode api --mode worker")
		before := delivered()
		created := s.send(&owner, http.MethodPost, "/orders", "k-"+uuid.NewString(), `{"sku":"m1","qty":1}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		s.eventually(t, "the worker set delivers the after slot of the create", func() bool { return delivered() == before+1 })
		if workerConnections() == 0 {
			t.Error("the worker mode holds no connection as the worker role")
		}
		if leaseHeld() {
			t.Error("the controller lease is held, but the process does not run the controller mode")
		}
		// The process runs all modes again for the cleanup.
		s.restartWith(t)
	})
}

// tenantResource gives the ID of the tenant resource of tenantID, the tenant-qualified form with the tenant ID as
// the key (authz.TenantResource, ST-SUP-Q132).
func (s *service) tenantResource(t *testing.T, tenantID string) string {
	t.Helper()
	resource, err := authz.TenantResource(s.as("u-resource", tenantID).tenant)
	must(t, err)
	return resource.ID
}

// relations reads, fully consistent, the relationships of the resource in SpiceDB, of the relation or of every
// relation when relation is empty, each as type:id#relation@type:id, in order.
func (s *service) relations(t *testing.T, resourceType, resourceID, relation string) []string {
	t.Helper()
	stream, err := v1.NewPermissionsServiceClient(s.spice).ReadRelationships(s.ctx, &v1.ReadRelationshipsRequest{
		Consistency:        &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: &v1.RelationshipFilter{ResourceType: resourceType, OptionalResourceId: resourceID, OptionalRelation: relation}})
	must(t, err)
	var out []string
	for {
		answer, err := stream.Recv()
		if err == io.EOF {
			break
		}
		must(t, err)
		r := answer.GetRelationship()
		out = append(out, r.GetResource().GetObjectType()+":"+r.GetResource().GetObjectId()+"#"+r.GetRelation()+"@"+
			r.GetSubject().GetObject().GetObjectType()+":"+r.GetSubject().GetObject().GetObjectId())
	}
	slices.Sort(out)
	return out
}

// awaitRelation waits until SpiceDB holds exactly want as the relationships of the relation on the resource, which
// the relay delivers after the commit, and gives them. It has no deadline of its own: the timeout of go test ends a
// wait that never ends, and a missing result is not a pass.
func (s *service) awaitRelation(t *testing.T, resourceType, resourceID, relation string, want []string) []string {
	t.Helper()
	for polls := 1; ; polls++ {
		got := s.relations(t, resourceType, resourceID, relation)
		if slices.Equal(got, want) {
			return got
		}
		// A wait that never ends shows in the log what SpiceDB held last.
		if polls%50 == 0 {
			t.Logf("after %d reads, SpiceDB holds on %s:%s#%s:\n%s\nwant:\n%s", polls, resourceType, resourceID, relation,
				strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// awaitTuples waits until SpiceDB holds exactly want on the tenant resource, and gives it.
func (s *service) awaitTuples(t *testing.T, resource string, want []string) []string {
	t.Helper()
	return s.awaitRelation(t, "tenant", resource, "", want)
}

// awaitPermission waits until SpiceDB answers, fully consistent, the permission of the service account on the
// resource as allowed says. It has no deadline of its own.
func (s *service) awaitPermission(t *testing.T, resourceType, resourceID, permission, account string, allowed bool) {
	t.Helper()
	want := v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION
	if allowed {
		want = v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
	}
	for {
		answer, err := v1.NewPermissionsServiceClient(s.spice).CheckPermission(s.ctx, &v1.CheckPermissionRequest{
			Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
			Resource:    &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID}, Permission: permission,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "service_account", ObjectId: account}}})
		must(t, err)
		if answer.GetPermissionship() == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// kafkaMessages counts the outbox messages of the destination kafka.
func (s *service) kafkaMessages(t *testing.T) int {
	return count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE destination = 'kafka'`)
}

// canEdit waits until SpiceDB answers, at its quantized revision, that the caller may edit the order id, as the
// service asks it on a patch or a delete.
func (s *service) canEdit(t *testing.T, c caller, id string) {
	t.Helper()
	resourceID, err := authz.ResourceID(c.tenant, id)
	must(t, err)
	s.eventually(t, "SpiceDB answers that the owner may edit the order", func() bool {
		answer, err := v1.NewPermissionsServiceClient(s.spice).CheckPermission(s.ctx, &v1.CheckPermissionRequest{
			Consistency: &v1.Consistency{Requirement: &v1.Consistency_MinimizeLatency{MinimizeLatency: true}},
			Resource:    &v1.ObjectReference{ObjectType: "order", ObjectId: resourceID}, Permission: "edit",
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject}}})
		return err == nil && answer.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
	})
}

// canProvision waits until SpiceDB answers, at its quantized revision, that the caller may provision on the
// service-level resource service:wiring-shop, as the service asks it on a Provision at the revocation floor.
func (s *service) canProvision(t *testing.T, c caller) {
	t.Helper()
	s.eventually(t, "SpiceDB answers that the operator may provision", func() bool {
		answer, err := v1.NewPermissionsServiceClient(s.spice).CheckPermission(s.ctx, &v1.CheckPermissionRequest{
			Consistency: &v1.Consistency{Requirement: &v1.Consistency_MinimizeLatency{MinimizeLatency: true}},
			Resource:    &v1.ObjectReference{ObjectType: "service", ObjectId: serviceName}, Permission: "provision",
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject}}})
		return err == nil && answer.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
	})
}

// message is one outbox message of the destination kafka.
type message struct{ id, resourceKey, outgoing string }

// eventually waits at most 60 s for ok.
func (s *service) eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("after 60 s: %s\n%s", what, s.log)
}

// eventDone waits until the one change event of the kind for the order id is done, and gives its message.
func (s *service) eventDone(t *testing.T, id, kind string) message {
	t.Helper()
	s.eventually(t, "the event "+kind+" of the order "+id+" is done", func() bool {
		return count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE destination = 'kafka' AND kind = $1
 AND resource_key LIKE '%:' || $2 AND state = 'done'`, kind, id) == 1
	})
	var m message
	must(t, s.admin.QueryRow(`SELECT id::text, resource_key, outgoing_key FROM stego_outbox.messages WHERE destination = 'kafka'
 AND kind = $1 AND resource_key LIKE '%:' || $2`, kind, id).Scan(&m.id, &m.resourceKey, &m.outgoing))
	return m
}

// records reads the records of the change topic whose payload names the order id: at least least of them, and then
// what more arrives in the next three seconds.
func (s *service) records(t *testing.T, id string, least int) []*kgo.Record {
	t.Helper()
	var mine []*kgo.Record
	for _, r := range s.broker.Records(t, changeTopic, 0, 10*time.Second, 3*time.Second) {
		if bytes.Contains(r.Value, []byte(`"id":"`+id+`"`)) {
			mine = append(mine, r)
		}
	}
	if len(mine) < least {
		t.Fatalf("the topic %s holds %d records of the order %s, want at least %d", changeTopic, len(mine), id, least)
	}
	return mine
}
