package infrastructure

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

func v9Request(messages ...application.ContextMessage) string {
	prompt, _ := application.EncodeAnalysisRequest(application.AnalyzeConversationRequestV1{
		Task: "ANALYZE_CONVERSATION", SchemaVersion: application.AnalysisSchemaV2, PromptVersion: application.AnalysisPromptV9,
		ConversationID: "c1", BaseConversationRevision: 1, AnalysisThroughMessageID: messages[len(messages)-1].ID, Messages: messages,
	})
	return prompt
}

func answerJSON(t *testing.T, facts string, agreements ...string) string {
	t.Helper()
	return `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","summary":"Клиент просит подробности, компания обещала.","facts":[` + facts + `],"agreements":[` + strings.Join(agreements, ",") + `]}`
}

func commitment(trigger, status string, confidence string) string {
	return `{"kind":"COMMITMENT","waitingFor":"BUSINESS","status":"` + status + `","triggerMessageId":"` + trigger + `","evidenceMessageIds":["` + trigger + `"],"confidence":` + confidence + `}`
}

var promiseAfterQuestion = []application.ContextMessage{
	{ID: "m1", Direction: "INCOMING", Body: "Нужны подробности про пробный урок."},
	{ID: "m2", Direction: "OUTGOING", Body: "Сформирую счёт и пришлю его сюда."},
}

func factsOf(t *testing.T, answer string) []domain.SemanticFact {
	t.Helper()
	result, err := application.ValidateAnalysisResultV2(answer, "")
	if err != nil {
		t.Fatalf("the answer must stay valid: %v\n%s", err, answer)
	}
	return result.Facts
}

func TestEnsureCommitmentFactAddsTheFactTheAgreementImplies(t *testing.T) {
	prompt := v9Request(promiseAfterQuestion...)
	answer := answerJSON(t, "", commitment("m2", "PENDING", "0.99"))
	got := ensureCommitmentFact(prompt, answer)
	facts := factsOf(t, got)
	if len(facts) != 1 || facts[0].Type != domain.FactBusinessCommitment || !facts[0].Value || facts[0].Confidence != .99 || len(facts[0].EvidenceMessageIDs) != 1 || facts[0].EvidenceMessageIDs[0] != "m2" {
		t.Fatalf("facts: %+v", facts)
	}
	// Достроенный ответ проходит ту же серверную проверку, что и ответ модели.
	result, err := application.ValidateAnalysisResultV2(got, "m2")
	if err != nil || application.ValidateAgreementEvidence(result, prompt) != nil {
		t.Fatalf("completed answer rejected: %v", err)
	}
	if len(result.Agreements) != 1 || result.Summary == "" {
		t.Fatalf("the rest of the answer must be preserved: %+v", result)
	}
}

func TestEnsureCommitmentFactLeavesTheAnswerAloneUnlessEverythingAgrees(t *testing.T) {
	trusted := commitment("m2", "PENDING", "0.99")
	cases := map[string]struct {
		messages []application.ContextMessage
		answer   string
	}{
		"fact already present":             {promiseAfterQuestion, answerJSON(t, `{"type":"BUSINESS_COMMITMENT","value":true,"confidence":0.99,"evidenceMessageIds":["m2"]}`, trusted)},
		"the model said no explicitly":     {promiseAfterQuestion, answerJSON(t, `{"type":"BUSINESS_COMMITMENT","value":false,"confidence":0.99,"evidenceMessageIds":["m2"]}`, trusted)},
		"weak agreement":                   {promiseAfterQuestion, answerJSON(t, "", commitment("m2", "PENDING", "0.6"))},
		"another kind of agreement":        {promiseAfterQuestion, answerJSON(t, "", `{"kind":"RESCHEDULE","waitingFor":"BUSINESS","status":"PENDING","triggerMessageId":"m2","evidenceMessageIds":["m2"],"confidence":0.99}`)},
		"no agreement":                     {promiseAfterQuestion, answerJSON(t, "")},
		"the customer promises":            {[]application.ContextMessage{{ID: "m1", Direction: "OUTGOING", Body: "Здравствуйте!"}, {ID: "m2", Direction: "INCOMING", Body: "Проверю и отвечу вам."}}, answerJSON(t, "", commitment("m2", "PENDING", "0.99"))},
		"negated promise":                  {[]application.ContextMessage{{ID: "m1", Direction: "INCOMING", Body: "Перезвоните мне?"}, {ID: "m2", Direction: "OUTGOING", Body: "Не обещаю, что сможем перезвонить."}}, answerJSON(t, "", trusted)},
		"hedged promise":                   {[]application.ContextMessage{{ID: "m1", Direction: "INCOMING", Body: "Что с заявкой?"}, {ID: "m2", Direction: "OUTGOING", Body: "Возможно, проверю и отвечу."}}, answerJSON(t, "", trusted)},
		"not a promise the server accepts": {[]application.ContextMessage{{ID: "m1", Direction: "INCOMING", Body: "Что с заявкой?"}, {ID: "m2", Direction: "OUTGOING", Body: "Заявка принята в работу."}}, answerJSON(t, "", trusted)},
		"trigger outside the context":      {promiseAfterQuestion, answerJSON(t, "", commitment("m9", "PENDING", "0.99"))},
		"not JSON":                         {promiseAfterQuestion, "not json"},
		"another contract":                 {promiseAfterQuestion, `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"m2","summary":"x","facts":[]}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ensureCommitmentFact(v9Request(tc.messages...), tc.answer); got != tc.answer {
				t.Fatalf("the answer must be returned unchanged:\n%s\n%s", tc.answer, got)
			}
		})
	}
}

func TestEnsureCommitmentFactCitesTheLatestPromiseWhateverItsStatus(t *testing.T) {
	messages := []application.ContextMessage{
		{ID: "m1", Direction: "OUTGOING", Body: "Пришлю ссылку на оплату."},
		{ID: "m2", Direction: "OUTGOING", Body: "Проверю наличие детали."},
		{ID: "m3", Direction: "OUTGOING", Body: "Ссылку на оплату отправил."},
	}
	prompt := v9Request(messages...)
	resolved := `{"kind":"COMMITMENT","waitingFor":"BUSINESS","status":"RESOLVED","triggerMessageId":"m1","evidenceMessageIds":["m1","m3"],"confidence":0.99}`
	answer := strings.Replace(answerJSON(t, "", resolved, commitment("m2", "PENDING", "0.97")), `"m2","summary"`, `"m3","summary"`, 1)
	facts := factsOf(t, ensureCommitmentFact(prompt, answer))
	if len(facts) != 1 || facts[0].EvidenceMessageIDs[0] != "m2" || facts[0].Confidence != .97 {
		t.Fatalf("the fact must cite the latest promise: %+v", facts)
	}
}

func TestLlamaProviderCompletesTheCommitmentFactOnlyForV9(t *testing.T) {
	model := func(through string) string {
		content := `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"` + through + `","summary":"Клиент просит подробности, компания обещала.","facts":[],"agreements":[{"kind":"COMMITMENT","waitingFor":"BUSINESS","status":"PENDING","triggerMessageId":"m2","evidenceMessageIds":["m2"],"confidence":0.99}]}`
		encoded, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
		return string(encoded)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(model("m2"))) }))
	defer server.Close()
	messages := []application.ContextMessage{{ID: "11111111-1111-1111-1111-111111111111", Direction: "INCOMING", Body: "Нужны подробности про пробный урок."}, {ID: "22222222-2222-2222-2222-222222222222", Direction: "OUTGOING", Body: "Сформирую счёт и пришлю его сюда."}}
	provider := LlamaProvider{URL: server.URL + "/v1/chat/completions"}
	v9, err := provider.Infer(context.Background(), v9Request(messages...))
	if err != nil {
		t.Fatal(err)
	}
	facts := factsOf(t, v9)
	if len(facts) != 1 || facts[0].Type != domain.FactBusinessCommitment || facts[0].EvidenceMessageIDs[0] != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("v9 must return the completed fact with canonical IDs: %+v", facts)
	}
	// v8 — историческая версия: ответ модели остаётся как есть.
	v8Prompt := strings.Replace(v9Request(messages...), application.AnalysisPromptV9, application.AnalysisPromptV8, 1)
	v8, err := provider.Infer(context.Background(), v8Prompt)
	if err != nil {
		t.Fatal(err)
	}
	if got := factsOf(t, v8); len(got) != 0 {
		t.Fatalf("v8 must not be completed: %+v", got)
	}
}

func TestNegatesPromise(t *testing.T) {
	for body, want := range map[string]bool{
		"Не обещаю, что сможем перезвонить.": true, "Не могу пообещать обратную связь.": true, "Ничего дополнительно отправлять не планируем.": true,
		"Сформирую счёт и пришлю его сюда.": false, "Проверю вашу оплату и подтвержу получение сегодня.": false, "Позвоню, если появится время": false,
	} {
		if got := negatesPromise(body); got != want {
			t.Errorf("%q: %v, want %v", body, got, want)
		}
	}
}
