package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

type analyticsSummary struct {
	Period struct {
		FromDate string `json:"fromDate"`
		ToDate   string `json:"toDate"`
		Timezone string `json:"timezone"`
	} `json:"period"`
	Messages struct {
		Total, Incoming, Outgoing, Conversations int
	} `json:"messages"`
	Opportunities struct {
		Created, Booked, Won, Lost int
	} `json:"opportunities"`
	Risks struct {
		Detected, Acted, Resolved int
		FalsePositive             int `json:"falsePositive"`
		ByType                    []struct {
			RiskType string `json:"riskType"`
			Detected int    `json:"detected"`
			Acted    int    `json:"acted"`
		} `json:"byType"`
	} `json:"risks"`
	Outcomes struct {
		Booked, Paid, Lost int
	} `json:"outcomes"`
	Revenue struct {
		Currency           string `json:"currency"`
		Potential          string `json:"potential"`
		Confirmed          string `json:"confirmed"`
		ConfirmedRecovered string `json:"confirmedRecovered"`
		ConfirmedPayments  int    `json:"confirmedPayments"`
	} `json:"revenue"`
}

func fetchAnalytics(t *testing.T, fixture apiFixture, cookie *http.Cookie, tenantID, query string) analyticsSummary {
	t.Helper()
	response := request(t, fixture.handler, http.MethodGet, "/api/v1/analytics/summary"+query, "", cookie, tenantID)
	requireStatus(t, response, http.StatusOK)
	var summary analyticsSummary
	if err := json.Unmarshal(response.Body.Bytes(), &summary); err != nil {
		t.Fatalf("сводка аналитики: %s, %v", response.Body.String(), err)
	}
	return summary
}

// TestAnalyticsSummaryMatchesRawDomainData доказывает выходной критерий этапа
// 22 на PostgreSQL: после полного денежного контура сводка показывает реальные
// сообщения, сделки, риски, исходы и деньги, совпадающие с Radar и с показателем
// возвращённой выручки; окно считается в часовом поясе организации.
func TestAnalyticsSummaryMatchesRawDomainData(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "analytics-owner@example.com", "Владелец аналитики")
	tenantID := createOrganization(t, fixture, owner, "Организация аналитики")
	stranger := register(t, fixture.handler, "analytics-stranger@example.com", "Посторонний")

	locationResponse := request(t, fixture.handler, http.MethodPost, "/api/v1/locations", `{
		"name":"Точка аналитики","timezone":"UTC","responseThresholdMinutes":45
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
	secret := "analytics-flow-secret-123"
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{
		"name":"Канал аналитики","locationId":"`+locationID+`","webhookSecret":"`+secret+`"
	}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	path := "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenantID + "/" + jsonID(t, connected)
	incomingAt := time.Now().UTC().Add(-60 * time.Minute).Format(time.RFC3339Nano)
	incoming := canonicalWebhook(
		"analytics-event-incoming", "message.received.v1", "analytics-dialog", "analytics-message-incoming", "analytics-contact",
		"INCOMING", "TEXT", "Нужна полировка", incomingAt, "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, incoming, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	processExactly(t, fixture, 3)
	if promoted, err := fixture.scheduler.RunOnce(context.Background(), 100); err != nil || promoted < 1 {
		t.Fatalf("перенос проверки R1 = %d, %v", promoted, err)
	}
	processExactly(t, fixture, 1)
	var riskID, opportunityID string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT id::text, opportunity_id::text FROM risk_signals WHERE tenant_id = $1 AND type = 'NO_RESPONSE'`, tenantID).Scan(&riskID, &opportunityID); err != nil {
		t.Fatal(err)
	}

	before := fetchAnalytics(t, fixture, owner.Cookie, tenantID, "")
	if before.Period.Timezone != "Europe/Moscow" || before.Messages.Incoming != 1 || before.Messages.Conversations != 1 ||
		before.Opportunities.Created != 1 || before.Risks.Detected != 1 || before.Risks.Acted != 0 ||
		before.Revenue.Potential != "5000.00" || before.Revenue.Confirmed != "0.00" || before.Revenue.ConfirmedPayments != 0 {
		t.Fatalf("сводка до действий = %+v", before)
	}

	// Полный денежный контур: рекомендация → действие → исходы → подтверждённая выручка RECOVERED.
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/risks/"+riskID+"/recommendation", "", owner.Cookie, tenantID), http.StatusOK)
	action := idempotentRequest(t, fixture.handler, http.MethodPost, "/api/v1/risks/"+riskID+"/actions",
		`{"type":"MARK_CONTACTED","note":"Связались с клиентом"}`, owner.Cookie, tenantID, "analytics-action")
	requireStatus(t, action, http.StatusCreated)
	actionID := jsonID(t, action)
	requireStatus(t, idempotentRequest(t, fixture.handler, http.MethodPost, "/api/v1/opportunities/"+opportunityID+"/outcomes",
		`{"status":"BOOKED","note":"Запись подтверждена"}`, owner.Cookie, tenantID, "analytics-booked"), http.StatusCreated)
	paid := idempotentRequest(t, fixture.handler, http.MethodPost, "/api/v1/opportunities/"+opportunityID+"/outcomes",
		`{"status":"PAID","note":"Оплата подтверждена"}`, owner.Cookie, tenantID, "analytics-paid")
	requireStatus(t, paid, http.StatusCreated)
	revenue := idempotentRequest(t, fixture.handler, http.MethodPost, "/api/v1/opportunities/"+opportunityID+"/revenue",
		`{"amount":"47000","currency":"RUB","attributionType":"RECOVERED","riskId":"`+riskID+`","actionId":"`+actionID+`","outcomeId":"`+jsonID(t, paid)+`"}`,
		owner.Cookie, tenantID, "analytics-revenue")
	requireStatus(t, revenue, http.StatusCreated)

	after := fetchAnalytics(t, fixture, owner.Cookie, tenantID, "")
	if after.Risks.Detected != 1 || after.Risks.Acted != 1 || len(after.Risks.ByType) != 5 || after.Risks.ByType[0].RiskType != "NO_RESPONSE" ||
		after.Risks.ByType[0].Acted != 1 || after.Outcomes.Booked != 1 || after.Outcomes.Paid != 1 || after.Outcomes.Lost != 0 ||
		after.Revenue.Currency != "RUB" || after.Revenue.Potential != "5000.00" || after.Revenue.Confirmed != "47000.00" ||
		after.Revenue.ConfirmedRecovered != "47000.00" || after.Revenue.ConfirmedPayments != 1 || after.Opportunities.Created != 1 {
		t.Fatalf("сводка после денежного контура = %+v", after)
	}
	radar := request(t, fixture.handler, http.MethodGet, "/api/v1/radar", "", owner.Cookie, tenantID)
	requireStatus(t, radar, http.StatusOK)
	recovered := request(t, fixture.handler, http.MethodGet, "/api/v1/revenue/confirmed-recovered?currency=RUB", "", owner.Cookie, tenantID)
	requireStatus(t, recovered, http.StatusOK)
	if !strings.Contains(radar.Body.String(), `"confirmedRecoveredRevenue":"`+after.Revenue.ConfirmedRecovered+`"`) ||
		!strings.Contains(recovered.Body.String(), `"amount":"`+after.Revenue.ConfirmedRecovered+`"`) {
		t.Fatalf("аналитика расходится с Radar (%s) и показателем выручки (%s)", radar.Body.String(), recovered.Body.String())
	}

	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/analytics/summary", "", stranger.Cookie, tenantID), http.StatusForbidden)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/analytics/summary?from=2026-09-10&to=2026-09-01", "", owner.Cookie, tenantID), http.StatusBadRequest)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/analytics/summary?from=вчера", "", owner.Cookie, tenantID), http.StatusBadRequest)
	location, _ := time.LoadLocation("Europe/Moscow")
	futureFrom := time.Now().In(location).AddDate(0, 0, 2).Format("2006-01-02")
	futureTo := time.Now().In(location).AddDate(0, 0, 3).Format("2006-01-02")
	future := fetchAnalytics(t, fixture, owner.Cookie, tenantID, "?from="+futureFrom+"&to="+futureTo)
	if future.Period.FromDate != futureFrom || future.Period.ToDate != futureTo || future.Messages.Total != 0 || future.Opportunities.Created != 0 ||
		future.Risks.Detected != 0 || future.Revenue.Confirmed != "0.00" || future.Revenue.Potential != "0.00" {
		t.Fatalf("окно в будущем = %+v", future)
	}
}
