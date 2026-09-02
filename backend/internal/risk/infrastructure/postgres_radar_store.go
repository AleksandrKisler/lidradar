package infrastructure

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/risk/application"
	"lidradar/backend/internal/risk/domain"
	"lidradar/backend/platform/ids"
)

// PostgresRadarStore собирает проекцию Radar из авторитетных таблиц Risk,
// Opportunity, Conversation, корректирующих фактов и подтверждённой выручки.
type PostgresRadarStore struct{ pool *pgxpool.Pool }

func NewPostgresRadarStore(pool *pgxpool.Pool) *PostgresRadarStore {
	return &PostgresRadarStore{pool: pool}
}

type radarCursor struct {
	SeverityRank int       `json:"s"`
	BookingRank  int       `json:"b"`
	RevenueSort  string    `json:"r"`
	DueAt        time.Time `json:"d"`
	DetectedAt   time.Time `json:"t"`
	RiskID       string    `json:"i"`
	FilterKey    string    `json:"f"`
}

type rankedDetail struct {
	Detail       application.Detail
	SeverityRank int
	BookingRank  int
	RevenueSort  string
}

const radarProjection = `
	r.id AS risk_id, r.tenant_id, r.opportunity_id, r.location_id, r.type, r.severity, r.status,
	r.source, r.confidence, r.ai_run_id, r.risk_engine_version, r.trigger_message_id, r.reason_code,
	r.reason_text, r.detected_at, r.due_at, r.updated_at,
	r.acknowledged_at, r.acted_at, r.resolved_at,
	o.id AS radar_opportunity_id, o.stage, COALESCE(o.estimated_amount::text, '') AS potential_revenue,
	o.currency,
	c.id AS radar_conversation_id, c.contact_id,
	CASE r.severity WHEN 'CRITICAL' THEN 4 WHEN 'HIGH' THEN 3 WHEN 'MEDIUM' THEN 2 ELSE 1 END AS severity_rank,
	CASE WHEN o.stage = 'BOOKING_INTENT' THEN 1 ELSE 0 END AS booking_rank,
	COALESCE(o.estimated_amount, -1::numeric)::text AS revenue_sort`

const radarJoins = `
	FROM risk_signals AS r
	JOIN opportunities AS o ON o.tenant_id = r.tenant_id AND o.id = r.opportunity_id
	JOIN conversations AS c ON c.tenant_id = o.tenant_id AND c.id = o.conversation_id`

func (store *PostgresRadarStore) List(
	ctx context.Context,
	tenantID string,
	query application.ListQuery,
) (application.Page, error) {
	if store == nil || store.pool == nil || tenantID == "" || query.Limit < 1 || query.Limit > 100 {
		return application.Page{}, application.ErrInvalidCommand
	}
	arguments := []any{tenantID}
	where := []string{"r.tenant_id = $1"}
	appendRadarFilters(&where, &arguments, query.Filters)
	if query.Status != "" {
		arguments = append(arguments, query.Status)
		where = append(where, fmt.Sprintf("r.status = $%d", len(arguments)))
	}

	var cursor *radarCursor
	if query.After != "" {
		decoded, err := decodeRadarCursor(query.After, filterKey(query))
		if err != nil {
			return application.Page{}, application.ErrInvalidCommand
		}
		cursor = &decoded
	}

	sql := `WITH ranked AS (SELECT ` + radarProjection + radarJoins + ` WHERE ` + strings.Join(where, " AND ") + `)
		SELECT * FROM ranked`
	if cursor != nil {
		start := len(arguments) + 1
		arguments = append(arguments, cursor.SeverityRank, cursor.BookingRank,
			cursor.RevenueSort, cursor.DueAt, cursor.DetectedAt, cursor.RiskID)
		sql += fmt.Sprintf(` WHERE
			severity_rank < $%[1]d
			OR (severity_rank = $%[1]d AND booking_rank < $%[2]d)
			OR (severity_rank = $%[1]d AND booking_rank = $%[2]d AND revenue_sort::numeric < $%[3]d::numeric)
			OR (severity_rank = $%[1]d AND booking_rank = $%[2]d AND revenue_sort::numeric = $%[3]d::numeric AND due_at > $%[4]d)
			OR (severity_rank = $%[1]d AND booking_rank = $%[2]d AND revenue_sort::numeric = $%[3]d::numeric AND due_at = $%[4]d AND detected_at > $%[5]d)
			OR (severity_rank = $%[1]d AND booking_rank = $%[2]d AND revenue_sort::numeric = $%[3]d::numeric AND due_at = $%[4]d AND detected_at = $%[5]d AND risk_id > $%[6]d)`,
			start, start+1, start+2, start+3, start+4, start+5)
	}
	arguments = append(arguments, query.Limit+1)
	sql += fmt.Sprintf(` ORDER BY severity_rank DESC, booking_rank DESC,
		revenue_sort::numeric DESC, due_at ASC, detected_at ASC, risk_id ASC LIMIT $%d`, len(arguments))

	rows, err := store.pool.Query(ctx, sql, arguments...)
	if err != nil {
		return application.Page{}, mapRadarError("чтение Radar", err)
	}
	defer rows.Close()
	items := make([]rankedDetail, 0, query.Limit+1)
	for rows.Next() {
		item, scanErr := scanRankedDetail(rows)
		if scanErr != nil {
			return application.Page{}, scanErr
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return application.Page{}, fmt.Errorf("обход Radar: %w", err)
	}
	if err := store.loadCorrective(ctx, tenantID, items); err != nil {
		return application.Page{}, err
	}
	page := application.Page{Items: make([]application.Detail, 0, min(query.Limit, len(items)))}
	for index := 0; index < len(items) && index < query.Limit; index++ {
		page.Items = append(page.Items, items[index].Detail)
	}
	if len(items) > query.Limit {
		last := items[query.Limit-1]
		page.NextCursor, err = encodeRadarCursor(radarCursor{
			SeverityRank: last.SeverityRank, BookingRank: last.BookingRank,
			RevenueSort: last.RevenueSort, DueAt: last.Detail.Risk.DueAt,
			DetectedAt: last.Detail.Risk.DetectedAt, RiskID: last.Detail.Risk.ID, FilterKey: filterKey(query),
		})
		if err != nil {
			return application.Page{}, err
		}
	}
	return page, nil
}

func (store *PostgresRadarStore) Get(
	ctx context.Context,
	tenantID, riskID string,
) (application.Detail, bool, error) {
	if store == nil || store.pool == nil || tenantID == "" || !ids.Valid(riskID) {
		return application.Detail{}, false, application.ErrInvalidCommand
	}
	row := store.pool.QueryRow(ctx, `SELECT `+radarProjection+radarJoins+` WHERE r.tenant_id = $1 AND r.id = $2`, tenantID, riskID)
	item, err := scanRankedDetail(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.Detail{}, false, nil
	}
	if err != nil {
		return application.Detail{}, false, mapRadarError("чтение деталей риска", err)
	}
	items := []rankedDetail{item}
	if err := store.loadCorrective(ctx, tenantID, items); err != nil {
		return application.Detail{}, false, err
	}
	return items[0].Detail, true, nil
}

func (store *PostgresRadarStore) loadCorrective(
	ctx context.Context,
	tenantID string,
	items []rankedDetail,
) error {
	if len(items) == 0 {
		return nil
	}
	riskIndices := make(map[string][]int, len(items))
	opportunityIndices := make(map[string][]int, len(items))
	riskIDs := make([]string, 0, len(items))
	opportunityIDs := make([]string, 0, len(items))
	for index := range items {
		riskID := items[index].Detail.Risk.ID
		opportunityID := items[index].Detail.Risk.OpportunityID
		if _, exists := riskIndices[riskID]; !exists {
			riskIDs = append(riskIDs, riskID)
		}
		if _, exists := opportunityIndices[opportunityID]; !exists {
			opportunityIDs = append(opportunityIDs, opportunityID)
		}
		riskIndices[riskID] = append(riskIndices[riskID], index)
		opportunityIndices[opportunityID] = append(opportunityIndices[opportunityID], index)
	}

	riskArguments, riskPlaceholders := identifierArguments(tenantID, riskIDs)
	recommendations, err := store.pool.Query(ctx, `
		SELECT risk_id, id, text
		FROM recommendations
		WHERE tenant_id = $1 AND risk_id IN (`+riskPlaceholders+`)
		ORDER BY risk_id, created_at, id`, riskArguments...)
	if err != nil {
		return mapRadarError("чтение рекомендаций Radar", err)
	}
	for recommendations.Next() {
		var riskID string
		var recommendation application.Recommendation
		if err := recommendations.Scan(&riskID, &recommendation.ID, &recommendation.Text); err != nil {
			recommendations.Close()
			return fmt.Errorf("чтение строки рекомендации Radar: %w", err)
		}
		for _, index := range riskIndices[riskID] {
			copy := recommendation
			items[index].Detail.Recommendation = &copy
		}
	}
	if err := recommendations.Err(); err != nil {
		recommendations.Close()
		return fmt.Errorf("обход рекомендаций Radar: %w", err)
	}
	recommendations.Close()

	actions, err := store.pool.Query(ctx, `
		SELECT risk_id, id, type, created_at
		FROM actions
		WHERE tenant_id = $1 AND risk_id IN (`+riskPlaceholders+`)
		ORDER BY risk_id, created_at, id`, riskArguments...)
	if err != nil {
		return mapRadarError("чтение действий Radar", err)
	}
	for actions.Next() {
		var riskID string
		var action application.Action
		if err := actions.Scan(&riskID, &action.ID, &action.Type, &action.CreatedAt); err != nil {
			actions.Close()
			return fmt.Errorf("чтение строки действия Radar: %w", err)
		}
		for _, index := range riskIndices[riskID] {
			items[index].Detail.Actions = append(items[index].Detail.Actions, action)
		}
	}
	if err := actions.Err(); err != nil {
		actions.Close()
		return fmt.Errorf("обход действий Radar: %w", err)
	}
	actions.Close()

	opportunityArguments, opportunityPlaceholders := identifierArguments(tenantID, opportunityIDs)
	outcomes, err := store.pool.Query(ctx, `
		SELECT DISTINCT ON (opportunity_id) opportunity_id, id, status, created_at
		FROM outcomes
		WHERE tenant_id = $1 AND opportunity_id IN (`+opportunityPlaceholders+`)
		ORDER BY opportunity_id, created_at DESC, id DESC`, opportunityArguments...)
	if err != nil {
		return mapRadarError("чтение исходов Radar", err)
	}
	for outcomes.Next() {
		var opportunityID string
		var outcome application.Outcome
		if err := outcomes.Scan(&opportunityID, &outcome.ID, &outcome.Type, &outcome.CreatedAt); err != nil {
			outcomes.Close()
			return fmt.Errorf("чтение строки исхода Radar: %w", err)
		}
		for _, index := range opportunityIndices[opportunityID] {
			copy := outcome
			items[index].Detail.Outcome = &copy
		}
	}
	if err := outcomes.Err(); err != nil {
		outcomes.Close()
		return fmt.Errorf("обход исходов Radar: %w", err)
	}
	outcomes.Close()

	revenues, err := store.pool.Query(ctx, `
		SELECT event.opportunity_id, event.currency,
		       COALESCE(sum(event.amount) FILTER (
		           WHERE attribution.type = 'RECOVERED'
		       ), 0)::numeric(20,2)::text AS confirmed_recovered
		FROM revenue_events AS event
		JOIN revenue_attributions AS attribution
		  ON attribution.tenant_id = event.tenant_id
		 AND attribution.revenue_event_id = event.id
		WHERE event.tenant_id = $1
		  AND event.opportunity_id IN (`+opportunityPlaceholders+`)
		  AND event.status = 'CONFIRMED'
		GROUP BY event.opportunity_id, event.currency`, opportunityArguments...)
	if err != nil {
		return mapRadarError("чтение выручки Radar", err)
	}
	for revenues.Next() {
		var opportunityID, currency, confirmed string
		if err := revenues.Scan(&opportunityID, &currency, &confirmed); err != nil {
			revenues.Close()
			return fmt.Errorf("чтение строки выручки Radar: %w", err)
		}
		for _, index := range opportunityIndices[opportunityID] {
			opportunity := items[index].Detail.Opportunity
			if opportunity == nil || opportunity.Currency != currency {
				continue
			}
			potential := "0.00"
			if opportunity.PotentialRevenue != nil {
				potential = *opportunity.PotentialRevenue
			}
			items[index].Detail.Revenue = &application.Revenue{
				Currency: currency, Potential: potential, ConfirmedRecovered: confirmed,
			}
		}
	}
	if err := revenues.Err(); err != nil {
		revenues.Close()
		return fmt.Errorf("обход выручки Radar: %w", err)
	}
	revenues.Close()
	return nil
}

func identifierArguments(tenantID string, identifiers []string) ([]any, string) {
	arguments := make([]any, 1, len(identifiers)+1)
	arguments[0] = tenantID
	placeholders := make([]string, len(identifiers))
	for index, identifier := range identifiers {
		arguments = append(arguments, identifier)
		placeholders[index] = fmt.Sprintf("$%d", index+2)
	}
	return arguments, strings.Join(placeholders, ",")
}

func (store *PostgresRadarStore) Summary(
	ctx context.Context,
	tenantID string,
	filters application.Filters,
) (application.Summary, error) {
	if store == nil || store.pool == nil || tenantID == "" {
		return application.Summary{}, application.ErrInvalidCommand
	}
	arguments := []any{tenantID}
	where := []string{"r.tenant_id = $1"}
	appendRadarFilters(&where, &arguments, filters)
	// `selected` применяет только запрошенные фильтры. Счётчики и незакрытые
	// деньги дополнительно сужаются до активных рисков, а возвращённая выручка —
	// нет: по ТЗ §39 это сумма подтверждённых событий с атрибуцией RECOVERED, и
	// закрытие риска, ради которого деньги вернули, не должно её уменьшать.
	query := `
		WITH selected AS (
			SELECT r.id AS risk_id, r.opportunity_id, r.severity, r.status
			FROM risk_signals AS r
			WHERE ` + strings.Join(where, " AND ") + `
		), matching AS (
			SELECT risk_id, opportunity_id, severity
			FROM selected
			WHERE status IN ('OPEN', 'ACKNOWLEDGED', 'ACTED')
		), counts AS (
			SELECT count(*) AS open_risks,
			       count(*) FILTER (WHERE severity = 'CRITICAL') AS critical_risks
			FROM matching
		), risky_opportunities AS (
			SELECT DISTINCT opportunity_id FROM matching
		), potential_money AS (
			SELECT COALESCE(sum(o.estimated_amount), 0)::numeric(20,2)::text AS potential_revenue
			FROM risky_opportunities AS risky
			JOIN opportunities AS o ON o.tenant_id = $1 AND o.id = risky.opportunity_id
			JOIN organizations AS organization ON organization.id = o.tenant_id
			WHERE o.currency = organization.default_currency
		), recovered_money AS (
			SELECT COALESCE(sum(event.amount), 0)::numeric(20,2)::text AS confirmed_recovered_revenue
			FROM selected
			JOIN revenue_attributions AS attribution
			  ON attribution.tenant_id = $1 AND attribution.risk_id = selected.risk_id
			 AND attribution.type = 'RECOVERED'
			JOIN revenue_events AS event
			  ON event.tenant_id = attribution.tenant_id
			 AND event.id = attribution.revenue_event_id
			JOIN organizations AS organization ON organization.id = event.tenant_id
			WHERE event.status = 'CONFIRMED'
			  AND event.currency = organization.default_currency
		)
		SELECT counts.open_risks, counts.critical_risks,
		       potential_money.potential_revenue,
		       recovered_money.confirmed_recovered_revenue
		FROM counts CROSS JOIN potential_money CROSS JOIN recovered_money`
	var summary application.Summary
	if err := store.pool.QueryRow(ctx, query, arguments...).Scan(
		&summary.OpenRisks, &summary.CriticalRisks, &summary.PotentialRevenue,
		&summary.ConfirmedRecoveredRevenue,
	); err != nil {
		return application.Summary{}, mapRadarError("чтение сводки Radar", err)
	}
	return summary, nil
}

func (store *PostgresRadarStore) Acknowledge(
	ctx context.Context,
	tenantID, riskID string,
	at time.Time,
) (application.Mutation, error) {
	return store.mutate(ctx, tenantID, riskID, at, true)
}

func (store *PostgresRadarStore) Resolve(
	ctx context.Context,
	tenantID, riskID string,
	at time.Time,
) (application.Mutation, error) {
	return store.mutate(ctx, tenantID, riskID, at, false)
}

func (store *PostgresRadarStore) mutate(
	ctx context.Context,
	tenantID, riskID string,
	at time.Time,
	acknowledge bool,
) (application.Mutation, error) {
	if store == nil || store.pool == nil || tenantID == "" || !ids.Valid(riskID) || at.IsZero() {
		return application.Mutation{}, application.ErrInvalidCommand
	}
	statusExpression := `CASE WHEN current.status IN ('OPEN','ACKNOWLEDGED','ACTED') THEN 'RESOLVED' ELSE current.status END`
	acknowledgedExpression := `current.acknowledged_at`
	resolvedExpression := `CASE WHEN current.status IN ('OPEN','ACKNOWLEDGED','ACTED') THEN $3 ELSE current.resolved_at END`
	changedExpression := `current.status IN ('OPEN','ACKNOWLEDGED','ACTED')`
	if acknowledge {
		statusExpression = `CASE WHEN current.status = 'OPEN' THEN 'ACKNOWLEDGED' ELSE current.status END`
		acknowledgedExpression = `CASE WHEN current.status = 'OPEN' THEN $3 ELSE current.acknowledged_at END`
		resolvedExpression = `current.resolved_at`
		changedExpression = `current.status = 'OPEN'`
	}
	query := fmt.Sprintf(`
		WITH current AS (
			SELECT * FROM risk_signals WHERE tenant_id = $1 AND id = $2 FOR UPDATE
		), changed AS (
			UPDATE risk_signals AS risk
			SET status = %s,
			    acknowledged_at = %s,
			    resolved_at = %s,
			    updated_at = CASE WHEN %s THEN $3 ELSE current.updated_at END
			FROM current
			WHERE risk.id = current.id
			RETURNING risk.id, risk.tenant_id, risk.opportunity_id, risk.location_id,
			          risk.type, risk.severity, risk.status, risk.source, risk.confidence, risk.ai_run_id,
			          risk.risk_engine_version, risk.trigger_message_id,
			          risk.reason_code, risk.reason_text, risk.detected_at, risk.due_at,
			          risk.updated_at, risk.acknowledged_at, risk.acted_at, risk.resolved_at,
			          %s AS state_changed
		)
		SELECT * FROM changed`, statusExpression, acknowledgedExpression, resolvedExpression,
		changedExpression, changedExpression)
	mutation, err := scanRadarMutation(store.pool.QueryRow(ctx, query, tenantID, riskID, at.UTC()))
	if errors.Is(err, pgx.ErrNoRows) {
		return application.Mutation{}, nil
	}
	if err != nil {
		return application.Mutation{}, mapRadarError("изменение риска Radar", err)
	}
	mutation.Found = true
	return mutation, nil
}

func appendRadarFilters(where *[]string, arguments *[]any, filters application.Filters) {
	if filters.LocationID != "" {
		*arguments = append(*arguments, filters.LocationID)
		*where = append(*where, fmt.Sprintf("r.location_id = $%d", len(*arguments)))
	}
	if filters.Severity != "" {
		*arguments = append(*arguments, filters.Severity)
		*where = append(*where, fmt.Sprintf("r.severity = $%d", len(*arguments)))
	}
	if filters.RiskType != "" {
		*arguments = append(*arguments, filters.RiskType)
		*where = append(*where, fmt.Sprintf("r.type = $%d", len(*arguments)))
	}
}

func scanRankedDetail(row riskRow) (rankedDetail, error) {
	var result rankedDetail
	var risk domain.Risk
	var opportunity application.Opportunity
	var conversation application.Conversation
	var potentialRevenue string
	if err := row.Scan(
		&risk.ID, &risk.TenantID, &risk.OpportunityID, &risk.LocationID,
		&risk.Type, &risk.Severity, &risk.Status, &risk.Source, &risk.Confidence,
		&risk.AIRunID, &risk.PolicyVersion,
		&risk.TriggerMessageID, &risk.ReasonCode, &risk.Reason, &risk.DetectedAt,
		&risk.DueAt, &risk.UpdatedAt, &risk.AcknowledgedAt, &risk.ActedAt, &risk.ResolvedAt,
		&opportunity.ID, &opportunity.Stage, &potentialRevenue, &opportunity.Currency,
		&conversation.ID, &conversation.ContactID,
		&result.SeverityRank, &result.BookingRank, &result.RevenueSort,
	); err != nil {
		return rankedDetail{}, err
	}
	if risk.Validate() != nil {
		return rankedDetail{}, domain.ErrInvalidRisk
	}
	opportunity.LocationID = risk.LocationID
	if potentialRevenue != "" {
		opportunity.PotentialRevenue = &potentialRevenue
	}
	result.Detail = application.Detail{
		Risk: risk, Opportunity: &opportunity, Conversation: &conversation,
		Actions: []application.Action{},
	}
	return result, nil
}

func scanRadarMutation(row riskRow) (application.Mutation, error) {
	var mutation application.Mutation
	if err := row.Scan(
		&mutation.Risk.ID, &mutation.Risk.TenantID, &mutation.Risk.OpportunityID,
		&mutation.Risk.LocationID, &mutation.Risk.Type, &mutation.Risk.Severity,
		&mutation.Risk.Status, &mutation.Risk.Source, &mutation.Risk.Confidence,
		&mutation.Risk.AIRunID, &mutation.Risk.PolicyVersion,
		&mutation.Risk.TriggerMessageID, &mutation.Risk.ReasonCode, &mutation.Risk.Reason,
		&mutation.Risk.DetectedAt, &mutation.Risk.DueAt, &mutation.Risk.UpdatedAt,
		&mutation.Risk.AcknowledgedAt, &mutation.Risk.ActedAt, &mutation.Risk.ResolvedAt,
		&mutation.Changed,
	); err != nil {
		return application.Mutation{}, err
	}
	if mutation.Risk.Validate() != nil {
		return application.Mutation{}, domain.ErrInvalidRisk
	}
	return mutation, nil
}

func filterKey(query application.ListQuery) string {
	return strings.Join([]string{
		string(query.Status), query.LocationID, string(query.Severity), string(query.RiskType),
	}, "\x00")
}

func encodeRadarCursor(cursor radarCursor) (string, error) {
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("кодирование курсора Radar: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeRadarCursor(value, expectedFilterKey string) (radarCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return radarCursor{}, application.ErrInvalidCommand
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var cursor radarCursor
	if err := decoder.Decode(&cursor); err != nil {
		return radarCursor{}, application.ErrInvalidCommand
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return radarCursor{}, application.ErrInvalidCommand
	}
	if cursor.SeverityRank < 1 || cursor.SeverityRank > 4 ||
		(cursor.BookingRank != 0 && cursor.BookingRank != 1) || cursor.RevenueSort == "" ||
		cursor.DueAt.IsZero() || cursor.DetectedAt.IsZero() || !ids.Valid(cursor.RiskID) || cursor.FilterKey != expectedFilterKey {
		return radarCursor{}, application.ErrInvalidCommand
	}
	return cursor, nil
}

func mapRadarError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "22P02", "22003", "23514":
			return application.ErrInvalidCommand
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}

var _ application.RadarStore = (*PostgresRadarStore)(nil)
