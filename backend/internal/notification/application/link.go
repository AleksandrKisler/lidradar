package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"
)

var ErrInvalidLinkToken = errors.New("некорректный или просроченный код привязки Telegram")

type LinkToken struct {
	ID, TenantID, UserID, TokenHash string
	ExpiresAt, CreatedAt            time.Time
	UsedAt                          *time.Time
}

type IssuedLinkToken struct {
	Plaintext, StartURL string
	ExpiresAt           time.Time
}

type TelegramLink struct {
	TenantID, UserID, TelegramUserID, ChatID string
	LinkedAt                                 time.Time
}

type TokenStore interface {
	SaveToken(context.Context, LinkToken) error
	UseToken(context.Context, string, string, time.Time, string, string) (LinkToken, error)
}

type LinkManagementStore interface {
	Link(context.Context, string, string) (TelegramLink, bool, error)
	DisableLink(context.Context, string, string, time.Time) (bool, error)
}

// TokenHash возвращает одностороннее представление. Открытый код запрещено
// сохранять в базе или журнале.
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

func (linker Linker) Issue(ctx context.Context, tenantID, userID string, ttl time.Duration) (IssuedLinkToken, error) {
	if linker.store == nil || linker.ids == nil || linker.now == nil || tenantID == "" || userID == "" || ttl <= 0 {
		return IssuedLinkToken{}, ErrInvalidLinkToken
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return IssuedLinkToken{}, err
	}
	plaintext := base64.RawURLEncoding.EncodeToString(random)
	id, err := linker.ids.NewID()
	if err != nil {
		return IssuedLinkToken{}, err
	}
	now := linker.now().UTC()
	token := LinkToken{
		ID: id, TenantID: tenantID, UserID: userID, TokenHash: TokenHash(plaintext),
		ExpiresAt: now.Add(ttl), CreatedAt: now,
	}
	if err := linker.store.SaveToken(ctx, token); err != nil {
		return IssuedLinkToken{}, err
	}
	return IssuedLinkToken{Plaintext: plaintext, ExpiresAt: token.ExpiresAt}, nil
}

func (linker Linker) Redeem(
	ctx context.Context,
	tenantID, plaintext, telegramUserID, chatID string,
) (LinkToken, error) {
	if linker.store == nil || linker.now == nil || tenantID == "" || plaintext == "" || telegramUserID == "" || chatID == "" {
		return LinkToken{}, ErrInvalidLinkToken
	}
	return linker.store.UseToken(ctx, tenantID, TokenHash(plaintext), linker.now().UTC(), telegramUserID, chatID)
}

type MembershipAuthorizer interface {
	ActiveMember(context.Context, string, string) (bool, error)
}

type LinkService struct {
	linker      Linker
	store       LinkManagementStore
	authorizer  MembershipAuthorizer
	botUsername string
	now         func() time.Time
}

var botUsernamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{5,32}$`)

func NewLinkService(
	linker Linker,
	store LinkManagementStore,
	authorizer MembershipAuthorizer,
	botUsername string,
	now func() time.Time,
) LinkService {
	return LinkService{
		linker: linker, store: store, authorizer: authorizer,
		botUsername: strings.TrimPrefix(strings.TrimSpace(botUsername), "@"), now: now,
	}
}

func (service LinkService) Issue(ctx context.Context, actorID, tenantID string) (IssuedLinkToken, error) {
	if service.authorizer == nil || service.store == nil || service.now == nil || !botUsernamePattern.MatchString(service.botUsername) {
		return IssuedLinkToken{}, ErrInvalidLinkToken
	}
	allowed, err := service.authorizer.ActiveMember(ctx, actorID, tenantID)
	if err != nil {
		return IssuedLinkToken{}, err
	}
	if !allowed {
		return IssuedLinkToken{}, ErrForbidden
	}
	issued, err := service.linker.Issue(ctx, tenantID, actorID, DefaultLinkTokenTTL)
	if err != nil {
		return IssuedLinkToken{}, err
	}
	issued.StartURL = "https://t.me/" + service.botUsername + "?start=" + issued.Plaintext
	return issued, nil
}

func (service LinkService) Status(ctx context.Context, actorID, tenantID string) (TelegramLink, bool, error) {
	if service.authorizer == nil || service.store == nil || actorID == "" || tenantID == "" {
		return TelegramLink{}, false, ErrInvalidLinkToken
	}
	allowed, err := service.authorizer.ActiveMember(ctx, actorID, tenantID)
	if err != nil {
		return TelegramLink{}, false, err
	}
	if !allowed {
		return TelegramLink{}, false, ErrForbidden
	}
	return service.store.Link(ctx, tenantID, actorID)
}

func (service LinkService) Disable(ctx context.Context, actorID, tenantID string) error {
	if service.authorizer == nil || service.store == nil || service.now == nil || actorID == "" || tenantID == "" {
		return ErrInvalidLinkToken
	}
	allowed, err := service.authorizer.ActiveMember(ctx, actorID, tenantID)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	_, err = service.store.DisableLink(ctx, tenantID, actorID, service.now().UTC())
	return err
}
