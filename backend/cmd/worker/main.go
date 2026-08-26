// Command worker запускает фоновые задания LidRadar.
package main

import (
	"context"
	"net/http"
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
	notificationapplication "lidradar/backend/internal/notification/application"
	notificationinfrastructure "lidradar/backend/internal/notification/infrastructure"
	opportunityapplication "lidradar/backend/internal/opportunity/application"
	opportunityinfrastructure "lidradar/backend/internal/opportunity/infrastructure"
	riskapplication "lidradar/backend/internal/risk/application"
	riskdomain "lidradar/backend/internal/risk/domain"
	riskinfrastructure "lidradar/backend/internal/risk/infrastructure"
	tenantapplication "lidradar/backend/internal/tenant/application"
	tenantinfrastructure "lidradar/backend/internal/tenant/infrastructure"
	"lidradar/backend/platform/bootstrap"
	"lidradar/backend/platform/config"
	"lidradar/backend/platform/ids"
	"lidradar/backend/platform/postgres"
)

const (
	idleInterval       = 500 * time.Millisecond
	errorInterval      = time.Second
	diagnosticInterval = time.Minute
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
	jobStore := jobsinfrastructure.NewPostgresStore(pool)
	eventStore := eventsinfrastructure.NewPostgresStore(pool)
	riskRepository := riskinfrastructure.NewPostgresRepository(pool)
	riskStates := riskinfrastructure.NewPostgresStateReader(pool)
	riskPolicy := riskdomain.NoResponsePolicy{}
	riskInvalidator := riskinfrastructure.NewPostgresInvalidator(pool)
	tenantRepository := tenantinfrastructure.NewPostgresRepository(pool)
	permissionService := tenantapplication.NewPermissionService(tenantRepository)
	riskRadar := riskapplication.NewRadar(
		riskinfrastructure.NewPostgresRadarStore(pool), permissionService, riskInvalidator, time.Now,
	)
	riskEvaluator := riskapplication.NewEvaluator(
		riskRepository, riskStates, riskPolicy, generator, time.Now,
	).WithInvalidator(riskInvalidator)
	riskPlanner := riskapplication.NewPlanner(
		riskStates, riskStates, jobStore, riskEvaluator, riskPolicy, generator, time.Now,
	)
	notificationRepository := notificationinfrastructure.NewPostgresRepository(pool)
	var notificationTransport notificationapplication.Transport
	var controlTransport notificationinfrastructure.TelegramControlTransport
	telegramEnabled := configuration.Notifications.TelegramBotToken != ""
	if telegramEnabled {
		telegram := notificationinfrastructure.TelegramTransport{
			BaseURL: "https://api.telegram.org", BotToken: configuration.Notifications.TelegramBotToken,
			Client: &http.Client{Timeout: 10 * time.Second},
		}
		notificationTransport = telegram
		controlTransport = telegram
	} else {
		logger.Warn(
			"Telegram-доставка не настроена; уведомления останутся ожидающими",
			"event", "notification.telegram.disabled",
		)
	}
	notificationService := notificationapplication.NewService(
		notificationRepository, notificationRepository, notificationTransport, generator, time.Now,
	)
	linker := notificationapplication.NewLinker(notificationRepository, generator, time.Now)
	callbackExecutor := notificationapplication.NewSafeCallbackExecutor(
		notificationRepository, riskRadar, generator, time.Now,
	)
	controlHandler := notificationinfrastructure.NewTelegramControlHandler(linker, callbackExecutor, controlTransport)
	normalization := connectorapplication.NewNormalizationService(
		connectorRepository, connectorinfrastructure.NewRegistry(), conversationService, time.Now,
	).WithControlSink(controlHandler)

	dispatcher := eventsapplication.NewDispatcher(
		eventStore, ownerID+":outbox",
		map[string]eventsapplication.Handler{
			connectorapplication.NormalizationEventType: connectorapplication.NormalizationEventHandler(jobStore, generator),
			opportunityapplication.ConversationChangedEventType: eventsapplication.ChainHandlers(
				opportunityapplication.CandidateEventHandler(jobStore, generator),
				riskapplication.ConversationChangedEventHandler(jobStore, generator),
			),
			riskapplication.OpportunityCreatedEventType: riskapplication.OpportunityEventHandler(jobStore, generator),
			riskapplication.OpportunityStageEventType:   riskapplication.OpportunityEventHandler(jobStore, generator),
			notificationapplication.RiskOpenedEventType: notificationapplication.RiskOpenedEventHandler(
				notificationService, notificationRepository,
			),
		},
		time.Now, eventsapplication.DefaultLease,
	)
	worker := jobsapplication.NewWorker(
		jobStore, ownerID+":jobs",
		map[string]jobsapplication.Handler{
			connectorapplication.NormalizationJobType:   connectorapplication.NormalizationJobHandler(normalization),
			opportunityapplication.CandidateJobType:     opportunityapplication.CandidateJobHandler(candidateProcessor),
			riskapplication.RefreshJobType:              riskapplication.RefreshJobHandler(riskPlanner),
			riskapplication.NoResponseEvaluationJobType: riskapplication.EvaluationJobHandler(riskEvaluator),
		},
		time.Now, jobsapplication.DefaultLease,
	)
	nextDiagnostics := time.Time{}

	for {
		now := time.Now().UTC()
		if !now.Before(nextDiagnostics) {
			logQueueStats(ctx, logger, jobStore, now)
			logNotificationStats(ctx, logger, notificationRepository, now)
			nextDiagnostics = now.Add(diagnosticInterval)
		}
		dispatched, dispatchErr := dispatcher.RunOne(ctx)
		if dispatchErr != nil && ctx.Err() == nil {
			logger.Error("Ошибка исходящего журнала", "event", "outbox.failed", "error", dispatchErr)
		}
		processed, processErr := worker.RunOne(ctx)
		if processErr != nil && ctx.Err() == nil {
			logger.Error("Ошибка фонового задания", "event", "job.failed", "error", processErr)
		}
		delivered := false
		var deliveryErr error
		if telegramEnabled {
			delivered, deliveryErr = notificationService.DispatchOne(ctx, ownerID+":notifications", notificationapplication.DefaultDeliveryLease)
			if deliveryErr != nil && ctx.Err() == nil {
				logger.Error("Ошибка доставки уведомления", "event", "notification.delivery_failed", "error", deliveryErr)
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		if dispatchErr != nil || processErr != nil || deliveryErr != nil {
			if !wait(ctx, errorInterval) {
				return nil
			}
			continue
		}
		if dispatched || processed || delivered {
			continue
		}
		if !wait(ctx, idleInterval) {
			return nil
		}
	}
}

func logNotificationStats(
	ctx context.Context,
	logger interface {
		Info(string, ...any)
		Warn(string, ...any)
	},
	store *notificationinfrastructure.PostgresRepository,
	at time.Time,
) {
	stats, err := store.DeliveryStats(ctx, at)
	if err != nil {
		logger.Warn("Не удалось прочитать состояние доставок", "event", "notification.queue.diagnostics_failed", "error", err)
		return
	}
	logger.Info(
		"Состояние очереди уведомлений",
		"event", "notification.queue.status",
		"deliveries_pending", stats.Pending,
		"deliveries_processing", stats.Processing,
		"deliveries_retry", stats.Retry,
		"deliveries_dead", stats.Dead,
		"deliveries_expired_leases", stats.ExpiredLeases,
		"deliveries_overdue", stats.Overdue,
	)
}

func logQueueStats(
	ctx context.Context,
	logger interface {
		Info(string, ...any)
		Warn(string, ...any)
	},
	store *jobsinfrastructure.PostgresStore,
	at time.Time,
) {
	stats, err := store.QueueStats(ctx, at)
	if err != nil {
		logger.Warn("Не удалось прочитать состояние очереди", "event", "background.queue.diagnostics_failed", "error", err)
		return
	}
	logger.Info(
		"Состояние очереди фоновых заданий",
		"event", "background.queue.status",
		"jobs_pending", stats.Pending,
		"jobs_processing", stats.Processing,
		"jobs_retry", stats.Retry,
		"jobs_dead", stats.Dead,
		"jobs_expired_leases", stats.ExpiredLeases,
		"scheduled_checks_overdue", stats.OverdueScheduled,
	)
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
