package command

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/sealed"
)

// pinWallet is a test entity with a compare-only PIN and the counter of its wrong attempts, as the generated code
// declares it (ST-SUP-RC32-OC24).
type pinWallet struct {
	ID          string
	Balance     int64
	PIN         sealed.Compared
	PINAttempts int32
}

const pinWalletDDL = `CREATE TABLE pin_wallets (tenant_id text NOT NULL, id text NOT NULL, balance bigint NOT NULL, pin text,
  pin_attempts integer NOT NULL DEFAULT 0, version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '',
  updated_time timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (tenant_id, id))`

// pinCounter is the counter of pin_wallets.pin with the limit 3, with the statements of the generator.
var pinCounter = Must(NewCounter(CounterSpec{Entity: "PinWallet", Field: "pin", Column: "pin_attempts", Limit: 3,
	Count: `UPDATE pin_wallets SET pin_attempts = LEAST(pin_attempts + $3::integer, 3), version = version + 1, updated_time = now() WHERE tenant_id = $1 AND id = $2`,
	Reset: `UPDATE pin_wallets SET pin_attempts = 0 WHERE tenant_id = $1 AND id = $2 AND pin_attempts <> 0`}))

func pinWalletEntity(t *testing.T) *Entity[pinWallet] {
	t.Helper()
	s := spec("PinWallet", "", "pin_wallets", []string{"balance", "pin", "pin_attempts"},
		func(row Scanner) (pinWallet, Meta, error) {
			var v pinWallet
			var m Meta
			err := row.Scan(&v.ID, &v.Balance, CountedAt(pinCounter, &v.ID, &v.PIN, &v.PINAttempts), &v.PINAttempts, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v pinWallet) []any { return []any{v.ID, v.Balance, HashOf(v.PIN), v.PINAttempts} },
		func(v pinWallet) map[string]any { return map[string]any{"id": v.ID, "balance": v.Balance} })
	e, err := NewEntity(s)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAttempts(t *testing.T) {
	const walletID, right = "0198a1b2-0000-7000-8000-000000000001", "4711"
	e := newEnv(t)
	e.exec(pinWalletDDL, "GRANT SELECT, INSERT, UPDATE, DELETE ON pin_wallets TO "+e.roles[0],
		"ALTER TABLE pin_wallets ENABLE ROW LEVEL SECURITY", "ALTER TABLE pin_wallets FORCE ROW LEVEL SECURITY",
		"CREATE POLICY pin_wallets_tenant ON pin_wallets USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))")
	hash, err := sealed.Hash(right)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.admin.ExecContext(e.ctx, `INSERT INTO pin_wallets (tenant_id, id, balance, pin) VALUES ('t1', $1, 100, $2)`, walletID, hash); err != nil {
		t.Fatal(err)
	}
	wallets := pinWalletEntity(t)
	rt := e.runtime(func(c *Config) { c.Entities = []Declared{wallets} })
	cmd := e.command("withdraw", nil, wallets)
	errWrongPIN := errors.New("pin.wrong")
	runs := 0
	// withdraw takes 10 from the wallet when the PIN matches, and else returns the refusal; matched tells what the
	// check gave.
	withdraw := func(read func(ctx context.Context, r *Repository[pinWallet]) (Loaded[pinWallet], error), pins ...string) (matched bool, err error) {
		runs++
		_, err = e.runOn(rt, cmd, e.request("t1", u7(t), fmt.Sprintf("w-%d", runs), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			r, err := Repo(h, wallets)
			if err != nil {
				return idempotency.Response{}, err
			}
			w, err := read(ctx, r)
			if err != nil {
				return idempotency.Response{}, err
			}
			v := w.Value()
			for _, pin := range pins {
				matched = v.PIN.Matches(pin)
			}
			if !matched {
				return idempotency.Response{}, errWrongPIN
			}
			v.Balance -= 10
			if _, err := r.Update(ctx, w, v); err != nil {
				return idempotency.Response{}, err
			}
			return ok, nil
		})
		return matched, err
	}
	locked := func(ctx context.Context, r *Repository[pinWallet]) (Loaded[pinWallet], error) {
		return r.GetForUpdate(ctx, walletID)
	}
	state := func() (balance int64, count int32, version int64) {
		t.Helper()
		if err := e.admin.QueryRowContext(e.ctx, `SELECT balance, pin_attempts, version FROM pin_wallets WHERE id = $1`, walletID).Scan(&balance, &count, &version); err != nil {
			t.Fatal(err)
		}
		return balance, count, version
	}
	t.Run("ST-SUP-RP10-LC54-T01 Each wrong PIN is counted after the refusal rolls back, with a new version", func(t *testing.T) {
		_, _, before := state()
		for i := int32(1); i <= 3; i++ {
			if matched, err := withdraw(locked, "0000"); matched || !errors.Is(err, errWrongPIN) {
				t.Fatalf("attempt %d: matched %v, error %v; want the refusal", i, matched, err)
			}
			balance, count, version := state()
			if balance != 100 || count != i || version != before+int64(i) {
				t.Fatalf("after wrong attempt %d: balance %d, count %d, version %d; want 100, %d, %d", i, balance, count, version, i, before+int64(i))
			}
		}
		if n := e.auditRows("t1", "withdraw"); n != 0 {
			t.Fatalf("the count wrote %d audit rows", n)
		}
		if n := e.count(`SELECT count(*) FROM stego_outbox.messages`); n != 0 {
			t.Fatalf("the count staged %d messages", n)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T01 At the limit the right PIN gives false and the refusal", func(t *testing.T) {
		if matched, err := withdraw(locked, right); matched || !errors.Is(err, errWrongPIN) {
			t.Fatalf("matched %v, error %v; want false and the refusal", matched, err)
		}
		if balance, count, _ := state(); balance != 100 || count != 3 {
			t.Fatalf("balance %d, count %d; want 100, 3", balance, count)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T01 After a reset the right PIN commits and leaves the count 0", func(t *testing.T) {
		runs++
		if _, err := e.runOn(rt, cmd, e.request("t1", u7(t), fmt.Sprintf("reset-%d", runs), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			r, err := Repo(h, wallets)
			if err != nil {
				return idempotency.Response{}, err
			}
			w, err := r.GetForUpdate(ctx, walletID)
			if err != nil {
				return idempotency.Response{}, err
			}
			v := w.Value()
			v.PINAttempts = 0
			_, err = r.Update(ctx, w, v)
			return ok, err
		}); err != nil {
			t.Fatal(err)
		}
		if matched, err := withdraw(locked, right); !matched || err != nil {
			t.Fatalf("matched %v, error %v; want a commit", matched, err)
		}
		if balance, count, _ := state(); balance != 90 || count != 0 {
			t.Fatalf("balance %d, count %d; want 90, 0", balance, count)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T01 A wrong PIN and then the right PIN in one command that commits count one", func(t *testing.T) {
		if matched, err := withdraw(locked, "0000", right); !matched || err != nil {
			t.Fatalf("matched %v, error %v; want a commit", matched, err)
		}
		if balance, count, _ := state(); balance != 80 || count != 1 {
			t.Fatalf("balance %d, count %d; want 80, 1", balance, count)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T01 The check of a row read with no lock gives false and counts nothing", func(t *testing.T) {
		unlocked := func(ctx context.Context, r *Repository[pinWallet]) (Loaded[pinWallet], error) {
			return r.Get(ctx, walletID)
		}
		if matched, err := withdraw(unlocked, right); matched || !errors.Is(err, errWrongPIN) {
			t.Fatalf("matched %v, error %v; want false", matched, err)
		}
		if _, count, _ := state(); count != 1 {
			t.Fatalf("count %d; want 1", count)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T01 A command that fails after a match leaves the count as it was", func(t *testing.T) {
		runs++
		failed := errors.New("short balance")
		_, err := e.runOn(rt, cmd, e.request("t1", u7(t), fmt.Sprintf("fail-%d", runs), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			r, err := Repo(h, wallets)
			if err != nil {
				return idempotency.Response{}, err
			}
			w, err := r.GetForUpdate(ctx, walletID)
			if err != nil {
				return idempotency.Response{}, err
			}
			if !w.Value().PIN.Matches(right) {
				return idempotency.Response{}, errWrongPIN
			}
			return idempotency.Response{}, failed
		})
		if !errors.Is(err, failed) {
			t.Fatalf("error %v; want the failure", err)
		}
		if _, count, _ := state(); count != 1 {
			t.Fatalf("count %d; want 1", count)
		}
	})
}
