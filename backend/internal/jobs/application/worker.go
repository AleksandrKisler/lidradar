// Package application исполняет общие фоновые задания независимо от их вида.
package application

import (
	"context"
	"errors"
	"time"

	"lidradar/backend/internal/jobs/domain"
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
		handleErr = handler(ctx, job)
	}
	finishedAt := worker.now().UTC()
	if handleErr == nil {
		return true, worker.store.Succeed(ctx, job.ID, worker.owner, finishedAt)
	}
	if ctx.Err() != nil {
		// Процесс прекращает работу без подтверждения. Задание вернётся после аренды.
		return true, ctx.Err()
	}
	retryable, code := domain.Classify(handleErr)
	next := finishedAt.Add(domain.RetryDelay(job.AttemptCount))
	_, err = worker.store.Fail(ctx, job.ID, worker.owner, code, retryable, next, finishedAt)
	return true, err
}
