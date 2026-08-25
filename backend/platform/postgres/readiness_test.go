package postgres_test

import (
	"context"
	"testing"

	"lidradar/backend/internal/testsupport"
	platformpostgres "lidradar/backend/platform/postgres"
)

func TestSchemaReadinessRejectsMigrationDrift(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	readiness := platformpostgres.NewSchemaReadiness(pool)
	status, err := readiness.Check(ctx)
	if err != nil || status.DatabaseMigration == "" {
		t.Fatalf("initial readiness = %#v, %v", status, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, status.DatabaseMigration); err != nil {
		t.Fatal(err)
	}
	if _, err := readiness.Check(ctx); err == nil {
		t.Fatal("readiness accepted a database with a missing migration")
	}
}
