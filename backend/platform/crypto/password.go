// Package crypto owns credential hashing primitives shared by authentication adapters.
package crypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonMemory      = 64 * 1024
	argonIterations  = 3
	argonParallelism = 2
	argonSaltLength  = 16
	argonKeyLength   = 32
)

var ErrInvalidPasswordHash = errors.New("invalid password hash")

// PasswordHasher stores passwords in the standard Argon2id PHC string format.
type PasswordHasher struct{}

func (PasswordHasher) Hash(password string) (string, error) {
	salt := make([]byte, argonSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonIterations, argonMemory, argonParallelism, argonKeyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemory,
		argonIterations,
		argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

func (PasswordHasher) Verify(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrInvalidPasswordHash
	}
	version, err := parseParameter(parts[2], "v", 32)
	if err != nil || version != argon2.Version {
		return false, ErrInvalidPasswordHash
	}
	parameters := strings.Split(parts[3], ",")
	if len(parameters) != 3 {
		return false, ErrInvalidPasswordHash
	}
	memory, memoryErr := parseParameter(parameters[0], "m", 32)
	iterations, iterationsErr := parseParameter(parameters[1], "t", 32)
	parallelism, parallelismErr := parseParameter(parameters[2], "p", 8)
	if memoryErr != nil || iterationsErr != nil || parallelismErr != nil ||
		memory == 0 || memory > 1024*1024 || iterations == 0 || iterations > 10 || parallelism == 0 || parallelism > 16 {
		return false, ErrInvalidPasswordHash
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return false, ErrInvalidPasswordHash
	}
	want, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false, ErrInvalidPasswordHash
	}
	got := argon2.IDKey([]byte(password), salt, uint32(iterations), uint32(memory), uint8(parallelism), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func parseParameter(value, name string, bits int) (uint64, error) {
	prefix, raw, found := strings.Cut(value, "=")
	if !found || prefix != name || raw == "" {
		return 0, ErrInvalidPasswordHash
	}
	parsed, err := strconv.ParseUint(raw, 10, bits)
	if err != nil {
		return 0, ErrInvalidPasswordHash
	}
	return parsed, nil
}
