// Package transport exposes corrective commands through the versioned API.
package transport

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"lidradar/backend/internal/corrective/application"
	"lidradar/backend/internal/corrective/domain"
	httpplatform "lidradar/backend/platform/http"
)

type PrincipalResolver interface {
	Principal(*http.Request) (actorID, tenantID string, ok bool)
}
type Handler struct {
	service    application.Service
	principals PrincipalResolver
}

func NewHandler(service application.Service, principals PrincipalResolver) Handler {
	return Handler{service, principals}
}
func (h Handler) Router() http.Handler {
	r := http.NewServeMux()
	r.HandleFunc("POST /api/v1/risks/{riskID}/recommendation", h.recommendation)
	r.HandleFunc("POST /api/v1/risks/{riskID}/actions", h.action)
	r.HandleFunc("POST /api/v1/opportunities/{opportunityID}/outcomes", h.outcome)
	return r
}
func (h Handler) principal(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	if h.principals == nil {
		writeError(w, r, 401, "UNAUTHENTICATED", "authentication required")
		return "", "", false
	}
	a, t, ok := h.principals.Principal(r)
	if !ok || a == "" || t == "" {
		writeError(w, r, 401, "UNAUTHENTICATED", "authentication required")
		return "", "", false
	}
	return a, t, true
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		writeError(w, r, 400, "INVALID_ARGUMENT", "invalid request")
		return false
	}
	return true
}
func (h Handler) recommendation(w http.ResponseWriter, r *http.Request) {
	a, t, ok := h.principal(w, r)
	if !ok {
		return
	}
	var body struct {
		RiskType string `json:"riskType"`
	}
	if !decode(w, r, &body) {
		return
	}
	v, err := h.service.EnsureRecommendation(r.Context(), a, t, r.PathValue("riskID"), body.RiskType)
	if handle(w, r, err) {
		return
	}
	writeJSON(w, 200, v)
}
func (h Handler) action(w http.ResponseWriter, r *http.Request) {
	a, t, ok := h.principal(w, r)
	if !ok {
		return
	}
	var body struct {
		Type domain.ActionType `json:"type"`
		Note string            `json:"note"`
	}
	if !decode(w, r, &body) {
		return
	}
	v, created, err := h.service.AddAction(r.Context(), a, t, r.PathValue("riskID"), strings.TrimSpace(r.Header.Get("Idempotency-Key")), body.Type, body.Note)
	if handle(w, r, err) {
		return
	}
	if created {
		writeJSON(w, 201, v)
	} else {
		writeJSON(w, 200, v)
	}
}
func (h Handler) outcome(w http.ResponseWriter, r *http.Request) {
	a, t, ok := h.principal(w, r)
	if !ok {
		return
	}
	var body struct {
		Status domain.OutcomeStatus `json:"status"`
		Note   string               `json:"note"`
	}
	if !decode(w, r, &body) {
		return
	}
	v, created, err := h.service.AddOutcome(r.Context(), a, t, r.PathValue("opportunityID"), strings.TrimSpace(r.Header.Get("Idempotency-Key")), body.Status, body.Note)
	if handle(w, r, err) {
		return
	}
	if created {
		writeJSON(w, 201, v)
	} else {
		writeJSON(w, 200, v)
	}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	httpplatform.WriteJSON(w, status, v)
}
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	httpplatform.WriteError(w, r, status, code, message, nil)
}
func handle(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, application.ErrForbidden):
		writeError(w, r, 403, "FORBIDDEN", "permission denied")
	case errors.Is(err, application.ErrNotFound):
		writeError(w, r, 404, "NOT_FOUND", "resource not found")
	case errors.Is(err, application.ErrConflict):
		writeError(w, r, 409, "IDEMPOTENCY_CONFLICT", "idempotency key was used for another request")
	case errors.Is(err, application.ErrInvalid), errors.Is(err, domain.ErrInvalid):
		writeError(w, r, 400, "INVALID_ARGUMENT", "invalid request")
	default:
		writeError(w, r, 500, "INTERNAL", "internal error")
	}
	return true
}
