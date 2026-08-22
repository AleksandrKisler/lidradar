// Package transport adapts HTTP requests to Risk application operations.
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"lidradar/backend/internal/risk/application"
	"lidradar/backend/internal/risk/domain"
)

// PrincipalResolver is implemented by the identity transport. Authentication
// details remain outside the Risk module.
type PrincipalResolver interface {
	Principal(*http.Request) (actorID, tenantID string, ok bool)
}

type Handler struct {
	radar      application.Radar
	principals PrincipalResolver
	events     *Hub
}

func NewHandler(radar application.Radar, principals PrincipalResolver, events *Hub) Handler {
	return Handler{radar: radar, principals: principals, events: events}
}

func (h Handler) Router() http.Handler {
	r := http.NewServeMux()
	r.HandleFunc("GET /api/v1/radar", h.summary)
	r.HandleFunc("GET /api/v1/risks", h.list)
	r.HandleFunc("GET /api/v1/risks/{riskID}", h.detail)
	r.HandleFunc("POST /api/v1/risks/{riskID}/acknowledge", h.acknowledge)
	r.HandleFunc("POST /api/v1/risks/{riskID}/resolve", h.resolve)
	r.HandleFunc("GET /api/v1/events", h.stream)
	return r
}

func (h Handler) principal(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	if h.principals == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
		return "", "", false
	}
	a, t, ok := h.principals.Principal(r)
	if !ok || a == "" || t == "" {
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
		return "", "", false
	}
	return a, t, true
}
func (h Handler) list(w http.ResponseWriter, r *http.Request) {
	a, t, ok := h.principal(w, r)
	if !ok {
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil {
			writeError(w, 400, "INVALID_ARGUMENT", "invalid limit")
			return
		}
	}
	page, err := h.radar.List(r.Context(), a, t, application.ListQuery{Status: domain.Status(r.URL.Query().Get("status")), Limit: limit, After: r.URL.Query().Get("cursor")})
	if handleError(w, err) {
		return
	}
	writeJSON(w, 200, map[string]any{"items": page.Items, "nextCursor": emptyNil(page.NextCursor)})
}
func (h Handler) detail(w http.ResponseWriter, r *http.Request) {
	a, t, ok := h.principal(w, r)
	if !ok {
		return
	}
	d, err := h.radar.Get(r.Context(), a, t, r.PathValue("riskID"))
	if handleError(w, err) {
		return
	}
	writeJSON(w, 200, d)
}
func (h Handler) summary(w http.ResponseWriter, r *http.Request) {
	a, t, ok := h.principal(w, r)
	if !ok {
		return
	}
	s, err := h.radar.Summary(r.Context(), a, t)
	if handleError(w, err) {
		return
	}
	writeJSON(w, 200, map[string]any{"openRisks": s.OpenRisks, "criticalRisks": s.CriticalRisks, "potentialRevenue": s.PotentialRevenue, "confirmedRecoveredRevenue": s.ConfirmedRecoveredRevenue})
}
func (h Handler) acknowledge(w http.ResponseWriter, r *http.Request) {
	h.command(w, r, h.radar.Acknowledge)
}
func (h Handler) resolve(w http.ResponseWriter, r *http.Request) { h.command(w, r, h.radar.Resolve) }
func (h Handler) command(w http.ResponseWriter, r *http.Request, fn func(context.Context, string, string, string) (domain.Risk, error)) {
	a, t, ok := h.principal(w, r)
	if !ok {
		return
	}
	risk, err := fn(r.Context(), a, t, r.PathValue("riskID"))
	if handleError(w, err) {
		return
	}
	writeJSON(w, 200, risk)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func handleError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, application.ErrForbidden):
		writeError(w, 403, "FORBIDDEN", "permission denied")
	case errors.Is(err, application.ErrNotFound):
		writeError(w, 404, "NOT_FOUND", "risk not found")
	case errors.Is(err, application.ErrInvalidCommand):
		writeError(w, 400, "INVALID_ARGUMENT", "invalid request")
	default:
		writeError(w, 500, "INTERNAL", "internal error")
	}
	return true
}
func emptyNil(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
