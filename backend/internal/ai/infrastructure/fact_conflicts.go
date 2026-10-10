package infrastructure

import (
	"encoding/json"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

// resolveContradictoryFacts убирает из ответа факты тех типов, по которым модель противоречит
// сама себе (например, BOOKING_INTENT одновременно true и false). Серверная проверка отвергает такой
// ответ целиком, и вместе со спорным фактом пропадали бы все остальные: на DEV v1 так терялся
// один случай из ста. При сомнении модель должна молчать (правило инструкции: ложное
// срабатывание опаснее пропуска), поэтому спорный тип не выдаётся вовсе, а остальное остаётся.
// Одинаковые повторы не считаются противоречием: их объединяет серверная проверка. Ответ, который
// не разбирается, возвращается как есть, чтобы проверка назвала настоящую причину.
func resolveContradictoryFacts(raw string) string {
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &envelope) != nil || len(envelope["facts"]) == 0 {
		return raw
	}
	var facts []domain.SemanticFact
	if json.Unmarshal(envelope["facts"], &facts) != nil {
		return raw
	}
	conflicting := application.ConflictingFactTypes(facts)
	if len(conflicting) == 0 {
		return raw
	}
	kept := make([]domain.SemanticFact, 0, len(facts))
	for _, fact := range facts {
		if !conflicting[fact.Type] {
			kept = append(kept, fact)
		}
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return raw
	}
	envelope["facts"] = encoded
	resolved, err := json.Marshal(envelope)
	if err != nil {
		return raw
	}
	return string(resolved)
}
