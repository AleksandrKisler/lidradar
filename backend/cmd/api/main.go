// Command api запускает HTTP API LidRadar.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	catalogapplication "lidradar/backend/internal/catalog/application"
	cataloginfrastructure "lidradar/backend/internal/catalog/infrastructure"
	catalogtransport "lidradar/backend/internal/catalog/transport"
	connectorapplication "lidradar/backend/internal/connector/application"
	connectorinfrastructure "lidradar/backend/internal/connector/infrastructure"
	connectortransport "lidradar/backend/internal/connector/transport"
	conversationapplication "lidradar/backend/internal/conversation/application"
	conversationinfrastructure "lidradar/backend/internal/conversation/infrastructure"
	conversationtransport "lidradar/backend/internal/conversation/transport"
	identityapplication "lidradar/backend/internal/identity/application"
	identityinfrastructure "lidradar/backend/internal/identity/infrastructure"
	identitytransport "lidradar/backend/internal/identity/transport"
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
	pool, err := postgres.Open(ctx, configuration.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	logger := bootstrap.Logger(ctx)
	logger.Info("PostgreSQL готов", "event", "postgres.ready")

	identityRepository := identityinfrastructure.NewPostgresRepository(pool)
	tenantRepository := tenantinfrastructure.NewPostgresRepository(pool)
	catalogRepository := cataloginfrastructure.NewPostgresRepository(pool)
	connectorRepository := connectorinfrastructure.NewPostgresRepository(pool)
	conversationRepository := conversationinfrastructure.NewPostgresRepository(pool)
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
	tenantService := tenantapplication.NewService(tenantRepository, permissionService, ids.Generator{}, time.Now)
	catalogService := catalogapplication.NewService(catalogRepository, permissionService, ids.Generator{}, time.Now)
	connectorService := connectorapplication.NewService(
		connectorRepository, permissionService, connectorRegistry, ids.Generator{}, time.Now, connectorOptions...,
	)
	conversationService := conversationapplication.NewService(conversationRepository, permissionService, ids.Generator{})
	identityService := identityapplication.NewService(
		identityRepository,
		cryptoplatform.PasswordHasher{},
		ids.Generator{},
		identityinfrastructure.SessionTokens{},
		time.Now,
		configuration.Auth.SessionTTL,
	)
	principalResolver := identitytransport.Resolver{Auth: identityService}

	router := httpplatform.NewRouter(
		"lidradar-api", logger, pool,
		httpplatform.WithAllowedOrigins(configuration.HTTP.AllowedOrigins),
	)
	router.Mount("/api/v1/auth", identitytransport.NewHandler(
		identityService,
		tenantService,
		identitytransport.CookieConfiguration{Secure: configuration.Auth.CookieSecure, TTL: configuration.Auth.SessionTTL},
	).Router())
	router.Mount("/api/v1/services", catalogtransport.NewHandler(catalogService, principalResolver).Router())
	router.Mount("/api/v1/conversations", conversationtransport.NewHandler(conversationService, principalResolver).Router())
	connectorHandler := connectortransport.NewHandler(connectorService, principalResolver)
	router.Mount("/api/v1/integrations", connectorHandler.ManagementRouter())
	router.Mount("/api/v1/webhooks", connectorHandler.WebhookRouter())
	router.Mount("/api/v1", tenanttransport.NewHandler(tenantService, principalResolver).Router())
	return httpplatform.Serve(ctx, configuration.HTTP, router, logger)
}
