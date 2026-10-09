package domain

import (
	"fmt"
	"strings"
	"time"
)

const UnfinishedAgreementPolicyVersion = "unfinished-agreement/v2"
const StrongAgreementConfidence = 0.85

type UnfinishedAgreementPolicy struct{}

func (UnfinishedAgreementPolicy) Type() Type      { return TypeUnfinishedAgreement }
func (UnfinishedAgreementPolicy) Version() string { return UnfinishedAgreementPolicyVersion }

func (p UnfinishedAgreementPolicy) Evaluate(state ConversationState, at time.Time) (Decision, error) {
	if err := validateState(state); err != nil || at.IsZero() {
		return Decision{}, ErrInvalidRisk
	}
	if !state.ActiveOpportunity {
		return Decision{Resolve: true}, nil
	}
	// A missing or superseded analysis cannot prove that an agreement ended.
	if !state.AgreementsCurrent {
		return Decision{}, nil
	}
	active, hasActive := state.ActiveRisks[TypeUnfinishedAgreement]
	var pending *AgreementSignal
	var due time.Time
	resolvedActive, transferredActive := false, false
	eligibleCount := 0
	for i := range state.Agreements {
		a := &state.Agreements[i]
		if !strongAgreement(*a) || a.Kind == "COMMITMENT" {
			continue
		}
		if hasActive && a.TriggerMessageID == active.TriggerMessageID && (a.Status == "RESOLVED" || a.Status == "CANCELLED" || a.Superseded) {
			resolvedActive = true
		}
		if a.Superseded || a.Status != "PENDING" || state.ClosedAgreementTriggers[TypeUnfinishedAgreement][a.TriggerMessageID] {
			continue
		}
		if a.Kind != "BOOKING_CONFIRMATION" && a.Kind != "RESCHEDULE" && a.Kind != "PURCHASE_BLOCKER" {
			continue
		}
		if duplicateAgreementAction(state, *a) {
			transferredActive = transferredActive || (hasActive && a.TriggerMessageID == active.TriggerMessageID)
			continue
		}
		candidateDue, err := unfinishedAgreementDue(state, *a)
		if err != nil {
			return Decision{}, err
		}
		eligibleCount++
		if preferAgreement(pending, due, *a, candidateDue, active.TriggerMessageID) {
			pending, due = a, candidateDue
		}
	}
	if hasActive && pending != nil && pending.TriggerMessageID != active.TriggerMessageID && !resolvedActive && !transferredActive {
		// Missing evidence (including a bounded snapshot) cannot replace an
		// unresolved active anchor with a newer, unrelated expectation.
		return Decision{}, nil
	}
	if pending == nil {
		return Decision{Resolve: resolvedActive || transferredActive}, nil
	}
	decision := Decision{DueAt: due, TriggerMessageID: pending.TriggerMessageID,
		Resolve: hasActive && (resolvedActive || transferredActive) && active.TriggerMessageID != pending.TriggerMessageID}
	if at.Before(due) {
		return decision, nil
	}
	actor := "клиента"
	if pending.WaitingFor == "BUSINESS" {
		actor = "компании"
	}
	kind := map[string]string{
		"BOOKING_CONFIRMATION": "подтверждение записи",
		"RESCHEDULE":           "согласование переноса",
		"PURCHASE_BLOCKER":     "устранение препятствия покупке",
	}[pending.Kind]
	if kind == "" {
		return Decision{}, nil
	}
	location, _ := time.LoadLocation(state.BusinessHours.Timezone)
	confidence, runID := pending.Confidence, pending.AIRunID
	decision.Finding = &Finding{
		TenantID: state.TenantID, OpportunityID: state.OpportunityID, LocationID: state.LocationID,
		TriggerMessageID: pending.TriggerMessageID, Severity: SeverityMedium, PolicyVersion: p.Version(),
		ReasonCode: "UNFINISHED_AGREEMENT_DUE",
		Reason: fmt.Sprintf("Ожидается %s от %s по договорённости из сообщения от %s", kind, actor,
			pending.TriggerAt.In(location).Format("02.01 15:04")),
		DueAt: due, Source: SourceHybrid, Confidence: &confidence, AIRunID: &runID,
	}
	if eligibleCount > 1 {
		decision.Finding.Reason += fmt.Sprintf(". Всего незавершённых ожиданий этого типа: %d", eligibleCount)
	}
	return decision, nil
}

func unfinishedAgreementDue(state ConversationState, a AgreementSignal) (time.Time, error) {
	threshold := state.AgreementThreshold
	if threshold == 0 {
		threshold = 120 * time.Minute
	}
	if threshold < time.Minute || threshold > 1440*time.Minute || threshold%time.Minute != 0 {
		return time.Time{}, ErrInvalidRisk
	}
	due, err := state.BusinessHours.AddBusinessTime(a.TriggerAt, threshold)
	if err != nil {
		return time.Time{}, err
	}
	text := strings.ToLower(a.TriggerText)
	if strings.Contains(text, "напиш") || strings.Contains(text, "ответ") || strings.Contains(text, "сообщ") || strings.Contains(text, "подтверж") || strings.Contains(text, "свяж") || strings.Contains(text, "подума") || strings.Contains(text, "отлож") || strings.Contains(text, "вернёмся") || strings.Contains(text, "вернемся") {
		location, err := time.LoadLocation(state.BusinessHours.Timezone)
		if err != nil {
			return time.Time{}, ErrInvalidBusinessHours
		}
		if explicit, ok := ParsePromisedDue(a.TriggerText, a.TriggerAt, location); ok {
			due = explicit.At
		}
	}
	return due, nil
}

func duplicateAgreementAction(state ConversationState, a AgreementSignal) bool {
	// Determine ownership before either rule materializes a risk. Checking
	// only active risks would allow the two-hour rule and a later specialised
	// check to open duplicate risks for the same action.
	for _, policy := range []Policy{NoResponsePolicy{}, BookingNotConfirmedPolicy{}, PromiseNotFulfilledPolicy{}, CustomerSilentAfterPricePolicy{}, FollowUpCandidatePolicy{}} {
		decision, err := policy.Evaluate(state, a.TriggerAt)
		if err == nil && decision.TriggerMessageID == a.TriggerMessageID && !decision.DueAt.IsZero() {
			return true
		}
		if active, ok := state.ActiveRisks[policy.Type()]; ok && active.TriggerMessageID == a.TriggerMessageID {
			return true
		}
	}
	return false
}
