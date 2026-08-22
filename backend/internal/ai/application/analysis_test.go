package application_test

import (
	"errors"
	"strings"
	"testing"

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

func TestAnalysisContractRejectsMalformedAndInconsistentResults(t *testing.T) {
	amount := "55000.00"
	cases := map[string]string{
		"invalid JSON":         `{`,
		"missing field":        `{"schemaVersion":"analyze-conversation.v1","facts":[]}`,
		"unknown enum":         validResult(`[{"type":"MAGIC","value":true,"confidence":0.9,"evidenceMessageIds":["m2"]}]`),
		"range":                validResult(`[{"type":"BOOKING_INTENT","value":true,"confidence":1.1,"evidenceMessageIds":["m2"]}]`),
		"semantic consistency": validResult(`[{"type":"PRICE_MENTIONED","value":false,"confidence":0.9,"evidenceMessageIds":["m2"],"amount":"` + amount + `","currency":"RUB"}]`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := application.ValidateAnalysisResultV1(raw, "m2"); !errors.Is(err, application.ErrInvalidAIOutput) {
				t.Fatalf("error = %v", err)
			}
		})
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
