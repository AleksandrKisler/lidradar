package benchmark

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
	"lidradar/backend/internal/ai/infrastructure"
)

func TestAgreementLabelsCannotPassOnFactsAlone(t *testing.T) {
	c := Case{Version: DatasetVersion, ID: "agreement-test", Split: SplitDev,
		Input:              application.AnalyzeConversationRequestV1{Task: "ANALYZE_CONVERSATION", SchemaVersion: application.AnalysisSchemaV2, PromptVersion: application.AnalysisPromptV8, ConversationID: "test", BaseConversationRevision: 1, AnalysisThroughMessageID: "m", Messages: []application.ContextMessage{{ID: "m", Direction: "INCOMING", Body: "Хочу записаться"}}},
		Expected:           []domain.SemanticFact{{Type: domain.FactBookingIntent, Value: true, Confidence: 1, EvidenceMessageIDs: []string{"m"}}},
		ExpectedAgreements: []domain.AgreementObservation{{Kind: domain.AgreementBookingConfirmation, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementPending, TriggerMessageID: "m", EvidenceMessageIDs: []string{"m"}, Confidence: 1}},
	}
	encoded, _ := json.Marshal(c)
	cases, digest, err := Load(strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		agreement  bool
		confidence float64
		passed     bool
	}{
		{"missing", false, 0, false}, {"weak", true, .7, false}, {"strong", true, .96, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := domain.AnalysisResultV2{SchemaVersion: application.AnalysisSchemaV2, AnalysisThroughMessageID: "m", Summary: "Запрос записи.", Facts: c.Expected, Agreements: []domain.AgreementObservation{}}
			if tc.agreement {
				a := c.ExpectedAgreements[0]
				a.Confidence = tc.confidence
				result.Agreements = append(result.Agreements, a)
			}
			raw, _ := json.Marshal(result)
			r, err := Run(context.Background(), infrastructure.FakeProvider{Output: string(raw)}, cases, digest, Thresholds{MinimumExactRate: 1, MinimumValidRate: 1})
			if err != nil || r.Passed != tc.passed || r.AgreementCases != 1 {
				t.Fatalf("report: %+v, error: %v", r, err)
			}
		})
	}
}

func TestSyntheticAgreementDatasetHasValidIndependentLabels(t *testing.T) {
	f, err := os.Open("../../../../models/datasets/agreements_dev_v2.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cases, _, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 20 {
		t.Fatal("missing agreement lifecycle cases")
	}
	for _, c := range cases {
		if c.Split != SplitDev || c.ExpectedAgreements == nil {
			t.Fatalf("unlabelled or non-development case: %s", c.ID)
		}
	}
	if _, err := AuditCases(cases); err != nil {
		t.Fatal(err)
	}
}

const dataset = `{"version":"lidradar-ai-benchmark.v1","id":"booking-001","split":"GOLDEN","input":{"task":"ANALYZE_CONVERSATION","schemaVersion":"analyze-conversation.v1","promptVersion":"analyze-conversation.prompt.v5","conversationId":"conversation-1","baseConversationRevision":1,"analysisThroughMessageId":"message-1","companyContext":"Детейлинг","messages":[{"id":"message-1","direction":"INCOMING","body":"Можно завтра?"}]},"expectedFacts":[{"type":"BOOKING_INTENT","value":true,"confidence":1,"evidenceMessageIds":["message-1"]}]}
`

func TestLoadRunAndGoldenProtection(t *testing.T) {
	cases, digest, err := Load(strings.NewReader(dataset))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyGolden(digest, digest); err != nil {
		t.Fatal(err)
	}
	if err := VerifyGolden(digest, "bad"); err == nil {
		t.Fatal("expected checksum mismatch")
	}
	provider := infrastructure.FakeProvider{Output: `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"message-1","summary":"Есть намерение записаться.","facts":[{"type":"BOOKING_INTENT","value":true,"confidence":0.95,"evidenceMessageIds":["message-1"]}]}`}
	report, err := Run(context.Background(), provider, cases, digest, Thresholds{MinimumPrecision: .9, MinimumRecall: .9, MinimumF1: .9, MinimumExactRate: .9})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.Exact != 1 || report.F1 != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if len(report.PromptVersions) != 1 || report.PromptVersions[0] != cases[0].Input.PromptVersion {
		t.Fatal("report must record evaluated prompt version")
	}
}

func TestV2DatasetParserAndProductionValidation(t *testing.T) {
	v2 := strings.Replace(dataset, `"schemaVersion":"analyze-conversation.v1"`, `"schemaVersion":"analyze-conversation.v2"`, 1)
	v2 = strings.Replace(v2, `"promptVersion":"analyze-conversation.prompt.v5"`, `"promptVersion":"analyze-conversation.prompt.v7"`, 1)
	v2 = strings.Replace(v2, `"id":"booking-001"`, `"id":"purchase-v2"`, 1)
	v2 = strings.Replace(v2, `"Можно завтра?"`, `"Хочу купить, оформите заказ."`, 1)
	v2 = strings.Replace(v2, `"type":"BOOKING_INTENT"`, `"type":"PURCHASE_INTENT"`, 1)
	cases, digest, err := Load(strings.NewReader(v2))
	if err != nil {
		t.Fatal(err)
	}
	output := `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"message-1","summary":"Клиент оформляет заказ.","facts":[{"type":"PURCHASE_INTENT","value":true,"confidence":0.95,"evidenceMessageIds":["message-1"]}],"agreements":[]}`
	report, err := Run(context.Background(), infrastructure.FakeProvider{Output: output}, cases, digest, Thresholds{MinimumPrecision: .9, MinimumRecall: .9})
	if err != nil || !report.Passed || report.TruePositive != 1 {
		t.Fatalf("v2 benchmark: %+v %v", report, err)
	}
}

func TestLoadRejectsDuplicateAndRunCountsInvalid(t *testing.T) {
	if _, _, err := Load(strings.NewReader(dataset + dataset)); err == nil {
		t.Fatal("expected duplicate rejection")
	}
	cases, digest, _ := Load(strings.NewReader(dataset))
	report, err := Run(context.Background(), infrastructure.FakeProvider{Output: `not json`}, cases, digest, Thresholds{MinimumRecall: 1})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.Invalid != 1 || report.FalseNegative != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestLoadRejectsUnknownEvidenceAndTrailingJSON(t *testing.T) {
	unknownEvidence := strings.Replace(dataset, `"message-1"]}]}`, `"missing-message"]}]}`, 1)
	if _, _, err := Load(strings.NewReader(unknownEvidence)); err == nil {
		t.Fatal("expected unknown evidence rejection")
	}
	withTrailingJSON := strings.TrimSpace(dataset) + `{}` + "\n"
	if _, _, err := Load(strings.NewReader(withTrailingJSON)); err == nil {
		t.Fatal("expected trailing JSON rejection")
	}
}

func TestLoadPreservesHistoricalPromptAndRejectsUnknownVersion(t *testing.T) {
	cases, _, err := Load(strings.NewReader(dataset))
	if err != nil || cases[0].Input.PromptVersion != "analyze-conversation.prompt.v5" {
		t.Fatalf("historical dataset changed: %v", err)
	}
	unknown := strings.ReplaceAll(dataset, "analyze-conversation.prompt.v5", "analyze-conversation.prompt.unknown")
	if _, _, err := Load(strings.NewReader(unknown)); err == nil {
		t.Fatal("unknown prompt version accepted")
	}
}

func TestBenchmarkRejectsUngroundedPriceLikeProduction(t *testing.T) {
	cases, digest, err := Load(strings.NewReader(dataset))
	if err != nil {
		t.Fatal(err)
	}
	cases[0].Input.Messages[0].Body = "Подскажите стоимость полировки."
	cases[0].Expected = nil
	provider := infrastructure.FakeProvider{Output: `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"message-1","summary":"Вопрос о цене.","facts":[{"type":"PRICE_MENTIONED","value":true,"confidence":0.99,"amount":"0","currency":"RUB","evidenceMessageIds":["message-1"]}]}`}
	report, err := Run(context.Background(), provider, cases, digest, Thresholds{MinimumValidRate: 1})
	if err != nil || report.Invalid != 1 || report.Passed {
		t.Fatalf("report = %#v, error = %v", report, err)
	}
}

func TestAuditRejectsConversationLeakage(t *testing.T) {
	cases, _, err := Load(strings.NewReader(dataset))
	if err != nil {
		t.Fatal(err)
	}
	copyCase := cases[0]
	copyCase.ID = "booking-validation-001"
	copyCase.Split = SplitDev
	if _, err := AuditCases(append(cases, copyCase)); err == nil {
		t.Fatal("expected duplicate conversation rejection")
	}
}

func TestPriceComparisonNormalizesDecimalNotation(t *testing.T) {
	expectedAmount := "15000.00"
	actualAmount := "015000.0"
	expected := domain.SemanticFact{Type: domain.FactPriceMentioned, Value: true, Amount: &expectedAmount, Currency: "RUB", EvidenceMessageIDs: []string{"message-1"}}
	actual := domain.SemanticFact{Type: domain.FactPriceMentioned, Value: true, Amount: &actualAmount, Currency: "RUB", EvidenceMessageIDs: []string{"message-1"}}
	if !sameSemanticFact(actual, expected) {
		t.Fatal("equivalent decimal amounts must match")
	}
}

func TestRunAppliesValidRateThreshold(t *testing.T) {
	cases, digest, err := Load(strings.NewReader(dataset))
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), infrastructure.FakeProvider{Output: `not json`}, cases, digest, Thresholds{MinimumValidRate: .99})
	if err != nil {
		t.Fatal(err)
	}
	if report.ValidRate != 0 || report.Passed {
		t.Fatalf("unexpected valid-rate result: %+v", report)
	}
}

func TestEmptyAgreementLabelsSurviveDatasetRoundTrip(t *testing.T) {
	cases, _, err := Load(strings.NewReader(dataset))
	if err != nil {
		t.Fatal(err)
	}
	c := cases[0]
	c.Input.SchemaVersion = application.AnalysisSchemaV2
	c.Input.PromptVersion = application.AnalysisPromptV8
	c.ExpectedAgreements = []domain.AgreementObservation{}
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := Load(strings.NewReader(string(encoded)))
	if err != nil || decoded[0].ExpectedAgreements == nil {
		t.Fatalf("negative agreement label lost: %s; %v", encoded, err)
	}
}

// An overall recall gate must not hide a completely missed smaller category.
func TestPerFactRecallGate(t *testing.T) {
	cases, digest, err := Load(strings.NewReader(dataset))
	if err != nil {
		t.Fatal(err)
	}
	provider := infrastructure.FakeProvider{Output: `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"message-1","summary":"Намерение пропущено.","facts":[]}`}
	for _, threshold := range []float64{0, .9} {
		report, err := Run(context.Background(), provider, cases, digest, Thresholds{MinimumFactRecall: threshold})
		if err != nil {
			t.Fatal(err)
		}
		if report.Passed != (threshold == 0) {
			t.Fatalf("threshold=%v report=%+v", threshold, report)
		}
	}
}

func TestIntentRegressionDatasetLabels(t *testing.T) {
	f, err := os.Open("../../../../models/datasets/intent_regression_v2.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cases, _, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 25 {
		t.Fatal("missing intent boundary scenarios")
	}
	if _, err := AuditCases(cases); err != nil {
		t.Fatal(err)
	}
}

func TestContextProbeDatasetStaysWithinProductLimitsAndIsValid(t *testing.T) {
	f, err := os.Open("../../../../models/datasets/context_probe_v1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cases, _, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AuditCases(cases); err != nil {
		t.Fatal(err)
	}
	longest := 0
	for _, c := range cases {
		runes := utf8.RuneCountInString(c.Input.CompanyContext)
		for _, m := range c.Input.Messages {
			runes += utf8.RuneCountInString(m.Body)
		}
		if runes > application.MaxContextRunes || len(c.Input.Messages) > application.MaxContextMessages {
			t.Fatalf("%s exceeds the product limits: %d runes, %d messages", c.ID, runes, len(c.Input.Messages))
		}
		if runes > longest {
			longest = runes
		}
		if c.Input.SchemaVersion != application.AnalysisSchemaV2 || c.Input.PromptVersion != application.CurrentAnalysisPrompt {
			t.Fatalf("%s must use the current contract and instruction", c.ID)
		}
	}
	// Зонд обязан доходить до предела продукта, иначе он ничего не доказывает.
	if longest < application.MaxContextRunes-application.MaxContextMessages {
		t.Fatalf("the longest probe has %d runes of %d allowed", longest, application.MaxContextRunes)
	}
}

func TestHoldoutIsASealedSplitThatNeedsItsOwnChecksum(t *testing.T) {
	for split, want := range map[Split]struct{ valid, sealed bool }{
		SplitDev: {true, false}, SplitGolden: {true, true}, SplitHoldout: {true, true}, Split("TRAIN"): {false, false}, Split(""): {false, false},
	} {
		if split.Valid() != want.valid || split.Sealed() != want.sealed {
			t.Fatalf("split %q: valid %v sealed %v, want %+v", split, split.Valid(), split.Sealed(), want)
		}
	}
	line := `{"version":"` + DatasetVersion + `","id":"holdout-1","split":"HOLDOUT","input":{"task":"ANALYZE_CONVERSATION","schemaVersion":"analyze-conversation.v2","promptVersion":"analyze-conversation.prompt.v9","conversationId":"c","baseConversationRevision":1,"analysisThroughMessageId":"m1","messages":[{"id":"m1","direction":"OUTGOING","body":"Отвечу завтра."}]},"expectedFacts":[{"type":"BUSINESS_COMMITMENT","value":true,"confidence":1,"evidenceMessageIds":["m1"]}]}`
	cases, digest, err := Load(strings.NewReader(line))
	if err != nil || len(cases) != 1 || cases[0].Split != SplitHoldout {
		t.Fatalf("a HOLDOUT case must load: %v %v", cases, err)
	}
	if err := VerifyGolden(digest, strings.Repeat("0", 64)); err == nil {
		t.Fatal("a sealed set must be refused when its checksum does not match")
	}
}

func TestCommitmentDevDatasetIsValidAndLabelsTheCompanyPromiseAsEvidence(t *testing.T) {
	f, err := os.Open("../../../../models/datasets/commitment_dev_v1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cases, _, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AuditCases(cases); err != nil {
		t.Fatal(err)
	}
	positives, negatives := 0, 0
	for _, c := range cases {
		if c.Split != SplitDev || c.Input.SchemaVersion != application.AnalysisSchemaV2 || c.Input.PromptVersion != application.CurrentAnalysisPrompt {
			t.Fatalf("%s must be a DEV case on the current contract and instruction", c.ID)
		}
		if len(c.Expected) > 1 || (len(c.Expected) == 1 && c.Expected[0].Type != domain.FactBusinessCommitment) {
			t.Fatalf("%s: the set measures business commitments only: %+v", c.ID, c.Expected)
		}
		if len(c.Expected) == 0 {
			negatives++
			continue
		}
		positives++
		evidence := c.Expected[0].EvidenceMessageIDs
		last := ""
		for _, m := range c.Input.Messages {
			if m.Direction == "OUTGOING" {
				last = m.ID
			}
		}
		if len(evidence) != 1 || evidence[0] != last {
			t.Fatalf("%s: the evidence must be the last company message", c.ID)
		}
	}
	if positives < 100 || negatives < 100 {
		t.Fatalf("the set needs both classes: %d promises, %d non-promises", positives, negatives)
	}
}

func TestCommitmentAgreementsAreTrackedAgainstTheLabelledPromise(t *testing.T) {
	promise := func(id, last string) Case {
		return Case{Version: DatasetVersion, ID: id, Split: SplitDev,
			Input: application.AnalyzeConversationRequestV1{Task: "ANALYZE_CONVERSATION", SchemaVersion: application.AnalysisSchemaV2, PromptVersion: application.AnalysisPromptV9, ConversationID: id, BaseConversationRevision: 1, AnalysisThroughMessageID: id + "-m2",
				Messages: []application.ContextMessage{{ID: id + "-m1", Direction: "INCOMING", Body: "Когда будет готов заказ?"}, {ID: id + "-m2", Direction: "OUTGOING", Body: last}}},
			Expected: []domain.SemanticFact{{Type: domain.FactBusinessCommitment, Value: true, Confidence: 1, EvidenceMessageIDs: []string{id + "-m2"}}}}
	}
	none := Case{Version: DatasetVersion, ID: "none", Split: SplitDev,
		Input: application.AnalyzeConversationRequestV1{Task: "ANALYZE_CONVERSATION", SchemaVersion: application.AnalysisSchemaV2, PromptVersion: application.AnalysisPromptV9, ConversationID: "none", BaseConversationRevision: 1, AnalysisThroughMessageID: "none-m2",
			Messages: []application.ContextMessage{{ID: "none-m1", Direction: "INCOMING", Body: "Сколько стоит?"}, {ID: "none-m2", Direction: "OUTGOING", Body: "Работаем до восьми."}}}}
	agreement := func(trigger string, confidence float64, waiting domain.AgreementWaitingFor) domain.AgreementObservation {
		return domain.AgreementObservation{Kind: domain.AgreementCommitment, WaitingFor: waiting, Status: domain.AgreementPending, TriggerMessageID: trigger, EvidenceMessageIDs: []string{trigger}, Confidence: confidence}
	}
	// ответы по случаям: найдено, пропущено, слабая договорённость, лишняя на необещании
	answers := map[string][]domain.AgreementObservation{
		"hit":  {agreement("hit-m2", .95, domain.AgreementBusiness)},
		"miss": {},
		"weak": {agreement("weak-m2", .6, domain.AgreementBusiness)},
		"wait": {agreement("wait-m2", .95, domain.AgreementCustomer)},
		"none": {agreement("none-m2", .95, domain.AgreementBusiness)},
	}
	cases := []Case{promise("hit", "Позвоню завтра."), promise("miss", "Позвоню завтра."), promise("weak", "Позвоню завтра."), promise("wait", "Позвоню завтра."), none}
	provider := answerByCase(func(id string) string {
		var facts []domain.SemanticFact
		for _, c := range cases {
			if c.ID == id {
				facts = c.Expected
			}
		}
		if facts == nil {
			facts = []domain.SemanticFact{}
		}
		through := id + "-m2"
		raw, _ := json.Marshal(domain.AnalysisResultV2{SchemaVersion: application.AnalysisSchemaV2, AnalysisThroughMessageID: through, Summary: "Резюме.", Facts: facts, Agreements: answers[id]})
		return string(raw)
	})
	report, err := Run(context.Background(), provider, cases, "sum", Thresholds{MinimumValidRate: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := report.CommitmentAgreements
	// найдена одна из четырёх; слабая и ожидающая клиента не в счёт; одна лишняя на необещании
	if got == nil || got.TruePositive != 1 || got.FalseNegative != 3 || got.FalsePositive != 1 || got.Precision != .5 || got.Recall != .25 {
		t.Fatalf("commitment agreements: %+v", got)
	}
}

func TestCommitmentAgreementsCountAnInvalidAnswerAsMissedPromises(t *testing.T) {
	c := Case{Version: DatasetVersion, ID: "bad", Split: SplitDev,
		Input: application.AnalyzeConversationRequestV1{Task: "ANALYZE_CONVERSATION", SchemaVersion: application.AnalysisSchemaV2, PromptVersion: application.AnalysisPromptV9, ConversationID: "bad", BaseConversationRevision: 1, AnalysisThroughMessageID: "bad-m2",
			Messages: []application.ContextMessage{{ID: "bad-m1", Direction: "INCOMING", Body: "Когда?"}, {ID: "bad-m2", Direction: "OUTGOING", Body: "Позвоню завтра."}}},
		Expected: []domain.SemanticFact{{Type: domain.FactBusinessCommitment, Value: true, Confidence: 1, EvidenceMessageIDs: []string{"bad-m2"}}}}
	report, err := Run(context.Background(), infrastructure.FakeProvider{Output: "not json"}, []Case{c}, "sum", Thresholds{MinimumValidRate: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := report.CommitmentAgreements; report.Invalid != 1 || got == nil || got.FalseNegative != 1 || got.TruePositive != 0 {
		t.Fatalf("an invalid answer misses the promise: %+v %+v", report.Invalid, got)
	}
	// Набор версии 1 договорённостей не содержит: метрики нет.
	v1, _, err := Load(strings.NewReader(dataset))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Run(context.Background(), infrastructure.FakeProvider{Output: `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"message-1","summary":"Запрос.","facts":[]}`}, v1, "sum", Thresholds{MinimumValidRate: 0})
	if err != nil || plain.CommitmentAgreements != nil {
		t.Fatalf("v1 has no agreements: %+v %v", plain.CommitmentAgreements, err)
	}
}

// answerByCase отвечает на запрос случая заранее заготовленным ответом.
type answerByCase func(caseID string) string

func (f answerByCase) Infer(ctx context.Context, _ string) (string, error) {
	return f(CaseID(ctx)), nil
}
