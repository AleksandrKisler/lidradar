package infrastructure

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/jobs/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

func TestClaimLeaseRetryAndDeadLifecycle(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	store := NewPostgresStore(pool)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job := newJob(t, tenant.TenantID, "retry", now)
	job.MaxAttempts = 2
	if _, inserted, err := store.Enqueue(ctx, job); err != nil || !inserted {
		t.Fatalf("Enqueue() = inserted %v, error %v", inserted, err)
	}

	first, err := store.Claim(ctx, "worker-1", now, now.Add(time.Minute), 1)
	if err != nil || len(first) != 1 || first[0].AttemptCount != 1 {
		t.Fatalf("first Claim() = %#v, %v", first, err)
	}
	next := now.Add(5 * time.Second)
	status, err := store.Fail(ctx, job.ID, "worker-1", "TEMPORARY_PROVIDER", true, next, now.Add(time.Second))
	if err != nil || status != domain.StatusRetry {
		t.Fatalf("first Fail() = %s, %v", status, err)
	}
	if early, err := store.Claim(ctx, "worker-2", now.Add(4*time.Second), now.Add(time.Minute), 1); err != nil || len(early) != 0 {
		t.Fatalf("early Claim() = %#v, %v", early, err)
	}
	second, err := store.Claim(ctx, "worker-2", next, next.Add(time.Minute), 1)
	if err != nil || len(second) != 1 || second[0].AttemptCount != 2 {
		t.Fatalf("second Claim() = %#v, %v", second, err)
	}
	status, err = store.Fail(ctx, job.ID, "worker-2", "TEMPORARY_PROVIDER", true, next.Add(time.Minute), next.Add(time.Second))
	if err != nil || status != domain.StatusDead {
		t.Fatalf("second Fail() = %s, %v", status, err)
	}
	var storedStatus domain.Status
	var completedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, completed_at FROM jobs WHERE id = $1`, job.ID).Scan(&storedStatus, &completedAt); err != nil {
		t.Fatal(err)
	}
	if storedStatus != domain.StatusDead || completedAt == nil {
		t.Fatalf("состояние задания = %s, completed_at=%v", storedStatus, completedAt)
	}
}

func TestClaimSkipsRowLockedByAnotherWorker(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	store := NewPostgresStore(pool)
	now := time.Date(2026, 8, 25, 12, 30, 0, 0, time.UTC)
	first := newJob(t, tenant.TenantID, "locked", now)
	second := newJob(t, tenant.TenantID, "available", now.Add(time.Second))
	if _, _, err := store.Enqueue(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Enqueue(ctx, second); err != nil {
		t.Fatal(err)
	}
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(ctx) }()
	if err := lock.QueryRow(ctx, `SELECT id FROM jobs WHERE id = $1 FOR UPDATE`, first.ID).Scan(new(string)); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Claim(ctx, "parallel-worker", now.Add(2*time.Second), now.Add(time.Minute), 2)
	if err != nil || len(claimed) != 1 || claimed[0].ID != second.ID {
		t.Fatalf("Claim() с заблокированной строкой = %#v, %v", claimed, err)
	}
	if err := lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.Claim(ctx, "next-worker", now.Add(2*time.Second), now.Add(time.Minute), 1)
	if err != nil || len(claimed) != 1 || claimed[0].ID != first.ID {
		t.Fatalf("Claim() после снятия блокировки = %#v, %v", claimed, err)
	}
}

func TestScheduledCheckPromotesOnce(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	store := NewPostgresStore(pool)
	due := time.Date(2026, 8, 25, 13, 0, 0, 0, time.UTC)
	checkID := newID(t)
	check, err := domain.NewScheduledCheck(
		checkID, tenant.TenantID, "NO_RESPONSE", "opportunity", tenant.LocationID,
		"risk.no-response.v1", "opportunity:"+tenant.LocationID+":"+due.Format(time.RFC3339),
		[]byte(`{"opportunityId":"`+tenant.LocationID+`"}`), due, due.Add(-time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, inserted, err := store.Schedule(ctx, check); err != nil || !inserted {
		t.Fatalf("Schedule() = inserted %v, error %v", inserted, err)
	}
	if _, inserted, err := store.Schedule(ctx, check); err != nil || inserted {
		t.Fatalf("duplicate Schedule() = inserted %v, error %v", inserted, err)
	}
	if promoted, err := store.PromoteDue(ctx, due.Add(-time.Second), 10); err != nil || promoted != 0 {
		t.Fatalf("early PromoteDue() = %d, %v", promoted, err)
	}
	if promoted, err := store.PromoteDue(ctx, due, 10); err != nil || promoted != 1 {
		t.Fatalf("PromoteDue() = %d, %v", promoted, err)
	}
	if promoted, err := store.PromoteDue(ctx, due.Add(time.Hour), 10); err != nil || promoted != 0 {
		t.Fatalf("repeat PromoteDue() = %d, %v", promoted, err)
	}
	var jobCount int
	var status domain.CheckStatus
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM jobs WHERE tenant_id = $1 AND job_type = 'risk.no-response.v1'), status
		FROM scheduled_checks WHERE id = $2`, tenant.TenantID, checkID).Scan(&jobCount, &status); err != nil {
		t.Fatal(err)
	}
	if jobCount != 1 || status != domain.CheckEnqueued {
		t.Fatalf("jobCount=%d, status=%s", jobCount, status)
	}
}

func TestDuplicateSideEffectAfterLeaseRecoveryOccursOnce(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	store := NewPostgresStore(pool)
	now := time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC)
	job := newJob(t, tenant.TenantID, "side-effect", now)
	if _, _, err := store.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE fixture_effects(job_id UUID PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	first, err := store.Claim(ctx, "worker-before-crash", now, now.Add(time.Minute), 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first Claim() = %#v, %v", first, err)
	}
	applyEffect := func(at time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO fixture_effects(job_id, applied_at) VALUES ($1, $2)
			ON CONFLICT (job_id) DO NOTHING`, job.ID, at); err != nil {
			t.Fatal(err)
		}
	}
	applyEffect(now)
	// ACK отсутствует: имитируем падение сразу после побочного эффекта.
	recoveredAt := now.Add(2 * time.Minute)
	second, err := store.Claim(ctx, "worker-after-crash", recoveredAt, recoveredAt.Add(time.Minute), 1)
	if err != nil || len(second) != 1 || second[0].ID != job.ID {
		t.Fatalf("recovery Claim() = %#v, %v", second, err)
	}
	applyEffect(recoveredAt)
	if err := store.Succeed(ctx, job.ID, "worker-after-crash", recoveredAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM fixture_effects WHERE job_id = $1`, job.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("побочный эффект выполнен %d раз, нужно 1", count)
	}
}

func TestWorkerProcessCanBeKilledAfterClaim(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	store := NewPostgresStore(pool)
	now := time.Date(2026, 8, 25, 15, 0, 0, 0, time.UTC)
	job := newJob(t, tenant.TenantID, "kill-9", now)
	if _, _, err := store.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestJobClaimHelper$", "-test.v")
	command.Env = append(os.Environ(),
		"LIDRADAR_JOB_CLAIM_HELPER=1",
		"LIDRADAR_JOB_HELPER_SCHEMA="+schema,
		"LIDRADAR_JOB_HELPER_NOW="+now.Format(time.RFC3339Nano),
		"LIDRADAR_JOB_HELPER_ID="+job.ID,
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	claimed := false
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "JOB_CLAIMED") {
			claimed = true
			break
		}
	}
	if !claimed {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("дочерний worker не захватил задание: %s", stderr.String())
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()

	recoveredAt := now.Add(2 * time.Minute)
	recovered, err := store.Claim(ctx, "surviving-worker", recoveredAt, recoveredAt.Add(time.Minute), 1)
	if err != nil || len(recovered) != 1 || recovered[0].ID != job.ID || recovered[0].AttemptCount != 2 {
		t.Fatalf("Claim() после kill -9 = %#v, %v", recovered, err)
	}
	if err := store.Succeed(ctx, job.ID, "killed-worker", recoveredAt.Add(time.Second)); !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("старый владелец Succeed() error = %v", err)
	}
	if err := store.Succeed(ctx, job.ID, "surviving-worker", recoveredAt.Add(time.Second)); err != nil {
		t.Fatalf("новый владелец Succeed() error = %v", err)
	}
}

func TestJobClaimHelper(t *testing.T) {
	if os.Getenv("LIDRADAR_JOB_CLAIM_HELPER") != "1" {
		t.Skip("вспомогательный процесс запускается только из crash-test")
	}
	configuration, err := pgxpool.ParseConfig(os.Getenv("LIDRADAR_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	configuration.ConnConfig.RuntimeParams["search_path"] = os.Getenv("LIDRADAR_JOB_HELPER_SCHEMA")
	pool, err := pgxpool.NewWithConfig(context.Background(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	now, err := time.Parse(time.RFC3339Nano, os.Getenv("LIDRADAR_JOB_HELPER_NOW"))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := NewPostgresStore(pool).Claim(context.Background(), "killed-worker", now, now.Add(time.Minute), 1)
	if err != nil || len(jobs) != 1 || jobs[0].ID != os.Getenv("LIDRADAR_JOB_HELPER_ID") {
		t.Fatalf("helper Claim() = %#v, %v", jobs, err)
	}
	fmt.Fprintln(os.Stdout, "JOB_CLAIMED")
	select {}
}

func newJob(t *testing.T, tenantID, suffix string, at time.Time) domain.Job {
	t.Helper()
	job, err := domain.NewJob(
		newID(t), tenantID, "fixture."+suffix+".v1", "dedup:"+suffix,
		[]byte(`{"fixture":"`+suffix+`"}`), 0, at, at,
	)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func newID(t *testing.T) string {
	t.Helper()
	value, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	return value
}
