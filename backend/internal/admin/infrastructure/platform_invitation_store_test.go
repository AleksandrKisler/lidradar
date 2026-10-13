package infrastructure

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/admin/application"
	"lidradar/backend/internal/admin/domain"
	"lidradar/backend/internal/testsupport"
)

// Коды платформы и право на организацию на настоящей PostgreSQL (ADR 0057): каждая команда пишет аудит
// в той же транзакции, повторный отзыв ничего не пишет, использованный код отозвать нельзя.

var invitationStoreNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func invitationAudit(t *testing.T, operation, entityType, entityID string) domain.AuditEntry {
	t.Helper()
	return domain.AuditEntry{
		ID: newID(t), Source: domain.SourceCLI, Operation: operation, EntityType: entityType, EntityID: entityID,
		Details: map[string]any{}, At: invitationStoreNow,
	}
}

func newInvitation(t *testing.T, hashSeed string, note string) domain.PlatformInvitation {
	t.Helper()
	invitation, err := domain.NewPlatformInvitation(newID(t), strings.Repeat(hashSeed, 64)[:64], note, 14, invitationStoreNow)
	if err != nil {
		t.Fatal(err)
	}
	return invitation
}

func auditCount(t *testing.T, store *PostgresStore, operation string) int {
	t.Helper()
	var count int
	if err := store.pool.QueryRow(context.Background(), `SELECT count(*) FROM admin_audit_log WHERE operation = $1`, operation).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestPostgresPlatformInvitationLifecycleWritesAuditAtomically(t *testing.T) {
	pool := testsupport.Postgres(t)
	store := NewPostgresStore(pool)
	ctx := context.Background()

	invitation := newInvitation(t, "a", "Салон «Линия»")
	if err := store.CreatePlatformInvitation(ctx, invitation, invitationAudit(t, "PLATFORM_INVITATION_ISSUED", "PLATFORM_INVITATION", invitation.ID)); err != nil {
		t.Fatalf("CreatePlatformInvitation() = %v", err)
	}
	listed, err := store.PlatformInvitations(ctx)
	if err != nil || len(listed) != 1 || listed[0].ID != invitation.ID || listed[0].Note == nil || *listed[0].Note != "Салон «Линия»" ||
		listed[0].Status(invitationStoreNow) != domain.PlatformInvitationPending || !listed[0].ExpiresAt.Equal(invitation.ExpiresAt) {
		t.Fatalf("PlatformInvitations() = %+v, %v", listed, err)
	}
	if got := auditCount(t, store, "PLATFORM_INVITATION_ISSUED"); got != 1 {
		t.Fatalf("записей аудита о выпуске %d, ожидалась одна", got)
	}

	duplicate := newInvitation(t, "a", "")
	if err := store.CreatePlatformInvitation(ctx, duplicate, invitationAudit(t, "PLATFORM_INVITATION_ISSUED", "PLATFORM_INVITATION", duplicate.ID)); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("тот же хеш кода = %v, want ErrConflict", err)
	}
	if got := auditCount(t, store, "PLATFORM_INVITATION_ISSUED"); got != 1 {
		t.Fatalf("неудачный выпуск оставил аудит: записей %d", got)
	}

	revokedAt := invitationStoreNow.Add(time.Hour)
	revoked, err := store.RevokePlatformInvitation(ctx, invitation.ID, revokedAt, invitationAudit(t, "PLATFORM_INVITATION_REVOKED", "PLATFORM_INVITATION", invitation.ID))
	if err != nil || revoked.RevokedAt == nil || revoked.Status(revokedAt) != domain.PlatformInvitationRevoked {
		t.Fatalf("RevokePlatformInvitation() = %+v, %v", revoked, err)
	}
	again, err := store.RevokePlatformInvitation(ctx, invitation.ID, revokedAt.Add(time.Hour), invitationAudit(t, "PLATFORM_INVITATION_REVOKED", "PLATFORM_INVITATION", invitation.ID))
	if err != nil || again.RevokedAt == nil || !again.RevokedAt.Equal(revokedAt) {
		t.Fatalf("повторный отзыв = %+v, %v: время отзыва не должно меняться", again, err)
	}
	if got := auditCount(t, store, "PLATFORM_INVITATION_REVOKED"); got != 1 {
		t.Fatalf("записей об отзыве %d, ожидалась одна: повтор не пишет аудит", got)
	}
	if _, err := store.RevokePlatformInvitation(ctx, newID(t), revokedAt, invitationAudit(t, "PLATFORM_INVITATION_REVOKED", "PLATFORM_INVITATION", newID(t))); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("неизвестный код = %v, want ErrNotFound", err)
	}
	if _, err := store.RevokePlatformInvitation(ctx, "not-a-uuid", revokedAt, invitationAudit(t, "PLATFORM_INVITATION_REVOKED", "PLATFORM_INVITATION", newID(t))); !errors.Is(err, application.ErrInvalid) {
		t.Fatalf("неверный идентификатор = %v, want ErrInvalid", err)
	}
}

func TestPostgresConsumedPlatformInvitationCannotBeRevoked(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	store := NewPostgresStore(pool)
	invitation := newInvitation(t, "b", "")
	if err := store.CreatePlatformInvitation(ctx, invitation, invitationAudit(t, "PLATFORM_INVITATION_ISSUED", "PLATFORM_INVITATION", invitation.ID)); err != nil {
		t.Fatal(err)
	}
	// Погашает код модуль identity при регистрации; здесь воспроизводится его запись.
	if _, err := pool.Exec(ctx, `UPDATE platform_invitations SET consumed_at = $2, consumed_by_user_id = $3 WHERE id = $1`,
		invitation.ID, invitationStoreNow, pair.A.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RevokePlatformInvitation(ctx, invitation.ID, invitationStoreNow, invitationAudit(t, "PLATFORM_INVITATION_REVOKED", "PLATFORM_INVITATION", invitation.ID)); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("отзыв использованного кода = %v, want ErrConflict", err)
	}
	if got := auditCount(t, store, "PLATFORM_INVITATION_REVOKED"); got != 0 {
		t.Fatalf("отказ оставил аудит: записей %d", got)
	}
	listed, err := store.PlatformInvitations(ctx)
	if err != nil || len(listed) != 1 || listed[0].Status(invitationStoreNow) != domain.PlatformInvitationUsed || listed[0].ConsumedBy == nil || *listed[0].ConsumedBy != pair.A.UserID {
		t.Fatalf("использованный код в списке: %+v, %v", listed, err)
	}
}

// Аудит и команда — одна транзакция: если запись аудита отвергнута, выпущенный код не появляется.
func TestPostgresInvitationIssueRollsBackWhenTheAuditIsRejected(t *testing.T) {
	pool := testsupport.Postgres(t)
	store := NewPostgresStore(pool)
	ctx := context.Background()
	invitation := newInvitation(t, "c", "")
	broken := invitationAudit(t, "PLATFORM_INVITATION_ISSUED", "PLATFORM_INVITATION", invitation.ID)
	broken.ID = "" // запись аудита невалидна
	if err := store.CreatePlatformInvitation(ctx, invitation, broken); err == nil {
		t.Fatal("выпуск с невалидным аудитом прошёл")
	}
	if listed, err := store.PlatformInvitations(ctx); err != nil || len(listed) != 0 {
		t.Fatalf("после отката в списке %+v, %v", listed, err)
	}
}

func TestPostgresOrganizationRightGrantsOneUnitAtATimeUpToTheLimit(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	store := NewPostgresStore(pool)

	for want := 1; want <= 3; want++ {
		rights, err := store.GrantOrganizationRight(ctx, pair.A.UserID, invitationAudit(t, "ORGANIZATION_RIGHT_GRANTED", "USER", pair.A.UserID))
		if err != nil || rights != want {
			t.Fatalf("выдача %d: прав %d, %v", want, rights, err)
		}
	}
	if got := auditCount(t, store, "ORGANIZATION_RIGHT_GRANTED"); got != 3 {
		t.Fatalf("записей аудита %d, ожидалось три", got)
	}
	var other int
	if err := pool.QueryRow(ctx, `SELECT organization_rights FROM users WHERE id = $1`, pair.B.UserID).Scan(&other); err != nil || other != 0 {
		t.Fatalf("у другого пользователя прав %d, %v: выдача не должна его затрагивать", other, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET organization_rights = $2 WHERE id = $1`, pair.A.UserID, domain.MaxOrganizationRights); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantOrganizationRight(ctx, pair.A.UserID, invitationAudit(t, "ORGANIZATION_RIGHT_GRANTED", "USER", pair.A.UserID)); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("выдача сверх предела = %v, want ErrConflict", err)
	}
	if got := auditCount(t, store, "ORGANIZATION_RIGHT_GRANTED"); got != 3 {
		t.Fatalf("отказ сверх предела оставил аудит: записей %d", got)
	}
	if _, err := store.GrantOrganizationRight(ctx, newID(t), invitationAudit(t, "ORGANIZATION_RIGHT_GRANTED", "USER", newID(t))); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("неизвестный пользователь = %v, want ErrNotFound", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'DISABLED' WHERE id = $1`, pair.B.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantOrganizationRight(ctx, pair.B.UserID, invitationAudit(t, "ORGANIZATION_RIGHT_GRANTED", "USER", pair.B.UserID)); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("отключённый пользователь = %v, want ErrNotFound: право выдают только действующим", err)
	}
}
