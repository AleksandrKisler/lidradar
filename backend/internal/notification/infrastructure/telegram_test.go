package infrastructure_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"lidradar/backend/internal/notification/infrastructure"
)

func TestTelegramTransportClassifiesFailures(t *testing.T) {
	for _, tc := range []struct {
		status int
		retry  bool
	}{{http.StatusBadRequest, false}, {http.StatusTooManyRequests, true}, {http.StatusBadGateway, true}} {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return response(tc.status, `{}`), nil
		})}
		transport := infrastructure.TelegramTransport{BaseURL: "https://telegram.test", BotToken: "token", Client: client}
		_, retry, err := transport.Send(context.Background(), "chat", "title", "body", "018f0000-0000-7000-8000-000000000001", true)
		if err == nil || retry != tc.retry {
			t.Fatalf("status %d retry=%v err=%v", tc.status, retry, err)
		}
	}
}
func TestTelegramTransportSuccess(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/bottoken/sendMessage" {
			t.Errorf("path %s", r.URL.Path)
		}
		var body struct {
			ReplyMarkup struct {
				Keyboard [][]struct {
					Data string `json:"callback_data"`
				} `json:"inline_keyboard"`
			} `json:"reply_markup"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.ReplyMarkup.Keyboard) != 3 ||
			body.ReplyMarkup.Keyboard[0][0].Data != "OPEN_RISK:018f0000-0000-7000-8000-000000000001" ||
			body.ReplyMarkup.Keyboard[1][0].Data != "ACKNOWLEDGE:018f0000-0000-7000-8000-000000000001" ||
			body.ReplyMarkup.Keyboard[2][0].Data != "SNOOZE:018f0000-0000-7000-8000-000000000001" {
			t.Fatalf("кнопки Telegram = %#v, err=%v", body.ReplyMarkup.Keyboard, err)
		}
		return response(http.StatusOK, `{"ok":true,"result":{"message_id":42}}`), nil
	})}
	transport := infrastructure.TelegramTransport{BaseURL: "https://telegram.test", BotToken: "token", Client: client}
	id, retry, err := transport.Send(context.Background(), "chat", "title", "body", "018f0000-0000-7000-8000-000000000001", true)
	if err != nil || retry || id != "42" {
		t.Fatalf("id=%q retry=%v err=%v", id, retry, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)),
	}
}
