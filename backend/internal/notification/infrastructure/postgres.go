package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/notification/application"
	"lidradar/backend/internal/notification/domain"
	"lidradar/backend/platform/ids"
)

// PostgresRepository хранит логические уведомления, доставки, настройки
// получателей, очередь сводок и Telegram-привязки. Все запросы, затрагивающие
// пользовательские данные, ограничены tenant_id.
type PostgresRepository struct{ pool *pgxpool.Pool }

type DeliveryStats struct {
	Pending, Processing, Retry, Dead, ExpiredLeases, Overdue int
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const (
	notificationColumns = `id, tenant_id, user_id, COALESCE(risk_id::text, ''), kind, dedup_key, title, body,
		snoozed_at, created_at, updated_at`
	deliveryColumns = `delivery.id, delivery.notification_id, delivery.tenant_id, delivery.destination,
		delivery.title, delivery.body, delivery.kind, delivery.channel, delivery.attempt, delivery.status,
		delivery.available_at, delivery.leased_by, delivery.lease_until,
		delivery.attempted_at, COALESCE(delivery.provider_message_id, ''),
		COALESCE(delivery.failure_code, ''), delivery.created_at, delivery.updated_at`
)

func (repository *PostgresRepository) DeliveryStats(ctx context.Context, at time.Time) (DeliveryStats, error) {
	if repository == nil || repository.pool == nil || at.IsZero() {
		return DeliveryStats{}, domain.ErrInvalid
	}
	var stats DeliveryStats
	err := repository.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'PENDING'),
			count(*) FILTER (WHERE status = 'PROCESSING'),
			count(*) FILTER (WHERE status = 'RETRY'),
			count(*) FILTER (WHERE status = 'DEAD'),
			count(*) FILTER (WHERE status = 'PROCESSING' AND lease_until <= $1),
			count(*) FILTER (WHERE status = 'PENDING' AND available_at <= $1)
		FROM notification_deliveries`, at.UTC()).Scan(
		&stats.Pending, &stats.Processing, &stats.Retry, &stats.Dead, &stats.ExpiredLeases, &stats.Overdue,
	)
	if err != nil {
		return DeliveryStats{}, mapNotificationError("чтение состояния доставок", err)
	}
	return stats, nil
}

func (repository *PostgresRepository) Create(
	ctx context.Context,
	notification domain.Notification,
	deliveries []domain.Delivery,
) (domain.Notification, bool, error) {
	if repository == nil || repository.pool == nil || notification.Validate() != nil || !validFirstDeliveries(notification, deliveries) {
		return domain.Notification{}, false, domain.ErrInvalid
	}
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return domain.Notification{}, false, fmt.Errorf("начало создания уведомления: %w", err)
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
	if err := tx.Commit(ctx); err != nil {
		return domain.Notification{}, false, fmt.Errorf("фиксация уведомления: %w", err)
	}
	return stored, created, nil
}

// validFirstDeliveries требует хотя бы одну первую попытку и не более одной на канал.
func validFirstDeliveries(notification domain.Notification, deliveries []domain.Delivery) bool {
	if len(deliveries) == 0 {
		return false
	}
	seen := make(map[domain.Channel]struct{}, len(deliveries))
	for _, delivery := range deliveries {
		if delivery.Validate() != nil || delivery.TenantID != notification.TenantID || delivery.NotificationID != notification.ID ||
			delivery.Attempt != 1 || delivery.Kind != notification.Kind {
			return false
		}
		if _, duplicate := seen[delivery.Channel]; duplicate {
			return false
		}
		seen[delivery.Channel] = struct{}{}
	}
	return true
}

// insertNotification сохраняет факт либо возвращает существующий с тем же ключом.
func insertNotification(ctx context.Context, tx pgx.Tx, notification domain.Notification) (domain.Notification, bool, error) {
	stored, err := scanNotification(tx.QueryRow(ctx, `
		INSERT INTO notifications(
			id, tenant_id, user_id, risk_id, kind, dedup_key, title, body, created_at, updated_at
		) VALUES ($1, $2, $3, NULLIF($4, '')::uuid, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (tenant_id, dedup_key) DO NOTHING
		RETURNING `+notificationColumns,
		notification.ID, notification.TenantID, notification.UserID, notification.RiskID, notification.Kind,
		notification.DedupKey, notification.Title, notification.Body, notification.CreatedAt, notification.UpdatedAt,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		stored, err = scanNotification(tx.QueryRow(ctx, `
			SELECT `+notificationColumns+`
			FROM notifications WHERE tenant_id = $1 AND dedup_key = $2`, notification.TenantID, notification.DedupKey))
		if err != nil {
			return domain.Notification{}, false, mapNotificationError("чтение повторного уведомления", err)
		}
		if stored.RiskID != notification.RiskID || stored.Kind != notification.Kind {
			return domain.Notification{}, false, domain.ErrInvalid
		}
		return stored, false, nil
	}
	if err != nil {
		return domain.Notification{}, false, mapNotificationError("создание уведомления", err)
	}
	return stored, true, nil
}

func (repository *PostgresRepository) ClaimDue(
	ctx context.Context,
	owner string,
	now, leaseUntil time.Time,
	limit int,
	channels []domain.Channel,
) ([]domain.Delivery, error) {
	if repository == nil || repository.pool == nil || owner == "" || now.IsZero() || !leaseUntil.After(now) ||
		limit < 1 || limit > 100 || len(channels) == 0 {
		return nil, domain.ErrInvalid
	}
	names := make([]string, 0, len(channels))
	for _, channel := range channels {
		if !domain.ValidChannel(channel) {
			return nil, domain.ErrInvalid
		}
		names = append(names, string(channel))
	}
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("начало аренды доставок: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE notification_deliveries
		SET status = 'DEAD', leased_by = NULL, lease_until = NULL,
		    attempted_at = $1, failure_code = 'LEASE_EXPIRED_MAX_ATTEMPTS', updated_at = $1
		WHERE status = 'PROCESSING' AND lease_until <= $1 AND attempt >= 5`, now.UTC()); err != nil {
		return nil, mapNotificationError("завершение исчерпанных аренд доставок", err)
	}
	rows, err := tx.Query(ctx, `
		WITH candidates AS (
			SELECT id FROM notification_deliveries
			WHERE ((status = 'PENDING' AND available_at <= $1)
			   OR (status = 'PROCESSING' AND lease_until <= $1 AND attempt < 5))
			  AND channel = ANY($5)
			ORDER BY available_at, created_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $4
		)
		UPDATE notification_deliveries AS delivery
		SET status = 'PROCESSING', leased_by = $2, lease_until = $3, updated_at = $1
		FROM candidates WHERE delivery.id = candidates.id
		RETURNING `+deliveryColumns,
		now.UTC(), owner, leaseUntil.UTC(), limit, names)
	if err != nil {
		return nil, mapNotificationError("аренда доставок", err)
	}
	deliveries := make([]domain.Delivery, 0, limit)
	for rows.Next() {
		delivery, scanErr := scanDelivery(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		deliveries = append(deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("обход доставок: %w", err)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("фиксация аренды доставок: %w", err)
	}
	return deliveries, nil
}

func (repository *PostgresRepository) Complete(
	ctx context.Context,
	owner string,
	delivery domain.Delivery,
	retry *domain.Delivery,
) error {
	if repository == nil || repository.pool == nil || owner == "" || delivery.Validate() != nil ||
		(delivery.Status != domain.DeliverySucceeded && delivery.Status != domain.DeliveryRetry && delivery.Status != domain.DeliveryDead) {
		return domain.ErrInvalid
	}
	if retry != nil && (retry.Validate() != nil || delivery.Status != domain.DeliveryRetry ||
		retry.NotificationID != delivery.NotificationID || retry.TenantID != delivery.TenantID ||
		retry.Channel != delivery.Channel || retry.Kind != delivery.Kind || retry.Attempt != delivery.Attempt+1) {
		return domain.ErrInvalid
	}
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начало завершения доставки: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := tx.Exec(ctx, `
		UPDATE notification_deliveries
		SET status = $4, leased_by = NULL, lease_until = NULL, attempted_at = $5,
		    provider_message_id = NULLIF($6, ''), failure_code = NULLIF($7, ''), updated_at = $8
		WHERE id = $1 AND tenant_id = $2 AND status = 'PROCESSING'
		  AND leased_by = $3 AND lease_until > $8`,
		delivery.ID, delivery.TenantID, owner, delivery.Status, delivery.AttemptedAt,
		delivery.ProviderMessageID, delivery.FailureCode, delivery.UpdatedAt)
	if err != nil {
		return mapNotificationError("завершение доставки", err)
	}
	if result.RowsAffected() != 1 {
		return application.ErrLeaseLost
	}
	if retry != nil {
		if err := insertDelivery(ctx, tx, *retry); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("фиксация завершения доставки: %w", err)
	}
	return nil
}

func (repository *PostgresRepository) TelegramDestination(
	ctx context.Context,
	tenantID, userID string,
) (string, bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || userID == "" {
		return "", false, domain.ErrInvalid
	}
	var chatID string
	err := repository.pool.QueryRow(ctx, `
		SELECT chat_id::text FROM telegram_user_links
		WHERE tenant_id = $1 AND user_id = $2 AND disabled_at IS NULL`, tenantID, userID).Scan(&chatID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, mapNotificationError("чтение Telegram-привязки", err)
	}
	return chatID, true, nil
}

func (repository *PostgresRepository) SaveToken(ctx context.Context, token application.LinkToken) error {
	if repository == nil || repository.pool == nil || token.ID == "" || token.TenantID == "" || token.UserID == "" ||
		len(token.TokenHash) != 64 || token.CreatedAt.IsZero() || !token.ExpiresAt.After(token.CreatedAt) || token.UsedAt != nil {
		return application.ErrInvalidLinkToken
	}
	_, err := repository.pool.Exec(ctx, `
		INSERT INTO telegram_link_tokens(id, tenant_id, user_id, token_hash, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, token.ID, token.TenantID, token.UserID, token.TokenHash, token.ExpiresAt, token.CreatedAt)
	if err != nil {
		return mapNotificationError("сохранение кода привязки", err)
	}
	return nil
}

func (repository *PostgresRepository) UseToken(
	ctx context.Context,
	tenantID, hash string,
	at time.Time,
	telegramUserID, chatID string,
) (application.LinkToken, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || len(hash) != 64 || at.IsZero() ||
		telegramUserID == "" || chatID == "" {
		return application.LinkToken{}, application.ErrInvalidLinkToken
	}
	linkID, err := ids.Generator{}.NewID()
	if err != nil {
		return application.LinkToken{}, err
	}
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return application.LinkToken{}, fmt.Errorf("начало привязки Telegram: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var token application.LinkToken
	err = tx.QueryRow(ctx, `
		UPDATE telegram_link_tokens
		SET used_at = $3
		WHERE tenant_id = $1 AND token_hash = $2 AND used_at IS NULL AND expires_at > $3
		RETURNING id, tenant_id, user_id, token_hash, expires_at, used_at, created_at`, tenantID, hash, at.UTC()).Scan(
		&token.ID, &token.TenantID, &token.UserID, &token.TokenHash, &token.ExpiresAt, &token.UsedAt, &token.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.LinkToken{}, application.ErrInvalidLinkToken
	}
	if err != nil {
		return application.LinkToken{}, mapNotificationError("использование кода привязки", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO telegram_user_links(
			id, tenant_id, user_id, telegram_user_id, chat_id, linked_at, updated_at
		) VALUES ($1, $2, $3, $4::bigint, $5::bigint, $6, $6)
		ON CONFLICT (tenant_id, user_id) DO UPDATE
		SET telegram_user_id = EXCLUDED.telegram_user_id, chat_id = EXCLUDED.chat_id,
		    linked_at = EXCLUDED.linked_at, updated_at = EXCLUDED.updated_at, disabled_at = NULL`,
		linkID, token.TenantID, token.UserID, telegramUserID, chatID, at.UTC())
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "23505" {
			return application.LinkToken{}, application.ErrInvalidLinkToken
		}
		return application.LinkToken{}, mapNotificationError("создание Telegram-привязки", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return application.LinkToken{}, fmt.Errorf("фиксация Telegram-привязки: %w", err)
	}
	return token, nil
}

func (repository *PostgresRepository) Link(
	ctx context.Context,
	tenantID, userID string,
) (application.TelegramLink, bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || userID == "" {
		return application.TelegramLink{}, false, application.ErrInvalidLinkToken
	}
	var link application.TelegramLink
	err := repository.pool.QueryRow(ctx, `
		SELECT tenant_id, user_id, telegram_user_id::text, chat_id::text, linked_at
		FROM telegram_user_links
		WHERE tenant_id = $1 AND user_id = $2 AND disabled_at IS NULL`, tenantID, userID).Scan(
		&link.TenantID, &link.UserID, &link.TelegramUserID, &link.ChatID, &link.LinkedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.TelegramLink{}, false, nil
	}
	if err != nil {
		return application.TelegramLink{}, false, mapNotificationError("чтение Telegram-привязки", err)
	}
	return link, true, nil
}

func (repository *PostgresRepository) DisableLink(
	ctx context.Context,
	tenantID, userID string,
	at time.Time,
) (bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || userID == "" || at.IsZero() {
		return false, application.ErrInvalidLinkToken
	}
	result, err := repository.pool.Exec(ctx, `
		UPDATE telegram_user_links SET disabled_at = $3, updated_at = $3
		WHERE tenant_id = $1 AND user_id = $2 AND disabled_at IS NULL`, tenantID, userID, at.UTC())
	if err != nil {
		return false, mapNotificationError("отключение Telegram-привязки", err)
	}
	return result.RowsAffected() == 1, nil
}

// CallbackTarget находит получателя и риск команды. Сводка не привязана к
// одному риску и командам не подлежит.
func (repository *PostgresRepository) CallbackTarget(
	ctx context.Context,
	tenantID, notificationID, telegramUserID, chatID string,
) (string, string, bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || !ids.Valid(notificationID) ||
		telegramUserID == "" || chatID == "" {
		return "", "", false, application.ErrUnsafeCallback
	}
	var userID, riskID string
	err := repository.pool.QueryRow(ctx, `
		SELECT notification.user_id::text, notification.risk_id::text
		FROM notifications AS notification
		JOIN telegram_user_links AS link
		  ON link.tenant_id = notification.tenant_id AND link.user_id = notification.user_id
		JOIN memberships AS membership
		  ON membership.tenant_id = notification.tenant_id AND membership.user_id = notification.user_id
		WHERE notification.tenant_id = $1 AND notification.id = $2 AND notification.risk_id IS NOT NULL
		  AND link.telegram_user_id = $3::bigint AND link.chat_id = $4::bigint
		  AND link.disabled_at IS NULL AND membership.status = 'ACTIVE'`,
		tenantID, notificationID, telegramUserID, chatID).Scan(&userID, &riskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, mapNotificationError("проверка команды Telegram", err)
	}
	return userID, riskID, true, nil
}

func (repository *PostgresRepository) CallbackRecorded(
	ctx context.Context,
	tenantID, idempotencyKey string,
) (bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || idempotencyKey == "" {
		return false, application.ErrUnsafeCallback
	}
	var exists bool
	if err := repository.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM telegram_callback_commands
			WHERE tenant_id = $1 AND idempotency_key = $2
		)`, tenantID, idempotencyKey).Scan(&exists); err != nil {
		return false, mapNotificationError("проверка повторной команды Telegram", err)
	}
	return exists, nil
}

func (repository *PostgresRepository) RecordCallback(
	ctx context.Context,
	command application.CallbackCommand,
	id string,
	at time.Time,
) (bool, error) {
	if repository == nil || repository.pool == nil || id == "" || at.IsZero() ||
		command.TenantID == "" || command.UserID == "" || !ids.Valid(command.NotificationID) ||
		!ids.Valid(command.RiskID) || command.IdempotencyKey == "" {
		return false, application.ErrUnsafeCallback
	}
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("начало фиксации команды Telegram: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var insertedID string
	err = tx.QueryRow(ctx, `
		INSERT INTO telegram_callback_commands(
			id, tenant_id, user_id, notification_id, risk_id, idempotency_key, action, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id, idempotency_key) DO NOTHING
		RETURNING id`, id, command.TenantID, command.UserID, command.NotificationID,
		command.RiskID, command.IdempotencyKey, command.Action, at.UTC()).Scan(&insertedID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("фиксация повторной команды Telegram: %w", err)
		}
		return false, nil
	}
	if err != nil {
		return false, mapNotificationError("сохранение команды Telegram", err)
	}
	if command.Action == application.CallbackSnooze {
		result, updateErr := tx.Exec(ctx, `
			UPDATE notifications SET snoozed_at = $5, updated_at = $5
			WHERE tenant_id = $1 AND id = $2 AND user_id = $3 AND risk_id = $4`,
			command.TenantID, command.NotificationID, command.UserID, command.RiskID, at.UTC())
		if updateErr != nil {
			return false, mapNotificationError("откладывание уведомления", updateErr)
		}
		if result.RowsAffected() != 1 {
			return false, application.ErrUnsafeCallback
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("фиксация команды Telegram: %w", err)
	}
	return true, nil
}

func insertDelivery(ctx context.Context, tx pgx.Tx, delivery domain.Delivery) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO notification_deliveries(
			id, tenant_id, notification_id, kind, channel, destination, title, body, attempt,
			status, available_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		delivery.ID, delivery.TenantID, delivery.NotificationID, delivery.Kind, delivery.Channel, delivery.Destination,
		delivery.Title, delivery.Body, delivery.Attempt, delivery.Status, delivery.AvailableAt,
		delivery.CreatedAt, delivery.UpdatedAt)
	if err != nil {
		return mapNotificationError("создание попытки доставки", err)
	}
	return nil
}

type rowScanner interface{ Scan(...any) error }

func scanNotification(row rowScanner) (domain.Notification, error) {
	var notification domain.Notification
	if err := row.Scan(
		&notification.ID, &notification.TenantID, &notification.UserID, &notification.RiskID, &notification.Kind,
		&notification.DedupKey, &notification.Title, &notification.Body, &notification.SnoozedAt,
		&notification.CreatedAt, &notification.UpdatedAt,
	); err != nil {
		return domain.Notification{}, err
	}
	if notification.Validate() != nil {
		return domain.Notification{}, domain.ErrInvalid
	}
	return notification, nil
}

func scanDelivery(row rowScanner) (domain.Delivery, error) {
	var delivery domain.Delivery
	if err := row.Scan(
		&delivery.ID, &delivery.NotificationID, &delivery.TenantID, &delivery.Destination,
		&delivery.Title, &delivery.Body, &delivery.Kind, &delivery.Channel, &delivery.Attempt, &delivery.Status,
		&delivery.AvailableAt, &delivery.LeasedBy, &delivery.LeaseUntil, &delivery.AttemptedAt,
		&delivery.ProviderMessageID, &delivery.FailureCode, &delivery.CreatedAt, &delivery.UpdatedAt,
	); err != nil {
		return domain.Delivery{}, err
	}
	if delivery.Validate() != nil {
		return domain.Delivery{}, domain.ErrInvalid
	}
	return delivery, nil
}

func mapNotificationError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "23503", "23514", "22P02", "22003":
			return domain.ErrInvalid
		case "23505":
			return fmt.Errorf("%s: конфликт", operation)
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}

var _ application.Store = (*PostgresRepository)(nil)
var _ application.LinkStore = (*PostgresRepository)(nil)
var _ application.TokenStore = (*PostgresRepository)(nil)
var _ application.LinkManagementStore = (*PostgresRepository)(nil)
var _ application.CallbackStore = (*PostgresRepository)(nil)
var _ application.PolicyStore = (*PostgresRepository)(nil)
var _ application.PreferenceStore = (*PostgresRepository)(nil)
