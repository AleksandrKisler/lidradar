// Command api runs the LidRadar HTTP API process.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"lidradar/backend/platform/bootstrap"
	"lidradar/backend/platform/config"
	httpplatform "lidradar/backend/platform/http"
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
	router := httpplatform.NewRouter("lidradar-api", logger, pool)
	return httpplatform.Serve(ctx, configuration.HTTP, router, logger)
}
