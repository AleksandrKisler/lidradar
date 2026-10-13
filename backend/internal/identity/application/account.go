package application

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"lidradar/backend/internal/identity/domain"
)

// Команды безопасности учётной записи (ADR 0058): смена и сброс пароля, подтверждение почты.

var (
	// ErrInvalidCurrentPassword: текущий пароль не подошёл или уже сменён из другого окна. Это не 401:
	// клиент считает 401 потерей сессии.
	ErrInvalidCurrentPassword = errors.New("current password is invalid")
	// ErrResetTokenInvalid и ErrVerificationTokenInvalid: токен неизвестен, просрочен, использован или
	// отозван; причина не различается.
	ErrResetTokenInvalid        = errors.New("password reset token cannot be used")
	ErrVerificationTokenInvalid = errors.New("email verification token cannot be used")
	// ErrMailUnavailable: почта не настроена, письмо поставить некуда.
	ErrMailUnavailable = errors.New("mail is not configured")
)

const (
	passwordResetTTL     = 60 * time.Minute
	emailVerificationTTL = 7 * 24 * time.Hour
	// noticeTTL — сколько письмо «пароль изменён» остаётся достойным отправки.
	noticeTTL = 24 * time.Hour

	// Ссылки несут токен во фрагменте: он не уходит на сервер и не попадает в журналы прокси.
	resetPasswordPath = "/reset-password#token="
	verifyEmailPath   = "/verify-email#token="

	operationPasswordChanged        = "PASSWORD_CHANGED"
	operationPasswordResetRequested = "PASSWORD_RESET_REQUESTED"
	operationPasswordResetCompleted = "PASSWORD_RESET_COMPLETED"
	operationEmailVerified          = "EMAIL_VERIFIED"
)

// accountTokenPattern — формат токенов: 256 бит в base64url без дополнения.
var accountTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// Mailer ставит письмо в очередь отправки. Контракт реализует модуль mail; ошибка означает, что письмо
// в очередь не попало.
type Mailer interface {
	Enqueue(ctx context.Context, kind, to, subject, text string, expiresAt time.Time) error
}

// Reporter получает сбои, которые не меняют ответ пользователю (письмо не поставлено, токен не выпущен).
// *slog.Logger его реализует.
type Reporter interface {
	Warn(message string, arguments ...any)
}

type mailSettings struct {
	mailer   Mailer
	baseURL  string
	reporter Reporter
}

// ready: письма можно ставить, только если есть очередь и адрес веб-клиента для ссылок.
func (m mailSettings) ready() bool { return m.mailer != nil && m.baseURL != "" }

// WithMail подключает очередь писем и адрес веб-клиента, из которого строятся ссылки. Без вызова письма не
// отправляются: сброс пароля отвечает как обычно, но писать некому, подтверждение почты недоступно.
func (s Service) WithMail(mailer Mailer, webBaseURL string, reporter Reporter) Service {
	s.mail = mailSettings{mailer: mailer, baseURL: strings.TrimRight(strings.TrimSpace(webBaseURL), "/"), reporter: reporter}
	return s
}

// ChangePassword меняет пароль вошедшего пользователя. Все остальные сессии завершаются, текущая остаётся.
func (s Service) ChangePassword(ctx context.Context, sessionToken, currentPassword, newPassword string, client Client) error {
	if err := s.ready(); err != nil {
		return ErrInvalidInput
	}
	user, err := s.Authenticate(ctx, sessionToken)
	if err != nil {
		return err
	}
	if !validPassword(newPassword) || currentPassword == "" || len(currentPassword) > maximumPasswordLength || newPassword == currentPassword {
		return ErrInvalidInput
	}
	// Предел расходуется до проверки текущего пароля: он защищает его от подбора украденной сессией. Ошибки
	// формата предел не тратят, потому что пароль при них не проверяется.
	subject := rateSubject(user.ID)
	if err := s.takeSubject(ctx, ScopePasswordChangeAccount, subject, passwordChangeAccountLimit, passwordChangeWindow); err != nil {
		return err
	}
	valid, err := s.passwords.Verify(currentPassword, user.PasswordHash)
	if err != nil {
		return err
	}
	if !valid {
		return ErrInvalidCurrentPassword
	}
	newHash, err := s.passwords.Hash(newPassword)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	audit, err := s.auditRecord(operationPasswordChanged, client, now)
	if err != nil {
		return err
	}
	err = s.repository.ChangePassword(ctx, domain.PasswordChange{
		UserID: user.ID, PreviousHash: user.PasswordHash, NewHash: newHash,
		KeepSessionHash: s.tokens.HashToken(sessionToken), At: now, Audit: audit,
	})
	switch {
	case errors.Is(err, domain.ErrConflict), errors.Is(err, domain.ErrNotFound):
		// Пароль сменили из другого окна или пользователя отключили: прежнее подтверждение недействительно.
		return ErrInvalidCurrentPassword
	case err != nil:
		return err
	}
	if err := s.limiter.Reset(ctx, ScopePasswordChangeAccount, subject); err != nil {
		return err
	}
	s.sendPasswordChangedNotice(ctx, user.Email, now)
	return nil
}

// RequestPasswordReset ставит в очередь письмо со ссылкой сброса. Ответ не зависит от того, есть ли адрес:
// пределы расходуются до поиска пользователя, а ошибки после него только записываются в журнал.
func (s Service) RequestPasswordReset(ctx context.Context, email string, client Client) error {
	if err := s.ready(); err != nil {
		return ErrInvalidInput
	}
	if err := s.take(ctx, ScopePasswordResetIP, client.IPAddress, passwordResetIPLimit, passwordResetWindow); err != nil {
		return err
	}
	if err := s.takeSubject(ctx, ScopePasswordResetAccount, rateSubject(email), passwordResetAccountLimit, passwordResetWindow); err != nil {
		return err
	}
	normalized, err := domain.NormalizeEmail(email)
	if err != nil {
		return nil
	}
	user, found, err := s.repository.UserByEmail(ctx, normalized)
	if err != nil {
		return err
	}
	if !found || user.Status != domain.UserActive {
		return nil
	}
	if !s.mail.ready() {
		s.report("identity.password_reset_unavailable", mailKindPasswordReset, ErrMailUnavailable)
		return nil
	}
	now := s.now().UTC()
	token, err := s.issueToken(ctx, user, domain.TokenPasswordReset, passwordResetTTL, operationPasswordResetRequested, client, now)
	if err != nil {
		s.report("identity.password_reset_token_failed", mailKindPasswordReset, err)
		return nil
	}
	subject, text := passwordResetMail(s.mail.baseURL + resetPasswordPath + token)
	if err := s.mail.mailer.Enqueue(ctx, mailKindPasswordReset, user.Email, subject, text, now.Add(passwordResetTTL)); err != nil {
		s.report("identity.password_reset_enqueue_failed", mailKindPasswordReset, err)
	}
	return nil
}

// ResetPassword завершает сброс по токену из письма. Все сессии пользователя завершаются, входа не выполняется.
func (s Service) ResetPassword(ctx context.Context, token, newPassword string, client Client) error {
	if err := s.ready(); err != nil {
		return ErrInvalidInput
	}
	if err := s.take(ctx, ScopePasswordResetConfirmIP, client.IPAddress, passwordResetConfirmLimit, passwordResetWindow); err != nil {
		return err
	}
	if !validPassword(newPassword) {
		return ErrInvalidInput
	}
	token = strings.TrimSpace(token)
	if !accountTokenPattern.MatchString(token) {
		return ErrResetTokenInvalid
	}
	digest := s.tokens.HashToken(token)
	// Дорогое хеширование пароля только для токена, который ещё можно погасить: подбор негодных токенов
	// не нагружает процессор.
	usable, err := s.repository.AccountTokenUsable(ctx, domain.TokenPasswordReset, digest, s.now().UTC())
	if err != nil {
		return err
	}
	if !usable {
		return ErrResetTokenInvalid
	}
	newHash, err := s.passwords.Hash(newPassword)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	audit, err := s.auditRecord(operationPasswordResetCompleted, client, now)
	if err != nil {
		return err
	}
	email, err := s.repository.CompletePasswordReset(ctx, domain.PasswordReset{TokenHash: digest, NewHash: newHash, At: now, Audit: audit})
	switch {
	case errors.Is(err, domain.ErrTokenInvalid):
		return ErrResetTokenInvalid
	case err != nil:
		return err
	}
	s.sendPasswordChangedNotice(ctx, email, now)
	return nil
}

// RequestEmailVerification отправляет письмо подтверждения вошедшему пользователю. Уже подтверждённый адрес
// не получает письма и не расходует предел.
func (s Service) RequestEmailVerification(ctx context.Context, sessionToken string, client Client) error {
	if err := s.ready(); err != nil {
		return ErrInvalidInput
	}
	user, err := s.Authenticate(ctx, sessionToken)
	if err != nil {
		return err
	}
	if user.EmailVerifiedAt != nil {
		return nil
	}
	if !s.mail.ready() {
		return ErrMailUnavailable
	}
	if err := s.takeSubject(ctx, ScopeEmailVerificationAccount, rateSubject(user.ID), emailVerificationAccountLimit, emailVerificationWindow); err != nil {
		return err
	}
	return s.sendEmailVerification(ctx, user, s.now().UTC(), client)
}

// VerifyEmail подтверждает адрес по токену из письма; сессия не требуется, ссылку открывают в любом браузере.
func (s Service) VerifyEmail(ctx context.Context, token string, client Client) error {
	if err := s.ready(); err != nil {
		return ErrInvalidInput
	}
	if err := s.take(ctx, ScopeEmailVerifyIP, client.IPAddress, emailVerifyIPLimit, emailVerificationWindow); err != nil {
		return err
	}
	token = strings.TrimSpace(token)
	if !accountTokenPattern.MatchString(token) {
		return ErrVerificationTokenInvalid
	}
	now := s.now().UTC()
	audit, err := s.auditRecord(operationEmailVerified, client, now)
	if err != nil {
		return err
	}
	err = s.repository.CompleteEmailVerification(ctx, s.tokens.HashToken(token), now, audit)
	switch {
	case errors.Is(err, domain.ErrTokenInvalid):
		return ErrVerificationTokenInvalid
	case err != nil:
		return err
	}
	return nil
}

// sendEmailVerification выпускает токен подтверждения и ставит письмо в очередь.
func (s Service) sendEmailVerification(ctx context.Context, user domain.User, now time.Time, client Client) error {
	token, err := s.issueToken(ctx, user, domain.TokenEmailVerification, emailVerificationTTL, "", client, now)
	if err != nil {
		return err
	}
	subject, text := emailVerificationMail(s.mail.baseURL + verifyEmailPath + token)
	return s.mail.mailer.Enqueue(ctx, mailKindEmailVerification, user.Email, subject, text, now.Add(emailVerificationTTL))
}

// sendPasswordChangedNotice ставит уведомление о смене пароля; сбой не отменяет уже выполненную смену.
func (s Service) sendPasswordChangedNotice(ctx context.Context, email string, now time.Time) {
	if !s.mail.ready() {
		return
	}
	subject, text := passwordChangedMail()
	if err := s.mail.mailer.Enqueue(ctx, mailKindPasswordChanged, email, subject, text, now.Add(noticeTTL)); err != nil {
		s.report("identity.password_notice_enqueue_failed", mailKindPasswordChanged, err)
	}
}

// issueToken создаёт токен, записывает его хэш (с аудитом, если задана операция) и возвращает открытое значение.
func (s Service) issueToken(ctx context.Context, user domain.User, purpose domain.TokenPurpose, ttl time.Duration, auditOperation string, client Client, now time.Time) (string, error) {
	plaintext, digest, err := s.tokens.NewToken()
	if err != nil {
		return "", err
	}
	id, err := s.ids.NewID()
	if err != nil {
		return "", err
	}
	token, err := domain.NewAccountToken(id, user.ID, purpose, digest, now, now.Add(ttl))
	if err != nil {
		return "", err
	}
	var audit *domain.AuditRecord
	if auditOperation != "" {
		record, err := s.auditRecord(auditOperation, client, now)
		if err != nil {
			return "", err
		}
		audit = &record
	}
	if err := s.repository.IssueAccountToken(ctx, token, audit); err != nil {
		return "", err
	}
	return plaintext, nil
}

func (s Service) auditRecord(operation string, client Client, at time.Time) (domain.AuditRecord, error) {
	id, err := s.ids.NewID()
	if err != nil {
		return domain.AuditRecord{}, err
	}
	return domain.NewAuditRecord(id, operation, client.IPAddress, at)
}

func (s Service) report(event, kind string, err error) {
	if s.mail.reporter == nil {
		return
	}
	s.mail.reporter.Warn("Сбой письма учётной записи", "event", event, "kind", kind, "error", err)
}
