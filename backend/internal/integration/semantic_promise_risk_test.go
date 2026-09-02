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

// TestSemanticPromiseRiskFlow доказывает выходной критерий этапа 17 на
// PostgreSQL: обещание компании проходит канонизацию, очередь AI, смысловой
// факт BUSINESS_COMMITMENT, разбор срока из текста обещания, проверку по
// расписанию, R4, Radar, рекомендацию и Telegram-заглушку. Исходящее
// сообщение после обещания закрывает риск.
func TestSemanticPromiseRiskFlow(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "promise-risk@example.com", "Владелец обещаний")
	tenantID := createOrganization(t, fixture, owner, "Организация обещаний")
	linkID, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `
		INSERT INTO telegram_user_links(id, tenant_id, user_id, telegram_user_id, chat_id, linked_at, updated_at)
		VALUES ($1,$2,$3,71701,71701,now(),now())`, linkID, tenantID, owner.ID); err != nil {
		t.Fatal(err)
	}

	location := request(t, fixture.handler, http.MethodPost, "/api/v1/locations", `{
		"name":"Точка обещаний","timezone":"UTC","responseThresholdMinutes":240
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

	webhookSecret := "stage-17-webhook-secret"
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{
		"name":"Канал обещаний","locationId":"`+locationID+`","webhookSecret":"`+webhookSecret+`"
	}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	connectionID := jsonID(t, connected)
	path := "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenantID + "/" + connectionID

	// Клиент написал 90 минут назад, компания пообещала ответить через десять
	// минут 45 минут назад: срок обещания (35 минут назад) уже наступил.
	incomingAt := time.Now().UTC().Add(-90 * time.Minute).Format(time.RFC3339Nano)
	incoming := canonicalWebhook(
		"promise-event-incoming", "message.received.v1", "promise-dialog", "promise-message-incoming",
		"promise-contact", "INCOMING", "TEXT", "Нужна полировка", incomingAt, "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, incoming, "X-LidRadar-Webhook-Secret", webhookSecret), http.StatusAccepted)
	processExactly(t, fixture, 3)
	conversationID, _, opportunityID := semanticFlowIDs(t, fixture, tenantID, "promise-dialog")

	promisedAt := time.Now().UTC().Add(-45 * time.Minute).Format(time.RFC3339Nano)
	promise := canonicalWebhook(
		"promise-event-outgoing", "message.received.v1", "promise-dialog", "promise-message-outgoing",
		"promise-contact", "OUTGOING", "TEXT", "Проверю расписание и отвечу через десять минут.", promisedAt, "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, promise, "X-LidRadar-Webhook-Secret", webhookSecret), http.StatusAccepted)
	processExactly(t, fixture, 3)
	var promiseMessageID string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT id FROM messages WHERE tenant_id = $1 AND conversation_id = $2 AND direction = 'OUTGOING'`,
		tenantID, conversationID).Scan(&promiseMessageID); err != nil {
		t.Fatal(err)
	}
	if count := countRisks(t, fixture, tenantID, opportunityID, "PROMISE_NOT_FULFILLED"); count != 0 {
		t.Fatalf("риск R4 создан без смыслового факта: %d", count)
	}

	aiStore := aiinfrastructure.NewPostgresStore(fixture.pool)
	aiBuilder := aiinfrastructure.NewPostgresAnalysisJobBuilder(fixture.pool, aiapplication.DefaultModelVersion)
	aiService := aiapplication.NewService(aiStore, ids.Generator{}, time.Now, aiapplication.DefaultLease).
		WithAnalysisDebounce(0).WithStaleJobBuilder(aiBuilder)
	nodeSecret := "stage-17-node-secret-with-at-least-32-characters"
	node, err := aiService.RegisterNode(context.Background(), tenantID, "AI-NODE-STAGE-17", nodeSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := aiService.Heartbeat(context.Background(), node.ID, nodeSecret, aiapplication.HeartbeatCommand{
		Status: aidomain.NodeReady, ModelVersion: aiapplication.DefaultModelVersion, AvailableSlots: 1,
	}); err != nil {
		t.Fatal(err)
	}
	job, found, err := aiService.Claim(context.Background(), node.ID, nodeSecret)
	if err != nil || !found || job.ConversationID != conversationID || job.AnalysisThroughMessageID != promiseMessageID {
		t.Fatalf("захват AI-задания = %#v, найдено=%v, ошибка=%v", job, found, err)
	}
	run, err := aiService.Started(context.Background(), node.ID, nodeSecret, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	output := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"` + promiseMessageID +
		`","summary":"Компания обещала проверить расписание.","facts":[{"type":"BUSINESS_COMMITMENT","value":true,"confidence":0.93,"evidenceMessageIds":["` + promiseMessageID + `"]}]}`
	completed, err := aiService.Complete(context.Background(), node.ID, nodeSecret, job.ID, run.ID, output)
	if err != nil || completed.ApplicationStatus != aidomain.ApplicationApplied {
		t.Fatalf("завершение AI = %#v, ошибка=%v", completed, err)
	}
	processExactly(t, fixture, 1)

	var scheduledCount int
	var dueAt time.Time
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*), min(due_at)
		FROM scheduled_checks
		WHERE tenant_id = $1 AND subject_id = $2 AND check_type = 'PROMISE_NOT_FULFILLED_DUE'`,
		tenantID, opportunityID).Scan(&scheduledCount, &dueAt); err != nil {
		t.Fatal(err)
	}
	promisedTime, _ := time.Parse(time.RFC3339Nano, promisedAt)
	if scheduledCount != 1 || !dueAt.Equal(promisedTime.Add(10*time.Minute).Truncate(time.Microsecond)) {
		t.Fatalf("проверок R4=%d, срок=%s, ожидался %s", scheduledCount, dueAt, promisedTime.Add(10*time.Minute))
	}
	promoted, err := fixture.scheduler.RunOnce(context.Background(), 100)
	if err != nil || promoted < 1 {
		t.Fatalf("перенос проверки R4 = %d, %v", promoted, err)
	}
	processExactly(t, fixture, 1)

	var riskID, status, source, severity, policy, reason, storedRunID string
	var confidence float64
	var riskCount int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT min(id::text), min(status), min(source), min(severity), min(risk_engine_version),
		       min(reason_text), min(confidence::double precision), min(ai_run_id::text), count(*)
		FROM risk_signals
		WHERE tenant_id = $1 AND opportunity_id = $2 AND type = 'PROMISE_NOT_FULFILLED'`,
		tenantID, opportunityID,
	).Scan(&riskID, &status, &source, &severity, &policy, &reason, &confidence, &storedRunID, &riskCount); err != nil {
		t.Fatal(err)
	}
	if riskCount != 1 || status != "OPEN" || source != "HYBRID" || severity != "HIGH" ||
		policy != "promise-not-fulfilled/v1" || confidence != 0.93 || storedRunID != run.ID ||
		!strings.Contains(reason, "через десять минут") {
		t.Fatalf("риск R4: count=%d status=%s source=%s severity=%s policy=%s confidence=%.3f run=%s reason=%q",
			riskCount, status, source, severity, policy, confidence, storedRunID, reason)
	}

	radar := request(t, fixture.handler, http.MethodGet, "/api/v1/risks/"+riskID, "", owner.Cookie, tenantID)
	requireStatus(t, radar, http.StatusOK)
	if body := radar.Body.String(); !strings.Contains(body, `"type":"PROMISE_NOT_FULFILLED"`) ||
		!strings.Contains(body, `"severity":"HIGH"`) || !strings.Contains(body, `"aiRunId":"`+run.ID+`"`) {
		t.Fatalf("карточка Radar = %s", body)
	}
	filtered := request(t, fixture.handler, http.MethodGet, "/api/v1/risks?riskType=PROMISE_NOT_FULFILLED", "", owner.Cookie, tenantID)
	requireStatus(t, filtered, http.StatusOK)
	if !strings.Contains(filtered.Body.String(), riskID) {
		t.Fatalf("фильтр Radar по типу не вернул риск: %s", filtered.Body.String())
	}
	recommendation := request(
		t, fixture.handler, http.MethodPost, "/api/v1/risks/"+riskID+"/recommendation", "", owner.Cookie, tenantID,
	)
	requireStatus(t, recommendation, http.StatusOK)
	if !strings.Contains(recommendation.Body.String(), "Выполнить обещанное клиенту или сообщить новый точный срок.") {
		t.Fatalf("рекомендация R4 = %s", recommendation.Body.String())
	}
	if delivered, err := fixture.notifications.DispatchOne(context.Background(), "stage-17-notifications", time.Minute); err != nil || !delivered {
		t.Fatalf("Telegram-доставка = %v, %v", delivered, err)
	}
	var title, deliveryStatus string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT notification.title, delivery.status
		FROM notifications AS notification
		JOIN notification_deliveries AS delivery
		  ON delivery.tenant_id = notification.tenant_id AND delivery.notification_id = notification.id
		WHERE notification.tenant_id = $1 AND notification.risk_id = $2`, tenantID, riskID,
	).Scan(&title, &deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if title != "Риск: обещание клиенту не выполнено" || deliveryStatus != "SUCCEEDED" {
		t.Fatalf("уведомление: title=%q status=%s", title, deliveryStatus)
	}

	// Компания написала клиенту после обещания — риск закрывается без AI.
	followUpAt := time.Now().UTC().Format(time.RFC3339Nano)
	followUp := canonicalWebhook(
		"promise-event-followup", "message.received.v1", "promise-dialog", "promise-message-followup",
		"promise-contact", "OUTGOING", "TEXT", "Расписание проверил: есть окно завтра в 18:00.", followUpAt, "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, followUp, "X-LidRadar-Webhook-Secret", webhookSecret), http.StatusAccepted)
	processExactly(t, fixture, 3)
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT status FROM risk_signals WHERE tenant_id = $1 AND id = $2`, tenantID, riskID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "RESOLVED" {
		t.Fatalf("после исходящего ответа риск имеет статус %s", status)
	}
	if count := countRisks(t, fixture, tenantID, opportunityID, "PROMISE_NOT_FULFILLED"); count != 1 {
		t.Fatalf("после закрытия рисков R4 = %d", count)
	}
}

func countRisks(t *testing.T, fixture apiFixture, tenantID, opportunityID, riskType string) int {
	t.Helper()
	var count int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM risk_signals WHERE tenant_id = $1 AND opportunity_id = $2 AND type = $3`,
		tenantID, opportunityID, riskType).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
