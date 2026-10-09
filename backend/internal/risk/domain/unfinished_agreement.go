package domain

import (
	"fmt"
	"strings"
	"time"
)

const UnfinishedAgreementPolicyVersion = "unfinished-agreement/v1"
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
	resolvedActive := false
	for i := range state.Agreements {
		a := &state.Agreements[i]
		if a.Confidence < StrongAgreementConfidence || a.TriggerMessageID == "" || a.TriggerAt.IsZero() || a.AIRunID == "" {
			continue
		}
		if hasActive && a.TriggerMessageID == active.TriggerMessageID &&
			(a.Status == "RESOLVED" || a.Status == "CANCELLED") {
			resolvedActive = true
		}
		if a.Status != "PENDING" || a.Kind == "COMMITMENT" ||
			(a.WaitingFor != "CUSTOMER" && a.WaitingFor != "BUSINESS") {
			continue
		}
		if pending == nil || a.TriggerAt.After(pending.TriggerAt) ||
			(a.TriggerAt.Equal(pending.TriggerAt) && a.TriggerMessageID > pending.TriggerMessageID) {
			pending = a
		}
	}
	if pending == nil {
		return Decision{Resolve: resolvedActive}, nil
	}
	// An older agreement visible in a bounded snapshot must not replace the
	// active, newer trigger merely because the newer text fell out of context.
	if hasActive && active.TriggerMessageID != pending.TriggerMessageID && !active.TriggerAt.IsZero() &&
		!pending.TriggerAt.After(active.TriggerAt) {
		return Decision{}, nil
	}
	if duplicateAgreementAction(state, *pending) {
		return Decision{Resolve: hasActive && active.TriggerMessageID == pending.TriggerMessageID}, nil
	}
	threshold := state.AgreementThreshold
	if threshold == 0 {
		threshold = 120 * time.Minute
	}
	if threshold < time.Minute || threshold > 1440*time.Minute || threshold%time.Minute != 0 {
		return Decision{}, ErrInvalidRisk
	}
	due, err := state.BusinessHours.AddBusinessTime(pending.TriggerAt, threshold)
	if err != nil {
		return Decision{}, err
	}
	// Appointment slots are not reply deadlines. Only an explicit promise to
	// answer/contact/confirm makes the existing time parser applicable.
	text := strings.ToLower(pending.TriggerText)
	if strings.Contains(text, "напиш") || strings.Contains(text, "ответ") ||
		strings.Contains(text, "сообщ") || strings.Contains(text, "подтверж") ||
		strings.Contains(text, "свяж") || strings.Contains(text, "подума") || strings.Contains(text, "отлож") || strings.Contains(text, "вернёмся") || strings.Contains(text, "вернемся") {
		location, err := time.LoadLocation(state.BusinessHours.Timezone)
		if err != nil {
			return Decision{}, ErrInvalidBusinessHours
		}
		if explicit, ok := ParsePromisedDue(pending.TriggerText, pending.TriggerAt, location); ok {
			due = explicit.At
		}
	}
	decision := Decision{DueAt: due, TriggerMessageID: pending.TriggerMessageID,
		Resolve: hasActive && active.TriggerMessageID != pending.TriggerMessageID}
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
	return decision, nil
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
