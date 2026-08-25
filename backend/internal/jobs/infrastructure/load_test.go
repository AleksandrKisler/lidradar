//go:build load

package infrastructure

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lidradar/backend/internal/jobs/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

func TestLoadQueueConcurrentClaimsExactlyOnce(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	store := NewPostgresStore(pool)
	generator := ids.Generator{}
	now := time.Now().UTC()
	const jobs = 300
	for index := 0; index < jobs; index++ {
		id, err := generator.NewID()
		if err != nil {
			t.Fatal(err)
		}
		job, err := domain.NewJob(
			id, pair.A.TenantID, "load.critical.v1", fmt.Sprintf("item:%d", index),
			[]byte(`{"load":true}`), 0, now, now,
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, created, err := store.Enqueue(ctx, job); err != nil || !created {
			t.Fatalf("Enqueue() = created %v, err %v", created, err)
		}
	}

	const workers = 12
	var processed atomic.Int64
	var duplicates atomic.Int64
	var seen sync.Map
	errorsChannel := make(chan error, workers)
	started := time.Now()
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			owner := fmt.Sprintf("load-worker-%d", index)
			for {
				claimed, err := store.Claim(ctx, owner, now, now.Add(time.Minute), 1)
				if err != nil {
					errorsChannel <- err
					return
				}
				if len(claimed) == 0 {
					return
				}
				if _, loaded := seen.LoadOrStore(claimed[0].ID, true); loaded {
					duplicates.Add(1)
				}
				if err := store.Succeed(ctx, claimed[0].ID, owner, now.Add(time.Second)); err != nil {
					errorsChannel <- err
					return
				}
				processed.Add(1)
			}
		}(index)
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Error(err)
		}
	}
	var succeeded int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE tenant_id = $1 AND status = 'SUCCEEDED'`, pair.A.TenantID).Scan(&succeeded); err != nil {
		t.Fatal(err)
	}
	if t.Failed() || processed.Load() != jobs || duplicates.Load() != 0 || succeeded != jobs {
		t.Fatalf("processed=%d, duplicates=%d, succeeded=%d", processed.Load(), duplicates.Load(), succeeded)
	}
	t.Logf("300 заданий, 12 обработчиков: всего=%s, скорость=%.0f заданий/с",
		time.Since(started), float64(jobs)/time.Since(started).Seconds())
}
