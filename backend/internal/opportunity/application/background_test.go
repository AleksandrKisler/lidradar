package application

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	eventsdomain "lidradar/backend/internal/events/domain"
	jobsdomain "lidradar/backend/internal/jobs/domain"
)

type capturedQueue struct{ jobs []jobsdomain.Job }

func (queue *capturedQueue) Enqueue(_ context.Context, job jobsdomain.Job) (jobsdomain.Job, bool, error) {
	queue.jobs = append(queue.jobs, job)
	return job, true, nil
}

func TestCandidateEventHandlerCreatesVersionedDeduplicatedJob(t *testing.T) {
	now := time.Now().UTC()
	data, _ := json.Marshal(candidateData{ConversationID: "conversation", MessageID: "message", Revision: 7})
	event, err := eventsdomain.NewEvent(
		"event", "conversation.changed", 1, "tenant", "conversation", "conversation", "message", data, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	queue := &capturedQueue{}
	handler := CandidateEventHandler(queue, &sequenceIDs{values: []string{"job"}})
	if err := handler(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if len(queue.jobs) != 1 || queue.jobs[0].Type != CandidateJobType ||
		queue.jobs[0].DedupKey != "conversation:conversation:revision:7" {
		t.Fatalf("задание = %#v", queue.jobs)
	}
}

func TestCandidateBackgroundRejectsMalformedPayloadPermanently(t *testing.T) {
	now := time.Now().UTC()
	event, err := eventsdomain.NewEvent(
		"event", "conversation.changed", 1, "tenant", "conversation", "conversation", "message", []byte(`{"revision":-1}`), now,
	)
	if err != nil {
		t.Fatal(err)
	}
	err = CandidateEventHandler(&capturedQueue{}, &sequenceIDs{values: []string{"job"}})(context.Background(), event)
	retryable, code := jobsdomain.Classify(err)
	if err == nil || retryable || code != "INVALID_OUTBOX_PAYLOAD" {
		t.Fatalf("ошибка = %v, retryable=%v, code=%s", err, retryable, code)
	}

	job, err := jobsdomain.NewJob(
		"job", "tenant", CandidateJobType, "bad", []byte(`{"revision":0}`), 0, now, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	err = CandidateJobHandler(CandidateProcessor{})(context.Background(), job)
	retryable, code = jobsdomain.Classify(err)
	if err == nil || retryable || code != "INVALID_JOB_PAYLOAD" {
		t.Fatalf("ошибка задания = %v, retryable=%v, code=%s", err, retryable, code)
	}
}
