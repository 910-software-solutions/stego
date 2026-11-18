package idempotency

import (
	"bytes"
	"errors"
	"strings"
)

// The sealed answers of ST-SUP-RP07-LC13 (decision on ST-SUP-Q182 (b)): an answer can hold the pii of a row, so a
// command runtime with the key ring of a service with a secret or a pii field gives Wrap a Sealer, and the key table
// keeps the headers and the body of each completed answer sealed for the retention of the key. A replay opens them
// and gives back the first answer byte for byte.

// Sealer seals and opens the stored headers and body of one answer at the place of its tenant, its column of the key
// table and its row, which holds the principal and the key. The command runtime gives one over its key ring.
type Sealer interface {
	SealAnswer(tenant, column, row string, plain []byte) ([]byte, error)
	OpenAnswer(tenant, column, row string, data []byte) ([]byte, error)
}

// SealedPrefix marks the stored fingerprint of a row whose answer is sealed. A plain fingerprint is lower-case
// hexadecimal and a keyed one starts with k, so neither starts with it.
const SealedPrefix = "s1:"

// The columns of the key table that hold a sealed answer, which the place of each sealed value names.
const (
	HeadersColumn = "response_headers"
	BodyColumn    = "response_body"
	// AnswerTable is the table of the place of a sealed answer.
	AnswerTable = "idempotency_keys"
)

// ErrSealed is the error of a stored answer that does not seal or open. Its text names no value.
var ErrSealed = errors.New("idempotency: a stored answer does not seal or open")

// AnswerRow gives the row of the place of a sealed answer: the principal and the key, each as a four-byte length and
// its bytes, so no two scopes give the same row.
func AnswerRow(principal, key string) string {
	var out bytes.Buffer
	putString(&out, principal)
	putString(&out, key)
	return out.String()
}

// storedFingerprint gives the fingerprint that the key row of req keeps: with SealedPrefix when its answer is sealed.
func (req Request) storedFingerprint() string {
	if req.Sealer != nil {
		return SealedPrefix + req.Fingerprint
	}
	return req.Fingerprint
}

// sealed gives the stored fingerprint without its prefix, and whether the row was sealed. A row of the other form
// does not match: a release that seals never replays a clear answer as its own, and a release that does not seal
// never replays the sealed bytes (ST-SUP-RP07-LC13).
func (req Request) unsealed(stored string) (string, bool) {
	rest, sealed := strings.CutPrefix(stored, SealedPrefix)
	if sealed != (req.Sealer != nil) {
		return "", false
	}
	return rest, true
}

// sealAnswer gives the headers and the body that the key row of req keeps.
func (req Request) sealAnswer(header, body []byte) ([]byte, []byte, error) {
	if req.Sealer == nil {
		return header, body, nil
	}
	row := AnswerRow(req.Principal, req.Key)
	h, err := req.Sealer.SealAnswer(req.Tenant, HeadersColumn, row, header)
	if err != nil {
		return nil, nil, ErrSealed
	}
	b, err := req.Sealer.SealAnswer(req.Tenant, BodyColumn, row, body)
	if err != nil {
		return nil, nil, ErrSealed
	}
	return h, b, nil
}

// openAnswer gives the headers and the body of a stored answer of req.
func (req Request) openAnswer(header, body []byte) ([]byte, []byte, error) {
	if req.Sealer == nil {
		return header, body, nil
	}
	row := AnswerRow(req.Principal, req.Key)
	h, err := req.Sealer.OpenAnswer(req.Tenant, HeadersColumn, row, header)
	if err != nil {
		return nil, nil, ErrSealed
	}
	b, err := req.Sealer.OpenAnswer(req.Tenant, BodyColumn, row, body)
	if err != nil {
		return nil, nil, ErrSealed
	}
	return h, b, nil
}
