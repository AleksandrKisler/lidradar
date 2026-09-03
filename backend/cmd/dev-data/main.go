// Command dev-data применяет и откатывает учебные кабинеты для фронтенда.
// Пароль хранится только в локальном файле 0600, исключённом из Git и образов.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lidradar/backend/internal/devdata"
	"lidradar/backend/platform/config"
	"lidradar/backend/platform/postgres"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || (args[0] != "up" && args[0] != "down" && args[0] != "status") {
		return errors.New("использование: dev-data up|status|down [-confirm frontend-v1] [-password-file runtime/frontend/password.txt]")
	}
	flags := flag.NewFlagSet("dev-data", flag.ContinueOnError)
	passwordFile := flags.String("password-file", "runtime/frontend/password.txt", "файл общего пароля трёх учебных пользователей (0600)")
	confirmation := flags.String("confirm", "", "для down обязательно frontend-v1: удаляет три учебных кабинета целиком")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("лишние аргументы команды")
	}
	if args[0] == "down" && *confirmation != devdata.Version {
		return errors.New("для отката добавьте -confirm frontend-v1; будут удалены и ручные изменения внутри учебных кабинетов")
	}
	environment := config.Environment(os.Getenv("LIDRADAR_ENV"))
	databaseURL := os.Getenv("LIDRADAR_DATABASE_URL")
	if err := devdata.ValidateTarget(environment, databaseURL); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := postgres.Open(ctx, config.Database{URL: databaseURL, MaxConnections: 2, MinConnections: 0, ConnectTimeout: 5 * time.Second})
	if err != nil {
		return errors.New("не удалось подключиться к отдельной базе фронтенда; проверьте адрес и запуск PostgreSQL")
	}
	defer pool.Close()
	password := ""
	if args[0] == "up" {
		status, err := devdata.Run(ctx, pool, environment, "status", "")
		if err != nil {
			return err
		}
		if !status.Applied {
			password, err = readOrCreatePassword(*passwordFile)
			if err != nil {
				return err
			}
		}
	}
	result, err := devdata.Run(ctx, pool, environment, args[0], password)
	if err != nil {
		return err
	}
	if result.Changed && result.Applied {
		fmt.Fprintln(os.Stderr, "Пароль учебных кабинетов находится в локальном файле:", *passwordFile)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

// Уже существующий файл не перезаписывается. Символические ссылки и
// доступные другим пользователям файлы отклоняются, содержимое не печатается.
func readOrCreatePassword(path string) (string, error) {
	if path == "" {
		return "", errors.New("не задан файл пароля")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", errors.New("не удалось создать каталог пароля")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		var random [24]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", errors.New("не удалось создать пароль")
		}
		password := base64.RawURLEncoding.EncodeToString(random[:])
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return "", errors.New("не удалось создать файл пароля без перезаписи; повторите команду")
		}
		_, writeErr := file.WriteString(password + "\n")
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			return "", errors.New("не удалось сохранить пароль; проверьте локальный файл")
		}
		return password, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 1025 {
		return "", errors.New("файл пароля должен быть обычным файлом с правами 0600")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("не удалось прочитать локальный файл пароля")
	}
	password := strings.TrimSuffix(string(contents), "\n")
	if len(password) < 12 || len(password) > 1024 {
		return "", errors.New("в файле нужен пароль длиной 12–1024 байта")
	}
	return password, nil
}
