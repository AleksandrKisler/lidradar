package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"lidradar/backend/internal/connector/domain"
)

func TestDevelopmentConnectorsVerifyIdentifyAndNormalize(t *testing.T) {
	connectors := []domain.Connector{TestConnector{}, ImportConnector{}, GenericWebhookConnector{}}
	payload := []byte(`{"id":"event-1","type":"message.received.v1","occurredAt":"2026-08-25T10:00:00Z","data":{"text":"fixture"}}`)
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
			if err != nil || len(events) != 1 || events[0].Type != "message.received.v1" {
				t.Fatalf("NormalizeEvent() = %#v, %v", events, err)
			}
		})
	}
}

func TestTelegramStubUsesBotAPIUpdateIDAndRemainsDegraded(t *testing.T) {
	connector := TelegramConnectedBusinessStubConnector{}
	connection := connectorConnection(t, connector.Provider(), "telegram_secret-123")
	payload := []byte(`{"update_id":9001,"business_message":{"business_connection_id":"business-1","message_id":42}}`)
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
	if err != nil || len(events) != 1 || events[0].Type != "message.received.v1" {
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
