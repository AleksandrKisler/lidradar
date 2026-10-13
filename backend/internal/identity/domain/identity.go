// Package domain owns LidRadar users and server-side sessions.
package domain

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalid  = errors.New("invalid identity state")
	ErrNotFound = errors.New("identity resource not found")
	ErrConflict = errors.New("identity resource conflict")
	// ErrInvitationInvalid: код платформы нельзя погасить (нет, просрочен, использован, отозван), ADR 0057.
	ErrInvitationInvalid = errors.New("registration invitation cannot be used")
)

type UserStatus string

const (
	UserActive   UserStatus = "ACTIVE"
	UserDisabled UserStatus = "DISABLED"
)

type User struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"`
	PasswordHash string     `json:"-"`
	DisplayName  string     `json:"displayName"`
	Status       UserStatus `json:"status"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	// OrganizationRights — сколько организаций пользователь ещё вправе создать в режиме invite_only
	// (ADR 0057); наружу не отдаётся, интерфейс получает только canCreateOrganization.
	OrganizationRights int `json:"-"`
}

func NewUser(id, email, passwordHash, displayName string, at time.Time) (User, error) {
	normalizedEmail, err := NormalizeEmail(email)
	displayName = strings.TrimSpace(displayName)
	if err != nil || id == "" || passwordHash == "" || displayName == "" || utf8.RuneCountInString(displayName) > 200 || at.IsZero() {
		return User{}, ErrInvalid
	}
	at = at.UTC()
	return User{ID: id, Email: normalizedEmail, PasswordHash: passwordHash, DisplayName: displayName, Status: UserActive, CreatedAt: at, UpdatedAt: at}, nil
}

func NormalizeEmail(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Address != value || len(value) > 254 {
		return "", ErrInvalid
	}
	return value, nil
}

type Session struct {
	ID        string
	UserID    string
	TokenHash string
	ExpiresAt time.Time
	IPAddress string
	UserAgent string
	CreatedAt time.Time
	RevokedAt *time.Time
}

func NewSession(id, userID, tokenHash string, createdAt, expiresAt time.Time, ipAddress, userAgent string) (Session, error) {
	if id == "" || userID == "" || len(tokenHash) != 64 || createdAt.IsZero() || !expiresAt.After(createdAt) {
		return Session{}, ErrInvalid
	}
	if len(userAgent) > 1024 {
		userAgent = userAgent[:1024]
	}
	return Session{
		ID: id, UserID: userID, TokenHash: tokenHash,
		CreatedAt: createdAt.UTC(), ExpiresAt: expiresAt.UTC(),
		IPAddress: strings.TrimSpace(ipAddress), UserAgent: userAgent,
	}, nil
}

// Repository owns durable users and opaque sessions. Session token plaintext
// never crosses this contract.
type Repository interface {
	CreateUserSession(context.Context, User, Session) error
	// CreateUserSessionWithPlatformInvitation создаёт пользователя, сессию и гасит код платформы в одной
	// транзакции (ADR 0057). Код, который нельзя погасить, возвращает ErrInvitationInvalid без изменений;
	// занятая почта — ErrConflict, и код при этом не расходуется.
	CreateUserSessionWithPlatformInvitation(ctx context.Context, user User, session Session, codeHash string, at time.Time) error
	// PlatformInvitationUsable сообщает, можно ли погасить код платформы в момент at, не расходуя его.
	PlatformInvitationUsable(ctx context.Context, codeHash string, at time.Time) (bool, error)
	// DeleteNewUser убирает только что созданного пользователя без других следов и его сессии: это
	// компенсация регистрации по коду сотрудника, когда приём кода не удался (ADR 0057).
	DeleteNewUser(ctx context.Context, userID string) error
	UserByEmail(context.Context, string) (User, bool, error)
	UserBySessionHash(context.Context, string, time.Time) (User, bool, error)
	CreateSession(context.Context, Session) error
	RotateSession(context.Context, string, Session, time.Time) (User, bool, error)
	RevokeSession(context.Context, string, time.Time) error
}
