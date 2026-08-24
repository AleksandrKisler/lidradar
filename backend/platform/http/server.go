package httpplatform

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"lidradar/backend/platform/config"
)

// Serve runs an HTTP server until cancellation and drains active requests on shutdown.
func Serve(ctx context.Context, configuration config.HTTP, handler http.Handler, logger *slog.Logger) error {
	server := &http.Server{
		Addr:              configuration.Address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	result := make(chan error, 1)
	go func() {
		if logger != nil {
			logger.Info("HTTP server listening", "event", "http.server.started", "address", configuration.Address)
		}
		result <- server.ListenAndServe()
	}()

	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), configuration.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return err
		}
		if err := <-result; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return ctx.Err()
	}
}
