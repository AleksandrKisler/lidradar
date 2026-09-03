package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"lidradar/backend/internal/notification/application"
	"lidradar/backend/internal/notification/domain"
)

const preferenceColumns = `id, tenant_id, user_id, risk_type, minimum_severity, delivery_mode,
	in_app_enabled, telegram_enabled, quiet_hours_enabled,
	to_char(quiet_hours_start, 'HH24:MI'), to_char(quiet_hours_end, 'HH24:MI'), to_char(digest_time, 'HH24:MI'),
	created_at, updated_at`

// Timezone возвращает часовой пояс организации: в нём считаются тихие часы и
// время сводки (ТЗ §3.7).
func (repository *PostgresRepository) Timezone(ctx context.Context, tenantID string) (string, bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" {
		return "", false, domain.ErrInvalid
	}
	var timezone string
	err := repository.pool.QueryRow(ctx, `SELECT default_timezone FROM organizations WHERE id = $1`, tenantID).Scan(&timezone)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, mapNotificationError("чтение часового пояса организации", err)
	}
	return timezone, true, nil
}

func (repository *PostgresRepository) Preferences(ctx context.Context, tenantID, userID string) ([]domain.Preference, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || userID == "" {
		return nil, domain.ErrInvalidPreference
	}
	rows, err := repository.pool.Query(ctx, `
		SELECT `+preferenceColumns+`
		FROM notification_preferences
		WHERE tenant_id = $1 AND user_id = $2
		ORDER BY risk_type`, tenantID, userID)
	if err != nil {
		return nil, mapNotificationError("чтение настроек уведомлений", err)
	}
	return collectPreferences(rows)
}

func collectPreferences(rows pgx.Rows) ([]domain.Preference, error) {
	defer rows.Close()
	preferences := make([]domain.Preference, 0, len(domain.RiskTypes()))
	for rows.Next() {
		preference, err := scanPreference(rows)
		if err != nil {
			return nil, err
		}
		preferences = append(preferences, preference)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("обход настроек уведомлений: %w", err)
	}
	return preferences, nil
}

func (repository *PostgresRepository) SavePreference(ctx context.Context, preference domain.Preference) (domain.Preference, error) {
	if repository == nil || repository.pool == nil || !preference.Stored() || preference.Validate() != nil {
		return domain.Preference{}, domain.ErrInvalidPreference
	}
	var start, end *string
	if preference.QuietHoursStart != nil {
		first, second := preference.QuietHoursStart.String(), preference.QuietHoursEnd.String()
		start, end = &first, &second
	}
	saved, err := scanPreference(repository.pool.QueryRow(ctx, `
		INSERT INTO notification_preferences(
			id, tenant_id, user_id, risk_type, minimum_severity, delivery_mode,
			in_app_enabled, telegram_enabled, quiet_hours_enabled,
			quiet_hours_start, quiet_hours_end, digest_time, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::time, $11::time, $12::time, $13, $13)
		ON CONFLICT (tenant_id, user_id, risk_type) DO UPDATE
		SET minimum_severity = EXCLUDED.minimum_severity, delivery_mode = EXCLUDED.delivery_mode,
		    in_app_enabled = EXCLUDED.in_app_enabled, telegram_enabled = EXCLUDED.telegram_enabled,
		    quiet_hours_enabled = EXCLUDED.quiet_hours_enabled, quiet_hours_start = EXCLUDED.quiet_hours_start,
		    quiet_hours_end = EXCLUDED.quiet_hours_end, digest_time = EXCLUDED.digest_time,
		    updated_at = EXCLUDED.updated_at
		RETURNING `+preferenceColumns,
		preference.ID, preference.TenantID, preference.UserID, preference.RiskType, preference.MinimumSeverity,
		preference.DeliveryMode, preference.InAppEnabled, preference.TelegramEnabled, preference.QuietHoursEnabled,
		start, end, preference.DigestTime.String(), preference.UpdatedAt,
	))
	if err != nil {
		return domain.Preference{}, mapNotificationError("сохранение настройки уведомлений", err)
	}
	return saved, nil
}

func (repository *PostgresRepository) DeletePreference(
	ctx context.Context,
	tenantID, userID string,
	riskType domain.RiskType,
) (bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || userID == "" || !domain.ValidRiskType(riskType) {
		return false, domain.ErrInvalidPreference
	}
	result, err := repository.pool.Exec(ctx, `
		DELETE FROM notification_preferences
		WHERE tenant_id = $1 AND user_id = $2 AND risk_type = $3`, tenantID, userID, riskType)
	if err != nil {
		return false, mapNotificationError("сброс настройки уведомлений", err)
	}
	return result.RowsAffected() == 1, nil
}

// TenantPolicy собирает активных участников с Telegram-привязкой и явными
// настройками; отсутствующая настройка действует по умолчанию (ТЗ §46).
func (repository *PostgresRepository) TenantPolicy(ctx context.Context, tenantID string) (application.TenantPolicy, error) {
	timezone, found, err := repository.Timezone(ctx, tenantID)
	if err != nil {
		return application.TenantPolicy{}, err
	}
	if !found {
		return application.TenantPolicy{}, application.ErrNotFound
	}
	rows, err := repository.pool.Query(ctx, `
		SELECT membership.user_id::text, COALESCE(link.chat_id::text, '')
		FROM memberships AS membership
		LEFT JOIN telegram_user_links AS link
		  ON link.tenant_id = membership.tenant_id AND link.user_id = membership.user_id AND link.disabled_at IS NULL
		WHERE membership.tenant_id = $1 AND membership.status = 'ACTIVE' AND membership.revoked_at IS NULL
		ORDER BY membership.created_at, membership.id`, tenantID)
	if err != nil {
		return application.TenantPolicy{}, mapNotificationError("чтение получателей", err)
	}
	policy := application.TenantPolicy{Timezone: timezone}
	index := make(map[string]int)
	for rows.Next() {
		var recipient application.Recipient
		if err := rows.Scan(&recipient.UserID, &recipient.TelegramDestination); err != nil {
			rows.Close()
			return application.TenantPolicy{}, fmt.Errorf("чтение получателя: %w", err)
		}
		recipient.Preferences = make(map[domain.RiskType]domain.Preference)
		index[recipient.UserID] = len(policy.Recipients)
		policy.Recipients = append(policy.Recipients, recipient)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return application.TenantPolicy{}, fmt.Errorf("обход получателей: %w", err)
	}
	rows.Close()
	preferenceRows, err := repository.pool.Query(ctx, `
		SELECT `+preferenceColumns+` FROM notification_preferences WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return application.TenantPolicy{}, mapNotificationError("чтение настроек получателей", err)
	}
	preferences, err := collectPreferences(preferenceRows)
	if err != nil {
		return application.TenantPolicy{}, err
	}
	for _, preference := range preferences {
		if position, known := index[preference.UserID]; known {
			policy.Recipients[position].Preferences[preference.RiskType] = preference
		}
	}
	return policy, nil
}

// OwnerRecipient возвращает первого активного владельца для эскалации.
func (repository *PostgresRepository) OwnerRecipient(ctx context.Context, tenantID string) (application.Recipient, bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" {
		return application.Recipient{}, false, domain.ErrInvalid
	}
	var recipient application.Recipient
	err := repository.pool.QueryRow(ctx, `
		SELECT membership.user_id::text, COALESCE(link.chat_id::text, '')
		FROM memberships AS membership
		LEFT JOIN telegram_user_links AS link
		  ON link.tenant_id = membership.tenant_id AND link.user_id = membership.user_id AND link.disabled_at IS NULL
		WHERE membership.tenant_id = $1 AND membership.role = 'OWNER'
		  AND membership.status = 'ACTIVE' AND membership.revoked_at IS NULL
		ORDER BY membership.created_at, membership.id
		LIMIT 1`, tenantID).Scan(&recipient.UserID, &recipient.TelegramDestination)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.Recipient{}, false, nil
	}
	if err != nil {
		return application.Recipient{}, false, mapNotificationError("поиск владельца для эскалации", err)
	}
	recipient.Preferences = make(map[domain.RiskType]domain.Preference)
	return recipient, true, nil
}

func (repository *PostgresRepository) EnqueueDigestItem(ctx context.Context, item domain.DigestItem) (bool, error) {
	if repository == nil || repository.pool == nil || item.Validate() != nil || item.ConsumedAt != nil {
		return false, domain.ErrInvalid
	}
	result, err := repository.pool.Exec(ctx, `
		INSERT INTO notification_digest_items(
			id, tenant_id, user_id, risk_id, risk_type, reason, slot, deliver_at,
			in_app_enabled, telegram_enabled, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (tenant_id, user_id, risk_id) DO NOTHING`,
		item.ID, item.TenantID, item.UserID, item.RiskID, item.RiskType, item.Reason, item.Slot, item.DeliverAt,
		item.InApp, item.Telegram, item.CreatedAt)
	if err != nil {
		return false, mapNotificationError("постановка риска в сводку", err)
	}
	return result.RowsAffected() == 1, nil
}

// PendingDigestEntries читает элементы слота вместе с актуальным состоянием
// рисков: важнейшие и самые ранние первыми.
func (repository *PostgresRepository) PendingDigestEntries(
	ctx context.Context,
	tenantID, userID, slot string,
) ([]domain.DigestEntry, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || userID == "" || !domain.ValidSlot(slot) {
		return nil, domain.ErrInvalid
	}
	rows, err := repository.pool.Query(ctx, `
		SELECT item.id, item.tenant_id, item.user_id, item.risk_id, item.risk_type, item.reason, item.slot,
		       item.deliver_at, item.in_app_enabled, item.telegram_enabled, COALESCE(item.notification_id::text, ''),
		       item.consumed_at, item.created_at,
		       risk.severity, risk.status, COALESCE(contact.display_name, ''), risk.detected_at
		FROM notification_digest_items AS item
		JOIN risk_signals AS risk ON risk.tenant_id = item.tenant_id AND risk.id = item.risk_id
		JOIN opportunities AS opportunity ON opportunity.tenant_id = risk.tenant_id AND opportunity.id = risk.opportunity_id
		JOIN conversations AS conversation
		  ON conversation.tenant_id = opportunity.tenant_id AND conversation.id = opportunity.conversation_id
		LEFT JOIN contacts AS contact ON contact.tenant_id = conversation.tenant_id AND contact.id = conversation.contact_id
		WHERE item.tenant_id = $1 AND item.user_id = $2 AND item.slot = $3 AND item.consumed_at IS NULL
		ORDER BY CASE risk.severity WHEN 'CRITICAL' THEN 4 WHEN 'HIGH' THEN 3 WHEN 'MEDIUM' THEN 2 ELSE 1 END DESC,
		         risk.detected_at, item.id`, tenantID, userID, slot)
	if err != nil {
		return nil, mapNotificationError("чтение элементов сводки", err)
	}
	defer rows.Close()
	entries := make([]domain.DigestEntry, 0)
	for rows.Next() {
		var entry domain.DigestEntry
		if err := rows.Scan(
			&entry.Item.ID, &entry.Item.TenantID, &entry.Item.UserID, &entry.Item.RiskID, &entry.Item.RiskType,
			&entry.Item.Reason, &entry.Item.Slot, &entry.Item.DeliverAt, &entry.Item.InApp, &entry.Item.Telegram,
			&entry.Item.NotificationID, &entry.Item.ConsumedAt, &entry.Item.CreatedAt,
			&entry.Severity, &entry.Status, &entry.Contact, &entry.DetectedAt,
		); err != nil {
			return nil, fmt.Errorf("чтение элемента сводки: %w", err)
		}
		if entry.Item.Validate() != nil {
			return nil, domain.ErrInvalid
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("обход элементов сводки: %w", err)
	}
	return entries, nil
}

// CreateDigest атомарно сохраняет сводку, её первые попытки и помечает
// элементы очереди доставленными. Повтор слота возвращает существующую сводку
// и лишь дозакрывает элементы, которые ещё не были помечены.
func (repository *PostgresRepository) CreateDigest(
	ctx context.Context,
	notification domain.Notification,
	deliveries []domain.Delivery,
	itemIDs []string,
) (domain.Notification, bool, error) {
	if repository == nil || repository.pool == nil || notification.Kind != domain.KindRiskDigest ||
		notification.Validate() != nil || !validFirstDeliveries(notification, deliveries) || len(itemIDs) == 0 {
		return domain.Notification{}, false, domain.ErrInvalid
	}
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return domain.Notification{}, false, fmt.Errorf("начало создания сводки: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stored, created, err := insertNotification(ctx, tx, notification)
	if err != nil {
		return domain.Notification{}, false, err
	}
	if created {
		for _, delivery := range deliveries {
			if err := insertDelivery(ctx, tx, delivery); err != nil {
				return domain.Notification{}, false, err
			}
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE notification_digest_items
		SET consumed_at = $3, notification_id = $4
		WHERE tenant_id = $1 AND id = ANY($2::uuid[]) AND user_id = $5 AND consumed_at IS NULL`,
		notification.TenantID, itemIDs, notification.CreatedAt, stored.ID, notification.UserID); err != nil {
		return domain.Notification{}, false, mapNotificationError("закрытие элементов сводки", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Notification{}, false, fmt.Errorf("фиксация сводки: %w", err)
	}
	return stored, created, nil
}

// ConsumeDigestItems закрывает элементы без сводки: риски уже неактуальны либо
// у получателя не осталось каналов.
func (repository *PostgresRepository) ConsumeDigestItems(ctx context.Context, tenantID string, itemIDs []string, at time.Time) error {
	if repository == nil || repository.pool == nil || tenantID == "" || len(itemIDs) == 0 || at.IsZero() {
		return domain.ErrInvalid
	}
	if _, err := repository.pool.Exec(ctx, `
		UPDATE notification_digest_items
		SET consumed_at = $3
		WHERE tenant_id = $1 AND id = ANY($2::uuid[]) AND consumed_at IS NULL`, tenantID, itemIDs, at.UTC()); err != nil {
		return mapNotificationError("закрытие элементов сводки", err)
	}
	return nil
}

// RiskAwaitingAction сообщает, остаётся ли риск открытым без чьей-либо реакции.
func (repository *PostgresRepository) RiskAwaitingAction(ctx context.Context, tenantID, riskID string) (bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || riskID == "" {
		return false, domain.ErrInvalid
	}
	var awaiting bool
	err := repository.pool.QueryRow(ctx, `
		SELECT status = 'OPEN' FROM risk_signals WHERE tenant_id = $1 AND id = $2`, tenantID, riskID).Scan(&awaiting)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, mapNotificationError("проверка реакции на риск", err)
	}
	return awaiting, nil
}

func scanPreference(row rowScanner) (domain.Preference, error) {
	var preference domain.Preference
	var start, end *string
	var digest string
	if err := row.Scan(
		&preference.ID, &preference.TenantID, &preference.UserID, &preference.RiskType, &preference.MinimumSeverity,
		&preference.DeliveryMode, &preference.InAppEnabled, &preference.TelegramEnabled, &preference.QuietHoursEnabled,
		&start, &end, &digest, &preference.CreatedAt, &preference.UpdatedAt,
	); err != nil {
		return domain.Preference{}, err
	}
	parsedDigest, err := domain.ParseClockTime(digest)
	if err != nil {
		return domain.Preference{}, err
	}
	preference.DigestTime = parsedDigest
	if start != nil && end != nil {
		parsedStart, err := domain.ParseClockTime(*start)
		if err != nil {
			return domain.Preference{}, err
		}
		parsedEnd, err := domain.ParseClockTime(*end)
		if err != nil {
			return domain.Preference{}, err
		}
		preference.QuietHoursStart, preference.QuietHoursEnd = &parsedStart, &parsedEnd
	}
	if err := preference.Validate(); err != nil {
		return domain.Preference{}, err
	}
	return preference, nil
}
