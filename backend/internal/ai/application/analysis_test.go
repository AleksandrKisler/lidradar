package application_test

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

func validResult(facts string) string {
	return `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"m2","summary":"Customer is considering a booking.","facts":` + facts + `}`
}

func TestContextBuilderBoundsAndVersionsRequest(t *testing.T) {
	messages := make([]application.ContextMessage, 25)
	for i := range messages {
		messages[i] = application.ContextMessage{ID: string(rune('a' + i)), Direction: "INCOMING", Body: "hello"}
	}
	r, err := application.BuildAnalysisContext(application.ConversationContext{TenantID: "t", ConversationID: "c", Revision: 7, CompanyContext: "Detailing", Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) != application.MaxContextMessages || r.Messages[0].ID != "f" || r.AnalysisThroughMessageID != "y" {
		t.Fatalf("unexpected window: %#v", r.Messages)
	}
	if r.SchemaVersion != application.AnalysisSchemaV1 || r.PromptVersion != application.AnalysisPromptV1 {
		t.Fatalf("versions = %q %q", r.SchemaVersion, r.PromptVersion)
	}
	encoded, err := application.EncodeAnalysisRequest(r)
	if err != nil || strings.Contains(encoded, `"tenantId"`) {
		t.Fatalf("encoded request = %q, %v", encoded, err)
	}
}

func TestContextBuilderKeepsNewestMessagesInsideRuneBudget(t *testing.T) {
	messages := []application.ContextMessage{
		{ID: "old", Direction: "INCOMING", Body: strings.Repeat("а", application.MaxContextRunes)},
		{ID: "new", Direction: "OUTGOING", Body: "Короткий свежий ответ"},
	}
	request, err := application.BuildAnalysisContext(application.ConversationContext{
		TenantID: "tenant", ConversationID: "conversation", Revision: 2,
		CompanyContext: "Компания", Messages: messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 1 || request.Messages[0].ID != "new" ||
		request.AnalysisThroughMessageID != "new" {
		t.Fatalf("неверное окно контекста: %#v", request.Messages)
	}
	if utf8.RuneCountInString(request.CompanyContext)+utf8.RuneCountInString(request.Messages[0].Body) > application.MaxContextRunes {
		t.Fatal("контекст превысил установленный предел")
	}
}

func TestAnalysisContractRejectsMalformedAndInconsistentResults(t *testing.T) {
	amount := "55000.00"
	cases := map[string]string{
		"invalid JSON":         `{`,
		"missing field":        `{"schemaVersion":"analyze-conversation.v1","facts":[]}`,
		"unknown field":        `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"m2","summary":"Кратко.","facts":[],"controlRisk":true}`,
		"unknown enum":         validResult(`[{"type":"MAGIC","value":true,"confidence":0.9,"evidenceMessageIds":["m2"]}]`),
		"contradictory facts":  validResult(`[{"type":"BOOKING_INTENT","value":true,"confidence":0.9,"evidenceMessageIds":["m1"]},{"type":"BOOKING_INTENT","value":false,"confidence":0.8,"evidenceMessageIds":["m2"]}]`),
		"range":                validResult(`[{"type":"BOOKING_INTENT","value":true,"confidence":1.1,"evidenceMessageIds":["m2"]}]`),
		"missing evidence":     validResult(`[{"type":"BOOKING_INTENT","value":true,"confidence":0.9,"evidenceMessageIds":[]}]`),
		"semantic consistency": validResult(`[{"type":"PRICE_MENTIONED","value":false,"confidence":0.9,"evidenceMessageIds":["m2"],"amount":"` + amount + `","currency":"RUB"}]`),
		"invalid currency":     validResult(`[{"type":"PRICE_MENTIONED","value":true,"confidence":0.9,"evidenceMessageIds":["m2"],"amount":"` + amount + `","currency":"rub"}]`),
		"multiple JSON values": validResult(`[]`) + ` {}`,
		"garbage after JSON":   validResult(`[]`) + ` недоверенные данные`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := application.ValidateAnalysisResultV1(raw, "m2"); !errors.Is(err, application.ErrInvalidAIOutput) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestAnalysisContractConservativelyMergesDuplicateFacts(t *testing.T) {
	result, err := application.ValidateAnalysisResultV1(validResult(`[
		{"type":"BOOKING_INTENT","value":true,"confidence":0.9,"evidenceMessageIds":["m1","m1"]},
		{"type":"BOOKING_INTENT","value":true,"confidence":0.7,"evidenceMessageIds":["m2"]}
	]`), "m2")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Facts) != 1 || result.Facts[0].Confidence != 0.7 ||
		len(result.Facts[0].EvidenceMessageIDs) != 2 ||
		result.Facts[0].EvidenceMessageIDs[0] != "m1" || result.Facts[0].EvidenceMessageIDs[1] != "m2" {
		t.Fatalf("объединённые факты = %#v", result.Facts)
	}
}

func TestConfidencePolicyExcludesOnlyUntrustedFacts(t *testing.T) {
	r, err := application.ValidateAnalysisResultV1(validResult(`[
		{"type":"BOOKING_INTENT","value":true,"confidence":0.85,"evidenceMessageIds":["m2"]},
		{"type":"FOLLOW_UP_CANDIDATE","value":true,"confidence":0.65,"evidenceMessageIds":["m2"]},
		{"type":"BUSINESS_COMMITMENT","value":true,"confidence":0.64,"evidenceMessageIds":["m2"]}
	]`), "m2")
	if err != nil {
		t.Fatal(err)
	}
	got := application.TrustedFacts(r)
	if len(got) != 2 || got[0].Type != domain.FactBookingIntent || got[1].Type != domain.FactFollowUpCandidate {
		t.Fatalf("trusted = %#v", got)
	}
}
