// Package httpplatform provides the shared LidRadar HTTP server and transport conventions.
package httpplatform

import (
	"encoding/json"
	"net/http"
)

// ErrorEnvelope is the only public error shape emitted by the HTTP platform.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody describes a stable machine code and a safe user-facing message.
type ErrorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	TraceID string         `json:"traceId"`
}

// WriteJSON serializes a response without exposing implementation details.
func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// WriteError emits the shared versioned API error envelope.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string, details map[string]any) {
	WriteJSON(w, status, ErrorEnvelope{Error: ErrorBody{
		Code: code, Message: message, Details: details, TraceID: TraceID(r.Context()),
	}})
}
