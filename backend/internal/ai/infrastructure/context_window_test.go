package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lidradar/backend/internal/ai/application"
)

const exceedBody = `{"error":{"code":400,"message":"request (6417 tokens) exceeds the available context size (4096 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":6417,"n_ctx":4096}}`

func longConversation(count, runes int) []application.ContextMessage {
	messages := make([]application.ContextMessage, count)
	for i := range messages {
		direction := "INCOMING"
		if i%2 == 1 {
			direction = "OUTGOING"
		}
		messages[i] = application.ContextMessage{ID: fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1), Direction: direction, Body: strings.Repeat("слово ", runes/6)}
	}
	return messages
}

// requestedMessages читает, сколько сообщений модель получила в последнем запросе.
func requestedMessages(t *testing.T, r *http.Request) []application.ContextMessage {
	t.Helper()
	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	var request application.AnalyzeConversationRequestV1
	if err := json.Unmarshal([]byte(body.Messages[len(body.Messages)-1].Content), &request); err != nil {
		t.Fatal(err)
	}
	return request.Messages
}

func okAnswer(through string) string {
	content := `{"schemaVersion":"analyze-conversation.v2","analysisThroughMessageId":"` + through + `","summary":"Переписка без ожиданий.","facts":[],"agreements":[]}`
	encoded, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
	return string(encoded)
}

func TestParseContextExceeded(t *testing.T) {
	got, ok := parseContextExceeded([]byte(exceedBody))
	if !ok || got.promptTokens != 6417 || got.contextTokens != 4096 {
		t.Fatalf("parsed: %+v %v", got, ok)
	}
	for _, body := range []string{`not json`, `{"error":{"type":"invalid_request_error","message":"bad"}}`, `{"error":{"type":"exceed_context_size_error"}}`, `{"error":{"type":"invalid_request_error","n_prompt_tokens":6000,"n_ctx":4096}}`, ``} {
		if _, ok := parseContextExceeded([]byte(body)); ok {
			t.Errorf("%q must not be treated as a context overflow", body)
		}
	}
}

func TestShrinkToContextDropsTheOldestMessagesAndKeepsTheLast(t *testing.T) {
	messages := longConversation(20, 600)
	prompt := v9Request(messages...)
	shrunk, ok := shrinkToContext(prompt, contextExceededError{promptTokens: 4657, contextTokens: 4096})
	if !ok {
		t.Fatal("a 20-message conversation must be shrinkable")
	}
	var request application.AnalyzeConversationRequestV1
	if err := json.Unmarshal([]byte(shrunk), &request); err != nil {
		t.Fatal(err)
	}
	// Превышение 1161 токен, сообщение весит около 284 (600 рун · 0,45 + 14): уходят пять самых старых.
	if len(request.Messages) != 15 || request.Messages[0].ID != messages[5].ID || request.Messages[len(request.Messages)-1].ID != messages[19].ID {
		t.Fatalf("window: %d messages, first %s", len(request.Messages), request.Messages[0].ID)
	}
	if request.AnalysisThroughMessageID != messages[19].ID || request.PromptVersion != application.AnalysisPromptV9 {
		t.Fatalf("the boundary and the instruction must stay: %+v", request)
	}
	// Даже при огромном превышении остаётся последнее сообщение, а один раз всегда убирается хотя бы одно.
	most, _ := shrinkToContext(prompt, contextExceededError{promptTokens: 90000, contextTokens: 4096})
	var last application.AnalyzeConversationRequestV1
	_ = json.Unmarshal([]byte(most), &last)
	if len(last.Messages) != 1 || last.Messages[0].ID != messages[19].ID {
		t.Fatalf("the last message must survive: %+v", last.Messages)
	}
	least, _ := shrinkToContext(prompt, contextExceededError{promptTokens: 4097, contextTokens: 4096})
	var once application.AnalyzeConversationRequestV1
	_ = json.Unmarshal([]byte(least), &once)
	if len(once.Messages) >= 20 {
		t.Fatal("at least one message must be dropped")
	}
	for _, single := range []string{v9Request(messages[:1]...), "not json"} {
		if _, ok := shrinkToContext(single, contextExceededError{promptTokens: 9000, contextTokens: 4096}); ok {
			t.Fatalf("nothing to shrink in %.40q", single)
		}
	}
}

func TestV9RetriesWithAShorterWindowWhenTheContextIsExceeded(t *testing.T) {
	var sizes []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		messages := requestedMessages(t, r)
		sizes = append(sizes, len(messages))
		if len(messages) > 14 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, exceedBody)
			return
		}
		_, _ = io.WriteString(w, okAnswer(messages[len(messages)-1].ID))
	}))
	defer server.Close()
	messages := longConversation(20, 600)
	answer, err := (LlamaProvider{URL: server.URL + "/v1/chat/completions"}).Infer(context.Background(), v9Request(messages...))
	if err != nil {
		t.Fatalf("a window that fits must be analysed: %v (windows %v)", err, sizes)
	}
	if len(sizes) < 2 || sizes[0] != 20 || sizes[len(sizes)-1] > 14 {
		t.Fatalf("windows tried: %v", sizes)
	}
	result, err := application.ValidateAnalysisResultV2(answer, messages[19].ID)
	if err != nil || result.AnalysisThroughMessageID != messages[19].ID {
		t.Fatalf("the answer must keep the canonical boundary: %v %s", err, answer)
	}
}

func TestContextRetriesAreBoundedAndOnlyForV9AndOnlyForTheContextError(t *testing.T) {
	count := func(version, body string, status int) (int, error) {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
		defer server.Close()
		prompt := strings.Replace(v9Request(longConversation(20, 600)...), application.AnalysisPromptV9, version, 1)
		_, err := (LlamaProvider{URL: server.URL + "/v1/chat/completions"}).Infer(context.Background(), prompt)
		return requests, err
	}
	// Небольшое превышение (601 токен): за повтор уходит по три сообщения, сервер упорствует — повторов ровно maxContextRetries.
	stubborn := `{"error":{"code":400,"message":"request (4097 tokens) exceeds the available context size (4096 tokens)","type":"exceed_context_size_error","n_prompt_tokens":4097,"n_ctx":4096}}`
	if requests, err := count(application.AnalysisPromptV9, stubborn, http.StatusBadRequest); err == nil || requests != 1+maxContextRetries {
		t.Fatalf("v9 must stop after %d retries: %d requests, error %v", maxContextRetries, requests, err)
	} else if !strings.Contains(err.Error(), "не умещается в контекст") {
		t.Fatalf("the error must say why: %v", err)
	}
	if requests, err := count(application.AnalysisPromptV9, exceedBody, http.StatusBadRequest); err == nil || requests != 3 {
		t.Fatalf("a large excess narrows the window to one message and then stops: %d requests, error %v", requests, err)
	}
	if requests, err := count(application.AnalysisPromptV9, `{"error":{"type":"server_error","message":"boom"}}`, http.StatusBadRequest); err == nil || requests != 1 {
		t.Fatalf("another error must not be retried: %d requests, error %v", requests, err)
	}
	if requests, err := count(application.AnalysisPromptV9, exceedBody, http.StatusInternalServerError); err == nil || requests != 1 {
		t.Fatalf("only status 400 means the context is exceeded: %d requests, error %v", requests, err)
	}
	if requests, err := count(application.AnalysisPromptV8, exceedBody, http.StatusBadRequest); err == nil || requests != 1 {
		t.Fatalf("historical v8 must not be shrunk: %d requests, error %v", requests, err)
	}
}
