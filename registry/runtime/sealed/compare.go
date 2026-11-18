package sealed

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// The parameters of the Argon2id hash of a compare-only secret (OWASP Password Storage Cheat Sheet): 19 MiB of memory,
// two passes and one lane, with a salt of 16 bytes and a hash of 32 bytes (ST-SUP-RP16-LC03).
const (
	hashMemory  = 19456
	hashTime    = 2
	hashThreads = 1
	saltSize    = 16
	hashSize    = 32
)

// The bounds of the parameters of a stored hash that Matches takes, so that a changed row cannot make a check slow
// without bound.
const (
	maxMemory  = 65536
	maxTime    = 10
	maxThreads = 4
)

// hashSlots bound the Argon2id hashes that run at one time: each takes 19 MiB, so parallel writes and checks of
// compare-only secrets cannot exhaust the memory of a pod (security review of PR #441, L3).
var hashSlots = make(chan struct{}, max(runtime.GOMAXPROCS(0), 1))

// idKey is argon2.IDKey in one of the hash slots. A wait for a slot ends with ctx, so a caller that gave up does
// not hash (security re-review of PR #441).
func idKey(ctx context.Context, value, salt []byte, passes, memory uint32, threads uint8, size uint32) ([]byte, error) {
	select {
	case hashSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-hashSlots }()
	return argon2.IDKey(value, salt, passes, memory, threads, size), nil
}

// Hash gives the PHC string of the Argon2id hash of value, with a new random salt (ST-SUP-RP16-LC03).
func Hash(value string) (string, error) { return HashContext(context.Background(), value) }

// HashContext is Hash, whose wait for a hash slot ends with ctx.
func HashContext(ctx context.Context, value string) (string, error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("sealed: no random salt: %w", err)
	}
	sum, err := idKey(ctx, []byte(value), salt, hashTime, hashMemory, hashThreads, hashSize)
	if err != nil {
		return "", err
	}
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, hashMemory, hashTime, hashThreads,
		b64.EncodeToString(salt), b64.EncodeToString(sum)), nil
}

// Matches tells whether value has the hash of the PHC string stored: it computes the hash of value with the salt and
// the parameters of stored and compares the two in constant time. A stored string that is not an Argon2id hash of
// version 19 within the bounds of the parameters matches no value (ST-SUP-RP16-LC03).
func Matches(stored, value string) bool { return MatchesContext(context.Background(), stored, value) }

// MatchesContext is Matches, whose wait for a hash slot ends with ctx; a wait that ends matches nothing.
func MatchesContext(ctx context.Context, stored, value string) bool {
	salt, sum, memory, passes, threads, ok := parseHash(stored)
	if !ok {
		return false
	}
	got, err := idKey(ctx, []byte(value), salt, passes, memory, threads, uint32(len(sum)))
	return err == nil && subtle.ConstantTimeCompare(got, sum) == 1
}

// parseHash reads a PHC string of Argon2id: $argon2id$v=19$m=<m>,t=<t>,p=<p>$<salt>$<hash>.
func parseHash(stored string) (salt, sum []byte, memory, passes uint32, threads uint8, ok bool) {
	parts := strings.Split(stored, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return nil, nil, 0, 0, 0, false
	}
	params := strings.Split(parts[3], ",")
	if len(params) != 3 {
		return nil, nil, 0, 0, 0, false
	}
	read := func(text, name string, most uint64) (uint64, bool) {
		value, found := strings.CutPrefix(text, name+"=")
		n, err := strconv.ParseUint(value, 10, 32)
		return n, found && err == nil && n >= 1 && n <= most
	}
	m, okM := read(params[0], "m", maxMemory)
	t, okT := read(params[1], "t", maxTime)
	p, okP := read(params[2], "p", maxThreads)
	if !okM || !okT || !okP || m < 8*p {
		return nil, nil, 0, 0, 0, false
	}
	b64 := base64.RawStdEncoding.Strict()
	salt, errSalt := b64.DecodeString(parts[4])
	sum, errSum := b64.DecodeString(parts[5])
	if errSalt != nil || errSum != nil || len(salt) < 8 || len(salt) > 64 || len(sum) < 16 || len(sum) > 64 {
		return nil, nil, 0, 0, 0, false
	}
	return salt, sum, uint32(m), uint32(t), uint8(p), true
}

// constantEqual compares two texts in constant time for texts of one length.
func constantEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
