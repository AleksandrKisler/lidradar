// Package infrastructure читает показатели аналитики напрямую из таблиц
// модулей: аналитика обязана совпадать с необработанными данными (ADR 0039).
package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	// Дневной ряд: дни считаются в часовом поясе организации, деньги — в её валюте.
	days, err := tx.Query(ctx, `
		WITH message_days AS (
			SELECT (sent_at AT TIME ZONE $4)::date AS day,
			       count(*) FILTER (WHERE direction = 'INCOMING') AS incoming,
			       count(*) FILTER (WHERE direction = 'OUTGOING') AS outgoing
			FROM messages
			WHERE tenant_id = $1 AND sent_at >= $2 AND sent_at < $3
			GROUP BY 1
		), risk_days AS (
			SELECT (detected_at AT TIME ZONE $4)::date AS day, count(*) AS detected
			FROM risk_signals
			WHERE tenant_id = $1 AND detected_at >= $2 AND detected_at < $3
			GROUP BY 1
		), money_days AS (
			SELECT (event.confirmed_at AT TIME ZONE $4)::date AS day,
			       COALESCE(sum(event.amount), 0)::numeric(20,2)::text AS confirmed,
			       COALESCE(sum(event.amount) FILTER (WHERE attribution.type = 'RECOVERED'), 0)::numeric(20,2)::text AS recovered,
			       count(event.id) AS payments
			FROM revenue_events AS event
			LEFT JOIN revenue_attributions AS attribution
			  ON attribution.tenant_id = event.tenant_id AND attribution.revenue_event_id = event.id
			WHERE event.tenant_id = $1 AND event.status = 'CONFIRMED' AND event.currency = $5
			  AND event.confirmed_at >= $2 AND event.confirmed_at < $3
			GROUP BY 1
		)
		SELECT to_char(day, 'YYYY-MM-DD'),
		       COALESCE(message_days.incoming, 0), COALESCE(message_days.outgoing, 0),
		       COALESCE(risk_days.detected, 0),
		       COALESCE(money_days.confirmed, '0.00'), COALESCE(money_days.recovered, '0.00'), COALESCE(money_days.payments, 0)
		FROM (
			SELECT day FROM message_days
			UNION SELECT day FROM risk_days
			UNION SELECT day FROM money_days
		) AS all_days
		LEFT JOIN message_days USING (day)
		LEFT JOIN risk_days USING (day)
		LEFT JOIN money_days USING (day)
		ORDER BY day`, tenantID, from, to, period.Timezone, currency)
	if err != nil {
		return domain.Summary{}, fmt.Errorf("подсчёт дневного ряда: %w", err)
	}
	for days.Next() {
		var point domain.DailyPoint
		if err := days.Scan(&point.Date, &point.Incoming, &point.Outgoing, &point.RisksDetected,
			&point.Confirmed, &point.ConfirmedRecovered, &point.Payments); err != nil {
			days.Close()
			return domain.Summary{}, fmt.Errorf("чтение дневного ряда: %w", err)
		}
		summary.Series = append(summary.Series, point)
	}
	if err := days.Err(); err != nil {
		days.Close()
		return domain.Summary{}, fmt.Errorf("обход дневного ряда: %w", err)
	}
	days.Close()
	splits, err := tx.Query(ctx, `
		SELECT COALESCE(attribution.type, 'UNKNOWN'),
		       COALESCE(sum(event.amount), 0)::numeric(20,2)::text, count(event.id)
		FROM revenue_events AS event
		LEFT JOIN revenue_attributions AS attribution
		  ON attribution.tenant_id = event.tenant_id AND attribution.revenue_event_id = event.id
		WHERE event.tenant_id = $1 AND event.status = 'CONFIRMED' AND event.currency = $4
		  AND event.confirmed_at >= $2 AND event.confirmed_at < $3
		GROUP BY 1`, tenantID, from, to, currency)
	if err != nil {
		return domain.Summary{}, fmt.Errorf("подсчёт атрибуций: %w", err)
	}
	for splits.Next() {
		var split domain.AttributionSplit
		if err := splits.Scan(&split.Type, &split.Amount, &split.Count); err != nil {
			splits.Close()
			return domain.Summary{}, fmt.Errorf("чтение атрибуций: %w", err)
		}
		summary.Attribution = append(summary.Attribution, split)
	}
	if err := splits.Err(); err != nil {
		splits.Close()
		return domain.Summary{}, fmt.Errorf("обход атрибуций: %w", err)
	}
	splits.Close()
	if err := tx.Commit(ctx); err != nil {
		return domain.Summary{}, fmt.Errorf("завершение чтения аналитики: %w", err)
	}
	summary.Revenue.Currency = currency
	return summary, nil
}

// Payments читает подтверждённые события окна с контекстом сделки, переписки,
// контакта и услуги; порядок — от новых к старым по (confirmed_at, id).
func (store *PostgresStore) Payments(
	ctx context.Context,
	tenantID string,
	period domain.Period,
	limit int,
	cursor *domain.PaymentCursor,
) ([]domain.Payment, bool, error) {
	if store == nil || store.pool == nil || tenantID == "" || period.From.IsZero() || !period.From.Before(period.To) || limit < 1 || limit > 100 {
		return nil, false, application.ErrInvalid
	}
	var cursorAt *time.Time
	var cursorID *string
	if cursor != nil {
		at := cursor.At.UTC()
		cursorAt, cursorID = &at, &cursor.ID
	}
	rows, err := store.pool.Query(ctx, `
		SELECT event.id, event.opportunity_id, o.conversation_id, c.contact_id, ct.display_name, s.name,
		       event.amount::numeric(20,2)::text, event.currency, COALESCE(attribution.type, 'UNKNOWN'), attribution.risk_id,
		       event.confirmed_by_user_id, event.confirmed_at
		FROM revenue_events AS event
		LEFT JOIN revenue_attributions AS attribution
		  ON attribution.tenant_id = event.tenant_id AND attribution.revenue_event_id = event.id
		JOIN opportunities AS o ON o.tenant_id = event.tenant_id AND o.id = event.opportunity_id
		JOIN conversations AS c ON c.tenant_id = o.tenant_id AND c.id = o.conversation_id
		JOIN contacts AS ct ON ct.tenant_id = c.tenant_id AND ct.id = c.contact_id
		LEFT JOIN service_catalog_items AS s ON s.tenant_id = o.tenant_id AND s.id = o.service_id
		WHERE event.tenant_id = $1 AND event.status = 'CONFIRMED'
		  AND event.confirmed_at >= $2 AND event.confirmed_at < $3
		  AND ($4::timestamptz IS NULL OR (event.confirmed_at, event.id) < ($4, $5::uuid))
		ORDER BY event.confirmed_at DESC, event.id DESC
		LIMIT $6`, tenantID, period.From, period.To, cursorAt, cursorID, limit+1)
	if err != nil {
		return nil, false, mapAnalyticsError("чтение оплат", err)
	}
	defer rows.Close()
	items := make([]domain.Payment, 0, limit+1)
	for rows.Next() {
		var payment domain.Payment
		if err := rows.Scan(&payment.EventID, &payment.OpportunityID, &payment.ConversationID, &payment.ContactID,
			&payment.ContactDisplayName, &payment.ServiceName, &payment.Amount, &payment.Currency, &payment.Attribution,
			&payment.RiskID, &payment.ConfirmedBy, &payment.ConfirmedAt); err != nil {
			return nil, false, fmt.Errorf("чтение оплаты: %w", err)
		}
		payment.ConfirmedAt = payment.ConfirmedAt.UTC()
		items = append(items, payment)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("обход оплат: %w", err)
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	return items, more, nil
}

func mapAnalyticsError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && (postgresError.Code == "22P02" || postgresError.Code == "22007") {
		return application.ErrInvalid
	}
	return fmt.Errorf("%s: %w", operation, err)
}

var _ application.Store = (*PostgresStore)(nil)
