package command

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/jsell-rh/stego/registry/runtime/authz"
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

// pinCounter is the counter of PinWallet.pin with the limit 3 and a window of 15 minutes; fixedCounter has no window.
var (
	pinCounter   = Must(NewCounter(CounterSpec{Entity: "PinWallet", Field: "pin", Limit: 3, UnlockAfter: 15 * time.Minute}))
	fixedCounter = Must(NewCounter(CounterSpec{Entity: "PinWallet", Field: "pin", Limit: 3}))
)

func pinWalletEntity(t *testing.T) *Entity[pinWallet] {
	t.Helper()
	return pinWalletEntityOf(t, pinCounter)
}

// pinWalletEntityOf is the PIN wallet entity whose PIN has the counter given.
func pinWalletEntityOf(t *testing.T, pinCounter *Counter) *Entity[pinWallet] {
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

// pinCopy is a test entity with a compare-only PIN with no limit of wrong attempts (ST-SUP-RC32-OC20).
type pinCopy struct {
	ID  string
	PIN sealed.Compared
}

func pinCopyEntity(t *testing.T) *Entity[pinCopy] {
	t.Helper()
	s := spec("PinCopy", "", "pin_copies", []string{"pin"},
		func(row Scanner) (pinCopy, Meta, error) {
			var v pinCopy
			var m Meta
			err := row.Scan(&v.ID, ComparedAt(&v.PIN), &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v pinCopy) []any { return []any{v.ID, HashOf(v.PIN)} },
		func(v pinCopy) map[string]any { return map[string]any{"id": v.ID} })
	e, err := NewEntity(s)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// pinWallets makes the table of the PIN wallets in the database of e with the wallet walletID, whose PIN is right.
func pinWallets(t *testing.T, e *env, walletID, right string) {
	t.Helper()
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
}

func TestAttempts(t *testing.T) {
	const walletID, right = "0198a1b2-0000-7000-8000-000000000001", "4711"
	e := newEnv(t)
	pinWallets(t, e, walletID, right)
	wallets := pinWalletEntity(t)
	copies := pinCopyEntity(t)
	e.exec(`CREATE TABLE pin_copies (tenant_id text NOT NULL, id text NOT NULL, pin text, version bigint NOT NULL DEFAULT 1,
  authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))`, "GRANT SELECT, INSERT, UPDATE, DELETE ON pin_copies TO "+e.roles[0])
	reader := sdkmetric.NewManualReader()
	rt := e.runtime(func(c *Config) {
		c.Entities = []Declared{wallets, copies}
		c.Meter = sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	})
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
	t.Run("ST-SUP-RP10-LC54-T04 A charge that cannot commit logs one error line through the log provider of the runtime, with the cause class and no value", func(t *testing.T) {
		logs := &logRecorder{}
		rt.logger = sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs))).Logger("test")
		held := statementLog
		// A new log, so the line of the failed charge of the test before is not held back.
		statementLog = authz.NewCauseLog(nil)
		defer func() { rt.logger, statementLog = nil, held }()
		e.exec("REVOKE INSERT, UPDATE ON attempt_counts FROM " + e.roles[0])
		defer e.exec("GRANT INSERT, UPDATE ON attempt_counts TO " + e.roles[0])
		if _, err := withdraw(locked, right); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("a failed charge gave %v, want ErrUnavailable", err)
		}
		var lines []sdklog.Record
		for _, r := range logs.all() {
			if r.EventName() == AttemptsFailedEvent {
				lines = append(lines, r)
			}
		}
		if len(lines) != 1 || lines[0].SeverityText() != "ERROR" {
			t.Fatalf("a failed charge logged %d lines of %s, want one ERROR line", len(lines), AttemptsFailedEvent)
		}
		got := map[string]string{}
		lines[0].WalkAttributes(func(kv attribute.KeyValue) bool {
			got[string(kv.Key)] = kv.Value.Emit()
			return true
		})
		want := map[string]string{"stego.action": "withdraw", "stego.entity": "PinWallet", "stego.cause": "internal", "stego.held_back": "0"}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("the line has the attributes %v, want %v", got, want)
		}
		if body := lines[0].Body().Emit(); strings.Contains(body, right) || strings.Contains(body, "permission") {
			t.Errorf("the line holds a value or an error text: %q", body)
		}
		for _, c := range []struct {
			err  error
			want string
		}{{context.DeadlineExceeded, "deadline"}, {fmt.Errorf("charge: %w", context.Canceled), "canceled"}, {errors.New("pin 1234 is wrong"), "internal"}} {
			if got := chargeCause(c.err); got != c.want {
				t.Errorf("chargeCause(%v) = %s, want %s", c.err, got, c.want)
			}
		}
	})
	t.Run("ST-SUP-RP10-LC54-T08 A flood of charges that cannot commit logs one line for each action, entity and cause in each LogEvery", func(t *testing.T) {
		logs := &logRecorder{}
		rt.logger = sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs))).Logger("test")
		tick := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
		held := statementLog
		statementLog = authz.NewCauseLog(func() time.Time { return tick })
		defer func() { rt.logger, statementLog = nil, held }()
		e.exec("REVOKE INSERT, UPDATE ON attempt_counts FROM " + e.roles[0])
		defer e.exec("GRANT INSERT, UPDATE ON attempt_counts TO " + e.roles[0])
		failed := func() []sdklog.Record {
			var lines []sdklog.Record
			for _, r := range logs.all() {
				if r.EventName() == AttemptsFailedEvent {
					lines = append(lines, r)
				}
			}
			return lines
		}
		for range 3 {
			if _, err := withdraw(locked, right); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("a failed charge gave %v, want ErrUnavailable", err)
			}
		}
		if lines := failed(); len(lines) != 1 {
			t.Fatalf("3 failed charges in one LogEvery logged %d lines of %s, want 1", len(lines), AttemptsFailedEvent)
		}
		tick = tick.Add(authz.LogEvery)
		if _, err := withdraw(locked, right); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("a failed charge gave %v, want ErrUnavailable", err)
		}
		lines := failed()
		if len(lines) != 2 {
			t.Fatalf("a failed charge after LogEvery logged %d lines in all, want 2", len(lines))
		}
		got := ""
		lines[1].WalkAttributes(func(kv attribute.KeyValue) bool {
			if kv.Key == "stego.held_back" {
				got = kv.Value.Emit()
			}
			return true
		})
		if got != "2" {
			t.Fatalf("the second line holds back %q lines, want 2", got)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T10 The counter command.attempts.failed counts each charge that cannot commit by action, entity and cause, with no tenant", func(t *testing.T) {
		held := statementLog
		statementLog = authz.NewCauseLog(nil)
		defer func() { statementLog = held }()
		// failures gives the sum of the counter by its attribute set, with each attribute key=value.
		failures := func() map[string]int64 {
			var data metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &data); err != nil {
				t.Fatal(err)
			}
			out := map[string]int64{}
			for _, scope := range data.ScopeMetrics {
				for _, m := range scope.Metrics {
					if m.Name != AttemptsFailedEvent {
						continue
					}
					if m.Unit != "{charge}" {
						t.Errorf("the counter %s has the unit %q, want {charge}", m.Name, m.Unit)
					}
					sum, ok := m.Data.(metricdata.Sum[int64])
					if !ok || !sum.IsMonotonic {
						t.Fatalf("the metric %s is %T, want a monotonic int64 sum", m.Name, m.Data)
					}
					for _, point := range sum.DataPoints {
						var keys []string
						for _, kv := range point.Attributes.ToSlice() {
							keys = append(keys, string(kv.Key)+"="+kv.Value.Emit())
						}
						out[strings.Join(keys, ",")] += point.Value
					}
				}
			}
			return out
		}
		key := "action=withdraw,cause=internal,entity=PinWallet"
		before := failures()
		e.exec("REVOKE INSERT, UPDATE ON attempt_counts FROM " + e.roles[0])
		defer e.exec("GRANT INSERT, UPDATE ON attempt_counts TO " + e.roles[0])
		for range 3 {
			if _, err := withdraw(locked, right); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("a failed charge gave %v, want ErrUnavailable", err)
			}
		}
		after := failures()
		if got := after[key] - before[key]; got != 3 {
			t.Fatalf("3 failed charges added %d to %s {%s}, want 3 (the counter is %v)", got, AttemptsFailedEvent, key, after)
		}
		for set := range after {
			if set != key && after[set] != before[set] {
				t.Errorf("a failed charge of withdraw added to the attribute set %s, want only %s", set, key)
			}
		}
	})
	t.Run("ST-SUP-RP10-LC54-T03 A rerun of the call after a conflict does not charge the checks of an earlier attempt again", func(t *testing.T) {
		setPIN(right)
		rerun := e.command("withdraw-rerun", func(s *CommandSpec[testProbe]) { s.RetryOnConflict = 2 }, wallets)
		charges.Store(0)
		attempts := 0
		n := runs.Add(1)
		result, err := e.runOn(rt, rerun, e.request("t1", u7(t), fmt.Sprintf("rerun-%d", n), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			attempts++
			r, err := Repo(h, wallets)
			if err != nil {
				return idempotency.Response{}, err
			}
			w, err := r.Get(ctx, walletID)
			if err != nil {
				return idempotency.Response{}, err
			}
			v := w.Value()
			// Two wrong PINs in each attempt.
			if v.PIN.Matches("0000") || v.PIN.Matches("1111") {
				return idempotency.Response{}, errors.New("a wrong PIN matched")
			}
			if attempts < 3 {
				// Another transaction moves the version of the row: the update of the attempt is a conflict, and the
				// call runs again.
				e.exec(`UPDATE pin_wallets SET version = version + 1 WHERE id = '` + walletID + `'`)
				if _, err := r.Update(ctx, w, v); err != nil {
					return idempotency.Response{}, err
				}
			}
			return idempotency.Response{}, errWrongPIN
		})
		if !errors.Is(err, errWrongPIN) || attempts != 3 || result.Attempts != 3 {
			t.Fatalf("the call gave %v after %d calls of the fill (%d attempts), want the refusal at the third", err, attempts, result.Attempts)
		}
		if got := count(); got != 2 || charges.Load() != 2 {
			t.Fatalf("three attempts of two wrong PINs left the count %d after %d charges, want 2 and 2", got, charges.Load())
		}
		// A new call is a new request: its checks are charged.
		if matched, err := withdraw(unlocked, "2222"); matched || !errors.Is(err, errWrongPIN) || count() != 3 {
			t.Fatalf("the next call gave %v, %v and the count %d, want a refusal and 3", matched, err, count())
		}
		setPIN(right)
	})
	t.Run("ST-SUP-RP10-LC54-T05 A counted PIN that a read gives is not written to a field with no limit or to another row", func(t *testing.T) {
		setPIN(right)
		copier := e.command("copy-pin", nil, wallets, copies)
		otherID, copyID := "0198a1b2-0000-7000-8000-0000000000a1", "0198a1b2-0000-7000-8000-0000000000a2"
		for _, write := range []struct {
			name string
			do   func(ctx context.Context, h *Handle[testProbe], pin sealed.Compared) error
		}{
			{"a field with no limit", func(ctx context.Context, h *Handle[testProbe], pin sealed.Compared) error {
				r, err := Repo(h, copies)
				if err != nil {
					return err
				}
				_, err = r.Create(ctx, pinCopy{ID: copyID, PIN: pin})
				return err
			}},
			{"a new row with a new count", func(ctx context.Context, h *Handle[testProbe], pin sealed.Compared) error {
				r, err := Repo(h, wallets)
				if err != nil {
					return err
				}
				_, err = r.Create(ctx, pinWallet{ID: otherID, Balance: 1, PIN: pin})
				return err
			}},
		} {
			n := runs.Add(1)
			_, err := e.runOn(rt, copier, e.request("t1", u7(t), fmt.Sprintf("copy-%d", n), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
				r, err := Repo(h, wallets)
				if err != nil {
					return idempotency.Response{}, err
				}
				w, err := r.Get(ctx, walletID)
				if err != nil {
					return idempotency.Response{}, err
				}
				return ok, write.do(ctx, h, w.Value().PIN)
			})
			if err == nil || !sealedFault(err) {
				t.Fatalf("%s: the copy of the counted PIN gave %v, want the internal error of a sealed value", write.name, err)
			}
		}
		if n := e.count(`SELECT count(*) FROM pin_copies`) + e.count(`SELECT count(*) FROM pin_wallets WHERE id = $1`, otherID); n != 0 {
			t.Fatalf("the copies of the counted PIN left %d rows, want 0", n)
		}
		// The same field of the same row takes the value that the read gave, as each update of the row does.
		if matched, err := withdraw(locked, right); !matched || err != nil {
			t.Fatalf("an update of the wallet with its own PIN gave %v and %v, want a commit", matched, err)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T06 A rerun after a new PIN between two attempts charges the checks of the new PIN", func(t *testing.T) {
		setPIN(right)
		newHash, err := sealed.Hash("9999")
		if err != nil {
			t.Fatal(err)
		}
		rerun := e.command("withdraw-reset", func(s *CommandSpec[testProbe]) { s.RetryOnConflict = 1 }, wallets)
		charges.Store(0)
		attempts := 0
		n := runs.Add(1)
		_, err = e.runOn(rt, rerun, e.request("t1", u7(t), fmt.Sprintf("reset-%d", n), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			attempts++
			r, err := Repo(h, wallets)
			if err != nil {
				return idempotency.Response{}, err
			}
			w, err := r.Get(ctx, walletID)
			if err != nil {
				return idempotency.Response{}, err
			}
			v := w.Value()
			if v.PIN.Matches("0000") || v.PIN.Matches("1111") {
				return idempotency.Response{}, errors.New("a wrong PIN matched")
			}
			if attempts == 1 {
				// A reset commits between the two attempts: a new PIN and a count of 0, and the version moves, so the
				// update of the attempt is a conflict and the call runs again.
				if _, err := e.admin.ExecContext(e.ctx, `UPDATE pin_wallets SET pin = $2, version = version + 1 WHERE id = $1`, walletID, newHash); err != nil {
					return idempotency.Response{}, err
				}
				e.exec(`DELETE FROM attempt_counts WHERE row_id = '` + walletID + `'`)
				if _, err := r.Update(ctx, w, v); err != nil {
					return idempotency.Response{}, err
				}
			}
			return idempotency.Response{}, errWrongPIN
		})
		if !errors.Is(err, errWrongPIN) || attempts != 2 {
			t.Fatalf("the call gave %v after %d attempts, want the refusal at the second", err, attempts)
		}
		if got := count(); got != 2 || charges.Load() != 4 {
			t.Fatalf("two wrong PINs against the new PIN left the count %d after %d charges, want 2 and 4", got, charges.Load())
		}
		setPIN(right)
	})
}

// TestAttemptPools proves that the charge of a check runs on its own pool, not on the main pool (security review of
// PR #451, L2).
func TestAttemptPools(t *testing.T) {
	t.Run("ST-SUP-RP10-LC54-T02 The charge of a check runs on the pool of the charges while the fill holds the only main connection", func(t *testing.T) {
		const walletID, right = "0198a1b2-0000-7000-8000-000000000002", "4711"
		e := newEnv(t, func(c *envConfig) { c.mainConns = 1 })
		pinWallets(t, e, walletID, right)
		wallets := pinWalletEntity(t)
		if _, err := New(e.ctx, Config{DB: e.pool, AuditDB: e.auditDB, ChargeDB: e.pool, Authz: e.authz, Audit: e.writer, Outbox: e.queue,
			Probes: e.probes, Service: "orders", ServiceVersion: "1.0.0", DeclarationVersion: "d1"}); err == nil {
			t.Fatal("New takes the main pool as the pool of the charges")
		}
		chargeDB := e.openPool(e.dsn(e.roles[0]))
		chargeDB.SetMaxOpenConns(1)
		rt := e.runtime(func(c *Config) { c.Entities, c.ChargeDB = []Declared{wallets}, chargeDB })
		cmd := e.command("withdraw", nil, wallets)
		for i, pin := range []string{"0000", right} {
			var matched bool
			_, err := e.runOn(rt, cmd, e.request("t1", u7(t), fmt.Sprintf("pool-%d", i), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
				r, err := Repo(h, wallets)
				if err != nil {
					return idempotency.Response{}, err
				}
				w, err := r.GetForUpdate(ctx, walletID)
				if err != nil {
					return idempotency.Response{}, err
				}
				if matched = w.Value().PIN.Matches(pin); !matched {
					return idempotency.Response{}, errors.New("pin.wrong")
				}
				return ok, nil
			})
			if matched != (pin == right) || (matched && err != nil) || errors.Is(err, ErrUnavailable) {
				t.Fatalf("the PIN %s gave %v and %v with one main connection, want a match only of the right PIN and no 503", pin, matched, err)
			}
		}
		if got := chargeDB.Stats().OpenConnections; got != 1 {
			t.Fatalf("the pool of the charges has %d open connections, want the one of the charges", got)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T09 The charges of one or two tenants never hold the whole pool of the charges, so a third tenant's check runs", func(t *testing.T) {
		const walletID, right = "0198a1b2-0000-7000-8000-000000000004", "4711"
		e := newEnv(t)
		pinWallets(t, e, walletID, right)
		hash, err := sealed.Hash(right)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"t2", "t3"} {
			if _, err := e.admin.ExecContext(e.ctx, `INSERT INTO pin_wallets (tenant_id, id, balance, pin) VALUES ($1, $2, 100, $3)`, id, walletID, hash); err != nil {
				t.Fatal(err)
			}
		}
		wallets := pinWalletEntity(t)
		// A pool of the charges of fewer than 3 connections is warned of when the runtime starts.
		var logs lockedBuffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		small := e.openPool(e.dsn(e.roles[0]))
		small.SetMaxOpenConns(2)
		e.runtime(func(c *Config) { c.ChargeDB = small })
		slog.SetDefault(previous)
		if text := logs.String(); !strings.Contains(text, PoolSmallEvent) || !strings.Contains(text, "max_open=2") {
			t.Fatalf("a pool of the charges of 2 connections logged %q, want %s with max_open=2", text, PoolSmallEvent)
		}
		chargeDB := e.openPool(e.dsn(e.roles[0]))
		chargeDB.SetMaxOpenConns(4)
		rt := e.runtime(func(c *Config) { c.Entities, c.ChargeDB = []Declared{wallets}, chargeDB })
		// The charges of t1 and t2 keep their connection until release closes; begun counts those that have one.
		release := make(chan struct{})
		var mu sync.Mutex
		begun := map[string]int{}
		rt.chargeBegun = func(id string) {
			if id == "t1" || id == "t2" {
				mu.Lock()
				begun[id]++
				mu.Unlock()
				<-release
			}
		}
		began := func() (int, int) {
			mu.Lock()
			defer mu.Unlock()
			return begun["t1"], begun["t2"]
		}
		cmd := e.command("withdraw", nil, wallets)
		check := func(tenantID, key, pin string) error {
			_, err := e.runOn(rt, cmd, e.request(tenantID, u7(t), key, `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
				r, err := Repo(h, wallets)
				if err != nil {
					return idempotency.Response{}, err
				}
				w, err := r.Get(ctx, walletID)
				if err != nil {
					return idempotency.Response{}, err
				}
				if !w.Value().PIN.Matches(pin) {
					return idempotency.Response{}, errors.New("pin.wrong")
				}
				return ok, nil
			})
			return err
		}
		const flood = 4
		results := make(chan error, 2*flood)
		for i := range flood {
			go func() { results <- check("t1", fmt.Sprintf("flood-1-%d", i), "0000") }()
		}
		// t1 holds its two slots before t2 floods too, so the order of the two floods is fixed.
		for n, _ := began(); n < 2; n, _ = began() {
			runtime.Gosched()
		}
		for i := range flood {
			go func() { results <- check("t2", fmt.Sprintf("flood-2-%d", i), "0000") }()
		}
		// Each check of t1 and t2 has a connection or waits for a slot.
		for {
			n1, n2 := began()
			if n1+n2+rt.chargeSlots.blocked("t1")+rt.chargeSlots.blocked("t2") == 2*flood {
				break
			}
			runtime.Gosched()
		}
		if n1, n2 := began(); n1 != 2 || n2 != 1 {
			close(release)
			t.Fatalf("t1 and t2 hold %d and %d connections of the pool of 4, want 2 and 1, so one stays free", n1, n2)
		}
		// The check of t3 gets the last connection while the charges of t1 and t2 hold theirs.
		if err := check("t3", "other", right); err != nil {
			close(release)
			t.Fatalf("the check of t3 behind the charges of t1 and t2 gave %v, want the match", err)
		}
		close(release)
		for range 2 * flood {
			if err := <-results; err == nil || errors.Is(err, ErrUnavailable) {
				t.Fatalf("a check of t1 or t2 gave %v, want the refusal of the wrong PIN", err)
			}
		}
		for _, id := range []string{"t1", "t2", "t3"} {
			if n := rt.chargeSlots.waiting(id); n != 0 {
				t.Fatalf("the slots of %s keep %d charges after the checks, want none", id, n)
			}
		}
	})
	t.Run("ST-SUP-RP10-LC54-T07 A charge ends at its lock timeout behind a reset, and a full pool of the charges is logged with no value", func(t *testing.T) {
		const walletID, right = "0198a1b2-0000-7000-8000-000000000003", "4711"
		e := newEnv(t)
		pinWallets(t, e, walletID, right)
		wallets := pinWalletEntity(t)
		chargeDB := e.openPool(e.dsn(e.roles[0]))
		chargeDB.SetMaxOpenConns(1)
		rt := e.runtime(func(c *Config) { c.Entities, c.ChargeDB = []Declared{wallets}, chargeDB })
		var chargeErrs []error
		var mu sync.Mutex
		rt.chargeErr = func(err error) { mu.Lock(); chargeErrs = append(chargeErrs, err); mu.Unlock() }
		cmd := e.command("withdraw", nil, wallets)
		check := func(key, pin string) error {
			_, err := e.runOn(rt, cmd, e.request("t1", u7(t), key, `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
				r, err := Repo(h, wallets)
				if err != nil {
					return idempotency.Response{}, err
				}
				w, err := r.Get(ctx, walletID)
				if err != nil {
					return idempotency.Response{}, err
				}
				if !w.Value().PIN.Matches(pin) {
					return idempotency.Response{}, errors.New("pin.wrong")
				}
				return ok, nil
			})
			return err
		}
		if err := check("lock-0", "0000"); err == nil || errors.Is(err, ErrUnavailable) {
			t.Fatalf("the first wrong PIN gave %v, want the refusal", err)
		}
		// A reset of the count that has not committed holds the row of the count.
		hold, err := e.admin.BeginTx(e.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hold.ExecContext(e.ctx, `DELETE FROM attempt_counts WHERE row_id = $1`, walletID); err != nil {
			t.Fatal(err)
		}
		err = check("lock-1", "0000")
		_ = hold.Rollback()
		var state interface{ SQLState() string }
		mu.Lock()
		got := append([]error(nil), chargeErrs...)
		mu.Unlock()
		if !errors.Is(err, ErrUnavailable) || len(got) != 1 || !errors.As(got[0], &state) || state.SQLState() != "55P03" || chargeCause(got[0]) != "lock_timeout" {
			t.Fatalf("the charge behind the reset gave %v with the charge errors %v, want 503 after the lock timeout (55P03)", err, got)
		}
		// A charge holds the one slot of the pool of the charges, so a second charge waits for a slot, and the wait is
		// logged before the charge waits (walkthrough 22, P1-PS: the slots never let the charges hold more
		// connections than the pool has, so only the wait for a slot shows a full pool). full holds the first charge
		// at its connection until the second waits for a slot, then lets both end and gives the result of the second.
		full := func(key string) error {
			t.Helper()
			hold, begun := make(chan struct{}), make(chan struct{})
			var once sync.Once
			rt.chargeBegun = func(string) { once.Do(func() { close(begun); <-hold }) }
			defer func() { rt.chargeBegun = nil }()
			first, done := make(chan error, 1), make(chan error, 1)
			go func() { first <- check(key+"-first", "0000") }()
			<-begun
			go func() { done <- check(key, "0000") }()
			for rt.chargeSlots.blocked("t1") == 0 {
				runtime.Gosched()
			}
			close(hold)
			if err := <-first; err == nil || errors.Is(err, ErrUnavailable) {
				t.Fatalf("the charge that held the pool gave %v, want the refusal of the wrong PIN", err)
			}
			return <-done
		}
		var logs lockedBuffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		t.Cleanup(func() { slog.SetDefault(previous) })
		result := full("full-1")
		if result == nil || errors.Is(result, ErrUnavailable) {
			t.Fatalf("the charge after the wait for the pool gave %v, want the refusal of the wrong PIN", result)
		}
		text := logs.String()
		if !strings.Contains(text, PoolSaturatedEvent) || !strings.Contains(text, "entity=PinWallet") || !strings.Contains(text, "max_open=1") || !strings.Contains(text, "in_use=1") || !strings.Contains(text, "waiting=1") ||
			strings.Contains(text, "0000") || strings.Contains(text, "argon2") || strings.Contains(text, "t1") {
			t.Fatalf("the log of the full pool is %q, want the entity and the counts of the pool and no value", text)
		}
		// With a log provider, the line goes through it, as each other log line of the runtime.
		records := &logRecorder{}
		rt.logger = sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(records))).Logger("test")
		rt.saturationLogged.Store(0)
		saturated := func() []sdklog.Record {
			var lines []sdklog.Record
			for _, r := range records.all() {
				if r.EventName() == PoolSaturatedEvent {
					lines = append(lines, r)
				}
			}
			return lines
		}
		result = full("full-2")
		if result == nil || errors.Is(result, ErrUnavailable) {
			t.Fatalf("the charge after the wait for the pool gave %v, want the refusal of the wrong PIN", result)
		}
		lines := saturated()
		if len(lines) != 1 {
			t.Fatalf("the provider got %d lines of %s, want one", len(lines), PoolSaturatedEvent)
		}
		attrs := map[string]string{}
		lines[0].WalkAttributes(func(kv attribute.KeyValue) bool {
			attrs[string(kv.Key)] = kv.Value.Emit()
			return true
		})
		if len(lines) != 1 || lines[0].SeverityText() != "WARN" || attrs["stego.entity"] != "PinWallet" || attrs["stego.pool.max_open"] != "1" ||
			attrs["stego.pool.in_use"] != "1" || attrs["stego.pool.waiting"] != "1" || strings.Contains(lines[0].Body().Emit(), "0000") {
			t.Fatalf("the provider got %d lines of %s with the attributes %v, want one WARN line with the entity and the pool and no value", len(lines), PoolSaturatedEvent, attrs)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T11 A charge that waits for a slot while the charges hold each shared slot logs a full pool, and a wait for the limit of its own tenant does not", func(t *testing.T) {
		const walletID = "0198a1b2-0000-7000-8000-000000000005"
		e := newEnv(t)
		pinWallets(t, e, walletID, "4711")
		hash, err := sealed.Hash("4711")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.admin.ExecContext(e.ctx, `INSERT INTO pin_wallets (tenant_id, id, balance, pin) VALUES ('t2', $1, 100, $2)`, walletID, hash); err != nil {
			t.Fatal(err)
		}
		wallets := pinWalletEntity(t)
		// A pool of 4: 3 shared slots, at most 2 for one tenant, and the reserved slot.
		chargeDB := e.openPool(e.dsn(e.roles[0]))
		chargeDB.SetMaxOpenConns(4)
		rt := e.runtime(func(c *Config) { c.Entities, c.ChargeDB = []Declared{wallets}, chargeDB })
		records := &logRecorder{}
		rt.logger = sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(records))).Logger("test")
		saturated := func() []sdklog.Record {
			var lines []sdklog.Record
			for _, r := range records.all() {
				if r.EventName() == PoolSaturatedEvent {
					lines = append(lines, r)
				}
			}
			return lines
		}
		// Each charge keeps its connection until release closes; begun counts the charges of each tenant that have one.
		release := make(chan struct{})
		var mu sync.Mutex
		begun := map[string]int{}
		rt.chargeBegun = func(id string) {
			mu.Lock()
			begun[id]++
			mu.Unlock()
			<-release
		}
		began := func(id string) int {
			mu.Lock()
			defer mu.Unlock()
			return begun[id]
		}
		cmd := e.command("withdraw", nil, wallets)
		results := make(chan error, 6)
		check := func(tenantID, key string) {
			go func() {
				_, err := e.runOn(rt, cmd, e.request(tenantID, u7(t), key, `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
					r, err := Repo(h, wallets)
					if err != nil {
						return idempotency.Response{}, err
					}
					w, err := r.Get(ctx, walletID)
					if err != nil {
						return idempotency.Response{}, err
					}
					if !w.Value().PIN.Matches("0000") {
						return idempotency.Response{}, errors.New("pin.wrong")
					}
					return ok, nil
				})
				results <- err
			}()
		}
		// t1 holds its 2 slots, and its third charge waits for the limit of t1 while a shared slot is free: that is
		// no full pool.
		check("t1", "t1-1")
		check("t1", "t1-2")
		for began("t1") < 2 {
			runtime.Gosched()
		}
		check("t1", "t1-3")
		for rt.chargeSlots.blocked("t1") < 1 {
			runtime.Gosched()
		}
		if lines := saturated(); len(lines) != 0 {
			close(release)
			t.Fatalf("a charge that waits for the limit of its tenant logged %d lines of %s, want none", len(lines), PoolSaturatedEvent)
		}
		// t2 takes the last shared slot, and its second charge waits: each shared slot is in use, and t2 cannot take
		// the reserved slot while it holds a slot. The connections in use stay under the pool of 4.
		check("t2", "t2-1")
		for began("t2") < 1 {
			runtime.Gosched()
		}
		check("t2", "t2-2")
		for rt.chargeSlots.blocked("t2") == 0 {
			runtime.Gosched()
		}
		inUse := chargeDB.Stats().InUse
		close(release)
		for range 5 {
			if err := <-results; err == nil || errors.Is(err, ErrUnavailable) {
				t.Fatalf("a check gave %v, want the refusal of the wrong PIN", err)
			}
		}
		lines := saturated()
		if len(lines) != 1 {
			t.Fatalf("the provider got %d lines of %s, want one", len(lines), PoolSaturatedEvent)
		}
		attrs := map[string]string{}
		lines[0].WalkAttributes(func(kv attribute.KeyValue) bool {
			attrs[string(kv.Key)] = kv.Value.Emit()
			return true
		})
		if len(lines) != 1 || attrs["stego.entity"] != "PinWallet" || attrs["stego.pool.max_open"] != "4" || attrs["stego.pool.in_use"] != "3" ||
			attrs["stego.pool.waiting"] != "2" || attrs["stego.tenant"] != "" || inUse >= 4 {
			t.Fatalf("the provider got %d lines of %s with the attributes %v and %d connections in use, want one line with 3 slots in use and 2 charges waiting of a pool of 4, under 4 connections in use and no tenant", len(lines), PoolSaturatedEvent, attrs, inUse)
		}
	})
}

// lockedBuffer is a bytes.Buffer that a log handler and the test can use at one time.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// decayWallet is a PIN wallet whose PIN has the counter given, in an env of its own: check runs withdraw with a PIN,
// count and failures read the counts of the row, chargedAt the time of its last charge, and age moves that time back.
type decayWallet struct {
	e         *env
	check     func(pin string) bool
	count     func() int
	failures  func() int
	chargedAt func() string
	age       func(interval string)
}

func newDecayWallet(t *testing.T, counter *Counter, walletID, right string) decayWallet {
	t.Helper()
	e := newEnv(t)
	pinWallets(t, e, walletID, right)
	wallets := pinWalletEntityOf(t, counter)
	rt := e.runtime(func(c *Config) { c.Entities = []Declared{wallets} })
	cmd := e.command("withdraw", nil, wallets)
	errWrongPIN := errors.New("pin.wrong")
	n := 0
	w := decayWallet{e: e}
	w.check = func(pin string) bool {
		t.Helper()
		n++
		var matched bool
		_, err := e.runOn(rt, cmd, e.request("t1", u7(t), fmt.Sprintf("decay-%d", n), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			r, err := Repo(h, wallets)
			if err != nil {
				return idempotency.Response{}, err
			}
			w, err := r.GetForUpdate(ctx, walletID)
			if err != nil {
				return idempotency.Response{}, err
			}
			if matched = w.Value().PIN.Matches(pin); !matched {
				return idempotency.Response{}, errWrongPIN
			}
			return ok, nil
		})
		if !matched && !errors.Is(err, errWrongPIN) || matched && err != nil {
			t.Fatalf("the check of %q gave %v", pin, err)
		}
		return matched
	}
	w.count = func() int {
		t.Helper()
		return e.count(`SELECT coalesce(sum(checks), 0) FROM attempt_counts WHERE counter = 'PinWallet.pin' AND row_id = $1`, walletID)
	}
	w.failures = func() int {
		t.Helper()
		// The checks since the last match or new value: the checks of the windows before and of this one.
		return e.count(`SELECT coalesce(sum(coalesce(failures, 0) + checks), 0) FROM attempt_counts WHERE counter = 'PinWallet.pin' AND row_id = $1`, walletID)
	}
	w.chargedAt = func() string {
		t.Helper()
		var at sql.NullString
		if err := e.admin.QueryRowContext(e.ctx, `SELECT charged_at::text FROM attempt_counts WHERE counter = 'PinWallet.pin' AND row_id = $1`, walletID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at.String
	}
	w.age = func(interval string) {
		t.Helper()
		e.exec(`UPDATE attempt_counts SET charged_at = pg_catalog.statement_timestamp() - interval '` + interval + `' WHERE row_id = '` + walletID + `'`)
	}
	return w
}

// TestAttemptDecay proves that a count decays after the window of its counter on the clock of the database, that no
// check moves the end of the window of a row at its limit, that a counter with no window does not decay, and that the
// failures of a row never decay (decisions ST-SUP-Q205 (c) and ST-SUP-Q207 (a)).
func TestAttemptDecay(t *testing.T) {
	const right = "4711"
	t.Run("ST-SUP-RP10-LC54-T12 A row at its limit opens again only after the window of its last charge on the clock of the database", func(t *testing.T) {
		for _, c := range []struct {
			after time.Duration
			ok    bool
			want  time.Duration
		}{{0, true, 0}, {time.Minute, true, time.Minute}, {24 * time.Hour, true, 24 * time.Hour},
			{59 * time.Second, false, 0}, {25 * time.Hour, false, 0}, {90*time.Second + 500*time.Millisecond, false, 0}, {-time.Minute, false, 0}} {
			counter, err := NewCounter(CounterSpec{Entity: "PinWallet", Field: "pin", Limit: 3, UnlockAfter: c.after})
			if c.ok != (err == nil) || (c.ok && counter.UnlockAfter() != c.want) {
				t.Fatalf("NewCounter with the window %s gave %v, %v; want ok %v and the window %s", c.after, counter, err, c.ok, c.want)
			}
		}
		const walletID = "0198a1b2-0000-7000-8000-000000000012"
		w := newDecayWallet(t, pinCounter, walletID, right)
		for range 3 {
			w.check("0000")
		}
		at := w.chargedAt()
		if w.count() != 3 || at == "" || w.check(right) {
			t.Fatalf("three wrong PINs left the count %d and the time %q, and the right PIN matched; want 3, a time and no match", w.count(), at)
		}
		// A check at the limit charges nothing, so it does not move the end of the window.
		if w.check(right); w.chargedAt() != at || w.count() != 3 {
			t.Fatalf("a check at the limit moved the time of the last charge from %s to %s, or the count to %d", at, w.chargedAt(), w.count())
		}
		// 5 s before the end of the window of 15 minutes, the row is still at its limit.
		w.age("14 minutes 55 seconds")
		if w.check(right) || w.count() != 3 {
			t.Fatalf("the right PIN matched 5 s before the end of the window, with the count %d", w.count())
		}
		// After the window, the count starts again at 1, the failures do not, and the right PIN matches and resets both.
		w.age("15 minutes 1 second")
		if w.check("0000") || w.count() != 1 || w.failures() != 4 {
			t.Fatalf("a wrong PIN after the window left the count %d and the failures %d, want 1 and 4", w.count(), w.failures())
		}
		if !w.check(right) || w.count() != 0 || w.failures() != 0 {
			t.Fatalf("the right PIN after the window gave no match or left the count %d and the failures %d", w.count(), w.failures())
		}
		// A count with no time, as a charge of a release before the decay leaves it, counts as charged now: the row at
		// its limit stays locked, gets the time of the check, and opens one window after it (security review of PR
		// #489, L1).
		w.e.exec(`INSERT INTO attempt_counts (tenant_id, counter, row_id, checks) VALUES ('t1', 'PinWallet.pin', '` + walletID + `', 3)`)
		if w.check(right) || w.count() != 3 || w.chargedAt() == "" {
			t.Fatalf("the right PIN on a count of 3 with no time matched, or left the count %d and the time %q; want no match, 3 and a time", w.count(), w.chargedAt())
		}
		w.age("15 minutes 1 second")
		if !w.check(right) {
			t.Fatal("the right PIN one window after the stamp gave no match")
		}
		// A counter with no window does not decay: the row stays at its limit after a day.
		fixed := newDecayWallet(t, fixedCounter, walletID, right)
		for range 3 {
			fixed.check("0000")
		}
		fixed.age("25 hours")
		if fixed.check(right) || fixed.count() != 3 {
			t.Fatalf("the right PIN of a counter with no window matched after a day, or left the count %d", fixed.count())
		}
	})
	t.Run("ST-SUP-RP10-LC54-T16 At the cap of its failures a row stays locked after the window until a new value", func(t *testing.T) {
		if FailureCap(2) != 20 || FailureCap(5) != 50 || FailureCap(10) != 100 || FailureCap(100) != 100 {
			t.Fatalf("FailureCap gave %d, %d, %d and %d, want 20, 50, 100 and 100", FailureCap(2), FailureCap(5), FailureCap(10), FailureCap(100))
		}
		const walletID = "0198a1b2-0000-7000-8000-000000000016"
		counter := Must(NewCounter(CounterSpec{Entity: "PinWallet", Field: "pin", Limit: 2, UnlockAfter: time.Minute}))
		w := newDecayWallet(t, counter, walletID, right)
		// Ten windows of two wrong PINs each reach the cap of 20 failures.
		for i := range 10 {
			w.check("0000")
			w.check("0000")
			if w.failures() != 2*(i+1) {
				t.Fatalf("window %d left the failures %d, want %d", i+1, w.failures(), 2*(i+1))
			}
			w.age("61 seconds")
		}
		// Past the window and at the cap, the right PIN gives no match and charges nothing.
		at := w.chargedAt()
		if w.check(right) || w.failures() != 20 || w.chargedAt() != at {
			t.Fatalf("the right PIN at the cap matched, or left the failures %d and the time %s (was %s)", w.failures(), w.chargedAt(), at)
		}
		w.age("25 hours")
		if w.check(right) {
			t.Fatal("the right PIN at the cap matched after a day")
		}
		// The delete of the count, which a new value and a reset command make at commit, opens the row.
		w.e.exec(`DELETE FROM attempt_counts WHERE row_id = '` + walletID + `'`)
		if !w.check(right) {
			t.Fatal("the right PIN after the reset gave no match")
		}
	})
	t.Run("ST-SUP-RP10-LC54-T17 The checks that a pod of a release before adds count toward the cap of the failures", func(t *testing.T) {
		const walletID = "0198a1b2-0000-7000-8000-000000000017"
		counter := Must(NewCounter(CounterSpec{Entity: "PinWallet", Field: "pin", Limit: 2, UnlockAfter: time.Minute}))
		w := newDecayWallet(t, counter, walletID, right)
		// A row of failures 18 whose two checks of this window a pod of a release before charged, which raises only
		// checks: 20 checks since the last match, the cap of the limit 2.
		w.e.exec(`INSERT INTO attempt_counts (tenant_id, counter, row_id, checks, failures, charged_at) VALUES ('t1', 'PinWallet.pin', '` + walletID + `', 2, 18, pg_catalog.statement_timestamp() - interval '61 seconds')`)
		if w.check(right) || w.failures() != 20 {
			t.Fatalf("the right PIN after the window of a row at the cap matched, or left the checks %d, want no match and 20", w.failures())
		}
		// Below the cap, the decay keeps the checks of the window before in failures.
		w.e.exec(`UPDATE attempt_counts SET failures = 10 WHERE row_id = '` + walletID + `'`)
		w.age("61 seconds")
		if w.check("0000") || w.failures() != 13 || w.count() != 1 {
			t.Fatalf("a wrong PIN after the window left the checks %d and the count %d, want 13 and 1", w.failures(), w.count())
		}
	})
}

// lockedWallet is a test entity of the resource type locked_wallet with a counted PIN of limit 2, which the command
// unlock may reset (ST-DECL-INV104).
type lockedWallet struct {
	ID  string
	PIN sealed.Compared
}

var lockedCounter = Must(NewCounter(CounterSpec{Entity: "LockedWallet", Field: "pin", Limit: 2, UnlockAfter: time.Hour}))

// TestAttemptResets proves the declared step-up reset of a locked row (decision ST-SUP-Q205 (c)).
func TestAttemptResets(t *testing.T) {
	t.Run("ST-SUP-RP10-LC54-T13 Only a command that declares the reset resets a count, on a row that its own permission allows, when it commits", func(t *testing.T) {
		const a, b, right = "0198a1b2-0000-7000-8000-0000000000a1", "0198a1b2-0000-7000-8000-0000000000b1", "4711"
		e := newEnv(t)
		e.exec(`CREATE TABLE locked_wallets (tenant_id text NOT NULL, id text NOT NULL, pin text, version bigint NOT NULL DEFAULT 1,
  authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))`, "GRANT SELECT, INSERT, UPDATE, DELETE ON locked_wallets TO "+e.roles[0])
		hash, err := sealed.Hash(right)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{a, b} {
			if _, err := e.admin.ExecContext(e.ctx, `INSERT INTO locked_wallets (tenant_id, id, pin) VALUES ('t1', $1, $2)`, id, hash); err != nil {
				t.Fatal(err)
			}
		}
		s := spec("LockedWallet", "locked_wallet", "locked_wallets", []string{"pin"},
			func(row Scanner) (lockedWallet, Meta, error) {
				var v lockedWallet
				var m Meta
				err := row.Scan(&v.ID, CountedAt(lockedCounter, &v.ID, &v.PIN), &m.Version, &m.Token)
				m.ID = v.ID
				return v, m, err
			},
			func(v lockedWallet) []any { return []any{v.ID, HashCountedOf(lockedCounter, v.ID, v.PIN)} },
			func(v lockedWallet) map[string]any { return map[string]any{"id": v.ID} })
		wallets, err := NewEntity(s)
		if err != nil {
			t.Fatal(err)
		}
		rt := e.runtime(func(c *Config) { c.Entities = []Declared{wallets} })
		var charges atomic.Int32
		rt.charged = func(bool) { charges.Add(1) }
		resource := &Resource{Type: "locked_wallet", Token: `SELECT authz_token FROM locked_wallets WHERE tenant_id = $1 AND id = $2 FOR SHARE`,
			ValidID: func(id string) bool { return id != "" }}
		for name, change := range map[string]func(*CommandSpec[testProbe]){
			"no permission": func(s *CommandSpec[testProbe]) { s.Resets = []string{"LockedWallet.pin"} },
			"an entity not written": func(s *CommandSpec[testProbe]) {
				s.Permission, s.Resource, s.Resets = "unlock", resource, []string{"PinWallet.pin"}
			},
			"no field": func(s *CommandSpec[testProbe]) {
				s.Permission, s.Resource, s.Resets = "unlock", resource, []string{"LockedWallet"}
			},
			"the service-level resource": func(s *CommandSpec[testProbe]) {
				s.Permission, s.Resource, s.Resets = "unlock", &Resource{Type: "service", Service: "wallets"}, []string{"LockedWallet.pin"}
			},
		} {
			spec := CommandSpec[testProbe]{Name: "unlock", Writes: []Declared{wallets}, Probe: newTestProbe}
			change(&spec)
			if _, err := NewCommand(spec); err == nil {
				t.Fatalf("NewCommand takes a reset with %s", name)
			}
		}
		for name, change := range map[string]func(*CommandSpec[testProbe]){
			"a guard with no reset": func(s *CommandSpec[testProbe]) {
				s.Permission, s.Resource, s.ResetGuards = "unlock", resource, []string{"operate"}
			},
			"its own permission as a guard": func(s *CommandSpec[testProbe]) {
				s.Permission, s.Resource, s.Resets, s.ResetGuards = "unlock", resource, []string{"LockedWallet.pin"}, []string{"unlock"}
			},
		} {
			spec := CommandSpec[testProbe]{Name: "unlock", Writes: []Declared{wallets}, Probe: newTestProbe}
			change(&spec)
			if _, err := NewCommand(spec); err == nil {
				t.Fatalf("NewCommand takes %s", name)
			}
		}
		withdraw := e.command("withdraw", func(s *CommandSpec[testProbe]) { s.Permission, s.Resource = "operate", resource }, wallets)
		unlock := e.command("unlock", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource, s.Resets, s.ResetGuards = "unlock", resource, []string{"LockedWallet.pin"}, []string{"operate"}
		}, wallets)
		// u-1 owns a; u-2 may unlock b and view a; u-3 is the support desk, which may unlock a.
		e.decision.setRelations(relation(t, "user:u-1", "operate", "t1", "locked_wallet", a), relation(t, "user:u-1", "view", "t1", "locked_wallet", a),
			relation(t, "user:u-2", "unlock", "t1", "locked_wallet", b), relation(t, "user:u-2", "view", "t1", "locked_wallet", a),
			relation(t, "user:u-2", "view", "t1", "locked_wallet", b),
			relation(t, "user:u-3", "unlock", "t1", "locked_wallet", a), relation(t, "user:u-3", "view", "t1", "locked_wallet", a),
			relation(t, "user:u-4", "unlock", "t1", "locked_wallet", a), relation(t, "user:u-4", "operate", "t1", "locked_wallet", a),
			relation(t, "user:u-4", "view", "t1", "locked_wallet", a))
		n := 0
		errFill := errors.New("the fill fails")
		// call runs cmd as subject on the resource, with a fill that reads the wallet a and then runs body.
		call := func(cmd *Command[testProbe], subject, on string, body func(ctx context.Context, h *Handle[testProbe], w Loaded[lockedWallet]) error) error {
			t.Helper()
			n++
			req := e.request("t1", person(t, subject), fmt.Sprintf("reset-%d", n), `{}`)
			req.Resource = on
			_, err := e.runOn(rt, cmd, req, func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
				r, err := Repo(h, wallets)
				if err != nil {
					return idempotency.Response{}, err
				}
				w, err := r.Get(ctx, a)
				if err != nil {
					return idempotency.Response{}, err
				}
				return ok, body(ctx, h, w)
			})
			return err
		}
		errWrongPIN := errors.New("pin.wrong")
		check := func(pin string) bool {
			t.Helper()
			matched := false
			err := call(withdraw, "user:u-1", a, func(_ context.Context, _ *Handle[testProbe], w Loaded[lockedWallet]) error {
				if matched = w.Value().PIN.Matches(pin); !matched {
					return errWrongPIN
				}
				return nil
			})
			if !matched && !errors.Is(err, errWrongPIN) || matched && err != nil {
				t.Fatalf("the check of %q gave %v", pin, err)
			}
			return matched
		}
		reset := func(ctx context.Context, h *Handle[testProbe], w Loaded[lockedWallet]) error {
			return ResetAttempts(ctx, h, lockedCounter, w)
		}
		count := func() int {
			t.Helper()
			return e.count(`SELECT coalesce(sum(checks), 0) FROM attempt_counts WHERE counter = 'LockedWallet.pin' AND row_id = $1`, a)
		}
		check("0000")
		check("0000")
		if count() != 2 || check(right) {
			t.Fatalf("two wrong PINs left the count %d, or the right PIN matched at the limit", count())
		}
		// The owner's command, which may check the PIN, declares no reset.
		if err := call(withdraw, "user:u-1", a, reset); err == nil || count() != 2 {
			t.Fatalf("a reset in a command that declares none gave %v and left the count %d, want an error and 2", err, count())
		}
		resets := func() int {
			t.Helper()
			return e.count(`SELECT count(*) FROM audit_event WHERE action = 'unlock' AND resource_id = $1`, a)
		}
		// u-2 may unlock b, and may only view a: the reset of a is decided with unlock on a, and denied.
		if err := call(unlock, "user:u-2", b, reset); !errors.Is(err, ErrNotFound) || count() != 2 || resets() != 0 {
			t.Fatalf("the reset of a by a caller who may unlock only b gave %v and left the count %d, want ErrNotFound and 2", err, count())
		}
		// u-4 may unlock a and also operate it, so it could check the PIN and reset it after each check: the reset is
		// refused, whatever the schema (security review of PR #489, second review, M2b).
		if err := call(unlock, "user:u-4", a, reset); !errors.Is(err, ErrDenied) || count() != 2 || resets() != 0 {
			t.Fatalf("the reset of a caller who may also operate a gave %v, left the count %d and wrote %d audit rows, want ErrDenied, 2 and none", err, count(), resets())
		}
		// The reset of the support desk commits only with its command.
		if err := call(unlock, "user:u-3", a, func(ctx context.Context, h *Handle[testProbe], w Loaded[lockedWallet]) error {
			if err := reset(ctx, h, w); err != nil {
				return err
			}
			return errFill
		}); !errors.Is(err, errFill) || count() != 2 || resets() != 0 {
			t.Fatalf("a reset whose command fails gave %v, left the count %d and wrote %d audit rows, want the error of the fill, 2 and none", err, count(), resets())
		}
		// The command that may reset the count does not check the PIN: the check gives no match, charges nothing and
		// fails the call (security review of PR #489, I1).
		before := charges.Load()
		if err := call(unlock, "user:u-3", a, func(_ context.Context, _ *Handle[testProbe], w Loaded[lockedWallet]) error {
			if w.Value().PIN.Matches(right) {
				return errors.New("the check in the reset command matched")
			}
			return nil
		}); err == nil || charges.Load() != before || count() != 2 {
			t.Fatalf("a check in the reset command gave %v, charged %d checks and left the count %d, want an error, none and 2", err, charges.Load()-before, count())
		}
		if err := call(unlock, "user:u-3", a, reset); err != nil || count() != 0 || charges.Load() != before || resets() != 1 {
			t.Fatalf("the reset of the support desk gave %v, left the count %d, charged %d checks and wrote %d audit rows, want nil, 0, none and 1", err, count(), charges.Load()-before, resets())
		}
		var marker string
		if err := e.admin.QueryRowContext(e.ctx, `SELECT after::text FROM audit_event WHERE action = 'unlock' AND resource_id = $1`, a).Scan(&marker); err != nil || !strings.Contains(marker, `"attempts_reset": "LockedWallet.pin"`) || strings.Contains(marker, right) {
			t.Fatalf("the audit row of the reset holds %q (%v), want the marker attempts_reset LockedWallet.pin and no PIN", marker, err)
		}
		// The answer of a reset is the same for a row with no count: it tells nothing of the secret.
		if err := call(unlock, "user:u-3", a, reset); err != nil {
			t.Fatalf("the reset of a row with no count gave %v, want nil", err)
		}
		if !check(right) {
			t.Fatal("the right PIN gave no match after the reset")
		}
	})
}

// TestTenantSlotScans proves that a charge that waits for the limit of its own tenant reads the counts of the
// waiting charges of all the tenants only for the log of a full pool (security review of PR #488, L2).
func TestTenantSlotScans(t *testing.T) {
	t.Run("ST-SUP-RP10-LC54-T14 A charge that waits with no report of a full pool does not count the waiting charges of all the tenants", func(t *testing.T) {
		var s tenantSlots
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reports := 0
		full := func(int, int) { reports++ }
		release, err := s.take(ctx, "t1", 1, 3, full)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			second, err := s.take(ctx, "t1", 1, 3, full)
			if err == nil {
				second()
			}
			done <- err
		}()
		for s.blocked("t1") == 0 {
			runtime.Gosched()
		}
		// A slot of t2 comes and goes, which wakes the waiting charge of t1 again.
		other, err := s.take(ctx, "t2", 1, 3, full)
		if err != nil {
			t.Fatal(err)
		}
		other()
		for s.blocked("t1") == 0 {
			runtime.Gosched()
		}
		s.mu.Lock()
		scans := s.scans
		s.mu.Unlock()
		if scans != 0 || reports != 0 {
			t.Fatalf("the waits for the limit of t1 counted the waiting charges %d times and reported %d full pools, want 0 and 0", scans, reports)
		}
		release()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// guardedCounter is the counter of GuardedWallet.pin, which a command resets (CounterSpec.Reset).
var guardedCounter = Must(NewCounter(CounterSpec{Entity: "GuardedWallet", Field: "pin", Limit: 3, Reset: true}))

// TestAttemptResetChecks proves that a counter that a command may reset is checked only on a row on which the
// command decided its own permission, so each caller who may check the row holds a reset guard on it (decision
// ST-SUP-Q209 (d), the residual sec-489c of the second security review of PR #489).
func TestAttemptResetChecks(t *testing.T) {
	t.Run("ST-SUP-RP10-LC54-T18 A counter with a reset is checked only on a row on which the command decided its own permission", func(t *testing.T) {
		const a, b, p, right = "0198a1b2-0000-7000-8000-0000000000a2", "0198a1b2-0000-7000-8000-0000000000b2", "0198a1b2-0000-7000-8000-0000000000c2", "4711"
		e := newEnv(t)
		e.exec(`CREATE TABLE guarded_wallets (tenant_id text NOT NULL, id text NOT NULL, pin text, version bigint NOT NULL DEFAULT 1,
  authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))`, "GRANT SELECT, INSERT, UPDATE, DELETE ON guarded_wallets TO "+e.roles[0])
		hash, err := sealed.Hash(right)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{a, b} {
			if _, err := e.admin.ExecContext(e.ctx, `INSERT INTO guarded_wallets (tenant_id, id, pin) VALUES ('t1', $1, $2)`, id, hash); err != nil {
				t.Fatal(err)
			}
		}
		s := spec("GuardedWallet", "guarded_wallet", "guarded_wallets", []string{"pin"},
			func(row Scanner) (lockedWallet, Meta, error) {
				var v lockedWallet
				var m Meta
				err := row.Scan(&v.ID, CountedAt(guardedCounter, &v.ID, &v.PIN), &m.Version, &m.Token)
				m.ID = v.ID
				return v, m, err
			},
			func(v lockedWallet) []any { return []any{v.ID, HashCountedOf(guardedCounter, v.ID, v.PIN)} },
			func(v lockedWallet) map[string]any { return map[string]any{"id": v.ID} })
		wallets, err := NewEntity(s)
		if err != nil {
			t.Fatal(err)
		}
		rt := e.runtime(func(c *Config) { c.Entities = []Declared{wallets} })
		var charges atomic.Int32
		rt.charged = func(bool) { charges.Add(1) }
		token := `SELECT authz_token FROM guarded_wallets WHERE tenant_id = $1 AND id = $2 FOR SHARE`
		wallet := &Resource{Type: "guarded_wallet", Token: token, ValidID: func(id string) bool { return id != "" }}
		payout := &Resource{Type: "payout", Token: token, ValidID: func(id string) bool { return id != "" }}
		withdraw := e.command("withdraw", func(s *CommandSpec[testProbe]) { s.Permission, s.Resource = "operate", wallet }, wallets)
		// pay is a command of another resource type that writes the wallet, as a payout that checks the PIN of the
		// wallet that it names.
		pay := e.command("pay", func(s *CommandSpec[testProbe]) { s.Permission, s.Resource = "pay", payout }, wallets)
		unlock := e.command("unlock", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource, s.Resets, s.ResetGuards = "unlock", wallet, []string{"GuardedWallet.pin"}, []string{"operate", "pay"}
		}, wallets)
		// u-1 owns a. u-5 is the support desk of a, which may unlock and view a and operate its own wallet b. u-6 may
		// unlock and view a and pay the payout p. u-7 may unlock a, pay on a and pay the payout p.
		e.decision.setRelations(relation(t, "user:u-1", "operate", "t1", "guarded_wallet", a), relation(t, "user:u-1", "view", "t1", "guarded_wallet", a),
			relation(t, "user:u-5", "unlock", "t1", "guarded_wallet", a), relation(t, "user:u-5", "view", "t1", "guarded_wallet", a),
			relation(t, "user:u-5", "operate", "t1", "guarded_wallet", b), relation(t, "user:u-5", "view", "t1", "guarded_wallet", b),
			relation(t, "user:u-6", "unlock", "t1", "guarded_wallet", a), relation(t, "user:u-6", "view", "t1", "guarded_wallet", a),
			relation(t, "user:u-6", "pay", "t1", "payout", p),
			relation(t, "user:u-7", "unlock", "t1", "guarded_wallet", a), relation(t, "user:u-7", "view", "t1", "guarded_wallet", a),
			relation(t, "user:u-7", "pay", "t1", "guarded_wallet", a), relation(t, "user:u-7", "pay", "t1", "payout", p))
		n := 0
		// call runs cmd as subject on the resource on, with a fill that reads the wallet a, locked or not, and then
		// runs body.
		call := func(cmd *Command[testProbe], subject, on string, locked bool, body func(ctx context.Context, h *Handle[testProbe], w Loaded[lockedWallet]) error) error {
			t.Helper()
			n++
			req := e.request("t1", person(t, subject), fmt.Sprintf("guarded-%d", n), `{}`)
			req.Resource = on
			_, err := e.runOn(rt, cmd, req, func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
				r, err := Repo(h, wallets)
				if err != nil {
					return idempotency.Response{}, err
				}
				read := r.Get
				if locked {
					read = func(ctx context.Context, id string) (Loaded[lockedWallet], error) { return r.GetForUpdate(ctx, id) }
				}
				w, err := read(ctx, a)
				if err != nil {
					return idempotency.Response{}, err
				}
				return ok, body(ctx, h, w)
			})
			return err
		}
		errWrongPIN := errors.New("pin.wrong")
		checkWith := func(pin string) func(context.Context, *Handle[testProbe], Loaded[lockedWallet]) error {
			return func(_ context.Context, _ *Handle[testProbe], w Loaded[lockedWallet]) error {
				if !w.Value().PIN.Matches(pin) {
					return errWrongPIN
				}
				return nil
			}
		}
		count := func() int {
			t.Helper()
			return e.count(`SELECT coalesce(sum(checks), 0) FROM attempt_counts WHERE counter = 'GuardedWallet.pin' AND row_id = $1`, a)
		}
		reset := func(ctx context.Context, h *Handle[testProbe], w Loaded[lockedWallet]) error {
			return ResetAttempts(ctx, h, guardedCounter, w)
		}
		// The support desk decides operate on its own wallet b and reads a with no lock, through view: the check of
		// the PIN of a gives no match, charges nothing and fails the call, so the desk cannot check a and reset it.
		before := charges.Load()
		if err := call(withdraw, "user:u-5", b, false, checkWith(right)); err == nil || errors.Is(err, errWrongPIN) || charges.Load() != before || count() != 0 {
			t.Fatalf("a check of a from the row b gave %v and charged %d checks, want an internal error and none", err, charges.Load()-before)
		}
		// Locked, the read decides operate on a, which the desk does not hold.
		if err := call(withdraw, "user:u-5", b, true, checkWith(right)); !errors.Is(err, ErrNotFound) || charges.Load() != before {
			t.Fatalf("a locked read of a from the row b gave %v, want ErrNotFound and no charge", err)
		}
		// From another resource type: pay is decided on the payout p, and a is read with no lock.
		if err := call(pay, "user:u-6", p, false, checkWith(right)); err == nil || errors.Is(err, errWrongPIN) || charges.Load() != before {
			t.Fatalf("a check of a from the payout p gave %v and charged %d checks, want an internal error and none", err, charges.Load()-before)
		}
		if err := call(pay, "user:u-6", p, true, checkWith(right)); !errors.Is(err, ErrNotFound) || charges.Load() != before {
			t.Fatalf("a locked read of a from the payout p by a caller with no pay on a gave %v, want ErrNotFound and no charge", err)
		}
		// A caller who holds pay on a checks it on the locked row, and so may not reset it: pay is a reset guard.
		if err := call(pay, "user:u-7", p, true, checkWith("0000")); !errors.Is(err, errWrongPIN) || count() != 1 {
			t.Fatalf("a check of the locked a by a caller with pay on a gave %v and left the count %d, want the wrong PIN and 1", err, count())
		}
		if err := call(unlock, "user:u-7", a, false, reset); !errors.Is(err, ErrDenied) || count() != 1 {
			t.Fatalf("the reset of a caller who may pay on a gave %v and left the count %d, want ErrDenied and 1", err, count())
		}
		// The owner checks on the row of its command, which Run decided, also with no lock.
		if err := call(withdraw, "user:u-1", a, false, checkWith("0000")); !errors.Is(err, errWrongPIN) || count() != 2 {
			t.Fatalf("the owner's check of a gave %v and left the count %d, want the wrong PIN and 2", err, count())
		}
		// The support desk, which holds no guard on a, resets it; and the owner's right PIN then matches.
		if err := call(unlock, "user:u-5", a, false, reset); err != nil || count() != 0 {
			t.Fatalf("the reset of the support desk gave %v and left the count %d, want nil and 0", err, count())
		}
		if err := call(withdraw, "user:u-1", a, true, checkWith(right)); err != nil {
			t.Fatalf("the owner's right PIN on the locked row gave %v, want a match", err)
		}
	})
}

// legacyCounter is a counter of a generator before command 1.26.0, with no Reset, whose field a command resets.
var legacyCounter = Must(NewCounter(CounterSpec{Entity: "LegacyWallet", Field: "pin", Limit: 3}))

// resetWallets makes the table of the entity name with the wallets a and b, whose PIN is right, and gives the entity
// with the counted PIN of counter c, on the resource type of table, and the resource of a command on a wallet.
func resetWallets(t *testing.T, e *env, name, table, resourceType string, c *Counter, right string, ids ...string) (*Entity[lockedWallet], *Resource) {
	t.Helper()
	e.exec(`CREATE TABLE `+table+` (tenant_id text NOT NULL, id text NOT NULL, pin text, version bigint NOT NULL DEFAULT 1,
  authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))`, "GRANT SELECT, INSERT, UPDATE, DELETE ON "+table+" TO "+e.roles[0])
	hash, err := sealed.Hash(right)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := e.admin.ExecContext(e.ctx, `INSERT INTO `+table+` (tenant_id, id, pin) VALUES ('t1', $1, $2)`, id, hash); err != nil {
			t.Fatal(err)
		}
	}
	s := spec(name, resourceType, table, []string{"pin"},
		func(row Scanner) (lockedWallet, Meta, error) {
			var v lockedWallet
			var m Meta
			err := row.Scan(&v.ID, CountedAt(c, &v.ID, &v.PIN), &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v lockedWallet) []any { return []any{v.ID, HashCountedOf(c, v.ID, v.PIN)} },
		func(v lockedWallet) map[string]any { return map[string]any{"id": v.ID} })
	wallets, err := NewEntity(s)
	if err != nil {
		t.Fatal(err)
	}
	return wallets, &Resource{Type: resourceType, Token: `SELECT authz_token FROM ` + table + ` WHERE tenant_id = $1 AND id = $2 FOR SHARE`,
		ValidID: func(id string) bool { return id != "" }}
}

// TestAttemptResetChecksDecided proves the findings of the security review of PR #491 on the rows that a check of a
// counter with a reset needs (decision ST-SUP-Q209 (d)).
func TestAttemptResetChecksDecided(t *testing.T) {
	const a, b, right = "0198a1b2-0000-7000-8000-0000000000a3", "0198a1b2-0000-7000-8000-0000000000b3", "4711"
	errWrongPIN := errors.New("pin.wrong")
	check := func(pin string, w Loaded[lockedWallet]) error {
		if !w.Value().PIN.Matches(pin) {
			return errWrongPIN
		}
		return nil
	}
	t.Run("ST-SUP-RP10-LC54-T19 A command on the service-level resource records no row, also through an effect", func(t *testing.T) {
		e := newEnv(t)
		wallets, _ := resetWallets(t, e, "GuardedWallet", "guarded_wallets", "guarded_wallet", guardedCounter, right, a)
		rt := e.runtime(func(c *Config) { c.Entities = []Declared{wallets} })
		var charges atomic.Int32
		rt.charged = func(bool) { charges.Add(1) }
		// open-payment is decided on service:wallets; the schema also gives open_payment on the wallet a, so its
		// effect for a decides it there.
		open := e.command("open-payment", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource = "open_payment", &Resource{Type: "service", Service: "wallets"}
		}, wallets)
		e.decision.setRelations(relation(t, "user:u-9", "open_payment", "t1", "service", "wallets"),
			relation(t, "user:u-9", "open_payment", "t1", "guarded_wallet", a), relation(t, "user:u-9", "view", "t1", "guarded_wallet", a))
		req := e.request("t1", person(t, "user:u-9"), "open-1", `{}`)
		staged := false
		_, err := e.runOn(rt, open, req, func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			r, err := Repo(h, wallets)
			if err != nil {
				return idempotency.Response{}, err
			}
			w, err := r.Get(ctx, a)
			if err != nil {
				return idempotency.Response{}, err
			}
			if err := h.Effects().Stage(ctx, "billing", "charge-requested", w, []byte(`{"amount":100}`)); err != nil {
				return idempotency.Response{}, err
			}
			staged = true
			return ok, check(right, w)
		})
		if !staged || err == nil || errors.Is(err, errWrongPIN) || charges.Load() != 0 {
			t.Fatalf("a check of a after an effect of a command on the service-level resource gave %v (staged %v) and charged %d checks, want an internal error and none", err, staged, charges.Load())
		}
	})
	t.Run("ST-SUP-RP10-LC54-T20 A locked read that a second decision denies records no row", func(t *testing.T) {
		e := newEnv(t)
		wallets, wallet := resetWallets(t, e, "GuardedWallet", "guarded_wallets", "guarded_wallet", guardedCounter, right, a, b)
		rt := e.runtime(func(c *Config) { c.Entities = []Declared{wallets} })
		var charges atomic.Int32
		rt.charged = func(bool) { charges.Add(1) }
		withdraw := e.command("withdraw", func(s *CommandSpec[testProbe]) { s.Permission, s.Resource = "operate", wallet }, wallets)
		view := relation(t, "user:u-8", "view", "t1", "guarded_wallet", a)
		operateA := relation(t, "user:u-8", "operate", "t1", "guarded_wallet", a)
		operateB := relation(t, "user:u-8", "operate", "t1", "guarded_wallet", b)
		e.decision.setRelations(view, operateA, operateB, relation(t, "user:u-8", "view", "t1", "guarded_wallet", b))
		// Between the read and the lock of a, operate on a goes and the token of a moves.
		resourceA := "guarded_wallet:" + a
		var once sync.Once
		e.decision.setAfter(func(ev evaluation) {
			if ev.Resource != resourceA || ev.Action != "operate" {
				return
			}
			once.Do(func() {
				e.decision.setRelations(view, operateB)
				e.exec(`UPDATE guarded_wallets SET authz_token = 'moved' WHERE id = '` + a + `'`)
			})
		})
		defer e.decision.setAfter(nil)
		req := e.request("t1", person(t, "user:u-8"), "moved-1", `{}`)
		req.Resource = b
		var lockErr error
		_, err := e.runOn(rt, withdraw, req, func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			r, err := Repo(h, wallets)
			if err != nil {
				return idempotency.Response{}, err
			}
			w, err := r.Get(ctx, a)
			if err != nil {
				return idempotency.Response{}, err
			}
			// The fill keeps the value of the read with no lock after the lock was denied.
			_, lockErr = r.GetForUpdate(ctx, a)
			return ok, check(right, w)
		})
		if !errors.Is(lockErr, ErrNotFound) || err == nil || errors.Is(err, errWrongPIN) || charges.Load() != 0 {
			t.Fatalf("a check of a after its lock was denied gave %v (the lock %v) and charged %d checks, want ErrNotFound, an internal error and none", err, lockErr, charges.Load())
		}
	})
	t.Run("ST-SUP-RP10-LC54-T21 A counter that a command resets is held with no Reset in its declaration", func(t *testing.T) {
		e := newEnv(t)
		wallets, wallet := resetWallets(t, e, "LegacyWallet", "legacy_wallets", "legacy_wallet", legacyCounter, right, a, b)
		rt := e.runtime(func(c *Config) { c.Entities = []Declared{wallets} })
		var charges atomic.Int32
		rt.charged = func(bool) { charges.Add(1) }
		withdraw := e.command("withdraw", func(s *CommandSpec[testProbe]) { s.Permission, s.Resource = "operate", wallet }, wallets)
		e.command("unlock", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource, s.Resets, s.ResetGuards = "unlock", wallet, []string{"LegacyWallet.pin"}, []string{"operate"}
		}, wallets)
		e.decision.setRelations(relation(t, "user:u-5", "view", "t1", "legacy_wallet", a),
			relation(t, "user:u-5", "operate", "t1", "legacy_wallet", b), relation(t, "user:u-5", "view", "t1", "legacy_wallet", b))
		req := e.request("t1", person(t, "user:u-5"), "legacy-1", `{}`)
		req.Resource = b
		_, err := e.runOn(rt, withdraw, req, func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			r, err := Repo(h, wallets)
			if err != nil {
				return idempotency.Response{}, err
			}
			w, err := r.Get(ctx, a)
			if err != nil {
				return idempotency.Response{}, err
			}
			return ok, check(right, w)
		})
		if err == nil || errors.Is(err, errWrongPIN) || charges.Load() != 0 {
			t.Fatalf("a check of a from b with a counter of no Reset that unlock resets gave %v and charged %d checks, want an internal error and none", err, charges.Load())
		}
	})
}
