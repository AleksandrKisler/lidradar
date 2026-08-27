package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

const maxCloudResponse = 1 << 20

// CloudClient реализует исходящий HTTPS-протокол домашнего AI-узла. Он не
// сохраняет prompt/result и не включает тело ответа в диагностические ошибки.
type CloudClient struct {
	BaseURL, NodeID, Secret string
	Client                  *http.Client
	IDs                     application.IDs
	Now                     func() time.Time
}

func (client CloudClient) Heartbeat(
	ctx context.Context,
	status domain.NodeStatus,
	modelVersion string,
	availableSlots int,
) error {
	return client.do(ctx, "/internal/v1/ai/nodes/heartbeat", map[string]any{
		"status": status, "modelVersion": modelVersion, "availableSlots": availableSlots,
	}, nil, http.StatusNoContent)
}

func (client CloudClient) Claim(ctx context.Context) (domain.Job, bool, error) {
	var response struct {
		ID, TenantID, JobType, EntityType, EntityID, Prompt string
		BaseConversationRevision                            int64
		AnalysisThroughMessageID                            string
		ModelVersion, PromptVersion, SchemaVersion          string
		LeaseUntil                                          time.Time
	}
	status, err := client.request(ctx, "/internal/v1/ai/jobs/claim", nil, &response)
	if err != nil {
		return domain.Job{}, false, err
	}
	if status == http.StatusNoContent {
		return domain.Job{}, false, nil
	}
	if status != http.StatusOK || response.ID == "" || response.Prompt == "" {
		return domain.Job{}, false, errors.New("AI cloud returned invalid claim response")
	}
	return domain.Job{
		ID: response.ID, TenantID: response.TenantID, JobType: response.JobType,
		EntityType: response.EntityType, ConversationID: response.EntityID,
		Prompt: response.Prompt, BaseConversationRevision: response.BaseConversationRevision,
		AnalysisThroughMessageID: response.AnalysisThroughMessageID,
		ModelVersion:             response.ModelVersion, PromptVersion: response.PromptVersion,
		SchemaVersion: response.SchemaVersion, Status: domain.JobLeased,
		ClaimedBy: client.NodeID, LeaseUntil: response.LeaseUntil,
	}, true, nil
}

func (client CloudClient) Started(ctx context.Context, jobID string) (string, error) {
	var response struct {
		RunID string `json:"runId"`
	}
	if err := client.do(ctx, "/internal/v1/ai/jobs/"+jobID+"/started", nil, &response, http.StatusOK); err != nil {
		return "", err
	}
	if response.RunID == "" {
		return "", errors.New("AI cloud returned empty run ID")
	}
	return response.RunID, nil
}

func (client CloudClient) Complete(ctx context.Context, jobID, runID, output string) error {
	return client.do(ctx, "/internal/v1/ai/jobs/"+jobID+"/complete", map[string]string{
		"runId": runID, "output": output,
	}, nil, http.StatusNoContent)
}

func (client CloudClient) Failed(ctx context.Context, jobID, runID, errorCode string) error {
	return client.do(ctx, "/internal/v1/ai/jobs/"+jobID+"/failed", map[string]string{
		"runId": runID, "errorCode": errorCode,
	}, nil, http.StatusNoContent)
}

func (client CloudClient) do(ctx context.Context, path string, body, output any, expectedStatus int) error {
	status, err := client.request(ctx, path, body, output)
	if err != nil {
		return err
	}
	if status != expectedStatus {
		return fmt.Errorf("AI cloud returned status %d", status)
	}
	return nil
}

func (client CloudClient) request(ctx context.Context, path string, body, output any) (int, error) {
	if client.NodeID == "" || client.Secret == "" || client.IDs == nil || client.Now == nil {
		return 0, errors.New("AI cloud credentials are required")
	}
	base, err := url.Parse(client.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return 0, errors.New("AI cloud URL is invalid")
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	var encoded []byte
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode AI cloud request: %w", err)
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), bytes.NewReader(encoded))
	if err != nil {
		return 0, fmt.Errorf("create AI cloud request: %w", err)
	}
	requestID, err := client.IDs.NewID()
	if err != nil {
		return 0, fmt.Errorf("generate AI cloud request ID: %w", err)
	}
	timestamp := client.Now().UTC().Format(time.RFC3339Nano)
	request.Header.Set("Authorization", "Bearer "+client.Secret)
	request.Header.Set(application.NodeIDHeader, client.NodeID)
	request.Header.Set(application.TimestampHeader, timestamp)
	request.Header.Set(application.RequestIDHeader, requestID)
	request.Header.Set(application.SignatureHeader, application.SignMachineRequest(
		client.Secret, request.Method, request.URL.EscapedPath(), timestamp, requestID, encoded,
	))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	httpClient := client.Client
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return 0, fmt.Errorf("call AI cloud: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode, nil
	}
	if response.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxCloudResponse))
		return response.StatusCode, fmt.Errorf("AI cloud returned status %d", response.StatusCode)
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxCloudResponse))
		return response.StatusCode, nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxCloudResponse))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return response.StatusCode, errors.New("AI cloud returned invalid JSON")
	}
	return response.StatusCode, nil
}
