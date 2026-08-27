// Package transport exposes the machine-only AI pull API.
package transport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
	httpplatform "lidradar/backend/platform/http"
	"lidradar/backend/platform/ids"
)

const (
	NodeIDHeader    = application.NodeIDHeader
	TimestampHeader = application.TimestampHeader
	RequestIDHeader = application.RequestIDHeader
	SignatureHeader = application.SignatureHeader
	maxRequestBody  = 1 << 20
)

type Handler struct{ Service application.Service }

type heartbeatRequest struct {
	Status         domain.NodeStatus `json:"status"`
	ModelVersion   string            `json:"modelVersion"`
	AvailableSlots int               `json:"availableSlots"`
}

type completeRequest struct {
	RunID  string `json:"runId"`
	Output string `json:"output"`
}

type failedRequest struct {
	RunID     string `json:"runId"`
	ErrorCode string `json:"errorCode"`
}

type claimResponse struct {
	ID                       string    `json:"id"`
	TenantID                 string    `json:"tenantId"`
	JobType                  string    `json:"jobType"`
	EntityType               string    `json:"entityType"`
	EntityID                 string    `json:"entityId"`
	Prompt                   string    `json:"prompt"`
	BaseConversationRevision int64     `json:"baseConversationRevision"`
	AnalysisThroughMessageID string    `json:"analysisThroughMessageId"`
	ModelVersion             string    `json:"modelVersion"`
	PromptVersion            string    `json:"promptVersion"`
	SchemaVersion            string    `json:"schemaVersion"`
	LeaseUntil               time.Time `json:"leaseUntil"`
}

type startedResponse struct {
	RunID string `json:"runId"`
}

// ServeHTTP serves /internal/v1/ai/nodes/heartbeat and the claim/status API.
// Credentials, signatures and bodies are never reflected into errors.
func (handler Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if !application.IsMachineMethod(request.Method) || !knownPath(request.URL.Path) {
		httpplatform.WriteError(writer, request, http.StatusNotFound, "ROUTE_NOT_FOUND", "Route not found", nil)
		return
	}
	body, err := readBody(writer, request)
	if err != nil {
		httpplatform.WriteError(writer, request, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request", nil)
		return
	}
	timestamp, err := time.Parse(time.RFC3339Nano, request.Header.Get(TimestampHeader))
	if err != nil || !ids.Valid(request.Header.Get(NodeIDHeader)) || !ids.Valid(request.Header.Get(RequestIDHeader)) {
		writeError(writer, request, application.ErrUnauthorized)
		return
	}
	secret := application.BearerSecret(request.Header.Get("Authorization"))
	if err := handler.Service.AuthenticateMachineRequest(request.Context(), application.MachineRequest{
		NodeID: request.Header.Get(NodeIDHeader), Secret: secret,
		RequestID: request.Header.Get(RequestIDHeader), Timestamp: timestamp,
		Method: request.Method, Path: request.URL.EscapedPath(),
		Signature: request.Header.Get(SignatureHeader), Body: body,
	}); err != nil {
		writeError(writer, request, err)
		return
	}

	id := request.Header.Get(NodeIDHeader)
	path := strings.TrimPrefix(request.URL.Path, "/internal/v1/ai/")
	switch {
	case path == "nodes/heartbeat":
		var input heartbeatRequest
		if decodeStrict(body, &input) != nil {
			writeError(writer, request, application.ErrInvalid)
			return
		}
		err = handler.Service.Heartbeat(request.Context(), id, secret, application.HeartbeatCommand{
			Status: input.Status, ModelVersion: input.ModelVersion, AvailableSlots: input.AvailableSlots,
		})
		if err == nil {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
	case path == "jobs/claim":
		if len(bytes.TrimSpace(body)) != 0 {
			writeError(writer, request, application.ErrInvalid)
			return
		}
		var job domain.Job
		var found bool
		job, found, err = handler.Service.Claim(request.Context(), id, secret)
		if err == nil && !found {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		if err == nil {
			httpplatform.WriteJSON(writer, http.StatusOK, claimResponse{
				ID: job.ID, TenantID: job.TenantID, JobType: job.JobType,
				EntityType: job.EntityType, EntityID: job.ConversationID, Prompt: job.Prompt,
				BaseConversationRevision: job.BaseConversationRevision,
				AnalysisThroughMessageID: job.AnalysisThroughMessageID,
				ModelVersion:             job.ModelVersion, PromptVersion: job.PromptVersion,
				SchemaVersion: job.SchemaVersion, LeaseUntil: job.LeaseUntil,
			})
			return
		}
	case strings.HasSuffix(path, "/started"):
		if len(bytes.TrimSpace(body)) != 0 {
			writeError(writer, request, application.ErrInvalid)
			return
		}
		var run domain.Run
		run, err = handler.Service.Started(request.Context(), id, secret, jobID(path, "/started"))
		if err == nil {
			httpplatform.WriteJSON(writer, http.StatusOK, startedResponse{RunID: run.ID})
			return
		}
	case strings.HasSuffix(path, "/complete"):
		var input completeRequest
		if decodeStrict(body, &input) != nil {
			writeError(writer, request, application.ErrInvalid)
			return
		}
		_, err = handler.Service.Complete(
			request.Context(), id, secret, jobID(path, "/complete"), input.RunID, input.Output,
		)
		if err == nil {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
	case strings.HasSuffix(path, "/failed"):
		var input failedRequest
		if decodeStrict(body, &input) != nil {
			writeError(writer, request, application.ErrInvalid)
			return
		}
		_, err = handler.Service.Failed(
			request.Context(), id, secret, jobID(path, "/failed"), input.RunID, input.ErrorCode,
		)
		if err == nil {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
	}
	writeError(writer, request, err)
}

func knownPath(path string) bool {
	trimmed := strings.TrimPrefix(path, "/internal/v1/ai/")
	return trimmed == "nodes/heartbeat" || trimmed == "jobs/claim" ||
		(strings.HasPrefix(trimmed, "jobs/") &&
			(strings.HasSuffix(trimmed, "/started") || strings.HasSuffix(trimmed, "/complete") || strings.HasSuffix(trimmed, "/failed")))
}

func readBody(writer http.ResponseWriter, request *http.Request) ([]byte, error) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBody)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func decodeStrict(body []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return application.ErrInvalid
	}
	return nil
}

func jobID(path, suffix string) string {
	value := strings.TrimSuffix(strings.TrimPrefix(path, "jobs/"), suffix)
	if !ids.Valid(value) {
		return ""
	}
	return value
}

func writeError(writer http.ResponseWriter, request *http.Request, err error) {
	status := http.StatusInternalServerError
	code := "INTERNAL"
	message := "Internal server error"
	if errors.Is(err, application.ErrUnauthorized) || errors.Is(err, application.ErrReplay) {
		status = http.StatusUnauthorized
		code, message = "UNAUTHENTICATED", "Authentication required"
	} else if errors.Is(err, application.ErrInvalid) {
		status = http.StatusBadRequest
		code, message = "INVALID_ARGUMENT", "Invalid request"
	} else if errors.Is(err, application.ErrNotFound) {
		status = http.StatusNotFound
		code, message = "NOT_FOUND", "Resource not found"
	} else if errors.Is(err, application.ErrLeaseLost) {
		status = http.StatusConflict
		code, message = "LEASE_LOST", "AI job lease was lost"
	} else if errors.Is(err, application.ErrConflict) {
		status = http.StatusConflict
		code, message = "CONFLICT", "AI resource conflicts with existing state"
	}
	httpplatform.WriteError(writer, request, status, code, message, nil)
}
