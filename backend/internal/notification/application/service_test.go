package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"lidradar/backend/internal/notification/application"
	"lidradar/backend/internal/notification/domain"
)

type ids struct{ n int }

func (i *ids) NewID() string { i.n++; return string(rune('a' + i.n)) }

type links struct{ destination string }

func (l links) TelegramDestination(context.Context, string, string) (string, bool, error) {
	return l.destination, l.destination != "", nil
}

type memory struct {
	notification domain.Notification
	deliveries   []domain.Delivery
}

func (m *memory) Create(_ context.Context, n domain.Notification, d domain.Delivery) (domain.Notification, bool, error) {
	if m.notification.DedupKey == n.DedupKey {
		return m.notification, false, nil
	}
	m.notification = n
	m.deliveries = append(m.deliveries, d)
	return n, true, nil
}
func (m *memory) Due(_ context.Context, at time.Time, _ int) ([]domain.Delivery, error) {
	var due []domain.Delivery
	for _, d := range m.deliveries {
		if d.Status == domain.DeliveryPending && !d.NextAttemptAt.After(at) {
			due = append(due, d)
		}
	}
	return due, nil
}
func (m *memory) Complete(_ context.Context, d domain.Delivery, retry *domain.Delivery) error {
	for i := range m.deliveries {
		if m.deliveries[i].ID == d.ID {
			m.deliveries[i] = d
		}
	}
	if retry != nil {
		m.deliveries = append(m.deliveries, *retry)
	}
	return nil
}

type transport struct {
	calls     int
	failures  int
	retryable bool
}

func (t *transport) Send(context.Context, string, string, string, string) (string, bool, error) {
	t.calls++
	if t.calls <= t.failures {
		return "", t.retryable, errors.New("down")
	}
	return "42", false, nil
}

func TestNotifyRiskDeduplicatesLogicalAlert(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	store := new(memory)
	id := new(ids)
	svc := application.NewService(store, links{"chat"}, new(transport), id, func() time.Time { return now })
	first, created, err := svc.NotifyRisk(context.Background(), "tenant", "owner", "risk-1", "Critical", "Reply now")
	if err != nil || !created {
		t.Fatalf("first notification: created=%v err=%v", created, err)
	}
	second, created, err := svc.NotifyRisk(context.Background(), "tenant", "owner", "risk-1", "Critical", "Reply now")
	if err != nil || created || second.ID != first.ID || len(store.deliveries) != 1 {
		t.Fatalf("replay duplicated notification or delivery")
	}
}

func TestTelegramDownRetriesWithoutDuplicatingNotification(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	store := new(memory)
	id := new(ids)
	tx := &transport{failures: 1, retryable: true}
	svc := application.NewService(store, links{"chat"}, tx, id, func() time.Time { return now })
	if _, _, err := svc.NotifyRisk(context.Background(), "tenant", "owner", "risk-1", "Critical", "Reply now"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DispatchDue(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if len(store.deliveries) != 2 || store.deliveries[0].Status != domain.DeliveryRetry || len([]domain.Notification{store.notification}) != 1 {
		t.Fatalf("unexpected retry state: %#v", store.deliveries)
	}
	now = now.Add(5 * time.Second)
	if err := svc.DispatchDue(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if store.deliveries[1].Status != domain.DeliverySucceeded || tx.calls != 2 {
		t.Fatalf("retry not delivered: %#v", store.deliveries[1])
	}
}

type callback struct{ called bool }

func (c *callback) ExecuteSafeCallback(context.Context, application.CallbackCommand) error {
	c.called = true
	return nil
}
func TestCallbacksAllowOnlySafeActions(t *testing.T) {
	c := new(callback)
	base := application.CallbackCommand{TenantID: "t", UserID: "u", RiskID: "r", IdempotencyKey: "key"}
	base.Action = application.CallbackAcknowledge
	if err := application.HandleCallback(context.Background(), c, base); err != nil || !c.called {
		t.Fatal("safe callback rejected")
	}
	c.called = false
	base.Action = "CONFIRM_REVENUE"
	if !errors.Is(application.HandleCallback(context.Background(), c, base), application.ErrUnsafeCallback) || c.called {
		t.Fatal("unsafe callback accepted")
	}
}
