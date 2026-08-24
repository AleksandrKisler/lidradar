// Command migrate applies LidRadar database migrations.
package main

import (
	"context"
	"os"

	"lidradar/backend/platform/bootstrap"
	"lidradar/backend/platform/config"
	"lidradar/backend/platform/postgres"
)

func main() {
	os.Exit(bootstrap.Run(context.Background(), "lidradar-migrate", os.Stderr, run))
}

func run(ctx context.Context, configuration config.Config) error {
	pool, err := postgres.Open(ctx, configuration.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := postgres.Migrate(ctx, pool); err != nil {
		return err
	}
	bootstrap.Logger(ctx).Info("database migrations applied", "event", "postgres.migrations.applied")
	return nil
}
