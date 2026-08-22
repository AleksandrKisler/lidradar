// Package domain owns the Risk aggregate and its business invariants.
package domain

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Type string

const TypeNoResponse Type = "NO_RESPONSE"

type Severity string

const (
	SeverityHigh     Severity = "HIGH"
	SeverityCritical Severity = "CRITICAL"
)

type Status string

const (
	StatusOpen         Status = "OPEN"
	StatusAcknowledged Status = "ACKNOWLEDGED"
	StatusActed        Status = "ACTED"
	StatusResolved     Status = "RESOLVED"
)

type Source string

const SourceRule Source = "RULE"

var ErrInvalidRisk = errors.New("invalid risk")

// Risk is a condition requiring attention, independent of Opportunity stage.
// All timestamps are instants and must be persisted as TIMESTAMPTZ in UTC.
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
	Reason           string     `json:"reason"`
	DetectedAt       time.Time  `json:"detectedAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	AcknowledgedAt   *time.Time `json:"acknowledgedAt,omitempty"`
	ResolvedAt       *time.Time `json:"resolvedAt,omitempty"`
}

// NewNoResponse creates the deterministic NO_RESPONSE aggregate.
func NewNoResponse(id string, finding Finding, now time.Time) (Risk, error) {
	if id == "" || finding.TenantID == "" || finding.OpportunityID == "" ||
		finding.LocationID == "" || finding.TriggerMessageID == "" ||
		finding.PolicyVersion == "" || finding.Reason == "" || now.IsZero() ||
		(finding.Severity != SeverityHigh && finding.Severity != SeverityCritical) {
		return Risk{}, ErrInvalidRisk
	}
	return Risk{
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
		Reason:           finding.Reason,
		DetectedAt:       now.UTC(),
		UpdatedAt:        now.UTC(),
	}, nil
}

// Refresh updates a repeated active finding instead of creating a duplicate.
func (r *Risk) Refresh(finding Finding, now time.Time) error {
	if !r.Active() || finding.TenantID != r.TenantID || finding.OpportunityID != r.OpportunityID || now.IsZero() {
		return ErrInvalidRisk
	}
	r.Severity = finding.Severity
	r.Reason = finding.Reason
	r.PolicyVersion = finding.PolicyVersion
	r.TriggerMessageID = finding.TriggerMessageID
	r.UpdatedAt = now.UTC()
	return nil
}

func (r Risk) Active() bool {
	return r.Status == StatusOpen || r.Status == StatusAcknowledged || r.Status == StatusActed
}

// Acknowledge records that a user has seen an active risk. Replays are
// deliberately idempotent and never reopen resolved risks.
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

// Repository is tenant-aware and atomically enforces one active risk per
// tenant, opportunity and type. A PostgreSQL adapter must back this contract
// with the corresponding partial unique index.
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

// ConversationState is a fresh, authoritative projection loaded when a
// scheduled check executes. Scheduled payloads must contain identifiers only.
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
	Reason                              string
}

type Decision struct {
	Finding *Finding
	DueAt   time.Time
}

// Policy is versioned so stored risk evidence remains explainable.
type Policy interface {
	Type() Type
	Version() string
	Evaluate(state ConversationState, at time.Time) (Decision, error)
}

func validateState(state ConversationState) error {
	if state.TenantID == "" || state.OpportunityID == "" || state.LocationID == "" ||
		state.LastMeaningfulID == "" || state.LastMeaningfulAt.IsZero() || state.ResponseThreshold < time.Minute ||
		state.ResponseThreshold > 1440*time.Minute || state.ResponseThreshold%time.Minute != 0 {
		return fmt.Errorf("%w: incomplete conversation state", ErrInvalidRisk)
	}
	return nil
}
