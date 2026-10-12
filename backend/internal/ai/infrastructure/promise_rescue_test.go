package infrastructure

import (
	"context"
	"strings"
	"testing"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

func rescueAnswer(through, facts string, agreements ...string) string {
	return `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"` + through + `","summary":"Клиент просит подробности.","facts":[` + facts + `],"agreements":[` + strings.Join(agreements, ",") + `]}`
}

func commitmentAgreement(trigger, status, confidence string, evidence ...string) string {
	if len(evidence) == 0 {
		evidence = []string{trigger}
	}
	return `{"kind":"COMMITMENT","waitingFor":"BUSINESS","status":"` + status + `","triggerMessageId":"` + trigger + `","evidenceMessageIds":["` + strings.Join(evidence, `","`) + `"],"confidence":` + confidence + `}`
}

func commitmentFact(value, confidence string, evidence ...string) string {
	return `{"type":"BUSINESS_COMMITMENT","value":` + value + `,"confidence":` + confidence + `,"evidenceMessageIds":["` + strings.Join(evidence, `","`) + `"]}`
}

func agreementsOf(t *testing.T, answer string) []domain.AgreementObservation {
	t.Helper()
	result, err := application.ValidateAnalysisResultV2(answer, "")
	if err != nil {
		t.Fatalf("the answer must stay valid: %v\n%s", err, answer)
	}
	return result.Agreements
}

func TestRescueAddsTheAgreementAndTheFactForAnAdmittedPromiseTheMainCallMissed(t *testing.T) {
	prompt := companyThread(admittedPromise...)
	answer := rescueAnswer(canonicalCompany, "")
	got := rescueCommitments(prompt, answer, map[string]float64{canonicalCompany: .96})
	agreements, facts := agreementsOf(t, got), commitmentFacts(t, got)
	if len(agreements) != 1 || agreements[0].Kind != domain.AgreementCommitment || agreements[0].WaitingFor != domain.AgreementBusiness || agreements[0].Status != domain.AgreementPending ||
		agreements[0].TriggerMessageID != canonicalCompany || agreements[0].Confidence != .96 {
		t.Fatalf("agreements: %+v", agreements)
	}
	if len(facts) != 1 || !facts[0].Value || facts[0].EvidenceMessageIDs[0] != canonicalCompany || facts[0].Confidence != .96 {
		t.Fatalf("facts: %+v", facts)
	}
	result, err := application.ValidateAnalysisResultV2(got, canonicalCompany)
	if err != nil || application.ValidateAgreementEvidence(result, prompt) != nil {
		t.Fatalf("the rescued answer must pass the server check: %v", err)
	}
}

func TestRescueNeedsTheThresholdAndKeepsItsBoundary(t *testing.T) {
	prompt := companyThread(admittedPromise...)
	answer := rescueAnswer(canonicalCompany, "")
	if got := rescueCommitments(prompt, answer, map[string]float64{canonicalCompany: promiseRescueThreshold - 1e-9}); got != answer {
		t.Fatal("below the threshold nothing is added")
	}
	if got := rescueCommitments(prompt, answer, map[string]float64{canonicalCompany: promiseRescueThreshold}); got == answer {
		t.Fatal("the threshold itself is enough")
	}
	for _, probability := range []float64{.9, .5, .9999} {
		got := rescueCommitments(prompt, answer, map[string]float64{canonicalCompany: probability})
		if probability < promiseRescueThreshold {
			continue
		}
		want := roundCents(clampTo(probability, promiseConfidenceMin, promiseConfidenceMax))
		if agreements := agreementsOf(t, got); len(agreements) != 1 || agreements[0].Confidence != want {
			t.Errorf("p=%v: confidence %v, want %v", probability, agreements, want)
		}
	}
	if rescueCommitments(prompt, answer, nil) != answer || rescueCommitments(prompt, answer, map[string]float64{}) != answer {
		t.Fatal("without the scan nothing is added")
	}
}

func clampTo(v, low, high float64) float64 {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}

func roundCents(v float64) float64 { return float64(int(v*100+.5)) / 100 }

func TestRescueNeverTouchesWhatTheMainCallAlreadyDecided(t *testing.T) {
	prompt := companyThread(admittedPromise...)
	for name, agreement := range map[string]string{
		"pending strong":    commitmentAgreement(canonicalCompany, "PENDING", "0.95"),
		"pending weak":      commitmentAgreement(canonicalCompany, "PENDING", "0.5"),
		"cancelled by hand": commitmentAgreement(canonicalCompany, "CANCELLED", "0.95", canonicalCompany),
	} {
		answer := rescueAnswer(canonicalCompany, "", agreement)
		if got := rescueCommitments(prompt, answer, map[string]float64{canonicalCompany: .99}); got != answer {
			t.Errorf("%s: an agreement for this trigger exists, the same expectation is not repeated", name)
		}
	}
}

func TestRescueIsLimitedToMessagesTheStructureAllows(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"an offer", "Хотите, пришлю прайс?"},
		{"a question", "Прислать вам прайс?"},
		{"a hedge", "Возможно, ответим завтра."},
		{"a refusal", "Не пришлю."},
	} {
		messages := []application.ContextMessage{admittedPromise[0], {ID: canonicalCompany, Direction: "OUTGOING", Body: tc.body}}
		answer := rescueAnswer(canonicalCompany, "")
		if got := rescueCommitments(companyThread(messages...), answer, map[string]float64{canonicalCompany: .99}); got != answer {
			t.Errorf("%s: %q must not be rescued", tc.name, tc.body)
		}
	}
	// слово обещания у клиента ничего не значит
	customer := []application.ContextMessage{{ID: canonicalClient, Direction: "INCOMING", Body: "Я позвоню завтра."}, {ID: canonicalCompany, Direction: "OUTGOING", Body: "Хорошо."}}
	answer := rescueAnswer(canonicalCompany, "")
	if got := rescueCommitments(companyThread(customer...), answer, map[string]float64{canonicalClient: .99, canonicalCompany: .5}); got != answer {
		t.Fatal("only the company promises")
	}
}

func TestRescueOfAMessageWithoutALexiconWordNeedsTheHigherThreshold(t *testing.T) {
	prompt := companyThread(openPromise...)
	answer := rescueAnswer(canonicalCompany, "")
	for _, tc := range []struct {
		probability float64
		rescued     bool
	}{{promiseRescueOpenThreshold, true}, {.999, true}, {promiseRescueOpenThreshold - 1e-9, false}, {.95, false}, {promiseRescueThreshold, false}} {
		got := rescueCommitments(prompt, answer, map[string]float64{canonicalCompany: tc.probability})
		if (got != answer) != tc.rescued {
			t.Errorf("p=%v: rescued=%v, want %v", tc.probability, got != answer, tc.rescued)
		}
		if tc.rescued {
			agreements := agreementsOf(t, got)
			if len(agreements) != 1 || agreements[0].TriggerMessageID != canonicalCompany || agreements[0].Status != domain.AgreementPending {
				t.Errorf("p=%v: agreements %+v", tc.probability, agreements)
			}
			if facts := commitmentFacts(t, got); len(facts) != 1 || !facts[0].Value || facts[0].EvidenceMessageIDs[0] != canonicalCompany {
				t.Errorf("p=%v: facts %+v", tc.probability, facts)
			}
		}
	}
	// То, что структура исключает, не спасается при любой вероятности.
	for _, body := range []string{"Хотите, пришлю прайс?", "Могу уточнить у мастера.", "Возможно, всё получится.", "Прислать вам прайс?"} {
		messages := []application.ContextMessage{openPromise[0], {ID: canonicalCompany, Direction: "OUTGOING", Body: body}}
		if got := rescueCommitments(companyThread(messages...), answer, map[string]float64{canonicalCompany: 1}); got != answer {
			t.Errorf("%q is ruled out by the structure and is not rescued", body)
		}
	}
	// Сообщение клиента не спасается, даже если проход его об этом спросил бы.
	customer := []application.ContextMessage{{ID: canonicalClient, Direction: "INCOMING", Body: "Будет сделано."}, {ID: canonicalCompany, Direction: "OUTGOING", Body: "Хорошо."}}
	if got := rescueCommitments(companyThread(customer...), answer, map[string]float64{canonicalClient: 1}); got != answer {
		t.Error("only the company promises")
	}
}

func TestWordsOfACompletedActionNoLongerBlockTheRescueOfARealPromise(t *testing.T) {
	// «Уже отправил, пришлю ещё акт завтра»: в сообщении есть и выполненное, и обещание. Решает проход, а не подстрока.
	for _, body := range []string{"Уже отправил, пришлю ещё акт завтра.", "Проверил наличие, завтра отправлю.", "Заявку передал инженерам, с вами свяжется техник."} {
		messages := []application.ContextMessage{admittedPromise[0], {ID: canonicalCompany, Direction: "OUTGOING", Body: body}}
		answer := rescueAnswer(canonicalCompany, "")
		if got := rescueCommitments(companyThread(messages...), answer, map[string]float64{canonicalCompany: .99}); got == answer {
			t.Errorf("%q: the scan says promise, nothing else blocks the rescue", body)
		}
		if got := rescueCommitments(companyThread(messages...), answer, map[string]float64{canonicalCompany: .2}); got != answer {
			t.Errorf("%q: the scan says it is not a promise, so nothing is rescued", body)
		}
	}
}

func TestRescueMovesTheFactToTheLatestPromiseAndKeepsAnExplicitNo(t *testing.T) {
	messages := []application.ContextMessage{
		admittedPromise[0],
		{ID: canonicalCompany, Direction: "OUTGOING", Body: "Пришлю счёт."},
		{ID: canonicalLater, Direction: "INCOMING", Body: "И ещё нужен акт."},
		{ID: canonicalLast, Direction: "OUTGOING", Body: "Акт подготовлю завтра."},
	}
	prompt := companyThread(messages...)
	// основной вызов записал первое обещание, второе пропустил: факт переезжает на последнее
	earlier := rescueAnswer(canonicalLast, commitmentFact("true", "0.9", canonicalCompany), commitmentAgreement(canonicalCompany, "PENDING", "0.9"))
	got := rescueCommitments(prompt, earlier, map[string]float64{canonicalCompany: .1, canonicalLast: .97})
	facts, agreements := commitmentFacts(t, got), agreementsOf(t, got)
	if len(agreements) != 2 || len(facts) != 1 || facts[0].EvidenceMessageIDs[0] != canonicalLast || facts[0].Confidence != .97 {
		t.Fatalf("the fact must point at the latest promise: %+v %+v", facts, agreements)
	}
	// основной вызов записал последнее, пропущено первое: основание факта остаётся последним
	later := rescueAnswer(canonicalLast, commitmentFact("true", "0.9", canonicalLast), commitmentAgreement(canonicalLast, "PENDING", "0.9"))
	got = rescueCommitments(prompt, later, map[string]float64{canonicalCompany: .97, canonicalLast: .1})
	facts, agreements = commitmentFacts(t, got), agreementsOf(t, got)
	if len(agreements) != 2 || len(facts) != 1 || facts[0].EvidenceMessageIDs[0] != canonicalLast || facts[0].Confidence != .9 {
		t.Fatalf("an earlier rescue must not move the fact back: %+v %+v", facts, agreements)
	}
	// явное решение модели «нет» сохраняется
	explicit := rescueAnswer(canonicalLast, commitmentFact("false", "0.9", canonicalCompany))
	got = rescueCommitments(prompt, explicit, map[string]float64{canonicalCompany: .99})
	facts = commitmentFacts(t, got)
	if len(facts) != 1 || facts[0].Value || len(agreementsOf(t, got)) != 1 {
		t.Fatalf("an explicit «no» is kept while the agreement is still recorded: %+v", facts)
	}
}

func TestRescueRecordsAPromiseThatIsAlreadyFulfilledAsResolved(t *testing.T) {
	messages := []application.ContextMessage{
		admittedPromise[0],
		{ID: canonicalCompany, Direction: "OUTGOING", Body: "Пришлю счёт завтра."},
		{ID: canonicalLast, Direction: "OUTGOING", Body: "Счёт отправил."},
	}
	prompt := companyThread(messages...)
	got := rescueCommitments(prompt, rescueAnswer(canonicalLast, ""), map[string]float64{canonicalCompany: .95})
	agreements := agreementsOf(t, got)
	if len(agreements) != 1 || agreements[0].Status != domain.AgreementResolved || strings.Join(agreements[0].EvidenceMessageIDs, ",") != canonicalCompany+","+canonicalLast {
		t.Fatalf("a fulfilled promise is resolved with its proof: %+v", agreements)
	}
	if facts := commitmentFacts(t, got); len(facts) != 1 || facts[0].EvidenceMessageIDs[0] != canonicalCompany {
		t.Fatalf("the fact stays as history: %+v", facts)
	}
}

func TestRescueNeverMakesTheAnswerWorse(t *testing.T) {
	prompt := companyThread(admittedPromise...)
	probabilities := map[string]float64{canonicalCompany: .99}
	for name, answer := range map[string]string{
		"not json":          "not json",
		"wrong boundary":    rescueAnswer("other", ""),
		"invalid agreement": rescueAnswer(canonicalCompany, "", commitmentAgreement(canonicalCompany, "RESOLVED", "0.9")),
	} {
		if got := rescueCommitments(prompt, answer, probabilities); got != answer {
			t.Errorf("%s: the answer must stay as it was", name)
		}
	}
	if got := rescueCommitments("not json", rescueAnswer(canonicalCompany, ""), probabilities); got != rescueAnswer(canonicalCompany, "") {
		t.Error("an unreadable request leaves the answer alone")
	}
	other := companyThread(admittedPromise[1])
	if got := rescueCommitments(other, rescueAnswer(canonicalCompany, ""), probabilities); got == rescueAnswer(canonicalCompany, "") {
		t.Log("a conversation made of the promise alone is rescued, as expected")
	}
	unknownBoundary := strings.Replace(prompt, `"analysisThroughMessageId":"`+canonicalCompany+`"`, `"analysisThroughMessageId":"nowhere"`, 1)
	if got := rescueCommitments(unknownBoundary, rescueAnswer("nowhere", ""), probabilities); got != rescueAnswer("nowhere", "") {
		t.Error("a boundary outside the context leaves the answer alone")
	}
}

func TestCanonicalProbabilitiesFollowTheAliases(t *testing.T) {
	aliases := analysisAliases{canonical: map[string]string{"m1": canonicalClient, "m2": canonicalCompany}}
	got := aliases.canonicalProbabilities(map[string]float64{"m2": .9, "m9": .8})
	if len(got) != 1 || got[canonicalCompany] != .9 || aliases.canonicalProbabilities(nil) != nil {
		t.Fatalf("probabilities: %v", got)
	}
}

func TestInferRescuesAPromiseTheMainCallLeftOut(t *testing.T) {
	missed := `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","summary":"Клиент ждёт звонка.","facts":[],"agreements":[]}`
	for _, tc := range []struct {
		probability float64
		rescued     bool
	}{{.97, true}, {.6, false}} {
		provider, scans, mains := inferServer(t,
			func(body scanRequestBody) (int, string) {
				return 200, scanReply(body.asked(), map[string]float64{"m2": tc.probability})
			},
			func([]byte) string { return mainReply(missed) })
		prompt := companyThread(admittedPromise...)
		raw, err := provider.Infer(context.Background(), prompt)
		if err != nil {
			t.Fatal(err)
		}
		agreements := agreementsOf(t, raw)
		if *scans != 1 || *mains != 1 || (len(agreements) == 1) != tc.rescued {
			t.Fatalf("p=%v: scans %d mains %d agreements %+v", tc.probability, *scans, *mains, agreements)
		}
		if tc.rescued && (agreements[0].TriggerMessageID != canonicalCompany || len(commitmentFacts(t, raw)) != 1) {
			t.Fatalf("canonical identifiers and the fact must be set: %+v\n%s", agreements, raw)
		}
	}
}

func TestRescueAddsOnlyWhatIsMissingWhenSeveralPromisesAreAdmitted(t *testing.T) {
	messages := []application.ContextMessage{
		admittedPromise[0],
		{ID: canonicalCompany, Direction: "OUTGOING", Body: "Пришлю счёт."},
		{ID: canonicalLater, Direction: "INCOMING", Body: "И ещё нужен акт."},
		{ID: canonicalLast, Direction: "OUTGOING", Body: "Акт подготовлю завтра."},
	}
	prompt := companyThread(messages...)
	// второе обещание модель записала; оба сообщения проход считает обещаниями — дубликат второго недопустим и не должен губить первое
	answer := rescueAnswer(canonicalLast, "", commitmentAgreement(canonicalLast, "PENDING", "0.9"))
	got := rescueCommitments(prompt, answer, map[string]float64{canonicalCompany: .97, canonicalLast: .98})
	agreements := agreementsOf(t, got)
	triggers := []string{}
	for _, a := range agreements {
		triggers = append(triggers, a.TriggerMessageID)
	}
	if len(agreements) != 2 || strings.Join(triggers, ",") != canonicalLast+","+canonicalCompany {
		t.Fatalf("the missing promise is added next to the recorded one: %v", triggers)
	}
}

func TestRescueKeepsAnExplicitNoWithItsOwnEvidence(t *testing.T) {
	messages := []application.ContextMessage{
		admittedPromise[0],
		{ID: canonicalCompany, Direction: "OUTGOING", Body: "Возможно, ответим завтра."},
		{ID: canonicalLater, Direction: "INCOMING", Body: "Жду."},
		{ID: canonicalLast, Direction: "OUTGOING", Body: "Акт подготовлю завтра."},
	}
	explicit := rescueAnswer(canonicalLast, commitmentFact("false", "0.9", canonicalCompany))
	got := rescueCommitments(companyThread(messages...), explicit, map[string]float64{canonicalLast: .99})
	facts := commitmentFacts(t, got)
	if len(facts) != 1 || facts[0].Value || facts[0].EvidenceMessageIDs[0] != canonicalCompany || facts[0].Confidence != .9 {
		t.Fatalf("an explicit «no» is not rewritten: %+v", facts)
	}
	if agreements := agreementsOf(t, got); len(agreements) != 1 || agreements[0].TriggerMessageID != canonicalLast {
		t.Fatalf("the promise itself is still recorded: %+v", agreements)
	}
}

func TestRescueKeepsTheConfidenceOfAFactThatAlreadyPointsAtThePromise(t *testing.T) {
	prompt := companyThread(admittedPromise...)
	factOnly := rescueAnswer(canonicalCompany, commitmentFact("true", "0.9", canonicalCompany))
	got := rescueCommitments(prompt, factOnly, map[string]float64{canonicalCompany: .99})
	facts := commitmentFacts(t, got)
	if len(facts) != 1 || facts[0].Confidence != .9 || facts[0].EvidenceMessageIDs[0] != canonicalCompany || len(agreementsOf(t, got)) != 1 {
		t.Fatalf("only the missing agreement is added: %+v", facts)
	}
}

func TestRescueIgnoresMessagesAfterTheAnalysisBoundary(t *testing.T) {
	messages := []application.ContextMessage{
		admittedPromise[0],
		{ID: canonicalCompany, Direction: "OUTGOING", Body: "Подумаем."},
		{ID: canonicalLast, Direction: "OUTGOING", Body: "Позвоню завтра."},
	}
	prompt, _ := application.EncodeAnalysisRequest(application.AnalyzeConversationRequestV1{
		Task: "ANALYZE_CONVERSATION", SchemaVersion: application.AnalysisSchemaV2, PromptVersion: application.AnalysisPromptV9,
		ConversationID: "c1", BaseConversationRevision: 1, AnalysisThroughMessageID: canonicalCompany, Messages: messages,
	})
	answer := rescueAnswer(canonicalCompany, "")
	if got := rescueCommitments(prompt, answer, map[string]float64{canonicalLast: .99}); got != answer {
		t.Fatal("a message after the boundary is not part of this analysis")
	}
}

func TestAWeakAgreementDoesNotDecideWhereTheFactPoints(t *testing.T) {
	messages := []application.ContextMessage{
		admittedPromise[0],
		{ID: canonicalCompany, Direction: "OUTGOING", Body: "Пришлю счёт."},
		{ID: canonicalLater, Direction: "INCOMING", Body: "И ещё нужен акт."},
		{ID: canonicalLast, Direction: "OUTGOING", Body: "Акт подготовлю завтра."},
	}
	weakLater := rescueAnswer(canonicalLast, "", commitmentAgreement(canonicalLast, "PENDING", "0.5"))
	got := rescueCommitments(companyThread(messages...), weakLater, map[string]float64{canonicalCompany: .97})
	facts := commitmentFacts(t, got)
	if len(facts) != 1 || facts[0].EvidenceMessageIDs[0] != canonicalCompany {
		t.Fatalf("only a strong agreement can be the evidence of the fact: %+v", facts)
	}
}

func TestRescueDoesNotTurnAnOfferInAQuestionIntoAPromise(t *testing.T) {
	messages := []application.ContextMessage{
		admittedPromise[0],
		{ID: canonicalCompany, Direction: "OUTGOING", Body: "Подскажите, какая у вас цель? И будет ли удобно, если тренер перезвонит вам для консультации?"},
	}
	answer := rescueAnswer(canonicalCompany, "")
	if got := rescueCommitments(companyThread(messages...), answer, map[string]float64{canonicalCompany: 1}); got != answer {
		t.Fatal("a promise word in a question is an offer: only the main call may decide")
	}
}
