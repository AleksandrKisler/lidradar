// Command ai-agent runs the outbound AI node agent.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"lidradar/backend/internal/ai/agent"
	"lidradar/backend/internal/ai/infrastructure"
	"lidradar/backend/platform/bootstrap"
	"lidradar/backend/platform/config"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(bootstrap.Run(ctx, "lidradar-ai-agent", os.Stderr, run))
}

func run(ctx context.Context, _ config.Config) error {
	bootstrap.Logger(ctx).Info("AI agent using local stubs", "event", "ai.stub.enabled")
	return (agent.Runner{
		Cloud:        infrastructure.StubCloud{},
		Provider:     infrastructure.FakeProvider{},
		PollInterval: time.Second,
	}).Run(ctx)
}
