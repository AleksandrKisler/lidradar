// Package domain описывает неизменяемые события исходящего журнала.
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	ErrInvalid   = errors.New("invalid outbox event")
	ErrConflict  = errors.New("outbox event conflict")
	ErrLeaseLost = errors.New("outbox event lease lost")
)

type Status string

const (
	StatusPending    Status = "PENDING"
	StatusProcessing Status = "PROCESSING"
	StatusRetry      Status = "RETRY"
	StatusPublished  Status = "PUBLISHED"
	StatusDead       Status = "DEAD"
)

func (status Status) Valid() bool {
	switch status {
	case StatusPending, StatusProcessing, StatusRetry, StatusPublished, StatusDead:
		return true
	default:
		return false
	}
}

// Event — версионированный envelope из ADR 0027. Data содержит только данные
// события; идентичность, tenant, aggregate и trace хранятся отдельно.
type Event struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	Version       int             `json:"version"`
	OccurredAt    time.Time       `json:"occurredAt"`
	TenantID      string          `json:"tenantId"`
	AggregateType string          `json:"aggregateType"`
	AggregateID   string          `json:"aggregateId"`
	TraceID       string          `json:"traceId"`
	Data          json.RawMessage `json:"data"`
	Status        Status          `json:"-"`
	AvailableAt   time.Time       `json:"-"`
	AttemptCount  int             `json:"-"`
	MaxAttempts   int             `json:"-"`
	LeaseOwner    *string         `json:"-"`
	LeaseUntil    *time.Time      `json:"-"`
	LastErrorCode *string         `json:"-"`
	CompletedAt   *time.Time      `json:"-"`
	CreatedAt     time.Time       `json:"-"`
	UpdatedAt     time.Time       `json:"-"`
}

func NewEvent(
	id, eventType string,
	version int,
	tenantID, aggregateType, aggregateID, traceID string,
	data json.RawMessage,
	occurredAt time.Time,
) (Event, error) {
	event := Event{
		ID: id, Type: strings.TrimSpace(eventType), Version: version, TenantID: tenantID,
		AggregateType: strings.TrimSpace(aggregateType), AggregateID: aggregateID, TraceID: traceID,
		Data: append(json.RawMessage(nil), data...), Status: StatusPending,
		AvailableAt: occurredAt.UTC(), MaxAttempts: 5, OccurredAt: occurredAt.UTC(),
		CreatedAt: occurredAt.UTC(), UpdatedAt: occurredAt.UTC(),
	}
	if event.Validate() != nil {
		return Event{}, ErrInvalid
	}
	return event, nil
}

func (event Event) Key() string { return fmt.Sprintf("%s.v%d", event.Type, event.Version) }

func (event Event) Validate() error {
	if event.ID == "" || event.TenantID == "" || event.AggregateID == "" || event.TraceID == "" ||
		!validName(event.Type) || !validName(event.AggregateType) || event.Version < 1 || !jsonObject(event.Data) ||
		!event.Status.Valid() || event.AvailableAt.IsZero() || event.OccurredAt.IsZero() ||
		event.AttemptCount < 0 || event.MaxAttempts < 1 || event.MaxAttempts > 20 || event.AttemptCount > event.MaxAttempts ||
		event.CreatedAt.IsZero() || event.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	if event.LastErrorCode != nil && !errorCodePattern.MatchString(*event.LastErrorCode) {
		return ErrInvalid
	}
	switch event.Status {
	case StatusProcessing:
		if event.LeaseOwner == nil || strings.TrimSpace(*event.LeaseOwner) == "" || event.LeaseUntil == nil || event.CompletedAt != nil {
			return ErrInvalid
		}
	case StatusPending, StatusRetry:
		if event.LeaseOwner != nil || event.LeaseUntil != nil || event.CompletedAt != nil {
			return ErrInvalid
		}
	case StatusPublished, StatusDead:
		if event.LeaseOwner != nil || event.LeaseUntil != nil || event.CompletedAt == nil {
			return ErrInvalid
		}
	}
	return nil
}

func validName(value string) bool {
	return value == strings.TrimSpace(value) && len(value) >= 1 && len(value) <= 100
}

var errorCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,99}$`)

func jsonObject(value json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(value, &object) == nil && object != nil
}
