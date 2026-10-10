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
