// Package testsupport provides isolated PostgreSQL fixtures for integration security tests.
package testsupport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/platform/ids"
	platformpostgres "lidradar/backend/platform/postgres"
)

// Postgres creates a private schema, applies all migrations and removes the
// schema after the test. Tests skip when no integration DSN is configured.
func Postgres(t testing.TB) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("LIDRADAR_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("LIDRADAR_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect integration PostgreSQL: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Skipf("integration PostgreSQL unavailable: %v", err)
	}
	schema := "test_" + randomHex(t, 8)
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		admin.Close()
		t.Fatalf("create integration schema: %v", err)
	}

	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse integration PostgreSQL: %v", err)
	}
	configuration.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatalf("connect integration schema: %v", err)
	}
	if err := platformpostgres.Migrate(ctx, pool); err != nil {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("migrate integration schema: %v", err)
	}
	if err := platformpostgres.Migrate(ctx, pool); err != nil {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("repeat integration migration: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanupContext, "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
	})
	return pool
}

type TenantFixture struct {
	UserID     string
	TenantID   string
	LocationID string
}

type TenantPair struct {
	A TenantFixture
	B TenantFixture
}

// TwoTenants creates two independent organizations and one location in each.
func TwoTenants(t testing.TB, ctx context.Context, pool *pgxpool.Pool) TenantPair {
	t.Helper()
	return TenantPair{
		A: tenantFixture(t, ctx, pool, "a"),
		B: tenantFixture(t, ctx, pool, "b"),
	}
}

func tenantFixture(t testing.TB, ctx context.Context, pool *pgxpool.Pool, label string) TenantFixture {
	t.Helper()
	generator := ids.Generator{}
	userID, err := generator.NewID()
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := generator.NewID()
	if err != nil {
		t.Fatal(err)
	}
	membershipID, err := generator.NewID()
	if err != nil {
		t.Fatal(err)
	}
	locationID, err := generator.NewID()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO users(id, email, password_hash, display_name, status)
		VALUES ($1, $2, '$integration-test$', $3, 'ACTIVE')`, userID, label+"@tenant.test", "Tenant "+label); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO organizations(id, name, default_timezone, default_currency, status)
		VALUES ($1, $2, 'Europe/Moscow', 'RUB', 'ACTIVE')`, tenantID, "Organization "+label); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memberships(id, tenant_id, user_id, role, status)
		VALUES ($1, $2, $3, 'OWNER', 'ACTIVE')`, membershipID, tenantID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO locations(id, tenant_id, name, timezone, response_threshold_minutes)
		VALUES ($1, $2, $3, 'Europe/Moscow', 45)`, locationID, tenantID, "Location "+label); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return TenantFixture{UserID: userID, TenantID: tenantID, LocationID: locationID}
}

func randomHex(t testing.TB, length int) string {
	t.Helper()
	value := make([]byte, length)
	if _, err := rand.Read(value); err != nil {
		t.Fatalf("generate integration schema name: %v", err)
	}
	return hex.EncodeToString(value)
}
