package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/platform/config"
)

// В staging и production подключение к PostgreSQL обязано быть зашифрованным.
// disable, allow и prefer (а значит, и строка без sslmode, для которой драйвер
// выбирает prefer) допускают открытый канал напрямую или как запасной вариант.
func TestEncryptedTransportAcceptsOnlyModesWithoutPlaintextFallback(t *testing.T) {
	t.Setenv("PGSSLMODE", "")
	cases := []struct {
		name string
		dsn  string
		env  string
		want bool
	}{
		{"disable", "postgres://u:p@db.example:5432/lidradar?sslmode=disable", "", false},
		{"allow", "postgres://u:p@db.example:5432/lidradar?sslmode=allow", "", false},
		{"prefer", "postgres://u:p@db.example:5432/lidradar?sslmode=prefer", "", false},
		{"без sslmode", "postgres://u:p@db.example:5432/lidradar", "", false},
		{"require", "postgres://u:p@db.example:5432/lidradar?sslmode=require", "", true},
		{"verify-ca", "postgres://u:p@db.example:5432/lidradar?sslmode=verify-ca", "", true},
		{"verify-full", "postgres://u:p@db.example:5432/lidradar?sslmode=verify-full", "", true},
		{"ключи без sslmode", "host=db.example user=u dbname=lidradar", "", false},
		{"ключи с require", "host=db.example user=u dbname=lidradar sslmode=require", "", true},
		{"PGSSLMODE заменяет отсутствующий sslmode", "postgres://u:p@db.example:5432/lidradar", "require", true},
		{"sslmode строки сильнее PGSSLMODE", "postgres://u:p@db.example:5432/lidradar?sslmode=disable", "require", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("PGSSLMODE", c.env)
			parsed, err := pgxpool.ParseConfig(c.dsn)
			if err != nil {
				t.Fatal(err)
			}
			if got := encryptedTransport(parsed.ConnConfig); got != c.want {
				t.Fatalf("encryptedTransport = %v, want %v", got, c.want)
			}
		})
	}
}

// Пул не открывается и не пытается подключиться, пока транспорт не зашифрован;
// ошибка называет способ исправления и не содержит строки подключения.
func TestOpenRefusesPlaintextWhenTLSIsRequired(t *testing.T) {
	t.Setenv("PGSSLMODE", "")
	const secret = "s3cret-password"
	plain := config.Database{
		URL:            "postgres://u:" + secret + "@127.0.0.1:1/lidradar?sslmode=disable",
		MaxConnections: 2, MinConnections: 0, ConnectTimeout: 500 * time.Millisecond,
		RequireTLS: true,
	}
	started := time.Now()
	_, err := Open(context.Background(), plain)
	if err == nil || !strings.Contains(err.Error(), "must use TLS") || !strings.Contains(err.Error(), "LIDRADAR_DATABASE_ALLOW_PLAINTEXT") {
		t.Fatalf("Open(plaintext, RequireTLS) error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("ошибка раскрывает пароль: %v", err)
	}
	if time.Since(started) > 200*time.Millisecond {
		t.Fatalf("отказ занял %v: проверка должна предшествовать подключению", time.Since(started))
	}
	// Без требования та же строка доходит до подключения (порт закрыт).
	plain.RequireTLS = false
	if _, err := Open(context.Background(), plain); err == nil || strings.Contains(err.Error(), "must use TLS") {
		t.Fatalf("Open(plaintext, без требования) error = %v, want connection error", err)
	}
	// Зашифрованный режим проходит проверку и доходит до подключения.
	encrypted := plain
	encrypted.URL = "postgres://u:" + secret + "@127.0.0.1:1/lidradar?sslmode=require"
	encrypted.RequireTLS = true
	if _, err := Open(context.Background(), encrypted); err == nil || strings.Contains(err.Error(), "must use TLS") {
		t.Fatalf("Open(require, RequireTLS) error = %v, want connection error", err)
	}
}
