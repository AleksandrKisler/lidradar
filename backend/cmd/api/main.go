// Command api запускает HTTP API LidRadar.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	adminapplication "lidradar/backend/internal/admin/application"
	admininfrastructure "lidradar/backend/internal/admin/infrastructure"
	admintransport "lidradar/backend/internal/admin/transport"
	aiapplication "lidradar/backend/internal/ai/application"
	aiinfrastructure "lidradar/backend/internal/ai/infrastructure"
	aitransport "lidradar/backend/internal/ai/transport"
	analyticsapplication "lidradar/backend/internal/analytics/application"
	analyticsinfrastructure "lidradar/backend/internal/analytics/infrastructure"
	analyticstransport "lidradar/backend/internal/analytics/transport"
	auditinfrastructure "lidradar/backend/internal/audit/infrastructure"
	catalogapplication "lidradar/backend/internal/catalog/application"
	cataloginfrastructure "lidradar/backend/internal/catalog/infrastructure"
	catalogtransport "lidradar/backend/internal/catalog/transport"
	connectorapplication "lidradar/backend/internal/connector/application"
	connectorinfrastructure "lidradar/backend/internal/connector/infrastructure"
	connectortransport "lidradar/backend/internal/connector/transport"
	conversationapplication "lidradar/backend/internal/conversation/application"
	conversationinfrastructure "lidradar/backend/internal/conversation/infrastructure"
	conversationtransport "lidradar/backend/internal/conversation/transport"
	correctiveapplication "lidradar/backend/internal/corrective/application"
	correctiveinfrastructure "lidradar/backend/internal/corrective/infrastructure"
	correctivetransport "lidradar/backend/internal/corrective/transport"
	identityapplication "lidradar/backend/internal/identity/application"
	identityinfrastructure "lidradar/backend/internal/identity/infrastructure"
	identitytransport "lidradar/backend/internal/identity/transport"
	notificationapplication "lidradar/backend/internal/notification/application"
	notificationinfrastructure "lidradar/backend/internal/notification/infrastructure"
	notificationtransport "lidradar/backend/internal/notification/transport"
	opportunityapplication "lidradar/backend/internal/opportunity/application"
	opportunityinfrastructure "lidradar/backend/internal/opportunity/infrastructure"
	opportunitytransport "lidradar/backend/internal/opportunity/transport"
	revenueapplication "lidradar/backend/internal/revenue/application"
	revenueinfrastructure "lidradar/backend/internal/revenue/infrastructure"
	revenuetransport "lidradar/backend/internal/revenue/transport"
	riskapplication "lidradar/backend/internal/risk/application"
	riskinfrastructure "lidradar/backend/internal/risk/infrastructure"
	risktransport "lidradar/backend/internal/risk/transport"
	tenantapplication "lidradar/backend/internal/tenant/application"
	tenantinfrastructure "lidradar/backend/internal/tenant/infrastructure"
	tenanttransport "lidradar/backend/internal/tenant/transport"
	"lidradar/backend/platform/bootstrap"
	"lidradar/backend/platform/config"
	cryptoplatform "lidradar/backend/platform/crypto"
	httpplatform "lidradar/backend/platform/http"
	"lidradar/backend/platform/ids"
	"lidradar/backend/platform/postgres"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(bootstrap.Run(ctx, "lidradar-api", os.Stderr, run))
}

func run(ctx context.Context, configuration config.Config) error {
	pool, err := postgres.OpenAs(ctx, configuration.Database, postgres.RoleApp)
	if err != nil {
		return err
	}
	defer pool.Close()
	// Узлы AI и администрирование работают вне контекста организации (ADR 0034).
	platformPool, err := postgres.OpenAs(ctx, configuration.Database, postgres.RolePlatform)
	if err != nil {
		return err
	}
	defer platformPool.Close()
	logger := bootstrap.Logger(ctx)
	logger.Info("PostgreSQL готов", "event", "postgres.ready")

	auditRecorder := auditinfrastructure.NewPostgresRecorder(pool, ids.Generator{})
	identityRepository := identityinfrastructure.NewPostgresRepository(pool)
	tenantRepository := tenantinfrastructure.NewPostgresRepository(pool)
	catalogRepository := cataloginfrastructure.NewPostgresRepository(pool)
	connectorRepository := connectorinfrastructure.NewPostgresRepository(pool)
	conversationRepository := conversationinfrastructure.NewPostgresRepository(pool)
	opportunityRepository := opportunityinfrastructure.NewPostgresRepository(pool)
	notificationRepository := notificationinfrastructure.NewPostgresRepository(pool)
	aiStore := aiinfrastructure.NewPostgresStore(platformPool)
	aiService := aiapplication.NewService(aiStore, ids.Generator{}, time.Now, aiapplication.DefaultLease).
		WithSignatureWindow(configuration.AI.SignatureWindow).
		WithStaleJobBuilder(aiinfrastructure.NewPostgresAnalysisJobBuilder(platformPool, configuration.AI.ModelVersion))
	connectorRegistry := connectorinfrastructure.NewRegistry()
	connectorOptions := make([]connectorapplication.Option, 0, 1)
	if configuration.Integrations.PublicBaseURL != "" {
		credentialCipher, cipherErr := cryptoplatform.NewCredentialCipher(configuration.Integrations.CredentialKey)
		if cipherErr != nil {
			return cipherErr
		}
		connectorRegistry = connectorinfrastructure.NewRegistry(connectorinfrastructure.TelegramConfiguration{
			WebhookBaseURL: configuration.Integrations.PublicBaseURL,
		})
		connectorOptions = append(connectorOptions, connectorapplication.WithCredentialCipher(credentialCipher))
	}
	permissionService := tenantapplication.NewPermissionService(tenantRepository)
	riskStore := riskinfrastructure.NewPostgresRadarStore(pool)
	riskInvalidator := riskinfrastructure.NewPostgresInvalidator(pool)
	riskEvents := risktransport.NewHub()
	riskRadar := riskapplication.NewRadar(riskStore, permissionService, riskInvalidator, time.Now).WithAuditor(auditRecorder)
	tenantService := tenantapplication.NewService(tenantRepository, permissionService, ids.Generator{}, time.Now).WithAuditor(auditRecorder)
	catalogService := catalogapplication.NewService(catalogRepository, permissionService, ids.Generator{}, time.Now)
	connectorService := connectorapplication.NewService(
		connectorRepository, permissionService, connectorRegistry, ids.Generator{}, time.Now, connectorOptions...,
	).WithAuditor(auditRecorder)
	conversationService := conversationapplication.NewService(conversationRepository, permissionService, ids.Generator{})
	opportunityService := opportunityapplication.NewService(
		opportunityRepository, permissionService, ids.Generator{}, time.Now,
	).WithAuditor(auditRecorder)
	correctiveService := correctiveapplication.NewService(
		correctiveinfrastructure.NewPostgresStore(pool), permissionService, ids.Generator{}, time.Now,
	).WithInvalidator(riskInvalidator)
	revenueService := revenueapplication.NewService(
		revenueinfrastructure.NewPostgresStore(pool), permissionService, ids.Generator{}, time.Now,
	).WithInvalidator(riskInvalidator)
	identityService := identityapplication.NewService(
		identityRepository,
		identityinfrastructure.NewPostgresRateLimiter(pool),
		cryptoplatform.PasswordHasher{},
		ids.Generator{},
		identityinfrastructure.SessionTokens{},
		time.Now,
		configuration.Auth.SessionTTL,
	).WithAuditor(auditRecorder)
	principalResolver := identitytransport.Resolver{Auth: identityService}
	notificationLinks := notificationapplication.NewLinkService(
		notificationapplication.NewLinker(notificationRepository, ids.Generator{}, time.Now),
		notificationRepository, permissionService, configuration.Notifications.TelegramUsername, time.Now,
	)
	notificationPreferences := notificationapplication.NewPreferenceService(
		notificationRepository, permissionService, ids.Generator{}, time.Now,
	).WithAuditor(auditRecorder)
	go runRiskInvalidationRelay(ctx, logger, riskInvalidator, riskEvents)

	router := httpplatform.NewRouter(
		"lidradar-api", logger, postgres.NewSchemaReadiness(pool),
		httpplatform.WithAllowedOrigins(configuration.HTTP.AllowedOrigins),
		httpplatform.WithStrictTransport(configuration.Auth.CookieSecure),
		httpplatform.WithRateLimit(
			httpplatform.RateLimit{
				Requests: int(configuration.HTTP.RateLimitPerMinute), Window: time.Minute,
				Prefixes: []string{"/api/v1/auth/"},
			},
			httpplatform.RateLimit{
				Requests: int(configuration.HTTP.WebhookRateLimitPerMinute), Window: time.Minute,
				Prefixes: []string{"/api/v1/webhooks/"},
			},
		),
	)
	router.Mount("/api/v1/auth", identitytransport.NewHandler(
		identityService,
		tenantService,
		identitytransport.CookieConfiguration{Secure: configuration.Auth.CookieSecure, TTL: configuration.Auth.SessionTTL},
	).Router())
	router.Mount("/internal/v1/ai", aitransport.Handler{Service: aiService})
	router.Mount("/api/v1/services", catalogtransport.NewHandler(catalogService, principalResolver).Router())
	router.Mount("/api/v1/conversations", conversationtransport.NewHandler(conversationService, principalResolver).Router())
	router.Mount("/api/v1/opportunities", opportunitytransport.NewHandler(opportunityService, principalResolver).Router())
	router.Mount("/api/v1/notifications", notificationtransport.NewHandler(notificationLinks, notificationPreferences, principalResolver).Router())
	router.Mount("/api/v1/admin", admintransport.NewHandler(
		adminapplication.NewService(admininfrastructure.NewPostgresStore(platformPool), ids.Generator{}, time.Now), principalResolver,
	).Router())
	router.Mount("/api/v1/analytics", analyticstransport.NewHandler(
		analyticsapplication.NewService(analyticsinfrastructure.NewPostgresStore(pool), permissionService, time.Now), principalResolver,
	).Router())
	connectorHandler := connectortransport.NewHandler(connectorService, principalResolver)
	router.Mount("/api/v1/integrations", connectorHandler.ManagementRouter())
	router.Mount("/api/v1/webhooks", connectorHandler.WebhookRouter())
	router.Mount("/api/v1", tenanttransport.NewHandler(tenantService, principalResolver).Router())
	riskFeedback := riskapplication.NewFeedback(
		riskinfrastructure.NewPostgresFeedbackStore(pool), permissionService, ids.Generator{}, time.Now,
	).WithInvalidator(riskInvalidator).WithLeadCorrector(opportunityService)
	risktransport.NewHandler(riskRadar, principalResolver, riskEvents).WithFeedback(riskFeedback).RegisterRoutes(router, "/api/v1")
	correctivetransport.NewHandler(correctiveService, principalResolver).RegisterRoutes(router, "/api/v1")
	revenuetransport.NewHandler(revenueService, principalResolver).RegisterRoutes(router, "/api/v1")
	return httpplatform.Serve(ctx, configuration.HTTP, router, logger)
}
