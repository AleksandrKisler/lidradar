package infrastructure

import (
	"context"
	"fmt"
	"time"
)

// QueueStats — неавторитетная эксплуатационная проекция. Бизнес-состояние и
// решения очереди по-прежнему опираются непосредственно на строки PostgreSQL.
type QueueStats struct {
	Pending          int64
	Processing       int64
	Retry            int64
	Dead             int64
	ExpiredLeases    int64
	OverdueScheduled int64
}

func (store *PostgresStore) QueueStats(ctx context.Context, at time.Time) (QueueStats, error) {
	if store == nil || store.pool == nil || at.IsZero() {
		return QueueStats{}, fmt.Errorf("queue diagnostics are not configured")
	}
	var result QueueStats
	err := store.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'PENDING'),
			count(*) FILTER (WHERE status = 'PROCESSING'),
			count(*) FILTER (WHERE status = 'RETRY'),
			count(*) FILTER (WHERE status = 'DEAD'),
			count(*) FILTER (WHERE status = 'PROCESSING' AND lease_until <= $1),
			(SELECT count(*) FROM scheduled_checks WHERE status = 'SCHEDULED' AND due_at <= $1)
		FROM jobs`, at.UTC()).Scan(
		&result.Pending, &result.Processing, &result.Retry, &result.Dead,
		&result.ExpiredLeases, &result.OverdueScheduled,
	)
	if err != nil {
		return QueueStats{}, fmt.Errorf("read queue diagnostics: %w", err)
	}
	return result, nil
}
