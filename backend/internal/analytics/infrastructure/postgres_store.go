// Package infrastructure читает показатели аналитики напрямую из таблиц
// модулей: аналитика обязана совпадать с необработанными данными (ADR 0039).
package infrastructure

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/analytics/application"
	"lidradar/backend/internal/analytics/domain"
)

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func (store *PostgresStore) Organization(ctx context.Context, tenantID string) (application.Organization, bool, error) {
	if store == nil || store.pool == nil || tenantID == "" {
		return application.Organization{}, false, application.ErrInvalid
	}
	var organization application.Organization
	err := store.pool.QueryRow(ctx, `
		SELECT default_timezone, default_currency FROM organizations WHERE id = $1`, tenantID).Scan(
		&organization.Timezone, &organization.Currency,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.Organization{}, false, nil
	}
	if err != nil {
		return application.Organization{}, false, fmt.Errorf("чтение организации для аналитики: %w", err)
	}
	return organization, true, nil
}

// Summary выполняет все запросы в одной транзакции только для чтения с
// уровнем REPEATABLE READ: показатели одного ответа берутся из одного снимка.
func (store *PostgresStore) Summary(
	ctx context.Context,
	tenantID string,
	period domain.Period,
	currency string,
) (domain.Summary, error) {
	if store == nil || store.pool == nil || tenantID == "" || period.From.IsZero() || !period.From.Before(period.To) || len(currency) != 3 {
		return domain.Summary{}, application.ErrInvalid
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.Summary{}, fmt.Errorf("начало чтения аналитики: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	from, to := period.From, period.To
	var summary domain.Summary
	if err := tx.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE direction = 'INCOMING'),
		       count(*) FILTER (WHERE direction = 'OUTGOING'),
		       (SELECT count(*) FROM conversations
		        WHERE tenant_id = $1 AND first_message_at >= $2 AND first_message_at < $3)
		FROM messages
		WHERE tenant_id = $1 AND sent_at >= $2 AND sent_at < $3`, tenantID, from, to).Scan(
		&summary.Messages.Total, &summary.Messages.Incoming, &summary.Messages.Outgoing, &summary.Messages.Conversations,
	); err != nil {
		return domain.Summary{}, fmt.Errorf("подсчёт сообщений: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM opportunities
		        WHERE tenant_id = $1 AND opened_at >= $2 AND opened_at < $3),
		       count(DISTINCT opportunity_id) FILTER (WHERE to_stage = 'BOOKED'),
		       count(DISTINCT opportunity_id) FILTER (WHERE to_stage = 'WON'),
		       count(DISTINCT opportunity_id) FILTER (WHERE to_stage = 'LOST')
		FROM opportunity_stage_history
		WHERE tenant_id = $1 AND created_at >= $2 AND created_at < $3`, tenantID, from, to).Scan(
		&summary.Opportunities.Created, &summary.Opportunities.Booked, &summary.Opportunities.Won, &summary.Opportunities.Lost,
	); err != nil {
		return domain.Summary{}, fmt.Errorf("подсчёт сделок: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT type,
		       count(*) FILTER (WHERE detected_at >= $2 AND detected_at < $3),
		       count(*) FILTER (WHERE acted_at >= $2 AND acted_at < $3),
		       count(*) FILTER (WHERE status = 'RESOLVED' AND resolved_at >= $2 AND resolved_at < $3),
		       count(*) FILTER (WHERE status = 'FALSE_POSITIVE' AND resolved_at >= $2 AND resolved_at < $3)
		FROM risk_signals
		WHERE tenant_id = $1
		  AND ((detected_at >= $2 AND detected_at < $3)
		    OR (acted_at >= $2 AND acted_at < $3)
		    OR (resolved_at >= $2 AND resolved_at < $3))
		GROUP BY type`, tenantID, from, to)
	if err != nil {
		return domain.Summary{}, fmt.Errorf("подсчёт рисков: %w", err)
	}
	for rows.Next() {
		var row domain.RiskTypeMetrics
		if err := rows.Scan(&row.RiskType, &row.Detected, &row.Acted, &row.Resolved, &row.FalsePositive); err != nil {
			rows.Close()
			return domain.Summary{}, fmt.Errorf("чтение рисков: %w", err)
		}
		summary.Risks.ByType = append(summary.Risks.ByType, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return domain.Summary{}, fmt.Errorf("обход рисков: %w", err)
	}
	rows.Close()
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'BOOKED'),
		       count(*) FILTER (WHERE status = 'PAID'),
		       count(*) FILTER (WHERE status = 'LOST')
		FROM outcomes
		WHERE tenant_id = $1 AND created_at >= $2 AND created_at < $3`, tenantID, from, to).Scan(
		&summary.Outcomes.Booked, &summary.Outcomes.Paid, &summary.Outcomes.Lost,
	); err != nil {
		return domain.Summary{}, fmt.Errorf("подсчёт исходов: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT (SELECT COALESCE(sum(estimated_amount), 0)::numeric(20,2)::text
		        FROM opportunities
		        WHERE tenant_id = $1 AND opened_at >= $2 AND opened_at < $3
		          AND stage NOT IN ('WON', 'LOST', 'ARCHIVED') AND currency = $4),
		       COALESCE(sum(event.amount), 0)::numeric(20,2)::text,
		       COALESCE(sum(event.amount) FILTER (WHERE attribution.type = 'RECOVERED'), 0)::numeric(20,2)::text,
		       count(event.id)
		FROM revenue_events AS event
		LEFT JOIN revenue_attributions AS attribution
		  ON attribution.tenant_id = event.tenant_id AND attribution.revenue_event_id = event.id
		WHERE event.tenant_id = $1 AND event.status = 'CONFIRMED' AND event.currency = $4
		  AND event.confirmed_at >= $2 AND event.confirmed_at < $3`, tenantID, from, to, currency).Scan(
		&summary.Revenue.Potential, &summary.Revenue.Confirmed, &summary.Revenue.ConfirmedRecovered, &summary.Revenue.ConfirmedPayments,
	); err != nil {
		return domain.Summary{}, fmt.Errorf("подсчёт денег: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Summary{}, fmt.Errorf("завершение чтения аналитики: %w", err)
	}
	summary.Revenue.Currency = currency
	return summary, nil
}

var _ application.Store = (*PostgresStore)(nil)
