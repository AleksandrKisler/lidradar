// Package application координирует создание уведомлений и устойчивую доставку.
package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	eventsapplication "lidradar/backend/internal/events/application"
	eventsdomain "lidradar/backend/internal/events/domain"
	jobsdomain "lidradar/backend/internal/jobs/domain"
	"lidradar/backend/internal/notification/domain"
	riskdomain "lidradar/backend/internal/risk/domain"
)

var (
	ErrNotFound       = errors.New("ресурс уведомления не найден")
	ErrForbidden      = errors.New("нет разрешения на уведомления")
	ErrUnsafeCallback = errors.New("небезопасная команда Telegram")
	ErrLeaseLost      = errors.New("аренда доставки потеряна")
)

const (
	RiskOpenedEventType  = "risk.opened.v1"
	DefaultDeliveryLease = 30 * time.Second
	DefaultLinkTokenTTL  = 15 * time.Minute
)

type Store interface {
	// Create атомарно сохраняет логический факт и первую попытку. Уникальный
	// (tenant_id, dedup_key) превращает повтор события в безопасное чтение.
	Create(context.Context, domain.Notification, domain.Delivery) (domain.Notification, bool, error)
	ClaimDue(context.Context, string, time.Time, time.Time, int) ([]domain.Delivery, error)
	Complete(context.Context, string, domain.Delivery, *domain.Delivery) error
}

type LinkStore interface {
	TelegramDestination(context.Context, string, string) (string, bool, error)
}

type RecipientStore interface {
	TelegramOwner(context.Context, string) (userID, destination string, found bool, err error)
}

type Transport interface {
	Send(context.Context, string, string, string, string) (providerMessageID string, retryable bool, err error)
}

type IDs interface{ NewID() (string, error) }

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

// NotifyRisk фиксирует намерение до внешнего запроса. Повтор возвращает уже
// существующее логическое уведомление и не создаёт новую доставку.
func (service Service) NotifyRisk(
	ctx context.Context,
	tenantID, userID, riskID, title, body string,
) (domain.Notification, bool, error) {
	if service.store == nil || service.links == nil || service.ids == nil || service.now == nil {
		return domain.Notification{}, false, domain.ErrInvalid
	}
	destination, found, err := service.links.TelegramDestination(ctx, tenantID, userID)
	if err != nil {
		return domain.Notification{}, false, err
	}
	if !found {
		return domain.Notification{}, false, ErrNotFound
	}
	notificationID, err := service.ids.NewID()
	if err != nil {
		return domain.Notification{}, false, err
	}
	deliveryID, err := service.ids.NewID()
	if err != nil {
		return domain.Notification{}, false, err
	}
	now := service.now().UTC()
	notification, err := domain.NewNotification(notificationID, tenantID, userID, riskID, title, body, now)
	if err != nil {
		return domain.Notification{}, false, err
	}
	delivery, err := domain.NewDelivery(deliveryID, notification, destination, domain.ChannelTelegram, now)
	if err != nil {
		return domain.Notification{}, false, err
	}
	return service.store.Create(ctx, notification, delivery)
}

// DispatchOne арендует и выполняет не более одной попытки. Провайдер никак не
// меняет Risk: успех и отказ отражаются только в NotificationDelivery.
func (service Service) DispatchOne(ctx context.Context, owner string, lease time.Duration) (bool, error) {
	if service.store == nil || service.telegram == nil || service.ids == nil || service.now == nil || owner == "" {
		return false, domain.ErrInvalid
	}
	if lease <= 0 {
		lease = DefaultDeliveryLease
	}
	now := service.now().UTC()
	deliveries, err := service.store.ClaimDue(ctx, owner, now, now.Add(lease), 1)
	if err != nil || len(deliveries) == 0 {
		return false, err
	}
	delivery := deliveries[0]
	messageID, retryable, sendErr := service.telegram.Send(
		ctx, delivery.Destination, delivery.Title, delivery.Body, delivery.NotificationID,
	)
	finishedAt := service.now().UTC()
	delivery.LeasedBy, delivery.LeaseUntil = nil, nil
	delivery.AttemptedAt = &finishedAt
	delivery.UpdatedAt = finishedAt
	var retry *domain.Delivery
	if sendErr == nil {
		delivery.Status = domain.DeliverySucceeded
		delivery.ProviderMessageID = messageID
	} else if retryable && delivery.Attempt < 5 {
		delivery.Status = domain.DeliveryRetry
		delivery.FailureCode = "TELEGRAM_PROVIDER_ERROR"
		retryID, idErr := service.ids.NewID()
		if idErr != nil {
			return true, idErr
		}
		next := delivery
		next.ID = retryID
		next.Attempt++
		next.Status = domain.DeliveryPending
		next.AvailableAt = finishedAt.Add(jobsdomain.RetryDelay(delivery.Attempt))
		next.AttemptedAt = nil
		next.ProviderMessageID = ""
		next.FailureCode = ""
		next.CreatedAt, next.UpdatedAt = finishedAt, finishedAt
		retry = &next
	} else {
		delivery.Status = domain.DeliveryDead
		delivery.FailureCode = "TELEGRAM_PROVIDER_ERROR"
	}
	if err := service.store.Complete(ctx, owner, delivery, retry); err != nil {
		return true, err
	}
	return true, nil
}

type riskOpenedData struct {
	RiskID        string              `json:"riskId"`
	OpportunityID string              `json:"opportunityId"`
	LocationID    string              `json:"locationId"`
	Type          riskdomain.Type     `json:"type"`
	Severity      riskdomain.Severity `json:"severity"`
}

// RiskOpenedEventHandler создаёт немедленное Telegram-уведомление для рисков,
// которым ТЗ назначает немедленную доставку.
func RiskOpenedEventHandler(service Service, recipients RecipientStore) eventsapplication.Handler {
	return func(ctx context.Context, event eventsdomain.Event) error {
		if recipients == nil {
			return jobsdomain.Permanent("NOTIFICATION_CONFIGURATION_INVALID", errors.New("получатели не настроены"))
		}
		var data riskOpenedData
		if json.Unmarshal(event.Data, &data) != nil || event.AggregateType != "risk" ||
			event.AggregateID == "" || data.RiskID != event.AggregateID || data.OpportunityID == "" || data.LocationID == "" ||
			(data.Type != riskdomain.TypeNoResponse && data.Type != riskdomain.TypeBookingNotConfirmed) ||
			(data.Severity != riskdomain.SeverityHigh && data.Severity != riskdomain.SeverityCritical) {
			return jobsdomain.Permanent("INVALID_RISK_OPENED_EVENT", errors.New("некорректное событие открытия риска"))
		}
		userID, _, found, err := recipients.TelegramOwner(ctx, event.TenantID)
		if err != nil {
			return jobsdomain.Retryable("NOTIFICATION_RECIPIENT_UNAVAILABLE", err)
		}
		if !found {
			return jobsdomain.Permanent("TELEGRAM_OWNER_NOT_LINKED", ErrNotFound)
		}
		title := "Риск: клиент ждёт ответа"
		body := "Ответьте клиенту как можно скорее. Откройте Radar для подробностей."
		if data.Type == riskdomain.TypeBookingNotConfirmed {
			if data.Severity != riskdomain.SeverityCritical {
				return jobsdomain.Permanent("INVALID_RISK_OPENED_EVENT", errors.New("неверная важность риска записи"))
			}
			title = "Критический риск: запись не подтверждена"
			body = "Предложите клиенту конкретный свободный слот. Откройте Radar для подробностей."
		} else if data.Severity == riskdomain.SeverityCritical {
			title = "Критический риск: клиент ждёт ответа"
		}
		_, _, err = service.NotifyRisk(
			ctx, event.TenantID, userID, data.RiskID,
			title, body,
		)
		if err == nil {
			return nil
		}
		if errors.Is(err, domain.ErrInvalid) || errors.Is(err, ErrNotFound) {
			return jobsdomain.Permanent("NOTIFICATION_INTENT_INVALID", err)
		}
		return jobsdomain.Retryable("NOTIFICATION_INTENT_UNAVAILABLE", err)
	}
}

type CallbackAction string

const (
	CallbackOpen        CallbackAction = "OPEN_RISK"
	CallbackAcknowledge CallbackAction = "ACKNOWLEDGE"
	CallbackSnooze      CallbackAction = "SNOOZE"
)

type CallbackCommand struct {
	TenantID, UserID, NotificationID, IdempotencyKey, RiskID string
	Action                                                   CallbackAction
}

type CallbackExecutor interface {
	ExecuteSafeCallback(context.Context, CallbackCommand) error
}

// HandleCallback пропускает только явно разрешённый идемпотентный набор.
func HandleCallback(ctx context.Context, executor CallbackExecutor, command CallbackCommand) error {
	if executor == nil || command.TenantID == "" || command.UserID == "" || command.NotificationID == "" ||
		command.RiskID == "" || command.IdempotencyKey == "" {
		return ErrUnsafeCallback
	}
	switch command.Action {
	case CallbackOpen, CallbackAcknowledge, CallbackSnooze:
	default:
		return fmt.Errorf("%w: действие", ErrUnsafeCallback)
	}
	return executor.ExecuteSafeCallback(ctx, command)
}
