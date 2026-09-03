// Package application публикует зафиксированные события исходящего журнала.
package application

import (
	"context"
	"errors"
	"time"

	"lidradar/backend/internal/events/domain"
	jobsdomain "lidradar/backend/internal/jobs/domain"
	"lidradar/backend/platform/observability"
)

const DefaultLease = 30 * time.Second

type Store interface {
	Claim(context.Context, string, time.Time, time.Time, int) ([]domain.Event, error)
	Publish(context.Context, string, string, time.Time) error
	Fail(context.Context, string, string, string, bool, time.Time, time.Time) (domain.Status, error)
}

type Handler func(context.Context, domain.Event) error

// ChainHandlers последовательно выполняет независимые подписки одного типа.
// При повторной доставке уже выполненные шаги остаются безопасными благодаря
// собственным устойчивым ключам дедупликации.
func ChainHandlers(handlers ...Handler) Handler {
	return func(ctx context.Context, event domain.Event) error {
		for _, handler := range handlers {
			if handler == nil {
				return jobsdomain.Permanent("BACKGROUND_CONFIGURATION_INVALID", errors.New("обработчик события не настроен"))
			}
			if err := handler(ctx, event); err != nil {
				return err
			}
		}
		return nil
	}
}

// Dispatcher доставляет исходящий журнал как минимум один раз. Поэтому каждый
// обработчик обязан опираться на устойчивый ключ дедупликации события.
type Dispatcher struct {
	store    Store
	owner    string
	handlers map[string]Handler
	now      func() time.Time
	lease    time.Duration
}

func NewDispatcher(store Store, owner string, handlers map[string]Handler, now func() time.Time, lease time.Duration) Dispatcher {
	if lease <= 0 {
		lease = DefaultLease
	}
	copyHandlers := make(map[string]Handler, len(handlers))
	for eventType, handler := range handlers {
		copyHandlers[eventType] = handler
	}
	return Dispatcher{store: store, owner: owner, handlers: copyHandlers, now: now, lease: lease}
}

func (dispatcher Dispatcher) RunOne(ctx context.Context) (bool, error) {
	if dispatcher.store == nil || dispatcher.owner == "" || dispatcher.now == nil {
		return false, domain.ErrInvalid
	}
	now := dispatcher.now().UTC()
	events, err := dispatcher.store.Claim(ctx, dispatcher.owner, now, now.Add(dispatcher.lease), 1)
	if err != nil || len(events) == 0 {
		return false, err
	}
	event := events[0]
	handler := dispatcher.handlers[event.Key()]
	var handleErr error
	if handler == nil {
		handleErr = jobsdomain.Permanent("UNSUPPORTED_EVENT_TYPE", errors.New("обработчик события не зарегистрирован"))
	} else {
		handleErr = handler(ctx, event)
	}
	finishedAt := dispatcher.now().UTC()
	if handleErr == nil {
		return true, dispatcher.store.Publish(ctx, event.ID, dispatcher.owner, finishedAt)
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	retryable, code := jobsdomain.Classify(handleErr)
	next := finishedAt.Add(jobsdomain.RetryDelay(event.AttemptCount))
	status, err := dispatcher.store.Fail(ctx, event.ID, dispatcher.owner, code, retryable, next, finishedAt)
	observability.Logger(ctx).Warn("Событие исходящего журнала завершилось ошибкой", "event", "outbox.failed",
		"event_id", event.ID, "event_type", event.Type, "tenant_id", event.TenantID, "aggregate_type", event.AggregateType,
		"aggregate_id", event.AggregateID, "trace_id", event.TraceID, "attempt", event.AttemptCount,
		"error_code", code, "retryable", retryable, "status", string(status), "duration_ms", finishedAt.Sub(now).Milliseconds())
	return true, err
}
