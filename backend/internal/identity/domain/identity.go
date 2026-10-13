// Package domain owns LidRadar users and server-side sessions.
package domain

import (
	"context"
	"errors"
	"net/mail"
	"regexp"
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
	// ErrTokenInvalid: токен подтверждения почты или сброса пароля нельзя погасить (нет, просрочен,
	// использован, отозван или пользователь отключён), ADR 0058. Причина не различается.
	ErrTokenInvalid = errors.New("account token cannot be used")
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
	// EmailVerifiedAt — когда подтверждён адрес почты (ADR 0058). Подтверждение мягкое: пустое значение
	// ничего не блокирует.
	EmailVerifiedAt *time.Time `json:"emailVerifiedAt"`
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

// TokenPurpose — назначение одноразового токена учётной записи (ADR 0058).
type TokenPurpose string

const (
	TokenEmailVerification TokenPurpose = "EMAIL_VERIFICATION"
	TokenPasswordReset     TokenPurpose = "PASSWORD_RESET"
)

// AccountToken — запись об одноразовом токене. Открытый токен в запись не попадает: только SHA-256.
type AccountToken struct {
	ID        string
	UserID    string
	Purpose   TokenPurpose
	TokenHash string
	CreatedAt time.Time
	ExpiresAt time.Time
}

func NewAccountToken(id, userID string, purpose TokenPurpose, tokenHash string, createdAt, expiresAt time.Time) (AccountToken, error) {
	if id == "" || userID == "" || len(tokenHash) != 64 || createdAt.IsZero() || !expiresAt.After(createdAt) ||
		(purpose != TokenEmailVerification && purpose != TokenPasswordReset) {
		return AccountToken{}, ErrInvalid
	}
	return AccountToken{
		ID: id, UserID: userID, Purpose: purpose, TokenHash: tokenHash,
		CreatedAt: createdAt.UTC(), ExpiresAt: expiresAt.UTC(),
	}, nil
}

var auditOperationPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,99}$`)

// AuditRecord — запись журнала входов, которую хранилище пишет в той же транзакции, что и изменение
// учётной записи (ADR 0058). Пользователя назначает команда, секреты в запись не попадают.
type AuditRecord struct {
	ID        string
	Operation string
	IPAddress string
	At        time.Time
}

func NewAuditRecord(id, operation, ipAddress string, at time.Time) (AuditRecord, error) {
	ipAddress = strings.TrimSpace(ipAddress)
	if id == "" || !auditOperationPattern.MatchString(operation) || len(ipAddress) > 64 || at.IsZero() {
		return AuditRecord{}, ErrInvalid
	}
	return AuditRecord{ID: id, Operation: operation, IPAddress: ipAddress, At: at.UTC()}, nil
}

// PasswordChange — смена пароля вошедшим пользователем. Выполняется одной транзакцией: пароль меняется,
// если хэш всё ещё прежний, остальные сессии отзываются, токены сброса отзываются, пишется аудит.
type PasswordChange struct {
	UserID       string
	PreviousHash string
	NewHash      string
	// KeepSessionHash — хэш текущей сессии: она остаётся действующей.
	KeepSessionHash string
	At              time.Time
	Audit           AuditRecord
}

// PasswordReset — завершение сброса пароля по токену из письма.
type PasswordReset struct {
	TokenHash string
	NewHash   string
	At        time.Time
	Audit     AuditRecord
}

// Repository owns durable users and opaque sessions. Session token plaintext
// never crosses this contract.
type Repository interface {
	// ChangePassword меняет пароль условно по прежнему хэшу: если он уже другой, возвращает ErrConflict.
	ChangePassword(context.Context, PasswordChange) error
	// IssueAccountToken отзывает неиспользованные токены того же назначения и записывает новый; audit
	// пишется в той же транзакции, если задан.
	IssueAccountToken(ctx context.Context, token AccountToken, audit *AuditRecord) error
	// AccountTokenUsable сообщает, можно ли погасить токен в момент at, не расходуя его.
	AccountTokenUsable(ctx context.Context, purpose TokenPurpose, tokenHash string, at time.Time) (bool, error)
	// CompletePasswordReset гасит токен, меняет пароль, отзывает все сессии и прочие токены сброса,
	// подтверждает адрес и пишет аудит; возвращает адрес пользователя. Негодный токен — ErrTokenInvalid.
	CompletePasswordReset(context.Context, PasswordReset) (email string, err error)
	// CompleteEmailVerification гасит токен и подтверждает адрес; негодный токен — ErrTokenInvalid.
	CompleteEmailVerification(ctx context.Context, tokenHash string, at time.Time, audit AuditRecord) error
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
