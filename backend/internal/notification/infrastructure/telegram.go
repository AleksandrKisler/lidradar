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

func (t TelegramTransport) Send(ctx context.Context, destination, title, body, notificationID string) (string, bool, error) {
	if t.Client == nil || t.BaseURL == "" || t.BotToken == "" || destination == "" || notificationID == "" {
		return "", false, ErrTelegram
	}
	payload := map[string]any{"chat_id": destination, "text": title + "\n\n" + body,
		"reply_markup": map[string]any{"inline_keyboard": [][]map[string]string{
			{{"text": "Открыть риск", "callback_data": "OPEN_RISK:" + notificationID}},
			{{"text": "Принять в работу", "callback_data": "ACKNOWLEDGE:" + notificationID}},
			{{"text": "Отложить", "callback_data": "SNOOZE:" + notificationID}},
		}}}
	var result struct {
		MessageID json.Number `json:"message_id"`
	}
	retryable, err := t.call(ctx, "sendMessage", payload, &result)
	if err != nil || result.MessageID == "" {
		if err == nil {
			err = ErrTelegram
		}
		return "", retryable, err
	}
	return string(result.MessageID), false, nil
}

func (t TelegramTransport) SendText(ctx context.Context, destination, body string) (string, bool, error) {
	if destination == "" || body == "" {
		return "", false, ErrTelegram
	}
	var result struct {
		MessageID json.Number `json:"message_id"`
	}
	retryable, err := t.call(ctx, "sendMessage", map[string]any{"chat_id": destination, "text": body}, &result)
	if err != nil || result.MessageID == "" {
		if err == nil {
			err = ErrTelegram
		}
		return "", retryable, err
	}
	return string(result.MessageID), false, nil
}

func (t TelegramTransport) AnswerCallback(ctx context.Context, callbackID, text string) (bool, error) {
	if callbackID == "" || text == "" {
		return false, ErrTelegram
	}
	var accepted bool
	retryable, err := t.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": callbackID, "text": text}, &accepted)
	if err != nil || !accepted {
		if err == nil {
			err = ErrTelegram
		}
		return retryable, err
	}
	return false, nil
}

func (t TelegramTransport) call(ctx context.Context, method string, payload any, result any) (bool, error) {
	if t.Client == nil || t.BaseURL == "" || t.BotToken == "" || method == "" {
		return false, ErrTelegram
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.BaseURL+"/bot"+t.BotToken+"/"+method, bytes.NewReader(encoded))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.Client.Do(req)
	if err != nil {
		return true, fmt.Errorf("%w: транспорт", ErrTelegram)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return true, ErrTelegram
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return true, fmt.Errorf("%w: состояние %d", ErrTelegram, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("%w: состояние %d", ErrTelegram, resp.StatusCode)
	}
	var response struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(data, &response) != nil || !response.OK || len(response.Result) == 0 || json.Unmarshal(response.Result, result) != nil {
		return false, ErrTelegram
	}
	return false, nil
}
