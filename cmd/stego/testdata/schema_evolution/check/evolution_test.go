// Package evolutioncheck runs the releases of ST-GEN-TD04 on one database, as an operator runs them in a rolling
// deploy: the migration job of v1, the rows of the data set, then the migration job of v2, which runs the expand
// steps and the backfill under its lease, while the api replicas of v1 and v2 serve. It runs the service binaries of
// STEGO_TD04_V1 and STEGO_TD04_V2 against the local PostgreSQL server of STEGO_TEST_POSTGRES_DSN and the local
// SpiceDB container of STEGO_TEST_SPICEDB, and calls the collections over HTTP.
package evolutioncheck

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"syscall"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/tenant"

	"github.com/example/service/out/internal/stegoruntime"
	"github.com/example/service/out/internal/tenancy"
)

const (
	issuer   = "https://issuer.example.test/realms/users"
	audience = "user-management"
	password = "td04-check"
	base     = "/api/user-mgmt/v1"
	change   = "user-email-rename"
	leaseOf  = "stego.backfill." + change
	// ttl is the default lease TTL of the backfill (lease.DefaultTTL), and renew its renew interval.
	ttl   = 15 * time.Second
	renew = 5 * time.Second
)

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func writeFile(t *testing.T, name string, data []byte) string {
	file := filepath.Join(t.TempDir(), name)
	must(t, os.WriteFile(file, data, 0o600))
	return file
}

// buffer is the output of a process, which the test reads while the process writes it.
type buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// database is one database of ST-GEN-TD04 after the migration job of v1 and the load of its rows.
type database struct {
	t     *testing.T
	admin *sql.DB
	spice *grpc.ClientConn
	key   *authntest.Key
	env   []string
	as    func(role string) string
}

var roles = []string{stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole, stegoruntime.OwnerRole}

// prepared makes a database of its own with the roles of the tenancy component, runs the migration job of v1 on
// it as the owner role, and loads the rows of seed.sql (ST-GEN-TD04 prepare).
func prepared(t *testing.T) *database { return preparedWith(t, true) }

// preparedWith is prepared, with the rows of seed.sql only when seed is true: the check of the relate step makes its
// own rows, so it does not load the 10,000 users of the data set.
func preparedWith(t *testing.T, seed bool) *database {
	dsn, endpoint := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_TEST_SPICEDB")
	if dsn == "" || endpoint == "" || os.Getenv("STEGO_TD04_V1") == "" || os.Getenv("STEGO_TD04_V2") == "" || os.Getenv("STEGO_TD04_V3") == "" || os.Getenv("STEGO_TD04_SEED") == "" {
		t.Fatal("the check needs STEGO_TEST_POSTGRES_DSN, STEGO_TEST_SPICEDB, STEGO_TD04_V1, STEGO_TD04_V2, STEGO_TD04_V3 and STEGO_TD04_SEED: a missing result is not a pass")
	}
	server, err := url.Parse(dsn)
	must(t, err)
	root, err := sql.Open("pgx", dsn)
	must(t, err)
	t.Cleanup(func() { root.Close() })
	t.Cleanup(func() {
		for _, role := range roles {
			_, _ = root.Exec("DROP OWNED BY " + quote(role) + " CASCADE")
			_, _ = root.Exec("DROP ROLE IF EXISTS " + quote(role))
		}
	})
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "td04_" + hex.EncodeToString(suffix)
	_, err = root.Exec("CREATE DATABASE " + name)
	must(t, err)
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)") })
	u := *server
	u.Path = "/" + name
	d := &database{t: t}
	d.admin, err = sql.Open("pgx", u.String())
	must(t, err)
	t.Cleanup(func() { d.admin.Close() })
	_, err = d.admin.Exec(tenancy.Roles)
	must(t, err)
	for _, role := range roles {
		_, err = d.admin.Exec("ALTER ROLE " + quote(role) + " LOGIN PASSWORD '" + password + "'")
		must(t, err)
	}
	d.as = func(role string) string {
		v := u
		v.User = url.UserPassword(role, password)
		q := v.Query()
		q.Set("sslmode", "disable")
		v.RawQuery = q.Encode()
		return v.String()
	}
	spiceKey := os.Getenv("STEGO_TEST_SPICEDB_KEY")
	d.spice, err = grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(bearer(spiceKey)))
	must(t, err)
	t.Cleanup(func() { d.spice.Close() })
	d.env = []string{"STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK=1", "STEGO_SPICEDB_ENDPOINT=" + endpoint,
		"STEGO_SPICEDB_KEY_FILE=" + writeFile(t, "spicedb.key", []byte(spiceKey)), "STEGO_SPICEDB_INSECURE_LOOPBACK=1"}
	d.key = authntest.NewKey(t, "k1")
	if j := d.migrate("v1"); j.wait(time.Minute) != nil {
		t.Fatalf("the migration job of v1 failed:\n%s", j.out)
	}
	if !seed {
		_, err = d.admin.Exec(`INSERT INTO public.stego_tenants (tenant_id) VALUES ('t-00')`)
		must(t, err)
		return d
	}
	rows, err := os.ReadFile(os.Getenv("STEGO_TD04_SEED"))
	must(t, err)
	_, err = d.admin.Exec(string(rows))
	must(t, err)
	return d
}

// binary gives the service binary of the release.
func binary(release string) string { return os.Getenv("STEGO_TD04_" + strings.ToUpper(release)) }

// job is one run of a migration job.
type job struct {
	cmd    *exec.Cmd
	out    *buffer
	done   chan struct{}
	err    error
	holder string
}

// migrate starts the migration job of the release as the owner role, with the probe console in JSON.
func (d *database) migrate(release string) *job {
	t := d.t
	j := &job{out: &buffer{}, done: make(chan struct{})}
	j.cmd = exec.Command(binary(release), "migrate")
	j.cmd.Env = append(append(os.Environ(), d.env...), "STEGO_MIGRATION_DATABASE_URL="+d.as(stegoruntime.OwnerRole), "STEGO_PROBE_CONSOLE=json")
	j.cmd.Stdout, j.cmd.Stderr = j.out, j.out
	must(t, j.cmd.Start())
	go func() { j.err = j.cmd.Wait(); close(j.done) }()
	t.Cleanup(func() {
		if t.Failed() {
			holder, token := d.leaseRow()
			out := j.out.String()
			if len(out) > 4000 {
				out = out[len(out)-4000:]
			}
			var activity []string
			if rows, err := d.admin.Query(`SELECT usename || ' ' || state || ' ' || COALESCE(wait_event_type, '') || ':' || COALESCE(wait_event, '') || ' ' || pg_catalog.pg_blocking_pids(pid)::text || ' ' || (now() - query_start)::text || ' ' || pg_catalog.left(query, 120)
FROM pg_catalog.pg_stat_activity WHERE datname = pg_catalog.current_database() AND pid <> pg_catalog.pg_backend_pid()`); err == nil {
				for rows.Next() {
					var a string
					_ = rows.Scan(&a)
					activity = append(activity, a)
				}
				rows.Close()
			}
			t.Logf("migration job of %s (running %v): checkpoint %+v, lease %s %d, sessions:\n%s\noutput:\n%s", release, j.running(), d.checkpoint(), holder, token, strings.Join(activity, "\n"), out)
		}
		_ = j.cmd.Process.Signal(syscall.SIGCONT)
		_ = j.cmd.Process.Kill()
		<-j.done
	})
	return j
}

// wait waits for the job to end within the time and gives its error.
func (j *job) wait(within time.Duration) error {
	select {
	case <-j.done:
		return j.err
	case <-time.After(within):
		return fmt.Errorf("the migration job did not end within %v", within)
	}
}

func (j *job) running() bool {
	select {
	case <-j.done:
		return false
	default:
		return true
	}
}

// api is one api replica of a release.
type api struct {
	base, release string
	log           *buffer
	stop          func()
}

// freePort gives a free port of the loopback address.
func freePort(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer listener.Close()
	return fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
}

// serve starts an api replica of the release on the database until it answers /livez.
func (d *database) serve(release string) *api { return d.start(release, "api") }

// start starts a process of the release in the mode on the database until it answers /livez.
func (d *database) start(release, mode string) *api {
	t := d.t
	port := freePort(t)
	cursor := make([]byte, 32)
	_, _ = rand.Read(cursor)
	a := &api{base: "http://127.0.0.1:" + port, release: release, log: &buffer{}}
	process := exec.Command(binary(release))
	process.Env = append(append(os.Environ(), d.env...), "STEGO_MODE="+mode, "PORT="+port, "DATABASE_URL="+d.as(stegoruntime.RuntimeRole),
		"STEGO_WORKER_DATABASE_URL="+d.as(stegoruntime.WorkerRole), "STEGO_RELAY_DATABASE_URL="+d.as(stegoruntime.RelayRole),
		"STEGO_AUTHN_KEYS_FILE="+writeFile(t, "keys.json", authntest.Document(d.key.JWK())),
		"STEGO_AUTHN_ISSUER="+issuer, "STEGO_AUTHN_AUDIENCE="+audience,
		"STEGO_QUERY_CURSOR_KEY_FILE="+writeFile(t, "cursor.key", cursor), "STEGO_SERVICE_VERSION="+release)
	if mode == "worker" {
		// The worker of the example also produces to Kafka; the check gives it a broker that does not answer, as its
		// relay of the destination authz does not need one.
		process.Env = append(process.Env, "STEGO_KAFKA_TOPIC=td04", "STEGO_KAFKA_BROKERS=127.0.0.1:1", "STEGO_KAFKA_INSECURE_LOOPBACK=1")
	}
	process.Stdout, process.Stderr = a.log, a.log
	must(t, process.Start())
	exited := make(chan struct{})
	go func() { _ = process.Wait(); close(exited) }()
	var once sync.Once
	a.stop = func() {
		once.Do(func() {
			_ = process.Process.Signal(os.Interrupt)
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				_ = process.Process.Kill()
			}
		})
	}
	t.Cleanup(func() {
		a.stop()
		if t.Failed() {
			t.Logf("%s api log:\n%s", release, a.log)
		}
	})
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if a.live() {
			return a
		}
		select {
		case <-exited:
			t.Fatalf("the %s api replica stopped before it answered /livez:\n%s", release, a.log)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the %s api replica does not answer /livez:\n%s", release, a.log)
		}
	}
}

func (a *api) live() bool {
	response, err := http.Get(a.base + "/livez")
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode == http.StatusOK
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

func (d *database) caller(subject, tenantID string, roles ...string) caller {
	t := d.t
	claims := authntest.Claims(issuer, audience, subject, time.Now())
	claims["tenant"] = tenantID
	if len(roles) > 0 {
		claims["roles"] = roles
	}
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	token := authntest.Mint(t, authntest.Header("k1"), claims, d.key)
	keys, err := authn.ParseKeys(authntest.Document(d.key.JWK()))
	must(t, err)
	verifier, err := authn.New(authn.Settings{Issuer: issuer, Audience: audience, TenantClaim: "tenant", ProblemCode: "CHECK", Keys: keys})
	must(t, err)
	defer verifier.Stop()
	id, err := verifier.Verify(context.Background(), token)
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
func (d *database) relate(c caller, resourceType, id, relation string) {
	resourceID, err := authz.ResourceID(c.tenant, id)
	must(d.t, err)
	_, err = v1.NewPermissionsServiceClient(d.spice).WriteRelationships(context.Background(), &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID}, Relation: relation,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject}}}}}})
	must(d.t, err)
}

// relateParent writes the relationship relation of the resource of the type and the ID in the tenant of the caller
// to the subject of the subject type and ID in the same tenant, as the outbox relay writes it.
func (d *database) relateParent(c caller, resourceType, id, relation, subjectType, subjectID string) {
	resourceID, err := authz.ResourceID(c.tenant, id)
	must(d.t, err)
	subject, err := authz.ResourceID(c.tenant, subjectID)
	must(d.t, err)
	_, err = v1.NewPermissionsServiceClient(d.spice).WriteRelationships(context.Background(), &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID}, Relation: relation,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: subjectType, ObjectId: subject}}}}}})
	must(d.t, err)
}

type reply struct {
	status int
	body   []byte
}

func (r reply) object(t *testing.T) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(r.body, &document); err != nil {
		t.Fatalf("the answer %s is not a JSON object: %v", r.body, err)
	}
	return document
}

// send sends one request as the caller to the api replica, with the token in the header that the example declares.
func (d *database) send(a *api, c caller, method, path, body string) reply {
	t := d.t
	t.Helper()
	// From v2 a user has the required field locale (user-locale-require), so a create or a change of a user through
	// v2 or v3 that names none sends the fill value en, which v1 does not know.
	if a.release != "v1" && (method == http.MethodPost || method == http.MethodPut) && strings.Contains(path, "/users") &&
		strings.HasPrefix(body, "{") && !strings.Contains(body, `"locale"`) {
		body = `{"locale":"en",` + strings.TrimPrefix(body, "{")
	}
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, a.base+base+path, reader)
	must(t, err)
	request.Header.Set("X-Internal-Token", "Bearer "+c.token)
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

func (d *database) count(query string, args ...any) int {
	d.t.Helper()
	var n int
	if err := d.admin.QueryRow(query, args...).Scan(&n); err != nil {
		d.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// checkpoint is the row of the change in stego_schema.backfills.
type checkpoint struct {
	moved, token int64
	completed    sql.NullTime
	present      bool
}

// checkpoint reads the row of the change; before the step migration of v2 makes it, the row is zero.
func (d *database) checkpoint() checkpoint {
	d.t.Helper()
	var c checkpoint
	err := d.admin.QueryRow(`SELECT rows_moved, token, completed_at FROM stego_schema.backfills WHERE change_id = $1`, change).Scan(&c.moved, &c.token, &c.completed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) && !strings.Contains(err.Error(), "SQLSTATE 42P01") && !strings.Contains(err.Error(), "SQLSTATE 3F000") {
		d.t.Fatal(err)
	}
	c.present = err == nil
	return c
}

// held starts the migration job of v2 with its backfill held at a known batch: a gate holds the lease of the backfill
// until the steps of the job end, since a row lock would block their DDL; then a row lock on the first user of the
// tenant holds the batch of that tenant, and the gate opens. It gives the job and the release of the row lock.
func (d *database) held(tenantID string) (*job, func()) {
	jobs, release := d.heldJobs(tenantID, 1)
	return jobs[0], release
}

// heldJobs is held with n migration jobs of v2, which all end their steps before the row lock, since the steps of a
// job that starts later would wait behind the lock and hold the batches of the others in the lock queue.
func (d *database) heldJobs(tenantID string, n int) ([]*job, func()) {
	t := d.t
	_, err := d.admin.Exec(`INSERT INTO public.leases (name, holder, expires_at) VALUES ($1, 'td04-gate', now() + interval '1 hour')`, leaseOf)
	must(t, err)
	// Each job starts when the one before it waits for the lease: the job waits for the lease once its steps end,
	// and its lease session last ran the acquire statement. A runtime step of the job takes an ACCESS EXCLUSIVE lock
	// on the entity tables, so the row lock waits until every job waits for the lease.
	var jobs []*job
	for i := range n {
		jobs = append(jobs, d.migrate("v2"))
		until(t, time.Minute, "the migration job of v2 waits for the lease of the backfill", func() bool {
			return d.count(`SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE datname = pg_catalog.current_database()
  AND query LIKE 'UPDATE public.leases SET holder=%' AND state = 'idle'`) > i
		})
	}
	release := d.lockRow(tenantID)
	t.Cleanup(func() {
		if t.Failed() {
			var locks []string
			rows, err := d.admin.Query(`SELECT l.pid::text || ' ' || l.locktype || ' ' || COALESCE(c.relname, '') || ' ' || l.mode || ' ' || l.granted::text
FROM pg_catalog.pg_locks l LEFT JOIN pg_catalog.pg_class c ON c.oid = l.relation WHERE NOT l.granted OR c.relname = 'users'`)
			if err == nil {
				for rows.Next() {
					var a string
					_ = rows.Scan(&a)
					locks = append(locks, a)
				}
				rows.Close()
			}
			var activity []string
			if rows, err := d.admin.Query(`SELECT pid::text || ' ' || usename || ' ' || state || ' ' || COALESCE(wait_event_type, '') || ':' || COALESCE(wait_event, '') || ' ' || pg_catalog.pg_blocking_pids(pid)::text || ' ' || pg_catalog.left(query, 80)
FROM pg_catalog.pg_stat_activity WHERE datname = pg_catalog.current_database()`); err == nil {
				for rows.Next() {
					var a string
					_ = rows.Scan(&a)
					activity = append(activity, a)
				}
				rows.Close()
			}
			t.Logf("while the row lock holds: locks\n%s\nsessions\n%s", strings.Join(locks, "\n"), strings.Join(activity, "\n"))
		}
	})
	_, err = d.admin.Exec(`UPDATE public.leases SET expires_at = now() - interval '1 second' WHERE name = $1 AND holder = 'td04-gate'`, leaseOf)
	must(t, err)
	return jobs, release
}

// holding gives the job of the two that moves rows, and the other.
func holding(t *testing.T, jobs []*job) (*job, *job) {
	t.Helper()
	var holder, other *job
	until(t, 30*time.Second, "one job reports moved rows", func() bool {
		for i, j := range jobs {
			if len(probeLines(j.out.String(), "schema.backfill.rows")) > 0 {
				holder, other = j, jobs[1-i]
				return true
			}
		}
		return false
	})
	return holder, other
}

// leaseRow is the lease of the backfill: its holder and its token.
func (d *database) leaseRow() (string, int64) {
	var holder string
	var token int64
	if err := d.admin.QueryRow(`SELECT holder, token FROM public.leases WHERE name = $1`, leaseOf).Scan(&holder, &token); err != nil {
		return "", 0
	}
	return holder, token
}

// column is one column of the users table as the catalog holds it.
type column struct {
	typ     string
	notNull bool
}

// catalog reads the columns, the unique indexes and the triggers of the users table.
func (d *database) catalog() (map[string]column, map[string]string, []string) {
	t := d.t
	columns := map[string]column{}
	rows, err := d.admin.Query(`SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attnotnull FROM pg_catalog.pg_attribute a
WHERE a.attrelid = 'public.users'::regclass AND a.attnum > 0 AND NOT a.attisdropped`)
	must(t, err)
	for rows.Next() {
		var n string
		var c column
		must(t, rows.Scan(&n, &c.typ, &c.notNull))
		columns[n] = c
	}
	rows.Close()
	unique := map[string]string{}
	rows, err = d.admin.Query(`SELECT c.relname, pg_catalog.pg_get_indexdef(i.indexrelid) FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
WHERE i.indrelid = 'public.users'::regclass AND i.indisunique AND i.indisvalid AND NOT i.indisprimary`)
	must(t, err)
	for rows.Next() {
		var n, definition string
		must(t, rows.Scan(&n, &definition))
		unique[n] = definition
	}
	rows.Close()
	var triggers []string
	rows, err = d.admin.Query(`SELECT tgname FROM pg_catalog.pg_trigger WHERE tgrelid = 'public.users'::regclass AND NOT tgisinternal ORDER BY 1`)
	must(t, err)
	for rows.Next() {
		var n string
		must(t, rows.Scan(&n))
		triggers = append(triggers, n)
	}
	rows.Close()
	return columns, unique, triggers
}

// lockRow holds a row lock on the first user of the tenant, in key order, until the returned function releases it.
func (d *database) lockRow(tenantID string) func() {
	t := d.t
	tx, err := d.admin.Begin()
	must(t, err)
	if _, err := tx.Exec(`SELECT id FROM public.users WHERE tenant_id = $1 ORDER BY id LIMIT 1 FOR UPDATE`, tenantID); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = tx.Rollback() }) }
	t.Cleanup(release)
	return release
}

// until polls the condition every 100 ms until it holds, or fails the test after the time.
func until(t *testing.T, within time.Duration, what string, condition func() bool) {
	t.Helper()
	for deadline := time.Now().Add(within); !condition(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %v", what, within)
		}
	}
}

// probeLines gives the JSON console lines of the output that name the observation, of the change of the rename or of
// no change.
func probeLines(output, name string) []map[string]any {
	var lines []map[string]any
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, `"`+name+`"`) {
			continue
		}
		var document map[string]any
		if json.Unmarshal([]byte(line), &document) != nil {
			continue
		}
		// The tests of the backfill read the lines of the rename; the backfill of the retype of the same job reports
		// under its own change.
		if fields, _ := document["fields"].(map[string]any); fields != nil && fields["change"] != nil && fields["change"] != change {
			continue
		}
		lines = append(lines, document)
	}
	return lines
}

func TestEvolution(t *testing.T) {
	d := prepared(t)
	owner := d.caller("u-owner", "t-00", "admin")
	org := "00000000-0000-0000-0000-000000000000"
	d.relate(owner, "organization", org, "owner")
	columnsBefore, uniqueBefore, _ := d.catalog()
	v1api := d.serve("v1")
	job := d.migrate("v2")
	jobErr := job.wait(5 * time.Minute)
	columnsAfter, uniqueAfter, triggers := d.catalog()
	v1Live := v1api.live()

	t.Run("ST-GEN-UC12-R02-T01 After the expand migration the old column is as it was and the new one is nullable", func(t *testing.T) {
		d.t = t
		if jobErr != nil {
			t.Fatalf("the migration job of v2 failed: %v\n%s", jobErr, job.out)
		}
		for name, c := range columnsBefore {
			if columnsAfter[name] != c {
				t.Errorf("the column %s was %v before the expand migration and is %v after it", name, c, columnsAfter[name])
			}
		}
		for name, definition := range uniqueBefore {
			if uniqueAfter[name] != definition {
				t.Errorf("the unique index %s was %q before the expand migration and is %q after it", name, definition, uniqueAfter[name])
			}
		}
		if uniqueBefore["users_email_key"] == "" {
			t.Errorf("v1 has no unique index users_email_key: %v", uniqueBefore)
		}
		if c, ok := columnsAfter["contact_email"]; !ok || c.notNull || c.typ != columnsBefore["email"].typ {
			t.Errorf("the new column contact_email is %v (there %v), want nullable and of the type of email", c, ok)
		}
		if !strings.Contains(uniqueAfter["users_contact_email_key"], `(tenant_id, contact_email)`) {
			t.Errorf("contact_email has no valid unique index users_contact_email_key: %v", uniqueAfter)
		}
		if strings.Join(triggers, ",") != "users_contact_email_sync" {
			t.Errorf("the users table has the triggers %v, want the one new trigger users_contact_email_sync", triggers)
		}
		if len(columnsAfter) != len(columnsBefore)+3 {
			t.Errorf("the table has the columns %v after the step, want those of v1 with contact_email, nickname and locale", columnsAfter)
		}
	})

	t.Run("ST-GEN-UC13-R05-T01 The change is complete with its time when no row is left", func(t *testing.T) {
		d.t = t
		c := d.checkpoint()
		if !c.completed.Valid || c.moved != 10000 {
			t.Fatalf("the checkpoint of %s has %d rows moved, completed %v; want 10000 and a time", change, c.moved, c.completed)
		}
		if n := d.count(`SELECT count(*) FROM public.users WHERE contact_email IS NULL OR contact_email <> email`); n != 0 {
			t.Fatalf("%d users have no contact_email equal to email", n)
		}
		// No batch after completion: the job ended, and the checkpoint does not move.
		time.Sleep(2 * time.Second)
		if again := d.checkpoint(); again != c {
			t.Fatalf("the checkpoint moved after completion: %v, then %v", c, again)
		}
	})

	v2api := d.serve("v2")
	t.Run("ST-GEN-UC12-R04-T01 After the rename the API still names the field email", func(t *testing.T) {
		d.t = t
		created := d.send(v2api, owner, http.MethodPost, "/organizations/"+org+"/users", `{"email":"r04@acme.test","display_name":"R","role":"viewer"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("a create through v2 with the field email answered %d %s, want 201", created.status, created.body)
		}
		id, _ := created.object(t)["id"].(string)
		d.relate(owner, "member", id, "owner")
		d.relateParent(owner, "member", id, "organization", "organization", org)
		read := d.send(v2api, owner, http.MethodGet, "/organizations/"+org+"/users/"+id, "").object(t)
		if read["email"] != "r04@acme.test" || read["contact_email"] != nil {
			t.Fatalf("the read through v2 is %v, want email and no contact_email", read)
		}
	})

	t.Run("ST-GEN-UC12-R03-T04 Each release reads what the other writes for a retype and a require at expand, and no value is lost", func(t *testing.T) {
		d.t = t
		if jobErr != nil {
			t.Fatalf("the migration job of v2 failed: %v\n%s", jobErr, job.out)
		}
		// The migration job of v2 converted every OrgSetting row before the rollout, and the require's default gave
		// every user there the fill value with no backfill (decision on ST-GEN-Q22 (a)).
		if n := d.count(`SELECT count(*) FROM public.org_settings WHERE rank_string IS DISTINCT FROM rank::text`); n != 0 {
			t.Errorf("%d OrgSetting rows have no rank_string equal to rank", n)
		}
		if n := d.count(`SELECT count(*) FROM stego_schema.backfills WHERE change_id = 'orgsetting-rank-retype' AND completed_at IS NOT NULL AND rows_moved = 100 AND failed_rows = 0`); n != 1 {
			t.Errorf("the backfill of the retype is not complete with 100 rows moved and none failed")
		}
		if n := d.count(`SELECT count(*) FROM public.users WHERE locale IS DISTINCT FROM 'en'`); n != 0 {
			t.Errorf("%d users do not read the fill value en of the require", n)
		}
		if n := d.count(`SELECT count(*) FROM stego_schema.backfills WHERE change_id = 'user-locale-require'`); n != 0 {
			t.Errorf("the require has a backfill row")
		}
		// A new setting gets the relationship to its organization by hand, as the harness runs no outbox relay.
		put := func(a *api, key, rank string) {
			r := d.send(a, owner, http.MethodPut, "/organizations/"+org+"/orgsettings", `{"key":"`+key+`","value":{},"rank":`+rank+`}`)
			if r.status != http.StatusOK && r.status != http.StatusCreated {
				t.Fatalf("an upsert of %s through %s answered %d %s", key, a.release, r.status, r.body)
			}
			if r.status == http.StatusCreated {
				id, _ := r.object(t)["id"].(string)
				d.relateParent(owner, "org_setting", id, "organization", "organization", org)
			}
		}
		stored := func(key string) (string, string) {
			var old, now sql.NullString
			must(t, d.admin.QueryRow(`SELECT rank::text, rank_string FROM public.org_settings WHERE tenant_id = 't-00' AND key = $1`, key).Scan(&old, &now))
			return old.String, now.String
		}
		// Edge values of int64 through each writer, compared in the database, where no JSON number rounds them.
		// The pattern of the new field takes at most 18 digits, so v2 writes the edges of that range and v1 the edges of
		// int64 too, which v2 reads as a text of 19 digits.
		for i, value := range []string{"-9223372036854775808", "-999999999999999999", "-9007199254740993", "0", "7", "9007199254740993", "999999999999999999", "9223372036854775807"} {
			for _, a := range []*api{v1api, v2api} {
				if a.release == "v2" && len(strings.TrimPrefix(value, "-")) > 18 {
					continue
				}
				key := fmt.Sprintf("r03-%s-%d", a.release, i)
				body := value
				if a.release == "v2" {
					body = `"` + value + `"`
				}
				put(a, key, body)
				if old, now := stored(key); old != value || now != value {
					t.Errorf("the rank %s written through %s is %q in the old column and %q in the new one", value, a.release, old, now)
				}
			}
		}
		// A change through each release of the row that the other wrote.
		put(v2api, "r03-v1-4", `"8"`)
		put(v1api, "r03-v2-4", "9")
		if old, now := stored("r03-v1-4"); old != "8" || now != "8" {
			t.Errorf("a change through v2 of the row of v1 gives %q, %q", old, now)
		}
		if old, now := stored("r03-v2-4"); old != "9" || now != "9" {
			t.Errorf("a change through v1 of the row of v2 gives %q, %q", old, now)
		}
		list := func(a *api) string {
			r := d.send(a, owner, http.MethodGet, "/organizations/"+org+"/orgsettings?pageSize=100", "")
			if r.status != http.StatusOK {
				t.Fatalf("a list through %s answered %d %s", a.release, r.status, r.body)
			}
			return string(r.body)
		}
		if body := list(v1api); !strings.Contains(body, `"rank":8`) {
			t.Errorf("v1 does not read the rank 8 of v2 as a number:\n%s", body)
		}
		if body := list(v2api); !strings.Contains(body, `"rank":"9"`) {
			t.Errorf("v2 does not read the rank 9 of v1 as a string:\n%s", body)
		}
		// A user that v1 creates, which knows no locale, reads the fill value through v2.
		created := d.send(v1api, owner, http.MethodPost, "/organizations/"+org+"/users", `{"email":"r03@acme.test","display_name":"R","role":"viewer"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("a create through v1 answered %d %s", created.status, created.body)
		}
		id, _ := created.object(t)["id"].(string)
		d.relate(owner, "member", id, "owner")
		d.relateParent(owner, "member", id, "organization", "organization", org)
		if read := d.send(v2api, owner, http.MethodGet, "/organizations/"+org+"/users/"+id, "").object(t); read["locale"] != "en" {
			t.Errorf("v2 reads the user of v1 as %v, want the locale en", read)
		}
	})

	t.Run("ST-GEN-UC12-FL-T The email rename starts while v1 and v2 both run on a table with rows", func(t *testing.T) {
		d.t = t
		if jobErr != nil || !v1Live {
			t.Fatalf("the migration job of v2 ended with %v and v1 is live %v, want 0 and live:\n%s", jobErr, v1Live, job.out)
		}
		create := func(a *api, email string) string {
			r := d.send(a, owner, http.MethodPost, "/organizations/"+org+"/users", `{"email":"`+email+`","display_name":"F","role":"viewer"}`)
			if r.status != http.StatusCreated {
				t.Fatalf("a create answered %d %s, want 201", r.status, r.body)
			}
			id, _ := r.object(t)["id"].(string)
			d.relate(owner, "member", id, "owner")
			// The view and the edit of a member need the view of its organization too (ST-SUP-Q156), through the
			// relationship that the outbox relay writes; the harness runs no relay, so it writes it by hand.
			d.relateParent(owner, "member", id, "organization", "organization", org)
			return id
		}
		update := func(a *api, id, email string) {
			r := d.send(a, owner, http.MethodPut, "/organizations/"+org+"/users/"+id, `{"email":"`+email+`","display_name":"F","role":"viewer"}`)
			if r.status != http.StatusOK {
				t.Fatalf("an update answered %d %s, want 200", r.status, r.body)
			}
		}
		read := func(a *api, id string) any {
			return d.send(a, owner, http.MethodGet, "/organizations/"+org+"/users/"+id, "").object(t)["email"]
		}
		equal := func(id, want string) {
			if n := d.count(`SELECT count(*) FROM public.users WHERE id = $1::uuid AND email = $2 AND contact_email = $2`, id, want); n != 1 {
				t.Errorf("the row %s does not hold %s in both email and contact_email", id, want)
			}
		}
		a, b := create(v1api, "a@flow.test"), create(v2api, "b@flow.test")
		equal(a, "a@flow.test")
		equal(b, "b@flow.test")
		update(v2api, a, "a2@flow.test")
		update(v1api, b, "b2@flow.test")
		equal(a, "a2@flow.test")
		equal(b, "b2@flow.test")
		for _, c := range []struct {
			replica *api
			id      string
			want    string
		}{{v1api, a, "a2@flow.test"}, {v2api, a, "a2@flow.test"}, {v1api, b, "b2@flow.test"}, {v2api, b, "b2@flow.test"}} {
			if got := read(c.replica, c.id); got != c.want {
				t.Errorf("a read of %s gives the email %v, want %s", c.id, got, c.want)
			}
		}
	})
}

// The tests of the backfill run the migration job of v2 on a database of their own, each from the migration job of
// v1 and the rows of ST-GEN-TD04, with the default lease TTL of 15 s and batch size of 1,000. A row lock on the first
// user of a tenant holds a job at a known batch: the batches of the tenants before it move, and the batch of that
// tenant waits at the lock timeout and runs again.

func TestBackfillLease(t *testing.T) {
	t.Run("ST-GEN-UC13-R01-T01 Of two migration jobs only the lease holder moves rows", func(t *testing.T) {
		d := prepared(t)
		jobs, release := d.heldJobs("t-03", 2)
		until(t, time.Minute, "the holder moves the rows of t-00 to t-02", func() bool { return d.checkpoint().moved == 3000 })
		first, second := holding(t, jobs)
		holder, _ := d.leaseRow()
		// Both jobs run while the lock holds: the other waits for the lease, and the checkpoint stays.
		for end := time.Now().Add(2 * ttl); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
			if h, _ := d.leaseRow(); h != holder {
				t.Fatalf("the lease went from %s to %s while the holder ran", holder, h)
			}
			if c := d.checkpoint(); c.moved != 3000 {
				t.Fatalf("the checkpoint moved to %d while the batch of t-03 waits for the lock", c.moved)
			}
		}
		release()
		if err := first.wait(time.Minute); err != nil {
			t.Fatalf("the holder: %v\n%s", err, first.out)
		}
		if err := second.wait(2 * ttl); err != nil {
			t.Fatalf("the other job: %v\n%s", err, second.out)
		}
		c := d.checkpoint()
		if !c.completed.Valid || c.moved != 10000 {
			t.Fatalf("the checkpoint has %d rows moved, complete %v; want each of the 10000 rows once", c.moved, c.completed.Valid)
		}
		if lines := probeLines(second.out.String(), "schema.backfill.rows"); len(lines) != 0 {
			t.Fatalf("the other job, which did not hold the lease while rows were left, reports moved rows: %v", lines)
		}
	})

	t.Run("ST-GEN-UC13-R02-T01 The batch of a job whose lease expired fails as fenced", func(t *testing.T) {
		d := prepared(t)
		jobs, release := d.heldJobs("t-03", 2)
		until(t, time.Minute, "the holder moves the rows of t-00 to t-02", func() bool { return d.checkpoint().moved == 3000 })
		first, second := holding(t, jobs)
		firstHolder, _ := d.leaseRow()
		must(t, first.cmd.Process.Signal(syscall.SIGSTOP))
		until(t, 3*ttl+renew, "the other job takes the lease", func() bool { h, _ := d.leaseRow(); return h != "" && h != firstHolder })
		_, takenAt := d.leaseRow()
		release()
		if err := second.wait(time.Minute); err != nil {
			t.Fatalf("the job that took the lease: %v\n%s", err, second.out)
		}
		after := d.checkpoint()
		must(t, first.cmd.Process.Signal(syscall.SIGCONT))
		if err := first.wait(2 * time.Minute); err != nil {
			t.Fatalf("the first job ends with %v after it lost the lease, want it to find the change complete:\n%s", err, first.out)
		}
		final := d.checkpoint()
		if final != after || final.moved != 10000 || !final.completed.Valid || final.token < takenAt {
			t.Fatalf("the checkpoint was %v when the second job ended and is %v after the first one went on; want 10000 rows, complete, with a token of the second job (%d or later), and no change by the first", after, final, takenAt)
		}
		if n := d.count(`SELECT count(*) FROM public.users WHERE contact_email IS DISTINCT FROM email`); n != 0 {
			t.Fatalf("%d users have no contact_email equal to email", n)
		}
	})

	t.Run("ST-GEN-UC13-R03-T01 A batch that meets a locked row times out and leaves the checkpoint where it was", func(t *testing.T) {
		d := prepared(t)
		j, release := d.held("t-01")
		until(t, time.Minute, "the first batch moves the rows of t-00", func() bool { return d.checkpoint().moved == 1000 })
		// Two lock timeouts of 2 s: the batch of t-01 fails at each, and the checkpoint stays.
		for end := time.Now().Add(4 * time.Second); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
			if c := d.checkpoint(); c.moved != 1000 || c.completed.Valid {
				t.Fatalf("the checkpoint moved to %v while the batch waits for the locked row", c)
			}
		}
		release()
		if err := j.wait(time.Minute); err != nil {
			t.Fatalf("the job: %v\n%s", err, j.out)
		}
		if c := d.checkpoint(); c.moved != 10000 || !c.completed.Valid {
			t.Fatalf("after the lock the checkpoint is %v, want 10000 rows and complete", c)
		}
		if n := d.count(`SELECT count(*) FROM public.users WHERE contact_email IS DISTINCT FROM email`); n != 0 {
			t.Fatalf("%d users, the locked one among them, have no contact_email equal to email", n)
		}
	})

	t.Run("ST-GEN-UC13-R04-T01 A backfill that is killed between batches resumes from its checkpoint", func(t *testing.T) {
		d := prepared(t)
		first, release := d.held("t-03")
		until(t, time.Minute, "the job moves 3,000 rows", func() bool { return d.checkpoint().moved == 3000 })
		must(t, first.cmd.Process.Kill())
		_ = first.wait(10 * time.Second)
		release()
		before := d.count(`SELECT count(*) FROM public.users WHERE contact_email IS NOT NULL`)
		second := d.migrate("v2")
		if err := second.wait(2*ttl + time.Minute); err != nil {
			t.Fatalf("the second job: %v\n%s", err, second.out)
		}
		c := d.checkpoint()
		var moved float64
		for _, line := range probeLines(second.out.String(), "schema.backfill.rows") {
			if v, ok := line["value"].(float64); ok {
				moved += v
			}
		}
		if c.moved != 10000 || !c.completed.Valid || before != 3000 || moved != 7000 {
			t.Fatalf("the checkpoint has %d rows moved, complete %v; %d rows were moved before the second job, and it reports %v; want 10000, complete, 3000 and 7000", c.moved, c.completed.Valid, before, moved)
		}
	})
}

func TestBackfillProgress(t *testing.T) {
	t.Run("ST-GEN-UC13-R06-T01 Progress is counts with no row value, and a stall is a probe signal", func(t *testing.T) {
		d := prepared(t)
		j, release := d.held("t-05")
		until(t, time.Minute, "the job moves 5,000 rows", func() bool { return d.checkpoint().moved == 5000 })
		stalledFrom := time.Now()
		until(t, 3*ttl+renew+5*time.Second, "schema.backfill.stalled", func() bool { return len(probeLines(j.out.String(), "schema.backfill.stalled")) > 0 })
		if waited := time.Since(stalledFrom); waited < 3*ttl-renew {
			t.Fatalf("the stall came %v after the last batch, before three lease TTLs", waited)
		}
		// The job keeps running through a stall of four lease TTLs.
		for end := stalledFrom.Add(4 * ttl); time.Now().Before(end); time.Sleep(time.Second) {
			if !j.running() {
				t.Fatalf("the job ended during the stall: %v\n%s", j.err, j.out)
			}
		}
		release()
		if err := j.wait(time.Minute); err != nil {
			t.Fatalf("the job: %v\n%s", err, j.out)
		}
		output := j.out.String()
		if strings.Contains(output, "@seed.test") {
			t.Fatal("the output of the job holds an email value")
		}
		lines := probeLines(output, "schema.backfill.rows")
		if len(lines) == 0 {
			t.Fatalf("the job reports no schema.backfill.rows:\n%s", output)
		}
		var moved float64
		for _, line := range lines {
			if !strings.Contains(fmt.Sprint(line), change) {
				t.Errorf("the line %v does not name the change", line)
			}
			v, _ := line["value"].(float64)
			moved += v
		}
		if moved != 10000 || len(probeLines(output, "schema.backfill.stalled")) != 1 || len(probeLines(output, "schema.backfill.completed")) != 1 {
			t.Fatalf("the job reports %v moved rows, %d stalls and %d completions; want 10000, 1 and 1:\n%s", moved,
				len(probeLines(output, "schema.backfill.stalled")), len(probeLines(output, "schema.backfill.completed")), output)
		}
	})

	t.Run("ST-GEN-UC13-FL-T The email rename backfills 10,000 rows under the lease while v1 and v2 write", func(t *testing.T) {
		d := prepared(t)
		owner := d.caller("u-owner", "t-09", "admin")
		org := "00000000-0000-0000-0000-000000000009"
		d.relate(owner, "organization", org, "owner")
		v1api := d.serve("v1")
		jobs, release := d.heldJobs("t-05", 2)
		first, second := jobs[0], jobs[1]
		until(t, time.Minute, "a job moves 5,000 rows", func() bool { return d.checkpoint().moved == 5000 })
		holder, _ := d.leaseRow()
		// v1 writes while the jobs run: the sync trigger copies each write, and the backfill leaves it.
		for i := range 3 {
			r := d.send(v1api, owner, http.MethodPost, "/organizations/"+org+"/users", fmt.Sprintf(`{"email":"w%d@flow.test","display_name":"W","role":"viewer"}`, i))
			if r.status != http.StatusCreated {
				t.Fatalf("a write of v1 during the backfill answered %d %s", r.status, r.body)
			}
		}
		// The fenced path: the holder stops for longer than its lease, the other job takes the lease and resumes.
		if h, _ := d.leaseRow(); h != holder {
			t.Fatal("the lease changed hands before the stop")
		}
		stopped, _ := holding(t, jobs)
		must(t, stopped.cmd.Process.Signal(syscall.SIGSTOP))
		until(t, 3*ttl+renew, "the other job takes the lease", func() bool { h, _ := d.leaseRow(); return h != "" && h != holder })
		release()
		other := first
		if stopped == first {
			other = second
		}
		if err := other.wait(time.Minute); err != nil {
			t.Fatalf("the job that took the lease: %v\n%s", err, other.out)
		}
		must(t, stopped.cmd.Process.Signal(syscall.SIGCONT))
		if err := stopped.wait(2 * time.Minute); err != nil {
			t.Fatalf("the job that lost the lease: %v\n%s", err, stopped.out)
		}
		c := d.checkpoint()
		if c.moved != 10000 || !c.completed.Valid {
			t.Fatalf("the checkpoint is %v, want each of the 10000 seeded rows moved once and complete", c)
		}
		if n := d.count(`SELECT count(*) FROM public.users WHERE contact_email IS DISTINCT FROM email`); n != 0 {
			t.Fatalf("%d users have no contact_email equal to email", n)
		}
		var moved float64
		completed := 0
		for _, j := range []*job{first, second} {
			out := j.out.String()
			if strings.Contains(out, "@seed.test") || strings.Contains(out, "@flow.test") {
				t.Fatal("the output of a job holds an email value")
			}
			for _, line := range probeLines(out, "schema.backfill.rows") {
				v, _ := line["value"].(float64)
				moved += v
			}
			completed += len(probeLines(out, "schema.backfill.completed"))
		}
		if moved != 10000 || completed != 1 {
			t.Fatalf("the jobs report %v moved rows and %d completions, want 10000 and 1", moved, completed)
		}
	})
}

// The tests of the contract step run the migration job of v3 of ST-GEN-TD04, which drops the old column email and
// the sync trigger of user-email-rename, each on a database of its own after the migration job of v2.

// backfilled runs the migration job of v2 to its end: the expand steps and the whole backfill.
func (d *database) backfilled() {
	d.t.Helper()
	if j := d.migrate("v2"); j.wait(5*time.Minute) != nil {
		d.t.Fatalf("the migration job of v2 failed:\n%s", j.out)
	}
}

// snapshot reads the schema of the users table and the ledger: the columns, the unique indexes, the triggers, the
// constraints with their validation, and the ledger rows.
func (d *database) snapshot() string {
	t := d.t
	t.Helper()
	columns, unique, triggers := d.catalog()
	var lines []string
	for _, query := range []string{`SELECT conname || ' ' || convalidated::text FROM pg_catalog.pg_constraint WHERE conrelid = 'public.users'::regclass ORDER BY 1`,
		`SELECT version || ' ' || digest FROM stego_schema.migrations ORDER BY version COLLATE "C"`} {
		rows, err := d.admin.Query(query)
		must(t, err)
		for rows.Next() {
			var line string
			must(t, rows.Scan(&line))
			lines = append(lines, line)
		}
		rows.Close()
	}
	return fmt.Sprint(columns, unique, triggers, lines)
}

// contractRows counts the ledger rows of the contract step.
func (d *database) contractRows() int {
	return d.count(`SELECT count(*) FROM stego_schema.migrations WHERE version LIKE '%_contract'`)
}

// unsetOne leaves one user with email set and contact_email empty after the backfill, as a session that passes the
// trigger and the completeness CHECK can, and adds the CHECK again NOT VALID.
func (d *database) unsetOne() {
	t := d.t
	tx, err := d.admin.Begin()
	must(t, err)
	defer tx.Rollback()
	for _, statement := range []string{"SET LOCAL session_replication_role = replica",
		`ALTER TABLE public.users DROP CONSTRAINT users_contact_email_complete`,
		`UPDATE public.users SET contact_email = NULL WHERE id = (SELECT id FROM public.users ORDER BY id LIMIT 1)`,
		`ALTER TABLE public.users ADD CONSTRAINT users_contact_email_complete CHECK ((contact_email IS NOT NULL) OR (email IS NULL)) NOT VALID`} {
		_, err := tx.Exec(statement)
		must(t, err)
	}
	must(t, tx.Commit())
}

// refused runs the migration job of v3, which must end non-zero within a minute; it gives its output.
func (d *database) refused(what string) string {
	t := d.t
	t.Helper()
	j := d.migrate("v3")
	if err := j.wait(2 * time.Minute); err == nil {
		t.Fatalf("the migration job of v3 ends with 0 %s, want a refusal:\n%s", what, j.out)
	}
	return j.out.String()
}

func TestContract(t *testing.T) {
	t.Run("ST-GEN-UC15-R01-T01 The contract migration refuses while rows are left and writes no ledger row", func(t *testing.T) {
		d := prepared(t)
		first, release := d.held("t-03")
		until(t, time.Minute, "the job moves 3,000 rows", func() bool { return d.checkpoint().moved == 3000 })
		must(t, first.cmd.Process.Kill())
		_ = first.wait(10 * time.Second)
		release()
		before := d.snapshot()
		out := d.refused("with 7,000 rows left")
		if !strings.Contains(out, change) || !strings.Contains(out, "new one empty: 7000;") || strings.Contains(out, "@seed.test") {
			t.Errorf("the refusal with 7,000 rows left does not name the change and the count, or holds a row value:\n%s", out)
		}
		if after := d.snapshot(); after != before || d.contractRows() != 0 {
			t.Errorf("the refused job changed the schema or the ledger:\n%s\n%s", before, after)
		}
		d.backfilled()
		d.unsetOne()
		before = d.snapshot()
		out = d.refused("with one row left after completion")
		if !strings.Contains(out, change) || !strings.Contains(out, "new one empty: 1;") || !strings.Contains(out, "is not validated") {
			t.Errorf("the refusal with one row left does not name the change, the count and the CHECK:\n%s", out)
		}
		if after := d.snapshot(); after != before || d.contractRows() != 0 {
			t.Errorf("the refused job changed the schema or the ledger:\n%s\n%s", before, after)
		}
	})

	t.Run("ST-GEN-UC15-R02-T01 After contract the old column and the trigger are gone and the contract release does not need them", func(t *testing.T) {
		d := prepared(t)
		owner := d.caller("u-owner", "t-02", "admin")
		org := "00000000-0000-0000-0000-000000000002"
		d.relate(owner, "organization", org, "owner")
		d.backfilled()
		if j := d.migrate("v3"); j.wait(5*time.Minute) != nil {
			t.Fatalf("the migration job of v3 failed:\n%s", j.out)
		}
		columns, unique, triggers := d.catalog()
		if _, ok := columns["email"]; ok || len(triggers) != 0 || !columns["contact_email"].notNull ||
			!strings.Contains(unique["users_contact_email_key"], "(tenant_id, contact_email)") {
			t.Fatalf("after the contract the users table has the columns %v, the unique indexes %v and the triggers %v; want no email, no trigger, contact_email NOT NULL with its unique index",
				columns, unique, triggers)
		}
		// The contract of the retype drops the old column rank and makes rank_string NOT NULL; the
		// contract of the require makes locale NOT NULL and keeps its default (decision on ST-GEN-Q22 (a)).
		if n := d.count(`SELECT count(*) FROM pg_catalog.pg_attribute a WHERE a.attrelid = 'public.org_settings'::regclass AND a.attnum > 0 AND NOT a.attisdropped
  AND (a.attname = 'rank' OR (a.attname = 'rank_string' AND a.attnotnull))`); n != 1 {
			t.Fatalf("after the contract the org_settings table has %d of the columns rank and rank_string NOT NULL, want only the second", n)
		}
		if n := d.count(`SELECT count(*) FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_attrdef f ON f.adrelid = a.attrelid AND f.adnum = a.attnum
  WHERE a.attrelid = 'public.users'::regclass AND a.attname = 'locale' AND a.attnotnull AND pg_catalog.pg_get_expr(f.adbin, f.adrelid) LIKE '''en''%'`); n != 1 {
			t.Fatalf("after the contract the users table has no locale NOT NULL with the default en")
		}
		if n := d.count(`SELECT count(*) FROM public.org_settings WHERE rank_string IS NULL`); n != 0 {
			t.Fatalf("%d OrgSetting rows lost their rank in the contract", n)
		}
		v3api := d.serve("v3")
		created := d.send(v3api, owner, http.MethodPost, "/organizations/"+org+"/users", `{"email":"r02@acme.test","display_name":"R","role":"viewer"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("a create through v3 answered %d %s", created.status, created.body)
		}
		id, _ := created.object(t)["id"].(string)
		d.relate(owner, "member", id, "owner")
		// The view and the edit of a member need the view of its organization too (ST-SUP-Q156); the harness runs
		// no relay.
		d.relateParent(owner, "member", id, "organization", "organization", org)
		if read := d.send(v3api, owner, http.MethodGet, "/organizations/"+org+"/users/"+id, ""); read.status != http.StatusOK || read.object(t)["email"] != "r02@acme.test" {
			t.Fatalf("a read through v3 answered %d %s", read.status, read.body)
		}
		if changed := d.send(v3api, owner, http.MethodPut, "/organizations/"+org+"/users/"+id, `{"email":"r02b@acme.test","display_name":"R","role":"viewer"}`); changed.status != http.StatusOK {
			t.Fatalf("a change through v3 answered %d %s", changed.status, changed.body)
		}
		if list := d.send(v3api, owner, http.MethodGet, "/organizations/"+org+"/users", ""); list.status != http.StatusOK || !strings.Contains(string(list.body), "r02b@acme.test") {
			t.Fatalf("a list through v3 answered %d %s", list.status, list.body)
		}
		if strings.Contains(v3api.log.String(), "does not satisfy the schema manifest") {
			t.Errorf("v3 alerts a schema mismatch after its contract:\n%s", v3api.log)
		}
	})

	t.Run("ST-GEN-UC15-FL-T The email rename finishes while v2 and v3 both run, and v1 can no longer start", func(t *testing.T) {
		d := prepared(t)
		owner := d.caller("u-owner", "t-04", "admin")
		org := "00000000-0000-0000-0000-000000000004"
		d.relate(owner, "organization", org, "owner")
		v1api := d.serve("v1")
		d.backfilled()
		v2api := d.serve("v2")
		// v1 reports itself once the expand migration made the table of the reports; the contract waits for it to
		// stop (decision on finding F8 of the schema-change research, owner, 2026-11-13).
		until(t, 30*time.Second, "v1 reports itself", func() bool {
			return d.count(`SELECT count(*) FROM stego_schema.releases WHERE version = 'v1'`) == 1
		})
		before := d.snapshot()
		out := d.refused("while v1 runs")
		if !strings.Contains(out, change) || !strings.Contains(out, "v1 (1 processes)") || strings.Contains(out, "v2 (") {
			t.Errorf("the refusal while v1 runs does not name the change and v1 alone:\n%s", out)
		}
		if after := d.snapshot(); after != before || d.contractRows() != 0 {
			t.Errorf("the job refused while v1 runs changed the schema or the ledger")
		}
		v1api.stop()
		if n := d.count(`SELECT count(*) FROM stego_schema.releases WHERE version = 'v1'`); n != 0 {
			t.Fatalf("v1 stopped and its report is still there (%d)", n)
		}
		if j := d.migrate("v3"); j.wait(5*time.Minute) != nil {
			t.Fatalf("the migration job of v3 failed while v2 runs:\n%s", j.out)
		}
		v3api := d.serve("v3")
		create := func(a *api, email string) string {
			r := d.send(a, owner, http.MethodPost, "/organizations/"+org+"/users", `{"email":"`+email+`","display_name":"F","role":"viewer"}`)
			if r.status != http.StatusCreated {
				t.Fatalf("a create answered %d %s, want 201", r.status, r.body)
			}
			id, _ := r.object(t)["id"].(string)
			d.relate(owner, "member", id, "owner")
			d.relateParent(owner, "member", id, "organization", "organization", org)
			return id
		}
		read := func(a *api, id string) any {
			return d.send(a, owner, http.MethodGet, "/organizations/"+org+"/users/"+id, "").object(t)["email"]
		}
		a, b := create(v2api, "a@contract.test"), create(v3api, "b@contract.test")
		if got := read(v3api, a); got != "a@contract.test" {
			t.Errorf("v3 reads the write of v2 as %v", got)
		}
		if got := read(v2api, b); got != "b@contract.test" {
			t.Errorf("v2 reads the write of v3 as %v", got)
		}
		// v2 restarts on the contracted schema, and its schema manifest accepts it.
		v2api.stop()
		again := d.serve("v2")
		if got := read(again, b); got != "b@contract.test" {
			t.Errorf("the restarted v2 reads %v", got)
		}
		for name, a := range map[string]*api{"v2": v2api, "restarted v2": again, "v3": v3api} {
			if strings.Contains(a.log.String(), "does not satisfy the schema manifest") {
				t.Errorf("%s alerts a schema mismatch on the contracted schema:\n%s", name, a.log)
			}
		}
		// v1 can no longer start: its migration job refuses the ledger, and an api replica alerts the schema
		// mismatch, as the start of a pod alerts and does not refuse (decision on finding F10, owner, 2026-11-13).
		j := d.migrate("v1")
		if err := j.wait(time.Minute); err == nil || !strings.Contains(j.out.String(), `"stage":"migrate.ledger_check"`) {
			t.Errorf("the migration job of v1 after the contract ends with %v:\n%s\nwant a refusal at its ledger check", err, j.out)
		}
		late := d.serve("v1")
		until(t, 10*time.Second, "v1 alerts the schema mismatch", func() bool {
			return strings.Contains(late.log.String(), "does not satisfy the schema manifest")
		})
	})
}

// TestRelate runs the relate step of v1r, which gives Organization the relation tenant (decision ST-SUP-Q212 (a);
// walkthrough 24, P2-R2), in a rolling deploy from v1: the rows of the data set and the rows that v1 makes before and
// during the rollout must all get the tuple of the relation in SpiceDB, so the auditor of the tenant sees each.
func TestRelate(t *testing.T) {
	t.Run("ST-SUP-RP10-LC58-T03 In a rolling deploy that adds a relation, each row of the release before gets its tuple after the release before stops", func(t *testing.T) {
		if binary("v1r") == "" {
			t.Fatal("the check needs STEGO_TD04_V1R: a missing result is not a pass")
		}
		d := preparedWith(t, false)
		owner, auditor := d.caller("u-owner", "t-00", "admin"), d.caller("u-auditor", "t-00")
		create := func(a *api, name string) string {
			t.Helper()
			r := d.send(a, owner, http.MethodPost, "/organizations", `{"name":"`+name+`"}`)
			if r.status != http.StatusCreated {
				t.Fatalf("a create of %s through %s answered %d %s, want 201", name, a.release, r.status, r.body)
			}
			id, _ := r.object(t)["id"].(string)
			return id
		}
		// An organization with the zero UUID, which the first page of a pass reads too, and two that v1 makes before
		// the release.
		_, err := d.admin.Exec(`INSERT INTO public.organizations (tenant_id, id, name) VALUES ('t-00', '00000000-0000-0000-0000-000000000000', 'org 0')`)
		must(t, err)
		rows := []string{"00000000-0000-0000-0000-000000000000"}
		v1api := d.serve("v1")
		rows = append(rows, create(v1api, "before-1"), create(v1api, "before-2"))
		if j := d.migrate("v1r"); j.wait(time.Minute) != nil {
			t.Fatalf("the migration job of v1r failed:\n%s", j.out)
		}
		// The rollout: v1 still serves and makes two more organizations; the pods of v1r start.
		rows = append(rows, create(v1api, "during-1"), create(v1api, "during-2"))
		v1rapi, worker := d.serve("v1r"), d.start("v1r", "worker")
		_ = worker
		rows = append(rows, create(v1rapi, "after-1"))
		controller := d.start("v1r", "controller")
		passLine := func(log *buffer, field string) bool {
			for _, line := range strings.Split(log.String(), "\n") {
				if strings.Contains(line, "command.relate.pass") && strings.Contains(line, "organization-tenant-relate") && strings.Contains(line, `"stego.`+field+`":true`) {
					return true
				}
			}
			return false
		}
		// While v1 reports itself, the relate job waits.
		for !passLine(controller.log, "waits") {
			time.Sleep(200 * time.Millisecond)
		}
		// The auditor of t-00 sees an organization only through the relation tenant.
		d.relate(auditor, "tenant", "t-00", "auditor")
		sees := func(id string) bool {
			t.Helper()
			resource, err := authz.ResourceID(auditor.tenant, id)
			must(t, err)
			r, err := v1.NewPermissionsServiceClient(d.spice).CheckPermission(context.Background(), &v1.CheckPermissionRequest{
				Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
				Resource:    &v1.ObjectReference{ObjectType: "organization", ObjectId: resource}, Permission: "view",
				Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: auditor.subject}}})
			must(t, err)
			return r.Permissionship == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
		}
		// v1 stops. Its last report and the row of the step are made old, as after their windows of 30 s and 3 minutes,
		// and the controller starts again, which runs the relate job at its start.
		v1api.stop()
		controller.stop()
		_, err = d.admin.Exec(`UPDATE stego_schema.seals SET created_at = created_at - interval '10 minutes' WHERE change_id = 'organization-tenant-relate'`)
		must(t, err)
		_, err = d.admin.Exec(`UPDATE stego_schema.releases SET seen_at = seen_at - interval '1 minute' WHERE version = 'v1'`)
		must(t, err)
		controller = d.start("v1r", "controller")
		for !passLine(controller.log, "completed") {
			time.Sleep(200 * time.Millisecond)
		}
		// The relay of v1r writes the staged tuples; then each row is seen.
		for d.count(`SELECT count(*) FROM stego_outbox.messages WHERE destination = 'authz' AND state <> 'done'`) != 0 {
			time.Sleep(200 * time.Millisecond)
		}
		for _, id := range rows {
			if !sees(id) {
				t.Errorf("the auditor of t-00 does not see the organization %s through the relation tenant", id)
			}
		}
		if n := d.count(`SELECT count(*) FROM audit_event WHERE action = 'backfill-relation'`); n == 0 {
			t.Error("the relate job wrote no audit row")
		}
	})
}
