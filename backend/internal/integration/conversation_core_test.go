package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"lidradar/backend/internal/tenant/domain"
)

func TestConversationCoreExitGateThroughWebhookWorkerAndReadAPI(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "conversation-owner@example.com", "Владелец переписок")
	tenantID := createOrganization(t, fixture, owner, "Организация переписок")
	locationID := createLocation(t, fixture, owner, tenantID, "Основная точка")
	manager := register(t, fixture.handler, "conversation-manager@example.com", "Менеджер переписок")
	if _, err := fixture.tenantService.AddMember(context.Background(), owner.ID, tenantID, manager.ID, domain.RoleManager); err != nil {
		t.Fatal(err)
	}

	secret := "conversation-fixture-secret"
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{
		"name":"Форма сайта","locationId":"`+locationID+`","webhookSecret":"`+secret+`"
	}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	connectionID := jsonID(t, connected)
	webhookPath := "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenantID + "/" + connectionID

	first := canonicalWebhook(
		"event-1", "message.received.v1", "dialog-1", "message-1", "contact-1",
		"INCOMING", "TEXT", "Здравствуйте", "2026-08-25T10:00:00Z", "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, webhookPath, first, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	processExactly(t, fixture, 1)
	requireCanonicalCounts(t, fixture, tenantID, 1, 1, 1, 1, 0)
	requireRawState(t, fixture, connectionID, 1, "PROCESSED", 0)

	list := request(t, fixture.handler, http.MethodGet, "/api/v1/conversations", "", owner.Cookie, tenantID)
	requireStatus(t, list, http.StatusOK)
	conversationID, revision := conversationFromList(t, list.Body.Bytes())
	if revision != 1 {
		t.Fatalf("revision первого сообщения = %d, нужно 1", revision)
	}
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/conversations", "", manager.Cookie, tenantID), http.StatusOK)

	second := canonicalWebhook(
		"event-2", "message.received.v1", "dialog-1", "message-2", "contact-1",
		"OUTGOING", "TEXT", "Добрый день", "2026-08-25T10:01:00Z", "message-1",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, webhookPath, second, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	processExactly(t, fixture, 1)
	requireCanonicalCounts(t, fixture, tenantID, 1, 1, 1, 2, 0)
	requireConversationRevision(t, fixture, tenantID, conversationID, 2)

	edited := canonicalWebhook(
		"event-3", "message.edited.v1", "dialog-1", "message-2", "",
		"OUTGOING", "TEXT", "Добрый день!", "2026-08-25T10:01:00Z", "message-1",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, webhookPath, edited, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	processExactly(t, fixture, 1)
	requireConversationRevision(t, fixture, tenantID, conversationID, 3)

	editedReplay := strings.Replace(edited, `"id":"event-3"`, `"id":"event-3-replay"`, 1)
	requireStatus(t, webhookRequest(t, fixture.handler, webhookPath, editedReplay, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	processExactly(t, fixture, 1)
	requireConversationRevision(t, fixture, tenantID, conversationID, 3)

	deleted := canonicalWebhook(
		"event-4", "message.deleted.v1", "dialog-1", "message-1", "",
		"", "", "", "", "",
	)
	requireStatus(t, webhookRequest(t, fixture.handler, webhookPath, deleted, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	processExactly(t, fixture, 1)
	requireConversationRevision(t, fixture, tenantID, conversationID, 4)

	attachment := `{
		"id":"event-5","type":"message.received.v1","occurredAt":"2026-08-25T10:05:00Z",
		"data":{"conversationExternalId":"dialog-1","messageExternalId":"message-3","contactExternalId":"contact-1",
		"direction":"INCOMING","messageType":"DOCUMENT","text":"Смета","sentAt":"2026-08-25T10:05:00Z",
		"attachments":[{"objectKey":"fixtures/estimate.pdf","mimeType":"application/pdf","sizeBytes":2048,
		"sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","providerFileId":"file-1"}],"metadata":{}}
	}`
	requireStatus(t, webhookRequest(t, fixture.handler, webhookPath, attachment, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	processExactly(t, fixture, 1)
	requireCanonicalCounts(t, fixture, tenantID, 1, 1, 1, 3, 1)
	requireConversationRevision(t, fixture, tenantID, conversationID, 5)

	invalidCanonical := `{"id":"event-invalid","type":"message.received.v1","occurredAt":"2026-08-25T10:06:00Z","data":{"text":"нет обязательных полей"}}`
	requireStatus(t, webhookRequest(t, fixture.handler, webhookPath, invalidCanonical, "X-LidRadar-Webhook-Secret", secret), http.StatusAccepted)
	processExactly(t, fixture, 1)
	requireCanonicalCounts(t, fixture, tenantID, 1, 1, 1, 3, 1)
	requireRawState(t, fixture, connectionID, 7, "FAILED", 0)

	detail := request(t, fixture.handler, http.MethodGet, "/api/v1/conversations/"+conversationID, "", owner.Cookie, tenantID)
	requireStatus(t, detail, http.StatusOK)
	if !strings.Contains(detail.Body.String(), `"displayName":"Ирина"`) || strings.Contains(detail.Body.String(), "GENERIC_WEBHOOK") {
		t.Fatalf("детали переписки = %s", detail.Body.String())
	}
	messages := request(t, fixture.handler, http.MethodGet, "/api/v1/conversations/"+conversationID+"/messages?limit=2", "", manager.Cookie, tenantID)
	requireStatus(t, messages, http.StatusOK)
	if !strings.Contains(messages.Body.String(), `"direction":"OUTGOING"`) ||
		!strings.Contains(messages.Body.String(), `"text":"Добрый день!"`) ||
		!strings.Contains(messages.Body.String(), `"nextCursor":"`) {
		t.Fatalf("страница сообщений = %s", messages.Body.String())
	}

	ownerB := register(t, fixture.handler, "conversation-owner-b@example.com", "Владелец B")
	tenantBID := createOrganization(t, fixture, ownerB, "Организация B")
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/conversations/"+conversationID, "", ownerB.Cookie, tenantBID), http.StatusNotFound)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/conversations/"+conversationID+"/messages", "", ownerB.Cookie, tenantBID), http.StatusNotFound)
}

func canonicalWebhook(
	eventID, eventType, conversationID, messageID, contactID, direction, messageType, text, sentAt, replyTo string,
) string {
	data := map[string]any{
		"conversationExternalId": conversationID,
		"messageExternalId":      messageID,
		"attachments":            []any{},
		"metadata":               map[string]any{},
	}
	if contactID != "" {
		data["contactExternalId"] = contactID
		data["contactDisplayName"] = "Ирина"
	}
	if direction != "" {
		data["direction"] = direction
		data["messageType"] = messageType
		data["text"] = text
		data["sentAt"] = sentAt
	}
	if replyTo != "" {
		data["replyToMessageExternalId"] = replyTo
	}
	occurredAt := sentAt
	if occurredAt == "" {
		occurredAt = "2026-08-25T10:04:00Z"
	}
	payload, _ := json.Marshal(map[string]any{
		"id": eventID, "type": eventType, "occurredAt": occurredAt, "data": data,
	})
	return string(payload)
}

func processExactly(t *testing.T, fixture apiFixture, want int) {
	t.Helper()
	processed, err := fixture.normalization.ProcessBatch(context.Background(), 50)
	if err != nil || processed != want {
		t.Fatalf("ProcessBatch() = %d, %v; нужно %d", processed, err, want)
	}
}

func conversationFromList(t *testing.T, body []byte) (string, int64) {
	t.Helper()
	var response struct {
		Items []struct {
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &response); err != nil || len(response.Items) != 1 {
		t.Fatalf("список переписок: %v, %s", err, body)
	}
	return response.Items[0].ID, response.Items[0].Revision
}

func requireCanonicalCounts(
	t *testing.T,
	fixture apiFixture,
	tenantID string,
	wantContacts, wantIdentities, wantConversations, wantMessages, wantAttachments int,
) {
	t.Helper()
	for _, expectation := range []struct {
		table string
		want  int
	}{
		{"contacts", wantContacts}, {"external_identities", wantIdentities}, {"conversations", wantConversations},
		{"messages", wantMessages}, {"attachments", wantAttachments},
	} {
		var count int
		if err := fixture.pool.QueryRow(
			context.Background(), "SELECT count(*) FROM "+expectation.table+" WHERE tenant_id = $1", tenantID,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != expectation.want {
			t.Fatalf("%s: count=%d, нужно %d", expectation.table, count, expectation.want)
		}
	}
}

func requireConversationRevision(t *testing.T, fixture apiFixture, tenantID, conversationID string, want int64) {
	t.Helper()
	var revision int64
	if err := fixture.pool.QueryRow(
		context.Background(), `SELECT revision FROM conversations WHERE tenant_id = $1 AND id = $2`, tenantID, conversationID,
	).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != want {
		t.Fatalf("revision=%d, нужно %d", revision, want)
	}
}

func requireRawState(t *testing.T, fixture apiFixture, connectionID string, wantCount int, lastStatus string, wantWork int) {
	t.Helper()
	var count, work int
	var status string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*), (array_agg(status ORDER BY received_at DESC, id DESC))[1]
		FROM raw_events WHERE connection_id = $1`, connectionID).Scan(&count, &status); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(
		context.Background(), `SELECT count(*) FROM raw_event_normalization_work WHERE connection_id = $1`, connectionID,
	).Scan(&work); err != nil {
		t.Fatal(err)
	}
	if count != wantCount || status != lastStatus || work != wantWork {
		t.Fatalf("RawEvent: count=%d status=%s work=%d; нужно %d %s %d", count, status, work, wantCount, lastStatus, wantWork)
	}
}
