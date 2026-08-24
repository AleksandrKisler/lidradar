package infrastructure

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

type SessionTokens struct{}

func (SessionTokens) NewToken() (string, string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", "", fmt.Errorf("generate session token: %w", err)
	}
	plaintext := base64.RawURLEncoding.EncodeToString(value)
	return plaintext, hashToken(plaintext), nil
}

func (SessionTokens) HashToken(plaintext string) string { return hashToken(plaintext) }

func hashToken(plaintext string) string {
	digest := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(digest[:])
}
