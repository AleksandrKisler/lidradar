package infrastructure

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/identity/application"
)

type PostgresRateLimiter struct{ pool *pgxpool.Pool }

func NewPostgresRateLimiter(pool *pgxpool.Pool) *PostgresRateLimiter {
	return &PostgresRateLimiter{pool: pool}
}

func (limiter *PostgresRateLimiter) Take(
	ctx context.Context,
	scope, subject string,
	limit int,
	window time.Duration,
	now time.Time,
) (application.RateLimitDecision, error) {
	key, err := hex.DecodeString(subject)
	if limiter == nil || limiter.pool == nil || scope == "" || err != nil || len(key) != 32 || limit < 1 || window <= 0 || now.IsZero() {
		return application.RateLimitDecision{}, errors.New("неверные параметры ограничения частоты")
	}
	tx, err := limiter.pool.Begin(ctx)
	if err != nil {
		return application.RateLimitDecision{}, fmt.Errorf("начало расходования попытки входа: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Небольшая ограниченная очистка не даёт таблице бесконечно расти из-за
	// случайных адресов и имён учётных записей.
	if _, err := tx.Exec(ctx, `
		WITH expired AS (
			SELECT scope, subject_hash
			FROM auth_rate_limits
			WHERE expires_at < $1::timestamptz - INTERVAL '1 day'
			ORDER BY expires_at
			FOR UPDATE SKIP LOCKED
			LIMIT 100
		)
		DELETE FROM auth_rate_limits AS bucket
		USING expired
		WHERE bucket.scope = expired.scope AND bucket.subject_hash = expired.subject_hash`, now.UTC()); err != nil {
		return application.RateLimitDecision{}, fmt.Errorf("очистка счётчиков входа: %w", err)
	}

	var attempts int
	var expiresAt time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO auth_rate_limits(scope, subject_hash, attempts, window_started_at, expires_at, updated_at)
		VALUES ($1, $2, 1, $3, $4, $3)
		ON CONFLICT (scope, subject_hash) DO UPDATE
		SET attempts = CASE
				WHEN auth_rate_limits.expires_at <= EXCLUDED.window_started_at THEN 1
				ELSE auth_rate_limits.attempts + 1
			END,
			window_started_at = CASE
				WHEN auth_rate_limits.expires_at <= EXCLUDED.window_started_at THEN EXCLUDED.window_started_at
				ELSE auth_rate_limits.window_started_at
			END,
			expires_at = CASE
				WHEN auth_rate_limits.expires_at <= EXCLUDED.window_started_at THEN EXCLUDED.expires_at
				ELSE auth_rate_limits.expires_at
			END,
			updated_at = EXCLUDED.updated_at
		RETURNING attempts, expires_at`, scope, key, now.UTC(), now.UTC().Add(window)).Scan(&attempts, &expiresAt); err != nil {
		return application.RateLimitDecision{}, fmt.Errorf("фиксация попытки входа: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return application.RateLimitDecision{}, fmt.Errorf("завершение расходования попытки входа: %w", err)
	}
	return application.RateLimitDecision{Allowed: attempts <= limit, ExpiresAt: expiresAt.UTC()}, nil
}

func (limiter *PostgresRateLimiter) Reset(ctx context.Context, scope, subject string) error {
	key, err := hex.DecodeString(subject)
	if limiter == nil || limiter.pool == nil || scope == "" || err != nil || len(key) != 32 {
		return errors.New("неверные параметры сброса ограничения частоты")
	}
	if _, err := limiter.pool.Exec(ctx, `
		DELETE FROM auth_rate_limits WHERE scope = $1 AND subject_hash = $2`, scope, key); err != nil {
		return fmt.Errorf("сброс счётчика входа: %w", err)
	}
	return nil
}
