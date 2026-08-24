// Package transport adapts Connector Core management and webhook receipt to HTTP.
package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"lidradar/backend/internal/connector/application"
	"lidradar/backend/internal/connector/domain"
	httpplatform "lidradar/backend/platform/http"
)

const maxWebhookBody = 1 << 20

type UserResolver interface {
	User(*http.Request) (userID string, ok bool, err error)
}

type ConnectorService interface {
	Connect(context.Context, string, string, application.ConnectCommand) (domain.ChannelConnection, error)
	List(context.Context, string, string) ([]domain.ChannelConnection, error)
	Health(context.Context, string, string, string) (domain.ConnectionHealth, error)
	Disconnect(context.Context, string, string, string) error
	Receive(context.Context, string, string, string, []byte, domain.Headers) (application.Receipt, error)
}

type Handler struct {
	service ConnectorService
	users   UserResolver
}

func NewHandler(service ConnectorService, users UserResolver) Handler {
	return Handler{service: service, users: users}
}

func (handler Handler) ManagementRouter() http.Handler {
	router := chi.NewRouter()
	router.Get("/", handler.list)
	router.Post("/{provider}/connect", handler.connect)
	router.Delete("/{connectionID}", handler.disconnect)
	router.Get("/{connectionID}/health", handler.health)
	return router
}

func (handler Handler) WebhookRouter() http.Handler {
	router := chi.NewRouter()
	router.Post("/{provider}/{tenantID}/{connectionID}", handler.receive)
	return router
}

type connectRequest struct {
	Name          *string `json:"name"`
	LocationID    *string `json:"locationId"`
	WebhookSecret *string `json:"webhookSecret"`
}

func (handler Handler) connect(w http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(w, request)
	if !ok {
		return
	}
	var body connectRequest
	if httpplatform.DecodeJSON(w, request, &body) != nil || body.Name == nil || body.WebhookSecret == nil {
		writeError(w, request, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request")
		return
	}
	connection, err := handler.service.Connect(request.Context(), actorID, tenantID, application.ConnectCommand{
		Provider: chi.URLParam(request, "provider"), Name: *body.Name,
		LocationID: body.LocationID, WebhookSecret: *body.WebhookSecret,
	})
	if handleError(w, request, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusCreated, connection)
}

func (handler Handler) list(w http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(w, request)
	if !ok {
		return
	}
	connections, err := handler.service.List(request.Context(), actorID, tenantID)
	if handleError(w, request, err) {
		return
	}
	if connections == nil {
		connections = []domain.ChannelConnection{}
	}
	httpplatform.WriteJSON(w, http.StatusOK, map[string]any{"items": connections})
}

func (handler Handler) health(w http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(w, request)
	if !ok {
		return
	}
	health, err := handler.service.Health(request.Context(), actorID, tenantID, chi.URLParam(request, "connectionID"))
	if handleError(w, request, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusOK, health)
}

func (handler Handler) disconnect(w http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(w, request)
	if !ok {
		return
	}
	if handleError(w, request, handler.service.Disconnect(
		request.Context(), actorID, tenantID, chi.URLParam(request, "connectionID"),
	)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (handler Handler) receive(w http.ResponseWriter, request *http.Request) {
	payload, err := io.ReadAll(http.MaxBytesReader(w, request.Body, maxWebhookBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, request, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "Webhook payload is too large")
			return
		}
		writeError(w, request, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request")
		return
	}
	receipt, err := handler.service.Receive(
		request.Context(), chi.URLParam(request, "provider"), chi.URLParam(request, "tenantID"),
		chi.URLParam(request, "connectionID"), payload, request.Header,
	)
	if handleError(w, request, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusAccepted, receipt)
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
	case errors.Is(err, application.ErrUnauthenticated):
		writeError(w, request, http.StatusUnauthorized, "WEBHOOK_UNAUTHENTICATED", "Webhook authentication failed")
	case errors.Is(err, application.ErrForbidden):
		writeError(w, request, http.StatusForbidden, "FORBIDDEN", "Permission denied")
	case errors.Is(err, application.ErrNotFound):
		writeError(w, request, http.StatusNotFound, "NOT_FOUND", "Resource not found")
	case errors.Is(err, application.ErrConflict):
		writeError(w, request, http.StatusConflict, "CONFLICT", "External event identifier conflict")
	case errors.Is(err, application.ErrUnavailable):
		writeError(w, request, http.StatusServiceUnavailable, "CONNECTOR_UNAVAILABLE", "Connector unavailable")
	default:
		writeError(w, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error")
	}
	return true
}

func writeError(w http.ResponseWriter, request *http.Request, status int, code, message string) {
	httpplatform.WriteError(w, request, status, code, message, nil)
}
