// Package transport публикует управление личной Telegram-привязкой.
package transport

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"lidradar/backend/internal/notification/application"
	httpplatform "lidradar/backend/platform/http"
)

type UserResolver interface {
	User(*http.Request) (string, bool, error)
}

type LinkService interface {
	Issue(context.Context, string, string) (application.IssuedLinkToken, error)
	Status(context.Context, string, string) (application.TelegramLink, bool, error)
	Disable(context.Context, string, string) error
}

type Handler struct {
	service LinkService
	users   UserResolver
}

func NewHandler(service LinkService, users UserResolver) Handler {
	return Handler{service: service, users: users}
}

func (handler Handler) Router() http.Handler {
	router := chi.NewRouter()
	router.Post("/telegram-link-token", handler.issue)
	router.Get("/telegram-link", handler.status)
	router.Delete("/telegram-link", handler.disable)
	return router
}

func (handler Handler) issue(writer http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	issued, err := handler.service.Issue(request.Context(), actorID, tenantID)
	if handleError(writer, request, err) {
		return
	}
	httpplatform.WriteJSON(writer, http.StatusCreated, map[string]any{
		"startUrl": issued.StartURL, "expiresAt": issued.ExpiresAt,
	})
}

func (handler Handler) status(writer http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	link, found, err := handler.service.Status(request.Context(), actorID, tenantID)
	if handleError(writer, request, err) {
		return
	}
	response := map[string]any{"linked": found}
	if found {
		response["linkedAt"] = link.LinkedAt
	}
	httpplatform.WriteJSON(writer, http.StatusOK, response)
}

func (handler Handler) disable(writer http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	if handleError(writer, request, handler.service.Disable(request.Context(), actorID, tenantID)) {
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (handler Handler) principal(writer http.ResponseWriter, request *http.Request) (string, string, bool) {
	if handler.users == nil {
		httpplatform.WriteError(writer, request, http.StatusUnauthorized, "UNAUTHENTICATED", "Требуется вход", nil)
		return "", "", false
	}
	actorID, found, err := handler.users.User(request)
	if err != nil {
		httpplatform.WriteError(writer, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Внутренняя ошибка", nil)
		return "", "", false
	}
	if !found || actorID == "" {
		httpplatform.WriteError(writer, request, http.StatusUnauthorized, "UNAUTHENTICATED", "Требуется вход", nil)
		return "", "", false
	}
	tenantID := strings.TrimSpace(request.Header.Get("X-Tenant-ID"))
	if tenantID == "" {
		httpplatform.WriteError(writer, request, http.StatusBadRequest, "TENANT_REQUIRED", "Требуется X-Tenant-ID", nil)
		return "", "", false
	}
	return actorID, tenantID, true
}

func handleError(writer http.ResponseWriter, request *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, application.ErrForbidden):
		httpplatform.WriteError(writer, request, http.StatusForbidden, "FORBIDDEN", "Нет разрешения", nil)
	case errors.Is(err, application.ErrInvalidLinkToken):
		httpplatform.WriteError(writer, request, http.StatusBadRequest, "INVALID_ARGUMENT", "Некорректный запрос", nil)
	default:
		httpplatform.WriteError(writer, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Внутренняя ошибка", nil)
	}
	return true
}
