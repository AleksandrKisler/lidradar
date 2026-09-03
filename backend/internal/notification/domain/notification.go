// Package domain описывает логические уведомления, отдельные попытки доставки
// и политику шумности: настройки получателя, тихие часы и сводки.
package domain

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

var ErrInvalid = errors.New("некорректное уведомление")

type Channel string

const (
	ChannelInApp    Channel = "IN_APP"
	ChannelTelegram Channel = "TELEGRAM"
)

func ValidChannel(channel Channel) bool {
	return channel == ChannelInApp || channel == ChannelTelegram
}

type DeliveryStatus string

const (
	DeliveryPending    DeliveryStatus = "PENDING"
	DeliveryProcessing DeliveryStatus = "PROCESSING"
	DeliverySucceeded  DeliveryStatus = "SUCCEEDED"
	DeliveryRetry      DeliveryStatus = "RETRY"
	DeliveryDead       DeliveryStatus = "DEAD"
)

// Kind — вид логического уведомления. Открытие риска и эскалация относятся к
// одному Risk; сводка объединяет несколько рисков одного получателя.
type Kind string

const (
	KindRiskOpened    Kind = "RISK_OPENED"
	KindRiskDigest    Kind = "RISK_DIGEST"
	KindRiskEscalated Kind = "RISK_ESCALATED"
)

func ValidKind(kind Kind) bool {
	switch kind {
	case KindRiskOpened, KindRiskDigest, KindRiskEscalated:
		return true
	default:
		return false
	}
}

// SlotLayout — локальный слот сводки в часовом поясе организации.
const SlotLayout = "2006-01-02T15:04"

var slotPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}$`)

// ValidSlot проверяет форму слота; календарная корректность даты не требуется,
// потому что слот всегда порождается из настоящего момента времени.
func ValidSlot(slot string) bool { return slotPattern.MatchString(slot) }

// Ключи дедупликации детерминированы (ТЗ §47) и персональны: один риск даёт
// один видимый факт каждому получателю, повтор события или доставки не
// создаёт второго.
func RiskOpenedDedupKey(riskID, userID string) string {
	return "risk:" + riskID + ":opened:user:" + userID
}

func RiskEscalatedDedupKey(riskID, userID string) string {
	return "risk:" + riskID + ":escalated:user:" + userID
}

func DigestDedupKey(userID, slot string) string {
	return "digest:user:" + userID + ":" + slot
}

// Notification — один пользовательский факт. Повтор события или доставки не
// создаёт второй факт с тем же tenant_id и dedup_key.
type Notification struct {
	ID, TenantID, UserID, RiskID, DedupKey string
	Kind                                   Kind
	Title, Body                            string
	SnoozedAt                              *time.Time
	CreatedAt, UpdatedAt                   time.Time
}

// NewNotification создаёт немедленное уведомление об открытии риска.
func NewNotification(id, tenantID, userID, riskID, title, body string, at time.Time) (Notification, error) {
	return newRiskNotification(KindRiskOpened, id, tenantID, userID, riskID, title, body, at)
}

// NewEscalation создаёт уведомление владельцу о риске без реакции.
func NewEscalation(id, tenantID, userID, riskID, title, body string, at time.Time) (Notification, error) {
	return newRiskNotification(KindRiskEscalated, id, tenantID, userID, riskID, title, body, at)
}

func newRiskNotification(kind Kind, id, tenantID, userID, riskID, title, body string, at time.Time) (Notification, error) {
	dedupKey := RiskOpenedDedupKey(riskID, userID)
	if kind == KindRiskEscalated {
		dedupKey = RiskEscalatedDedupKey(riskID, userID)
	}
	notification := Notification{
		ID: id, TenantID: tenantID, UserID: userID, RiskID: riskID, Kind: kind,
		DedupKey: dedupKey, Title: strings.TrimSpace(title), Body: strings.TrimSpace(body),
		CreatedAt: at.UTC(), UpdatedAt: at.UTC(),
	}
	if notification.Validate() != nil {
		return Notification{}, ErrInvalid
	}
	return notification, nil
}

// NewDigest создаёт сводку слота: один факт на получателя и слот.
func NewDigest(id, tenantID, userID, slot, title, body string, at time.Time) (Notification, error) {
	notification := Notification{
		ID: id, TenantID: tenantID, UserID: userID, Kind: KindRiskDigest,
		DedupKey: DigestDedupKey(userID, slot), Title: strings.TrimSpace(title), Body: strings.TrimSpace(body),
		CreatedAt: at.UTC(), UpdatedAt: at.UTC(),
	}
	if notification.Validate() != nil {
		return Notification{}, ErrInvalid
	}
	return notification, nil
}

func (notification Notification) Validate() error {
	if notification.ID == "" || notification.TenantID == "" || notification.UserID == "" || !ValidKind(notification.Kind) ||
		len(notification.Title) < 1 || len(notification.Title) > 200 ||
		len(notification.Body) < 1 || len(notification.Body) > 2000 ||
		notification.CreatedAt.IsZero() || notification.UpdatedAt.IsZero() || notification.UpdatedAt.Before(notification.CreatedAt) {
		return ErrInvalid
	}
	switch notification.Kind {
	case KindRiskOpened:
		if notification.RiskID == "" || notification.DedupKey != RiskOpenedDedupKey(notification.RiskID, notification.UserID) {
			return ErrInvalid
		}
	case KindRiskEscalated:
		if notification.RiskID == "" || notification.DedupKey != RiskEscalatedDedupKey(notification.RiskID, notification.UserID) {
			return ErrInvalid
		}
	case KindRiskDigest:
		if notification.RiskID != "" || !ValidSlot(notification.Slot()) {
			return ErrInvalid
		}
	}
	return nil
}

// Slot возвращает локальный слот сводки; для остальных видов — пустую строку.
func (notification Notification) Slot() string {
	if notification.Kind != KindRiskDigest {
		return ""
	}
	prefix := "digest:user:" + notification.UserID + ":"
	if !strings.HasPrefix(notification.DedupKey, prefix) {
		return ""
	}
	return strings.TrimPrefix(notification.DedupKey, prefix)
}

// Delivery — снимок одной конкретной попытки транспорта. Destination,
// заголовок и текст не перечитываются из изменяемых настроек при повторе.
// Kind нужен транспорту: сводка без единственного риска не получает кнопок.
type Delivery struct {
	ID, NotificationID, TenantID, Destination string
	Title, Body                               string
	Kind                                      Kind
	Channel                                   Channel
	Attempt                                   int
	Status                                    DeliveryStatus
	AvailableAt                               time.Time
	LeasedBy                                  *string
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
		Kind: notification.Kind, Channel: channel, Attempt: 1, Status: DeliveryPending, AvailableAt: at.UTC(),
		CreatedAt: at.UTC(), UpdatedAt: at.UTC(),
	}
	if delivery.Validate() != nil {
		return Delivery{}, ErrInvalid
	}
	return delivery, nil
}

// Actions сообщает, уместны ли кнопки управления одним риском.
func (delivery Delivery) Actions() bool { return delivery.Kind != KindRiskDigest }

func (delivery Delivery) Validate() error {
	if delivery.ID == "" || delivery.NotificationID == "" || delivery.TenantID == "" ||
		delivery.Destination == "" || len(delivery.Destination) > 255 ||
		len(delivery.Title) < 1 || len(delivery.Title) > 200 || len(delivery.Body) < 1 || len(delivery.Body) > 2000 ||
		!ValidKind(delivery.Kind) || !ValidChannel(delivery.Channel) ||
		delivery.Attempt < 1 || delivery.Attempt > 5 || delivery.AvailableAt.IsZero() ||
		delivery.CreatedAt.IsZero() || delivery.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	switch delivery.Status {
	case DeliveryPending:
		if delivery.LeasedBy != nil || delivery.LeaseUntil != nil || delivery.AttemptedAt != nil ||
			delivery.ProviderMessageID != "" || delivery.FailureCode != "" {
			return ErrInvalid
		}
	case DeliveryRetry:
		if delivery.LeasedBy != nil || delivery.LeaseUntil != nil || delivery.AttemptedAt == nil ||
			delivery.ProviderMessageID != "" || delivery.FailureCode == "" {
			return ErrInvalid
		}
	case DeliveryProcessing:
		if delivery.LeasedBy == nil || strings.TrimSpace(*delivery.LeasedBy) == "" || delivery.LeaseUntil == nil ||
			delivery.AttemptedAt != nil || delivery.ProviderMessageID != "" || delivery.FailureCode != "" {
			return ErrInvalid
		}
	case DeliverySucceeded:
		if delivery.LeasedBy != nil || delivery.LeaseUntil != nil || delivery.AttemptedAt == nil ||
			delivery.ProviderMessageID == "" || delivery.FailureCode != "" {
			return ErrInvalid
		}
	case DeliveryDead:
		if delivery.LeasedBy != nil || delivery.LeaseUntil != nil || delivery.AttemptedAt == nil ||
			delivery.ProviderMessageID != "" || delivery.FailureCode == "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
