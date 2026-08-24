package infrastructure

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"lidradar/backend/internal/connector/domain"
)

const (
	developmentSecretHeader = "X-LidRadar-Webhook-Secret"
	telegramSecretHeader    = "X-Telegram-Bot-Api-Secret-Token"
	telegramSpikeErrorCode  = "TELEGRAM_SPIKE_NOT_VERIFIED"
)

type Registry struct {
	registrations map[domain.Provider]domain.ConnectorRegistration
}

func NewRegistry() Registry {
	return Registry{registrations: map[domain.Provider]domain.ConnectorRegistration{
		domain.ProviderTest: {
			Connector: TestConnector{},
			Capabilities: []domain.Capability{
				domain.CapabilityReceiveMessages, domain.CapabilityImportHistory,
				domain.CapabilityReceiveEdits, domain.CapabilityReceiveDeletions,
				domain.CapabilityReceiveAttachments, domain.CapabilityIdentifyContact,
			},
		},
		domain.ProviderImport: {
			Connector: ImportConnector{},
			Capabilities: []domain.Capability{
				domain.CapabilityReceiveMessages, domain.CapabilityImportHistory,
				domain.CapabilityReceiveEdits, domain.CapabilityReceiveDeletions,
				domain.CapabilityReceiveAttachments, domain.CapabilityIdentifyContact,
			},
		},
		domain.ProviderGenericWebhook: {
			Connector: GenericWebhookConnector{},
			Capabilities: []domain.Capability{
				domain.CapabilityReceiveMessages, domain.CapabilityReceiveEdits,
				domain.CapabilityReceiveDeletions, domain.CapabilityReceiveAttachments,
				domain.CapabilityIdentifyContact,
			},
		},
		domain.ProviderTelegramConnectedBusinessBot: {
			Connector: TelegramConnectedBusinessStubConnector{},
			Capabilities: []domain.Capability{
				domain.CapabilityReceiveMessages, domain.CapabilitySendMessages,
				domain.CapabilityReceiveEdits, domain.CapabilityReceiveDeletions,
				domain.CapabilityReceiveAttachments, domain.CapabilityIdentifyContact,
			},
		},
	}}
}

func (registry Registry) Lookup(provider domain.Provider) (domain.ConnectorRegistration, bool) {
	registration, found := registry.registrations[provider]
	if !found {
		return domain.ConnectorRegistration{}, false
	}
	registration.Capabilities = append([]domain.Capability(nil), registration.Capabilities...)
	return registration, true
}

type TestConnector struct{}

func (TestConnector) Provider() domain.Provider { return domain.ProviderTest }
func (connector TestConnector) VerifyEvent(_ context.Context, connection domain.ChannelConnection, payload []byte, headers domain.Headers) error {
	return verifyEnvelopeEvent(connection, connector.Provider(), payload, headers)
}
func (TestConnector) ExternalEventID(payload []byte, _ domain.Headers) (string, error) {
	event, err := decodeEnvelope(payload)
	return event.ID, err
}
func (connector TestConnector) NormalizeEvent(_ context.Context, connection domain.ChannelConnection, event domain.RawEvent) ([]domain.CanonicalEvent, error) {
	return normalizeEnvelope(connection, connector.Provider(), event)
}
func (TestConnector) Health(_ context.Context, _ domain.ChannelConnection) domain.ConnectionHealth {
	return activeHealth()
}

type ImportConnector struct{}

func (ImportConnector) Provider() domain.Provider { return domain.ProviderImport }
func (connector ImportConnector) VerifyEvent(_ context.Context, connection domain.ChannelConnection, payload []byte, headers domain.Headers) error {
	return verifyEnvelopeEvent(connection, connector.Provider(), payload, headers)
}
func (ImportConnector) ExternalEventID(payload []byte, _ domain.Headers) (string, error) {
	event, err := decodeEnvelope(payload)
	return event.ID, err
}
func (connector ImportConnector) NormalizeEvent(_ context.Context, connection domain.ChannelConnection, event domain.RawEvent) ([]domain.CanonicalEvent, error) {
	return normalizeEnvelope(connection, connector.Provider(), event)
}
func (ImportConnector) Health(_ context.Context, _ domain.ChannelConnection) domain.ConnectionHealth {
	return activeHealth()
}

type GenericWebhookConnector struct{}

func (GenericWebhookConnector) Provider() domain.Provider { return domain.ProviderGenericWebhook }
func (connector GenericWebhookConnector) VerifyEvent(_ context.Context, connection domain.ChannelConnection, payload []byte, headers domain.Headers) error {
	return verifyEnvelopeEvent(connection, connector.Provider(), payload, headers)
}
func (GenericWebhookConnector) ExternalEventID(payload []byte, _ domain.Headers) (string, error) {
	event, err := decodeEnvelope(payload)
	return event.ID, err
}
func (connector GenericWebhookConnector) NormalizeEvent(_ context.Context, connection domain.ChannelConnection, event domain.RawEvent) ([]domain.CanonicalEvent, error) {
	return normalizeEnvelope(connection, connector.Provider(), event)
}
func (GenericWebhookConnector) Health(_ context.Context, _ domain.ChannelConnection) domain.ConnectionHealth {
	return activeHealth()
}

// TelegramConnectedBusinessStubConnector validates the current Bot API update
// envelope and secret-token header without making network calls. It remains
// DEGRADED until the repository's required real-account spike is completed.
type TelegramConnectedBusinessStubConnector struct{}

func (TelegramConnectedBusinessStubConnector) Provider() domain.Provider {
	return domain.ProviderTelegramConnectedBusinessBot
}

func (connector TelegramConnectedBusinessStubConnector) VerifyEvent(
	_ context.Context,
	connection domain.ChannelConnection,
	payload []byte,
	headers domain.Headers,
) error {
	if connection.Provider != connector.Provider() {
		return domain.ErrInvalid
	}
	if err := verifySecret(connection.VerificationSecretHash, headers.Get(telegramSecretHeader)); err != nil {
		return err
	}
	_, _, err := decodeTelegramUpdate(payload)
	return err
}

func (TelegramConnectedBusinessStubConnector) ExternalEventID(payload []byte, _ domain.Headers) (string, error) {
	updateID, _, err := decodeTelegramUpdate(payload)
	return updateID, err
}

func (connector TelegramConnectedBusinessStubConnector) NormalizeEvent(
	_ context.Context,
	connection domain.ChannelConnection,
	event domain.RawEvent,
) ([]domain.CanonicalEvent, error) {
	if connection.Provider != connector.Provider() || event.Provider != connector.Provider() || connection.ID != event.ConnectionID {
		return nil, domain.ErrInvalid
	}
	updateID, eventType, err := decodeTelegramUpdate(event.Payload)
	if err != nil {
		return nil, err
	}
	return []domain.CanonicalEvent{{
		ExternalEventID: updateID,
		Type:            eventType,
		OccurredAt:      event.ReceivedAt.UTC(),
		Data:            append(json.RawMessage(nil), event.Payload...),
	}}, nil
}

func (TelegramConnectedBusinessStubConnector) Health(_ context.Context, _ domain.ChannelConnection) domain.ConnectionHealth {
	code := telegramSpikeErrorCode
	return domain.ConnectionHealth{
		Status: domain.ConnectionDegraded, LastErrorCode: &code, CheckedAt: time.Now().UTC(),
	}
}

type envelope struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	OccurredAt time.Time       `json:"occurredAt"`
	Data       json.RawMessage `json:"data"`
}

func verifyEnvelopeEvent(
	connection domain.ChannelConnection,
	provider domain.Provider,
	payload []byte,
	headers domain.Headers,
) error {
	if connection.Provider != provider {
		return domain.ErrInvalid
	}
	if err := verifySecret(connection.VerificationSecretHash, headers.Get(developmentSecretHeader)); err != nil {
		return err
	}
	_, err := decodeEnvelope(payload)
	return err
}

func normalizeEnvelope(connection domain.ChannelConnection, provider domain.Provider, event domain.RawEvent) ([]domain.CanonicalEvent, error) {
	if connection.Provider != provider || event.Provider != provider || connection.ID != event.ConnectionID {
		return nil, domain.ErrInvalid
	}
	decoded, err := decodeEnvelope(event.Payload)
	if err != nil {
		return nil, err
	}
	return []domain.CanonicalEvent{{
		ExternalEventID: decoded.ID, Type: decoded.Type, OccurredAt: decoded.OccurredAt.UTC(),
		Data: append(json.RawMessage(nil), decoded.Data...),
	}}, nil
}

func decodeEnvelope(payload []byte) (envelope, error) {
	var event envelope
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		return envelope{}, domain.ErrInvalidPayload
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return envelope{}, domain.ErrInvalidPayload
	}
	event.ID = strings.TrimSpace(event.ID)
	event.Type = strings.TrimSpace(event.Type)
	if event.ID == "" || len(event.ID) > 512 || event.Type == "" || len(event.Type) > 200 || event.OccurredAt.IsZero() || !json.Valid(event.Data) {
		return envelope{}, domain.ErrInvalidPayload
	}
	return event, nil
}

func decodeTelegramUpdate(payload []byte) (string, string, error) {
	var update map[string]json.RawMessage
	if err := json.Unmarshal(payload, &update); err != nil {
		return "", "", domain.ErrInvalidPayload
	}
	rawUpdateID, found := update["update_id"]
	if !found {
		return "", "", domain.ErrInvalidPayload
	}
	var updateID json.Number
	decoder := json.NewDecoder(bytes.NewReader(rawUpdateID))
	decoder.UseNumber()
	if err := decoder.Decode(&updateID); err != nil {
		return "", "", domain.ErrInvalidPayload
	}
	numericID, err := strconv.ParseInt(updateID.String(), 10, 64)
	if err != nil || numericID < 0 {
		return "", "", domain.ErrInvalidPayload
	}
	types := []struct {
		field string
		kind  string
	}{
		{"business_connection", "connection.updated.v1"},
		{"business_message", "message.received.v1"},
		{"edited_business_message", "message.edited.v1"},
		{"deleted_business_messages", "message.deleted.v1"},
	}
	eventType := ""
	for _, candidate := range types {
		if raw, exists := update[candidate.field]; exists && len(raw) > 0 && string(raw) != "null" {
			if eventType != "" {
				return "", "", domain.ErrInvalidPayload
			}
			eventType = candidate.kind
		}
	}
	if eventType == "" {
		return "", "", domain.ErrInvalidPayload
	}
	return strconv.FormatInt(numericID, 10), eventType, nil
}

func verifySecret(expectedHash, providedSecret string) error {
	if expectedHash == "" || providedSecret == "" {
		return domain.ErrUnauthenticated
	}
	expected, err := hex.DecodeString(expectedHash)
	if err != nil || len(expected) != sha256.Size {
		return domain.ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(providedSecret))
	if subtle.ConstantTimeCompare(expected, digest[:]) != 1 {
		return domain.ErrUnauthenticated
	}
	return nil
}

func activeHealth() domain.ConnectionHealth {
	return domain.ConnectionHealth{Status: domain.ConnectionActive, CheckedAt: time.Now().UTC()}
}
