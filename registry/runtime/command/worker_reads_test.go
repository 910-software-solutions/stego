package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/registry/runtime/sealed"
)

// readWallet is a test entity with a readable secret api_key, as a worker that writes it gets it, and
// readWalletView the same row as a worker that only reads it gets it, with no secret field (ST-SUP-RC35-OC19).
type readWallet struct {
	ID, Label string
	APIKey    sealed.Secret
}

type readWalletView struct{ ID, Label string }

const readWalletDDL = `CREATE TABLE read_wallets (tenant_id text NOT NULL, id text NOT NULL, label text NOT NULL, api_key bytea,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))`

// readWalletViewSpec is the read-only declaration of read_wallets, as the generator gives it.
func readWalletViewSpec() EntitySpec[readWalletView] {
	return EntitySpec[readWalletView]{Name: "Wallet", ResourceType: "wallet", ReadPermission: "view", ListPermission: "view", ReadOnly: true,
		Select: `SELECT id, label, version, authz_token FROM read_wallets WHERE tenant_id = $1 AND id = $2`,
		Scan: func(row Scanner) (readWalletView, Meta, error) {
			var v readWalletView
			var m Meta
			err := row.Scan(&v.ID, &v.Label, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		Clone:   func(v readWalletView) readWalletView { return v },
		ValidID: func(id string) bool { return id != "" }}
}

func TestWorkerReads(t *testing.T) {
	e := newEnv(t)
	e.exec(readWalletDDL, "GRANT SELECT, INSERT, UPDATE, DELETE ON read_wallets TO "+e.roles[0],
		"ALTER TABLE read_wallets ENABLE ROW LEVEL SECURITY", "ALTER TABLE read_wallets FORCE ROW LEVEL SECURITY",
		"CREATE POLICY read_wallets_tenant ON read_wallets USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))",
		`INSERT INTO read_wallets (tenant_id, id, label, api_key) VALUES ('t1', 'w1', 'one', '\x0102'), ('t1', 'w2', 'two', '\x0304'), ('t1', 'w3', 'three', '\x0506')`)
	view, err := NewEntity(readWalletViewSpec())
	if err != nil {
		t.Fatal(err)
	}
	full := spec("Wallet", "wallet", "read_wallets", []string{"label", "api_key"},
		func(row Scanner) (readWallet, Meta, error) {
			var v readWallet
			var m Meta
			err := row.Scan(&v.ID, &v.Label, OpenAt("read_wallets", "api_key", &v.ID, &v.APIKey), &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v readWallet) []any {
			return []any{v.ID, v.Label, SealAt("read_wallets", "api_key", v.ID, v.APIKey)}
		},
		func(v readWallet) map[string]any { return map[string]any{"id": v.ID, "label": v.Label} })
	full.Sealed = true
	written, err := NewEntity(full)
	if err != nil {
		t.Fatal(err)
	}
	all, err := NewList(view, ListSpec{Name: "all", Read: `SELECT id, label, version, authz_token FROM read_wallets WHERE tenant_id = $1 ORDER BY id LIMIT $2`})
	if err != nil {
		t.Fatal(err)
	}
	// The worker may not read w2.
	e.decision.setAllow(func(_, _, resource string) bool { return resource != "wallet:w2" })
	w := e.worker("sum-wallets", "sync", func(s *WorkerSpec[testProbe]) { s.Reads = []Declared{view}; s.Lists = []AnyList{all} }, e.orders)
	run := func(fill func(ctx context.Context, h *Handle[testProbe]) error) error {
		return RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "t1"), nil, tickFill(fill))
	}
	t.Run("ST-SUP-RP10-LC53-T01 A worker reads the rows of an entity of its reads with no secret, and writes what it writes", func(t *testing.T) {
		e.statements.reset()
		var got readWalletView
		var listed []string
		if err := run(func(ctx context.Context, h *Handle[testProbe]) error {
			r, err := ReadRepo(h, view)
			if err != nil {
				return err
			}
			one, err := r.Get(ctx, "w1")
			if err != nil {
				return err
			}
			got = one.Value()
			rows, err := r.List(ctx, all, 10)
			if err != nil {
				return err
			}
			for _, row := range rows {
				listed = append(listed, row.ID()+"="+row.Value().Label)
			}
			_, err = repo(t, h, e.orders).Create(ctx, order{ID: "o-sum", Status: strings.Join(listed, ",")})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got != (readWalletView{ID: "w1", Label: "one"}) || strings.Join(listed, ",") != "w1=one,w3=three" {
			t.Fatalf("the worker read %+v and listed %v; want w1 one, and w1 and w3 without the denied w2", got, listed)
		}
		if n := e.count(`SELECT count(*) FROM orders WHERE id = 'o-sum'`); n != 1 {
			t.Fatalf("the run did not commit its write")
		}
		for _, s := range e.statements.all() {
			if strings.Contains(s, "api_key") {
				t.Fatalf("a statement of the worker names the secret column: %s", s)
			}
		}
	})
	t.Run("ST-SUP-RP10-LC53-T01 A denied row of an entity of its reads is not found", func(t *testing.T) {
		err := run(func(ctx context.Context, h *Handle[testProbe]) error {
			r, err := ReadRepo(h, view)
			if err != nil {
				return err
			}
			_, err = r.Get(ctx, "w2")
			return err
		})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("the read of the denied wallet gave %v, want not found", err)
		}
	})
	t.Run("ST-SUP-RP10-LC53-T01 A repository of a read-only entity, and a reader of another entity, are refused with no statement", func(t *testing.T) {
		other, err := NewEntity(readWalletViewSpec())
		if err != nil {
			t.Fatal(err)
		}
		for name, call := range map[string]func(h *Handle[testProbe]) error{
			"Repo of the read-only entity":           func(h *Handle[testProbe]) error { _, err := Repo(h, view); return err },
			"ReadRepo of the written entity":         func(h *Handle[testProbe]) error { _, err := ReadRepo(h, written); return err },
			"ReadRepo of an entity it does not read": func(h *Handle[testProbe]) error { _, err := ReadRepo(h, other); return err },
		} {
			e.statements.reset()
			var refused error
			_ = run(func(ctx context.Context, h *Handle[testProbe]) error {
				before := len(e.statements.all())
				refused = call(h)
				if len(e.statements.all()) != before {
					t.Errorf("%s ran a statement", name)
				}
				return refused
			})
			if refused == nil || !strings.Contains(refused.Error(), "Wallet") {
				t.Errorf("%s gave %v, want an error that names Wallet", name, refused)
			}
		}
	})
	t.Run("ST-SUP-RP10-LC53-T01 A worker or an entity that mixes reads and writes is refused", func(t *testing.T) {
		for name, s := range map[string]WorkerSpec[testProbe]{
			"the read-only entity in Writes":  {Writes: []Declared{view}},
			"the written entity in Reads":     {Writes: []Declared{e.orders}, Reads: []Declared{written}},
			"both declarations of one entity": {Writes: []Declared{written}, Reads: []Declared{view}},
		} {
			s.Name, s.Permission, s.Probe = "mixed", "sync", newTestProbe
			if _, err := NewWorker(s); err == nil {
				t.Errorf("NewWorker with %s is accepted", name)
			}
		}
		for name, change := range map[string]func(*EntitySpec[readWalletView]){
			"an Insert": func(s *EntitySpec[readWalletView]) { s.Insert = "INSERT INTO read_wallets VALUES ($1)" },
			"Values":    func(s *EntitySpec[readWalletView]) { s.Values = func(readWalletView) []any { return nil } },
			"Sealed":    func(s *EntitySpec[readWalletView]) { s.Sealed = true },
		} {
			s := readWalletViewSpec()
			change(&s)
			if _, err := NewEntity(s); err == nil {
				t.Errorf("a read-only entity with %s is accepted", name)
			}
		}
		if _, err := NewList(view, ListSpec{Name: "locked", Read: `SELECT id FROM read_wallets WHERE tenant_id = $1 LIMIT $2`,
			Lock: `SELECT id FROM read_wallets WHERE tenant_id = $1 AND id = ANY($2) ORDER BY id FOR NO KEY UPDATE`}); err == nil {
			t.Error("a locked list of a read-only entity is accepted")
		}
	})
}
