// Package transport публикует чтение и ручное изменение коммерческих возможностей.
package transport

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"lidradar/backend/internal/opportunity/application"
	"lidradar/backend/internal/opportunity/domain"
	httpplatform "lidradar/backend/platform/http"
)

type UserResolver interface {
	User(*http.Request) (userID string, ok bool, err error)
}

type OpportunityService interface {
	Detail(context.Context, string, string, string) (domain.Detail, error)
	ChangeStage(context.Context, string, string, string, domain.Stage) (domain.Opportunity, bool, error)
}

type Handler struct {
	service OpportunityService
	users   UserResolver
}

func NewHandler(service OpportunityService, users UserResolver) Handler {
	return Handler{service: service, users: users}
}

func (handler Handler) Router() http.Handler {
	router := chi.NewRouter()
	router.Get("/{opportunityID}", handler.detail)
	router.Patch("/{opportunityID}", handler.changeStage)
	return router
}

type stageRequest struct {
	Stage domain.Stage `json:"stage"`
}

func (handler Handler) detail(w http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(w, request)
	if !ok {
		return
	}
	detail, err := handler.service.Detail(
		request.Context(), actorID, tenantID, chi.URLParam(request, "opportunityID"),
	)
	if handleError(w, request, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusOK, detail)
}

func (handler Handler) changeStage(w http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(w, request)
	if !ok {
		return
	}
	var body stageRequest
	if httpplatform.DecodeJSON(w, request, &body) != nil || !body.Stage.Valid() {
		writeError(w, request, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request")
		return
	}
	opportunity, _, err := handler.service.ChangeStage(
		request.Context(), actorID, tenantID, chi.URLParam(request, "opportunityID"), body.Stage,
	)
	if handleError(w, request, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusOK, opportunity)
}

func (handler Handler) principal(w http.ResponseWriter, request *http.Request) (string, string, bool) {
	if handler.users == nil {
		writeError(w, request, http.StatusUnauthorized, "UNAUTHENTICATED", "Authentication required")
		return "", "", false
	}
	actorID, ok, err := handler.users.User(request)
	if err != nil {
		writeError(w, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error")
		return "", "", false
	}
	if !ok || actorID == "" {
		writeError(w, request, http.StatusUnauthorized, "UNAUTHENTICATED", "Authentication required")
		return "", "", false
	}
	tenantID := strings.TrimSpace(request.Header.Get("X-Tenant-ID"))
	if tenantID == "" {
		writeError(w, request, http.StatusBadRequest, "TENANT_REQUIRED", "X-Tenant-ID is required")
		return "", "", false
	}
	return actorID, tenantID, true
}

func handleError(w http.ResponseWriter, request *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, application.ErrInvalid):
		writeError(w, request, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request")
	case errors.Is(err, application.ErrForbidden):
		writeError(w, request, http.StatusForbidden, "FORBIDDEN", "Permission denied")
	case errors.Is(err, application.ErrNotFound):
		writeError(w, request, http.StatusNotFound, "NOT_FOUND", "Resource not found")
	case errors.Is(err, application.ErrConflict), errors.Is(err, application.ErrInvalidTransition):
		writeError(w, request, http.StatusConflict, "INVALID_STAGE_TRANSITION", "Stage transition is not allowed")
	default:
		writeError(w, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error")
	}
	return true
}

func writeError(w http.ResponseWriter, request *http.Request, status int, code, message string) {
	httpplatform.WriteError(w, request, status, code, message, nil)
}
