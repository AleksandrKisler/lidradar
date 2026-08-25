package domain

import (
	"fmt"
	"time"
)

const NoResponsePolicyVersion = "no-response/v1"

type NoResponsePolicy struct{}

func (NoResponsePolicy) Type() Type      { return TypeNoResponse }
func (NoResponsePolicy) Version() string { return NoResponsePolicyVersion }

func (p NoResponsePolicy) Evaluate(state ConversationState, at time.Time) (Decision, error) {
	if err := validateState(state); err != nil || at.IsZero() {
		return Decision{}, ErrInvalidRisk
	}
	due, err := state.BusinessHours.AddBusinessTime(state.LastMeaningfulAt, state.ResponseThreshold)
	if err != nil {
		return Decision{}, err
	}
	decision := Decision{DueAt: due}
	if state.LastMeaningful != DirectionIncoming || !state.ActiveOpportunity || state.OutgoingAfterTrigger {
		decision.Resolve = true
		return decision, nil
	}
	if at.Before(due) {
		return decision, nil
	}
	elapsed, err := state.BusinessHours.ElapsedBusinessTime(state.LastMeaningfulAt, at)
	if err != nil {
		return Decision{}, err
	}
	severity := SeverityHigh
	if elapsed >= 90*time.Minute {
		severity = SeverityCritical
	}
	decision.Finding = &Finding{
		TenantID: state.TenantID, OpportunityID: state.OpportunityID, LocationID: state.LocationID,
		TriggerMessageID: state.LastMeaningfulID, Severity: severity, PolicyVersion: p.Version(),
		ReasonCode: "NO_RESPONSE_THRESHOLD_EXCEEDED", DueAt: due,
		Reason: fmt.Sprintf("Бизнес не ответил клиенту в течение %d рабочих минут", int(elapsed/time.Minute)),
	}
	return decision, nil
}
