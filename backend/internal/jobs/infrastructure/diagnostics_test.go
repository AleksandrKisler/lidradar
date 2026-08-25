package infrastructure

import (
	"context"
	"testing"
	"time"

	"lidradar/backend/internal/jobs/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

func TestQueueStatsReportsActionableStates(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	store := NewPostgresStore(pool)
	now := time.Date(2026, 8, 25, 18, 0, 0, 0, time.UTC)

	pending := diagnosticJob(t, tenant.TenantID, "pending", now)
	retry := diagnosticJob(t, tenant.TenantID, "retry", now)
	dead := diagnosticJob(t, tenant.TenantID, "dead", now)
	for _, job := range []domain.Job{pending, retry, dead} {
		if _, _, err := store.Enqueue(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'RETRY' WHERE id = $1`, retry.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE jobs SET status = 'DEAD', completed_at = $2 WHERE id = $1`, dead.ID, now); err != nil {
		t.Fatal(err)
	}
	check, err := domain.NewScheduledCheck(
		diagnosticID(t), tenant.TenantID, "diagnostic", "opportunity", tenant.LocationID,
		"diagnostic.v1", "diagnostic:overdue", []byte(`{"subject":"diagnostic"}`), now.Add(-time.Minute), now.Add(-time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Schedule(ctx, check); err != nil {
		t.Fatal(err)
	}

	stats, err := store.QueueStats(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 || stats.Retry != 1 || stats.Dead != 1 || stats.OverdueScheduled != 1 {
		t.Fatalf("queue stats = %#v", stats)
	}
}

func diagnosticJob(t *testing.T, tenantID, suffix string, at time.Time) domain.Job {
	t.Helper()
	job, err := domain.NewJob(
		diagnosticID(t), tenantID, "diagnostic."+suffix+".v1", "diagnostic:"+suffix,
		[]byte(`{"fixture":"`+suffix+`"}`), 0, at, at,
	)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func diagnosticID(t *testing.T) string {
	t.Helper()
	value, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	return value
}
