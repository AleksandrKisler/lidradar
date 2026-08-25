package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/connector/domain"
)

type testAuthorizer bool

func (allowed testAuthorizer) Allowed(_ context.Context, actorID, tenantID, permission string) (bool, error) {
	return bool(allowed) && actorID == "owner" && tenantID == "tenant" && permission == PermissionManage, nil
}

type testIDs struct{ next int }

func (ids *testIDs) NewID() (string, error) {
	ids.next++
	return fmt.Sprintf("id-%d", ids.next), nil
}

type testConnector struct {
	provider       domain.Provider
	verifyErr      error
	identifierErr  error
	normalizeCalls int
	normalized     []domain.CanonicalEvent
	normalizeErr   error
}

func (connector *testConnector) Provider() domain.Provider {
	if connector.provider.Valid() {
		return connector.provider
	}
	return domain.ProviderTest
}
func (connector *testConnector) VerifyEvent(context.Context, domain.ChannelConnection, []byte, domain.Headers) error {
	return connector.verifyErr
}
func (connector *testConnector) ExternalEventID([]byte, domain.Headers) (string, error) {
	return "external-1", connector.identifierErr
}
func (connector *testConnector) NormalizeEvent(context.Context, domain.ChannelConnection, domain.RawEvent) ([]domain.CanonicalEvent, error) {
	connector.normalizeCalls++
	return connector.normalized, connector.normalizeErr
}
func (*testConnector) Health(context.Context, domain.ChannelConnection) domain.ConnectionHealth {
	return domain.ConnectionHealth{Status: domain.ConnectionActive, CheckedAt: time.Now()}
}

type testRegistry struct {
	connector   *testConnector
	provisioner domain.ConnectionProvisioner
}

func (registry testRegistry) Lookup(provider domain.Provider) (domain.ConnectorRegistration, bool) {
	if registry.connector == nil || provider != registry.connector.Provider() {
		return domain.ConnectorRegistration{}, false
	}
	return domain.ConnectorRegistration{
		Connector: registry.connector, Capabilities: []domain.Capability{domain.CapabilityReceiveMessages},
		Provisioner: registry.provisioner,
	}, true
}

type testCredentialCipher struct {
	plaintext []byte
	aad       []byte
}

func (cipher *testCredentialCipher) Encrypt(plaintext, aad []byte) ([]byte, error) {
	cipher.plaintext = append([]byte(nil), plaintext...)
	cipher.aad = append([]byte(nil), aad...)
	return []byte("encrypted-telegram-token"), nil
}

func (cipher *testCredentialCipher) Decrypt(encrypted, aad []byte) ([]byte, error) {
	if string(encrypted) != "encrypted-telegram-token" || string(aad) != string(cipher.aad) {
		return nil, errors.New("неверная привязка шифротекста")
	}
	return append([]byte(nil), cipher.plaintext...), nil
}

type testProvisioner struct {
	provisioned   int
	deprovisioned int
	secret        string
	credentials   []byte
	err           error
}

func (provisioner *testProvisioner) Provision(
	_ context.Context,
	_ domain.ChannelConnection,
	secret string,
	credentials json.RawMessage,
) (domain.ConnectionHealth, error) {
	provisioner.provisioned++
	provisioner.secret = secret
	provisioner.credentials = append([]byte(nil), credentials...)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	return domain.ConnectionHealth{Status: domain.ConnectionActive, CheckedAt: now}, provisioner.err
}

func (provisioner *testProvisioner) Deprovision(
	_ context.Context,
	_ domain.ChannelConnection,
	credentials json.RawMessage,
) error {
	provisioner.deprovisioned++
	provisioner.credentials = append([]byte(nil), credentials...)
	return provisioner.err
}

type testRepository struct {
	connections map[string]domain.ChannelConnection
	events      []domain.RawEvent
	works       []*domain.NormalizationWork
	pending     []domain.NormalizationItem
	completed   []string
	failed      []string
}

func newTestRepository() *testRepository {
	return &testRepository{connections: map[string]domain.ChannelConnection{}}
}

func (repository *testRepository) ListConnections(_ context.Context, tenantID string) ([]domain.ChannelConnection, error) {
	result := make([]domain.ChannelConnection, 0)
	for _, connection := range repository.connections {
		if connection.TenantID == tenantID {
			result = append(result, connection)
		}
	}
	return result, nil
}
func (repository *testRepository) Connection(_ context.Context, tenantID, connectionID string) (domain.ChannelConnection, bool, error) {
	connection, found := repository.connections[connectionID]
	if !found || connection.TenantID != tenantID {
		return domain.ChannelConnection{}, false, nil
	}
	return connection, true, nil
}
func (repository *testRepository) CreateConnection(_ context.Context, tenantID string, connection domain.ChannelConnection) error {
	if tenantID != connection.TenantID {
		return domain.ErrInvalid
	}
	repository.connections[connection.ID] = connection
	return nil
}
func (repository *testRepository) UpdateConnectionHealth(
	_ context.Context,
	tenantID, connectionID string,
	health domain.ConnectionHealth,
) (domain.ChannelConnection, bool, error) {
	connection, found := repository.connections[connectionID]
	if !found || connection.TenantID != tenantID {
		return domain.ChannelConnection{}, false, nil
	}
	connection.Status = health.Status
	connection.UpdatedAt = health.CheckedAt
	if health.Status == domain.ConnectionActive {
		connection.LastSuccessAt = &connection.UpdatedAt
		connection.LastErrorAt = nil
		connection.LastErrorCode = nil
	} else {
		connection.LastErrorAt = &connection.UpdatedAt
		connection.LastErrorCode = health.LastErrorCode
	}
	repository.connections[connectionID] = connection
	return connection, true, nil
}
func (repository *testRepository) DisconnectConnection(_ context.Context, tenantID, connectionID string, at time.Time) (domain.ChannelConnection, bool, error) {
	connection, found := repository.connections[connectionID]
	if !found || connection.TenantID != tenantID {
		return domain.ChannelConnection{}, false, nil
	}
	if err := connection.Disconnect(at); err != nil {
		return domain.ChannelConnection{}, false, err
	}
	repository.connections[connectionID] = connection
	return connection, true, nil
}
func (repository *testRepository) PersistEvent(
	_ context.Context,
	tenantID, connectionID string,
	event domain.RawEvent,
	work *domain.NormalizationWork,
	_ domain.ConnectionHealth,
) (domain.PersistResult, error) {
	if tenantID != event.TenantID || connectionID != event.ConnectionID {
		return domain.PersistResult{}, domain.ErrInvalid
	}
	repository.events = append(repository.events, event)
	repository.works = append(repository.works, work)
	return domain.PersistResult{Event: event, Inserted: true}, nil
}
func (repository *testRepository) PendingNormalization(context.Context, int) ([]domain.NormalizationItem, error) {
	return append([]domain.NormalizationItem(nil), repository.pending...), nil
}
func (repository *testRepository) CompleteNormalization(_ context.Context, _, rawEventID string, _ time.Time) error {
	repository.completed = append(repository.completed, rawEventID)
	return nil
}
func (repository *testRepository) FailNormalization(_ context.Context, _, rawEventID, _ string, _ time.Time) error {
	repository.failed = append(repository.failed, rawEventID)
	return nil
}

type testCanonicalSink struct {
	events []domain.CanonicalEvent
	err    error
}

func (sink *testCanonicalSink) IngestCanonical(_ context.Context, event domain.CanonicalEvent) error {
	sink.events = append(sink.events, event)
	return sink.err
}

func TestReceivePersistsBeforeNormalization(t *testing.T) {
	repository := newTestRepository()
	connector := &testConnector{}
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	service := NewService(repository, testAuthorizer(true), testRegistry{connector: connector}, &testIDs{}, func() time.Time { return now })
	connection, err := service.Connect(context.Background(), "owner", "tenant", ConnectCommand{
		Provider: "TEST", Name: "Fixture", WebhookSecret: "fixture-secret-123",
	})
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	receipt, err := service.Receive(
		context.Background(), "TEST", "tenant", connection.ID, []byte(`{"fixture":true}`), http.Header{"X-Test": []string{"value"}},
	)
	if err != nil {
		t.Fatalf("Receive() error = %v", err)
	}
	if receipt.Status != domain.RawEventReceived || len(repository.events) != 1 || len(repository.works) != 1 || repository.works[0] == nil {
		t.Fatalf("persisted receipt = %#v, events %d, work %#v", receipt, len(repository.events), repository.works)
	}
	if connector.normalizeCalls != 0 {
		t.Fatalf("NormalizeEvent called %d times in receive path", connector.normalizeCalls)
	}
}

func TestInvalidPayloadIsPersistedFailedWithoutWork(t *testing.T) {
	repository := newTestRepository()
	connector := &testConnector{verifyErr: domain.ErrInvalidPayload, identifierErr: domain.ErrInvalidPayload}
	service := NewService(repository, testAuthorizer(true), testRegistry{connector: connector}, &testIDs{}, time.Now)
	connection, err := service.Connect(context.Background(), "owner", "tenant", ConnectCommand{
		Provider: "TEST", Name: "Fixture", WebhookSecret: "fixture-secret-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := service.Receive(context.Background(), "TEST", "tenant", connection.ID, []byte(`not-json`), http.Header{"X-Test": []string{"value"}})
	if err != nil {
		t.Fatalf("Receive() error = %v", err)
	}
	if receipt.Status != domain.RawEventFailed || repository.works[0] != nil || repository.events[0].ErrorCode == nil {
		t.Fatalf("failed receipt = %#v, event %#v, work %#v", receipt, repository.events[0], repository.works[0])
	}
}

func TestUnauthenticatedPayloadIsNotPersisted(t *testing.T) {
	repository := newTestRepository()
	connector := &testConnector{verifyErr: domain.ErrUnauthenticated}
	service := NewService(repository, testAuthorizer(true), testRegistry{connector: connector}, &testIDs{}, time.Now)
	connection, err := service.Connect(context.Background(), "owner", "tenant", ConnectCommand{
		Provider: "TEST", Name: "Fixture", WebhookSecret: "fixture-secret-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Receive(context.Background(), "TEST", "tenant", connection.ID, []byte(`{"fixture":true}`), http.Header{"X-Test": []string{"bad"}})
	if !errors.Is(err, ErrUnauthenticated) || len(repository.events) != 0 {
		t.Fatalf("Receive() error = %v, events = %d", err, len(repository.events))
	}
}

func TestManagerCannotManageConnections(t *testing.T) {
	service := NewService(newTestRepository(), testAuthorizer(false), testRegistry{connector: &testConnector{}}, &testIDs{}, time.Now)
	if _, err := service.Connect(context.Background(), "manager", "tenant", ConnectCommand{
		Provider: "TEST", Name: "Fixture", WebhookSecret: "fixture-secret-123",
	}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Connect() error = %v", err)
	}
}

func TestTelegramConnectionEncryptsTokenProvisionsAndDeprovisions(t *testing.T) {
	repository := newTestRepository()
	connector := &testConnector{provider: domain.ProviderTelegramConnectedBusinessBot}
	provisioner := &testProvisioner{}
	cipher := &testCredentialCipher{}
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	service := NewService(
		repository, testAuthorizer(true), testRegistry{connector: connector, provisioner: provisioner},
		&testIDs{}, func() time.Time { return now }, WithCredentialCipher(cipher),
	)
	token := "123456:abcdefghijklmnopqrstuvwxyzABCD"
	connection, err := service.Connect(context.Background(), "owner", "tenant", ConnectCommand{
		Provider: "CONNECTED_BUSINESS_BOT", Name: "Telegram", WebhookSecret: "telegram_secret_123", BotToken: token,
	})
	if err != nil || connection.Status != domain.ConnectionActive || provisioner.provisioned != 1 {
		t.Fatalf("Connect() = %#v, %v; provisioned=%d", connection, err, provisioner.provisioned)
	}
	if string(connection.EncryptedCredentials) != "encrypted-telegram-token" ||
		strings.Contains(string(connection.EncryptedCredentials), token) || strings.Contains(string(provisioner.credentials), "encrypted") ||
		!strings.Contains(string(provisioner.credentials), token) || provisioner.secret != "telegram_secret_123" {
		t.Fatalf("реквизиты обработаны неверно: connection=%#v provisioner=%q", connection, provisioner.credentials)
	}
	encoded, err := json.Marshal(connection)
	if err != nil || strings.Contains(string(encoded), token) || strings.Contains(string(encoded), "encrypted-telegram-token") {
		t.Fatalf("публичный ответ раскрыл реквизиты: %s, %v", encoded, err)
	}
	if err := service.Disconnect(context.Background(), "owner", "tenant", connection.ID); err != nil || provisioner.deprovisioned != 1 {
		t.Fatalf("Disconnect() = %v; deprovisioned=%d", err, provisioner.deprovisioned)
	}
}

func TestTelegramProvisioningFailureIsPersistedAsSafeHealthCode(t *testing.T) {
	repository := newTestRepository()
	provisioner := &testProvisioner{err: errors.New("секретная ошибка с токеном")}
	service := NewService(
		repository, testAuthorizer(true), testRegistry{
			connector: &testConnector{provider: domain.ProviderTelegramConnectedBusinessBot}, provisioner: provisioner,
		},
		&testIDs{}, time.Now, WithCredentialCipher(&testCredentialCipher{}),
	)
	connection, err := service.Connect(context.Background(), "owner", "tenant", ConnectCommand{
		Provider: "CONNECTED_BUSINESS_BOT", Name: "Telegram", WebhookSecret: "telegram_secret_123",
		BotToken: "123456:abcdefghijklmnopqrstuvwxyzABCD",
	})
	if err != nil || connection.Status != domain.ConnectionError || connection.LastErrorCode == nil ||
		*connection.LastErrorCode != telegramSetupFailedCode || strings.Contains(*connection.LastErrorCode, "токен") {
		t.Fatalf("Connect() = %#v, %v", connection, err)
	}
}

func TestTelegramDisconnectCanRetryRemoteWebhookRemoval(t *testing.T) {
	repository := newTestRepository()
	provisioner := &testProvisioner{}
	service := NewService(
		repository, testAuthorizer(true), testRegistry{
			connector: &testConnector{provider: domain.ProviderTelegramConnectedBusinessBot}, provisioner: provisioner,
		},
		&testIDs{}, time.Now, WithCredentialCipher(&testCredentialCipher{}),
	)
	connection, err := service.Connect(context.Background(), "owner", "tenant", ConnectCommand{
		Provider: "CONNECTED_BUSINESS_BOT", Name: "Telegram", WebhookSecret: "telegram_secret_123",
		BotToken: "123456:abcdefghijklmnopqrstuvwxyzABCD",
	})
	if err != nil {
		t.Fatal(err)
	}
	provisioner.err = errors.New("Telegram временно недоступен")
	if err := service.Disconnect(context.Background(), "owner", "tenant", connection.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("first Disconnect() error = %v", err)
	}
	if stored := repository.connections[connection.ID]; stored.Status != domain.ConnectionDisconnected {
		t.Fatalf("локальное состояние после ошибки = %#v", stored)
	}
	provisioner.err = nil
	if err := service.Disconnect(context.Background(), "owner", "tenant", connection.ID); err != nil || provisioner.deprovisioned != 2 {
		t.Fatalf("retry Disconnect() error = %v; deprovisioned=%d", err, provisioner.deprovisioned)
	}
}

func TestNormalizationCompletesOnlyAfterCanonicalIngestion(t *testing.T) {
	repository, connector, item := normalizationFixture(t)
	connector.normalized = []domain.CanonicalEvent{canonicalFixture(item)}
	sink := &testCanonicalSink{}
	service := NewNormalizationService(repository, testRegistry{connector: connector}, sink, time.Now)

	processed, err := service.ProcessBatch(context.Background(), 10)
	if err != nil || processed != 1 || len(sink.events) != 1 || len(repository.completed) != 1 || len(repository.failed) != 0 {
		t.Fatalf("ProcessBatch() = %d, %v; sink=%d completed=%v failed=%v", processed, err, len(sink.events), repository.completed, repository.failed)
	}
}

func TestNormalizationMarksInvalidCanonicalEventFailed(t *testing.T) {
	repository, connector, _ := normalizationFixture(t)
	connector.normalized = []domain.CanonicalEvent{{}}
	service := NewNormalizationService(repository, testRegistry{connector: connector}, &testCanonicalSink{}, time.Now)

	processed, err := service.ProcessBatch(context.Background(), 10)
	if err != nil || processed != 1 || len(repository.failed) != 1 || len(repository.completed) != 0 {
		t.Fatalf("ProcessBatch() = %d, %v; completed=%v failed=%v", processed, err, repository.completed, repository.failed)
	}
}

func TestNormalizationKeepsWorkPendingOnTemporarySinkFailure(t *testing.T) {
	repository, connector, item := normalizationFixture(t)
	connector.normalized = []domain.CanonicalEvent{canonicalFixture(item)}
	wantErr := errors.New("временная ошибка PostgreSQL")
	service := NewNormalizationService(repository, testRegistry{connector: connector}, &testCanonicalSink{err: wantErr}, time.Now)

	processed, err := service.ProcessBatch(context.Background(), 10)
	if !errors.Is(err, wantErr) || processed != 0 || len(repository.completed) != 0 || len(repository.failed) != 0 {
		t.Fatalf("ProcessBatch() = %d, %v; completed=%v failed=%v", processed, err, repository.completed, repository.failed)
	}
}

func normalizationFixture(t *testing.T) (*testRepository, *testConnector, domain.NormalizationItem) {
	t.Helper()
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	health := domain.ConnectionHealth{Status: domain.ConnectionActive, CheckedAt: now}
	connection, err := domain.NewChannelConnection(
		"connection", "tenant", nil, domain.ProviderTest, "Тест", []domain.Capability{domain.CapabilityReceiveMessages},
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", health, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := domain.NewRawEvent(
		"raw", "tenant", "connection", domain.ProviderTest, "external", []byte(`{"fixture":true}`),
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		domain.RawEventReceived, nil, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	item := domain.NormalizationItem{Connection: connection, Event: raw}
	repository := newTestRepository()
	repository.pending = []domain.NormalizationItem{item}
	return repository, &testConnector{}, item
}

func canonicalFixture(item domain.NormalizationItem) domain.CanonicalEvent {
	text := "Здравствуйте"
	return domain.CanonicalEvent{
		SourceEventID: "external", Type: domain.CanonicalMessageReceived,
		TenantID: item.Event.TenantID, ConnectionID: item.Connection.ID, Provider: item.Connection.Provider,
		ConversationExternalID: "dialog", MessageExternalID: "message", ContactExternalID: "contact",
		Direction: domain.CanonicalIncoming, MessageType: domain.CanonicalText, Text: &text,
		SentAt: item.Event.ReceivedAt, OccurredAt: item.Event.ReceivedAt, ReceivedAt: item.Event.ReceivedAt,
		Attachments: []domain.CanonicalAttachment{}, Metadata: []byte(`{}`),
	}
}
