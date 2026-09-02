package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/risk/application"
	"lidradar/backend/internal/risk/domain"
)

// PostgresStateReader перечитывает принадлежащие Conversation, Opportunity и
// Location таблицы как один read-only срез. Он не изменяет чужие агрегаты.
type PostgresStateReader struct{ pool *pgxpool.Pool }

// activeOpportunityStages повторяет определение активной сделки из ТЗ §26 и
// частичного уникального индекса `opportunities_one_active_per_conversation_idx`.
// Проекция обязана совпадать с индексом: пока индекс держит сделку активной и не
// даёт создать вторую, правила должны продолжать её наблюдать. Исключения
// конкретного правила (например `BOOKED` для R3) принадлежат самому правилу.
const activeOpportunityStages = `stage NOT IN ('WON', 'LOST', 'ARCHIVED')`

func NewPostgresStateReader(pool *pgxpool.Pool) *PostgresStateReader {
	return &PostgresStateReader{pool: pool}
}

func (reader *PostgresStateReader) ActiveOpportunityByConversation(
	ctx context.Context,
	tenantID, conversationID string,
) (string, bool, error) {
	if reader == nil || reader.pool == nil || tenantID == "" || conversationID == "" {
		return "", false, application.ErrStateIncomplete
	}
	var opportunityID string
	err := reader.pool.QueryRow(ctx, `
		SELECT id FROM opportunities
		WHERE tenant_id = $1 AND conversation_id = $2
		  AND `+activeOpportunityStages, tenantID, conversationID).Scan(&opportunityID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("поиск активной возможности для риска: %w", err)
	}
	return opportunityID, true, nil
}

func (reader *PostgresStateReader) CurrentState(
	ctx context.Context,
	tenantID, opportunityID string,
) (domain.ConversationState, error) {
	if reader == nil || reader.pool == nil || tenantID == "" || opportunityID == "" {
		return domain.ConversationState{}, application.ErrStateIncomplete
	}
	var state domain.ConversationState
	var locationID, messageID, direction *string
	var sentAt *time.Time
	var timezone *string
	var threshold *int
	var bookingConfidence *float64
	var bookingRunID, bookingMessageID *string
	var bookingAt *time.Time
	err := reader.pool.QueryRow(ctx, `
		SELECT o.tenant_id, o.id, o.stage,
		       o.`+activeOpportunityStages+`,
		       c.location_id, l.timezone, l.response_threshold_minutes,
		       message.id, message.sent_at, message.direction,
		       booking.confidence, booking.ai_run_id,
		       booking.evidence_message_id, booking.evidence_at
		FROM opportunities AS o
		JOIN conversations AS c
		  ON c.tenant_id = o.tenant_id AND c.id = o.conversation_id
		LEFT JOIN locations AS l
		  ON l.tenant_id = c.tenant_id AND l.id = c.location_id AND l.active = TRUE
		LEFT JOIN LATERAL (
			SELECT m.id, m.sent_at, m.direction
			FROM messages AS m
			WHERE m.tenant_id = c.tenant_id AND m.conversation_id = c.id
			  AND m.provider_deleted_at IS NULL
			  AND m.direction IN ('INCOMING', 'OUTGOING')
			ORDER BY m.sent_at DESC, m.id DESC
			LIMIT 1
		) AS message ON TRUE
		LEFT JOIN LATERAL (
			SELECT m.id
			FROM messages AS m
			WHERE m.tenant_id = c.tenant_id AND m.conversation_id = c.id
			  AND m.provider_deleted_at IS NULL
			  AND m.direction IN ('INCOMING', 'OUTGOING')
			  AND m.text IS NOT NULL AND btrim(m.text) <> ''
			ORDER BY m.sent_at DESC, m.id DESC
			LIMIT 1
		) AS analysis_message ON TRUE
		LEFT JOIN conversation_summaries AS summary
		  ON summary.tenant_id = c.tenant_id AND summary.conversation_id = c.id
		 AND summary.base_conversation_revision = c.revision
		 AND summary.analysis_through_message_id = analysis_message.id
		LEFT JOIN LATERAL (
			SELECT (fact.value ->> 'confidence')::double precision AS confidence,
			       summary.ai_run_id::text AS ai_run_id,
			       evidence_message.id::text AS evidence_message_id,
			       evidence_message.sent_at AS evidence_at
			FROM jsonb_array_elements(summary.semantic_facts) AS fact(value)
			CROSS JOIN LATERAL jsonb_array_elements_text(fact.value -> 'evidenceMessageIds') AS evidence(id)
			JOIN messages AS evidence_message
			  ON evidence_message.tenant_id = c.tenant_id
			 AND evidence_message.conversation_id = c.id
			 AND evidence_message.id::text = evidence.id
			 AND evidence_message.direction = 'INCOMING'
			 AND evidence_message.provider_deleted_at IS NULL
			WHERE fact.value ->> 'type' = 'BOOKING_INTENT'
			  AND fact.value ->> 'value' = 'true'
			  AND (fact.value ->> 'trusted')::boolean
			ORDER BY evidence_message.sent_at DESC, evidence_message.id DESC
			LIMIT 1
		) AS booking ON TRUE
		WHERE o.tenant_id = $1 AND o.id = $2`, tenantID, opportunityID).Scan(
		&state.TenantID, &state.OpportunityID, &state.OpportunityStage,
		&state.ActiveOpportunity, &locationID, &timezone, &threshold,
		&messageID, &sentAt, &direction, &bookingConfidence, &bookingRunID,
		&bookingMessageID, &bookingAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ConversationState{}, application.ErrStateNotFound
	}
	if err != nil {
		return domain.ConversationState{}, fmt.Errorf("чтение актуального состояния риска: %w", err)
	}
	if locationID == nil || timezone == nil || threshold == nil || messageID == nil || sentAt == nil || direction == nil {
		return domain.ConversationState{}, application.ErrStateIncomplete
	}
	state.LocationID = *locationID
	state.LastMeaningfulID = *messageID
	state.LastMeaningfulAt = sentAt.UTC()
	state.LastMeaningful = domain.Direction(*direction)
	if bookingConfidence != nil && bookingRunID != nil && bookingMessageID != nil && bookingAt != nil {
		state.BookingIntent = &domain.BookingIntentSignal{
			Value: true, Confidence: *bookingConfidence, AIRunID: *bookingRunID,
			EvidenceMessageID: *bookingMessageID, EvidenceAt: bookingAt.UTC(),
		}
	}
	state.ResponseThreshold = time.Duration(*threshold) * time.Minute
	state.BusinessHours = domain.BusinessHours{Timezone: *timezone, Weekly: make(map[time.Weekday][]domain.BusinessPeriod)}

	rows, err := reader.pool.Query(ctx, `
		SELECT weekday, is_closed,
		       COALESCE(to_char(opens_at, 'HH24:MI'), ''),
		       COALESCE(to_char(closes_at, 'HH24:MI'), '')
		FROM location_business_hours
		WHERE tenant_id = $1 AND location_id = $2
		ORDER BY weekday`, tenantID, state.LocationID)
	if err != nil {
		return domain.ConversationState{}, fmt.Errorf("чтение рабочих часов риска: %w", err)
	}
	defer rows.Close()
	rowCount := 0
	for rows.Next() {
		var weekday int
		var closed bool
		var opensAt, closesAt string
		if err := rows.Scan(&weekday, &closed, &opensAt, &closesAt); err != nil {
			return domain.ConversationState{}, fmt.Errorf("разбор рабочих часов риска: %w", err)
		}
		rowCount++
		if closed {
			continue
		}
		open, err := riskClock(opensAt)
		if err != nil {
			return domain.ConversationState{}, application.ErrStateIncomplete
		}
		closeAt, err := riskClock(closesAt)
		if err != nil {
			return domain.ConversationState{}, application.ErrStateIncomplete
		}
		day := time.Weekday(weekday % 7)
		state.BusinessHours.Weekly[day] = append(state.BusinessHours.Weekly[day], domain.BusinessPeriod{Open: open, Close: closeAt})
	}
	if err := rows.Err(); err != nil {
		return domain.ConversationState{}, fmt.Errorf("обход рабочих часов риска: %w", err)
	}
	if rowCount != 7 {
		return domain.ConversationState{}, application.ErrStateIncomplete
	}
	return state, nil
}

func riskClock(value string) (domain.TimeOfDay, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, application.ErrStateIncomplete
	}
	hour, hourErr := strconv.Atoi(parts[0])
	minute, minuteErr := strconv.Atoi(parts[1])
	if hourErr != nil || minuteErr != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, application.ErrStateIncomplete
	}
	return domain.Clock(hour, minute), nil
}

var _ application.StateReader = (*PostgresStateReader)(nil)
var _ application.OpportunityLocator = (*PostgresStateReader)(nil)
