package infrastructure

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"lidradar/backend/internal/lifecycle/domain"
)

// Личная выгрузка (ADR 0059): учётная запись, членства, сессии, события безопасности, настройки уведомлений,
// привязки Telegram, решения по согласию и число внесённых записей. Всё читается одной транзакцией REPEATABLE READ
// только по чтению и строго по идентификатору пользователя: платформенный пул не даёт страховки RLS, поэтому
// каждый запрос несёт условие пользователя, а изоляцию проверяет тест с двумя пользователями.

// readSection читает до domain.SectionLimit строк; лишняя строка означает, что список обрезан.
func readSection[T any](ctx context.Context, tx pgx.Tx, query string, userID string, scan func(pgx.Rows) (T, error)) (domain.Section[T], error) {
	rows, err := tx.Query(ctx, query, userID, domain.SectionLimit+1)
	if err != nil {
		return domain.Section[T]{}, fmt.Errorf("read export section: %w", err)
	}
	defer rows.Close()
	section := domain.Section[T]{Items: []T{}}
	for rows.Next() {
		if len(section.Items) == domain.SectionLimit {
			section.Truncated = true
			break
		}
		item, err := scan(rows)
		if err != nil {
			return domain.Section[T]{}, fmt.Errorf("scan export section: %w", err)
		}
		section.Items = append(section.Items, item)
	}
	if err := rows.Err(); err != nil {
		return domain.Section[T]{}, fmt.Errorf("iterate export section: %w", err)
	}
	return section, nil
}

func (s *PostgresStore) PersonalExport(ctx context.Context, userID string) (domain.PersonalExport, error) {
	if s == nil || s.platform == nil || userID == "" {
		return domain.PersonalExport{}, domain.ErrInvalid
	}
	tx, err := s.platform.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.PersonalExport{}, fmt.Errorf("begin personal export: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL TIME ZONE 'UTC'`); err != nil {
		return domain.PersonalExport{}, fmt.Errorf("set export time zone: %w", err)
	}

	var document domain.PersonalExport
	err = tx.QueryRow(ctx, `
		SELECT id::text, email, display_name, status, created_at, email_verified_at, deletion_requested_at, deletion_due_at
		FROM users WHERE id = $1::uuid`, userID,
	).Scan(&document.Account.ID, &document.Account.Email, &document.Account.DisplayName, &document.Account.Status,
		&document.Account.CreatedAt, &document.Account.EmailVerifiedAt, &document.Account.DeletionRequestedAt, &document.Account.DeletionDueAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.PersonalExport{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.PersonalExport{}, fmt.Errorf("read account: %w", err)
	}

	if document.Memberships, err = readSection(ctx, tx, `
		SELECT o.name, m.role, m.status, m.created_at, m.revoked_at
		FROM memberships m JOIN organizations o ON o.id = m.tenant_id
		WHERE m.user_id = $1::uuid
		ORDER BY m.created_at, m.id
		LIMIT $2`, userID, func(rows pgx.Rows) (domain.ExportMembership, error) {
		var item domain.ExportMembership
		err := rows.Scan(&item.Organization, &item.Role, &item.Status, &item.JoinedAt, &item.RevokedAt)
		return item, err
	}); err != nil {
		return domain.PersonalExport{}, err
	}
	if document.Sessions, err = readSection(ctx, tx, `
		SELECT created_at, expires_at, revoked_at, COALESCE(host(ip), ''), COALESCE(user_agent, '')
		FROM sessions
		WHERE user_id = $1::uuid
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, userID, func(rows pgx.Rows) (domain.ExportSession, error) {
		var item domain.ExportSession
		err := rows.Scan(&item.CreatedAt, &item.ExpiresAt, &item.RevokedAt, &item.IPAddress, &item.UserAgent)
		return item, err
	}); err != nil {
		return domain.PersonalExport{}, err
	}
	if document.AuthEvents, err = readSection(ctx, tx, `
		SELECT operation, COALESCE(ip_address, ''), created_at
		FROM auth_audit_log
		WHERE user_id = $1::uuid
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, userID, func(rows pgx.Rows) (domain.ExportAuthEvent, error) {
		var item domain.ExportAuthEvent
		err := rows.Scan(&item.Operation, &item.IPAddress, &item.At)
		return item, err
	}); err != nil {
		return domain.PersonalExport{}, err
	}
	if document.NotificationSettings, err = readSection(ctx, tx, `
		SELECT o.name, p.risk_type, p.minimum_severity, p.delivery_mode, p.in_app_enabled, p.telegram_enabled,
		       p.quiet_hours_enabled, substr(p.quiet_hours_start::text, 1, 5), substr(p.quiet_hours_end::text, 1, 5),
		       substr(p.digest_time::text, 1, 5)
		FROM notification_preferences p JOIN organizations o ON o.id = p.tenant_id
		WHERE p.user_id = $1::uuid
		ORDER BY o.name, o.id, p.risk_type
		LIMIT $2`, userID, func(rows pgx.Rows) (domain.ExportNotificationRule, error) {
		var item domain.ExportNotificationRule
		err := rows.Scan(&item.Organization, &item.RiskType, &item.MinimumSeverity, &item.DeliveryMode, &item.InAppEnabled,
			&item.TelegramEnabled, &item.QuietHoursEnabled, &item.QuietHoursStart, &item.QuietHoursEnd, &item.DigestTime)
		return item, err
	}); err != nil {
		return domain.PersonalExport{}, err
	}
	if document.TelegramLinks, err = readSection(ctx, tx, `
		SELECT o.name, l.telegram_user_id, l.chat_id, l.linked_at, l.disabled_at
		FROM telegram_user_links l JOIN organizations o ON o.id = l.tenant_id
		WHERE l.user_id = $1::uuid
		ORDER BY l.linked_at, l.id
		LIMIT $2`, userID, func(rows pgx.Rows) (domain.ExportTelegramLink, error) {
		var item domain.ExportTelegramLink
		err := rows.Scan(&item.Organization, &item.TelegramUserID, &item.ChatID, &item.LinkedAt, &item.DisabledAt)
		return item, err
	}); err != nil {
		return domain.PersonalExport{}, err
	}
	if document.ConsentDecisions, err = readSection(ctx, tx, `
		SELECT organization, scope, decision, at FROM (
			SELECT o.name AS organization, c.scope, 'GRANTED' AS decision, c.granted_at AS at, c.id
			FROM ml_consents c JOIN organizations o ON o.id = c.tenant_id WHERE c.granted_by = $1::uuid
			UNION ALL
			SELECT o.name, c.scope, 'REVOKED', c.revoked_at, c.id
			FROM ml_consents c JOIN organizations o ON o.id = c.tenant_id WHERE c.revoked_by = $1::uuid
		) decisions
		ORDER BY at, id, decision
		LIMIT $2`, userID, func(rows pgx.Rows) (domain.ExportConsentDecision, error) {
		var item domain.ExportConsentDecision
		err := rows.Scan(&item.Organization, &item.Scope, &item.Decision, &item.At)
		return item, err
	}); err != nil {
		return domain.PersonalExport{}, err
	}

	if document.AuthoredRecords, err = readAuthored(ctx, tx, userID); err != nil {
		return domain.PersonalExport{}, err
	}
	return document, nil
}

// readAuthored считает записи, внесённые пользователем, по каждой организации, где у него есть членство. Только
// числа: содержимое записей принадлежит организации.
func readAuthored(ctx context.Context, tx pgx.Tx, userID string) ([]domain.ExportAuthored, error) {
	rows, err := tx.Query(ctx, `
		SELECT o.name,
		       (SELECT count(*) FROM actions x WHERE x.tenant_id = m.tenant_id AND x.actor_user_id = m.user_id),
		       (SELECT count(*) FROM outcomes x WHERE x.tenant_id = m.tenant_id AND x.actor_user_id = m.user_id),
		       (SELECT count(*) FROM risk_feedback x WHERE x.tenant_id = m.tenant_id AND x.actor_user_id = m.user_id),
		       (SELECT count(*) FROM revenue_events x WHERE x.tenant_id = m.tenant_id AND x.confirmed_by_user_id = m.user_id),
		       (SELECT count(*) FROM opportunity_stage_history x WHERE x.tenant_id = m.tenant_id AND x.actor_user_id = m.user_id),
		       (SELECT count(*) FROM membership_invitations x WHERE x.tenant_id = m.tenant_id AND x.created_by_user_id = m.user_id),
		       (SELECT count(*) FROM audit_log x WHERE x.tenant_id = m.tenant_id AND x.actor_user_id = m.user_id)
		FROM memberships m JOIN organizations o ON o.id = m.tenant_id
		WHERE m.user_id = $1::uuid
		ORDER BY m.created_at, m.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("count authored records: %w", err)
	}
	defer rows.Close()
	authored := []domain.ExportAuthored{}
	for rows.Next() {
		var item domain.ExportAuthored
		if err := rows.Scan(&item.Organization, &item.Actions, &item.Outcomes, &item.RiskFeedback, &item.RevenueConfirmations,
			&item.StageChanges, &item.InvitationsCreated, &item.AuditEvents); err != nil {
			return nil, fmt.Errorf("scan authored records: %w", err)
		}
		authored = append(authored, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate authored records: %w", err)
	}
	return authored, nil
}
