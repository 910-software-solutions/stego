package check

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// throwawayKey gives a new random key of the key ring form. The check uses throwaway keys only, in files of a folder
// of the test that the test removes.
func throwawayKey(t *testing.T) string {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	must(t, err)
	return base64.StdEncoding.EncodeToString(key)
}

// writeRing writes a key ring file of the lines in the folder of the key rings and gives its path.
func (s *service) writeRing(t *testing.T, name string, lines ...string) string {
	file := filepath.Join(s.ringDir, name)
	must(t, os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return file
}

// verified sends the command verify-order for the order with the token and the PIN, and gives whether each is the
// secret of the order, and the status.
func (s *service) verified(t *testing.T, c caller, id, token, pin string) (tokenOK, pinOK bool, status int) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"token": token, "pin": pin})
	must(t, err)
	got := s.send(&c, http.MethodPost, "/orders/"+id+"/verify", "k-"+uuid.NewString(), string(body))
	// A command answers its response with 201 (ST-SUP-RC32-OC02).
	if got.status != http.StatusCreated {
		return false, false, got.status
	}
	var document map[string]any
	if err := json.Unmarshal(got.body, &document); err != nil {
		t.Fatalf("the verify answer %s is not a JSON object: %v", got.body, err)
	}
	flag := func(names ...string) bool {
		for _, name := range names {
			if v, ok := document[name].(bool); ok {
				return v
			}
		}
		return false
	}
	return flag("token_ok", "tokenOk"), flag("pin_ok", "pinOk"), got.status
}

// textOfAllRows gives the text of each row of each table of the database outside the catalogs, as the database owner
// reads it, by table.
func (s *service) textOfAllRows(t *testing.T) map[string][]string {
	t.Helper()
	rows, err := s.admin.Query(`SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname) FROM pg_class c
 JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind IN ('r', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema')
 AND n.nspname NOT LIKE 'pg_toast%'`)
	must(t, err)
	var tables []string
	for rows.Next() {
		var name string
		must(t, rows.Scan(&name))
		tables = append(tables, name)
	}
	must(t, rows.Close())
	out := map[string][]string{}
	for _, table := range tables {
		r, err := s.admin.Query(`SELECT t::text FROM ` + table + ` t`)
		must(t, err)
		for r.Next() {
			var text string
			must(t, r.Scan(&text))
			out[table] = append(out[table], text)
		}
		must(t, r.Close())
	}
	return out
}

// secretChecks are the checks of the secret fields of Order in the generated service (decision ST-DECL-Q44): the
// readable secret api_token and the compare-only secret pin.
func secretChecks(t *testing.T, s *service, owner caller) {
	const token, pin, nextToken = "s3cr3t-value-0042", "pin-73915-x", "n3w-s3cr3t-0043"
	secrets := []string{token, pin, nextToken, "0th3r-s3cr3t-0044"}
	var vault, other string
	var otherSealed []byte

	t.Run("ST-SUP-RC20-OC48-T01 A secret field is written by a request, never answered, and kept by a write that leaves it out", func(t *testing.T) {
		s.t = t
		// The tenant of the owner is provisioned by the collection check before.
		created := s.send(&owner, http.MethodPost, "/orders", "k-"+uuid.NewString(), `{"sku":"v1","qty":1,"api_token":"`+token+`","pin":"`+pin+`"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		vault = created.member(t, "id")
		s.own(owner, vault)
		s.canEdit(t, owner, vault)
		answers := []reply{created, s.send(&owner, http.MethodGet, "/orders/"+vault, "", ""), s.send(&owner, http.MethodGet, "/orders", "", "")}
		patched := s.send(&owner, http.MethodPatch, "/orders/"+vault, "k-"+uuid.NewString(), `{"qty":5}`)
		if patched.status != http.StatusOK {
			t.Fatalf("the patch of qty answered %d %s, want 200", patched.status, patched.body)
		}
		answers = append(answers, patched)
		for _, a := range answers {
			for _, secret := range secrets {
				if bytes.Contains(a.body, []byte(secret)) {
					t.Fatalf("an answer holds a secret: %s", a.body)
				}
			}
		}
		if tokenOK, pinOK, status := s.verified(t, owner, vault, token, pin); !tokenOK || !pinOK {
			t.Fatalf("after a patch that leaves the secrets out, verify gave token %t, pin %t, status %d, want both kept", tokenOK, pinOK, status)
		}
		if got := s.send(&owner, http.MethodPatch, "/orders/"+vault, "k-"+uuid.NewString(), `{"api_token":"`+nextToken+`"}`); got.status != http.StatusOK || bytes.Contains(got.body, []byte(nextToken)) {
			t.Fatalf("the patch of api_token answered %d %s, want 200 with no secret", got.status, got.body)
		}
		if tokenOK, pinOK, _ := s.verified(t, owner, vault, nextToken, pin); !tokenOK || !pinOK {
			t.Fatal("after a patch of api_token, the new token does not verify or the PIN changed")
		}
		if tokenOK, _, _ := s.verified(t, owner, vault, token, pin); tokenOK {
			t.Fatal("after a patch of api_token, the old token still verifies")
		}
		if got := s.send(&owner, http.MethodPatch, "/orders/"+vault, "k-"+uuid.NewString(), `{"api_token":"short"}`); got.status != http.StatusBadRequest || bytes.Contains(got.body, []byte("short\"")) {
			t.Errorf("a token shorter than min_length answered %d %s, want 400 with no value", got.status, got.body)
		}
		if got := s.send(&owner, http.MethodPost, "/orders", "k-"+uuid.NewString(), `{"sku":"v2","qty":1,"pin":"12"}`); got.status != http.StatusBadRequest {
			t.Errorf("a PIN shorter than min_length answered %d %s, want 400", got.status, got.body)
		}
	})

	t.Run("ST-SUP-RC32-OC20-T01 A command fill reads a readable secret and checks a compare-only one", func(t *testing.T) {
		s.t = t
		if vault == "" {
			t.Fatal("no order with secrets: the check before did not pass")
		}
		for _, c := range []struct {
			token, pin         string
			wantToken, wantPIN bool
		}{{nextToken, pin, true, true}, {nextToken + "x", pin, false, true}, {nextToken, pin + "x", true, false}, {"", "", false, false}} {
			tokenOK, pinOK, status := s.verified(t, owner, vault, c.token, c.pin)
			if status != http.StatusCreated || tokenOK != c.wantToken || pinOK != c.wantPIN {
				t.Errorf("verify of a token %t and a PIN %t gave %t, %t, status %d", c.wantToken, c.wantPIN, tokenOK, pinOK, status)
			}
		}
		stranger := s.as("u-"+uuid.NewString()[:8], owner.tenant.ID())
		if _, _, status := s.verified(t, stranger, vault, nextToken, pin); status != http.StatusNotFound {
			t.Errorf("verify by a caller who may not view the order answered %d, want 404", status)
		}
	})

	t.Run("ST-SUP-RP10-LC44-T03 A sealed value copied to another order does not open in the generated service", func(t *testing.T) {
		s.t = t
		if vault == "" {
			t.Fatal("no order with secrets: the check before did not pass")
		}
		created := s.send(&owner, http.MethodPost, "/orders", "k-"+uuid.NewString(), `{"sku":"v3","qty":1,"api_token":"0th3r-s3cr3t-0044"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		other = created.member(t, "id")
		s.own(owner, other)
		s.canEdit(t, owner, other)
		must(t, s.admin.QueryRow(`SELECT api_token FROM orders WHERE id = $1`, other).Scan(&otherSealed))
		execAll(t, s.admin, fmt.Sprintf(`UPDATE orders SET api_token = (SELECT api_token FROM orders WHERE id = '%s') WHERE id = '%s'`, vault, other))
		if _, _, status := s.verified(t, owner, other, nextToken, ""); status != http.StatusInternalServerError {
			t.Errorf("verify of an order with the sealed token of another order answered %d, want 500", status)
		}
		for _, secret := range secrets {
			if strings.Contains(s.log.String(), secret) {
				t.Fatal("the service log holds a secret after a value that does not open")
			}
		}
		_, err := s.admin.Exec(`UPDATE orders SET api_token = $1 WHERE id = $2`, otherSealed, other)
		must(t, err)
		if tokenOK, _, _ := s.verified(t, owner, other, "0th3r-s3cr3t-0044", ""); !tokenOK {
			t.Fatal("the order does not verify its own token again")
		}
	})

	t.Run("ST-DECL-UC05-R06-T02 A secret field is never stored", func(t *testing.T) {
		s.t = t
		if vault == "" {
			t.Fatal("no order with secrets: the check before did not pass")
		}
		var sealedToken []byte
		var hash string
		must(t, s.admin.QueryRow(`SELECT api_token, pin FROM orders WHERE id = $1`, vault).Scan(&sealedToken, &hash))
		if !bytes.HasPrefix(sealedToken, []byte{1, 0, 0, 0, 1}) || !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
			t.Fatalf("the column api_token does not hold a value sealed with the key 1, or pin no Argon2id hash")
		}
		for table, rows := range s.textOfAllRows(t) {
			for _, row := range rows {
				for _, secret := range secrets {
					if strings.Contains(row, secret) || strings.Contains(row, fmt.Sprintf("%x", secret)) {
						t.Errorf("a row of %s holds a secret", table)
					}
				}
			}
		}
		for _, r := range s.broker.Records(t, changeTopic, 0, 10*time.Second, 3*time.Second) {
			for _, secret := range secrets {
				if bytes.Contains(r.Value, []byte(secret)) || bytes.Contains(r.Key, []byte(secret)) {
					t.Errorf("a change event holds a secret: %s", r.Value)
				}
			}
		}
		for _, secret := range secrets {
			if strings.Contains(s.log.String(), secret) {
				t.Error("the log of the service holds a secret")
			}
		}
	})

	t.Run("ST-SUP-RP10-LC46-T02 After a key rotation the generated controller moves each sealed value to the new key", func(t *testing.T) {
		s.t = t
		if vault == "" || other == "" {
			t.Fatal("no orders with secrets: the checks before did not pass")
		}
		secondKey := throwawayKey(t)
		s.ring = s.writeRing(t, "ring-2", "2 "+secondKey, "1 "+s.firstKey)
		s.restart(t)
		if tokenOK, pinOK, status := s.verified(t, owner, vault, nextToken, pin); !tokenOK || !pinOK {
			t.Fatalf("after the rotation, an old value does not verify: %t, %t, status %d", tokenOK, pinOK, status)
		}
		s.eventually(t, "the re-seal job moves each sealed value to the key 2", func() bool {
			return count(t, s.admin, `SELECT count(*) FROM orders WHERE api_token IS NOT NULL AND substring(api_token from 1 for 5) <> $1`, []byte{1, 0, 0, 0, 2}) == 0
		})
		if !strings.Contains(s.log.String(), "re-seal of a secret column") {
			t.Error("the log has no line of the re-seal job")
		}
		s.ring = s.writeRing(t, "ring-3", "2 "+secondKey)
		s.restart(t)
		for id, want := range map[string]string{vault: nextToken, other: "0th3r-s3cr3t-0044"} {
			if tokenOK, _, status := s.verified(t, owner, id, want, ""); !tokenOK {
				t.Errorf("with the key 2 alone, the order %s does not verify its token: status %d", id, status)
			}
		}
	})

	t.Run(jsonLinesCheck, func(t *testing.T) {
		s.t = t
		if vault == "" {
			t.Fatal("no order with a PIN: the checks before did not pass")
		}
		// A wrong PIN leaves a count row of the order. A transaction of the check holds the lock of each count row,
		// so each charge of the next checks waits for 1 s, ends at its lock timeout, and the call answers 503
		// (walkthrough 21, U-E). The charges of one tenant hold at most half of the pool of the charges, so the pool
		// is never full here; the line of a full pool goes through the same log provider as the line of a failed
		// charge.
		if _, pinOK, status := s.verified(t, owner, vault, nextToken, pin+"-wrong"); pinOK || status != http.StatusCreated {
			t.Fatalf("a wrong PIN gave %t, status %d", pinOK, status)
		}
		tx, err := s.admin.Begin()
		must(t, err)
		var held int
		must(t, tx.QueryRow(`SELECT count(*) FROM (SELECT 1 FROM public.attempt_counts FOR UPDATE) AS locked`).Scan(&held))
		if held == 0 {
			_ = tx.Rollback()
			t.Fatal("the wrong PIN left no count row")
		}
		statuses := make(chan int, 4)
		for range 4 {
			go func() {
				_, _, status := s.verified(t, owner, vault, nextToken, pin)
				statuses <- status
			}()
		}
		for range 4 {
			if status := <-statuses; status != http.StatusServiceUnavailable {
				t.Errorf("a check of the PIN whose count row is locked answered %d, want 503", status)
			}
		}
		must(t, tx.Rollback())
		if _, pinOK, status := s.verified(t, owner, vault, nextToken, pin); !pinOK || status != http.StatusCreated {
			t.Fatalf("after the lock, the right PIN gave %t, status %d", pinOK, status)
		}
		// Each line of these events is one JSON line, with its event name and its stego.* attributes.
		type line struct {
			Event      string         `json:"event.name"`
			Message    string         `json:"message"`
			Attributes map[string]any `json:"attributes"`
		}
		lines := func(what, output, mark string) []line {
			t.Helper()
			var found []line
			for _, text := range strings.Split(output, "\n") {
				// The failure record of a refused job names the line of the refused step in its remedy.
				if !strings.Contains(text, mark) || strings.Contains(text, `"event.name":"service.failed"`) {
					continue
				}
				var l line
				if err := json.Unmarshal([]byte(text), &l); err != nil {
					t.Errorf("%s: the line is not JSON: %s", what, text)
					continue
				}
				found = append(found, l)
			}
			if len(found) == 0 {
				t.Errorf("%s: no line holds %q", what, mark)
			}
			return found
		}
		log := s.log.String()
		for _, l := range lines("the failed charge", log, "command.attempts.failed") {
			if l.Event != "command.attempts.failed" || l.Attributes["stego.action"] != "verify-order" || l.Attributes["stego.entity"] != "Order" ||
				(l.Attributes["stego.cause"] != "lock_timeout" && l.Attributes["stego.cause"] != "deadline") {
				t.Errorf("the line of a failed charge is %+v, want the action verify-order, the entity Order and the cause lock_timeout or deadline", l)
			}
		}
		for _, l := range lines("the re-seal job of the controller", log, "re-seal of a secret column") {
			if l.Event != "command.reseal.pass" || l.Attributes["stego.entity"] != "Order" || l.Attributes["stego.column"] != "api_token" {
				t.Errorf("the line of the re-seal job is %+v, want the event command.reseal.pass, the entity Order and the column api_token", l)
			}
		}
		for _, l := range lines("the refused migration job", s.foreignMigration.output+s.legacyMigration.output, "migrate: the step is refused") {
			if l.Event != "migrate.step.refused" || l.Attributes["stego.step"] != "ledger_check" || l.Attributes["stego.refusal"] == "" {
				t.Errorf("the line of a refused step is %+v, want the event migrate.step.refused, the step ledger_check and the refusal", l)
			}
		}
		if strings.Contains(log, pin) || strings.Contains(log, nextToken) {
			t.Error("the log of the service holds a secret value")
		}
	})
}

// jsonLinesCheck is the check that the lines of the counted checks, of the re-seal job and of a refused step of the
// migration job are JSON lines (walkthrough 21, U-E).
const jsonLinesCheck = "ST-SUP-RP03-LC16-T03 The generated service writes the lines of a failed charge, the re-seal job and a refused step of the migration job as JSON lines with stego attributes"
