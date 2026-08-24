// Package postgres owns the shared PostgreSQL pool and migration infrastructure.
package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/platform/config"
)

// Open creates and verifies a bounded PostgreSQL connection pool.
func Open(ctx context.Context, configuration config.Database) (*pgxpool.Pool, error) {
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
