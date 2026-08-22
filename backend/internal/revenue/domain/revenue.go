// Package domain owns confirmed revenue and its formal attribution.
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ErrInvalid = errors.New("invalid revenue record")

type AttributionType string

const (
	AttributionRecovered AttributionType = "RECOVERED"
	AttributionOrganic   AttributionType = "ORGANIC"
	AttributionUnknown   AttributionType = "UNKNOWN"
)

// Money uses integer minor units internally and a decimal string at boundaries.
// This deliberately avoids binary floating point for business values.
type Money struct{ cents int64 }

var moneyPattern = regexp.MustCompile(`^([0-9]{1,12})(?:\.([0-9]{1,2}))?$`)

func ParseMoney(value string) (Money, error) {
	m := moneyPattern.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return Money{}, ErrInvalid
	}
	whole, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return Money{}, ErrInvalid
	}
	fraction := m[2] + strings.Repeat("0", 2-len(m[2]))
	cents, _ := strconv.ParseInt(fraction, 10, 64)
	valueInCents := whole*100 + cents
	if valueInCents <= 0 {
		return Money{}, ErrInvalid
	}
	return Money{cents: valueInCents}, nil
}

func (m Money) String() string { return fmt.Sprintf("%d.%02d", m.cents/100, m.cents%100) }
func (m Money) Cents() int64   { return m.cents }
func NewMoneyFromCents(cents int64) (Money, error) {
	if cents < 0 {
		return Money{}, ErrInvalid
	}
	return Money{cents: cents}, nil
}

type RevenueEvent struct {
	ID            string    `json:"id"`
	TenantID      string    `json:"-"`
	OpportunityID string    `json:"opportunityId"`
	Amount        Money     `json:"amount"`
	Currency      string    `json:"currency"`
	Status        string    `json:"status"`
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
	if err != nil || id == "" || tenant == "" || opportunity == "" || actor == "" || len(currency) != 3 || at.IsZero() {
		return RevenueEvent{}, ErrInvalid
	}
	for _, r := range currency {
		if r < 'A' || r > 'Z' {
			return RevenueEvent{}, ErrInvalid
		}
	}
	return RevenueEvent{id, tenant, opportunity, money, currency, "CONFIRMED", actor, at.UTC()}, nil
}

func NewAttribution(id string, event RevenueEvent, kind AttributionType, risk, action, outcome string, at time.Time) (RevenueAttribution, error) {
	if id == "" || event.ID == "" || at.IsZero() {
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
	return RevenueAttribution{id, event.TenantID, event.ID, event.OpportunityID, kind, risk, action, outcome, at.UTC()}, nil
}

func (m Money) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(m.String())), nil }
