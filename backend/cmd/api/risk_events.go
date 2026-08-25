package main

import (
	"context"
	"log/slog"
	"time"

	riskinfrastructure "lidradar/backend/internal/risk/infrastructure"
	risktransport "lidradar/backend/internal/risk/transport"
)

const riskInvalidationReconnectDelay = time.Second

func runRiskInvalidationRelay(
	ctx context.Context,
	logger *slog.Logger,
	notifier *riskinfrastructure.PostgresInvalidator,
	hub *risktransport.Hub,
) {
	for ctx.Err() == nil {
		err := notifier.Listen(ctx, hub, nil)
		if ctx.Err() != nil {
			return
		}
		logger.Warn(
			"Поток уведомлений Risk разорван, выполняется переподключение",
			"event", "risk.invalidation.reconnect", "error", err,
		)
		timer := time.NewTimer(riskInvalidationReconnectDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
