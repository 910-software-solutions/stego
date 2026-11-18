package command

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/sealed"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// note is a test entity with a readable secret, body, whose seal step is open: the field is in the clear column
// body and in the sealed column body_sealed, as the generated code declares it (ST-SUP-RC32-OC23).
type note struct {
	ID, Label string
	Body      sealed.Secret
}

const noteDDL = `CREATE TABLE notes (tenant_id text NOT NULL, id text NOT NULL, label text NOT NULL, body text, body_sealed bytea,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))`

const noteChange = "note-body-seal"

// noteSpec is the repository declaration of note, as the generator gives it while the seal step is open.
func noteSpec() EntitySpec[note] {
	s := spec("Note", "", "notes", []string{"label", "body", "body_sealed"},
		func(row Scanner) (note, Meta, error) {
			var v note
			var m Meta
			sealingBody := SealingAt("notes", "body_sealed", &v.ID, &v.Body)
			err := row.Scan(&v.ID, &v.Label, sealingBody.Clear(), sealingBody.Sealed(), &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v note) []any {
			return []any{v.ID, v.Label, ClearOf(v.Body), SealAt("notes", "body_sealed", v.ID, v.Body)}
		},
		func(v note) map[string]any { return map[string]any{"id": v.ID, "label": v.Label} })
	s.Sealed = true
	return s
}

// noteSeal is the seal column of note, as the PostgreSQL adapter gives it; the test table keeps the ID as text.
var noteSeal = []SealColumn{{Change: noteChange, Entity: "Note", Table: "notes", Clear: "body", Sealed: "body_sealed",
	Select: "SELECT id, body::text, body_sealed FROM notes WHERE tenant_id = $1 AND id > $2 ORDER BY id LIMIT $3 FOR UPDATE",
	Update: "UPDATE notes SET body_sealed = $3 WHERE tenant_id = $1 AND id = $2"}}

// noteEnv is the command test env with the table notes, its entity, a runtime with the key ring, the release
// reports and the seal checkpoint of the change.
type noteEnv struct {
	*env
	notes *Entity[note]
	rt    *Runtime
	cmd   *Command[testProbe]
	ring  *sealed.Ring
	reads int
}

func newNoteEnv(t *testing.T) *noteEnv {
	t.Helper()
	e := newEnv(t)
	releases, err := lease.Releases(e.roles[0])
	if err != nil {
		t.Fatal(err)
	}
	seals, err := lease.SealsText(e.roles[0])
	if err != nil {
		t.Fatal(err)
	}
	e.exec(noteDDL, "GRANT SELECT, INSERT, UPDATE, DELETE ON notes TO "+e.roles[0],
		"ALTER TABLE notes ENABLE ROW LEVEL SECURITY", "ALTER TABLE notes FORCE ROW LEVEL SECURITY",
		"CREATE POLICY notes_tenant ON notes USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))",
		"CREATE SCHEMA IF NOT EXISTS stego_schema", releases, seals,
		"INSERT INTO stego_schema.seals (change_id, expand, created_at) VALUES ('"+noteChange+"', 'step_0002_note-body-seal_expand', clock_timestamp() - interval '10 minutes')",
		"INSERT INTO leases (name) VALUES ('"+SealLease(noteChange)+"')")
	entity, err := NewEntity(noteSpec())
	if err != nil {
		t.Fatal(err)
	}
	n := &noteEnv{env: e, notes: entity, ring: vaultRing(t, "1 "+throwawayKey(t))}
	n.rt = e.runtime(func(c *Config) { c.Entities = []Declared{n.notes}; c.Ring = n.ring })
	n.cmd = e.command("keep-note", nil, n.notes)
	return n
}

// write runs the command once in the tenant with the fill.
func (n *noteEnv) write(tenantID string, fill func(ctx context.Context, r *Repository[note]) error) error {
	n.t.Helper()
	n.reads++
	_, err := n.runOn(n.rt, n.cmd, n.request(tenantID, u7(n.t), fmt.Sprintf("w-%d", n.reads), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
		r, err := Repo(h, n.notes)
		if err != nil {
			return idempotency.Response{}, err
		}
		return ok, fill(ctx, r)
	})
	return err
}

// read gives the note of id in the tenant through a handle.
func (n *noteEnv) read(tenantID, id string) note {
	n.t.Helper()
	var got note
	if err := n.write(tenantID, func(ctx context.Context, r *Repository[note]) error {
		l, err := r.Get(ctx, id)
		if err != nil {
			return err
		}
		got = l.Value()
		return nil
	}); err != nil {
		n.t.Fatal(err)
	}
	return got
}

// opened gives the stored clear value and the opened sealed value of the row, and whether each is set.
func (n *noteEnv) opened(tenantID, id string) (clear string, clearSet bool, opened string, sealedSet bool) {
	n.t.Helper()
	var c *string
	var raw []byte
	if err := n.admin.QueryRowContext(n.ctx, `SELECT body, body_sealed FROM notes WHERE tenant_id = $1 AND id = $2`, tenantID, id).Scan(&c, &raw); err != nil {
		n.t.Fatal(err)
	}
	if c != nil {
		clear, clearSet = *c, true
	}
	if raw != nil {
		plain, err := n.ring.Open(raw, sealed.Place{Tenant: tenantID, Table: "notes", Column: "body_sealed", Row: id})
		if err != nil {
			return clear, clearSet, "<does not open>", true
		}
		opened, sealedSet = string(plain), true
	}
	return clear, clearSet, opened, sealedSet
}

// job gives the seal job of note over the tenants org-a and org-b.
func (n *noteEnv) job() *SealJob {
	n.t.Helper()
	j, err := NewSealJob(n.pool, "unused", n.ring, noteSeal, "holder-"+uuid.NewString(), lease.Settings{})
	if err != nil {
		n.t.Fatal(err)
	}
	j.tenants = func(context.Context) ([]tenant.Tenant, error) {
		return []tenant.Tenant{mustTenant(n.t, "org-b"), mustTenant(n.t, "org-a")}, nil
	}
	j.pause = 0
	return j
}

func TestSealStep(t *testing.T) {
	const secret = "member-0042@example.com"
	t.Run("ST-SUP-RP10-LC49-T01 While a seal step is open a write sets both columns and a read takes the clear value when the two differ", func(t *testing.T) {
		n := newNoteEnv(t)
		id, other := uuid.NewString(), uuid.NewString()
		for _, row := range []string{id, other} {
			if err := n.write("org-a", func(ctx context.Context, r *Repository[note]) error {
				_, err := r.Create(ctx, note{ID: row, Label: "x", Body: sealed.SecretOf(secret + row[:4])})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		if clear, _, opened, ok := n.opened("org-a", id); clear != secret+id[:4] || !ok || opened != clear {
			t.Fatalf("a create stores the clear %q and the sealed %q (%v), want both the value", clear, opened, ok)
		}
		if got := n.read("org-a", id); got.Body.Reveal() != secret+id[:4] {
			t.Fatal("a read does not give the value that the handle wrote")
		}
		// A release before writes the clear column alone: the read gives its value.
		n.exec(`UPDATE notes SET body = 'written-by-the-release-before' WHERE id = '` + id + `'`)
		if got := n.read("org-a", id); got.Body.Reveal() != "written-by-the-release-before" {
			t.Fatal("a read after a write of the release before does not give the clear value")
		}
		// A sealed value of another row does not open at this row: the read gives the clear value and no error.
		n.exec(`UPDATE notes SET body_sealed = (SELECT body_sealed FROM notes WHERE id = '` + other + `') WHERE id = '` + id + `'`)
		if got := n.read("org-a", id); got.Body.Reveal() != "written-by-the-release-before" {
			t.Fatal("a read of a sealed value that does not open does not give the clear value")
		}
		// A release before set the field to no value: the read gives no value.
		n.exec(`UPDATE notes SET body = NULL WHERE id = '` + id + `'`)
		if got := n.read("org-a", id); got.Body.IsSet() {
			t.Fatal("a read of a row with no clear value gives a value")
		}
		// An update of the handle writes both columns again.
		if err := n.write("org-a", func(ctx context.Context, r *Repository[note]) error {
			l, err := r.GetForUpdate(ctx, id)
			if err != nil {
				return err
			}
			v := l.Value()
			v.Body = sealed.SecretOf(secret)
			_, err = r.Update(ctx, l, v)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if clear, _, opened, _ := n.opened("org-a", id); clear != secret || opened != secret {
			t.Fatalf("an update stores the clear %q and the sealed %q, want both the value", clear, opened)
		}
		n.noSecretOutside(secret)
	})

	t.Run("ST-SUP-RP10-LC50-T01 The seal job seals and checks each row in passes under the backfill lease, goes on from its checkpoint and waits for a release before", func(t *testing.T) {
		n := newNoteEnv(t)
		const rows = 105
		ids := make([]string, rows)
		for i := range ids {
			ids[i] = uuid.NewString()
			tn := "org-a"
			if i%2 == 1 {
				tn = "org-b"
			}
			// The release before writes each row in clear.
			n.exec(fmt.Sprintf(`INSERT INTO notes (tenant_id, id, label, body) VALUES ('%s', '%s', 'x', '%s-%03d')`, tn, ids[i], secret, i))
		}
		n.exec(`UPDATE notes SET body = NULL WHERE id = '` + ids[3] + `'`)
		versions := n.count(`SELECT sum(version) FROM notes`)
		// A release before reports itself: the job starts no pass.
		n.exec(`INSERT INTO stego_schema.releases VALUES ('old', 'step_0001_x_expand', 'v1', 10000, clock_timestamp())`)
		results, err := n.job().Run(n.ctx)
		if err != nil || len(results) != 1 || !results[0].Waits || results[0].Checked != 0 {
			t.Fatalf("the job gave %+v, %v while a release before runs, want it to wait", results, err)
		}
		if left := n.count(`SELECT count(*) FROM notes WHERE body_sealed IS NOT NULL`); left != 0 {
			t.Fatalf("the job sealed %d rows while a release before runs", left)
		}
		n.exec(`UPDATE stego_schema.releases SET seen_at = clock_timestamp() - interval '1 minute'`)
		// The first run stops after its first batch; the second goes on from the checkpoint.
		j := n.job()
		ctx, cancel := context.WithCancel(n.ctx)
		j.batched = cancel
		if _, err := j.Run(ctx); err == nil {
			t.Fatal("the run that its context ended gives no error")
		}
		cancel()
		state := n.sealState()
		// The first batch is the 53 rows of org-a, the first tenant in order, fewer than SealBatch.
		if !state.Started || state.Completed || state.Checked != (rows+1)/2 || state.LastTenant != "org-a" {
			t.Fatalf("the checkpoint after one batch is %+v, want the %d rows of org-a checked", state, (rows+1)/2)
		}
		results, err = n.job().Run(n.ctx)
		if err != nil || len(results) != 1 || !results[0].Completed || results[0].Checked != rows || results[0].Sealed != rows-1 {
			t.Fatalf("the run from the checkpoint gave %+v, %v, want the pass completed with %d rows checked and %d sealed", results, err, rows, rows-1)
		}
		for i, id := range ids {
			tn := "org-a"
			if i%2 == 1 {
				tn = "org-b"
			}
			clear, clearSet, opened, sealedSet := n.opened(tn, id)
			if clearSet != sealedSet || opened != clear {
				t.Fatalf("the row %d holds the clear %q and the sealed %q", i, clear, opened)
			}
		}
		if n.count(`SELECT sum(version) FROM notes`) != versions {
			t.Fatal("the seal job changed the version of a row")
		}
		if again, err := n.job().Run(n.ctx); err != nil || !again[0].Completed {
			t.Fatalf("a run after the pass gave %+v, %v", again, err)
		}
		// A roll back: a release before runs again and writes the clear column of a sealed row and clears another.
		n.exec(`UPDATE stego_schema.releases SET seen_at = clock_timestamp()`)
		n.exec(`UPDATE notes SET body = 'rolled-back' WHERE id = '` + ids[0] + `'`)
		n.exec(`UPDATE notes SET body = NULL WHERE id = '` + ids[1] + `'`)
		if state := n.sealState(); !state.Stale {
			t.Fatalf("the checkpoint after a run of a release before is %+v, want stale", state)
		}
		if waits, err := n.job().Run(n.ctx); err != nil || !waits[0].Waits {
			t.Fatalf("the job gave %+v, %v while the release before runs again, want it to wait", waits, err)
		}
		// The pass completed ten minutes ago, and the release before stopped one minute ago, after it: the
		// checkpoint stays stale, and the job starts a pass again. A report never moves back in time.
		n.exec(`UPDATE stego_schema.seals SET pass_started = pass_started - interval '10 minutes', completed_at = completed_at - interval '10 minutes'`,
			`UPDATE stego_schema.releases SET seen_at = clock_timestamp() - interval '1 minute'`)
		if state := n.sealState(); !state.Stale || state.Waiting {
			t.Fatalf("the checkpoint after the release before stopped is %+v, want stale and not waiting", state)
		}
		results, err = n.job().Run(n.ctx)
		if err != nil || !results[0].Completed || results[0].Sealed != 2 || results[0].Checked != rows {
			t.Fatalf("the pass after the roll back gave %+v, %v, want 2 rows sealed again", results, err)
		}
		if clear, _, opened, _ := n.opened("org-a", ids[0]); opened != "rolled-back" || clear != opened {
			t.Fatalf("the row that the release before wrote holds the sealed %q", opened)
		}
		if _, _, _, sealedSet := n.opened("org-b", ids[1]); sealedSet {
			t.Fatal("the row that the release before cleared keeps its sealed value")
		}
		n.noSecretOutside(secret)
	})
}

// sealState reads the seal checkpoint of the note change.
func (n *noteEnv) sealState() lease.SealState {
	n.t.Helper()
	tx, err := n.pool.BeginTx(n.ctx, nil)
	if err != nil {
		n.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	s, err := lease.ReadSeal(n.ctx, tx, noteChange)
	if err != nil {
		n.t.Fatal(err)
	}
	return s
}

// noSecretOutside fails when a row of the audit, outbox or idempotency tables, as text, holds the value.
func (n *noteEnv) noSecretOutside(value string) {
	n.t.Helper()
	for _, table := range []string{"audit_event", "stego_outbox.messages", "idempotency_keys"} {
		rows, err := n.admin.QueryContext(n.ctx, `SELECT t::text FROM `+table+` t`)
		if err != nil {
			n.t.Fatalf("%s: %v", table, err)
		}
		for rows.Next() {
			var text []byte
			if err := rows.Scan(&text); err != nil {
				n.t.Fatal(err)
			}
			if bytes.Contains(text, []byte(value)) {
				n.t.Fatalf("a row of %s holds a secret value", table)
			}
		}
		_ = rows.Close()
	}
}
