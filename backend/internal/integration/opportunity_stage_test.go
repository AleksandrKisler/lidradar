package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"lidradar/backend/internal/tenant/domain"
)

func TestOpportunityStageSevenExitGateThroughRealMessageFlow(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "opportunity-owner@example.com", "Владелец продаж")
	tenantID := createOrganization(t, fixture, owner, "Организация возможностей")
	locationID := createLocation(t, fixture, owner, tenantID, "Основная точка")
	manager := register(t, fixture.handler, "opportunity-manager@example.com", "Менеджер продаж")
	if _, err := fixture.tenantService.AddMember(context.Background(), owner.ID, tenantID, manager.ID, domain.RoleManager); err != nil {
		t.Fatal(err)
	}

	service := request(t, fixture.handler, http.MethodPost, "/api/v1/services", `{
		"name":"Полировка кузова","locationId":"`+locationID+`",
		"priceFrom":"7500.00","priceTo":"7500.00","currency":"RUB"
	}`, owner.Cookie, tenantID)
	requireStatus(t, service, http.StatusCreated)
	serviceID := jsonID(t, service)

	secret := "opportunity-fixture-secret"
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{
		"name":"Форма коммерческих обращений","locationId":"`+locationID+`","webhookSecret":"`+secret+`"
	}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	connectionID := jsonID(t, connected)
	webhookPath := "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenantID + "/" + connectionID

	commercial := canonicalWebhook(
		"opportunity-event-1", "message.received.v1", "commercial-dialog", "commercial-message", "commercial-contact",
		"INCOMING", "TEXT", "Здравствуйте, нужна полировка кузова", "2026-08-25T12:00:00Z", "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, webhookPath, commercial, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	processExactly(t, fixture, 1)

	var opportunityID string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT id FROM opportunities WHERE tenant_id = $1`, tenantID).Scan(&opportunityID); err != nil {
		t.Fatal(err)
	}
	detail := request(t, fixture.handler, http.MethodGet, "/api/v1/opportunities/"+opportunityID, "", manager.Cookie, tenantID)
	requireStatus(t, detail, http.StatusOK)
	if !strings.Contains(detail.Body.String(), `"stage":"NEW"`) ||
		!strings.Contains(detail.Body.String(), `"serviceId":"`+serviceID+`"`) ||
		!strings.Contains(detail.Body.String(), `"estimatedAmount":"7500.00"`) ||
		!strings.Contains(detail.Body.String(), `"estimatedAmountConfidence":1.000`) ||
		!strings.Contains(detail.Body.String(), `"source":"RULE"`) {
		t.Fatalf("детали возможности = %s", detail.Body.String())
	}

	changeStage := func(stage string, want int) {
		t.Helper()
		response := request(t, fixture.handler, http.MethodPatch, "/api/v1/opportunities/"+opportunityID,
			`{"stage":"`+stage+`"}`, manager.Cookie, tenantID)
		requireStatus(t, response, want)
	}
	changeStage("PRICE_SENT", http.StatusOK)
	changeStage("PRICE_SENT", http.StatusOK)
	changeStage("ENGAGED", http.StatusConflict)
	changeStage("WON", http.StatusConflict)
	changeStage("BOOKED", http.StatusOK)
	changeStage("WON", http.StatusOK)
	changeStage("ARCHIVED", http.StatusOK)

	detail = request(t, fixture.handler, http.MethodGet, "/api/v1/opportunities/"+opportunityID, "", owner.Cookie, tenantID)
	requireStatus(t, detail, http.StatusOK)
	if strings.Count(detail.Body.String(), `"source":"USER"`) != 4 ||
		!strings.Contains(detail.Body.String(), `"stage":"ARCHIVED"`) ||
		!strings.Contains(detail.Body.String(), `"closedAt":"`) {
		t.Fatalf("история после переходов = %s", detail.Body.String())
	}

	notLead := canonicalWebhook(
		"opportunity-event-2", "message.received.v1", "service-dialog", "service-message", "service-contact",
		"INCOMING", "TEXT", "Подскажите, где скачать старый чек?", "2026-08-25T12:10:00Z", "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, webhookPath, notLead, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	processExactly(t, fixture, 1)
	var count int
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM opportunities WHERE tenant_id = $1`, tenantID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("некоммерческое обращение создало возможность: count=%d", count)
	}

	for _, body := range []string{
		`{"stage":"NEW","unknown":true}`,
		`{"stage":"NEW"}{"stage":"LOST"}`,
		`{"stage":"' OR 1=1; --"}`,
		`{"stage":123}`,
	} {
		response := request(t, fixture.handler, http.MethodPatch, "/api/v1/opportunities/"+opportunityID, body, manager.Cookie, tenantID)
		requireStatus(t, response, http.StatusBadRequest)
	}

	ownerB := register(t, fixture.handler, "opportunity-owner-b@example.com", "Владелец B")
	tenantBID := createOrganization(t, fixture, ownerB, "Организация B")
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/opportunities/"+opportunityID, "", ownerB.Cookie, tenantBID), http.StatusNotFound)
	requireStatus(t, request(t, fixture.handler, http.MethodPatch, "/api/v1/opportunities/"+opportunityID, `{"stage":"LOST"}`, ownerB.Cookie, tenantBID), http.StatusNotFound)
}
