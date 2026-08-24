package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version  string
	checksum string
	sql      string
}

// Migrate applies every immutable SQL migration exactly once.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1279541842)`); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			checksum TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}

	for _, item := range migrations {
		var checksum string
		err := tx.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version = $1`, item.version).Scan(&checksum)
		switch {
		case err == nil && checksum != item.checksum:
			return fmt.Errorf("migration %s checksum changed", item.version)
		case err == nil:
			continue
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("read migration %s: %w", item.version, err)
		}
		if _, err := tx.Exec(ctx, item.sql); err != nil {
			return fmt.Errorf("apply migration %s: %w", item.version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version, checksum) VALUES ($1, $2)`, item.version, item.checksum); err != nil {
			return fmt.Errorf("record migration %s: %w", item.version, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	result := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		contents, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		digest := sha256.Sum256(contents)
		result = append(result, migration{version: strings.TrimSuffix(entry.Name(), ".sql"), checksum: hex.EncodeToString(digest[:]), sql: string(contents)})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no SQL migrations found")
	}
	return result, nil
}
