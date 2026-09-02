// Package application coordinates authentication and session lifecycle.
package application

import (
	"context"
	"errors"
	"strings"
	"time"

	"lidradar/backend/internal/identity/domain"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrUnauthenticated    = errors.New("authentication required")
	ErrInvalidInput       = errors.New("invalid identity input")
	ErrEmailConflict      = errors.New("email already registered")
)

const (
	minimumPasswordLength = 12
	maximumPasswordLength = 1024
)

type Passwords interface {
	Hash(string) (string, error)
	Verify(string, string) (bool, error)
}

type IDs interface{ NewID() (string, error) }

type Tokens interface {
	NewToken() (plaintext, digest string, err error)
	HashToken(string) string
}

type Client struct {
	IPAddress string
	UserAgent string
}

type Authenticated struct {
	User  domain.User
	Token string
}

// MembershipSummary is the small cross-module contract used by /auth/me so a
// user can select an explicit tenant after login.
type MembershipSummary struct {
	TenantID         string `json:"tenantId"`
	OrganizationName string `json:"organizationName"`
	Role             string `json:"role"`
}

type MembershipLister interface {
	MembershipsForUser(context.Context, string) ([]MembershipSummary, error)
}

type Service struct {
	repository domain.Repository
	limiter    RateLimiter
	passwords  Passwords
	ids        IDs
	tokens     Tokens
	now        func() time.Time
	sessionTTL time.Duration
}

func NewService(repository domain.Repository, limiter RateLimiter, passwords Passwords, ids IDs, tokens Tokens, now func() time.Time, sessionTTL time.Duration) Service {
	return Service{repository: repository, limiter: limiter, passwords: passwords, ids: ids, tokens: tokens, now: now, sessionTTL: sessionTTL}
}

func (s Service) Register(ctx context.Context, email, password, displayName string, client Client) (Authenticated, error) {
	if err := s.ready(); err != nil {
		return Authenticated{}, ErrInvalidInput
	}
	if err := s.take(ctx, ScopeRegisterIP, client.IPAddress, registerIPLimit, registerIPWindow); err != nil {
		return Authenticated{}, err
	}
	if !validPassword(password) {
		return Authenticated{}, ErrInvalidInput
	}
	normalizedEmail, err := domain.NormalizeEmail(email)
	if err != nil {
		return Authenticated{}, ErrInvalidInput
	}
	passwordHash, err := s.passwords.Hash(password)
	if err != nil {
		return Authenticated{}, err
	}
	userID, err := s.ids.NewID()
	if err != nil {
		return Authenticated{}, err
	}
	now := s.now().UTC()
	user, err := domain.NewUser(userID, normalizedEmail, passwordHash, displayName, now)
	if err != nil {
		return Authenticated{}, ErrInvalidInput
	}
	session, token, err := s.newSession(user.ID, now, client)
	if err != nil {
		return Authenticated{}, err
	}
	if err := s.repository.CreateUserSession(ctx, user, session); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return Authenticated{}, ErrEmailConflict
		}
		return Authenticated{}, err
	}
	return Authenticated{User: user, Token: token}, nil
}

func (s Service) Login(ctx context.Context, email, password string, client Client) (Authenticated, error) {
	if err := s.ready(); err != nil {
		return Authenticated{}, ErrInvalidCredentials
	}
	if err := s.take(ctx, ScopeLoginIP, client.IPAddress, loginIPLimit, loginIPWindow); err != nil {
		return Authenticated{}, err
	}
	accountSubject := rateSubject(email)
	if err := s.takeSubject(ctx, ScopeLoginAccount, accountSubject, loginAccountLimit, loginAccountWindow); err != nil {
		return Authenticated{}, err
	}
	if password == "" || len(password) > maximumPasswordLength {
		return Authenticated{}, ErrInvalidCredentials
	}
	normalizedEmail, err := domain.NormalizeEmail(email)
	if err != nil {
		return Authenticated{}, ErrInvalidCredentials
	}
	user, found, err := s.repository.UserByEmail(ctx, normalizedEmail)
	if err != nil {
		return Authenticated{}, err
	}
	if !found {
		// Perform the same password-hardening work for an unknown email so the
		// public response does not become a practical account enumeration signal.
		if _, err := s.passwords.Hash(password); err != nil {
			return Authenticated{}, err
		}
		return Authenticated{}, ErrInvalidCredentials
	}
	valid, err := s.passwords.Verify(password, user.PasswordHash)
	if err != nil {
		return Authenticated{}, err
	}
	if !valid || user.Status != domain.UserActive {
		return Authenticated{}, ErrInvalidCredentials
	}
	if err := s.limiter.Reset(ctx, ScopeLoginAccount, accountSubject); err != nil {
		return Authenticated{}, err
	}
	now := s.now().UTC()
	session, token, err := s.newSession(user.ID, now, client)
	if err != nil {
		return Authenticated{}, err
	}
	if err := s.repository.CreateSession(ctx, session); err != nil {
		return Authenticated{}, err
	}
	return Authenticated{User: user, Token: token}, nil
}

func (s Service) Authenticate(ctx context.Context, token string) (domain.User, error) {
	if s.repository == nil || s.tokens == nil || s.now == nil || strings.TrimSpace(token) == "" {
		return domain.User{}, ErrUnauthenticated
	}
	user, found, err := s.repository.UserBySessionHash(ctx, s.tokens.HashToken(token), s.now().UTC())
	if err != nil {
		return domain.User{}, err
	}
	if !found || user.Status != domain.UserActive {
		return domain.User{}, ErrUnauthenticated
	}
	return user, nil
}

func (s Service) Logout(ctx context.Context, token string) error {
	if s.repository == nil || s.tokens == nil || s.now == nil || strings.TrimSpace(token) == "" {
		return nil
	}
	return s.repository.RevokeSession(ctx, s.tokens.HashToken(token), s.now().UTC())
}

func (s Service) Refresh(ctx context.Context, currentToken string, client Client) (Authenticated, error) {
	if err := s.ready(); err != nil {
		return Authenticated{}, ErrUnauthenticated
	}
	if err := s.take(ctx, ScopeRefreshIP, client.IPAddress, refreshIPLimit, refreshIPWindow); err != nil {
		return Authenticated{}, err
	}
	if strings.TrimSpace(currentToken) == "" {
		return Authenticated{}, ErrUnauthenticated
	}
	currentUser, err := s.Authenticate(ctx, currentToken)
	if err != nil {
		return Authenticated{}, err
	}
	now := s.now().UTC()
	newSession, plaintext, err := s.newSession(currentUser.ID, now, client)
	if err != nil {
		return Authenticated{}, err
	}
	user, found, err := s.repository.RotateSession(ctx, s.tokens.HashToken(currentToken), newSession, now)
	if err != nil {
		return Authenticated{}, err
	}
	if !found {
		return Authenticated{}, ErrUnauthenticated
	}
	return Authenticated{User: user, Token: plaintext}, nil
}

func (s Service) ready() error {
	if s.repository == nil || s.limiter == nil || s.passwords == nil || s.ids == nil || s.tokens == nil || s.now == nil || s.sessionTTL <= 0 {
		return ErrInvalidInput
	}
	return nil
}

func (s Service) take(ctx context.Context, scope, value string, limit int, window time.Duration) error {
	return s.takeSubject(ctx, scope, rateSubject(value), limit, window)
}

func (s Service) takeSubject(ctx context.Context, scope, subject string, limit int, window time.Duration) error {
	now := s.now().UTC()
	decision, err := s.limiter.Take(ctx, scope, subject, limit, window, now)
	if err != nil {
		return err
	}
	if decision.Allowed {
		return nil
	}
	retryAfter := decision.ExpiresAt.Sub(now)
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	return RateLimitError{RetryAfter: retryAfter}
}

func (s Service) newSession(userID string, now time.Time, client Client) (domain.Session, string, error) {
	sessionID, err := s.ids.NewID()
	if err != nil {
		return domain.Session{}, "", err
	}
	plaintext, digest, err := s.tokens.NewToken()
	if err != nil {
		return domain.Session{}, "", err
	}
	session, err := domain.NewSession(sessionID, userID, digest, now, now.Add(s.sessionTTL), client.IPAddress, client.UserAgent)
	return session, plaintext, err
}

func validPassword(password string) bool {
	return len(password) >= minimumPasswordLength && len(password) <= maximumPasswordLength
}
