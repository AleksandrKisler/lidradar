// Package bootstrap provides the minimal lifecycle shared by LidRadar runtime
// commands. Configuration, logging, and runtime-specific behavior are added by
// their owning platform packages rather than being hidden in command mains.
package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"os"

	"lidradar/backend/platform/config"
	"lidradar/backend/platform/observability"
)

// Workload is the process-specific function run by a LidRadar command.
type Workload func(context.Context, config.Config) error

// Run executes a workload and translates its result into a process exit code.
// Cancellation is a normal, graceful shutdown.
func Run(ctx context.Context, service string, stderr io.Writer, workload Workload) int {
	logger := observability.NewLogger(stderr, service, "unknown")
	configuration, err := config.Load(os.LookupEnv)
	if err != nil {
		logger.Error("invalid configuration", "event", "runtime.configuration_invalid", "error", err)
		return 1
	}
	logger = observability.NewLogger(stderr, service, string(configuration.Environment))
	ctx = observability.WithLogger(ctx, logger)
	logger.Info("runtime starting", "event", "runtime.starting")

	if err := workload(ctx, configuration); err != nil && ctx.Err() == nil {
		logger.Error("runtime failed", "event", "runtime.failed", "error", err)
		return 1
	}

	logger.Info("runtime stopped", "event", "runtime.stopped")
	return 0
}

// Wait keeps a long-running process alive until its context is cancelled.
func Wait(ctx context.Context, _ config.Config) error {
	<-ctx.Done()
	return ctx.Err()
}

// Complete is the workload for a command that has no long-running loop.
func Complete(context.Context, config.Config) error { return nil }

// Logger exposes the contextual structured logger to runtime workloads.
func Logger(ctx context.Context) *slog.Logger { return observability.Logger(ctx) }
