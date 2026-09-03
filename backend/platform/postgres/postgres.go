// Package postgres owns the shared PostgreSQL pool and migration infrastructure.
package postgres

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/platform/config"
	"lidradar/backend/platform/tenantctx"
)

// Роли ADR 0034. Владелец схемы (миграции, CLI) состоит в lidradar_platform и
// не ограничен RLS; рабочие процессы подключаются под ролью с FORCE RLS.
const (
	RoleApp      = "lidradar_app"
	RoleWorker   = "lidradar_worker"
	RolePlatform = "lidradar_platform"
)

// Open creates and verifies a bounded PostgreSQL connection pool for the
// schema owner: migrations and command-line tools.
func Open(ctx context.Context, configuration config.Database) (*pgxpool.Pool, error) {
	return open(ctx, configuration, "")
}

// OpenAs подключается под ролью PostgreSQL: SET ROLE после соединения и
// контекст организации/пользователя перед каждой выдачей соединения из пула.
func OpenAs(ctx context.Context, configuration config.Database, role string) (*pgxpool.Pool, error) {
	if strings.TrimSpace(role) == "" {
		return nil, fmt.Errorf("PostgreSQL role is required")
	}
	return open(ctx, configuration, role)
}

func open(ctx context.Context, configuration config.Database, role string) (*pgxpool.Pool, error) {
	if strings.TrimSpace(configuration.URL) == "" {
		return nil, fmt.Errorf("LIDRADAR_DATABASE_URL is required for this runtime")
	}
	poolConfiguration, err := pgxpool.ParseConfig(configuration.URL)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL configuration: %w", err)
	}
	poolConfiguration.MaxConns = configuration.MaxConnections
	poolConfiguration.MinConns = configuration.MinConnections
	poolConfiguration.ConnConfig.ConnectTimeout = configuration.ConnectTimeout
	if role != "" {
		ConfigureRole(poolConfiguration, role)
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfiguration)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL pool: %w", err)
	}
	pingContext, cancel := context.WithTimeout(ctx, configuration.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingContext); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	return pool, nil
}

type connectionContext struct {
	tenant, actor string
}

// ConfigureRole переводит каждое соединение пула в роль и перед каждой
// выдачей (PrepareConn) приводит настройки сеанса lidradar.tenant_id и lidradar.user_id к
// контексту запроса. Соединение без контекста получает пустые значения,
// поэтому запрос без организации видит пустые таблицы (fail-closed).
func ConfigureRole(configuration *pgxpool.Config, role string) {
	states := &sync.Map{}
	configuration.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
			return fmt.Errorf("switch PostgreSQL role %s: %w", role, err)
		}
		return nil
	}
	configuration.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
		desired := connectionContext{tenant: tenantctx.Tenant(ctx), actor: tenantctx.Actor(ctx)}
		if current, known := states.Load(conn); known && current.(connectionContext) == desired {
			return true, nil
		}
		if _, err := conn.Exec(ctx,
			`SELECT set_config('lidradar.tenant_id', $1, false), set_config('lidradar.user_id', $2, false)`,
			desired.tenant, desired.actor); err != nil {
			states.Delete(conn)
			return false, fmt.Errorf("apply PostgreSQL tenant context: %w", err)
		}
		states.Store(conn, desired)
		return true, nil
	}
	configuration.BeforeClose = func(conn *pgx.Conn) { states.Delete(conn) }
}
