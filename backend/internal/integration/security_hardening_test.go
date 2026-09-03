package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/tenant/domain"
	"lidradar/backend/platform/tenantctx"
)

type riskScenario struct {
	riskID, opportunityID, conversationID, connectionID, secret, webhookPath string
}

// provisionNoResponseRisk проводит организацию через точку, услугу, канал и
// входящее сообщение часовой давности до открытого риска NO_RESPONSE.
func provisionNoResponseRisk(t *testing.T, fixture apiFixture, owner registeredUser, tenantID, label string) riskScenario {
	t.Helper()
	locationResponse := request(t, fixture.handler, http.MethodPost, "/api/v1/locations", `{
		"name":"Точка `+label+`","timezone":"UTC","responseThresholdMinutes":45
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
	secret := "secret-" + label + "-1234567890"
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{
		"name":"Канал `+label+`","locationId":"`+locationID+`","webhookSecret":"`+secret+`"
	}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	connectionID := jsonID(t, connected)
	path := "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenantID + "/" + connectionID
	incomingAt := time.Now().UTC().Add(-60 * time.Minute).Format(time.RFC3339Nano)
	incoming := canonicalWebhook(
		label+"-event-incoming", "message.received.v1", label+"-dialog", label+"-message-incoming", label+"-contact",
		"INCOMING", "TEXT", "Нужна полировка", incomingAt, "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, path, incoming, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	processExactly(t, fixture, 3)
	if promoted, err := fixture.scheduler.RunOnce(context.Background(), 100); err != nil || promoted < 1 {
		t.Fatalf("перенос проверки R1 = %d, %v", promoted, err)
	}
	processExactly(t, fixture, 1)
	scenario := riskScenario{connectionID: connectionID, secret: secret, webhookPath: path}
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT risk.id::text, risk.opportunity_id::text, opportunity.conversation_id::text
		FROM risk_signals AS risk
		JOIN opportunities AS opportunity ON opportunity.tenant_id = risk.tenant_id AND opportunity.id = risk.opportunity_id
		WHERE risk.tenant_id = $1 AND risk.type = 'NO_RESPONSE'`, tenantID).Scan(&scenario.riskID, &scenario.opportunityID, &scenario.conversationID); err != nil {
		t.Fatal(err)
	}
	return scenario
}

func auditCount(t *testing.T, fixture apiFixture, tenantID, operation string) int {
	t.Helper()
	var count int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND operation = $2`, tenantID, operation).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// TestCriticalActionsAreAuditedAndResponsesCarrySecurityHeaders доказывает
// LR-BE-2405/2406: каждое критическое действие §65 оставляет запись аудита, а
// ответы API несут заголовки безопасности и cookie с безопасными признаками.
func TestCriticalActionsAreAuditedAndResponsesCarrySecurityHeaders(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "audit-owner@example.com", "Владелец аудита")
	tenantID := createOrganization(t, fixture, owner, "Организация аудита")
	manager := register(t, fixture.handler, "audit-manager@example.com", "Менеджер аудита")

	login := request(t, fixture.handler, http.MethodPost, "/api/v1/auth/login", `{"email":"audit-owner@example.com","password":"very-secure-password"}`, nil, "")
	if login.Code != http.StatusOK {
		t.Fatalf("вход = %d %s", login.Code, login.Body.String())
	}
	if header := login.Header(); header.Get("X-Content-Type-Options") != "nosniff" || header.Get("Cache-Control") != "no-store" ||
		header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("заголовки безопасности: %v", header)
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/" {
		t.Fatalf("cookie входа: %+v", cookies)
	}
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/auth/logout", "", cookies[0], ""), http.StatusNoContent)
	var registered, loggedIn, loggedOut int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE operation = 'USER_REGISTERED'), count(*) FILTER (WHERE operation = 'USER_LOGGED_IN'),
		       count(*) FILTER (WHERE operation = 'USER_LOGGED_OUT')
		FROM auth_audit_log WHERE user_id = $1`, owner.ID).Scan(&registered, &loggedIn, &loggedOut); err != nil {
		t.Fatal(err)
	}
	if registered != 1 || loggedIn != 1 || loggedOut != 1 {
		t.Fatalf("аудит входа: registered=%d in=%d out=%d", registered, loggedIn, loggedOut)
	}

	requireStatus(t, request(t, fixture.handler, http.MethodPatch, "/api/v1/organization", `{"name":"Организация аудита 2"}`, owner.Cookie, tenantID), http.StatusOK)
	if _, err := fixture.tenantService.AddMember(tenantctx.WithTenant(context.Background(), tenantID), owner.ID, tenantID, manager.ID, domain.RoleManager); err != nil {
		t.Fatal(err)
	}
	scenario := provisionNoResponseRisk(t, fixture, owner, tenantID, "audit")
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/risks/"+scenario.riskID+"/acknowledge", "", manager.Cookie, tenantID), http.StatusOK)
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/risks/"+scenario.riskID+"/acknowledge", "", manager.Cookie, tenantID), http.StatusOK)
	requireStatus(t, request(t, fixture.handler, http.MethodPatch, "/api/v1/opportunities/"+scenario.opportunityID, `{"stage":"PRICE_SENT"}`, owner.Cookie, tenantID), http.StatusOK)
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/risks/"+scenario.riskID+"/resolve", "", owner.Cookie, tenantID), http.StatusOK)
	requireStatus(t, request(t, fixture.handler, http.MethodPut, "/api/v1/notifications/preferences/NO_RESPONSE",
		`{"minimumSeverity":"HIGH","deliveryMode":"DIGEST","inAppEnabled":true,"telegramEnabled":false,"quietHoursEnabled":false,"quietHoursStart":null,"quietHoursEnd":null,"digestTime":"10:00"}`,
		owner.Cookie, tenantID), http.StatusOK)
	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/notifications/preferences/NO_RESPONSE", "", owner.Cookie, tenantID), http.StatusNoContent)
	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/integrations/"+scenario.connectionID, "", owner.Cookie, tenantID), http.StatusNoContent)

	expected := map[string]int{
		"ORGANIZATION_UPDATED": 1, "MEMBER_ADDED": 1, "INTEGRATION_CONNECTED": 1, "INTEGRATION_DISCONNECTED": 1,
		"RISK_ACKNOWLEDGED": 1, "RISK_RESOLVED": 1, "OPPORTUNITY_STAGE_CHANGED": 1,
		"NOTIFICATION_POLICY_CHANGED": 1, "NOTIFICATION_POLICY_RESET": 1,
	}
	for operation, want := range expected {
		if got := auditCount(t, fixture, tenantID, operation); got != want {
			t.Fatalf("аудит %s = %d, ожидалось %d", operation, got, want)
		}
	}
	var actors int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(DISTINCT actor_user_id) FROM audit_log WHERE tenant_id = $1 AND operation IN ('RISK_ACKNOWLEDGED', 'RISK_RESOLVED')`, tenantID).Scan(&actors); err != nil {
		t.Fatal(err)
	}
	if actors != 2 {
		t.Fatalf("аудит различает акторов: %d", actors)
	}
	if body := request(t, fixture.handler, http.MethodGet, "/api/v1/auth/me", "", owner.Cookie, "").Body.String(); !strings.Contains(body, `"tenantId":"`+tenantID+`"`) {
		t.Fatalf("список организаций под RLS пуст: %s", body)
	}
}

// TestCrossTenantIdentifiersAreRejectedUnderRLS доказывает LR-BE-2417 при
// включённом RLS: владелец организации A, зная идентификаторы организации B,
// не читает и не меняет её данные ни по своему заголовку, ни по чужому.
func TestCrossTenantIdentifiersAreRejectedUnderRLS(t *testing.T) {
	fixture := newAPIFixture(t)
	ownerA := register(t, fixture.handler, "attacker-owner@example.com", "Владелец A")
	tenantA := createOrganization(t, fixture, ownerA, "Организация A")
	ownerB := register(t, fixture.handler, "victim-owner@example.com", "Владелец B")
	tenantB := createOrganization(t, fixture, ownerB, "Организация B")
	victim := provisionNoResponseRisk(t, fixture, ownerB, tenantB, "victim")
	provisionNoResponseRisk(t, fixture, ownerA, tenantA, "attacker")

	notFound := func(name, method, path, body string) {
		t.Helper()
		response := request(t, fixture.handler, method, path, body, ownerA.Cookie, tenantA)
		if response.Code != http.StatusNotFound && response.Code != http.StatusForbidden {
			t.Fatalf("%s: чужой идентификатор дал %d %s", name, response.Code, response.Body.String())
		}
	}
	notFound("риск", http.MethodGet, "/api/v1/risks/"+victim.riskID, "")
	notFound("подтверждение риска", http.MethodPost, "/api/v1/risks/"+victim.riskID+"/acknowledge", "")
	notFound("закрытие риска", http.MethodPost, "/api/v1/risks/"+victim.riskID+"/resolve", "")
	notFound("обратная связь", http.MethodPost, "/api/v1/risks/"+victim.riskID+"/feedback", `{"verdict":"TRUE_POSITIVE"}`)
	notFound("рекомендация", http.MethodPost, "/api/v1/risks/"+victim.riskID+"/recommendation", "")
	notFound("сделка", http.MethodGet, "/api/v1/opportunities/"+victim.opportunityID, "")
	notFound("этап сделки", http.MethodPatch, "/api/v1/opportunities/"+victim.opportunityID, `{"stage":"PRICE_SENT"}`)
	notFound("переписка", http.MethodGet, "/api/v1/conversations/"+victim.conversationID, "")
	notFound("сообщения", http.MethodGet, "/api/v1/conversations/"+victim.conversationID+"/messages", "")
	notFound("здоровье канала", http.MethodGet, "/api/v1/integrations/"+victim.connectionID+"/health", "")
	notFound("отключение канала", http.MethodDelete, "/api/v1/integrations/"+victim.connectionID, "")
	if response := idempotentRequest(t, fixture.handler, http.MethodPost, "/api/v1/opportunities/"+victim.opportunityID+"/outcomes",
		`{"status":"LOST"}`, ownerA.Cookie, tenantA, "attack-outcome"); response.Code != http.StatusNotFound && response.Code != http.StatusForbidden {
		t.Fatalf("исход чужой сделки дал %d %s", response.Code, response.Body.String())
	}
	if response := idempotentRequest(t, fixture.handler, http.MethodPost, "/api/v1/risks/"+victim.riskID+"/actions",
		`{"type":"CALL"}`, ownerA.Cookie, tenantA, "attack-action"); response.Code != http.StatusNotFound && response.Code != http.StatusForbidden {
		t.Fatalf("действие по чужому риску дало %d %s", response.Code, response.Body.String())
	}
	for _, path := range []string{"/api/v1/risks", "/api/v1/radar", "/api/v1/analytics/summary", "/api/v1/notifications/preferences", "/api/v1/integrations", "/api/v1/conversations", "/api/v1/organization"} {
		if response := request(t, fixture.handler, http.MethodGet, path, "", ownerA.Cookie, tenantB); response.Code != http.StatusForbidden {
			t.Fatalf("%s с чужим заголовком организации дал %d %s", path, response.Code, response.Body.String())
		}
	}
	list := request(t, fixture.handler, http.MethodGet, "/api/v1/risks", "", ownerA.Cookie, tenantA)
	requireStatus(t, list, http.StatusOK)
	if strings.Contains(list.Body.String(), victim.riskID) {
		t.Fatal("Radar организации A содержит риск организации B")
	}
	forged := canonicalWebhook("forged-event", "message.received.v1", "victim-dialog", "forged-message", "victim-contact",
		"INCOMING", "TEXT", "Взлом", time.Now().UTC().Format(time.RFC3339Nano), "")
	if response := webhookRequest(t, fixture.handler, victim.webhookPath, forged, "X-LidRadar-Webhook-Secret", "secret-attacker-1234567890"); response.Code != http.StatusUnauthorized {
		t.Fatalf("вебхук с чужим секретом дал %d", response.Code)
	}
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/admin/organizations", "", ownerA.Cookie, ""), http.StatusForbidden)
	var leaked int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND actor_user_id = $2`, tenantB, ownerA.ID).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("организация B получила аудит от чужого пользователя: %d", leaked)
	}
}
