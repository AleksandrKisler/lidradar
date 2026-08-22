package application

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"time"
)

var ErrInvalidLinkToken = errors.New("invalid or expired telegram link token")

type LinkToken struct {
	ID, TenantID, UserID, TokenHash string
	ExpiresAt                       time.Time
	UsedAt                          *time.Time
}
type TokenStore interface {
	SaveToken(context.Context, LinkToken) error
	UseToken(context.Context, string, time.Time, string, string) (LinkToken, error)
}

// TokenHash returns a one-way representation; plaintext tokens must never be persisted.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func TokenHashMatches(hash, token string) bool {
	want, err := hex.DecodeString(hash)
	if err != nil {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(want, got[:]) == 1
}

type Linker struct {
	store TokenStore
	ids   IDs
	now   func() time.Time
}

func NewLinker(store TokenStore, ids IDs, now func() time.Time) Linker {
	return Linker{store: store, ids: ids, now: now}
}
func (l Linker) Issue(ctx context.Context, tenantID, userID, plaintext string, ttl time.Duration) error {
	if l.store == nil || l.ids == nil || l.now == nil || tenantID == "" || userID == "" || plaintext == "" || ttl <= 0 {
		return ErrInvalidLinkToken
	}
	now := l.now().UTC()
	return l.store.SaveToken(ctx, LinkToken{ID: l.ids.NewID(), TenantID: tenantID, UserID: userID, TokenHash: TokenHash(plaintext), ExpiresAt: now.Add(ttl)})
}
func (l Linker) Redeem(ctx context.Context, plaintext, telegramUserID, chatID string) (LinkToken, error) {
	if l.store == nil || l.now == nil || plaintext == "" || telegramUserID == "" || chatID == "" {
		return LinkToken{}, ErrInvalidLinkToken
	}
	return l.store.UseToken(ctx, TokenHash(plaintext), l.now().UTC(), telegramUserID, chatID)
}
