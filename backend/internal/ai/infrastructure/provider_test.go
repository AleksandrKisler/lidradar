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
	kwargs, ok := request["chat_template_kwargs"].(map[string]any)
	if !ok || kwargs["enable_thinking"] != false {
		t.Fatalf("chat_template_kwargs = %#v", request["chat_template_kwargs"])
	}
	messages, ok := request["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages = %#v", request["messages"])
	}
	systemMessage, ok := messages[0].(map[string]any)
	systemContent, contentOK := systemMessage["content"].(string)
	if !ok || !contentOK || systemMessage["role"] != "system" ||
		!strings.Contains(systemContent, "PRICE_MENTIONED") ||
		!strings.Contains(systemContent, "не более одного раза") {
		t.Fatalf("system message = %#v", messages[0])
	}
	userMessage, ok := messages[1].(map[string]any)
	if !ok || userMessage["role"] != "user" || userMessage["content"] != "private prompt" {
		t.Fatalf("user message = %#v", messages[1])
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
