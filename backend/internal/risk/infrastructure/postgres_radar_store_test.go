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
	insertRadarCorrectiveFacts(
		t, pool, tenants.A.TenantID, tenants.A.UserID, critical.ID,
		critical.OpportunityID, base.Add(4*time.Minute),
	)
	insertRadarCorrectiveFacts(
		t, pool, tenants.B.TenantID, tenants.B.UserID, foreign.ID,
		foreign.OpportunityID, base.Add(4*time.Minute),
	)

	store := NewPostgresRadarStore(pool)
	first, err := store.List(ctx, tenants.A.TenantID, application.ListQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || first.Items[0].Risk.ID != critical.ID || first.NextCursor == "" {
		t.Fatalf("первая страница = %#v", first)
	}
	if first.Items[0].Recommendation == nil || len(first.Items[0].Actions) != 2 ||
		first.Items[0].Outcome == nil || first.Items[0].Outcome.Type != "THINKING" {
		t.Fatalf("корректирующие факты первой страницы = %#v", first.Items[0])
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
		detail.Opportunity.LocationID != tenants.A.LocationID || detail.Recommendation == nil ||
		detail.Recommendation.Text != "Ответить клиенту сейчас." || len(detail.Actions) != 2 ||
		detail.Actions[0].Type != "OPEN_CONVERSATION" || detail.Actions[1].Type != "MARK_CONTACTED" ||
		detail.Outcome == nil || detail.Outcome.Type != "THINKING" {
		t.Fatalf("детали = %#v, found = %v, ошибка = %v", detail, found, err)
	}
	if _, found, err := store.Get(ctx, tenants.A.TenantID, foreign.ID); err != nil || found {
		t.Fatalf("чужой риск раскрыт: found=%v, err=%v", found, err)
	}
}

func insertRadarCorrectiveFacts(
	t *testing.T,
	pool *pgxpool.Pool,
	tenantID, userID, riskID, opportunityID string,
	at time.Time,
) {
	t.Helper()
	newID := func() string {
		id, err := (ids.Generator{}).NewID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO recommendations(id, tenant_id, risk_id, text, source, created_at)
		VALUES ($1,$2,$3,'Ответить клиенту сейчас.','TEMPLATE',$4)`,
		newID(), tenantID, riskID, at,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO actions(id, tenant_id, risk_id, opportunity_id, actor_user_id, type, note, created_at)
		VALUES
			($1,$2,$3,$8,$4,'OPEN_CONVERSATION','',$5),
			($6,$2,$3,$8,$4,'MARK_CONTACTED','Готово',$7)`,
		newID(), tenantID, riskID, userID, at, newID(), at.Add(time.Minute), opportunityID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO outcomes(id, tenant_id, opportunity_id, actor_user_id, status, note, created_at)
		VALUES
			($1,$2,$3,$4,'BOOKED','',$5),
			($6,$2,$3,$4,'THINKING','Исправление',$7)`,
		newID(), tenantID, opportunityID, userID, at, newID(), at.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
}

// Возвращённая выручка по ТЗ §39 — сумма подтверждённых событий с атрибуцией
// RECOVERED. Риск закрывается раньше, чем деньги подтверждают, поэтому привязка
// показателя к активным рискам обнуляла бы его в обычном рабочем сценарии.
func TestPostgresRadarSummaryKeepsRecoveredRevenueAfterRiskResolution(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenants := testsupport.TwoTenants(t, ctx, pool)
	fixture := insertRiskFixture(t, pool, tenants.A.TenantID, tenants.A.LocationID, domain.DirectionIncoming)
	foreign := insertRiskFixture(t, pool, tenants.B.TenantID, tenants.B.LocationID, domain.DirectionIncoming)
	setRadarOpportunity(t, pool, fixture, "NEW", "5000.00", "RUB")
	setRadarOpportunity(t, pool, foreign, "NEW", "7000.00", "RUB")

	base := time.Date(2026, 8, 24, 15, 0, 0, 0, time.UTC)
	risk := storeRadarRisk(t, pool, fixture, domain.SeverityHigh, base.Add(-time.Hour), base)
	foreignRisk := storeRadarRisk(t, pool, foreign, domain.SeverityHigh, base.Add(-time.Hour), base)
	insertRecoveredRevenue(t, pool, tenants.A.TenantID, tenants.A.UserID, risk, "5000.00", base.Add(time.Hour))
	insertRecoveredRevenue(t, pool, tenants.B.TenantID, tenants.B.UserID, foreignRisk, "7000.00", base.Add(time.Hour))

	store := NewPostgresRadarStore(pool)
	open, err := store.Summary(ctx, tenants.A.TenantID, application.Filters{})
	if err != nil || open.ConfirmedRecoveredRevenue != "5000.00" || open.OpenRisks != 1 {
		t.Fatalf("сводка при активном риске = %#v, ошибка = %v", open, err)
	}

	if _, err := store.Resolve(ctx, tenants.A.TenantID, risk.ID, base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	resolved, err := store.Summary(ctx, tenants.A.TenantID, application.Filters{})
	if err != nil || resolved.ConfirmedRecoveredRevenue != "5000.00" {
		t.Fatalf("возвращённая выручка после закрытия риска = %#v, ошибка = %v", resolved, err)
	}
	if resolved.OpenRisks != 0 || resolved.CriticalRisks != 0 || resolved.PotentialRevenue != "0.00" {
		t.Fatalf("закрытый риск остался в счётчиках: %#v", resolved)
	}

	// Фильтры Radar продолжают действовать и на возвращённую выручку.
	filtered, err := store.Summary(ctx, tenants.A.TenantID, application.Filters{Severity: domain.SeverityCritical})
	if err != nil || filtered.ConfirmedRecoveredRevenue != "0.00" {
		t.Fatalf("сводка по CRITICAL = %#v, ошибка = %v", filtered, err)
	}
}

func insertRecoveredRevenue(
	t *testing.T,
	pool *pgxpool.Pool,
	tenantID, userID string,
	risk domain.Risk,
	amount string,
	confirmedAt time.Time,
) {
	t.Helper()
	ctx := context.Background()
	newID := func() string {
		id, err := (ids.Generator{}).NewID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	actionID, outcomeID, eventID := newID(), newID(), newID()
	if _, err := pool.Exec(ctx, `
		INSERT INTO actions(id, tenant_id, risk_id, opportunity_id, actor_user_id, type, note, created_at)
		VALUES ($1,$2,$3,$4,$5,'MARK_CONTACTED','',$6)`,
		actionID, tenantID, risk.ID, risk.OpportunityID, userID, risk.DetectedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO outcomes(id, tenant_id, opportunity_id, actor_user_id, status, note, created_at)
		VALUES ($1,$2,$3,$4,'BOOKED','',$5)`,
		outcomeID, tenantID, risk.OpportunityID, userID, risk.DetectedAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO revenue_events(
			id, tenant_id, opportunity_id, amount, currency, status, source,
			confirmed_by_user_id, confirmed_at, created_at
		) VALUES ($1,$2,$3,$4,'RUB','CONFIRMED','USER_CONFIRMED',$5,$6,$6)`,
		eventID, tenantID, risk.OpportunityID, amount, userID, confirmedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO revenue_attributions(
			id, tenant_id, revenue_event_id, opportunity_id, type, risk_id, action_id, outcome_id, created_at
		) VALUES ($1,$2,$3,$4,'RECOVERED',$5,$6,$7,$8)`,
		newID(), tenantID, eventID, risk.OpportunityID,
		risk.ID, actionID, outcomeID, confirmedAt); err != nil {
		t.Fatal(err)
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
