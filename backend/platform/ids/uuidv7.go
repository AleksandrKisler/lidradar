// Package ids generates server-owned public identifiers.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"time"
)

// Generator creates UUIDv7-compatible identifiers using the current UTC
// millisecond and cryptographically secure random bits.
type Generator struct{}

func (Generator) NewID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate UUID randomness: %w", err)
	}

	milliseconds := uint64(time.Now().UTC().UnixMilli())
	var timestamp [8]byte
	binary.BigEndian.PutUint64(timestamp[:], milliseconds)
	copy(value[0:6], timestamp[2:8])
	value[6] = (value[6] & 0x0f) | 0x70
	value[8] = (value[8] & 0x3f) | 0x80

	return format(value), nil
}

func format(value [16]byte) string {
	var encoded [36]byte
	hex.Encode(encoded[0:8], value[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], value[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], value[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], value[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], value[10:16])
	return string(encoded[:])
}
