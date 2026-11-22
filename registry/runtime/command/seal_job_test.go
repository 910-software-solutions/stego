package command

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
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
	// The entity after the contract step of the seal, with the sealed column alone, as the generator gives it
	// (ST-SUP-RP10-LC49).
	after := spec("Note", "", "notes", []string{"label", "body_sealed"},
		func(row Scanner) (note, Meta, error) {
			var v note
			var m Meta
			err := row.Scan(&v.ID, &v.Label, OpenAt("notes", "body_sealed", &v.ID, &v.Body), &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v note) []any { return []any{v.ID, v.Label, SealAt("notes", "body_sealed", v.ID, v.Body)} }, nil)
	s.AfterSeal = &AfterSeal[note]{Table: "public.notes", Clear: []string{"body"}, Changes: []string{noteChange}, Select: after.Select, SelectForUpdate: after.SelectForUpdate,
		SelectForDelete: after.SelectForDelete, Insert: after.Insert, Update: after.Update, Scan: after.Scan, Values: after.Values}
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
	t.Run("ST-SUP-RP10-LC50-T04 After a restore the seal job waits 3 minutes from its first run in this database before a pass", func(t *testing.T) {
		n := newNoteEnv(t)
		n.exec(fmt.Sprintf(`INSERT INTO notes (tenant_id, id, label, body) VALUES ('org-a', '%s', 'x', '%s-restored')`, uuid.NewString(), secret))
		// The row comes from a dump of another database: ten minutes old, and seen last in another instance.
		n.exec(`UPDATE stego_schema.seals SET armed_in = '1/20261101000000000000', armed_at = clock_timestamp() - interval '9 minutes'`)
		for run := range 2 {
			results, err := n.job().Run(n.ctx)
			if err != nil || len(results) != 1 || !results[0].Waits || results[0].Checked != 0 {
				t.Fatalf("run %d of the job on a restored seal gave %+v, %v, want it to wait", run, results, err)
			}
		}
		// The first run committed the guard time of this database: it is not the one of the dump.
		if fresh := n.count(`SELECT count(*) FROM stego_schema.seals WHERE armed_in = stego_schema.seal_instance() AND armed_at > clock_timestamp() - interval '1 minute'`); fresh != 1 {
			t.Fatalf("%d seal rows hold a guard time of this database after the first run, want 1", fresh)
		}
		n.exec(`UPDATE stego_schema.seals SET armed_at = armed_at - interval '4 minutes'`)
		results, err := n.job().Run(n.ctx)
		if err != nil || !results[0].Completed || results[0].Sealed != 1 {
			t.Fatalf("the run 3 minutes after the first sight gave %+v, %v, want the pass completed", results, err)
		}
		n.noSecretOutside(secret)
	})
	t.Run("ST-SUP-RP10-LC50-T05 After a restore of a completed pass the seal job starts a new pass and seals the rows again", func(t *testing.T) {
		n := newNoteEnv(t)
		id := uuid.NewString()
		n.exec(fmt.Sprintf(`INSERT INTO notes (tenant_id, id, label, body) VALUES ('org-a', '%s', 'x', '%s-first')`, id, secret))
		results, err := n.job().Run(n.ctx)
		if err != nil || !results[0].Completed || results[0].Sealed != 1 {
			t.Fatalf("the first pass gave %+v, %v, want it completed", results, err)
		}
		// A restore brings the completed pass from a dump of another database instance, and a process of a release
		// before that has not reported itself yet writes the clear column alone.
		n.exec(`UPDATE stego_schema.seals SET armed_in = '1/20261101000000000000/1/1', armed_at = clock_timestamp() - interval '9 minutes'`)
		n.exec(fmt.Sprintf(`UPDATE notes SET body = '%s-after' WHERE id = '%s'`, secret, id))
		results, err = n.job().Run(n.ctx)
		if err != nil || len(results) != 1 || results[0].Completed || !results[0].Waits {
			t.Fatalf("the first run after the restore gave %+v, %v, want it to wait and not to report the pass completed", results, err)
		}
		if state := n.sealState(); state.Started || state.Completed {
			t.Fatalf("the checkpoint after the first run is %+v, want no pass", state)
		}
		n.exec(`UPDATE stego_schema.seals SET armed_at = armed_at - interval '4 minutes'`)
		results, err = n.job().Run(n.ctx)
		if err != nil || !results[0].Completed || results[0].Sealed != 1 || results[0].Checked != 1 {
			t.Fatalf("the run 3 minutes after the first sight gave %+v, %v, want a new pass that seals the row again", results, err)
		}
		if clear, _, opened, _ := n.opened("org-a", id); opened != secret+"-after" || clear != opened {
			t.Fatalf("the row holds the clear %q and the sealed %q after the new pass", clear, opened)
		}
		n.noSecretOutside(secret)
	})
}

// TestSealContract holds the release of the expand step of a seal after its contract step (walkthrough 14, W14-B6
// and W14-B7).
func TestSealContract(t *testing.T) {
	const secret = "member-0042@example.com"
	t.Run("ST-SUP-RP10-LC49-T02 After the contract step drops the clear column, the release of the expand step reads and writes the sealed column alone", func(t *testing.T) {
		n := newNoteEnv(t)
		id := uuid.NewString()
		if err := n.write("org-a", func(ctx context.Context, r *Repository[note]) error {
			_, err := r.Create(ctx, note{ID: id, Label: "x", Body: sealed.SecretOf(secret + "-before")})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// A 42703 while the clear column is there changes nothing: an entity whose read names a column that the
		// table does not have fails, and keeps its statements.
		broken := noteSpec()
		broken.Select = strings.Replace(broken.Select, "body_sealed", "missing_column", 1)
		wrong, err := NewEntity(broken)
		if err != nil {
			t.Fatal(err)
		}
		rt := n.runtime(func(c *Config) { c.Entities = []Declared{wrong}; c.Ring = n.ring })
		cmd := n.command("read-wrong", nil, wrong)
		if _, err := n.runOn(rt, cmd, n.request("org-a", u7(t), "wrong", `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			r, err := Repo(h, wrong)
			if err != nil {
				return idempotency.Response{}, err
			}
			_, err = r.Get(ctx, id)
			return ok, err
		}); !errors.Is(err, ErrInternal) || wrong.contracted.Load() {
			t.Fatalf("a read of an undefined column while the clear column is there gives %v and takes the statements after the contract: %v", err, wrong.contracted.Load())
		}
		// The contract step drops the clear column and deletes the checkpoint of the change.
		n.exec(`ALTER TABLE notes DROP COLUMN body`, `DELETE FROM stego_schema.seals WHERE change_id = '`+noteChange+`'`)
		// The read, the create and the update of the release of the expand step go on, each in one call, with no
		// declared Retry on conflict: the first statement fails, and the call runs again with the sealed column.
		if got := n.read("org-a", id); got.Body.Reveal() != secret+"-before" {
			t.Fatal("a read after the contract does not give the sealed value")
		}
		if !n.notes.contracted.Load() {
			t.Fatal("the entity did not take its statements after the contract")
		}
		other := uuid.NewString()
		if err := n.write("org-a", func(ctx context.Context, r *Repository[note]) error {
			_, err := r.Create(ctx, note{ID: other, Label: "y", Body: sealed.SecretOf(secret + "-after")})
			return err
		}); err != nil {
			t.Fatalf("a create after the contract fails: %v", err)
		}
		if err := n.write("org-a", func(ctx context.Context, r *Repository[note]) error {
			l, err := r.GetForUpdate(ctx, id)
			if err != nil {
				return err
			}
			v := l.Value()
			v.Body = sealed.SecretOf(secret + "-updated")
			_, err = r.Update(ctx, l, v)
			return err
		}); err != nil {
			t.Fatalf("an update after the contract fails: %v", err)
		}
		for row, want := range map[string]string{id: secret + "-updated", other: secret + "-after"} {
			var raw []byte
			if err := n.admin.QueryRowContext(n.ctx, `SELECT body_sealed FROM notes WHERE tenant_id = 'org-a' AND id = $1`, row).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			plain, err := n.ring.Open(raw, sealed.Place{Tenant: "org-a", Table: "notes", Column: "body_sealed", Row: row})
			if err != nil || string(plain) != want {
				t.Fatalf("the row %s holds a sealed value that does not open to its value: %v", row, err)
			}
		}
		// A second process of the same release starts with the statements of the spec, and takes the statements after
		// the contract at its first statement too.
		fresh, err := NewEntity(noteSpec())
		if err != nil {
			t.Fatal(err)
		}
		rt = n.runtime(func(c *Config) { c.Entities = []Declared{fresh}; c.Ring = n.ring })
		cmd = n.command("read-fresh", nil, fresh)
		var got note
		if _, err := n.runOn(rt, cmd, n.request("org-a", u7(t), "fresh", `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			r, err := Repo(h, fresh)
			if err != nil {
				return idempotency.Response{}, err
			}
			l, err := r.Get(ctx, other)
			if err == nil {
				got = l.Value()
			}
			return ok, err
		}); err != nil || got.Body.Reveal() != secret+"-after" {
			t.Fatalf("a new process of the release reads %v after the contract", err)
		}
		n.noSecretOutside(secret)
	})

	t.Run("ST-SUP-RP10-LC49-T03 A call that began before another call switched the entity, and then fails with an undefined column, runs again with the statements after the contract", func(t *testing.T) {
		n := newNoteEnv(t)
		undefined := &pgconn.PgError{Code: undefinedColumn}
		// The clear column is there: an undefined column of another cause switches nothing.
		if n.notes.switchAfterSeal(n.ctx, n.pool, undefined, sealSwitches.Load()) || n.notes.contracted.Load() {
			t.Fatal("an undefined column while the clear column is there switches the entity")
		}
		began := sealSwitches.Load()
		n.exec(`ALTER TABLE notes DROP COLUMN body`)
		// One call switches the entity.
		if !n.notes.switchAfterSeal(n.ctx, n.pool, undefined, sealSwitches.Load()) || !n.notes.contracted.Load() {
			t.Fatal("the first undefined column after the contract does not switch the entity")
		}
		// A call that began before that switch ran a statement before the contract: the same conflict, which Run runs
		// again once (security review of PR #452, L2).
		if !n.notes.switchAfterSeal(n.ctx, n.pool, undefined, began) {
			t.Fatal("a call that began before the switch and failed with an undefined column is not the conflict of the switch")
		}
		state := &state{rt: n.rt, sealSwitches: began}
		if err := state.failOf(n.ctx, n.notes, "read Note", undefined); !sealSwitched(err) {
			t.Fatalf("failOf of a call that began before the switch gives %v, want the conflict of the switch", err)
		}
		// A call that began after the switch ran the statements after the contract: its undefined column is its own
		// failure, and another error is never the switch.
		if n.notes.switchAfterSeal(n.ctx, n.pool, undefined, sealSwitches.Load()) {
			t.Fatal("a call that began after the switch takes its undefined column as the switch")
		}
		if n.notes.switchAfterSeal(n.ctx, n.pool, &pgconn.PgError{Code: "23505"}, began) {
			t.Fatal("another SQLSTATE is the switch")
		}
	})

	t.Run("ST-SUP-RP10-LC50-T02 The seal job has no work after the contract step and fails with the remedy when its lease row is missing", func(t *testing.T) {
		n := newNoteEnv(t)
		n.exec(`INSERT INTO notes (tenant_id, id, label, body) VALUES ('org-a', '` + uuid.NewString() + `', 'x', '` + secret + `')`)
		// The lease row is not there, as on a database that the job of a later release took past the expand step:
		// the run fails at once and names the lease, and seals nothing.
		n.exec(`DELETE FROM leases WHERE name = '` + SealLease(noteChange) + `'`)
		if _, err := n.job().Run(n.ctx); !errors.Is(err, ErrSealLease) || !strings.Contains(err.Error(), SealLease(noteChange)) {
			t.Fatalf("a run with no lease row gives %v, want ErrSealLease with the lease", err)
		}
		if sealedRows := n.count(`SELECT count(*) FROM notes WHERE body_sealed IS NOT NULL`); sealedRows != 0 {
			t.Fatalf("the run with no lease row sealed %d rows", sealedRows)
		}
		n.exec(`INSERT INTO leases (name) VALUES ('` + SealLease(noteChange) + `')`)
		if results, err := n.job().Run(n.ctx); err != nil || !results[0].Completed {
			t.Fatalf("a run after the lease row is made gives %+v, %v, want the pass completed", results, err)
		}
		// The contract step drops the clear column and deletes the checkpoint: the run has no work and no error.
		n.exec(`ALTER TABLE notes DROP COLUMN body`, `DELETE FROM stego_schema.seals WHERE change_id = '`+noteChange+`'`)
		for range 2 {
			if results, err := n.job().Run(n.ctx); err != nil || len(results) != 1 || !results[0].Completed || results[0].Checked != 0 {
				t.Fatalf("a run after the contract gives %+v, %v, want no work", results, err)
			}
		}
	})

	t.Run("ST-SUP-RP10-LC49-T04 A process that took its statements after the contract writes the clear column again once a restore brings the column back", func(t *testing.T) {
		n := newNoteEnv(t)
		id := uuid.NewString()
		if err := n.write("org-a", func(ctx context.Context, r *Repository[note]) error {
			_, err := r.Create(ctx, note{ID: id, Label: "x", Body: sealed.SecretOf(secret + "-before")})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// The contract drops the clear column, and the process takes its statements after the contract.
		n.exec(`ALTER TABLE notes DROP COLUMN body`, `DELETE FROM stego_schema.seals WHERE change_id = '`+noteChange+`'`)
		if got := n.read("org-a", id); got.Body.Reveal() != secret+"-before" || !n.notes.contracted.Load() {
			t.Fatal("the entity did not take its statements after the contract")
		}
		// A restore of a backup from before the contract brings back the clear column, its value and the checkpoint,
		// while the process runs on.
		n.exec(`ALTER TABLE notes ADD COLUMN body text`, `UPDATE notes SET body = '`+secret+`-before'`,
			"INSERT INTO stego_schema.seals (change_id, expand, created_at) VALUES ('"+noteChange+"', 'step_0002_note-body-seal_expand', clock_timestamp() - interval '10 minutes')")
		// The next catalog check is due, as sealRecheck after the last one.
		n.notes.recheckAt.Store(0)
		if err := n.write("org-a", func(ctx context.Context, r *Repository[note]) error {
			l, err := r.GetForUpdate(ctx, id)
			if err != nil {
				return err
			}
			v := l.Value()
			v.Body = sealed.SecretOf(secret + "-updated")
			_, err = r.Update(ctx, l, v)
			return err
		}); err != nil {
			t.Fatalf("an update after the restore fails: %v", err)
		}
		if n.notes.contracted.Load() {
			t.Fatal("the entity keeps its statements after the contract on a table with the clear column")
		}
		// The update wrote both columns, so a read that takes the clear value first, and the seal job, keep it.
		if clear, clearSet, opened, _ := n.opened("org-a", id); !clearSet || clear != secret+"-updated" || opened != secret+"-updated" {
			t.Fatalf("after the restore the update leaves the clear column set %t and the sealed column opening to its value %t", clearSet && clear == secret+"-updated", opened == secret+"-updated")
		}
		if got := n.read("org-a", id); got.Body.Reveal() != secret+"-updated" {
			t.Fatal("a read after the restore does not give the updated value")
		}
		n.noSecretOutside(secret)
	})

	t.Run("ST-SUP-RP10-LC49-T06 Two declarations of one entity, as the command contract and the rest-api data package give them, each switch after the contract and each go back after a restore", func(t *testing.T) {
		n := newNoteEnv(t)
		// The second declaration of Note, on a runtime of its own, as a process holds the copy of the command
		// contract and the copy of internal/api/data; its idempotency keys differ from those of the first.
		other, err := NewEntity(noteSpec())
		if err != nil {
			t.Fatal(err)
		}
		m := &noteEnv{env: n.env, notes: other, ring: n.ring, reads: 1000}
		m.rt = n.env.runtime(func(c *Config) { c.Entities = []Declared{other}; c.Ring = n.ring })
		m.cmd = n.env.command("keep-note-copy", nil, other)
		id := uuid.NewString()
		if err := n.write("org-a", func(ctx context.Context, r *Repository[note]) error {
			_, err := r.Create(ctx, note{ID: id, Label: "x", Body: sealed.SecretOf(secret + "-before")})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		n.exec(`ALTER TABLE notes DROP COLUMN body`, `DELETE FROM stego_schema.seals WHERE change_id = '`+noteChange+`'`)
		for i, d := range []*noteEnv{n, m} {
			if got := d.read("org-a", id); got.Body.Reveal() != secret+"-before" || !d.notes.contracted.Load() {
				t.Fatalf("declaration %d did not take its statements after the contract", i+1)
			}
		}
		n.exec(`ALTER TABLE notes ADD COLUMN body text`, `UPDATE notes SET body = '`+secret+`-before'`,
			"INSERT INTO stego_schema.seals (change_id, expand, created_at) VALUES ('"+noteChange+"', 'step_0002_note-body-seal_expand', clock_timestamp() - interval '10 minutes')")
		// Each declaration checks the catalog at its own next attempt, and the reset of one leaves the other as it is
		// until then.
		n.notes.recheckAt.Store(0)
		other.recheckAt.Store(0)
		_ = n.read("org-a", id)
		if n.notes.contracted.Load() || !other.contracted.Load() {
			t.Fatalf("after an attempt of the first declaration, it is contracted %t and the second %t, want false and true", n.notes.contracted.Load(), other.contracted.Load())
		}
		if err := m.write("org-a", func(ctx context.Context, r *Repository[note]) error {
			l, err := r.GetForUpdate(ctx, id)
			if err != nil {
				return err
			}
			v := l.Value()
			v.Body = sealed.SecretOf(secret + "-updated")
			_, err = r.Update(ctx, l, v)
			return err
		}); err != nil {
			t.Fatalf("an update of the second declaration after the restore fails: %v", err)
		}
		if other.contracted.Load() {
			t.Fatal("the second declaration keeps its statements after the contract on a table with the clear column")
		}
		// The update of the second declaration wrote both columns, so the first reads its value.
		if clear, clearSet, opened, _ := n.opened("org-a", id); !clearSet || clear != secret+"-updated" || opened != secret+"-updated" {
			t.Fatalf("the update of the second declaration leaves the clear column set %t and the sealed column opening to its value %t", clearSet && clear == secret+"-updated", opened == secret+"-updated")
		}
		if got := n.read("org-a", id); got.Body.Reveal() != secret+"-updated" {
			t.Fatal("the first declaration does not read the value that the second wrote")
		}
		n.noSecretOutside(secret)
	})

	t.Run("ST-SUP-RP10-LC49-T05 A process that took its statements after the contract keeps the sealed column alone when a clear column comes back with no seal checkpoint", func(t *testing.T) {
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		t.Cleanup(func() { slog.SetDefault(previous) })
		n := newNoteEnv(t)
		id := uuid.NewString()
		if err := n.write("org-a", func(ctx context.Context, r *Repository[note]) error {
			_, err := r.Create(ctx, note{ID: id, Label: "x", Body: sealed.SecretOf(secret + "-before")})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		n.exec(`ALTER TABLE notes DROP COLUMN body`, `DELETE FROM stego_schema.seals WHERE change_id = '`+noteChange+`'`)
		if got := n.read("org-a", id); got.Body.Reveal() != secret+"-before" || !n.notes.contracted.Load() {
			t.Fatal("the entity did not take its statements after the contract")
		}
		// A person adds a column of the clear name by hand; no checkpoint row comes back.
		n.exec(`ALTER TABLE notes ADD COLUMN body text`)
		for i := range 2 {
			n.notes.recheckAt.Store(0)
			if err := n.write("org-a", func(ctx context.Context, r *Repository[note]) error {
				l, err := r.GetForUpdate(ctx, id)
				if err != nil {
					return err
				}
				v := l.Value()
				v.Body = sealed.SecretOf(fmt.Sprintf("%s-stray-%d", secret, i))
				_, err = r.Update(ctx, l, v)
				return err
			}); err != nil {
				t.Fatalf("an update with a stray clear column fails: %v", err)
			}
		}
		if !n.notes.contracted.Load() {
			t.Fatal("the entity takes the statements with the clear column again with no seal checkpoint")
		}
		if clear, clearSet, opened, _ := n.opened("org-a", id); clearSet || opened != secret+"-stray-1" {
			t.Fatalf("the stray clear column is set %t (%t), and the sealed column holds the update %t", clearSet, clear != "", opened == secret+"-stray-1")
		}
		if got := strings.Count(logs.String(), "with no seal checkpoint"); got != 1 {
			t.Fatalf("the stray clear column is logged %d times, want once", got)
		}
		n.noSecretOutside(secret)
	})

	t.Run("ST-SUP-RP10-LC50-T03 The seal job with no checkpoint completes nothing while the clear column is there, and logs the contract again after a restore", func(t *testing.T) {
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		t.Cleanup(func() { slog.SetDefault(previous) })
		n := newNoteEnv(t)
		n.exec(`INSERT INTO notes (tenant_id, id, label, body) VALUES ('org-a', '` + uuid.NewString() + `', 'x', '` + secret + `')`)
		// The checkpoint row is gone while the clear column is there, as before the migration job of the release:
		// the run is no completed pass, fails with the remedy and seals nothing.
		n.exec(`DELETE FROM stego_schema.seals WHERE change_id = '` + noteChange + `'`)
		j := n.job()
		results, err := j.Run(n.ctx)
		if !errors.Is(err, ErrSealCheckpoint) || len(results) != 1 || results[0].Completed {
			t.Fatalf("a run with no checkpoint and the clear column gives %+v, %v, want ErrSealCheckpoint and no completed pass", results, err)
		}
		if sealedRows := n.count(`SELECT count(*) FROM notes WHERE body_sealed IS NOT NULL`); sealedRows != 0 {
			t.Fatalf("the run with no checkpoint sealed %d rows", sealedRows)
		}
		// A second run fails the same way and logs nothing more (security review of PR #459, I4).
		if _, err := j.Run(n.ctx); !errors.Is(err, ErrSealCheckpoint) {
			t.Fatalf("a second run with no checkpoint gives %v", err)
		}
		if got := strings.Count(logs.String(), "has no checkpoint while its clear column is there"); got != 1 {
			t.Fatalf("the run with no checkpoint is logged %d times, want once", got)
		}
		// The contract drops the clear column: the job has no work and takes the change as contracted.
		n.exec(`ALTER TABLE notes DROP COLUMN body`)
		if results, err := j.Run(n.ctx); err != nil || !results[0].Completed || !j.contracted[noteChange] {
			t.Fatalf("a run after the contract gives %+v, %v, contracted %t, want no work", results, err, j.contracted[noteChange])
		}
		// A restore from before the contract brings the column and the checkpoint back: the job seals the row again
		// and no longer takes the change as contracted.
		n.exec(`ALTER TABLE notes ADD COLUMN body text`, `UPDATE notes SET body = '`+secret+`'`,
			"INSERT INTO stego_schema.seals (change_id, expand, created_at) VALUES ('"+noteChange+"', 'step_0002_note-body-seal_expand', clock_timestamp() - interval '10 minutes')")
		if results, err := j.Run(n.ctx); err != nil || !results[0].Completed || results[0].Sealed != 1 || j.contracted[noteChange] {
			t.Fatalf("a run after the restore gives %+v, %v, contracted %t, want the row sealed", results, err, j.contracted[noteChange])
		}
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

// payoutChange is the relate step of the test: release 2 gives each payout the relation tenant (walkthrough 24,
// P2-R2), and the relation approver from the field approver_id.
const payoutChange, approverChange = "relate-payout-tenant", "relate-payout-approver"

// payoutRelates are the relate changes of the payouts, as the PostgreSQL adapter gives them; the test table keeps the
// ID as text.
var payoutRelates = []RelateChange{
	{Change: payoutChange, Entity: "Payout", ResourceType: "payout", Relation: "tenant", Subject: RelateTenant,
		Select: "SELECT id, NULL::text FROM payouts WHERE tenant_id = $1 AND ($2::text IS NULL OR id > $2) ORDER BY id LIMIT $3 FOR SHARE"},
	{Change: approverChange, Entity: "Payout", ResourceType: "payout", Relation: "approver", Subject: RelateField, SubjectType: "user",
		Select: "SELECT id, approver_id FROM payouts WHERE tenant_id = $1 AND ($2::text IS NULL OR id > $2) ORDER BY id LIMIT $3 FOR SHARE"},
}

func TestRelateStep(t *testing.T) {
	t.Run("ST-SUP-RP10-LC50-T06 The relate job gives each row the tuple of the new relation, also the rows of a release before during the rollout, and only after it stopped", func(t *testing.T) {
		e := newEnv(t)
		releases, err := lease.Releases(e.roles[0])
		if err != nil {
			t.Fatal(err)
		}
		seals, err := lease.SealsText(e.roles[0])
		if err != nil {
			t.Fatal(err)
		}
		e.exec(`CREATE TABLE payouts (tenant_id text NOT NULL, id text NOT NULL, approver_id text, PRIMARY KEY (tenant_id, id))`,
			"GRANT SELECT, INSERT, UPDATE, DELETE ON payouts TO "+e.roles[0],
			"ALTER TABLE payouts ENABLE ROW LEVEL SECURITY", "ALTER TABLE payouts FORCE ROW LEVEL SECURITY",
			"CREATE POLICY payouts_tenant ON payouts USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))",
			"CREATE SCHEMA IF NOT EXISTS stego_schema", releases, seals)
		// The expand migrations of the two relate steps of release 2, as the PostgreSQL adapter gives them.
		for _, c := range payoutRelates {
			e.exec("INSERT INTO stego_schema.seals (change_id, expand, created_at) VALUES ('"+c.Change+"', 'step_0002_"+c.Change+"_expand', clock_timestamp() - interval '10 minutes')",
				"INSERT INTO leases (name) VALUES ('"+SealLease(c.Change)+"')")
		}
		insert := func(tenantID string, n int, approver bool) []string {
			t.Helper()
			var ids []string
			for range n {
				id := uuid.NewString()
				value := "NULL"
				if approver {
					value = "'amir'"
				}
				e.exec(fmt.Sprintf(`INSERT INTO payouts (tenant_id, id, approver_id) VALUES ('%s', '%s', %s)`, tenantID, id, value))
				ids = append(ids, id)
			}
			return ids
		}
		// Rows of the release before the step, made before the release, the first with the zero UUID.
		e.exec(`INSERT INTO payouts (tenant_id, id, approver_id) VALUES ('t1', '00000000-0000-0000-0000-000000000000', 'amir')`)
		before := append([]string{"00000000-0000-0000-0000-000000000000"}, append(insert("t1", 3, true), insert("t2", 2, false)...)...)
		job, err := NewRelateJob(RelateJobConfig{DB: e.pool, Schema: "unused", Authz: e.authz, Audit: e.writer, Service: "payouts",
			Changes: payoutRelates, Holder: "holder-" + uuid.NewString(), Lease: lease.Settings{}})
		if err != nil {
			t.Fatal(err)
		}
		job.tenants = func(context.Context) ([]tenant.Tenant, error) {
			return []tenant.Tenant{mustTenant(t, "t2"), mustTenant(t, "t1")}, nil
		}
		job.pause = 0
		staged := func(relation string) map[string]string {
			t.Helper()
			rows, err := e.admin.QueryContext(e.ctx, `SELECT p->'tuple'->>'resource_id', p->'tuple'->>'subject_type', p->'tuple'->>'subject_id'
 FROM (SELECT convert_from(payload, 'UTF8')::jsonb AS p FROM stego_outbox.messages WHERE destination = 'authz') m
 WHERE p->>'op' = 'touch' AND p->'tuple'->>'resource_type' = 'payout' AND p->'tuple'->>'relation' = $1`, relation)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			out := map[string]string{}
			for rows.Next() {
				var resource, subjectType, subject string
				if err := rows.Scan(&resource, &subjectType, &subject); err != nil {
					t.Fatal(err)
				}
				_, key, _ := strings.Cut(resource, "/")
				raw, err := base64.RawURLEncoding.DecodeString(key)
				if err != nil {
					t.Fatal(err)
				}
				out[string(raw)] = subjectType
			}
			return out
		}
		// The rollout: a process of release 1 reports itself, so the job waits and stages nothing.
		e.exec(`INSERT INTO stego_schema.releases VALUES ('old', 'step_0001_x_expand', 'v1', 10000, clock_timestamp())`)
		results, err := job.Run(e.ctx)
		if err != nil || len(results) != 2 || !results[0].Waits || !results[1].Waits || len(staged("tenant")) != 0 {
			t.Fatalf("the job while release 1 runs gave %+v, %v and staged %d tuples, want it to wait", results, err, len(staged("tenant")))
		}
		// Release 1 makes rows during the rollout, and then stops: its report goes stale.
		during := insert("t1", 2, true)
		e.exec(`UPDATE stego_schema.releases SET seen_at = clock_timestamp() - interval '1 minute'`)
		// The approver of a payout is a field subject; a row with no approver gets no tuple.
		results, err = job.Run(e.ctx)
		if err != nil || len(results) != 2 || !results[0].Completed || results[0].Checked != 8 || results[0].Related != 8 ||
			!results[1].Completed || results[1].Checked != 8 || results[1].Related != 6 {
			t.Fatalf("the job after release 1 stopped gave %+v, %v, want 8 rows read and 8 tenant and 6 approver tuples staged", results, err)
		}
		tenants, approvers := staged("tenant"), staged("approver")
		for _, id := range append(before, during...) {
			if tenants[id] != "tenant" {
				t.Errorf("the row %s has no staged tuple of the relation tenant", id)
			}
		}
		for _, id := range append(before[:4], during...) {
			if approvers[id] != "user" {
				t.Errorf("the row %s has no staged tuple of the relation approver", id)
			}
		}
		if len(tenants) != 8 || len(approvers) != 6 {
			t.Fatalf("the job staged %d tenant and %d approver tuples, want 8 and 6", len(tenants), len(approvers))
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE action = 'backfill-relation'`); n != 3 {
			t.Fatalf("the job wrote %d audit rows, want one for each tenant and change that staged tuples: t1 and t2 of tenant and t1 of approver", n)
		}
		// A completed pass has no work: a run again stages nothing.
		messages := e.count(`SELECT count(*) FROM stego_outbox.messages WHERE destination = 'authz'`)
		if results, err := job.Run(e.ctx); err != nil || !results[0].Completed || e.count(`SELECT count(*) FROM stego_outbox.messages WHERE destination = 'authz'`) != messages {
			t.Fatalf("a run after the completed pass gave %+v, %v and staged more tuples", results, err)
		}
	})
	t.Run("ST-SUP-RP10-LC50-T07 A pass during which a release before reported itself starts again, so a row that it made later gets the tuple", func(t *testing.T) {
		e := newEnv(t)
		releases, err := lease.Releases(e.roles[0])
		if err != nil {
			t.Fatal(err)
		}
		seals, err := lease.SealsText(e.roles[0])
		if err != nil {
			t.Fatal(err)
		}
		e.exec(`CREATE TABLE payouts (tenant_id text NOT NULL, id text NOT NULL, approver_id text, PRIMARY KEY (tenant_id, id))`,
			"GRANT SELECT, INSERT, UPDATE, DELETE ON payouts TO "+e.roles[0],
			"CREATE SCHEMA IF NOT EXISTS stego_schema", releases, seals,
			"INSERT INTO stego_schema.seals (change_id, expand, created_at) VALUES ('"+payoutChange+"', 'step_0002_"+payoutChange+"_expand', clock_timestamp() - interval '10 minutes')",
			"INSERT INTO leases (name) VALUES ('"+SealLease(payoutChange)+"')")
		e.exec(fmt.Sprintf(`INSERT INTO payouts (tenant_id, id) VALUES ('t1', '%s')`, uuid.NewString()))
		job, err := NewRelateJob(RelateJobConfig{DB: e.pool, Schema: "unused", Authz: e.authz, Audit: e.writer, Service: "payouts",
			Changes: payoutRelates[:1], Holder: "holder-" + uuid.NewString(), Lease: lease.Settings{}})
		if err != nil {
			t.Fatal(err)
		}
		job.tenants = func(context.Context) ([]tenant.Tenant, error) { return []tenant.Tenant{mustTenant(t, "t1")}, nil }
		job.pause = 0
		late := uuid.NewString()
		// A release before restarts after the pass started, as at a roll back: its report is later than the start.
		ran := false
		job.tenants = func(context.Context) ([]tenant.Tenant, error) {
			if !ran {
				ran = true
				e.exec(`INSERT INTO stego_schema.releases VALUES ('old', 'step_0001_x_expand', 'v1', 10000, clock_timestamp())`)
			}
			return []tenant.Tenant{mustTenant(t, "t1")}, nil
		}
		results, err := job.Run(e.ctx)
		if err != nil || results[0].Completed {
			t.Fatalf("the pass during which a release before reported itself gave %+v, %v, want it not completed", results, err)
		}
		// It makes a row with no tuple, and stops.
		e.exec(fmt.Sprintf(`INSERT INTO payouts (tenant_id, id) VALUES ('t1', '%s')`, late),
			`UPDATE stego_schema.releases SET seen_at = clock_timestamp() - interval '1 minute'`)
		results, err = job.Run(e.ctx)
		if err != nil || !results[0].Completed || results[0].Related != 2 {
			t.Fatalf("the pass again gave %+v, %v, want it completed with 2 tuples", results, err)
		}
		if n := e.count(`SELECT count(*) FROM stego_outbox.messages WHERE destination = 'authz' AND convert_from(payload, 'UTF8')::jsonb->'tuple'->>'resource_id' LIKE '%' || $1`,
			base64.RawURLEncoding.EncodeToString([]byte(late))); n == 0 {
			t.Fatal("the row that the release before made during the pass has no staged tuple")
		}
	})
}
