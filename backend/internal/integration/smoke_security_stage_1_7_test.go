package integration_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lidradar/backend/platform/ids"
)

func TestSmokeStagesOneThroughSevenRejectsHostileInputWithoutMutation(t *testing.T) {
	fixture := newAPIFixture(t)
	for _, path := range []string{"/health/live", "/health/ready"} {
		requireStatus(t, request(t, fixture.handler, http.MethodGet, path, "", nil, ""), http.StatusOK)
	}
	owner := register(t, fixture.handler, "smoke-security@example.com", "Проверка безопасности")
	tenantID := createOrganization(t, fixture, owner, "Организация проверки")
	locationID := createLocation(t, fixture, owner, tenantID, "Точка проверки")

	invalidTenant := httptest.NewRequest(http.MethodGet, "http://api.example/api/v1/services", nil)
	invalidTenant.AddCookie(owner.Cookie)
	invalidTenant.Header.Set("X-Tenant-ID", `' OR 1=1; DROP TABLE organizations; --`)
	invalidTenantResponse := httptest.NewRecorder()
	fixture.handler.ServeHTTP(invalidTenantResponse, invalidTenant)
	requireStatus(t, invalidTenantResponse, http.StatusBadRequest)

	invalidOrigin := httptest.NewRequest(http.MethodPost, "http://api.example/api/v1/services", strings.NewReader(`{"name":"Атака"}`))
	invalidOrigin.AddCookie(owner.Cookie)
	invalidOrigin.Header.Set("X-Tenant-ID", tenantID)
	invalidOrigin.Header.Set("Origin", "https://evil.example")
	invalidOriginResponse := httptest.NewRecorder()
	fixture.handler.ServeHTTP(invalidOriginResponse, invalidOrigin)
	requireStatus(t, invalidOriginResponse, http.StatusForbidden)

	for name, body := range map[string][]byte{
		"несколько JSON":         []byte(`{"name":"Услуга"}{"name":"Вторая"}`),
		"неизвестное поле":       []byte(`{"name":"Услуга","admin":true}`),
		"нулевой байт":           []byte(`{"name":"Услуга\u0000атака"}`),
		"неверный UTF-8":         append([]byte(`{"name":"`), 0xff, '"', '}'),
		"слишком большой запрос": []byte(`{"name":"` + strings.Repeat("x", 70<<10) + `"}`),
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://api.example/api/v1/services", bytes.NewReader(body))
			request.AddCookie(owner.Cookie)
			request.Header.Set("X-Tenant-ID", tenantID)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			fixture.handler.ServeHTTP(response, request)
			requireStatus(t, response, http.StatusBadRequest)
		})
	}
	var serviceCount int
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM service_catalog_items WHERE tenant_id = $1`, tenantID).Scan(&serviceCount); err != nil {
		t.Fatal(err)
	}
	if serviceCount != 0 {
		t.Fatalf("враждебные запросы изменили каталог: %d", serviceCount)
	}

	secret := "smoke-security-secret-123"
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{
		"name":"Защищённый вход","locationId":"`+locationID+`","webhookSecret":"`+secret+`"
	}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	connectionID := jsonID(t, connected)
	webhookPath := "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenantID + "/" + connectionID

	maliciousPayload := `{"id":"hostile-event","type":"message.received.v1","occurredAt":"2026-08-25T14:00:00Z","data":{"conversationExternalId":"hostile-dialog","messageExternalId":"hostile-message","contactExternalId":"hostile-contact","direction":"INCOMING","messageType":"TEXT","text":"\u0000 DROP TABLE opportunities","sentAt":"2026-08-25T14:00:00Z","attachments":[],"metadata":{}}}`
	requireStatus(t, webhookRequest(t, fixture.handler, webhookPath, maliciousPayload, "X-LidRadar-Webhook-Secret", "wrong-secret-value"), http.StatusUnauthorized)
	var rawCount int
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM raw_events WHERE connection_id = $1`, connectionID).Scan(&rawCount); err != nil {
		t.Fatal(err)
	}
	if rawCount != 0 {
		t.Fatal("неверно подписанный запрос был сохранён")
	}

	oversized := `{"id":"large","padding":"` + strings.Repeat("x", (1<<20)+1) + `"}`
	requireStatus(t, webhookRequest(t, fixture.handler, webhookPath, oversized, "X-LidRadar-Webhook-Secret", secret), http.StatusRequestEntityTooLarge)

	requireStatus(t, webhookRequest(t, fixture.handler, webhookPath, `{{`, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	requireStatus(t, webhookRequest(t, fixture.handler, webhookPath, maliciousPayload, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	processExactly(t, fixture, 1)
	var failedRaw, conversations, opportunities int
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM raw_events WHERE connection_id = $1 AND status = 'FAILED'`, connectionID).Scan(&failedRaw); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM conversations WHERE tenant_id = $1`, tenantID).Scan(&conversations); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM opportunities WHERE tenant_id = $1`, tenantID).Scan(&opportunities); err != nil {
		t.Fatal(err)
	}
	if failedRaw != 2 || conversations != 0 || opportunities != 0 {
		t.Fatalf("failedRaw=%d, conversations=%d, opportunities=%d", failedRaw, conversations, opportunities)
	}

	validUnknownID, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/opportunities/"+validUnknownID, "", owner.Cookie, tenantID), http.StatusNotFound)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/opportunities/'%20OR%201=1", "", owner.Cookie, tenantID), http.StatusBadRequest)

	requestID := httptest.NewRequest(http.MethodGet, "http://api.example/health/live", nil)
	requestID.Header.Set("X-Request-ID", "<script>alert(1)</script>")
	requestIDResponse := httptest.NewRecorder()
	fixture.handler.ServeHTTP(requestIDResponse, requestID)
	requireStatus(t, requestIDResponse, http.StatusOK)
	if requestIDResponse.Header().Get("X-Request-ID") == "<script>alert(1)</script>" {
		t.Fatal("опасный идентификатор запроса был отражён в ответ")
	}
}
