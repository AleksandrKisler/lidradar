package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lidradar/backend/internal/connector/domain"
)

func TestDevelopmentConnectorsVerifyIdentifyAndNormalize(t *testing.T) {
	connectors := []domain.Connector{TestConnector{}, ImportConnector{}, GenericWebhookConnector{}}
	payload := []byte(`{
		"id":"event-1","type":"message.received.v1","occurredAt":"2026-08-25T10:00:00Z",
		"data":{
			"conversationExternalId":"dialog-1","messageExternalId":"message-1","contactExternalId":"contact-1",
			"contactDisplayName":"Клиент","direction":"INCOMING","messageType":"TEXT","text":"Проверка",
			"sentAt":"2026-08-25T09:59:00Z","attachments":[],"metadata":{}
		}
	}`)
	for _, connector := range connectors {
		t.Run(string(connector.Provider()), func(t *testing.T) {
			connection := connectorConnection(t, connector.Provider(), "shared-fixture-secret")
			headers := http.Header{}
			headers.Set(developmentSecretHeader, "shared-fixture-secret")
			if err := connector.VerifyEvent(context.Background(), connection, payload, headers); err != nil {
				t.Fatalf("VerifyEvent() error = %v", err)
			}
			identifier := connector.(domain.EventIdentifier)
			eventID, err := identifier.ExternalEventID(payload, headers)
			if err != nil || eventID != "event-1" {
				t.Fatalf("ExternalEventID() = %q, %v", eventID, err)
			}
			raw := rawFixture(t, connection, payload, eventID)
			events, err := connector.NormalizeEvent(context.Background(), connection, raw)
			if err != nil || len(events) != 1 || events[0].Type != domain.CanonicalMessageReceived ||
				events[0].ConversationExternalID != "dialog-1" || events[0].Direction != domain.CanonicalIncoming {
				t.Fatalf("NormalizeEvent() = %#v, %v", events, err)
			}
		})
	}
}

func TestTelegramStubUsesBotAPIUpdateIDAndRemainsDegraded(t *testing.T) {
	connector := TelegramConnectedBusinessStubConnector{}
	connection := connectorConnection(t, connector.Provider(), "telegram_secret-123")
	payload := []byte(`{
		"update_id":9001,
		"business_message":{
			"business_connection_id":"business-1","message_id":42,"date":1787648400,
			"from":{"id":7001,"first_name":"Ирина"},
			"chat":{"id":7001,"type":"private","first_name":"Ирина"},
			"text":"Здравствуйте"
		}
	}`)
	headers := http.Header{}
	headers.Set(telegramSecretHeader, "telegram_secret-123")
	if err := connector.VerifyEvent(context.Background(), connection, payload, headers); err != nil {
		t.Fatalf("VerifyEvent() error = %v", err)
	}
	eventID, err := connector.ExternalEventID(payload, headers)
	if err != nil || eventID != "9001" {
		t.Fatalf("ExternalEventID() = %q, %v", eventID, err)
	}
	events, err := connector.NormalizeEvent(context.Background(), connection, rawFixture(t, connection, payload, eventID))
	if err != nil || len(events) != 1 || events[0].Type != domain.CanonicalMessageReceived ||
		events[0].Direction != domain.CanonicalIncoming || events[0].MessageExternalID != "7001:42" {
		t.Fatalf("NormalizeEvent() = %#v, %v", events, err)
	}
	health := connector.Health(context.Background(), connection)
	if health.Status != domain.ConnectionDegraded || health.LastErrorCode == nil || *health.LastErrorCode != telegramSpikeErrorCode {
		t.Fatalf("Health() = %#v", health)
	}

	badHeaders := http.Header{}
	badHeaders.Set(telegramSecretHeader, "wrong-secret-value")
	if err := connector.VerifyEvent(context.Background(), connection, payload, badHeaders); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("wrong-secret VerifyEvent() error = %v", err)
	}
}

func TestConnectorsRejectMalformedAndUnknownPayloads(t *testing.T) {
	connector := GenericWebhookConnector{}
	connection := connectorConnection(t, connector.Provider(), "shared-fixture-secret")
	headers := http.Header{}
	headers.Set(developmentSecretHeader, "shared-fixture-secret")
	for _, payload := range [][]byte{
		[]byte(`not-json`),
		[]byte(`{"id":"event","type":"message.received.v1","occurredAt":"2026-08-25T10:00:00Z","data":{},"unknown":true}`),
		[]byte(`{"id":"","type":"message.received.v1","occurredAt":"2026-08-25T10:00:00Z","data":{}}`),
	} {
		if err := connector.VerifyEvent(context.Background(), connection, payload, headers); !errors.Is(err, domain.ErrInvalidPayload) {
			t.Errorf("VerifyEvent(%s) error = %v", payload, err)
		}
	}
}

func TestConnectorFixtureSetsProduceCanonicalEvents(t *testing.T) {
	tests := []struct {
		name      string
		connector domain.Connector
		folder    string
		header    string
		secret    string
	}{
		{"test", TestConnector{}, "test", developmentSecretHeader, "fixture-secret-test"},
		{"import", ImportConnector{}, "import", developmentSecretHeader, "fixture-secret-import"},
		{"generic", GenericWebhookConnector{}, "generic_webhook", developmentSecretHeader, "fixture-secret-generic"},
		{"telegram", TelegramConnectedBusinessStubConnector{}, "telegram", telegramSecretHeader, "fixture_secret_telegram"},
	}
	fixtureExpectations := []struct {
		name      string
		eventType domain.CanonicalEventType
	}{
		{"new_message.json", domain.CanonicalMessageReceived},
		{"edited_message.json", domain.CanonicalMessageEdited},
		{"deleted_message.json", domain.CanonicalMessageDeleted},
		{"attachment.json", domain.CanonicalMessageReceived},
		{"duplicate_event.json", domain.CanonicalMessageReceived},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			connection := connectorConnection(t, test.connector.Provider(), test.secret)
			headers := http.Header{}
			headers.Set(test.header, test.secret)
			var firstSourceEventID string
			for _, expectation := range fixtureExpectations {
				payload, err := os.ReadFile(filepath.Join("testdata", "fixtures", test.folder, expectation.name))
				if err != nil {
					t.Fatal(err)
				}
				if err := test.connector.VerifyEvent(context.Background(), connection, payload, headers); err != nil {
					t.Fatalf("VerifyEvent(%s): %v", expectation.name, err)
				}
				identifier := test.connector.(domain.EventIdentifier)
				eventID, err := identifier.ExternalEventID(payload, headers)
				if err != nil {
					t.Fatalf("ExternalEventID(%s): %v", expectation.name, err)
				}
				events, err := test.connector.NormalizeEvent(
					context.Background(), connection, rawFixture(t, connection, payload, eventID),
				)
				if err != nil || len(events) != 1 || events[0].Type != expectation.eventType || events[0].Validate() != nil {
					t.Fatalf("NormalizeEvent(%s) = %#v, %v", expectation.name, events, err)
				}
				if expectation.name == "new_message.json" {
					firstSourceEventID = events[0].SourceEventID
				}
				if expectation.name == "duplicate_event.json" && events[0].SourceEventID != firstSourceEventID {
					t.Fatalf("duplicate source event = %s, нужно %s", events[0].SourceEventID, firstSourceEventID)
				}
				if expectation.name == "attachment.json" && len(events[0].Attachments) != 1 {
					t.Fatalf("вложение не нормализовано: %#v", events[0])
				}
			}
		})
	}
}

func TestDirectionDetectionForManualOutgoingMessages(t *testing.T) {
	generic := GenericWebhookConnector{}
	genericConnection := connectorConnection(t, generic.Provider(), "generic-direction-secret")
	genericPayload := []byte(`{
		"id":"direction-1","type":"message.received.v1","occurredAt":"2026-08-25T10:00:00Z",
		"data":{"conversationExternalId":"dialog","messageExternalId":"message","contactExternalId":"contact",
		"direction":"OUTGOING","messageType":"TEXT","text":"Ответ","sentAt":"2026-08-25T10:00:00Z","metadata":{}}
	}`)
	genericEvents, err := generic.NormalizeEvent(
		context.Background(), genericConnection, rawFixture(t, genericConnection, genericPayload, "direction-1"),
	)
	if err != nil || len(genericEvents) != 1 || genericEvents[0].Direction != domain.CanonicalOutgoing {
		t.Fatalf("generic outgoing = %#v, %v", genericEvents, err)
	}

	telegram := TelegramConnectedBusinessStubConnector{}
	telegramConnection := connectorConnection(t, telegram.Provider(), "telegram_direction_secret")
	telegramPayload := []byte(`{
		"update_id":9201,"business_message":{"business_connection_id":"business-1","message_id":50,"date":1787649000,
		"from":{"id":8001,"first_name":"Владелец"},"chat":{"id":7001,"type":"private","first_name":"Ирина"},"text":"Ответ"}}
	`)
	telegramEvents, err := telegram.NormalizeEvent(
		context.Background(), telegramConnection, rawFixture(t, telegramConnection, telegramPayload, "9201"),
	)
	if err != nil || len(telegramEvents) != 1 || telegramEvents[0].Direction != domain.CanonicalOutgoing {
		t.Fatalf("telegram outgoing = %#v, %v", telegramEvents, err)
	}
}

func TestTelegramAttachmentRequiresDownloadableFileIdentifier(t *testing.T) {
	connector := TelegramConnectedBusinessStubConnector{}
	connection := connectorConnection(t, connector.Provider(), "telegram_file_secret")
	payload := []byte(`{
		"update_id":9301,"business_message":{"business_connection_id":"business-1","message_id":51,"date":1787649000,
		"from":{"id":7001},"chat":{"id":7001,"type":"private"},"document":{"file_unique_id":"unique-only","file_size":10}}}
	`)
	_, err := connector.NormalizeEvent(
		context.Background(), connection, rawFixture(t, connection, payload, "9301"),
	)
	if !errors.Is(err, domain.ErrInvalidPayload) {
		t.Fatalf("NormalizeEvent() error = %v", err)
	}
}

func connectorConnection(t *testing.T, provider domain.Provider, secret string) domain.ChannelConnection {
	t.Helper()
	now := time.Now()
	connection, err := domain.NewChannelConnection(
		"connection", "tenant", nil, provider, "Fixture",
		[]domain.Capability{domain.CapabilityReceiveMessages}, testDigest([]byte(secret)),
		domain.ConnectionHealth{Status: domain.ConnectionActive, CheckedAt: now}, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func rawFixture(t *testing.T, connection domain.ChannelConnection, payload []byte, externalID string) domain.RawEvent {
	t.Helper()
	event, err := domain.NewRawEvent(
		"raw", connection.TenantID, connection.ID, connection.Provider, externalID,
		json.RawMessage(payload), testDigest(payload), domain.RawEventReceived, nil, time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func testDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
