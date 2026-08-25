// Package infrastructure хранит фоновые задания и проверки в PostgreSQL.
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

	"lidradar/backend/internal/jobs/domain"
)

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func (store *PostgresStore) Enqueue(ctx context.Context, job domain.Job) (domain.Job, bool, error) {
	if store == nil || store.pool == nil || job.Status != domain.StatusPending || job.Validate() != nil {
		return domain.Job{}, false, domain.ErrInvalid
	}
	persisted, err := scanJob(store.pool.QueryRow(ctx, `
		INSERT INTO jobs(
			id, tenant_id, job_type, dedup_key, payload, status, priority, available_at,
			attempt_count, max_attempts, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5::jsonb, 'PENDING', $6, $7, 0, $8, $9, $9)
		ON CONFLICT (tenant_id, job_type, dedup_key) DO NOTHING
		RETURNING id, tenant_id, job_type, dedup_key, payload, status, priority,
		          available_at, attempt_count, max_attempts, lease_owner, lease_until,
		          last_error_code, completed_at, created_at, updated_at`,
		job.ID, job.TenantID, job.Type, job.DedupKey, string(job.Payload), job.Priority,
		job.AvailableAt, job.MaxAttempts, job.CreatedAt,
	))
	if err == nil {
		return persisted, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Job{}, false, mapError("добавление задания", err)
	}
	var samePayload bool
	persisted, err = scanJob(store.pool.QueryRow(ctx, `
		SELECT id, tenant_id, job_type, dedup_key, payload, status, priority,
		       available_at, attempt_count, max_attempts, lease_owner, lease_until,
		       last_error_code, completed_at, created_at, updated_at,
		       payload = $4::jsonb
		FROM jobs
		WHERE tenant_id = $1 AND job_type = $2 AND dedup_key = $3`,
		job.TenantID, job.Type, job.DedupKey, string(job.Payload),
	), &samePayload)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Job{}, false, domain.ErrConflict
	}
	if err != nil {
		return domain.Job{}, false, mapError("чтение повторного задания", err)
	}
	if !samePayload {
		return domain.Job{}, false, domain.ErrConflict
	}
	return persisted, false, nil
}

// Claim атомарно захватывает доступные задания через FOR UPDATE SKIP LOCKED.
// Истёкшая аренда доступна другому владельцу; исчерпавшая попытки становится DEAD.
func (store *PostgresStore) Claim(
	ctx context.Context,
	owner string,
	now, leaseUntil time.Time,
	limit int,
) ([]domain.Job, error) {
	if store == nil || store.pool == nil || owner == "" || now.IsZero() || !leaseUntil.After(now) || limit < 1 || limit > 100 {
		return nil, domain.ErrInvalid
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("начало захвата заданий: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE jobs
		SET status = 'DEAD', lease_owner = NULL, lease_until = NULL,
		    last_error_code = 'LEASE_EXPIRED_MAX_ATTEMPTS', completed_at = $1, updated_at = $1
		WHERE status = 'PROCESSING' AND lease_until <= $1 AND attempt_count >= max_attempts`, now.UTC()); err != nil {
		return nil, mapError("завершение заданий с исчерпанной арендой", err)
	}
	rows, err := tx.Query(ctx, `
		WITH candidates AS (
			SELECT id
			FROM jobs
			WHERE attempt_count < max_attempts
			  AND (
				(status IN ('PENDING', 'RETRY') AND available_at <= $1)
				OR (status = 'PROCESSING' AND lease_until <= $1)
			  )
			ORDER BY priority DESC, available_at, created_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $4
		)
		UPDATE jobs AS job
		SET status = 'PROCESSING', attempt_count = job.attempt_count + 1,
		    lease_owner = $2, lease_until = $3, updated_at = $1
		FROM candidates
		WHERE job.id = candidates.id
		RETURNING job.id, job.tenant_id, job.job_type, job.dedup_key, job.payload,
		          job.status, job.priority, job.available_at, job.attempt_count,
		          job.max_attempts, job.lease_owner, job.lease_until,
		          job.last_error_code, job.completed_at, job.created_at, job.updated_at`,
		now.UTC(), owner, leaseUntil.UTC(), limit)
	if err != nil {
		return nil, mapError("захват заданий", err)
	}
	jobs := make([]domain.Job, 0, limit)
	for rows.Next() {
		job, scanErr := scanJob(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("обход захваченных заданий: %w", err)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("фиксация захвата заданий: %w", err)
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].Priority != jobs[j].Priority {
			return jobs[i].Priority > jobs[j].Priority
		}
		if !jobs[i].AvailableAt.Equal(jobs[j].AvailableAt) {
			return jobs[i].AvailableAt.Before(jobs[j].AvailableAt)
		}
		return jobs[i].ID < jobs[j].ID
	})
	return jobs, nil
}

func (store *PostgresStore) Succeed(ctx context.Context, jobID, owner string, at time.Time) error {
	if store == nil || store.pool == nil || jobID == "" || owner == "" || at.IsZero() {
		return domain.ErrInvalid
	}
	result, err := store.pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'SUCCEEDED', lease_owner = NULL, lease_until = NULL,
		    completed_at = $3, updated_at = $3
		WHERE id = $1 AND status = 'PROCESSING' AND lease_owner = $2 AND lease_until > $3`,
		jobID, owner, at.UTC())
	if err != nil {
		return mapError("успешное завершение задания", err)
	}
	if result.RowsAffected() != 1 {
		return domain.ErrLeaseLost
	}
	return nil
}

func (store *PostgresStore) Fail(
	ctx context.Context,
	jobID, owner, code string,
	retryable bool,
	next, at time.Time,
) (domain.Status, error) {
	if store == nil || store.pool == nil || jobID == "" || owner == "" || code == "" ||
		next.IsZero() || at.IsZero() || next.Before(at) {
		return "", domain.ErrInvalid
	}
	var status domain.Status
	err := store.pool.QueryRow(ctx, `
		UPDATE jobs
		SET status = CASE WHEN $4 AND attempt_count < max_attempts THEN 'RETRY' ELSE 'DEAD' END,
		    available_at = CASE WHEN $4 AND attempt_count < max_attempts THEN $5 ELSE available_at END,
		    lease_owner = NULL,
		    lease_until = NULL,
		    last_error_code = $3,
		    completed_at = CASE WHEN $4 AND attempt_count < max_attempts THEN NULL ELSE $6 END,
		    updated_at = $6
		WHERE id = $1 AND status = 'PROCESSING' AND lease_owner = $2 AND lease_until > $6
		RETURNING status`, jobID, owner, code, retryable, next.UTC(), at.UTC()).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrLeaseLost
	}
	if err != nil {
		return "", mapError("завершение ошибочного задания", err)
	}
	return status, nil
}

func (store *PostgresStore) Schedule(ctx context.Context, check domain.ScheduledCheck) (domain.ScheduledCheck, bool, error) {
	if store == nil || store.pool == nil || check.Status != domain.CheckScheduled || check.Validate() != nil {
		return domain.ScheduledCheck{}, false, domain.ErrInvalid
	}
	persisted, err := scanCheck(store.pool.QueryRow(ctx, `
		INSERT INTO scheduled_checks(
			id, tenant_id, check_type, subject_type, subject_id, job_type, dedup_key,
			payload, due_at, status, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, 'SCHEDULED', $10, $10)
		ON CONFLICT (tenant_id, check_type, dedup_key) DO NOTHING
		RETURNING id, tenant_id, check_type, subject_type, subject_id, job_type,
		          dedup_key, payload, due_at, status, job_id, created_at, updated_at`,
		check.ID, check.TenantID, check.Type, check.SubjectType, check.SubjectID,
		check.JobType, check.DedupKey, string(check.Payload), check.DueAt, check.CreatedAt,
	))
	if err == nil {
		return persisted, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.ScheduledCheck{}, false, mapError("добавление проверки по расписанию", err)
	}
	var equivalent bool
	persisted, err = scanCheck(store.pool.QueryRow(ctx, `
		SELECT id, tenant_id, check_type, subject_type, subject_id, job_type,
		       dedup_key, payload, due_at, status, job_id, created_at, updated_at,
		       subject_type = $4 AND subject_id = $5 AND job_type = $6
		       AND payload = $7::jsonb AND due_at = $8
		FROM scheduled_checks
		WHERE tenant_id = $1 AND check_type = $2 AND dedup_key = $3`,
		check.TenantID, check.Type, check.DedupKey, check.SubjectType, check.SubjectID,
		check.JobType, string(check.Payload), check.DueAt,
	), &equivalent)
	if err != nil {
		return domain.ScheduledCheck{}, false, mapError("чтение повторной проверки", err)
	}
	if !equivalent {
		return domain.ScheduledCheck{}, false, domain.ErrConflict
	}
	return persisted, false, nil
}

// PromoteDue держит блокировки проверок и создаёт Job в той же транзакции.
func (store *PostgresStore) PromoteDue(ctx context.Context, now time.Time, limit int) (int, error) {
	if store == nil || store.pool == nil || now.IsZero() || limit < 1 || limit > 100 {
		return 0, domain.ErrInvalid
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("начало планирования: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT id, tenant_id, check_type, subject_type, subject_id, job_type,
		       dedup_key, payload, due_at, status, job_id, created_at, updated_at
		FROM scheduled_checks
		WHERE status = 'SCHEDULED' AND due_at <= $1
		ORDER BY due_at, created_at, id
		FOR UPDATE SKIP LOCKED
		LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return 0, mapError("захват проверок по расписанию", err)
	}
	checks := make([]domain.ScheduledCheck, 0, limit)
	for rows.Next() {
		check, scanErr := scanCheck(rows)
		if scanErr != nil {
			rows.Close()
			return 0, scanErr
		}
		checks = append(checks, check)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("обход проверок по расписанию: %w", err)
	}
	rows.Close()
	for _, check := range checks {
		var jobID string
		err := tx.QueryRow(ctx, `
			INSERT INTO jobs(
				id, tenant_id, job_type, dedup_key, payload, status, priority,
				available_at, attempt_count, max_attempts, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5::jsonb, 'PENDING', 0, $6, 0, 5, $7, $7)
			ON CONFLICT (tenant_id, job_type, dedup_key) DO UPDATE
			SET updated_at = jobs.updated_at
			WHERE jobs.payload = EXCLUDED.payload
			RETURNING id`, check.ID, check.TenantID, check.JobType, check.DedupKey,
			string(check.Payload), check.DueAt, now.UTC()).Scan(&jobID)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, domain.ErrConflict
		}
		if err != nil {
			return 0, mapError("создание запланированного задания", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE scheduled_checks
			SET status = 'ENQUEUED', job_id = $2, updated_at = $3
			WHERE id = $1`, check.ID, jobID, now.UTC()); err != nil {
			return 0, mapError("завершение проверки по расписанию", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("фиксация планирования: %w", err)
	}
	return len(checks), nil
}

type rowScanner interface{ Scan(...any) error }

func scanJob(row rowScanner, extra ...any) (domain.Job, error) {
	var job domain.Job
	values := []any{
		&job.ID, &job.TenantID, &job.Type, &job.DedupKey, &job.Payload, &job.Status,
		&job.Priority, &job.AvailableAt, &job.AttemptCount, &job.MaxAttempts,
		&job.LeaseOwner, &job.LeaseUntil, &job.LastErrorCode, &job.CompletedAt,
		&job.CreatedAt, &job.UpdatedAt,
	}
	values = append(values, extra...)
	if err := row.Scan(values...); err != nil {
		return domain.Job{}, err
	}
	if job.Validate() != nil {
		return domain.Job{}, domain.ErrInvalid
	}
	return job, nil
}

func scanCheck(row rowScanner, extra ...any) (domain.ScheduledCheck, error) {
	var check domain.ScheduledCheck
	values := []any{
		&check.ID, &check.TenantID, &check.Type, &check.SubjectType, &check.SubjectID,
		&check.JobType, &check.DedupKey, &check.Payload, &check.DueAt, &check.Status,
		&check.JobID, &check.CreatedAt, &check.UpdatedAt,
	}
	values = append(values, extra...)
	if err := row.Scan(values...); err != nil {
		return domain.ScheduledCheck{}, err
	}
	if check.Validate() != nil {
		return domain.ScheduledCheck{}, domain.ErrInvalid
	}
	return check, nil
}

func mapError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "23503":
			return domain.ErrInvalid
		case "23505":
			return domain.ErrConflict
		case "23514", "22P02":
			return domain.ErrInvalid
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}
