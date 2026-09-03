// Command load-generate создаёт синтетический нагрузочный набор ТЗ §72 в
// указанной базе (LR-BE-2501). Предназначен для стенда: использует владельца
// схемы и пишет данные напрямую, минуя API.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"lidradar/backend/internal/loadgen"
	"lidradar/backend/platform/bootstrap"
	"lidradar/backend/platform/config"
	"lidradar/backend/platform/postgres"
)

func main() {
	flags := flag.NewFlagSet("load-generate", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	organizations := flags.Int("organizations", 100, "число организаций")
	conversations := flags.Int("conversations", 500, "переписок на организацию")
	messages := flags.Int("messages", 10, "сообщений на переписку")
	label := flags.String("label", "load", "метка набора в внешних идентификаторах и почте владельцев")
	secret := flags.String("webhook-secret", "load-webhook-secret-1234567890", "секрет сгенерированных каналов")
	if err := flags.Parse(os.Args[1:]); err != nil || flags.NArg() != 0 {
		os.Exit(2)
	}
	os.Exit(bootstrap.Run(context.Background(), "lidradar-load-generate", os.Stderr, func(ctx context.Context, configuration config.Config) error {
		if configuration.Environment == config.EnvironmentProduction {
			return fmt.Errorf("генерация нагрузочного набора в production запрещена")
		}
		pool, err := postgres.Open(ctx, configuration.Database)
		if err != nil {
			return err
		}
		defer pool.Close()
		result, err := loadgen.Generate(ctx, pool, loadgen.Plan{
			Label: *label, Organizations: *organizations, Conversations: *conversations, Messages: *messages, WebhookSecret: *secret,
		})
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, result.Summary())
		return nil
	}))
}
