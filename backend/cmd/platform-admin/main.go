// Command platform-admin выдаёт, отзывает и показывает право PLATFORM_ADMIN.
// Первый администратор появляется только через эту команду: у API нет
// пользователя, который мог бы его выдать.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"lidradar/backend/internal/admin/application"
	"lidradar/backend/internal/admin/infrastructure"
	"lidradar/backend/platform/bootstrap"
	"lidradar/backend/platform/config"
	"lidradar/backend/platform/ids"
	"lidradar/backend/platform/postgres"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "использование: platform-admin grant|revoke|list [--email адрес] [--note заметка]")
		os.Exit(2)
	}
	action := os.Args[1]
	flags := flag.NewFlagSet("platform-admin "+action, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	email := flags.String("email", "", "электронная почта существующего пользователя")
	note := flags.String("note", "", "заметка о причине выдачи")
	if err := flags.Parse(os.Args[2:]); err != nil || flags.NArg() != 0 {
		os.Exit(2)
	}
	switch action {
	case "grant", "revoke":
		if strings.TrimSpace(*email) == "" {
			fmt.Fprintln(os.Stderr, "обязателен --email")
			os.Exit(2)
		}
	case "list":
	default:
		fmt.Fprintln(os.Stderr, "неизвестное действие: "+action)
		os.Exit(2)
	}
	ctx := context.Background()
	os.Exit(bootstrap.Run(ctx, "lidradar-platform-admin", os.Stderr, func(ctx context.Context, configuration config.Config) error {
		return run(ctx, configuration, action, strings.TrimSpace(*email), strings.TrimSpace(*note))
	}))
}

func run(ctx context.Context, configuration config.Config, action, email, note string) error {
	pool, err := postgres.Open(ctx, configuration.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	service := application.NewService(infrastructure.NewPostgresStore(pool), ids.Generator{}, time.Now)
	switch action {
	case "grant":
		admin, created, err := service.GrantFromCLI(ctx, email, note)
		if errors.Is(err, application.ErrNotFound) {
			return errors.New("пользователь с такой почтой не найден или отключён")
		}
		if err != nil {
			return err
		}
		if created {
			fmt.Fprintf(os.Stdout, "право PLATFORM_ADMIN выдано: %s (%s)\n", admin.Email, admin.UserID)
		} else {
			fmt.Fprintf(os.Stdout, "право PLATFORM_ADMIN уже действует: %s (%s)\n", admin.Email, admin.UserID)
		}
	case "revoke":
		revoked, err := service.RevokeFromCLI(ctx, email)
		if errors.Is(err, application.ErrNotFound) {
			return errors.New("пользователь с такой почтой не найден или отключён")
		}
		if err != nil {
			return err
		}
		if revoked {
			fmt.Fprintf(os.Stdout, "право PLATFORM_ADMIN отозвано: %s\n", email)
		} else {
			fmt.Fprintf(os.Stdout, "действующего права PLATFORM_ADMIN нет: %s\n", email)
		}
	case "list":
		admins, err := service.ListAdmins(ctx)
		if err != nil {
			return err
		}
		for _, admin := range admins {
			state := "активен"
			if !admin.Active() {
				state = "отозван " + admin.RevokedAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(os.Stdout, "%s\t%s\t%s\tвыдан %s\n", admin.Email, admin.UserID, state, admin.GrantedAt.UTC().Format(time.RFC3339))
		}
	}
	return nil
}
