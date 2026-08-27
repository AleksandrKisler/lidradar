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

// FakeProvider makes the agent and disconnect recovery testable without a GPU.
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

// LlamaProvider calls a llama.cpp OpenAI-compatible endpoint reachable only
// from the node. It deliberately retains neither prompts nor responses.
type LlamaProvider struct {
	URL, HealthURL, Model string
	Client                *http.Client
}

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
		"model":           p.Model,
		"messages":        []map[string]string{{"role": "user", "content": prompt}},
		"temperature":     0,
		"response_format": map[string]string{"type": "json_object"},
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
