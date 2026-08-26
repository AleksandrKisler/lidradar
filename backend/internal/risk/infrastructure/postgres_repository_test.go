package infrastructure

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	opportunitydomain "lidradar/backend/internal/opportunity/domain"
	opportunityinfrastructure "lidradar/backend/internal/opportunity/infrastructure"
	"lidradar/backend/internal/risk/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

func TestPostgresRiskDedupResolveAndTenantIsolation(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	fixture := insertRiskFixture(t, pool, pair.A.TenantID, pair.A.LocationID, domain.DirectionIncoming)
	repository := NewPostgresRepository(pool)
	now := fixture.messageAt.Add(60 * time.Minute)

	const workers = 24
	created := make(chan bool, workers)
	errorsChannel := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			riskID, _ := (ids.Generator{}).NewID()
			risk, err := domain.NewNoResponse(riskID, riskFinding(fixture, now.Add(-15*time.Minute)), now)
			if err != nil {
				errorsChannel <- err
				return
			}
			_, wasCreated, err := repository.UpsertActive(ctx, risk)
			created <- wasCreated
			errorsChannel <- err
		}()
	}
	wait.Wait()
	close(created)
	close(errorsChannel)
	createdCount := 0
	for value := range created {
		if value {
			createdCount++
		}
	}
	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("конкурентная запись риска: %v", err)
		}
	}
	if createdCount != 1 {
		t.Fatalf("создано активных рисков = %d, нужен один", createdCount)
	}
	var openedEvents int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events
		WHERE tenant_id = $1 AND event_type = 'risk.opened' AND event_version = 1`, pair.A.TenantID).Scan(&openedEvents); err != nil {
		t.Fatal(err)
	}
	if openedEvents != 1 {
		t.Fatalf("событий risk.opened.v1 = %d, нужно одно", openedEvents)
	}
	if _, found, err := repository.FindActive(ctx, pair.B.TenantID, fixture.opportunityID, domain.TypeNoResponse); err != nil || found {
		t.Fatalf("чужой tenant: found=%v, err=%v", found, err)
	}
	if resolved, err := repository.ResolveActive(ctx, pair.A.TenantID, fixture.opportunityID, domain.TypeNoResponse, now.Add(time.Minute)); err != nil || !resolved {
		t.Fatalf("закрытие = %v, %v", resolved, err)
	}
	if resolved, err := repository.ResolveActive(ctx, pair.A.TenantID, fixture.opportunityID, domain.TypeNoResponse, now.Add(2*time.Minute)); err != nil || resolved {
		t.Fatalf("повторное закрытие = %v, %v", resolved, err)
	}

	riskID, _ := (ids.Generator{}).NewID()
	next, _ := domain.NewNoResponse(riskID, riskFinding(fixture, now), now.Add(time.Minute))
	if _, wasCreated, err := repository.UpsertActive(ctx, next); err != nil || !wasCreated {
		t.Fatalf("новый эпизод = %v, %v", wasCreated, err)
	}
	var total, active int
	if err := pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE status IN ('OPEN','ACKNOWLEDGED','ACTED'))
		FROM risk_signals WHERE tenant_id = $1 AND opportunity_id = $2`, pair.A.TenantID, fixture.opportunityID).Scan(&total, &active); err != nil {
		t.Fatal(err)
	}
	if total != 2 || active != 1 {
		t.Fatalf("всего=%d, активных=%d", total, active)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events
		WHERE tenant_id = $1 AND event_type = 'risk.opened' AND event_version = 1`, pair.A.TenantID).Scan(&openedEvents); err != nil {
		t.Fatal(err)
	}
	if openedEvents != 2 {
		t.Fatalf("событий двух эпизодов риска = %d, нужно два", openedEvents)
	}
}

func TestPostgresStateReaderUsesLatestCanonicalMessageAndBusinessHours(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	fixture := insertRiskFixture(t, pool, pair.A.TenantID, pair.A.LocationID, domain.DirectionIncoming)
	reader := NewPostgresStateReader(pool)

	state, err := reader.CurrentState(ctx, pair.A.TenantID, fixture.opportunityID)
	if err != nil || state.LastMeaningful != domain.DirectionIncoming || state.LastMeaningfulID != fixture.messageID ||
		state.ResponseThreshold != 45*time.Minute || len(state.BusinessHours.Weekly) != 7 {
		t.Fatalf("состояние = %#v, ошибка = %v", state, err)
	}
	if _, err := reader.CurrentState(ctx, pair.B.TenantID, fixture.opportunityID); err == nil {
		t.Fatal("чужой tenant прочитал состояние")
	}

	outgoingID, _ := (ids.Generator{}).NewID()
	outgoingAt := fixture.messageAt.Add(time.Minute)
	if _, err := pool.Exec(ctx, `
		INSERT INTO messages(
			id, tenant_id, conversation_id, connection_id, external_id, direction,
			type, text, sent_at, received_at, metadata, created_at
		) VALUES ($1,$2,$3,$4,$5,'OUTGOING','TEXT','Ответ',$6,$6,'{}'::jsonb,$6)`,
		outgoingID, pair.A.TenantID, fixture.conversationID, fixture.connectionID, "outgoing-"+outgoingID, outgoingAt); err != nil {
		t.Fatal(err)
	}
	state, err = reader.CurrentState(ctx, pair.A.TenantID, fixture.opportunityID)
	if err != nil || state.LastMeaningful != domain.DirectionOutgoing || state.LastMeaningfulID != outgoingID {
		t.Fatalf("состояние после ответа = %#v, ошибка = %v", state, err)
	}
}

type riskFixture struct {
	conversationID, connectionID, opportunityID, messageID, locationID, tenantID string
	messageAt                                                                    time.Time
}

func insertRiskFixture(
	t *testing.T,
	pool *pgxpool.Pool,
	tenantID, locationID string,
	direction domain.Direction,
) riskFixture {
	t.Helper()
	ctx := context.Background()
	generator := ids.Generator{}
	newID := func() string {
		value, err := generator.NewID()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	fixture := riskFixture{
		conversationID: newID(), connectionID: newID(), opportunityID: newID(),
		messageID: newID(), locationID: locationID, tenantID: tenantID,
		messageAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
	}
	contactID := newID()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO channel_connections(
			id, tenant_id, location_id, provider, name, status, capabilities,
			verification_secret_hash, created_at, updated_at
		) VALUES ($1,$2,$3,'TEST','Risk fixture','ACTIVE','["RECEIVE_MESSAGES"]'::jsonb,repeat('0',64),$4,$4)`,
		fixture.connectionID, tenantID, locationID, fixture.messageAt); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO contacts(id,tenant_id,display_name,created_at,updated_at) VALUES ($1,$2,'Клиент',$3,$3)`, contactID, tenantID, fixture.messageAt); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO conversations(
			id,tenant_id,location_id,connection_id,contact_id,external_id,status,
			first_message_at,last_message_at,last_message_direction,revision,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,'ACTIVE',$7,$7,$8,1,$7,$7)`,
		fixture.conversationID, tenantID, locationID, fixture.connectionID, contactID,
		"conversation-"+fixture.conversationID, fixture.messageAt, direction); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO messages(
			id,tenant_id,conversation_id,connection_id,external_id,direction,type,text,
			sent_at,received_at,metadata,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,'TEXT','Нужна услуга',$7,$7,'{}'::jsonb,$7)`,
		fixture.messageID, tenantID, fixture.conversationID, fixture.connectionID,
		"message-"+fixture.messageID, direction, fixture.messageAt); err != nil {
		t.Fatal(err)
	}
	for weekday := 1; weekday <= 7; weekday++ {
		if _, err := tx.Exec(ctx, `
			INSERT INTO location_business_hours(id,tenant_id,location_id,weekday,is_closed,opens_at,closes_at)
			VALUES ($1,$2,$3,$4,FALSE,'09:00','21:00')
			ON CONFLICT (tenant_id,location_id,weekday) DO NOTHING`, newID(), tenantID, locationID, weekday); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	opportunity, err := opportunitydomain.NewOpportunity(
		fixture.opportunityID, tenantID, fixture.conversationID, nil, nil, nil, "RUB", fixture.messageAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	history, err := opportunitydomain.NewHistory(
		newID(), tenantID, fixture.opportunityID, nil, opportunitydomain.StageNew,
		opportunitydomain.SourceRule, nil, nil, nil, fixture.messageAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := opportunityinfrastructure.NewPostgresRepository(pool).Create(ctx, opportunity, history); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func riskFinding(fixture riskFixture, dueAt time.Time) domain.Finding {
	return domain.Finding{
		TenantID: fixture.tenantID, OpportunityID: fixture.opportunityID, LocationID: fixture.locationID,
		TriggerMessageID: fixture.messageID, Severity: domain.SeverityHigh,
		PolicyVersion: domain.NoResponsePolicyVersion, ReasonCode: "NO_RESPONSE_THRESHOLD_EXCEEDED",
		Reason: "Бизнес не ответил клиенту", DueAt: dueAt,
	}
}
