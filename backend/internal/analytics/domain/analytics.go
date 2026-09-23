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

// DailyPoint — показатели одного календарного дня организации (для графика
// на экране аналитики). Деньги — только в валюте организации.
type DailyPoint struct {
	Date               string `json:"date"`
	Incoming           int    `json:"incoming"`
	Outgoing           int    `json:"outgoing"`
	RisksDetected      int    `json:"risksDetected"`
	Confirmed          string `json:"confirmed"`
	ConfirmedRecovered string `json:"confirmedRecovered"`
	Payments           int    `json:"payments"`
}

// AttributionSplit — подтверждённые события окна по типу атрибуции.
type AttributionSplit struct {
	Type   string `json:"type"`
	Amount string `json:"amount"`
	Count  int    `json:"count"`
}

// AttributionTypes перечисляет типы атрибуции в порядке ТЗ §39; разбивка
// всегда содержит все три.
func AttributionTypes() []string { return []string{"RECOVERED", "ORGANIC", "UNKNOWN"} }

type Summary struct {
	Period        Period             `json:"period"`
	Messages      Messages           `json:"messages"`
	Opportunities Opportunities      `json:"opportunities"`
	Risks         Risks              `json:"risks"`
	Outcomes      Outcomes           `json:"outcomes"`
	Revenue       Revenue            `json:"revenue"`
	Series        []DailyPoint       `json:"series"`
	Attribution   []AttributionSplit `json:"attribution"`
}

// Dates перечисляет календарные даты окна в часовом поясе организации.
func (period Period) Dates() ([]string, error) {
	location, err := time.LoadLocation(period.Timezone)
	if err != nil {
		return nil, ErrInvalidPeriod
	}
	start, err := time.ParseInLocation(DateLayout, period.FromDate, location)
	if err != nil {
		return nil, ErrInvalidPeriod
	}
	end, err := time.ParseInLocation(DateLayout, period.ToDate, location)
	if err != nil || end.Before(start) {
		return nil, ErrInvalidPeriod
	}
	dates := make([]string, 0, MaxPeriodDays)
	for day := start; !day.After(end) && len(dates) <= MaxPeriodDays; day = day.AddDate(0, 0, 1) {
		dates = append(dates, day.Format(DateLayout))
	}
	return dates, nil
}

// FillSeries дополняет дневной ряд нулями по каждой дате окна и упорядочивает
// его по возрастанию даты; дни вне окна отбрасываются.
func FillSeries(period Period, rows []DailyPoint) ([]DailyPoint, error) {
	dates, err := period.Dates()
	if err != nil {
		return nil, err
	}
	byDate := make(map[string]DailyPoint, len(rows))
	for _, row := range rows {
		byDate[row.Date] = row
	}
	series := make([]DailyPoint, 0, len(dates))
	for _, date := range dates {
		point, known := byDate[date]
		if !known {
			point = DailyPoint{Confirmed: "0.00", ConfirmedRecovered: "0.00"}
		}
		point.Date = date
		if point.Confirmed == "" {
			point.Confirmed = "0.00"
		}
		if point.ConfirmedRecovered == "" {
			point.ConfirmedRecovered = "0.00"
		}
		series = append(series, point)
	}
	return series, nil
}

// AttributionFromRows собирает разбивку по трём типам в каноническом порядке,
// дополняя отсутствующие типы нулями.
func AttributionFromRows(rows []AttributionSplit) []AttributionSplit {
	byType := make(map[string]AttributionSplit, len(rows))
	for _, row := range rows {
		byType[row.Type] = row
	}
	result := make([]AttributionSplit, 0, 3)
	for _, kind := range AttributionTypes() {
		split, known := byType[kind]
		if !known {
			split = AttributionSplit{Amount: "0.00"}
		}
		split.Type = kind
		if split.Amount == "" {
			split.Amount = "0.00"
		}
		result = append(result, split)
	}
	return result
}

// Payment — подтверждённое событие выручки окна с контекстом для списка
// последних оплат: у каждого события ровно одна атрибуция; суммы приводятся в
// валюте события, поэтому список содержит все валюты, а итоги сводки — только
// валюту организации. Возвратов в модели нет: событие неизменяемо и
// положительно (ТЗ §39).
type Payment struct {
	EventID            string    `json:"eventId"`
	OpportunityID      string    `json:"opportunityId"`
	ConversationID     string    `json:"conversationId"`
	ContactID          string    `json:"contactId"`
	ContactDisplayName *string   `json:"contactDisplayName"`
	ServiceName        *string   `json:"serviceName"`
	Amount             string    `json:"amount"`
	Currency           string    `json:"currency"`
	Attribution        string    `json:"attribution"`
	RiskID             *string   `json:"riskId"`
	ConfirmedBy        string    `json:"confirmedBy"`
	ConfirmedAt        time.Time `json:"confirmedAt"`
}

// PaymentCursor — устойчивая граница следующей страницы списка оплат.
type PaymentCursor struct {
	At time.Time
	ID string
}
