package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"lidradar/backend/internal/tenant/application"
	"lidradar/backend/internal/tenant/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
	"lidradar/backend/platform/tenantctx"
)

func newTeamID(t *testing.T) string {
	t.Helper()
	value, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// Команда организации под ролью с RLS (ADR 0045): список участников, смена
// роли и отзыв с защитой последнего владельца, приглашения по одноразовому
// коду и их приём без организации в контексте запроса.
func TestPostgresTeamInvitationsAndOnboardingUnderRLS(t *testing.T) {
	pools := testsupport.PostgresRoles(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pools.Owner)
	repository := NewPostgresRepository(pools.App)
	tenantCtx := tenantctx.WithTenant(ctx, pair.A.TenantID)
	at := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

	guestID := newTeamID(t)
	if _, err := pools.Owner.Exec(ctx, `
		INSERT INTO users(id, email, password_hash, display_name, status)
		VALUES ($1, 'guest@tenant.test', '$integration-test$', 'Гость', 'ACTIVE')`, guestID); err != nil {
		t.Fatal(err)
	}

	members, err := repository.ListMembers(tenantCtx, pair.A.TenantID)
	if err != nil || len(members) != 1 || members[0].UserID != pair.A.UserID || members[0].Email != "a@tenant.test" ||
		members[0].DisplayName != "Tenant a" || members[0].Role != domain.RoleOwner || members[0].Status != domain.MembershipActive {
		t.Fatalf("ListMembers() = %#v, %v", members, err)
	}
	if _, err := repository.ChangeMemberRole(tenantCtx, pair.A.TenantID, pair.A.UserID, domain.RoleManager, at); !errors.Is(err, domain.ErrLastOwner) {
		t.Fatalf("понижение последнего владельца: %v", err)
	}
	if _, _, err := repository.RevokeMember(tenantCtx, pair.A.TenantID, pair.A.UserID, at); !errors.Is(err, domain.ErrLastOwner) {
		t.Fatalf("отзыв последнего владельца: %v", err)
	}
	if _, err := repository.ChangeMemberRole(tenantCtx, pair.A.TenantID, guestID, domain.RoleManager, at); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("смена роли не участника: %v", err)
	}

	hash := func(code string) string {
		digest := sha256.Sum256([]byte(code))
		return hex.EncodeToString(digest[:])
	}
	invitation, err := domain.NewInvitation(newTeamID(t), pair.A.TenantID, domain.RoleManager, hash("manager-code"), nil, pair.A.UserID, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateInvitation(tenantCtx, invitation, domain.AuditEntry{ID: newTeamID(t), ActorID: pair.A.UserID, Operation: "MEMBER_INVITED", EntityType: "INVITATION", At: at}); err != nil {
		t.Fatalf("CreateInvitation() = %v", err)
	}
	listed, err := repository.ListInvitations(tenantCtx, pair.A.TenantID)
	if err != nil || len(listed) != 1 || listed[0].ID != invitation.ID || listed[0].Status(at) != domain.InvitationPending ||
		listed[0].Status(at.Add(domain.InvitationTTL)) != domain.InvitationExpired {
		t.Fatalf("ListInvitations() = %#v, %v", listed, err)
	}
	if foreign, err := repository.ListInvitations(tenantctx.WithTenant(ctx, pair.B.TenantID), pair.B.TenantID); err != nil || len(foreign) != 0 {
		t.Fatalf("приглашения чужой организации = %#v, %v", foreign, err)
	}

	// Приём: без организации в контексте — строка находится по хешу кода.
	if _, err := repository.AcceptInvitation(ctx, domain.AcceptInvitationCommand{CodeHash: hash("unknown-code"), UserID: guestID, MembershipID: newTeamID(t), At: at, AuditID: newTeamID(t)}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("неизвестный код: %v", err)
	}
	if _, err := repository.AcceptInvitation(ctx, domain.AcceptInvitationCommand{CodeHash: hash("manager-code"), UserID: guestID, MembershipID: newTeamID(t), At: at.Add(domain.InvitationTTL), AuditID: newTeamID(t)}); !errors.Is(err, domain.ErrInvitationExpired) {
		t.Fatalf("просроченный код: %v", err)
	}
	account, err := repository.AcceptInvitation(ctx, domain.AcceptInvitationCommand{CodeHash: hash("manager-code"), UserID: guestID, MembershipID: newTeamID(t), At: at.Add(time.Hour), AuditID: newTeamID(t)})
	if err != nil || account.Membership.Role != domain.RoleManager || account.Membership.Status != domain.MembershipActive ||
		account.Organization.ID != pair.A.TenantID || account.Organization.Name != "Organization a" {
		t.Fatalf("AcceptInvitation() = %#v, %v", account, err)
	}
	if _, err := repository.AcceptInvitation(ctx, domain.AcceptInvitationCommand{CodeHash: hash("manager-code"), UserID: guestID, MembershipID: newTeamID(t), At: at.Add(2 * time.Hour), AuditID: newTeamID(t)}); !errors.Is(err, domain.ErrInvitationUsed) {
		t.Fatalf("повторный приём: %v", err)
	}
	permissions := application.NewPermissionService(repository)
	if allowed, err := permissions.Allowed(tenantCtx, guestID, pair.A.TenantID, application.PermissionRiskManage); err != nil || !allowed {
		t.Fatalf("новый менеджер без прав: %v, %v", allowed, err)
	}
	if allowed, _ := permissions.Allowed(tenantCtx, guestID, pair.A.TenantID, application.PermissionMemberManage); allowed {
		t.Fatal("менеджер получил member.manage")
	}
	var audits int
	if err := pools.Owner.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND operation IN ('MEMBER_INVITED', 'INVITATION_ACCEPTED')`, pair.A.TenantID).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("аудит приглашения = %d, %v", audits, err)
	}

	// Второй владелец через приглашение OWNER: теперь первого можно понизить.
	ownerInvitation, err := domain.NewInvitation(newTeamID(t), pair.A.TenantID, domain.RoleOwner, hash("owner-code"), nil, pair.A.UserID, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateInvitation(tenantCtx, ownerInvitation, domain.AuditEntry{ID: newTeamID(t), ActorID: pair.A.UserID, Operation: "MEMBER_INVITED", EntityType: "INVITATION", At: at}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.AcceptInvitation(ctx, domain.AcceptInvitationCommand{CodeHash: hash("owner-code"), UserID: guestID, MembershipID: newTeamID(t), At: at.Add(time.Hour), AuditID: newTeamID(t)}); !errors.Is(err, domain.ErrAlreadyMember) {
		t.Fatalf("активный участник принял второе приглашение: %v", err)
	}
	promoted, err := repository.ChangeMemberRole(tenantCtx, pair.A.TenantID, guestID, domain.RoleOwner, at.Add(time.Hour))
	if err != nil || promoted.Role != domain.RoleOwner {
		t.Fatalf("повышение до владельца = %#v, %v", promoted, err)
	}
	demoted, err := repository.ChangeMemberRole(tenantCtx, pair.A.TenantID, pair.A.UserID, domain.RoleManager, at.Add(time.Hour))
	if err != nil || demoted.Role != domain.RoleManager {
		t.Fatalf("понижение при двух владельцах = %#v, %v", demoted, err)
	}
	revoked, changed, err := repository.RevokeMember(tenantCtx, pair.A.TenantID, pair.A.UserID, at.Add(2*time.Hour))
	if err != nil || !changed || revoked.Status != domain.MembershipDisabled || revoked.RevokedAt == nil {
		t.Fatalf("отзыв менеджера = %#v, %v, %v", revoked, changed, err)
	}
	if _, changed, err := repository.RevokeMember(tenantCtx, pair.A.TenantID, pair.A.UserID, at.Add(3*time.Hour)); err != nil || changed {
		t.Fatalf("повторный отзыв = %v, %v", changed, err)
	}
	if _, err := repository.ChangeMemberRole(tenantCtx, pair.A.TenantID, pair.A.UserID, domain.RoleOwner, at.Add(3*time.Hour)); !errors.Is(err, domain.ErrMemberDisabled) {
		t.Fatalf("смена роли отозванного: %v", err)
	}
	// Отозванное членство восстанавливается приглашением с новой ролью.
	if _, err := repository.AcceptInvitation(ctx, domain.AcceptInvitationCommand{CodeHash: hash("owner-code"), UserID: pair.A.UserID, MembershipID: newTeamID(t), At: at.Add(4 * time.Hour), AuditID: newTeamID(t)}); err != nil {
		t.Fatalf("восстановление членства: %v", err)
	}
	restored, found, err := repository.Membership(tenantCtx, pair.A.TenantID, pair.A.UserID)
	if err != nil || !found || restored.Status != domain.MembershipActive || restored.Role != domain.RoleOwner || restored.RevokedAt != nil {
		t.Fatalf("восстановленное членство = %#v, %v, %v", restored, found, err)
	}

	// Отзыв приглашения: ожидающее отзывается, принятое — нет.
	pending, err := domain.NewInvitation(newTeamID(t), pair.A.TenantID, domain.RoleManager, hash("pending-code"), nil, guestID, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateInvitation(tenantCtx, pending, domain.AuditEntry{ID: newTeamID(t), ActorID: guestID, Operation: "MEMBER_INVITED", EntityType: "INVITATION", At: at}); err != nil {
		t.Fatal(err)
	}
	revokedInvitation, err := repository.RevokeInvitation(tenantCtx, pair.A.TenantID, pending.ID, guestID, at.Add(time.Hour), domain.AuditEntry{ID: newTeamID(t), ActorID: guestID, Operation: "INVITATION_REVOKED", EntityType: "INVITATION", At: at.Add(time.Hour)})
	if err != nil || revokedInvitation.Status(at.Add(time.Hour)) != domain.InvitationRevoked || revokedInvitation.RevokedBy == nil || *revokedInvitation.RevokedBy != guestID {
		t.Fatalf("RevokeInvitation() = %#v, %v", revokedInvitation, err)
	}
	if _, err := repository.RevokeInvitation(tenantCtx, pair.A.TenantID, pending.ID, guestID, at.Add(2*time.Hour), domain.AuditEntry{ID: newTeamID(t), ActorID: guestID, Operation: "INVITATION_REVOKED", EntityType: "INVITATION", At: at.Add(2 * time.Hour)}); err != nil {
		t.Fatalf("повторный отзыв приглашения: %v", err)
	}
	if _, err := repository.AcceptInvitation(ctx, domain.AcceptInvitationCommand{CodeHash: hash("pending-code"), UserID: newTeamID(t), MembershipID: newTeamID(t), At: at.Add(3 * time.Hour), AuditID: newTeamID(t)}); !errors.Is(err, domain.ErrInvitationRevoked) {
		t.Fatalf("приём отозванного: %v", err)
	}
	if _, err := repository.RevokeInvitation(tenantCtx, pair.A.TenantID, invitation.ID, guestID, at.Add(time.Hour), domain.AuditEntry{ID: newTeamID(t), ActorID: guestID, Operation: "INVITATION_REVOKED", EntityType: "INVITATION", At: at.Add(time.Hour)}); !errors.Is(err, domain.ErrInvitationUsed) {
		t.Fatalf("отзыв принятого: %v", err)
	}
	if _, err := repository.RevokeInvitation(tenantctx.WithTenant(ctx, pair.B.TenantID), pair.B.TenantID, invitation.ID, pair.B.UserID, at, domain.AuditEntry{ID: newTeamID(t), ActorID: pair.B.UserID, Operation: "INVITATION_REVOKED", EntityType: "INVITATION", At: at}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("чужое приглашение отозвано: %v", err)
	}

	// Онбординг: точка без графика не считается настроенной.
	facts, err := repository.OnboardingFacts(tenantCtx, pair.A.TenantID, pair.A.UserID)
	if err != nil || facts != (domain.OnboardingFacts{ActiveLocations: 1}) {
		t.Fatalf("факты онбординга = %#v, %v", facts, err)
	}
	status := domain.OnboardingFrom(facts, at)
	if status.Complete || status.NextStep == nil || *status.NextStep != domain.OnboardingStepLocation || len(status.Steps) != 5 || !status.Steps[0].Done {
		t.Fatalf("статус онбординга = %#v", status)
	}
	for weekday := 1; weekday <= 7; weekday++ {
		if _, err := pools.Owner.Exec(ctx, `
			INSERT INTO location_business_hours(id, tenant_id, location_id, weekday, is_closed, opens_at, closes_at)
			VALUES ($1, $2, $3, $4, FALSE, '09:00', '21:00')`, newTeamID(t), pair.A.TenantID, pair.A.LocationID, weekday); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pools.Owner.Exec(ctx, `
		INSERT INTO service_catalog_items(id, tenant_id, location_id, name, normalized_name, currency, active, created_at, updated_at)
		VALUES ($1, $2, NULL, 'Полировка', 'полировка', 'RUB', true, $3, $3), ($4, $2, NULL, 'Старая', 'старая', 'RUB', false, $3, $3)`,
		newTeamID(t), pair.A.TenantID, at, newTeamID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := pools.Owner.Exec(ctx, `
		INSERT INTO channel_connections(id, tenant_id, location_id, provider, name, status, capabilities, verification_secret_hash, created_at, updated_at)
		VALUES ($1, $2, $3, 'TEST', 'Отключён', 'DISCONNECTED', '["CAN_RECEIVE_MESSAGES"]'::jsonb, repeat('0', 64), $4, $4)`,
		newTeamID(t), pair.A.TenantID, pair.A.LocationID, at); err != nil {
		t.Fatal(err)
	}
	facts, err = repository.OnboardingFacts(tenantCtx, pair.A.TenantID, pair.A.UserID)
	if err != nil || facts != (domain.OnboardingFacts{ActiveLocations: 1, LocationsWithSchedule: 1, ActiveServices: 1, Connections: 1}) {
		t.Fatalf("факты после настройки = %#v, %v", facts, err)
	}
	if status := domain.OnboardingFrom(facts, at); status.Complete || *status.NextStep != domain.OnboardingStepChannel {
		t.Fatalf("статус без живого канала = %#v", status)
	}
	if _, err := pools.Owner.Exec(ctx, `UPDATE channel_connections SET status = 'ACTIVE' WHERE tenant_id = $1`, pair.A.TenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := pools.Owner.Exec(ctx, `
		INSERT INTO telegram_user_links(id, tenant_id, user_id, telegram_user_id, chat_id, linked_at, updated_at)
		VALUES ($1, $2, $3, 42, 42, $4, $4)`, newTeamID(t), pair.A.TenantID, pair.A.UserID, at); err != nil {
		t.Fatal(err)
	}
	facts, err = repository.OnboardingFacts(tenantCtx, pair.A.TenantID, pair.A.UserID)
	if err != nil || !facts.TelegramLinked || facts.LiveConnections != 1 {
		t.Fatalf("факты с живым каналом = %#v, %v", facts, err)
	}
	if status := domain.OnboardingFrom(facts, at); !status.Complete || status.NextStep != nil {
		t.Fatalf("завершённый онбординг = %#v", status)
	}
	otherFacts, err := repository.OnboardingFacts(tenantCtx, pair.A.TenantID, guestID)
	if err != nil || otherFacts.TelegramLinked {
		t.Fatalf("привязка Telegram чужого пользователя = %#v, %v", otherFacts, err)
	}
	if status := domain.OnboardingFrom(otherFacts, at); !status.Complete || status.NextStep == nil || *status.NextStep != domain.OnboardingStepTelegramLink {
		t.Fatalf("необязательный шаг = %#v", status)
	}
}
