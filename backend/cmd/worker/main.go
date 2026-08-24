// Command worker запускает фоновые задания LidRadar.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	connectorapplication "lidradar/backend/internal/connector/application"
	connectorinfrastructure "lidradar/backend/internal/connector/infrastructure"
	conversationapplication "lidradar/backend/internal/conversation/application"
	conversationinfrastructure "lidradar/backend/internal/conversation/infrastructure"
	"lidradar/backend/platform/bootstrap"
	"lidradar/backend/platform/config"
	"lidradar/backend/platform/ids"
	"lidradar/backend/platform/postgres"
)

const (
	normalizationBatch = 50
	idleInterval       = 500 * time.Millisecond
	errorInterval      = time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(bootstrap.Run(ctx, "lidradar-worker", os.Stderr, run))
}

func run(ctx context.Context, configuration config.Config) error {
	pool, err := postgres.Open(ctx, configuration.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	logger := bootstrap.Logger(ctx)
	logger.Info("PostgreSQL готов", "event", "postgres.ready")

	connectorRepository := connectorinfrastructure.NewPostgresRepository(pool)
	conversationRepository := conversationinfrastructure.NewPostgresRepository(pool)
	conversationService := conversationapplication.NewService(conversationRepository, nil, ids.Generator{})
	normalization := connectorapplication.NewNormalizationService(
		connectorRepository, connectorinfrastructure.NewRegistry(), conversationService, time.Now,
	)

	for {
		processed, processErr := normalization.ProcessBatch(ctx, normalizationBatch)
		if processErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			logger.Error("Ошибка канонизации", "event", "normalization.failed", "error", processErr)
			if !wait(ctx, errorInterval) {
				return nil
			}
			continue
		}
		if processed == normalizationBatch {
			continue
		}
		if !wait(ctx, idleInterval) {
			return nil
		}
	}
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
