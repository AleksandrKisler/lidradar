// Package application исполняет общие фоновые задания независимо от их вида.
package application

import (
	"context"
	"errors"
	"time"

	"lidradar/backend/internal/jobs/domain"
	"lidradar/backend/platform/observability"
	"lidradar/backend/platform/tenantctx"
)

const DefaultLease = 30 * time.Second

type Store interface {
	Enqueue(context.Context, domain.Job) (domain.Job, bool, error)
	Claim(context.Context, string, time.Time, time.Time, int) ([]domain.Job, error)
	Succeed(context.Context, string, string, time.Time) error
	Fail(context.Context, string, string, string, bool, time.Time, time.Time) (domain.Status, error)
}

type Handler func(context.Context, domain.Job) error

// Worker выполняет один захваченный Job за вызов. Малый размер захвата не даёт
// аренде простаивать, пока процесс занят предыдущими элементами пачки.
type Worker struct {
	store    Store
	owner    string
	handlers map[string]Handler
	now      func() time.Time
	lease    time.Duration
}

func NewWorker(store Store, owner string, handlers map[string]Handler, now func() time.Time, lease time.Duration) Worker {
	if lease <= 0 {
		lease = DefaultLease
	}
	copyHandlers := make(map[string]Handler, len(handlers))
	for jobType, handler := range handlers {
		copyHandlers[jobType] = handler
	}
	return Worker{store: store, owner: owner, handlers: copyHandlers, now: now, lease: lease}
}

// RunOne возвращает false, когда готового задания нет.
func (worker Worker) RunOne(ctx context.Context) (bool, error) {
	if worker.store == nil || worker.owner == "" || worker.now == nil {
		return false, domain.ErrInvalid
	}
	now := worker.now().UTC()
	claimed, err := worker.store.Claim(ctx, worker.owner, now, now.Add(worker.lease), 1)
	if err != nil || len(claimed) == 0 {
		return false, err
	}
	job := claimed[0]
	handler := worker.handlers[job.Type]
	var handleErr error
	if handler == nil {
		handleErr = domain.Permanent("UNSUPPORTED_JOB_TYPE", errors.New("обработчик задания не зарегистрирован"))
	} else {
		// Обработчик работает в контексте организации задания (RLS, ADR 0034).
		handleErr = handler(tenantctx.WithTenant(ctx, job.TenantID), job)
	}
	finishedAt := worker.now().UTC()
	logger := observability.Logger(ctx).With(
		"job_id", job.ID, "job_type", job.Type, "tenant_id", job.TenantID,
		"attempt", job.AttemptCount, "duration_ms", finishedAt.Sub(now).Milliseconds(),
	)
	if handleErr == nil {
		logger.Debug("Фоновое задание выполнено", "event", "job.succeeded")
		return true, worker.store.Succeed(ctx, job.ID, worker.owner, finishedAt)
	}
	if ctx.Err() != nil {
		// Процесс прекращает работу без подтверждения. Задание вернётся после аренды.
		return true, ctx.Err()
	}
	retryable, code := domain.Classify(handleErr)
	next := finishedAt.Add(domain.RetryDelay(job.AttemptCount))
	status, err := worker.store.Fail(ctx, job.ID, worker.owner, code, retryable, next, finishedAt)
	// Причина ошибки не пишется целиком: она может содержать текст сообщения (§64).
	logger.Warn("Фоновое задание завершилось ошибкой", "event", "job.failed",
		"error_code", code, "retryable", retryable, "status", string(status))
	return true, err
}
