// Package transport adapts tenant setup operations to the public REST API.
package transport

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"lidradar/backend/internal/tenant/application"
	"lidradar/backend/internal/tenant/domain"
	httpplatform "lidradar/backend/platform/http"
)

type UserResolver interface {
	User(*http.Request) (userID string, ok bool, err error)
}

type TenantService interface {
	CreateOrganization(context.Context, string, string, string, string) (domain.Organization, error)
	GetOrganization(context.Context, string, string) (domain.Organization, error)
	UpdateOrganization(context.Context, string, string, application.OrganizationUpdate) (domain.Organization, error)
	ListLocations(context.Context, string, string) ([]domain.Location, error)
	CreateLocation(context.Context, string, string, string, string, int) (domain.Location, error)
	UpdateLocation(context.Context, string, string, string, application.LocationUpdate) (domain.Location, error)
	ReplaceBusinessHours(context.Context, string, string, string, string, []application.BusinessHourInput) (domain.Location, error)
}

type Handler struct {
	service TenantService
	users   UserResolver
}

func NewHandler(service TenantService, users UserResolver) Handler {
	return Handler{service: service, users: users}
}

func (handler Handler) Router() http.Handler {
	router := chi.NewRouter()
	router.Post("/organizations", handler.createOrganization)
	router.Get("/organization", handler.getOrganization)
	router.Patch("/organization", handler.updateOrganization)
	router.Get("/locations", handler.listLocations)
	router.Post("/locations", handler.createLocation)
	router.Patch("/locations/{locationID}", handler.updateLocation)
	router.Put("/locations/{locationID}/business-hours", handler.replaceBusinessHours)
	return router
}

type organizationRequest struct {
	Name            *string `json:"name"`
	DefaultTimezone *string `json:"defaultTimezone"`
	DefaultCurrency *string `json:"defaultCurrency"`
}

func (handler Handler) createOrganization(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.user(w, r)
	if !ok {
		return
	}
	var request organizationRequest
	if httpplatform.DecodeJSON(w, r, &request) != nil || request.Name == nil || request.DefaultTimezone == nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request")
		return
	}
	currency := ""
	if request.DefaultCurrency != nil {
		currency = *request.DefaultCurrency
	}
	organization, err := handler.service.CreateOrganization(r.Context(), actorID, *request.Name, *request.DefaultTimezone, currency)
	if handleError(w, r, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusCreated, organization)
}

func (handler Handler) getOrganization(w http.ResponseWriter, r *http.Request) {
	actorID, tenantID, ok := handler.principal(w, r)
	if !ok {
		return
	}
	organization, err := handler.service.GetOrganization(r.Context(), actorID, tenantID)
	if handleError(w, r, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusOK, organization)
}

func (handler Handler) updateOrganization(w http.ResponseWriter, r *http.Request) {
	actorID, tenantID, ok := handler.principal(w, r)
	if !ok {
		return
	}
	var request organizationRequest
	if httpplatform.DecodeJSON(w, r, &request) != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request")
		return
	}
	organization, err := handler.service.UpdateOrganization(r.Context(), actorID, tenantID, application.OrganizationUpdate{
		Name: request.Name, DefaultTimezone: request.DefaultTimezone, DefaultCurrency: request.DefaultCurrency,
	})
	if handleError(w, r, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusOK, organization)
}

type locationRequest struct {
	Name                     *string `json:"name"`
	Timezone                 *string `json:"timezone"`
	ResponseThresholdMinutes *int    `json:"responseThresholdMinutes"`
	Active                   *bool   `json:"active"`
}

func (handler Handler) listLocations(w http.ResponseWriter, r *http.Request) {
	actorID, tenantID, ok := handler.principal(w, r)
	if !ok {
		return
	}
	locations, err := handler.service.ListLocations(r.Context(), actorID, tenantID)
	if handleError(w, r, err) {
		return
	}
	if locations == nil {
		locations = []domain.Location{}
	}
	httpplatform.WriteJSON(w, http.StatusOK, map[string]any{"items": locations})
}

func (handler Handler) createLocation(w http.ResponseWriter, r *http.Request) {
	actorID, tenantID, ok := handler.principal(w, r)
	if !ok {
		return
	}
	var request locationRequest
	if httpplatform.DecodeJSON(w, r, &request) != nil || request.Name == nil || request.Timezone == nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request")
		return
	}
	threshold := 0
	if request.ResponseThresholdMinutes != nil {
		threshold = *request.ResponseThresholdMinutes
	}
	location, err := handler.service.CreateLocation(r.Context(), actorID, tenantID, *request.Name, *request.Timezone, threshold)
	if handleError(w, r, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusCreated, location)
}

func (handler Handler) updateLocation(w http.ResponseWriter, r *http.Request) {
	actorID, tenantID, ok := handler.principal(w, r)
	if !ok {
		return
	}
	var request locationRequest
	if httpplatform.DecodeJSON(w, r, &request) != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request")
		return
	}
	location, err := handler.service.UpdateLocation(r.Context(), actorID, tenantID, chi.URLParam(r, "locationID"), application.LocationUpdate{
		Name: request.Name, Timezone: request.Timezone,
		ResponseThresholdMinutes: request.ResponseThresholdMinutes, Active: request.Active,
	})
	if handleError(w, r, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusOK, location)
}

type businessHoursRequest struct {
	Timezone string            `json:"timezone"`
	Days     []businessHourDay `json:"days"`
}

type businessHourDay struct {
	Weekday  int    `json:"weekday"`
	Closed   bool   `json:"closed"`
	OpensAt  string `json:"opensAt,omitempty"`
	ClosesAt string `json:"closesAt,omitempty"`
}

func (handler Handler) replaceBusinessHours(w http.ResponseWriter, r *http.Request) {
	actorID, tenantID, ok := handler.principal(w, r)
	if !ok {
		return
	}
	var request businessHoursRequest
	if httpplatform.DecodeJSON(w, r, &request) != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request")
		return
	}
	days := make([]application.BusinessHourInput, 0, len(request.Days))
	for _, day := range request.Days {
		days = append(days, application.BusinessHourInput{Weekday: day.Weekday, Closed: day.Closed, OpensAt: day.OpensAt, ClosesAt: day.ClosesAt})
	}
	location, err := handler.service.ReplaceBusinessHours(r.Context(), actorID, tenantID, chi.URLParam(r, "locationID"), request.Timezone, days)
	if handleError(w, r, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusOK, location)
}

func (handler Handler) user(w http.ResponseWriter, r *http.Request) (string, bool) {
	if handler.users == nil {
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "Authentication required")
		return "", false
	}
	userID, ok, err := handler.users.User(r)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error")
		return "", false
	}
	if !ok || userID == "" {
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "Authentication required")
		return "", false
	}
	return userID, true
}

func (handler Handler) principal(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	actorID, ok := handler.user(w, r)
	if !ok {
		return "", "", false
	}
	tenantID := strings.TrimSpace(r.Header.Get("X-Tenant-ID"))
	if tenantID == "" {
		writeError(w, r, http.StatusBadRequest, "TENANT_REQUIRED", "X-Tenant-ID is required")
		return "", "", false
	}
	return actorID, tenantID, true
}

func handleError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, application.ErrInvalid):
		writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request")
	case errors.Is(err, application.ErrForbidden):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "Permission denied")
	case errors.Is(err, application.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "Resource not found")
	case errors.Is(err, application.ErrConflict):
		writeError(w, r, http.StatusConflict, "CONFLICT", "Resource already exists")
	default:
		writeError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error")
	}
	return true
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	httpplatform.WriteError(w, r, status, code, message, nil)
}
