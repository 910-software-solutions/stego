package sealed

import (
	"errors"
	"fmt"
	"log/slog"
)

// Redacted is the text that a Secret and a Compared give for each fmt verb, for String and for slog.
const Redacted = "[secret]"

// ErrDecode is the error of a Secret or a Compared that a JSON decoder fills: a secret field is never decoded from
// JSON into its stored form, only given through SecretOf or ComparedOf.
var ErrDecode = errors.New("sealed: a secret value is not decoded from JSON")

// Secret is the value of a readable secret field: its text, or not set. Only Reveal gives its text; it gives [secret]
// for each fmt verb, for String and for slog, and encodes as JSON null (ST-SUP-RP16-LC04).
type Secret struct {
	text string
	set  bool
}

// SecretOf gives the Secret of text; an empty text gives a Secret that is not set.
func SecretOf(text string) Secret { return Secret{text: text, set: text != ""} }

// Reveal gives the text of the secret, or empty when it is not set.
func (s Secret) Reveal() string { return s.text }

// IsSet tells whether the secret has a text.
func (s Secret) IsSet() bool { return s.set }

// Equal tells, in constant time for texts of one length, whether text is the secret. A secret that is not set is
// equal to no text, so a fill can check a given value with no import of its own.
func (s Secret) Equal(text string) bool { return s.set && text != "" && constantEqual(s.text, text) }

// String gives [secret].
func (s Secret) String() string { return Redacted }

// GoString gives [secret].
func (s Secret) GoString() string { return Redacted }

// Format writes [secret] for each verb.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// LogValue gives [secret] to slog.
func (s Secret) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON encodes the secret as null.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// MarshalText encodes the secret as [secret], for an encoder that takes text, such as a map key.
func (s Secret) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// UnmarshalJSON refuses each value.
func (s *Secret) UnmarshalJSON([]byte) error { return ErrDecode }

// Compared is the value of a compare-only secret field: a new text that a write hashes, or the stored hash that a read
// gave. It never gives a text: Matches checks a given value (ST-SUP-RP16-LC03, ST-SUP-RP16-LC04).
type Compared struct {
	// text is a new value that the next write hashes, and hash the stored Argon2id string; at most one is set.
	text string
	hash string
}

// ComparedOf gives the Compared of a new text, which the next write stores as its hash; an empty text gives a
// Compared that is not set.
func ComparedOf(text string) Compared { return Compared{text: text} }

// ComparedHash gives the Compared of a stored hash, as a read of its column gives it.
func ComparedHash(hash string) Compared { return Compared{hash: hash} }

// IsSet tells whether the value has a new text or a stored hash.
func (c Compared) IsSet() bool { return c.text != "" || c.hash != "" }

// Matches tells whether value is the secret: against the stored hash in constant time, or against a new text that is
// not stored yet, also in constant time. An empty value and a Compared that is not set match nothing.
func (c Compared) Matches(value string) bool {
	switch {
	case value == "":
		return false
	case c.hash != "":
		return Matches(c.hash, value)
	case c.text != "":
		return constantEqual(c.text, value)
	}
	return false
}

// Check runs ok on a new text before its hash, such as the generated check of the declared length; a stored hash and
// a value that is not set pass, because their text is not known. ok never gets a stored hash.
func (c Compared) Check(ok func(text string) bool) bool {
	if c.text == "" {
		return true
	}
	return ok(c.text)
}

// Stored gives the text that the column stores: the hash of a new text, the stored hash, or empty when not set.
func (c Compared) Stored() (string, error) {
	if c.text != "" {
		return Hash(c.text)
	}
	return c.hash, nil
}

// String gives [secret].
func (c Compared) String() string { return Redacted }

// GoString gives [secret].
func (c Compared) GoString() string { return Redacted }

// Format writes [secret] for each verb.
func (c Compared) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// LogValue gives [secret] to slog.
func (c Compared) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON encodes the value as null.
func (c Compared) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// MarshalText encodes the value as [secret].
func (c Compared) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// UnmarshalJSON refuses each value.
func (c *Compared) UnmarshalJSON([]byte) error { return ErrDecode }
