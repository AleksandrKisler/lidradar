package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// StubTransport accepts notification deliveries locally without contacting Telegram.
type StubTransport struct{}

func (StubTransport) Send(_ context.Context, destination, title, body, callbackData string) (string, bool, error) {
	digest := sha256.Sum256([]byte(destination + "\x00" + title + "\x00" + body + "\x00" + callbackData))
	return "stub-" + hex.EncodeToString(digest[:8]), false, nil
}
