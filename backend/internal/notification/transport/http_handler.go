// Package transport публикует управление личной Telegram-привязкой и личными
// настройками уведомлений.
package transport

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"lidradar/backend/internal/notification/application"
	"lidradar/backend/internal/notification/domain"
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

type PreferenceService interface {
	List(context.Context, string, string) ([]application.PreferenceView, error)
	Put(context.Context, string, string, string, application.PreferenceInput) (application.PreferenceView, error)
	Reset(context.Context, string, string, string) error
}

type Handler struct {
	service     LinkService
	preferences PreferenceService
	users       UserResolver
}

func NewHandler(service LinkService, preferences PreferenceService, users UserResolver) Handler {
	return Handler{service: service, preferences: preferences, users: users}
}

func (handler Handler) Router() http.Handler {
	router := chi.NewRouter()
	router.Post("/telegram-link-token", handler.issue)
	router.Get("/telegram-link", handler.status)
	router.Delete("/telegram-link", handler.disable)
	router.Get("/preferences", handler.listPreferences)
	router.Put("/preferences/{riskType}", handler.putPreference)
	router.Delete("/preferences/{riskType}", handler.resetPreference)
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

type preferenceRequest struct {
	MinimumSeverity   *string `json:"minimumSeverity"`
	DeliveryMode      *string `json:"deliveryMode"`
	InAppEnabled      *bool   `json:"inAppEnabled"`
	TelegramEnabled   *bool   `json:"telegramEnabled"`
	QuietHoursEnabled *bool   `json:"quietHoursEnabled"`
	QuietHoursStart   *string `json:"quietHoursStart"`
	QuietHoursEnd     *string `json:"quietHoursEnd"`
	DigestTime        *string `json:"digestTime"`
}

type preferenceResponse struct {
	RiskType          string     `json:"riskType"`
	MinimumSeverity   string     `json:"minimumSeverity"`
	DeliveryMode      string     `json:"deliveryMode"`
	InAppEnabled      bool       `json:"inAppEnabled"`
	TelegramEnabled   bool       `json:"telegramEnabled"`
	QuietHoursEnabled bool       `json:"quietHoursEnabled"`
	QuietHoursStart   *string    `json:"quietHoursStart"`
	QuietHoursEnd     *string    `json:"quietHoursEnd"`
	DigestTime        string     `json:"digestTime"`
	Timezone          string     `json:"timezone"`
	IsDefault         bool       `json:"isDefault"`
	UpdatedAt         *time.Time `json:"updatedAt"`
}

func renderPreference(view application.PreferenceView) preferenceResponse {
	preference := view.Preference
	response := preferenceResponse{
		RiskType: string(preference.RiskType), MinimumSeverity: string(preference.MinimumSeverity),
		DeliveryMode: string(preference.DeliveryMode), InAppEnabled: preference.InAppEnabled,
		TelegramEnabled: preference.TelegramEnabled, QuietHoursEnabled: preference.QuietHoursEnabled,
		DigestTime: preference.DigestTime.String(), Timezone: view.Timezone, IsDefault: !preference.Stored(),
	}
	if preference.QuietHoursStart != nil && preference.QuietHoursEnd != nil {
		start, end := preference.QuietHoursStart.String(), preference.QuietHoursEnd.String()
		response.QuietHoursStart, response.QuietHoursEnd = &start, &end
	}
	if preference.Stored() {
		updatedAt := preference.UpdatedAt
		response.UpdatedAt = &updatedAt
	}
	return response
}

func (handler Handler) listPreferences(writer http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	views, err := handler.preferences.List(request.Context(), actorID, tenantID)
	if handleError(writer, request, err) {
		return
	}
	items := make([]preferenceResponse, 0, len(views))
	for _, view := range views {
		items = append(items, renderPreference(view))
	}
	httpplatform.WriteJSON(writer, http.StatusOK, map[string]any{"items": items})
}

func (handler Handler) putPreference(writer http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var body preferenceRequest
	if httpplatform.DecodeJSON(writer, request, &body) != nil || body.MinimumSeverity == nil || body.DeliveryMode == nil ||
		body.InAppEnabled == nil || body.TelegramEnabled == nil || body.QuietHoursEnabled == nil || body.DigestTime == nil {
		httpplatform.WriteError(writer, request, http.StatusBadRequest, "INVALID_ARGUMENT", "Некорректный запрос", nil)
		return
	}
	view, err := handler.preferences.Put(request.Context(), actorID, tenantID, chi.URLParam(request, "riskType"), application.PreferenceInput{
		MinimumSeverity: *body.MinimumSeverity, DeliveryMode: *body.DeliveryMode,
		InAppEnabled: *body.InAppEnabled, TelegramEnabled: *body.TelegramEnabled, QuietHoursEnabled: *body.QuietHoursEnabled,
		QuietHoursStart: body.QuietHoursStart, QuietHoursEnd: body.QuietHoursEnd, DigestTime: *body.DigestTime,
	})
	if handleError(writer, request, err) {
		return
	}
	httpplatform.WriteJSON(writer, http.StatusOK, renderPreference(view))
}

func (handler Handler) resetPreference(writer http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	if handleError(writer, request, handler.preferences.Reset(request.Context(), actorID, tenantID, chi.URLParam(request, "riskType"))) {
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
	case errors.Is(err, application.ErrNotFound):
		httpplatform.WriteError(writer, request, http.StatusNotFound, "NOT_FOUND", "Ресурс не найден", nil)
	case errors.Is(err, application.ErrInvalidLinkToken), errors.Is(err, domain.ErrInvalidPreference):
		httpplatform.WriteError(writer, request, http.StatusBadRequest, "INVALID_ARGUMENT", "Некорректный запрос", nil)
	default:
		httpplatform.WriteError(writer, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Внутренняя ошибка", nil)
	}
	return true
}
