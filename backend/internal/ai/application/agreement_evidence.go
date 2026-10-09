package application

import (
	"encoding/json"
	"fmt"
	"strings"

	"lidradar/backend/internal/ai/domain"
)

// ValidateAgreementEvidence binds every observation to the exact, bounded job
// context. The snapshot/revision check remains the responsibility of Complete.
func ValidateAgreementEvidence(result domain.AnalysisResultV2, prompt string) error {
	var request AnalyzeConversationRequestV1
	if err := json.Unmarshal([]byte(prompt), &request); err != nil || request.SchemaVersion != AnalysisSchemaV2 || request.AnalysisThroughMessageID != result.AnalysisThroughMessageID {
		return fmt.Errorf("%w: agreement context unavailable", ErrInvalidAIOutput)
	}
	index := make(map[string]int, len(request.Messages))
	for i, m := range request.Messages {
		index[m.ID] = i
	}
	if len(index) != len(request.Messages) {
		return fmt.Errorf("%w: duplicate context message", ErrInvalidAIOutput)
	}
	for _, fact := range result.Facts {
		for _, id := range fact.EvidenceMessageIDs {
			i, ok := index[id]
			if !ok {
				return fmt.Errorf("%w: fact evidence outside context", ErrInvalidAIOutput)
			}
			if fact.Value {
				m := request.Messages[i]
				switch fact.Type {
				case domain.FactBookingIntent:
					if m.Direction != "INCOMING" || RejectsBookingIntent(m.Body) {
						return fmt.Errorf("%w: booking intent lacks incoming evidence", ErrInvalidAIOutput)
					}
				case domain.FactBusinessCommitment:
					if !PossibleBusinessCommitmentEvidence(m) {
						return fmt.Errorf("%w: business commitment lacks outgoing promise", ErrInvalidAIOutput)
					}
				case domain.FactFollowUpCandidate:
					if m.Direction != "INCOMING" || !HasFollowUpDeferral(m.Body) {
						return fmt.Errorf("%w: follow up lacks incoming deferral", ErrInvalidAIOutput)
					}
				case domain.FactPurchaseIntent:
					if m.Direction != "INCOMING" || !HasExplicitPurchaseIntent(m.Body) {
						return fmt.Errorf("%w: purchase intent lacks incoming order evidence", ErrInvalidAIOutput)
					}
				}
			}
		}
	}
	for _, a := range result.Agreements {
		triggerIndex, ok := index[a.TriggerMessageID]
		if !ok {
			return fmt.Errorf("%w: agreement trigger outside context", ErrInvalidAIOutput)
		}
		trigger := request.Messages[triggerIndex]
		if !validAgreementActor(a, trigger) || (!explicitAgreementTrigger(a.Kind, trigger.Body) && !agreementFactSupportsTrigger(a, result.Facts)) {
			return fmt.Errorf("%w: agreement actor or trigger unsupported", ErrInvalidAIOutput)
		}
		if a.Kind == domain.AgreementBookingConfirmation && isDeferral(trigger.Body) {
			priorOffer := false
			for _, m := range request.Messages[:triggerIndex] {
				if m.Direction == "OUTGOING" && explicitAgreementTrigger(domain.AgreementBookingConfirmation, m.Body) && !isDeferral(m.Body) {
					priorOffer = true
				}
			}
			if !priorOffer {
				return fmt.Errorf("%w: postponement has no booking offer", ErrInvalidAIOutput)
			}
		}
		triggerCited, laterEvidence := false, false
		for _, id := range a.EvidenceMessageIDs {
			i, ok := index[id]
			if !ok || i < triggerIndex {
				return fmt.Errorf("%w: agreement evidence outside current window", ErrInvalidAIOutput)
			}
			if id == a.TriggerMessageID {
				triggerCited = true
			}
			if i > triggerIndex {
				m := request.Messages[i]
				if a.Status == domain.AgreementResolved && m.Direction == waitingForDirection(a.WaitingFor) && resolvesAgreement(a.Kind, trigger.Body, m.Body) {
					laterEvidence = true
				}
				if a.Status == domain.AgreementCancelled && explicitCancellation(m.Body) {
					laterEvidence = true
				}
			}
		}
		if !triggerCited || (a.Status != domain.AgreementPending && !laterEvidence) {
			return fmt.Errorf("%w: agreement lacks explicit transition evidence", ErrInvalidAIOutput)
		}
		if a.Status == domain.AgreementPending {
			if a.Kind == domain.AgreementBookingConfirmation && a.WaitingFor == domain.AgreementBusiness && trigger.Direction == "INCOMING" && ConfirmsBookingOffer(trigger.Body) {
				for _, earlier := range request.Messages[:triggerIndex] {
					if earlier.Direction == "OUTGOING" && HasExplicitBookingOffer(earlier.Body) {
						return fmt.Errorf("%w: confirmation must resolve its booking offer", ErrInvalidAIOutput)
					}
				}
			}
			for _, m := range request.Messages[triggerIndex+1:] {
				if a.Kind == domain.AgreementBookingConfirmation && a.WaitingFor == domain.AgreementBusiness && m.Direction == "OUTGOING" && HasExplicitBookingOffer(m.Body) {
					return fmt.Errorf("%w: business already offered a booking slot", ErrInvalidAIOutput)
				}
				if explicitCancellation(m.Body) || (m.Direction == waitingForDirection(a.WaitingFor) && resolvesAgreement(a.Kind, trigger.Body, m.Body)) {
					return fmt.Errorf("%w: pending agreement contradicts later message", ErrInvalidAIOutput)
				}
				if a.Kind == domain.AgreementBookingConfirmation && m.Direction == trigger.Direction && explicitAgreementTrigger(a.Kind, m.Body) {
					return fmt.Errorf("%w: superseded booking trigger", ErrInvalidAIOutput)
				}
				if a.Kind == domain.AgreementBookingConfirmation && isDeferral(m.Body) {
					return fmt.Errorf("%w: booking postponed after trigger", ErrInvalidAIOutput)
				}
				if a.Kind == domain.AgreementReschedule && explicitAgreementTrigger(a.Kind, m.Body) {
					return fmt.Errorf("%w: superseded reschedule trigger", ErrInvalidAIOutput)
				}
			}
		}
	}
	return nil
}

func validAgreementActor(a domain.AgreementObservation, trigger ContextMessage) bool {
	if a.Kind == domain.AgreementCommitment {
		return trigger.Direction == "OUTGOING" && a.WaitingFor == domain.AgreementBusiness
	}
	if a.Kind == domain.AgreementBookingConfirmation && isDeferral(trigger.Body) {
		return trigger.Direction == "INCOMING" && a.WaitingFor == domain.AgreementCustomer
	}
	if a.Kind == domain.AgreementPurchaseBlocker {
		b := strings.ToLower(trigger.Body)
		if trigger.Direction == "INCOMING" {
			return a.WaitingFor == domain.AgreementBusiness || (a.WaitingFor == domain.AgreementCustomer && containsAny(b, "мне нужно", "я уточню", "попробую", "позже", "i will"))
		}
		return a.WaitingFor == domain.AgreementCustomer || (a.WaitingFor == domain.AgreementBusiness && containsAny(b, "проверим", "исправим", "решим", "we will"))
	}
	if a.Kind == domain.AgreementReschedule && containsAny(strings.ToLower(trigger.Body), "позже", "подума", "отлож", "later", "postpon") {
		return (trigger.Direction == "INCOMING" && a.WaitingFor == domain.AgreementCustomer) || (trigger.Direction == "OUTGOING" && a.WaitingFor == domain.AgreementBusiness)
	}
	if trigger.Direction == "OUTGOING" {
		return a.WaitingFor == domain.AgreementCustomer
	}
	return trigger.Direction == "INCOMING" && a.WaitingFor == domain.AgreementBusiness
}

// WaitingForDirection maps the obligated party to the message direction.
func waitingForDirection(a domain.AgreementWaitingFor) string {
	if a == domain.AgreementCustomer {
		return "INCOMING"
	}
	return "OUTGOING"
}

func explicitAgreementTrigger(kind domain.AgreementKind, body string) bool {
	b := strings.ToLower(body)
	switch kind {
	case domain.AgreementBookingConfirmation:
		return containsAny(b, "запис", "брон", "свободно", "окно", "время", "предлага", "slot", "book", "appoint") || isDeferral(b)
	case domain.AgreementReschedule:
		return containsAny(b, "перенес", "перенос", "другое время", "позже", "подума", "отлож", "reschedul", "postpon", "later")
	case domain.AgreementCommitment:
		return containsAny(b, "обещ", "проверю", "проверим", "ответим", "отвечу", "отправлю", "отправим", "пришлю", "пришлём", "пришлем", "перезвон", "уточню", "уточним", "позвоню", "подготовлю", "подготовим", "свяжусь", "свяжемся", "передам", "передадим", "сообщу", "сообщим", "напишу", "напишем", "вышлю", "скину", "сделаю", "сделаем", "will ", "i'll ")
	case domain.AgreementPurchaseBlocker:
		return containsAny(b, "не могу", "мешает", "проблем", "блок", "нет оплаты", "не получается", "can't", "cannot", "problem", "block")
	}
	return false
}

func isDeferral(body string) bool {
	return containsAny(strings.ToLower(body), "давайте позже", "решим позже", "подума", "отлож", "решим завтра", "окончательно отвечу", "decide later", "postpone")
}

// RejectsBookingIntent excludes an explicit refusal unless another clause
// directly requests or confirms a booking (e.g. cancel Monday, book Tuesday).
func RejectsBookingIntent(body string) bool {
	b := strings.ToLower(body)
	if !containsAny(b, "отмен", "не запис", "не брони", "не подтверж", "никакой записи", "запись не нужна", "запись не требуется") {
		return false
	}
	for _, clause := range strings.FieldsFunc(b, func(r rune) bool { return strings.ContainsRune(".,;!?", r) }) {
		if explicitBookingIntent(clause) || ConfirmsBookingOffer(clause) {
			return false
		}
	}
	return true
}

// ExplicitBookingEvidence identifies a direct request or confirmation usable as
// the most recent proof. Other incoming wording remains for the model to assess.
func ExplicitBookingEvidence(m ContextMessage) bool {
	return m.Direction == "INCOMING" && (explicitBookingIntent(m.Body) || ConfirmsBookingOffer(m.Body))
}

// PossibleBusinessCommitmentEvidence requires an outgoing future action anchor.
// A model still decides whether this is a promise in the conversation context.
func PossibleBusinessCommitmentEvidence(m ContextMessage) bool {
	if m.Direction != "OUTGOING" || containsAny(strings.ToLower(m.Body), "возможно", "может быть") {
		return false
	}
	future := explicitAgreementTrigger(domain.AgreementCommitment, m.Body)
	return future
}

// HasExplicitPurchaseIntent identifies direct order language, excluding mere budgets.
func HasExplicitPurchaseIntent(body string) bool {
	b := strings.ToLower(body)
	return containsAny(b, "хочу заказать", "оформите заказ", "заказываю", "хочу купить", "хочу заказать", "куплю", "покупаю", "оплачу", "i want to buy", "i'll buy", "place an order") && !containsAny(b, "не закаж", "не куп", "пока не", "отмен", "not buy", "don't buy")
}

func explicitBookingIntent(body string) bool {
	b := strings.ToLower(body)
	return containsAny(b, "запис", "брон", "свободно ли", "есть время", "свободное окно", "есть свободное", "можно к вам", "подходит", "book", "appointment") &&
		!containsAny(b, "не запис", "не брони", "пока не", "отмен", "не подходит", "не подтверж", "никакой записи", "не требуется", "not book")
}

func explicitResolution(kind domain.AgreementKind, body string) bool {
	b := strings.ToLower(body)
	if strings.Contains(b, "?") || hasUncertainty(b) || hasNegation(b) {
		return false
	}
	if kind == domain.AgreementCommitment {
		return containsAny(b, "проверил", "проверили", "отправил", "отправили", "выслал", "выслали", "прислал", "прислали", "уточнил", "ответил", "позвонил", "перезвонил", "подготовил", "готово", "done", "sent", "checked", "called", "prepared", "replied", "clarified")
	}
	if kind == domain.AgreementPurchaseBlocker {
		return containsAny(b, "решено", "исправил", "исправлен", "устран", "заработало", "получилось", "оплатил", "оплатила", "resolved", "fixed", "paid")
	}
	if kind == domain.AgreementReschedule && containsAny(b, "перенесли", "перенесено", "перенёс", "перенесла", "rescheduled") {
		return true
	}
	return containsAny(b, "подтвержд", "подходит", "соглас", "устраивает", "оформляйте", "буду в указанное время", "буду ждать вас", "будем ждать вас", "жду вас", "ждём вас", "ждем вас", "оформите запись", "берём", "берем", "выбираю", "запишите", "записали", "записаны", "confirmed", "works for me")
}

func resolvesAgreement(kind domain.AgreementKind, trigger, reply string) bool {
	if !explicitResolution(kind, reply) {
		return false
	}
	if kind != domain.AgreementCommitment {
		return true
	}
	t, r := strings.ToLower(trigger), strings.ToLower(reply)
	if containsAny(r, "готово", "done") {
		return true
	}
	groups := []struct{ future, past []string }{
		{[]string{"проверю", "проверим", "check"}, []string{"проверил", "проверили", "checked"}},
		{[]string{"отправ", "пришл", "вышл", "скину", "send"}, []string{"отправил", "отправили", "выслал", "прислал", "sent"}},
		{[]string{"позвон", "перезвон", "call"}, []string{"позвонил", "перезвонил", "called"}},
		{[]string{"уточн", "clarify"}, []string{"уточнил", "clarified"}},
		{[]string{"отвеч", "ответ", "reply"}, []string{"ответил", "replied"}},
		{[]string{"подготов", "prepare"}, []string{"подготовил", "prepared"}},
	}
	known := false
	for _, group := range groups {
		if containsAny(t, group.future...) {
			known = true
			if containsAny(r, group.past...) {
				return true
			}
		}
	}
	return !known
}

func explicitCancellation(body string) bool {
	b := strings.ToLower(body)
	return !containsAny(b, "?", "если ", "if ", "не отмен", "не отказ", "not cancel", "not decline", "don't cancel") && containsAny(b, "отмен", "отказ", "не подходит", "не нужно", "не надо", "cancel", "decline", "no longer")
}

func containsAny(body string, fragments ...string) bool {
	for _, fragment := range fragments {
		if strings.Contains(body, fragment) {
			return true
		}
	}
	return false
}

func AppliedAgreements(result domain.AnalysisResultV2) []domain.Agreement {
	applied := make([]domain.Agreement, 0, len(result.Agreements))
	for _, a := range result.Agreements {
		if confidenceBand(a.Confidence) == domain.ConfidenceUntrusted {
			continue
		}
		applied = append(applied, domain.Agreement{Kind: a.Kind, WaitingFor: a.WaitingFor, Status: a.Status, TriggerMessageID: a.TriggerMessageID, EvidenceMessageIDs: a.EvidenceMessageIDs, Confidence: a.Confidence, Trusted: confidenceBand(a.Confidence) == domain.ConfidenceStrong})
	}
	return applied
}

func hasNegation(body string) bool {
	for _, word := range strings.Fields(body) {
		word = strings.Trim(word, ".,!?;:()«»")
		if word == "не" || word == "not" || strings.HasSuffix(word, "n't") {
			return true
		}
	}
	return false
}

func hasUncertainty(body string) bool {
	for _, word := range strings.Fields(body) {
		switch strings.Trim(word, ".,!?;:()«»") {
		case "если", "может", "возможно", "if", "maybe":
			return true
		}
	}
	return false
}

func agreementFactSupportsTrigger(a domain.AgreementObservation, facts []domain.SemanticFact) bool {
	for _, fact := range facts {
		if !fact.Value || fact.Confidence < .85 {
			continue
		}
		matches := (a.Kind == domain.AgreementBookingConfirmation && fact.Type == domain.FactBookingIntent) ||
			(a.Kind == domain.AgreementCommitment && fact.Type == domain.FactBusinessCommitment)
		if matches {
			for _, id := range fact.EvidenceMessageIDs {
				if id == a.TriggerMessageID {
					return true
				}
			}
		}
	}
	return false
}

// HasExplicitBookingOffer separates a concrete offer from a promise to check availability.
func HasExplicitBookingOffer(body string) bool {
	b := strings.ToLower(body)
	return !containsAny(b, "возможно", "может быть", "если ", "проверю", "уточню", "нет свобод", "не свобод") && containsAny(b, "свободно", "свободны", "записываю", "записать вас", "вам подходит")
}

// ConfirmsBookingOffer recognises explicit acceptance, not a question or deferral.
func ConfirmsBookingOffer(body string) bool {
	return explicitResolution(domain.AgreementBookingConfirmation, body)
}

// AgreementTransitionCandidates constrains generation to transitions with
// evidence in this snapshot. It grants no trust: the submitted observation
// must still cite that evidence and pass ValidateAgreementEvidence.
func AgreementTransitionCandidates(kind domain.AgreementKind, actor domain.AgreementWaitingFor, trigger string, later []ContextMessage) []domain.AgreementStatus {
	resolved, cancelled := false, false
	for _, m := range later {
		resolved = resolved || (m.Direction == waitingForDirection(actor) && resolvesAgreement(kind, trigger, m.Body))
		cancelled = cancelled || explicitCancellation(m.Body)
	}
	statuses := []domain.AgreementStatus{}
	if !resolved && !cancelled {
		statuses = append(statuses, domain.AgreementPending)
	}
	if resolved {
		statuses = append(statuses, domain.AgreementResolved)
	}
	if cancelled {
		statuses = append(statuses, domain.AgreementCancelled)
	}
	return statuses
}

// HasFollowUpDeferral requires the client to defer a decision or invite later
// contact. Asking for the status of a request is not a deferral.
func HasFollowUpDeferral(body string) bool {
	b := strings.ToLower(body)
	return containsAny(b, "подума", "позже", "поздн", "потом", "отлож", "нужно время", "пауз", "подожду", "свяжусь", "снова напишу", "решу", "решим", "вернусь", "вернемся", "вернёмся", "обсужу", "посоветую", "сравню", "определюсь", "дам ответ", "дам знать", "окончательно отвечу", "напомните", "свяжитесь", "перезвоните", "decide later", "think about", "get back", "remind me") &&
		!containsAny(b, "не пишите", "не звоните", "не связывайтесь", "не напоминайте", "больше не", "окончательно отказ", "don't contact", "do not contact")
}
