// Package infrastructure хранит исходящий журнал в PostgreSQL.
package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/events/domain"
)

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

// Append добавляет событие отдельной операцией.
func (store *PostgresStore) Append(ctx context.Context, event domain.Event) (domain.Event, bool, error) {
	if store == nil || store.pool == nil || event.Status != domain.StatusPending || event.Validate() != nil {
		return domain.Event{}, false, domain.ErrInvalid
	}
	return appendEvent(ctx, store.pool, event)
}

// AppendTx добавляет событие в уже открытую транзакцию владельца бизнес-
// изменения. Это основной вход для transactional outbox.
func (store *PostgresStore) AppendTx(ctx context.Context, tx pgx.Tx, event domain.Event) (domain.Event, bool, error) {
	if store == nil || store.pool == nil || tx == nil || event.Status != domain.StatusPending || event.Validate() != nil {
		return domain.Event{}, false, domain.ErrInvalid
	}
	return appendEvent(ctx, tx, event)
}

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func appendEvent(ctx context.Context, queryer queryRower, event domain.Event) (domain.Event, bool, error) {
	persisted, err := scanEvent(queryer.QueryRow(ctx, `
		INSERT INTO outbox_events(
			id, tenant_id, event_type, event_version, aggregate_type, aggregate_id,
			trace_id, data, status, available_at, attempt_count, max_attempts,
			occurred_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, 'PENDING', $9, 0, $10, $11, $12, $12)
		ON CONFLICT (id) DO NOTHING
		RETURNING id, tenant_id, event_type, event_version, aggregate_type, aggregate_id,
		          trace_id, data, status, available_at, attempt_count, max_attempts,
		          leased_by, lease_until, last_error_code, occurred_at, completed_at,
		          created_at, updated_at`,
		event.ID, event.TenantID, event.Type, event.Version, event.AggregateType,
		event.AggregateID, event.TraceID, string(event.Data), event.AvailableAt,
		event.MaxAttempts, event.OccurredAt, event.CreatedAt,
	))
	if err == nil {
		return persisted, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Event{}, false, mapError("добавление события", err)
	}
	var equivalent bool
	persisted, err = scanEvent(queryer.QueryRow(ctx, `
		SELECT id, tenant_id, event_type, event_version, aggregate_type, aggregate_id,
		       trace_id, data, status, available_at, attempt_count, max_attempts,
		       leased_by, lease_until, last_error_code, occurred_at, completed_at,
		       created_at, updated_at,
		       tenant_id = $2 AND event_type = $3 AND event_version = $4
		       AND aggregate_type = $5 AND aggregate_id = $6 AND trace_id = $7
		       AND data = $8::jsonb AND occurred_at = $9
		FROM outbox_events WHERE id = $1`,
		event.ID, event.TenantID, event.Type, event.Version, event.AggregateType,
		event.AggregateID, event.TraceID, string(event.Data), event.OccurredAt,
	), &equivalent)
	if err != nil {
		return domain.Event{}, false, mapError("чтение повторного события", err)
	}
	if !equivalent {
		return domain.Event{}, false, domain.ErrConflict
	}
	return persisted, false, nil
}

func (store *PostgresStore) Claim(
	ctx context.Context,
	owner string,
	now, leaseUntil time.Time,
	limit int,
) ([]domain.Event, error) {
	if store == nil || store.pool == nil || owner == "" || now.IsZero() || !leaseUntil.After(now) || limit < 1 || limit > 100 {
		return nil, domain.ErrInvalid
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("начало захвата событий: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE outbox_events
		SET status = 'DEAD', leased_by = NULL, lease_until = NULL,
		    last_error_code = 'LEASE_EXPIRED_MAX_ATTEMPTS', completed_at = $1, updated_at = $1
		WHERE status = 'PROCESSING' AND lease_until <= $1 AND attempt_count >= max_attempts`, now.UTC()); err != nil {
		return nil, mapError("завершение событий с исчерпанной арендой", err)
	}
	rows, err := tx.Query(ctx, `
		WITH candidates AS (
			SELECT id
			FROM outbox_events
			WHERE attempt_count < max_attempts
			  AND (
				(status IN ('PENDING', 'RETRY') AND available_at <= $1)
				OR (status = 'PROCESSING' AND lease_until <= $1)
			  )
			ORDER BY available_at, occurred_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $4
		)
		UPDATE outbox_events AS event
		SET status = 'PROCESSING', attempt_count = event.attempt_count + 1,
		    leased_by = $2, lease_until = $3, updated_at = $1
		FROM candidates
		WHERE event.id = candidates.id
		RETURNING event.id, event.tenant_id, event.event_type, event.event_version,
		          event.aggregate_type, event.aggregate_id, event.trace_id, event.data,
		          event.status, event.available_at, event.attempt_count, event.max_attempts,
		          event.leased_by, event.lease_until, event.last_error_code,
		          event.occurred_at, event.completed_at, event.created_at, event.updated_at`,
		now.UTC(), owner, leaseUntil.UTC(), limit)
	if err != nil {
		return nil, mapError("захват событий", err)
	}
	events := make([]domain.Event, 0, limit)
	for rows.Next() {
		event, scanErr := scanEvent(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("обход захваченных событий: %w", err)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("фиксация захвата событий: %w", err)
	}
	sort.Slice(events, func(i, j int) bool {
		if !events[i].AvailableAt.Equal(events[j].AvailableAt) {
			return events[i].AvailableAt.Before(events[j].AvailableAt)
		}
		return events[i].ID < events[j].ID
	})
	return events, nil
}

func (store *PostgresStore) Publish(ctx context.Context, eventID, owner string, at time.Time) error {
	if store == nil || store.pool == nil || eventID == "" || owner == "" || at.IsZero() {
		return domain.ErrInvalid
	}
	result, err := store.pool.Exec(ctx, `
		UPDATE outbox_events
		SET status = 'PUBLISHED', leased_by = NULL, lease_until = NULL,
		    completed_at = $3, updated_at = $3
		WHERE id = $1 AND status = 'PROCESSING' AND leased_by = $2 AND lease_until > $3`,
		eventID, owner, at.UTC())
	if err != nil {
		return mapError("публикация события", err)
	}
	if result.RowsAffected() != 1 {
		return domain.ErrLeaseLost
	}
	return nil
}

func (store *PostgresStore) Fail(
	ctx context.Context,
	eventID, owner, code string,
	retryable bool,
	next, at time.Time,
) (domain.Status, error) {
	if store == nil || store.pool == nil || eventID == "" || owner == "" || code == "" ||
		next.IsZero() || at.IsZero() || next.Before(at) {
		return "", domain.ErrInvalid
	}
	var status domain.Status
	err := store.pool.QueryRow(ctx, `
		UPDATE outbox_events
		SET status = CASE WHEN $4 AND attempt_count < max_attempts THEN 'RETRY' ELSE 'DEAD' END,
		    available_at = CASE WHEN $4 AND attempt_count < max_attempts THEN $5 ELSE available_at END,
		    leased_by = NULL,
		    lease_until = NULL,
		    last_error_code = $3,
		    completed_at = CASE WHEN $4 AND attempt_count < max_attempts THEN NULL ELSE $6 END,
		    updated_at = $6
		WHERE id = $1 AND status = 'PROCESSING' AND leased_by = $2 AND lease_until > $6
		RETURNING status`, eventID, owner, code, retryable, next.UTC(), at.UTC()).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrLeaseLost
	}
	if err != nil {
		return "", mapError("завершение ошибочного события", err)
	}
	return status, nil
}

type rowScanner interface{ Scan(...any) error }

func scanEvent(row rowScanner, extra ...any) (domain.Event, error) {
	var event domain.Event
	values := []any{
		&event.ID, &event.TenantID, &event.Type, &event.Version, &event.AggregateType,
		&event.AggregateID, &event.TraceID, &event.Data, &event.Status, &event.AvailableAt,
		&event.AttemptCount, &event.MaxAttempts, &event.LeasedBy, &event.LeaseUntil,
		&event.LastErrorCode, &event.OccurredAt, &event.CompletedAt, &event.CreatedAt, &event.UpdatedAt,
	}
	values = append(values, extra...)
	if err := row.Scan(values...); err != nil {
		return domain.Event{}, err
	}
	if event.Validate() != nil {
		return domain.Event{}, domain.ErrInvalid
	}
	return event, nil
}

func mapError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "23503", "23514", "22P02":
			return domain.ErrInvalid
		case "23505":
			return domain.ErrConflict
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}
