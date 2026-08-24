// Package runtime contains composition helpers shared by executable commands.
package runtime

import (
	"context"

	"lidradar/backend/platform/bootstrap"
	"lidradar/backend/platform/config"
	"lidradar/backend/platform/postgres"
)

// WaitForDatabase verifies PostgreSQL at startup and then runs until cancelled.
func WaitForDatabase(ctx context.Context, configuration config.Config) error {
	pool, err := postgres.Open(ctx, configuration.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	bootstrap.Logger(ctx).Info("PostgreSQL ready", "event", "postgres.ready")
	return bootstrap.Wait(ctx, configuration)
}
