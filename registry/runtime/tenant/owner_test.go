package tenant

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jsell-rh/stego/registry/runtime/internal/pgtest"
)

// countingServer accepts TCP connections and closes them at once, and counts them.
func countingServer(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var count atomic.Int64
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			count.Add(1)
			_ = conn.Close()
		}
	}()
	return listener.Addr().String(), &count
}

func TestTenantConnectors(t *testing.T) {
	t.Run("ST-SUP-RP04-LC06-T01 The owner connector opens only in the migration job", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		address, count := countingServer(t)
		cfg := Config{InsecureLoopback: true, DSN: "postgres://owner:secret@" + address + "/service?sslmode=disable&connect_timeout=2"}
		for _, mode := range []string{"api", "worker", "controller", "all"} {
			db, err := OpenOwner(ctx, mode, cfg)
			if err == nil {
				_ = db.Close()
				t.Fatalf("the owner connector opened in the mode %s", mode)
			}
			if !strings.Contains(err.Error(), `"`+mode+`"`) {
				t.Errorf("the refusal in the mode %s does not name the mode: %v", mode, err)
			}
		}
		if got := count.Load(); got != 0 {
			t.Fatalf("the refused calls opened %d connections, want none", got)
		}
		// In the migration job the connector connects: to the counting server, which closes the connection, and
		// to the local PostgreSQL server.
		if db, err := OpenOwner(ctx, MigrationJob, cfg); err == nil {
			_ = db.Close()
		}
		if count.Load() == 0 {
			t.Fatal("the owner connector made no connection in the migration job")
		}
		env := newPG(t)
		db, err := OpenOwner(ctx, MigrationJob, Config{InsecureLoopback: true, DSN: env.dsn(env.owner)})
		if err != nil {
			t.Fatalf("the owner connector did not open in the migration job: %v", err)
		}
		_ = db.Close()
	})

	t.Run("ST-SUP-RP04-LC06-T02 The runtime connector refuses a role that owns a table", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		owning := env.role(t, "owning", "")
		adminExec(t, env.db, "CREATE TABLE owned (id int)", "ALTER TABLE owned OWNER TO "+owning)
		db, err := Open(ctx, Config{InsecureLoopback: true, DSN: env.dsn(owning)})
		if err == nil {
			_ = db.Close()
			t.Fatal("the runtime connector started as a role that owns a table")
		}
		if !strings.Contains(err.Error(), owning) || !strings.Contains(err.Error(), "public.owned") {
			t.Fatalf("the startup error does not name the role %s and the table public.owned: %v", owning, err)
		}
	})

	t.Run("ST-SUP-RP04-LC06-T03 The runtime connector refuses a role with BYPASSRLS", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		bypassing := env.role(t, "bypassing", "BYPASSRLS")
		db, err := Open(ctx, Config{InsecureLoopback: true, DSN: env.dsn(bypassing)})
		if err == nil {
			_ = db.Close()
			t.Fatal("the runtime connector started as a role with BYPASSRLS")
		}
		if !strings.Contains(err.Error(), "BYPASSRLS") {
			t.Errorf("the startup error does not name BYPASSRLS: %v", err)
		}
		// A superuser bypasses row-level security as BYPASSRLS does.
		superuser := env.role(t, "superuser", "SUPERUSER")
		if db, err := Open(ctx, Config{InsecureLoopback: true, DSN: env.dsn(superuser)}); err == nil {
			_ = db.Close()
			t.Error("the runtime connector started as a superuser")
		} else if !strings.Contains(err.Error(), "is a superuser") {
			t.Errorf("the startup error for the superuser does not say so: %v", err)
		}
		plain, err := Open(ctx, Config{InsecureLoopback: true, DSN: env.dsn(env.runtime)})
		if err != nil {
			t.Fatalf("the runtime connector did not start as the plain runtime role: %v", err)
		}
		_ = plain.Close()
	})
	t.Run("ST-SUP-RP04-LC06-T04 The runtime connector refuses a role that gets superuser or BYPASSRLS through membership", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		bypassGroup := env.role(t, "bypassgroup", "NOLOGIN BYPASSRLS")
		bypassMember := env.role(t, "bypassmember", "INHERIT")
		superGroup := env.role(t, "supergroup", "NOLOGIN SUPERUSER")
		superMember := env.role(t, "supermember", "INHERIT")
		adminExec(t, env.admin, "GRANT "+bypassGroup+" TO "+bypassMember, "GRANT "+superGroup+" TO "+superMember)
		// A member of the outbox relay role of a service could take it and change every tenant's messages.
		relayMember := env.role(t, "relaymember", "INHERIT")
		relayRole := "shop_" + strings.TrimPrefix(env.name, "tenant_") + "_outbox_relay"
		adminExec(t, env.admin, "CREATE ROLE "+relayRole+" NOLOGIN NOINHERIT", "GRANT "+relayRole+" TO "+relayMember)
		t.Cleanup(func() { _, _ = env.admin.Exec("DROP ROLE IF EXISTS " + relayRole) })
		// A role of another name that owns the relay functions counts as a relay role too: it may run them.
		claimMember := env.role(t, "claimmember", "INHERIT")
		claimOwner := env.role(t, "claimowner", "NOLOGIN NOINHERIT")
		// Every role of the service has USAGE on the schema, as the outbox migration grants it.
		adminExec(t, env.db, "CREATE SCHEMA stego_outbox", "GRANT USAGE ON SCHEMA stego_outbox TO PUBLIC",
			"CREATE FUNCTION stego_outbox.claim(timestamptz, integer, uuid, bigint) RETURNS bigint LANGUAGE sql AS 'SELECT 0::bigint'",
			"REVOKE ALL ON FUNCTION stego_outbox.claim(timestamptz, integer, uuid, bigint) FROM PUBLIC",
			"ALTER FUNCTION stego_outbox.claim(timestamptz, integer, uuid, bigint) OWNER TO "+claimOwner)
		adminExec(t, env.admin, "GRANT "+claimOwner+" TO "+claimMember)
		// A role that may run claim itself is refused for every pool but the worker mode's, and a role whose own name
		// is an outbox relay or purge role name is refused for every pool.
		claimer := env.role(t, "claimer", "INHERIT")
		adminExec(t, env.db, "GRANT EXECUTE ON FUNCTION stego_outbox.claim(timestamptz, integer, uuid, bigint) TO "+claimer)
		tag := strings.TrimPrefix(env.name, "tenant_")
		named := map[string]string{}
		for _, suffix := range []string{"_outbox_relay", "_idempotency_purge"} {
			role := "own_" + tag + suffix
			adminExec(t, env.admin, "CREATE ROLE "+role+" LOGIN NOINHERIT PASSWORD '"+rolePassword+"'")
			t.Cleanup(func() { _, _ = env.admin.Exec("DROP ROLE IF EXISTS " + role) })
			named[role] = "is named as an outbox relay or idempotency purge role"
		}
		t.Cleanup(func() { _, _ = env.db.Exec("DROP SCHEMA IF EXISTS stego_outbox CASCADE") })
		// A member of the idempotency purge role could take it and read or delete every tenant's key rows.
		purgeMember := env.role(t, "purgemember", "INHERIT")
		purgeRole := "shop_" + strings.TrimPrefix(env.name, "tenant_") + "_idempotency_purge"
		adminExec(t, env.admin, "CREATE ROLE "+purgeRole+" NOLOGIN NOINHERIT", "GRANT "+purgeRole+" TO "+purgeMember)
		t.Cleanup(func() { _, _ = env.admin.Exec("DROP ROLE IF EXISTS " + purgeRole) })
		// A role of another name that owns a purge function counts as a purge role too.
		ownerMember := env.role(t, "fnmember", "INHERIT")
		fnOwner := env.role(t, "fnowner", "NOLOGIN NOINHERIT")
		adminExec(t, env.db, "CREATE FUNCTION public.idempotency_keys_purge(timestamptz, integer) RETURNS bigint LANGUAGE sql AS 'SELECT 0::bigint'",
			"ALTER FUNCTION public.idempotency_keys_purge(timestamptz, integer) OWNER TO "+fnOwner)
		adminExec(t, env.admin, "GRANT "+fnOwner+" TO "+ownerMember)
		t.Cleanup(func() {
			_, _ = env.db.Exec("DROP FUNCTION IF EXISTS public.idempotency_keys_purge(timestamptz, integer)")
		})
		for role, want := range map[string]string{
			bypassMember: "holds BYPASSRLS through its membership of " + bypassGroup,
			superMember:  "is a superuser through its membership of " + superGroup,
			relayMember:  "is a member of the outbox relay, idempotency purge or hook roles " + relayRole,
			claimMember:  "is a member of the outbox relay, idempotency purge or hook roles " + claimOwner,
			purgeMember:  "is a member of the outbox relay, idempotency purge or hook roles " + purgeRole,
			ownerMember:  "is a member of the outbox relay, idempotency purge or hook roles " + fnOwner,
		} {
			db, err := Open(ctx, Config{InsecureLoopback: true, DSN: env.dsn(role)})
			if err == nil {
				_ = db.Close()
				t.Errorf("the runtime connector started as the role %s", role)
				continue
			}
			if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), role) {
				t.Errorf("the startup error for %s does not say %q: %v", role, want, err)
			}
		}
		for role, want := range named {
			for _, claims := range []bool{false, true} {
				db, err := Open(ctx, Config{InsecureLoopback: true, DSN: env.dsn(role), Claims: claims})
				if err == nil {
					_ = db.Close()
					t.Errorf("the runtime connector started as the role %s (Claims %v)", role, claims)
				} else if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), role) {
					t.Errorf("the startup error for %s does not say %q: %v", role, want, err)
				}
			}
		}
		if db, err := Open(ctx, Config{InsecureLoopback: true, DSN: env.dsn(claimer)}); err == nil {
			_ = db.Close()
			t.Error("the runtime connector started as a role that may run claim itself")
		} else if !strings.Contains(err.Error(), "may run the outbox relay function stego_outbox.claim") {
			t.Errorf("the startup error for %s does not name claim: %v", claimer, err)
		}
		worker, err := Open(ctx, Config{InsecureLoopback: true, DSN: env.dsn(claimer), Claims: true})
		if err != nil {
			t.Fatalf("the worker mode's pool did not start as a role that may run claim: %v", err)
		}
		_ = worker.Close()
		plain, err := Open(ctx, Config{InsecureLoopback: true, DSN: env.dsn(env.runtime)})
		if err != nil {
			t.Fatalf("the runtime connector did not start as the plain runtime role: %v", err)
		}
		_ = plain.Close()
	})
}

func TestStalePlans(t *testing.T) {
	t.Run("ST-SUP-RP04-LC11-T01 After a stale prepared statement each connection of the pool prepares its statements again", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		adminExec(t, env.ownerDB, "CREATE TABLE plans (id text NOT NULL, mark integer)", "INSERT INTO plans VALUES ('a', 1)",
			"GRANT SELECT ON plans TO "+env.runtime)
		db, err := Open(ctx, Config{InsecureLoopback: true, DSN: env.dsn(env.runtime), Pool: PoolBounds{MaxOpen: 2, MaxIdle: 2}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		// read reads the row on each of the two connections of the pool, which it holds together, and gives the
		// error of each. The statement has a parameter, so the connection prepares it and keeps it in its cache.
		read := func() [2]error {
			var conns [2]*sql.Conn
			var errs [2]error
			for i := range conns {
				c, err := db.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				conns[i] = c
				var mark any
				errs[i] = c.QueryRowContext(ctx, "SELECT id, mark FROM plans WHERE id = $1", "a").Scan(new(string), &mark)
			}
			for _, c := range conns {
				_ = c.Close()
			}
			return errs
		}
		if errs := read(); errs[0] != nil || errs[1] != nil {
			t.Fatalf("the first reads fail: %v", errs)
		}
		// A person drops the column and adds it again with another type: the prepared statement of each
		// connection is stale.
		adminExec(t, env.ownerDB, "ALTER TABLE plans DROP COLUMN mark", "ALTER TABLE plans ADD COLUMN mark text")
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = c.QueryRowContext(ctx, "SELECT id, mark FROM plans WHERE id = $1", "a").Scan(new(string), new(any))
		_ = c.Close()
		if !StalePlan(err) {
			t.Fatalf("the read after the change gives %v, not a stale prepared statement", err)
		}
		// The server function names the error in each language of lc_messages, also when the text is not English.
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || !StalePlan(&pgconn.PgError{Code: "0A000", Message: "translated", Routine: pg.Routine}) {
			t.Fatal("StalePlan does not hold for the error of the server with a message in another language")
		}
		if StalePlan(errors.New("cached plan must not change result type")) || StalePlan(&pgconn.PgError{Code: "0A000", Message: "other"}) {
			t.Fatal("StalePlan holds for an error that is not the refusal of a stale prepared statement")
		}
		ResetStatements()
		if errs := read(); errs[0] != nil || errs[1] != nil {
			t.Fatalf("after ResetStatements a connection of the pool still runs its stale statement: %v", errs)
		}
	})
}

func TestOpened(t *testing.T) {
	t.Run("ST-SUP-RP04-LC08-T01 Opened is true only for an open pool of Open or OpenOwner", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		db, err := Open(ctx, Config{InsecureLoopback: true, DSN: env.dsn(env.runtime)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		owner, err := OpenOwner(ctx, MigrationJob, Config{InsecureLoopback: true, DSN: env.dsn(env.owner)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = owner.Close() })
		plain, err := sql.Open("pgx", env.dsn(env.runtime))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = plain.Close() })
		// A copy of the pool value, made by reflection so that vet does not refuse the copy of its lock.
		copied := reflect.New(reflect.TypeOf(db).Elem())
		copied.Elem().Set(reflect.ValueOf(db).Elem())
		for name, c := range map[string]struct {
			db   *sql.DB
			want bool
		}{
			"the pool of Open":        {db, true},
			"the pool of OpenOwner":   {owner, true},
			"a pool of sql.Open":      {plain, false},
			"a copy of the Open pool": {copied.Interface().(*sql.DB), false},
			"no pool":                 {nil, false},
		} {
			if got := Opened(c.db); got != c.want {
				t.Errorf("Opened of %s is %v, want %v", name, got, c.want)
			}
		}
		closed, err := Open(ctx, Config{InsecureLoopback: true, DSN: env.dsn(env.runtime)})
		if err != nil {
			t.Fatal(err)
		}
		if !Opened(closed) {
			t.Fatal("Opened of a new pool of Open is false")
		}
		_ = closed.Close()
		if Opened(closed) {
			t.Error("Opened of a closed pool of Open is true")
		}

		// Earlier pools are collected; many new pools, of sql.Open and of Open, are checked by concurrent callers
		// with no false positive.
		for i := 0; i < 5; i++ {
			old, err := Open(ctx, Config{InsecureLoopback: true, DSN: env.dsn(env.runtime)})
			if err != nil {
				t.Fatal(err)
			}
			_ = old.Close()
		}
		for i := 0; i < 5; i++ {
			runtime.GC()
		}
		var fresh []*sql.DB
		for i := 0; i < 3; i++ {
			p, err := Open(ctx, Config{InsecureLoopback: true, DSN: env.dsn(env.runtime)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			fresh = append(fresh, p)
		}
		var others []*sql.DB
		for i := 0; i < 200; i++ {
			p, err := sql.Open("pgx", env.dsn(env.runtime))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			others = append(others, p)
		}
		var wrong atomic.Int64
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, p := range others {
					if Opened(p) {
						wrong.Add(1)
					}
				}
				for _, p := range fresh {
					if !Opened(p) {
						wrong.Add(1)
					}
				}
			}()
		}
		wg.Wait()
		if n := wrong.Load(); n != 0 {
			t.Errorf("%d answers of Opened were wrong under concurrent callers", n)
		}
	})
}

// serverTLS makes a CA and a server certificate for localhost and 127.0.0.1, writes the server's certificate and key
// into the data folder of the test server with COPY TO PROGRAM, and turns TLS on with ALTER SYSTEM and a reload. It turns TLS
// off again when the test ends. It gives the pool of the CA.
// reloadReads bounds the reads of serverTLS that wait for the reload by their count, not by a time, so a server that
// does not reload fails the test and does not hang it.
const reloadReads = 100000

func serverTLS(t *testing.T, admin *sql.DB) *x509.CertPool {
	t.Helper()
	ctx := context.Background()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "tenant test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(2),
		Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	var dataDir string
	if err := admin.QueryRowContext(ctx, "SHOW data_directory").Scan(&dataDir); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) {
		t.Helper()
		lines := strings.Split(strings.TrimSpace(text), "\n")
		quoted := make([]string, len(lines))
		for i, l := range lines {
			quoted[i] = "('" + l + "')"
		}
		// The server reads a key only when no group or other may read it, so the file is written with umask 077
		// by COPY TO PROGRAM, which the superuser may run.
		if _, err := admin.ExecContext(ctx, "COPY (SELECT line FROM (VALUES "+strings.Join(quoted, ", ")+
			") AS v(line)) TO PROGRAM 'rm -f "+dataDir+"/"+name+" && umask 077 && cat > "+dataDir+"/"+name+"'"); err != nil {
			t.Fatalf("write %s on the server: %v", name, err)
		}
	}
	write("stego_test_server.crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})))
	write("stego_test_server.key", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	// The reload is asynchronous: the postmaster reads the files and then signals each server process, so a
	// process whose pg_conf_load_time moves past its value before the reload shows that the postmaster, which starts
	// each new connection, runs with TLS on. The test waits for that on one session, with no sleep.
	watch, err := admin.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watch.Close() }()
	var loaded time.Time
	if err := watch.QueryRowContext(ctx, "SELECT pg_catalog.pg_conf_load_time()").Scan(&loaded); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"ALTER SYSTEM SET ssl_cert_file = 'stego_test_server.crt'",
		"ALTER SYSTEM SET ssl_key_file = 'stego_test_server.key'", "ALTER SYSTEM SET ssl = on", "SELECT pg_reload_conf()"} {
		if _, err := admin.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	t.Cleanup(func() {
		for _, statement := range []string{"ALTER SYSTEM RESET ssl", "ALTER SYSTEM RESET ssl_cert_file",
			"ALTER SYSTEM RESET ssl_key_file", "SELECT pg_reload_conf()"} {
			_, _ = admin.ExecContext(context.Background(), statement)
		}
	})
	reloaded := false
	for range reloadReads {
		if err := watch.QueryRowContext(ctx, "SELECT pg_catalog.pg_conf_load_time() > $1", loaded).Scan(&reloaded); err != nil {
			t.Fatal(err)
		}
		if reloaded {
			break
		}
	}
	if !reloaded {
		t.Fatalf("the server did not load its configuration again after %d reads", reloadReads)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return pool
}

func TestOpener(t *testing.T) {
	t.Run("ST-SUP-RP04-LC09-T02 A connection string whose TLS does not verify the server is refused", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		for _, mode := range []string{"disable", "prefer", "require", "allow"} {
			for _, role := range []string{env.runtime, env.owner} {
				u := env.url(role, rolePassword)
				q := u.Query()
				q.Set("sslmode", mode)
				u.RawQuery = q.Encode()
				if db, err := Open(ctx, Config{DSN: u.String()}); !errors.Is(err, errNoVerifiedTLS) {
					if db != nil {
						db.Close()
					}
					t.Errorf("Open of %s with sslmode %s gave %v, want the refusal of TLS that does not verify the server", role, mode, err)
				}
				if db, err := OpenOwner(ctx, MigrationJob, Config{DSN: u.String()}); !errors.Is(err, errNoVerifiedTLS) {
					if db != nil {
						db.Close()
					}
					t.Errorf("OpenOwner of %s with sslmode %s gave %v, want the refusal of TLS that does not verify the server", role, mode, err)
				}
			}
		}
		// The loopback test exception opens a loopback server with no TLS, and only a loopback server.
		plain := env.url(env.runtime, rolePassword)
		q := plain.Query()
		q.Set("sslmode", "disable")
		plain.RawQuery = q.Encode()
		db, err := Open(ctx, Config{DSN: plain.String(), InsecureLoopback: true})
		if err != nil {
			t.Fatalf("Open with the loopback test exception gave %v", err)
		}
		db.Close()
		_, port, err := net.SplitHostPort(plain.Host)
		if err != nil {
			t.Fatal(err)
		}
		for _, host := range []string{"db.example:5432", "localhost:" + port} {
			remote := *plain
			remote.Host = host
			if _, err := Open(ctx, Config{DSN: remote.String(), InsecureLoopback: true}); !errors.Is(err, errNoVerifiedTLS) {
				t.Errorf("Open of the host %s, which is not a loopback IP address, with the exception gave %v, want the refusal", host, err)
			}
		}
		socket := plain.Query()
		socket.Set("host", "/var/run/postgresql")
		viaSocket := *plain
		viaSocket.RawQuery = socket.Encode()
		if _, err := Open(ctx, Config{DSN: viaSocket.String(), InsecureLoopback: true}); !errors.Is(err, errNoVerifiedTLS) {
			t.Errorf("Open of a socket path with the exception gave %v, want the refusal", err)
		}
	})
	t.Run("ST-SUP-RP04-LC09-T01 A pool from the adapter's configuration keeps verified TLS and its bounds", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		roots := serverTLS(t, env.admin)
		// The opener's configuration: the role and database of the test, verified TLS to localhost, no fallback.
		opener := func(role string) *pgx.ConnConfig {
			t.Helper()
			c, err := pgx.ParseConfig(env.url(role, rolePassword).String())
			if err != nil {
				t.Fatal(err)
			}
			c.Fallbacks = nil
			c.TLSConfig = &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}
			return c
		}
		bounds := PoolBounds{MaxOpen: 3, MaxIdle: 2, MaxLifetime: time.Minute, MaxIdleTime: 30 * time.Second}
		check := func(name string, db *sql.DB) {
			t.Helper()
			defer db.Close()
			var ssl bool
			if err := db.QueryRowContext(ctx, "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Scan(&ssl); err != nil || !ssl {
				t.Fatalf("the %s pool does not connect over TLS: %t, %v", name, ssl, err)
			}
			if !Opened(db) {
				t.Fatalf("Opened is false for the %s pool", name)
			}
			if n := db.Stats().MaxOpenConnections; n != bounds.MaxOpen {
				t.Fatalf("the %s pool holds at most %d connections, want %d", name, n, bounds.MaxOpen)
			}
		}
		runtimeDB, err := Open(ctx, Config{Conn: opener(env.runtime), Pool: bounds})
		if err != nil {
			t.Fatalf("Open with the opener's configuration: %v", err)
		}
		check("runtime", runtimeDB)
		ownerDB, err := OpenOwner(ctx, MigrationJob, Config{Conn: opener(env.owner), Pool: bounds})
		if err != nil {
			t.Fatalf("OpenOwner with the opener's configuration: %v", err)
		}
		check("owner", ownerDB)

		setting := opener(env.runtime)
		setting.RuntimeParams["search_path"] = "elsewhere"
		if _, err := Open(ctx, Config{Conn: setting}); err == nil || !strings.Contains(err.Error(), "search_path") {
			t.Fatalf("a configuration with a session setting gave %v, want the guard's refusal", err)
		}
		if err := Guard(Config{Conn: setting}); err == nil {
			t.Fatal("Guard took a configuration with a session setting")
		}

		// A configuration with an after-connect hook can set a session value, and is refused.
		hooked := opener(env.runtime)
		hooked.AfterConnect = func(context.Context, *pgconn.PgConn) error { return nil }
		netHooked := opener(env.runtime)
		netHooked.AfterNetConnect = func(_ context.Context, _ *pgconn.Config, c net.Conn) (net.Conn, error) { return c, nil }
		for name, c := range map[string]*pgx.ConnConfig{"AfterConnect": hooked, "AfterNetConnect": netHooked} {
			if err := Guard(Config{Conn: c}); err == nil {
				t.Errorf("Guard took a configuration with %s", name)
			}
			if _, err := Open(ctx, Config{Conn: c}); err == nil {
				t.Errorf("Open took a configuration with %s", name)
			}
		}

		// A pool without verified TLS is refused.
		plain := opener(env.runtime)
		plain.TLSConfig = nil
		insecure := opener(env.runtime)
		insecure.TLSConfig = &tls.Config{InsecureSkipVerify: true}
		fallback := opener(env.runtime)
		fallback.Fallbacks = []*pgconn.FallbackConfig{{Host: "127.0.0.1", Port: fallback.Port}}
		for name, c := range map[string]*pgx.ConnConfig{"no TLS": plain, "TLS that skips the check": insecure,
			"a fallback without TLS": fallback} {
			if _, err := Open(ctx, Config{Conn: c}); !errors.Is(err, errNoVerifiedTLS) {
				t.Errorf("a configuration with %s gave %v, want a refusal", name, err)
			}
		}
		// The opener's configuration is used as it is: a DSN beside it is refused, so nothing parses one.
		if _, err := Open(ctx, Config{Conn: opener(env.runtime), DSN: env.url(env.runtime, rolePassword).String()}); err == nil {
			t.Fatal("a configuration with both Conn and DSN was accepted")
		}
		// A certificate of another name is not verified.
		wrong := opener(env.runtime)
		wrong.TLSConfig = &tls.Config{RootCAs: roots, ServerName: "elsewhere.example"}
		if _, err := Open(ctx, Config{Conn: wrong}); err == nil {
			t.Fatal("a server certificate for another name was accepted")
		}
	})
}

// keyTable is the database of one test of ValidateForeignKey: as the role owner, the tenant list stego_tenants with
// tenant-a and tenant-b, and the table orders under FORCE ROW LEVEL SECURITY with the policy of a tenant table and
// rows of both tenants, and one row of the tenant secret-orphan, which the list does not hold, unless clean; and the
// foreign key orders_tenant_id_fkey from orders to the list, NOT VALID.
type keyTable struct {
	db    *sql.DB
	owner string
}

const ordersKey = "orders_tenant_id_fkey"

func newKeyTable(t *testing.T, rows int, clean bool, extra ...string) *keyTable {
	t.Helper()
	db, err := sql.Open("pgx", pgtest.Database(t, "tenant_key"))
	if err != nil {
		t.Fatal(err)
	}
	k := &keyTable{db: db, owner: "key_owner_" + randomSuffix(t)}
	t.Cleanup(func() {
		_, _ = db.Exec("DROP OWNED BY " + k.owner)
		_, _ = db.Exec("DROP ROLE " + k.owner)
		_ = db.Close()
	})
	statements := []string{
		"CREATE ROLE " + k.owner + " NOLOGIN NOSUPERUSER NOBYPASSRLS",
		"GRANT CREATE ON SCHEMA public TO " + k.owner,
		"SET ROLE " + k.owner,
		"CREATE TABLE public.stego_tenants (tenant_id text PRIMARY KEY, retired_at timestamptz)",
		"CREATE TABLE public.orders (tenant_id text NOT NULL, id bigint NOT NULL, PRIMARY KEY (tenant_id, id))",
		"ALTER TABLE public.orders ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE public.orders FORCE ROW LEVEL SECURITY",
		"CREATE POLICY stego_tenant ON public.orders USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))",
		"RESET ROLE",
		"INSERT INTO public.stego_tenants VALUES ('tenant-a'), ('tenant-b')",
		"INSERT INTO public.orders SELECT CASE WHEN g % 2 = 0 THEN 'tenant-a' ELSE 'tenant-b' END, g FROM generate_series(1, " + strconv.Itoa(rows) + ") g",
	}
	if !clean {
		statements = append(statements, "INSERT INTO public.orders VALUES ('secret-orphan', -1)")
	}
	statements = append(statements, "SET ROLE "+k.owner,
		"ALTER TABLE ONLY public.orders ADD CONSTRAINT "+ordersKey+" FOREIGN KEY (tenant_id) REFERENCES public.stego_tenants (tenant_id) NOT VALID",
		"RESET ROLE")
	for _, statement := range append(statements, extra...) {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return k
}

// acl gives the grants of orders as relacl gives them.
func (k *keyTable) acl(t *testing.T) string {
	t.Helper()
	var text sql.NullString
	if err := k.db.QueryRow("SELECT relacl::text FROM pg_catalog.pg_class WHERE oid = 'public.orders'::regclass").Scan(&text); err != nil {
		t.Fatal(err)
	}
	return text.String
}

// validated reports whether the key of orders is validated.
func (k *keyTable) validated(t *testing.T) bool {
	t.Helper()
	var validated bool
	if err := k.db.QueryRow("SELECT convalidated FROM pg_catalog.pg_constraint WHERE conname = $1", ordersKey).Scan(&validated); err != nil {
		t.Fatal(err)
	}
	return validated
}

// ownerReads reports whether the owner role has SELECT on orders.
func (k *keyTable) ownerReads(t *testing.T) bool {
	t.Helper()
	var reads bool
	if err := k.db.QueryRow("SELECT pg_catalog.has_table_privilege($1, 'public.orders', 'SELECT')", k.owner).Scan(&reads); err != nil {
		t.Fatal(err)
	}
	return reads
}

// validate runs validate in one transaction as the owner role, with no tenant setting, as the migration job runs a
// step, and commits it when validate passes.
func (k *keyTable) validate(ctx context.Context, validate func(context.Context, *sql.Tx) error) error {
	tx, err := k.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+k.owner); err != nil {
		return err
	}
	if err := validate(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func validateOrders(ctx context.Context, tx *sql.Tx) error {
	return ValidateForeignKey(ctx, tx, `public."orders"`, ordersKey)
}

func TestValidateForeignKey(t *testing.T) {
	t.Run("ST-SUP-RP04-LC10-T01 A row that the key refuses fails the validation under FORCE ROW LEVEL SECURITY, with the grants of the table unchanged", func(t *testing.T) {
		k := newKeyTable(t, 1000, false)
		before := k.acl(t)
		err := k.validate(context.Background(), validateOrders)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
			t.Fatalf("the validation over a row of an unlisted tenant gave %v, want the foreign key violation 23503", err)
		}
		if k.validated(t) {
			t.Error("the key is validated after the refusal")
		}
		if after := k.acl(t); after != before || !k.ownerReads(t) {
			t.Errorf("after the refusal the grants of orders are %q, before %q, and the owner role reads orders %v", after, before, k.ownerReads(t))
		}
	})

	t.Run("ST-SUP-RP04-LC10-T02 A grant of SELECT to PUBLIC fails the validation before it runs, with the grants of the table unchanged", func(t *testing.T) {
		k := newKeyTable(t, 100, false, "GRANT SELECT ON public.orders TO PUBLIC")
		before := k.acl(t)
		err := k.validate(context.Background(), validateOrders)
		if err == nil || !strings.Contains(err.Error(), "reads the columns of the foreign key orders_tenant_id_fkey of public.\"orders\" after its SELECT is revoked") {
			t.Fatalf("the validation with SELECT granted to PUBLIC gave %v, want the failure that the role still reads the table", err)
		}
		if k.validated(t) {
			t.Error("the key is validated past the check of the read")
		}
		if after := k.acl(t); after != before {
			t.Errorf("after the failure the grants of orders are %q, before %q", after, before)
		}
	})

	t.Run("ST-SUP-RP04-LC10-T03 A cancel during the validation leaves the grants of the table as before", func(t *testing.T) {
		k := newKeyTable(t, 100, true)
		before := k.acl(t)
		// A session holds an EXCLUSIVE lock of the tenant list, so the validation waits for its ROW SHARE lock of
		// the list after the revoke.
		hold, err := k.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = hold.Rollback() }()
		if _, err := hold.Exec("LOCK TABLE public.stego_tenants IN EXCLUSIVE MODE"); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- k.validate(ctx, validateOrders) }()
		for until := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			var waits bool
			if err := k.db.QueryRow("SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_locks WHERE relation = 'public.stego_tenants'::regclass AND NOT granted)").Scan(&waits); err != nil {
				t.Fatal(err)
			}
			if waits {
				break
			}
			if time.Now().After(until) {
				t.Fatal("the validation does not wait for the lock of the tenant list")
			}
		}
		cancel()
		if err := <-done; err == nil {
			t.Fatal("the cancelled validation passed")
		}
		if err := hold.Rollback(); err != nil {
			t.Fatal(err)
		}
		if after := k.acl(t); after != before || !k.ownerReads(t) || k.validated(t) {
			t.Errorf("after the cancel the grants of orders are %q, before %q; the owner role reads orders %v; the key is validated %v", after, before, k.ownerReads(t), k.validated(t))
		}
	})

	t.Run("ST-SUP-RP04-LC10-T04 With row_security off, a validation by the one query of PostgreSQL fails with 42501 and does not validate over rows that it does not see", func(t *testing.T) {
		k := newKeyTable(t, 100, false)
		// The validation with no revoke is the one that a PostgreSQL that takes its one query in spite of the revoke
		// would run: row-level security hides the row of secret-orphan from the owner role.
		err := k.validate(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
			return validateForeignKey(ctx, tx, `public."orders"`, ordersKey, false)
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("the validation by the one query gave %v, want the row-level security failure 42501", err)
		}
		if k.validated(t) {
			t.Error("the key is validated over a row that the query did not see")
		}
	})

	t.Run("ST-SUP-RP04-LC10-T05 A validation that passes grants SELECT again and sets row_security back, and a key on a table with no row-level security is validated with no revoke", func(t *testing.T) {
		k := newKeyTable(t, 1000, true)
		before := k.acl(t)
		var setting string
		if err := k.validate(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
			if err := validateOrders(ctx, tx); err != nil {
				return err
			}
			return tx.QueryRowContext(ctx, "SELECT pg_catalog.current_setting('row_security')").Scan(&setting)
		}); err != nil {
			t.Fatal(err)
		}
		if !k.validated(t) || !k.ownerReads(t) || setting != "on" {
			t.Errorf("after the validation the key is validated %v, the owner role reads orders %v, and row_security is %q", k.validated(t), k.ownerReads(t), setting)
		}
		// The table had no grant, so its grants are now written out as the default of its owner: the same privileges.
		var same bool
		if err := k.db.QueryRow("SELECT coalesce(relacl = pg_catalog.acldefault('r', relowner), true) FROM pg_catalog.pg_class WHERE oid = 'public.orders'::regclass").Scan(&same); err != nil || !same {
			t.Errorf("after the validation the grants of orders are %q, before %q, which is not the default of its owner (error %v)", k.acl(t), before, err)
		}
		// A table with no row-level security: the key is validated with its grants left as they are, here none.
		for _, statement := range []string{"SET ROLE " + k.owner,
			"CREATE TABLE public.items (tenant_id text NOT NULL REFERENCES public.stego_tenants (tenant_id))",
			"RESET ROLE", "INSERT INTO public.items VALUES ('tenant-a')", "SET ROLE " + k.owner,
			"ALTER TABLE public.items ADD CONSTRAINT items_list_fkey FOREIGN KEY (tenant_id) REFERENCES public.stego_tenants (tenant_id) NOT VALID",
			"RESET ROLE"} {
			if _, err := k.db.Exec(statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		if err := k.validate(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
			return ValidateForeignKey(ctx, tx, "public.items", "items_list_fkey")
		}); err != nil {
			t.Fatal(err)
		}
		var acl sql.NullString
		var validated bool
		if err := k.db.QueryRow("SELECT c.relacl::text, k.convalidated FROM pg_catalog.pg_class c JOIN pg_catalog.pg_constraint k ON k.conrelid = c.oid WHERE c.oid = 'public.items'::regclass AND k.conname = 'items_list_fkey'").Scan(&acl, &validated); err != nil {
			t.Fatal(err)
		}
		if acl.Valid || !validated {
			t.Errorf("the key of items is validated %v with the grants %q, want it validated and no grant written", validated, acl.String)
		}
	})

	t.Run("ST-SUP-RP04-LC10-T07 A table name with no schema is refused before any statement on the table, with the key not validated and the grants unchanged", func(t *testing.T) {
		k := newKeyTable(t, 100, false)
		before := k.acl(t)
		for _, table := range []string{"orders", `"orders"`} {
			err := k.validate(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, "SELECT pg_catalog.set_config('search_path', 'public', true)"); err != nil {
					return err
				}
				return ValidateForeignKey(ctx, tx, table, ordersKey)
			})
			if err == nil || !strings.Contains(err.Error(), "names no schema") {
				t.Errorf("the validation of %s gave %v, want the refusal of a name with no schema", table, err)
			}
		}
		if k.validated(t) {
			t.Error("the key is validated through a name with no schema")
		}
		if after := k.acl(t); after != before {
			t.Errorf("after the refusals the grants of orders are %q, before %q", after, before)
		}
	})

	t.Run("ST-SUP-RP04-LC10-T06 A key that refers to its own table is checked past row-level security, and leaves no column grant", func(t *testing.T) {
		k := newKeyTable(t, 100, true)
		// Each order of tenant-a and tenant-b refers to the order two before it of its tenant, and order 7 to an
		// order that is not there.
		for _, statement := range []string{"ALTER TABLE public.orders ADD COLUMN parent_id bigint",
			"UPDATE public.orders SET parent_id = id - 2 WHERE id > 2", "UPDATE public.orders SET parent_id = 999999 WHERE id = 7",
			"SET ROLE " + k.owner,
			"ALTER TABLE public.orders ADD CONSTRAINT orders_parent_fkey FOREIGN KEY (tenant_id, parent_id) REFERENCES public.orders (tenant_id, id) NOT VALID",
			"RESET ROLE"} {
			if _, err := k.db.Exec(statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		validateParent := func(ctx context.Context, tx *sql.Tx) error {
			return ValidateForeignKey(ctx, tx, "public.orders", "orders_parent_fkey")
		}
		columns := func() string {
			var text string
			if err := k.db.QueryRow("SELECT coalesce(pg_catalog.string_agg(attname || '=' || attacl::text, ',' ORDER BY attname), '') FROM pg_catalog.pg_attribute WHERE attrelid = 'public.orders'::regclass AND attacl IS NOT NULL").Scan(&text); err != nil {
				t.Fatal(err)
			}
			return text
		}
		before := k.acl(t)
		err := k.validate(context.Background(), validateParent)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
			t.Fatalf("the validation over an order that refers to no order gave %v, want the foreign key violation 23503", err)
		}
		if after := k.acl(t); after != before || columns() != "" {
			t.Errorf("after the refusal the grants of orders are %q, before %q, with the column grants %q", after, before, columns())
		}
		if _, err := k.db.Exec("UPDATE public.orders SET parent_id = NULL WHERE id = 7"); err != nil {
			t.Fatal(err)
		}
		if err := k.validate(context.Background(), validateParent); err != nil {
			t.Fatalf("the validation with every parent there: %v", err)
		}
		var validated bool
		if err := k.db.QueryRow("SELECT convalidated FROM pg_catalog.pg_constraint WHERE conname = 'orders_parent_fkey'").Scan(&validated); err != nil || !validated {
			t.Errorf("the key of the parent is validated %v (error %v)", validated, err)
		}
		if !k.ownerReads(t) || columns() != "" {
			t.Errorf("after the validation the owner role reads orders %v, with the column grants %q, want none", k.ownerReads(t), columns())
		}
	})
}
