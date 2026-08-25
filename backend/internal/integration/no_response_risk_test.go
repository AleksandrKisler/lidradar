package integration_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNoResponseRiskRealMessageFlow доказывает выходной критерий этапа 8:
// внешний webhook проходит канонизацию, создаёт Opportunity, долговечную
// проверку и Risk без AI; ответ бизнеса закрывает тот же Risk автоматически.
func TestNoResponseRiskRealMessageFlow(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "risk-flow@example.com", "Владелец риска")
	tenantID := createOrganization(t, fixture, owner, "Организация риска")

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

	var riskID, severity, status, source, policyVersion string
	var riskCount int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT min(id::text), min(severity), min(status), min(source), min(risk_engine_version), count(*)
		FROM risk_signals
		WHERE tenant_id = $1 AND type = 'NO_RESPONSE'`, tenantID).Scan(
		&riskID, &severity, &status, &source, &policyVersion, &riskCount,
	); err != nil {
		t.Fatal(err)
	}
	if riskCount != 1 || severity != "HIGH" || status != "OPEN" || source != "RULE" || policyVersion != "no-response/v1" {
		t.Fatalf("риск: id=%s count=%d severity=%s status=%s source=%s policy=%s", riskID, riskCount, severity, status, source, policyVersion)
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

	outsider := register(t, fixture.handler, "risk-outsider@example.com", "Посторонний")
	forbidden := request(t, fixture.handler, http.MethodGet, "/api/v1/risks/"+riskID, "", outsider.Cookie, tenantID)
	requireStatus(t, forbidden, http.StatusForbidden)
	acknowledged := request(
		t, fixture.handler, http.MethodPost, "/api/v1/risks/"+riskID+"/acknowledge", "", owner.Cookie, tenantID,
	)
	requireStatus(t, acknowledged, http.StatusOK)
	if !strings.Contains(acknowledged.Body.String(), `"status":"ACKNOWLEDGED"`) {
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
