package infrastructure

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/analytics/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

type analyticsSeed struct {
	t      *testing.T
	pool   *pgxpool.Pool
	tenant testsupport.TenantFixture
	ctx    context.Context
}

func (seed analyticsSeed) id() string {
	seed.t.Helper()
	value, err := (ids.Generator{}).NewID()
	if err != nil {
		seed.t.Fatal(err)
	}
	return value
}

func (seed analyticsSeed) exec(query string, args ...any) {
	seed.t.Helper()
	if _, err := seed.pool.Exec(seed.ctx, query, args...); err != nil {
		seed.t.Fatalf("%s: %v", query[:40], err)
	}
}

func at(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}

func (seed analyticsSeed) conversation(connectionID, contactID string, firstMessageAt *time.Time) string {
	conversationID := seed.id()
	var direction *string
	if firstMessageAt != nil {
		direction = pointer("INCOMING")
	}
	seed.exec(`
		INSERT INTO conversations(
			id,tenant_id,location_id,connection_id,contact_id,external_id,status,first_message_at,last_message_at,
			last_message_direction,revision,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,'ACTIVE',$7,$7,$9,1,$8,$8)`,
		conversationID, seed.tenant.TenantID, seed.tenant.LocationID, connectionID, contactID, "conversation-"+conversationID,
		firstMessageAt, at("2026-07-01T00:00:00Z"), direction)
	return conversationID
}

func (seed analyticsSeed) message(conversationID, connectionID, direction string, sentAt time.Time) string {
	messageID := seed.id()
	seed.exec(`
		INSERT INTO messages(id,tenant_id,conversation_id,connection_id,external_id,direction,type,text,sent_at,received_at,metadata,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,'TEXT','Сообщение',$7,$7,'{}'::jsonb,$7)`,
		messageID, seed.tenant.TenantID, conversationID, connectionID, "message-"+messageID, direction, sentAt)
	return messageID
}

func (seed analyticsSeed) opportunity(conversationID, stage, amount, currency string, openedAt time.Time, closedAt *time.Time) string {
	opportunityID := seed.id()
	seed.exec(`
		INSERT INTO opportunities(id,tenant_id,conversation_id,stage,estimated_amount,currency,opened_at,closed_at,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5::numeric,$6,$7,$8,$7,$7)`,
		opportunityID, seed.tenant.TenantID, conversationID, stage, amount, currency, openedAt, closedAt)
	return opportunityID
}

func (seed analyticsSeed) transition(opportunityID, from, to string, when time.Time) {
	seed.exec(`
		INSERT INTO opportunity_stage_history(id,tenant_id,opportunity_id,from_stage,to_stage,source,actor_user_id,created_at)
		VALUES ($1,$2,$3,$4,$5,'USER',$6,$7)`, seed.id(), seed.tenant.TenantID, opportunityID, from, to, seed.tenant.UserID, when)
}

func (seed analyticsSeed) risk(opportunityID, messageID, status string, detectedAt time.Time, acknowledgedAt, actedAt, resolvedAt *time.Time) string {
	riskID := seed.id()
	seed.exec(`
		INSERT INTO risk_signals(
			id,tenant_id,opportunity_id,location_id,type,severity,status,reason_code,reason_text,source,risk_engine_version,
			trigger_message_id,detected_at,due_at,acknowledged_at,acted_at,resolved_at,created_at,updated_at
		) VALUES ($1,$2,$3,$4,'NO_RESPONSE','HIGH',$5,'NO_RESPONSE_THRESHOLD_EXCEEDED','Бизнес не ответил','RULE','no-response/v1',
		          $6,$7,$7,$8,$9,$10,$7,$7)`,
		riskID, seed.tenant.TenantID, opportunityID, seed.tenant.LocationID, status, messageID, detectedAt, acknowledgedAt, actedAt, resolvedAt)
	return riskID
}

func (seed analyticsSeed) outcome(opportunityID, status string, when time.Time) string {
	outcomeID := seed.id()
	seed.exec(`
		INSERT INTO outcomes(id,tenant_id,opportunity_id,actor_user_id,status,note,created_at) VALUES ($1,$2,$3,$4,$5,'',$6)`,
		outcomeID, seed.tenant.TenantID, opportunityID, seed.tenant.UserID, status, when)
	return outcomeID
}

func (seed analyticsSeed) revenue(opportunityID, amount, currency, attribution string, confirmedAt time.Time, riskID, actionID, outcomeID *string) {
	eventID := seed.id()
	seed.exec(`
		INSERT INTO revenue_events(id,tenant_id,opportunity_id,amount,currency,status,source,confirmed_by_user_id,confirmed_at,created_at)
		VALUES ($1,$2,$3,$4::numeric,$5,'CONFIRMED','USER_CONFIRMED',$6,$7,$7)`,
		eventID, seed.tenant.TenantID, opportunityID, amount, currency, seed.tenant.UserID, confirmedAt)
	seed.exec(`
		INSERT INTO revenue_attributions(id,tenant_id,revenue_event_id,opportunity_id,type,risk_id,action_id,outcome_id,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		seed.id(), seed.tenant.TenantID, eventID, opportunityID, attribution, riskID, actionID, outcomeID, confirmedAt)
}

func pointer[T any](value T) *T { return &value }

// Окно август 2026 по Москве: [2026-07-31T21:00Z, 2026-08-31T21:00Z). Границы,
// чужие валюты, вторая организация и события вне окна не попадают в сводку;
// каждый показатель считается из необработанных таблиц (ADR 0039).
func TestPostgresAnalyticsSummaryCountsRawFactsInsidePeriod(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	seed := analyticsSeed{t: t, pool: pool, tenant: pair.A, ctx: ctx}
	connectionID, contactID := seed.id(), seed.id()
	seed.exec(`
		INSERT INTO channel_connections(id,tenant_id,location_id,provider,name,status,capabilities,verification_secret_hash,created_at,updated_at)
		VALUES ($1,$2,$3,'TEST','Аналитика','ACTIVE','["RECEIVE_MESSAGES"]'::jsonb,repeat('0',64),$4,$4)`,
		connectionID, pair.A.TenantID, pair.A.LocationID, at("2026-07-01T00:00:00Z"))
	seed.exec(`INSERT INTO contacts(id,tenant_id,display_name,created_at,updated_at) VALUES ($1,$2,'Клиент',$3,$3)`, contactID, pair.A.TenantID, at("2026-07-01T00:00:00Z"))

	inside := seed.conversation(connectionID, contactID, pointer(at("2026-07-31T21:30:00Z")))
	outside := seed.conversation(connectionID, contactID, pointer(at("2026-08-31T21:10:00Z")))
	euro := seed.conversation(connectionID, contactID, nil)
	incoming := seed.message(inside, connectionID, "INCOMING", at("2026-07-31T21:30:00Z"))
	outgoing := seed.message(inside, connectionID, "OUTGOING", at("2026-08-31T20:30:00Z"))
	seed.message(inside, connectionID, "SYSTEM", at("2026-08-10T10:00:00Z"))
	late := seed.message(outside, connectionID, "INCOMING", at("2026-08-31T21:10:00Z"))

	active := seed.opportunity(inside, "QUALIFYING", "5000.00", "RUB", at("2026-08-05T10:00:00Z"), nil)
	lost := seed.opportunity(outside, "LOST", "7000.00", "RUB", at("2026-08-06T10:00:00Z"), pointer(at("2026-08-15T10:00:00Z")))
	won := seed.opportunity(outside, "WON", "1000.00", "RUB", at("2026-07-01T10:00:00Z"), pointer(at("2026-08-20T10:00:00Z")))
	foreign := seed.opportunity(euro, "QUALIFYING", "900.00", "EUR", at("2026-08-07T10:00:00Z"), nil)
	seed.transition(active, "NEW", "BOOKED", at("2026-08-12T10:00:00Z"))
	seed.transition(active, "WAITING_BUSINESS", "BOOKED", at("2026-08-13T10:00:00Z"))
	seed.transition(lost, "NEW", "LOST", at("2026-08-15T10:00:00Z"))
	seed.transition(won, "BOOKED", "WON", at("2026-08-20T10:00:00Z"))
	seed.transition(foreign, "QUALIFYING", "WON", at("2026-09-01T10:00:00Z"))

	acted := seed.risk(active, incoming, "ACTED", at("2026-08-05T11:00:00Z"), pointer(at("2026-08-05T11:05:00Z")), pointer(at("2026-08-05T11:10:00Z")), nil)
	seed.risk(lost, late, "RESOLVED", at("2026-07-20T11:00:00Z"), nil, nil, pointer(at("2026-08-15T11:00:00Z")))
	seed.risk(won, outgoing, "FALSE_POSITIVE", at("2026-08-25T11:00:00Z"), nil, nil, pointer(at("2026-08-26T11:00:00Z")))
	seed.risk(foreign, incoming, "OPEN", at("2026-09-05T11:00:00Z"), nil, nil, nil)
	actionID := seed.id()
	seed.exec(`INSERT INTO actions(id,tenant_id,risk_id,opportunity_id,actor_user_id,type,note,created_at) VALUES ($1,$2,$3,$4,$5,'MARK_CONTACTED','',$6)`,
		actionID, pair.A.TenantID, acted, active, pair.A.UserID, at("2026-08-05T12:00:00Z"))
	seed.outcome(active, "BOOKED", at("2026-08-06T10:00:00Z"))
	paid := seed.outcome(active, "PAID", at("2026-08-07T10:00:00Z"))
	seed.outcome(lost, "LOST", at("2026-08-15T10:00:00Z"))
	seed.outcome(active, "THINKING", at("2026-08-08T10:00:00Z"))
	seed.outcome(won, "BOOKED", at("2026-07-15T10:00:00Z"))
	seed.revenue(active, "47000.00", "RUB", "RECOVERED", at("2026-08-08T10:00:00Z"), &acted, &actionID, &paid)
	seed.revenue(lost, "3000.00", "RUB", "ORGANIC", at("2026-08-20T10:00:00Z"), nil, nil, nil)
	seed.revenue(active, "100.00", "EUR", "UNKNOWN", at("2026-08-21T10:00:00Z"), nil, nil, nil)
	seed.revenue(active, "5000.00", "RUB", "ORGANIC", at("2026-09-02T10:00:00Z"), nil, nil, nil)

	other := analyticsSeed{t: t, pool: pool, tenant: pair.B, ctx: ctx}
	otherConnection, otherContact := other.id(), other.id()
	other.exec(`
		INSERT INTO channel_connections(id,tenant_id,location_id,provider,name,status,capabilities,verification_secret_hash,created_at,updated_at)
		VALUES ($1,$2,$3,'TEST','Аналитика B','ACTIVE','["RECEIVE_MESSAGES"]'::jsonb,repeat('1',64),$4,$4)`,
		otherConnection, pair.B.TenantID, pair.B.LocationID, at("2026-07-01T00:00:00Z"))
	other.exec(`INSERT INTO contacts(id,tenant_id,display_name,created_at,updated_at) VALUES ($1,$2,'Клиент B',$3,$3)`, otherContact, pair.B.TenantID, at("2026-07-01T00:00:00Z"))
	otherConversation := other.conversation(otherConnection, otherContact, pointer(at("2026-08-02T10:00:00Z")))
	other.message(otherConversation, otherConnection, "INCOMING", at("2026-08-02T10:00:00Z"))
	other.opportunity(otherConversation, "NEW", "1.00", "RUB", at("2026-08-02T10:00:00Z"), nil)

	store := NewPostgresStore(pool)
	organization, found, err := store.Organization(ctx, pair.A.TenantID)
	if err != nil || !found || organization.Timezone != "Europe/Moscow" || organization.Currency != "RUB" {
		t.Fatalf("организация: %#v, found=%v, %v", organization, found, err)
	}
	location, _ := time.LoadLocation("Europe/Moscow")
	period, err := domain.ResolvePeriod("2026-08-01", "2026-08-31", at("2026-09-03T10:00:00Z"), location)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(ctx, pair.A.TenantID, period, "RUB")
	if err != nil {
		t.Fatal(err)
	}
	summary.Risks = domain.RisksFromTypes(summary.Risks.ByType)
	if summary.Messages != (domain.Messages{Total: 3, Incoming: 1, Outgoing: 1, Conversations: 1}) {
		t.Fatalf("сообщения = %#v", summary.Messages)
	}
	if summary.Opportunities != (domain.Opportunities{Created: 3, Booked: 1, Won: 1, Lost: 1}) {
		t.Fatalf("сделки = %#v", summary.Opportunities)
	}
	if summary.Risks.RiskCounters != (domain.RiskCounters{Detected: 2, Acted: 1, Resolved: 1, FalsePositive: 1}) ||
		summary.Risks.ByType[0].RiskType != "NO_RESPONSE" || summary.Risks.ByType[0].Detected != 2 || summary.Risks.ByType[1].Detected != 0 {
		t.Fatalf("риски = %#v", summary.Risks)
	}
	if summary.Outcomes != (domain.Outcomes{Booked: 1, Paid: 1, Lost: 1}) {
		t.Fatalf("исходы = %#v", summary.Outcomes)
	}
	if summary.Revenue != (domain.Revenue{Currency: "RUB", Potential: "5000.00", Confirmed: "50000.00", ConfirmedRecovered: "47000.00", ConfirmedPayments: 2}) {
		t.Fatalf("деньги = %#v", summary.Revenue)
	}
	otherSummary, err := store.Summary(ctx, pair.B.TenantID, period, "RUB")
	if err != nil || otherSummary.Messages.Total != 1 || otherSummary.Opportunities.Created != 1 || otherSummary.Revenue.Confirmed != "0.00" ||
		len(otherSummary.Risks.ByType) != 0 {
		t.Fatalf("сводка другой организации = %#v, %v", otherSummary, err)
	}
	empty, err := domain.ResolvePeriod("2026-10-01", "2026-10-31", at("2026-11-03T10:00:00Z"), location)
	if err != nil {
		t.Fatal(err)
	}
	if blank, err := store.Summary(ctx, pair.A.TenantID, empty, "RUB"); err != nil || blank.Messages.Total != 0 || blank.Revenue.Potential != "0.00" ||
		blank.Revenue.ConfirmedRecovered != "0.00" || blank.Opportunities.Created != 0 {
		t.Fatalf("пустое окно = %#v, %v", blank, err)
	}
}
