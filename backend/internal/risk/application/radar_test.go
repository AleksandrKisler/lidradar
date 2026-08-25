package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"lidradar/backend/internal/risk/application"
	"lidradar/backend/internal/risk/domain"
	"lidradar/backend/internal/risk/infrastructure"
)

type permissions map[string]bool

func (p permissions) Allowed(_ context.Context, actor, tenant, permission string) (bool, error) {
	return p[actor+"/"+tenant+"/"+permission], nil
}

type signals []string

func (s *signals) Publish(tenant, event, id string) { *s = append(*s, tenant+"/"+event+"/"+id) }

func storeRisk(t *testing.T, repo *infrastructure.MemoryRepository, id, tenant string, severity domain.Severity, at time.Time) {
	t.Helper()
	risk, err := domain.NewNoResponse(id, domain.Finding{TenantID: tenant, OpportunityID: "opp-" + id, LocationID: "loc", TriggerMessageID: "msg", Severity: severity, PolicyVersion: "v1", ReasonCode: "NO_RESPONSE_THRESHOLD_EXCEEDED", Reason: "ожидание ответа", DueAt: at}, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = repo.UpsertActive(context.Background(), risk); err != nil {
		t.Fatal(err)
	}
}

func TestRadarOrdersSummarizesAndChangesRisk(t *testing.T) {
	repo := infrastructure.NewTestMemoryRepository()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	storeRisk(t, repo, "high", "tenant", domain.SeverityHigh, now.Add(-2*time.Hour))
	storeRisk(t, repo, "critical", "tenant", domain.SeverityCritical, now.Add(-time.Hour))
	storeRisk(t, repo, "foreign", "other", domain.SeverityCritical, now)
	var events signals
	auth := permissions{"user/tenant/risks.read": true, "user/tenant/risks.manage": true}
	radar := application.NewRadar(repo, auth, &events, func() time.Time { return now })
	page, err := radar.List(context.Background(), "user", "tenant", application.ListQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].Risk.ID != "critical" {
		t.Fatalf("unexpected priority order: %#v", page.Items)
	}
	summary, err := radar.Summary(context.Background(), "user", "tenant")
	if err != nil || summary.OpenRisks != 2 || summary.CriticalRisks != 1 {
		t.Fatalf("summary=%#v err=%v", summary, err)
	}
	ack, err := radar.Acknowledge(context.Background(), "user", "tenant", "critical")
	if err != nil || ack.Status != domain.StatusAcknowledged || ack.AcknowledgedAt == nil {
		t.Fatalf("ack=%#v err=%v", ack, err)
	}
	resolved, err := radar.Resolve(context.Background(), "user", "tenant", "critical")
	if err != nil || resolved.Status != domain.StatusResolved {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	if len(events) != 2 {
		t.Fatalf("signals=%v", events)
	}
}

func TestRadarRejectsCrossTenantAndMissingPermission(t *testing.T) {
	repo := infrastructure.NewTestMemoryRepository()
	storeRisk(t, repo, "foreign", "other", domain.SeverityHigh, time.Now())
	radar := application.NewRadar(repo, permissions{"user/tenant/risks.read": true}, nil, time.Now)
	if _, err := radar.Get(context.Background(), "user", "tenant", "foreign"); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("cross-tenant err=%v", err)
	}
	if _, err := radar.Resolve(context.Background(), "user", "tenant", "foreign"); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("permission err=%v", err)
	}
}
