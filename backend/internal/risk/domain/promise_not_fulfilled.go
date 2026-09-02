package domain

import (
	"fmt"
	"time"
)

const (
	PromiseNotFulfilledPolicyVersion = "promise-not-fulfilled/v1"
	// PromiseFallbackThreshold действует, когда срок в обещании не назван
	// явно (LR-BE-1703).
	PromiseFallbackThreshold   = 60 * time.Minute
	StrongCommitmentConfidence = 0.85
)

// CommitmentSignal — проверенный смысловой факт BUSINESS_COMMITMENT из
// производной проекции AI. Доказательство всегда исходящее сообщение
// компании; FollowedUp сообщает, писала ли компания клиенту после него.
type CommitmentSignal struct {
	Value             bool
	Confidence        float64
	AIRunID           string
	EvidenceMessageID string
	EvidenceAt        time.Time
	EvidenceText      string
	FollowedUp        bool
}

type PromiseNotFulfilledPolicy struct{}

func (PromiseNotFulfilledPolicy) Type() Type { return TypePromiseNotFulfilled }
func (PromiseNotFulfilledPolicy) Version() string {
	return PromiseNotFulfilledPolicyVersion
}

// Evaluate применяет правило R4 (ТЗ §32). AI сообщает факт обязательства;
// срок, запас, соответствующий follow-up и решение о риске вычисляются здесь.
// Follow-up — любое исходящее сообщение компании после обещания: ложное
// срабатывание опаснее пропуска, а смысловая проверка ответа остаётся
// уточнением следующих этапов.
func (policy PromiseNotFulfilledPolicy) Evaluate(state ConversationState, at time.Time) (Decision, error) {
	if err := validateState(state); err != nil || at.IsZero() {
		return Decision{}, ErrInvalidRisk
	}
	if !state.ActiveOpportunity {
		return Decision{Resolve: true}, nil
	}
	// Активный риск закрывается, как только компания написала клиенту после
	// сообщения-основания, даже если проекция AI уже не содержит факта.
	active, hasActive := state.ActiveRisks[TypePromiseNotFulfilled]
	activeFollowedUp := hasActive && active.OutgoingAfterTrigger

	signal := state.Commitment
	if signal == nil || !signal.Value || signal.Confidence < StrongCommitmentConfidence {
		return Decision{Resolve: activeFollowedUp}, nil
	}
	if signal.AIRunID == "" || signal.EvidenceMessageID == "" || signal.EvidenceAt.IsZero() {
		return Decision{}, ErrInvalidRisk
	}
	if signal.FollowedUp {
		return Decision{Resolve: true, TriggerMessageID: signal.EvidenceMessageID}, nil
	}
	location, err := time.LoadLocation(state.BusinessHours.Timezone)
	if err != nil {
		return Decision{}, fmt.Errorf("%w: timezone: %v", ErrInvalidBusinessHours, err)
	}
	promised, parsed := ParsePromisedDue(signal.EvidenceText, signal.EvidenceAt, location)
	due := promised.At
	if !parsed {
		if due, err = state.BusinessHours.AddBusinessTime(signal.EvidenceAt, PromiseFallbackThreshold); err != nil {
			return Decision{}, err
		}
	}
	decision := Decision{
		DueAt: due, TriggerMessageID: signal.EvidenceMessageID,
		Resolve: activeFollowedUp && active.TriggerMessageID != signal.EvidenceMessageID,
	}
	if at.Before(due) {
		return decision, nil
	}
	reason := "Компания дала клиенту обещание, но не написала ему в течение 60 рабочих минут"
	if parsed {
		reason = fmt.Sprintf(
			"Компания пообещала клиенту действие к %s («%s»), но не написала ему после обещания",
			due.In(location).Format("02.01 15:04"), promised.Phrase,
		)
	}
	confidence, runID := signal.Confidence, signal.AIRunID
	decision.Finding = &Finding{
		TenantID: state.TenantID, OpportunityID: state.OpportunityID,
		LocationID: state.LocationID, TriggerMessageID: signal.EvidenceMessageID,
		Severity: SeverityHigh, PolicyVersion: policy.Version(),
		ReasonCode: "PROMISE_NOT_FULFILLED_AFTER_DUE", Reason: reason,
		DueAt: due, Source: SourceHybrid, Confidence: &confidence, AIRunID: &runID,
	}
	return decision, nil
}
