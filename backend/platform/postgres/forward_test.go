package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	platformpostgres "lidradar/backend/platform/postgres"
)

// LR-BE-2409: схема прежнего выпуска (миграции по 000019) принимает следующие
// миграции без ручных шагов, повтор ничего не меняет, а подмена уже
// применённой миграции останавливает запуск.
func TestMigrationsApplyForwardFromPreviousRelease(t *testing.T) {
	databaseURL := os.Getenv("LIDRADAR_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("LIDRADAR_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var suffix string
	if err := admin.QueryRow(ctx, `SELECT substr(md5(random()::text), 1, 12)`).Scan(&suffix); err != nil {
		t.Fatal(err)
	}
	schema := "test_forward_" + suffix
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE") }()
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	configuration.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := platformpostgres.MigrateUpTo(ctx, pool, "000019_platform_admin"); err != nil {
		t.Fatalf("миграции прежнего выпуска: %v", err)
	}
	var latest string
	if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&latest); err != nil || latest != "000019_platform_admin" {
		t.Fatalf("последняя миграция прежнего выпуска = %q, %v", latest, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id, email, password_hash, display_name, status) VALUES (gen_random_uuid(), 'forward@tenant.test', '$test$', 'Forward', 'ACTIVE')`); err != nil {
		t.Fatal(err)
	}
	if err := platformpostgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("догон до текущего выпуска: %v", err)
	}
	if err := platformpostgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("повторный запуск: %v", err)
	}
	var users, policies int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM users WHERE email = 'forward@tenant.test'),
		       (SELECT count(*) FROM pg_policies WHERE schemaname = current_schema() AND policyname = 'tenant_isolation')`).Scan(&users, &policies); err != nil {
		t.Fatal(err)
	}
	if users != 1 || policies < 30 {
		t.Fatalf("после догона: пользователей=%d политик=%d", users, policies)
	}
	if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&latest); err != nil || latest < "000021_auth_audit" {
		t.Fatalf("последняя миграция = %q, %v", latest, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET checksum = repeat('0', 64) WHERE version = '000019_platform_admin'`); err != nil {
		t.Fatal(err)
	}
	if err := platformpostgres.Migrate(ctx, pool); err == nil {
		t.Fatal("изменённая контрольная сумма применённой миграции не остановила запуск")
	}
	if err := platformpostgres.MigrateUpTo(ctx, pool, "000099_unknown"); err == nil {
		t.Fatal("неизвестная версия принята")
	}
}
