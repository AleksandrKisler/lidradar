package infrastructure

import "lidradar/backend/internal/ai/application"

// V6/V8's price-grounding example accidentally labelled an explicit
// availability request as having no facts. Preserve historical prompts while
// removing that contradictory supervision from V9.
var analysisFactExamplesV9 = func() []map[string]string {
	result := make([]map[string]string, len(analysisFactExamplesV8))
	for i, m := range analysisFactExamplesV8 {
		result[i] = map[string]string{"role": m["role"], "content": m["content"]}
	}
	result[3]["content"] = `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"example-message-2","summary":"Клиент уточняет доступность времени; цена в сообщениях не названа.","facts":[{"type":"BOOKING_INTENT","value":true,"confidence":0.99,"evidenceMessageIds":["example-message-1"]}],"agreements":[]}`
	return result
}()

// Two outgoing promises are independent even without a customer request.
var independentAgreementExampleV9 = []map[string]string{
	{"role": "user", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","messages":[{"id":"m1","direction":"OUTGOING","body":"Пришлю ссылку на оплату."},{"id":"m2","direction":"OUTGOING","body":"Проверю наличие детали."}]}`},
	{"role": "assistant", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","summary":"Компания обещала прислать ссылку и проверить наличие.","facts":[{"type":"BUSINESS_COMMITMENT","value":true,"confidence":0.99,"evidenceMessageIds":["m2"]}],"agreements":[{"kind":"COMMITMENT","waitingFor":"BUSINESS","status":"PENDING","triggerMessageId":"m1","evidenceMessageIds":["m1"],"confidence":0.99},{"kind":"COMMITMENT","waitingFor":"BUSINESS","status":"PENDING","triggerMessageId":"m2","evidenceMessageIds":["m2"],"confidence":0.99}]}`},
	{"role": "user", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m3","messages":[{"id":"m1","direction":"OUTGOING","body":"Пришлю ссылку на оплату."},{"id":"m2","direction":"OUTGOING","body":"Проверю наличие детали."},{"id":"m3","direction":"OUTGOING","body":"Ссылку на оплату отправил."}]}`},
	{"role": "assistant", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m3","summary":"Ссылка отправлена, наличие ещё проверяется.","facts":[{"type":"BUSINESS_COMMITMENT","value":true,"confidence":0.99,"evidenceMessageIds":["m2"]}],"agreements":[{"kind":"COMMITMENT","waitingFor":"BUSINESS","status":"RESOLVED","triggerMessageId":"m1","evidenceMessageIds":["m1","m3"],"confidence":0.99},{"kind":"COMMITMENT","waitingFor":"BUSINESS","status":"PENDING","triggerMessageId":"m2","evidenceMessageIds":["m2"],"confidence":0.99}]}`},
	{"role": "user", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m3","messages":[{"id":"m1","direction":"OUTGOING","body":"Пришлю ссылку на оплату."},{"id":"m2","direction":"OUTGOING","body":"Проверю наличие детали."},{"id":"m3","direction":"INCOMING","body":"Проверять наличие не нужно."}]}`},
	{"role": "assistant", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m3","summary":"Проверка наличия отменена, ссылка ещё ожидается.","facts":[{"type":"BUSINESS_COMMITMENT","value":true,"confidence":0.99,"evidenceMessageIds":["m2"]}],"agreements":[{"kind":"COMMITMENT","waitingFor":"BUSINESS","status":"PENDING","triggerMessageId":"m1","evidenceMessageIds":["m1"],"confidence":0.99},{"kind":"COMMITMENT","waitingFor":"BUSINESS","status":"CANCELLED","triggerMessageId":"m2","evidenceMessageIds":["m2","m3"],"confidence":0.99}]}`},
}

// Keep the examples within the small deployed context. They illustrate the
// output format; eligibility never forces a positive model observation.
func selectExamplesV9(messages []application.ContextMessage) []map[string]string {
	promises, purchase, booking := 0, false, false
	for _, m := range messages {
		if application.PossibleBusinessCommitmentEvidence(m) {
			promises++
		}
		purchase = purchase || (m.Direction == "INCOMING" && application.HasExplicitPurchaseIntent(m.Body))
		booking = booking || application.ExplicitBookingEvidence(m) || (m.Direction == "OUTGOING" && application.HasExplicitBookingOffer(m.Body))
	}
	if promises > 1 && !booking {
		return independentAgreementExampleV9
	}
	if purchase && len(agreementGenerationCandidates(messages)) == 0 {
		return purchaseExampleV9
	}
	if len(agreementGenerationCandidates(messages)) > 0 {
		return analysisFewShotMessagesV8
	}
	if len(messages) > 6 {
		return analysisFactExamplesV9[4:8]
	}
	return analysisFactExamplesV9
}

var purchaseExampleV9 = []map[string]string{
	{"role": "user", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m1","messages":[{"id":"m1","direction":"INCOMING","body":"Первый заказ отмените. Хочу приобрести новый комплект."}]}`},
	{"role": "assistant", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m1","summary":"Клиент отменяет прежний заказ и просит оформить новый.","facts":[{"type":"PURCHASE_INTENT","value":true,"confidence":0.99,"evidenceMessageIds":["m1"]}],"agreements":[]}`},
}
