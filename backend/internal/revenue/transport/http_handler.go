// Package transport exposes revenue operations through the versioned API.
package transport

import (
	"encoding/json"
	"errors"
	"lidradar/backend/internal/revenue/application"
	"lidradar/backend/internal/revenue/domain"
	"net/http"
	"strings"
)

type PrincipalResolver interface {
	Principal(*http.Request) (string, string, bool)
}
type Handler struct {
	service    application.Service
	principals PrincipalResolver
}

func NewHandler(s application.Service, p PrincipalResolver) Handler { return Handler{s, p} }
func (h Handler) Router() http.Handler {
	r := http.NewServeMux()
	r.HandleFunc("POST /api/v1/opportunities/{opportunityID}/revenue", h.confirm)
	r.HandleFunc("GET /api/v1/revenue/confirmed-recovered", h.total)
	return r
}
func (h Handler) principal(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	if h.principals == nil {
		writeError(w, 401, "UNAUTHENTICATED", "authentication required")
		return "", "", false
	}
	a, t, ok := h.principals.Principal(r)
	if !ok || a == "" || t == "" {
		writeError(w, 401, "UNAUTHENTICATED", "authentication required")
		return "", "", false
	}
	return a, t, true
}
func (h Handler) confirm(w http.ResponseWriter, r *http.Request) {
	a, t, ok := h.principal(w, r)
	if !ok {
		return
	}
	var b struct {
		Amount          string                 `json:"amount"`
		Currency        string                 `json:"currency"`
		AttributionType domain.AttributionType `json:"attributionType"`
		RiskID          string                 `json:"riskId"`
		ActionID        string                 `json:"actionId"`
		OutcomeID       string                 `json:"outcomeId"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if d.Decode(&b) != nil {
		writeError(w, 400, "INVALID_ARGUMENT", "invalid request")
		return
	}
	v, created, err := h.service.Confirm(r.Context(), a, t, r.PathValue("opportunityID"), strings.TrimSpace(r.Header.Get("Idempotency-Key")), application.ConfirmCommand{
		Amount: b.Amount, Currency: b.Currency, Type: b.AttributionType,
		RiskID: b.RiskID, ActionID: b.ActionID, OutcomeID: b.OutcomeID,
	})
	if handle(w, err) {
		return
	}
	if created {
		writeJSON(w, 201, v)
	} else {
		writeJSON(w, 200, v)
	}
}
func (h Handler) total(w http.ResponseWriter, r *http.Request) {
	a, t, ok := h.principal(w, r)
	if !ok {
		return
	}
	v, err := h.service.ConfirmedRecovered(r.Context(), a, t, r.URL.Query().Get("currency"))
	if handle(w, err) {
		return
	}
	writeJSON(w, 200, map[string]string{"amount": v.String(), "currency": strings.ToUpper(r.URL.Query().Get("currency"))})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func handle(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, application.ErrForbidden):
		writeError(w, 403, "FORBIDDEN", "permission denied")
	case errors.Is(err, application.ErrNotFound):
		writeError(w, 404, "NOT_FOUND", "resource not found")
	case errors.Is(err, application.ErrConflict):
		writeError(w, 409, "IDEMPOTENCY_CONFLICT", "idempotency key was used for another request")
	case errors.Is(err, application.ErrInvalid), errors.Is(err, domain.ErrInvalid):
		writeError(w, 400, "INVALID_ARGUMENT", "invalid request")
	default:
		writeError(w, 500, "INTERNAL", "internal error")
	}
	return true
}
