// Package domain владеет подтверждённой выручкой и её формальной атрибуцией.
package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

var ErrInvalid = errors.New("некорректная запись выручки")

type AttributionType string

const (
	AttributionRecovered AttributionType = "RECOVERED"
	AttributionOrganic   AttributionType = "ORGANIC"
	AttributionUnknown   AttributionType = "UNKNOWN"
)

type Status string
type Source string

const (
	StatusConfirmed Status = "CONFIRMED"
	SourceUser      Source = "USER_CONFIRMED"
)

// Money хранит десятичное значение без двоичной плавающей точки. Граница
// предметной области допускает не более 12 цифр до точки и две денежные
// позиции в представлении ответа.
type Money struct{ value decimal.Decimal }

var moneyPattern = regexp.MustCompile(`^[0-9]{1,12}(?:\.[0-9]{1,2})?$`)
var aggregateMoneyPattern = regexp.MustCompile(`^[0-9]{1,18}(?:\.[0-9]{1,2})?$`)

// ParseMoney принимает только положительную сумму, пригодную для подтверждения.
func ParseMoney(raw string) (Money, error) { return parseMoney(raw, false) }

// ParseNonNegativeMoney применяется при чтении агрегатов, где ноль допустим.
func ParseNonNegativeMoney(raw string) (Money, error) { return parseMoney(raw, true) }

func parseMoney(raw string, allowZero bool) (Money, error) {
	raw = strings.TrimSpace(raw)
	pattern := moneyPattern
	if allowZero {
		pattern = aggregateMoneyPattern
	}
	if !pattern.MatchString(raw) {
		return Money{}, ErrInvalid
	}
	value, err := decimal.NewFromString(raw)
	if err != nil || value.IsNegative() || (!allowZero && value.IsZero()) || value.Exponent() < -2 {
		return Money{}, ErrInvalid
	}
	return Money{value: value}, nil
}

func (money Money) String() string           { return money.value.StringFixed(2) }
func (money Money) Decimal() decimal.Decimal { return money.value }

func (money Money) MarshalJSON() ([]byte, error) { return json.Marshal(money.String()) }

func (money *Money) UnmarshalJSON(data []byte) error {
	if money == nil || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return ErrInvalid
	}
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return ErrInvalid
	}
	parsed, err := ParseMoney(raw)
	if err != nil {
		return err
	}
	*money = parsed
	return nil
}

type RevenueEvent struct {
	ID            string    `json:"id"`
	TenantID      string    `json:"-"`
	OpportunityID string    `json:"opportunityId"`
	Amount        Money     `json:"amount"`
	Currency      string    `json:"currency"`
	Status        Status    `json:"status"`
	Source        Source    `json:"source"`
	ConfirmedBy   string    `json:"confirmedBy"`
	ConfirmedAt   time.Time `json:"confirmedAt"`
}

type RevenueAttribution struct {
	ID             string          `json:"id"`
	TenantID       string          `json:"-"`
	RevenueEventID string          `json:"revenueEventId"`
	OpportunityID  string          `json:"opportunityId"`
	Type           AttributionType `json:"type"`
	RiskID         string          `json:"riskId,omitempty"`
	ActionID       string          `json:"actionId,omitempty"`
	OutcomeID      string          `json:"outcomeId,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
}

func NewConfirmedEvent(id, tenant, opportunity, amount, currency, actor string, at time.Time) (RevenueEvent, error) {
	money, err := ParseMoney(amount)
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if err != nil || id == "" || tenant == "" || opportunity == "" || actor == "" || !ValidCurrency(currency) || at.IsZero() {
		return RevenueEvent{}, ErrInvalid
	}
	return RevenueEvent{
		ID: id, TenantID: tenant, OpportunityID: opportunity, Amount: money,
		Currency: currency, Status: StatusConfirmed, Source: SourceUser,
		ConfirmedBy: actor, ConfirmedAt: at.UTC(),
	}, nil
}

func NewAttribution(id string, event RevenueEvent, kind AttributionType, risk, action, outcome string, at time.Time) (RevenueAttribution, error) {
	if id == "" || event.ID == "" || event.TenantID == "" || event.OpportunityID == "" || at.IsZero() {
		return RevenueAttribution{}, ErrInvalid
	}
	switch kind {
	case AttributionRecovered:
		if risk == "" || action == "" || outcome == "" {
			return RevenueAttribution{}, ErrInvalid
		}
	case AttributionOrganic, AttributionUnknown:
		if risk != "" || action != "" || outcome != "" {
			return RevenueAttribution{}, ErrInvalid
		}
	default:
		return RevenueAttribution{}, ErrInvalid
	}
	return RevenueAttribution{
		ID: id, TenantID: event.TenantID, RevenueEventID: event.ID,
		OpportunityID: event.OpportunityID, Type: kind, RiskID: risk,
		ActionID: action, OutcomeID: outcome, CreatedAt: at.UTC(),
	}, nil
}

func ValidCurrency(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for _, symbol := range currency {
		if symbol < 'A' || symbol > 'Z' {
			return false
		}
	}
	return true
}
