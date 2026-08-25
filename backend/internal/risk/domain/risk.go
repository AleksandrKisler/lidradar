// Package domain содержит агрегат Risk и его бизнес-инварианты.
package domain

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type Type string

const TypeNoResponse Type = "NO_RESPONSE"

type Severity string

const (
	SeverityLow      Severity = "LOW"
	SeverityMedium   Severity = "MEDIUM"
	SeverityHigh     Severity = "HIGH"
	SeverityCritical Severity = "CRITICAL"
)

type Status string

const (
	StatusOpen          Status = "OPEN"
	StatusAcknowledged  Status = "ACKNOWLEDGED"
	StatusActed         Status = "ACTED"
	StatusResolved      Status = "RESOLVED"
	StatusFalsePositive Status = "FALSE_POSITIVE"
	StatusIgnored       Status = "IGNORED"
	StatusExpired       Status = "EXPIRED"
)

type Source string

const (
	SourceRule   Source = "RULE"
	SourceHybrid Source = "HYBRID"
	SourceManual Source = "MANUAL"
)

var ErrInvalidRisk = errors.New("некорректный риск")

// Risk — требующее внимания состояние, не зависящее от этапа Opportunity.
// Все временные отметки являются моментами времени и хранятся в UTC.
type Risk struct {
	ID               string     `json:"id"`
	TenantID         string     `json:"-"`
	OpportunityID    string     `json:"opportunityId"`
	LocationID       string     `json:"locationId"`
	Type             Type       `json:"type"`
	Severity         Severity   `json:"severity"`
	Status           Status     `json:"status"`
	Source           Source     `json:"source"`
	PolicyVersion    string     `json:"policyVersion"`
	TriggerMessageID string     `json:"triggerMessageId"`
	ReasonCode       string     `json:"reasonCode"`
	Reason           string     `json:"reason"`
	DetectedAt       time.Time  `json:"detectedAt"`
	DueAt            time.Time  `json:"dueAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	AcknowledgedAt   *time.Time `json:"acknowledgedAt,omitempty"`
	ActedAt          *time.Time `json:"actedAt,omitempty"`
	ResolvedAt       *time.Time `json:"resolvedAt,omitempty"`
}

// NewNoResponse создаёт детерминированный агрегат NO_RESPONSE.
func NewNoResponse(id string, finding Finding, now time.Time) (Risk, error) {
	if id == "" || finding.TenantID == "" || finding.OpportunityID == "" ||
		finding.LocationID == "" || finding.TriggerMessageID == "" ||
		finding.PolicyVersion == "" || finding.ReasonCode == "" || finding.Reason == "" ||
		finding.DueAt.IsZero() || now.IsZero() || finding.DueAt.After(now) ||
		(finding.Severity != SeverityHigh && finding.Severity != SeverityCritical) {
		return Risk{}, ErrInvalidRisk
	}
	risk := Risk{
		ID:               id,
		TenantID:         finding.TenantID,
		OpportunityID:    finding.OpportunityID,
		LocationID:       finding.LocationID,
		Type:             TypeNoResponse,
		Severity:         finding.Severity,
		Status:           StatusOpen,
		Source:           SourceRule,
		PolicyVersion:    finding.PolicyVersion,
		TriggerMessageID: finding.TriggerMessageID,
		ReasonCode:       finding.ReasonCode,
		Reason:           finding.Reason,
		DetectedAt:       now.UTC(),
		DueAt:            finding.DueAt.UTC(),
		UpdatedAt:        now.UTC(),
	}
	if risk.Validate() != nil {
		return Risk{}, ErrInvalidRisk
	}
	return risk, nil
}

// Refresh обновляет повторное активное обнаружение вместо создания дубликата.
func (r *Risk) Refresh(finding Finding, now time.Time) error {
	if !r.Active() || finding.TenantID != r.TenantID || finding.OpportunityID != r.OpportunityID || now.IsZero() {
		return ErrInvalidRisk
	}
	r.Severity = finding.Severity
	r.LocationID = finding.LocationID
	r.ReasonCode = finding.ReasonCode
	r.Reason = finding.Reason
	r.PolicyVersion = finding.PolicyVersion
	r.TriggerMessageID = finding.TriggerMessageID
	r.DetectedAt = now.UTC()
	r.DueAt = finding.DueAt.UTC()
	r.UpdatedAt = now.UTC()
	return r.Validate()
}

func (r Risk) Active() bool {
	return r.Status == StatusOpen || r.Status == StatusAcknowledged || r.Status == StatusActed
}

var reasonCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,99}$`)

// Validate защищает агрегат и при создании, и при чтении из PostgreSQL.
func (r Risk) Validate() error {
	if r.ID == "" || r.TenantID == "" || r.OpportunityID == "" || r.LocationID == "" ||
		r.Type != TypeNoResponse || (r.Severity != SeverityHigh && r.Severity != SeverityCritical) ||
		(r.Source != SourceRule) || r.PolicyVersion == "" || r.PolicyVersion != strings.TrimSpace(r.PolicyVersion) ||
		r.TriggerMessageID == "" || !reasonCodePattern.MatchString(r.ReasonCode) || r.Reason == "" ||
		r.Reason != strings.TrimSpace(r.Reason) || r.DetectedAt.IsZero() || r.DueAt.IsZero() ||
		r.DueAt.After(r.DetectedAt) || r.UpdatedAt.IsZero() {
		return ErrInvalidRisk
	}
	switch r.Status {
	case StatusOpen:
		if r.AcknowledgedAt != nil || r.ActedAt != nil || r.ResolvedAt != nil {
			return ErrInvalidRisk
		}
	case StatusAcknowledged:
		if r.AcknowledgedAt == nil || r.ActedAt != nil || r.ResolvedAt != nil {
			return ErrInvalidRisk
		}
	case StatusActed:
		if r.AcknowledgedAt == nil || r.ActedAt == nil || r.ResolvedAt != nil {
			return ErrInvalidRisk
		}
	case StatusResolved, StatusFalsePositive, StatusIgnored, StatusExpired:
		if r.ResolvedAt == nil {
			return ErrInvalidRisk
		}
	default:
		return ErrInvalidRisk
	}
	return nil
}

// Acknowledge отмечает, что пользователь увидел активный риск. Повтор вызова
// идемпотентен и никогда не открывает закрытый риск заново.
func (r *Risk) Acknowledge(now time.Time) error {
	if now.IsZero() {
		return ErrInvalidRisk
	}
	if r.Status == StatusResolved || r.Status == StatusAcknowledged || r.Status == StatusActed {
		return nil
	}
	if r.Status != StatusOpen {
		return ErrInvalidRisk
	}
	at := now.UTC()
	r.Status = StatusAcknowledged
	r.AcknowledgedAt = &at
	r.UpdatedAt = at
	return nil
}

func (r *Risk) Resolve(now time.Time) error {
	if now.IsZero() {
		return ErrInvalidRisk
	}
	if !r.Active() {
		return nil
	}
	resolved := now.UTC()
	r.Status = StatusResolved
	r.ResolvedAt = &resolved
	r.UpdatedAt = resolved
	return nil
}

// Repository учитывает организацию и атомарно допускает не более одного
// активного риска на tenant, Opportunity и тип. PostgreSQL-адаптер подкрепляет
// этот контракт частичным уникальным индексом.
type Repository interface {
	UpsertActive(ctx context.Context, risk Risk) (Risk, bool, error)
	FindActive(ctx context.Context, tenantID, opportunityID string, riskType Type) (Risk, bool, error)
	ResolveActive(ctx context.Context, tenantID, opportunityID string, riskType Type, at time.Time) (bool, error)
}

type Direction string

const (
	DirectionIncoming Direction = "INCOMING"
	DirectionOutgoing Direction = "OUTGOING"
)

// ConversationState — свежая авторитетная проекция, читаемая при выполнении
// проверки по расписанию. В полезной нагрузке хранятся только идентификаторы.
type ConversationState struct {
	TenantID             string
	OpportunityID        string
	LocationID           string
	ActiveOpportunity    bool
	LastMeaningfulID     string
	LastMeaningfulAt     time.Time
	LastMeaningful       Direction
	OutgoingAfterTrigger bool
	ResponseThreshold    time.Duration
	BusinessHours        BusinessHours
}

type Finding struct {
	TenantID, OpportunityID, LocationID string
	TriggerMessageID                    string
	Severity                            Severity
	PolicyVersion                       string
	ReasonCode                          string
	Reason                              string
	DueAt                               time.Time
}

type Decision struct {
	Finding *Finding
	DueAt   time.Time
	Resolve bool
}

// Policy версионируется, чтобы основание сохранённого риска оставалось объяснимым.
type Policy interface {
	Type() Type
	Version() string
	Evaluate(state ConversationState, at time.Time) (Decision, error)
}

func validateState(state ConversationState) error {
	if state.TenantID == "" || state.OpportunityID == "" || state.LocationID == "" ||
		state.LastMeaningfulID == "" || state.LastMeaningfulAt.IsZero() || state.ResponseThreshold < time.Minute ||
		state.ResponseThreshold > 1440*time.Minute || state.ResponseThreshold%time.Minute != 0 ||
		(state.LastMeaningful != DirectionIncoming && state.LastMeaningful != DirectionOutgoing) {
		return fmt.Errorf("%w: incomplete conversation state", ErrInvalidRisk)
	}
	return nil
}
