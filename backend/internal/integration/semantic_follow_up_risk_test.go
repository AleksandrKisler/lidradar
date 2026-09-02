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

// TestSemanticFollowUpRiskFlow доказывает выходной критерий этапа 19 на
// PostgreSQL: колебание клиента переводит сделку в WAITING_CUSTOMER, 24 рабочих
// часа без возвращения дают мягкого кандидата MEDIUM без немедленного
// уведомления, а явный отказ (исход NOT_A_LEAD) закрывает его; вторая сделка
// с тем же колебанием и зафиксированным отказом кандидата не получает.
func TestSemanticFollowUpRiskFlow(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "follow-up-risk@example.com", "Владелец кандидатов")
	tenantID := createOrganization(t, fixture, owner, "Организация кандидатов")

	location := request(t, fixture.handler, http.MethodPost, "/api/v1/locations", `{
		"name":"Точка кандидатов","timezone":"UTC","responseThresholdMinutes":1440
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

	webhookSecret := "stage-19-webhook-secret"
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{
		"name":"Канал кандидатов","locationId":"`+locationID+`","webhookSecret":"`+webhookSecret+`"
	}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	connectionID := jsonID(t, connected)
	path := "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenantID + "/" + connectionID

	aiStore := aiinfrastructure.NewPostgresStore(fixture.pool)
	aiBuilder := aiinfrastructure.NewPostgresAnalysisJobBuilder(fixture.pool, aiapplication.DefaultModelVersion)
	aiService := aiapplication.NewService(aiStore, ids.Generator{}, time.Now, aiapplication.DefaultLease).
		WithAnalysisDebounce(0).WithStaleJobBuilder(aiBuilder)
	nodeSecret := "stage-19-node-secret-with-at-least-32-characters"
	node, err := aiService.RegisterNode(context.Background(), tenantID, "AI-NODE-STAGE-19", nodeSecret)
	if err != nil {
		t.Fatal(err)
	}
	ready := aiapplication.HeartbeatCommand{
		Status: aidomain.NodeReady, ModelVersion: aiapplication.DefaultModelVersion, AvailableSlots: 1,
	}

	// Клиент колеблется 26 часов назад: «подумаю и вернусь».
	hesitatedAt := time.Now().UTC().Add(-26 * time.Hour).Format(time.RFC3339Nano)
	hesitation := canonicalWebhook(
		"follow-up-event-hesitation", "message.received.v1", "follow-up-dialog", "follow-up-message-hesitation",
		"follow-up-contact", "INCOMING", "TEXT", "Полировка интересна, но я подумаю и вернусь к вам позже.", hesitatedAt, "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, hesitation, "X-LidRadar-Webhook-Secret", webhookSecret), http.StatusAccepted)
	processExactly(t, fixture, 3)
	conversationID, messageID, opportunityID := semanticFlowIDs(t, fixture, tenantID, "follow-up-dialog")

	runID := analyseFollowUp(t, aiService, node.ID, nodeSecret, ready, conversationID, messageID)
	processExactly(t, fixture, 1)

	var stage, historySource string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT o.stage, COALESCE((SELECT source FROM opportunity_stage_history AS h
		        WHERE h.tenant_id = o.tenant_id AND h.opportunity_id = o.id AND h.to_stage = 'WAITING_CUSTOMER'), '')
		FROM opportunities AS o WHERE o.tenant_id = $1 AND o.id = $2`,
		tenantID, opportunityID).Scan(&stage, &historySource); err != nil {
		t.Fatal(err)
	}
	if stage != "WAITING_CUSTOMER" || historySource != "AI" {
		t.Fatalf("сделка после колебания: stage=%s source=%s", stage, historySource)
	}
	var scheduledCount int
	var dueAt time.Time
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*), min(due_at) FROM scheduled_checks
		WHERE tenant_id = $1 AND subject_id = $2 AND check_type = 'FOLLOW_UP_CANDIDATE_DUE'`,
		tenantID, opportunityID).Scan(&scheduledCount, &dueAt); err != nil {
		t.Fatal(err)
	}
	if scheduledCount != 1 || dueAt.After(time.Now().UTC()) {
		t.Fatalf("проверок R5=%d, срок=%s", scheduledCount, dueAt)
	}
	if promoted, err := fixture.scheduler.RunOnce(context.Background(), 100); err != nil || promoted < 1 {
		t.Fatalf("перенос проверки R5 = %d, %v", promoted, err)
	}
	processExactly(t, fixture, 1)

	var riskID, status, source, severity, policy, reason, storedRunID string
	var riskCount int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT min(id::text), min(status), min(source), min(severity), min(risk_engine_version),
		       min(reason_text), min(ai_run_id::text), count(*)
		FROM risk_signals
		WHERE tenant_id = $1 AND opportunity_id = $2 AND type = 'FOLLOW_UP_CANDIDATE'`,
		tenantID, opportunityID,
	).Scan(&riskID, &status, &source, &severity, &policy, &reason, &storedRunID, &riskCount); err != nil {
		t.Fatal(err)
	}
	if riskCount != 1 || status != "OPEN" || source != "HYBRID" || severity != "MEDIUM" ||
		policy != "follow-up-candidate/v1" || storedRunID != runID || !strings.Contains(reason, "остаётся ли услуга актуальной") {
		t.Fatalf("риск R5: count=%d status=%s source=%s severity=%s policy=%s run=%s reason=%q",
			riskCount, status, source, severity, policy, storedRunID, reason)
	}
	var notifications int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM notifications WHERE tenant_id = $1 AND risk_id = $2`, tenantID, riskID).Scan(&notifications); err != nil {
		t.Fatal(err)
	}
	if notifications != 0 {
		t.Fatalf("DIGEST-кандидат создал немедленных уведомлений: %d", notifications)
	}
	recommendation := request(
		t, fixture.handler, http.MethodPost, "/api/v1/risks/"+riskID+"/recommendation", "", owner.Cookie, tenantID,
	)
	requireStatus(t, recommendation, http.StatusOK)
	if !strings.Contains(recommendation.Body.String(), "Уточнить, остаётся ли услуга актуальной.") {
		t.Fatalf("рекомендация R5 = %s", recommendation.Body.String())
	}

	// Явный отказ, зафиксированный исходом NOT_A_LEAD, закрывает кандидата.
	outcome := idempotentRequest(t, fixture.handler, http.MethodPost, "/api/v1/opportunities/"+opportunityID+"/outcomes",
		`{"status":"NOT_A_LEAD","note":"Клиент отказался окончательно"}`, owner.Cookie, tenantID, "follow-up-outcome-1")
	if outcome.Code != http.StatusCreated && outcome.Code != http.StatusOK {
		t.Fatalf("исход = %d %s", outcome.Code, outcome.Body.String())
	}
	// Исход не порождает событий переписки: перепроверка приходит с новым
	// сообщением — компания вежливо закрывает диалог.
	closingAt := time.Now().UTC().Format(time.RFC3339Nano)
	closing := canonicalWebhook(
		"follow-up-event-closing", "message.received.v1", "follow-up-dialog", "follow-up-message-closing",
		"follow-up-contact", "OUTGOING", "TEXT", "Понимаем, будем рады помочь в другой раз.", closingAt, "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, closing, "X-LidRadar-Webhook-Secret", webhookSecret), http.StatusAccepted)
	processExactly(t, fixture, 3)
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT status FROM risk_signals WHERE tenant_id = $1 AND id = $2`, tenantID, riskID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "RESOLVED" {
		t.Fatalf("после явного отказа кандидат имеет статус %s", status)
	}
}

func analyseFollowUp(
	t *testing.T,
	service aiapplication.Service,
	nodeID, nodeSecret string,
	ready aiapplication.HeartbeatCommand,
	conversationID, messageID string,
) string {
	t.Helper()
	if err := service.Heartbeat(context.Background(), nodeID, nodeSecret, ready); err != nil {
		t.Fatal(err)
	}
	job, found, err := service.Claim(context.Background(), nodeID, nodeSecret)
	if err != nil || !found || job.ConversationID != conversationID || job.AnalysisThroughMessageID != messageID {
		t.Fatalf("захват AI-задания = %#v, найдено=%v, ошибка=%v", job, found, err)
	}
	run, err := service.Started(context.Background(), nodeID, nodeSecret, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	output := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"` + messageID +
		`","summary":"Клиент отложил решение и допускает возвращение.","facts":[{"type":"FOLLOW_UP_CANDIDATE","value":true,"confidence":0.92,"evidenceMessageIds":["` + messageID + `"]}]}`
	completed, err := service.Complete(context.Background(), nodeID, nodeSecret, job.ID, run.ID, output)
	if err != nil || completed.ApplicationStatus != aidomain.ApplicationApplied {
		t.Fatalf("завершение AI = %#v, ошибка=%v", completed, err)
	}
	return run.ID
}
