package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"lidradar/backend/platform/config"
)

func TestRunSuccess(t *testing.T) {
	t.Setenv("LIDRADAR_ENV", "test")
	var stderr bytes.Buffer

	if code := Run(context.Background(), "test-service", &stderr, Complete); code != 0 {
		t.Fatalf("Run() code = %d, want 0", code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run() stderr = %q, want empty", stderr.String())
	}
}

func TestRunFailure(t *testing.T) {
	t.Setenv("LIDRADAR_ENV", "test")
	var stderr bytes.Buffer
	want := errors.New("startup failed")

	code := Run(context.Background(), "test-service", &stderr, func(context.Context, config.Config) error {
		return want
	})

	if code != 1 {
		t.Fatalf("Run() code = %d, want 1", code)
	}
	if got := stderr.String(); got != "test-service: startup failed\n" {
		t.Fatalf("Run() stderr = %q", got)
	}
}

func TestWaitTreatsCancellationAsGracefulShutdown(t *testing.T) {
	t.Setenv("LIDRADAR_ENV", "test")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stderr bytes.Buffer

	if code := Run(ctx, "test-service", &stderr, Wait); code != 0 {
		t.Fatalf("Run() code = %d, want 0", code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run() stderr = %q, want empty", stderr.String())
	}
}

func TestRunRejectsInvalidConfigurationBeforeStartingWorkload(t *testing.T) {
	t.Setenv("LIDRADAR_ENV", "invalid")
	var stderr bytes.Buffer
	started := false

	code := Run(context.Background(), "test-service", &stderr, func(context.Context, config.Config) error {
		started = true
		return nil
	})

	if code != 1 {
		t.Fatalf("Run() code = %d, want 1", code)
	}
	if started {
		t.Fatal("Run() started workload with invalid configuration")
	}
	if !strings.Contains(stderr.String(), "invalid configuration") {
		t.Fatalf("Run() stderr = %q, want configuration error", stderr.String())
	}
}
