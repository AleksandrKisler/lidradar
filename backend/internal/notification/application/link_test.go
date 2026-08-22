package application_test

import (
	"context"
	"testing"
	"time"

	"lidradar/backend/internal/notification/application"
)

type tokens struct {
	saved              application.LinkToken
	telegramUser, chat string
}

func (t *tokens) SaveToken(_ context.Context, v application.LinkToken) error { t.saved = v; return nil }
func (t *tokens) UseToken(_ context.Context, hash string, at time.Time, user, chat string) (application.LinkToken, error) {
	if hash != t.saved.TokenHash || !at.Before(t.saved.ExpiresAt) || t.saved.UsedAt != nil {
		return application.LinkToken{}, application.ErrInvalidLinkToken
	}
	t.saved.UsedAt = &at
	t.telegramUser, t.chat = user, chat
	return t.saved, nil
}
func TestLinkTokenIsHashedExpiringAndOneTime(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	store := new(tokens)
	linker := application.NewLinker(store, new(ids), func() time.Time { return now })
	if err := linker.Issue(context.Background(), "tenant", "owner", "plaintext-secret", time.Minute); err != nil {
		t.Fatal(err)
	}
	if store.saved.TokenHash == "plaintext-secret" || !application.TokenHashMatches(store.saved.TokenHash, "plaintext-secret") {
		t.Fatal("plaintext token was stored")
	}
	if _, err := linker.Redeem(context.Background(), "plaintext-secret", "tg-user", "chat"); err != nil {
		t.Fatal(err)
	}
	if _, err := linker.Redeem(context.Background(), "plaintext-secret", "tg-user", "chat"); err == nil {
		t.Fatal("token reused")
	}
}
