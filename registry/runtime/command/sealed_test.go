package command

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/sealed"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// vault is a test entity with a readable secret, token, and a compare-only secret, pin, as the generated code
// declares it (ST-SUP-RC32-OC20).
type vault struct {
	ID, Label string
	Token     sealed.Secret
	PIN       sealed.Compared
}

const vaultDDL = `CREATE TABLE vaults (tenant_id text NOT NULL, id text NOT NULL, label text NOT NULL, token bytea,
  pin text, version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))`

// throwawayKey gives a new random key of the key ring form; the tests use throwaway keys only, which live in memory.
func throwawayKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, sealed.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func vaultRing(t *testing.T, lines ...string) *sealed.Ring {
	t.Helper()
	r, err := sealed.ParseRing([]byte(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// vaultSpec is the repository declaration of vault, as the generator gives it.
func vaultSpec() EntitySpec[vault] {
	s := spec("Vault", "", "vaults", []string{"label", "token", "pin"},
		func(row Scanner) (vault, Meta, error) {
			var v vault
			var m Meta
			err := row.Scan(&v.ID, &v.Label, OpenAt("vaults", "token", &v.ID, &v.Token), ComparedAt(&v.PIN), &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v vault) []any {
			return []any{v.ID, v.Label, SealAt("vaults", "token", v.ID, v.Token), HashOf(v.PIN)}
		},
		func(v vault) map[string]any { return map[string]any{"id": v.ID, "label": v.Label} })
	s.Sealed = true
	return s
}

// vaultReseal is the re-seal column of vault, as the PostgreSQL adapter gives it.
var vaultReseal = []ResealColumn{{Entity: "Vault", Table: "vaults", Column: "token",
	Select: "SELECT id, token FROM vaults WHERE tenant_id = $1 AND token IS NOT NULL AND substring(token from 1 for 5) <> $2 AND id > $4 ORDER BY id LIMIT $3 FOR UPDATE SKIP LOCKED",
	Update: "UPDATE vaults SET token = $3 WHERE tenant_id = $1 AND id = $2 AND token = $4",
	Left:   "SELECT count(*) FROM vaults WHERE tenant_id = $1 AND token IS NOT NULL AND substring(token from 1 for 5) <> $2"}}

func vaultEntity(t *testing.T) *Entity[vault] {
	t.Helper()
	e, err := NewEntity(vaultSpec())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// vaultEnv is the command test env with the table vaults, its entity and a runtime with the key ring.
type vaultEnv struct {
	*env
	vaults *Entity[vault]
	rt     *Runtime
	cmd    *Command[testProbe]
	reads  int
	// printed are the fmt and slog forms of each loaded value that a read gave.
	printed []string
}

// loadedPrints gives the fmt forms of a loaded value, alone and in an unexported field of a wrapper, and its slog text
// and JSON forms, as a fill that logs it by mistake writes them (security review of PR #441, M1).
func loadedPrints(l Loaded[vault]) []string {
	type wrapper struct {
		loaded Loaded[vault]
		value  vault
	}
	w := wrapper{loaded: l, value: l.Value()}
	var out []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		out = append(out, fmt.Sprintf(verb, l), fmt.Sprintf(verb, w), fmt.Errorf("verify "+verb, w).Error())
	}
	var b bytes.Buffer
	slog.New(slog.NewTextHandler(&b, nil)).Info("read", "loaded", l, "wrapped", w, "value", l.Value())
	slog.New(slog.NewJSONHandler(&b, nil)).Info("read", "loaded", l, "wrapped", w, "value", l.Value())
	return append(out, b.String())
}

func newVaultEnv(t *testing.T, ring *sealed.Ring) *vaultEnv {
	t.Helper()
	e := newEnv(t)
	e.exec(vaultDDL, "GRANT SELECT, INSERT, UPDATE, DELETE ON vaults TO "+e.roles[0],
		"ALTER TABLE vaults ENABLE ROW LEVEL SECURITY", "ALTER TABLE vaults FORCE ROW LEVEL SECURITY",
		"CREATE POLICY vaults_tenant ON vaults USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))")
	v := &vaultEnv{env: e, vaults: vaultEntity(t)}
	v.rt = e.runtime(func(c *Config) { c.Entities = []Declared{v.vaults}; c.Ring = ring })
	v.cmd = e.command("keep-vault", nil, v.vaults)
	return v
}

// write runs the command once in the tenant with the fill.
func (v *vaultEnv) write(tenantID, key string, fill func(ctx context.Context, r *Repository[vault]) error) error {
	v.t.Helper()
	_, err := v.runOn(v.rt, v.cmd, v.request(tenantID, u7(v.t), key, `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
		r, err := Repo(h, v.vaults)
		if err != nil {
			return idempotency.Response{}, err
		}
		return ok, fill(ctx, r)
	})
	return err
}

// read gives the vault of id in the tenant through a handle of the runtime rt.
func (v *vaultEnv) read(rt *Runtime, tenantID, id string) (vault, error) {
	v.t.Helper()
	var got vault
	v.reads++
	_, err := v.runOn(rt, v.cmd, v.request(tenantID, u7(v.t), fmt.Sprintf("read-%d", v.reads), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
		r, err := Repo(h, v.vaults)
		if err != nil {
			return idempotency.Response{}, err
		}
		l, err := r.Get(ctx, id)
		if err != nil {
			return idempotency.Response{}, err
		}
		got = l.Value()
		v.printed = append(v.printed, loadedPrints(l)...)
		return ok, nil
	})
	return got, err
}

func (v *vaultEnv) stored(tenantID, id string) (token []byte, pin string) {
	v.t.Helper()
	var p *string
	if err := v.admin.QueryRowContext(v.ctx, `SELECT token, pin FROM vaults WHERE tenant_id = $1 AND id = $2`, tenantID, id).Scan(&token, &p); err != nil {
		v.t.Fatal(err)
	}
	if p != nil {
		pin = *p
	}
	return token, pin
}

// noValueAnywhere fails when any row of the audit, outbox and idempotency tables, as text, or a recorded statement
// value, holds one of the values.
func (v *vaultEnv) noValueAnywhere(values ...string) {
	v.t.Helper()
	for _, table := range []string{"audit_event", "stego_outbox.messages", "idempotency_keys"} {
		rows, err := v.admin.QueryContext(v.ctx, `SELECT t::text FROM `+table+` t`)
		if err != nil {
			v.t.Fatalf("%s: %v", table, err)
		}
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				v.t.Fatal(err)
			}
			for _, value := range values {
				if strings.Contains(text, value) {
					v.t.Fatalf("a row of %s holds a secret value", table)
				}
			}
		}
		_ = rows.Close()
	}
}

func TestSealedFields(t *testing.T) {
	const token, pin = "s3cr3t-value-0042", "4711"
	t.Run("ST-SUP-RP10-LC44-T01 The handle stores a readable secret sealed at its place and a compare-only secret hashed, and reads them back", func(t *testing.T) {
		ring := vaultRing(t, "1 "+throwawayKey(t))
		v := newVaultEnv(t, ring)
		var statementArgs []string
		v.rt.recorder = func(query string, args []any) {
			v.statements.add(query, args)
			statementArgs = append(statementArgs, fmt.Sprint(args...))
		}
		if err := v.write("org-a", "k1", func(ctx context.Context, r *Repository[vault]) error {
			_, err := r.Create(ctx, vault{ID: "v1", Label: "first", Token: sealed.SecretOf(token), PIN: sealed.ComparedOf(pin)})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := v.write("org-a", "k2", func(ctx context.Context, r *Repository[vault]) error {
			_, err := r.Create(ctx, vault{ID: "v2", Label: "second", Token: sealed.SecretOf("other-token")})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// An ID in upper case is sealed at its lower-case form, which the scan gives (security review of PR #441, L7).
		if err := v.write("org-a", "k-upper", func(ctx context.Context, r *Repository[vault]) error {
			_, err := r.Create(ctx, vault{ID: "UPPER-ID", Label: "upper", Token: sealed.SecretOf("upper-token")})
			return err
		}); err != nil {
			t.Fatalf("a create with an ID in upper case gave %v", err)
		}
		if err := v.write("org-b", "k3", func(ctx context.Context, r *Repository[vault]) error {
			_, err := r.Create(ctx, vault{ID: "v1", Label: "of b", Token: sealed.SecretOf("b-token")})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		storedToken, storedPin := v.stored("org-a", "v1")
		if bytes.Contains(storedToken, []byte(token)) || !bytes.HasPrefix(storedToken, sealed.Prefix(1)) {
			t.Fatalf("the column token holds % x, not the sealed value of the key 1", storedToken)
		}
		if strings.Contains(storedPin, pin) || !strings.HasPrefix(storedPin, "$argon2id$v=19$") {
			t.Fatalf("the column pin holds %q, not an Argon2id hash", storedPin)
		}
		if _, nullPin := v.stored("org-a", "v2"); nullPin != "" {
			t.Fatal("a compare-only secret that is not set is not NULL")
		}
		got, err := v.read(v.rt, "org-a", "v1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Token.Reveal() != token || !got.PIN.Matches(pin) || got.PIN.Matches("4712") || got.PIN.Matches("") {
			t.Fatal("the read does not give back the readable secret, or the compare-only check is wrong")
		}
		for _, p := range v.printed {
			if strings.Contains(p, token) || strings.Contains(p, "$argon2id$") {
				t.Fatalf("a loaded value prints a secret or its hash: %s", p)
			}
		}
		if !strings.Contains(v.printed[0], "loaded Vault v1") {
			t.Fatalf("a loaded value prints %q, want its entity and ID", v.printed[0])
		}
		// An update of another field keeps the hash and seals the token again, at the same place.
		if err := v.write("org-a", "k4", func(ctx context.Context, r *Repository[vault]) error {
			l, err := r.GetForUpdate(ctx, "v1")
			if err != nil {
				return err
			}
			next := l.Value()
			next.Label = "renamed"
			_, err = r.Update(ctx, l, next)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		afterToken, afterPin := v.stored("org-a", "v1")
		if afterPin != storedPin || bytes.Equal(afterToken, storedToken) {
			t.Fatal("an update changed the stored hash, or did not seal the token again with a new nonce")
		}
		if got, err := v.read(v.rt, "org-a", "v1"); err != nil || got.Token.Reveal() != token || !got.PIN.Matches(pin) {
			t.Fatalf("the read after the update gave %v", err)
		}
		for _, args := range statementArgs {
			if strings.Contains(args, token) || strings.Contains(args, pin+"]") {
				t.Fatal("a statement got the secret value in clear")
			}
		}
		v.noValueAnywhere(token, "other-token", "b-token")
	})
	t.Run("ST-SUP-RP10-LC44-T02 A sealed value copied to another row or tenant does not open, and the error names no value", func(t *testing.T) {
		ring := vaultRing(t, "1 "+throwawayKey(t))
		v := newVaultEnv(t, ring)
		for _, row := range []struct{ tenant, id, key string }{{"org-a", "v1", "c1"}, {"org-a", "v2", "c2"}, {"org-b", "v1", "c3"}} {
			if err := v.write(row.tenant, row.key, func(ctx context.Context, r *Repository[vault]) error {
				_, err := r.Create(ctx, vault{ID: row.id, Label: "x", Token: sealed.SecretOf(token + "-" + row.tenant + row.id)})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		v.exec(`UPDATE vaults SET token = (SELECT token FROM vaults WHERE tenant_id = 'org-a' AND id = 'v1') WHERE tenant_id = 'org-a' AND id = 'v2'`,
			`UPDATE vaults SET token = (SELECT token FROM vaults WHERE tenant_id = 'org-a' AND id = 'v1') WHERE tenant_id = 'org-b' AND id = 'v1'`)
		for _, at := range []struct{ tenant, id string }{{"org-a", "v2"}, {"org-b", "v1"}} {
			_, err := v.read(v.rt, at.tenant, at.id)
			if !errors.Is(err, ErrInternal) || !strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), token) {
				t.Fatalf("the read of the copied value in %s/%s gave %v, want an internal error that names the column", at.tenant, at.id, err)
			}
		}
		if got, err := v.read(v.rt, "org-a", "v1"); err != nil || got.Token.Reveal() != token+"-org-av1" {
			t.Fatalf("the row of the value does not read: %v", err)
		}
		if _, err := New(v.ctx, Config{DB: v.pool, AuditDB: v.auditDB, Authz: v.authz, Audit: v.writer, Outbox: v.queue,
			Probes: v.probes, Service: "orders", Entities: []Declared{vaultEntity(t)}}); err == nil || !strings.Contains(err.Error(), "key ring") {
			t.Fatalf("a runtime with no key ring for an entity with a readable secret gave %v", err)
		}
	})
	t.Run("ST-SUP-RP10-LC46-T01 After a key rotation old values read, and the re-seal job moves them to the new key", func(t *testing.T) {
		oldKey, newKey := throwawayKey(t), throwawayKey(t)
		v := newVaultEnv(t, vaultRing(t, "1 "+oldKey))
		const rows = ResealBatch + 5
		for i := range rows {
			tn := "org-a"
			if i%2 == 1 {
				tn = "org-b"
			}
			if err := v.write(tn, fmt.Sprintf("r%d", i), func(ctx context.Context, r *Repository[vault]) error {
				_, err := r.Create(ctx, vault{ID: fmt.Sprintf("v%03d", i), Label: "x", Token: sealed.SecretOf(fmt.Sprintf("%s-%03d", token, i))})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		versions := v.count(`SELECT sum(version) FROM vaults`)
		rotated := vaultRing(t, "2 "+newKey, "1 "+oldKey)
		rt := v.runtime(func(c *Config) { c.Ring = rotated })
		// The key table holds keyed fingerprints of the key 1 with the mark of a sealed answer, and a request again with the key of an earlier write
		// replays on the rotated runtime, whose first key is 2 (ST-SUP-RP16-LC05).
		if n := v.count(`SELECT count(*) FROM idempotency_keys WHERE fingerprint NOT LIKE 's1:k1:%'`); n != 0 {
			t.Fatalf("%d key rows hold a fingerprint that is not keyed with the key 1", n)
		}
		replay, err := v.runOn(rt, v.cmd, v.request("org-a", u7(t), "r0", `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			return idempotency.Response{}, errors.New("the fill ran again")
		})
		if err != nil || !replay.Replayed {
			t.Fatalf("a request again with an earlier key on the rotated runtime gave %+v, %v, want a replay", replay, err)
		}
		changed, err := v.runOn(rt, v.cmd, v.request("org-a", u7(t), "r0", `{"other":true}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			return idempotency.Response{}, errors.New("the fill ran again")
		})
		if err != nil || changed.Problem != idempotency.ProblemReused {
			t.Fatalf("a request again with an earlier key and another body gave %+v, %v, want the key reused problem", changed, err)
		}
		if got, err := v.read(rt, "org-a", "v000"); err != nil || got.Token.Reveal() != token+"-000" {
			t.Fatalf("an old value does not read after the rotation: %v", err)
		}
		// The first row in ID order holds the value of another row, which does not open at its place: the job logs and
		// skips it, and moves each row after it (security review of PR #441, L4).
		v.exec(`UPDATE vaults SET token = (SELECT token FROM vaults WHERE tenant_id = 'org-a' AND id = 'v002') WHERE tenant_id = 'org-a' AND id = 'v000'`)
		job, err := NewReseal(v.pool, "unused", rotated, vaultReseal)
		if err != nil {
			t.Fatal(err)
		}
		job.tenants = func(context.Context) ([]tenant.Tenant, error) {
			return []tenant.Tenant{mustTenant(t, "org-a"), mustTenant(t, "org-b")}, nil
		}
		// A command holds the row v001 of org-b locked while the job runs: SKIP LOCKED passes it, and the count with
		// no lock after the pages still names it as left (security re-review of PR #441, N1).
		lock, err := v.admin.BeginTx(v.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lock.ExecContext(v.ctx, `SELECT 1 FROM vaults WHERE tenant_id = 'org-b' AND id = 'v001' FOR UPDATE`); err != nil {
			t.Fatal(err)
		}
		results, err := job.Run(v.ctx)
		_ = lock.Rollback()
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 || results[0].Moved != rows-2 || results[0].Left != 2 || results[0].Failed != 1 {
			t.Fatalf("the re-seal job gave %+v, want %d moved, the bad and the locked row left and the bad row failed", results, rows-2)
		}
		if again, err := job.Run(v.ctx); err != nil || again[0].Moved != 1 || again[0].Left != 1 || again[0].Failed != 1 {
			t.Fatalf("the run after the lock gave %+v, %v, want the locked row moved and only the bad row left", again, err)
		}
		if n := v.count(`SELECT count(*) FROM vaults WHERE substring(token from 1 for 5) <> $1`, sealed.Prefix(2)); n != 1 {
			t.Fatalf("%d values are not sealed with the key 2, want only the bad row", n)
		}
		if v.count(`SELECT sum(version) FROM vaults`) != versions {
			t.Fatal("the re-seal job changed the version of a row")
		}
		onlyNew := v.runtime(func(c *Config) { c.Ring = vaultRing(t, "2 "+newKey) })
		for i := 1; i < rows; i++ {
			tn := "org-a"
			if i%2 == 1 {
				tn = "org-b"
			}
			if got, err := v.read(onlyNew, tn, fmt.Sprintf("v%03d", i)); err != nil || got.Token.Reveal() != fmt.Sprintf("%s-%03d", token, i) {
				t.Fatalf("the value %d does not read with the new key alone: %v", i, err)
			}
		}
		if again, err := job.Run(v.ctx); err != nil || again[0].Moved != 0 || again[0].Left != 1 || again[0].Failed != 1 {
			t.Fatalf("a second run gave %+v, %v", again, err)
		}
		v.noValueAnywhere(token)
	})
}
