// Package domain описывает период и показатели базовой аналитики: считанные
// из необработанных фактов модулей числа за окно в часовом поясе организации.
package domain

import (
	"errors"
	"time"
)

var ErrInvalidPeriod = errors.New("некорректный период аналитики")

const (
	DateLayout        = "2006-01-02"
	DefaultPeriodDays = 30
	MaxPeriodDays     = 366
)

// Period — окно [From, To), заданное календарными датами включительно в
// часовом поясе организации (LR-BE-2202). Границы переводятся в UTC один раз.
type Period struct {
	FromDate string    `json:"fromDate"`
	ToDate   string    `json:"toDate"`
	Timezone string    `json:"timezone"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
}

// ResolvePeriod принимает даты YYYY-MM-DD; пустая конечная дата — сегодня по
// часовому поясу, пустая начальная — 30 дней включая конечную. Окно не длиннее
// 366 дней, чтобы отчёт оставался предсказуемым по стоимости.
func ResolvePeriod(fromDate, toDate string, now time.Time, location *time.Location) (Period, error) {
	if location == nil || now.IsZero() {
		return Period{}, ErrInvalidPeriod
	}
	if toDate == "" {
		toDate = now.In(location).Format(DateLayout)
	}
	end, err := time.ParseInLocation(DateLayout, toDate, location)
	if err != nil {
		return Period{}, ErrInvalidPeriod
	}
	if fromDate == "" {
		fromDate = end.AddDate(0, 0, -(DefaultPeriodDays - 1)).Format(DateLayout)
	}
	start, err := time.ParseInLocation(DateLayout, fromDate, location)
	if err != nil || start.After(end) {
		return Period{}, ErrInvalidPeriod
	}
	to := end.AddDate(0, 0, 1)
	if to.After(start.AddDate(0, 0, MaxPeriodDays)) {
		return Period{}, ErrInvalidPeriod
	}
	return Period{
		FromDate: start.Format(DateLayout), ToDate: end.Format(DateLayout), Timezone: location.String(),
		From: start.UTC(), To: to.UTC(),
	}, nil
}

// Contains сообщает, попадает ли момент в окно.
func (period Period) Contains(at time.Time) bool {
	return !at.Before(period.From) && at.Before(period.To)
}

type Messages struct {
	Total         int `json:"total"`
	Incoming      int `json:"incoming"`
	Outgoing      int `json:"outgoing"`
	Conversations int `json:"conversations"`
}

// Opportunities: Created — открытые в окне; Booked, Won и Lost — переходы
// этапов в окне по неизменяемой истории.
type Opportunities struct {
	Created int `json:"created"`
	Booked  int `json:"booked"`
	Won     int `json:"won"`
	Lost    int `json:"lost"`
}

// RiskCounters: Detected — обнаруженные в окне, Acted — с действием в окне,
// Resolved — закрытые как RESOLVED в окне, FalsePositive — закрытые как
// ложные в окне (LR-BE-2205).
type RiskCounters struct {
	Detected      int `json:"detected"`
	Acted         int `json:"acted"`
	Resolved      int `json:"resolved"`
	FalsePositive int `json:"falsePositive"`
}

type RiskTypeMetrics struct {
	RiskType string `json:"riskType"`
	RiskCounters
}

type Risks struct {
	RiskCounters
	ByType []RiskTypeMetrics `json:"byType"`
}

// RiskTypes перечисляет типы в порядке ТЗ §27; отчёт всегда содержит все пять.
func RiskTypes() []string {
	return []string{
		"NO_RESPONSE", "CUSTOMER_SILENT_AFTER_PRICE", "BOOKING_NOT_CONFIRMED",
		"PROMISE_NOT_FULFILLED", "FOLLOW_UP_CANDIDATE",
	}
}

// RisksFromTypes собирает разрез по типам в каноническом порядке, дополняя
// отсутствующие типы нулями, и считает итоги как их сумму.
func RisksFromTypes(rows []RiskTypeMetrics) Risks {
	byType := make(map[string]RiskCounters, len(rows))
	for _, row := range rows {
		byType[row.RiskType] = row.RiskCounters
	}
	risks := Risks{ByType: make([]RiskTypeMetrics, 0, len(RiskTypes()))}
	for _, riskType := range RiskTypes() {
		counters := byType[riskType]
		risks.ByType = append(risks.ByType, RiskTypeMetrics{RiskType: riskType, RiskCounters: counters})
		risks.Detected += counters.Detected
		risks.Acted += counters.Acted
		risks.Resolved += counters.Resolved
		risks.FalsePositive += counters.FalsePositive
	}
	return risks
}

// Outcomes — зафиксированные исходы в окне (LR-BE-2206).
type Outcomes struct {
	Booked int `json:"booked"`
	Paid   int `json:"paid"`
	Lost   int `json:"lost"`
}

// Revenue считается только в валюте организации по умолчанию: Potential —
// оценка ещё открытых сделок, открытых в окне (ТЗ §26: не выручка);
// Confirmed — подтверждённые события окна; ConfirmedRecovered — их часть с
// атрибуцией RECOVERED (ТЗ §39); ConfirmedPayments — число событий.
type Revenue struct {
	Currency           string `json:"currency"`
	Potential          string `json:"potential"`
	Confirmed          string `json:"confirmed"`
	ConfirmedRecovered string `json:"confirmedRecovered"`
	ConfirmedPayments  int    `json:"confirmedPayments"`
}

type Summary struct {
	Period        Period        `json:"period"`
	Messages      Messages      `json:"messages"`
	Opportunities Opportunities `json:"opportunities"`
	Risks         Risks         `json:"risks"`
	Outcomes      Outcomes      `json:"outcomes"`
	Revenue       Revenue       `json:"revenue"`
}
