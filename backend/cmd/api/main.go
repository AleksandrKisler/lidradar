// Command api runs the LidRadar HTTP API process.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	logger.Info("PostgreSQL ready", "event", "postgres.ready")

	identityRepository := identityinfrastructure.NewPostgresRepository(pool)
	tenantRepository := tenantinfrastructure.NewPostgresRepository(pool)
	permissionService := tenantapplication.NewPermissionService(tenantRepository)
	tenantService := tenantapplication.NewService(tenantRepository, permissionService, ids.Generator{}, time.Now)
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
	router.Mount("/api/v1", tenanttransport.NewHandler(tenantService, principalResolver).Router())
	return httpplatform.Serve(ctx, configuration.HTTP, router, logger)
}
