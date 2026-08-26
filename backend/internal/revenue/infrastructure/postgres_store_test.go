package infrastructure

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/revenue/application"
	"lidradar/backend/internal/revenue/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

type allowRevenue struct{}

func (allowRevenue) Allowed(context.Context, string, string, string) (bool, error) { return true, nil }

type revenueFixture struct {
	tenantID, userID, opportunityID, riskID, actionID, outcomeID string
}

func TestPostgresRevenueFormalAttributionIsolationWindowAndCurrencies(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenants := testsupport.TwoTenants(t, ctx, pool)
	confirmedAt := time.Now().UTC().Truncate(time.Microsecond)
	own := insertRevenueFixture(t, pool, tenants.A, confirmedAt.Add(-2*time.Hour))
	otherOpportunity := insertRevenueFixture(t, pool, tenants.A, confirmedAt.Add(-time.Hour))
	foreign := insertRevenueFixture(t, pool, tenants.B, confirmedAt.Add(-time.Hour))
	old := insertRevenueFixture(t, pool, tenants.A, confirmedAt.Add(-31*24*time.Hour-time.Hour))
	future := insertRevenueFixture(t, pool, tenants.A, confirmedAt.Add(time.Hour))
	clock := confirmedAt
	service := application.NewService(NewPostgresStore(pool), allowRevenue{}, ids.Generator{}, func() time.Time { return clock })

	command := recoveredCommand(own, "47000", "RUB")
	confirmation, created, err := service.Confirm(
		ctx, own.userID, own.tenantID, own.opportunityID, "payment-47000", command,
	)
	if err != nil || !created || confirmation.Event.Amount.String() != "47000.00" ||
		confirmation.Event.Source != domain.SourceUser {
		t.Fatalf("подтверждение = %#v, created=%v, ошибка=%v", confirmation, created, err)
	}
	clock = confirmedAt.Add(31 * 24 * time.Hour)
	replayed, created, err := service.Confirm(
		ctx, own.userID, own.tenantID, own.opportunityID, "payment-47000", command,
	)
	if err != nil || created || replayed.Event.ID != confirmation.Event.ID {
		t.Fatalf("повтор после окна = %#v, created=%v, ошибка=%v", replayed, created, err)
	}
	clock = confirmedAt
	conflict := command
	conflict.Amount = "47001"
	if _, _, err := service.Confirm(
		ctx, own.userID, own.tenantID, own.opportunityID, "payment-47000", conflict,
	); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("изменённый повтор не дал конфликт: %v", err)
	}
	if _, _, err := service.Confirm(
		ctx, own.userID, own.tenantID, own.opportunityID, "organic-eur",
		application.ConfirmCommand{Amount: "125.50", Currency: "EUR", Type: domain.AttributionOrganic},
	); err != nil {
		t.Fatalf("органическая выручка: %v", err)
	}
	if _, _, err := service.Confirm(
		ctx, own.userID, own.tenantID, own.opportunityID, "recovered-eur",
		recoveredCommand(own, "15.25", "EUR"),
	); err != nil {
		t.Fatalf("возвращённая выручка EUR: %v", err)
	}
	if _, _, err := service.Confirm(
		ctx, own.userID, own.tenantID, foreign.opportunityID, "foreign-opportunity",
		application.ConfirmCommand{Amount: "1", Currency: "RUB", Type: domain.AttributionOrganic},
	); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("чужая возможность не скрыта: %v", err)
	}
	if _, _, err := service.Confirm(
		ctx, own.userID, own.tenantID, own.opportunityID, "foreign-chain",
		recoveredCommand(foreign, "1", "RUB"),
	); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("чужая цепочка не скрыта: %v", err)
	}
	if _, _, err := service.Confirm(
		ctx, own.userID, own.tenantID, own.opportunityID, "other-opportunity-chain",
		recoveredCommand(otherOpportunity, "1", "RUB"),
	); !errors.Is(err, application.ErrInvalid) {
		t.Fatalf("цепочка другой возможности принята: %v", err)
	}
	if _, _, err := service.Confirm(
		ctx, own.userID, own.tenantID, old.opportunityID, "old-chain",
		recoveredCommand(old, "1", "RUB"),
	); !errors.Is(err, application.ErrInvalid) {
		t.Fatalf("просроченная цепочка принята: %v", err)
	}
	if _, _, err := service.Confirm(
		ctx, own.userID, own.tenantID, future.opportunityID, "future-chain",
		recoveredCommand(future, "1", "RUB"),
	); !errors.Is(err, application.ErrInvalid) {
		t.Fatalf("будущая цепочка принята: %v", err)
	}

	rubles, err := service.ConfirmedRecovered(ctx, own.userID, own.tenantID, "RUB")
	if err != nil || rubles.String() != "47000.00" {
		t.Fatalf("RUB = %s, ошибка=%v", rubles.String(), err)
	}
	euros, err := service.ConfirmedRecovered(ctx, own.userID, own.tenantID, "EUR")
	if err != nil || euros.String() != "15.25" {
		t.Fatalf("EUR recovered = %s, ошибка=%v", euros.String(), err)
	}
	var events, attributions, audits, keys int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM revenue_events WHERE tenant_id=$1),
		  (SELECT count(*) FROM revenue_attributions WHERE tenant_id=$1),
		  (SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND operation='REVENUE_CONFIRMED'),
		  (SELECT count(*) FROM idempotency_keys WHERE tenant_id=$1 AND operation='revenue.confirm')`,
		own.tenantID,
	).Scan(&events, &attributions, &audits, &keys); err != nil {
		t.Fatal(err)
	}
	if events != 3 || attributions != 3 || audits != 3 || keys != 3 {
		t.Fatalf("состояние: events=%d attributions=%d audits=%d keys=%d", events, attributions, audits, keys)
	}
}

func TestPostgresRevenueConcurrentReplayAtomicRollbackUniqueAndAppendOnly(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	confirmedAt := time.Now().UTC().Truncate(time.Microsecond)
	fixture := insertRevenueFixture(t, pool, tenant, confirmedAt.Add(-time.Hour))
	service := application.NewService(NewPostgresStore(pool), allowRevenue{}, ids.Generator{}, func() time.Time { return confirmedAt })
	command := recoveredCommand(fixture, "47000", "RUB")

	var createdCount atomic.Int32
	identifiers := make(chan string, 20)
	errorsFound := make(chan error, 20)
	var wait sync.WaitGroup
	for index := 0; index < 20; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			confirmation, created, err := service.Confirm(
				ctx, fixture.userID, fixture.tenantID, fixture.opportunityID,
				"concurrent-payment", command,
			)
			if err != nil {
				errorsFound <- err
				return
			}
			if created {
				createdCount.Add(1)
			}
			identifiers <- confirmation.Event.ID
		}()
	}
	wait.Wait()
	close(identifiers)
	close(errorsFound)
	for err := range errorsFound {
		t.Errorf("конкурентное подтверждение: %v", err)
	}
	var firstID string
	for id := range identifiers {
		if firstID == "" {
			firstID = id
		}
		if id != firstID {
			t.Errorf("разные события для одного ключа: %s и %s", firstID, id)
		}
	}
	var events, attributions, audits, keys int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM revenue_events WHERE tenant_id=$1),
		  (SELECT count(*) FROM revenue_attributions WHERE tenant_id=$1),
		  (SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND operation='REVENUE_CONFIRMED'),
		  (SELECT count(*) FROM idempotency_keys WHERE tenant_id=$1 AND operation='revenue.confirm')`,
		fixture.tenantID,
	).Scan(&events, &attributions, &audits, &keys); err != nil {
		t.Fatal(err)
	}
	if createdCount.Load() != 1 || events != 1 || attributions != 1 || audits != 1 || keys != 1 {
		t.Fatalf("гонка: created=%d events=%d attrs=%d audits=%d keys=%d", createdCount.Load(), events, attributions, audits, keys)
	}

	store := NewPostgresStore(pool)
	eventID := newRevenueID(t)
	attributionID := newRevenueID(t)
	event, _ := domain.NewConfirmedEvent(
		eventID, fixture.tenantID, fixture.opportunityID, "5", "RUB", fixture.userID, confirmedAt.Add(time.Minute),
	)
	attribution, _ := domain.NewAttribution(
		attributionID, event, domain.AttributionRecovered,
		fixture.riskID, fixture.actionID, fixture.outcomeID, event.ConfirmedAt,
	)
	_, _, err := store.Confirm(ctx, application.Confirmation{Event: event, Attribution: attribution},
		"rollback-payment", [32]byte{7}, application.AuditRecord{
			ID: firstRevenueAuditID(t, pool, fixture.tenantID), TenantID: fixture.tenantID,
			ActorID: fixture.userID, Operation: "REVENUE_CONFIRMED", ResourceType: "REVENUE_EVENT",
			ResourceID: event.ID, At: event.ConfirmedAt,
		}, application.AttributionWindow)
	if !errors.Is(err, application.ErrConflict) {
		t.Fatalf("коллизия аудита не откатила транзакцию: %v", err)
	}
	var rolledBackEvents, rolledBackAttributions, rolledBackKeys int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM revenue_events WHERE tenant_id=$1 AND id=$2),
		  (SELECT count(*) FROM revenue_attributions WHERE tenant_id=$1 AND id=$3),
		  (SELECT count(*) FROM idempotency_keys WHERE tenant_id=$1 AND key='rollback-payment')`,
		fixture.tenantID, event.ID, attribution.ID,
	).Scan(&rolledBackEvents, &rolledBackAttributions, &rolledBackKeys); err != nil {
		t.Fatal(err)
	}
	if rolledBackEvents != 0 || rolledBackAttributions != 0 || rolledBackKeys != 0 {
		t.Fatalf("частичная запись: events=%d attrs=%d keys=%d", rolledBackEvents, rolledBackAttributions, rolledBackKeys)
	}

	duplicateID := newRevenueID(t)
	_, err = pool.Exec(ctx, `
		INSERT INTO revenue_attributions(
		  id,tenant_id,revenue_event_id,opportunity_id,type,risk_id,action_id,outcome_id,created_at
		)
		SELECT $1,tenant_id,revenue_event_id,opportunity_id,type,risk_id,action_id,outcome_id,created_at
		FROM revenue_attributions WHERE tenant_id=$2 LIMIT 1`, duplicateID, fixture.tenantID)
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError.Code != "23505" {
		t.Fatalf("единственная атрибуция не защищена: %v", err)
	}
	for _, table := range []string{"revenue_events", "revenue_attributions"} {
		_, err := pool.Exec(ctx, "DELETE FROM "+table+" WHERE tenant_id=$1", fixture.tenantID)
		postgresError = nil
		if !errors.As(err, &postgresError) || postgresError.Code != "55000" {
			t.Errorf("%s допускает удаление: %v", table, err)
		}
	}
}

func recoveredCommand(fixture revenueFixture, amount, currency string) application.ConfirmCommand {
	return application.ConfirmCommand{
		Amount: amount, Currency: currency, Type: domain.AttributionRecovered,
		RiskID: fixture.riskID, ActionID: fixture.actionID, OutcomeID: fixture.outcomeID,
	}
}

func firstRevenueAuditID(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		SELECT id FROM audit_log
		WHERE tenant_id=$1 AND operation='REVENUE_CONFIRMED'
		ORDER BY created_at,id LIMIT 1`, tenantID,
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertRevenueFixture(
	t *testing.T,
	pool *pgxpool.Pool,
	tenant testsupport.TenantFixture,
	at time.Time,
) revenueFixture {
	t.Helper()
	connectionID := newRevenueID(t)
	contactID := newRevenueID(t)
	conversationID := newRevenueID(t)
	messageID := newRevenueID(t)
	opportunityID := newRevenueID(t)
	riskID := newRevenueID(t)
	actionID := newRevenueID(t)
	outcomeID := newRevenueID(t)
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO channel_connections(
		  id,tenant_id,location_id,provider,name,status,capabilities,
		  verification_secret_hash,created_at,updated_at
		) VALUES ($1,$2,$3,'TEST','Канал выручки','ACTIVE','["MESSAGES"]',$4,$5,$5)`,
		connectionID, tenant.TenantID, tenant.LocationID,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", at,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO contacts(id,tenant_id,display_name,created_at,updated_at)
		VALUES ($1,$2,'Клиент выручки',$3,$3)`, contactID, tenant.TenantID, at); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO conversations(
		  id,tenant_id,location_id,connection_id,contact_id,external_id,status,
		  first_message_at,last_message_at,last_message_direction,revision,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,'ACTIVE',$7,$7,'INCOMING',1,$7,$7)`,
		conversationID, tenant.TenantID, tenant.LocationID, connectionID, contactID,
		"revenue-dialog-"+conversationID, at,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO messages(
		  id,tenant_id,conversation_id,connection_id,external_id,direction,type,text,
		  sent_at,received_at,created_at
		) VALUES ($1,$2,$3,$4,$5,'INCOMING','TEXT','Нужна услуга',$6,$6,$6)`,
		messageID, tenant.TenantID, conversationID, connectionID, "revenue-message-"+messageID, at,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO opportunities(
		  id,tenant_id,conversation_id,stage,estimated_amount,currency,opened_at,created_at,updated_at
		) VALUES ($1,$2,$3,'BOOKED',47000,'RUB',$4,$4,$4)`,
		opportunityID, tenant.TenantID, conversationID, at,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO risk_signals(
		  id,tenant_id,opportunity_id,location_id,type,severity,status,reason_code,reason_text,
		  source,risk_engine_version,trigger_message_id,detected_at,due_at,
		  acknowledged_at,acted_at,created_at,updated_at
		) VALUES ($1,$2,$3,$4,'NO_RESPONSE','HIGH','ACTED','NO_RESPONSE_DUE',
		  'Клиент ожидал ответа','RULE','no-response/v1',$5,$6,$7,$6,$6,$6,$6)`,
		riskID, tenant.TenantID, opportunityID, tenant.LocationID, messageID, at, at.Add(-time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO actions(id,tenant_id,risk_id,actor_user_id,type,note,created_at)
		VALUES ($1,$2,$3,$4,'MARK_CONTACTED','Связались с клиентом',$5)`,
		actionID, tenant.TenantID, riskID, tenant.UserID, at.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO outcomes(id,tenant_id,opportunity_id,actor_user_id,status,note,created_at)
		VALUES ($1,$2,$3,$4,'PAID','Оплата подтверждена',$5)`,
		outcomeID, tenant.TenantID, opportunityID, tenant.UserID, at.Add(2*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	return revenueFixture{
		tenantID: tenant.TenantID, userID: tenant.UserID, opportunityID: opportunityID,
		riskID: riskID, actionID: actionID, outcomeID: outcomeID,
	}
}

func newRevenueID(t *testing.T) string {
	t.Helper()
	id, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
