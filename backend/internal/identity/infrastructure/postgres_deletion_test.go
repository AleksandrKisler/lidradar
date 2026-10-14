package infrastructure_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"lidradar/backend/internal/identity/application"
	"lidradar/backend/internal/identity/domain"
	"lidradar/backend/internal/identity/infrastructure"
)

// Назначение и отмена закрытия учётной записи на настоящей PostgreSQL (ADR 0059): одна транзакция с аудитом,
// идемпотентность при гонке, CHECK таблицы и защита создания сессии от закрываемого пользователя.

func (e *accountEnv) schedule(t testing.TB, at time.Time, grace time.Duration) domain.DeletionSchedule {
	t.Helper()
	schedule, err := domain.NewDeletionSchedule(e.user.ID, at, at.Add(grace), e.audit(t, "ACCOUNT_DELETION_REQUESTED"))
	if err != nil {
		t.Fatal(err)
	}
	return schedule
}

func TestScheduleDeletionIsOneTransactionWithTheAudit(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	schedule := env.schedule(t, env.now, 30*24*time.Hour)

	due, created, err := env.repository.ScheduleDeletion(context.Background(), schedule)
	if err != nil || !created || !due.Equal(schedule.Due) {
		t.Fatalf("ScheduleDeletion() = %s, %v, %v", due, created, err)
	}
	user, found, err := env.repository.UserByEmail(context.Background(), "owner@example.com")
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if user.DeletionDueAt == nil || !user.DeletionDueAt.Equal(schedule.Due) || user.DeletionRequestedAt == nil || !user.DeletionRequestedAt.Equal(schedule.At) {
		t.Fatalf("stored schedule = %v / %v", user.DeletionRequestedAt, user.DeletionDueAt)
	}
	if user.Status != domain.UserActive {
		t.Fatalf("the account must stay active during the grace period, status = %s", user.Status)
	}
	if ops := env.auditOperations(t); len(ops) != 1 || ops[0] != "ACCOUNT_DELETION_REQUESTED" {
		t.Fatalf("audit = %v", ops)
	}
	// Все три способа найти пользователя несут срок: по почте, по сессии и при ротации.
	bySession, found, err := env.repository.UserBySessionHash(context.Background(), env.sessions[0].TokenHash, env.now.Add(time.Minute))
	if err != nil || !found || bySession.DeletionDueAt == nil || !bySession.DeletionDueAt.Equal(schedule.Due) {
		t.Fatalf("UserBySessionHash() = %v, %v, %v", bySession.DeletionDueAt, found, err)
	}
	replacement, err := domain.NewSession(newUUID(t), env.user.ID, codeHash("rotated"), env.now, env.now.Add(time.Hour), "192.0.2.5", "test")
	if err != nil {
		t.Fatal(err)
	}
	rotated, found, err := env.repository.RotateSession(context.Background(), env.sessions[0].TokenHash, replacement, env.now.Add(time.Minute))
	if err != nil || !found || rotated.DeletionDueAt == nil || !rotated.DeletionDueAt.Equal(schedule.Due) {
		t.Fatalf("RotateSession() = %v, %v, %v", rotated.DeletionDueAt, found, err)
	}
}

func TestScheduleDeletionRepeatKeepsTheFirstDueDateAndWritesNothing(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	first := env.schedule(t, env.now, 30*24*time.Hour)
	if _, created, err := env.repository.ScheduleDeletion(context.Background(), first); err != nil || !created {
		t.Fatal(created, err)
	}

	later := env.schedule(t, env.now.Add(48*time.Hour), 30*24*time.Hour)
	due, created, err := env.repository.ScheduleDeletion(context.Background(), later)
	if err != nil || created || !due.Equal(first.Due) {
		t.Fatalf("repeat = %s, %v, %v; want the first due %s", due, created, err, first.Due)
	}
	if ops := env.auditOperations(t); len(ops) != 1 {
		t.Fatalf("a repeated schedule wrote audit: %v", ops)
	}
	var requested time.Time
	if err := env.pool.QueryRow(context.Background(), `SELECT deletion_requested_at FROM users WHERE id = $1`, env.user.ID).Scan(&requested); err != nil || !requested.Equal(first.At) {
		t.Fatalf("deletion_requested_at = %s, %v", requested, err)
	}
}

func TestScheduleDeletionByParallelRequestsCreatesExactlyOneSchedule(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	const parallel = 12
	results := make(chan struct {
		due     time.Time
		created bool
		err     error
	}, parallel)
	var group sync.WaitGroup
	for index := 0; index < parallel; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			// Разные сроки у каждого запроса: победитель определяется только порядком блокировки.
			schedule := env.schedule(t, env.now.Add(time.Duration(index)*time.Minute), 30*24*time.Hour)
			due, created, err := env.repository.ScheduleDeletion(context.Background(), schedule)
			results <- struct {
				due     time.Time
				created bool
				err     error
			}{due, created, err}
		}()
	}
	group.Wait()
	close(results)
	created := 0
	var winner time.Time
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.created {
			created++
			winner = result.due
		}
	}
	if created != 1 {
		t.Fatalf("%d requests believe they created the schedule", created)
	}
	for _, due := range []time.Time{winner} {
		var stored time.Time
		if err := env.pool.QueryRow(context.Background(), `SELECT deletion_due_at FROM users WHERE id = $1`, env.user.ID).Scan(&stored); err != nil || !stored.Equal(due) {
			t.Fatalf("stored due = %s, %v; want the winner's %s", stored, err, due)
		}
	}
	if ops := env.auditOperations(t); len(ops) != 1 {
		t.Fatalf("audit = %v, want exactly one record", ops)
	}
}

func TestScheduleDeletionRefusesDisabledAndUnknownUsers(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	if _, err := env.pool.Exec(context.Background(), `UPDATE users SET status = 'DISABLED' WHERE id = $1`, env.user.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.repository.ScheduleDeletion(context.Background(), env.schedule(t, env.now, time.Hour)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("disabled user error = %v, want ErrNotFound", err)
	}
	if _, err := env.repository.CancelDeletion(context.Background(), env.user.ID, env.now, env.audit(t, "ACCOUNT_DELETION_CANCELLED")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cancel for a disabled user error = %v, want ErrNotFound", err)
	}
	unknown, err := domain.NewDeletionSchedule(newUUID(t), env.now, env.now.Add(time.Hour), env.audit(t, "ACCOUNT_DELETION_REQUESTED"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.repository.ScheduleDeletion(context.Background(), unknown); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown user error = %v, want ErrNotFound", err)
	}
	if ops := env.auditOperations(t); len(ops) != 0 {
		t.Fatalf("a refused command left audit: %v", ops)
	}
}

func TestCancelDeletionClearsTheScheduleWithTheAudit(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	if _, _, err := env.repository.ScheduleDeletion(context.Background(), env.schedule(t, env.now, time.Hour)); err != nil {
		t.Fatal(err)
	}

	cancelled, err := env.repository.CancelDeletion(context.Background(), env.user.ID, env.now.Add(time.Minute), env.audit(t, "ACCOUNT_DELETION_CANCELLED"))
	if err != nil || !cancelled {
		t.Fatalf("CancelDeletion() = %v, %v", cancelled, err)
	}
	user, _, err := env.repository.UserByEmail(context.Background(), "owner@example.com")
	if err != nil || user.DeletionDueAt != nil || user.DeletionRequestedAt != nil {
		t.Fatalf("schedule was not cleared: %v / %v, %v", user.DeletionRequestedAt, user.DeletionDueAt, err)
	}
	ops := env.auditOperations(t)
	if len(ops) != 2 || ops[1] != "ACCOUNT_DELETION_CANCELLED" {
		t.Fatalf("audit = %v", ops)
	}

	again, err := env.repository.CancelDeletion(context.Background(), env.user.ID, env.now.Add(2*time.Minute), env.audit(t, "ACCOUNT_DELETION_CANCELLED"))
	if err != nil || again {
		t.Fatalf("repeated cancel = %v, %v; want false without an error", again, err)
	}
	if ops := env.auditOperations(t); len(ops) != 2 {
		t.Fatalf("a repeated cancel wrote audit: %v", ops)
	}
	// После отмены можно назначить заново, срок отсчитывается от нового запроса.
	second := env.schedule(t, env.now.Add(time.Hour), 30*24*time.Hour)
	due, created, err := env.repository.ScheduleDeletion(context.Background(), second)
	if err != nil || !created || !due.Equal(second.Due) {
		t.Fatalf("reschedule = %s, %v, %v", due, created, err)
	}
}

func TestUserDeletionColumnsRejectInconsistentStates(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	for name, statement := range map[string]string{
		"срок без отметки запроса":       `UPDATE users SET deletion_due_at = now() + interval '1 day' WHERE id = $1`,
		"отметка запроса без срока":      `UPDATE users SET deletion_requested_at = now() WHERE id = $1`,
		"срок не позже запроса":          `UPDATE users SET deletion_requested_at = now(), deletion_due_at = now() WHERE id = $1`,
		"срок раньше запроса":            `UPDATE users SET deletion_requested_at = now(), deletion_due_at = now() - interval '1 day' WHERE id = $1`,
		"обезличенный остаётся активным": `UPDATE users SET deleted_at = now() WHERE id = $1`,
	} {
		_, err := env.pool.Exec(context.Background(), statement, env.user.ID)
		var violation *pgconn.PgError
		if !errors.As(err, &violation) || violation.Code != "23514" {
			t.Errorf("%s: error = %v, want a CHECK violation", name, err)
		}
	}
	// Допустимые состояния проходят: назначено; обезличено без назначения (операторская команда).
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE users SET deletion_requested_at = now(), deletion_due_at = now() + interval '30 days' WHERE id = $1`, env.user.ID); err != nil {
		t.Errorf("a valid schedule was rejected: %v", err)
	}
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE users SET deletion_requested_at = NULL, deletion_due_at = NULL, status = 'DISABLED', deleted_at = now() WHERE id = $1`, env.user.ID); err != nil {
		t.Errorf("a valid anonymized state was rejected: %v", err)
	}
}

func TestDueDeletionsAreFoundByTheIndexedPredicate(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	other, _ := newUserWithSession(t, "other@example.com", 0)
	otherSession, err := domain.NewSession(newUUID(t), other.ID, codeHash("other-"+other.ID), env.now, env.now.Add(time.Hour), "192.0.2.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.repository.CreateUserSession(context.Background(), other, otherSession); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(context.Background(), `
		UPDATE users SET deletion_requested_at = $2::timestamptz - interval '31 days', deletion_due_at = $2::timestamptz - interval '1 day'
		WHERE id = $1`, env.user.ID, env.now); err != nil {
		t.Fatal(err)
	}
	if got := count(t, env.pool, `SELECT count(*) FROM users WHERE deletion_due_at <= $1 AND deleted_at IS NULL`, env.now); got != 1 {
		t.Fatalf("due users = %d, want 1", got)
	}
	// Определение индекса читается по имени в схеме теста: просмотр pg_indexes обходит все схемы и спотыкается о
	// схемы соседних тестов, которые в этот момент удаляются.
	var definition string
	if err := env.pool.QueryRow(context.Background(), `SELECT pg_get_indexdef('users_deletion_due_idx'::regclass)`).Scan(&definition); err != nil {
		t.Fatalf("index users_deletion_due_idx is missing: %v", err)
	}
	for _, want := range []string{"deletion_due_at, id", "deletion_due_at IS NOT NULL", "deleted_at IS NULL"} {
		if !strings.Contains(definition, want) {
			t.Errorf("index definition %q lacks %q", definition, want)
		}
	}
}

func TestSessionIsNotCreatedForAUserWhoIsNoLongerActive(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	before := count(t, env.pool, `SELECT count(*) FROM sessions WHERE user_id = $1`, env.user.ID)
	if _, err := env.pool.Exec(context.Background(), `UPDATE users SET status = 'DISABLED' WHERE id = $1`, env.user.ID); err != nil {
		t.Fatal(err)
	}
	session, err := domain.NewSession(newUUID(t), env.user.ID, codeHash("late-session"), env.now, env.now.Add(time.Hour), "198.51.100.9", "late")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.repository.CreateSession(context.Background(), session); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("CreateSession() error = %v, want ErrNotFound", err)
	}
	if got := count(t, env.pool, `SELECT count(*) FROM sessions WHERE user_id = $1`, env.user.ID); got != before {
		t.Fatalf("sessions %d → %d: a session with an address was stored for a disabled user", before, got)
	}
}

// Вход, начавшийся до закрытия учётной записи, ждёт блокировку строки пользователя и после фиксации не оставляет
// сессию: исполнитель обезличивания держит эту строку FOR UPDATE до конца транзакции.
func TestSessionCreationWaitsForAClosingTransaction(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	ctx := context.Background()
	closing, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closing.Rollback(ctx) }()
	var locked string
	if err := closing.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, env.user.ID).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	session, err := domain.NewSession(newUUID(t), env.user.ID, codeHash("racing-session"), env.now, env.now.Add(time.Hour), "198.51.100.9", "racing")
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- env.repository.CreateSession(ctx, session) }()

	select {
	case err := <-finished:
		t.Fatalf("CreateSession() finished while the user row was locked: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := closing.Exec(ctx, `UPDATE users SET status = 'DISABLED', deleted_at = now() WHERE id = $1`, env.user.ID); err != nil {
		t.Fatal(err)
	}
	if err := closing.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("CreateSession() after the user was closed error = %v, want ErrNotFound", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CreateSession() did not finish after the lock was released")
	}
	if got := count(t, env.pool, `SELECT count(*) FROM sessions WHERE token_hash = $1`, session.TokenHash); got != 0 {
		t.Fatal("the racing session was stored")
	}
}

func TestSessionCreationStillWorksForActiveUsers(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	before := count(t, env.pool, `SELECT count(*) FROM sessions WHERE user_id = $1`, env.user.ID)
	env.addSession(t)
	if got := count(t, env.pool, `SELECT count(*) FROM sessions WHERE user_id = $1`, env.user.ID); got != before+1 {
		t.Fatalf("sessions %d → %d", before, got)
	}
	// Регистрация создаёт пользователя и сессию одной транзакцией: проверка видит только что вставленную строку.
	registered, session := newUserWithSession(t, "fresh@example.com", 0)
	if err := env.repository.CreateUserSession(context.Background(), registered, session); err != nil {
		t.Fatalf("CreateUserSession() error = %v", err)
	}
}

// Новые пределы принимаются таблицей: список в CHECK совпадает с константами службы.
func TestNewRateLimitScopesAreAcceptedByTheTable(t *testing.T) {
	env := newAccountEnv(t, "owner@example.com")
	limiter := infrastructure.NewPostgresRateLimiter(env.pool)
	for _, scope := range []string{application.ScopeAccountExportAccount, application.ScopeAccountDeletionAccount, "ORGANIZATION_EXPORT_TENANT"} {
		decision, err := limiter.Take(context.Background(), scope, codeHash("subject-"+scope), 3, time.Hour, env.now)
		if err != nil || !decision.Allowed {
			t.Errorf("scope %s: %v, %v", scope, decision, err)
		}
	}
	if _, err := limiter.Take(context.Background(), "UNKNOWN_SCOPE", codeHash("subject"), 3, time.Hour, env.now); err == nil {
		t.Error("an unknown scope must still be rejected by the table")
	}
}
