package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	conversationdomain "lidradar/backend/internal/conversation/domain"
	eventsapplication "lidradar/backend/internal/events/application"
	eventsdomain "lidradar/backend/internal/events/domain"
	jobsapplication "lidradar/backend/internal/jobs/application"
	jobsdomain "lidradar/backend/internal/jobs/domain"
)

const (
	ConversationChangedEventType = conversationdomain.ChangedEventName + ".v1"
	CandidateJobType             = "opportunity.evaluate-commercial-candidate.v1"
)

type candidateData struct {
	ConversationID string `json:"conversationId"`
	MessageID      string `json:"messageId"`
	Revision       int64  `json:"revision"`
}

type JobQueue interface {
	Enqueue(context.Context, jobsdomain.Job) (jobsdomain.Job, bool, error)
}

func CandidateEventHandler(queue JobQueue, ids IDs) eventsapplication.Handler {
	return func(ctx context.Context, event eventsdomain.Event) error {
		if queue == nil || ids == nil {
			return jobsdomain.Permanent("BACKGROUND_CONFIGURATION_INVALID", errors.New("обработчик кандидата не настроен"))
		}
		var data candidateData
		if json.Unmarshal(event.Data, &data) != nil || data.ConversationID == "" || data.MessageID == "" || data.Revision < 1 ||
			event.AggregateType != "conversation" || event.AggregateID != data.ConversationID {
			return jobsdomain.Permanent("INVALID_OUTBOX_PAYLOAD", errors.New("некорректное событие переписки"))
		}
		jobID, err := ids.NewID()
		if err != nil {
			return jobsdomain.Retryable("JOB_ID_UNAVAILABLE", err)
		}
		job, err := jobsdomain.NewJob(
			jobID, event.TenantID, CandidateJobType,
			fmt.Sprintf("conversation:%s:revision:%d", data.ConversationID, data.Revision),
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

func CandidateJobHandler(processor CandidateProcessor) jobsapplication.Handler {
	return func(ctx context.Context, job jobsdomain.Job) error {
		var data candidateData
		if json.Unmarshal(job.Payload, &data) != nil || data.ConversationID == "" || data.MessageID == "" || data.Revision < 1 {
			return jobsdomain.Permanent("INVALID_JOB_PAYLOAD", errors.New("некорректное задание кандидата"))
		}
		_, _, err := processor.Evaluate(ctx, job.TenantID, data.ConversationID)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrInvalid), errors.Is(err, ErrNotFound), errors.Is(err, ErrInvalidTransition):
			return jobsdomain.Permanent("COMMERCIAL_CANDIDATE_INVALID", err)
		default:
			return jobsdomain.Retryable("COMMERCIAL_CANDIDATE_TEMPORARY", err)
		}
	}
}
