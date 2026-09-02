package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	aiapplication "lidradar/backend/internal/ai/application"
	aidomain "lidradar/backend/internal/ai/domain"
	aiinfrastructure "lidradar/backend/internal/ai/infrastructure"
	"lidradar/backend/platform/ids"
)

// TestSemanticBookingRiskFlow доказывает выходной критерий этапа 16 на
// PostgreSQL: реальная входная фраза проходит канонизацию, очередь AI,
// смысловой факт, R3, рабочие минуты, Radar, рекомендацию и Telegram-заглушку.
// Повтор результата не создаёт дубликат, а BOOKED закрывает риск.
func TestSemanticBookingRiskFlow(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "booking-risk@example.com", "Владелец записи")
	tenantID := createOrganization(t, fixture, owner, "Организация записи")
	linkID, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `
		INSERT INTO telegram_user_links(id, tenant_id, user_id, telegram_user_id, chat_id, linked_at, updated_at)
		VALUES ($1,$2,$3,71601,71601,now(),now())`, linkID, tenantID, owner.ID); err != nil {
		t.Fatal(err)
	}

	location := request(t, fixture.handler, http.MethodPost, "/api/v1/locations", `{
		"name":"Точка записи","timezone":"UTC","responseThresholdMinutes":120
	}`, owner.Cookie, tenantID)
	requireStatus(t, location, http.StatusCreated)
	locationID := jsonID(t, location)
	requireStatus(t, request(t, fixture.handler, http.MethodPut, "/api/v1/locations/"+locationID+"/business-hours", `{
		"timezone":"UTC","days":[
			{"weekday":1,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":2,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":3,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":4,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":5,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":6,"closed":false,"opensAt":"00:00","closesAt":"23:59"},
			{"weekday":7,"closed":false,"opensAt":"00:00","closesAt":"23:59"}
		]
	}`, owner.Cookie, tenantID), http.StatusOK)
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/services", `{
		"name":"Полировка","locationId":"`+locationID+`","priceFrom":"5000","priceTo":"5000"
	}`, owner.Cookie, tenantID), http.StatusCreated)

	webhookSecret := "stage-16-webhook-secret"
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{
		"name":"Канал записи","locationId":"`+locationID+`","webhookSecret":"`+webhookSecret+`"
	}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	connectionID := jsonID(t, connected)
	path := "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenantID + "/" + connectionID

	baselineAt := time.Now().UTC().Add(-61 * time.Minute).Format(time.RFC3339Nano)
	baseline := canonicalWebhook(
		"booking-event-baseline", "message.received.v1", "booking-dialog-strong", "booking-message-baseline",
		"booking-contact-strong", "INCOMING", "TEXT", "Нужна полировка", baselineAt, "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, baseline, "X-LidRadar-Webhook-Secret", webhookSecret), http.StatusAccepted)
	processExactly(t, fixture, 3)

	conversationID, baselineMessageID, opportunityID := semanticFlowIDs(t, fixture, tenantID, "booking-dialog-strong")
	aiStore := aiinfrastructure.NewPostgresStore(fixture.pool)
	aiBuilder := aiinfrastructure.NewPostgresAnalysisJobBuilder(fixture.pool, aiapplication.DefaultModelVersion)
	aiService := aiapplication.NewService(aiStore, ids.Generator{}, time.Now, aiapplication.DefaultLease).WithAnalysisDebounce(0).
		WithStaleJobBuilder(aiBuilder)
	nodeSecret := "stage-16-node-secret-with-at-least-32-characters"
	node, err := aiService.RegisterNode(context.Background(), tenantID, "AI-NODE-STAGE-16", nodeSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := aiService.Heartbeat(context.Background(), node.ID, nodeSecret, aiapplication.HeartbeatCommand{
		Status: aidomain.NodeReady, ModelVersion: aiapplication.DefaultModelVersion, AvailableSlots: 1,
	}); err != nil {
		t.Fatal(err)
	}
	job, found, err := aiService.Claim(context.Background(), node.ID, nodeSecret)
	if err != nil || !found || job.ConversationID != conversationID {
		t.Fatalf("захват AI-задания = %#v, найдено=%v, ошибка=%v", job, found, err)
	}
	baselineRun, err := aiService.Started(context.Background(), node.ID, nodeSecret, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	baselineOutput := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"` + baselineMessageID +
		`","summary":"Клиент интересуется полировкой.","facts":[]}`
	if completed, err := aiService.Complete(
		context.Background(), node.ID, nodeSecret, job.ID, baselineRun.ID, baselineOutput,
	); err != nil || completed.ApplicationStatus != aidomain.ApplicationApplied {
		t.Fatalf("исходный анализ = %#v, ошибка=%v", completed, err)
	}
	processExactly(t, fixture, 1)

	incomingAt := time.Now().UTC().Add(-60 * time.Minute).Format(time.RFC3339Nano)
	incoming := canonicalWebhook(
		"booking-event-strong", "message.received.v1", "booking-dialog-strong", "booking-message-strong",
		"booking-contact-strong", "INCOMING", "TEXT", "А завтра вечером можно?", incomingAt, "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, incoming, "X-LidRadar-Webhook-Secret", webhookSecret), http.StatusAccepted)
	processExactly(t, fixture, 3)
	_, messageID, currentOpportunityID := semanticFlowIDs(t, fixture, tenantID, "booking-dialog-strong")
	if currentOpportunityID != opportunityID {
		t.Fatalf("возможность изменилась: %s вместо %s", currentOpportunityID, opportunityID)
	}
	if err := aiService.Heartbeat(context.Background(), node.ID, nodeSecret, aiapplication.HeartbeatCommand{
		Status: aidomain.NodeReady, ModelVersion: aiapplication.DefaultModelVersion, AvailableSlots: 1,
	}); err != nil {
		t.Fatal(err)
	}
	job, found, err = aiService.Claim(context.Background(), node.ID, nodeSecret)
	if err != nil || !found || job.ConversationID != conversationID {
		t.Fatalf("захват актуального AI-задания = %#v, найдено=%v, ошибка=%v", job, found, err)
	}
	analysisRequest := decodeAnalysisJob(t, job.Prompt)
	if len(analysisRequest.Messages) == 0 || analysisRequest.Messages[len(analysisRequest.Messages)-1].Body != "А завтра вечером можно?" {
		t.Fatalf("AI получил неверный контекст: %#v", analysisRequest.Messages)
	}
	run, err := aiService.Started(context.Background(), node.ID, nodeSecret, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	output := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"` + messageID +
		`","summary":"Клиент уточняет время для записи на завтра.","facts":[{"type":"BOOKING_INTENT","value":true,"confidence":0.95,"evidenceMessageIds":["` + messageID + `"]}]}`
	completed, err := aiService.Complete(context.Background(), node.ID, nodeSecret, job.ID, run.ID, output)
	if err != nil || completed.ApplicationStatus != aidomain.ApplicationApplied {
		t.Fatalf("завершение AI = %#v, ошибка=%v", completed, err)
	}
	// Точный повтор ответа узла не создаёт второе событие или проверку.
	if _, err := aiService.Complete(context.Background(), node.ID, nodeSecret, job.ID, run.ID, output); err != nil {
		t.Fatalf("повтор завершения AI: %v", err)
	}
	processExactly(t, fixture, 1)

	var scheduledCount int
	var dueAt time.Time
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*), min(due_at)
		FROM scheduled_checks
		WHERE tenant_id = $1 AND subject_id = $2
		  AND check_type = 'BOOKING_NOT_CONFIRMED_DUE'`, tenantID, opportunityID).Scan(&scheduledCount, &dueAt); err != nil {
		t.Fatal(err)
	}
	if scheduledCount != 1 || dueAt.After(time.Now().UTC()) {
		t.Fatalf("проверок R3=%d, срок=%s", scheduledCount, dueAt)
	}
	promoted, err := fixture.scheduler.RunOnce(context.Background(), 100)
	if err != nil || promoted != 1 {
		t.Fatalf("перенос проверки R3 = %d, %v", promoted, err)
	}
	processExactly(t, fixture, 1)

	var riskID, status, source, policy, reason, storedRunID string
	var confidence float64
	var riskCount int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT min(id::text), min(status), min(source), min(risk_engine_version),
		       min(reason_text), min(confidence::double precision), min(ai_run_id::text), count(*)
		FROM risk_signals
		WHERE tenant_id = $1 AND opportunity_id = $2 AND type = 'BOOKING_NOT_CONFIRMED'`,
		tenantID, opportunityID,
	).Scan(&riskID, &status, &source, &policy, &reason, &confidence, &storedRunID, &riskCount); err != nil {
		t.Fatal(err)
	}
	if riskCount != 1 || status != "OPEN" || source != "HYBRID" ||
		policy != "booking-not-confirmed/v1" || confidence != 0.95 || storedRunID != run.ID ||
		!strings.Contains(reason, "30 рабочих минут") {
		t.Fatalf("риск R3: count=%d status=%s source=%s policy=%s confidence=%.3f run=%s reason=%q",
			riskCount, status, source, policy, confidence, storedRunID, reason)
	}

	radar := request(t, fixture.handler, http.MethodGet, "/api/v1/risks/"+riskID, "", owner.Cookie, tenantID)
	requireStatus(t, radar, http.StatusOK)
	if body := radar.Body.String(); !strings.Contains(body, `"type":"BOOKING_NOT_CONFIRMED"`) ||
		!strings.Contains(body, `"severity":"CRITICAL"`) || !strings.Contains(body, `"confidence":0.95`) ||
		!strings.Contains(body, `"aiRunId":"`+run.ID+`"`) {
		t.Fatalf("карточка Radar = %s", body)
	}
	recommendation := request(
		t, fixture.handler, http.MethodPost, "/api/v1/risks/"+riskID+"/recommendation", "", owner.Cookie, tenantID,
	)
	requireStatus(t, recommendation, http.StatusOK)
	if !strings.Contains(recommendation.Body.String(), "Предложить клиенту конкретный свободный слот.") {
		t.Fatalf("рекомендация R3 = %s", recommendation.Body.String())
	}
	if delivered, err := fixture.notifications.DispatchOne(context.Background(), "stage-16-notifications", time.Minute); err != nil || !delivered {
		t.Fatalf("Telegram-доставка = %v, %v", delivered, err)
	}
	var title, body, deliveryStatus string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT notification.title, notification.body, delivery.status
		FROM notifications AS notification
		JOIN notification_deliveries AS delivery
		  ON delivery.tenant_id = notification.tenant_id AND delivery.notification_id = notification.id
		WHERE notification.tenant_id = $1 AND notification.risk_id = $2`, tenantID, riskID,
	).Scan(&title, &body, &deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if title != "Критический риск: запись не подтверждена" ||
		!strings.Contains(body, "конкретный свободный слот") || deliveryStatus != "SUCCEEDED" {
		t.Fatalf("уведомление: title=%q body=%q status=%s", title, body, deliveryStatus)
	}

	booked := request(
		t, fixture.handler, http.MethodPatch, "/api/v1/opportunities/"+opportunityID,
		`{"stage":"BOOKED"}`, owner.Cookie, tenantID,
	)
	requireStatus(t, booked, http.StatusOK)
	processExactly(t, fixture, 1)
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT status FROM risk_signals WHERE tenant_id = $1 AND id = $2`, tenantID, riskID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "RESOLVED" {
		t.Fatalf("после BOOKED риск имеет статус %s", status)
	}
}

func semanticFlowIDs(t *testing.T, fixture apiFixture, tenantID, externalConversationID string) (string, string, string) {
	t.Helper()
	var conversationID, messageID, opportunityID string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT conversation.id, message.id, opportunity.id
		FROM conversations AS conversation
		JOIN messages AS message
		  ON message.tenant_id = conversation.tenant_id AND message.conversation_id = conversation.id
		JOIN opportunities AS opportunity
		  ON opportunity.tenant_id = conversation.tenant_id AND opportunity.conversation_id = conversation.id
		WHERE conversation.tenant_id = $1 AND conversation.external_id = $2
		ORDER BY message.sent_at DESC, message.id DESC
		LIMIT 1`,
		tenantID, externalConversationID,
	).Scan(&conversationID, &messageID, &opportunityID); err != nil {
		t.Fatal(err)
	}
	return conversationID, messageID, opportunityID
}

func decodeAnalysisJob(t *testing.T, prompt string) aiapplication.AnalyzeConversationRequestV1 {
	t.Helper()
	var request aiapplication.AnalyzeConversationRequestV1
	if err := json.Unmarshal([]byte(prompt), &request); err != nil {
		t.Fatal(err)
	}
	return request
}
