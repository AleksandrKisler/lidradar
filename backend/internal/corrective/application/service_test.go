package application_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"lidradar/backend/internal/corrective/application"
	"lidradar/backend/internal/corrective/domain"
	"lidradar/backend/internal/corrective/infrastructure"
)

type allow struct{}

func (allow) Allowed(context.Context, string, string, string) (bool, error) { return true, nil }

type ids struct{ n int }

func (i *ids) NewID() string { i.n++; return fmt.Sprintf("id-%d", i.n) }
func fixture() (application.Service, *infrastructure.MemoryStore) {
	s := infrastructure.NewMemoryStore()
	s.AddRisk("tenant", "risk", "opportunity")
	return application.NewService(s, allow{}, &ids{}, func() time.Time { return time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC) }), s
}

func TestCorrectiveFlowIsAppendOnlyAuditedAndIdempotent(t *testing.T) {
	service, store := fixture()
	ctx := context.Background()
	r, err := service.EnsureRecommendation(ctx, "actor", "tenant", "risk", "NO_RESPONSE")
	if err != nil || r.Text != "Ответить клиенту сейчас." || r.Source != "TEMPLATE" {
		t.Fatalf("recommendation=%+v err=%v", r, err)
	}
	a, created, err := service.AddAction(ctx, "actor", "tenant", "risk", "action-key", domain.ActionMarkContacted, "called")
	if err != nil || !created {
		t.Fatalf("action=%+v created=%v err=%v", a, created, err)
	}
	replayed, created, err := service.AddAction(ctx, "actor", "tenant", "risk", "action-key", domain.ActionMarkContacted, "called")
	if err != nil || created || replayed.ID != a.ID || len(store.Actions()) != 1 {
		t.Fatalf("replay=%+v created=%v err=%v", replayed, created, err)
	}
	o, created, err := service.AddOutcome(ctx, "actor", "tenant", "opportunity", "outcome-key", domain.OutcomeBooked, "")
	if err != nil || !created || o.Status != domain.OutcomeBooked {
		t.Fatalf("outcome=%+v created=%v err=%v", o, created, err)
	}
	_, created, err = service.AddOutcome(ctx, "actor", "tenant", "opportunity", "outcome-key-2", domain.OutcomeLost, "correction")
	if err != nil || !created || len(store.Outcomes()) != 2 || len(store.Audits()) != 3 {
		t.Fatalf("append-only outcome/audit failed: created=%v err=%v", created, err)
	}
}
func TestIdempotencyConflictAndTenantIsolation(t *testing.T) {
	service, store := fixture()
	ctx := context.Background()
	_, _, _ = service.AddAction(ctx, "actor", "tenant", "risk", "same", domain.ActionCall, "")
	_, _, err := service.AddAction(ctx, "actor", "tenant", "risk", "same", domain.ActionOther, "")
	if !errors.Is(err, application.ErrConflict) {
		t.Fatalf("err=%v", err)
	}
	_, _, err = service.AddAction(ctx, "actor", "other", "risk", "new", domain.ActionCall, "")
	if !errors.Is(err, application.ErrNotFound) || len(store.Actions()) != 1 {
		t.Fatalf("cross tenant err=%v", err)
	}
}
