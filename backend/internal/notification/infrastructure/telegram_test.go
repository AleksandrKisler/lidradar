package infrastructure_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"lidradar/backend/internal/notification/infrastructure"
)

func TestTelegramTransportClassifiesFailures(t *testing.T) {
	for _, tc := range []struct {
		status int
		retry  bool
	}{{http.StatusBadRequest, false}, {http.StatusTooManyRequests, true}, {http.StatusBadGateway, true}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status) }))
		transport := infrastructure.TelegramTransport{BaseURL: srv.URL, BotToken: "token", Client: srv.Client()}
		_, retry, err := transport.Send(context.Background(), "chat", "title", "body", "OPEN_RISK:n")
		srv.Close()
		if err == nil || retry != tc.retry {
			t.Fatalf("status %d retry=%v err=%v", tc.status, retry, err)
		}
	}
}
func TestTelegramTransportSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottoken/sendMessage" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	}))
	defer srv.Close()
	transport := infrastructure.TelegramTransport{BaseURL: srv.URL, BotToken: "token", Client: srv.Client()}
	id, retry, err := transport.Send(context.Background(), "chat", "title", "body", "OPEN_RISK:n")
	if err != nil || retry || id != "42" {
		t.Fatalf("id=%q retry=%v err=%v", id, retry, err)
	}
}
