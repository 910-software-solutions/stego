package sealed

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testKey gives a new random key in base64; the tests use throwaway keys only.
func testKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func testRing(t *testing.T, lines ...string) *Ring {
	t.Helper()
	r, err := ParseRing([]byte(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSeal(t *testing.T) {
	t.Run("ST-SUP-RP16-LC01-T01 A sealed value has its header and opens only at its place and with its key", func(t *testing.T) {
		r := testRing(t, "7 "+testKey(t))
		at := Place{Tenant: "org-a", Table: "todos", Column: "api_token", Row: "0f0e0d0c-0000-4000-8000-000000000001"}
		plain := []byte("s3cr3t-value-0042")
		first, err := r.Seal(plain, at)
		if err != nil {
			t.Fatal(err)
		}
		second, err := r.Seal(plain, at)
		if err != nil {
			t.Fatal(err)
		}
		if first[0] != Version || !bytes.Equal(first[1:HeaderSize], []byte{0, 0, 0, 7}) ||
			len(first) != HeaderSize+NonceSize+len(plain)+TagSize {
			t.Fatalf("the sealed value has not the header version 1, key ID 7, nonce and tag: % x", first[:HeaderSize])
		}
		if bytes.Equal(first, second) || bytes.Equal(first[HeaderSize:HeaderSize+NonceSize], second[HeaderSize:HeaderSize+NonceSize]) {
			t.Fatal("two seals of one value gave the same nonce or the same bytes")
		}
		if bytes.Contains(first, plain) {
			t.Fatal("the sealed value holds the plain value")
		}
		got, err := r.Open(first, at)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("Open at the place gave %q, %v", got, err)
		}
		if id, ok := KeyID(first); !ok || id != 7 {
			t.Fatalf("KeyID gave %d, %t", id, ok)
		}
		moved := map[string]Place{
			"another row":    {Tenant: at.Tenant, Table: at.Table, Column: at.Column, Row: "0f0e0d0c-0000-4000-8000-000000000002"},
			"another column": {Tenant: at.Tenant, Table: at.Table, Column: "other_token", Row: at.Row},
			"another table":  {Tenant: at.Tenant, Table: "notes", Column: at.Column, Row: at.Row},
			"another tenant": {Tenant: "org-b", Table: at.Table, Column: at.Column, Row: at.Row},
			// The length prefix keeps apart two places whose parts join to the same text.
			"a shifted part": {Tenant: at.Tenant, Table: at.Table + "a", Column: "pi_token", Row: at.Row},
		}
		for name, place := range moved {
			if _, err := r.Open(first, place); !errors.Is(err, ErrOpen) {
				t.Errorf("Open at %s gave %v, want ErrOpen", name, err)
			}
		}
		changed := map[string][]byte{
			"another version": append([]byte{2}, first[1:]...),
			"too short":       first[:HeaderSize+NonceSize+TagSize-1],
			"a changed byte":  append(append([]byte{}, first[:len(first)-1]...), first[len(first)-1]^1),
			"an unknown key":  append([]byte{Version, 0, 0, 0, 8}, first[HeaderSize:]...),
		}
		for name, value := range changed {
			_, err := r.Open(value, at)
			if !errors.Is(err, ErrOpen) || strings.Contains(err.Error(), string(plain)) {
				t.Errorf("Open of %s gave %v, want ErrOpen with no value", name, err)
			}
		}
		other := testRing(t, "7 "+testKey(t))
		if _, err := other.Open(first, at); !errors.Is(err, ErrOpen) {
			t.Errorf("Open with another key of the same ID gave %v, want ErrOpen", err)
		}
	})
}

func TestRing(t *testing.T) {
	t.Run("ST-SUP-RP16-LC02-T01 The ring seals with its first key, opens with each key, and refuses a bad file without a key in the error", func(t *testing.T) {
		oldKey, newKey := testKey(t), testKey(t)
		old := testRing(t, "1 "+oldKey)
		at := Place{Table: "todos", Column: "api_token", Row: "r1"}
		sealedOld, err := old.Seal([]byte("v"), at)
		if err != nil {
			t.Fatal(err)
		}
		rotated := testRing(t, "# rotation", "", "2 "+newKey, "1 "+oldKey)
		if rotated.First() != 2 || fmt.Sprint(rotated.IDs()) != "[2 1]" {
			t.Fatalf("the ring has the first key %d and the IDs %v", rotated.First(), rotated.IDs())
		}
		if got, err := rotated.Open(sealedOld, at); err != nil || string(got) != "v" {
			t.Fatalf("the rotated ring does not open a value of the old key: %v", err)
		}
		sealedNew, err := rotated.Seal([]byte("v"), at)
		if err != nil {
			t.Fatal(err)
		}
		if id, _ := KeyID(sealedNew); id != 2 || !bytes.HasPrefix(sealedNew, Prefix(2)) {
			t.Fatalf("the rotated ring sealed with the key %d, want 2", id)
		}
		if _, err := old.Open(sealedNew, at); !errors.Is(err, ErrOpen) {
			t.Fatalf("the old ring opened a value of the new key: %v", err)
		}
		short := base64.StdEncoding.EncodeToString(make([]byte, 16))
		bad := map[string]string{
			"no key":              "# only a comment\n\n",
			"one field":           "1",
			"three fields":        "1 " + oldKey + " x",
			"a zero key ID":       "0 " + oldKey,
			"a key ID too large":  "4294967296 " + oldKey,
			"a key ID with a 0":   "01 " + oldKey,
			"a negative key ID":   "-1 " + oldKey,
			"a short key":         "1 " + short,
			"a key not in base64": "1 !!!!",
			"a key ID twice":      "1 " + oldKey + "\n1 " + newKey,
			"one key twice":       "1 " + oldKey + "\n2 " + oldKey,
		}
		for name, text := range bad {
			_, err := ParseRing([]byte(text))
			if !errors.Is(err, ErrRing) || strings.Contains(err.Error(), oldKey) || strings.Contains(err.Error(), newKey) {
				t.Errorf("ParseRing of %s gave %v, want ErrRing with no key", name, err)
			}
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "key-ring")
		if err := os.WriteFile(path, []byte("2 "+newKey+"\n1 "+oldKey+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if r, err := ReadRing(path); err != nil || r.First() != 2 {
			t.Fatalf("ReadRing gave %v", err)
		}
		if _, err := ReadRing(filepath.Join(dir, "missing")); !errors.Is(err, ErrRing) || strings.Contains(err.Error(), dir) {
			t.Fatalf("ReadRing of a missing file gave %v, want ErrRing with no path", err)
		}
	})
}

func TestCompare(t *testing.T) {
	t.Run("ST-SUP-RP16-LC03-T01 A compare-only secret is an Argon2id hash that matches only its value, and a hostile hash matches nothing", func(t *testing.T) {
		hash, err := Hash("4711")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") || strings.Contains(hash, "4711") {
			t.Fatalf("the hash is not the PHC string of the parameters: %s", hash)
		}
		again, _ := Hash("4711")
		if again == hash {
			t.Fatal("two hashes of one value have the same salt")
		}
		if !Matches(hash, "4711") || Matches(hash, "4712") || Matches(hash, "") {
			t.Fatal("Matches does not tell the right value from a wrong one")
		}
		parts := strings.Split(hash, "$")
		hostile := map[string]string{
			"argon2i":         strings.Replace(hash, "argon2id", "argon2i", 1),
			"version 16":      strings.Replace(hash, "v=19", "v=16", 1),
			"a large memory":  strings.Replace(hash, "m=19456", "m=4194304", 1),
			"a large time":    strings.Replace(hash, "t=2", "t=1000", 1),
			"many threads":    strings.Replace(hash, "p=1", "p=255", 1),
			"a zero time":     strings.Replace(hash, "t=2", "t=0", 1),
			"no salt":         strings.Join([]string{"", parts[1], parts[2], parts[3], "", parts[5]}, "$"),
			"a short hash":    strings.Join([]string{"", parts[1], parts[2], parts[3], parts[4], "AAAA"}, "$"),
			"an extra part":   hash + "$x",
			"the plain value": "4711",
			"empty":           "",
		}
		for name, stored := range hostile {
			if Matches(stored, "4711") {
				t.Errorf("Matches accepted %s", name)
			}
		}
		c := ComparedHash(hash)
		if !c.IsSet() || !c.Matches("4711") || c.Matches("4712") || c.Matches("") {
			t.Fatal("a stored Compared does not check right and wrong values")
		}
		if stored, err := c.Stored(); err != nil || stored != hash {
			t.Fatal("a stored Compared does not keep its hash")
		}
		n := ComparedOf("4711")
		if !n.Matches("4711") || n.Matches("471") {
			t.Fatal("a new Compared does not check right and wrong values")
		}
		if stored, err := n.Stored(); err != nil || !Matches(stored, "4711") {
			t.Fatal("a new Compared does not store the hash of its text")
		}
		if ComparedOf("").IsSet() || ComparedOf("").Matches("") {
			t.Fatal("an empty Compared is set or matches")
		}
		// With each hash slot taken, a wait ends with its context and hashes nothing.
		for range cap(hashSlots) {
			hashSlots <- struct{}{}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = HashContext(ctx, "4711")
		matched := MatchesContext(ctx, hash, "4711")
		for range cap(hashSlots) {
			<-hashSlots
		}
		if !errors.Is(err, context.Canceled) || matched {
			t.Fatalf("a hash with every slot taken and an ended context gave %v and a match %t", err, matched)
		}
	})
}

func TestValues(t *testing.T) {
	t.Run("ST-SUP-RP16-LC04-T01 A Secret and a Compared never print, log or encode their text", func(t *testing.T) {
		const text = "s3cr3t-value-0042"
		s, c := SecretOf(text), ComparedOf(text)
		if s.Reveal() != text || !s.IsSet() || SecretOf("").IsSet() || !s.Equal(text) || s.Equal(text+"x") || s.Equal("") || SecretOf("").Equal("") {
			t.Fatal("Reveal does not give the text of a Secret")
		}
		type row struct {
			Token Secret
			PIN   Compared
			Ptr   *Secret
		}
		value := row{Token: s, PIN: c, Ptr: &s}
		// fmt and slog reach a value through an unexported field without its methods (security review of PR #441,
		// M1): the wrapper holds the row, a stored hash and the secrets in unexported fields.
		type wrapper struct {
			inner  row
			hashed Compared
			list   []Secret
		}
		hashed, err := Hash(text)
		if err != nil {
			t.Fatal(err)
		}
		wrapped := wrapper{inner: value, hashed: ComparedHash(hashed), list: []Secret{s}}
		var out []string
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T"} {
			out = append(out, fmt.Sprintf(verb, value), fmt.Sprintf(verb, s), fmt.Sprintf(verb, c), fmt.Sprintf(verb, wrapped),
				fmt.Sprintf(verb, &wrapped), fmt.Errorf("save "+verb, wrapped).Error())
		}
		out = append(out, s.String(), c.String(), fmt.Sprint(s, c), fmt.Sprintln(&s, &c))
		encoded, err := json.Marshal(map[string]any{"row": value, "secret": s, "compared": c, "key": map[Secret]int{s: 1}})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(encoded))
		var logged bytes.Buffer
		for _, h := range []slog.Handler{slog.NewJSONHandler(&logged, nil), slog.NewTextHandler(&logged, nil)} {
			slog.New(h).Info("write", "token", s, "pin", c, "row", value, "wrapped", wrapped)
		}
		out = append(out, logged.String())
		hex := fmt.Sprintf("%x", text)
		for _, o := range out {
			if strings.Contains(o, text) || strings.Contains(strings.ToLower(o), hex) || strings.Contains(o, "$argon2id$") {
				t.Fatalf("an output holds the secret: %s", o)
			}
		}
		if !strings.Contains(string(encoded), `"secret":null`) || !strings.Contains(logged.String(), Redacted) {
			t.Fatalf("the JSON is not null or the log is not [secret]: %s %s", encoded, logged.String())
		}
		var back row
		if err := json.Unmarshal([]byte(`{"Token":"x","PIN":"y"}`), &back); !errors.Is(err, ErrDecode) {
			t.Fatalf("a JSON decode of a secret gave %v, want ErrDecode", err)
		}
	})
}

func TestFingerprint(t *testing.T) {
	t.Run("ST-SUP-RP16-LC05-T01 A keyed fingerprint matches with the key of its time and tells nothing of the body", func(t *testing.T) {
		oldKey, newKey := testKey(t), testKey(t)
		old := testRing(t, "1 "+oldKey)
		raw := "4b227777d4dd1fc61c6f884f48641d02b4d121d3fd328cb08b5531fcacdabf8a"
		keyed := old.Fingerprint(raw)
		if !strings.HasPrefix(keyed, "k1:") || strings.Contains(keyed, raw) || len(keyed) != len("k1:")+64 {
			t.Fatalf("the keyed fingerprint is %q, want k1: and an HMAC", keyed)
		}
		if old.Fingerprint(raw) != keyed || !old.FingerprintMatches(keyed, raw) || old.FingerprintMatches(keyed, raw+"0") {
			t.Fatal("the keyed fingerprint is not stable, or matches another fingerprint")
		}
		rotated := testRing(t, "2 "+newKey, "1 "+oldKey)
		if !rotated.FingerprintMatches(keyed, raw) || !strings.HasPrefix(rotated.Fingerprint(raw), "k2:") {
			t.Fatal("the rotated ring does not match a fingerprint of the old key, or does not key with its first key")
		}
		if testRing(t, "2 "+newKey).FingerprintMatches(keyed, raw) || testRing(t, "1 "+newKey).FingerprintMatches(keyed, raw) {
			t.Fatal("a ring without the old key matches its fingerprint")
		}
		for _, bad := range []string{raw, "", "k1", "kx:" + keyed[3:], "k9:" + keyed[3:], "1:" + keyed[3:]} {
			if rotated.FingerprintMatches(bad, raw) {
				t.Errorf("the stored fingerprint %q matches", bad)
			}
		}
	})
}
