// Package infrastructure читает таблицы всех модулей для диагностики и
// выполняет административные команды над мёртвыми элементами очередей
// (ADR 0040). Тексты сообщений, промпты и сырой вывод модели не читаются.
package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/admin/application"
	"lidradar/backend/internal/admin/domain"
	"lidradar/backend/platform/ids"
)

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

const (
	adminColumns = `admin.id::text, admin.user_id::text, COALESCE(u.email, ''), COALESCE(u.display_name, ''),
		admin.granted_by::text, admin.granted_at, admin.revoked_by::text, admin.revoked_at, admin.note`
	jobColumns = `id::text, tenant_id::text, job_type, dedup_key, status, priority, available_at, attempt_count, max_attempts,
		leased_by, lease_until, last_error_code, completed_at, discarded_at, created_at, updated_at, payload`
	outboxColumns = `id::text, tenant_id::text, event_type, aggregate_type, aggregate_id::text, status, attempt_count, max_attempts,
		last_error_code, occurred_at, completed_at, discarded_at`
	aiJobColumns = `id::text, tenant_id::text, entity_id::text, analysis_through_message_id::text, status, model_requirement,
		attempts, max_attempts, last_error_code, leased_by::text, leased_at, lease_until, completed_at, discarded_at, created_at`
	deliveryColumns = `id::text, tenant_id::text, notification_id::text, kind, channel, status, attempt, failure_code,
		attempted_at, discarded_at, created_at`
	aiRunColumns = `id::text, tenant_id::text, job_id::text, node_id::text, entity_id::text, status, application_status,
		model_version, prompt_version, schema_version, error_code, validation_error, started_at, completed_at`
)

type rowScanner interface{ Scan(...any) error }

func (store *PostgresStore) ready() bool { return store != nil && store.pool != nil }

func (store *PostgresStore) IsAdmin(ctx context.Context, userID string) (bool, error) {
	if !store.ready() || !ids.Valid(userID) {
		return false, nil
	}
	var active bool
	if err := store.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM platform_admins AS admin
			JOIN users AS u ON u.id = admin.user_id
			WHERE admin.user_id = $1 AND admin.revoked_at IS NULL AND u.status = 'ACTIVE'
		)`, userID).Scan(&active); err != nil {
		return false, fmt.Errorf("проверка права администратора: %w", err)
	}
	return active, nil
}

func scanAdmin(row rowScanner) (domain.Admin, error) {
	var admin domain.Admin
	if err := row.Scan(
		&admin.ID, &admin.UserID, &admin.Email, &admin.DisplayName, &admin.GrantedBy, &admin.GrantedAt,
		&admin.RevokedBy, &admin.RevokedAt, &admin.Note,
	); err != nil {
		return domain.Admin{}, err
	}
	return admin, nil
}

func (store *PostgresStore) Admins(ctx context.Context) ([]domain.Admin, error) {
	if !store.ready() {
		return nil, application.ErrInvalid
	}
	rows, err := store.pool.Query(ctx, `
		SELECT `+adminColumns+`
		FROM platform_admins AS admin JOIN users AS u ON u.id = admin.user_id
		ORDER BY admin.granted_at DESC, admin.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("чтение администраторов: %w", err)
	}
	defer rows.Close()
	admins := make([]domain.Admin, 0)
	for rows.Next() {
		admin, err := scanAdmin(rows)
		if err != nil {
			return nil, fmt.Errorf("чтение администратора: %w", err)
		}
		admins = append(admins, admin)
	}
	return admins, rows.Err()
}

func (store *PostgresStore) UserByEmail(ctx context.Context, email string) (application.UserRecord, bool, error) {
	if !store.ready() || email == "" {
		return application.UserRecord{}, false, application.ErrInvalid
	}
	var user application.UserRecord
	err := store.pool.QueryRow(ctx, `
		SELECT id::text, email, display_name FROM users WHERE email = $1 AND status = 'ACTIVE'`, email).Scan(
		&user.ID, &user.Email, &user.DisplayName,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.UserRecord{}, false, nil
	}
	if err != nil {
		return application.UserRecord{}, false, fmt.Errorf("поиск пользователя: %w", err)
	}
	return user, true, nil
}

func insertAudit(ctx context.Context, tx pgx.Tx, audit domain.AuditEntry) error {
	if err := audit.Validate(); err != nil {
		return application.ErrInvalid
	}
	details, err := json.Marshal(audit.Details)
	if err != nil {
		return fmt.Errorf("кодирование деталей аудита: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO admin_audit_log(id, actor_user_id, source, operation, entity_type, entity_id, tenant_id, details, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9)`,
		audit.ID, audit.ActorUserID, audit.Source, audit.Operation, audit.EntityType, audit.EntityID, audit.TenantID,
		string(details), audit.At.UTC()); err != nil {
		return mapAdminError("запись административного аудита", err)
	}
	return nil
}

// GrantAdmin опирается на частичный уникальный индекс: повторная выдача
// действующего права возвращает существующую строку.
func (store *PostgresStore) GrantAdmin(ctx context.Context, admin domain.Admin, audit domain.AuditEntry) (domain.Admin, bool, error) {
	if !store.ready() || admin.Validate() != nil || !admin.Active() {
		return domain.Admin{}, false, application.ErrInvalid
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.Admin{}, false, fmt.Errorf("начало выдачи права: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var stored domain.Admin
	var inserted bool
	err = tx.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO platform_admins(id, user_id, granted_by, granted_at, note)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (user_id) WHERE revoked_at IS NULL DO NOTHING
			RETURNING id, user_id, granted_by, granted_at, revoked_by, revoked_at, note, TRUE AS created
		), current AS (
			SELECT id, user_id, granted_by, granted_at, revoked_by, revoked_at, note, FALSE AS created
			FROM platform_admins WHERE user_id = $2 AND revoked_at IS NULL
		)
		SELECT admin.id::text, admin.user_id::text, COALESCE(u.email, ''), COALESCE(u.display_name, ''),
		       admin.granted_by::text, admin.granted_at, admin.revoked_by::text, admin.revoked_at, admin.note, admin.created
		FROM (SELECT * FROM inserted UNION ALL SELECT * FROM current) AS admin
		JOIN users AS u ON u.id = admin.user_id
		ORDER BY admin.created DESC LIMIT 1`,
		admin.ID, admin.UserID, admin.GrantedBy, admin.GrantedAt, admin.Note).Scan(
		&stored.ID, &stored.UserID, &stored.Email, &stored.DisplayName, &stored.GrantedBy, &stored.GrantedAt,
		&stored.RevokedBy, &stored.RevokedAt, &stored.Note, &inserted,
	)
	if err != nil {
		return domain.Admin{}, false, mapAdminError("выдача права администратора", err)
	}
	if inserted {
		if err := insertAudit(ctx, tx, audit); err != nil {
			return domain.Admin{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Admin{}, false, fmt.Errorf("фиксация выдачи права: %w", err)
	}
	return stored, inserted, nil
}

func (store *PostgresStore) RevokeAdmin(ctx context.Context, userID string, revokedBy *string, at time.Time, audit domain.AuditEntry) (bool, error) {
	if !store.ready() || !ids.Valid(userID) || at.IsZero() {
		return false, application.ErrInvalid
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("начало отзыва права: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := tx.Exec(ctx, `
		UPDATE platform_admins SET revoked_at = $2, revoked_by = $3
		WHERE user_id = $1 AND revoked_at IS NULL`, userID, at.UTC(), revokedBy)
	if err != nil {
		return false, mapAdminError("отзыв права администратора", err)
	}
	if result.RowsAffected() == 0 {
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("фиксация пустого отзыва: %w", err)
		}
		return false, nil
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("фиксация отзыва права: %w", err)
	}
	return true, nil
}

func (store *PostgresStore) Organizations(ctx context.Context, now time.Time) ([]domain.OrganizationSummary, error) {
	if !store.ready() || now.IsZero() {
		return nil, application.ErrInvalid
	}
	rows, err := store.pool.Query(ctx, `
		SELECT o.id::text, o.name, o.default_timezone, o.default_currency, o.status, o.created_at,
		       (SELECT count(*) FROM memberships AS m WHERE m.tenant_id = o.id AND m.status = 'ACTIVE' AND m.revoked_at IS NULL),
		       (SELECT count(*) FROM locations AS l WHERE l.tenant_id = o.id),
		       (SELECT count(*) FROM channel_connections AS c WHERE c.tenant_id = o.id),
		       (SELECT count(*) FROM risk_signals AS r WHERE r.tenant_id = o.id AND r.status IN ('OPEN', 'ACKNOWLEDGED', 'ACTED')),
		       (SELECT count(*) FROM messages AS msg WHERE msg.tenant_id = o.id AND msg.sent_at >= $1::timestamptz - INTERVAL '24 hours' AND msg.sent_at < $1::timestamptz)
		FROM organizations AS o
		ORDER BY o.created_at, o.id`, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("чтение организаций: %w", err)
	}
	defer rows.Close()
	result := make([]domain.OrganizationSummary, 0)
	for rows.Next() {
		var item domain.OrganizationSummary
		if err := rows.Scan(&item.ID, &item.Name, &item.Timezone, &item.Currency, &item.Status, &item.CreatedAt,
			&item.Members, &item.Locations, &item.Connections, &item.OpenRisks, &item.MessagesLast24h); err != nil {
			return nil, fmt.Errorf("чтение организации: %w", err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (store *PostgresStore) Connections(ctx context.Context) ([]domain.ConnectionHealth, error) {
	if !store.ready() {
		return nil, application.ErrInvalid
	}
	rows, err := store.pool.Query(ctx, `
		SELECT c.id::text, c.tenant_id::text, o.name, c.provider, c.name, c.status, c.location_id::text,
		       c.last_event_at, c.last_success_at, c.last_error_at, c.last_error_code,
		       (SELECT count(*) FROM raw_events AS e WHERE e.tenant_id = c.tenant_id AND e.connection_id = c.id AND e.status IN ('RECEIVED', 'PROCESSING')),
		       (SELECT count(*) FROM raw_events AS e WHERE e.tenant_id = c.tenant_id AND e.connection_id = c.id AND e.status = 'FAILED')
		FROM channel_connections AS c
		JOIN organizations AS o ON o.id = c.tenant_id
		ORDER BY o.name, c.created_at, c.id`)
	if err != nil {
		return nil, fmt.Errorf("чтение каналов: %w", err)
	}
	defer rows.Close()
	result := make([]domain.ConnectionHealth, 0)
	for rows.Next() {
		var item domain.ConnectionHealth
		if err := rows.Scan(&item.ID, &item.TenantID, &item.TenantName, &item.Provider, &item.Name, &item.Status, &item.LocationID,
			&item.LastEventAt, &item.LastSuccessAt, &item.LastErrorAt, &item.LastErrorCode, &item.RawEventsPending, &item.RawEventsFailed); err != nil {
			return nil, fmt.Errorf("чтение канала: %w", err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (store *PostgresStore) QueueStats(ctx context.Context, now time.Time) (domain.QueueStats, error) {
	if !store.ready() || now.IsZero() {
		return domain.QueueStats{}, application.ErrInvalid
	}
	stats := domain.QueueStats{CheckedAt: now.UTC()}
	err := store.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM jobs WHERE status = 'PENDING'),
			(SELECT count(*) FROM jobs WHERE status = 'PROCESSING'),
			(SELECT count(*) FROM jobs WHERE status = 'RETRY'),
			(SELECT count(*) FROM jobs WHERE status = 'DEAD'),
			(SELECT count(*) FROM jobs WHERE status = 'PROCESSING' AND lease_until <= $1),
			(SELECT count(*) FROM outbox_events WHERE status = 'PENDING'),
			(SELECT count(*) FROM outbox_events WHERE status = 'PROCESSING'),
			(SELECT count(*) FROM outbox_events WHERE status = 'RETRY'),
			(SELECT count(*) FROM outbox_events WHERE status = 'DEAD'),
			(SELECT count(*) FROM outbox_events WHERE status = 'PROCESSING' AND lease_until <= $1),
			(SELECT count(*) FROM ai_jobs WHERE status = 'PENDING'),
			(SELECT count(*) FROM ai_jobs WHERE status = 'LEASED'),
			(SELECT count(*) FROM ai_jobs WHERE status = 'RUNNING'),
			(SELECT count(*) FROM ai_jobs WHERE status = 'RETRY'),
			(SELECT count(*) FROM ai_jobs WHERE status = 'DEAD'),
			(SELECT count(*) FROM ai_nodes WHERE status = 'READY'),
			(SELECT count(*) FROM notification_deliveries WHERE status = 'PENDING'),
			(SELECT count(*) FROM notification_deliveries WHERE status = 'PROCESSING'),
			(SELECT count(*) FROM notification_deliveries WHERE status = 'RETRY'),
			(SELECT count(*) FROM notification_deliveries WHERE status = 'DEAD'),
			(SELECT count(*) FROM scheduled_checks WHERE status = 'SCHEDULED' AND due_at <= $1),
			(SELECT count(*) FROM jobs WHERE status = 'DEAD' AND discarded_at IS NULL)
			+ (SELECT count(*) FROM outbox_events WHERE status = 'DEAD' AND discarded_at IS NULL)
			+ (SELECT count(*) FROM ai_jobs WHERE status = 'DEAD' AND discarded_at IS NULL)
			+ (SELECT count(*) FROM notification_deliveries WHERE status = 'DEAD' AND discarded_at IS NULL)`, now.UTC()).Scan(
		&stats.Jobs.Pending, &stats.Jobs.Processing, &stats.Jobs.Retry, &stats.Jobs.Dead, &stats.Jobs.ExpiredLeases,
		&stats.Outbox.Pending, &stats.Outbox.Processing, &stats.Outbox.Retry, &stats.Outbox.Dead, &stats.Outbox.ExpiredLeases,
		&stats.AIJobs.Pending, &stats.AIJobs.Leased, &stats.AIJobs.Running, &stats.AIJobs.Retry, &stats.AIJobs.Dead, &stats.AIJobs.NodesReady,
		&stats.Deliveries.Pending, &stats.Deliveries.Processing, &stats.Deliveries.Retry, &stats.Deliveries.Dead,
		&stats.ScheduledOverdue, &stats.DeadUnhandled,
	)
	if err != nil {
		return domain.QueueStats{}, fmt.Errorf("чтение состояния очередей: %w", err)
	}
	return stats, nil
}

func scanJob(row rowScanner) (domain.JobRecord, error) {
	var job domain.JobRecord
	if err := row.Scan(&job.ID, &job.TenantID, &job.Type, &job.DedupKey, &job.Status, &job.Priority, &job.AvailableAt,
		&job.AttemptCount, &job.MaxAttempts, &job.LeasedBy, &job.LeaseUntil, &job.LastErrorCode, &job.CompletedAt,
		&job.DiscardedAt, &job.CreatedAt, &job.UpdatedAt, &job.Payload); err != nil {
		return domain.JobRecord{}, err
	}
	return job, nil
}

func (store *PostgresStore) Jobs(ctx context.Context, filter application.JobFilter) ([]domain.JobRecord, error) {
	if !store.ready() || filter.Limit < 1 {
		return nil, application.ErrInvalid
	}
	where := []string{"TRUE"}
	arguments := []any{filter.Limit}
	if filter.TenantID != "" {
		if !ids.Valid(filter.TenantID) {
			return nil, application.ErrInvalid
		}
		arguments = append(arguments, filter.TenantID)
		where = append(where, fmt.Sprintf("tenant_id = $%d", len(arguments)))
	}
	if filter.Status != "" {
		arguments = append(arguments, filter.Status)
		where = append(where, fmt.Sprintf("status = $%d", len(arguments)))
	}
	if filter.Type != "" {
		arguments = append(arguments, filter.Type)
		where = append(where, fmt.Sprintf("job_type = $%d", len(arguments)))
	}
	rows, err := store.pool.Query(ctx, `
		SELECT `+jobColumns+` FROM jobs WHERE `+strings.Join(where, " AND ")+`
		ORDER BY created_at DESC, id DESC LIMIT $1`, arguments...)
	if err != nil {
		return nil, fmt.Errorf("чтение заданий: %w", err)
	}
	defer rows.Close()
	result := make([]domain.JobRecord, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("чтение задания: %w", err)
		}
		result = append(result, job)
	}
	return result, rows.Err()
}

func scanOutbox(row rowScanner) (domain.OutboxRecord, error) {
	var event domain.OutboxRecord
	if err := row.Scan(&event.ID, &event.TenantID, &event.EventType, &event.AggregateType, &event.AggregateID, &event.Status,
		&event.AttemptCount, &event.MaxAttempts, &event.LastErrorCode, &event.OccurredAt, &event.CompletedAt, &event.DiscardedAt); err != nil {
		return domain.OutboxRecord{}, err
	}
	return event, nil
}

func scanAIJob(row rowScanner) (domain.AIJobRecord, error) {
	var job domain.AIJobRecord
	if err := row.Scan(&job.ID, &job.TenantID, &job.ConversationID, &job.AnalysisThroughMessageID, &job.Status, &job.ModelRequirement,
		&job.Attempts, &job.MaxAttempts, &job.LastErrorCode, &job.LeasedBy, &job.LeasedAt, &job.LeaseUntil, &job.CompletedAt,
		&job.DiscardedAt, &job.CreatedAt); err != nil {
		return domain.AIJobRecord{}, err
	}
	return job, nil
}

func scanDelivery(row rowScanner) (domain.DeliveryRecord, error) {
	var delivery domain.DeliveryRecord
	if err := row.Scan(&delivery.ID, &delivery.TenantID, &delivery.NotificationID, &delivery.Kind, &delivery.Channel, &delivery.Status,
		&delivery.Attempt, &delivery.FailureCode, &delivery.AttemptedAt, &delivery.DiscardedAt, &delivery.CreatedAt); err != nil {
		return domain.DeliveryRecord{}, err
	}
	return delivery, nil
}

func (store *PostgresStore) DeadLetters(ctx context.Context, limit int) (domain.DeadLetters, error) {
	if !store.ready() || limit < 1 {
		return domain.DeadLetters{}, application.ErrInvalid
	}
	letters := domain.DeadLetters{
		Jobs: []domain.JobRecord{}, Outbox: []domain.OutboxRecord{}, AIJobs: []domain.AIJobRecord{}, Deliveries: []domain.DeliveryRecord{},
	}
	jobRows, err := store.pool.Query(ctx, `
		SELECT `+jobColumns+` FROM jobs WHERE status = 'DEAD' AND discarded_at IS NULL
		ORDER BY updated_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return domain.DeadLetters{}, fmt.Errorf("чтение мёртвых заданий: %w", err)
	}
	for jobRows.Next() {
		job, err := scanJob(jobRows)
		if err != nil {
			jobRows.Close()
			return domain.DeadLetters{}, fmt.Errorf("чтение мёртвого задания: %w", err)
		}
		letters.Jobs = append(letters.Jobs, job)
	}
	jobRows.Close()
	outboxRows, err := store.pool.Query(ctx, `
		SELECT `+outboxColumns+` FROM outbox_events WHERE status = 'DEAD' AND discarded_at IS NULL
		ORDER BY updated_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return domain.DeadLetters{}, fmt.Errorf("чтение мёртвых событий: %w", err)
	}
	for outboxRows.Next() {
		event, err := scanOutbox(outboxRows)
		if err != nil {
			outboxRows.Close()
			return domain.DeadLetters{}, fmt.Errorf("чтение мёртвого события: %w", err)
		}
		letters.Outbox = append(letters.Outbox, event)
	}
	outboxRows.Close()
	aiRows, err := store.pool.Query(ctx, `
		SELECT `+aiJobColumns+` FROM ai_jobs WHERE status = 'DEAD' AND discarded_at IS NULL
		ORDER BY updated_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return domain.DeadLetters{}, fmt.Errorf("чтение мёртвых AI-заданий: %w", err)
	}
	for aiRows.Next() {
		job, err := scanAIJob(aiRows)
		if err != nil {
			aiRows.Close()
			return domain.DeadLetters{}, fmt.Errorf("чтение мёртвого AI-задания: %w", err)
		}
		letters.AIJobs = append(letters.AIJobs, job)
	}
	aiRows.Close()
	deliveryRows, err := store.pool.Query(ctx, `
		SELECT `+deliveryColumns+` FROM notification_deliveries WHERE status = 'DEAD' AND discarded_at IS NULL
		ORDER BY updated_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return domain.DeadLetters{}, fmt.Errorf("чтение мёртвых доставок: %w", err)
	}
	for deliveryRows.Next() {
		delivery, err := scanDelivery(deliveryRows)
		if err != nil {
			deliveryRows.Close()
			return domain.DeadLetters{}, fmt.Errorf("чтение мёртвой доставки: %w", err)
		}
		letters.Deliveries = append(letters.Deliveries, delivery)
	}
	deliveryRows.Close()
	return letters, nil
}

// command выполняет UPDATE над одной мёртвой строкой и пишет аудит с
// организацией строки. Отсутствие строки — ErrNotFound, иное состояние — ErrConflict.
func (store *PostgresStore) command(
	ctx context.Context,
	table, id, update string,
	arguments []any,
	audit domain.AuditEntry,
	scan func(rowScanner) (string, error),
) error {
	if !store.ready() || !ids.Valid(id) {
		return application.ErrInvalid
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начало административной команды: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := scan(tx.QueryRow(ctx, update, arguments...))
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if probe := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id = $1)`, id).Scan(&exists); probe != nil {
			return fmt.Errorf("проверка объекта команды: %w", probe)
		}
		if !exists {
			return application.ErrNotFound
		}
		return application.ErrConflict
	}
	if err != nil {
		return mapAdminError("административная команда", err)
	}
	audit.TenantID = &tenantID
	if err := insertAudit(ctx, tx, audit); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("фиксация административной команды: %w", err)
	}
	return nil
}

func (store *PostgresStore) RetryJob(ctx context.Context, jobID string, at time.Time, audit domain.AuditEntry) (domain.JobRecord, error) {
	var job domain.JobRecord
	err := store.command(ctx, "jobs", jobID, `
		UPDATE jobs SET status = 'PENDING', attempt_count = 0, available_at = $2, leased_by = NULL, lease_until = NULL,
		    completed_at = NULL, discarded_at = NULL, discarded_by = NULL, updated_at = $2
		WHERE id = $1 AND status = 'DEAD'
		RETURNING `+jobColumns, []any{jobID, at.UTC()}, audit, func(row rowScanner) (string, error) {
		var scanErr error
		job, scanErr = scanJob(row)
		return job.TenantID, scanErr
	})
	return job, err
}

func (store *PostgresStore) DiscardJob(ctx context.Context, jobID, actorID string, at time.Time, audit domain.AuditEntry) (domain.JobRecord, error) {
	var job domain.JobRecord
	err := store.command(ctx, "jobs", jobID, `
		UPDATE jobs SET discarded_at = $2, discarded_by = $3, updated_at = $2
		WHERE id = $1 AND status = 'DEAD' AND discarded_at IS NULL
		RETURNING `+jobColumns, []any{jobID, at.UTC(), actorID}, audit, func(row rowScanner) (string, error) {
		var scanErr error
		job, scanErr = scanJob(row)
		return job.TenantID, scanErr
	})
	return job, err
}

func (store *PostgresStore) ReplayEvent(ctx context.Context, eventID string, at time.Time, audit domain.AuditEntry) (domain.OutboxRecord, error) {
	var event domain.OutboxRecord
	err := store.command(ctx, "outbox_events", eventID, `
		UPDATE outbox_events SET status = 'PENDING', attempt_count = 0, available_at = $2, leased_by = NULL, lease_until = NULL,
		    completed_at = NULL, discarded_at = NULL, discarded_by = NULL, updated_at = $2
		WHERE id = $1 AND status = 'DEAD'
		RETURNING `+outboxColumns, []any{eventID, at.UTC()}, audit, func(row rowScanner) (string, error) {
		var scanErr error
		event, scanErr = scanOutbox(row)
		return event.TenantID, scanErr
	})
	return event, err
}

func (store *PostgresStore) DiscardEvent(ctx context.Context, eventID, actorID string, at time.Time, audit domain.AuditEntry) (domain.OutboxRecord, error) {
	var event domain.OutboxRecord
	err := store.command(ctx, "outbox_events", eventID, `
		UPDATE outbox_events SET discarded_at = $2, discarded_by = $3, updated_at = $2
		WHERE id = $1 AND status = 'DEAD' AND discarded_at IS NULL
		RETURNING `+outboxColumns, []any{eventID, at.UTC(), actorID}, audit, func(row rowScanner) (string, error) {
		var scanErr error
		event, scanErr = scanOutbox(row)
		return event.TenantID, scanErr
	})
	return event, err
}

func (store *PostgresStore) RetryAIJob(ctx context.Context, jobID string, at time.Time, audit domain.AuditEntry) (domain.AIJobRecord, error) {
	var job domain.AIJobRecord
	err := store.command(ctx, "ai_jobs", jobID, `
		UPDATE ai_jobs SET status = 'PENDING', attempts = 0, available_at = $2, leased_by = NULL, lease_until = NULL, leased_at = NULL,
		    completed_at = NULL, discarded_at = NULL, discarded_by = NULL, updated_at = $2
		WHERE id = $1 AND status = 'DEAD'
		RETURNING `+aiJobColumns, []any{jobID, at.UTC()}, audit, func(row rowScanner) (string, error) {
		var scanErr error
		job, scanErr = scanAIJob(row)
		return job.TenantID, scanErr
	})
	return job, err
}

func (store *PostgresStore) DiscardAIJob(ctx context.Context, jobID, actorID string, at time.Time, audit domain.AuditEntry) (domain.AIJobRecord, error) {
	var job domain.AIJobRecord
	err := store.command(ctx, "ai_jobs", jobID, `
		UPDATE ai_jobs SET discarded_at = $2, discarded_by = $3, updated_at = $2
		WHERE id = $1 AND status = 'DEAD' AND discarded_at IS NULL
		RETURNING `+aiJobColumns, []any{jobID, at.UTC(), actorID}, audit, func(row rowScanner) (string, error) {
		var scanErr error
		job, scanErr = scanAIJob(row)
		return job.TenantID, scanErr
	})
	return job, err
}

func (store *PostgresStore) DiscardDelivery(ctx context.Context, deliveryID, actorID string, at time.Time, audit domain.AuditEntry) (domain.DeliveryRecord, error) {
	var delivery domain.DeliveryRecord
	err := store.command(ctx, "notification_deliveries", deliveryID, `
		UPDATE notification_deliveries SET discarded_at = $2, discarded_by = $3, updated_at = $2
		WHERE id = $1 AND status = 'DEAD' AND discarded_at IS NULL
		RETURNING `+deliveryColumns, []any{deliveryID, at.UTC(), actorID}, audit, func(row rowScanner) (string, error) {
		var scanErr error
		delivery, scanErr = scanDelivery(row)
		return delivery.TenantID, scanErr
	})
	return delivery, err
}

func (store *PostgresStore) AINodes(ctx context.Context) ([]domain.AINode, error) {
	if !store.ready() {
		return nil, application.ErrInvalid
	}
	rows, err := store.pool.Query(ctx, `
		SELECT n.id::text, n.name, n.status, n.model_version, n.available_slots, n.last_heartbeat_at, n.revoked_at, n.created_at,
		       COALESCE((SELECT array_agg(t.tenant_id::text ORDER BY t.created_at) FROM ai_node_tenants AS t WHERE t.node_id = n.id), '{}'::text[]),
		       (SELECT count(*) FROM ai_jobs AS j WHERE j.leased_by = n.id AND j.status IN ('LEASED', 'RUNNING'))
		FROM ai_nodes AS n
		ORDER BY n.created_at, n.id`)
	if err != nil {
		return nil, fmt.Errorf("чтение AI-узлов: %w", err)
	}
	defer rows.Close()
	result := make([]domain.AINode, 0)
	for rows.Next() {
		var node domain.AINode
		if err := rows.Scan(&node.ID, &node.Name, &node.Status, &node.ModelVersion, &node.AvailableSlots, &node.LastHeartbeatAt,
			&node.RevokedAt, &node.CreatedAt, &node.Tenants, &node.Inflight); err != nil {
			return nil, fmt.Errorf("чтение AI-узла: %w", err)
		}
		if node.Tenants == nil {
			node.Tenants = []string{}
		}
		result = append(result, node)
	}
	return result, rows.Err()
}

func scanAIRun(row rowScanner) (domain.AIRun, error) {
	var run domain.AIRun
	if err := row.Scan(&run.ID, &run.TenantID, &run.JobID, &run.NodeID, &run.ConversationID, &run.Status, &run.ApplicationStatus,
		&run.ModelVersion, &run.PromptVersion, &run.SchemaVersion, &run.ErrorCode, &run.ValidationError, &run.StartedAt, &run.CompletedAt); err != nil {
		return domain.AIRun{}, err
	}
	if run.CompletedAt != nil {
		duration := run.CompletedAt.Sub(run.StartedAt).Milliseconds()
		run.DurationMs = &duration
	}
	return run, nil
}

func (store *PostgresStore) AIRuns(ctx context.Context, filter application.RunFilter) ([]domain.AIRun, error) {
	if !store.ready() || filter.Limit < 1 {
		return nil, application.ErrInvalid
	}
	where := []string{"TRUE"}
	arguments := []any{filter.Limit}
	if filter.TenantID != "" {
		if !ids.Valid(filter.TenantID) {
			return nil, application.ErrInvalid
		}
		arguments = append(arguments, filter.TenantID)
		where = append(where, fmt.Sprintf("tenant_id = $%d", len(arguments)))
	}
	if filter.Status != "" {
		arguments = append(arguments, filter.Status)
		where = append(where, fmt.Sprintf("status = $%d", len(arguments)))
	}
	if filter.ApplicationStatus != "" {
		arguments = append(arguments, filter.ApplicationStatus)
		where = append(where, fmt.Sprintf("application_status = $%d", len(arguments)))
	}
	rows, err := store.pool.Query(ctx, `
		SELECT `+aiRunColumns+` FROM ai_runs WHERE `+strings.Join(where, " AND ")+`
		ORDER BY started_at DESC, id DESC LIMIT $1`, arguments...)
	if err != nil {
		return nil, fmt.Errorf("чтение AI-прогонов: %w", err)
	}
	defer rows.Close()
	result := make([]domain.AIRun, 0)
	for rows.Next() {
		run, err := scanAIRun(rows)
		if err != nil {
			return nil, fmt.Errorf("чтение AI-прогона: %w", err)
		}
		result = append(result, run)
	}
	return result, rows.Err()
}

func (store *PostgresStore) ConversationSummary(ctx context.Context, tenantID, conversationID string) (domain.ConversationSummary, bool, error) {
	if !store.ready() || !ids.Valid(tenantID) || !ids.Valid(conversationID) {
		return domain.ConversationSummary{}, false, application.ErrInvalid
	}
	summary, err := scanSummary(store.pool.QueryRow(ctx, `
		SELECT tenant_id::text, conversation_id::text, base_conversation_revision, analysis_through_message_id::text,
		       model_version, prompt_version, schema_version, ai_run_id::text, updated_at, semantic_facts
		FROM conversation_summaries WHERE tenant_id = $1 AND conversation_id = $2`, tenantID, conversationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ConversationSummary{}, false, nil
	}
	if err != nil {
		return domain.ConversationSummary{}, false, fmt.Errorf("чтение семантического результата: %w", err)
	}
	return summary, true, nil
}

func scanSummary(row rowScanner) (domain.ConversationSummary, error) {
	var summary domain.ConversationSummary
	var facts []byte
	if err := row.Scan(&summary.TenantID, &summary.ConversationID, &summary.Revision, &summary.AnalysisThroughMessageID,
		&summary.ModelVersion, &summary.PromptVersion, &summary.SchemaVersion, &summary.AIRunID, &summary.UpdatedAt, &facts); err != nil {
		return domain.ConversationSummary{}, err
	}
	summary.Facts = []domain.SemanticFact{}
	if len(facts) > 0 {
		if err := json.Unmarshal(facts, &summary.Facts); err != nil {
			return domain.ConversationSummary{}, fmt.Errorf("разбор семантических фактов: %w", err)
		}
	}
	for index := range summary.Facts {
		if summary.Facts[index].EvidenceMessageIDs == nil {
			summary.Facts[index].EvidenceMessageIDs = []string{}
		}
		if summary.Facts[index].Trusted {
			summary.TrustedFacts++
		} else {
			summary.WeakFacts++
		}
	}
	return summary, nil
}

func (store *PostgresStore) Usage(ctx context.Context, from, to time.Time) ([]domain.TenantUsage, error) {
	if !store.ready() || from.IsZero() || !from.Before(to) {
		return nil, application.ErrInvalid
	}
	rows, err := store.pool.Query(ctx, `
		SELECT o.id::text, o.name,
		       (SELECT count(*) FROM messages AS m WHERE m.tenant_id = o.id AND m.sent_at >= $1 AND m.sent_at < $2),
		       (SELECT count(*) FROM raw_events AS e WHERE e.tenant_id = o.id AND e.received_at >= $1 AND e.received_at < $2),
		       (SELECT count(*) FROM jobs AS j WHERE j.tenant_id = o.id AND j.created_at >= $1 AND j.created_at < $2),
		       (SELECT count(*) FROM ai_jobs AS a WHERE a.tenant_id = o.id AND a.created_at >= $1 AND a.created_at < $2),
		       (SELECT count(*) FROM ai_runs AS r WHERE r.tenant_id = o.id AND r.started_at >= $1 AND r.started_at < $2),
		       (SELECT count(*) FROM ai_runs AS r WHERE r.tenant_id = o.id AND r.started_at >= $1 AND r.started_at < $2 AND r.application_status = 'APPLIED'),
		       (SELECT count(*) FROM ai_runs AS r WHERE r.tenant_id = o.id AND r.started_at >= $1 AND r.started_at < $2 AND r.application_status = 'REJECTED'),
		       (SELECT count(*) FROM ai_runs AS r WHERE r.tenant_id = o.id AND r.started_at >= $1 AND r.started_at < $2 AND r.application_status = 'STALE'),
		       (SELECT COALESCE(sum(EXTRACT(EPOCH FROM (r.completed_at - r.started_at))), 0)::double precision
		        FROM ai_runs AS r WHERE r.tenant_id = o.id AND r.started_at >= $1 AND r.started_at < $2 AND r.completed_at IS NOT NULL),
		       (SELECT count(*) FROM risk_signals AS rs WHERE rs.tenant_id = o.id AND rs.detected_at >= $1 AND rs.detected_at < $2),
		       (SELECT count(*) FROM notifications AS n WHERE n.tenant_id = o.id AND n.created_at >= $1 AND n.created_at < $2),
		       (SELECT count(*) FROM notification_deliveries AS d WHERE d.tenant_id = o.id AND d.created_at >= $1 AND d.created_at < $2)
		FROM organizations AS o
		ORDER BY o.name, o.id`, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("чтение потребления: %w", err)
	}
	defer rows.Close()
	result := make([]domain.TenantUsage, 0)
	for rows.Next() {
		var usage domain.TenantUsage
		if err := rows.Scan(&usage.TenantID, &usage.Name, &usage.Messages, &usage.RawEvents, &usage.Jobs, &usage.AIJobs, &usage.AIRuns,
			&usage.AIRunsApplied, &usage.AIRunsRejected, &usage.AIRunsStale, &usage.AIRunSeconds, &usage.Risks,
			&usage.Notifications, &usage.Deliveries); err != nil {
			return nil, fmt.Errorf("чтение потребления организации: %w", err)
		}
		result = append(result, usage)
	}
	return result, rows.Err()
}

// Trace собирает цепочку LR-BE-2310 по сообщению и его переписке.
func (store *PostgresStore) Trace(ctx context.Context, tenantID, messageID string) (domain.Trace, bool, error) {
	if !store.ready() || !ids.Valid(tenantID) || !ids.Valid(messageID) {
		return domain.Trace{}, false, application.ErrInvalid
	}
	var trace domain.Trace
	err := store.pool.QueryRow(ctx, `
		SELECT id::text, tenant_id::text, conversation_id::text, connection_id::text, direction, type, external_id, sent_at, received_at
		FROM messages WHERE tenant_id = $1 AND id = $2`, tenantID, messageID).Scan(
		&trace.Message.ID, &trace.Message.TenantID, &trace.Message.ConversationID, &trace.Message.ConnectionID, &trace.Message.Direction,
		&trace.Message.Type, &trace.Message.ExternalID, &trace.Message.SentAt, &trace.Message.ReceivedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Trace{}, false, nil
	}
	if err != nil {
		return domain.Trace{}, false, fmt.Errorf("чтение сообщения трассы: %w", err)
	}
	conversationID := trace.Message.ConversationID
	trace.Jobs, trace.AIJobs, trace.AIRuns = []domain.JobRecord{}, []domain.AIJobRecord{}, []domain.AIRun{}
	trace.Risks, trace.Notifications, trace.Actions = []domain.TraceRisk{}, []domain.TraceNotification{}, []domain.TraceAction{}
	trace.Outcomes, trace.Revenue = []domain.TraceOutcome{}, []domain.TraceRevenue{}

	if err := store.collect(ctx, `
		SELECT `+jobColumns+` FROM jobs
		WHERE tenant_id = $1 AND (payload ->> 'messageId' = $2::text OR payload ->> 'conversationId' = $3::text
		   OR payload ->> 'opportunityId' IN (SELECT id::text FROM opportunities WHERE tenant_id = $1 AND conversation_id = $3::uuid))
		ORDER BY created_at, id LIMIT 200`, []any{tenantID, messageID, conversationID}, func(row rowScanner) error {
		job, err := scanJob(row)
		trace.Jobs = append(trace.Jobs, job)
		return err
	}); err != nil {
		return domain.Trace{}, false, fmt.Errorf("чтение заданий трассы: %w", err)
	}
	if err := store.collect(ctx, `
		SELECT `+aiJobColumns+` FROM ai_jobs WHERE tenant_id = $1 AND entity_id = $2 ORDER BY created_at, id LIMIT 200`,
		[]any{tenantID, conversationID}, func(row rowScanner) error {
			job, err := scanAIJob(row)
			trace.AIJobs = append(trace.AIJobs, job)
			return err
		}); err != nil {
		return domain.Trace{}, false, fmt.Errorf("чтение AI-заданий трассы: %w", err)
	}
	if err := store.collect(ctx, `
		SELECT `+aiRunColumns+` FROM ai_runs WHERE tenant_id = $1 AND entity_id = $2 ORDER BY started_at, id LIMIT 200`,
		[]any{tenantID, conversationID}, func(row rowScanner) error {
			run, err := scanAIRun(row)
			trace.AIRuns = append(trace.AIRuns, run)
			return err
		}); err != nil {
		return domain.Trace{}, false, fmt.Errorf("чтение AI-прогонов трассы: %w", err)
	}
	summary, found, err := store.ConversationSummary(ctx, tenantID, conversationID)
	if err != nil {
		return domain.Trace{}, false, err
	}
	if found {
		trace.Summary = &summary
	}
	if err := store.collect(ctx, `
		SELECT id::text, opportunity_id::text, type, severity, status, source, risk_engine_version, ai_run_id::text, detected_at, resolved_at
		FROM risk_signals
		WHERE tenant_id = $1 AND (trigger_message_id = $2::uuid
		   OR opportunity_id IN (SELECT id FROM opportunities WHERE tenant_id = $1 AND conversation_id = $3::uuid))
		ORDER BY detected_at, id LIMIT 200`, []any{tenantID, messageID, conversationID}, func(row rowScanner) error {
		var risk domain.TraceRisk
		err := row.Scan(&risk.ID, &risk.OpportunityID, &risk.Type, &risk.Severity, &risk.Status, &risk.Source, &risk.PolicyVersion,
			&risk.AIRunID, &risk.DetectedAt, &risk.ResolvedAt)
		trace.Risks = append(trace.Risks, risk)
		return err
	}); err != nil {
		return domain.Trace{}, false, fmt.Errorf("чтение рисков трассы: %w", err)
	}
	riskIDs := make([]string, 0, len(trace.Risks))
	for _, risk := range trace.Risks {
		riskIDs = append(riskIDs, risk.ID)
	}
	if err := store.collect(ctx, `
		SELECT id::text, user_id::text, kind, risk_id::text, dedup_key, created_at FROM notifications
		WHERE tenant_id = $1 AND risk_id = ANY($2::uuid[]) ORDER BY created_at, id LIMIT 200`, []any{tenantID, riskIDs}, func(row rowScanner) error {
		var notification domain.TraceNotification
		err := row.Scan(&notification.ID, &notification.UserID, &notification.Kind, &notification.RiskID, &notification.DedupKey, &notification.CreatedAt)
		notification.Deliveries = []domain.DeliveryRecord{}
		trace.Notifications = append(trace.Notifications, notification)
		return err
	}); err != nil {
		return domain.Trace{}, false, fmt.Errorf("чтение уведомлений трассы: %w", err)
	}
	for index := range trace.Notifications {
		if err := store.collect(ctx, `
			SELECT `+deliveryColumns+` FROM notification_deliveries WHERE tenant_id = $1 AND notification_id = $2 ORDER BY attempt, created_at`,
			[]any{tenantID, trace.Notifications[index].ID}, func(row rowScanner) error {
				delivery, err := scanDelivery(row)
				trace.Notifications[index].Deliveries = append(trace.Notifications[index].Deliveries, delivery)
				return err
			}); err != nil {
			return domain.Trace{}, false, fmt.Errorf("чтение доставок трассы: %w", err)
		}
	}
	if err := store.collect(ctx, `
		SELECT id::text, risk_id::text, type, actor_user_id::text, created_at FROM actions
		WHERE tenant_id = $1 AND risk_id = ANY($2::uuid[]) ORDER BY created_at, id LIMIT 200`, []any{tenantID, riskIDs}, func(row rowScanner) error {
		var action domain.TraceAction
		err := row.Scan(&action.ID, &action.RiskID, &action.Type, &action.ActorID, &action.CreatedAt)
		trace.Actions = append(trace.Actions, action)
		return err
	}); err != nil {
		return domain.Trace{}, false, fmt.Errorf("чтение действий трассы: %w", err)
	}
	if err := store.collect(ctx, `
		SELECT id::text, opportunity_id::text, status, actor_user_id::text, created_at FROM outcomes
		WHERE tenant_id = $1 AND opportunity_id IN (SELECT id FROM opportunities WHERE tenant_id = $1 AND conversation_id = $2)
		ORDER BY created_at, id LIMIT 200`, []any{tenantID, conversationID}, func(row rowScanner) error {
		var outcome domain.TraceOutcome
		err := row.Scan(&outcome.ID, &outcome.OpportunityID, &outcome.Status, &outcome.ActorID, &outcome.CreatedAt)
		trace.Outcomes = append(trace.Outcomes, outcome)
		return err
	}); err != nil {
		return domain.Trace{}, false, fmt.Errorf("чтение исходов трассы: %w", err)
	}
	if err := store.collect(ctx, `
		SELECT event.id::text, event.opportunity_id::text, event.amount::text, event.currency, event.status,
		       attribution.type, attribution.risk_id::text, event.confirmed_at
		FROM revenue_events AS event
		LEFT JOIN revenue_attributions AS attribution
		  ON attribution.tenant_id = event.tenant_id AND attribution.revenue_event_id = event.id
		WHERE event.tenant_id = $1 AND event.opportunity_id IN (SELECT id FROM opportunities WHERE tenant_id = $1 AND conversation_id = $2)
		ORDER BY event.confirmed_at, event.id LIMIT 200`, []any{tenantID, conversationID}, func(row rowScanner) error {
		var revenue domain.TraceRevenue
		err := row.Scan(&revenue.EventID, &revenue.OpportunityID, &revenue.Amount, &revenue.Currency, &revenue.Status,
			&revenue.Attribution, &revenue.RiskID, &revenue.ConfirmedAt)
		trace.Revenue = append(trace.Revenue, revenue)
		return err
	}); err != nil {
		return domain.Trace{}, false, fmt.Errorf("чтение выручки трассы: %w", err)
	}
	return trace, true, nil
}

func (store *PostgresStore) collect(ctx context.Context, query string, arguments []any, each func(rowScanner) error) error {
	rows, err := store.pool.Query(ctx, query, arguments...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := each(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func mapAdminError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "23505":
			return application.ErrConflict
		case "23503", "23514", "22P02":
			return application.ErrInvalid
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}

var _ application.Store = (*PostgresStore)(nil)
