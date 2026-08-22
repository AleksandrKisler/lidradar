// Package domain owns notification facts and delivery-attempt invariants.
package domain

import (
	"errors"
	"time"
)

var ErrInvalid = errors.New("invalid notification")

type Channel string

const (
	ChannelInApp    Channel = "IN_APP"
	ChannelTelegram Channel = "TELEGRAM"
)

type DeliveryStatus string

const (
	DeliveryPending   DeliveryStatus = "PENDING"
	DeliverySucceeded DeliveryStatus = "SUCCEEDED"
	DeliveryRetry     DeliveryStatus = "RETRY"
	DeliveryDead      DeliveryStatus = "DEAD"
)

// Notification is the deduplicated, user-visible fact. Delivery retries never
// create another Notification with the same tenant and DedupKey.
type Notification struct {
	ID, TenantID, UserID, RiskID, DedupKey string
	Title, Body                            string
	CreatedAt                              time.Time
}

func NewNotification(id, tenantID, userID, riskID, title, body string, at time.Time) (Notification, error) {
	if id == "" || tenantID == "" || userID == "" || riskID == "" || title == "" || body == "" || at.IsZero() {
		return Notification{}, ErrInvalid
	}
	return Notification{ID: id, TenantID: tenantID, UserID: userID, RiskID: riskID,
		DedupKey: "risk:" + riskID + ":opened", Title: title, Body: body, CreatedAt: at.UTC()}, nil
}

// Delivery records one concrete transport attempt.
type Delivery struct {
	ID, NotificationID, TenantID, Destination string
	Title, Body                               string
	Channel                                   Channel
	Attempt                                   int
	Status                                    DeliveryStatus
	NextAttemptAt                             time.Time
	AttemptedAt                               *time.Time
	ProviderMessageID                         string
	FailureCode                               string
}

func NewDelivery(id string, notification Notification, destination string, channel Channel, at time.Time) (Delivery, error) {
	if id == "" || notification.ID == "" || destination == "" || at.IsZero() || (channel != ChannelInApp && channel != ChannelTelegram) {
		return Delivery{}, ErrInvalid
	}
	return Delivery{ID: id, NotificationID: notification.ID, TenantID: notification.TenantID,
		Destination: destination, Title: notification.Title, Body: notification.Body, Channel: channel,
		Attempt: 1, Status: DeliveryPending, NextAttemptAt: at.UTC()}, nil
}
