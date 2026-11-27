package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/910-software-solutions/stego/registry/runtime/query"
	"github.com/910-software-solutions/stego/registry/runtime/tenant"
)

// secretValue is in the value of each setting of the test, so a refusal that shows a value shows it.
const secretValue = "s3cret-value"

func TestSettings(t *testing.T) {
	t.Run("ST-SUP-RP14-LC01-T01 Settings come from the process environment by fixed names", func(t *testing.T) {
		dir := t.TempDir()
		spiceKey := writeFile(t, "spicedb.key", []byte(secretValue+"-key\n"))
		cursor := writeFile(t, "cursor.key", []byte(strings.Repeat("c", 32)+secretValue))
		keys := writeFile(t, "keys.json", []byte(`{"keys":[]}`))
		dsnFile := writeFile(t, "dsn", []byte("postgres://rt:"+secretValue+"@db/app\n"))
		migrationFile := writeFile(t, "migration-dsn", []byte("postgres://owner:"+secretValue+"@db/app"))
		full := map[string]string{
			DatabaseURLSetting:          "postgres://rt:" + secretValue + "@db/app",
			MigrationDatabaseURLSetting: "postgres://owner:" + secretValue + "@db/app",
			IssuerSetting:               "https://issuer.example/" + secretValue,
			AudienceSetting:             "orders-" + secretValue,
			KeysURLSetting:              "https://issuer.example/keys/" + secretValue,
			SpiceDBEndpointSetting:      "127.0.0.1:50051",
			SpiceDBKeyFileSetting:       spiceKey,
			CursorKeyFileSetting:        cursor,
			VersionSetting:              "1.2.3",
		}
		set := func(t *testing.T, env map[string]string) {
			for _, name := range []string{DatabaseURLSetting, DatabaseURLFileSetting, MigrationDatabaseURLSetting,
				MigrationDatabaseURLFileSetting, IssuerSetting, AudienceSetting, KeysURLSetting, KeysFileSetting, CAFileSetting,
				SpiceDBEndpointSetting, SpiceDBKeyFileSetting, SpiceDBInsecureLoopbackSetting, CursorKeyFileSetting, ExtraCursorKeyFileSetting, VersionSetting} {
				t.Setenv(name, env[name])
			}
		}
		with := func(change map[string]string) map[string]string {
			env := map[string]string{}
			for k, v := range full {
				env[k] = v
			}
			for k, v := range change {
				env[k] = v
			}
			return env
		}

		t.Run("every setting by its value", func(t *testing.T) {
			set(t, with(map[string]string{SpiceDBInsecureLoopbackSetting: "1"}))
			s, err := ReadSettings(Auth{Header: "X-Forwarded-Access-Token", RoleClaim: "realm_access.roles"})
			if err != nil {
				t.Fatal(err)
			}
			m, err := ReadMigrationSettings()
			if err != nil {
				t.Fatal(err)
			}
			if s.DatabaseURL != full[DatabaseURLSetting] || s.Issuer != full[IssuerSetting] || s.Audience != full[AudienceSetting] ||
				s.KeysURL != full[KeysURLSetting] || s.SpiceDB.Endpoint != "127.0.0.1:50051" || s.SpiceDB.Key != secretValue+"-key" ||
				!s.SpiceDB.InsecureLoopback || len(s.CursorKey) != 32+len(secretValue) || s.ServiceVersion != "1.2.3" ||
				s.Header != "X-Forwarded-Access-Token" || s.RoleClaim != "realm_access.roles" || m.DatabaseURL != full[MigrationDatabaseURLSetting] {
				t.Fatalf("the settings are %+v and %+v", s, m)
			}
		})
		t.Run("each file form in place of its value", func(t *testing.T) {
			set(t, with(map[string]string{DatabaseURLSetting: "", DatabaseURLFileSetting: dsnFile, MigrationDatabaseURLSetting: "",
				MigrationDatabaseURLFileSetting: migrationFile, KeysURLSetting: "", KeysFileSetting: keys}))
			s, err := ReadSettings(Auth{})
			if err != nil {
				t.Fatal(err)
			}
			m, err := ReadMigrationSettings()
			if err != nil {
				t.Fatal(err)
			}
			if s.DatabaseURL != full[DatabaseURLSetting] || s.KeysFile != keys || m.DatabaseURL != full[MigrationDatabaseURLSetting] {
				t.Fatalf("the file forms gave %+v and %+v", s, m)
			}
		})
		t.Run("the declared verifier settings win", func(t *testing.T) {
			set(t, with(map[string]string{IssuerSetting: "", KeysURLSetting: ""}))
			s, err := ReadSettings(Auth{Issuer: "https://declared.example", KeyFile: "/etc/keys/issuer.pem"})
			if err != nil || s.Issuer != "https://declared.example" || s.KeyFile != "/etc/keys/issuer.pem" {
				t.Fatalf("the declared settings gave %+v, %v", s, err)
			}
			set(t, with(nil))
			if _, err := ReadSettings(Auth{Issuer: "https://declared.example"}); err == nil || !strings.Contains(err.Error(), IssuerSetting) {
				t.Fatalf("an issuer setting that differs from the declared one gave %v", err)
			}
			if _, err := ReadSettings(Auth{KeyFile: "/etc/keys/issuer.pem"}); err == nil || !strings.Contains(err.Error(), KeysURLSetting) {
				t.Fatalf("a keys setting beside a declared key file gave %v", err)
			}
		})
		t.Run("ST-SUP-RP14-LC11-T01 Only the api mode reads the key of the list cursors", func(t *testing.T) {
			set(t, with(map[string]string{CursorKeyFileSetting: ""}))
			if _, err := ReadModeSettings(Auth{}, true); err == nil || !strings.Contains(err.Error(), CursorKeyFileSetting) {
				t.Fatalf("the api mode with no cursor key gave %v, want a refusal that names %s", err, CursorKeyFileSetting)
			}
			if _, err := ReadSettings(Auth{}); err == nil {
				t.Fatal("ReadSettings, which reads every setting, took no cursor key")
			}
			worker, err := ReadModeSettings(Auth{}, false)
			if err != nil || len(worker.CursorKey) != query.MinCursorKey {
				t.Fatalf("a process with no api mode gave %v with a key of %d bytes, want a random key of %d", err, len(worker.CursorKey), query.MinCursorKey)
			}
			other, _ := ReadModeSettings(Auth{}, false)
			if bytes.Equal(worker.CursorKey, other.CursorKey) || bytes.Equal(worker.CursorKey, make([]byte, query.MinCursorKey)) {
				t.Fatal("a process with no api mode gets a fixed key, want a new random key each time")
			}
			// A cursor key file that the api mode refuses is not read by a process with no api mode.
			set(t, with(map[string]string{CursorKeyFileSetting: writeFile(t, "short.key", []byte("short"))}))
			if _, err := ReadModeSettings(Auth{}, false); err != nil {
				t.Fatalf("a process with no api mode read the cursor key file: %v", err)
			}
			api, err := ReadModeSettings(Auth{}, true)
			if err == nil || api.CursorKey != nil {
				t.Fatal("the api mode took a cursor key of fewer than 32 bytes")
			}
		})
		t.Run("ST-SUP-RP11-LC23-T02 The api mode reads the extra cursor key, and refuses a short one or the same key", func(t *testing.T) {
			set(t, with(nil))
			if s, err := ReadModeSettings(Auth{}, true); err != nil || s.ExtraCursorKey != nil {
				t.Fatalf("no extra key gave %v with an extra key of %d bytes, want none", err, len(s.ExtraCursorKey))
			}
			extra := []byte(strings.Repeat("e", 32))
			set(t, with(map[string]string{ExtraCursorKeyFileSetting: writeFile(t, "extra.key", extra)}))
			s, err := ReadModeSettings(Auth{}, true)
			if err != nil || !bytes.Equal(s.ExtraCursorKey, extra) {
				t.Fatalf("the extra key gave %v and %q", err, s.ExtraCursorKey)
			}
			if worker, err := ReadModeSettings(Auth{}, false); err != nil || worker.ExtraCursorKey != nil {
				t.Fatalf("a process with no api mode gave %v and an extra key of %d bytes, want none", err, len(worker.ExtraCursorKey))
			}
			for name, file := range map[string]string{
				"short":   writeFile(t, "extra-short.key", []byte("short")),
				"the key": full[CursorKeyFileSetting],
				"missing": filepath.Join(dir, "none.key"),
			} {
				set(t, with(map[string]string{ExtraCursorKeyFileSetting: file}))
				if _, err := ReadModeSettings(Auth{}, true); err == nil || !strings.Contains(err.Error(), ExtraCursorKeyFileSetting) {
					t.Errorf("an extra key that is %s gave %v, want a refusal that names %s", name, err, ExtraCursorKeyFileSetting)
				}
			}
		})
		t.Run("ST-SUP-RP14-LC01-T02 A declaration with discovery takes no key document and the CA file", func(t *testing.T) {
			ca := writeFile(t, "ca.pem", []byte("CA"))
			oidc := Auth{Issuer: "https://declared.example/realms/r1", Discovery: true, Algorithms: []string{"RS256", "ES256"},
				SubjectClaim: "oid"}
			set(t, with(map[string]string{IssuerSetting: "", KeysURLSetting: "", CAFileSetting: ca}))
			s, err := ReadSettings(oidc)
			if err != nil || !s.Discovery || s.KeysURL != "" || s.KeysFile != "" || s.CAFile != ca || s.Issuer != oidc.Issuer ||
				len(s.Algorithms) != 2 || s.SubjectClaim != "oid" {
				t.Fatalf("the settings of a declaration with discovery are %+v, %v", s, err)
			}
			stated := oidc
			stated.CAFile = "/etc/stego/declared-ca.pem"
			set(t, with(map[string]string{IssuerSetting: "", KeysURLSetting: ""}))
			if s, err := ReadSettings(stated); err != nil || s.CAFile != stated.CAFile {
				t.Fatalf("the declared CA file gave %+v, %v", s, err)
			}
			for name, c := range map[string]struct {
				env   map[string]string
				auth  Auth
				names string
			}{
				"a key URL beside discovery":       {map[string]string{IssuerSetting: ""}, oidc, KeysURLSetting},
				"a key file beside discovery":      {map[string]string{IssuerSetting: "", KeysURLSetting: "", KeysFileSetting: "/k-" + secretValue}, oidc, KeysFileSetting},
				"discovery and a public key file":  {map[string]string{IssuerSetting: "", KeysURLSetting: ""}, Auth{Issuer: oidc.Issuer, Discovery: true, KeyFile: "/k.pem"}, KeysURLSetting},
				"a CA file other than the stated":  {map[string]string{IssuerSetting: "", KeysURLSetting: "", CAFileSetting: "/other-" + secretValue}, stated, CAFileSetting},
				"a CA file beside a key file only": {map[string]string{KeysURLSetting: "", KeysFileSetting: keys, CAFileSetting: ca}, Auth{}, CAFileSetting},
			} {
				t.Run(name, func(t *testing.T) {
					set(t, with(c.env))
					_, err := ReadSettings(c.auth)
					if err == nil || !strings.Contains(err.Error(), c.names) || strings.Contains(err.Error(), secretValue) {
						t.Fatalf("%s gave %v, want a refusal that names %s and holds no value", name, err, c.names)
					}
				})
			}
		})
		refused := map[string]map[string]string{
			DatabaseURLSetting:             {DatabaseURLSetting: ""},
			IssuerSetting:                  {IssuerSetting: ""},
			AudienceSetting:                {AudienceSetting: ""},
			KeysURLSetting:                 {KeysURLSetting: ""},
			SpiceDBEndpointSetting:         {SpiceDBEndpointSetting: ""},
			SpiceDBKeyFileSetting:          {SpiceDBKeyFileSetting: ""},
			CursorKeyFileSetting:           {CursorKeyFileSetting: ""},
			DatabaseURLFileSetting:         {DatabaseURLSetting: "", DatabaseURLFileSetting: dir + "/missing-" + secretValue},
			SpiceDBInsecureLoopbackSetting: {SpiceDBInsecureLoopbackSetting: "yes-" + secretValue},
		}
		t.Run("refused: no TLS to an address that is not loopback", func(t *testing.T) {
			set(t, with(map[string]string{SpiceDBInsecureLoopbackSetting: "1", SpiceDBEndpointSetting: "spicedb.example:50051"}))
			if _, err := ReadSettings(Auth{}); err == nil || !strings.Contains(err.Error(), SpiceDBInsecureLoopbackSetting) {
				t.Fatalf("no TLS to a remote SpiceDB gave %v", err)
			}
		})
		short := writeFile(t, "short.key", []byte(secretValue))
		refused[CursorKeyFileSetting+" short"] = map[string]string{CursorKeyFileSetting: short}
		for name, change := range refused {
			t.Run("refused without "+name, func(t *testing.T) {
				set(t, with(change))
				_, err := ReadSettings(Auth{})
				setting, _, _ := strings.Cut(name, " ")
				if err == nil || !strings.Contains(err.Error(), setting) || strings.Contains(err.Error(), secretValue) {
					t.Fatalf("the settings with %s missing or malformed gave %v, want a refusal that names it and holds no value", name, err)
				}
			})
		}
		t.Run("refused without the migration database URL", func(t *testing.T) {
			set(t, with(map[string]string{MigrationDatabaseURLSetting: ""}))
			_, err := ReadMigrationSettings()
			if err == nil || !strings.Contains(err.Error(), MigrationDatabaseURLSetting) || strings.Contains(err.Error(), secretValue) {
				t.Fatalf("the migration settings with no URL gave %v", err)
			}
		})
	})
}

func TestSettingFiles(t *testing.T) {
	t.Run("ST-SUP-RP14-LC01-T03 The key files pass the checks of a connection string file", func(t *testing.T) {
		dir := t.TempDir()
		good := map[string]string{
			SpiceDBKeyFileSetting: writeFile(t, "spicedb.key", []byte(secretValue+"-key\n")),
			CursorKeyFileSetting:  writeFile(t, "cursor.key", []byte(strings.Repeat("c", 32)+secretValue)),
		}
		base := map[string]string{
			DatabaseURLSetting:     "postgres://rt:" + secretValue + "@db/app",
			IssuerSetting:          "https://issuer.example/",
			AudienceSetting:        "orders",
			KeysURLSetting:         "https://issuer.example/keys",
			SpiceDBEndpointSetting: "127.0.0.1:50051",
		}
		set := func(t *testing.T, change map[string]string) {
			t.Helper()
			for _, name := range []string{DatabaseURLFileSetting, KeysFileSetting, CAFileSetting, SpiceDBInsecureLoopbackSetting, VersionSetting} {
				t.Setenv(name, "")
			}
			for k, v := range base {
				t.Setenv(k, v)
			}
			for k, v := range good {
				t.Setenv(k, v)
			}
			for k, v := range change {
				t.Setenv(k, v)
			}
		}
		set(t, nil)
		if _, err := ReadSettings(Auth{}); err != nil {
			t.Fatalf("the good key files were refused: %v", err)
		}
		withMode := func(name string, mode os.FileMode) string {
			path := writeFile(t, name, []byte(strings.Repeat("k", 48)))
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			return path
		}
		fifo := filepath.Join(dir, "fifo-"+secretValue)
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "zero-"+secretValue)
		if err := os.Symlink("/dev/zero", link); err != nil {
			t.Fatal(err)
		}
		bad := map[string]string{
			"a relative path":             "keys/" + secretValue + ".key",
			"a missing file":              filepath.Join(dir, "missing-"+secretValue),
			"a folder":                    dir,
			"a FIFO":                      fifo,
			"a link to a device":          link,
			"a file that group can write": withMode("group-write.key", 0o620),
			"a file that other can write": withMode("other-write.key", 0o602),
			"an executable file":          withMode("exec.key", 0o700),
			"a file above 64 KiB":         writeFile(t, "big.key", []byte(strings.Repeat("k", 65537))),
		}
		for _, setting := range []string{SpiceDBKeyFileSetting, CursorKeyFileSetting} {
			for name, path := range bad {
				t.Run(setting+" with "+name, func(t *testing.T) {
					set(t, map[string]string{setting: path})
					done := make(chan error, 1)
					go func() { _, err := ReadSettings(Auth{}); done <- err }()
					select {
					case err := <-done:
						if err == nil || !strings.Contains(err.Error(), setting) || strings.Contains(err.Error(), secretValue) {
							t.Fatalf("ReadSettings gave %v, want a refusal that names %s and holds no path", err, setting)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("ReadSettings waits on the file")
					}
				})
			}
		}
		// The migration job reads the SpiceDB key file with the same checks.
		set(t, map[string]string{MigrationDatabaseURLSetting: "postgres://owner@db/app", SpiceDBKeyFileSetting: bad["a file that group can write"]})
		if _, err := ReadMigrationSettings(); err == nil || !strings.Contains(err.Error(), SpiceDBKeyFileSetting) {
			t.Fatalf("ReadMigrationSettings took a key file that group can write: %v", err)
		}
	})
}

func TestStartStages(t *testing.T) {
	t.Run("ST-SUP-RP14-LC05-T02 A setting that ReadSettings refuses gives the stage of its component and its variable, with no value", func(t *testing.T) {
		names := []string{DatabaseURLSetting, DatabaseURLFileSetting, IssuerSetting, AudienceSetting, KeysURLSetting, KeysFileSetting,
			CAFileSetting, SpiceDBEndpointSetting, SpiceDBKeyFileSetting, SpiceDBInsecureLoopbackSetting, CursorKeyFileSetting, VersionSetting}
		full := map[string]string{
			DatabaseURLSetting:     "postgres://rt:" + secretValue + "@db/app",
			IssuerSetting:          "https://issuer.example/" + secretValue,
			AudienceSetting:        "orders-" + secretValue,
			KeysURLSetting:         "https://issuer.example/keys/" + secretValue,
			SpiceDBEndpointSetting: "127.0.0.1:50051",
			SpiceDBKeyFileSetting:  writeFile(t, "spicedb.key", []byte(secretValue+"-key\n")),
			CursorKeyFileSetting:   writeFile(t, "cursor.key", []byte(strings.Repeat("c", 32)+secretValue)),
		}
		short := writeFile(t, "short.key", []byte(secretValue))
		for _, c := range []struct {
			name   string
			change map[string]string
			auth   Auth
			stage  string
		}{
			{"no DATABASE_URL and no DATABASE_URL_FILE", map[string]string{DatabaseURLSetting: ""}, Auth{},
				"service.start.setting.postgres-adapter.DATABASE_URL"},
			{"both DATABASE_URL and DATABASE_URL_FILE", map[string]string{DatabaseURLFileSetting: short}, Auth{},
				"service.start.setting.postgres-adapter.DATABASE_URL"},
			{"a DATABASE_URL_FILE that is not an absolute path", map[string]string{DatabaseURLSetting: "", DatabaseURLFileSetting: "dsn-" + secretValue}, Auth{},
				"service.start.setting.postgres-adapter.DATABASE_URL_FILE"},
			{"no issuer for jwt-auth", map[string]string{IssuerSetting: ""}, Auth{},
				"service.start.setting.jwt-auth.STEGO_AUTHN_ISSUER"},
			{"no audience for oidc-auth", map[string]string{AudienceSetting: ""}, Auth{Component: "oidc-auth"},
				"service.start.setting.oidc-auth.STEGO_AUTHN_AUDIENCE"},
			{"an issuer that differs from the declared one of rh-sso-auth", nil, Auth{Issuer: "https://declared.example", Component: "rh-sso-auth"},
				"service.start.setting.rh-sso-auth.STEGO_AUTHN_ISSUER"},
			{"no key document", map[string]string{KeysURLSetting: ""}, Auth{},
				"service.start.setting.jwt-auth.STEGO_AUTHN_KEYS_URL"},
			{"no SpiceDB address", map[string]string{SpiceDBEndpointSetting: ""}, Auth{},
				"service.start.setting.spicedb-authz.STEGO_SPICEDB_ENDPOINT"},
			{"a SpiceDB loopback mode that is not 0 or 1", map[string]string{SpiceDBInsecureLoopbackSetting: secretValue}, Auth{},
				"service.start.setting.spicedb-authz.STEGO_SPICEDB_INSECURE_LOOPBACK"},
			{"a cursor key that is too short", map[string]string{CursorKeyFileSetting: short}, Auth{},
				"service.start.setting.query.STEGO_QUERY_CURSOR_KEY_FILE"},
		} {
			t.Run(c.name, func(t *testing.T) {
				for _, name := range names {
					value, changed := c.change[name]
					if !changed {
						value = full[name]
					}
					t.Setenv(name, value)
				}
				_, err := ReadSettings(c.auth)
				stage := StartStage(err)
				if err == nil || stage != c.stage {
					t.Fatalf("ReadSettings gave %v at the stage %q, want the stage %s", err, stage, c.stage)
				}
				if strings.Contains(stage, secretValue) || strings.Contains(err.Error(), secretValue) {
					t.Fatalf("the refusal shows a value: %q, %v", stage, err)
				}
			})
		}
		if stage := StartStage(errors.New("service: " + secretValue)); stage != "" {
			t.Fatalf("StartStage named the stage %q for a failure that is not of a setting", stage)
		}
	})
	t.Run("ST-SUP-RP14-LC05-T03 A database pool that does not open gives the stage service.start.database, with no value", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		closed := listener.Addr().String()
		_ = listener.Close()
		settings := Settings{DatabaseURL: "postgres://rt:" + secretValue + "@" + closed + "/app?sslmode=disable"}
		roles := func() Parts {
			p := parts(nil, nil)
			p.OwnerRole, p.WorkerRole, p.OutboxRelayRole, p.RelayRole, p.PurgeRole = "owner", "worker", "outbox_relay", "relay", "purge"
			return p
		}
		_, err = New(context.Background(), settings, roles())
		if stage := StartStage(err); err == nil || stage != "service.start.database" {
			t.Fatalf("New on a closed database port gave %v at the stage %q, want service.start.database", err, stage)
		}
		refused := roles()
		refused.TenantConfig = func(string) (tenant.Config, error) { return tenant.Config{}, errors.New("no configuration") }
		if _, err := New(context.Background(), settings, refused); StartStage(err) != "service.start.database" {
			t.Fatalf("New with a refused connection configuration gave %v at the stage %q", err, StartStage(err))
		}
		if strings.Contains(StartStage(err), secretValue) {
			t.Fatalf("the stage shows a value: %q", StartStage(err))
		}
	})
	t.Run("ST-SUP-RP14-LC05-T05 A database pool that does not open gives the failure record its fault class, the database, a remedy and a cause class, with no value", func(t *testing.T) {
		roles := func() Parts {
			p := parts(nil, nil)
			p.OwnerRole, p.WorkerRole, p.OutboxRelayRole, p.RelayRole, p.PurgeRole = "owner", "worker", "outbox_relay", "relay", "purge"
			return p
		}
		type classified interface {
			FaultClass() string
			FaultObject() string
			FaultRemedy() string
			CauseClass() string
		}
		check := func(name string, err error, fault, cause string) {
			t.Helper()
			var c classified
			if StartStage(err) != "service.start.database" || !errors.As(err, &c) {
				t.Fatalf("%s gave %v at the stage %q, want service.start.database with a fault", name, err, StartStage(err))
			}
			if c.FaultClass() != fault || c.FaultObject() != "database" || c.FaultRemedy() == "" || c.CauseClass() != cause {
				t.Errorf("%s gives the fault %q, the object %q, the remedy %q and the cause %q, want %q and %q", name,
					c.FaultClass(), c.FaultObject(), c.FaultRemedy(), c.CauseClass(), fault, cause)
			}
			for _, text := range []string{c.FaultClass(), c.FaultObject(), c.FaultRemedy(), c.CauseClass()} {
				if strings.Contains(text, secretValue) || strings.Contains(text, "127.0.0.1") {
					t.Errorf("%s: the failure record shows a value: %q", name, text)
				}
			}
		}
		// A server that accepts the connection and closes it at once, as a database server that restarts after a
		// crash does (walkthrough 21, step 11): the connection ends in the middle of the startup message.
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				// The server reads the startup message first and then closes, so the client reads the end of the
				// stream (EOF) and not a reset of a connection with unread data.
				_, _ = conn.Read(make([]byte, 1024))
				_ = conn.Close()
			}
		}()
		settings := Settings{DatabaseURL: "postgres://rt:" + secretValue + "@" + listener.Addr().String() + "/app?sslmode=disable"}
		_, err = New(context.Background(), settings, roles())
		check("a server that closes the connection", err, DatabaseConnectionFault, "connection_closed")
		refused := roles()
		refused.TenantConfig = func(string) (tenant.Config, error) {
			return tenant.Config{}, errors.New("no configuration " + secretValue)
		}
		_, err = New(context.Background(), settings, refused)
		check("a refused connection configuration", err, DatabaseSettingsFault, "invalid_setting")
		check("a runtime role that fails a check", &databaseError{err: fmt.Errorf("open: %w", tenant.ErrRuntimeRole)}, DatabaseRuntimeRoleFault, "refused")
		check("another cause of the connection", &databaseError{err: errors.New("tenant: open the database: " + secretValue)}, DatabaseConnectionFault, "connection_failed")
	})
}
