package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"lidradar/backend/internal/jobs/domain"
)

type memoryStore struct {
	job        domain.Job
	failStatus domain.Status
	succeeded  bool
	failedCode string
	retryable  bool
}

func (store *memoryStore) Enqueue(context.Context, domain.Job) (domain.Job, bool, error) {
	return domain.Job{}, false, errors.New("не используется")
}
func (store *memoryStore) Claim(_ context.Context, owner string, now, lease time.Time, _ int) ([]domain.Job, error) {
	if store.job.ID == "" {
		return nil, nil
	}
	store.job.Status = domain.StatusProcessing
	store.job.AttemptCount++
	store.job.LeaseOwner = &owner
	store.job.LeaseUntil = &lease
	store.job.UpdatedAt = now
	job := store.job
	store.job = domain.Job{}
	return []domain.Job{job}, nil
}
func (store *memoryStore) Succeed(context.Context, string, string, time.Time) error {
	store.succeeded = true
	return nil
}
func (store *memoryStore) Fail(_ context.Context, _, _, code string, retryable bool, _, _ time.Time) (domain.Status, error) {
	store.failedCode = code
	store.retryable = retryable
	return store.failStatus, nil
}

func TestWorkerClassifiesPermanentFailureWithoutRetry(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job, err := domain.NewJob("job", "tenant", "fixture.v1", "fixture", []byte(`{}`), 0, now, now)
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{job: job, failStatus: domain.StatusDead}
	worker := NewWorker(store, "worker", map[string]Handler{
		"fixture.v1": func(context.Context, domain.Job) error {
			return domain.Permanent("INVALID_PAYLOAD", errors.New("неверные данные"))
		},
	}, func() time.Time { return now }, time.Minute)
	processed, err := worker.RunOne(context.Background())
	if err != nil || !processed || store.succeeded || store.retryable || store.failedCode != "INVALID_PAYLOAD" {
		t.Fatalf("RunOne() = %v, %v; store=%#v", processed, err, store)
	}
}
