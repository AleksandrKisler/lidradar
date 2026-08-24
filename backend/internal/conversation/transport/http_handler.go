// Package transport связывает HTTP API с модулем переписок.
package transport

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"lidradar/backend/internal/conversation/application"
	"lidradar/backend/internal/conversation/domain"
	httpplatform "lidradar/backend/platform/http"
)

// UserResolver получает пользователя из серверного сеанса.
type UserResolver interface {
	User(*http.Request) (userID string, ok bool, err error)
}

// ConversationService описывает доступные HTTP-обработчику операции чтения.
type ConversationService interface {
	List(context.Context, string, string, int, string) (application.ConversationPage, error)
	Detail(context.Context, string, string, string) (domain.ConversationDetail, error)
	Messages(context.Context, string, string, string, int, string) (application.MessagePage, error)
}

// Handler публикует HTTP API чтения переписок.
type Handler struct {
	service ConversationService
	users   UserResolver
}

// NewHandler создаёт обработчик с прикладным сервисом и проверкой сеанса.
func NewHandler(service ConversationService, users UserResolver) Handler {
	return Handler{service: service, users: users}
}

// Router возвращает маршруты списка, деталей и сообщений.
func (handler Handler) Router() http.Handler {
	router := chi.NewRouter()
	router.Get("/", handler.list)
	router.Get("/{conversationID}", handler.detail)
	router.Get("/{conversationID}/messages", handler.messages)
	return router
}

func (handler Handler) list(writer http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	limit, ok := pageLimit(writer, request)
	if !ok {
		return
	}
	page, err := handler.service.List(request.Context(), actorID, tenantID, limit, request.URL.Query().Get("cursor"))
	if handleError(writer, request, err) {
		return
	}
	httpplatform.WriteJSON(writer, http.StatusOK, page)
}

func (handler Handler) detail(writer http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	detail, err := handler.service.Detail(
		request.Context(), actorID, tenantID, chi.URLParam(request, "conversationID"),
	)
	if handleError(writer, request, err) {
		return
	}
	httpplatform.WriteJSON(writer, http.StatusOK, detail)
}

func (handler Handler) messages(writer http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	limit, ok := pageLimit(writer, request)
	if !ok {
		return
	}
	page, err := handler.service.Messages(
		request.Context(), actorID, tenantID, chi.URLParam(request, "conversationID"),
		limit, request.URL.Query().Get("cursor"),
	)
	if handleError(writer, request, err) {
		return
	}
	httpplatform.WriteJSON(writer, http.StatusOK, page)
}

func (handler Handler) principal(writer http.ResponseWriter, request *http.Request) (string, string, bool) {
	if handler.users == nil {
		writeError(writer, request, http.StatusUnauthorized, "UNAUTHENTICATED", "Требуется вход в систему")
		return "", "", false
	}
	actorID, ok, err := handler.users.User(request)
	if err != nil {
		writeError(writer, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Внутренняя ошибка")
		return "", "", false
	}
	if !ok || actorID == "" {
		writeError(writer, request, http.StatusUnauthorized, "UNAUTHENTICATED", "Требуется вход в систему")
		return "", "", false
	}
	tenantID := strings.TrimSpace(request.Header.Get("X-Tenant-ID"))
	if tenantID == "" {
		writeError(writer, request, http.StatusBadRequest, "TENANT_REQUIRED", "Требуется X-Tenant-ID")
		return "", "", false
	}
	return actorID, tenantID, true
}

func pageLimit(writer http.ResponseWriter, request *http.Request) (int, bool) {
	value := request.URL.Query().Get("limit")
	if value == "" {
		return 0, true
	}
	limit, err := strconv.Atoi(value)
	if err != nil {
		writeError(writer, request, http.StatusBadRequest, "INVALID_ARGUMENT", "Некорректный limit")
		return 0, false
	}
	return limit, true
}

func handleError(writer http.ResponseWriter, request *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, application.ErrInvalid):
		writeError(writer, request, http.StatusBadRequest, "INVALID_ARGUMENT", "Некорректный запрос")
	case errors.Is(err, application.ErrForbidden):
		writeError(writer, request, http.StatusForbidden, "FORBIDDEN", "Недостаточно прав")
	case errors.Is(err, application.ErrNotFound):
		writeError(writer, request, http.StatusNotFound, "NOT_FOUND", "Переписка не найдена")
	case errors.Is(err, application.ErrConflict):
		writeError(writer, request, http.StatusConflict, "CONFLICT", "Конфликт канонических данных")
	default:
		writeError(writer, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Внутренняя ошибка")
	}
	return true
}

func writeError(writer http.ResponseWriter, request *http.Request, status int, code, message string) {
	httpplatform.WriteError(writer, request, status, code, message, nil)
}
