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

// FakeProvider makes the agent and disconnect recovery testable without a GPU.
type FakeProvider struct {
	Output string
	Err    error
}

func (p FakeProvider) Infer(context.Context, string) (string, error) {
	if p.Err != nil {
		return "", p.Err
	}
	if p.Output == "" {
		return `{"facts":[]}`, nil
	}
	return p.Output, nil
}

// LlamaProvider calls a llama.cpp OpenAI-compatible endpoint reachable only
// from the node. It deliberately retains neither prompts nor responses.
type LlamaProvider struct {
	URL    string
	Client *http.Client
}

func (p LlamaProvider) Infer(ctx context.Context, prompt string) (string, error) {
	if p.URL == "" {
		return "", errors.New("llama.cpp URL is required")
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	body, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": prompt}}, "temperature": 0})
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
