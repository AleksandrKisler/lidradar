// Command worker запускает фоновые задания LidRadar.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	aiapplication "lidradar/backend/internal/ai/application"
	aiinfrastructure "lidradar/backend/internal/ai/infrastructure"
	cataloginfrastructure "lidradar/backend/internal/catalog/infrastructure"
	connectorapplication "lidradar/backend/internal/connector/application"
	connectorinfrastructure "lidradar/backend/internal/connector/infrastructure"
	conversationapplication "lidradar/backend/internal/conversation/application"
	conversationinfrastructure "lidradar/backend/internal/conversation/infrastructure"
	eventsapplication "lidradar/backend/internal/events/application"
	eventsinfrastructure "lidradar/backend/internal/events/infrastructure"
	jobsapplication "lidradar/backend/internal/jobs/application"
	jobsinfrastructure "lidradar/backend/internal/jobs/infrastructure"
	mailapplication "lidradar/backend/internal/mail/application"
	maildomain "lidradar/backend/internal/mail/domain"
	mailinfrastructure "lidradar/backend/internal/mail/infrastructure"
	notificationapplication "lidradar/backend/internal/notification/application"
	notificationdomain "lidradar/backend/internal/notification/domain"
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

	os.Exit(bootstrap.Run(ctx, "lidradar-worker", os.Stderr, run, config.Config.ValidateWorker))
}

func run(ctx context.Context, configuration config.Config) error {
	pool, err := postgres.OpenAs(ctx, configuration.Database, postgres.RoleWorker)
	if err != nil {
		return err
	}
	defer pool.Close()
	// Захват заданий, исходящий журнал и доставки охватывают все организации (ADR 0034).
	platformPool, err := postgres.OpenAs(ctx, configuration.Database, postgres.RolePlatform)
	if err != nil {
		return err
	}
	defer platformPool.Close()
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
	claimStore := jobsinfrastructure.NewPostgresStore(platformPool)
	eventStore := eventsinfrastructure.NewPostgresStore(platformPool)
	aiStore := aiinfrastructure.NewPostgresStore(pool)
	aiBuilder := aiinfrastructure.NewPostgresAnalysisJobBuilder(pool, configuration.AI.ModelVersion)
	aiService := aiapplication.NewService(aiStore, generator, time.Now, aiapplication.DefaultLease).
		WithStaleJobBuilder(aiBuilder)
	riskRepository := riskinfrastructure.NewPostgresRepository(pool)
	riskStates := riskinfrastructure.NewPostgresStateReader(pool)
	riskPolicy := riskdomain.NoResponsePolicy{}
	bookingPolicy := riskdomain.BookingNotConfirmedPolicy{}
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
	bookingEvaluator := riskapplication.NewEvaluator(
		riskRepository, riskStates, bookingPolicy, generator, time.Now,
	).WithInvalidator(riskInvalidator)
	bookingPlanner := riskapplication.NewPlanner(
		riskStates, riskStates, jobStore, bookingEvaluator, bookingPolicy, generator, time.Now,
	)
	promisePolicy := riskdomain.PromiseNotFulfilledPolicy{}
	promiseEvaluator := riskapplication.NewEvaluator(
		riskRepository, riskStates, promisePolicy, generator, time.Now,
	).WithInvalidator(riskInvalidator)
	promisePlanner := riskapplication.NewPlanner(
		riskStates, riskStates, jobStore, promiseEvaluator, promisePolicy, generator, time.Now,
	)
	pricePolicy := riskdomain.CustomerSilentAfterPricePolicy{}
	priceEvaluator := riskapplication.NewEvaluator(
		riskRepository, riskStates, pricePolicy, generator, time.Now,
	).WithInvalidator(riskInvalidator)
	pricePlanner := riskapplication.NewPlanner(
		riskStates, riskStates, jobStore, priceEvaluator, pricePolicy, generator, time.Now,
	)
	followUpPolicy := riskdomain.FollowUpCandidatePolicy{}
	followUpEvaluator := riskapplication.NewEvaluator(
		riskRepository, riskStates, followUpPolicy, generator, time.Now,
	).WithInvalidator(riskInvalidator)
	followUpPlanner := riskapplication.NewPlanner(
		riskStates, riskStates, jobStore, followUpEvaluator, followUpPolicy, generator, time.Now,
	)
	agreementPolicy := riskdomain.UnfinishedAgreementPolicy{}
	agreementEvaluator := riskapplication.NewEvaluator(
		riskRepository, riskStates, agreementPolicy, generator, time.Now,
	).WithInvalidator(riskInvalidator)
	agreementPlanner := riskapplication.NewPlanner(
		riskStates, riskStates, jobStore, agreementEvaluator, agreementPolicy, generator, time.Now,
	)
	notificationRepository := notificationinfrastructure.NewPostgresRepository(pool)
	deliveryRepository := notificationinfrastructure.NewPostgresRepository(platformPool)
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
	).WithPolicy(notificationRepository, jobStore).WithEscalation(notificationdomain.EscalationPolicy{
		Enabled: configuration.Notifications.OwnerEscalationEnabled, After: configuration.Notifications.OwnerEscalationAfter,
	})
	deliveryService := notificationapplication.NewService(
		deliveryRepository, deliveryRepository, notificationTransport, generator, time.Now,
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
				aiapplication.ConversationChangedEventHandler(aiService, aiBuilder),
				riskapplication.ConversationChangedEventHandler(jobStore, generator),
			),
			aiapplication.AnalysisAppliedEventType: eventsapplication.ChainHandlers(
				opportunityapplication.AnalysisAppliedEventHandler(
					opportunityRepository, opportunityinfrastructure.NewPostgresSemanticFacts(pool), generator, time.Now,
				),
				riskapplication.AIAnalysisAppliedEventHandler(jobStore, generator),
			),
			riskapplication.OpportunityCreatedEventType: riskapplication.OpportunityEventHandler(jobStore, generator),
			riskapplication.OpportunityStageEventType:   riskapplication.OpportunityEventHandler(jobStore, generator),
			notificationapplication.RiskOpenedEventType: notificationapplication.RiskOpenedEventHandler(notificationService),
		},
		time.Now, eventsapplication.DefaultLease,
	)
	worker := jobsapplication.NewWorker(
		claimStore, ownerID+":jobs",
		map[string]jobsapplication.Handler{
			connectorapplication.NormalizationJobType:   connectorapplication.NormalizationJobHandler(normalization),
			opportunityapplication.CandidateJobType:     opportunityapplication.CandidateJobHandler(candidateProcessor),
			riskapplication.RefreshJobType:              riskapplication.RefreshPlansJobHandler(riskPlanner, bookingPlanner, promisePlanner, pricePlanner, followUpPlanner, agreementPlanner),
			riskapplication.NoResponseEvaluationJobType: riskapplication.EvaluationJobHandler(riskPlanner),
			riskapplication.BookingEvaluationJobType:    riskapplication.EvaluationJobHandler(bookingPlanner),
			riskapplication.PromiseEvaluationJobType:    riskapplication.EvaluationJobHandler(promisePlanner),
			riskapplication.PriceEvaluationJobType:      riskapplication.EvaluationJobHandler(pricePlanner),
			riskapplication.FollowUpEvaluationJobType:   riskapplication.EvaluationJobHandler(followUpPlanner),
			riskapplication.AgreementEvaluationJobType:  riskapplication.EvaluationJobHandler(agreementPlanner),
			notificationapplication.DigestJobType:       notificationapplication.DigestJobHandler(notificationService),
			notificationapplication.EscalationJobType:   notificationapplication.EscalationJobHandler(notificationService),
		},
		time.Now, jobsapplication.DefaultLease,
	)
	mailService, err := newMailService(ctx, configuration, pool, generator, logger)
	if err != nil {
		return err
	}
	// In-app доставка всегда локальна; Telegram подключается только с токеном.
	deliveryChannels := []notificationdomain.Channel{notificationdomain.ChannelInApp}
	if telegramEnabled {
		deliveryChannels = append(deliveryChannels, notificationdomain.ChannelTelegram)
	}

	// Каждая очередь живёт в своей цепочке (loops.go): медленный Telegram не задерживает разбор сообщений и вычисление рисков.
	runLoops(ctx, logger, workerPacing,
		loop{event: "outbox.failed", message: "Ошибка исходящего журнала", step: dispatcher.RunOne},
		loop{event: "job.failed", message: "Ошибка фонового задания", step: worker.RunOne},
		loop{event: "notification.delivery_failed", message: "Ошибка доставки уведомления", step: func(ctx context.Context) (bool, error) {
			return deliveryService.DispatchOne(ctx, ownerID+":notifications", notificationapplication.DefaultDeliveryLease, deliveryChannels...)
		}},
		// Письма учётной записи (ADR 0058): ссылка сброса теряет смысл через час, поэтому медленный SMTP не должен
		// стоять в очереди за Telegram.
		loop{event: "mail.delivery_failed", message: "Ошибка доставки письма", step: func(ctx context.Context) (bool, error) {
			return mailService.DispatchOne(ctx, ownerID+":mail", maildomain.DefaultLease)
		}},
		loop{event: "diagnostics.failed", message: "Ошибка диагностики очередей", every: diagnosticInterval, step: func(ctx context.Context) (bool, error) {
			now := time.Now().UTC()
			logQueueStats(ctx, logger, claimStore, now)
			logNotificationStats(ctx, logger, deliveryRepository, now)
			logMailStats(ctx, logger, mailService)
			return false, nil
		}},
	)
	return nil
}

// newMailService выбирает транспорт писем. В staging и production только SMTP (ValidateWorker отказывает в запуске
// без него); без SMTP-хоста в разработке и тестах письма с ссылками пишутся в журнал воркера.
func newMailService(
	ctx context.Context,
	configuration config.Config,
	pool *pgxpool.Pool,
	generator ids.Generator,
	logger *slog.Logger,
) (mailapplication.Service, error) {
	var transport mailapplication.Transport
	smtp := configuration.Mail.SMTP
	switch {
	case smtp.Host != "":
		smtpTransport, err := mailinfrastructure.NewSMTPTransport(mailinfrastructure.SMTPConfig{
			Host: smtp.Host, Port: smtp.Port, Security: string(smtp.Security), Username: smtp.Username, Password: smtp.Password,
			FromAddress: configuration.Mail.From, FromName: configuration.Mail.FromName,
		})
		if err != nil {
			return mailapplication.Service{}, err
		}
		transport = smtpTransport
		// Проверка без отправки: ошибка в реквизитах видна сразу при запуске, а не при первом сбросе пароля.
		go func() {
			checkContext, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := smtpTransport.Check(checkContext); err != nil {
				logger.Warn("SMTP недоступен или реквизиты не приняты", "event", "mail.smtp.check_failed", "error", err)
				return
			}
			logger.Info("SMTP принимает соединение и вход", "event", "mail.smtp.ready", "host", smtp.Host, "port", smtp.Port)
		}()
	case configuration.Environment == config.EnvironmentStaging || configuration.Environment == config.EnvironmentProduction:
		return mailapplication.Service{}, errors.New("SMTP is required to deliver mail in " + string(configuration.Environment))
	default:
		logger.Warn(
			"SMTP не настроен: письма записываются в журнал воркера вместе со ссылками (только для разработки)",
			"event", "mail.logged_transport",
		)
		transport = mailinfrastructure.LogTransport{Logger: logger}
	}
	return mailapplication.NewService(mailinfrastructure.NewPostgresStore(pool), transport, generator, time.Now).WithReporter(logger), nil
}

func logMailStats(ctx context.Context, logger *slog.Logger, service mailapplication.Service) {
	stats, err := service.Stats(ctx)
	if err != nil {
		logger.Warn("Не удалось прочитать состояние очереди писем", "event", "mail.queue.diagnostics_failed", "error", err)
		return
	}
	logger.Info(
		"Состояние очереди писем",
		"event", "mail.queue.status",
		"mail_pending", stats.Pending,
		"mail_retry", stats.Retry,
		"mail_processing", stats.Processing,
		"mail_dead_24h", stats.Dead,
		"mail_overdue", stats.Overdue,
		"mail_expired_leases", stats.ExpiredLeases,
	)
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
