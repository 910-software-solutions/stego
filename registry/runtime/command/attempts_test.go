package command

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/sealed"
)

// pinWallet is a test entity with a compare-only PIN with a limit of wrong attempts, as the generated code declares it
// (ST-SUP-RC32-OC24).
type pinWallet struct {
	ID      string
	Balance int64
	PIN     sealed.Compared
}

const pinWalletDDL = `CREATE TABLE pin_wallets (tenant_id text NOT NULL, id text NOT NULL, balance bigint NOT NULL, pin text,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))`

// pinCounter is the counter of PinWallet.pin with the limit 3.
var pinCounter = Must(NewCounter(CounterSpec{Entity: "PinWallet", Field: "pin", Limit: 3}))

func pinWalletEntity(t *testing.T) *Entity[pinWallet] {
	t.Helper()
	s := spec("PinWallet", "", "pin_wallets", []string{"balance", "pin"},
		func(row Scanner) (pinWallet, Meta, error) {
			var v pinWallet
			var m Meta
			err := row.Scan(&v.ID, &v.Balance, CountedAt(pinCounter, &v.ID, &v.PIN), &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v pinWallet) []any { return []any{v.ID, v.Balance, HashCountedOf(pinCounter, v.ID, v.PIN)} },
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
	var charges, refusedCharges atomic.Int32
	rt.charged = func(ok bool) {
		if ok {
			charges.Add(1)
		} else {
			refusedCharges.Add(1)
		}
	}
	cmd := e.command("withdraw", nil, wallets)
	errWrongPIN := errors.New("pin.wrong")
	var runs atomic.Int32
	type read func(ctx context.Context, r *Repository[pinWallet]) (Loaded[pinWallet], error)
	locked := func(ctx context.Context, r *Repository[pinWallet]) (Loaded[pinWallet], error) {
		return r.GetForUpdate(ctx, walletID)
	}
	unlocked := func(ctx context.Context, r *Repository[pinWallet]) (Loaded[pinWallet], error) {
		return r.Get(ctx, walletID)
	}
	// withdraw takes 10 from the wallet when the last PIN matches, and else returns the refusal; matched tells what
	// the last check gave.
	withdraw := func(get read, pins ...string) (matched bool, err error) {
		n := runs.Add(1)
		_, err = e.runOn(rt, cmd, e.request("t1", u7(t), fmt.Sprintf("w-%d", n), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			r, err := Repo(h, wallets)
			if err != nil {
				return idempotency.Response{}, err
			}
			w, err := get(ctx, r)
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
	// setPIN writes a new PIN with a command, which resets the count.
	setPIN := func(pin string) {
		t.Helper()
		n := runs.Add(1)
		if _, err := e.runOn(rt, cmd, e.request("t1", u7(t), fmt.Sprintf("set-%d", n), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			r, err := Repo(h, wallets)
			if err != nil {
				return idempotency.Response{}, err
			}
			w, err := r.GetForUpdate(ctx, walletID)
			if err != nil {
				return idempotency.Response{}, err
			}
			v := w.Value()
			v.PIN = sealed.ComparedOf(pin)
			_, err = r.Update(ctx, w, v)
			return ok, err
		}); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		t.Helper()
		return e.count(`SELECT coalesce(sum(checks), 0) FROM attempt_counts WHERE counter = 'PinWallet.pin' AND row_id = $1`, walletID)
	}
	state := func() (balance, version int64) {
		t.Helper()
		if err := e.admin.QueryRowContext(e.ctx, `SELECT balance, version FROM pin_wallets WHERE id = $1`, walletID).Scan(&balance, &version); err != nil {
			t.Fatal(err)
		}
		return balance, version
	}
	t.Run("ST-SUP-RP10-LC54-T01 Each wrong PIN is charged before its check, and the charge stays after the refusal", func(t *testing.T) {
		_, before := state()
		for i := 1; i <= 3; i++ {
			if matched, err := withdraw(locked, "0000"); matched || !errors.Is(err, errWrongPIN) {
				t.Fatalf("attempt %d: matched %v, error %v; want the refusal", i, matched, err)
			}
			balance, version := state()
			if balance != 100 || count() != i || version != before {
				t.Fatalf("after wrong attempt %d: balance %d, count %d, version %d; want 100, %d, %d", i, balance, count(), version, i, before)
			}
		}
		if n := e.auditRows("t1", "withdraw"); n != 0 {
			t.Fatalf("the count wrote %d audit rows", n)
		}
		if n := e.count(`SELECT count(*) FROM stego_outbox.messages`); n != 0 {
			t.Fatalf("the count staged %d messages", n)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T01 At the limit the right PIN gives false and runs no charge", func(t *testing.T) {
		refusedBefore := refusedCharges.Load()
		if matched, err := withdraw(locked, right); matched || !errors.Is(err, errWrongPIN) {
			t.Fatalf("matched %v, error %v; want false and the refusal", matched, err)
		}
		if balance, _ := state(); balance != 100 || count() != 3 || refusedCharges.Load() != refusedBefore+1 {
			t.Fatalf("balance %d, count %d, refused charges %d; want 100, 3, one more", balance, count(), refusedCharges.Load()-refusedBefore)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T01 A new value of the field resets the count, and a match that commits resets it", func(t *testing.T) {
		_, before := state()
		setPIN(right)
		if count() != 0 {
			t.Fatalf("after a new PIN the count is %d, want 0", count())
		}
		if _, err := withdraw(locked, "0000"); !errors.Is(err, errWrongPIN) || count() != 1 {
			t.Fatalf("a wrong PIN gave %v and the count %d, want the refusal and 1", err, count())
		}
		if matched, err := withdraw(locked, right); !matched || err != nil || count() != 0 {
			t.Fatalf("the right PIN gave %v, %v and the count %d; want a commit and 0", matched, err, count())
		}
		if balance, version := state(); balance != 90 || version != before+2 {
			t.Fatalf("balance %d, version %d; want 90 and two writes", balance, version)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T01 A command that fails after a match leaves the charge", func(t *testing.T) {
		n := runs.Add(1)
		failed := errors.New("short balance")
		_, err := e.runOn(rt, cmd, e.request("t1", u7(t), fmt.Sprintf("fail-%d", n), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			r, err := Repo(h, wallets)
			if err != nil {
				return idempotency.Response{}, err
			}
			w, err := r.Get(ctx, walletID)
			if err != nil {
				return idempotency.Response{}, err
			}
			if !w.Value().PIN.Matches(right) {
				return idempotency.Response{}, errWrongPIN
			}
			return idempotency.Response{}, failed
		})
		if !errors.Is(err, failed) || count() != 1 {
			t.Fatalf("error %v, count %d; want the failure and 1", err, count())
		}
		setPIN(right)
	})
	t.Run("ST-SUP-RP10-LC54-T01 Parallel wrong PINs against the limit 3 run at most 3 checks", func(t *testing.T) {
		for _, get := range []struct {
			name string
			read read
		}{{"with no lock", unlocked}, {"locked", locked}} {
			setPIN(right)
			before := charges.Load()
			var wg sync.WaitGroup
			errs := make(chan error, 12)
			for range 12 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if matched, err := withdraw(get.read, "0000"); matched || !errors.Is(err, errWrongPIN) {
						errs <- fmt.Errorf("matched %v, error %v", matched, err)
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Errorf("%s: %v", get.name, err)
			}
			if checks := charges.Load() - before; checks != 3 || count() != 3 {
				t.Fatalf("%s: 12 parallel wrong PINs ran %d checks and left the count %d; want 3 and 3", get.name, checks, count())
			}
		}
		setPIN(right)
	})
	t.Run("ST-SUP-RP10-LC54-T01 A charge that cannot commit fails the check and answers 503", func(t *testing.T) {
		e.exec("REVOKE INSERT, UPDATE ON attempt_counts FROM " + e.roles[0])
		defer e.exec("GRANT INSERT, UPDATE ON attempt_counts TO " + e.roles[0])
		matched, err := withdraw(locked, right)
		if matched || !errors.Is(err, ErrUnavailable) {
			t.Fatalf("matched %v, error %v; want false and ErrUnavailable", matched, err)
		}
		if balance, _ := state(); balance != 90 {
			t.Fatalf("the balance is %d after a failed charge, want 90", balance)
		}
	})
}
