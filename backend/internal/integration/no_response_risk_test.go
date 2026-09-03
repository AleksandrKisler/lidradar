package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	notificationdomain "lidradar/backend/internal/notification/domain"
	"lidradar/backend/platform/ids"
)

// TestNoResponseRiskRealMessageFlow доказывает выходные критерии этапов 8–12:
// внешний webhook проходит канонизацию, создаёт Opportunity, долговечную
// проверку и Risk без AI; затем API создаёт рекомендацию, действие и исход, а
// цепочка BOOKED → PAID формально подтверждает 47 000 возвращённой выручки.
// Ответ бизнеса после этого закрывает тот же Risk автоматически.
func TestNoResponseRiskRealMessageFlow(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "risk-flow@example.com", "Владелец риска")
	tenantID := createOrganization(t, fixture, owner, "Организация риска")
	linkID, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `
		INSERT INTO telegram_user_links(id, tenant_id, user_id, telegram_user_id, chat_id, linked_at, updated_at)
		VALUES ($1,$2,$3,7001,7001,now(),now())`, linkID, tenantID, owner.ID); err != nil {
		t.Fatal(err)
	}

	locationResponse := request(t, fixture.handler, http.MethodPost, "/api/v1/locations", `{
		"name":"Круглосуточная точка","timezone":"UTC","responseThresholdMinutes":45
	}`, owner.Cookie, tenantID)
	requireStatus(t, locationResponse, http.StatusCreated)
	locationID := jsonID(t, locationResponse)
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
	secret := "risk-flow-secret-123"
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{
		"name":"Канал риска","locationId":"`+locationID+`","webhookSecret":"`+secret+`"
	}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	connectionID := jsonID(t, connected)
	path := "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenantID + "/" + connectionID

	// Клиент подписывается до появления риска. Поток передаёт только сигнал,
	// поэтому после него ниже обязательно выполняется REST-перечитывание.
	server := httptest.NewServer(fixture.handler)
	defer server.Close()
	streamContext, stopStream := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopStream()
	streamRequest, err := http.NewRequestWithContext(
		streamContext, http.MethodGet, server.URL+"/api/v1/events", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	streamRequest.AddCookie(owner.Cookie)
	streamRequest.Header.Set("X-Tenant-ID", tenantID)
	streamResponse, err := server.Client().Do(streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer streamResponse.Body.Close()
	if streamResponse.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d", streamResponse.StatusCode)
	}
	stream := bufio.NewReader(streamResponse.Body)
	if line, _ := stream.ReadString('\n'); line != ": connected\n" {
		t.Fatalf("начало SSE = %q", line)
	}
	_, _ = stream.ReadString('\n')

	incomingAt := time.Now().UTC().Add(-60 * time.Minute).Format(time.RFC3339Nano)
	incoming := canonicalWebhook(
		"risk-event-incoming", "message.received.v1", "risk-dialog", "risk-message-incoming", "risk-contact",
		"INCOMING", "TEXT", "Нужна полировка", incomingAt, "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, incoming, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	processExactly(t, fixture, 3)
	var payloadOnlyIdentifiers bool
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT check_row.payload ? 'opportunityId'
		       AND (SELECT count(*) FROM jsonb_object_keys(check_row.payload)) = 1
		FROM scheduled_checks AS check_row
		WHERE tenant_id = $1 AND check_type = 'NO_RESPONSE_DUE'`, tenantID).Scan(&payloadOnlyIdentifiers); err != nil {
		t.Fatal(err)
	}
	if !payloadOnlyIdentifiers {
		t.Fatal("проверка по расписанию содержит устаревающий снимок вместо идентификатора")
	}

	promoted, err := fixture.scheduler.RunOnce(context.Background(), 100)
	if err != nil || promoted != 1 {
		t.Fatalf("перенос наступившей проверки = %d, %v", promoted, err)
	}
	processExactly(t, fixture, 1)

	var riskID, opportunityID, severity, status, source, policyVersion string
	var riskCount int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT min(id::text), min(opportunity_id::text), min(severity), min(status), min(source), min(risk_engine_version), count(*)
		FROM risk_signals
		WHERE tenant_id = $1 AND type = 'NO_RESPONSE'`, tenantID).Scan(
		&riskID, &opportunityID, &severity, &status, &source, &policyVersion, &riskCount,
	); err != nil {
		t.Fatal(err)
	}
	if riskCount != 1 || severity != "HIGH" || status != "OPEN" || source != "RULE" || policyVersion != "no-response/v1" {
		t.Fatalf("риск: id=%s count=%d severity=%s status=%s source=%s policy=%s", riskID, riskCount, severity, status, source, policyVersion)
	}
	if delivered, err := fixture.notifications.DispatchOne(context.Background(), "integration-notifications", time.Minute, notificationdomain.ChannelTelegram); err != nil || !delivered {
		t.Fatalf("доставка уведомления = %v, %v", delivered, err)
	}
	var notificationCount, successfulDeliveries int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(DISTINCT notification.id),
		       count(*) FILTER (WHERE delivery.status = 'SUCCEEDED' AND delivery.channel = 'TELEGRAM')
		FROM notifications AS notification
		JOIN notification_deliveries AS delivery ON delivery.notification_id = notification.id
		WHERE notification.tenant_id = $1 AND notification.risk_id = $2`, tenantID, riskID).Scan(
		&notificationCount, &successfulDeliveries,
	); err != nil {
		t.Fatal(err)
	}
	if notificationCount != 1 || successfulDeliveries != 1 {
		t.Fatalf("уведомления=%d успешных доставок=%d", notificationCount, successfulDeliveries)
	}
	eventLine, eventErr := stream.ReadString('\n')
	dataLine, dataErr := stream.ReadString('\n')
	if eventErr != nil || dataErr != nil || eventLine != "event: risk.changed\n" ||
		!strings.Contains(dataLine, `"resourceId":"`+riskID+`"`) {
		t.Fatalf("сигнал создания: event=%q data=%q err=%v/%v", eventLine, dataLine, eventErr, dataErr)
	}
	_, _ = stream.ReadString('\n')

	summaryResponse := request(t, fixture.handler, http.MethodGet, "/api/v1/radar", "", owner.Cookie, tenantID)
	requireStatus(t, summaryResponse, http.StatusOK)
	if body := summaryResponse.Body.String(); !strings.Contains(body, `"openRisks":1`) ||
		!strings.Contains(body, `"criticalRisks":0`) || !strings.Contains(body, `"potentialRevenue":"5000.00"`) ||
		!strings.Contains(body, `"confirmedRecoveredRevenue":"0.00"`) {
		t.Fatalf("сводка Radar = %s", body)
	}
	listResponse := request(t, fixture.handler, http.MethodGet, "/api/v1/risks?limit=1", "", owner.Cookie, tenantID)
	requireStatus(t, listResponse, http.StatusOK)
	if body := listResponse.Body.String(); !strings.Contains(body, `"id":"`+riskID+`"`) ||
		!strings.Contains(body, `"actions":[]`) || !strings.Contains(body, `"opportunity"`) ||
		!strings.Contains(body, `"conversation"`) {
		t.Fatalf("список Radar = %s", body)
	}
	detailResponse := request(t, fixture.handler, http.MethodGet, "/api/v1/risks/"+riskID, "", owner.Cookie, tenantID)
	requireStatus(t, detailResponse, http.StatusOK)
	if body := detailResponse.Body.String(); !strings.Contains(body, `"potentialRevenue":"5000.00"`) ||
		strings.Contains(body, `"recommendation"`) || strings.Contains(body, `"outcome"`) ||
		strings.Contains(body, `"revenue"`) {
		t.Fatalf("детали Radar = %s", body)
	}

	recommendation := request(
		t, fixture.handler, http.MethodPost, "/api/v1/risks/"+riskID+"/recommendation", "", owner.Cookie, tenantID,
	)
	requireStatus(t, recommendation, http.StatusOK)
	if body := recommendation.Body.String(); !strings.Contains(body, `"source":"TEMPLATE"`) ||
		!strings.Contains(body, "Ответить клиенту сейчас.") {
		t.Fatalf("рекомендация = %s", body)
	}
	action := idempotentRequest(
		t, fixture.handler, http.MethodPost, "/api/v1/risks/"+riskID+"/actions",
		`{"type":"MARK_CONTACTED","note":"Связались с клиентом"}`,
		owner.Cookie, tenantID, "e2e-action",
	)
	requireStatus(t, action, http.StatusCreated)
	actionID := jsonID(t, action)
	replayedAction := idempotentRequest(
		t, fixture.handler, http.MethodPost, "/api/v1/risks/"+riskID+"/actions",
		`{"type":"MARK_CONTACTED","note":"Связались с клиентом"}`,
		owner.Cookie, tenantID, "e2e-action",
	)
	requireStatus(t, replayedAction, http.StatusOK)
	if replayedID := jsonID(t, replayedAction); replayedID != actionID {
		t.Fatalf("повтор действия вернул %s вместо %s", replayedID, actionID)
	}
	conflictingAction := idempotentRequest(
		t, fixture.handler, http.MethodPost, "/api/v1/risks/"+riskID+"/actions",
		`{"type":"CALL"}`, owner.Cookie, tenantID, "e2e-action",
	)
	requireStatus(t, conflictingAction, http.StatusConflict)
	outcome := idempotentRequest(
		t, fixture.handler, http.MethodPost, "/api/v1/opportunities/"+opportunityID+"/outcomes",
		`{"status":"BOOKED","note":"Запись подтверждена"}`,
		owner.Cookie, tenantID, "e2e-outcome",
	)
	requireStatus(t, outcome, http.StatusCreated)

	correctiveDetail := request(t, fixture.handler, http.MethodGet, "/api/v1/risks/"+riskID, "", owner.Cookie, tenantID)
	requireStatus(t, correctiveDetail, http.StatusOK)
	if body := correctiveDetail.Body.String(); !strings.Contains(body, `"status":"ACTED"`) ||
		!strings.Contains(body, `"recommendation"`) || !strings.Contains(body, `"type":"MARK_CONTACTED"`) ||
		!strings.Contains(body, `"outcome":{"id"`) || !strings.Contains(body, `"type":"BOOKED"`) {
		t.Fatalf("полная корректирующая карточка = %s", body)
	}
	paidOutcome := idempotentRequest(
		t, fixture.handler, http.MethodPost, "/api/v1/opportunities/"+opportunityID+"/outcomes",
		`{"status":"PAID","note":"Оплата 47 000 подтверждена"}`,
		owner.Cookie, tenantID, "e2e-paid-outcome",
	)
	requireStatus(t, paidOutcome, http.StatusCreated)
	paidOutcomeID := jsonID(t, paidOutcome)
	revenueBody := `{"amount":"47000","currency":"RUB","attributionType":"RECOVERED","riskId":"` +
		riskID + `","actionId":"` + actionID + `","outcomeId":"` + paidOutcomeID + `"}`
	revenue := idempotentRequest(
		t, fixture.handler, http.MethodPost, "/api/v1/opportunities/"+opportunityID+"/revenue",
		revenueBody, owner.Cookie, tenantID, "e2e-revenue-47000",
	)
	requireStatus(t, revenue, http.StatusCreated)
	var confirmation struct {
		Revenue struct {
			ID     string `json:"id"`
			Amount string `json:"amount"`
			Source string `json:"source"`
		} `json:"revenue"`
		Attribution struct {
			Type string `json:"type"`
		} `json:"attribution"`
	}
	if err := json.Unmarshal(revenue.Body.Bytes(), &confirmation); err != nil ||
		confirmation.Revenue.ID == "" || confirmation.Revenue.Amount != "47000.00" ||
		confirmation.Revenue.Source != "USER_CONFIRMED" || confirmation.Attribution.Type != "RECOVERED" {
		t.Fatalf("подтверждение выручки = %#v, ошибка=%v, body=%s", confirmation, err, revenue.Body.String())
	}
	replayedRevenue := idempotentRequest(
		t, fixture.handler, http.MethodPost, "/api/v1/opportunities/"+opportunityID+"/revenue",
		revenueBody, owner.Cookie, tenantID, "e2e-revenue-47000",
	)
	requireStatus(t, replayedRevenue, http.StatusOK)
	if replayedRevenue.Body.String() != revenue.Body.String() {
		t.Fatalf("повтор выручки изменил ответ: first=%s replay=%s", revenue.Body.String(), replayedRevenue.Body.String())
	}
	conflictingRevenue := idempotentRequest(
		t, fixture.handler, http.MethodPost, "/api/v1/opportunities/"+opportunityID+"/revenue",
		strings.Replace(revenueBody, `"47000"`, `"47001"`, 1),
		owner.Cookie, tenantID, "e2e-revenue-47000",
	)
	requireStatus(t, conflictingRevenue, http.StatusConflict)
	recoveredKPI := request(
		t, fixture.handler, http.MethodGet, "/api/v1/revenue/confirmed-recovered?currency=RUB",
		"", owner.Cookie, tenantID,
	)
	requireStatus(t, recoveredKPI, http.StatusOK)
	if recoveredKPI.Body.String() != "{\"amount\":\"47000.00\",\"currency\":\"RUB\"}\n" {
		t.Fatalf("показатель возвращённой выручки = %s", recoveredKPI.Body.String())
	}
	revenueDetail := request(t, fixture.handler, http.MethodGet, "/api/v1/risks/"+riskID, "", owner.Cookie, tenantID)
	requireStatus(t, revenueDetail, http.StatusOK)
	if body := revenueDetail.Body.String(); !strings.Contains(body, `"outcome":{"id"`) ||
		!strings.Contains(body, `"type":"PAID"`) ||
		!strings.Contains(body, `"revenue":{"currency":"RUB","potential":"5000.00","confirmedRecovered":"47000.00"}`) {
		t.Fatalf("денежная карточка Radar = %s", body)
	}
	revenueSummary := request(t, fixture.handler, http.MethodGet, "/api/v1/radar", "", owner.Cookie, tenantID)
	requireStatus(t, revenueSummary, http.StatusOK)
	if !strings.Contains(revenueSummary.Body.String(), `"confirmedRecoveredRevenue":"47000.00"`) {
		t.Fatalf("денежная сводка Radar = %s", revenueSummary.Body.String())
	}

	var actionCount, outcomeCount, revenueEventCount, attributionCount, auditCount, keyCount int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT
			(SELECT count(*) FROM actions WHERE tenant_id = $1 AND risk_id = $2),
			(SELECT count(*) FROM outcomes WHERE tenant_id = $1 AND opportunity_id = $3),
			(SELECT count(*) FROM revenue_events WHERE tenant_id = $1 AND opportunity_id = $3),
			(SELECT count(*) FROM revenue_attributions WHERE tenant_id = $1 AND opportunity_id = $3),
			(SELECT count(*) FROM audit_log WHERE tenant_id = $1),
			(SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1)`,
		tenantID, riskID, opportunityID,
	).Scan(&actionCount, &outcomeCount, &revenueEventCount, &attributionCount, &auditCount, &keyCount); err != nil {
		t.Fatal(err)
	}
	if actionCount != 1 || outcomeCount != 2 || revenueEventCount != 1 || attributionCount != 1 || auditCount != 4 || keyCount != 4 {
		t.Fatalf("денежная цепочка: actions=%d outcomes=%d events=%d attributions=%d audits=%d keys=%d",
			actionCount, outcomeCount, revenueEventCount, attributionCount, auditCount, keyCount)
	}

	outsider := register(t, fixture.handler, "risk-outsider@example.com", "Посторонний")
	forbidden := request(t, fixture.handler, http.MethodGet, "/api/v1/risks/"+riskID, "", outsider.Cookie, tenantID)
	requireStatus(t, forbidden, http.StatusForbidden)
	acknowledged := request(
		t, fixture.handler, http.MethodPost, "/api/v1/risks/"+riskID+"/acknowledge", "", owner.Cookie, tenantID,
	)
	requireStatus(t, acknowledged, http.StatusOK)
	if !strings.Contains(acknowledged.Body.String(), `"status":"ACTED"`) {
		t.Fatalf("подтверждение риска = %s", acknowledged.Body.String())
	}
	replayed := request(
		t, fixture.handler, http.MethodPost, "/api/v1/risks/"+riskID+"/acknowledge", "", owner.Cookie, tenantID,
	)
	requireStatus(t, replayed, http.StatusOK)

	outgoingAt := time.Now().UTC().Format(time.RFC3339Nano)
	outgoing := canonicalWebhook(
		"risk-event-outgoing", "message.received.v1", "risk-dialog", "risk-message-outgoing", "risk-contact",
		"OUTGOING", "TEXT", "Добрый день, готовы принять вас", outgoingAt, "risk-message-incoming",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, outgoing, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	processExactly(t, fixture, 3)
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT status FROM risk_signals WHERE tenant_id = $1 AND id = $2`, tenantID, riskID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "RESOLVED" {
		t.Fatalf("после ответа статус = %s, нужен RESOLVED", status)
	}
	closedSummary := request(t, fixture.handler, http.MethodGet, "/api/v1/radar", "", owner.Cookie, tenantID)
	requireStatus(t, closedSummary, http.StatusOK)
	if !strings.Contains(closedSummary.Body.String(), `"openRisks":0`) {
		t.Fatalf("сводка после закрытия = %s", closedSummary.Body.String())
	}
}
