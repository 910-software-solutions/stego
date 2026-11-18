package sealed

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strconv"
	"strings"
)

// fingerprintLabel is the HKDF label of the key of the request fingerprints, so it is never the key that seals.
const fingerprintLabel = "stego-idempotency-fingerprint-v1"

// Fingerprint gives the keyed form of the request fingerprint raw, which the idempotency key table stores in a
// service with a secret field: k<key ID>: and the hexadecimal HMAC-SHA256 of raw with the fingerprint key of the
// first key of the ring (ST-SUP-RP16-LC05). A person who reads the table or a backup cannot then test candidate
// values of a secret in a request body against it (security review of PR #441, M2).
func (r *Ring) Fingerprint(raw string) string {
	return r.fingerprintWith(r.ids[0], raw)
}

func (r *Ring) fingerprintWith(id uint32, raw string) string {
	mac := hmac.New(sha256.New, r.fingerprint[id])
	mac.Write([]byte(raw))
	return "k" + strconv.FormatUint(uint64(id), 10) + ":" + hex.EncodeToString(mac.Sum(nil))
}

// FingerprintMatches tells, in constant time, whether stored is the keyed form of raw with a key of the ring: the
// key that its prefix names, so a key that is no longer first still matches the fingerprints of its time while the
// ring holds it.
func (r *Ring) FingerprintMatches(stored, raw string) bool {
	rest, ok := strings.CutPrefix(stored, "k")
	if !ok {
		return false
	}
	idText, _, ok := strings.Cut(rest, ":")
	if !ok {
		return false
	}
	id, err := strconv.ParseUint(idText, 10, 32)
	if err != nil {
		return false
	}
	if _, held := r.fingerprint[uint32(id)]; !held {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(r.fingerprintWith(uint32(id), raw))) == 1
}
