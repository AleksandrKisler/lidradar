package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/platform/health"
)

// SchemaReadiness проверяет доступность базы и точное совпадение встроенных
// миграций с журналом применённых миграций.
type SchemaReadiness struct{ pool *pgxpool.Pool }

func NewSchemaReadiness(pool *pgxpool.Pool) SchemaReadiness {
	return SchemaReadiness{pool: pool}
}

func (readiness SchemaReadiness) Check(ctx context.Context) (health.Status, error) {
	if readiness.pool == nil {
		return health.Status{}, errors.New("PostgreSQL readiness is not configured")
	}
	if err := readiness.pool.Ping(ctx); err != nil {
		return health.Status{}, fmt.Errorf("ping PostgreSQL: %w", err)
	}
	migrations, err := loadMigrations()
	if err != nil {
		return health.Status{}, err
	}
	rows, err := readiness.pool.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return health.Status{}, fmt.Errorf("read migration ledger: %w", err)
	}
	defer rows.Close()
	applied := make(map[string]string, len(migrations))
	latestApplied := ""
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return health.Status{}, fmt.Errorf("scan migration ledger: %w", err)
		}
		applied[version] = checksum
		if version > latestApplied {
			latestApplied = version
		}
	}
	if err := rows.Err(); err != nil {
		return health.Status{}, fmt.Errorf("iterate migration ledger: %w", err)
	}
	latest := migrations[len(migrations)-1].version
	if len(applied) != len(migrations) || latestApplied != latest {
		return health.Status{}, fmt.Errorf(
			"database migration ledger has %d entries with latest %q, build expects %d with latest %q",
			len(applied), latestApplied, len(migrations), latest,
		)
	}
	for _, item := range migrations {
		checksum, ok := applied[item.version]
		if !ok || checksum != item.checksum {
			return health.Status{}, fmt.Errorf("database migration %s does not match build", item.version)
		}
	}
	return health.Status{DatabaseMigration: latest, Applied: latestApplied, Latest: latest}, nil
}

var _ health.Checker = SchemaReadiness{}
