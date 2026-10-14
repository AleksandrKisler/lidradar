package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"lidradar/backend/internal/identity/domain"
)

// Назначение и отмена закрытия учётной записи (ADR 0059). Как команды ADR 0058, каждая выполняется одной
// транзакцией вместе со своей записью в auth_audit_log, а строка пользователя блокируется первой: исполнитель
// обезличивания (модуль lifecycle) берёт ту же блокировку, поэтому назначение, отмена и исполнение идут по очереди.

func (r *PostgresRepository) ScheduleDeletion(ctx context.Context, schedule domain.DeletionSchedule) (time.Time, bool, error) {
	if r == nil || r.pool == nil || schedule.UserID == "" || !schedule.Due.After(schedule.At) || schedule.Audit.ID == "" {
		return time.Time{}, false, domain.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("begin deletion schedule: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var current *time.Time
	err = tx.QueryRow(ctx, `SELECT deletion_due_at FROM users WHERE id = $1 AND status = 'ACTIVE' FOR UPDATE`, schedule.UserID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, domain.ErrNotFound
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("lock user for deletion schedule: %w", err)
	}
	if current != nil {
		// Уже назначено другим запросом: срок не двигается, аудит не дублируется.
		return current.UTC(), false, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE users SET deletion_requested_at = $2, deletion_due_at = $3, updated_at = $2
		WHERE id = $1`, schedule.UserID, schedule.At.UTC(), schedule.Due.UTC()); err != nil {
		return time.Time{}, false, fmt.Errorf("schedule account deletion: %w", err)
	}
	if err := insertAuditRecord(ctx, tx, schedule.UserID, schedule.Audit); err != nil {
		return time.Time{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return time.Time{}, false, fmt.Errorf("commit deletion schedule: %w", err)
	}
	return schedule.Due.UTC(), true, nil
}

func (r *PostgresRepository) CancelDeletion(ctx context.Context, userID string, at time.Time, audit domain.AuditRecord) (bool, error) {
	if r == nil || r.pool == nil || userID == "" || at.IsZero() || audit.ID == "" {
		return false, domain.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin deletion cancel: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var current *time.Time
	err = tx.QueryRow(ctx, `SELECT deletion_due_at FROM users WHERE id = $1 AND status = 'ACTIVE' FOR UPDATE`, userID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, domain.ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("lock user for deletion cancel: %w", err)
	}
	if current == nil {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE users SET deletion_requested_at = NULL, deletion_due_at = NULL, updated_at = $2
		WHERE id = $1`, userID, at.UTC()); err != nil {
		return false, fmt.Errorf("cancel account deletion: %w", err)
	}
	if err := insertAuditRecord(ctx, tx, userID, audit); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit deletion cancel: %w", err)
	}
	return true, nil
}
