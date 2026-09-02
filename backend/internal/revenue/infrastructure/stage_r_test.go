package infrastructure

import (
	"context"
	"errors"
	"testing"
	"time"

	"lidradar/backend/internal/revenue/application"
	"lidradar/backend/internal/revenue/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

// LR-BE-RM-001: одна Opportunity даёт не более одной атрибуции RECOVERED.
// Ограничение держит PostgreSQL; приложение переводит его в отдельную ошибку,
// а оплату частями продолжает принимать как ORGANIC.
func TestPostgresSecondRecoveredAttributionIsRejectedByDatabase(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenants := testsupport.TwoTenants(t, ctx, pool)
	confirmedAt := time.Now().UTC().Truncate(time.Microsecond)
	own := insertRevenueFixture(t, pool, tenants.A, confirmedAt.Add(-time.Hour))
	service := application.NewService(NewPostgresStore(pool), allowRevenue{}, ids.Generator{}, func() time.Time { return confirmedAt })

	if _, created, err := service.Confirm(
		ctx, own.userID, own.tenantID, own.opportunityID, "payment-part-1", recoveredCommand(own, "3000", "RUB"),
	); err != nil || !created {
		t.Fatalf("первая RECOVERED = created %v, ошибка %v", created, err)
	}
	_, created, err := service.Confirm(
		ctx, own.userID, own.tenantID, own.opportunityID, "payment-part-2", recoveredCommand(own, "2000", "RUB"),
	)
	if !errors.Is(err, application.ErrRecoveredAlreadyAttributed) || created {
		t.Fatalf("вторая RECOVERED = created %v, ошибка %v; ожидалось ErrRecoveredAlreadyAttributed", created, err)
	}

	var recovered, events int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM revenue_attributions WHERE tenant_id = $1 AND opportunity_id = $2 AND type = 'RECOVERED'),
		       (SELECT count(*) FROM revenue_events WHERE tenant_id = $1 AND opportunity_id = $2)`,
		own.tenantID, own.opportunityID).Scan(&recovered, &events); err != nil {
		t.Fatal(err)
	}
	if recovered != 1 || events != 1 {
		t.Fatalf("после отклонения: атрибуций RECOVERED %d, событий %d; отклонённая транзакция оставила след", recovered, events)
	}
	// Ключ идемпотентности отклонённой команды не сохранился: повтор с ORGANIC проходит.
	organic := application.ConfirmCommand{Amount: "2000", Currency: "RUB", Type: domain.AttributionOrganic}
	if _, created, err := service.Confirm(
		ctx, own.userID, own.tenantID, own.opportunityID, "payment-part-2", organic,
	); err != nil || !created {
		t.Fatalf("ORGANIC после RECOVERED = created %v, ошибка %v", created, err)
	}
}

// LR-BE-RM-001: цепочка Risk → Action → Outcome чужой Opportunity внутри своей
// организации отклоняется составными внешними ключами, а не только приложением.
func TestPostgresAttributionChainMustBelongToOneOpportunity(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenants := testsupport.TwoTenants(t, ctx, pool)
	confirmedAt := time.Now().UTC().Truncate(time.Microsecond)
	own := insertRevenueFixture(t, pool, tenants.A, confirmedAt.Add(-time.Hour))
	other := insertRevenueFixture(t, pool, tenants.A, confirmedAt.Add(-time.Hour))

	eventID := newRevenueID(t)
	if _, err := pool.Exec(ctx, `
		INSERT INTO revenue_events(
			id, tenant_id, opportunity_id, amount, currency, status, source,
			confirmed_by_user_id, confirmed_at, created_at
		) VALUES ($1,$2,$3,'1000.00','RUB','CONFIRMED','USER_CONFIRMED',$4,$5,$5)`,
		eventID, own.tenantID, own.opportunityID, own.userID, confirmedAt); err != nil {
		t.Fatal(err)
	}
	// Триггер validate_revenue_attribution отключается только здесь, чтобы
	// доказать, что внешние ключи держат инвариант самостоятельно.
	if _, err := pool.Exec(ctx, `ALTER TABLE revenue_attributions DISABLE TRIGGER revenue_attributions_validate`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `ALTER TABLE revenue_attributions ENABLE TRIGGER revenue_attributions_validate`)
	}()
	for _, mixed := range []struct {
		name                        string
		riskID, actionID, outcomeID string
	}{
		{"риск чужой Opportunity", other.riskID, own.actionID, own.outcomeID},
		{"действие чужой Opportunity", own.riskID, other.actionID, own.outcomeID},
		{"исход чужой Opportunity", own.riskID, own.actionID, other.outcomeID},
	} {
		_, err := pool.Exec(ctx, `
			INSERT INTO revenue_attributions(
				id, tenant_id, revenue_event_id, opportunity_id, type, risk_id, action_id, outcome_id, created_at
			) VALUES ($1,$2,$3,$4,'RECOVERED',$5,$6,$7,$8)`,
			newRevenueID(t), own.tenantID, eventID, own.opportunityID,
			mixed.riskID, mixed.actionID, mixed.outcomeID, confirmedAt)
		if err == nil {
			t.Fatalf("%s: атрибуция принята без триггера", mixed.name)
		}
		if mapRevenueError("проверка цепочки", err) != application.ErrNotFound {
			t.Fatalf("%s: ожидалось нарушение внешнего ключа, получено %v", mixed.name, err)
		}
	}
}
