package infrastructure

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

func answerWithFacts(facts string) string {
	return `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"m2","summary":"Клиент уточняет статус запроса.","facts":[` + facts + `],"agreements":[]}`
}

const (
	bookingTrue  = `{"type":"BOOKING_INTENT","value":true,"confidence":0.95,"evidenceMessageIds":["m1"]}`
	bookingFalse = `{"type":"BOOKING_INTENT","value":false,"confidence":0.95,"evidenceMessageIds":["m1"]}`
	followUp     = `{"type":"FOLLOW_UP_CANDIDATE","value":true,"confidence":0.9,"evidenceMessageIds":["m1"]}`
)

func TestResolveContradictoryFactsDropsOnlyTheDisputedType(t *testing.T) {
	// Именно эта форма ответа раньше делала недействительным весь ответ («fact 1 contradicts type BOOKING_INTENT»).
	broken := answerWithFacts(bookingTrue + "," + bookingFalse + "," + followUp)
	if _, err := application.ValidateAnalysisResultV2(broken, "m2"); err == nil {
		t.Fatal("the fixture must be rejected by the server validation")
	}
	resolved := resolveContradictoryFacts(broken)
	result, err := application.ValidateAnalysisResultV2(resolved, "m2")
	if err != nil {
		t.Fatalf("the resolved answer must be valid: %v\n%s", err, resolved)
	}
	if len(result.Facts) != 1 || result.Facts[0].Type != domain.FactFollowUpCandidate || result.Summary == "" {
		t.Fatalf("only the disputed type goes, the rest stays: %+v", result)
	}
	// Все факты спорные: остаётся пустой, но действительный список.
	only := resolveContradictoryFacts(answerWithFacts(bookingTrue + "," + bookingFalse))
	if result, err := application.ValidateAnalysisResultV2(only, "m2"); err != nil || result.Facts == nil || len(result.Facts) != 0 {
		t.Fatalf("an empty list is a valid answer: %v %+v", err, result)
	}
}

func TestResolveContradictoryFactsLeavesEverythingElseAlone(t *testing.T) {
	for name, answer := range map[string]string{
		"no facts":            answerWithFacts(""),
		"identical repeat":    answerWithFacts(bookingTrue + "," + bookingTrue),
		"different types":     answerWithFacts(bookingTrue + "," + followUp),
		"not json":            "not json",
		"no facts field":      `{"schemaVersion":"analyze-conversation.v2"}`,
		"facts is not a list": `{"facts":"x"}`,
	} {
		if got := resolveContradictoryFacts(answer); got != answer {
			t.Errorf("%s: the answer must be returned unchanged:\n%s\n%s", name, answer, got)
		}
	}
}

func TestConflictingFactTypesComparesAmountsTheWayTheServerDoes(t *testing.T) {
	amount := func(value string) *string { return &value }
	price := func(value bool, a *string, currency string) domain.SemanticFact {
		return domain.SemanticFact{Type: domain.FactPriceMentioned, Value: value, Confidence: 1, EvidenceMessageIDs: []string{"m1"}, Amount: a, Currency: currency}
	}
	// Десятичная запятая приводится к точке до сравнения, как в серверной проверке.
	same := application.ConflictingFactTypes([]domain.SemanticFact{price(true, amount("3500,50"), "RUB"), price(true, amount("3500.50"), "RUB")})
	if len(same) != 0 {
		t.Fatalf("3500,50 and 3500.50 are one amount: %v", same)
	}
	for name, facts := range map[string][]domain.SemanticFact{
		"another amount":   {price(true, amount("3500"), "RUB"), price(true, amount("4000"), "RUB")},
		"another value":    {price(true, amount("3500"), "RUB"), price(false, nil, "")},
		"another currency": {price(true, amount("3500"), "RUB"), price(true, amount("3500"), "USD")},
	} {
		if got := application.ConflictingFactTypes(facts); !got[domain.FactPriceMentioned] {
			t.Errorf("%s must be a contradiction: %v", name, got)
		}
	}
}

func TestV9ProviderAnswersDespiteAContradictoryFactAndV8StaysStrict(t *testing.T) {
	model := func(through string) string {
		content := `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"` + through + `","summary":"Клиент спросил о запросе.","facts":[` + bookingTrue + `,` + bookingFalse + `],"agreements":[]}`
		encoded, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
		return string(encoded)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, model("m2"))
	}))
	defer server.Close()
	messages := []application.ContextMessage{{ID: "11111111-1111-1111-1111-111111111111", Direction: "INCOMING", Body: "Что с запросом на стрижку?"}, {ID: "22222222-2222-2222-2222-222222222222", Direction: "OUTGOING", Body: "Когда появится время, вопрос могут посмотреть."}}
	provider := LlamaProvider{URL: server.URL + "/v1/chat/completions"}
	answer, err := provider.Infer(context.Background(), v9Request(messages...))
	if err != nil {
		t.Fatalf("v9 must return a valid answer: %v", err)
	}
	if facts := factsOf(t, answer); len(facts) != 0 {
		t.Fatalf("the disputed type must be dropped: %+v", facts)
	}
	v8 := strings.Replace(v9Request(messages...), application.AnalysisPromptV9, application.AnalysisPromptV8, 1)
	if _, err := provider.Infer(context.Background(), v8); err == nil {
		t.Fatal("historical v8 keeps the strict behaviour")
	}
}
