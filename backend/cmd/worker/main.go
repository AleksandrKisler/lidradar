// Command worker запускает фоновые задания LidRadar.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	cataloginfrastructure "lidradar/backend/internal/catalog/infrastructure"
	connectorapplication "lidradar/backend/internal/connector/application"
	connectorinfrastructure "lidradar/backend/internal/connector/infrastructure"
	conversationapplication "lidradar/backend/internal/conversation/application"
	conversationinfrastructure "lidradar/backend/internal/conversation/infrastructure"
	eventsapplication "lidradar/backend/internal/events/application"
	eventsinfrastructure "lidradar/backend/internal/events/infrastructure"
	jobsapplication "lidradar/backend/internal/jobs/application"
	jobsinfrastructure "lidradar/backend/internal/jobs/infrastructure"
	opportunityapplication "lidradar/backend/internal/opportunity/application"
	opportunityinfrastructure "lidradar/backend/internal/opportunity/infrastructure"
	"lidradar/backend/platform/bootstrap"
	"lidradar/backend/platform/config"
	"lidradar/backend/platform/ids"
	"lidradar/backend/platform/postgres"
)

const (
	idleInterval  = 500 * time.Millisecond
	errorInterval = time.Second
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

	generator := ids.Generator{}
	ownerID, err := generator.NewID()
	if err != nil {
		return err
	}
	connectorRepository := connectorinfrastructure.NewPostgresRepository(pool)
	catalogRepository := cataloginfrastructure.NewPostgresRepository(pool)
	conversationRepository := conversationinfrastructure.NewPostgresRepository(pool)
	conversationService := conversationapplication.NewService(conversationRepository, nil, generator)
	opportunityRepository := opportunityinfrastructure.NewPostgresRepository(pool)
	candidateProcessor := opportunityapplication.NewCandidateProcessor(
		opportunityRepository, conversationService, catalogRepository, generator, time.Now,
	)
	normalization := connectorapplication.NewNormalizationService(
		connectorRepository, connectorinfrastructure.NewRegistry(), conversationService, time.Now,
	)
	jobStore := jobsinfrastructure.NewPostgresStore(pool)
	eventStore := eventsinfrastructure.NewPostgresStore(pool)

	dispatcher := eventsapplication.NewDispatcher(
		eventStore, ownerID+":outbox",
		map[string]eventsapplication.Handler{
			connectorapplication.NormalizationEventType:         connectorapplication.NormalizationEventHandler(jobStore, generator),
			opportunityapplication.ConversationChangedEventType: opportunityapplication.CandidateEventHandler(jobStore, generator),
		},
		time.Now, eventsapplication.DefaultLease,
	)
	worker := jobsapplication.NewWorker(
		jobStore, ownerID+":jobs",
		map[string]jobsapplication.Handler{
			connectorapplication.NormalizationJobType: connectorapplication.NormalizationJobHandler(normalization),
			opportunityapplication.CandidateJobType:   opportunityapplication.CandidateJobHandler(candidateProcessor),
		},
		time.Now, jobsapplication.DefaultLease,
	)

	for {
		dispatched, dispatchErr := dispatcher.RunOne(ctx)
		if dispatchErr != nil && ctx.Err() == nil {
			logger.Error("Ошибка исходящего журнала", "event", "outbox.failed", "error", dispatchErr)
		}
		processed, processErr := worker.RunOne(ctx)
		if processErr != nil && ctx.Err() == nil {
			logger.Error("Ошибка фонового задания", "event", "job.failed", "error", processErr)
		}
		if ctx.Err() != nil {
			return nil
		}
		if dispatchErr != nil || processErr != nil {
			if !wait(ctx, errorInterval) {
				return nil
			}
			continue
		}
		if dispatched || processed {
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
