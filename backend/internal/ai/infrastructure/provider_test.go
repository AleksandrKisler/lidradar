package infrastructure

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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
