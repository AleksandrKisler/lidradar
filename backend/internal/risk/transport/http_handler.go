// Package transport adapts HTTP requests to Risk application operations.
package transport

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"lidradar/backend/internal/risk/application"
	"lidradar/backend/internal/risk/domain"
	httpplatform "lidradar/backend/platform/http"
)

// PrincipalResolver is implemented by the identity transport. Authentication
// details remain outside the Risk module.
type PrincipalResolver interface {
	Principal(*http.Request) (actorID, tenantID string, ok bool)
}

// FeedbackService записывает вердикты по рискам и считает точность (этап 21).
type FeedbackService interface {
	Record(context.Context, string, string, string, application.FeedbackCommand) (domain.Feedback, error)
	Precision(context.Context, string, string, time.Time, time.Time) (application.PrecisionReport, error)
}

type Handler struct {
	radar      application.Radar
	principals PrincipalResolver
	events     *Hub
	feedback   FeedbackService
}

func NewHandler(radar application.Radar, principals PrincipalResolver, events *Hub) Handler {
	return Handler{radar: radar, principals: principals, events: events}
}

// WithFeedback подключает обратную связь и метрики точности.
func (h Handler) WithFeedback(feedback FeedbackService) Handler {
	h.feedback = feedback
	return h
}

func (h Handler) Router() http.Handler {
	r := chi.NewRouter()
	h.RegisterRoutes(r, "")
	return r
}

// RegisterRoutes добавляет маршруты Radar к общему корневому маршрутизатору.
// Это позволяет сосуществовать с уже смонтированными маршрутами /api/v1.
func (h Handler) RegisterRoutes(router chi.Router, prefix string) {
	prefix = strings.TrimSuffix(prefix, "/")
	router.Get(prefix+"/radar", h.summary)
	router.Get(prefix+"/risks", h.list)
	router.Get(prefix+"/risks/{riskID}", h.detail)
	router.Post(prefix+"/risks/{riskID}/acknowledge", h.acknowledge)
	router.Post(prefix+"/risks/{riskID}/resolve", h.resolve)
	router.Get(prefix+"/events", h.stream)
	if h.feedback != nil {
		router.Get(prefix+"/risks/precision", h.precision)
		router.Post(prefix+"/risks/{riskID}/feedback", h.recordFeedback)
	}
}

func (h Handler) recordFeedback(w http.ResponseWriter, r *http.Request) {
	a, t, ok := h.principal(w, r)
	if !ok {
		return
	}
	var body struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
		Note    string `json:"note"`
	}
	if httpplatform.DecodeJSON(w, r, &body) != nil {
		writeError(w, r, 400, "INVALID_ARGUMENT", "invalid request")
		return
	}
	feedback, err := h.feedback.Record(r.Context(), a, t, chi.URLParam(r, "riskID"), application.FeedbackCommand{
		Verdict: body.Verdict, Reason: body.Reason, Note: body.Note,
	})
	if handleError(w, r, err) {
		return
	}
	writeJSON(w, 201, feedback)
}

// precision читает границы окна обнаружения из from/to (RFC 3339).
func (h Handler) precision(w http.ResponseWriter, r *http.Request) {
	a, t, ok := h.principal(w, r)
	if !ok {
		return
	}
	var from, to time.Time
	var err error
	if raw := r.URL.Query().Get("from"); raw != "" {
		if from, err = time.Parse(time.RFC3339, raw); err != nil {
			writeError(w, r, 400, "INVALID_ARGUMENT", "invalid from")
			return
		}
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		if to, err = time.Parse(time.RFC3339, raw); err != nil {
			writeError(w, r, 400, "INVALID_ARGUMENT", "invalid to")
			return
		}
	}
	report, err := h.feedback.Precision(r.Context(), a, t, from, to)
	if handleError(w, r, err) {
		return
	}
	writeJSON(w, 200, report)
}

func (h Handler) principal(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	if h.principals == nil {
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
		return "", "", false
	}
	a, t, ok := h.principals.Principal(r)
	if !ok || a == "" || t == "" {
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
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
			writeError(w, r, 400, "INVALID_ARGUMENT", "invalid limit")
			return
		}
	}
	filters := filtersFrom(r)
	page, err := h.radar.List(r.Context(), a, t, application.ListQuery{
		Filters: filters, Status: domain.Status(r.URL.Query().Get("status")),
		Limit: limit, After: r.URL.Query().Get("cursor"),
	})
	if handleError(w, r, err) {
		return
	}
	writeJSON(w, 200, map[string]any{"items": page.Items, "nextCursor": emptyNil(page.NextCursor)})
}
func (h Handler) detail(w http.ResponseWriter, r *http.Request) {
	a, t, ok := h.principal(w, r)
	if !ok {
		return
	}
	d, err := h.radar.Get(r.Context(), a, t, chi.URLParam(r, "riskID"))
	if handleError(w, r, err) {
		return
	}
	writeJSON(w, 200, d)
}
func (h Handler) summary(w http.ResponseWriter, r *http.Request) {
	a, t, ok := h.principal(w, r)
	if !ok {
		return
	}
	s, err := h.radar.Summary(r.Context(), a, t, filtersFrom(r))
	if handleError(w, r, err) {
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
	risk, err := fn(r.Context(), a, t, chi.URLParam(r, "riskID"))
	if handleError(w, r, err) {
		return
	}
	writeJSON(w, 200, risk)
}

func filtersFrom(r *http.Request) application.Filters {
	return application.Filters{
		LocationID: r.URL.Query().Get("locationId"),
		Severity:   domain.Severity(r.URL.Query().Get("severity")),
		RiskType:   domain.Type(r.URL.Query().Get("riskType")),
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	httpplatform.WriteJSON(w, status, v)
}
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	httpplatform.WriteError(w, r, status, code, message, nil)
}
func handleError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, application.ErrForbidden):
		writeError(w, r, 403, "FORBIDDEN", "permission denied")
	case errors.Is(err, application.ErrNotFound):
		writeError(w, r, 404, "NOT_FOUND", "risk not found")
	case errors.Is(err, application.ErrInvalidCommand):
		writeError(w, r, 400, "INVALID_ARGUMENT", "invalid request")
	default:
		writeError(w, r, 500, "INTERNAL", "internal error")
	}
	return true
}
func emptyNil(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
