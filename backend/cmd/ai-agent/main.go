// Command ai-agent runs the outbound AI node agent.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"lidradar/backend/internal/ai/agent"
	"lidradar/backend/internal/ai/infrastructure"
	"lidradar/backend/platform/bootstrap"
	"lidradar/backend/platform/config"
	"lidradar/backend/platform/ids"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(bootstrap.Run(ctx, "lidradar-ai-agent", os.Stderr, run))
}

func run(ctx context.Context, configuration config.Config) error {
	if configuration.AI.CloudURL == "" || configuration.AI.CredentialsFile == "" {
		return errors.New("LIDRADAR_AI_CLOUD_URL and LIDRADAR_AI_CREDENTIALS_FILE are required")
	}
	credentials, err := infrastructure.LoadNodeCredentials(configuration.AI.CredentialsFile)
	if err != nil {
		return err
	}
	if configuration.AI.Provider == "fake" &&
		(configuration.Environment == config.EnvironmentStaging || configuration.Environment == config.EnvironmentProduction) {
		return errors.New("fake AI provider is forbidden outside development and tests")
	}
	providerClient := &http.Client{Timeout: configuration.AI.HTTPTimeout}
	var provider agent.Provider
	switch configuration.AI.Provider {
	case "fake":
		provider = infrastructure.FakeProvider{}
	case "llama":
		provider = infrastructure.LlamaProvider{
			URL: configuration.AI.LlamaURL, Model: configuration.AI.ModelVersion,
			Client: providerClient,
		}
	default:
		return fmt.Errorf("unsupported AI provider")
	}
	cloudTimeout := 30 * time.Second
	if configuration.AI.HTTPTimeout < cloudTimeout {
		cloudTimeout = configuration.AI.HTTPTimeout
	}
	cloud := infrastructure.CloudClient{
		BaseURL: configuration.AI.CloudURL, NodeID: credentials.NodeID,
		Secret: credentials.NodeSecret, Client: &http.Client{Timeout: cloudTimeout},
		IDs: ids.Generator{}, Now: time.Now,
	}
	logger := bootstrap.Logger(ctx)
	logger.Info("AI agent configured", "event", "ai.agent.configured", "provider", configuration.AI.Provider, "model", configuration.AI.ModelVersion)
	return (agent.Runner{
		Cloud: cloud, Provider: provider, ModelVersion: configuration.AI.ModelVersion,
		PollInterval:      configuration.AI.PollInterval,
		HeartbeatInterval: configuration.AI.HeartbeatInterval,
		OnError: func(err error) {
			logger.Warn("AI agent operation failed", "event", "ai.agent.operation_failed", "error", err)
		},
	}).Run(ctx)
}
