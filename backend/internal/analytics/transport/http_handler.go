// Package transport публикует сводку аналитики через REST.
package transport

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"lidradar/backend/internal/analytics/application"
	"lidradar/backend/internal/analytics/domain"
	httpplatform "lidradar/backend/platform/http"
)

type PrincipalResolver interface {
	Principal(*http.Request) (actorID, tenantID string, ok bool)
}

type SummaryService interface {
	Summary(ctx context.Context, actor, tenant, fromDate, toDate string) (domain.Summary, error)
	Payments(ctx context.Context, actor, tenant, fromDate, toDate string, limit int, cursor string) (application.PaymentPage, error)
}

type Handler struct {
	service    SummaryService
	principals PrincipalResolver
}

func NewHandler(service SummaryService, principals PrincipalResolver) Handler {
	return Handler{service: service, principals: principals}
}

func (handler Handler) Router() http.Handler {
	router := chi.NewRouter()
	router.Get("/summary", handler.summary)
	router.Get("/payments", handler.payments)
	return router
}

// payments отдаёт последние подтверждённые оплаты окна с курсором.
func (handler Handler) payments(w http.ResponseWriter, r *http.Request) {
	if handler.principals == nil || handler.service == nil {
		httpplatform.WriteError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required", nil)
		return
	}
	actorID, tenantID, ok := handler.principals.Principal(r)
	if !ok || actorID == "" || tenantID == "" {
		httpplatform.WriteError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required", nil)
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			httpplatform.WriteError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid limit", nil)
			return
		}
		limit = parsed
	}
	query := r.URL.Query()
	page, err := handler.service.Payments(r.Context(), actorID, tenantID, query.Get("from"), query.Get("to"), limit, query.Get("cursor"))
	if err == nil {
		httpplatform.WriteJSON(w, http.StatusOK, page)
		return
	}
	writeServiceError(w, r, err)
}

func writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrForbidden):
		httpplatform.WriteError(w, r, http.StatusForbidden, "FORBIDDEN", "permission denied", nil)
	case errors.Is(err, application.ErrNotFound):
		httpplatform.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "organization not found", nil)
	case errors.Is(err, application.ErrInvalid):
		httpplatform.WriteError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid request", nil)
	default:
		httpplatform.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error", nil)
	}
}

func (handler Handler) summary(w http.ResponseWriter, r *http.Request) {
	if handler.principals == nil || handler.service == nil {
		httpplatform.WriteError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required", nil)
		return
	}
	actorID, tenantID, ok := handler.principals.Principal(r)
	if !ok || actorID == "" || tenantID == "" {
		httpplatform.WriteError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required", nil)
		return
	}
	summary, err := handler.service.Summary(r.Context(), actorID, tenantID, r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	switch {
	case err == nil:
		httpplatform.WriteJSON(w, http.StatusOK, summary)
	case errors.Is(err, application.ErrForbidden):
		httpplatform.WriteError(w, r, http.StatusForbidden, "FORBIDDEN", "permission denied", nil)
	case errors.Is(err, application.ErrNotFound):
		httpplatform.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "organization not found", nil)
	case errors.Is(err, application.ErrInvalid):
		httpplatform.WriteError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid period", nil)
	default:
		httpplatform.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error", nil)
	}
}
