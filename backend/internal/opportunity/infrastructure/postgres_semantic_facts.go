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
	return source.trustedIncomingFact(ctx, tenantID, conversationID, runID, "BOOKING_INTENT", "намерения записаться")
}

// TrustedFollowUpCandidate читает колебание клиента с доказательством во
// входящем сообщении.
func (source *PostgresSemanticFacts) TrustedFollowUpCandidate(
	ctx context.Context,
	tenantID, conversationID, runID string,
) (application.BookingIntentFact, bool, error) {
	return source.trustedIncomingFact(ctx, tenantID, conversationID, runID, "FOLLOW_UP_CANDIDATE", "колебания клиента")
}

func (source *PostgresSemanticFacts) trustedIncomingFact(
	ctx context.Context,
	tenantID, conversationID, runID, factType, description string,
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
		  AND item.value ->> 'type' = $4
		  AND item.value ->> 'value' = 'true'
		  AND (item.value ->> 'trusted')::boolean
		  AND EXISTS (
			SELECT 1
			FROM jsonb_array_elements_text(item.value -> 'evidenceMessageIds') AS evidence(id)
			JOIN messages AS message
			  ON message.tenant_id = summary.tenant_id AND message.conversation_id = summary.conversation_id
			 AND message.id::text = evidence.id AND message.direction = 'INCOMING'
			 AND message.provider_deleted_at IS NULL
		  )
		GROUP BY summary.ai_run_id`, tenantID, conversationID, runID, factType,
	).Scan(&fact.RunID, &fact.Confidence)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.BookingIntentFact{}, false, nil
	}
	if err != nil {
		return application.BookingIntentFact{}, false, fmt.Errorf("чтение факта %s: %w", description, err)
	}
	return fact, true, nil
}

// TrustedPriceMentioned читает названную компанией цену: доверенный факт
// PRICE_MENTIONED с суммой и валютой, доказательство — исходящее сообщение.
func (source *PostgresSemanticFacts) TrustedPriceMentioned(
	ctx context.Context,
	tenantID, conversationID, runID string,
) (application.PriceFact, bool, error) {
	if source == nil || source.pool == nil || tenantID == "" || conversationID == "" || runID == "" {
		return application.PriceFact{}, false, application.ErrInvalid
	}
	var fact application.PriceFact
	err := source.pool.QueryRow(ctx, `
		SELECT summary.ai_run_id::text,
		       to_char((item.value ->> 'confidence')::numeric, 'FM0.000'),
		       item.value ->> 'amount', item.value ->> 'currency'
		FROM conversation_summaries AS summary
		CROSS JOIN LATERAL jsonb_array_elements(summary.semantic_facts) AS item(value)
		WHERE summary.tenant_id = $1 AND summary.conversation_id = $2 AND summary.ai_run_id = $3
		  AND item.value ->> 'type' = 'PRICE_MENTIONED'
		  AND item.value ->> 'value' = 'true'
		  AND (item.value ->> 'trusted')::boolean
		  AND item.value ? 'amount' AND item.value ? 'currency'
		  AND EXISTS (
			SELECT 1
			FROM jsonb_array_elements_text(item.value -> 'evidenceMessageIds') AS evidence(id)
			JOIN messages AS message
			  ON message.tenant_id = summary.tenant_id AND message.conversation_id = summary.conversation_id
			 AND message.id::text = evidence.id AND message.direction = 'OUTGOING'
			 AND message.provider_deleted_at IS NULL
		  )
		ORDER BY (item.value ->> 'confidence')::numeric DESC
		LIMIT 1`, tenantID, conversationID, runID,
	).Scan(&fact.RunID, &fact.Confidence, &fact.Amount, &fact.Currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.PriceFact{}, false, nil
	}
	if err != nil {
		return application.PriceFact{}, false, fmt.Errorf("чтение факта названной цены: %w", err)
	}
	return fact, true, nil
}

var _ application.SemanticFactSource = (*PostgresSemanticFacts)(nil)
