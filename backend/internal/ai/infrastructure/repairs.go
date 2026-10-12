package infrastructure

import (
	"encoding/json"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

// Починки ответа v9. Серверная проверка отвергает ответ целиком, если в нём есть одна неверная запись, и вместе с ней пропадают
// верные: на слепой выборке так терялись два обещания из девяноста, хотя договорённость и факт в этих ответах были правильными.
// Починка убирает только спорную запись и ничего не добавляет; при сомнении модель должна молчать (правило инструкции), а
// остальное остаётся. Как и resolveContradictoryFacts, она работает до серверной проверки и не смягчает её.

// dedupeAgreements оставляет первую из договорённостей с одним видом и одним основанием: повтор серверная проверка отвергает как
// «duplicate or contradictory agreement». Ответ, который не разбирается, возвращается как есть.
func dedupeAgreements(raw string) string {
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &envelope) != nil || len(envelope["agreements"]) == 0 {
		return raw
	}
	var agreements []domain.AgreementObservation
	if json.Unmarshal(envelope["agreements"], &agreements) != nil {
		return raw
	}
	type key struct {
		kind    domain.AgreementKind
		trigger string
	}
	seen := make(map[key]bool, len(agreements))
	kept := make([]domain.AgreementObservation, 0, len(agreements))
	for _, agreement := range agreements {
		k := key{agreement.Kind, agreement.TriggerMessageID}
		if seen[k] {
			continue
		}
		seen[k] = true
		kept = append(kept, agreement)
	}
	if len(kept) == len(agreements) {
		return raw
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return raw
	}
	envelope["agreements"] = encoded
	repaired, err := json.Marshal(envelope)
	if err != nil {
		return raw
	}
	return string(repaired)
}

// dropUnsupportedPrices убирает положительный факт о цене, суммы которого нет в цитируемых сообщениях («200 штук» из сообщения клиента
// вместо цены в сообщении компании). Ответ целиком отвергается проверкой цены (application.ValidatePriceEvidence), хотя остальные
// записи верны. Ответ, который не разбирается, возвращается как есть.
func dropUnsupportedPrices(prompt, answer string) string {
	var request application.AnalyzeConversationRequestV1
	if json.Unmarshal([]byte(prompt), &request) != nil {
		return answer
	}
	result, err := application.ValidateAnalysisResultV2(answer, request.AnalysisThroughMessageID)
	if err != nil {
		return answer
	}
	kept := make([]domain.SemanticFact, 0, len(result.Facts))
	for _, fact := range result.Facts {
		if fact.Type == domain.FactPriceMentioned && fact.Value {
			single := domain.AnalysisResultV1{AnalysisThroughMessageID: result.AnalysisThroughMessageID, Facts: []domain.SemanticFact{fact}}
			if application.ValidatePriceEvidence(single, prompt) != nil {
				continue
			}
		}
		kept = append(kept, fact)
	}
	if len(kept) == len(result.Facts) {
		return answer
	}
	result.Facts = kept
	encoded, err := json.Marshal(result)
	if err != nil {
		return answer
	}
	return string(encoded)
}
