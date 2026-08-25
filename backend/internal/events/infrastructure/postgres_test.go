package infrastructure

import (
	"context"
	"errors"
	"testing"
	"time"

	eventsapplication "lidradar/backend/internal/events/application"
	"lidradar/backend/internal/events/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

func TestOutboxLeaseRecoveryAndIdempotentAppend(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	store := NewPostgresStore(pool)
	now := time.Date(2026, 8, 25, 16, 0, 0, 0, time.UTC)
	event := newEvent(t, tenant.TenantID, tenant.LocationID, now)
	if _, inserted, err := store.Append(ctx, event); err != nil || !inserted {
		t.Fatalf("Append() = inserted %v, error %v", inserted, err)
	}
	if _, inserted, err := store.Append(ctx, event); err != nil || inserted {
		t.Fatalf("duplicate Append() = inserted %v, error %v", inserted, err)
	}
	conflicting := event
	conflicting.Data = []byte(`{"changed":true}`)
	if _, _, err := store.Append(ctx, conflicting); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("conflicting Append() error = %v", err)
	}

	first, err := store.Claim(ctx, "outbox-1", now, now.Add(time.Minute), 1)
	if err != nil || len(first) != 1 || first[0].AttemptCount != 1 {
		t.Fatalf("first Claim() = %#v, %v", first, err)
	}
	if early, err := store.Claim(ctx, "outbox-2", now.Add(30*time.Second), now.Add(2*time.Minute), 1); err != nil || len(early) != 0 {
		t.Fatalf("early Claim() = %#v, %v", early, err)
	}
	recoveredAt := now.Add(2 * time.Minute)
	second, err := store.Claim(ctx, "outbox-2", recoveredAt, recoveredAt.Add(time.Minute), 1)
	if err != nil || len(second) != 1 || second[0].ID != event.ID || second[0].AttemptCount != 2 {
		t.Fatalf("recovery Claim() = %#v, %v", second, err)
	}
	if err := store.Publish(ctx, event.ID, "outbox-1", recoveredAt.Add(time.Second)); !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("старый владелец Publish() error = %v", err)
	}
	if err := store.Publish(ctx, event.ID, "outbox-2", recoveredAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestDispatcherMovesUnsupportedEventToDead(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	store := NewPostgresStore(pool)
	now := time.Date(2026, 8, 25, 17, 0, 0, 0, time.UTC)
	event := newEvent(t, tenant.TenantID, tenant.LocationID, now)
	if _, _, err := store.Append(ctx, event); err != nil {
		t.Fatal(err)
	}
	dispatcher := eventsapplication.NewDispatcher(store, "dispatcher", nil, func() time.Time { return now }, time.Minute)
	processed, err := dispatcher.RunOne(ctx)
	if err != nil || !processed {
		t.Fatalf("RunOne() = %v, %v", processed, err)
	}
	var status domain.Status
	var code string
	if err := pool.QueryRow(ctx, `SELECT status, last_error_code FROM outbox_events WHERE id = $1`, event.ID).Scan(&status, &code); err != nil {
		t.Fatal(err)
	}
	if status != domain.StatusDead || code != "UNSUPPORTED_EVENT_TYPE" {
		t.Fatalf("event status=%s code=%s", status, code)
	}
}

func newEvent(t *testing.T, tenantID, aggregateID string, at time.Time) domain.Event {
	t.Helper()
	id, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	event, err := domain.NewEvent(
		id, "fixture.created", 1, tenantID, "location", aggregateID, id,
		[]byte(`{"fixture":true}`), at,
	)
	if err != nil {
		t.Fatal(err)
	}
	return event
}
