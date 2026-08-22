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
func setup(t *testing.T) (application.Service, *time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	svc := application.NewService(infrastructure.NewMemoryStore(), &ids{}, func() time.Time { return now }, application.DefaultLease)
	if _, err := svc.RegisterNode(context.Background(), "home", "secret"); err != nil {
		t.Fatal(err)
	}
	return svc, &now
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
	s, _ := setup(t)
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
	done, err := s.Complete(ctx, "1", "secret", j.ID, r.ID, `{"facts":[]}`, 3, "new-message")
	if err != nil {
		t.Fatal(err)
	}
	if done.ApplicationStatus != domain.ApplicationStale {
		t.Fatalf("status = %s", done.ApplicationStatus)
	}
}
func TestExpiredLeaseIsReclaimedAndOldNodeCannotComplete(t *testing.T) {
	s, now := setup(t)
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
	s, _ := setup(t)
	if err := s.Heartbeat(context.Background(), "1", "wrong"); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("error = %v", err)
	}
	if _, err := s.Enqueue(context.Background(), application.EnqueueCommand{}); !errors.Is(err, application.ErrInvalid) {
		t.Fatalf("error = %v", err)
	}
}
