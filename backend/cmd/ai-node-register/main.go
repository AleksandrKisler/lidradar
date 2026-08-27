// Command ai-node-register safely registers a disposable home AI node and
// writes its only plaintext credentials copy to an owner-only file.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/infrastructure"
	"lidradar/backend/platform/bootstrap"
	"lidradar/backend/platform/config"
	"lidradar/backend/platform/ids"
	"lidradar/backend/platform/postgres"
)

func main() {
	flags := flag.NewFlagSet("ai-node-register", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	name := flags.String("name", "AI-NODE-01", "понятное имя домашнего AI-узла")
	output := flags.String("output", "", "новый файл реквизитов с правами 0600")
	if err := flags.Parse(os.Args[1:]); err != nil || strings.TrimSpace(*output) == "" {
		if err == nil {
			fmt.Fprintln(os.Stderr, "обязателен параметр --output")
		}
		os.Exit(2)
	}
	ctx := context.Background()
	os.Exit(bootstrap.Run(ctx, "lidradar-ai-node-register", os.Stderr, func(ctx context.Context, configuration config.Config) error {
		return run(ctx, configuration, strings.TrimSpace(*name), filepath.Clean(*output))
	}))
}

func run(ctx context.Context, configuration config.Config, name, output string) error {
	if output == "." || name == "" {
		return errors.New("AI node name and credentials output are required")
	}
	if _, err := os.Lstat(output); err == nil {
		return errors.New("AI node credentials output already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect AI node credentials output: %w", err)
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return errors.New("generate AI node secret")
	}
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)
	defer func() {
		secret = ""
		for index := range secretBytes {
			secretBytes[index] = 0
		}
	}()
	generator := ids.Generator{}
	nodeID, err := generator.NewID()
	if err != nil {
		return errors.New("generate AI node ID")
	}
	if err := infrastructure.WriteNodeCredentials(output, infrastructure.NodeCredentials{
		NodeID: nodeID, NodeSecret: secret,
	}); err != nil {
		return err
	}
	keepCredentials := false
	defer func() {
		if !keepCredentials {
			_ = os.Remove(output)
		}
	}()
	pool, err := postgres.Open(ctx, configuration.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	service := application.NewService(infrastructure.NewPostgresStore(pool), generator, time.Now, application.DefaultLease)
	node, err := service.RegisterNodeWithID(ctx, nodeID, name, secret)
	if err != nil {
		return err
	}
	keepCredentials = true
	bootstrap.Logger(ctx).Info(
		"AI node registered", "event", "ai.node.registered",
		"node_id", node.ID, "credentials_file", output,
	)
	return nil
}
