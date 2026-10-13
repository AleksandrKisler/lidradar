// Package application coordinates authentication and session lifecycle.
package application

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	auditapplication "lidradar/backend/internal/audit/application"
	"lidradar/backend/internal/identity/domain"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrUnauthenticated    = errors.New("authentication required")
	ErrInvalidInput       = errors.New("invalid identity input")
	ErrEmailConflict      = errors.New("email already registered")
	// ErrInvitationRequired: в режиме invite_only регистрация без кода невозможна (ADR 0057).
	ErrInvitationRequired = errors.New("registration requires an invitation")
	// ErrInvitationInvalid: код неизвестен, просрочен, использован или отозван; причина не различается.
	ErrInvitationInvalid = errors.New("invitation code cannot be used")
)

// RegistrationMode задаёт, кто может создать учётную запись (ADR 0057). Нулевое значение
// считается закрытым режимом: забытая настройка не должна открывать регистрацию.
type RegistrationMode string

const (
	RegistrationOpen       RegistrationMode = "open"
	RegistrationInviteOnly RegistrationMode = "invite_only"
)

// invitationCodePattern — формат кодов обоих видов: 256 бит в base64url без дополнения.
var invitationCodePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// MemberInvitations — узкий контракт приглашений сотрудников (ADR 0045), который реализует модуль
// tenant: регистрация проверяет код, не расходуя его, создаёт пользователя и только потом принимает код.
type MemberInvitations interface {
	// CheckMemberInvitation возвращает nil, если код можно принять сейчас, и ErrInvitationInvalid, если нельзя.
	CheckMemberInvitation(ctx context.Context, code string) error
	// AcceptMemberInvitation принимает код для только что созданного пользователя; ErrInvitationInvalid
	// означает, что приём однозначно не состоялся (гонка), любая другая ошибка неоднозначна.
	AcceptMemberInvitation(ctx context.Context, userID, code string) error
}

// RegistrationInput — поля запроса на регистрацию; InviteCode необязателен только в режиме open.
type RegistrationInput struct {
	Email       string
	Password    string
	DisplayName string
	InviteCode  string
}

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
	auditor    auditapplication.Recorder
	mode       RegistrationMode
	invitation MemberInvitations
	mail       mailSettings
}

// WithAuditor включает аудит входа, выхода и регистрации (ТЗ §65).
func (s Service) WithAuditor(auditor auditapplication.Recorder) Service {
	s.auditor = auditor
	return s
}

// WithRegistration задаёт режим регистрации и источник кодов сотрудников (ADR 0057); без вызова
// действует закрытый режим, а коды сотрудников не принимаются.
func (s Service) WithRegistration(mode RegistrationMode, invitations MemberInvitations) Service {
	s.mode = mode
	s.invitation = invitations
	return s
}

// Mode возвращает действующий режим; пустое значение приравнено к закрытому.
func (s Service) Mode() RegistrationMode {
	if s.mode == RegistrationOpen {
		return RegistrationOpen
	}
	return RegistrationInviteOnly
}

func NewService(repository domain.Repository, limiter RateLimiter, passwords Passwords, ids IDs, tokens Tokens, now func() time.Time, sessionTTL time.Duration) Service {
	return Service{repository: repository, limiter: limiter, passwords: passwords, ids: ids, tokens: tokens, now: now, sessionTTL: sessionTTL}
}

type admission int

const (
	admissionNone admission = iota
	admissionPlatform
	admissionMember
)

// Register создаёт учётную запись. Код приглашения обрабатывается в обоих режимах, но обязателен только
// в invite_only; недействительный код отвечает одинаково при любой причине (ADR 0057).
func (s Service) Register(ctx context.Context, input RegistrationInput, client Client) (Authenticated, error) {
	if err := s.ready(); err != nil {
		return Authenticated{}, ErrInvalidInput
	}
	if err := s.take(ctx, ScopeRegisterIP, client.IPAddress, registerIPLimit, registerIPWindow); err != nil {
		return Authenticated{}, err
	}
	if !validPassword(input.Password) {
		return Authenticated{}, ErrInvalidInput
	}
	normalizedEmail, err := domain.NormalizeEmail(input.Email)
	if err != nil {
		return Authenticated{}, ErrInvalidInput
	}
	now := s.now().UTC()
	code := strings.TrimSpace(input.InviteCode)
	kind, codeHash, err := s.admit(ctx, code, now)
	if err != nil {
		return Authenticated{}, err
	}
	passwordHash, err := s.passwords.Hash(input.Password)
	if err != nil {
		return Authenticated{}, err
	}
	userID, err := s.ids.NewID()
	if err != nil {
		return Authenticated{}, err
	}
	user, err := domain.NewUser(userID, normalizedEmail, passwordHash, input.DisplayName, now)
	if err != nil {
		return Authenticated{}, ErrInvalidInput
	}
	session, token, err := s.newSession(user.ID, now, client)
	if err != nil {
		return Authenticated{}, err
	}
	switch kind {
	case admissionPlatform:
		user.OrganizationRights = 1
		err = s.repository.CreateUserSessionWithPlatformInvitation(ctx, user, session, codeHash, now)
	default:
		err = s.repository.CreateUserSession(ctx, user, session)
	}
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConflict):
			return Authenticated{}, ErrEmailConflict
		case errors.Is(err, domain.ErrInvitationInvalid):
			return Authenticated{}, ErrInvitationInvalid
		}
		return Authenticated{}, err
	}
	if kind == admissionMember {
		if err := s.invitation.AcceptMemberInvitation(ctx, user.ID, code); err != nil {
			if !errors.Is(err, ErrInvitationInvalid) {
				// Исход приёма неизвестен: пользователя не удаляем, чтобы не потерять уже принятое членство.
				return Authenticated{}, err
			}
			if removeErr := s.repository.DeleteNewUser(ctx, user.ID); removeErr != nil {
				return Authenticated{}, removeErr
			}
			return Authenticated{}, ErrInvitationInvalid
		}
	}
	if err := s.auditAuth(ctx, user.ID, "USER_REGISTERED", client, s.now().UTC()); err != nil {
		return Authenticated{}, err
	}
	// Письмо подтверждения мягкое (ADR 0058): сбой не отменяет регистрацию, пользователь запросит письмо
	// заново из интерфейса.
	if s.mail.ready() {
		if err := s.sendEmailVerification(ctx, user, now, client); err != nil {
			s.report("identity.verification_failed", mailKindEmailVerification, err)
		}
	}
	return Authenticated{User: user, Token: token}, nil
}

// admit определяет вид кода, не расходуя его. Пустой код допустим только в open; код сотрудника
// принимается лишь при подключённом контракте.
func (s Service) admit(ctx context.Context, code string, now time.Time) (admission, string, error) {
	if code == "" {
		if s.Mode() != RegistrationOpen {
			return admissionNone, "", ErrInvitationRequired
		}
		return admissionNone, "", nil
	}
	if !invitationCodePattern.MatchString(code) {
		return admissionNone, "", ErrInvitationInvalid
	}
	codeHash := s.tokens.HashToken(code)
	usable, err := s.repository.PlatformInvitationUsable(ctx, codeHash, now)
	if err != nil {
		return admissionNone, "", err
	}
	if usable {
		return admissionPlatform, codeHash, nil
	}
	if s.invitation == nil {
		return admissionNone, "", ErrInvitationInvalid
	}
	if err := s.invitation.CheckMemberInvitation(ctx, code); err != nil {
		return admissionNone, "", err
	}
	return admissionMember, codeHash, nil
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
	if err := s.auditAuth(ctx, user.ID, "USER_LOGGED_IN", client, now); err != nil {
		return Authenticated{}, err
	}
	return Authenticated{User: user, Token: token}, nil
}

func (s Service) auditAuth(ctx context.Context, userID, operation string, client Client, at time.Time) error {
	if s.auditor == nil {
		return nil
	}
	return s.auditor.Auth(ctx, auditapplication.AuthEvent(userID, operation, client.IPAddress, at))
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
	now := s.now().UTC()
	// Пользователь определяется до отзыва: запись аудита выхода нуждается в нём.
	user, found, err := s.repository.UserBySessionHash(ctx, s.tokens.HashToken(token), now)
	if err != nil {
		return err
	}
	if err := s.repository.RevokeSession(ctx, s.tokens.HashToken(token), now); err != nil {
		return err
	}
	if !found {
		return nil
	}
	return s.auditAuth(ctx, user.ID, "USER_LOGGED_OUT", Client{}, now)
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
