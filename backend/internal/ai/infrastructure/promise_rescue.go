package infrastructure

import (
	"encoding/json"
	"math"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

// Спасение обещаний (ADR 0055). Основной вызов решает за раз много задач и по замерам пропускает 13–16% обещаний, которые словарь
// допустил (безличные формы «будет отправлен», переписка, начатая сообщением компании, будущее время, прочитанное как прошедшее), и
// около пятой части обещаний без слов словаря, которые проход назвал promise. Проход задаёт тот же вопрос отдельно и на этих формах
// ошибается заметно реже.
//
// Спасение добавляет договорённость COMMITMENT (и факт BUSINESS_COMMITMENT) только если одновременно выполнено всё:
//   - сообщение исходящее, не исключённое структурой, и либо в нём есть слово обещания, не исключённое структурой и стоящее в
//     повествовательном предложении (слово в вопросе «Будет ли удобно, если тренер перезвонит?» предлагает, а не обещает), либо слов
//     словаря нет, но проход ответил promise с вероятностью не ниже promiseRescueOpenThreshold;
//   - проход назвал сообщение promise с вероятностью не ниже promiseRescueThreshold (для словарных) или promiseRescueOpenThreshold (для
//     остальных): без слова словаря единственное основание — проход, поэтому порог выше;
//   - основной вызов договорённости на это сообщение не записал;
//   - состояние договорённости (ожидает, выполнена, отменена) и доказательства определены теми же правилами, что и допуск модели
//     (application.AgreementTransitionCandidates), а итоговый ответ проходит серверную проверку.
const (
	promiseRescueThreshold     = 0.9
	promiseRescueOpenThreshold = 0.99
)

// rescueCommitments дополняет ответ. probabilities — вероятность «обещание» по идентификатору сообщения запроса. Любая неудача
// возвращает ответ без изменений.
func rescueCommitments(prompt, answer string, probabilities map[string]float64) string {
	var request application.AnalyzeConversationRequestV1
	if json.Unmarshal([]byte(prompt), &request) != nil {
		return answer
	}
	result, err := application.ValidateAnalysisResultV2(answer, request.AnalysisThroughMessageID)
	if err != nil {
		return answer
	}
	through := -1
	for i, m := range request.Messages {
		if m.ID == request.AnalysisThroughMessageID {
			through = i
		}
	}
	if through < 0 {
		return answer
	}
	taken := map[string]bool{}
	for _, a := range result.Agreements {
		if a.Kind == domain.AgreementCommitment {
			taken[a.TriggerMessageID] = true
		}
	}
	rescued := false
	for i := 0; i <= through; i++ {
		m := request.Messages[i]
		probability := probabilities[m.ID]
		if m.Direction != "OUTGOING" || taken[m.ID] || !rescuable(m.Body, probability) {
			continue
		}
		confidence := math.Round(math.Min(promiseConfidenceMax, math.Max(promiseConfidenceMin, probability))*100) / 100
		if agreement, ok := commitmentAgreementFor(prompt, request, i, through, confidence); ok {
			result.Agreements = append(result.Agreements, agreement)
			rescued = true
		}
	}
	if !rescued {
		return answer
	}
	result.Facts = latestPromiseFact(request, result)
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

// rescuable: проход назвал сообщение promise достаточно уверенно для своего случая. Со словом словаря в повествовательном предложении
// хватает promiseRescueThreshold; без слова словаря сообщение не должно быть исключено структурой, и порог выше.
func rescuable(body string, probability float64) bool {
	switch application.CommitmentSupport(body) {
	case application.LexicalAdmitted:
		return probability >= promiseRescueThreshold && application.CommitmentStated(body)
	case application.LexicalOpen:
		return probability >= promiseRescueOpenThreshold
	}
	return false
}

// Уверенность добавленных записей: вероятность модели не откалибрована, поэтому запись получает «сильную» уверенность не ниже границы
// доверия (0,85) и не выше 0,99.
const (
	promiseConfidenceMin = 0.85
	promiseConfidenceMax = 0.99
)

// commitmentAgreementFor строит договорённость COMMITMENT по сообщению-обещанию: «ожидает», пока дальше нет явного выполнения или
// отмены, иначе «выполнена» или «отменена» с первым подтверждающим сообщением. Состояние и доказательства проверяет серверная проверка.
func commitmentAgreementFor(prompt string, request application.AnalyzeConversationRequestV1, trigger, through int, confidence float64) (domain.AgreementObservation, bool) {
	messages := request.Messages[:through+1]
	later := messages[trigger+1:]
	statuses := application.AgreementTransitionCandidates(domain.AgreementCommitment, domain.AgreementBusiness, messages[trigger].Body, later)
	for _, status := range statuses {
		proofs := [][]string{{messages[trigger].ID}}
		if status != domain.AgreementPending {
			proofs = proofs[:0]
			for _, message := range later {
				proofs = append(proofs, []string{messages[trigger].ID, message.ID})
			}
		}
		for _, proof := range proofs {
			observation := domain.AgreementObservation{Kind: domain.AgreementCommitment, WaitingFor: domain.AgreementBusiness, Status: status,
				TriggerMessageID: messages[trigger].ID, EvidenceMessageIDs: proof, Confidence: confidence}
			check := domain.AnalysisResultV2{AnalysisThroughMessageID: request.AnalysisThroughMessageID, Agreements: []domain.AgreementObservation{observation}}
			if application.ValidateAgreementEvidence(check, prompt) == nil {
				return observation, true
			}
		}
	}
	return domain.AgreementObservation{}, false
}

// latestPromiseFact согласует факт BUSINESS_COMMITMENT с договорённостями: его основание — последнее сообщение, по которому есть
// сильная договорённость COMMITMENT от компании (так риск и отчёт видят последнее обещание). Явное решение модели «нет» не
// переопределяется, а более позднее основание факта сохраняется.
func latestPromiseFact(request application.AnalyzeConversationRequestV1, result domain.AnalysisResultV2) []domain.SemanticFact {
	position := make(map[string]int, len(request.Messages))
	for i, m := range request.Messages {
		position[m.ID] = i
	}
	latest, confidence := -1, 0.0
	for _, a := range result.Agreements {
		if a.Kind != domain.AgreementCommitment || a.WaitingFor != domain.AgreementBusiness || a.Confidence < promiseConfidenceMin {
			continue
		}
		if i, ok := position[a.TriggerMessageID]; ok && i > latest {
			latest, confidence = i, a.Confidence
		}
	}
	facts := make([]domain.SemanticFact, 0, len(result.Facts)+1)
	present := false
	for _, fact := range result.Facts {
		if fact.Type != domain.FactBusinessCommitment {
			facts = append(facts, fact)
			continue
		}
		present = true
		if !fact.Value || latest < 0 {
			facts = append(facts, fact)
			continue
		}
		evidence := -1
		for _, id := range fact.EvidenceMessageIDs {
			if i, ok := position[id]; ok && i > evidence {
				evidence = i
			}
		}
		if evidence < latest {
			fact.EvidenceMessageIDs, fact.Confidence = []string{request.Messages[latest].ID}, math.Max(fact.Confidence, confidence)
		}
		facts = append(facts, fact)
	}
	if !present && latest >= 0 {
		facts = append(facts, domain.SemanticFact{Type: domain.FactBusinessCommitment, Value: true, Confidence: confidence, EvidenceMessageIDs: []string{request.Messages[latest].ID}})
	}
	return facts
}
