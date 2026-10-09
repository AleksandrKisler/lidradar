package application

import (
	"encoding/json"

	"lidradar/backend/internal/ai/domain"
)

type agreementKey struct {
	kind    domain.AgreementKind
	trigger string
}

// MergeAgreementProjection retains independently anchored observations. valid
// describes whether each old entry still matches its original prompt and the
// canonical messages. Fresh entries have already passed Complete validation.
func MergeAgreementProjection(old, fresh []domain.Agreement, valid []bool, oldRunID, newRunID, currentPrompt string) []domain.Agreement {
	merged := make([]domain.Agreement, 0, len(old)+len(fresh))
	positions := make(map[agreementKey]int, len(old)+len(fresh))
	for i, agreement := range old {
		if agreement.SourceRunID == "" {
			agreement.SourceRunID = oldRunID // legacy projection
		}
		if i >= len(valid) || !valid[i] {
			agreement.Trusted = false
		}
		key := agreementKey{agreement.Kind, agreement.TriggerMessageID}
		if _, exists := positions[key]; !exists {
			positions[key] = len(merged)
			merged = append(merged, agreement)
		}
	}
	var request AnalyzeConversationRequestV1
	_ = json.Unmarshal([]byte(currentPrompt), &request)
	for _, agreement := range fresh {
		agreement.SourceRunID = newRunID
		key := agreementKey{agreement.Kind, agreement.TriggerMessageID}
		if at, exists := positions[key]; exists {
			previous := merged[at]
			// A weak observation cannot erase trusted state; a terminal anchor
			// cannot be reopened by replaying the original message.
			if previous.Status != domain.AgreementPending && agreement.Status == domain.AgreementPending {
				continue
			}
			if previous.Trusted && (!agreement.Trusted || previous.Status != domain.AgreementPending) {
				continue
			}
			if previous.Trusted && previous.Status == agreement.Status {
				// Reobserving an unchanged state adds no new evidence. Keep
				// the original evidence set and its actual source run.
				continue
			}
			merged[at] = agreement
			continue
		}
		if agreement.Trusted && agreement.Status == domain.AgreementPending && agreement.Kind == domain.AgreementBookingConfirmation {
			for i := range merged {
				previous := &merged[i]
				if previous.Trusted && previous.Kind == agreement.Kind && previous.Status == domain.AgreementPending &&
					bookingContinuation(*previous, agreement, request.Messages) {
					// Keep the original state/provenance and record the explicit
					// continuation separately, so risk policies can distinguish it
					// from a missing or independently pending anchor.
					previous.SupersededByTriggerMessageID = agreement.TriggerMessageID
				}
			}
		}
		positions[key] = len(merged)
		merged = append(merged, agreement)
	}
	return merged
}

// A fresh offer answers a single earlier request, or an explicit deferral
// continues a single offer. Other relationships remain independent.
func bookingContinuation(old, fresh domain.Agreement, messages []ContextMessage) bool {
	oldAt, freshAt := -1, -1
	for i, message := range messages {
		if message.ID == old.TriggerMessageID {
			oldAt = i
		}
		if message.ID == fresh.TriggerMessageID {
			freshAt = i
		}
	}
	// Without an adjacent reply, the relationship between two booking
	// anchors is ambiguous and both expectations stay visible.
	if oldAt < 0 || freshAt != oldAt+1 {
		return false
	}
	previous, next := messages[oldAt], messages[freshAt]
	if old.WaitingFor == domain.AgreementBusiness && previous.Direction == "INCOMING" &&
		fresh.WaitingFor == domain.AgreementCustomer && next.Direction == "OUTGOING" && HasExplicitBookingOffer(next.Body) {
		return true
	}
	if old.WaitingFor == domain.AgreementCustomer && previous.Direction == "OUTGOING" && HasExplicitBookingOffer(previous.Body) &&
		fresh.WaitingFor == domain.AgreementCustomer && next.Direction == "INCOMING" && isDeferral(next.Body) {
		return true
	}
	return false
}
