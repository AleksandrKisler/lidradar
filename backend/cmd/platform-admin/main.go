// Command platform-admin — инструмент оператора платформы: право PLATFORM_ADMIN и коды
// приглашений (ADR 0057). Первый администратор и первый код появляются только через эту
// команду: у закрытого API нет пользователя, который мог бы их выдать.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"lidradar/backend/internal/admin/application"
	"lidradar/backend/internal/admin/domain"
	"lidradar/backend/internal/admin/infrastructure"
	"lidradar/backend/platform/bootstrap"
	"lidradar/backend/platform/config"
	"lidradar/backend/platform/ids"
	"lidradar/backend/platform/postgres"
)

const usage = "использование: platform-admin grant|revoke --email адрес [--note заметка] | list | " +
	"invite [--note заметка] [--days N] | invitations | revoke-invitation --id UUID | allow-organization --email адрес"

// command — разобранные аргументы командной строки.
type command struct {
	action string
	email  string
	note   string
	id     string
	days   int
}

// parseArguments разбирает аргументы и проверяет обязательные флаги действия. Ошибка уже содержит
// понятное сообщение; вызывающий завершает процесс кодом 2.
func parseArguments(args []string, stderr io.Writer) (command, error) {
	if len(args) < 1 {
		return command{}, errors.New(usage)
	}
	parsed := command{action: args[0]}
	flags := flag.NewFlagSet("platform-admin "+parsed.action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&parsed.email, "email", "", "электронная почта существующего пользователя")
	flags.StringVar(&parsed.note, "note", "", "заметка: причина выдачи права или кому предназначен код")
	flags.StringVar(&parsed.id, "id", "", "идентификатор кода приглашения")
	flags.IntVar(&parsed.days, "days", domain.PlatformInvitationDefaultDays, "срок действия кода в сутках")
	if err := flags.Parse(args[1:]); err != nil {
		return command{}, err
	}
	if flags.NArg() != 0 {
		return command{}, errors.New(usage)
	}
	parsed.email = strings.TrimSpace(parsed.email)
	parsed.note = strings.TrimSpace(parsed.note)
	parsed.id = strings.TrimSpace(parsed.id)
	switch parsed.action {
	case "grant", "revoke", "allow-organization":
		if parsed.email == "" {
			return command{}, errors.New("обязателен --email")
		}
	case "revoke-invitation":
		if parsed.id == "" {
			return command{}, errors.New("обязателен --id")
		}
	case "invite":
		if parsed.days < 1 || parsed.days > domain.PlatformInvitationMaxDays {
			return command{}, fmt.Errorf("--days должен быть от 1 до %d", domain.PlatformInvitationMaxDays)
		}
	case "list", "invitations":
	default:
		return command{}, errors.New("неизвестное действие: " + parsed.action)
	}
	return parsed, nil
}

func main() {
	parsed, err := parseArguments(os.Args[1:], os.Stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(2)
	}
	ctx := context.Background()
	os.Exit(bootstrap.Run(ctx, "lidradar-platform-admin", os.Stderr, func(ctx context.Context, configuration config.Config) error {
		return run(ctx, configuration, parsed, os.Stdout)
	}))
}

func run(ctx context.Context, configuration config.Config, parsed command, stdout io.Writer) error {
	pool, err := postgres.Open(ctx, configuration.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	service := application.NewService(infrastructure.NewPostgresStore(pool), ids.Generator{}, time.Now)
	return execute(ctx, service, parsed, stdout)
}

func execute(ctx context.Context, service application.Service, parsed command, stdout io.Writer) error {
	switch parsed.action {
	case "grant":
		admin, created, err := service.GrantFromCLI(ctx, parsed.email, parsed.note)
		if errors.Is(err, application.ErrNotFound) {
			return errors.New("пользователь с такой почтой не найден или отключён")
		}
		if err != nil {
			return err
		}
		if created {
			fmt.Fprintf(stdout, "право PLATFORM_ADMIN выдано: %s (%s)\n", admin.Email, admin.UserID)
		} else {
			fmt.Fprintf(stdout, "право PLATFORM_ADMIN уже действует: %s (%s)\n", admin.Email, admin.UserID)
		}
	case "revoke":
		revoked, err := service.RevokeFromCLI(ctx, parsed.email)
		if errors.Is(err, application.ErrNotFound) {
			return errors.New("пользователь с такой почтой не найден или отключён")
		}
		if err != nil {
			return err
		}
		if revoked {
			fmt.Fprintf(stdout, "право PLATFORM_ADMIN отозвано: %s\n", parsed.email)
		} else {
			fmt.Fprintf(stdout, "действующего права PLATFORM_ADMIN нет: %s\n", parsed.email)
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
			fmt.Fprintf(stdout, "%s\t%s\t%s\tвыдан %s\n", admin.Email, admin.UserID, state, admin.GrantedAt.UTC().Format(time.RFC3339))
		}
	case "invite":
		invitation, code, err := service.IssuePlatformInvitationFromCLI(ctx, parsed.note, parsed.days)
		if err != nil {
			return err
		}
		// Код выводится один раз и больше нигде не хранится: его нет ни в базе, ни в аудите.
		fmt.Fprintf(stdout, "код приглашения платформы (показан один раз): %s\n", code)
		fmt.Fprintf(stdout, "идентификатор: %s\nдействует до: %s\n", invitation.ID, invitation.ExpiresAt.UTC().Format(time.RFC3339))
	case "invitations":
		invitations, err := service.PlatformInvitations(ctx)
		if err != nil {
			return err
		}
		now := time.Now()
		for _, invitation := range invitations {
			note := ""
			if invitation.Note != nil {
				note = *invitation.Note
			}
			fmt.Fprintf(stdout, "%s\t%s\tдо %s\t%s\n", invitation.ID, invitation.Status(now), invitation.ExpiresAt.UTC().Format(time.RFC3339), note)
		}
	case "revoke-invitation":
		invitation, err := service.RevokePlatformInvitationFromCLI(ctx, parsed.id)
		switch {
		case errors.Is(err, application.ErrNotFound):
			return errors.New("код приглашения не найден")
		case errors.Is(err, application.ErrConflict):
			return errors.New("код уже использован при регистрации: отозвать его нельзя")
		case err != nil:
			return err
		}
		fmt.Fprintf(stdout, "код приглашения отозван: %s\n", invitation.ID)
	case "allow-organization":
		user, rights, err := service.AllowOrganizationFromCLI(ctx, parsed.email)
		switch {
		case errors.Is(err, application.ErrNotFound):
			return errors.New("пользователь с такой почтой не найден или отключён")
		case errors.Is(err, application.ErrConflict):
			return fmt.Errorf("у пользователя уже предельное число прав (%d)", domain.MaxOrganizationRights)
		case err != nil:
			return err
		}
		fmt.Fprintf(stdout, "право создать организацию добавлено: %s (%s), теперь прав: %d\n", user.Email, user.ID, rights)
	}
	return nil
}
