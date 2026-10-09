package domain

import "time"

func strongAgreement(a AgreementSignal) bool {
	return a.Confidence >= StrongAgreementConfidence && a.TriggerMessageID != "" && !a.TriggerAt.IsZero() && a.AIRunID != "" && (a.WaitingFor == "CUSTOMER" || a.WaitingFor == "BUSINESS")
}

// Keep an open aggregate attached to its unfinished representative. Otherwise
// choose the earliest deadline, independent of model array order or recency.
func preferAgreement(current *AgreementSignal, due time.Time, candidate AgreementSignal, candidateDue time.Time, activeTrigger string) bool {
	if current == nil {
		return true
	}
	if current.TriggerMessageID == activeTrigger {
		return false
	}
	if candidate.TriggerMessageID == activeTrigger {
		return true
	}
	if !candidateDue.Equal(due) {
		return candidateDue.Before(due)
	}
	if !candidate.TriggerAt.Equal(current.TriggerAt) {
		return candidate.TriggerAt.Before(current.TriggerAt)
	}
	return candidate.TriggerMessageID < current.TriggerMessageID
}
