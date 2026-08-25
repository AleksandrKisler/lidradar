package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"lidradar/backend/internal/connector/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

func TestPostgresRepositoryPersistsOnceAndKeepsTenantScope(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	repository := NewPostgresRepository(pool)
	generator := ids.Generator{}

	foreignLocation := newConnection(t, generator, pair.A.TenantID, &pair.B.LocationID, domain.ProviderGenericWebhook)
	if err := repository.CreateConnection(ctx, pair.A.TenantID, foreignLocation); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant location CreateConnection() error = %v", err)
	}

	connection := newConnection(t, generator, pair.A.TenantID, &pair.A.LocationID, domain.ProviderGenericWebhook)
	if err := repository.CreateConnection(ctx, pair.A.TenantID, connection); err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}
	if _, found, err := repository.Connection(ctx, pair.B.TenantID, connection.ID); err != nil || found {
		t.Fatalf("cross-tenant Connection() = found %v, error %v", found, err)
	}
	if _, found, err := repository.DisconnectConnection(ctx, pair.B.TenantID, connection.ID, time.Now()); err != nil || found {
		t.Fatalf("cross-tenant DisconnectConnection() = found %v, error %v", found, err)
	}

	payload := json.RawMessage(`{"id":"external-1","data":{"text":"fixture"}}`)
	var firstRawID string
	for requestNumber := 0; requestNumber < 10; requestNumber++ {
		event := newRawEvent(t, generator, connection, "external-1", payload, domain.RawEventReceived, nil)
		work := newWork(t, generator, event)
		result, err := repository.PersistEvent(ctx, pair.A.TenantID, connection.ID, event, &work, activeHealth())
		if err != nil {
			t.Fatalf("PersistEvent(%d) error = %v", requestNumber, err)
		}
		if requestNumber == 0 {
			if !result.Inserted {
				t.Fatal("first raw event was reported as a duplicate")
			}
			firstRawID = result.Event.ID
		} else if result.Inserted || result.Event.ID != firstRawID {
			t.Fatalf("duplicate result = %#v, first raw ID = %s", result, firstRawID)
		}
	}

	var rawCount, workCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM raw_events WHERE tenant_id = $1 AND connection_id = $2`, pair.A.TenantID, connection.ID).Scan(&rawCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM outbox_events event
		JOIN raw_events raw ON raw.id = event.aggregate_id
		WHERE event.tenant_id = $1 AND raw.connection_id = $2`, pair.A.TenantID, connection.ID).Scan(&workCount); err != nil {
		t.Fatal(err)
	}
	if rawCount != 1 || workCount != 1 {
		t.Fatalf("duplicate persistence created raw=%d, work=%d; want 1, 1", rawCount, workCount)
	}

	conflictingPayload := json.RawMessage(`{"id":"external-1","data":{"text":"different"}}`)
	conflicting := newRawEvent(t, generator, connection, "external-1", conflictingPayload, domain.RawEventReceived, nil)
	conflictingWork := newWork(t, generator, conflicting)
	if _, err := repository.PersistEvent(ctx, pair.A.TenantID, connection.ID, conflicting, &conflictingWork, activeHealth()); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("same external ID with another payload error = %v", err)
	}

	var storedHash string
	var encryptedIsNull bool
	if err := pool.QueryRow(ctx, `
		SELECT verification_secret_hash, encrypted_credentials IS NULL
		FROM channel_connections WHERE tenant_id = $1 AND id = $2`, pair.A.TenantID, connection.ID,
	).Scan(&storedHash, &encryptedIsNull); err != nil {
		t.Fatal(err)
	}
	if storedHash == "shared-fixture-secret" || storedHash != testDigest([]byte("shared-fixture-secret")) || !encryptedIsNull {
		t.Fatalf("credential persistence hash=%q encryptedIsNull=%v", storedHash, encryptedIsNull)
	}
}

func TestPostgresRepositoryInvalidPayloadUpdatesHealthWithoutWork(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	repository := NewPostgresRepository(pool)
	generator := ids.Generator{}
	connection := newConnection(t, generator, pair.A.TenantID, nil, domain.ProviderGenericWebhook)
	if err := repository.CreateConnection(ctx, pair.A.TenantID, connection); err != nil {
		t.Fatal(err)
	}

	errorCode := "INVALID_PAYLOAD"
	invalid := newRawEvent(
		t, generator, connection, "invalid-1", json.RawMessage(`{"encoding":"base64","raw":"bm90LWpzb24="}`),
		domain.RawEventFailed, &errorCode,
	)
	if result, err := repository.PersistEvent(ctx, pair.A.TenantID, connection.ID, invalid, nil, activeHealth()); err != nil || !result.Inserted {
		t.Fatalf("failed-event PersistEvent() = %#v, %v", result, err)
	}
	stored, found, err := repository.Connection(ctx, pair.A.TenantID, connection.ID)
	if err != nil || !found || stored.Status != domain.ConnectionError || stored.LastErrorCode == nil || *stored.LastErrorCode != errorCode {
		t.Fatalf("connection after invalid payload = %#v, found=%v, err=%v", stored, found, err)
	}

	valid := newRawEvent(t, generator, connection, "external-2", json.RawMessage(`{"id":"external-2","data":{}}`), domain.RawEventReceived, nil)
	work := newWork(t, generator, valid)
	if _, err := repository.PersistEvent(ctx, pair.A.TenantID, connection.ID, valid, &work, activeHealth()); err != nil {
		t.Fatal(err)
	}
	stored, found, err = repository.Connection(ctx, pair.A.TenantID, connection.ID)
	if err != nil || !found || stored.Status != domain.ConnectionActive || stored.LastErrorCode != nil {
		t.Fatalf("connection after valid payload = %#v, found=%v, err=%v", stored, found, err)
	}

	var rawCount, workCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM raw_events WHERE connection_id = $1`, connection.ID).Scan(&rawCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM outbox_events event
		JOIN raw_events raw ON raw.id = event.aggregate_id
		WHERE raw.connection_id = $1`, connection.ID).Scan(&workCount); err != nil {
		t.Fatal(err)
	}
	if rawCount != 2 || workCount != 1 {
		t.Fatalf("invalid/valid persistence created raw=%d, work=%d; want 2, 1", rawCount, workCount)
	}
}

func TestRawEventAndOutboxIntentAreOneTransaction(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	repository := NewPostgresRepository(pool)
	generator := ids.Generator{}
	connection := newConnection(t, generator, pair.A.TenantID, nil, domain.ProviderGenericWebhook)
	if err := repository.CreateConnection(ctx, pair.A.TenantID, connection); err != nil {
		t.Fatal(err)
	}
	first := newRawEvent(t, generator, connection, "atomic-1", json.RawMessage(`{"id":"atomic-1"}`), domain.RawEventReceived, nil)
	firstWork := newWork(t, generator, first)
	if _, err := repository.PersistEvent(ctx, pair.A.TenantID, connection.ID, first, &firstWork, activeHealth()); err != nil {
		t.Fatal(err)
	}
	second := newRawEvent(t, generator, connection, "atomic-2", json.RawMessage(`{"id":"atomic-2"}`), domain.RawEventReceived, nil)
	secondWork := newWork(t, generator, second)
	secondWork.ID = firstWork.ID // Принудительный конфликт исходящего журнала.
	if _, err := repository.PersistEvent(ctx, pair.A.TenantID, connection.ID, second, &secondWork, activeHealth()); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("PersistEvent() error = %v", err)
	}
	var rawCount, eventCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM raw_events WHERE connection_id = $1`, connection.ID).Scan(&rawCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, pair.A.TenantID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if rawCount != 1 || eventCount != 1 {
		t.Fatalf("после отката raw=%d outbox=%d, нужно 1 и 1", rawCount, eventCount)
	}
}

func TestPostgresRepositoryPersistsEncryptedCredentialsAndProvisioningHealth(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	repository := NewPostgresRepository(pool)
	connection := newConnection(
		t, ids.Generator{}, pair.A.TenantID, nil, domain.ProviderTelegramConnectedBusinessBot,
	)
	connection.EncryptedCredentials = []byte{1, 2, 3, 4, 5}
	if err := repository.CreateConnection(ctx, pair.A.TenantID, connection); err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	stored, found, err := repository.Connection(ctx, pair.A.TenantID, connection.ID)
	if err != nil || !found || string(stored.EncryptedCredentials) != string(connection.EncryptedCredentials) {
		t.Fatalf("Connection() = %#v, found=%v, err=%v", stored, found, err)
	}
	now := time.Now().UTC()
	health := domain.ConnectionHealth{Status: domain.ConnectionActive, LastSuccessAt: &now, CheckedAt: now}
	updated, found, err := repository.UpdateConnectionHealth(ctx, pair.A.TenantID, connection.ID, health)
	if err != nil || !found || updated.Status != domain.ConnectionActive || updated.LastSuccessAt == nil ||
		string(updated.EncryptedCredentials) != string(connection.EncryptedCredentials) {
		t.Fatalf("UpdateConnectionHealth() = %#v, found=%v, err=%v", updated, found, err)
	}
	if _, found, err := repository.UpdateConnectionHealth(ctx, pair.B.TenantID, connection.ID, health); err != nil || found {
		t.Fatalf("cross-tenant UpdateConnectionHealth() = found %v, error %v", found, err)
	}
}

func newConnection(
	t *testing.T,
	generator ids.Generator,
	tenantID string,
	locationID *string,
	provider domain.Provider,
) domain.ChannelConnection {
	t.Helper()
	id, err := generator.NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	connection, err := domain.NewChannelConnection(
		id, tenantID, locationID, provider, "Fixture connection",
		[]domain.Capability{domain.CapabilityReceiveMessages}, testDigest([]byte("shared-fixture-secret")),
		domain.ConnectionHealth{Status: domain.ConnectionActive, CheckedAt: now}, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func newRawEvent(
	t *testing.T,
	generator ids.Generator,
	connection domain.ChannelConnection,
	externalID string,
	payload json.RawMessage,
	status domain.RawEventStatus,
	errorCode *string,
) domain.RawEvent {
	t.Helper()
	id, err := generator.NewID()
	if err != nil {
		t.Fatal(err)
	}
	event, err := domain.NewRawEvent(
		id, connection.TenantID, connection.ID, connection.Provider, externalID, payload,
		testDigest(payload), status, errorCode, time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("new raw event %s: %v", externalID, err)
	}
	return event
}

func newWork(t *testing.T, generator ids.Generator, event domain.RawEvent) domain.NormalizationWork {
	t.Helper()
	id, err := generator.NewID()
	if err != nil {
		t.Fatal(err)
	}
	work, err := domain.NewNormalizationWork(id, event, time.Now().UTC())
	if err != nil {
		t.Fatalf("new work for %s: %v", event.ExternalEventID, err)
	}
	return work
}

func TestPostgresNormalizationLookupKeepsTenantScope(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	repository := NewPostgresRepository(pool)
	generator := ids.Generator{}
	first := newConnection(t, generator, pair.A.TenantID, nil, domain.ProviderGenericWebhook)
	if err := repository.CreateConnection(ctx, pair.A.TenantID, first); err != nil {
		t.Fatal(err)
	}
	event := newRawEvent(t, generator, first, "unpaired-event", json.RawMessage(`{"id":"unpaired-event"}`), domain.RawEventReceived, nil)
	work := newWork(t, generator, event)
	if _, err := repository.PersistEvent(ctx, pair.A.TenantID, first.ID, event, &work, activeHealth()); err != nil {
		t.Fatal(err)
	}
	if item, found, err := repository.Normalization(ctx, pair.A.TenantID, event.ID); err != nil || !found || item.Event.ID != event.ID {
		t.Fatalf("Normalization() = %#v, found=%v, error=%v", item, found, err)
	}
	if _, found, err := repository.Normalization(ctx, pair.B.TenantID, event.ID); err != nil || found {
		t.Fatalf("cross-tenant Normalization() = found=%v, error=%v", found, err)
	}
}
