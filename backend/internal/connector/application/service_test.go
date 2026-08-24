package application

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	verifyErr      error
	identifierErr  error
	normalizeCalls int
}

func (*testConnector) Provider() domain.Provider { return domain.ProviderTest }
func (connector *testConnector) VerifyEvent(context.Context, domain.ChannelConnection, []byte, domain.Headers) error {
	return connector.verifyErr
}
func (connector *testConnector) ExternalEventID([]byte, domain.Headers) (string, error) {
	return "external-1", connector.identifierErr
}
func (connector *testConnector) NormalizeEvent(context.Context, domain.ChannelConnection, domain.RawEvent) ([]domain.CanonicalEvent, error) {
	connector.normalizeCalls++
	return nil, nil
}
func (*testConnector) Health(context.Context, domain.ChannelConnection) domain.ConnectionHealth {
	return domain.ConnectionHealth{Status: domain.ConnectionActive, CheckedAt: time.Now()}
}

type testRegistry struct{ connector *testConnector }

func (registry testRegistry) Lookup(provider domain.Provider) (domain.ConnectorRegistration, bool) {
	if provider != domain.ProviderTest {
		return domain.ConnectorRegistration{}, false
	}
	return domain.ConnectorRegistration{
		Connector: registry.connector, Capabilities: []domain.Capability{domain.CapabilityReceiveMessages},
	}, true
}

type testRepository struct {
	connections map[string]domain.ChannelConnection
	events      []domain.RawEvent
	works       []*domain.NormalizationWork
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

func TestReceivePersistsBeforeNormalization(t *testing.T) {
	repository := newTestRepository()
	connector := &testConnector{}
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	service := NewService(repository, testAuthorizer(true), testRegistry{connector}, &testIDs{}, func() time.Time { return now })
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
	service := NewService(repository, testAuthorizer(true), testRegistry{connector}, &testIDs{}, time.Now)
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
	service := NewService(repository, testAuthorizer(true), testRegistry{connector}, &testIDs{}, time.Now)
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
	service := NewService(newTestRepository(), testAuthorizer(false), testRegistry{&testConnector{}}, &testIDs{}, time.Now)
	if _, err := service.Connect(context.Background(), "manager", "tenant", ConnectCommand{
		Provider: "TEST", Name: "Fixture", WebhookSecret: "fixture-secret-123",
	}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Connect() error = %v", err)
	}
}
