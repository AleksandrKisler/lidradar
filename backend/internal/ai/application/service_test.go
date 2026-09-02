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

const (
	nodeSecret   = "primary-secret-with-at-least-32-characters"
	backupSecret = "backup-secret-with-at-least-32-characters"
)

func (i *ids) NewID() (string, error) { i.n++; return fmt.Sprint(i.n), nil }

type staleBuilder struct{}

func (staleBuilder) BuildAnalysisJob(context.Context, string, string) (application.EnqueueCommand, error) {
	return application.EnqueueCommand{
		TenantID: "tenant", ConversationID: "conversation", Prompt: "fresh prompt",
		BaseConversationRevision: 3, AnalysisThroughMessageID: "new-message",
	}, nil
}

type freshnessChangingStore struct {
	application.Store
	memory  *infrastructure.MemoryStore
	changed bool
}

func (store *freshnessChangingStore) Finalize(ctx context.Context, final application.Finalization) (domain.Run, error) {
	if final.Summary != nil && !store.changed {
		store.changed = true
		store.memory.SetConversationSnapshot("tenant", "conversation", domain.ConversationSnapshot{
			Revision: 3, LastMessageID: "new-message",
		})
	}
	return store.Store.Finalize(ctx, final)
}

func setup(t *testing.T) (application.Service, *infrastructure.MemoryStore, *time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	store := infrastructure.NewTestMemoryStore()
	svc := application.NewService(store, &ids{}, func() time.Time { return now }, application.DefaultLease).
		WithStaleJobBuilder(staleBuilder{})
	if _, err := svc.RegisterNode(context.Background(), "tenant", "home", nodeSecret); err != nil {
		t.Fatal(err)
	}
	return svc, store, &now
}
func enqueue(t *testing.T, s application.Service) domain.Job {
	t.Helper()
	j, err := s.Enqueue(context.Background(), application.EnqueueCommand{
		TenantID: "tenant", ConversationID: "conversation", Prompt: "analyse",
		BaseConversationRevision: 2, AnalysisThroughMessageID: "message", ModelVersion: "test-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func TestAuthenticatedLifecycleAndFreshness(t *testing.T) {
	s, store, _ := setup(t)
	ctx := context.Background()
	j := enqueue(t, s)
	if err := s.Heartbeat(ctx, "1", nodeSecret, application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: "test-model", AvailableSlots: 1}); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.Claim(ctx, "1", nodeSecret)
	if err != nil || !ok || claimed.ID != j.ID {
		t.Fatalf("claim = %#v, %v, %v", claimed, ok, err)
	}
	r, err := s.Started(ctx, "1", nodeSecret, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	store.SetConversationSnapshot("tenant", "conversation", domain.ConversationSnapshot{Revision: 3, LastMessageID: "new-message"})
	done, err := s.Complete(ctx, "1", nodeSecret, j.ID, r.ID, `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"message","summary":"Customer asked about service.","facts":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if done.ApplicationStatus != domain.ApplicationStale {
		t.Fatalf("status = %s", done.ApplicationStatus)
	}
	if replacement, ok := store.Job("4"); !ok || replacement.BaseConversationRevision != 3 {
		t.Fatal("stale analysis was not rescheduled")
	}
}
func TestExpiredLeaseIsReclaimedAndOldNodeCannotComplete(t *testing.T) {
	s, _, now := setup(t)
	ctx := context.Background()
	if _, err := s.RegisterNode(ctx, "tenant", "backup", backupSecret); err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat(ctx, "1", nodeSecret, application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: "test-model", AvailableSlots: 1}); err != nil {
		t.Fatal(err)
	}
	j := enqueue(t, s)
	if _, ok, err := s.Claim(ctx, "1", nodeSecret); err != nil || !ok {
		t.Fatal(err)
	}
	r, err := s.Started(ctx, "1", nodeSecret, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(application.DefaultLease + time.Second)
	if err := s.Heartbeat(ctx, "2", backupSecret, application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: "test-model", AvailableSlots: 1}); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.Claim(ctx, "2", backupSecret); err != nil || !ok || got.ID != j.ID {
		t.Fatalf("reclaim failed: %#v %v %v", got, ok, err)
	}
	if _, err := s.Complete(ctx, "1", nodeSecret, j.ID, r.ID, "{}"); !errors.Is(err, application.ErrLeaseLost) {
		t.Fatalf("error = %v", err)
	}
}
func TestAuthenticationAndValidation(t *testing.T) {
	s, _, _ := setup(t)
	if err := s.Heartbeat(context.Background(), "1", "wrong-secret-with-at-least-32-characters", application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: "test", AvailableSlots: 1}); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("error = %v", err)
	}
	if _, err := s.Enqueue(context.Background(), application.EnqueueCommand{}); !errors.Is(err, application.ErrInvalid) {
		t.Fatalf("error = %v", err)
	}
}

func TestRotateAndRevokeNodeCredentials(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	newSecret := "rotated-secret-with-at-least-32-characters"
	if err := s.RotateNodeSecret(ctx, "1", newSecret); err != nil {
		t.Fatal(err)
	}
	ready := application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: "test-model", AvailableSlots: 1}
	if err := s.Heartbeat(ctx, "1", nodeSecret, ready); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("old secret error = %v", err)
	}
	if err := s.Heartbeat(ctx, "1", newSecret, ready); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeNode(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeNode(ctx, "1"); err != nil {
		t.Fatalf("repeated revoke = %v", err)
	}
	if err := s.Heartbeat(ctx, "1", newSecret, ready); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("revoked node error = %v", err)
	}
	if err := s.RotateNodeSecret(ctx, "1", nodeSecret); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("rotate revoked node error = %v", err)
	}
}

func TestFreshnessIsRecheckedInsideFinalization(t *testing.T) {
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	memory := infrastructure.NewTestMemoryStore()
	store := &freshnessChangingStore{Store: memory, memory: memory}
	s := application.NewService(store, &ids{}, func() time.Time { return now }, application.DefaultLease).
		WithStaleJobBuilder(staleBuilder{})
	ctx := context.Background()
	if _, err := s.RegisterNode(ctx, "tenant", "home", nodeSecret); err != nil {
		t.Fatal(err)
	}
	job := enqueue(t, s)
	ready := application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: "test-model", AvailableSlots: 1}
	if err := s.Heartbeat(ctx, "1", nodeSecret, ready); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.Claim(ctx, "1", nodeSecret); err != nil || !found {
		t.Fatalf("claim = %v, %v", found, err)
	}
	run, err := s.Started(ctx, "1", nodeSecret, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	output := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"message","summary":"Already stale.","facts":[]}`
	completed, err := s.Complete(ctx, "1", nodeSecret, job.ID, run.ID, output)
	if err != nil || completed.ApplicationStatus != domain.ApplicationStale {
		t.Fatalf("complete = %#v, %v", completed, err)
	}
	if _, ok := memory.Summary("tenant", "conversation"); ok {
		t.Fatal("race produced a stale summary")
	}
	if replacement, ok := memory.Job("4"); !ok || replacement.BaseConversationRevision != 3 {
		t.Fatalf("replacement = %#v, %v", replacement, ok)
	}
}

func TestValidResultPersistsVersionedSummary(t *testing.T) {
	s, store, _ := setup(t)
	j := enqueue(t, s)
	if err := s.Heartbeat(context.Background(), "1", nodeSecret, application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: "test-model", AvailableSlots: 1}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Claim(context.Background(), "1", nodeSecret); err != nil || !ok {
		t.Fatal(err)
	}
	r, err := s.Started(context.Background(), "1", nodeSecret, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"message","summary":"A concise summary.","facts":[]}`
	done, err := s.Complete(context.Background(), "1", nodeSecret, j.ID, r.ID, out)
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
	if err := s.Heartbeat(context.Background(), "1", nodeSecret, application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: "test-model", AvailableSlots: 1}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Claim(context.Background(), "1", nodeSecret); err != nil || !ok {
		t.Fatal(err)
	}
	r, _ := s.Started(context.Background(), "1", nodeSecret, j.ID)
	done, err := s.Complete(context.Background(), "1", nodeSecret, j.ID, r.ID, `{"facts":[]}`)
	if err != nil || done.ApplicationStatus != domain.ApplicationRejected || done.ValidationError == "" {
		t.Fatalf("complete = %#v, %v", done, err)
	}
	if _, ok := store.Summary("tenant", "conversation"); ok {
		t.Fatal("invalid result changed derived domain state")
	}
}
