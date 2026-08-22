// Package application coordinates durable notification and delivery ports.
package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"lidradar/backend/internal/notification/domain"
)

var (
	ErrNotFound       = errors.New("notification resource not found")
	ErrUnsafeCallback = errors.New("unsafe telegram callback")
)

type Store interface {
	// Create atomically inserts the logical notification and its initial
	// delivery. Implementations enforce UNIQUE(tenant_id, dedup_key).
	Create(ctx context.Context, notification domain.Notification, delivery domain.Delivery) (domain.Notification, bool, error)
	Due(ctx context.Context, at time.Time, limit int) ([]domain.Delivery, error)
	Complete(ctx context.Context, delivery domain.Delivery, retry *domain.Delivery) error
}

type LinkStore interface {
	TelegramDestination(ctx context.Context, tenantID, userID string) (string, bool, error)
}

type Transport interface {
	Send(ctx context.Context, destination, title, body, callbackData string) (providerMessageID string, retryable bool, err error)
}

type IDs interface{ NewID() string }

type Service struct {
	store    Store
	links    LinkStore
	telegram Transport
	ids      IDs
	now      func() time.Time
}

func NewService(store Store, links LinkStore, telegram Transport, ids IDs, now func() time.Time) Service {
	return Service{store: store, links: links, telegram: telegram, ids: ids, now: now}
}

// NotifyRisk persists intent before any external request. Replays return the
// existing logical notification and do not enqueue a duplicate delivery.
func (s Service) NotifyRisk(ctx context.Context, tenantID, userID, riskID, title, body string) (domain.Notification, bool, error) {
	if s.store == nil || s.links == nil || s.ids == nil || s.now == nil {
		return domain.Notification{}, false, domain.ErrInvalid
	}
	destination, ok, err := s.links.TelegramDestination(ctx, tenantID, userID)
	if err != nil {
		return domain.Notification{}, false, err
	}
	if !ok {
		return domain.Notification{}, false, ErrNotFound
	}
	n, err := domain.NewNotification(s.ids.NewID(), tenantID, userID, riskID, title, body, s.now())
	if err != nil {
		return domain.Notification{}, false, err
	}
	d, err := domain.NewDelivery(s.ids.NewID(), n, destination, domain.ChannelTelegram, n.CreatedAt)
	if err != nil {
		return domain.Notification{}, false, err
	}
	return s.store.Create(ctx, n, d)
}

var backoff = []time.Duration{0, 5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// DispatchDue handles durable outbox deliveries. Provider failures never
// mutate or resolve the associated Risk.
func (s Service) DispatchDue(ctx context.Context, limit int) error {
	if s.store == nil || s.telegram == nil || s.ids == nil || s.now == nil || limit < 1 {
		return domain.ErrInvalid
	}
	now := s.now().UTC()
	deliveries, err := s.store.Due(ctx, now, limit)
	if err != nil {
		return err
	}
	for _, d := range deliveries {
		messageID, retryable, sendErr := s.telegram.Send(ctx, d.Destination, d.Title, d.Body, "OPEN_RISK:"+d.NotificationID)
		attempted := now
		d.AttemptedAt = &attempted
		var retry *domain.Delivery
		if sendErr == nil {
			d.Status, d.ProviderMessageID = domain.DeliverySucceeded, messageID
		} else {
			d.FailureCode = "provider_error"
			if retryable && d.Attempt < len(backoff) {
				d.Status = domain.DeliveryRetry
				next := d
				next.ID, next.Attempt = s.ids.NewID(), d.Attempt+1
				next.Status, next.AttemptedAt, next.FailureCode = domain.DeliveryPending, nil, ""
				next.NextAttemptAt = now.Add(backoff[next.Attempt-1])
				retry = &next
			} else {
				d.Status = domain.DeliveryDead
			}
		}
		if err := s.store.Complete(ctx, d, retry); err != nil {
			return err
		}
	}
	return nil
}

type CallbackAction string

const (
	CallbackOpen        CallbackAction = "OPEN_RISK"
	CallbackAcknowledge CallbackAction = "ACKNOWLEDGE"
	CallbackSnooze      CallbackAction = "SNOOZE"
)

type CallbackCommand struct {
	TenantID, UserID, IdempotencyKey, RiskID string
	Action                                   CallbackAction
}
type CallbackExecutor interface {
	ExecuteSafeCallback(context.Context, CallbackCommand) error
}

// HandleCallback accepts only the explicitly safe, idempotent command set.
func HandleCallback(ctx context.Context, executor CallbackExecutor, command CallbackCommand) error {
	if executor == nil || command.TenantID == "" || command.UserID == "" || command.RiskID == "" || command.IdempotencyKey == "" {
		return ErrUnsafeCallback
	}
	switch command.Action {
	case CallbackOpen, CallbackAcknowledge, CallbackSnooze:
	default:
		return fmt.Errorf("%w: action", ErrUnsafeCallback)
	}
	return executor.ExecuteSafeCallback(ctx, command)
}
