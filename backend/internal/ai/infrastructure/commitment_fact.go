package infrastructure

import (
	"encoding/json"
	"strings"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

// ensureCommitmentFact согласует два поля одного ответа v9. Инструкция требует, чтобы каждое
// обещание компании было записано и как COMMITMENT в agreements, и как факт
// BUSINESS_COMMITMENT в facts (его читает PROMISE_NOT_FULFILLED). Небольшая модель пишет facts
// раньше agreements и на простых обещаниях («Сформирую счёт и пришлю его сюда») оставляет
// facts пустым, хотя соглашение записывает: на DEV v1 так пропадало 4 из 15 фактов, а общие доли
// этого не показывали. Ограничение формата надёжнее просьбы в инструкции, поэтому
// поставщик достраивает недостающий факт из соглашения этого же ответа.
//
// Достройка консервативна и никогда не ухудшает ответ:
//   - только если фактов BUSINESS_COMMITMENT нет вовсе (явное решение модели, в том числе
//     value=false, не переопределяется);
//   - только по соглашению COMMITMENT от компании с доверенной уверенностью (>= 0,85);
//   - только если сообщение-основание — исходящее обещание, которое принимает серверная проверка
//     факта (PossibleBusinessCommitmentEvidence), и в нём нет отрицания («не обещаю…»);
//   - доказательство — основание самого позднего такого соглашения: факт ссылается на
//     последнее обещание, как в примерах инструкции;
//   - дополненный ответ обязан пройти ту же серверную проверку, иначе возвращается исходный.
//
// Решение остаётся за приложением: оно, как и прежде, проверяет доказательства и уверенность.
func ensureCommitmentFact(prompt, answer string) string {
	var request application.AnalyzeConversationRequestV1
	if json.Unmarshal([]byte(prompt), &request) != nil {
		return answer
	}
	result, err := application.ValidateAnalysisResultV2(answer, request.AnalysisThroughMessageID)
	if err != nil {
		return answer
	}
	for _, fact := range result.Facts {
		if fact.Type == domain.FactBusinessCommitment {
			return answer
		}
	}
	position := make(map[string]int, len(request.Messages))
	for i, m := range request.Messages {
		position[m.ID] = i
	}
	latest, found := -1, domain.AgreementObservation{}
	for _, agreement := range result.Agreements {
		if agreement.Kind != domain.AgreementCommitment || agreement.WaitingFor != domain.AgreementBusiness || agreement.Confidence < .85 {
			continue
		}
		i, ok := position[agreement.TriggerMessageID]
		if !ok {
			continue
		}
		message := request.Messages[i]
		if !application.PossibleBusinessCommitmentEvidence(message) || negatesPromise(message.Body) {
			continue
		}
		if i > latest {
			latest, found = i, agreement
		}
	}
	if latest < 0 {
		return answer
	}
	result.Facts = append(result.Facts, domain.SemanticFact{
		Type: domain.FactBusinessCommitment, Value: true, Confidence: found.Confidence,
		EvidenceMessageIDs: []string{found.TriggerMessageID},
	})
	encoded, err := json.Marshal(result)
	if err != nil {
		return answer
	}
	completed, err := application.ValidateAnalysisResultV2(string(encoded), request.AnalysisThroughMessageID)
	if err != nil || application.ValidateAgreementEvidence(completed, prompt) != nil {
		return answer
	}
	return string(encoded)
}

// negatesPromise отсекает «не обещаю», «не могу пообещать», «ничего отправлять не планируем»:
// слова обещания в них есть, обещания нет. Лучше не достроить факт, чем достроить ложный.
func negatesPromise(body string) bool {
	for _, word := range strings.Fields(strings.ToLower(body)) {
		switch strings.Trim(word, ".,!?;:()«»") {
		case "не", "нет", "ничего", "никто", "нельзя", "невозможно":
			return true
		}
	}
	return false
}
