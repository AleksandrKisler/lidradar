package httpplatform

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"lidradar/backend/platform/ids"
)

// Readiness is implemented by critical runtime dependencies such as PostgreSQL.
type Readiness interface {
	Ping(context.Context) error
}

type routerOptions struct{ allowedOrigins []string }

// RouterOption configures shared HTTP boundary behavior.
type RouterOption func(*routerOptions)

// WithAllowedOrigins permits browser mutation requests from explicitly trusted
// origins in addition to the API's own origin.
func WithAllowedOrigins(origins []string) RouterOption {
	return func(options *routerOptions) {
		options.allowedOrigins = append([]string(nil), origins...)
	}
}

// NewRouter creates the shared platform router. Feature routers are mounted by
// the API composition root after these platform routes and middleware exist.
func NewRouter(service string, logger *slog.Logger, readiness Readiness, options ...RouterOption) *chi.Mux {
	if logger == nil {
		logger = slog.Default()
	}
	configuration := routerOptions{}
	for _, option := range options {
		if option != nil {
			option(&configuration)
		}
	}
	router := chi.NewRouter()
	router.Use(correlation)
	router.Use(validTenantSelector)
	router.Use(recovery(logger))
	router.Use(requestLogging(logger))
	router.Use(originProtection(configuration.allowedOrigins))

	router.Get("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": service})
	})
	router.Get("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		if readiness == nil || readiness.Ping(r.Context()) != nil {
			WriteError(w, r, http.StatusServiceUnavailable, "SERVICE_NOT_READY", "Service is not ready", nil)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ready", "service": service})
	})
	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, http.StatusNotFound, "ROUTE_NOT_FOUND", "Route not found", nil)
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed", nil)
	})
	return router
}

func validTenantSelector(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		tenantID := request.Header.Get("X-Tenant-ID")
		if tenantID != "" && !ids.Valid(tenantID) {
			WriteError(w, request, http.StatusBadRequest, "INVALID_TENANT", "X-Tenant-ID must be a UUID", nil)
			return
		}
		next.ServeHTTP(w, request)
	})
}
