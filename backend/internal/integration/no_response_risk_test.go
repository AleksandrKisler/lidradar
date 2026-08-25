package integration_test

import (
	"context"
	"net/http"
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
}
