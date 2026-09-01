package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// FakeProvider позволяет проверять агент и восстановление после разрыва без GPU.
type FakeProvider struct {
	Output string
	Err    error
}

func (p FakeProvider) Ready(context.Context) error { return p.Err }

func (p FakeProvider) Infer(_ context.Context, prompt string) (string, error) {
	if p.Err != nil {
		return "", p.Err
	}
	if p.Output == "" {
		var request struct {
			AnalysisThroughMessageID string `json:"analysisThroughMessageId"`
		}
		if err := json.Unmarshal([]byte(prompt), &request); err != nil || request.AnalysisThroughMessageID == "" {
			return "", errors.New("fake AI received invalid versioned prompt")
		}
		result, _ := json.Marshal(map[string]any{
			"schemaVersion":            "analyze-conversation.v1",
			"analysisThroughMessageId": request.AnalysisThroughMessageID,
			"summary":                  "Существенные факты не обнаружены.",
			"facts":                    []any{},
		})
		return string(result), nil
	}
	return p.Output, nil
}

// LlamaProvider вызывает совместимый с OpenAI маршрут llama.cpp, доступный
// только внутри узла. Запросы и ответы намеренно не сохраняются.
type LlamaProvider struct {
	URL, HealthURL, Model string
	Client                *http.Client
}

// analysisResultGenerationSchemaV1 — совместимое с грамматикой подмножество
// канонического контракта analyze-conversation.v1. Валидатор приложения
// остаётся авторитетным и дополнительно проверяет длину и смысловую
// согласованность. Ограничение summary.maxLength намеренно отсутствует:
// llama.cpp разворачивает большую строковую границу в слишком крупную грамматику.
var analysisResultGenerationSchemaV1 = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["schemaVersion", "analysisThroughMessageId", "summary", "facts"],
  "properties": {
    "schemaVersion": {"const": "analyze-conversation.v1"},
    "analysisThroughMessageId": {"type": "string", "minLength": 1},
    "summary": {"type": "string", "minLength": 1},
    "facts": {
      "type": "array",
      "items": {
        "oneOf": [
          {
            "type": "object",
            "additionalProperties": false,
            "required": ["type", "value", "confidence", "evidenceMessageIds", "amount", "currency"],
            "properties": {
              "type": {"const": "PRICE_MENTIONED"},
              "value": {"const": true},
              "confidence": {"type": "number", "minimum": 0, "maximum": 1},
              "evidenceMessageIds": {"type": "array", "minItems": 1, "items": {"type": "string", "minLength": 1}},
              "amount": {"type": "string", "minLength": 1},
              "currency": {"type": "string", "minLength": 1}
            }
          },
          {
            "type": "object",
            "additionalProperties": false,
            "required": ["type", "value", "confidence", "evidenceMessageIds"],
            "properties": {
              "type": {"const": "PRICE_MENTIONED"},
              "value": {"const": false},
              "confidence": {"type": "number", "minimum": 0, "maximum": 1},
              "evidenceMessageIds": {"type": "array", "minItems": 1, "items": {"type": "string", "minLength": 1}}
            }
          },
          {
            "type": "object",
            "additionalProperties": false,
            "required": ["type", "value", "confidence", "evidenceMessageIds"],
            "properties": {
              "type": {"enum": ["BOOKING_INTENT", "BUSINESS_COMMITMENT", "FOLLOW_UP_CANDIDATE"]},
              "value": {"type": "boolean"},
              "confidence": {"type": "number", "minimum": 0, "maximum": 1},
              "evidenceMessageIds": {"type": "array", "minItems": 1, "items": {"type": "string", "minLength": 1}}
            }
          }
        ]
      }
    }
  }
}`)

func (p LlamaProvider) Ready(ctx context.Context) error {
	healthURL := p.HealthURL
	if healthURL == "" {
		healthURL = strings.TrimSuffix(p.URL, "/v1/chat/completions") + "/health"
	}
	if healthURL == "/health" {
		return errors.New("llama.cpp health URL is required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return err
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("llama.cpp health returned status %d", response.StatusCode)
	}
	return nil
}

func (p LlamaProvider) Infer(ctx context.Context, prompt string) (string, error) {
	if p.URL == "" {
		return "", errors.New("llama.cpp URL is required")
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	body, _ := json.Marshal(map[string]any{
		"model": p.Model,
		"messages": []map[string]string{
			{"role": "system", "content": `Верни только JSON, строго соответствующий переданной схеме.
Каждый тип факта указывай не более одного раза, объединяя подтверждающие сообщения в evidenceMessageIds.
Для PRICE_MENTIONED с value=true обязательно укажи amount строкой и currency трёхбуквенным кодом; с value=false не указывай amount и currency.
Для остальных типов никогда не указывай amount и currency. В evidenceMessageIds используй только ID сообщений из запроса.`},
			{"role": "user", "content": prompt},
		},
		"temperature":          0,
		"reasoning_effort":     "none",
		"chat_template_kwargs": map[string]bool{"enable_thinking": false},
		"response_format": map[string]any{
			"type":   "json_object",
			"schema": analysisResultGenerationSchemaV1,
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("llama.cpp returned status %d", resp.StatusCode)
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if len(result.Choices) != 1 || !json.Valid([]byte(result.Choices[0].Message.Content)) {
		return "", errors.New("llama.cpp returned invalid structured output")
	}
	return result.Choices[0].Message.Content, nil
}
