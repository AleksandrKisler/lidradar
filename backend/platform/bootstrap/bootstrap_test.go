package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestRunSuccess(t *testing.T) {
	var stderr bytes.Buffer

	if code := Run(context.Background(), "test-service", &stderr, Complete); code != 0 {
		t.Fatalf("Run() code = %d, want 0", code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run() stderr = %q, want empty", stderr.String())
	}
}

func TestRunFailure(t *testing.T) {
	var stderr bytes.Buffer
	want := errors.New("startup failed")

	code := Run(context.Background(), "test-service", &stderr, func(context.Context) error {
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
