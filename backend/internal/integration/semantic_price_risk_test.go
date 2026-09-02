package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	aiapplication "lidradar/backend/internal/ai/application"
	aidomain "lidradar/backend/internal/ai/domain"
	aiinfrastructure "lidradar/backend/internal/ai/infrastructure"
	"lidradar/backend/platform/ids"
)

// TestSemanticPriceRiskFlow доказывает выходной критерий этапа 18 на
// PostgreSQL: названная компанией цена проходит очередь AI, факт
// PRICE_MENTIONED переводит сделку в PRICE_SENT и записывает оценку выручки
// только из доверенного факта, молчание клиента 24 рабочих часа открывает
// R2 MEDIUM и назначает эскалацию, немедленное Telegram-уведомление не
// создаётся (DIGEST), а входящее клиента закрывает риск.
func TestSemanticPriceRiskFlow(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "price-risk@example.com", "Владелец цен")
	tenantID := createOrganization(t, fixture, owner, "Организация цен")

	location := request(t, fixture.handler, http.MethodPost, "/api/v1/locations", `{
		"name":"Точка цен","timezone":"UTC","responseThresholdMinutes":1440
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
	// Диапазон цен не даёт точной оценки: потенциальная выручка остаётся NULL,
	// пока компания не назовёт цену.
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/services", `{
		"name":"Полировка","locationId":"`+locationID+`","priceFrom":"4000","priceTo":"6000"
	}`, owner.Cookie, tenantID), http.StatusCreated)

	webhookSecret := "stage-18-webhook-secret"
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{
		"name":"Канал цен","locationId":"`+locationID+`","webhookSecret":"`+webhookSecret+`"
	}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	connectionID := jsonID(t, connected)
	path := "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenantID + "/" + connectionID

	// Клиент спросил 30 часов назад, компания назвала цену 26 часов назад:
	// 24 рабочих часа молчания уже прошли, 48 — ещё нет.
	incomingAt := time.Now().UTC().Add(-30 * time.Hour).Format(time.RFC3339Nano)
	incoming := canonicalWebhook(
		"price-event-incoming", "message.received.v1", "price-dialog", "price-message-incoming",
		"price-contact", "INCOMING", "TEXT", "Нужна полировка, сколько стоит?", incomingAt, "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, incoming, "X-LidRadar-Webhook-Secret", webhookSecret), http.StatusAccepted)
	processExactly(t, fixture, 3)
	conversationID, _, opportunityID := semanticFlowIDs(t, fixture, tenantID, "price-dialog")
	var initialEstimate *string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT estimated_amount::text FROM opportunities WHERE tenant_id = $1 AND id = $2`,
		tenantID, opportunityID).Scan(&initialEstimate); err != nil {
		t.Fatal(err)
	}
	if initialEstimate != nil {
		t.Fatalf("диапазон каталога дал выдуманную оценку %s", *initialEstimate)
	}

	pricedAt := time.Now().UTC().Add(-26 * time.Hour).Format(time.RFC3339Nano)
	priced := canonicalWebhook(
		"price-event-outgoing", "message.received.v1", "price-dialog", "price-message-outgoing",
		"price-contact", "OUTGOING", "TEXT", "Стоимость полировки — 5200 рублей.", pricedAt, "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, priced, "X-LidRadar-Webhook-Secret", webhookSecret), http.StatusAccepted)
	processExactly(t, fixture, 3)
	var priceMessageID string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT id FROM messages WHERE tenant_id = $1 AND conversation_id = $2 AND direction = 'OUTGOING'`,
		tenantID, conversationID).Scan(&priceMessageID); err != nil {
		t.Fatal(err)
	}
	if count := countRisks(t, fixture, tenantID, opportunityID, "CUSTOMER_SILENT_AFTER_PRICE"); count != 0 {
		t.Fatalf("риск R2 создан без факта цены и этапа PRICE_SENT: %d", count)
	}

	aiStore := aiinfrastructure.NewPostgresStore(fixture.pool)
	aiBuilder := aiinfrastructure.NewPostgresAnalysisJobBuilder(fixture.pool, aiapplication.DefaultModelVersion)
	aiService := aiapplication.NewService(aiStore, ids.Generator{}, time.Now, aiapplication.DefaultLease).
		WithAnalysisDebounce(0).WithStaleJobBuilder(aiBuilder)
	nodeSecret := "stage-18-node-secret-with-at-least-32-characters"
	node, err := aiService.RegisterNode(context.Background(), tenantID, "AI-NODE-STAGE-18", nodeSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := aiService.Heartbeat(context.Background(), node.ID, nodeSecret, aiapplication.HeartbeatCommand{
		Status: aidomain.NodeReady, ModelVersion: aiapplication.DefaultModelVersion, AvailableSlots: 1,
	}); err != nil {
		t.Fatal(err)
	}
	job, found, err := aiService.Claim(context.Background(), node.ID, nodeSecret)
	if err != nil || !found || job.ConversationID != conversationID || job.AnalysisThroughMessageID != priceMessageID {
		t.Fatalf("захват AI-задания = %#v, найдено=%v, ошибка=%v", job, found, err)
	}
	run, err := aiService.Started(context.Background(), node.ID, nodeSecret, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	output := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"` + priceMessageID +
		`","summary":"Компания назвала цену полировки.","facts":[{"type":"PRICE_MENTIONED","value":true,"confidence":0.96,"evidenceMessageIds":["` + priceMessageID + `"],"amount":"5200","currency":"RUB"}]}`
	completed, err := aiService.Complete(context.Background(), node.ID, nodeSecret, job.ID, run.ID, output)
	if err != nil || completed.ApplicationStatus != aidomain.ApplicationApplied {
		t.Fatalf("завершение AI = %#v, ошибка=%v", completed, err)
	}
	processExactly(t, fixture, 1)

	var stage, estimate, estimateConfidence, historySource string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT o.stage, COALESCE(o.estimated_amount::text, ''), COALESCE(o.estimated_amount_confidence::text, ''),
		       COALESCE((SELECT source FROM opportunity_stage_history AS h
		                 WHERE h.tenant_id = o.tenant_id AND h.opportunity_id = o.id AND h.to_stage = 'PRICE_SENT'), '')
		FROM opportunities AS o WHERE o.tenant_id = $1 AND o.id = $2`,
		tenantID, opportunityID).Scan(&stage, &estimate, &estimateConfidence, &historySource); err != nil {
		t.Fatal(err)
	}
	if stage != "PRICE_SENT" || estimate != "5200.00" || estimateConfidence != "0.960" || historySource != "AI" {
		t.Fatalf("сделка после цены: stage=%s estimate=%s confidence=%s source=%s", stage, estimate, estimateConfidence, historySource)
	}

	var scheduledCount int
	var dueAt time.Time
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*), min(due_at)
		FROM scheduled_checks
		WHERE tenant_id = $1 AND subject_id = $2 AND check_type = 'CUSTOMER_SILENT_AFTER_PRICE_DUE'
		  AND dedup_key NOT LIKE '%:escalation'`,
		tenantID, opportunityID).Scan(&scheduledCount, &dueAt); err != nil {
		t.Fatal(err)
	}
	if scheduledCount != 1 || dueAt.After(time.Now().UTC()) {
		t.Fatalf("проверок R2=%d, срок=%s", scheduledCount, dueAt)
	}
	promoted, err := fixture.scheduler.RunOnce(context.Background(), 100)
	if err != nil || promoted < 1 {
		t.Fatalf("перенос проверки R2 = %d, %v", promoted, err)
	}
	processExactly(t, fixture, 1)

	var riskID, status, source, severity, policy, reason, storedRunID string
	var confidence float64
	var riskCount int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT min(id::text), min(status), min(source), min(severity), min(risk_engine_version),
		       min(reason_text), min(confidence::double precision), min(ai_run_id::text), count(*)
		FROM risk_signals
		WHERE tenant_id = $1 AND opportunity_id = $2 AND type = 'CUSTOMER_SILENT_AFTER_PRICE'`,
		tenantID, opportunityID,
	).Scan(&riskID, &status, &source, &severity, &policy, &reason, &confidence, &storedRunID, &riskCount); err != nil {
		t.Fatal(err)
	}
	if riskCount != 1 || status != "OPEN" || source != "HYBRID" || severity != "MEDIUM" ||
		policy != "customer-silent-after-price/v1" || confidence != 0.96 || storedRunID != run.ID ||
		!strings.Contains(reason, "5200 RUB") {
		t.Fatalf("риск R2: count=%d status=%s source=%s severity=%s policy=%s confidence=%.3f run=%s reason=%q",
			riskCount, status, source, severity, policy, confidence, storedRunID, reason)
	}
	// Первая проверка назначила эскалацию до HIGH через 48 рабочих часов.
	var escalations int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM scheduled_checks
		WHERE tenant_id = $1 AND subject_id = $2 AND check_type = 'CUSTOMER_SILENT_AFTER_PRICE_DUE'
		  AND dedup_key LIKE '%:escalation' AND status = 'SCHEDULED' AND due_at > now()`,
		tenantID, opportunityID).Scan(&escalations); err != nil {
		t.Fatal(err)
	}
	if escalations != 1 {
		t.Fatalf("проверок эскалации = %d", escalations)
	}
	// R2 доставляется дайджестом (этап 20): немедленного уведомления нет.
	var notifications int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM notifications WHERE tenant_id = $1 AND risk_id = $2`, tenantID, riskID).Scan(&notifications); err != nil {
		t.Fatal(err)
	}
	if notifications != 0 {
		t.Fatalf("DIGEST-риск создал немедленных уведомлений: %d", notifications)
	}

	radar := request(t, fixture.handler, http.MethodGet, "/api/v1/risks/"+riskID, "", owner.Cookie, tenantID)
	requireStatus(t, radar, http.StatusOK)
	if body := radar.Body.String(); !strings.Contains(body, `"type":"CUSTOMER_SILENT_AFTER_PRICE"`) ||
		!strings.Contains(body, `"severity":"MEDIUM"`) || !strings.Contains(body, `"potentialRevenue":"5200.00"`) ||
		!strings.Contains(body, `"stage":"PRICE_SENT"`) {
		t.Fatalf("карточка Radar = %s", body)
	}
	recommendation := request(
		t, fixture.handler, http.MethodPost, "/api/v1/risks/"+riskID+"/recommendation", "", owner.Cookie, tenantID,
	)
	requireStatus(t, recommendation, http.StatusOK)
	if !strings.Contains(recommendation.Body.String(), "Напомнить клиенту о предложении") {
		t.Fatalf("рекомендация R2 = %s", recommendation.Body.String())
	}

	// Новое входящее клиента закрывает риск без участия AI.
	replyAt := time.Now().UTC().Format(time.RFC3339Nano)
	reply := canonicalWebhook(
		"price-event-reply", "message.received.v1", "price-dialog", "price-message-reply",
		"price-contact", "INCOMING", "TEXT", "Подходит, когда можно приехать?", replyAt, "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, reply, "X-LidRadar-Webhook-Secret", webhookSecret), http.StatusAccepted)
	processExactly(t, fixture, 3)
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT status FROM risk_signals WHERE tenant_id = $1 AND id = $2`, tenantID, riskID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "RESOLVED" {
		t.Fatalf("после входящего клиента риск имеет статус %s", status)
	}
}
