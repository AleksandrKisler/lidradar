// Package transport adapts the Identity application to HTTP cookies and JSON.
package transport

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"lidradar/backend/internal/identity/application"
	"lidradar/backend/internal/identity/domain"
	httpplatform "lidradar/backend/platform/http"
)

const SessionCookieName = "lidradar_session"

type Authentication interface {
	Register(context.Context, string, string, string, application.Client) (application.Authenticated, error)
	Login(context.Context, string, string, application.Client) (application.Authenticated, error)
	Authenticate(context.Context, string) (domain.User, error)
	Logout(context.Context, string) error
	Refresh(context.Context, string, application.Client) (application.Authenticated, error)
}

type CookieConfiguration struct {
	Secure bool
	TTL    time.Duration
}

type Handler struct {
	auth        Authentication
	memberships application.MembershipLister
	cookie      CookieConfiguration
}

func NewHandler(auth Authentication, memberships application.MembershipLister, cookie CookieConfiguration) Handler {
	return Handler{auth: auth, memberships: memberships, cookie: cookie}
}

func (h Handler) Router() http.Handler {
	router := chi.NewRouter()
	router.Post("/register", h.register)
	router.Post("/login", h.login)
	router.Post("/logout", h.logout)
	router.Post("/refresh", h.refresh)
	router.Get("/me", h.me)
	return router
}

type credentials struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName,omitempty"`
}

func (h Handler) register(w http.ResponseWriter, r *http.Request) {
	var request credentials
	if httpplatform.DecodeJSON(w, r, &request) != nil {
		httpplatform.WriteError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request", nil)
		return
	}
	authenticated, err := h.auth.Register(r.Context(), request.Email, request.Password, request.DisplayName, client(r))
	if handleError(w, r, err) {
		return
	}
	h.setSession(w, authenticated.Token)
	httpplatform.WriteJSON(w, http.StatusCreated, map[string]any{"user": authenticated.User})
}

func (h Handler) login(w http.ResponseWriter, r *http.Request) {
	var request credentials
	if httpplatform.DecodeJSON(w, r, &request) != nil {
		httpplatform.WriteError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request", nil)
		return
	}
	authenticated, err := h.auth.Login(r.Context(), request.Email, request.Password, client(r))
	if handleError(w, r, err) {
		return
	}
	h.setSession(w, authenticated.Token)
	httpplatform.WriteJSON(w, http.StatusOK, map[string]any{"user": authenticated.User})
}

func (h Handler) logout(w http.ResponseWriter, r *http.Request) {
	token := sessionToken(r)
	if err := h.auth.Logout(r.Context(), token); err != nil {
		httpplatform.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error", nil)
		return
	}
	h.clearSession(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h Handler) refresh(w http.ResponseWriter, r *http.Request) {
	authenticated, err := h.auth.Refresh(r.Context(), sessionToken(r), client(r))
	if handleError(w, r, err) {
		return
	}
	h.setSession(w, authenticated.Token)
	httpplatform.WriteJSON(w, http.StatusOK, map[string]any{"user": authenticated.User})
}

func (h Handler) me(w http.ResponseWriter, r *http.Request) {
	user, err := h.auth.Authenticate(r.Context(), sessionToken(r))
	if handleError(w, r, err) {
		return
	}
	memberships := []application.MembershipSummary{}
	if h.memberships != nil {
		memberships, err = h.memberships.MembershipsForUser(r.Context(), user.ID)
		if err != nil {
			httpplatform.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error", nil)
			return
		}
	}
	httpplatform.WriteJSON(w, http.StatusOK, map[string]any{"user": user, "memberships": memberships})
}

func (h Handler) setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: token, Path: "/",
		HttpOnly: true, Secure: h.cookie.Secure, SameSite: http.SameSiteStrictMode,
		MaxAge: int(h.cookie.TTL.Seconds()), Expires: time.Now().UTC().Add(h.cookie.TTL),
	})
}

func (h Handler) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: "", Path: "/",
		HttpOnly: true, Secure: h.cookie.Secure, SameSite: http.SameSiteStrictMode,
		MaxAge: -1, Expires: time.Unix(1, 0).UTC(),
	})
}

func sessionToken(r *http.Request) string {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func client(r *http.Request) application.Client {
	address := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(address); err == nil {
		address = host
	}
	return application.Client{IPAddress: address, UserAgent: r.UserAgent()}
}

func handleError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, application.ErrInvalidInput):
		httpplatform.WriteError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid request", nil)
	case errors.Is(err, application.ErrEmailConflict):
		httpplatform.WriteError(w, r, http.StatusConflict, "EMAIL_ALREADY_REGISTERED", "Email is already registered", nil)
	case errors.Is(err, application.ErrInvalidCredentials):
		httpplatform.WriteError(w, r, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Email or password is invalid", nil)
	case errors.Is(err, application.ErrUnauthenticated):
		httpplatform.WriteError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "Authentication required", nil)
	case errors.Is(err, application.ErrRateLimited):
		retryAfter := application.RateLimitRetryAfter(err)
		seconds := int64((retryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		httpplatform.WriteError(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests", nil)
	default:
		httpplatform.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error", nil)
	}
	return true
}

// Resolver authenticates the opaque cookie and selects an explicit tenant from
// X-Tenant-ID. Permission checks remain in each owning application service.
type Resolver struct{ Auth Authentication }

func (resolver Resolver) User(r *http.Request) (string, bool, error) {
	if resolver.Auth == nil {
		return "", false, nil
	}
	user, err := resolver.Auth.Authenticate(r.Context(), sessionToken(r))
	if errors.Is(err, application.ErrUnauthenticated) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return user.ID, true, nil
}

func (resolver Resolver) Principal(r *http.Request) (actorID, tenantID string, ok bool) {
	actorID, ok, err := resolver.User(r)
	tenantID = strings.TrimSpace(r.Header.Get("X-Tenant-ID"))
	return actorID, tenantID, err == nil && ok && tenantID != ""
}
