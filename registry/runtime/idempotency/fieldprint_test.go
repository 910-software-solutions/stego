package idempotency

import "testing"

func TestFieldFingerprint(t *testing.T) {
	t.Run("ST-SUP-RP07-LC10-T01 A field fingerprint reads only the declared fields", func(t *testing.T) {
		fields := []string{"sku", "qty"}
		print := func(target, body string) string {
			t.Helper()
			f, err := FieldFingerprint("POST", target, []byte(body), fields)
			if err != nil {
				t.Fatal(err)
			}
			return f
		}
		a := print("/orders", `{"sku":"A-1","qty":2}`)
		if b := print("/orders", `{"qty":2.0, "note":"extra", "sku":"A-1"}`); a != b {
			t.Fatal("an extra field changed the fingerprint")
		}
		if b := print("/orders?trace=1", `{"sku":"A-1","qty":2}`); a != b {
			t.Fatal("the query changed the fingerprint")
		}
		if b := print("/orders", `{"sku":"A-1","qty":3}`); a == b {
			t.Fatal("two bodies that differ in qty have one fingerprint")
		}
		if b := print("/other", `{"sku":"A-1","qty":2}`); a == b {
			t.Fatal("two paths have one fingerprint")
		}
		whole, err := Fingerprint("POST", "/orders", []byte(`{"qty":2,"sku":"A-1"}`))
		if err != nil || whole != a {
			t.Fatalf("the field fingerprint %s is not Fingerprint over the kept body %s: %v", a, whole, err)
		}
		if _, err := FieldFingerprint("POST", "/orders", []byte(`[1]`), fields); err == nil {
			t.Fatal("a body that is not an object was taken")
		}
	})
}

func TestFieldFingerprintFolding(t *testing.T) {
	t.Run("ST-SUP-RP07-LC10-T02 A member that the decoder matches to a declared field under case folding is in the fingerprint", func(t *testing.T) {
		fields := []string{"title", "due_at", "sku"}
		print := func(body string) string {
			t.Helper()
			f, err := FieldFingerprint("POST", "/tasks", []byte(body), fields)
			if err != nil {
				t.Fatal(err)
			}
			return f
		}
		a := print(`{"title":"plan","due_at":"2026-12-01T10:00:00Z"}`)
		for _, body := range []string{
			`{"title":"plan","DUE_AT":"2026-12-02T10:00:00Z"}`,
			`{"title":"plan","Due_At":"2026-12-02T10:00:00Z"}`,
			`{"title":"plan"}`,
		} {
			if b := print(body); a == b {
				t.Errorf("the body %s has the fingerprint of due_at 2026-12-01T10:00:00Z", body)
			}
		}
		// The decoder reads DUE_AT as due_at, so the same value under another case is the same request.
		if b := print(`{"TITLE":"plan","DUE_AT":"2026-12-01T10:00:00Z"}`); a != b {
			t.Error("the same values under member names in another case have another fingerprint")
		}
		// Simple folding as bytes.EqualFold: U+017F folds to s and U+212A to k.
		s := print(`{"sku":"A-1"}`)
		if b := print(`{"ſku":"A-2"}`); s == b {
			t.Error("a member ſku with another value has the fingerprint of sku A-1")
		}
		if b := print(`{"ſKu":"A-1"}`); s != b {
			t.Error("a member ſKu with the value of sku has another fingerprint")
		}
	})
}

func TestFingerprintRepeatedKeys(t *testing.T) {
	t.Run("ST-SUP-RP07-LC05-T04 A body whose keys repeat or are equal under case folding is refused", func(t *testing.T) {
		for name, body := range map[string]string{
			"a repeated key":                  `{"qty":1,"qty":9}`,
			"two keys equal under case":       `{"qty":1,"QTY":9}`,
			"a repeated key in a deep value":  `{"lines":[{"sku":"A","sku":"B"}]}`,
			"keys equal under simple folding": `{"K":1,"k":2}`,
		} {
			if _, err := Fingerprint("POST", "/orders", []byte(body)); err == nil {
				t.Errorf("Fingerprint took a body with %s", name)
			}
			if _, err := FieldFingerprint("POST", "/orders", []byte(body), []string{"qty", "lines", "k"}); err == nil {
				t.Errorf("FieldFingerprint took a body with %s", name)
			}
		}
		// A repeated key in a field that is not declared is refused too: the body is read whole.
		if _, err := FieldFingerprint("POST", "/orders", []byte(`{"qty":1,"note":"a","Note":"b"}`), []string{"qty"}); err == nil {
			t.Error("FieldFingerprint took repeated keys outside the declared fields")
		}
		if _, err := Fingerprint("POST", "/orders", []byte(`{"qty":1,"sku":"A"}`)); err != nil {
			t.Fatalf("a body with distinct keys gave %v", err)
		}
	})
}
