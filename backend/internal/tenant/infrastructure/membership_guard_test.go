package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"lidradar/backend/internal/tenant/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
	"lidradar/backend/platform/tenantctx"
)

// Членство создаётся только для действующего пользователя и в порядке очереди с обезличиванием учётной записи
// (ADR 0059): принятое в эту минуту приглашение не оставит активное членство у закрытого пользователя, которое
// считалось бы активным владельцем и мешало защите последнего владельца.

func newMember(t *testing.T, tenantID, userID string, role domain.Role, at time.Time) domain.Membership {
	t.Helper()
	id, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	return domain.Membership{ID: id, TenantID: tenantID, UserID: userID, Role: role, Status: domain.MembershipActive, CreatedAt: at, UpdatedAt: at}
}

func TestCreateMembershipRefusesAClosedOrUnknownUser(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	repository := NewPostgresRepository(pool)
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

	newUser, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id, email, password_hash, display_name, status) VALUES ($1, 'late@example.test', 'x', 'Поздний', 'ACTIVE')`, newUser); err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateMembership(ctx, pair.A.TenantID, newMember(t, pair.A.TenantID, newUser, domain.RoleManager, at)); err != nil {
		t.Fatalf("действующий пользователь получил отказ: %v", err)
	}

	// Закрытый пользователь членства не получает, неизвестный тоже.
	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'DISABLED', deleted_at = now() WHERE id = $1`, newUser); err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateMembership(ctx, pair.B.TenantID, newMember(t, pair.B.TenantID, newUser, domain.RoleOwner, at)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("закрытый пользователь: error = %v, want ErrNotFound", err)
	}
	stranger, _ := (ids.Generator{}).NewID()
	if err := repository.CreateMembership(ctx, pair.B.TenantID, newMember(t, pair.B.TenantID, stranger, domain.RoleManager, at)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("неизвестный пользователь: error = %v, want ErrNotFound", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE tenant_id = $1 AND user_id IN ($2, $3)`, pair.B.TenantID, newUser, stranger).Scan(&count); err != nil || count != 0 {
		t.Fatalf("членства закрытого пользователя: %d, %v", count, err)
	}
	// Повторное членство в той же организации по-прежнему конфликт, а не «не найдено».
	if err := repository.CreateMembership(ctx, pair.A.TenantID, newMember(t, pair.A.TenantID, pair.A.UserID, domain.RoleManager, at)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("дубликат: error = %v, want ErrConflict", err)
	}
}

func TestMembershipCreationWaitsForAClosingTransaction(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	repository := NewPostgresRepository(pool)
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	invitee, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id, email, password_hash, display_name, status) VALUES ($1, 'invitee@example.test', 'x', 'Приглашённый', 'ACTIVE')`, invitee); err != nil {
		t.Fatal(err)
	}
	// Исполнитель обезличивания держит строку пользователя FOR UPDATE до конца транзакции.
	closing, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closing.Rollback(ctx) }()
	var locked string
	if err := closing.QueryRow(ctx, `SELECT id::text FROM users WHERE id = $1 FOR UPDATE`, invitee).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- repository.CreateMembership(ctx, pair.A.TenantID, newMember(t, pair.A.TenantID, invitee, domain.RoleOwner, at))
	}()
	select {
	case err := <-finished:
		t.Fatalf("членство создано, пока строка пользователя заблокирована: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := closing.Exec(ctx, `UPDATE users SET status = 'DISABLED', deleted_at = now() WHERE id = $1`, invitee); err != nil {
		t.Fatal(err)
	}
	if err := closing.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("членство для закрытого пользователя: error = %v, want ErrNotFound", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("создание членства не завершилось после снятия блокировки")
	}
}

func TestListMembersHidesTheAddressOfAClosedAccount(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	repository := NewPostgresRepository(pool)
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	colleague, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id, email, password_hash, display_name, status) VALUES ($1, 'colleague@example.test', 'x', 'Коллега', 'ACTIVE')`, colleague); err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateMembership(ctx, pair.A.TenantID, newMember(t, pair.A.TenantID, colleague, domain.RoleManager, at)); err != nil {
		t.Fatal(err)
	}
	// Учётную запись обезличивают так же, как исполнитель: служебный адрес, имя «Удалён», членство отозвано.
	if _, err := pool.Exec(ctx, `UPDATE users SET email = id::text || '@deleted.invalid', display_name = 'Удалён', status = 'DISABLED', deleted_at = now() WHERE id = $1`, colleague); err != nil {
		t.Fatal(err)
	}
	if revoked, err := repository.RevokeMembership(ctx, pair.A.TenantID, colleague, at.Add(time.Minute)); err != nil || !revoked {
		t.Fatal(revoked, err)
	}
	members, err := repository.ListMembers(ctx, pair.A.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]domain.Member{}
	for _, member := range members {
		found[member.UserID] = member
	}
	closed, owner := found[colleague], found[pair.A.UserID]
	if closed.Email != "" || closed.DisplayName != "Удалён" || closed.Status != domain.MembershipDisabled || closed.RevokedAt == nil {
		t.Errorf("закрытый участник = %+v", closed)
	}
	if owner.Email != "a@tenant.test" {
		t.Errorf("активный участник потерял адрес: %+v", owner)
	}
}

func invitationCodeHash(code string) string {
	digest := sha256.Sum256([]byte(code))
	return hex.EncodeToString(digest[:])
}

// restoreFixture готовит организацию, в которой у бывшего участника отозвано членство, и ожидающее приглашение.
// Приглашение принимается через восстановление отозванного членства: ветка, которой нет у вставки нового.
type restoreFixture struct {
	pools         testsupport.Pools
	repository    *PostgresRepository
	tenantID      string
	former        string
	code          string
	acceptCommand domain.AcceptInvitationCommand
}

func newRestoreFixture(t *testing.T) restoreFixture {
	t.Helper()
	pools := testsupport.PostgresRoles(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pools.Owner)
	repository := NewPostgresRepository(pools.App)
	tenantCtx := tenantctx.WithTenant(ctx, pair.A.TenantID)
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

	former := newTeamID(t)
	if _, err := pools.Owner.Exec(ctx, `INSERT INTO users(id, email, password_hash, display_name, status) VALUES ($1, 'former@example.test', 'x', 'Бывший', 'ACTIVE')`, former); err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateMembership(tenantCtx, pair.A.TenantID, newMember(t, pair.A.TenantID, former, domain.RoleManager, at)); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := repository.RevokeMember(tenantCtx, pair.A.TenantID, former, at.Add(time.Minute)); err != nil || !changed {
		t.Fatalf("отзыв членства: %v, %v", changed, err)
	}
	const code = "restore-code"
	invitation, err := domain.NewInvitation(newTeamID(t), pair.A.TenantID, domain.RoleManager, invitationCodeHash(code), nil, pair.A.UserID, at.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateInvitation(tenantCtx, invitation, domain.AuditEntry{ID: newTeamID(t), ActorID: pair.A.UserID, Operation: "MEMBER_INVITED", EntityType: "INVITATION", At: at}); err != nil {
		t.Fatal(err)
	}
	return restoreFixture{
		pools: pools, repository: repository, tenantID: pair.A.TenantID, former: former, code: code,
		acceptCommand: domain.AcceptInvitationCommand{
			CodeHash: invitationCodeHash(code), UserID: former, MembershipID: newTeamID(t), At: at.Add(time.Hour), AuditID: newTeamID(t),
		},
	}
}

// requireUntouched проверяет, что отказ не оставил следов: членство всё ещё отозвано, приглашение не принято.
func (f restoreFixture) requireUntouched(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var status string
	var revoked bool
	if err := f.pools.Owner.QueryRow(ctx, `SELECT status, revoked_at IS NOT NULL FROM memberships WHERE tenant_id = $1 AND user_id = $2`, f.tenantID, f.former).Scan(&status, &revoked); err != nil {
		t.Fatal(err)
	}
	if status != "DISABLED" || !revoked {
		t.Errorf("членство закрытого пользователя: status = %s, revoked = %v", status, revoked)
	}
	var accepted int
	if err := f.pools.Owner.QueryRow(ctx, `SELECT count(*) FROM membership_invitations WHERE tenant_id = $1 AND accepted_at IS NOT NULL`, f.tenantID).Scan(&accepted); err != nil || accepted != 0 {
		t.Errorf("принятых приглашений: %d, %v", accepted, err)
	}
	var audits int
	if err := f.pools.Owner.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND operation = 'INVITATION_ACCEPTED'`, f.tenantID).Scan(&audits); err != nil || audits != 0 {
		t.Errorf("записей приёма: %d, %v", audits, err)
	}
}

func TestAcceptInvitationDoesNotRestoreAMembershipOfAClosedUser(t *testing.T) {
	f := newRestoreFixture(t)
	ctx := context.Background()

	// Пока пользователь действующий, приём восстановил бы отозванное членство (проверено в основном тесте команды).
	if _, err := f.pools.Owner.Exec(ctx, `UPDATE users SET email = id::text || '@deleted.invalid', display_name = 'Удалён', status = 'DISABLED', deleted_at = now() WHERE id = $1`, f.former); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repository.AcceptInvitation(ctx, f.acceptCommand); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("приём для закрытого пользователя: error = %v, want ErrNotFound", err)
	}
	f.requireUntouched(t)

	// Порядок ответов прежний: неизвестный код остаётся «не найден», а использованный код — «использован».
	unknown := f.acceptCommand
	unknown.CodeHash = invitationCodeHash("unknown-code")
	if _, err := f.repository.AcceptInvitation(ctx, unknown); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("неизвестный код: %v", err)
	}
}

func TestAcceptInvitationWaitsForAClosingTransaction(t *testing.T) {
	f := newRestoreFixture(t)
	ctx := context.Background()
	closing, err := f.pools.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closing.Rollback(ctx) }()
	var locked string
	if err := closing.QueryRow(ctx, `SELECT id::text FROM users WHERE id = $1 FOR UPDATE`, f.former).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := f.repository.AcceptInvitation(ctx, f.acceptCommand)
		finished <- err
	}()
	select {
	case err := <-finished:
		t.Fatalf("приглашение принято, пока строка пользователя заблокирована: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	// Исполнитель обезличивания: отзывает членства и закрывает пользователя, затем фиксирует.
	if _, err := closing.Exec(ctx, `UPDATE users SET status = 'DISABLED', deleted_at = now() WHERE id = $1`, f.former); err != nil {
		t.Fatal(err)
	}
	if err := closing.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("приём после закрытия: error = %v, want ErrNotFound", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("приём не завершился после снятия блокировки")
	}
	f.requireUntouched(t)
}
