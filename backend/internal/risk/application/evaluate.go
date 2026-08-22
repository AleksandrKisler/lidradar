// Package application coordinates Risk use cases through domain ports.
package application

import (
	"context"
	"errors"
	"time"

	"lidradar/backend/internal/risk/domain"
)

var ErrInvalidCheck = errors.New("invalid risk check")

// StateReader reloads current canonical conversation/opportunity state. This
// prevents a delayed worker from making decisions from a stale job payload.
type StateReader interface {
	CurrentState(ctx context.Context, tenantID, opportunityID string) (domain.ConversationState, error)
}

type IDGenerator func() string

type Evaluator struct {
	repository domain.Repository
	states     StateReader
	policy     domain.Policy
	newID      IDGenerator
	now        func() time.Time
}

func NewEvaluator(repository domain.Repository, states StateReader, policy domain.Policy, newID IDGenerator, now func() time.Time) Evaluator {
	return Evaluator{repository: repository, states: states, policy: policy, newID: newID, now: now}
}

// EvaluateDue re-reads authoritative state, evaluates the versioned policy,
// atomically deduplicates an active finding, and resolves a no-longer-valid one.
func (e Evaluator) EvaluateDue(ctx context.Context, tenantID, opportunityID string) (domain.Risk, bool, error) {
	if tenantID == "" || opportunityID == "" || e.repository == nil || e.states == nil || e.policy == nil || e.newID == nil || e.now == nil {
		return domain.Risk{}, false, ErrInvalidCheck
	}
	state, err := e.states.CurrentState(ctx, tenantID, opportunityID)
	if err != nil {
		return domain.Risk{}, false, err
	}
	// A reader returning another tenant/entity is a boundary violation, not a
	// harmless cache miss.
	if state.TenantID != tenantID || state.OpportunityID != opportunityID {
		return domain.Risk{}, false, ErrInvalidCheck
	}
	now := e.now().UTC()
	decision, err := e.policy.Evaluate(state, now)
	if err != nil {
		return domain.Risk{}, false, err
	}
	if decision.Finding == nil {
		_, err = e.repository.ResolveActive(ctx, tenantID, opportunityID, e.policy.Type(), now)
		return domain.Risk{}, false, err
	}
	risk, err := domain.NewNoResponse(e.newID(), *decision.Finding, now)
	if err != nil {
		return domain.Risk{}, false, err
	}
	return e.repository.UpsertActive(ctx, risk)
}

// DueAt computes the durable scheduled-check time from current state. The job
// should persist tenant/opportunity identifiers and this instant, not a state snapshot.
func (e Evaluator) DueAt(state domain.ConversationState) (time.Time, error) {
	decision, err := e.policy.Evaluate(state, state.LastMeaningfulAt)
	return decision.DueAt, err
}
