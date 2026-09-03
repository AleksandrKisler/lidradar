// Package loadgen создаёт синтетический нагрузочный набор ТЗ §72: организации
// с точками, услугами, каналами, переписками, сообщениями, сделками и
// рисками. Набор пишется напрямую владельцем схемы пакетными COPY, чтобы
// объём в сотни тысяч сообщений появлялся за секунды (LR-BE-2501).
package loadgen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/platform/ids"
)

// Plan — размер набора: организации × переписки × сообщения (ТЗ §72:
// 100 × 500 × 10 ≈ 500 000 сообщений в месяц).
type Plan struct {
	Label            string
	Organizations    int
	Conversations    int
	Messages         int
	WebhookSecret    string
	OpportunityShare float64
	RiskShare        float64
	Now              time.Time
}

// Organization — координаты сгенерированной организации для нагрузочных
// сценариев: владелец, точка, канал с известным секретом и услуга.
type Organization struct {
	TenantID, UserID, Email, LocationID, ConnectionID, ServiceID string
	SampleConversationID, SampleOpportunityID                    string
	OpportunityIDs                                               []string
}

type Result struct {
	Organizations []Organization
	Conversations int
	Messages      int
	Opportunities int
	Risks         int
	Duration      time.Duration
}

func (plan Plan) normalized() (Plan, error) {
	if plan.Organizations <= 0 || plan.Conversations <= 0 || plan.Messages <= 0 {
		return Plan{}, fmt.Errorf("план набора должен быть положительным")
	}
	if plan.Label == "" {
		plan.Label = "load"
	}
	if plan.WebhookSecret == "" {
		plan.WebhookSecret = "load-webhook-secret-1234567890"
	}
	if plan.OpportunityShare <= 0 || plan.OpportunityShare > 1 {
		plan.OpportunityShare = 0.4
	}
	if plan.RiskShare <= 0 || plan.RiskShare > 1 {
		plan.RiskShare = 0.3
	}
	if plan.Now.IsZero() {
		plan.Now = time.Now().UTC()
	}
	return plan, nil
}

// SecretHash — отпечаток секрета вебхука в том же виде, что и у модуля Connector.
func SecretHash(secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(digest[:])
}

// Generate пишет набор по одной организации на транзакцию.
func Generate(ctx context.Context, pool *pgxpool.Pool, plan Plan) (Result, error) {
	plan, err := plan.normalized()
	if err != nil {
		return Result{}, err
	}
	started := time.Now()
	result := Result{}
	generator := ids.Generator{}
	newID := func() (string, error) { return generator.NewID() }
	stages := []string{"NEW", "QUALIFYING", "PRICE_SENT", "WAITING_CUSTOMER"}
	windowStart := plan.Now.Add(-30 * 24 * time.Hour)
	conversationStep := 30 * 24 * time.Hour / time.Duration(plan.Conversations)
	for organizationIndex := 0; organizationIndex < plan.Organizations; organizationIndex++ {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return Result{}, fmt.Errorf("начало организации %d: %w", organizationIndex, err)
		}
		organization, counts, err := generateOrganization(ctx, tx, plan, organizationIndex, newID, stages, windowStart, conversationStep)
		if err != nil {
			_ = tx.Rollback(ctx)
			return Result{}, fmt.Errorf("организация %d: %w", organizationIndex, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return Result{}, fmt.Errorf("фиксация организации %d: %w", organizationIndex, err)
		}
		result.Organizations = append(result.Organizations, organization)
		result.Conversations += counts[0]
		result.Messages += counts[1]
		result.Opportunities += counts[2]
		result.Risks += counts[3]
	}
	result.Duration = time.Since(started)
	return result, nil
}

func generateOrganization(
	ctx context.Context,
	tx pgx.Tx,
	plan Plan,
	index int,
	newID func() (string, error),
	stages []string,
	windowStart time.Time,
	conversationStep time.Duration,
) (Organization, [4]int, error) {
	var counts [4]int
	tenantID, err := newID()
	if err != nil {
		return Organization{}, counts, err
	}
	userID, _ := newID()
	membershipID, _ := newID()
	locationID, _ := newID()
	serviceID, _ := newID()
	connectionID, _ := newID()
	createdAt := windowStart.Add(-time.Hour)
	email := fmt.Sprintf("%s-owner-%d@load.test", plan.Label, index)
	organization := Organization{TenantID: tenantID, UserID: userID, Email: email, LocationID: locationID, ConnectionID: connectionID, ServiceID: serviceID}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id, email, password_hash, display_name, status, created_at, updated_at) VALUES ($1, $2, '$load$', $3, 'ACTIVE', $4, $4)`,
			[]any{userID, email, fmt.Sprintf("Владелец %d", index), createdAt}},
		{`INSERT INTO organizations(id, name, default_timezone, default_currency, status, created_at, updated_at) VALUES ($1, $2, 'Europe/Moscow', 'RUB', 'ACTIVE', $3, $3)`,
			[]any{tenantID, fmt.Sprintf("Нагрузочная организация %d", index), createdAt}},
		{`INSERT INTO memberships(id, tenant_id, user_id, role, status, created_at, updated_at) VALUES ($1, $2, $3, 'OWNER', 'ACTIVE', $4, $4)`,
			[]any{membershipID, tenantID, userID, createdAt}},
		{`INSERT INTO locations(id, tenant_id, name, timezone, response_threshold_minutes, active, created_at, updated_at) VALUES ($1, $2, $3, 'Europe/Moscow', 45, TRUE, $4, $4)`,
			[]any{locationID, tenantID, fmt.Sprintf("Точка %d", index), createdAt}},
		{`INSERT INTO service_catalog_items(id, tenant_id, location_id, name, normalized_name, price_from, price_to, currency, active, created_at, updated_at) VALUES ($1, $2, $3, 'Полировка', 'полировка', 5000, 5000, 'RUB', TRUE, $4, $4)`,
			[]any{serviceID, tenantID, locationID, createdAt}},
		{`INSERT INTO channel_connections(id, tenant_id, location_id, provider, name, status, capabilities, verification_secret_hash, created_at, updated_at) VALUES ($1, $2, $3, 'GENERIC_WEBHOOK', $4, 'ACTIVE', '["CAN_RECEIVE_MESSAGES","CAN_RECEIVE_EDITS","CAN_RECEIVE_DELETES","CAN_RECEIVE_ATTACHMENTS","CAN_IDENTIFY_CONTACT"]'::jsonb, $5, $6, $6)`,
			[]any{connectionID, tenantID, locationID, fmt.Sprintf("Канал %d", index), SecretHash(plan.WebhookSecret), createdAt}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.query, statement.args...); err != nil {
			return Organization{}, counts, fmt.Errorf("%s: %w", statement.query[:40], err)
		}
	}
	for weekday := 1; weekday <= 7; weekday++ {
		hoursID, _ := newID()
		if _, err := tx.Exec(ctx, `
			INSERT INTO location_business_hours(id, tenant_id, location_id, weekday, is_closed, opens_at, closes_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, FALSE, '09:00', '21:00', $5, $5)`, hoursID, tenantID, locationID, weekday, createdAt); err != nil {
			return Organization{}, counts, fmt.Errorf("часы работы: %w", err)
		}
	}

	contacts := make([][]any, 0, plan.Conversations)
	conversations := make([][]any, 0, plan.Conversations)
	messages := make([][]any, 0, plan.Conversations*plan.Messages)
	opportunities := make([][]any, 0, plan.Conversations/2)
	history := make([][]any, 0, plan.Conversations/2)
	risks := make([][]any, 0, plan.Conversations/4)
	for conversationIndex := 0; conversationIndex < plan.Conversations; conversationIndex++ {
		contactID, _ := newID()
		conversationID, _ := newID()
		firstAt := windowStart.Add(time.Duration(conversationIndex) * conversationStep)
		lastAt := firstAt.Add(time.Duration(plan.Messages-1) * 5 * time.Minute)
		lastDirection := "INCOMING"
		if (plan.Messages-1)%2 == 1 {
			lastDirection = "OUTGOING"
		}
		contacts = append(contacts, []any{contactID, tenantID, fmt.Sprintf("Клиент %d-%d", index, conversationIndex), firstAt, firstAt})
		conversations = append(conversations, []any{
			conversationID, tenantID, locationID, connectionID, contactID,
			fmt.Sprintf("%s-%d-%d", plan.Label, index, conversationIndex), "ACTIVE", firstAt, lastAt, lastDirection,
			int64(plan.Messages), firstAt, lastAt,
		})
		var lastIncomingID string
		for messageIndex := 0; messageIndex < plan.Messages; messageIndex++ {
			messageID, _ := newID()
			sentAt := firstAt.Add(time.Duration(messageIndex) * 5 * time.Minute)
			direction, text := "INCOMING", "Нужна полировка, подскажите цену и свободное время."
			if messageIndex%2 == 1 {
				direction, text = "OUTGOING", "Здравствуйте! Полировка от 5000 ₽, могу предложить слот завтра."
			} else {
				lastIncomingID = messageID
			}
			messages = append(messages, []any{
				messageID, tenantID, conversationID, connectionID,
				fmt.Sprintf("%s-%d-%d-%d", plan.Label, index, conversationIndex, messageIndex), direction, "TEXT", text,
				sentAt, sentAt, []byte(`{}`), sentAt,
			})
		}
		if organization.SampleConversationID == "" {
			organization.SampleConversationID = conversationID
		}
		if float64(conversationIndex%100) < plan.OpportunityShare*100 {
			opportunityID, _ := newID()
			historyID, _ := newID()
			stage := stages[conversationIndex%len(stages)]
			opportunities = append(opportunities, []any{opportunityID, tenantID, conversationID, serviceID, stage, int64(5000), "RUB", firstAt, firstAt, lastAt})
			history = append(history, []any{historyID, tenantID, opportunityID, nil, stage, "RULE", firstAt})
			organization.OpportunityIDs = append(organization.OpportunityIDs, opportunityID)
			if organization.SampleOpportunityID == "" {
				organization.SampleOpportunityID = opportunityID
			}
			if float64(conversationIndex%100) < plan.OpportunityShare*plan.RiskShare*100 && lastIncomingID != "" {
				riskID, _ := newID()
				detectedAt := lastAt.Add(45 * time.Minute)
				status, resolvedAt := "OPEN", any(nil)
				if conversationIndex%2 == 1 {
					status, resolvedAt = "RESOLVED", detectedAt.Add(30*time.Minute)
				}
				risks = append(risks, []any{
					riskID, tenantID, opportunityID, locationID, "NO_RESPONSE", "HIGH", status, "NO_RESPONSE_THRESHOLD_EXCEEDED",
					"Бизнес не ответил клиенту за 45 рабочих минут", "RULE", "no-response/v1", lastIncomingID, detectedAt, detectedAt,
					resolvedAt, detectedAt, detectedAt,
				})
			}
		}
	}
	copies := []struct {
		table   string
		columns []string
		rows    [][]any
	}{
		{"contacts", []string{"id", "tenant_id", "display_name", "created_at", "updated_at"}, contacts},
		{"conversations", []string{"id", "tenant_id", "location_id", "connection_id", "contact_id", "external_id", "status", "first_message_at", "last_message_at", "last_message_direction", "revision", "created_at", "updated_at"}, conversations},
		{"messages", []string{"id", "tenant_id", "conversation_id", "connection_id", "external_id", "direction", "type", "text", "sent_at", "received_at", "metadata", "created_at"}, messages},
		{"opportunities", []string{"id", "tenant_id", "conversation_id", "service_id", "stage", "estimated_amount", "currency", "opened_at", "created_at", "updated_at"}, opportunities},
		{"opportunity_stage_history", []string{"id", "tenant_id", "opportunity_id", "from_stage", "to_stage", "source", "created_at"}, history},
		{"risk_signals", []string{"id", "tenant_id", "opportunity_id", "location_id", "type", "severity", "status", "reason_code", "reason_text", "source", "risk_engine_version", "trigger_message_id", "detected_at", "due_at", "resolved_at", "created_at", "updated_at"}, risks},
	}
	for _, item := range copies {
		if len(item.rows) == 0 {
			continue
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{item.table}, item.columns, pgx.CopyFromRows(item.rows)); err != nil {
			return Organization{}, counts, fmt.Errorf("COPY %s: %w", item.table, err)
		}
	}
	counts = [4]int{len(conversations), len(messages), len(opportunities), len(risks)}
	return organization, counts, nil
}

// Summary — одна строка для журнала и отчёта.
func (result Result) Summary() string {
	return strings.TrimSpace(fmt.Sprintf(
		"организаций=%d переписок=%d сообщений=%d сделок=%d рисков=%d за %s",
		len(result.Organizations), result.Conversations, result.Messages, result.Opportunities, result.Risks, result.Duration.Round(time.Millisecond),
	))
}
