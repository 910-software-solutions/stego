package idempotency

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/registry/runtime/sealed"
)

// ringSealer is the Sealer of the command runtime over a throwaway key ring.
type ringSealer struct{ ring *sealed.Ring }

func (r ringSealer) SealAnswer(tenant, column, row string, plain []byte) ([]byte, error) {
	return r.ring.Seal(plain, sealed.Place{Tenant: tenant, Table: AnswerTable, Column: column, Row: row})
}

func (r ringSealer) OpenAnswer(tenant, column, row string, data []byte) ([]byte, error) {
	return r.ring.Open(data, sealed.Place{Tenant: tenant, Table: AnswerTable, Column: column, Row: row})
}

// throwawayRing gives a key ring of one new random key of the ID id; the key lives only in the memory of the test.
func throwawayRing(t *testing.T, id string) *sealed.Ring {
	t.Helper()
	key := make([]byte, sealed.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	ring, err := sealed.ParseRing([]byte(id + " " + base64.StdEncoding.EncodeToString(key) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func TestSealedAnswers(t *testing.T) {
	ctx := context.Background()

	t.Run("ST-SUP-RP07-LC13-T01 A sealed answer replays byte for byte and holds no clear text", func(t *testing.T) {
		db, admin := testDB(t)
		sealer := ringSealer{ring: throwawayRing(t, "1")}
		withSealer := func(key string) Request {
			req := request(key, "f1")
			req.Sealer = sealer
			return req
		}
		runs := 0
		const secret = "member@example.com"
		answer := func(ctx context.Context, tx Tx) (Response, error) {
			runs++
			if err := writeOrder(ctx, tx, "o1"); err != nil {
				return Response{}, err
			}
			h := http.Header{}
			h.Set("Location", "/v1/orders/o1")
			h["X-Nul"] = []string{"a\x00b", ""}
			return Response{Status: 201, Header: h, Body: []byte(`{"email": "` + secret + `" }` + "\n")}, nil
		}
		first, err := runWrap(ctx, db, withSealer("k1"), answer)
		if err != nil || !first.Commit {
			t.Fatalf("Wrap = %+v, %v", first, err)
		}
		again, err := runWrap(ctx, db, withSealer("k1"), answer)
		if err != nil {
			t.Fatal(err)
		}
		if runs != 1 || !again.Replayed || again.Response.Status != 201 || !bytes.Equal(again.Response.Body, first.Response.Body) ||
			!sameHeader(again.Response.Header, first.Response.Header) {
			t.Fatalf("replay = %+v after %d runs, want the first answer once", again, runs)
		}
		if got := again.Response.Header["X-Nul"]; len(got) != 2 || got[0] != "a\x00b" {
			t.Fatalf("X-Nul = %q, want the NUL value byte for byte", got)
		}
		schema := currentSchema(db)
		var fingerprint string
		var header, body []byte
		if err := admin.QueryRowContext(ctx, "SELECT fingerprint, response_headers, response_body FROM "+schema+
			".idempotency_keys WHERE key = 'k1'").Scan(&fingerprint, &header, &body); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(fingerprint, SealedPrefix) {
			t.Fatalf("the stored fingerprint %q has no prefix %s", fingerprint, SealedPrefix)
		}
		for _, stored := range [][]byte{header, body} {
			if bytes.Contains(stored, []byte(secret)) || bytes.Contains(stored, []byte("/v1/orders")) {
				t.Fatal("the key row holds the answer in clear")
			}
			if id, ok := sealed.KeyID(stored); !ok || id != 1 {
				t.Fatalf("a stored part is not sealed with the key 1 (%d, %v)", id, ok)
			}
		}

		// A request with no sealer, as of a release before, does not replay the sealed bytes.
		plain, err := runWrap(ctx, db, request("k1", "f1"), answer)
		if err != nil || plain.Problem != ProblemReused || runs != 1 {
			t.Fatalf("a request with no sealer = %+v, %v; want %s and no run", plain, err, ProblemReused)
		}
		// A clear row of a release before does not match a request with a sealer.
		if _, err := runWrap(ctx, db, request("k2", "f1"), func(ctx context.Context, tx Tx) (Response, error) {
			return Response{Status: 200, Body: []byte("{}")}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if res, err := runWrap(ctx, db, withSealer("k2"), answer); err != nil || res.Problem != ProblemReused {
			t.Fatalf("a request with a sealer on a clear row = %+v, %v; want %s", res, err, ProblemReused)
		}

		// The sealed body of k1 copied to the column of the headers, or into the row of k3, does not open.
		if _, err := runWrap(ctx, db, withSealer("k3"), func(ctx context.Context, tx Tx) (Response, error) {
			return Response{Status: 200, Body: []byte("{}")}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, "UPDATE "+schema+".idempotency_keys SET response_body = $1 WHERE key = 'k3'", body); err != nil {
			t.Fatal(err)
		}
		// A sealed answer that does not open is answered as a reused key, so the client sends again with a new key,
		// and the handler does not run (security review of PR #443, L3).
		res, err := runWrap(ctx, db, withSealer("k3"), answer)
		if err != nil || res.Problem != ProblemReused || res.Replayed || runs != 1 {
			t.Fatalf("a moved sealed body = %+v, %v; want %s and no run", res, err, ProblemReused)
		}
		// A ring with no key 1 does not open the answer of k1.
		other := request("k1", "f1")
		other.Sealer = ringSealer{ring: throwawayRing(t, "2")}
		if res, err := runWrap(ctx, db, other, answer); err != nil || res.Problem != ProblemReused || runs != 1 {
			t.Fatalf("a ring with no key 1 = %+v, %v; want %s", res, err, ProblemReused)
		}
	})
}
