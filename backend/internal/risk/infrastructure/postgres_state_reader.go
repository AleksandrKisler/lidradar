package infrastructure

import (
	"context"
	"encoding/json"
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
	var commitmentConfidence *float64
	var commitmentRunID, commitmentMessageID, commitmentText *string
	var commitmentAt *time.Time
	var commitmentFollowedUp *bool
	var priceConfidence *float64
	var priceRunID, priceMessageID, priceAmount, priceCurrency *string
	var priceAt *time.Time
	var priceIncomingAfter *bool
	var followUpConfidence *float64
	var followUpRunID, followUpMessageID *string
	var followUpAt *time.Time
	var followUpIncomingAfter *bool
	var lastOutgoingID *string
	var lastOutgoingAt *time.Time
	var lastIncomingID *string
	var lastIncomingAt *time.Time
	var latestOutcome *string
	var activeRisks []byte
	err := reader.pool.QueryRow(ctx, `
		SELECT o.tenant_id, o.id, o.stage,
		       o.`+activeOpportunityStages+`,
		       c.location_id, l.timezone, l.response_threshold_minutes,
		       message.id, message.sent_at, message.direction,
		       booking.confidence, booking.ai_run_id,
		       booking.evidence_message_id, booking.evidence_at,
		       commitment.confidence, commitment.ai_run_id, commitment.evidence_message_id,
		       commitment.evidence_at, commitment.evidence_text, commitment.followed_up,
		       price.confidence, price.ai_run_id, price.evidence_message_id, price.evidence_at,
		       price.amount, price.currency, price.incoming_after,
		       follow_up.confidence, follow_up.ai_run_id, follow_up.evidence_message_id,
		       follow_up.evidence_at, follow_up.incoming_after,
		       last_outgoing.id, last_outgoing.sent_at, last_incoming.id, last_incoming.sent_at,
		       latest_outcome.status,
		       active_risks.items
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
		-- Обещание компании — исторический факт: оно остаётся в силе после новых
		-- сообщений, поэтому читается из последней проекции без условия ревизии.
		LEFT JOIN conversation_summaries AS commitment_summary
		  ON commitment_summary.tenant_id = c.tenant_id AND commitment_summary.conversation_id = c.id
		LEFT JOIN LATERAL (
			SELECT (fact.value ->> 'confidence')::double precision AS confidence,
			       commitment_summary.ai_run_id::text AS ai_run_id,
			       evidence_message.id::text AS evidence_message_id,
			       evidence_message.sent_at AS evidence_at,
			       COALESCE(evidence_message.text, '') AS evidence_text,
			       EXISTS (
				SELECT 1 FROM messages AS later
				WHERE later.tenant_id = c.tenant_id AND later.conversation_id = c.id
				  AND later.direction = 'OUTGOING' AND later.provider_deleted_at IS NULL
				  AND (later.sent_at, later.id) > (evidence_message.sent_at, evidence_message.id)
			       ) AS followed_up
			FROM jsonb_array_elements(commitment_summary.semantic_facts) AS fact(value)
			CROSS JOIN LATERAL jsonb_array_elements_text(fact.value -> 'evidenceMessageIds') AS evidence(id)
			JOIN messages AS evidence_message
			  ON evidence_message.tenant_id = c.tenant_id
			 AND evidence_message.conversation_id = c.id
			 AND evidence_message.id::text = evidence.id
			 AND evidence_message.direction = 'OUTGOING'
			 AND evidence_message.provider_deleted_at IS NULL
			WHERE fact.value ->> 'type' = 'BUSINESS_COMMITMENT'
			  AND fact.value ->> 'value' = 'true'
			  AND (fact.value ->> 'trusted')::boolean
			ORDER BY evidence_message.sent_at DESC, evidence_message.id DESC
			LIMIT 1
		) AS commitment ON TRUE
		-- Названная цена — тоже исторический факт исходящего сообщения.
		LEFT JOIN LATERAL (
			SELECT (fact.value ->> 'confidence')::double precision AS confidence,
			       commitment_summary.ai_run_id::text AS ai_run_id,
			       evidence_message.id::text AS evidence_message_id,
			       evidence_message.sent_at AS evidence_at,
			       COALESCE(fact.value ->> 'amount', '') AS amount,
			       COALESCE(fact.value ->> 'currency', '') AS currency,
			       EXISTS (
				SELECT 1 FROM messages AS later
				WHERE later.tenant_id = c.tenant_id AND later.conversation_id = c.id
				  AND later.direction = 'INCOMING' AND later.provider_deleted_at IS NULL
				  AND (later.sent_at, later.id) > (evidence_message.sent_at, evidence_message.id)
			       ) AS incoming_after
			FROM jsonb_array_elements(commitment_summary.semantic_facts) AS fact(value)
			CROSS JOIN LATERAL jsonb_array_elements_text(fact.value -> 'evidenceMessageIds') AS evidence(id)
			JOIN messages AS evidence_message
			  ON evidence_message.tenant_id = c.tenant_id
			 AND evidence_message.conversation_id = c.id
			 AND evidence_message.id::text = evidence.id
			 AND evidence_message.direction = 'OUTGOING'
			 AND evidence_message.provider_deleted_at IS NULL
			WHERE fact.value ->> 'type' = 'PRICE_MENTIONED'
			  AND fact.value ->> 'value' = 'true'
			  AND (fact.value ->> 'trusted')::boolean
			ORDER BY evidence_message.sent_at DESC, evidence_message.id DESC
			LIMIT 1
		) AS price ON TRUE
		-- Колебание клиента — входящее сообщение из последней проекции.
		LEFT JOIN LATERAL (
			SELECT (fact.value ->> 'confidence')::double precision AS confidence,
			       commitment_summary.ai_run_id::text AS ai_run_id,
			       evidence_message.id::text AS evidence_message_id,
			       evidence_message.sent_at AS evidence_at,
			       EXISTS (
				SELECT 1 FROM messages AS later
				WHERE later.tenant_id = c.tenant_id AND later.conversation_id = c.id
				  AND later.direction = 'INCOMING' AND later.provider_deleted_at IS NULL
				  AND (later.sent_at, later.id) > (evidence_message.sent_at, evidence_message.id)
			       ) AS incoming_after
			FROM jsonb_array_elements(commitment_summary.semantic_facts) AS fact(value)
			CROSS JOIN LATERAL jsonb_array_elements_text(fact.value -> 'evidenceMessageIds') AS evidence(id)
			JOIN messages AS evidence_message
			  ON evidence_message.tenant_id = c.tenant_id
			 AND evidence_message.conversation_id = c.id
			 AND evidence_message.id::text = evidence.id
			 AND evidence_message.direction = 'INCOMING'
			 AND evidence_message.provider_deleted_at IS NULL
			WHERE fact.value ->> 'type' = 'FOLLOW_UP_CANDIDATE'
			  AND fact.value ->> 'value' = 'true'
			  AND (fact.value ->> 'trusted')::boolean
			ORDER BY evidence_message.sent_at DESC, evidence_message.id DESC
			LIMIT 1
		) AS follow_up ON TRUE
		LEFT JOIN LATERAL (
			SELECT m.id::text AS id, m.sent_at
			FROM messages AS m
			WHERE m.tenant_id = c.tenant_id AND m.conversation_id = c.id
			  AND m.provider_deleted_at IS NULL AND m.direction = 'OUTGOING'
			ORDER BY m.sent_at DESC, m.id DESC
			LIMIT 1
		) AS last_outgoing ON TRUE
		LEFT JOIN LATERAL (
			SELECT m.id::text AS id, m.sent_at
			FROM messages AS m
			WHERE m.tenant_id = c.tenant_id AND m.conversation_id = c.id
			  AND m.provider_deleted_at IS NULL AND m.direction = 'INCOMING'
			ORDER BY m.sent_at DESC, m.id DESC
			LIMIT 1
		) AS last_incoming ON TRUE
		LEFT JOIN LATERAL (
			SELECT outcome.status
			FROM outcomes AS outcome
			WHERE outcome.tenant_id = o.tenant_id AND outcome.opportunity_id = o.id
			ORDER BY outcome.created_at DESC, outcome.id DESC
			LIMIT 1
		) AS latest_outcome ON TRUE
		LEFT JOIN LATERAL (
			SELECT jsonb_agg(jsonb_build_object(
				'type', risk.type,
				'triggerMessageId', risk.trigger_message_id::text,
				'triggerAt', trigger_message.sent_at,
				'outgoingAfterTrigger', EXISTS (
					SELECT 1 FROM messages AS later
					WHERE later.tenant_id = c.tenant_id AND later.conversation_id = c.id
					  AND later.direction = 'OUTGOING' AND later.provider_deleted_at IS NULL
					  AND (later.sent_at, later.id) > (trigger_message.sent_at, trigger_message.id)
				),
				'incomingAfterTrigger', EXISTS (
					SELECT 1 FROM messages AS later
					WHERE later.tenant_id = c.tenant_id AND later.conversation_id = c.id
					  AND later.direction = 'INCOMING' AND later.provider_deleted_at IS NULL
					  AND (later.sent_at, later.id) > (trigger_message.sent_at, trigger_message.id)
				)
			)) AS items
			FROM risk_signals AS risk
			JOIN messages AS trigger_message
			  ON trigger_message.tenant_id = risk.tenant_id AND trigger_message.id = risk.trigger_message_id
			WHERE risk.tenant_id = o.tenant_id AND risk.opportunity_id = o.id
			  AND risk.status IN ('OPEN', 'ACKNOWLEDGED', 'ACTED')
		) AS active_risks ON TRUE
		WHERE o.tenant_id = $1 AND o.id = $2`, tenantID, opportunityID).Scan(
		&state.TenantID, &state.OpportunityID, &state.OpportunityStage,
		&state.ActiveOpportunity, &locationID, &timezone, &threshold,
		&messageID, &sentAt, &direction, &bookingConfidence, &bookingRunID,
		&bookingMessageID, &bookingAt,
		&commitmentConfidence, &commitmentRunID, &commitmentMessageID,
		&commitmentAt, &commitmentText, &commitmentFollowedUp,
		&priceConfidence, &priceRunID, &priceMessageID, &priceAt,
		&priceAmount, &priceCurrency, &priceIncomingAfter,
		&followUpConfidence, &followUpRunID, &followUpMessageID, &followUpAt, &followUpIncomingAfter,
		&lastOutgoingID, &lastOutgoingAt, &lastIncomingID, &lastIncomingAt, &latestOutcome,
		&activeRisks,
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
	if commitmentConfidence != nil && commitmentRunID != nil && commitmentMessageID != nil &&
		commitmentAt != nil && commitmentText != nil && commitmentFollowedUp != nil {
		state.Commitment = &domain.CommitmentSignal{
			Value: true, Confidence: *commitmentConfidence, AIRunID: *commitmentRunID,
			EvidenceMessageID: *commitmentMessageID, EvidenceAt: commitmentAt.UTC(),
			EvidenceText: *commitmentText, FollowedUp: *commitmentFollowedUp,
		}
	}
	if priceConfidence != nil && priceRunID != nil && priceMessageID != nil && priceAt != nil &&
		priceAmount != nil && priceCurrency != nil && priceIncomingAfter != nil {
		state.Price = &domain.PriceSignal{
			Value: true, Confidence: *priceConfidence, AIRunID: *priceRunID,
			Amount: *priceAmount, Currency: *priceCurrency,
			EvidenceMessageID: *priceMessageID, EvidenceAt: priceAt.UTC(), IncomingAfter: *priceIncomingAfter,
		}
	}
	if followUpConfidence != nil && followUpRunID != nil && followUpMessageID != nil && followUpAt != nil && followUpIncomingAfter != nil {
		state.FollowUp = &domain.FollowUpSignal{
			Value: true, Confidence: *followUpConfidence, AIRunID: *followUpRunID,
			EvidenceMessageID: *followUpMessageID, EvidenceAt: followUpAt.UTC(), IncomingAfter: *followUpIncomingAfter,
		}
	}
	if lastOutgoingID != nil && lastOutgoingAt != nil {
		state.LastOutgoing = &domain.MessageRef{ID: *lastOutgoingID, At: lastOutgoingAt.UTC()}
	}
	if lastIncomingID != nil && lastIncomingAt != nil {
		state.LastIncoming = &domain.MessageRef{ID: *lastIncomingID, At: lastIncomingAt.UTC()}
	}
	if latestOutcome != nil {
		state.LatestOutcome = *latestOutcome
	}
	state.ActiveRisks = make(map[domain.Type]domain.ActiveRiskSnapshot)
	if len(activeRisks) > 0 {
		var items []struct {
			Type                 domain.Type `json:"type"`
			TriggerMessageID     string      `json:"triggerMessageId"`
			TriggerAt            time.Time   `json:"triggerAt"`
			OutgoingAfterTrigger bool        `json:"outgoingAfterTrigger"`
			IncomingAfterTrigger bool        `json:"incomingAfterTrigger"`
		}
		if err := json.Unmarshal(activeRisks, &items); err != nil {
			return domain.ConversationState{}, fmt.Errorf("разбор активных рисков: %w", err)
		}
		for _, item := range items {
			state.ActiveRisks[item.Type] = domain.ActiveRiskSnapshot{
				TriggerMessageID: item.TriggerMessageID, TriggerAt: item.TriggerAt.UTC(),
				OutgoingAfterTrigger: item.OutgoingAfterTrigger, IncomingAfterTrigger: item.IncomingAfterTrigger,
			}
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
