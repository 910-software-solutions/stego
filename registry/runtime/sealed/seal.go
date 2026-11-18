package sealed

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
)

// Version is the version byte of the sealed form of ST-SUP-RP16-LC01.
const Version byte = 1

// HeaderSize is the size of the header of a sealed value: the version and the key ID. NonceSize and TagSize are those
// of AES-256-GCM.
const (
	HeaderSize = 1 + 4
	NonceSize  = 12
	TagSize    = 16
)

// aadLabel opens the associated data of each sealed value, so that no other use of a key gives the same associated
// data.
const aadLabel = "stego-sealed-v1"

// Place is where a sealed value is stored: the tenant of its row, empty in a service with no tenancy, its table, its
// column and the ID of its row. The associated data holds each of them, so a value does not open at another place.
type Place struct {
	Tenant, Table, Column, Row string
}

// ErrOpen is the class of each sealed value that does not open. The error names the column and the row of its place,
// never a value.
var ErrOpen = errors.New("sealed: the value does not open")

// aad gives the associated data of a value of the header at the place: the label, a zero byte, the header and then
// each part of the place as a four-byte length and its bytes.
func aad(header []byte, at Place) []byte {
	out := make([]byte, 0, len(aadLabel)+1+HeaderSize+16+len(at.Tenant)+len(at.Table)+len(at.Column)+len(at.Row))
	out = append(out, aadLabel...)
	out = append(out, 0)
	out = append(out, header...)
	for _, part := range []string{at.Tenant, at.Table, at.Column, at.Row} {
		out = binary.BigEndian.AppendUint32(out, uint32(len(part)))
		out = append(out, part...)
	}
	return out
}

// Seal seals plain at the place with the first key of the ring: the version, the key ID, a new random nonce and the
// AES-256-GCM ciphertext with its tag (ST-SUP-RP16-LC01).
func (r *Ring) Seal(plain []byte, at Place) ([]byte, error) {
	id := r.ids[0]
	aead := r.aead[id]
	out := make([]byte, HeaderSize+NonceSize, HeaderSize+NonceSize+len(plain)+TagSize)
	out[0] = Version
	binary.BigEndian.PutUint32(out[1:HeaderSize], id)
	if _, err := rand.Read(out[HeaderSize:]); err != nil {
		return nil, fmt.Errorf("sealed: no random nonce: %w", err)
	}
	return aead.Seal(out, out[HeaderSize:], plain, aad(out[:HeaderSize], at)), nil
}

// Open opens a sealed value at the place with the key of its key ID. It refuses another version, a key ID that is not
// in the ring, a value shorter than its header, nonce and tag, and a tag that does not match, also when the value
// comes from another place (ST-SUP-RP16-LC01). The error names the column and the row, never a value.
func (r *Ring) Open(sealed []byte, at Place) ([]byte, error) {
	if len(sealed) < HeaderSize+NonceSize+TagSize {
		return nil, openError(at, "is shorter than a sealed value")
	}
	if sealed[0] != Version {
		return nil, openError(at, "has a version that this runtime does not open")
	}
	id := binary.BigEndian.Uint32(sealed[1:HeaderSize])
	aead, ok := r.aead[id]
	if !ok {
		return nil, openError(at, fmt.Sprintf("is sealed with the key ID %d, which is not in the key ring", id))
	}
	plain, err := aead.Open(nil, sealed[HeaderSize:HeaderSize+NonceSize], sealed[HeaderSize+NonceSize:], aad(sealed[:HeaderSize], at))
	if err != nil {
		return nil, openError(at, "does not match its place or its key")
	}
	return plain, nil
}

// KeyID gives the key ID of a sealed value, and false for a value too short for a header or of another version.
func KeyID(sealed []byte) (uint32, bool) {
	if len(sealed) < HeaderSize || sealed[0] != Version {
		return 0, false
	}
	return binary.BigEndian.Uint32(sealed[1:HeaderSize]), true
}

// Prefix gives the header of a value that the key ID seals: the version and the key ID. The re-seal job reads the rows
// whose value does not start with the Prefix of the first key.
func Prefix(id uint32) []byte {
	out := make([]byte, HeaderSize)
	out[0] = Version
	binary.BigEndian.PutUint32(out[1:], id)
	return out
}

func openError(at Place, problem string) error {
	return fmt.Errorf("%w: the column %s of the row %s of %s %s", ErrOpen, at.Column, at.Row, at.Table, problem)
}
