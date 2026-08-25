package application

import (
	"context"
	"encoding/json"
	"errors"

	eventsapplication "lidradar/backend/internal/events/application"
	eventsdomain "lidradar/backend/internal/events/domain"
	jobsapplication "lidradar/backend/internal/jobs/application"
	jobsdomain "lidradar/backend/internal/jobs/domain"
)

const (
	NormalizationEventType = "connector.raw-event.received.v1"
	NormalizationJobType   = "connector.normalize-raw-event.v1"
)

type normalizationData struct {
	RawEventID string `json:"rawEventId"`
}

type JobQueue interface {
	Enqueue(context.Context, jobsdomain.Job) (jobsdomain.Job, bool, error)
}

// NormalizationEventHandler преобразует событие исходящего журнала в одно
// дедуплицированное задание общей очереди.
func NormalizationEventHandler(queue JobQueue, ids IDs) eventsapplication.Handler {
	return func(ctx context.Context, event eventsdomain.Event) error {
		if queue == nil || ids == nil {
			return jobsdomain.Permanent("BACKGROUND_CONFIGURATION_INVALID", errors.New("обработчик канонизации не настроен"))
		}
		var data normalizationData
		if json.Unmarshal(event.Data, &data) != nil || data.RawEventID == "" ||
			event.AggregateType != "raw_event" || event.AggregateID != data.RawEventID {
			return jobsdomain.Permanent("INVALID_OUTBOX_PAYLOAD", errors.New("некорректное событие канонизации"))
		}
		jobID, err := ids.NewID()
		if err != nil {
			return jobsdomain.Retryable("JOB_ID_UNAVAILABLE", err)
		}
		job, err := jobsdomain.NewJob(
			jobID, event.TenantID, NormalizationJobType, "raw-event:"+data.RawEventID,
			event.Data, 0, event.OccurredAt, event.OccurredAt,
		)
		if err != nil {
			return jobsdomain.Permanent("INVALID_JOB_PAYLOAD", err)
		}
		_, _, err = queue.Enqueue(ctx, job)
		if errors.Is(err, jobsdomain.ErrConflict) || errors.Is(err, jobsdomain.ErrInvalid) {
			return jobsdomain.Permanent("JOB_DEDUP_CONFLICT", err)
		}
		if err != nil {
			return jobsdomain.Retryable("JOB_ENQUEUE_FAILED", err)
		}
		return nil
	}
}

// NormalizationJobHandler связывает общий worker с владельцем сценария.
func NormalizationJobHandler(service NormalizationService) jobsapplication.Handler {
	return func(ctx context.Context, job jobsdomain.Job) error {
		var data normalizationData
		if json.Unmarshal(job.Payload, &data) != nil || data.RawEventID == "" {
			return jobsdomain.Permanent("INVALID_JOB_PAYLOAD", errors.New("некорректное задание канонизации"))
		}
		err := service.Process(ctx, job.TenantID, data.RawEventID)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrInvalid), errors.Is(err, ErrNotFound):
			return jobsdomain.Permanent("NORMALIZATION_INVALID", err)
		default:
			return jobsdomain.Retryable("NORMALIZATION_TEMPORARY", err)
		}
	}
}
