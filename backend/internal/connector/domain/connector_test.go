package domain

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestConnectionHealthTransitions(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	connection, err := NewChannelConnection(
		"connection", "tenant", nil, ProviderGenericWebhook, "Website",
		[]Capability{CapabilityIdentifyContact, CapabilityReceiveMessages},
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ConnectionHealth{Status: ConnectionActive, CheckedAt: now}, now,
	)
	if err != nil {
		t.Fatalf("NewChannelConnection() error = %v", err)
	}
	if connection.Status != ConnectionActive {
		t.Fatalf("initial status = %s", connection.Status)
	}

	errorAt := now.Add(time.Minute)
	if err := connection.RecordFailure("INVALID_PAYLOAD", errorAt); err != nil {
		t.Fatalf("RecordFailure() error = %v", err)
	}
	if connection.Status != ConnectionError || connection.LastErrorCode == nil || *connection.LastErrorCode != "INVALID_PAYLOAD" {
		t.Fatalf("error connection = %#v", connection)
	}

	successAt := errorAt.Add(time.Minute)
	if err := connection.RecordSuccess(successAt, ConnectionHealth{Status: ConnectionActive, CheckedAt: successAt}); err != nil {
		t.Fatalf("RecordSuccess() error = %v", err)
	}
	if connection.Status != ConnectionActive || connection.LastErrorCode != nil || connection.LastSuccessAt == nil {
		t.Fatalf("recovered connection = %#v", connection)
	}

	if err := connection.Disconnect(successAt.Add(time.Minute)); err != nil {
		t.Fatalf("Disconnect() error = %v", err)
	}
	if connection.Status != ConnectionDisconnected {
		t.Fatalf("disconnected status = %s", connection.Status)
	}
}

func TestRawEventValidationRequiresFailureDiagnostics(t *testing.T) {
	now := time.Now()
	payload := json.RawMessage(`{"id":"event"}`)
	hash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := NewRawEvent("event", "tenant", "connection", ProviderTest, "external", payload, hash, RawEventFailed, nil, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("failed event without code error = %v", err)
	}
	code := "INVALID_PAYLOAD"
	event, err := NewRawEvent("event", "tenant", "connection", ProviderTest, "external", payload, hash, RawEventFailed, &code, now)
	if err != nil || event.ProcessedAt == nil {
		t.Fatalf("failed event = %#v, %v", event, err)
	}
	if _, err := NewNormalizationWork("work", event, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("failed event work error = %v", err)
	}
}

func TestConnectionJSONDoesNotExposeVerificationSecretHash(t *testing.T) {
	now := time.Now()
	connection, err := NewChannelConnection(
		"connection", "tenant", nil, ProviderTest, "Test",
		[]Capability{CapabilityReceiveMessages},
		"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		ConnectionHealth{Status: ConnectionActive, CheckedAt: now}, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(connection)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || contains(string(encoded), connection.VerificationSecretHash) {
		t.Fatalf("connection JSON disclosed secret hash: %s", encoded)
	}
}

func contains(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
