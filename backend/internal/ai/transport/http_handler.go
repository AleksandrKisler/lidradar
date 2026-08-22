// Package transport exposes the machine-only AI pull API.
package transport

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"lidradar/backend/internal/ai/application"
)

type Handler struct{ Service application.Service }

// ServeHTTP serves /internal/v1/ai/nodes/heartbeat and the claim/status API.
// Node credentials are never accepted in query strings or response bodies.
func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, secret := r.Header.Get("X-AI-Node-ID"), r.Header.Get("Authorization")
	secret = strings.TrimPrefix(secret, "Bearer ")
	path := strings.TrimPrefix(r.URL.Path, "/internal/v1/ai/")
	var result any
	var err error
	switch {
	case r.Method == http.MethodPost && path == "nodes/heartbeat":
		err = h.Service.Heartbeat(r.Context(), id, secret)
	case r.Method == http.MethodPost && path == "jobs/claim":
		var ok bool
		result, ok, err = h.Service.Claim(r.Context(), id, secret)
		if err == nil && !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/started"):
		result, err = h.Service.Started(r.Context(), id, secret, jobID(path, "/started"))
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/complete"):
		var in struct {
			RunID, Output, AnalysisThroughMessageID string
			CurrentRevision                         int64
		}
		if decodeErr := json.NewDecoder(r.Body).Decode(&in); decodeErr != nil {
			err = application.ErrInvalid
		} else {
			result, err = h.Service.Complete(r.Context(), id, secret, jobID(path, "/complete"), in.RunID, in.Output, in.CurrentRevision, in.AnalysisThroughMessageID)
		}
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/failed"):
		var in struct{ RunID, Error string }
		if decodeErr := json.NewDecoder(r.Body).Decode(&in); decodeErr != nil {
			err = application.ErrInvalid
		} else {
			result, err = h.Service.Failed(r.Context(), id, secret, jobID(path, "/failed"), in.RunID, in.Error)
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if result == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
func jobID(path, suffix string) string {
	return strings.TrimSuffix(strings.TrimPrefix(path, "jobs/"), suffix)
}
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, application.ErrUnauthorized) {
		status = http.StatusUnauthorized
	} else if errors.Is(err, application.ErrInvalid) {
		status = http.StatusBadRequest
	} else if errors.Is(err, application.ErrNotFound) {
		status = http.StatusNotFound
	} else if errors.Is(err, application.ErrLeaseLost) {
		status = http.StatusConflict
	}
	http.Error(w, http.StatusText(status), status)
}
