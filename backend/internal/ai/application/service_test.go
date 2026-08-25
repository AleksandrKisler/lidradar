package application_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
	"lidradar/backend/internal/ai/infrastructure"
)

type ids struct{ n int }

func (i *ids) NewID() string { i.n++; return fmt.Sprint(i.n) }
func setup(t *testing.T) (application.Service, *infrastructure.MemoryStore, *time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	store := infrastructure.NewTestMemoryStore()
	svc := application.NewService(store, &ids{}, func() time.Time { return now }, application.DefaultLease)
	if _, err := svc.RegisterNode(context.Background(), "home", "secret"); err != nil {
		t.Fatal(err)
	}
	return svc, store, &now
}
func enqueue(t *testing.T, s application.Service) domain.Job {
	t.Helper()
	j, err := s.Enqueue(context.Background(), application.EnqueueCommand{TenantID: "tenant", ConversationID: "conversation", Prompt: "analyse", BaseConversationRevision: 2, AnalysisThroughMessageID: "message"})
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func TestAuthenticatedLifecycleAndFreshness(t *testing.T) {
	s, store, _ := setup(t)
	ctx := context.Background()
	j := enqueue(t, s)
	if err := s.Heartbeat(ctx, "1", "secret"); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.Claim(ctx, "1", "secret")
	if err != nil || !ok || claimed.ID != j.ID {
		t.Fatalf("claim = %#v, %v, %v", claimed, ok, err)
	}
	r, err := s.Started(ctx, "1", "secret", j.ID)
	if err != nil {
		t.Fatal(err)
	}
	done, err := s.Complete(ctx, "1", "secret", j.ID, r.ID, `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"message","summary":"Customer asked about service.","facts":[]}`, 3, "new-message")
	if err != nil {
		t.Fatal(err)
	}
	if done.ApplicationStatus != domain.ApplicationStale {
		t.Fatalf("status = %s", done.ApplicationStatus)
	}
	if _, ok := store.Job(j.ID + ":revision:3"); !ok {
		t.Fatal("stale analysis was not rescheduled")
	}
}
func TestExpiredLeaseIsReclaimedAndOldNodeCannotComplete(t *testing.T) {
	s, _, now := setup(t)
	ctx := context.Background()
	if _, err := s.RegisterNode(ctx, "backup", "other"); err != nil {
		t.Fatal(err)
	}
	j := enqueue(t, s)
	if _, ok, err := s.Claim(ctx, "1", "secret"); err != nil || !ok {
		t.Fatal(err)
	}
	r, err := s.Started(ctx, "1", "secret", j.ID)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(application.DefaultLease + time.Second)
	if got, ok, err := s.Claim(ctx, "2", "other"); err != nil || !ok || got.ID != j.ID {
		t.Fatalf("reclaim failed: %#v %v %v", got, ok, err)
	}
	if _, err := s.Complete(ctx, "1", "secret", j.ID, r.ID, "{}", 2, "message"); !errors.Is(err, application.ErrLeaseLost) {
		t.Fatalf("error = %v", err)
	}
}
func TestAuthenticationAndValidation(t *testing.T) {
	s, _, _ := setup(t)
	if err := s.Heartbeat(context.Background(), "1", "wrong"); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("error = %v", err)
	}
	if _, err := s.Enqueue(context.Background(), application.EnqueueCommand{}); !errors.Is(err, application.ErrInvalid) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidResultPersistsVersionedSummary(t *testing.T) {
	s, store, _ := setup(t)
	j := enqueue(t, s)
	if _, ok, err := s.Claim(context.Background(), "1", "secret"); err != nil || !ok {
		t.Fatal(err)
	}
	r, err := s.Started(context.Background(), "1", "secret", j.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"message","summary":"A concise summary.","facts":[]}`
	done, err := s.Complete(context.Background(), "1", "secret", j.ID, r.ID, out, 2, "message")
	if err != nil || done.ApplicationStatus != domain.ApplicationApplied {
		t.Fatalf("complete = %#v, %v", done, err)
	}
	summary, ok := store.Summary("tenant", "conversation")
	if !ok || summary.Text != "A concise summary." || summary.SchemaVersion != application.AnalysisSchemaV1 || summary.RunID != r.ID {
		t.Fatalf("summary = %#v, %v", summary, ok)
	}
}

func TestInvalidAndLowConfidenceResultsDoNotProduceTrustedFacts(t *testing.T) {
	s, store, _ := setup(t)
	j := enqueue(t, s)
	if _, ok, err := s.Claim(context.Background(), "1", "secret"); err != nil || !ok {
		t.Fatal(err)
	}
	r, _ := s.Started(context.Background(), "1", "secret", j.ID)
	done, err := s.Complete(context.Background(), "1", "secret", j.ID, r.ID, `{"facts":[]}`, 2, "message")
	if err != nil || done.ApplicationStatus != domain.ApplicationRejected || done.ValidationError == "" {
		t.Fatalf("complete = %#v, %v", done, err)
	}
	if _, ok := store.Summary("tenant", "conversation"); ok {
		t.Fatal("invalid result changed derived domain state")
	}
}
