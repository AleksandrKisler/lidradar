package infrastructure

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/risk/application"
	"lidradar/backend/internal/risk/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

func TestPostgresRadarPriorityCursorSummaryDetailAndIsolation(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenants := testsupport.TwoTenants(t, ctx, pool)
	criticalFixture := insertRiskFixture(t, pool, tenants.A.TenantID, tenants.A.LocationID, domain.DirectionIncoming)
	bookingFixture := insertRiskFixture(t, pool, tenants.A.TenantID, tenants.A.LocationID, domain.DirectionIncoming)
	revenueFixture := insertRiskFixture(t, pool, tenants.A.TenantID, tenants.A.LocationID, domain.DirectionIncoming)
	foreignFixture := insertRiskFixture(t, pool, tenants.B.TenantID, tenants.B.LocationID, domain.DirectionIncoming)

	setRadarOpportunity(t, pool, criticalFixture, "NEW", "1000.00", "RUB")
	setRadarOpportunity(t, pool, bookingFixture, "BOOKING_INTENT", "3000.00", "RUB")
	setRadarOpportunity(t, pool, revenueFixture, "NEW", "9000.00", "EUR")
	setRadarOpportunity(t, pool, foreignFixture, "BOOKING_INTENT", "999999.00", "RUB")

	base := time.Date(2026, 8, 24, 15, 0, 0, 0, time.UTC)
	critical := storeRadarRisk(t, pool, criticalFixture, domain.SeverityCritical, base.Add(-time.Hour), base)
	booking := storeRadarRisk(t, pool, bookingFixture, domain.SeverityHigh, base.Add(-2*time.Hour), base.Add(time.Minute))
	revenue := storeRadarRisk(t, pool, revenueFixture, domain.SeverityHigh, base.Add(-3*time.Hour), base.Add(2*time.Minute))
	foreign := storeRadarRisk(t, pool, foreignFixture, domain.SeverityCritical, base.Add(-4*time.Hour), base.Add(3*time.Minute))

	store := NewPostgresRadarStore(pool)
	first, err := store.List(ctx, tenants.A.TenantID, application.ListQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || first.Items[0].Risk.ID != critical.ID || first.NextCursor == "" {
		t.Fatalf("первая страница = %#v", first)
	}
	second, err := store.List(ctx, tenants.A.TenantID, application.ListQuery{Limit: 1, After: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].Risk.ID != booking.ID || second.NextCursor == "" {
		t.Fatalf("вторая страница = %#v", second)
	}
	third, err := store.List(ctx, tenants.A.TenantID, application.ListQuery{Limit: 1, After: second.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Items) != 1 || third.Items[0].Risk.ID != revenue.ID || third.NextCursor != "" {
		t.Fatalf("третья страница = %#v", third)
	}
	if _, err := store.List(ctx, tenants.A.TenantID, application.ListQuery{
		Filters: application.Filters{Severity: domain.SeverityHigh}, Limit: 1, After: first.NextCursor,
	}); !errors.Is(err, application.ErrInvalidCommand) {
		t.Fatalf("курсор другого фильтра принят: %v", err)
	}

	highOnly, err := store.List(ctx, tenants.A.TenantID, application.ListQuery{
		Filters: application.Filters{Severity: domain.SeverityHigh, RiskType: domain.TypeNoResponse}, Limit: 10,
	})
	if err != nil || len(highOnly.Items) != 2 || highOnly.Items[0].Risk.ID != booking.ID {
		t.Fatalf("фильтр HIGH = %#v, ошибка = %v", highOnly, err)
	}
	summary, err := store.Summary(ctx, tenants.A.TenantID, application.Filters{})
	if err != nil || summary.OpenRisks != 3 || summary.CriticalRisks != 1 ||
		summary.PotentialRevenue != "4000.00" || summary.ConfirmedRecoveredRevenue != "0.00" {
		t.Fatalf("сводка = %#v, ошибка = %v", summary, err)
	}
	highSummary, err := store.Summary(ctx, tenants.A.TenantID, application.Filters{Severity: domain.SeverityHigh})
	if err != nil || highSummary.OpenRisks != 2 || highSummary.CriticalRisks != 0 ||
		highSummary.PotentialRevenue != "3000.00" {
		t.Fatalf("сводка HIGH = %#v, ошибка = %v", highSummary, err)
	}
	detail, found, err := store.Get(ctx, tenants.A.TenantID, critical.ID)
	if err != nil || !found || detail.Opportunity == nil || detail.Conversation == nil ||
		detail.Opportunity.PotentialRevenue == nil || *detail.Opportunity.PotentialRevenue != "1000.00" ||
		detail.Opportunity.LocationID != tenants.A.LocationID || detail.Actions == nil {
		t.Fatalf("детали = %#v, found = %v, ошибка = %v", detail, found, err)
	}
	if _, found, err := store.Get(ctx, tenants.A.TenantID, foreign.ID); err != nil || found {
		t.Fatalf("чужой риск раскрыт: found=%v, err=%v", found, err)
	}
}

func TestPostgresRadarCommandsAreIdempotentAndTenantScoped(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenants := testsupport.TwoTenants(t, ctx, pool)
	fixture := insertRiskFixture(t, pool, tenants.A.TenantID, tenants.A.LocationID, domain.DirectionIncoming)
	now := time.Date(2026, 8, 24, 15, 0, 0, 0, time.UTC)
	risk := storeRadarRisk(t, pool, fixture, domain.SeverityHigh, now.Add(-time.Hour), now)
	store := NewPostgresRadarStore(pool)

	foreign, err := store.Acknowledge(ctx, tenants.B.TenantID, risk.ID, now.Add(time.Minute))
	if err != nil || foreign.Found || foreign.Changed {
		t.Fatalf("чужая команда = %#v, ошибка = %v", foreign, err)
	}
	acknowledged, err := store.Acknowledge(ctx, tenants.A.TenantID, risk.ID, now.Add(time.Minute))
	if err != nil || !acknowledged.Found || !acknowledged.Changed ||
		acknowledged.Risk.Status != domain.StatusAcknowledged {
		t.Fatalf("подтверждение = %#v, ошибка = %v", acknowledged, err)
	}
	replayedAck, err := store.Acknowledge(ctx, tenants.A.TenantID, risk.ID, now.Add(2*time.Minute))
	if err != nil || !replayedAck.Found || replayedAck.Changed ||
		!replayedAck.Risk.UpdatedAt.Equal(acknowledged.Risk.UpdatedAt) {
		t.Fatalf("повтор подтверждения = %#v, ошибка = %v", replayedAck, err)
	}
	resolved, err := store.Resolve(ctx, tenants.A.TenantID, risk.ID, now.Add(3*time.Minute))
	if err != nil || !resolved.Changed || resolved.Risk.Status != domain.StatusResolved {
		t.Fatalf("закрытие = %#v, ошибка = %v", resolved, err)
	}
	replayedResolve, err := store.Resolve(ctx, tenants.A.TenantID, risk.ID, now.Add(4*time.Minute))
	if err != nil || replayedResolve.Changed ||
		!replayedResolve.Risk.UpdatedAt.Equal(resolved.Risk.UpdatedAt) {
		t.Fatalf("повтор закрытия = %#v, ошибка = %v", replayedResolve, err)
	}
}

type capturedInvalidation struct {
	tenantID, eventType, resourceID string
}

type invalidationCapture chan capturedInvalidation

func (capture invalidationCapture) Publish(tenantID, eventType, resourceID string) {
	capture <- capturedInvalidation{tenantID: tenantID, eventType: eventType, resourceID: resourceID}
}

func TestPostgresInvalidatorDeliversBetweenConnections(t *testing.T) {
	pool := testsupport.Postgres(t)
	tenants := testsupport.TwoTenants(t, context.Background(), pool)
	riskID, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	notifier := NewPostgresInvalidator(pool)
	capture := make(invalidationCapture, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- notifier.Listen(ctx, capture, func() { close(ready) })
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("приёмник PostgreSQL не запустился")
	}
	notifier.Publish(tenants.A.TenantID, "risk.changed\ninjected", riskID)
	notifier.Publish(tenants.A.TenantID, "risk.changed", riskID)

delivery:
	for {
		select {
		case signal := <-capture:
			// Канал PostgreSQL общий для локальной базы; параллельный ручной
			// запуск может публиковать свои корректные сигналы.
			if signal.tenantID == tenants.A.TenantID && signal.eventType == "risk.changed" && signal.resourceID == riskID {
				break delivery
			}
		case <-ctx.Done():
			t.Fatal("сигнал PostgreSQL не доставлен")
		}
	}
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("остановка приёмника: %v", err)
	}
}

func setRadarOpportunity(
	t *testing.T,
	pool *pgxpool.Pool,
	fixture riskFixture,
	stage, amount, currency string,
) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		UPDATE opportunities
		SET stage = $3, estimated_amount = $4, currency = $5, updated_at = $6
		WHERE tenant_id = $1 AND id = $2`,
		fixture.tenantID, fixture.opportunityID, stage, amount, currency, fixture.messageAt,
	); err != nil {
		t.Fatal(err)
	}
}

func storeRadarRisk(
	t *testing.T,
	pool *pgxpool.Pool,
	fixture riskFixture,
	severity domain.Severity,
	dueAt, detectedAt time.Time,
) domain.Risk {
	t.Helper()
	riskID, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	finding := riskFinding(fixture, dueAt)
	finding.Severity = severity
	risk, err := domain.NewNoResponse(riskID, finding, detectedAt)
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := NewPostgresRepository(pool).UpsertActive(context.Background(), risk)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}
