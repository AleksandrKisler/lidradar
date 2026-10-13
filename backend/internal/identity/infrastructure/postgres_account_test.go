package infrastructure_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/identity/domain"
	"lidradar/backend/internal/identity/infrastructure"
	"lidradar/backend/internal/testsupport"
)

// Команды безопасности учётной записи на настоящей PostgreSQL (ADR 0058): смена и сброс пароля и подтверждение
// почты выполняются одной транзакцией вместе с аудитом, токены одноразовы даже при гонке, а выпуск, погашение и
// смена пароля не блокируют друг друга намертво.

type accountEnv struct {
	pool       *pgxpool.Pool
	repository *infrastructure.PostgresRepository
	user       domain.User
	sessions   []domain.Session
	now        time.Time
}

func newAccountEnv(t testing.TB, email string) *accountEnv {
	t.Helper()
	pool := testsupport.Postgres(t)
	env := &accountEnv{pool: pool, repository: infrastructure.NewPostgresRepository(pool), now: invitationNow}
	user, first := newUserWithSession(t, email, 0)
	env.user = user
	if err := env.repository.CreateUserSession(context.Background(), user, first); err != nil {
		t.Fatal(err)
	}
	env.sessions = []domain.Session{first}
	for index := 0; index < 2; index++ {
		env.addSession(t)
	}
	return env
}

func (e *accountEnv) addSession(t testing.TB) domain.Session {
	t.Helper()
	id := newUUID(t)
	session, err := domain.NewSession(id, e.user.ID, codeHash("session-"+id), e.now, e.now.Add(24*time.Hour), "192.0.2.5", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.repository.CreateSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	e.sessions = append(e.sessions, session)
	return session
}

func (e *accountEnv) audit(t testing.TB, operation string) domain.AuditRecord {
	t.Helper()
	record, err := domain.NewAuditRecord(newUUID(t), operation, "198.51.100.7", e.now)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func (e *accountEnv) issue(t testing.TB, purpose domain.TokenPurpose, secret string, ttl time.Duration, audit *domain.AuditRecord) domain.AccountToken {
	t.Helper()
	token, err := domain.NewAccountToken(newUUID(t), e.user.ID, purpose, codeHash(secret), e.now, e.now.Add(ttl))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.repository.IssueAccountToken(context.Background(), token, audit); err != nil {
		t.Fatalf("IssueAccountToken() error = %v", err)
	}
	return token
}

func (e *accountEnv) activeSessions(t testing.TB) int {
	t.Helper()
	return count(t, e.pool, `SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, e.user.ID)
}

func (e *accountEnv) passwordHash(t testing.TB) string {
	t.Helper()
	var hash string
	if err := e.pool.QueryRow(context.Background(), `SELECT password_hash FROM users WHERE id = $1`, e.user.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

func (e *accountEnv) auditOperations(t testing.TB) []string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT operation FROM auth_audit_log WHERE user_id = $1 ORDER BY created_at, id`, e.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var operations []string
	for rows.Next() {
		var operation string
		if err := rows.Scan(&operation); err != nil {
			t.Fatal(err)
		}
		operations = append(operations, operation)
	}
	return operations
}

func (e *accountEnv) change(t testing.TB, newHash string, keep domain.Session) domain.PasswordChange {
	t.Helper()
	return domain.PasswordChange{
		UserID: e.user.ID, PreviousHash: e.user.PasswordHash, NewHash: newHash, KeepSessionHash: keep.TokenHash,
		At: e.now.Add(time.Minute), Audit: e.audit(t, "PASSWORD_CHANGED"),
	}
}

func TestChangePasswordIsOneAtomicTransaction(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	reset := env.issue(t, domain.TokenPasswordReset, "reset-secret", time.Hour, nil)
	verification := env.issue(t, domain.TokenEmailVerification, "verify-secret", 24*time.Hour, nil)
	keep := env.sessions[0]

	if err := env.repository.ChangePassword(context.Background(), env.change(t, "$argon2id$new", keep)); err != nil {
		t.Fatalf("ChangePassword() error = %v", err)
	}
	if got := env.passwordHash(t); got != "$argon2id$new" {
		t.Fatalf("password hash = %q", got)
	}
	if env.activeSessions(t) != 1 {
		t.Fatalf("only the current session must survive, got %d", env.activeSessions(t))
	}
	if user, found, err := env.repository.UserBySessionHash(context.Background(), keep.TokenHash, env.now.Add(2*time.Minute)); err != nil || !found || user.ID != env.user.ID {
		t.Fatalf("the current session must stay valid: %v %v", found, err)
	}
	if got := count(t, env.pool, `SELECT count(*) FROM account_tokens WHERE id = $1 AND revoked_at IS NOT NULL`, reset.ID); got != 1 {
		t.Fatal("the pending reset link must be revoked by a password change")
	}
	if got := count(t, env.pool, `SELECT count(*) FROM account_tokens WHERE id = $1 AND revoked_at IS NULL AND used_at IS NULL`, verification.ID); got != 1 {
		t.Fatal("a verification link has nothing to do with the password and must stay valid")
	}
	if ops := env.auditOperations(t); len(ops) != 1 || ops[0] != "PASSWORD_CHANGED" {
		t.Fatalf("audit = %v", ops)
	}
	var address string
	if err := env.pool.QueryRow(context.Background(), `SELECT ip_address FROM auth_audit_log WHERE user_id = $1`, env.user.ID).Scan(&address); err != nil || address != "198.51.100.7" {
		t.Fatalf("audit address = %q, %v", address, err)
	}
}

func TestChangePasswordRejectsAStaleProofWithoutAnyChange(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	change := env.change(t, "$argon2id$new", env.sessions[0])
	change.PreviousHash = "$argon2id$something-else"
	if err := env.repository.ChangePassword(context.Background(), change); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
	if got := env.passwordHash(t); got != env.user.PasswordHash || env.activeSessions(t) != 3 || len(env.auditOperations(t)) != 0 {
		t.Fatalf("a rejected change left traces: hash=%q sessions=%d audit=%v", got, env.activeSessions(t), env.auditOperations(t))
	}
}

func TestChangePasswordRollsBackEverythingWhenTheAuditCannotBeWritten(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	reset := env.issue(t, domain.TokenPasswordReset, "reset-secret", time.Hour, nil)
	// Идентификатор записи аудита уже занят: вставка падает на последнем шаге транзакции.
	taken := env.audit(t, "USER_LOGGED_IN")
	if _, err := env.pool.Exec(context.Background(), `INSERT INTO auth_audit_log(id, user_id, operation, created_at) VALUES ($1, $2, $3, $4)`,
		taken.ID, env.user.ID, taken.Operation, env.now); err != nil {
		t.Fatal(err)
	}
	change := env.change(t, "$argon2id$new", env.sessions[0])
	change.Audit.ID = taken.ID
	if err := env.repository.ChangePassword(context.Background(), change); err == nil {
		t.Fatal("a failing audit must fail the whole command")
	}
	if env.passwordHash(t) != env.user.PasswordHash || env.activeSessions(t) != 3 ||
		count(t, env.pool, `SELECT count(*) FROM account_tokens WHERE id = $1 AND revoked_at IS NULL`, reset.ID) != 1 {
		t.Fatal("the password, sessions and tokens must be exactly as before: the change is all or nothing")
	}
}

func TestChangePasswordRefusesDisabledAndMissingUsers(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	if _, err := env.pool.Exec(context.Background(), `UPDATE users SET status = 'DISABLED' WHERE id = $1`, env.user.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.repository.ChangePassword(context.Background(), env.change(t, "$argon2id$new", env.sessions[0])); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("disabled user: %v", err)
	}
	change := env.change(t, "$argon2id$new", env.sessions[0])
	change.UserID = newUUID(t)
	if err := env.repository.ChangePassword(context.Background(), change); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing user: %v", err)
	}
	invalid := env.change(t, "", env.sessions[0])
	if err := env.repository.ChangePassword(context.Background(), invalid); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty hash: %v", err)
	}
}

func TestIssueAccountTokenRevokesThePreviousTokenOfTheSamePurposeOnly(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	firstVerification := env.issue(t, domain.TokenEmailVerification, "verify-1", time.Hour, nil)
	reset := env.issue(t, domain.TokenPasswordReset, "reset-1", time.Hour, nil)
	secondVerification := env.issue(t, domain.TokenEmailVerification, "verify-2", time.Hour, nil)

	revoked := func(id string) bool {
		return count(t, env.pool, `SELECT count(*) FROM account_tokens WHERE id = $1 AND revoked_at IS NOT NULL`, id) == 1
	}
	if !revoked(firstVerification.ID) || revoked(secondVerification.ID) || revoked(reset.ID) {
		t.Fatalf("revoked: first=%v second=%v reset=%v", revoked(firstVerification.ID), revoked(secondVerification.ID), revoked(reset.ID))
	}
	// Страховка схемы: два действующих токена одного назначения не уживаются даже в обход репозитория.
	_, err := env.pool.Exec(context.Background(), `
		INSERT INTO account_tokens(id, user_id, purpose, token_hash, created_at, expires_at)
		VALUES ($1, $2, 'EMAIL_VERIFICATION', $3, $4, $5)`, newUUID(t), env.user.ID, codeHash("verify-3"), env.now, env.now.Add(time.Hour))
	if err == nil {
		t.Fatal("the schema must refuse a second active token of the same purpose")
	}
}

func TestIssueAccountTokenWritesTheAuditInTheSameTransaction(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	audit := env.audit(t, "PASSWORD_RESET_REQUESTED")
	env.issue(t, domain.TokenPasswordReset, "reset-1", time.Hour, &audit)
	if ops := env.auditOperations(t); len(ops) != 1 || ops[0] != "PASSWORD_RESET_REQUESTED" {
		t.Fatalf("audit = %v", ops)
	}
	// Не удалась запись аудита — токен не выпускается, а прежний остаётся действующим.
	taken := env.audit(t, "USER_LOGGED_IN")
	if _, err := env.pool.Exec(context.Background(), `INSERT INTO auth_audit_log(id, user_id, operation, created_at) VALUES ($1, $2, $3, $4)`,
		taken.ID, env.user.ID, taken.Operation, env.now); err != nil {
		t.Fatal(err)
	}
	token, err := domain.NewAccountToken(newUUID(t), env.user.ID, domain.TokenPasswordReset, codeHash("reset-2"), env.now, env.now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := env.repository.IssueAccountToken(context.Background(), token, &taken); err == nil {
		t.Fatal("a failing audit must fail the issue")
	}
	if got := count(t, env.pool, `SELECT count(*) FROM account_tokens WHERE purpose = 'PASSWORD_RESET' AND revoked_at IS NULL`); got != 1 {
		t.Fatalf("active reset tokens = %d: the failed issue must neither add a token nor revoke the old one", got)
	}
	// Без аудита (письмо подтверждения) запись не нужна.
	env.issue(t, domain.TokenEmailVerification, "verify-1", time.Hour, nil)
	if len(env.auditOperations(t)) != 2 {
		t.Fatalf("a verification issue is not audited, got %v", env.auditOperations(t))
	}
}

func TestIssueAccountTokenForAnUnknownUser(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	token, err := domain.NewAccountToken(newUUID(t), newUUID(t), domain.TokenPasswordReset, codeHash("x"), env.now, env.now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := env.repository.IssueAccountToken(context.Background(), token, nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v", err)
	}
}

func TestAccountTokenUsableChecksEveryCondition(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	ctx := context.Background()
	env.issue(t, domain.TokenPasswordReset, "good", time.Hour, nil)
	usable := func(purpose domain.TokenPurpose, secret string, at time.Time) bool {
		t.Helper()
		ok, err := env.repository.AccountTokenUsable(ctx, purpose, codeHash(secret), at)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !usable(domain.TokenPasswordReset, "good", env.now.Add(59*time.Minute)) {
		t.Fatal("a fresh token must be usable")
	}
	if usable(domain.TokenPasswordReset, "good", env.now.Add(time.Hour)) {
		t.Fatal("a token is not usable at the instant it expires")
	}
	if usable(domain.TokenEmailVerification, "good", env.now) {
		t.Fatal("a token of another purpose must not be usable")
	}
	if usable(domain.TokenPasswordReset, "unknown", env.now) {
		t.Fatal("an unknown token must not be usable")
	}
	if _, err := env.pool.Exec(ctx, `UPDATE users SET status = 'DISABLED' WHERE id = $1`, env.user.ID); err != nil {
		t.Fatal(err)
	}
	if usable(domain.TokenPasswordReset, "good", env.now) {
		t.Fatal("a disabled user's token must not be usable")
	}
	if _, err := env.pool.Exec(ctx, `UPDATE users SET status = 'ACTIVE' WHERE id = $1`, env.user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `UPDATE account_tokens SET used_at = $1`, env.now); err != nil {
		t.Fatal(err)
	}
	if usable(domain.TokenPasswordReset, "good", env.now) {
		t.Fatal("a used token must not be usable")
	}
	if _, err := env.pool.Exec(ctx, `UPDATE account_tokens SET used_at = NULL, revoked_at = $1`, env.now); err != nil {
		t.Fatal(err)
	}
	if usable(domain.TokenPasswordReset, "good", env.now) {
		t.Fatal("a revoked token must not be usable")
	}
	if _, err := env.repository.AccountTokenUsable(ctx, domain.TokenPasswordReset, "short", env.now); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("a malformed hash must be refused: %v", err)
	}
}

func TestCompletePasswordResetIsAtomicAndRevokesEverything(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	reset := env.issue(t, domain.TokenPasswordReset, "reset-secret", time.Hour, nil)
	verification := env.issue(t, domain.TokenEmailVerification, "verify-secret", 24*time.Hour, nil)
	at := env.now.Add(10 * time.Minute)

	email, err := env.repository.CompletePasswordReset(context.Background(), domain.PasswordReset{
		TokenHash: reset.TokenHash, NewHash: "$argon2id$reset", At: at, Audit: env.audit(t, "PASSWORD_RESET_COMPLETED"),
	})
	if err != nil {
		t.Fatalf("CompletePasswordReset() error = %v", err)
	}
	if email != "owner@example.com" {
		t.Fatalf("email = %q", email)
	}
	if env.passwordHash(t) != "$argon2id$reset" {
		t.Fatal("the password was not changed")
	}
	if env.activeSessions(t) != 0 {
		t.Fatalf("a reset must end every session, %d remain", env.activeSessions(t))
	}
	if count(t, env.pool, `SELECT count(*) FROM account_tokens WHERE id = $1 AND used_at IS NOT NULL`, reset.ID) != 1 {
		t.Fatal("the reset token must be marked used")
	}
	if count(t, env.pool, `SELECT count(*) FROM account_tokens WHERE id = $1 AND used_at IS NULL AND revoked_at IS NULL`, verification.ID) != 1 {
		t.Fatal("the verification token is unrelated and must stay valid")
	}
	user, found, err := env.repository.UserByEmail(context.Background(), "owner@example.com")
	if err != nil || !found || user.EmailVerifiedAt == nil || !user.EmailVerifiedAt.Equal(at) {
		t.Fatalf("a reset link that reached the mailbox must verify the address: %+v %v", user.EmailVerifiedAt, err)
	}
	if ops := env.auditOperations(t); len(ops) != 1 || ops[0] != "PASSWORD_RESET_COMPLETED" {
		t.Fatalf("audit = %v", ops)
	}
	// Одноразовость.
	if _, err := env.repository.CompletePasswordReset(context.Background(), domain.PasswordReset{
		TokenHash: reset.TokenHash, NewHash: "$argon2id$again", At: at, Audit: env.audit(t, "PASSWORD_RESET_COMPLETED"),
	}); !errors.Is(err, domain.ErrTokenInvalid) {
		t.Fatalf("a used token must be refused: %v", err)
	}
	if env.passwordHash(t) != "$argon2id$reset" {
		t.Fatal("a refused reset changed the password")
	}
}

func TestCompletePasswordResetKeepsAnEarlierVerificationTime(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	earlier := env.now.Add(-48 * time.Hour)
	if _, err := env.pool.Exec(context.Background(), `UPDATE users SET email_verified_at = $2 WHERE id = $1`, env.user.ID, earlier); err != nil {
		t.Fatal(err)
	}
	reset := env.issue(t, domain.TokenPasswordReset, "reset-secret", time.Hour, nil)
	if _, err := env.repository.CompletePasswordReset(context.Background(), domain.PasswordReset{
		TokenHash: reset.TokenHash, NewHash: "$argon2id$reset", At: env.now, Audit: env.audit(t, "PASSWORD_RESET_COMPLETED"),
	}); err != nil {
		t.Fatal(err)
	}
	user, _, _ := env.repository.UserByEmail(context.Background(), "owner@example.com")
	if user.EmailVerifiedAt == nil || !user.EmailVerifiedAt.Equal(earlier) {
		t.Fatalf("the original verification time must stay: %v", user.EmailVerifiedAt)
	}
}

func TestCompletePasswordResetRejectsEveryUnusableToken(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		prepare func(env *accountEnv, token domain.AccountToken)
		at      time.Duration
		hash    string
	}{
		"неизвестный":  {func(*accountEnv, domain.AccountToken) {}, 0, codeHash("unknown")},
		"просроченный": {func(*accountEnv, domain.AccountToken) {}, time.Hour, ""},
		"использован": {func(env *accountEnv, token domain.AccountToken) {
			env.mustExec(t, `UPDATE account_tokens SET used_at = $1 WHERE id = $2`, env.now, token.ID)
		}, 0, ""},
		"отозван": {func(env *accountEnv, token domain.AccountToken) {
			env.mustExec(t, `UPDATE account_tokens SET revoked_at = $1 WHERE id = $2`, env.now, token.ID)
		}, 0, ""},
		"отключён": {func(env *accountEnv, token domain.AccountToken) {
			env.mustExec(t, `UPDATE users SET status = 'DISABLED' WHERE id = $1`, env.user.ID)
		}, 0, ""},
		"другое назначение": {func(env *accountEnv, token domain.AccountToken) {
			env.mustExec(t, `UPDATE account_tokens SET purpose = 'EMAIL_VERIFICATION' WHERE id = $1`, token.ID)
		}, 0, ""},
	} {
		env := newAccountEnv(t, "owner@example.com")
		token := env.issue(t, domain.TokenPasswordReset, "reset-secret", time.Hour, nil)
		tc.prepare(env, token)
		hash := tc.hash
		if hash == "" {
			hash = token.TokenHash
		}
		_, err := env.repository.CompletePasswordReset(ctx, domain.PasswordReset{
			TokenHash: hash, NewHash: "$argon2id$reset", At: env.now.Add(tc.at), Audit: env.audit(t, "PASSWORD_RESET_COMPLETED"),
		})
		if !errors.Is(err, domain.ErrTokenInvalid) {
			t.Errorf("%s: error = %v, want ErrTokenInvalid", name, err)
		}
		if env.passwordHash(t) != env.user.PasswordHash || env.activeSessions(t) != 3 || len(env.auditOperations(t)) != 0 {
			t.Errorf("%s: a refused reset left traces", name)
		}
	}
}

func (e *accountEnv) mustExec(t testing.TB, query string, arguments ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), query, arguments...); err != nil {
		t.Fatal(err)
	}
}

func TestCompletePasswordResetRollsBackWhenTheAuditCannotBeWritten(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	reset := env.issue(t, domain.TokenPasswordReset, "reset-secret", time.Hour, nil)
	taken := env.audit(t, "USER_LOGGED_IN")
	env.mustExec(t, `INSERT INTO auth_audit_log(id, user_id, operation, created_at) VALUES ($1, $2, $3, $4)`, taken.ID, env.user.ID, taken.Operation, env.now)
	audit := env.audit(t, "PASSWORD_RESET_COMPLETED")
	audit.ID = taken.ID
	if _, err := env.repository.CompletePasswordReset(context.Background(), domain.PasswordReset{
		TokenHash: reset.TokenHash, NewHash: "$argon2id$reset", At: env.now, Audit: audit,
	}); err == nil {
		t.Fatal("a failing audit must fail the reset")
	}
	if env.passwordHash(t) != env.user.PasswordHash || env.activeSessions(t) != 3 ||
		count(t, env.pool, `SELECT count(*) FROM account_tokens WHERE id = $1 AND used_at IS NULL AND revoked_at IS NULL`, reset.ID) != 1 {
		t.Fatal("password, sessions and token must be untouched: the reset is all or nothing")
	}
	user, _, _ := env.repository.UserByEmail(context.Background(), "owner@example.com")
	if user.EmailVerifiedAt != nil {
		t.Fatal("the address must not become verified by a rolled back reset")
	}
}

// Одну ссылку нельзя погасить дважды, даже если запросы пришли одновременно: победитель один, пароль его.
func TestConcurrentResetsOfOneTokenSucceedExactlyOnce(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	reset := env.issue(t, domain.TokenPasswordReset, "reset-secret", time.Hour, nil)
	const attempts = 8
	var group sync.WaitGroup
	results := make([]error, attempts)
	for index := 0; index < attempts; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			_, results[index] = env.repository.CompletePasswordReset(context.Background(), domain.PasswordReset{
				TokenHash: reset.TokenHash, NewHash: fmt.Sprintf("$argon2id$winner-%d", index), At: env.now.Add(time.Minute),
				Audit: env.audit(t, "PASSWORD_RESET_COMPLETED"),
			})
		}(index)
	}
	group.Wait()
	winner := -1
	for index, err := range results {
		switch {
		case err == nil && winner == -1:
			winner = index
		case err == nil:
			t.Fatalf("two resets succeeded: %d and %d", winner, index)
		case !errors.Is(err, domain.ErrTokenInvalid):
			t.Fatalf("attempt %d: unexpected error %v", index, err)
		}
	}
	if winner == -1 {
		t.Fatal("nobody won")
	}
	if got := env.passwordHash(t); got != fmt.Sprintf("$argon2id$winner-%d", winner) {
		t.Fatalf("password hash = %q, want the winner's", got)
	}
	if ops := env.auditOperations(t); len(ops) != 1 {
		t.Fatalf("exactly one audit record is expected, got %v", ops)
	}
}

func TestCompleteEmailVerificationConfirmsOnceAndAuditsOnce(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	token := env.issue(t, domain.TokenEmailVerification, "verify-secret", time.Hour, nil)
	at := env.now.Add(5 * time.Minute)
	if err := env.repository.CompleteEmailVerification(context.Background(), token.TokenHash, at, env.audit(t, "EMAIL_VERIFIED")); err != nil {
		t.Fatalf("CompleteEmailVerification() error = %v", err)
	}
	user, found, err := env.repository.UserByEmail(context.Background(), "owner@example.com")
	if err != nil || !found || user.EmailVerifiedAt == nil || !user.EmailVerifiedAt.Equal(at) {
		t.Fatalf("EmailVerifiedAt = %v, %v", user.EmailVerifiedAt, err)
	}
	if ops := env.auditOperations(t); len(ops) != 1 || ops[0] != "EMAIL_VERIFIED" {
		t.Fatalf("audit = %v", ops)
	}
	if err := env.repository.CompleteEmailVerification(context.Background(), token.TokenHash, at, env.audit(t, "EMAIL_VERIFIED")); !errors.Is(err, domain.ErrTokenInvalid) {
		t.Fatalf("a used token must be refused: %v", err)
	}
	if env.activeSessions(t) != 3 {
		t.Fatal("verifying an address must not touch sessions")
	}
}

func TestCompleteEmailVerificationOfAnAlreadyVerifiedAddressConsumesTheTokenQuietly(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	earlier := env.now.Add(-24 * time.Hour)
	env.mustExec(t, `UPDATE users SET email_verified_at = $2 WHERE id = $1`, env.user.ID, earlier)
	token := env.issue(t, domain.TokenEmailVerification, "verify-secret", time.Hour, nil)
	if err := env.repository.CompleteEmailVerification(context.Background(), token.TokenHash, env.now, env.audit(t, "EMAIL_VERIFIED")); err != nil {
		t.Fatal(err)
	}
	user, _, _ := env.repository.UserByEmail(context.Background(), "owner@example.com")
	if user.EmailVerifiedAt == nil || !user.EmailVerifiedAt.Equal(earlier) {
		t.Fatalf("the original time must stay: %v", user.EmailVerifiedAt)
	}
	if len(env.auditOperations(t)) != 0 {
		t.Fatal("nothing changed, so nothing is audited")
	}
	if count(t, env.pool, `SELECT count(*) FROM account_tokens WHERE id = $1 AND used_at IS NOT NULL`, token.ID) != 1 {
		t.Fatal("the token must still be consumed")
	}
}

func TestCompleteEmailVerificationRejectsUnusableTokens(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		purpose domain.TokenPurpose
		wait    time.Duration
		mutate  string
	}{
		"токен сброса": {domain.TokenPasswordReset, 0, ""},
		"просроченный": {domain.TokenEmailVerification, 2 * time.Hour, ""},
		"отозванный":   {domain.TokenEmailVerification, 0, `UPDATE account_tokens SET revoked_at = now()`},
		"отключённый":  {domain.TokenEmailVerification, 0, `UPDATE users SET status = 'DISABLED'`},
	} {
		env := newAccountEnv(t, "owner@example.com")
		token := env.issue(t, tc.purpose, "secret", time.Hour, nil)
		if tc.mutate != "" {
			env.mustExec(t, tc.mutate)
		}
		if err := env.repository.CompleteEmailVerification(ctx, token.TokenHash, env.now.Add(tc.wait), env.audit(t, "EMAIL_VERIFIED")); !errors.Is(err, domain.ErrTokenInvalid) {
			t.Errorf("%s: error = %v", name, err)
		}
		if user, _, _ := env.repository.UserByEmail(ctx, "owner@example.com"); user.EmailVerifiedAt != nil {
			t.Errorf("%s: the address must stay unverified", name)
		}
	}
	env := newAccountEnv(t, "owner@example.com")
	if err := env.repository.CompleteEmailVerification(ctx, codeHash("never-issued"), env.now, env.audit(t, "EMAIL_VERIFIED")); !errors.Is(err, domain.ErrTokenInvalid) {
		t.Errorf("unknown token: %v", err)
	}
}

func TestConcurrentVerificationOfOneTokenSucceedsExactlyOnce(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	token := env.issue(t, domain.TokenEmailVerification, "verify-secret", time.Hour, nil)
	const attempts = 8
	var group sync.WaitGroup
	results := make([]error, attempts)
	for index := 0; index < attempts; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			results[index] = env.repository.CompleteEmailVerification(context.Background(), token.TokenHash, env.now, env.audit(t, "EMAIL_VERIFIED"))
		}(index)
	}
	group.Wait()
	successes := 0
	for index, err := range results {
		switch {
		case err == nil:
			successes++
		case !errors.Is(err, domain.ErrTokenInvalid):
			t.Fatalf("attempt %d: unexpected error %v", index, err)
		}
	}
	if successes != 1 || len(env.auditOperations(t)) != 1 {
		t.Fatalf("successes = %d, audit = %v", successes, env.auditOperations(t))
	}
}

// Выпуск, погашение и смена пароля берут блокировки в одном порядке (пользователь, затем токены): при
// одновременной работе над одним пользователем взаимной блокировки быть не должно.
func TestIssueResetAndChangeDoNotDeadlock(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var group sync.WaitGroup
	failures := make(chan error, 64)
	record := func(err error) {
		if err != nil && !errors.Is(err, domain.ErrTokenInvalid) && !errors.Is(err, domain.ErrConflict) {
			failures <- err
		}
	}
	for worker := 0; worker < 4; worker++ {
		group.Add(3)
		go func(worker int) {
			defer group.Done()
			for round := 0; round < 12; round++ {
				token, err := domain.NewAccountToken(newUUID(t), env.user.ID, domain.TokenPasswordReset, codeHash(fmt.Sprintf("issue-%d-%d", worker, round)), env.now, env.now.Add(time.Hour))
				if err != nil {
					failures <- err
					return
				}
				record(env.repository.IssueAccountToken(ctx, token, nil))
			}
		}(worker)
		go func(worker int) {
			defer group.Done()
			for round := 0; round < 12; round++ {
				_, err := env.repository.CompletePasswordReset(ctx, domain.PasswordReset{
					TokenHash: codeHash(fmt.Sprintf("issue-%d-%d", worker, round)), NewHash: fmt.Sprintf("$argon2id$r-%d-%d", worker, round),
					At: env.now.Add(time.Minute), Audit: env.audit(t, "PASSWORD_RESET_COMPLETED"),
				})
				record(err)
			}
		}(worker)
		go func(worker int) {
			defer group.Done()
			for round := 0; round < 12; round++ {
				var current string
				if err := env.pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1`, env.user.ID).Scan(&current); err != nil {
					failures <- err
					return
				}
				change := env.change(t, fmt.Sprintf("$argon2id$c-%d-%d", worker, round), env.sessions[0])
				change.PreviousHash = current
				record(env.repository.ChangePassword(ctx, change))
			}
		}(worker)
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("a concurrent command failed (a deadlock is the suspect): %v", err)
	}
}
