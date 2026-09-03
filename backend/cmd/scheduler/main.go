// Command scheduler переносит наступившие проверки в общую очередь LidRadar.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	jobsapplication "lidradar/backend/internal/jobs/application"
	jobsinfrastructure "lidradar/backend/internal/jobs/infrastructure"
	"lidradar/backend/platform/bootstrap"
	"lidradar/backend/platform/config"
	"lidradar/backend/platform/postgres"
)

const (
	schedulerBatch = 100
	idleInterval   = 500 * time.Millisecond
	errorInterval  = time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(bootstrap.Run(ctx, "lidradar-scheduler", os.Stderr, run))
}

func run(ctx context.Context, configuration config.Config) error {
	pool, err := postgres.OpenAs(ctx, configuration.Database, postgres.RolePlatform)
	if err != nil {
		return err
	}
	defer pool.Close()
	logger := bootstrap.Logger(ctx)
	logger.Info("PostgreSQL готов", "event", "postgres.ready")
	scheduler := jobsapplication.NewScheduler(jobsinfrastructure.NewPostgresStore(pool), time.Now)

	for {
		promoted, promoteErr := scheduler.RunOnce(ctx, schedulerBatch)
		if promoteErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			logger.Error("Ошибка планировщика", "event", "scheduler.failed", "error", promoteErr)
			if !wait(ctx, errorInterval) {
				return nil
			}
			continue
		}
		if promoted == schedulerBatch {
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
