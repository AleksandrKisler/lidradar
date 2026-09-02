package infrastructure_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lidradar/backend/internal/identity/application"
	"lidradar/backend/internal/identity/infrastructure"
	"lidradar/backend/internal/testsupport"
)

func TestPostgresRateLimiterAtomicallyCapsConcurrentAttempts(t *testing.T) {
	pool := testsupport.Postgres(t)
	limiter := infrastructure.NewPostgresRateLimiter(pool)
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	digest := sha256.Sum256([]byte("owner@example.com"))
	subject := hex.EncodeToString(digest[:])

	var allowed atomic.Int32
	errorsFound := make(chan error, 40)
	var group sync.WaitGroup
	for range 40 {
		group.Add(1)
		go func() {
			defer group.Done()
			decision, err := limiter.Take(
				context.Background(), application.ScopeLoginAccount, subject, 5, 15*time.Minute, now,
			)
			if err != nil {
				errorsFound <- err
				return
			}
			if decision.Allowed {
				allowed.Add(1)
			}
		}()
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	if got := allowed.Load(); got != 5 {
		t.Fatalf("разрешено попыток = %d, ожидалось 5", got)
	}

	if err := limiter.Reset(context.Background(), application.ScopeLoginAccount, subject); err != nil {
		t.Fatal(err)
	}
	decision, err := limiter.Take(
		context.Background(), application.ScopeLoginAccount, subject, 5, 15*time.Minute, now,
	)
	if err != nil || !decision.Allowed {
		t.Fatalf("первая попытка после сброса = %#v, %v", decision, err)
	}
}
