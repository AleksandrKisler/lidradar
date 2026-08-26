// Package domain описывает логические уведомления и отдельные попытки доставки.
package domain

import (
	"errors"
	"strings"
	"time"
)

var ErrInvalid = errors.New("некорректное уведомление")

type Channel string

const (
	ChannelInApp    Channel = "IN_APP"
	ChannelTelegram Channel = "TELEGRAM"
)

type DeliveryStatus string

const (
	DeliveryPending    DeliveryStatus = "PENDING"
	DeliveryProcessing DeliveryStatus = "PROCESSING"
	DeliverySucceeded  DeliveryStatus = "SUCCEEDED"
	DeliveryRetry      DeliveryStatus = "RETRY"
	DeliveryDead       DeliveryStatus = "DEAD"
)

// Notification — один пользовательский факт. Повтор события или доставки не
// создаёт второй факт с тем же tenant_id и dedup_key.
type Notification struct {
	ID, TenantID, UserID, RiskID, DedupKey string
	Title, Body                            string
	SnoozedAt                              *time.Time
	CreatedAt, UpdatedAt                   time.Time
}

func NewNotification(id, tenantID, userID, riskID, title, body string, at time.Time) (Notification, error) {
	notification := Notification{
		ID: id, TenantID: tenantID, UserID: userID, RiskID: riskID,
		DedupKey: "risk:" + riskID + ":opened", Title: strings.TrimSpace(title), Body: strings.TrimSpace(body),
		CreatedAt: at.UTC(), UpdatedAt: at.UTC(),
	}
	if notification.Validate() != nil {
		return Notification{}, ErrInvalid
	}
	return notification, nil
}

func (notification Notification) Validate() error {
	if notification.ID == "" || notification.TenantID == "" || notification.UserID == "" || notification.RiskID == "" ||
		notification.DedupKey != "risk:"+notification.RiskID+":opened" ||
		len(notification.Title) < 1 || len(notification.Title) > 200 ||
		len(notification.Body) < 1 || len(notification.Body) > 2000 ||
		notification.CreatedAt.IsZero() || notification.UpdatedAt.IsZero() || notification.UpdatedAt.Before(notification.CreatedAt) {
		return ErrInvalid
	}
	return nil
}

// Delivery — снимок одной конкретной попытки внешнего транспорта. Destination,
// заголовок и текст не перечитываются из изменяемых настроек при повторе.
type Delivery struct {
	ID, NotificationID, TenantID, Destination string
	Title, Body                               string
	Channel                                   Channel
	Attempt                                   int
	Status                                    DeliveryStatus
	AvailableAt                               time.Time
	LeaseOwner                                *string
	LeaseUntil                                *time.Time
	AttemptedAt                               *time.Time
	ProviderMessageID                         string
	FailureCode                               string
	CreatedAt, UpdatedAt                      time.Time
}

func NewDelivery(id string, notification Notification, destination string, channel Channel, at time.Time) (Delivery, error) {
	delivery := Delivery{
		ID: id, NotificationID: notification.ID, TenantID: notification.TenantID,
		Destination: strings.TrimSpace(destination), Title: notification.Title, Body: notification.Body,
		Channel: channel, Attempt: 1, Status: DeliveryPending, AvailableAt: at.UTC(),
		CreatedAt: at.UTC(), UpdatedAt: at.UTC(),
	}
	if delivery.Validate() != nil {
		return Delivery{}, ErrInvalid
	}
	return delivery, nil
}

func (delivery Delivery) Validate() error {
	if delivery.ID == "" || delivery.NotificationID == "" || delivery.TenantID == "" ||
		delivery.Destination == "" || len(delivery.Destination) > 255 ||
		len(delivery.Title) < 1 || len(delivery.Title) > 200 || len(delivery.Body) < 1 || len(delivery.Body) > 2000 ||
		(delivery.Channel != ChannelInApp && delivery.Channel != ChannelTelegram) ||
		delivery.Attempt < 1 || delivery.Attempt > 5 || delivery.AvailableAt.IsZero() ||
		delivery.CreatedAt.IsZero() || delivery.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	switch delivery.Status {
	case DeliveryPending:
		if delivery.LeaseOwner != nil || delivery.LeaseUntil != nil || delivery.AttemptedAt != nil ||
			delivery.ProviderMessageID != "" || delivery.FailureCode != "" {
			return ErrInvalid
		}
	case DeliveryRetry:
		if delivery.LeaseOwner != nil || delivery.LeaseUntil != nil || delivery.AttemptedAt == nil ||
			delivery.ProviderMessageID != "" || delivery.FailureCode == "" {
			return ErrInvalid
		}
	case DeliveryProcessing:
		if delivery.LeaseOwner == nil || strings.TrimSpace(*delivery.LeaseOwner) == "" || delivery.LeaseUntil == nil ||
			delivery.AttemptedAt != nil || delivery.ProviderMessageID != "" || delivery.FailureCode != "" {
			return ErrInvalid
		}
	case DeliverySucceeded:
		if delivery.LeaseOwner != nil || delivery.LeaseUntil != nil || delivery.AttemptedAt == nil ||
			delivery.ProviderMessageID == "" || delivery.FailureCode != "" {
			return ErrInvalid
		}
	case DeliveryDead:
		if delivery.LeaseOwner != nil || delivery.LeaseUntil != nil || delivery.AttemptedAt == nil ||
			delivery.ProviderMessageID != "" || delivery.FailureCode == "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
