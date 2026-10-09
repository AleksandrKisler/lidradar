package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lidradar/backend/internal/ai/application"
)

func TestAnalysisAliasesPreserveMessagesAndRestoreAllEvidence(t *testing.T) {
	request := application.AnalyzeConversationRequestV1{
		SchemaVersion: application.AnalysisSchemaV2, PromptVersion: application.AnalysisPromptV8,
		ConversationID: "canonical-conversation", AnalysisThroughMessageID: "canonical-offer",
		Messages: []application.ContextMessage{
			{ID: "canonical-intent", Direction: "INCOMING", Body: "Хочу записаться. Код m2 в тексте менять нельзя."},
			{ID: "canonical-offer", Direction: "OUTGOING", Body: "Свободно в пятницу. Вам подходит?"},
		},
	}
	prompt, _ := application.EncodeAnalysisRequest(request)
	encoded, aliases, err := aliasAnalysisRequest(prompt)
	if err != nil {
		t.Fatal(err)
	}
	var input application.AnalyzeConversationRequestV1
	if json.Unmarshal([]byte(encoded), &input) != nil {
		t.Fatal("invalid aliased request")
	}
	if input.Messages[0].ID != "m1" || input.Messages[1].ID != "m2" || input.AnalysisThroughMessageID != "m2" || input.Messages[0].Body != request.Messages[0].Body {
		t.Fatalf("message content or identity map changed: %#v", input)
	}
	raw := `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","summary":"Предложено время.","facts":[{"type":"BOOKING_INTENT","value":true,"confidence":0.96,"evidenceMessageIds":["m1"]}],"agreements":[{"kind":"BOOKING_CONFIRMATION","waitingFor":"CUSTOMER","status":"PENDING","triggerMessageId":"m2","evidenceMessageIds":["m2"],"confidence":0.96}]}`
	restored, err := aliases.restore(raw)
	if err != nil {
		t.Fatal(err)
	}
	result, err := application.ValidateAnalysisResultV2(restored, request.AnalysisThroughMessageID)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.ValidateAgreementEvidence(result, prompt); err != nil {
		t.Fatal(err)
	}
	if result.Facts[0].EvidenceMessageIDs[0] != "canonical-intent" || result.Agreements[0].TriggerMessageID != "canonical-offer" || result.Agreements[0].EvidenceMessageIDs[0] != "canonical-offer" {
		t.Fatal("canonical evidence was not restored")
	}
	if _, err := aliases.restore(strings.Replace(raw, `"evidenceMessageIds":["m1"]`, `"evidenceMessageIds":["m99"]`, 1)); err == nil {
		t.Fatal("foreign evidence alias accepted")
	}
	if _, err := aliases.restore(strings.Replace(raw, `"analysisThroughMessageId":"m2"`, `"analysisThroughMessageId":"m1"`, 1)); err == nil {
		t.Fatal("wrong analysis boundary accepted")
	}
}

func TestAnalysisAliasesRejectAmbiguousContext(t *testing.T) {
	for _, prompt := range []string{
		`{"analysisThroughMessageId":"missing","messages":[{"id":"a"}]}`,
		`{"analysisThroughMessageId":"a","messages":[{"id":"a"},{"id":"a"}]}`,
		`{"analysisThroughMessageId":"a","messages":[{"id":""}]}`,
	} {
		if _, _, err := aliasAnalysisRequest(prompt); err == nil {
			t.Fatalf("accepted ambiguous context: %s", prompt)
		}
	}
}

func TestV8LongDialoguePreservesEveryMessage(t *testing.T) {
	request := application.AnalyzeConversationRequestV1{SchemaVersion: application.AnalysisSchemaV2, PromptVersion: application.AnalysisPromptV8, ConversationID: "synthetic"}
	for i := 1; i <= 20; i++ {
		request.Messages = append(request.Messages, application.ContextMessage{ID: fmt.Sprintf("canonical-%d", i), Direction: "INCOMING", Body: fmt.Sprintf("Синтетическое сообщение %d", i)})
	}
	request.AnalysisThroughMessageID = "canonical-20"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []map[string]string `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if len(body.Messages) >= 20 {
			t.Error("teaching examples consumed long-dialogue budget")
		}
		var input application.AnalyzeConversationRequestV1
		if err := json.Unmarshal([]byte(body.Messages[len(body.Messages)-1]["content"]), &input); err != nil {
			t.Error(err)
			return
		}
		if len(input.Messages) != len(request.Messages) {
			t.Error("dialogue was truncated")
		}
		for i, m := range input.Messages {
			if m.Body != request.Messages[i].Body {
				t.Error("message text changed")
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m20","summary":"Тест.","facts":[],"agreements":[]}`}}}})
	}))
	defer server.Close()
	prompt, _ := application.EncodeAnalysisRequest(request)
	raw, err := (LlamaProvider{URL: server.URL}).Infer(context.Background(), prompt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.ValidateAnalysisResultV2(raw, request.AnalysisThroughMessageID); err != nil {
		t.Fatal(err)
	}
}
