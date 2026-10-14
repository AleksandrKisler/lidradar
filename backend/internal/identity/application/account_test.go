package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/identity/domain"
)

// Тесты команд безопасности учётной записи (ADR 0058) на памяти: смена и сброс пароля, подтверждение
// почты, пределы и однотипные ответы. Поведение PostgreSQL проверяют тесты хранилища.

type memoryToken struct {
	domain.AccountToken
	usedAt    *time.Time
	revokedAt *time.Time
}

type memoryAudit struct {
	UserID string
	domain.AuditRecord
}

func (r *memoryRepository) userByID(id string) (string, domain.User, bool) {
	for email, user := range r.users {
		if user.ID == id {
			return email, user, true
		}
	}
	return "", domain.User{}, false
}

func (r *memoryRepository) revokeSessions(userID string, at time.Time, keep string) {
	for hash, session := range r.sessions {
		if session.UserID == userID && session.RevokedAt == nil && hash != keep {
			revoked := at
			session.RevokedAt = &revoked
			r.sessions[hash] = session
		}
	}
}

func (r *memoryRepository) revokeResetTokens(userID string, at time.Time) {
	for _, token := range r.tokens {
		if token.UserID == userID && token.Purpose == domain.TokenPasswordReset && token.usedAt == nil && token.revokedAt == nil {
			revoked := at
			token.revokedAt = &revoked
		}
	}
}

func (r *memoryRepository) ChangePassword(_ context.Context, change domain.PasswordChange) error {
	if r.beforeChange != nil {
		r.beforeChange()
	}
	email, user, ok := r.userByID(change.UserID)
	if !ok || user.Status != domain.UserActive {
		return domain.ErrNotFound
	}
	if user.PasswordHash != change.PreviousHash {
		return domain.ErrConflict
	}
	user.PasswordHash, user.UpdatedAt = change.NewHash, change.At
	r.users[email] = user
	r.revokeSessions(user.ID, change.At, change.KeepSessionHash)
	r.revokeResetTokens(user.ID, change.At)
	r.audits = append(r.audits, memoryAudit{UserID: user.ID, AuditRecord: change.Audit})
	return nil
}

func (r *memoryRepository) IssueAccountToken(_ context.Context, token domain.AccountToken, audit *domain.AuditRecord) error {
	if _, _, ok := r.userByID(token.UserID); !ok {
		return domain.ErrNotFound
	}
	for _, existing := range r.tokens {
		if existing.UserID == token.UserID && existing.Purpose == token.Purpose && existing.usedAt == nil && existing.revokedAt == nil {
			revoked := token.CreatedAt
			existing.revokedAt = &revoked
		}
	}
	r.tokens[token.TokenHash] = &memoryToken{AccountToken: token}
	if audit != nil {
		r.audits = append(r.audits, memoryAudit{UserID: token.UserID, AuditRecord: *audit})
	}
	return nil
}

func (r *memoryRepository) usableToken(purpose domain.TokenPurpose, hash string, at time.Time) (*memoryToken, bool) {
	token, ok := r.tokens[hash]
	if !ok || token.Purpose != purpose || token.usedAt != nil || token.revokedAt != nil || !token.ExpiresAt.After(at) {
		return nil, false
	}
	if _, user, found := r.userByID(token.UserID); !found || user.Status != domain.UserActive {
		return nil, false
	}
	return token, true
}

func (r *memoryRepository) AccountTokenUsable(_ context.Context, purpose domain.TokenPurpose, hash string, at time.Time) (bool, error) {
	_, ok := r.usableToken(purpose, hash, at)
	return ok, nil
}

func (r *memoryRepository) CompletePasswordReset(_ context.Context, reset domain.PasswordReset) (string, error) {
	token, ok := r.usableToken(domain.TokenPasswordReset, reset.TokenHash, reset.At)
	if !ok {
		return "", domain.ErrTokenInvalid
	}
	email, user, _ := r.userByID(token.UserID)
	user.PasswordHash, user.UpdatedAt = reset.NewHash, reset.At
	if user.EmailVerifiedAt == nil {
		verified := reset.At
		user.EmailVerifiedAt = &verified
	}
	r.users[email] = user
	used := reset.At
	token.usedAt = &used
	r.revokeResetTokens(user.ID, reset.At)
	r.revokeSessions(user.ID, reset.At, "")
	r.audits = append(r.audits, memoryAudit{UserID: user.ID, AuditRecord: reset.Audit})
	return email, nil
}

func (r *memoryRepository) CompleteEmailVerification(_ context.Context, hash string, at time.Time, audit domain.AuditRecord) error {
	token, ok := r.usableToken(domain.TokenEmailVerification, hash, at)
	if !ok {
		return domain.ErrTokenInvalid
	}
	email, user, _ := r.userByID(token.UserID)
	used := at
	token.usedAt = &used
	if user.EmailVerifiedAt == nil {
		verified := at
		user.EmailVerifiedAt = &verified
		r.users[email] = user
		r.audits = append(r.audits, memoryAudit{UserID: user.ID, AuditRecord: audit})
	}
	return nil
}

func (r *memoryRepository) auditOperations() []string {
	operations := make([]string, 0, len(r.audits))
	for _, audit := range r.audits {
		operations = append(operations, audit.Operation)
	}
	return operations
}

// longTokens выдают токены формата боевых (43 символа), иначе проверка формата отклонила бы их.
type longTokens struct{ testTokens }

func (t *longTokens) NewToken() (string, string, error) {
	t.next++
	value := fmt.Sprintf("tok%040d", t.next)
	return value, t.HashToken(value), nil
}

type sentMail struct {
	Kind, To, Subject, Text string
	ExpiresAt               time.Time
}

type fakeMailer struct {
	mails []sentMail
	err   error
}

func (m *fakeMailer) Enqueue(_ context.Context, kind, to, subject, text string, expiresAt time.Time) error {
	if m.err != nil {
		return m.err
	}
	m.mails = append(m.mails, sentMail{Kind: kind, To: to, Subject: subject, Text: text, ExpiresAt: expiresAt})
	return nil
}

func (m *fakeMailer) last(t *testing.T) sentMail {
	t.Helper()
	if len(m.mails) == 0 {
		t.Fatal("no mail was queued")
	}
	return m.mails[len(m.mails)-1]
}

type fakeReporter struct{ events []string }

func (r *fakeReporter) Warn(_ string, arguments ...any) {
	for index := 0; index+1 < len(arguments); index += 2 {
		if arguments[index] == "event" {
			r.events = append(r.events, fmt.Sprint(arguments[index+1]))
		}
	}
}

const (
	accountBaseURL = "https://app.example.test"
	accountPass    = "very-secure-password"
	accountNewPass = "another-secure-password"
)

type accountFixture struct {
	service  Service
	repo     *memoryRepository
	mailer   *fakeMailer
	reporter *fakeReporter
	now      time.Time
}

func newAccountFixture() *accountFixture {
	fixture := &accountFixture{
		repo: newMemoryRepository(), mailer: &fakeMailer{}, reporter: &fakeReporter{},
		now: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC),
	}
	fixture.service = NewService(
		fixture.repo, newTestRateLimiter(), testPasswords{}, &testIDs{}, &longTokens{},
		func() time.Time { return fixture.now }, 24*time.Hour,
	).WithRegistration(RegistrationOpen, nil).WithMail(fixture.mailer, accountBaseURL+"/", fixture.reporter)
	return fixture
}

func (f *accountFixture) register(t *testing.T, email string) Authenticated {
	t.Helper()
	authenticated, err := f.service.Register(context.Background(), RegistrationInput{Email: email, Password: accountPass, DisplayName: "Owner"}, Client{IPAddress: "192.0.2.1"})
	if err != nil {
		t.Fatalf("Register(%s) error = %v", email, err)
	}
	return authenticated
}

func (f *accountFixture) login(t *testing.T, email, password string) (Authenticated, error) {
	t.Helper()
	return f.service.Login(context.Background(), email, password, Client{IPAddress: "192.0.2.1"})
}

func tokenFromMail(t *testing.T, mail sentMail) string {
	t.Helper()
	_, after, found := strings.Cut(mail.Text, "#token=")
	if !found {
		t.Fatalf("mail has no token link: %q", mail.Text)
	}
	token, _, _ := strings.Cut(after, "\n")
	return strings.TrimSpace(token)
}

func TestChangePasswordRevokesOtherSessionsAndKeepsCurrent(t *testing.T) {
	f := newAccountFixture()
	registered := f.register(t, "owner@example.com")
	other, err := f.login(t, "owner@example.com", accountPass)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.RequestPasswordReset(context.Background(), "owner@example.com", Client{IPAddress: "192.0.2.2"}); err != nil {
		t.Fatal(err)
	}
	resetToken := tokenFromMail(t, f.mailer.last(t))
	f.mailer.mails = nil

	if err := f.service.ChangePassword(context.Background(), registered.Token, accountPass, accountNewPass, Client{IPAddress: "192.0.2.1"}); err != nil {
		t.Fatalf("ChangePassword() error = %v", err)
	}

	if _, err := f.service.Authenticate(context.Background(), registered.Token); err != nil {
		t.Fatalf("current session must stay valid: %v", err)
	}
	if _, err := f.service.Authenticate(context.Background(), other.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("other session must be revoked, got %v", err)
	}
	if _, err := f.login(t, "owner@example.com", accountPass); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password must stop working, got %v", err)
	}
	if _, err := f.login(t, "owner@example.com", accountNewPass); err != nil {
		t.Fatalf("new password must work: %v", err)
	}
	if err := f.service.ResetPassword(context.Background(), resetToken, "yet-another-password", Client{IPAddress: "192.0.2.3"}); !errors.Is(err, ErrResetTokenInvalid) {
		t.Fatalf("pending reset token must be revoked by a password change, got %v", err)
	}
	notice := f.mailer.last(t)
	if notice.Kind != mailKindPasswordChanged || notice.To != "owner@example.com" || strings.Contains(notice.Text, "#token=") {
		t.Fatalf("notice = %#v", notice)
	}
	if got := f.repo.auditOperations(); got[len(got)-1] != "PASSWORD_CHANGED" {
		t.Fatalf("audit = %v", got)
	}
}

func TestChangePasswordRejectsWrongCurrentPasswordAndLimitsAttempts(t *testing.T) {
	f := newAccountFixture()
	registered := f.register(t, "owner@example.com")
	client := Client{IPAddress: "192.0.2.1"}
	for attempt := 1; attempt <= passwordChangeAccountLimit; attempt++ {
		if err := f.service.ChangePassword(context.Background(), registered.Token, "wrong-current-password", accountNewPass, client); !errors.Is(err, ErrInvalidCurrentPassword) {
			t.Fatalf("attempt %d error = %v", attempt, err)
		}
	}
	// Предел расходуется до проверки: подбор текущего пароля украденной сессией упирается в него.
	err := f.service.ChangePassword(context.Background(), registered.Token, accountPass, accountNewPass, client)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("limited attempt error = %v", err)
	}
	if _, loginErr := f.login(t, "owner@example.com", accountPass); loginErr != nil {
		t.Fatalf("password must be unchanged: %v", loginErr)
	}
	f.now = f.now.Add(passwordChangeWindow)
	if err := f.service.ChangePassword(context.Background(), registered.Token, accountPass, accountNewPass, client); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestChangePasswordSuccessResetsTheAttemptCounter(t *testing.T) {
	f := newAccountFixture()
	registered := f.register(t, "owner@example.com")
	client := Client{IPAddress: "192.0.2.1"}
	for attempt := 1; attempt < passwordChangeAccountLimit; attempt++ {
		_ = f.service.ChangePassword(context.Background(), registered.Token, "wrong-current-password", accountNewPass, client)
	}
	if err := f.service.ChangePassword(context.Background(), registered.Token, accountPass, accountNewPass, client); err != nil {
		t.Fatal(err)
	}
	// Успех сбросил счётчик: снова доступно полное число попыток.
	for attempt := 1; attempt <= passwordChangeAccountLimit; attempt++ {
		if err := f.service.ChangePassword(context.Background(), registered.Token, "wrong-current-password", "third-secure-password", client); !errors.Is(err, ErrInvalidCurrentPassword) {
			t.Fatalf("attempt %d error = %v", attempt, err)
		}
	}
}

func TestChangePasswordRejectsInvalidInputWithoutChangingAnything(t *testing.T) {
	f := newAccountFixture()
	registered := f.register(t, "owner@example.com")
	client := Client{IPAddress: "192.0.2.1"}
	cases := map[string][2]string{
		"short new password":   {accountPass, "short"},
		"same password":        {accountPass, accountPass},
		"empty current":        {"", accountNewPass},
		"oversized new":        {accountPass, strings.Repeat("a", maximumPasswordLength+1)},
		"oversized current":    {strings.Repeat("a", maximumPasswordLength+1), accountNewPass},
		"whitespace only new":  {accountPass, "   "},
		"eleven byte password": {accountPass, "12345678901"},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if err := f.service.ChangePassword(context.Background(), registered.Token, input[0], input[1], client); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
		})
	}
	if _, err := f.login(t, "owner@example.com", accountPass); err != nil {
		t.Fatalf("password must be unchanged: %v", err)
	}
	if len(f.repo.audits) != 0 {
		t.Fatalf("rejected input must not be audited as a change: %v", f.repo.auditOperations())
	}
	// Ошибки формата не тратят попытки: пароль при них не проверялся, подбирать нечего.
	for attempt := 1; attempt <= passwordChangeAccountLimit; attempt++ {
		if err := f.service.ChangePassword(context.Background(), registered.Token, "wrong-current-password", accountNewPass, client); !errors.Is(err, ErrInvalidCurrentPassword) {
			t.Fatalf("attempt %d after invalid input: error = %v", attempt, err)
		}
	}
}

func TestChangePasswordParallelChangeInvalidatesTheProof(t *testing.T) {
	f := newAccountFixture()
	registered := f.register(t, "owner@example.com")
	// Между проверкой пароля и записью пароль успевают сменить из другого окна.
	f.repo.beforeChange = func() {
		user := f.repo.users["owner@example.com"]
		user.PasswordHash = "hashed:changed-in-another-window"
		f.repo.users["owner@example.com"] = user
	}
	err := f.service.ChangePassword(context.Background(), registered.Token, accountPass, accountNewPass, Client{})
	if !errors.Is(err, ErrInvalidCurrentPassword) {
		t.Fatalf("error = %v, want ErrInvalidCurrentPassword", err)
	}
	if got := f.repo.users["owner@example.com"].PasswordHash; got != "hashed:changed-in-another-window" {
		t.Fatalf("stored hash = %q: the stale change must not overwrite", got)
	}
}

func TestChangePasswordRequiresSession(t *testing.T) {
	f := newAccountFixture()
	f.register(t, "owner@example.com")
	for _, token := range []string{"", "unknown-session"} {
		if err := f.service.ChangePassword(context.Background(), token, accountPass, accountNewPass, Client{}); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("token %q: error = %v", token, err)
		}
	}
}

func TestChangePasswordSucceedsWhenTheNoticeCannotBeQueued(t *testing.T) {
	f := newAccountFixture()
	registered := f.register(t, "owner@example.com")
	f.mailer.err = errors.New("queue unavailable")
	if err := f.service.ChangePassword(context.Background(), registered.Token, accountPass, accountNewPass, Client{}); err != nil {
		t.Fatalf("a missing notice must not undo the change: %v", err)
	}
	if _, err := f.login(t, "owner@example.com", accountNewPass); err != nil {
		t.Fatalf("new password must work: %v", err)
	}
	if len(f.reporter.events) == 0 {
		t.Fatal("the failure must be reported")
	}
}

func TestRequestPasswordResetAnswersTheSameForEveryAddress(t *testing.T) {
	f := newAccountFixture()
	f.register(t, "known@example.com")
	disabled := f.register(t, "disabled@example.com")
	user := f.repo.users["disabled@example.com"]
	user.Status = domain.UserDisabled
	f.repo.users["disabled@example.com"] = user
	_ = disabled
	f.mailer.mails = nil
	tokensBefore := len(f.repo.tokens)

	for index, email := range []string{"unknown@example.com", "disabled@example.com", "not an address", "", strings.Repeat("a", 300) + "@example.com"} {
		client := Client{IPAddress: fmt.Sprintf("192.0.2.%d", 10+index)}
		if err := f.service.RequestPasswordReset(context.Background(), email, client); err != nil {
			t.Fatalf("RequestPasswordReset(%q) error = %v: the answer must not depend on the address", email, err)
		}
	}
	if len(f.mailer.mails) != 0 || len(f.repo.tokens) != tokensBefore {
		t.Fatalf("no mail or token may be created for unknown, disabled or malformed addresses: mails=%d tokens=%d", len(f.mailer.mails), len(f.repo.tokens)-tokensBefore)
	}

	if err := f.service.RequestPasswordReset(context.Background(), "known@example.com", Client{IPAddress: "192.0.2.50"}); err != nil {
		t.Fatalf("known address error = %v", err)
	}
	if len(f.mailer.mails) != 1 {
		t.Fatalf("known address must get exactly one mail, got %d", len(f.mailer.mails))
	}
}

func TestRequestPasswordResetIssuesTokenAndMail(t *testing.T) {
	f := newAccountFixture()
	f.register(t, "Owner@Example.com")
	f.mailer.mails = nil

	if err := f.service.RequestPasswordReset(context.Background(), " OWNER@example.com ", Client{IPAddress: "192.0.2.9"}); err != nil {
		t.Fatal(err)
	}
	mail := f.mailer.last(t)
	token := tokenFromMail(t, mail)
	if mail.Kind != mailKindPasswordReset || mail.To != "owner@example.com" {
		t.Fatalf("mail = %#v", mail)
	}
	if !strings.Contains(mail.Text, accountBaseURL+resetPasswordPath+token) || strings.Contains(mail.Text, accountBaseURL+"//") {
		t.Fatalf("link is malformed: %q", mail.Text)
	}
	if !mail.ExpiresAt.Equal(f.now.Add(passwordResetTTL)) {
		t.Fatalf("mail expires at %s, want %s", mail.ExpiresAt, f.now.Add(passwordResetTTL))
	}
	for hash, stored := range f.repo.tokens {
		if hash == token || stored.TokenHash == token {
			t.Fatal("the plaintext token must not be stored")
		}
	}
	if got := f.repo.auditOperations(); got[len(got)-1] != "PASSWORD_RESET_REQUESTED" {
		t.Fatalf("audit = %v", got)
	}
}

func TestRequestPasswordResetRevokesThePreviousToken(t *testing.T) {
	f := newAccountFixture()
	f.register(t, "owner@example.com")
	f.mailer.mails = nil
	for index := 0; index < 2; index++ {
		if err := f.service.RequestPasswordReset(context.Background(), "owner@example.com", Client{IPAddress: fmt.Sprintf("192.0.2.%d", 20+index)}); err != nil {
			t.Fatal(err)
		}
	}
	first, second := tokenFromMail(t, f.mailer.mails[0]), tokenFromMail(t, f.mailer.mails[1])
	if err := f.service.ResetPassword(context.Background(), first, accountNewPass, Client{IPAddress: "192.0.2.30"}); !errors.Is(err, ErrResetTokenInvalid) {
		t.Fatalf("the first link must be dead after a newer request, got %v", err)
	}
	if err := f.service.ResetPassword(context.Background(), second, accountNewPass, Client{IPAddress: "192.0.2.30"}); err != nil {
		t.Fatalf("the newest link must work: %v", err)
	}
}

func TestRequestPasswordResetLimitsPerAddressAndPerIPWithoutRevealingExistence(t *testing.T) {
	f := newAccountFixture()
	f.register(t, "known@example.com")
	for _, email := range []string{"known@example.com", "unknown@example.com"} {
		for attempt := 1; attempt <= passwordResetAccountLimit; attempt++ {
			client := Client{IPAddress: fmt.Sprintf("198.51.100.%d", attempt)}
			if err := f.service.RequestPasswordReset(context.Background(), email, client); err != nil {
				t.Fatalf("%s attempt %d: %v", email, attempt, err)
			}
		}
		err := f.service.RequestPasswordReset(context.Background(), email, Client{IPAddress: "198.51.100.99"})
		if !errors.Is(err, ErrRateLimited) {
			t.Fatalf("%s: the %dth request must be limited, got %v", email, passwordResetAccountLimit+1, err)
		}
		if retry := RateLimitRetryAfter(err); retry != passwordResetWindow {
			t.Fatalf("%s: retry after = %s", email, retry)
		}
	}

	ip := Client{IPAddress: "203.0.113.7"}
	for attempt := 1; attempt <= passwordResetIPLimit; attempt++ {
		if err := f.service.RequestPasswordReset(context.Background(), fmt.Sprintf("user%d@example.com", attempt), ip); err != nil {
			t.Fatalf("ip attempt %d: %v", attempt, err)
		}
	}
	if err := f.service.RequestPasswordReset(context.Background(), "another@example.com", ip); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("the IP limit must apply, got %v", err)
	}
}

func TestRequestPasswordResetSurvivesQueueFailureAndMissingMail(t *testing.T) {
	f := newAccountFixture()
	f.register(t, "owner@example.com")
	f.mailer.err = errors.New("queue unavailable")
	if err := f.service.RequestPasswordReset(context.Background(), "owner@example.com", Client{IPAddress: "192.0.2.5"}); err != nil {
		t.Fatalf("a queue failure must not change the answer: %v", err)
	}
	if len(f.reporter.events) != 1 || f.reporter.events[0] != "identity.password_reset_enqueue_failed" {
		t.Fatalf("events = %v", f.reporter.events)
	}

	bare := NewService(f.repo, newTestRateLimiter(), testPasswords{}, &testIDs{}, &longTokens{}, func() time.Time { return f.now }, time.Hour)
	tokensBefore := len(f.repo.tokens)
	if err := bare.RequestPasswordReset(context.Background(), "owner@example.com", Client{}); err != nil {
		t.Fatalf("without mail the answer stays the same: %v", err)
	}
	if len(f.repo.tokens) != tokensBefore {
		t.Fatal("no token may be issued when there is nowhere to send it")
	}
}

func TestResetPasswordCompletesAndRevokesEverySession(t *testing.T) {
	f := newAccountFixture()
	registered := f.register(t, "owner@example.com")
	second, err := f.login(t, "owner@example.com", accountPass)
	if err != nil {
		t.Fatal(err)
	}
	f.mailer.mails = nil
	if err := f.service.RequestPasswordReset(context.Background(), "owner@example.com", Client{IPAddress: "192.0.2.9"}); err != nil {
		t.Fatal(err)
	}
	token := tokenFromMail(t, f.mailer.last(t))
	f.mailer.mails = nil

	if err := f.service.ResetPassword(context.Background(), token, accountNewPass, Client{IPAddress: "192.0.2.9"}); err != nil {
		t.Fatalf("ResetPassword() error = %v", err)
	}
	for name, session := range map[string]string{"first": registered.Token, "second": second.Token} {
		if _, err := f.service.Authenticate(context.Background(), session); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("%s session must be revoked, got %v", name, err)
		}
	}
	if _, err := f.login(t, "owner@example.com", accountPass); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password must stop working, got %v", err)
	}
	if _, err := f.login(t, "owner@example.com", accountNewPass); err != nil {
		t.Fatalf("new password must work: %v", err)
	}
	if f.repo.users["owner@example.com"].EmailVerifiedAt == nil {
		t.Fatal("a reset link that reached the mailbox verifies the address")
	}
	if got := f.mailer.last(t); got.Kind != mailKindPasswordChanged || got.To != "owner@example.com" {
		t.Fatalf("notice = %#v", got)
	}
	if got := f.repo.auditOperations(); got[len(got)-1] != "PASSWORD_RESET_COMPLETED" {
		t.Fatalf("audit = %v", got)
	}
	// Токен одноразовый.
	if err := f.service.ResetPassword(context.Background(), token, "yet-another-password", Client{IPAddress: "192.0.2.9"}); !errors.Is(err, ErrResetTokenInvalid) {
		t.Fatalf("reused token error = %v", err)
	}
}

func TestResetPasswordRejectsEveryUnusableToken(t *testing.T) {
	f := newAccountFixture()
	f.register(t, "owner@example.com")
	f.mailer.mails = nil
	if err := f.service.RequestPasswordReset(context.Background(), "owner@example.com", Client{IPAddress: "192.0.2.9"}); err != nil {
		t.Fatal(err)
	}
	valid := tokenFromMail(t, f.mailer.last(t))

	client := Client{IPAddress: "192.0.2.77"}
	rejected := map[string]string{
		"unknown":   "x" + valid[1:],
		"malformed": "short",
		"empty":     "",
		"padded":    valid + "=",
	}
	for name, token := range rejected {
		if err := f.service.ResetPassword(context.Background(), token, accountNewPass, client); !errors.Is(err, ErrResetTokenInvalid) {
			t.Fatalf("%s token error = %v", name, err)
		}
	}

	// Просроченный токен.
	f.now = f.now.Add(passwordResetTTL)
	if err := f.service.ResetPassword(context.Background(), valid, accountNewPass, client); !errors.Is(err, ErrResetTokenInvalid) {
		t.Fatalf("expired token error = %v", err)
	}
	f.now = f.now.Add(-passwordResetTTL + time.Minute)
	// Отключённый пользователь.
	user := f.repo.users["owner@example.com"]
	user.Status = domain.UserDisabled
	f.repo.users["owner@example.com"] = user
	if err := f.service.ResetPassword(context.Background(), valid, accountNewPass, client); !errors.Is(err, ErrResetTokenInvalid) {
		t.Fatalf("disabled user token error = %v", err)
	}
	user.Status = domain.UserActive
	f.repo.users["owner@example.com"] = user
	if err := f.service.ResetPassword(context.Background(), valid, accountNewPass, client); err != nil {
		t.Fatalf("the token itself must still be good: %v", err)
	}
}

func TestResetPasswordWithWeakPasswordKeepsTheToken(t *testing.T) {
	f := newAccountFixture()
	f.register(t, "owner@example.com")
	f.mailer.mails = nil
	_ = f.service.RequestPasswordReset(context.Background(), "owner@example.com", Client{IPAddress: "192.0.2.9"})
	token := tokenFromMail(t, f.mailer.last(t))
	client := Client{IPAddress: "192.0.2.77"}
	if err := f.service.ResetPassword(context.Background(), token, "short", client); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("weak password error = %v", err)
	}
	if err := f.service.ResetPassword(context.Background(), token, accountNewPass, client); err != nil {
		t.Fatalf("a weak password must not burn the token: %v", err)
	}
}

func TestResetPasswordLimitsAttemptsPerIP(t *testing.T) {
	f := newAccountFixture()
	client := Client{IPAddress: "192.0.2.88"}
	bad := strings.Repeat("a", 43)
	for attempt := 1; attempt <= passwordResetConfirmLimit; attempt++ {
		if err := f.service.ResetPassword(context.Background(), bad, accountNewPass, client); !errors.Is(err, ErrResetTokenInvalid) {
			t.Fatalf("attempt %d error = %v", attempt, err)
		}
	}
	if err := f.service.ResetPassword(context.Background(), bad, accountNewPass, client); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("limited attempt error = %v", err)
	}
}

func TestAVerificationTokenCannotResetAPasswordAndViceVersa(t *testing.T) {
	f := newAccountFixture()
	f.register(t, "owner@example.com")
	verification := tokenFromMail(t, f.mailer.last(t))
	f.mailer.mails = nil
	_ = f.service.RequestPasswordReset(context.Background(), "owner@example.com", Client{IPAddress: "192.0.2.9"})
	reset := tokenFromMail(t, f.mailer.last(t))
	client := Client{IPAddress: "192.0.2.77"}

	if err := f.service.ResetPassword(context.Background(), verification, accountNewPass, client); !errors.Is(err, ErrResetTokenInvalid) {
		t.Fatalf("verification token reset the password: %v", err)
	}
	if err := f.service.VerifyEmail(context.Background(), reset, client); !errors.Is(err, ErrVerificationTokenInvalid) {
		t.Fatalf("reset token verified the address: %v", err)
	}
	if f.repo.users["owner@example.com"].EmailVerifiedAt != nil {
		t.Fatal("the address must stay unverified")
	}
}

func TestRegisterQueuesVerificationMailAndSurvivesQueueFailure(t *testing.T) {
	f := newAccountFixture()
	registered := f.register(t, "owner@example.com")
	mail := f.mailer.last(t)
	if mail.Kind != mailKindEmailVerification || mail.To != "owner@example.com" ||
		!strings.Contains(mail.Text, accountBaseURL+verifyEmailPath+tokenFromMail(t, mail)) ||
		!mail.ExpiresAt.Equal(f.now.Add(emailVerificationTTL)) {
		t.Fatalf("mail = %#v", mail)
	}
	if registered.User.EmailVerifiedAt != nil {
		t.Fatal("a new address is unverified")
	}
	// Подтверждение мягкое: регистрация не зависит от очереди.
	f.mailer.err = errors.New("queue unavailable")
	if _, err := f.service.Register(context.Background(), RegistrationInput{Email: "second@example.com", Password: accountPass, DisplayName: "Second"}, Client{IPAddress: "192.0.2.4"}); err != nil {
		t.Fatalf("registration must survive a queue failure: %v", err)
	}
	if len(f.reporter.events) != 1 || f.reporter.events[0] != "identity.verification_failed" {
		t.Fatalf("events = %v", f.reporter.events)
	}
}

func TestVerifyEmailConfirmsOnceAndAuditsOnce(t *testing.T) {
	f := newAccountFixture()
	f.register(t, "owner@example.com")
	token := tokenFromMail(t, f.mailer.last(t))
	client := Client{IPAddress: "192.0.2.77"}

	if err := f.service.VerifyEmail(context.Background(), token, client); err != nil {
		t.Fatalf("VerifyEmail() error = %v", err)
	}
	verified := f.repo.users["owner@example.com"].EmailVerifiedAt
	if verified == nil || !verified.Equal(f.now) {
		t.Fatalf("EmailVerifiedAt = %v", verified)
	}
	if got := f.repo.auditOperations(); got[len(got)-1] != "EMAIL_VERIFIED" {
		t.Fatalf("audit = %v", got)
	}
	if err := f.service.VerifyEmail(context.Background(), token, client); !errors.Is(err, ErrVerificationTokenInvalid) {
		t.Fatalf("reused token error = %v", err)
	}
	if err := f.service.VerifyEmail(context.Background(), "short", client); !errors.Is(err, ErrVerificationTokenInvalid) {
		t.Fatalf("malformed token error = %v", err)
	}
}

func TestVerifyEmailRejectsExpiredTokens(t *testing.T) {
	f := newAccountFixture()
	f.register(t, "owner@example.com")
	token := tokenFromMail(t, f.mailer.last(t))
	f.now = f.now.Add(emailVerificationTTL)
	if err := f.service.VerifyEmail(context.Background(), token, Client{IPAddress: "192.0.2.77"}); !errors.Is(err, ErrVerificationTokenInvalid) {
		t.Fatalf("expired token error = %v", err)
	}
}

func TestVerifyEmailLimitsAttemptsPerIP(t *testing.T) {
	f := newAccountFixture()
	client := Client{IPAddress: "192.0.2.99"}
	bad := strings.Repeat("b", 43)
	for attempt := 1; attempt <= emailVerifyIPLimit; attempt++ {
		if err := f.service.VerifyEmail(context.Background(), bad, client); !errors.Is(err, ErrVerificationTokenInvalid) {
			t.Fatalf("attempt %d error = %v", attempt, err)
		}
	}
	if err := f.service.VerifyEmail(context.Background(), bad, client); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("limited attempt error = %v", err)
	}
}

func TestRequestEmailVerificationResendsRevokesTheOldLinkAndIsLimited(t *testing.T) {
	f := newAccountFixture()
	registered := f.register(t, "owner@example.com")
	first := tokenFromMail(t, f.mailer.last(t))
	f.mailer.mails = nil
	client := Client{IPAddress: "192.0.2.1"}

	for attempt := 1; attempt <= emailVerificationAccountLimit; attempt++ {
		if err := f.service.RequestEmailVerification(context.Background(), registered.Token, client); err != nil {
			t.Fatalf("resend %d: %v", attempt, err)
		}
	}
	if len(f.mailer.mails) != emailVerificationAccountLimit {
		t.Fatalf("mails = %d", len(f.mailer.mails))
	}
	if err := f.service.RequestEmailVerification(context.Background(), registered.Token, client); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("limited resend error = %v", err)
	}
	if err := f.service.VerifyEmail(context.Background(), first, client); !errors.Is(err, ErrVerificationTokenInvalid) {
		t.Fatalf("the registration link must be dead after a resend, got %v", err)
	}
	if err := f.service.VerifyEmail(context.Background(), tokenFromMail(t, f.mailer.last(t)), client); err != nil {
		t.Fatalf("the newest link must work: %v", err)
	}
}

func TestRequestEmailVerificationNeedsNoMailWhenAlreadyVerified(t *testing.T) {
	f := newAccountFixture()
	registered := f.register(t, "owner@example.com")
	if err := f.service.VerifyEmail(context.Background(), tokenFromMail(t, f.mailer.last(t)), Client{IPAddress: "192.0.2.77"}); err != nil {
		t.Fatal(err)
	}
	f.mailer.mails = nil
	for attempt := 0; attempt < emailVerificationAccountLimit+2; attempt++ {
		if err := f.service.RequestEmailVerification(context.Background(), registered.Token, Client{}); err != nil {
			t.Fatalf("an already verified address must be a quiet success: %v", err)
		}
	}
	if len(f.mailer.mails) != 0 {
		t.Fatal("no mail is sent to a verified address")
	}
}

func TestRequestEmailVerificationErrors(t *testing.T) {
	f := newAccountFixture()
	registered := f.register(t, "owner@example.com")
	if err := f.service.RequestEmailVerification(context.Background(), "", Client{}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("no session error = %v", err)
	}
	bare := NewService(f.repo, newTestRateLimiter(), testPasswords{}, &testIDs{}, &longTokens{}, func() time.Time { return f.now }, time.Hour)
	if err := bare.RequestEmailVerification(context.Background(), registered.Token, Client{}); !errors.Is(err, ErrMailUnavailable) {
		t.Fatalf("without mail error = %v", err)
	}
	f.mailer.err = errors.New("queue unavailable")
	if err := f.service.RequestEmailVerification(context.Background(), registered.Token, Client{}); err == nil {
		t.Fatal("an explicit resend must report a queue failure")
	}
}

func TestMailTextsStateTheLifetimesOfTheirLinks(t *testing.T) {
	if _, text := passwordResetMail("link"); passwordResetTTL != 60*time.Minute || !strings.Contains(text, "60 минут") {
		t.Fatalf("the reset mail must state the %s lifetime: %q", passwordResetTTL, text)
	}
	if _, text := emailVerificationMail("link"); emailVerificationTTL != 7*24*time.Hour || !strings.Contains(text, "7 суток") {
		t.Fatalf("the verification mail must state the %s lifetime: %q", emailVerificationTTL, text)
	}
	for _, build := range []func() (string, string){
		func() (string, string) { return passwordResetMail("https://app.example.test/x#token=abc") },
		func() (string, string) { return emailVerificationMail("https://app.example.test/x#token=abc") },
		passwordChangedMail,
	} {
		subject, text := build()
		if subject == "" || strings.ContainsAny(subject, "\r\n") || !strings.HasSuffix(text, "\n") {
			t.Fatalf("malformed mail: %q %q", subject, text)
		}
	}
}
