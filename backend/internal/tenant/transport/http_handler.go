// Package transport adapts tenant setup operations to the public REST API.
package transport

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	identityapplication "lidradar/backend/internal/identity/application"
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
	MLConsent(context.Context, string, string) (domain.MLConsent, bool, error)
	GrantMLConsent(context.Context, string, string) (domain.MLConsent, bool, error)
	RevokeMLConsent(context.Context, string, string) (domain.MLConsent, bool, error)
	ListMembers(context.Context, string, string) ([]domain.Member, error)
	ChangeMemberRole(context.Context, string, string, string, domain.Role) (domain.Membership, error)
	RevokeMember(context.Context, string, string, string) error
	CreateInvitation(context.Context, string, string, domain.Role, *string) (application.InvitationView, string, error)
	ListInvitations(context.Context, string, string) ([]application.InvitationView, error)
	RevokeInvitation(context.Context, string, string, string) error
	AcceptInvitation(context.Context, string, string) (identityapplication.MembershipSummary, error)
	Onboarding(context.Context, string, string) (domain.OnboardingStatus, error)
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
	router.Get("/organization/ml-consent", handler.getMLConsent)
	router.Post("/organization/ml-consent", handler.grantMLConsent)
	router.Delete("/organization/ml-consent", handler.revokeMLConsent)
	router.Get("/organization/onboarding", handler.onboarding)
	router.Get("/organization/members", handler.listMembers)
	router.Patch("/organization/members/{userID}", handler.changeMemberRole)
	router.Delete("/organization/members/{userID}", handler.revokeMember)
	router.Get("/organization/invitations", handler.listInvitations)
	router.Post("/organization/invitations", handler.createInvitation)
	router.Delete("/organization/invitations/{invitationID}", handler.revokeInvitation)
	router.Post("/invitations/accept", handler.acceptInvitation)
	return router
}

func (handler Handler) onboarding(w http.ResponseWriter, r *http.Request) {
	actorID, tenantID, ok := handler.principal(w, r)
	if !ok {
		return
	}
	status, err := handler.service.Onboarding(r.Context(), actorID, tenantID)
	if handleError(w, r, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusOK, status)
}

func (handler Handler) listMembers(w http.ResponseWriter, r *http.Request) {
	actorID, tenantID, ok := handler.principal(w, r)
	if !ok {
		return
	}
	members, err := handler.service.ListMembers(r.Context(), actorID, tenantID)
	if handleError(w, r, err) {
		return
	}
	if members == nil {
		members = []domain.Member{}
	}
	httpplatform.WriteJSON(w, http.StatusOK, map[string]any{"items": members})
}

type memberRoleRequest struct {
	Role *string `json:"role"`
}

func (handler Handler) changeMemberRole(w http.ResponseWriter, r *http.Request) {
	actorID, tenantID, ok := handler.principal(w, r)
	if !ok {
		return
	}
	var request memberRoleRequest
	if httpplatform.DecodeJSON(w, r, &request) != nil || request.Role == nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request")
		return
	}
	membership, err := handler.service.ChangeMemberRole(r.Context(), actorID, tenantID, chi.URLParam(r, "userID"), domain.Role(*request.Role))
	if handleError(w, r, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusOK, membership)
}

func (handler Handler) revokeMember(w http.ResponseWriter, r *http.Request) {
	actorID, tenantID, ok := handler.principal(w, r)
	if !ok {
		return
	}
	if handleError(w, r, handler.service.RevokeMember(r.Context(), actorID, tenantID, chi.URLParam(r, "userID"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (handler Handler) listInvitations(w http.ResponseWriter, r *http.Request) {
	actorID, tenantID, ok := handler.principal(w, r)
	if !ok {
		return
	}
	invitations, err := handler.service.ListInvitations(r.Context(), actorID, tenantID)
	if handleError(w, r, err) {
		return
	}
	if invitations == nil {
		invitations = []application.InvitationView{}
	}
	httpplatform.WriteJSON(w, http.StatusOK, map[string]any{"items": invitations})
}

type invitationRequest struct {
	Role *string `json:"role"`
	Note *string `json:"note"`
}

// createInvitation возвращает открытый код ровно один раз: сервер хранит
// только его хеш и повторно показать код не может.
func (handler Handler) createInvitation(w http.ResponseWriter, r *http.Request) {
	actorID, tenantID, ok := handler.principal(w, r)
	if !ok {
		return
	}
	var request invitationRequest
	if httpplatform.DecodeJSON(w, r, &request) != nil || request.Role == nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request")
		return
	}
	invitation, code, err := handler.service.CreateInvitation(r.Context(), actorID, tenantID, domain.Role(*request.Role), request.Note)
	if handleError(w, r, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusCreated, map[string]any{"invitation": invitation, "code": code})
}

func (handler Handler) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	actorID, tenantID, ok := handler.principal(w, r)
	if !ok {
		return
	}
	if handleError(w, r, handler.service.RevokeInvitation(r.Context(), actorID, tenantID, chi.URLParam(r, "invitationID"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type acceptInvitationRequest struct {
	Code *string `json:"code"`
}

// acceptInvitation принимает код от имени сеанса без X-Tenant-ID: организацию
// определяет само приглашение.
func (handler Handler) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.user(w, r)
	if !ok {
		return
	}
	var request acceptInvitationRequest
	if httpplatform.DecodeJSON(w, r, &request) != nil || request.Code == nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request")
		return
	}
	membership, err := handler.service.AcceptInvitation(r.Context(), actorID, *request.Code)
	if handleError(w, r, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusOK, map[string]any{"membership": membership})
}

func mlConsentResponse(consent domain.MLConsent, active bool) map[string]any {
	response := map[string]any{"scope": domain.MLConsentScopeDatasets, "active": active}
	if active {
		response["consent"] = consent
	} else {
		response["consent"] = nil
	}
	return response
}

func (handler Handler) getMLConsent(w http.ResponseWriter, r *http.Request) {
	actorID, tenantID, ok := handler.principal(w, r)
	if !ok {
		return
	}
	consent, active, err := handler.service.MLConsent(r.Context(), actorID, tenantID)
	if handleError(w, r, err) {
		return
	}
	httpplatform.WriteJSON(w, http.StatusOK, mlConsentResponse(consent, active))
}

func (handler Handler) grantMLConsent(w http.ResponseWriter, r *http.Request) {
	actorID, tenantID, ok := handler.principal(w, r)
	if !ok {
		return
	}
	consent, created, err := handler.service.GrantMLConsent(r.Context(), actorID, tenantID)
	if handleError(w, r, err) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpplatform.WriteJSON(w, status, mlConsentResponse(consent, true))
}

func (handler Handler) revokeMLConsent(w http.ResponseWriter, r *http.Request) {
	actorID, tenantID, ok := handler.principal(w, r)
	if !ok {
		return
	}
	if _, _, err := handler.service.RevokeMLConsent(r.Context(), actorID, tenantID); handleError(w, r, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	// Новая точка всегда активна: поле active принимается только в PATCH,
	// иначе клиентское ожидание молча игнорировалось бы (GAP-CONTRACT-019).
	if httpplatform.DecodeJSON(w, r, &request) != nil || request.Name == nil || request.Timezone == nil || request.Active != nil {
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
	case errors.Is(err, application.ErrLastOwner):
		writeError(w, r, http.StatusConflict, "LAST_OWNER", "The last active owner cannot be demoted or revoked")
	case errors.Is(err, application.ErrMemberDisabled):
		writeError(w, r, http.StatusConflict, "MEMBER_DISABLED", "Membership is disabled")
	case errors.Is(err, application.ErrInvitationExpired):
		writeError(w, r, http.StatusConflict, "INVITATION_EXPIRED", "Invitation has expired")
	case errors.Is(err, application.ErrInvitationRevoked):
		writeError(w, r, http.StatusConflict, "INVITATION_REVOKED", "Invitation was revoked")
	case errors.Is(err, application.ErrInvitationUsed):
		writeError(w, r, http.StatusConflict, "INVITATION_USED", "Invitation was already accepted")
	case errors.Is(err, application.ErrAlreadyMember):
		writeError(w, r, http.StatusConflict, "ALREADY_MEMBER", "User is already an active member")
	default:
		writeError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error")
	}
	return true
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	httpplatform.WriteError(w, r, status, code, message, nil)
}
