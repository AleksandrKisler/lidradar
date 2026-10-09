package infrastructure

import (
	"encoding/json"
	"fmt"
	"strings"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

// Restrict the model's new observations to plausible message anchors. This is
// only a generation aid: server-side evidence and freshness validation remain
// authoritative. Price statements, greetings and isolated refusals cannot be
// invented into promises or completed agreements.
func agreementGenerationCandidates(messages []application.ContextMessage) []any {
	candidates := []any{}
	priorBooking := false
	for i, m := range messages {
		b := strings.ToLower(m.Body)
		has := func(parts ...string) bool {
			for _, p := range parts {
				if strings.Contains(b, p) {
					return true
				}
			}
			return false
		}
		negative := has("отмен", "отказ", "не запис", "не подтверж", "никакой записи", "не приед", "не буду", "не нужно", "не нужна", "не интерес", "не надо")
		deferred := has("подума", "позже", "отлож", "вернусь с решением", "решим завтра", "окончательно отвечу")
		booking := !negative && !deferred && has("запис", "брон", "запланир", "постав", "предлага", "свободн", "окно", "подход", "удобн", "слот", "book", "appoint", "slot")
		promise := m.Direction == "OUTGOING" && !has("возможно", "может быть", "проверил", "отправил", "передал", "подготовил", "уже ") && has("обещ", "проверю", "ответим", "отвечу", "отправлю", "отправим", "пришлю", "пришлём", "пришлем", "перезвон", "уточню", "уточним", "позвоню", "подготовлю", "подготовим", "свяж", "вышлю", "скину", "сделаю", "сделаем", "will ", "i'll ")
		add := func(kind, actor string) {
			allowedIDs := make([]string, 0, len(messages)-i)
			for _, message := range messages[i:] {
				allowedIDs = append(allowedIDs, message.ID)
			}
			statuses := application.AgreementTransitionCandidates(domain.AgreementKind(kind), domain.AgreementWaitingFor(actor), m.Body, messages[i+1:])
			if kind == "BOOKING_CONFIRMATION" && actor == "BUSINESS" {
				if application.ConfirmsBookingOffer(m.Body) {
					for _, earlier := range messages[:i] {
						if earlier.Direction == "OUTGOING" && application.HasExplicitBookingOffer(earlier.Body) {
							return // acceptance resolves the offer, it is not a new wait for the business
						}
					}
				}
				for _, later := range messages[i+1:] {
					if later.Direction == "OUTGOING" && application.HasExplicitBookingOffer(later.Body) {
						return // the concrete offer supersedes the incoming availability request
					}
				}
			}
			// Eliminate states the application would reject (e.g. an old offer
			// after a deferral). This narrows generation, never grants trust.
			request := application.AnalyzeConversationRequestV1{SchemaVersion: application.AnalysisSchemaV2, AnalysisThroughMessageID: messages[len(messages)-1].ID, Messages: messages}
			contextJSON, _ := application.EncodeAnalysisRequest(request)
			validStatuses := statuses[:0]
			for _, status := range statuses {
				observation := domain.AgreementObservation{Kind: domain.AgreementKind(kind), WaitingFor: domain.AgreementWaitingFor(actor), Status: status, TriggerMessageID: m.ID, EvidenceMessageIDs: allowedIDs, Confidence: 1}
				result := domain.AnalysisResultV2{AnalysisThroughMessageID: request.AnalysisThroughMessageID, Agreements: []domain.AgreementObservation{observation}}
				if application.ValidateAgreementEvidence(result, contextJSON) == nil {
					validStatuses = append(validStatuses, status)
				}
			}
			statuses = validStatuses
			if len(statuses) == 0 {
				return
			}
			// Constrain evidence to a complete proof: trigger plus one explicit
			// transition, never a completion message without its original anchor.
			for _, status := range statuses {
				proofs := [][]string{}
				if status == domain.AgreementPending {
					proofs = append(proofs, []string{m.ID})
				} else {
					for _, later := range messages[i+1:] {
						proof := []string{m.ID, later.ID}
						observation := domain.AgreementObservation{Kind: domain.AgreementKind(kind), WaitingFor: domain.AgreementWaitingFor(actor), Status: status, TriggerMessageID: m.ID, EvidenceMessageIDs: proof, Confidence: 1}
						result := domain.AnalysisResultV2{AnalysisThroughMessageID: request.AnalysisThroughMessageID, Agreements: []domain.AgreementObservation{observation}}
						if application.ValidateAgreementEvidence(result, contextJSON) == nil {
							proofs = append(proofs, proof)
						}
					}
				}
				if len(proofs) == 0 {
					continue
				}
				proofJSON, _ := json.Marshal(proofs)
				statusJSON, _ := json.Marshal([]domain.AgreementStatus{status})
				kindJSON, _ := json.Marshal(kind)
				actorJSON, _ := json.Marshal(actor)
				triggerJSON, _ := json.Marshal(m.ID)
				candidates = append(candidates, json.RawMessage(fmt.Sprintf(`{"type":"object","additionalProperties":false,
"required":["kind","waitingFor","status","triggerMessageId","evidenceMessageIds","confidence"],"properties":{
"kind":{"const":%s},"waitingFor":{"const":%s},"status":{"enum":%s},"triggerMessageId":{"const":%s},
"evidenceMessageIds":{"enum":%s},"confidence":{"type":"number","minimum":0,"maximum":1}}}`, kindJSON, actorJSON, statusJSON, triggerJSON, proofJSON)))
			}
		}
		actor := "BUSINESS"
		if m.Direction == "OUTGOING" {
			actor = "CUSTOMER"
		}
		if booking && !(m.Direction == "OUTGOING" && has("записал", "записаны", "записано", "забронировал", "перенесли", "перенесено", "перенёс", "перенесла")) {
			add("BOOKING_CONFIRMATION", actor)
			priorBooking = true
		}
		if !negative && has("перенес", "перенос", "reschedul") && !(m.Direction == "OUTGOING" && has("перенесли", "перенесено", "перенёс", "перенесла", "rescheduled")) {
			add("RESCHEDULE", actor)
		}
		if deferred && priorBooking {
			actor = "CUSTOMER"
			if m.Direction == "OUTGOING" {
				actor = "BUSINESS"
			}
			add("BOOKING_CONFIRMATION", actor)
		}
		if promise {
			add("COMMITMENT", "BUSINESS")
		}
		if has("не могу", "мешает", "проблем", "блок", "нет оплаты", "не получается", "can't", "cannot", "problem", "block") {
			add("PURCHASE_BLOCKER", actor)
		}
	}
	return candidates
}
