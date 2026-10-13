package infrastructure_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/identity/domain"
	"lidradar/backend/internal/identity/infrastructure"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

// Регистрация по коду платформы на настоящей PostgreSQL (ADR 0057): погашение кода, пользователь и сессия —
// одна транзакция; недействительный код отвечает раньше, чем проверяется почта; два одновременных
// погашения одного кода дают ровно одного пользователя.

var invitationNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func codeHash(code string) string {
	digest := sha256.Sum256([]byte(code))
	return hex.EncodeToString(digest[:])
}

func newUUID(t testing.TB) string {
	t.Helper()
	value, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// insertCode кладёт код платформы напрямую: выпуск принадлежит модулю admin и здесь не проверяется.
func insertCode(t testing.TB, pool *pgxpool.Pool, code string, expiresAt time.Time, revoked bool) {
	t.Helper()
	var revokedAt *time.Time
	if revoked {
		revokedAt = &invitationNow
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO platform_invitations(id, code_hash, created_at, expires_at, revoked_at)
		VALUES ($1, $2, $3, $4, $5)`,
		newUUID(t), codeHash(code), invitationNow.Add(-time.Hour), expiresAt, revokedAt); err != nil {
		t.Fatalf("выпуск тестового кода: %v", err)
	}
}

func newUserWithSession(t testing.TB, email string, rights int) (domain.User, domain.Session) {
	t.Helper()
	user, err := domain.NewUser(newUUID(t), email, "$argon2id$test", "Тест", invitationNow)
	if err != nil {
		t.Fatal(err)
	}
	user.OrganizationRights = rights
	digest := sha256.Sum256([]byte("session-" + user.ID))
	session, err := domain.NewSession(newUUID(t), user.ID, hex.EncodeToString(digest[:]), invitationNow, invitationNow.Add(time.Hour), "192.0.2.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	return user, session
}

func count(t testing.TB, pool *pgxpool.Pool, query string, arguments ...any) int {
	t.Helper()
	var value int
	if err := pool.QueryRow(context.Background(), query, arguments...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestPlatformInvitationIsRedeemedAtomicallyWithTheNewUser(t *testing.T) {
	pool := testsupport.Postgres(t)
	repository := infrastructure.NewPostgresRepository(pool)
	ctx := context.Background()
	code := "A" + fmt.Sprintf("%042d", 1)
	insertCode(t, pool, code, invitationNow.Add(24*time.Hour), false)

	if usable, err := repository.PlatformInvitationUsable(ctx, codeHash(code), invitationNow); err != nil || !usable {
		t.Fatalf("PlatformInvitationUsable() = %v, %v", usable, err)
	}
	user, session := newUserWithSession(t, "owner@example.com", 1)
	if err := repository.CreateUserSessionWithPlatformInvitation(ctx, user, session, codeHash(code), invitationNow); err != nil {
		t.Fatalf("CreateUserSessionWithPlatformInvitation() = %v", err)
	}

	stored, found, err := repository.UserByEmail(ctx, "owner@example.com")
	if err != nil || !found || stored.OrganizationRights != 1 {
		t.Fatalf("UserByEmail() = %+v, %v, %v: право на организацию должно сохраниться", stored, found, err)
	}
	if bySession, found, err := repository.UserBySessionHash(ctx, session.TokenHash, invitationNow); err != nil || !found || bySession.OrganizationRights != 1 {
		t.Fatalf("UserBySessionHash() = %+v, %v, %v", bySession, found, err)
	}
	var consumedBy *string
	var consumedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT consumed_by_user_id::text, consumed_at FROM platform_invitations WHERE code_hash = $1`, codeHash(code)).Scan(&consumedBy, &consumedAt); err != nil {
		t.Fatal(err)
	}
	if consumedBy == nil || *consumedBy != user.ID || consumedAt == nil || !consumedAt.Equal(invitationNow) {
		t.Fatalf("код погашен как %v в %v", consumedBy, consumedAt)
	}
	if usable, _ := repository.PlatformInvitationUsable(ctx, codeHash(code), invitationNow); usable {
		t.Fatal("погашенный код всё ещё считается пригодным")
	}

	second, secondSession := newUserWithSession(t, "second@example.com", 1)
	if err := repository.CreateUserSessionWithPlatformInvitation(ctx, second, secondSession, codeHash(code), invitationNow); !errors.Is(err, domain.ErrInvitationInvalid) {
		t.Fatalf("повторное погашение = %v, want ErrInvitationInvalid", err)
	}
	if got := count(t, pool, `SELECT count(*) FROM users`); got != 1 {
		t.Fatalf("пользователей %d, ожидался один: неудачное погашение не оставляет следов", got)
	}
}

func TestUnusableCodesAreRefusedWithoutTouchingTheEmail(t *testing.T) {
	pool := testsupport.Postgres(t)
	repository := infrastructure.NewPostgresRepository(pool)
	ctx := context.Background()
	existing, existingSession := newUserWithSession(t, "taken@example.com", 0)
	if err := repository.CreateUserSession(ctx, existing, existingSession); err != nil {
		t.Fatal(err)
	}
	expired, revoked, unknown := "E"+fmt.Sprintf("%042d", 1), "R"+fmt.Sprintf("%042d", 1), "U"+fmt.Sprintf("%042d", 1)
	insertCode(t, pool, expired, invitationNow.Add(-time.Second), false)
	insertCode(t, pool, revoked, invitationNow.Add(24*time.Hour), true)
	for name, code := range map[string]string{"просрочен": expired, "отозван": revoked, "неизвестен": unknown} {
		if usable, err := repository.PlatformInvitationUsable(ctx, codeHash(code), invitationNow); err != nil || usable {
			t.Errorf("%s: PlatformInvitationUsable() = %v, %v", name, usable, err)
		}
		// Почта занята, но код недействителен: ответ обязан быть про код, иначе по нему проверяли бы адреса.
		user, session := newUserWithSession(t, "taken@example.com", 1)
		if err := repository.CreateUserSessionWithPlatformInvitation(ctx, user, session, codeHash(code), invitationNow); !errors.Is(err, domain.ErrInvitationInvalid) {
			t.Errorf("%s: %v, want ErrInvitationInvalid", name, err)
		}
	}
}

func TestEmailConflictLeavesThePlatformCodeUsable(t *testing.T) {
	pool := testsupport.Postgres(t)
	repository := infrastructure.NewPostgresRepository(pool)
	ctx := context.Background()
	existing, existingSession := newUserWithSession(t, "taken@example.com", 0)
	if err := repository.CreateUserSession(ctx, existing, existingSession); err != nil {
		t.Fatal(err)
	}
	code := "C" + fmt.Sprintf("%042d", 1)
	insertCode(t, pool, code, invitationNow.Add(time.Hour), false)
	user, session := newUserWithSession(t, "taken@example.com", 1)
	if err := repository.CreateUserSessionWithPlatformInvitation(ctx, user, session, codeHash(code), invitationNow); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("занятая почта = %v, want ErrConflict", err)
	}
	if usable, err := repository.PlatformInvitationUsable(ctx, codeHash(code), invitationNow); err != nil || !usable {
		t.Fatalf("код после конфликта: usable=%v, %v: он не должен сгорать", usable, err)
	}
	if got := count(t, pool, `SELECT count(*) FROM users`); got != 1 {
		t.Fatalf("пользователей %d, ожидался один", got)
	}
}

func TestConcurrentRedemptionsOfOneCodeCreateExactlyOneUser(t *testing.T) {
	pool := testsupport.Postgres(t)
	repository := infrastructure.NewPostgresRepository(pool)
	code := "K" + fmt.Sprintf("%042d", 1)
	insertCode(t, pool, code, invitationNow.Add(time.Hour), false)

	// Все участники гонки используют одну почту: проигравший должен услышать про код, а не про занятый адрес.
	// Блокировка строки кода до вставки пользователя даёт именно такой порядок отказов.
	const attempts = 12
	results := make(chan error, attempts)
	var group sync.WaitGroup
	for i := 0; i < attempts; i++ {
		user, session := newUserWithSession(t, "racer@example.com", 1)
		group.Add(1)
		go func() {
			defer group.Done()
			results <- repository.CreateUserSessionWithPlatformInvitation(context.Background(), user, session, codeHash(code), invitationNow)
		}()
	}
	group.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case !errors.Is(err, domain.ErrInvitationInvalid):
			t.Errorf("проигравший получил %v, want ErrInvitationInvalid", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("погасили код %d раз, ожидался один", succeeded)
	}
	if users, sessions := count(t, pool, `SELECT count(*) FROM users`), count(t, pool, `SELECT count(*) FROM sessions`); users != 1 || sessions != 1 {
		t.Fatalf("пользователей %d, сессий %d: проигравшие не должны оставлять следов", users, sessions)
	}
}

// Компенсация регистрации по коду сотрудника удаляет только «чистого» пользователя.
func TestDeleteNewUserRemovesOnlyAnUntouchedUser(t *testing.T) {
	pools := testsupport.PostgresRoles(t)
	repository := infrastructure.NewPostgresRepository(pools.Owner)
	ctx := context.Background()
	fresh, freshSession := newUserWithSession(t, "fresh@example.com", 0)
	if err := repository.CreateUserSession(ctx, fresh, freshSession); err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteNewUser(ctx, fresh.ID); err != nil {
		t.Fatalf("DeleteNewUser() = %v", err)
	}
	if users, sessions := count(t, pools.Owner, `SELECT count(*) FROM users WHERE id = $1`, fresh.ID), count(t, pools.Owner, `SELECT count(*) FROM sessions WHERE user_id = $1`, fresh.ID); users != 0 || sessions != 0 {
		t.Fatalf("после удаления пользователей %d, сессий %d", users, sessions)
	}
	if err := repository.DeleteNewUser(ctx, fresh.ID); err != nil {
		t.Fatalf("повторное удаление должно быть безопасным: %v", err)
	}

	pair := testsupport.TwoTenants(t, ctx, pools.Owner) // у владельца организации есть членство
	if err := repository.DeleteNewUser(ctx, pair.A.UserID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("удаление пользователя с членством = %v, want ErrConflict", err)
	}
	if got := count(t, pools.Owner, `SELECT count(*) FROM users WHERE id = $1`, pair.A.UserID); got != 1 {
		t.Fatal("пользователь с членством удалён")
	}
}
