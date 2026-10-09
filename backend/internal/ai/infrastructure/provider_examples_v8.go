package infrastructure

import "encoding/json"

// Compact examples leave room for message context and a complete answer.
var analysisFewShotMessagesV8 = append([]map[string]string{
	{"role": "user", "content": `{"analysisThroughMessageId":"m3","messages":[{"id":"m1","direction":"INCOMING","body":"Можно записаться на стрижку?"},{"id":"m2","direction":"OUTGOING","body":"Свободно во вторник в 11:15. Записываю?"},{"id":"m3","direction":"INCOMING","body":"Подтверждаю, время устраивает."}]}`},
	{"role": "assistant", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m3","summary":"Клиент подтвердил предложенное время.","facts":[{"type":"BOOKING_INTENT","value":true,"confidence":0.99,"evidenceMessageIds":["m3"]}],"agreements":[{"kind":"BOOKING_CONFIRMATION","waitingFor":"CUSTOMER","status":"RESOLVED","triggerMessageId":"m2","evidenceMessageIds":["m2","m3"],"confidence":0.99}]}`},
	{"role": "user", "content": `{"analysisThroughMessageId":"m2","messages":[{"id":"m1","direction":"INCOMING","body":"Хочу заказать набор, но не могу оплатить картой."},{"id":"m2","direction":"OUTGOING","body":"Цена набора 1750 RUB."}]}`},
	{"role": "assistant", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","summary":"Клиент хочет заказать набор, требуется помощь с оплатой.","facts":[{"type":"PURCHASE_INTENT","value":true,"confidence":0.99,"evidenceMessageIds":["m1"]},{"type":"PRICE_MENTIONED","value":true,"confidence":0.99,"evidenceMessageIds":["m2"],"amount":"1750","currency":"RUB"}],"agreements":[{"kind":"PURCHASE_BLOCKER","waitingFor":"BUSINESS","status":"PENDING","triggerMessageId":"m1","evidenceMessageIds":["m1"],"confidence":0.99}]}`},
	{"role": "user", "content": `{"analysisThroughMessageId":"m2","messages":[{"id":"m1","direction":"OUTGOING","body":"Перезвоню после согласования."},{"id":"m2","direction":"OUTGOING","body":"Здравствуйте!"}]}`},
	{"role": "assistant", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","summary":"Компания обещала перезвонить, выполнения пока нет.","facts":[{"type":"BUSINESS_COMMITMENT","value":true,"confidence":0.99,"evidenceMessageIds":["m1"]}],"agreements":[{"kind":"COMMITMENT","waitingFor":"BUSINESS","status":"PENDING","triggerMessageId":"m1","evidenceMessageIds":["m1"],"confidence":0.99}]}`},
	{"role": "user", "content": `{"analysisThroughMessageId":"m1","companyContext":"Каталог: 800 RUB.","messages":[{"id":"m1","direction":"INCOMING","body":"Сколько стоит? Пока не записываюсь, напишу после обсуждения с семьёй."}]}`},
	{"role": "assistant", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m1","summary":"Клиент уточняет цену и откладывает решение.","facts":[{"type":"FOLLOW_UP_CANDIDATE","value":true,"confidence":0.99,"evidenceMessageIds":["m1"]}],"agreements":[]}`},
	{"role": "user", "content": `{"analysisThroughMessageId":"m3","messages":[{"id":"m1","direction":"INCOMING","body":"Запишите на осмотр."},{"id":"m2","direction":"OUTGOING","body":"Свободно в среду, записать вас?"},{"id":"m3","direction":"INCOMING","body":"Дайте подумать, вернусь с ответом послезавтра."}]}`},
	{"role": "assistant", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m3","summary":"Клиент отложил подтверждение записи.","facts":[{"type":"BOOKING_INTENT","value":true,"confidence":0.99,"evidenceMessageIds":["m1"]},{"type":"FOLLOW_UP_CANDIDATE","value":true,"confidence":0.99,"evidenceMessageIds":["m3"]}],"agreements":[{"kind":"BOOKING_CONFIRMATION","waitingFor":"CUSTOMER","status":"PENDING","triggerMessageId":"m3","evidenceMessageIds":["m3"],"confidence":0.99}]}`},
	{"role": "user", "content": `{"analysisThroughMessageId":"m1","messages":[{"id":"m1","direction":"INCOMING","body":"Не записывайте: обращение отправлено по ошибке."}]}`},
	{"role": "assistant", "content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m1","summary":"Клиент отказался от ошибочного обращения.","facts":[],"agreements":[]}`},
}, analysisFewShotMessagesV7[:2]...)

// The established negative/price/follow-up examples stay available when a
// dialogue contains no agreement anchor. Only the result envelope changes.
var analysisFactExamplesV8 = func() []map[string]string {
	result := make([]map[string]string, 0, len(analysisFewShotMessagesV6))
	for _, message := range analysisFewShotMessagesV6 {
		var body map[string]any
		if err := json.Unmarshal([]byte(message["content"]), &body); err != nil {
			panic(err)
		}
		body["schemaVersion"] = "analyze-conversation.v2"
		if message["role"] == "user" {
			body["promptVersion"] = "analyze-conversation.prompt.v8"
		} else {
			body["agreements"] = []any{}
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		result = append(result, map[string]string{"role": message["role"], "content": string(encoded)})
	}
	return result
}()
