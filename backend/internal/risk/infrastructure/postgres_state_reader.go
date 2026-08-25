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
		  AND stage NOT IN ('WON', 'LOST', 'ARCHIVED')`, tenantID, conversationID).Scan(&opportunityID)
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
	err := reader.pool.QueryRow(ctx, `
		SELECT o.tenant_id, o.id,
		       o.stage NOT IN ('WON', 'LOST', 'ARCHIVED'),
		       c.location_id, l.timezone, l.response_threshold_minutes,
		       message.id, message.sent_at, message.direction
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
		WHERE o.tenant_id = $1 AND o.id = $2`, tenantID, opportunityID).Scan(
		&state.TenantID, &state.OpportunityID, &state.ActiveOpportunity,
		&locationID, &timezone, &threshold, &messageID, &sentAt, &direction,
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
