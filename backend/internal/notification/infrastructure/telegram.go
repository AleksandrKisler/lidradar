// Package infrastructure contains notification persistence and provider adapters.
package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

var ErrTelegram = errors.New("telegram delivery failed")

type TelegramTransport struct {
	BaseURL, BotToken string
	Client            *http.Client
}

func (t TelegramTransport) Send(ctx context.Context, destination, title, body, callbackData string) (string, bool, error) {
	if t.Client == nil || t.BaseURL == "" || t.BotToken == "" || destination == "" {
		return "", false, ErrTelegram
	}
	payload := map[string]any{"chat_id": destination, "text": title + "\n\n" + body,
		"reply_markup": map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "Открыть риск", "callback_data": callbackData}}}}}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.BaseURL+"/bot"+t.BotToken+"/sendMessage", bytes.NewReader(encoded))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.Client.Do(req)
	if err != nil {
		return "", true, fmt.Errorf("%w: transport", ErrTelegram)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", true, ErrTelegram
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return "", true, fmt.Errorf("%w: status %d", ErrTelegram, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false, fmt.Errorf("%w: status %d", ErrTelegram, resp.StatusCode)
	}
	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID json.Number `json:"message_id"`
		} `json:"result"`
	}
	if json.Unmarshal(data, &result) != nil || !result.OK || result.Result.MessageID == "" {
		return "", false, ErrTelegram
	}
	return string(result.Result.MessageID), false, nil
}
