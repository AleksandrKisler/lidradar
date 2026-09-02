package infrastructure

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/opportunity/application"
)

// PostgresSemanticFacts читает проверенный факт намерения записаться из
// производной проекции AI. Чтение ограничено конкретным AI-run: если проекцию
// уже заменил более свежий анализ, событие старого run ничего не делает — его
// сменщик принесёт собственное событие.
type PostgresSemanticFacts struct{ pool *pgxpool.Pool }

func NewPostgresSemanticFacts(pool *pgxpool.Pool) *PostgresSemanticFacts {
	return &PostgresSemanticFacts{pool: pool}
}

func (source *PostgresSemanticFacts) TrustedBookingIntent(
	ctx context.Context,
	tenantID, conversationID, runID string,
) (application.BookingIntentFact, bool, error) {
	if source == nil || source.pool == nil || tenantID == "" || conversationID == "" || runID == "" {
		return application.BookingIntentFact{}, false, application.ErrInvalid
	}
	var fact application.BookingIntentFact
	err := source.pool.QueryRow(ctx, `
		SELECT summary.ai_run_id::text,
		       to_char(max((item.value ->> 'confidence')::numeric), 'FM0.000')
		FROM conversation_summaries AS summary
		CROSS JOIN LATERAL jsonb_array_elements(summary.semantic_facts) AS item(value)
		WHERE summary.tenant_id = $1 AND summary.conversation_id = $2 AND summary.ai_run_id = $3
		  AND item.value ->> 'type' = 'BOOKING_INTENT'
		  AND item.value ->> 'value' = 'true'
		  AND (item.value ->> 'trusted')::boolean
		GROUP BY summary.ai_run_id`, tenantID, conversationID, runID,
	).Scan(&fact.RunID, &fact.Confidence)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.BookingIntentFact{}, false, nil
	}
	if err != nil {
		return application.BookingIntentFact{}, false, fmt.Errorf("чтение факта намерения записаться: %w", err)
	}
	return fact, true, nil
}

var _ application.SemanticFactSource = (*PostgresSemanticFacts)(nil)
