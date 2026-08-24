// Package observability owns process-wide logging and correlation primitives.
package observability

import (
	"context"
	"io"
	"log/slog"
)

type loggerKey struct{}

// NewLogger returns the JSON logger shared by one runtime process.
func NewLogger(output io.Writer, service, environment string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: slog.LevelInfo})).With(
		"service", service,
		"environment", environment,
	)
}

// WithLogger adds a runtime logger to a context.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}

// Logger returns the contextual runtime logger or slog.Default.
func Logger(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return slog.Default()
}
