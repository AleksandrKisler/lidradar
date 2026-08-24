// Package transport adapts service catalog use cases to the public REST API.
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"lidradar/backend/internal/catalog/application"
	"lidradar/backend/internal/catalog/domain"
	httpplatform "lidradar/backend/platform/http"
)

type UserResolver interface {
	User(*http.Request) (userID string, ok bool, err error)
}

type CatalogService interface {
	List(context.Context, string, string) ([]domain.ServiceCatalogItem, error)
	Create(context.Context, string, string, application.CreateCommand) (domain.ServiceCatalogItem, error)
	Update(context.Context, string, string, string, application.UpdateCommand) (domain.ServiceCatalogItem, error)
	Deactivate(context.Context, string, string, string) error
}

type Handler struct {
	service CatalogService
	users   UserResolver
}

func NewHandler(service CatalogService, users UserResolver) Handler {
	return Handler{service: service, users: users}
}

func (handler Handler) Router() http.Handler {
	router := chi.NewRouter()
	router.Get("/", handler.list)
	router.Post("/", handler.create)
	router.Patch("/{serviceID}", handler.update)
	router.Delete("/{serviceID}", handler.deactivate)
	return router
}

type createRequest struct {
	Name       *string `json:"name"`
	LocationID *string `json:"locationId"`
	PriceFrom  *string `json:"priceFrom"`
	PriceTo    *string `json:"priceTo"`
	Currency   *string `json:"currency"`
}

func (handler Handler) list(w http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(w, request)
	if !ok {
		return
	}
	items, err := handler.service.List(request.Context(), actorID, tenantID)
	if handleError(w, request, err) {
		return
	}
	if items == nil {
		items = []domain.ServiceCatalogItem{}
	}
	httpplatform.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (handler Handler) create(w http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(w, request)
	if !ok {
		return
	}
	var body createRequest
	if httpplatform.DecodeJSON(w, request, &body) != nil || body.Name == nil {
		writeError(w, request, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request")
		return
	}
	currency := ""
	if body.Currency != nil {
		currency = *body.Currency
	}
	item, err := handler.service.Create(request.Context(), actorID, tenantID, application.CreateCommand{
		Name: *body.Name, LocationID: body.LocationID, PriceFrom: body.PriceFrom, PriceTo: body.PriceTo, Currency: currency,
	})
	if handleError(w, request, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusCreated, item)
}

type optionalNullableString struct {
	Set   bool
	Value *string
}

func (value *optionalNullableString) UnmarshalJSON(data []byte) error {
	value.Set = true
	if string(data) == "null" {
		value.Value = nil
		return nil
	}
	var decoded string
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	value.Value = &decoded
	return nil
}

type updateRequest struct {
	Name       *string                `json:"name"`
	LocationID optionalNullableString `json:"locationId"`
	PriceFrom  optionalNullableString `json:"priceFrom"`
	PriceTo    optionalNullableString `json:"priceTo"`
	Currency   *string                `json:"currency"`
	Active     *bool                  `json:"active"`
}

func (handler Handler) update(w http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(w, request)
	if !ok {
		return
	}
	var body updateRequest
	if httpplatform.DecodeJSON(w, request, &body) != nil {
		writeError(w, request, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request")
		return
	}
	item, err := handler.service.Update(request.Context(), actorID, tenantID, chi.URLParam(request, "serviceID"), application.UpdateCommand{
		Name:       body.Name,
		LocationID: application.OptionalString{Set: body.LocationID.Set, Value: body.LocationID.Value},
		PriceFrom:  application.OptionalString{Set: body.PriceFrom.Set, Value: body.PriceFrom.Value},
		PriceTo:    application.OptionalString{Set: body.PriceTo.Set, Value: body.PriceTo.Value},
		Currency:   body.Currency,
		Active:     body.Active,
	})
	if handleError(w, request, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusOK, item)
}

func (handler Handler) deactivate(w http.ResponseWriter, request *http.Request) {
	actorID, tenantID, ok := handler.principal(w, request)
	if !ok {
		return
	}
	if handleError(w, request, handler.service.Deactivate(request.Context(), actorID, tenantID, chi.URLParam(request, "serviceID"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	case errors.Is(err, application.ErrConflict):
		writeError(w, request, http.StatusConflict, "CONFLICT", "Resource already exists")
	default:
		writeError(w, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error")
	}
	return true
}

func writeError(w http.ResponseWriter, request *http.Request, status int, code, message string) {
	httpplatform.WriteError(w, request, status, code, message, nil)
}
