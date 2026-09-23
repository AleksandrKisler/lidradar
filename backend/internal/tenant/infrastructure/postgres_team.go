package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"lidradar/backend/internal/tenant/domain"
)

// Команда организации, приглашения и статус онбординга (ADR 0045). Список
// участников читает таблицу users модуля identity: адрес почты и имя нужны
// экрану команды, а собственной копии учётных данных у модуля tenant нет.

const invitationColumns = `id, tenant_id, role, code_hash, note, created_by_user_id::text, created_at, expires_at,
	accepted_at, accepted_by_user_id::text, revoked_at, revoked_by_user_id::text`

func (r *PostgresRepository) ListMembers(ctx context.Context, tenantID string) ([]domain.Member, error) {
	if r == nil || r.pool == nil || tenantID == "" {
		return nil, domain.ErrInvalid
	}
	rows, err := r.pool.Query(ctx, `
		SELECT m.id, m.user_id, u.email, u.display_name, m.role, m.status, m.revoked_at, m.created_at, m.updated_at
		FROM memberships AS m
		JOIN users AS u ON u.id = m.user_id
		WHERE m.tenant_id = $1
		ORDER BY m.created_at, m.id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()
	members := make([]domain.Member, 0)
	for rows.Next() {
		var member domain.Member
		if err := rows.Scan(&member.MembershipID, &member.UserID, &member.Email, &member.DisplayName, &member.Role,
			&member.Status, &member.RevokedAt, &member.CreatedAt, &member.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate members: %w", err)
	}
	return members, nil
}

// lockedMemberships блокирует все членства организации на время транзакции:
// защита последнего владельца требует согласованного счётчика.
type lockedMemberships struct {
	target       domain.Membership
	found        bool
	activeOwners int
}

func lockMemberships(ctx context.Context, tx pgx.Tx, tenantID, userID string) (lockedMemberships, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, tenant_id, user_id, role, status, revoked_at, created_at, updated_at
		FROM memberships WHERE tenant_id = $1 ORDER BY id FOR UPDATE`, tenantID)
	if err != nil {
		return lockedMemberships{}, fmt.Errorf("lock memberships: %w", err)
	}
	defer rows.Close()
	var locked lockedMemberships
	for rows.Next() {
		membership, _, err := scanMembership(rows)
		if err != nil {
			return lockedMemberships{}, err
		}
		if membership.Role == domain.RoleOwner && membership.Status == domain.MembershipActive {
			locked.activeOwners++
		}
		if membership.UserID == userID {
			locked.target, locked.found = membership, true
		}
	}
	if err := rows.Err(); err != nil {
		return lockedMemberships{}, fmt.Errorf("iterate locked memberships: %w", err)
	}
	return locked, nil
}

func (r *PostgresRepository) ChangeMemberRole(ctx context.Context, tenantID, userID string, role domain.Role, at time.Time) (domain.Membership, error) {
	if r == nil || r.pool == nil || tenantID == "" || userID == "" || at.IsZero() || (role != domain.RoleOwner && role != domain.RoleManager) {
		return domain.Membership{}, domain.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.Membership{}, fmt.Errorf("begin role change: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	locked, err := lockMemberships(ctx, tx, tenantID, userID)
	if err != nil {
		return domain.Membership{}, err
	}
	switch {
	case !locked.found:
		return domain.Membership{}, domain.ErrNotFound
	case locked.target.Status != domain.MembershipActive:
		return domain.Membership{}, domain.ErrMemberDisabled
	case locked.target.Role == role:
		return locked.target, nil
	case locked.target.Role == domain.RoleOwner && locked.activeOwners <= 1:
		return domain.Membership{}, domain.ErrLastOwner
	}
	membership, found, err := scanMembership(tx.QueryRow(ctx, `
		UPDATE memberships SET role = $3, updated_at = $4
		WHERE tenant_id = $1 AND user_id = $2
		RETURNING id, tenant_id, user_id, role, status, revoked_at, created_at, updated_at`,
		tenantID, userID, role, at.UTC()))
	if err != nil {
		return domain.Membership{}, mapPostgresError("change member role", err)
	}
	if !found {
		return domain.Membership{}, domain.ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Membership{}, fmt.Errorf("commit role change: %w", err)
	}
	return membership, nil
}

func (r *PostgresRepository) RevokeMember(ctx context.Context, tenantID, userID string, at time.Time) (domain.Membership, bool, error) {
	if r == nil || r.pool == nil || tenantID == "" || userID == "" || at.IsZero() {
		return domain.Membership{}, false, domain.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.Membership{}, false, fmt.Errorf("begin member revocation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	locked, err := lockMemberships(ctx, tx, tenantID, userID)
	if err != nil {
		return domain.Membership{}, false, err
	}
	switch {
	case !locked.found:
		return domain.Membership{}, false, domain.ErrNotFound
	case locked.target.Status == domain.MembershipDisabled:
		return locked.target, false, nil
	case locked.target.Role == domain.RoleOwner && locked.target.Status == domain.MembershipActive && locked.activeOwners <= 1:
		return domain.Membership{}, false, domain.ErrLastOwner
	}
	membership, found, err := scanMembership(tx.QueryRow(ctx, `
		UPDATE memberships SET status = $3, revoked_at = $4, updated_at = $4
		WHERE tenant_id = $1 AND user_id = $2
		RETURNING id, tenant_id, user_id, role, status, revoked_at, created_at, updated_at`,
		tenantID, userID, domain.MembershipDisabled, at.UTC()))
	if err != nil {
		return domain.Membership{}, false, mapPostgresError("revoke member", err)
	}
	if !found {
		return domain.Membership{}, false, domain.ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Membership{}, false, fmt.Errorf("commit member revocation: %w", err)
	}
	return membership, true, nil
}

func (r *PostgresRepository) CreateInvitation(ctx context.Context, invitation domain.Invitation, audit domain.AuditEntry) error {
	if r == nil || r.pool == nil || invitation.Validate() != nil || audit.ID == "" {
		return domain.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin invitation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO membership_invitations(id, tenant_id, role, code_hash, note, created_by_user_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		invitation.ID, invitation.TenantID, invitation.Role, invitation.CodeHash, invitation.Note,
		invitation.CreatedBy, invitation.CreatedAt, invitation.ExpiresAt); err != nil {
		return mapPostgresError("insert invitation", err)
	}
	if err := insertTenantAudit(ctx, tx, invitation.TenantID, audit, invitation.ID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit invitation: %w", err)
	}
	return nil
}

func (r *PostgresRepository) ListInvitations(ctx context.Context, tenantID string) ([]domain.Invitation, error) {
	if r == nil || r.pool == nil || tenantID == "" {
		return nil, domain.ErrInvalid
	}
	rows, err := r.pool.Query(ctx, `SELECT `+invitationColumns+`
		FROM membership_invitations WHERE tenant_id = $1 ORDER BY created_at DESC, id DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}
	defer rows.Close()
	invitations := make([]domain.Invitation, 0)
	for rows.Next() {
		invitation, err := scanInvitation(rows)
		if err != nil {
			return nil, err
		}
		invitations = append(invitations, invitation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate invitations: %w", err)
	}
	return invitations, nil
}

func (r *PostgresRepository) RevokeInvitation(ctx context.Context, tenantID, invitationID, actorID string, at time.Time, audit domain.AuditEntry) (domain.Invitation, error) {
	if r == nil || r.pool == nil || tenantID == "" || invitationID == "" || actorID == "" || at.IsZero() || audit.ID == "" {
		return domain.Invitation{}, domain.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.Invitation{}, fmt.Errorf("begin invitation revocation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	invitation, err := scanInvitation(tx.QueryRow(ctx, `SELECT `+invitationColumns+`
		FROM membership_invitations WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, invitationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Invitation{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Invitation{}, mapPostgresError("lock invitation", err)
	}
	switch invitation.Status(at) {
	case domain.InvitationAccepted:
		return domain.Invitation{}, domain.ErrInvitationUsed
	case domain.InvitationRevoked:
		return invitation, nil
	}
	invitation, err = scanInvitation(tx.QueryRow(ctx, `
		UPDATE membership_invitations SET revoked_at = $3, revoked_by_user_id = $4
		WHERE tenant_id = $1 AND id = $2
		RETURNING `+invitationColumns, tenantID, invitationID, at.UTC(), actorID))
	if err != nil {
		return domain.Invitation{}, mapPostgresError("revoke invitation", err)
	}
	if err := insertTenantAudit(ctx, tx, tenantID, audit, invitationID); err != nil {
		return domain.Invitation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Invitation{}, fmt.Errorf("commit invitation revocation: %w", err)
	}
	return invitation, nil
}

// AcceptInvitation работает без организации в контексте запроса: строка
// приглашения открывается политикой invitation_by_code по хешу, заданному на
// время транзакции, затем контекст организации задаётся локально для
// остальных таблиц (миграция 000022).
func (r *PostgresRepository) AcceptInvitation(ctx context.Context, command domain.AcceptInvitationCommand) (domain.AccountMembership, error) {
	if r == nil || r.pool == nil || len(command.CodeHash) != 64 || command.UserID == "" || command.MembershipID == "" ||
		command.At.IsZero() || command.AuditID == "" {
		return domain.AccountMembership{}, domain.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.AccountMembership{}, fmt.Errorf("begin invitation acceptance: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('lidradar.invitation_code_hash', $1, true)`, command.CodeHash); err != nil {
		return domain.AccountMembership{}, fmt.Errorf("scope invitation lookup: %w", err)
	}
	var invitationID, tenantID string
	err = tx.QueryRow(ctx, `SELECT id, tenant_id FROM membership_invitations WHERE code_hash = $1`, command.CodeHash).Scan(&invitationID, &tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AccountMembership{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.AccountMembership{}, fmt.Errorf("find invitation: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('lidradar.tenant_id', $1, true)`, tenantID); err != nil {
		return domain.AccountMembership{}, fmt.Errorf("scope invitation tenant: %w", err)
	}
	invitation, err := scanInvitation(tx.QueryRow(ctx, `SELECT `+invitationColumns+`
		FROM membership_invitations WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, invitationID))
	if err != nil {
		return domain.AccountMembership{}, mapPostgresError("lock invitation", err)
	}
	switch invitation.Status(command.At) {
	case domain.InvitationAccepted:
		return domain.AccountMembership{}, domain.ErrInvitationUsed
	case domain.InvitationRevoked:
		return domain.AccountMembership{}, domain.ErrInvitationRevoked
	case domain.InvitationExpired:
		return domain.AccountMembership{}, domain.ErrInvitationExpired
	}
	organization, found, err := scanOrganization(tx.QueryRow(ctx, `
		SELECT id, name, default_timezone, default_currency, status, created_at, updated_at
		FROM organizations WHERE id = $1 AND status = 'ACTIVE' FOR SHARE`, tenantID))
	if err != nil {
		return domain.AccountMembership{}, mapPostgresError("read invited organization", err)
	}
	if !found {
		return domain.AccountMembership{}, domain.ErrNotFound
	}
	existing, found, err := scanMembership(tx.QueryRow(ctx, `
		SELECT id, tenant_id, user_id, role, status, revoked_at, created_at, updated_at
		FROM memberships WHERE tenant_id = $1 AND user_id = $2 FOR UPDATE`, tenantID, command.UserID))
	if err != nil {
		return domain.AccountMembership{}, mapPostgresError("lock membership", err)
	}
	var membership domain.Membership
	switch {
	case found && existing.Status == domain.MembershipActive:
		return domain.AccountMembership{}, domain.ErrAlreadyMember
	case found:
		membership, found, err = scanMembership(tx.QueryRow(ctx, `
			UPDATE memberships SET status = $3, role = $4, revoked_at = NULL, updated_at = $5
			WHERE tenant_id = $1 AND user_id = $2
			RETURNING id, tenant_id, user_id, role, status, revoked_at, created_at, updated_at`,
			tenantID, command.UserID, domain.MembershipActive, invitation.Role, command.At.UTC()))
		if err != nil {
			return domain.AccountMembership{}, mapPostgresError("restore membership", err)
		}
		if !found {
			return domain.AccountMembership{}, domain.ErrNotFound
		}
	default:
		membership, err = domain.NewMembership(command.MembershipID, tenantID, command.UserID, invitation.Role, command.At)
		if err != nil {
			return domain.AccountMembership{}, domain.ErrInvalid
		}
		if err := insertMembership(ctx, tx, membership); err != nil {
			return domain.AccountMembership{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE membership_invitations SET accepted_at = $3, accepted_by_user_id = $4
		WHERE tenant_id = $1 AND id = $2`, tenantID, invitationID, command.At.UTC(), command.UserID); err != nil {
		return domain.AccountMembership{}, mapPostgresError("accept invitation", err)
	}
	if err := insertTenantAudit(ctx, tx, tenantID, domain.AuditEntry{
		ID: command.AuditID, ActorID: command.UserID, Operation: "INVITATION_ACCEPTED", EntityType: "MEMBERSHIP", EntityID: membership.ID, At: command.At,
	}, membership.ID); err != nil {
		return domain.AccountMembership{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.AccountMembership{}, fmt.Errorf("commit invitation acceptance: %w", err)
	}
	return domain.AccountMembership{Membership: membership, Organization: organization}, nil
}

// OnboardingFacts читает факты настройки из таблиц точек, каталога, каналов и
// личных привязок Telegram одним запросом.
func (r *PostgresRepository) OnboardingFacts(ctx context.Context, tenantID, userID string) (domain.OnboardingFacts, error) {
	if r == nil || r.pool == nil || tenantID == "" || userID == "" {
		return domain.OnboardingFacts{}, domain.ErrInvalid
	}
	var facts domain.OnboardingFacts
	if err := r.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM locations WHERE tenant_id = $1 AND active),
			(SELECT count(*) FROM locations AS location
			 WHERE location.tenant_id = $1 AND location.active
			   AND (SELECT count(*) FROM location_business_hours AS hours
			        WHERE hours.tenant_id = location.tenant_id AND hours.location_id = location.id) = 7),
			(SELECT count(*) FROM service_catalog_items WHERE tenant_id = $1 AND active),
			(SELECT count(*) FROM channel_connections WHERE tenant_id = $1),
			(SELECT count(*) FROM channel_connections WHERE tenant_id = $1 AND status <> 'DISCONNECTED'),
			EXISTS (SELECT 1 FROM telegram_user_links WHERE tenant_id = $1 AND user_id = $2 AND disabled_at IS NULL)`,
		tenantID, userID).Scan(
		&facts.ActiveLocations, &facts.LocationsWithSchedule, &facts.ActiveServices,
		&facts.Connections, &facts.LiveConnections, &facts.TelegramLinked,
	); err != nil {
		return domain.OnboardingFacts{}, mapPostgresError("read onboarding facts", err)
	}
	return facts, nil
}

func scanInvitation(row rowScanner) (domain.Invitation, error) {
	var invitation domain.Invitation
	if err := row.Scan(&invitation.ID, &invitation.TenantID, &invitation.Role, &invitation.CodeHash, &invitation.Note,
		&invitation.CreatedBy, &invitation.CreatedAt, &invitation.ExpiresAt, &invitation.AcceptedAt, &invitation.AcceptedBy,
		&invitation.RevokedAt, &invitation.RevokedBy); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Invitation{}, err
		}
		return domain.Invitation{}, fmt.Errorf("scan invitation: %w", err)
	}
	return invitation, nil
}
