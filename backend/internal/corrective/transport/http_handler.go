// Package transport открывает корректирующие команды через версионированный API.
package transport

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

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
	r := chi.NewRouter()
	h.RegisterRoutes(r, "")
	return r
}

func (h Handler) RegisterRoutes(router chi.Router, prefix string) {
	prefix = strings.TrimSuffix(prefix, "/")
	router.Post(prefix+"/risks/{riskID}/recommendation", h.recommendation)
	router.Post(prefix+"/risks/{riskID}/actions", h.action)
	router.Post(prefix+"/opportunities/{opportunityID}/outcomes", h.outcome)
}

func (h Handler) principal(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	if h.principals == nil {
		writeError(w, r, 401, "UNAUTHENTICATED", "требуется аутентификация")
		return "", "", false
	}
	a, t, ok := h.principals.Principal(r)
	if !ok || a == "" || t == "" {
		writeError(w, r, 401, "UNAUTHENTICATED", "требуется аутентификация")
		return "", "", false
	}
	return a, t, true
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		writeError(w, r, 400, "INVALID_ARGUMENT", "некорректный запрос")
		return false
	}
	if err := d.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, r, 400, "INVALID_ARGUMENT", "некорректный запрос")
		return false
	}
	return true
}
func (h Handler) recommendation(w http.ResponseWriter, r *http.Request) {
	a, t, ok := h.principal(w, r)
	if !ok {
		return
	}
	v, err := h.service.EnsureRecommendation(r.Context(), a, t, chi.URLParam(r, "riskID"))
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
	v, created, err := h.service.AddAction(r.Context(), a, t, chi.URLParam(r, "riskID"), strings.TrimSpace(r.Header.Get("Idempotency-Key")), body.Type, body.Note)
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
	v, created, err := h.service.AddOutcome(r.Context(), a, t, chi.URLParam(r, "opportunityID"), strings.TrimSpace(r.Header.Get("Idempotency-Key")), body.Status, body.Note)
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
		writeError(w, r, 403, "FORBIDDEN", "недостаточно прав")
	case errors.Is(err, application.ErrNotFound):
		writeError(w, r, 404, "NOT_FOUND", "объект не найден")
	case errors.Is(err, application.ErrConflict):
		writeError(w, r, 409, "IDEMPOTENCY_CONFLICT", "ключ идемпотентности уже использован для другого запроса")
	case errors.Is(err, application.ErrInvalid), errors.Is(err, domain.ErrInvalid):
		writeError(w, r, 400, "INVALID_ARGUMENT", "некорректный запрос")
	default:
		writeError(w, r, 500, "INTERNAL", "внутренняя ошибка")
	}
	return true
}
