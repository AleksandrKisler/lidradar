package infrastructure_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
	"lidradar/backend/internal/ai/infrastructure"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

const projectionSecret = "projection-test-secret-with-at-least-32-characters"

type projectionFixture struct {
	t                    *testing.T
	pool                 *pgxpool.Pool
	tenant, conversation string
	service              application.Service
	store                *infrastructure.PostgresStore
	node, secret         string
	now                  time.Time
	messages             []application.ContextMessage
}

func newProjectionFixture(t *testing.T, pool *pgxpool.Pool, tenant testsupport.TenantFixture) *projectionFixture {
	t.Helper()
	conversation, first := insertAIConversation(t, pool, tenant, "Проверю наличие завтра.")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE messages SET direction = 'OUTGOING' WHERE tenant_id = $1 AND id = $2`, tenant.TenantID, first); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE conversations SET last_message_direction = 'OUTGOING' WHERE tenant_id = $1 AND id = $2`, tenant.TenantID, conversation); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	store := infrastructure.NewPostgresStore(pool)
	f := &projectionFixture{t: t, pool: pool, tenant: tenant.TenantID, conversation: conversation, store: store, now: now, secret: projectionSecret + tenant.TenantID,
		messages: []application.ContextMessage{{ID: first, Direction: "OUTGOING", Body: "Проверю наличие завтра."}}}
	f.service = application.NewService(store, ids.Generator{}, func() time.Time { return f.now }, application.DefaultLease).WithAnalysisDebounce(0)
	node, err := f.service.RegisterNode(ctx, tenant.TenantID, "PROJECTION-TEST-"+tenant.TenantID, f.secret)
	if err != nil {
		t.Fatal(err)
	}
	f.node = node.ID
	return f
}

func (f *projectionFixture) appendMessage(direction, body string) string {
	f.t.Helper()
	ctx := context.Background()
	id := mustID(f.t)
	var connection string
	var revision int64
	if err := f.pool.QueryRow(ctx, `SELECT connection_id, revision FROM conversations WHERE tenant_id = $1 AND id = $2`, f.tenant, f.conversation).Scan(&connection, &revision); err != nil {
		f.t.Fatal(err)
	}
	at := time.Date(2026, 8, 26, 11, 0, 0, 0, time.UTC).Add(time.Duration(revision) * time.Minute)
	if _, err := f.pool.Exec(ctx, `INSERT INTO messages(id, tenant_id, conversation_id, connection_id, external_id, direction, type, text, sent_at, received_at) VALUES ($1, $2, $3, $4, $5, $6, 'TEXT', $7, $8, $8)`, id, f.tenant, f.conversation, connection, "projection-"+id, direction, body, at); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE conversations SET revision = revision + 1, last_message_at = $3, last_message_direction = $4, updated_at = $3 WHERE tenant_id = $1 AND id = $2`, f.tenant, f.conversation, at, direction); err != nil {
		f.t.Fatal(err)
	}
	f.messages = append(f.messages, application.ContextMessage{ID: id, Direction: direction, Body: body})
	return id
}

func (f *projectionFixture) complete(observations ...domain.AgreementObservation) string {
	f.t.Helper()
	ctx := context.Background()
	last := f.messages[len(f.messages)-1].ID
	var revision int64
	if err := f.pool.QueryRow(ctx, `SELECT revision FROM conversations WHERE tenant_id = $1 AND id = $2`, f.tenant, f.conversation).Scan(&revision); err != nil {
		f.t.Fatal(err)
	}
	request, err := application.BuildAnalysisContext(application.ConversationContext{
		TenantID: f.tenant, ConversationID: f.conversation, Revision: revision, Messages: f.messages,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	prompt, err := application.EncodeAnalysisRequest(request)
	if err != nil {
		f.t.Fatal(err)
	}
	job, err := f.service.Enqueue(ctx, application.EnqueueCommand{TenantID: f.tenant, ConversationID: f.conversation,
		Prompt: prompt, BaseConversationRevision: revision, AnalysisThroughMessageID: last,
		SchemaVersion: application.AnalysisSchemaV2, PromptVersion: application.CurrentAnalysisPrompt, ModelVersion: "projection-test-model"})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.service.Heartbeat(ctx, f.node, f.secret, application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: "projection-test-model", AvailableSlots: 1}); err != nil {
		f.t.Fatal(err)
	}
	if _, found, err := f.service.Claim(ctx, f.node, f.secret); err != nil || !found {
		f.t.Fatalf("claim: %v, %v", found, err)
	}
	run, err := f.service.Started(ctx, f.node, f.secret, job.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	if observations == nil {
		observations = []domain.AgreementObservation{}
	}
	output, err := json.Marshal(domain.AnalysisResultV2{SchemaVersion: application.AnalysisSchemaV2,
		AnalysisThroughMessageID: last, Summary: "Синтетическая переписка.", Facts: []domain.SemanticFact{}, Agreements: observations})
	if err != nil {
		f.t.Fatal(err)
	}
	completed, err := f.service.Complete(ctx, f.node, f.secret, job.ID, run.ID, string(output))
	if err != nil || completed.ApplicationStatus != domain.ApplicationApplied {
		f.t.Fatalf("complete: %#v, %v", completed, err)
	}
	f.now = f.now.Add(time.Minute)
	return run.ID
}

func (f *projectionFixture) agreements() []domain.Agreement {
	f.t.Helper()
	var raw []byte
	if err := f.pool.QueryRow(context.Background(), `SELECT agreements FROM conversation_summaries WHERE tenant_id = $1 AND conversation_id = $2`, f.tenant, f.conversation).Scan(&raw); err != nil {
		f.t.Fatal(err)
	}
	var agreements []domain.Agreement
	if err := json.Unmarshal(raw, &agreements); err != nil {
		f.t.Fatal(err)
	}
	return agreements
}

func TestPostgresAgreementProjectionRetainsIndependentExpectations(t *testing.T) {
	pool := testsupport.Postgres(t)
	tenants := testsupport.TwoTenants(t, context.Background(), pool)
	f := newProjectionFixture(t, pool, tenants.A)
	promise := f.messages[0].ID
	blocker := f.appendMessage("INCOMING", "Не могу оплатить, ошибка оплаты.")
	commitment := domain.AgreementObservation{Kind: domain.AgreementCommitment, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementPending, TriggerMessageID: promise, EvidenceMessageIDs: []string{promise}, Confidence: .96}
	blocked := domain.AgreementObservation{Kind: domain.AgreementPurchaseBlocker, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementPending, TriggerMessageID: blocker, EvidenceMessageIDs: []string{blocker}, Confidence: .96}
	first := f.complete(commitment, blocked)
	f.appendMessage("INCOMING", "Спасибо.")
	f.complete()
	got := f.agreements()
	if len(got) != 2 || !got[0].Trusted || !got[1].Trusted || got[0].SourceRunID != first || got[1].SourceRunID != first {
		t.Fatalf("omission: %#v", got)
	}
	fulfilled := f.appendMessage("OUTGOING", "Проверил наличие, деталь есть.")
	commitment.Status = domain.AgreementResolved
	commitment.EvidenceMessageIDs = []string{promise, fulfilled}
	resolvedRun := f.complete(commitment)
	got = f.agreements()
	if len(got) != 2 || got[0].Status != domain.AgreementResolved || got[0].SourceRunID != resolvedRun || got[1].Status != domain.AgreementPending || got[1].SourceRunID != first {
		t.Fatalf("partial resolution: %#v", got)
	}
	fixed := f.appendMessage("OUTGOING", "Исправили оплату, решено.")
	blocked.Status, blocked.Confidence, blocked.EvidenceMessageIDs = domain.AgreementResolved, .7, []string{blocker, fixed}
	f.complete(blocked)
	got = f.agreements()
	if got[1].Status != domain.AgreementPending || !got[1].Trusted || got[1].SourceRunID != first {
		t.Fatalf("weak resolution closed strong pending: %#v", got)
	}
	// A foreign run cannot become the origin of an agreement in this tenant.
	foreign := newProjectionFixture(t, pool, tenants.B)
	foreignRun := foreign.complete(domain.AgreementObservation{Kind: domain.AgreementCommitment, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementPending, TriggerMessageID: foreign.messages[0].ID, EvidenceMessageIDs: []string{foreign.messages[0].ID}, Confidence: .96})
	if _, err := pool.Exec(context.Background(), `UPDATE conversation_summaries SET agreements = jsonb_set(agreements, '{1,sourceRunId}', to_jsonb($3::text)) WHERE tenant_id = $1 AND conversation_id = $2`, f.tenant, f.conversation, foreignRun); err != nil {
		t.Fatal(err)
	}
	f.appendMessage("INCOMING", "Ещё вопрос.")
	f.complete()
	got = f.agreements()
	if got[1].Trusted || got[1].Status != domain.AgreementPending || got[1].SourceRunID != foreignRun {
		t.Fatalf("cross-tenant source trusted: %#v", got)
	}
}

func TestPostgresAgreementProjectionRejectsEditedOrDeletedOriginalEvidence(t *testing.T) {
	for _, change := range []string{"edited", "deleted", "direction"} {
		t.Run(change, func(t *testing.T) {
			pool := testsupport.Postgres(t)
			f := newProjectionFixture(t, pool, testsupport.TwoTenants(t, context.Background(), pool).A)
			promise := f.messages[0].ID
			f.complete(domain.AgreementObservation{Kind: domain.AgreementCommitment, WaitingFor: domain.AgreementBusiness, Status: domain.AgreementPending, TriggerMessageID: promise, EvidenceMessageIDs: []string{promise}, Confidence: .96})
			var err error
			switch change {
			case "edited":
				_, err = pool.Exec(context.Background(), `UPDATE messages SET text = 'Уточню завтра.' WHERE tenant_id = $1 AND id = $2`, f.tenant, promise)
				f.messages[0].Body = "Уточню завтра."
			case "deleted":
				_, err = pool.Exec(context.Background(), `UPDATE messages SET provider_deleted_at = now() WHERE tenant_id = $1 AND id = $2`, f.tenant, promise)
				f.messages = nil
			case "direction":
				_, err = pool.Exec(context.Background(), `UPDATE messages SET direction = 'INCOMING' WHERE tenant_id = $1 AND id = $2`, f.tenant, promise)
				f.messages[0].Direction = "INCOMING"
			}
			if err != nil {
				t.Fatal(err)
			}
			f.appendMessage("INCOMING", "Спасибо.")
			f.complete()
			got := f.agreements()
			if len(got) != 1 || got[0].Trusted || got[0].Status != domain.AgreementPending {
				t.Fatalf("changed evidence trusted: %#v", got)
			}
		})
	}
}

func TestPostgresAgreementProjectionKeepsVerifiedAnchorOutsideCurrentWindow(t *testing.T) {
	pool := testsupport.Postgres(t)
	f := newProjectionFixture(t, pool, testsupport.TwoTenants(t, context.Background(), pool).A)
	promise := f.messages[0].ID
	origin := f.complete(domain.AgreementObservation{Kind: domain.AgreementCommitment, WaitingFor: domain.AgreementBusiness,
		Status: domain.AgreementPending, TriggerMessageID: promise, EvidenceMessageIDs: []string{promise}, Confidence: .96})
	for i := 0; i < application.MaxContextMessages+1; i++ {
		f.appendMessage("INCOMING", "Спасибо.")
	}
	f.complete()
	got := f.agreements()
	if len(got) != 1 || !got[0].Trusted || got[0].SourceRunID != origin || got[0].Status != domain.AgreementPending {
		t.Fatalf("out-of-window anchor lost: %#v", got)
	}
}
