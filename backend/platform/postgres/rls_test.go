package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
	"lidradar/backend/platform/tenantctx"
)

// LR-BE-RM-018: SELECT без контекста под lidradar_app даёт 0 строк; с
// контекстом виден только свой tenant; запись в чужую организацию
// отвергается; платформенная роль видит всё; пользователь видит свои членства.
func TestRowLevelSecurityIsFailClosedPerRole(t *testing.T) {
	pools := testsupport.PostgresRoles(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pools.Owner)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	for _, tenant := range []testsupport.TenantFixture{pair.A, pair.B} {
		contactID, err := (ids.Generator{}).NewID()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pools.Owner.Exec(ctx, `INSERT INTO contacts(id,tenant_id,display_name,created_at,updated_at) VALUES ($1,$2,'Клиент',$3,$3)`, contactID, tenant.TenantID, now); err != nil {
			t.Fatal(err)
		}
	}
	count := func(pool *pgxpool.Pool, ctx context.Context, query string) int {
		t.Helper()
		var value int
		if err := pool.QueryRow(ctx, query).Scan(&value); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return value
	}
	var role string
	if err := pools.App.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil || role != "lidradar_app" {
		t.Fatalf("роль приложения = %q, %v", role, err)
	}
	if n := count(pools.App, ctx, `SELECT count(*) FROM contacts`); n != 0 {
		t.Fatalf("без контекста видно %d строк", n)
	}
	if n := count(pools.App, tenantctx.WithTenant(ctx, pair.A.TenantID), `SELECT count(*) FROM contacts`); n != 1 {
		t.Fatalf("организация A видит %d контактов", n)
	}
	if n := count(pools.Worker, tenantctx.WithTenant(ctx, pair.B.TenantID), `SELECT count(*) FROM contacts`); n != 1 {
		t.Fatalf("worker организации B видит %d контактов", n)
	}
	if n := count(pools.App, ctx, `SELECT count(*) FROM contacts`); n != 0 {
		t.Fatalf("после запроса с контекстом соединение сохранило контекст: %d", n)
	}
	if n := count(pools.Platform, ctx, `SELECT count(*) FROM contacts`); n != 2 {
		t.Fatalf("платформенная роль видит %d контактов", n)
	}
	if n := count(pools.Owner, ctx, `SELECT count(*) FROM contacts`); n != 2 {
		t.Fatalf("владелец схемы видит %d контактов", n)
	}
	foreignID, _ := (ids.Generator{}).NewID()
	_, err := pools.App.Exec(tenantctx.WithTenant(ctx, pair.A.TenantID),
		`INSERT INTO contacts(id,tenant_id,display_name,created_at,updated_at) VALUES ($1,$2,'Чужой',$3,$3)`, foreignID, pair.B.TenantID, now)
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError.Code != "42501" {
		t.Fatalf("запись в чужую организацию прошла: %v", err)
	}
	if n := count(pools.App, tenantctx.WithTenant(ctx, pair.A.TenantID), `SELECT count(*) FROM organizations`); n != 1 {
		t.Fatalf("организация A видит %d организаций", n)
	}
	if n := count(pools.App, ctx, `SELECT count(*) FROM memberships`); n != 0 {
		t.Fatalf("членства без контекста: %d", n)
	}
	actorContext := tenantctx.WithActor(ctx, pair.A.UserID)
	if n := count(pools.App, actorContext, `SELECT count(*) FROM memberships`); n != 1 {
		t.Fatalf("пользователь A видит %d членств", n)
	}
	if n := count(pools.App, actorContext, `SELECT count(*) FROM organizations`); n != 1 {
		t.Fatalf("пользователь A видит %d организаций", n)
	}
	if n := count(pools.App, actorContext, `SELECT count(*) FROM contacts`); n != 0 {
		t.Fatalf("контекст пользователя открыл контакты: %d", n)
	}
}
