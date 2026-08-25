package application

import (
	"context"
	"time"

	"lidradar/backend/internal/jobs/domain"
)

type ScheduleStore interface {
	Schedule(context.Context, domain.ScheduledCheck) (domain.ScheduledCheck, bool, error)
	PromoteDue(context.Context, time.Time, int) (int, error)
}

// Scheduler атомарно превращает наступившие проверки в общие задания.
type Scheduler struct {
	store ScheduleStore
	now   func() time.Time
}

func NewScheduler(store ScheduleStore, now func() time.Time) Scheduler {
	return Scheduler{store: store, now: now}
}

func (scheduler Scheduler) RunOnce(ctx context.Context, limit int) (int, error) {
	if scheduler.store == nil || scheduler.now == nil || limit < 1 || limit > 100 {
		return 0, domain.ErrInvalid
	}
	return scheduler.store.PromoteDue(ctx, scheduler.now().UTC(), limit)
}
