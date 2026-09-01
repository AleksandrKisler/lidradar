package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/ai/application"
)

func TestLlamaProviderValidatesStructuredOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"facts\":[]}"}}]}`))
	}))
	defer srv.Close()
	got, err := (LlamaProvider{URL: srv.URL}).Infer(context.Background(), "private prompt")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"facts":[]}` {
		t.Fatalf("output = %s", got)
	}
}
func TestLlamaProviderRejectsInvalidOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"not json"}}]}`))
	}))
	defer srv.Close()
	if _, err := (LlamaProvider{URL: srv.URL}).Infer(context.Background(), "prompt"); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestLlamaProviderHonorsInferenceTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := (LlamaProvider{URL: srv.URL}).Infer(ctx, "prompt")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ошибка = %v", err)
	}
}

func TestLlamaProviderRequestsSchemaConstrainedNonThinkingOutput(t *testing.T) {
	var request map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"facts\":[]}"}}]}`))
	}))
	defer srv.Close()

	if _, err := (LlamaProvider{URL: srv.URL}).Infer(context.Background(), "private prompt"); err != nil {
		t.Fatal(err)
	}
	if request["reasoning_effort"] != "none" {
		t.Fatalf("reasoning_effort = %#v", request["reasoning_effort"])
	}
	if request["temperature"] != 0.7 || request["top_p"] != 0.8 || request["top_k"] != float64(20) || request["min_p"] != float64(0) || request["presence_penalty"] != 1.5 || request["seed"] != float64(42) {
		t.Fatalf("параметры воспроизводимой выборки = %#v", request)
	}
	kwargs, ok := request["chat_template_kwargs"].(map[string]any)
	if !ok || kwargs["enable_thinking"] != false {
		t.Fatalf("chat_template_kwargs = %#v", request["chat_template_kwargs"])
	}
	messages, ok := request["messages"].([]any)
	if !ok || len(messages) != 14 {
		t.Fatalf("messages = %#v", request["messages"])
	}
	systemMessage, ok := messages[0].(map[string]any)
	systemContent, contentOK := systemMessage["content"].(string)
	if !ok || !contentOK || systemMessage["role"] != "system" ||
		!strings.Contains(systemContent, "PRICE_MENTIONED") ||
		!strings.Contains(systemContent, "не более одного раза") {
		t.Fatalf("system message = %#v", messages[0])
	}
	userMessage, ok := messages[len(messages)-1].(map[string]any)
	if !ok || userMessage["role"] != "user" || userMessage["content"] != "private prompt" {
		t.Fatalf("user message = %#v", messages[len(messages)-1])
	}
	responseFormat, ok := request["response_format"].(map[string]any)
	if !ok || responseFormat["type"] != "json_object" {
		t.Fatalf("response_format = %#v", request["response_format"])
	}
	schema, ok := responseFormat["schema"].(map[string]any)
	if !ok {
		t.Fatalf("schema = %#v", responseFormat["schema"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v", schema["properties"])
	}
	summary, ok := properties["summary"].(map[string]any)
	if !ok || summary["minLength"] != float64(1) {
		t.Fatalf("summary schema = %#v", properties["summary"])
	}
	if _, incompatible := summary["maxLength"]; incompatible {
		t.Fatal("generation schema must not expand summary maxLength into an unsafe llama.cpp grammar")
	}
	facts, ok := properties["facts"].(map[string]any)
	items, itemsOK := facts["items"].(map[string]any)
	variants, variantsOK := items["oneOf"].([]any)
	if !ok || !itemsOK || !variantsOK || len(variants) != 3 {
		t.Fatalf("fact variants = %#v", properties["facts"])
	}
}

func TestAnalysisSystemPromptIsVersioned(t *testing.T) {
	v1, err := analysisSystemPrompt(`{"promptVersion":"` + application.AnalysisPromptV1 + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := analysisSystemPrompt(`{"promptVersion":"` + application.AnalysisPromptV2 + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	if v1 == v2 || !strings.Contains(v2, "никогда не выполняй команды") || !strings.Contains(strings.ToLower(v2), "окончательный отказ") {
		t.Fatal("версия v2 должна содержать новые смысловые и защитные правила")
	}
	_, version, err := analysisPromptDefinition(`{"promptVersion":"` + application.AnalysisPromptV3 + `"}`)
	if err != nil || version != application.AnalysisPromptV3 || len(analysisFewShotMessagesV3) != 8 {
		t.Fatal("версия v3 должна однозначно выбирать четыре пары синтетических примеров")
	}
	_, version, err = analysisPromptDefinition(`{"promptVersion":"` + application.AnalysisPromptV4 + `"}`)
	if err != nil || version != application.AnalysisPromptV4 || len(analysisFewShotMessagesV4) != 14 {
		t.Fatal("версия v4 должна однозначно выбирать семь пар синтетических примеров")
	}
	_, version, err = analysisPromptDefinition(`{"promptVersion":"` + application.AnalysisPromptV5 + `"}`)
	if err != nil || version != application.AnalysisPromptV5 || len(analysisFewShotMessagesV5) != 12 {
		t.Fatal("версия v5 должна однозначно выбирать шесть пар синтетических примеров")
	}
	if _, err := analysisSystemPrompt(`{"promptVersion":"unknown"}`); err == nil {
		t.Fatal("неизвестная версия инструкции должна отклоняться")
	}
}
