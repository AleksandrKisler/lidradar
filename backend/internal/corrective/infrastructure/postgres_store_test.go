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

	"lidradar/backend/internal/corrective/application"
	"lidradar/backend/internal/corrective/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

type allowCorrective struct{}

func (allowCorrective) Allowed(context.Context, string, string, string) (bool, error) {
	return true, nil
}

type correctiveFixture struct {
	tenantID, userID, riskID, opportunityID string
}

func TestPostgresCorrectiveFlowIsTenantScopedIdempotentAndAudited(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenants := testsupport.TwoTenants(t, ctx, pool)
	own := insertCorrectiveFixture(t, pool, tenants.A.TenantID, tenants.A.LocationID, tenants.A.UserID)
	foreign := insertCorrectiveFixture(t, pool, tenants.B.TenantID, tenants.B.LocationID, tenants.B.UserID)
	service := application.NewService(NewPostgresStore(pool), allowCorrective{}, ids.Generator{}, time.Now)

	recommendation, err := service.EnsureRecommendation(ctx, own.userID, own.tenantID, own.riskID)
	if err != nil || recommendation.Text != "Ответить клиенту сейчас." || recommendation.Source != "TEMPLATE" {
		t.Fatalf("рекомендация = %#v, ошибка = %v", recommendation, err)
	}
	replayedRecommendation, err := service.EnsureRecommendation(ctx, own.userID, own.tenantID, own.riskID)
	if err != nil || replayedRecommendation.ID != recommendation.ID {
		t.Fatalf("повтор рекомендации = %#v, ошибка = %v", replayedRecommendation, err)
	}

	action, created, err := service.AddAction(
		ctx, own.userID, own.tenantID, own.riskID,
		"action-key", domain.ActionMarkContacted, "  Позвонили клиенту  ",
	)
	if err != nil || !created || action.Note != "Позвонили клиенту" {
		t.Fatalf("действие = %#v, created=%v, ошибка=%v", action, created, err)
	}
	replayedAction, created, err := service.AddAction(
		ctx, own.userID, own.tenantID, own.riskID,
		"action-key", domain.ActionMarkContacted, "Позвонили клиенту",
	)
	if err != nil || created || replayedAction.ID != action.ID {
		t.Fatalf("повтор действия = %#v, created=%v, ошибка=%v", replayedAction, created, err)
	}
	if _, _, err := service.AddAction(
		ctx, own.userID, own.tenantID, own.riskID,
		"action-key", domain.ActionCall, "Позвонили клиенту",
	); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("изменённый запрос не дал конфликт: %v", err)
	}

	firstOutcome, created, err := service.AddOutcome(
		ctx, own.userID, own.tenantID, own.opportunityID,
		"outcome-1", domain.OutcomeBooked, "Запись подтверждена",
	)
	if err != nil || !created || firstOutcome.Status != domain.OutcomeBooked {
		t.Fatalf("первый исход = %#v, created=%v, ошибка=%v", firstOutcome, created, err)
	}
	secondOutcome, created, err := service.AddOutcome(
		ctx, own.userID, own.tenantID, own.opportunityID,
		"outcome-2", domain.OutcomeThinking, "Клиент попросил время",
	)
	if err != nil || !created || secondOutcome.ID == firstOutcome.ID {
		t.Fatalf("исправляющий исход = %#v, created=%v, ошибка=%v", secondOutcome, created, err)
	}
	if replayed, created, err := service.AddOutcome(
		ctx, own.userID, own.tenantID, own.opportunityID,
		"outcome-2", domain.OutcomeThinking, "Клиент попросил время",
	); err != nil || created || replayed.ID != secondOutcome.ID {
		t.Fatalf("повтор исхода = %#v, created=%v, ошибка=%v", replayed, created, err)
	}

	if _, err := service.EnsureRecommendation(ctx, own.userID, own.tenantID, foreign.riskID); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("чужая рекомендация: %v", err)
	}
	if _, _, err := service.AddAction(
		ctx, own.userID, own.tenantID, foreign.riskID,
		"foreign-action", domain.ActionCall, "",
	); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("чужое действие: %v", err)
	}
	if _, _, err := service.AddOutcome(
		ctx, own.userID, own.tenantID, foreign.opportunityID,
		"foreign-outcome", domain.OutcomeLost, "",
	); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("чужой исход: %v", err)
	}

	var recommendations, actions, outcomes, audits, idempotency int
	var riskStatus string
	var acknowledgedAt, actedAt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM recommendations WHERE tenant_id = $1),
			(SELECT count(*) FROM actions WHERE tenant_id = $1),
			(SELECT count(*) FROM outcomes WHERE tenant_id = $1),
			(SELECT count(*) FROM audit_log WHERE tenant_id = $1),
			(SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1),
			status, acknowledged_at, acted_at
		FROM risk_signals WHERE tenant_id = $1 AND id = $2`, own.tenantID, own.riskID,
	).Scan(&recommendations, &actions, &outcomes, &audits, &idempotency, &riskStatus, &acknowledgedAt, &actedAt); err != nil {
		t.Fatal(err)
	}
	if recommendations != 1 || actions != 1 || outcomes != 2 || audits != 3 || idempotency != 3 ||
		riskStatus != "ACTED" || acknowledgedAt == nil || actedAt == nil {
		t.Fatalf("состояние: recommendations=%d actions=%d outcomes=%d audits=%d keys=%d risk=%s ack=%v acted=%v",
			recommendations, actions, outcomes, audits, idempotency, riskStatus, acknowledgedAt, actedAt)
	}
}

func TestPostgresCorrectiveConcurrentReplayAndAtomicRollback(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	fixture := insertCorrectiveFixture(t, pool, tenant.TenantID, tenant.LocationID, tenant.UserID)
	service := application.NewService(NewPostgresStore(pool), allowCorrective{}, ids.Generator{}, time.Now)

	var createdCount atomic.Int32
	var wait sync.WaitGroup
	errorsFound := make(chan error, 20)
	identifiers := make(chan string, 20)
	for index := 0; index < 20; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			action, created, err := service.AddAction(
				ctx, fixture.userID, fixture.tenantID, fixture.riskID,
				"concurrent-action", domain.ActionCall, "Один звонок",
			)
			if err != nil {
				errorsFound <- err
				return
			}
			if created {
				createdCount.Add(1)
			}
			identifiers <- action.ID
		}()
	}
	wait.Wait()
	close(errorsFound)
	close(identifiers)
	for err := range errorsFound {
		t.Errorf("конкурентный вызов: %v", err)
	}
	var firstID string
	for id := range identifiers {
		if firstID == "" {
			firstID = id
		}
		if id != firstID {
			t.Errorf("повтор вернул другой ID: %s вместо %s", id, firstID)
		}
	}
	var actionCount, auditCount, keyCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM actions WHERE tenant_id = $1),
			(SELECT count(*) FROM audit_log WHERE tenant_id = $1),
			(SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1)`,
		fixture.tenantID,
	).Scan(&actionCount, &auditCount, &keyCount); err != nil {
		t.Fatal(err)
	}
	if createdCount.Load() != 1 || actionCount != 1 || auditCount != 1 || keyCount != 1 {
		t.Fatalf("гонка: created=%d actions=%d audits=%d keys=%d", createdCount.Load(), actionCount, auditCount, keyCount)
	}

	store := NewPostgresStore(pool)
	actionID, _ := (ids.Generator{}).NewID()
	action, _ := domain.NewAction(
		actionID, fixture.tenantID, fixture.riskID, fixture.userID,
		domain.ActionOther, "Проверка отката", time.Now(),
	)
	requestHash := [32]byte{1}
	_, _, err := store.AppendAction(ctx, action, "rollback-action", requestHash, application.AuditRecord{
		ID: firstAuditID(t, pool, fixture.tenantID), TenantID: fixture.tenantID,
		ActorID: fixture.userID, Operation: "ACTION_RECORDED", ResourceType: "ACTION",
		ResourceID: action.ID, At: action.CreatedAt,
	})
	if !errors.Is(err, application.ErrConflict) {
		t.Fatalf("ошибка аудита не откатила команду ожидаемым конфликтом: %v", err)
	}
	var rolledBackActions, rolledBackKeys int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM actions WHERE tenant_id = $1 AND id = $2),
			(SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1 AND key = 'rollback-action')`,
		fixture.tenantID, action.ID,
	).Scan(&rolledBackActions, &rolledBackKeys); err != nil {
		t.Fatal(err)
	}
	if rolledBackActions != 0 || rolledBackKeys != 0 {
		t.Fatalf("частичная запись после отката: actions=%d keys=%d", rolledBackActions, rolledBackKeys)
	}

	for table := range map[string]struct{}{"actions": {}, "audit_log": {}, "idempotency_keys": {}} {
		_, err := pool.Exec(ctx, "DELETE FROM "+table+" WHERE tenant_id = $1", fixture.tenantID)
		var postgresError *pgconn.PgError
		if !errors.As(err, &postgresError) || postgresError.Code != "55000" {
			t.Errorf("%s допускает удаление: %v", table, err)
		}
	}
}

func firstAuditID(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		SELECT id FROM audit_log WHERE tenant_id = $1 ORDER BY created_at, id LIMIT 1`, tenantID,
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertCorrectiveFixture(
	t *testing.T,
	pool *pgxpool.Pool,
	tenantID, locationID, userID string,
) correctiveFixture {
	t.Helper()
	generator := ids.Generator{}
	newID := func() string {
		id, err := generator.NewID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	connectionID := newID()
	contactID := newID()
	conversationID := newID()
	messageID := newID()
	opportunityID := newID()
	riskID := newID()
	now := time.Now().UTC()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO channel_connections(
			id, tenant_id, location_id, provider, name, status, capabilities,
			verification_secret_hash, created_at, updated_at
		) VALUES ($1,$2,$3,'TEST','Испытательный канал','ACTIVE','["MESSAGES"]',$4,$5,$5)`,
		connectionID, tenantID, locationID,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO contacts(id, tenant_id, display_name, created_at, updated_at)
		VALUES ($1,$2,'Испытательный клиент',$3,$3)`, contactID, tenantID, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO conversations(
			id, tenant_id, location_id, connection_id, contact_id, external_id,
			status, first_message_at, last_message_at, last_message_direction,
			revision, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,'ACTIVE',$7,$7,'INCOMING',1,$7,$7)`,
		conversationID, tenantID, locationID, connectionID, contactID, "dialog-"+conversationID, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO messages(
			id, tenant_id, conversation_id, connection_id, external_id,
			direction, type, text, sent_at, received_at, created_at
		) VALUES ($1,$2,$3,$4,$5,'INCOMING','TEXT','Нужна услуга',$6,$6,$6)`,
		messageID, tenantID, conversationID, connectionID, "message-"+messageID, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO opportunities(
			id, tenant_id, conversation_id, stage, currency, opened_at, created_at, updated_at
		) VALUES ($1,$2,$3,'NEW','RUB',$4,$4,$4)`, opportunityID, tenantID, conversationID, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO risk_signals(
			id, tenant_id, opportunity_id, location_id, type, severity, status,
			reason_code, reason_text, source, risk_engine_version, trigger_message_id,
			detected_at, due_at, created_at, updated_at
		) VALUES ($1,$2,$3,$4,'NO_RESPONSE','HIGH','OPEN','NO_RESPONSE_DUE',
			'Клиент ожидает ответа','RULE','no-response/v1',$5,$6,$7,$6,$6)`,
		riskID, tenantID, opportunityID, locationID, messageID, now, now.Add(-time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	return correctiveFixture{tenantID: tenantID, userID: userID, riskID: riskID, opportunityID: opportunityID}
}
