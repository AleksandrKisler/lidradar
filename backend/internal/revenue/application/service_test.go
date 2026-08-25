package application_test

import (
	"context"
	"errors"
	"fmt"
	"lidradar/backend/internal/revenue/application"
	"lidradar/backend/internal/revenue/domain"
	"lidradar/backend/internal/revenue/infrastructure"
	"testing"
	"time"
)

type allow struct{}

func (allow) Allowed(context.Context, string, string, string) (bool, error) { return true, nil }

type ids struct{ n int }

func (i *ids) NewID() string { i.n++; return fmt.Sprintf("id-%d", i.n) }
func fixture() (application.Service, *infrastructure.MemoryStore, time.Time) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	s := infrastructure.NewTestMemoryStore()
	s.AddOpportunity("tenant", "opportunity")
	f := application.RelatedFact{OpportunityID: "opportunity", At: now.Add(-time.Hour)}
	s.AddRisk("tenant", "risk", f)
	s.AddAction("tenant", "action", f)
	s.AddOutcome("tenant", "outcome", f)
	return application.NewService(s, allow{}, &ids{}, func() time.Time { return now }), s, now
}
func TestMoneyLoopRecoveredRevenueIsFormalIdempotentAndAudited(t *testing.T) {
	svc, store, _ := fixture()
	cmd := application.ConfirmCommand{Amount: "47000", Currency: "rub", Type: domain.AttributionRecovered, RiskID: "risk", ActionID: "action", OutcomeID: "outcome"}
	got, created, err := svc.Confirm(context.Background(), "actor", "tenant", "opportunity", "payment-1", cmd)
	if err != nil || !created || got.Event.Amount.String() != "47000.00" {
		t.Fatalf("got=%+v created=%v err=%v", got, created, err)
	}
	replay, created, err := svc.Confirm(context.Background(), "actor", "tenant", "opportunity", "payment-1", cmd)
	if err != nil || created || replay.Event.ID != got.Event.ID || len(store.Confirmations()) != 1 || len(store.Audits()) != 1 {
		t.Fatalf("replay=%+v created=%v err=%v", replay, created, err)
	}
	total, err := svc.ConfirmedRecovered(context.Background(), "actor", "tenant", "RUB")
	if err != nil || total.String() != "47000.00" {
		t.Fatalf("total=%s err=%v", total.String(), err)
	}
}
func TestRecoveredRequiresSameOpportunityAndWindow(t *testing.T) {
	svc, store, now := fixture()
	store.AddAction("tenant", "other-action", application.RelatedFact{OpportunityID: "other", At: now.Add(-time.Hour)})
	cmd := application.ConfirmCommand{Amount: "1.00", Currency: "RUB", Type: domain.AttributionRecovered, RiskID: "risk", ActionID: "other-action", OutcomeID: "outcome"}
	_, _, err := svc.Confirm(context.Background(), "actor", "tenant", "opportunity", "key", cmd)
	if !errors.Is(err, application.ErrInvalid) || len(store.Confirmations()) != 0 {
		t.Fatalf("err=%v", err)
	}
	store.AddAction("tenant", "old-action", application.RelatedFact{OpportunityID: "opportunity", At: now.Add(-31 * 24 * time.Hour)})
	cmd.ActionID = "old-action"
	_, _, err = svc.Confirm(context.Background(), "actor", "tenant", "opportunity", "key", cmd)
	if !errors.Is(err, application.ErrInvalid) {
		t.Fatalf("old err=%v", err)
	}
}
func TestConflictTenantIsolationAndOrganicExclusion(t *testing.T) {
	svc, store, _ := fixture()
	organic := application.ConfirmCommand{Amount: "100.00", Currency: "RUB", Type: domain.AttributionOrganic}
	_, _, err := svc.Confirm(context.Background(), "actor", "other", "opportunity", "same", organic)
	if !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("tenant err=%v", err)
	}
	_, _, err = svc.Confirm(context.Background(), "actor", "tenant", "opportunity", "same", organic)
	if err != nil {
		t.Fatal(err)
	}
	organic.Amount = "101.00"
	_, _, err = svc.Confirm(context.Background(), "actor", "tenant", "opportunity", "same", organic)
	if !errors.Is(err, application.ErrConflict) {
		t.Fatalf("conflict err=%v", err)
	}
	total, _ := svc.ConfirmedRecovered(context.Background(), "actor", "tenant", "RUB")
	if total.String() != "0.00" {
		t.Fatalf("organic total=%s", total.String())
	}
	if len(store.Confirmations()) != 1 {
		t.Fatal("duplicate revenue")
	}
}
