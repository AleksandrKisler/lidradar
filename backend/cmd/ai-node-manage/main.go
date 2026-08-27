// Command ai-node-manage rotates or revokes credentials of a disposable AI
// node without printing the plaintext secret.
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
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	action := os.Args[1]
	flags := flag.NewFlagSet("ai-node-manage "+action, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	nodeID := flags.String("node-id", "", "идентификатор AI-узла")
	var output *string
	if action == "rotate" {
		output = flags.String("output", "", "новый файл реквизитов с правами 0600")
	} else if action != "revoke" {
		usage()
		os.Exit(2)
	}
	if err := flags.Parse(os.Args[2:]); err != nil || !ids.Valid(strings.TrimSpace(*nodeID)) || flags.NArg() != 0 ||
		(action == "rotate" && strings.TrimSpace(*output) == "") {
		if err == nil {
			fmt.Fprintln(os.Stderr, "обязательны корректные параметры команды")
		}
		os.Exit(2)
	}

	ctx := context.Background()
	os.Exit(bootstrap.Run(ctx, "lidradar-ai-node-manage", os.Stderr, func(ctx context.Context, configuration config.Config) error {
		if action == "rotate" {
			return rotate(ctx, configuration, strings.TrimSpace(*nodeID), filepath.Clean(*output))
		}
		return revoke(ctx, configuration, strings.TrimSpace(*nodeID))
	}))
}

func rotate(ctx context.Context, configuration config.Config, nodeID, output string) error {
	if output == "." {
		return errors.New("AI node credentials output is required")
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
	service, closePool, err := nodeService(ctx, configuration)
	if err != nil {
		return err
	}
	defer closePool()
	if err := service.RotateNodeSecret(ctx, nodeID, secret); err != nil {
		return err
	}
	keepCredentials = true
	bootstrap.Logger(ctx).Info(
		"AI node secret rotated", "event", "ai.node.secret_rotated",
		"node_id", nodeID, "credentials_file", output,
	)
	return nil
}

func revoke(ctx context.Context, configuration config.Config, nodeID string) error {
	service, closePool, err := nodeService(ctx, configuration)
	if err != nil {
		return err
	}
	defer closePool()
	if err := service.RevokeNode(ctx, nodeID); err != nil {
		return err
	}
	bootstrap.Logger(ctx).Info("AI node revoked", "event", "ai.node.revoked", "node_id", nodeID)
	return nil
}

func nodeService(ctx context.Context, configuration config.Config) (application.Service, func(), error) {
	pool, err := postgres.Open(ctx, configuration.Database)
	if err != nil {
		return application.Service{}, func() {}, err
	}
	return application.NewService(
		infrastructure.NewPostgresStore(pool), ids.Generator{}, time.Now, application.DefaultLease,
	), pool.Close, nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "использование:")
	fmt.Fprintln(os.Stderr, "  ai-node-manage rotate --node-id <uuid> --output <новый-файл>")
	fmt.Fprintln(os.Stderr, "  ai-node-manage revoke --node-id <uuid>")
}
