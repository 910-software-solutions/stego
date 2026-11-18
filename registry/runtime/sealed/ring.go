package sealed

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// KeySize is the size in bytes of each key of the ring: AES-256.
const KeySize = 32

// maxRingFile is the most bytes that a key ring file may hold; a file of a thousand keys is far below it.
const maxRingFile = 1 << 20

// Ring is the key ring of the secret fields of a service (ST-SUP-RP16-LC02): its first key seals each new value, and
// each key opens a value of its key ID. A Ring is safe for concurrent use; it never changes after Parse.
type Ring struct {
	ids  []uint32
	aead map[uint32]cipher.AEAD
}

// ErrRing is the class of each error of a key ring file that does not read. The error names a line number and never a
// key.
var ErrRing = errors.New("sealed: the key ring does not read")

// ringError is a key ring file that does not read, at a line or as a whole.
func ringError(line int, problem string) error {
	if line == 0 {
		return fmt.Errorf("%w: %s", ErrRing, problem)
	}
	return fmt.Errorf("%w: line %d %s", ErrRing, line, problem)
}

// ReadRing reads the key ring from the file at path, with the rules of ParseRing. The error names a line number,
// never a key.
func ReadRing(path string) (*Ring, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ringError(0, "because the file does not open")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRingFile+1))
	if err != nil {
		return nil, ringError(0, "because the file does not read")
	}
	if len(data) > maxRingFile {
		return nil, ringError(0, "because the file holds more than 1 MiB")
	}
	return ParseRing(data)
}

// ParseRing reads a key ring from the lines <key ID> <key>: the key ID is a decimal number from 1 to 4294967295 and
// the key the standard base64 of exactly KeySize bytes. Empty lines and lines that start with # are skipped. It
// refuses no key, a line of another form, a key ID twice and one key twice, naming the line and never a key
// (ST-SUP-RP16-LC02).
func ParseRing(data []byte) (*Ring, error) {
	r := &Ring{aead: map[uint32]cipher.AEAD{}}
	var keys [][]byte
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 4096), maxRingFile)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 {
			return nil, ringError(line, "is not of the form <key ID> <key>")
		}
		id, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil || id == 0 || fields[0] != strconv.FormatUint(id, 10) {
			return nil, ringError(line, "has a key ID that is not a decimal number from 1 to 4294967295")
		}
		key, err := base64.StdEncoding.Strict().DecodeString(fields[1])
		if err != nil || len(key) != KeySize {
			return nil, ringError(line, fmt.Sprintf("has a key that is not the base64 of exactly %d bytes", KeySize))
		}
		if _, twice := r.aead[uint32(id)]; twice {
			return nil, ringError(line, "has a key ID that an earlier line has")
		}
		for _, other := range keys {
			if subtle.ConstantTimeCompare(other, key) == 1 {
				return nil, ringError(line, "has a key that an earlier line has")
			}
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, ringError(line, "has a key that AES does not take")
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, ringError(line, "has a key that GCM does not take")
		}
		keys = append(keys, key)
		r.ids = append(r.ids, uint32(id))
		r.aead[uint32(id)] = aead
	}
	if err := scanner.Err(); err != nil {
		return nil, ringError(line+1, "is longer than the file limit")
	}
	if len(r.ids) == 0 {
		return nil, ringError(0, "because it holds no key")
	}
	return r, nil
}

// First gives the key ID of the first key, which seals each new value.
func (r *Ring) First() uint32 { return r.ids[0] }

// IDs gives the key IDs of the ring in the order of the file.
func (r *Ring) IDs() []uint32 { return append([]uint32(nil), r.ids...) }
