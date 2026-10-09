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
	decision := Decision{DueAt: due, TriggerMessageID: state.LastMeaningfulID}
	if state.LastMeaningful != DirectionIncoming || !state.ActiveOpportunity {
		return Decision{Resolve: true}, nil
	}
	if state.AgreementsCurrent {
		terminalLatest := false
		pendingBusiness := false
		var latest *AgreementSignal
		for i := range state.Agreements {
			a := &state.Agreements[i]
			if a.Confidence >= StrongAgreementConfidence && (latest == nil || a.TriggerAt.After(latest.TriggerAt)) {
				latest = a
			}
		}
		if latest != nil && latest.Status == "PENDING" && latest.WaitingFor == "CUSTOMER" {
			return Decision{Resolve: true}, nil
		}
		for _, agreement := range state.Agreements {
			if agreement.Confidence < StrongAgreementConfidence {
				continue
			}
			if agreement.Status == "PENDING" && agreement.WaitingFor == "BUSINESS" &&
				agreement.Kind == "BOOKING_CONFIRMATION" && BookingRiskEligible(state.OpportunityStage) &&
				agreement.TriggerMessageID == state.LastMeaningfulID {
				return Decision{Resolve: true}, nil // the specialised booking rule owns this action
			}
			if agreement.Status == "PENDING" && agreement.WaitingFor == "BUSINESS" && agreement.TriggerMessageID == state.LastMeaningfulID {
				pendingBusiness = true
			}
			if agreement.Status == "RESOLVED" || agreement.Status == "CANCELLED" {
				for _, id := range agreement.EvidenceMessageIDs {
					if id == state.LastMeaningfulID {
						terminalLatest = true
					}
				}
			}
		}
		if terminalLatest && !pendingBusiness {
			return Decision{Resolve: true}, nil
		}
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
		Source: SourceRule,
	}
	return decision, nil
}
