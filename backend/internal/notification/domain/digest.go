package domain

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// DigestItem — элемент очереди отложенной доставки. Один риск попадает в
// очередь получателя не более одного раза (ТЗ §47); элемент считается
// доставленным, когда его забрала сводка слота.
type DigestItem struct {
	ID, TenantID, UserID, RiskID string
	RiskType                     RiskType
	Reason                       DeferReason
	Slot                         string
	DeliverAt                    time.Time
	InApp, Telegram              bool
	NotificationID               string
	ConsumedAt                   *time.Time
	CreatedAt                    time.Time
}

func NewDigestItem(id, tenantID, userID, riskID string, riskType RiskType, decision PolicyDecision, at time.Time) (DigestItem, error) {
	if !decision.Deliver || decision.Immediate() {
		return DigestItem{}, ErrInvalid
	}
	item := DigestItem{
		ID: id, TenantID: tenantID, UserID: userID, RiskID: riskID, RiskType: riskType,
		Reason: decision.Reason, Slot: decision.Slot, DeliverAt: decision.DeliverAt.UTC(),
		InApp: decision.InApp, Telegram: decision.Telegram, CreatedAt: at.UTC(),
	}
	if item.Validate() != nil {
		return DigestItem{}, ErrInvalid
	}
	return item, nil
}

func (item DigestItem) Validate() error {
	if item.ID == "" || item.TenantID == "" || item.UserID == "" || item.RiskID == "" || !ValidRiskType(item.RiskType) ||
		(item.Reason != DeferDigest && item.Reason != DeferQuietHours) || !ValidSlot(item.Slot) ||
		item.DeliverAt.IsZero() || (!item.InApp && !item.Telegram) || item.CreatedAt.IsZero() ||
		(item.NotificationID != "" && item.ConsumedAt == nil) ||
		(item.ConsumedAt != nil && item.ConsumedAt.Before(item.CreatedAt)) {
		return ErrInvalid
	}
	return nil
}

// DigestEntry — элемент очереди вместе с актуальным на момент отправки
// состоянием риска (LR-BE-RM-020).
type DigestEntry struct {
	Item       DigestItem
	Severity   Severity
	Status     string
	Contact    string
	DetectedAt time.Time
}

// Active сообщает, требует ли риск внимания прямо сейчас.
func (entry DigestEntry) Active() bool {
	switch entry.Status {
	case "OPEN", "ACKNOWLEDGED", "ACTED":
		return true
	default:
		return false
	}
}

const (
	digestMaxLines   = 15
	digestContactMax = 40
)

func RiskTypeLabel(riskType RiskType) string {
	switch riskType {
	case RiskNoResponse:
		return "клиент ждёт ответа"
	case RiskBookingNotConfirmed:
		return "запись не подтверждена"
	case RiskPromiseNotFulfilled:
		return "обещание не выполнено"
	case RiskCustomerSilentAfterPrice:
		return "клиент молчит после цены"
	case RiskFollowUpCandidate:
		return "стоит напомнить о себе"
	default:
		return string(riskType)
	}
}

func SeverityLabel(severity Severity) string {
	switch severity {
	case SeverityCritical:
		return "Критический"
	case SeverityHigh:
		return "Высокий"
	case SeverityMedium:
		return "Средний"
	case SeverityLow:
		return "Низкий"
	default:
		return string(severity)
	}
}

// ComposeDigest формирует заголовок и текст одной сводки в порядке,
// заданном вызывающей стороной. Текст умещается в ограничение схемы: не
// более 15 строк, остальные риски представлены счётчиком.
func ComposeDigest(entries []DigestEntry, location *time.Location) (string, string) {
	if location == nil {
		location = time.UTC
	}
	quietOnly := len(entries) > 0
	for _, entry := range entries {
		if entry.Item.Reason != DeferQuietHours {
			quietOnly = false
		}
	}
	title := fmt.Sprintf("Сводка рисков: %d", len(entries))
	if quietOnly {
		title = fmt.Sprintf("Риски за тихие часы: %d", len(entries))
	}
	var builder strings.Builder
	for index, entry := range entries {
		if index == digestMaxLines {
			fmt.Fprintf(&builder, "…и ещё %d.\n", len(entries)-index)
			break
		}
		contact := strings.TrimSpace(entry.Contact)
		if contact == "" {
			contact = "клиент"
		}
		if utf8.RuneCountInString(contact) > digestContactMax {
			contact = string([]rune(contact)[:digestContactMax]) + "…"
		}
		fmt.Fprintf(&builder, "%d. %s · %s: %s, с %s\n", index+1, SeverityLabel(entry.Severity),
			RiskTypeLabel(entry.Item.RiskType), contact, entry.DetectedAt.In(location).Format("02.01 15:04"))
	}
	builder.WriteString("\nОткройте Radar для подробностей.")
	return title, builder.String()
}
