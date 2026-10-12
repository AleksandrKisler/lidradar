package infrastructure

import (
	"context"
	"strings"
	"testing"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

func TestDedupeAgreementsKeepsTheFirstOfTwoWithTheSameKindAndTrigger(t *testing.T) {
	reschedule := `{"kind":"RESCHEDULE","waitingFor":"CUSTOMER","status":"PENDING","triggerMessageId":"m2","evidenceMessageIds":["m2"],"confidence":0.99}`
	commit := `{"kind":"COMMITMENT","waitingFor":"BUSINESS","status":"PENDING","triggerMessageId":"m2","evidenceMessageIds":["m2"],"confidence":0.99}`
	other := strings.Replace(commit, `"m2"`, `"m3"`, 2)
	raw := `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m3","summary":"Резюме.","facts":[],"agreements":[` + reschedule + `,` + reschedule + `,` + commit + `,` + other + `,` + strings.Replace(commit, "0.99", "0.5", 1) + `]}`
	if _, err := application.ValidateAnalysisResultV2(raw, "m3"); err == nil {
		t.Fatal("the duplicate must be what the server check refuses")
	}
	repaired := dedupeAgreements(raw)
	result, err := application.ValidateAnalysisResultV2(repaired, "m3")
	if err != nil || len(result.Agreements) != 3 {
		t.Fatalf("the repaired answer keeps one agreement per kind and trigger: %v %+v", err, result.Agreements)
	}
	if result.Agreements[2].TriggerMessageID != "m3" || result.Agreements[1].Confidence != .99 {
		t.Fatalf("the first of the duplicates stays, order is kept: %+v", result.Agreements)
	}
	for _, untouched := range []string{
		`{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m3","summary":"Резюме.","facts":[],"agreements":[` + reschedule + `,` + commit + `]}`,
		`{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m3","summary":"Резюме.","facts":[],"agreements":[]}`,
		`{"summary":"без договорённостей"}`, `not json`, `{"agreements":"x"}`,
	} {
		if dedupeAgreements(untouched) != untouched {
			t.Errorf("nothing to repair: %s", untouched)
		}
	}
}

func TestDropUnsupportedPricesRemovesOnlyTheFactTheMessageDoesNotSupport(t *testing.T) {
	prompt := companyThread(
		application.ContextMessage{ID: canonicalClient, Direction: "INCOMING", Body: "Нужно ещё 200 штук таких же."},
		application.ContextMessage{ID: canonicalCompany, Direction: "OUTGOING", Body: "Выставим счёт сегодня, тираж за три дня."})
	priced := func(amount, evidence string) string {
		return `{"type":"PRICE_MENTIONED","value":true,"confidence":0.99,"evidenceMessageIds":["` + evidence + `"],"amount":"` + amount + `","currency":"RUB"}`
	}
	promise := commitmentFact("true", "0.99", canonicalCompany)
	answer := rescueAnswer(canonicalCompany, promise+","+priced("200", canonicalCompany), commitmentAgreement(canonicalCompany, "PENDING", "0.99"))
	result, err := application.ValidateAnalysisResultV2(answer, canonicalCompany)
	if err != nil || application.ValidatePriceEvidence(domain.AnalysisResultV1{AnalysisThroughMessageID: result.AnalysisThroughMessageID, Facts: result.Facts}, prompt) == nil {
		t.Fatal("the invented price must be what the server check refuses")
	}
	repaired := dropUnsupportedPrices(prompt, answer)
	got, err := application.ValidateAnalysisResultV2(repaired, canonicalCompany)
	if err != nil || len(got.Facts) != 1 || got.Facts[0].Type != domain.FactBusinessCommitment || len(got.Agreements) != 1 {
		t.Fatalf("only the unsupported price goes: %v %+v", err, got)
	}
	if application.ValidatePriceEvidence(domain.AnalysisResultV1{AnalysisThroughMessageID: got.AnalysisThroughMessageID, Facts: got.Facts}, prompt) != nil {
		t.Fatal("the repaired answer passes the price check")
	}
	// цена, которая есть в цитируемом сообщении, и отрицательный факт о цене остаются
	supported := application.ContextMessage{ID: canonicalCompany, Direction: "OUTGOING", Body: "Тираж стоит 4500 рублей."}
	pricePrompt := companyThread(admittedPromise[0], supported)
	keepSupported := rescueAnswer(canonicalCompany, " "+priced("4500", canonicalCompany))
	keepNegative := rescueAnswer(canonicalCompany, ` {"type":"PRICE_MENTIONED","value":false,"confidence":0.9,"evidenceMessageIds":["`+canonicalClient+`"]}`)
	for _, keep := range []string{keepSupported, keepNegative} {
		if got := dropUnsupportedPrices(pricePrompt, keep); got != keep {
			t.Fatalf("a supported price and a negative fact stay, the answer is returned as it was: %s", got)
		}
	}
	// неразбираемое остаётся как есть
	for _, untouched := range []string{"not json", rescueAnswer("other", "")} {
		if dropUnsupportedPrices(prompt, untouched) != untouched {
			t.Errorf("nothing to repair: %s", untouched)
		}
	}
	if dropUnsupportedPrices("not json", answer) != answer {
		t.Error("an unreadable request leaves the answer alone")
	}
}

func TestACommitmentFactIsCompletedWhenOnlyAnotherPartOfTheMessageSaysNo(t *testing.T) {
	messages := []application.ContextMessage{
		{ID: canonicalClient, Direction: "INCOMING", Body: "Есть платье 44 размера?"},
		{ID: canonicalCompany, Direction: "OUTGOING", Body: "На сайте 44-го нет, но на складе мог остаться один. Сейчас позвоню туда и уточню."},
	}
	prompt := companyThread(messages...)
	answer := rescueAnswer(canonicalCompany, "", commitmentAgreement(canonicalCompany, "PENDING", "0.95"))
	if facts := commitmentFacts(t, completeCommitmentFact(prompt, answer, nil)); len(facts) != 1 || facts[0].EvidenceMessageIDs[0] != canonicalCompany {
		t.Fatalf("a negation elsewhere in the message must not cancel the promise: %+v", facts)
	}
	// настоящий отказ по-прежнему не достраивается
	refusal := []application.ContextMessage{messages[0], {ID: canonicalCompany, Direction: "OUTGOING", Body: "Не обещаю, что получится быстро, но позвоню."}}
	if facts := commitmentFacts(t, completeCommitmentFact(companyThread(refusal...), answer, nil)); len(facts) != 1 {
		t.Fatalf("a promise word that no negation touches stays a promise: %+v", facts)
	}
	noPromise := []application.ContextMessage{messages[0], {ID: canonicalCompany, Direction: "OUTGOING", Body: "Ничего не обещаем, звонить не будем."}}
	if facts := commitmentFacts(t, completeCommitmentFact(companyThread(noPromise...), answer, scanEligibility{canonicalCompany: true})); len(facts) != 0 {
		t.Fatalf("a refusal is not completed into a fact: %+v", facts)
	}
}

func TestInferRepairsAnAnswerThatTheServerCheckWouldRefuseAsAWhole(t *testing.T) {
	messages := []application.ContextMessage{
		{ID: canonicalClient, Direction: "INCOMING", Body: "Нужно ещё 200 штук таких же."},
		{ID: canonicalCompany, Direction: "OUTGOING", Body: "Выставим счёт сегодня, после оплаты отпечатаем."},
	}
	prompt := companyThread(messages...)
	duplicated := `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","summary":"Клиент просит тираж.","facts":[` + commitmentFact("true", "0.99", "m2") + `],"agreements":[` +
		commitmentAgreement("m2", "PENDING", "0.99") + `,` + commitmentAgreement("m2", "PENDING", "0.99") + `]}`
	invented := `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","summary":"Клиент просит тираж.","facts":[` + commitmentFact("true", "0.99", "m2") +
		`,{"type":"PRICE_MENTIONED","value":true,"confidence":0.99,"evidenceMessageIds":["m2"],"amount":"200","currency":"RUB"}],"agreements":[` + commitmentAgreement("m2", "PENDING", "0.99") + `]}`
	for name, reply := range map[string]string{"duplicate agreement": duplicated, "price from another message": invented} {
		provider, _, _ := inferServer(t,
			func(body scanRequestBody) (int, string) {
				return 200, scanReply(body.asked(), map[string]float64{"m2": .1})
			},
			func([]byte) string { return mainReply(reply) })
		raw, err := provider.Infer(context.Background(), prompt)
		if err != nil {
			t.Fatalf("%s: the answer must survive: %v", name, err)
		}
		result, err := application.ValidateAnalysisResultV2(raw, canonicalCompany)
		if err != nil || application.ValidateAgreementEvidence(result, prompt) != nil || application.ValidatePriceEvidence(domain.AnalysisResultV1{AnalysisThroughMessageID: result.AnalysisThroughMessageID, Facts: result.Facts}, prompt) != nil {
			t.Fatalf("%s: the repaired answer passes the whole server check: %v\n%s", name, err, raw)
		}
		if facts := commitmentFacts(t, raw); len(facts) != 1 || len(result.Agreements) != 1 || result.Agreements[0].TriggerMessageID != canonicalCompany {
			t.Fatalf("%s: the promise must survive the repair: %+v %+v", name, facts, result.Agreements)
		}
	}
}

func TestANegationElsewhereDoesNotBlockAPromiseTheScanAndTheModelBothAccepted(t *testing.T) {
	messages := []application.ContextMessage{
		{ID: canonicalClient, Direction: "INCOMING", Body: "Когда будут анализы?"},
		{ID: canonicalCompany, Direction: "OUTGOING", Body: "Результаты ещё не пришли из лаборатории. Как придут, врач посмотрит их и наберёт вас с рекомендациями."},
	}
	prompt := companyThread(messages...)
	answer := rescueAnswer(canonicalCompany, "", commitmentAgreement(canonicalCompany, "PENDING", "0.95"))
	// без допуска прохода слов обещания в сообщении нет: общий поиск отрицания остаётся
	if got := completeCommitmentFact(prompt, answer, nil); got != answer {
		t.Fatal("the lexicon does not admit this message")
	}
	if facts := commitmentFacts(t, completeCommitmentFact(prompt, answer, scanEligibility{canonicalCompany: true})); len(facts) != 1 {
		t.Fatalf("a message that the scan and the model both accepted keeps its promise despite «не пришли»: %+v", facts)
	}
	// отказ, который структура исключает, допуска не получает
	refusal := []application.ContextMessage{messages[0], {ID: canonicalCompany, Direction: "OUTGOING", Body: "Ничего не обещаем, перезванивать не будем."}}
	if got := completeCommitmentFact(companyThread(refusal...), answer, scanEligibility{canonicalCompany: true}); got != answer {
		t.Fatal("a refusal is not completed into a fact even if flagged")
	}
}
