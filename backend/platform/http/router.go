package httpplatform

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"lidradar/backend/platform/buildinfo"
	"lidradar/backend/platform/health"
	"lidradar/backend/platform/ids"
	"lidradar/backend/platform/tenantctx"
)

type routerOptions struct {
	allowedOrigins  []string
	strictTransport bool
	rateLimits      []RateLimit
	now             func() time.Time
}

// RouterOption configures shared HTTP boundary behavior.
type RouterOption func(*routerOptions)

// WithAllowedOrigins permits browser mutation requests from explicitly trusted
// origins in addition to the API's own origin.
func WithAllowedOrigins(origins []string) RouterOption {
	return func(options *routerOptions) {
		options.allowedOrigins = append([]string(nil), origins...)
	}
}

// WithStrictTransport включает HSTS для ответов за TLS-прокси.
func WithStrictTransport(enabled bool) RouterOption {
	return func(options *routerOptions) { options.strictTransport = enabled }
}

// WithRateLimit ограничивает частоту запросов на адрес для указанных префиксов.
// Каждое правило считается отдельно; при совпадении нескольких применяется первое.
func WithRateLimit(limits ...RateLimit) RouterOption {
	return func(options *routerOptions) { options.rateLimits = append(options.rateLimits, limits...) }
}

// WithClock подменяет часы ограничителя частоты в тестах.
func WithClock(now func() time.Time) RouterOption {
	return func(options *routerOptions) { options.now = now }
}

// NewRouter creates the shared platform router. Feature routers are mounted by
// the API composition root after these platform routes and middleware exist.
func NewRouter(service string, logger *slog.Logger, readiness health.Checker, options ...RouterOption) *chi.Mux {
	if logger == nil {
		logger = slog.Default()
	}
	configuration := routerOptions{}
	for _, option := range options {
		if option != nil {
			option(&configuration)
		}
	}
	if configuration.now == nil {
		configuration.now = time.Now
	}
	router := chi.NewRouter()
	router.Use(correlation)
	// recovery стоит сразу за correlation: паника в любом следующем слое
	// превращается в 500 с корреляцией, а не обрывает соединение.
	router.Use(recovery(logger))
	router.Use(securityHeaders(configuration.strictTransport))
	router.Use(validTenantSelector)
	router.Use(requestLogging(logger))
	router.Use(originProtection(configuration.allowedOrigins))
	limiters := make([]*rateLimiter, 0, len(configuration.rateLimits))
	for _, limit := range configuration.rateLimits {
		limiters = append(limiters, newRateLimiter(limit, configuration.now))
	}
	router.Use(rateLimited(limiters))

	router.Get("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": service})
	})
	router.Get("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		if readiness == nil {
			WriteError(w, r, http.StatusServiceUnavailable, "SERVICE_NOT_READY", "Service is not ready", nil)
			return
		}
		status, err := readiness.Check(r.Context())
		if err != nil {
			WriteError(w, r, http.StatusServiceUnavailable, "SERVICE_NOT_READY", "Service is not ready", nil)
			return
		}
		build := buildinfo.Current()
		WriteJSON(w, http.StatusOK, map[string]any{
			"status": "ready", "service": service,
			"build": map[string]any{
				"version": build.Version, "revision": build.Revision, "modified": build.Modified,
			},
			"databaseMigration": status.DatabaseMigration,
			"migrations": map[string]string{
				"applied": status.Applied, "latest": status.Latest,
			},
		})
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
		if tenantID != "" {
			// Контекст организации для RLS (ADR 0034); проверки членства остаются на приложении.
			request = request.WithContext(tenantctx.WithTenant(request.Context(), tenantID))
		}
		next.ServeHTTP(w, request)
	})
}
