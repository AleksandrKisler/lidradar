// Package domain owns LidRadar users and server-side sessions.
package domain

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"
)

var (
	ErrInvalid  = errors.New("invalid identity state")
	ErrNotFound = errors.New("identity resource not found")
	ErrConflict = errors.New("identity resource conflict")
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
}

func NewUser(id, email, passwordHash, displayName string, at time.Time) (User, error) {
	normalizedEmail, err := NormalizeEmail(email)
	displayName = strings.TrimSpace(displayName)
	if err != nil || id == "" || passwordHash == "" || displayName == "" || len(displayName) > 200 || at.IsZero() {
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
	UserByEmail(context.Context, string) (User, bool, error)
	UserBySessionHash(context.Context, string, time.Time) (User, bool, error)
	CreateSession(context.Context, Session) error
	RotateSession(context.Context, string, Session, time.Time) (User, bool, error)
	RevokeSession(context.Context, string, time.Time) error
}
