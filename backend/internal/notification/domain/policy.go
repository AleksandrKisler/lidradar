package domain

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var ErrInvalidPreference = errors.New("некорректная настройка уведомлений")

// RiskType и Severity повторяют словарь модуля Risk (ТЗ §26, §46). Домен не
// импортирует чужие слои, а значения зафиксированы контрактом и схемой.
type RiskType string

const (
	RiskNoResponse               RiskType = "NO_RESPONSE"
	RiskBookingNotConfirmed      RiskType = "BOOKING_NOT_CONFIRMED"
	RiskPromiseNotFulfilled      RiskType = "PROMISE_NOT_FULFILLED"
	RiskCustomerSilentAfterPrice RiskType = "CUSTOMER_SILENT_AFTER_PRICE"
	RiskFollowUpCandidate        RiskType = "FOLLOW_UP_CANDIDATE"
)

// RiskTypes перечисляет типы в каноническом порядке ТЗ §46.
func RiskTypes() []RiskType {
	return []RiskType{
		RiskNoResponse, RiskBookingNotConfirmed, RiskPromiseNotFulfilled,
		RiskCustomerSilentAfterPrice, RiskFollowUpCandidate,
	}
}

func ValidRiskType(riskType RiskType) bool {
	for _, known := range RiskTypes() {
		if known == riskType {
			return true
		}
	}
	return false
}

type Severity string

const (
	SeverityLow      Severity = "LOW"
	SeverityMedium   Severity = "MEDIUM"
	SeverityHigh     Severity = "HIGH"
	SeverityCritical Severity = "CRITICAL"
)

// SeverityRank упорядочивает важность; ноль означает неизвестное значение.
func SeverityRank(severity Severity) int {
	switch severity {
	case SeverityLow:
		return 1
	case SeverityMedium:
		return 2
	case SeverityHigh:
		return 3
	case SeverityCritical:
		return 4
	default:
		return 0
	}
}

func ValidSeverity(severity Severity) bool { return SeverityRank(severity) > 0 }

// DeliveryMode — режим ТЗ §46: немедленно, сводкой или никак.
type DeliveryMode string

const (
	ModeImmediate DeliveryMode = "IMMEDIATE"
	ModeDigest    DeliveryMode = "DIGEST"
	ModeDisabled  DeliveryMode = "DISABLED"
)

func ValidDeliveryMode(mode DeliveryMode) bool {
	return mode == ModeImmediate || mode == ModeDigest || mode == ModeDisabled
}

// ClockTime — время суток с точностью до минуты в часовом поясе организации.
type ClockTime struct{ Hour, Minute int }

func ParseClockTime(value string) (ClockTime, error) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return ClockTime{}, ErrInvalidPreference
	}
	hour, hourErr := strconv.Atoi(parts[0])
	minute, minuteErr := strconv.Atoi(parts[1])
	clock := ClockTime{Hour: hour, Minute: minute}
	if hourErr != nil || minuteErr != nil || !clock.Valid() {
		return ClockTime{}, ErrInvalidPreference
	}
	return clock, nil
}

func (clock ClockTime) Valid() bool {
	return clock.Hour >= 0 && clock.Hour <= 23 && clock.Minute >= 0 && clock.Minute <= 59
}

func (clock ClockTime) String() string { return fmt.Sprintf("%02d:%02d", clock.Hour, clock.Minute) }

func (clock ClockTime) minutes() int { return clock.Hour*60 + clock.Minute }

// on возвращает это время суток в календарный день момента day.
func (clock ClockTime) on(day time.Time) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), clock.Hour, clock.Minute, 0, 0, day.Location())
}

// Значения по умолчанию: тихие часы 22:00–08:00 через полночь (LR-BE-RM-020)
// заполнены, но выключены; сводка приходит в 09:00 по времени организации.
var (
	DefaultQuietHoursStart = ClockTime{Hour: 22}
	DefaultQuietHoursEnd   = ClockTime{Hour: 8}
	DefaultDigestTime      = ClockTime{Hour: 9}
)

// Preference — настройка получателя для одного типа риска (ТЗ §3.7).
type Preference struct {
	ID, TenantID, UserID           string
	RiskType                       RiskType
	MinimumSeverity                Severity
	DeliveryMode                   DeliveryMode
	InAppEnabled, TelegramEnabled  bool
	QuietHoursEnabled              bool
	QuietHoursStart, QuietHoursEnd *ClockTime
	DigestTime                     ClockTime
	CreatedAt, UpdatedAt           time.Time
}

// DefaultDeliveryMode возвращает режим по умолчанию ТЗ §46.
func DefaultDeliveryMode(riskType RiskType) DeliveryMode {
	switch riskType {
	case RiskCustomerSilentAfterPrice, RiskFollowUpCandidate:
		return ModeDigest
	default:
		return ModeImmediate
	}
}

// DefaultPreference — неявная настройка: строка не хранится, поэтому ID пуст.
func DefaultPreference(tenantID, userID string, riskType RiskType) Preference {
	start, end := DefaultQuietHoursStart, DefaultQuietHoursEnd
	return Preference{
		TenantID: tenantID, UserID: userID, RiskType: riskType,
		MinimumSeverity: SeverityLow, DeliveryMode: DefaultDeliveryMode(riskType),
		InAppEnabled: true, TelegramEnabled: true, QuietHoursEnabled: false,
		QuietHoursStart: &start, QuietHoursEnd: &end, DigestTime: DefaultDigestTime,
	}
}

// Stored сообщает, хранится ли настройка явно или действует по умолчанию.
func (preference Preference) Stored() bool { return preference.ID != "" }

func (preference Preference) Validate() error {
	if preference.TenantID == "" || preference.UserID == "" || !ValidRiskType(preference.RiskType) ||
		!ValidSeverity(preference.MinimumSeverity) || !ValidDeliveryMode(preference.DeliveryMode) ||
		!preference.DigestTime.Valid() || (preference.QuietHoursStart == nil) != (preference.QuietHoursEnd == nil) {
		return ErrInvalidPreference
	}
	if preference.QuietHoursStart != nil {
		if !preference.QuietHoursStart.Valid() || !preference.QuietHoursEnd.Valid() ||
			*preference.QuietHoursStart == *preference.QuietHoursEnd {
			return ErrInvalidPreference
		}
	}
	if preference.QuietHoursEnabled && preference.QuietHoursStart == nil {
		return ErrInvalidPreference
	}
	if preference.Stored() && (preference.CreatedAt.IsZero() || preference.UpdatedAt.IsZero() ||
		preference.UpdatedAt.Before(preference.CreatedAt)) {
		return ErrInvalidPreference
	}
	return nil
}

// DeferReason объясняет, почему уведомление ждёт слота.
type DeferReason string

const (
	DeferDigest     DeferReason = "DIGEST"
	DeferQuietHours DeferReason = "QUIET_HOURS"
)

// PolicyDecision — результат применения настройки к открытому риску.
type PolicyDecision struct {
	Deliver         bool
	DeliverAt       time.Time // пусто для немедленной доставки, иначе момент UTC
	Slot            string    // локальный слот сводки в SlotLayout
	Reason          DeferReason
	InApp, Telegram bool
}

func (decision PolicyDecision) Immediate() bool {
	return decision.Deliver && decision.DeliverAt.IsZero()
}

func (decision PolicyDecision) deferred(at time.Time, reason DeferReason) PolicyDecision {
	decision.DeliverAt = at.UTC()
	decision.Slot = at.Format(SlotLayout)
	decision.Reason = reason
	return decision
}

// Decide применяет настройку к риску заданной важности. Нулевое решение
// означает, что получатель ничего не увидит: режим выключен, важность ниже
// порога либо ни один канал недоступен.
func (preference Preference) Decide(severity Severity, now time.Time, location *time.Location, telegramLinked bool) PolicyDecision {
	if preference.Validate() != nil || preference.DeliveryMode == ModeDisabled ||
		SeverityRank(severity) < SeverityRank(preference.MinimumSeverity) {
		return PolicyDecision{}
	}
	decision := PolicyDecision{InApp: preference.InAppEnabled, Telegram: preference.TelegramEnabled && telegramLinked}
	if !decision.InApp && !decision.Telegram {
		return PolicyDecision{}
	}
	if location == nil {
		location = time.UTC
	}
	local := now.In(location)
	decision.Deliver = true
	if preference.DeliveryMode == ModeImmediate {
		if !preference.InQuietHours(local) {
			return decision
		}
		return decision.deferred(preference.QuietHoursEndAfter(local), DeferQuietHours)
	}
	at := NextOccurrence(local, preference.DigestTime)
	if preference.InQuietHours(at) {
		at = preference.QuietHoursEndAfter(at)
	}
	return decision.deferred(at, DeferDigest)
}

// InQuietHours реализует LR-BE-RM-020: при start > end интервал трактуется как
// [start, 24:00) ∪ [00:00, end) в часовом поясе организации.
func (preference Preference) InQuietHours(local time.Time) bool {
	if !preference.QuietHoursEnabled || preference.QuietHoursStart == nil || preference.QuietHoursEnd == nil {
		return false
	}
	minute := local.Hour()*60 + local.Minute()
	start, end := preference.QuietHoursStart.minutes(), preference.QuietHoursEnd.minutes()
	if start < end {
		return minute >= start && minute < end
	}
	return minute >= start || minute < end
}

// QuietHoursEndAfter возвращает ближайший конец тихих часов для момента внутри них.
func (preference Preference) QuietHoursEndAfter(local time.Time) time.Time {
	end := *preference.QuietHoursEnd
	if preference.QuietHoursStart.minutes() < end.minutes() {
		return end.on(local)
	}
	if local.Hour()*60+local.Minute() >= preference.QuietHoursStart.minutes() {
		return end.on(local.AddDate(0, 0, 1))
	}
	return end.on(local)
}

// NextOccurrence — ближайшее наступление времени суток строго после момента.
func NextOccurrence(local time.Time, clock ClockTime) time.Time {
	candidate := clock.on(local)
	if candidate.After(local) {
		return candidate
	}
	return clock.on(local.AddDate(0, 0, 1))
}

// EscalationPolicy — основа LR-BE-2010: эскалация владельцу включается флагом
// и по умолчанию выключена. При включении риск важности HIGH и выше, не
// принятый в работу за After, получает отдельное уведомление владельцу.
type EscalationPolicy struct {
	Enabled bool
	After   time.Duration
}

func (policy EscalationPolicy) Applies(severity Severity) bool {
	return policy.Enabled && policy.After > 0 && SeverityRank(severity) >= SeverityRank(SeverityHigh)
}
