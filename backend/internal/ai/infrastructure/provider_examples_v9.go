package infrastructure

import (
	"strings"

	"lidradar/backend/internal/ai/application"
)

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
func selectExamples(messages []application.ContextMessage, scanned scanEligibility) []map[string]string {
	examples := selectBaseExamples(messages, scanned)
	if !scanned.admitsBeyondLexicon(messages) {
		return examples
	}
	// Переписка с обещанием без слов словаря: после общих примеров идут примеры такого обещания и контрпример справки.
	return append(append(make([]map[string]string, 0, len(examples)+len(openPromiseExamplesV9)), examples...), openPromiseExamplesV9...)
}

func selectBaseExamples(messages []application.ContextMessage, scanned scanEligibility) []map[string]string {
	promises, purchase, booking := 0, false, false
	for _, m := range messages {
		if scanned.allows(m) {
			promises++
		}
		purchase = purchase || (m.Direction == "INCOMING" && application.HasExplicitPurchaseIntent(m.Body))
		booking = booking || application.ExplicitBookingEvidence(m) || (m.Direction == "OUTGOING" && application.HasExplicitBookingOffer(m.Body))
	}
	if promises > 1 && !booking {
		return independentAgreementExampleV9
	}
	if purchase && len(agreementCandidates(messages, scanned)) == 0 {
		return purchaseExampleV9
	}
	if len(agreementCandidates(messages, scanned)) > 0 {
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

// openPromiseExamplesV9 показывают обещание, которое словарь не узнаёт: глагол услуги в третьем лице («мастер приедет и устранит») и
// редкую форму первого лица множественного числа, и контрпример: справка о порядке оплаты, где то же будущее время ничего не обещает.
// Они идут, только если проход по обещаниям допустил сообщение без слов словаря (promise_scan.go).
var openPromiseExamplesV9 = []map[string]string{
	{"role": "user", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","messages":[{"id":"m1","direction":"INCOMING","body":"После вашего ремонта стиральная машина снова течёт."},{"id":"m2","direction":"OUTGOING","body":"Добрый день! Извините. Мастер приедет завтра между 10 и 12 и устранит течь по гарантии."}]}`},
	{"role": "assistant", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","summary":"Клиент сообщил о течи после ремонта; компания обещала выезд мастера завтра.","facts":[{"type":"BUSINESS_COMMITMENT","value":true,"confidence":0.99,"evidenceMessageIds":["m2"]}],"agreements":[{"kind":"COMMITMENT","waitingFor":"BUSINESS","status":"PENDING","triggerMessageId":"m2","evidenceMessageIds":["m2"],"confidence":0.99}]}`},
	{"role": "user", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","messages":[{"id":"m1","direction":"INCOMING","body":"Мне прислали не тот размер."},{"id":"m2","direction":"OUTGOING","body":"Приносим извинения. Заменим на нужный размер за наш счёт: курьер заберёт старую пару и привезёт новую."}]}`},
	{"role": "assistant", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","summary":"Клиенту прислали не тот размер; компания обещала замену за свой счёт.","facts":[{"type":"BUSINESS_COMMITMENT","value":true,"confidence":0.99,"evidenceMessageIds":["m2"]}],"agreements":[{"kind":"COMMITMENT","waitingFor":"BUSINESS","status":"PENDING","triggerMessageId":"m2","evidenceMessageIds":["m2"],"confidence":0.99}]}`},
	{"role": "user", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","messages":[{"id":"m1","direction":"INCOMING","body":"Как проходит оплата?"},{"id":"m2","direction":"OUTGOING","body":"Оплата принимается картой и наличными. Курьер привозит терминал с собой и выдаёт чек."}]}`},
	{"role": "assistant", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","summary":"Клиент спросил про оплату; компания описала порядок, обещаний нет.","facts":[],"agreements":[]}`},
	{"role": "user", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","messages":[{"id":"m1","direction":"INCOMING","body":"Не открывается личный кабинет."},{"id":"m2","direction":"OUTGOING","body":"Добрый день! Пришлите скриншот ошибки и напишите, каким браузером пользуетесь."}]}`},
	{"role": "assistant", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","summary":"Клиент не может войти в кабинет; компания просит скриншот и название браузера, обещаний нет.","facts":[],"agreements":[]}`},
	{"role": "user", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","messages":[{"id":"m1","direction":"INCOMING","body":"Когда будут в наличии кроссовки?"},{"id":"m2","direction":"OUTGOING","body":"Новая партия приезжает на склад в среду, после этого размеры появятся на сайте."}]}`},
	{"role": "assistant", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","summary":"Клиент спросил о наличии; компания назвала ожидаемую дату поставки, обещаний нет.","facts":[],"agreements":[]}`},
	{"role": "user", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","messages":[{"id":"m1","direction":"INCOMING","body":"Отменил заказ, а деньги не вернулись."},{"id":"m2","direction":"OUTGOING","body":"Вижу отмену. Деньги вернём на вашу карту завтра, банк зачислит их за пару дней."}]}`},
	{"role": "assistant", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","summary":"Клиент отменил заказ, компания обещала вернуть деньги завтра.","facts":[{"type":"BUSINESS_COMMITMENT","value":true,"confidence":0.99,"evidenceMessageIds":["m2"]}],"agreements":[{"kind":"COMMITMENT","waitingFor":"BUSINESS","status":"PENDING","triggerMessageId":"m2","evidenceMessageIds":["m2"],"confidence":0.99}]}`},
}

// scanHint сообщает основному вызову, какие сообщения проход по обещаниям счёл вероятными обещаниями вне словаря. Подсказка называет только
// псевдонимы сообщений; решение остаётся за моделью, а исключения (справка, просьба клиенту, предложение «могу») названы прямо.
func scanHint(messages []application.ContextMessage, scanned scanEligibility) string {
	var flagged []string
	for _, m := range messages {
		if m.Direction == "OUTGOING" && scanned[m.ID] && !application.PossibleBusinessCommitmentEvidence(m) {
			flagged = append(flagged, m.ID)
		}
	}
	if len(flagged) == 0 {
		return ""
	}
	return "\nПроверка отметила как вероятные обещания компании сообщения " + strings.Join(flagged, ", ") + ". Оцени каждое по смыслу. Если компания берёт на себя " +
		"действие для клиента (привезти, вернуть деньги, исправить, отправить, сообщить, организовать), запиши его как договорённость COMMITMENT и " +
		"факт BUSINESS_COMMITMENT. Справка о сроках и статусе, просьба к клиенту, предложение «могу», решение другой стороны и рассказ о том, " +
		"как проходит услуга обещанием не являются: тогда ничего не записывай."
}
