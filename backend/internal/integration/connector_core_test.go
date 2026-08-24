package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	connectordomain "lidradar/backend/internal/connector/domain"
	"lidradar/backend/internal/tenant/domain"
)

func TestConnectorCoreManagementPersistFirstDeduplicationAndIsolation(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "connector-owner@example.com", "Connector Owner")
	tenantID := createOrganization(t, fixture, owner, "Connector tenant")
	locationID := createLocation(t, fixture, owner, tenantID, "Connector location")

	manager := register(t, fixture.handler, "connector-manager@example.com", "Connector Manager")
	if _, err := fixture.tenantService.AddMember(context.Background(), owner.ID, tenantID, manager.ID, domain.RoleManager); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/integrations", "", manager.Cookie, tenantID), http.StatusForbidden)
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/TEST/connect", `{
		"name":"Forbidden","webhookSecret":"manager-secret-123"
	}`, manager.Cookie, tenantID), http.StatusForbidden)

	secret := "generic-fixture-secret-123"
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{
		"name":"Public website","locationId":"`+locationID+`","webhookSecret":"`+secret+`"
	}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	connectionID := jsonID(t, connected)
	if strings.Contains(connected.Body.String(), secret) || !strings.Contains(connected.Body.String(), `"status":"ACTIVE"`) {
		t.Fatalf("connection response exposed a secret or wrong status: %s", connected.Body.String())
	}

	listed := request(t, fixture.handler, http.MethodGet, "/api/v1/integrations", "", owner.Cookie, tenantID)
	requireStatus(t, listed, http.StatusOK)
	if !strings.Contains(listed.Body.String(), connectionID) || strings.Contains(listed.Body.String(), secret) {
		t.Fatalf("integration list = %s", listed.Body.String())
	}

	payload := `{"id":"website-event-1","type":"message.received.v1","occurredAt":"2026-08-25T10:00:00Z","data":{"text":"hello"}}`
	duplicateCount := 0
	var firstRawID string
	for attempt := 0; attempt < 10; attempt++ {
		response := webhookRequest(
			t, fixture.handler, "/api/v1/webhooks/GENERIC_WEBHOOK/"+tenantID+"/"+connectionID,
			payload, "X-LidRadar-Webhook-Secret", secret,
		)
		requireStatus(t, response, http.StatusAccepted)
		var receipt struct {
			RawEventID string                         `json:"rawEventId"`
			Status     connectordomain.RawEventStatus `json:"status"`
			Duplicate  bool                           `json:"duplicate"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.Status != connectordomain.RawEventReceived || receipt.RawEventID == "" {
			t.Fatalf("receipt = %#v", receipt)
		}
		if attempt == 0 {
			firstRawID = receipt.RawEventID
		} else if receipt.RawEventID != firstRawID {
			t.Fatalf("duplicate returned raw ID %s, want %s", receipt.RawEventID, firstRawID)
		}
		if receipt.Duplicate {
			duplicateCount++
		}
	}
	if duplicateCount != 9 {
		t.Fatalf("duplicate receipts = %d, want 9", duplicateCount)
	}
	requireEventCounts(t, fixture, connectionID, 1, 1)

	badSecret := webhookRequest(
		t, fixture.handler, "/api/v1/webhooks/GENERIC_WEBHOOK/"+tenantID+"/"+connectionID,
		payload, "X-LidRadar-Webhook-Secret", "wrong-secret-value",
	)
	requireStatus(t, badSecret, http.StatusUnauthorized)
	requireEventCounts(t, fixture, connectionID, 1, 1)

	invalid := webhookRequest(
		t, fixture.handler, "/api/v1/webhooks/GENERIC_WEBHOOK/"+tenantID+"/"+connectionID,
		"not-json", "X-LidRadar-Webhook-Secret", secret,
	)
	requireStatus(t, invalid, http.StatusAccepted)
	if !strings.Contains(invalid.Body.String(), `"status":"FAILED"`) {
		t.Fatalf("invalid payload receipt = %s", invalid.Body.String())
	}
	requireEventCounts(t, fixture, connectionID, 2, 1)
	invalidDuplicate := webhookRequest(
		t, fixture.handler, "/api/v1/webhooks/GENERIC_WEBHOOK/"+tenantID+"/"+connectionID,
		"not-json", "X-LidRadar-Webhook-Secret", secret,
	)
	requireStatus(t, invalidDuplicate, http.StatusAccepted)
	if !strings.Contains(invalidDuplicate.Body.String(), `"status":"FAILED"`) ||
		!strings.Contains(invalidDuplicate.Body.String(), `"duplicate":true`) {
		t.Fatalf("invalid duplicate receipt = %s", invalidDuplicate.Body.String())
	}
	requireEventCounts(t, fixture, connectionID, 2, 1)
	health := request(t, fixture.handler, http.MethodGet, "/api/v1/integrations/"+connectionID+"/health", "", owner.Cookie, tenantID)
	requireStatus(t, health, http.StatusOK)
	if !strings.Contains(health.Body.String(), `"status":"ERROR"`) || !strings.Contains(health.Body.String(), `"lastErrorCode":"INVALID_PAYLOAD"`) {
		t.Fatalf("health after invalid payload = %s", health.Body.String())
	}

	validAfterError := webhookRequest(
		t, fixture.handler, "/api/v1/webhooks/GENERIC_WEBHOOK/"+tenantID+"/"+connectionID,
		`{"id":"website-event-2","type":"message.received.v1","occurredAt":"2026-08-25T10:01:00Z","data":{}}`,
		"X-LidRadar-Webhook-Secret", secret,
	)
	requireStatus(t, validAfterError, http.StatusAccepted)
	requireEventCounts(t, fixture, connectionID, 3, 2)
	health = request(t, fixture.handler, http.MethodGet, "/api/v1/integrations/"+connectionID+"/health", "", owner.Cookie, tenantID)
	requireStatus(t, health, http.StatusOK)
	if !strings.Contains(health.Body.String(), `"status":"ACTIVE"`) || !strings.Contains(health.Body.String(), `"lastErrorCode":null`) {
		t.Fatalf("health after recovery = %s", health.Body.String())
	}

	telegram := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/CONNECTED_BUSINESS_BOT/connect", `{
		"name":"Telegram fixture","webhookSecret":"telegram_secret-123"
	}`, owner.Cookie, tenantID)
	requireStatus(t, telegram, http.StatusCreated)
	if !strings.Contains(telegram.Body.String(), `"status":"DEGRADED"`) ||
		!strings.Contains(telegram.Body.String(), `"lastErrorCode":"TELEGRAM_SPIKE_NOT_VERIFIED"`) {
		t.Fatalf("Telegram fixture connection = %s", telegram.Body.String())
	}

	ownerB := register(t, fixture.handler, "connector-owner-b@example.com", "Connector Owner B")
	tenantBID := createOrganization(t, fixture, ownerB, "Connector tenant B")
	connectionB := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/TEST/connect", `{
		"name":"Tenant B private connector","webhookSecret":"tenant-b-secret-123"
	}`, ownerB.Cookie, tenantBID)
	requireStatus(t, connectionB, http.StatusCreated)
	connectionBID := jsonID(t, connectionB)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/integrations/"+connectionBID+"/health", "", owner.Cookie, tenantID), http.StatusNotFound)
	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/integrations/"+connectionBID, "", owner.Cookie, tenantID), http.StatusNotFound)
	listed = request(t, fixture.handler, http.MethodGet, "/api/v1/integrations", "", owner.Cookie, tenantID)
	requireStatus(t, listed, http.StatusOK)
	if strings.Contains(listed.Body.String(), connectionBID) || strings.Contains(listed.Body.String(), "Tenant B private connector") {
		t.Fatalf("tenant list disclosed another tenant: %s", listed.Body.String())
	}

	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/integrations/"+connectionID, "", owner.Cookie, tenantID), http.StatusNoContent)
	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/integrations/"+connectionID, "", owner.Cookie, tenantID), http.StatusNoContent)
	health = request(t, fixture.handler, http.MethodGet, "/api/v1/integrations/"+connectionID+"/health", "", owner.Cookie, tenantID)
	requireStatus(t, health, http.StatusOK)
	if !strings.Contains(health.Body.String(), `"status":"DISCONNECTED"`) {
		t.Fatalf("health after disconnect = %s", health.Body.String())
	}
	requireStatus(t, webhookRequest(
		t, fixture.handler, "/api/v1/webhooks/GENERIC_WEBHOOK/"+tenantID+"/"+connectionID,
		`{"id":"after-disconnect","type":"message.received.v1","occurredAt":"2026-08-25T10:02:00Z","data":{}}`,
		"X-LidRadar-Webhook-Secret", secret,
	), http.StatusServiceUnavailable)
}

func webhookRequest(
	t *testing.T,
	handler http.Handler,
	path, body, secretHeader, secret string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://api.example"+path, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(secretHeader, secret)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func requireEventCounts(t *testing.T, fixture apiFixture, connectionID string, wantRaw, wantWork int) {
	t.Helper()
	var rawCount, workCount int
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM raw_events WHERE connection_id = $1`, connectionID).Scan(&rawCount); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM raw_event_normalization_work WHERE connection_id = $1`, connectionID).Scan(&workCount); err != nil {
		t.Fatal(err)
	}
	if rawCount != wantRaw || workCount != wantWork {
		t.Fatalf("event counts raw=%d, work=%d; want %d, %d", rawCount, workCount, wantRaw, wantWork)
	}
}
