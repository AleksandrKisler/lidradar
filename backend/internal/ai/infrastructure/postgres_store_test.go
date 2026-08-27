package infrastructure_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
	"lidradar/backend/internal/ai/infrastructure"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

type finalizationMutatingStore struct {
	application.Store
	pool                     *pgxpool.Pool
	tenantID, conversationID string
	changed                  bool
}

func (store *finalizationMutatingStore) Finalize(ctx context.Context, final application.Finalization) (domain.Run, error) {
	if final.Summary != nil && !store.changed {
		store.changed = true
		if _, err := store.pool.Exec(ctx, `
			UPDATE conversations SET revision = revision + 1
			WHERE tenant_id = $1 AND id = $2`, store.tenantID, store.conversationID); err != nil {
			return domain.Run{}, err
		}
	}
	return store.Store.Finalize(ctx, final)
}

func TestPostgresAIQueuePersistsLifecycleAndSummary(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenants := testsupport.TwoTenants(t, ctx, pool)
	conversationID, messageID := insertAIConversation(t, pool, tenants.A, "Нужна запись на завтра")
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	store := infrastructure.NewPostgresStore(pool)
	builder := infrastructure.NewPostgresAnalysisJobBuilder(pool, "test-model-v1")
	service := application.NewService(store, ids.Generator{}, func() time.Time { return now }, application.DefaultLease).
		WithStaleJobBuilder(builder)
	node, err := service.RegisterNode(ctx, "AI-NODE-01", "secret-with-at-least-32-characters")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Heartbeat(ctx, node.ID, "secret-with-at-least-32-characters", application.HeartbeatCommand{
		Status: domain.NodeReady, ModelVersion: "test-model-v1", AvailableSlots: 1,
	}); err != nil {
		t.Fatal(err)
	}
	command, err := builder.BuildAnalysisJob(ctx, tenants.A.TenantID, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if command.AnalysisThroughMessageID != messageID || strings.Contains(command.Prompt, tenants.A.TenantID) {
		t.Fatalf("небезопасный или неверный prompt: %#v", command)
	}
	job, err := service.Enqueue(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := service.Enqueue(ctx, command)
	if err != nil || repeated.ID != job.ID {
		t.Fatalf("idempotent enqueue = %#v, %v", repeated, err)
	}
	conflictingCommand := command
	conflictingCommand.Prompt = command.Prompt + " "
	if _, err := service.Enqueue(ctx, conflictingCommand); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("conflicting enqueue error = %v", err)
	}
	claimed, found, err := service.Claim(ctx, node.ID, "secret-with-at-least-32-characters")
	if err != nil || !found || claimed.ID != job.ID || claimed.Status != domain.JobLeased {
		t.Fatalf("claim = %#v, %v, %v", claimed, found, err)
	}
	run, err := service.Started(ctx, node.ID, "secret-with-at-least-32-characters", job.ID)
	if err != nil {
		t.Fatal(err)
	}
	output := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"` + messageID + `","summary":"Клиент хочет записаться завтра.","facts":[]}`
	completed, err := service.Complete(ctx, node.ID, "secret-with-at-least-32-characters", job.ID, run.ID, output)
	if err != nil || completed.ApplicationStatus != domain.ApplicationApplied {
		t.Fatalf("complete = %#v, %v", completed, err)
	}
	var jobStatus domain.JobStatus
	var runStatus domain.RunStatus
	var summary string
	var storedDigest []byte
	if err := pool.QueryRow(ctx, `SELECT status FROM ai_jobs WHERE id = $1`, job.ID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM ai_runs WHERE id = $1`, run.ID).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT summary_text FROM conversation_summaries WHERE tenant_id = $1 AND conversation_id = $2`, tenants.A.TenantID, conversationID).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT secret_digest FROM ai_nodes WHERE id = $1`, node.ID).Scan(&storedDigest); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("secret-with-at-least-32-characters"))
	if jobStatus != domain.JobSucceeded || runStatus != domain.RunSucceeded ||
		summary != "Клиент хочет записаться завтра." || string(storedDigest) != string(digest[:]) {
		t.Fatalf("durable state = job %s, run %s, summary %q", jobStatus, runStatus, summary)
	}
}

func TestPostgresExpiredLeaseIsReclaimedAfterDisconnect(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	conversationID, messageID := insertAIConversation(t, pool, tenant, "Можно завтра вечером?")
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	store := infrastructure.NewPostgresStore(pool)
	service := application.NewService(store, ids.Generator{}, func() time.Time { return now }, application.DefaultLease)
	first, err := service.RegisterNode(ctx, "AI-NODE-01", "first-secret-with-at-least-32-chars")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.RegisterNode(ctx, "AI-NODE-02", "second-secret-with-at-least-32-chars")
	if err != nil {
		t.Fatal(err)
	}
	ready := application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: "test-model-v1", AvailableSlots: 1}
	if err := service.Heartbeat(ctx, first.ID, "first-secret-with-at-least-32-chars", ready); err != nil {
		t.Fatal(err)
	}
	job, err := service.Enqueue(ctx, application.EnqueueCommand{
		TenantID: tenant.TenantID, ConversationID: conversationID,
		Prompt:                   `{"analysisThroughMessageId":"` + messageID + `"}`,
		BaseConversationRevision: 1, AnalysisThroughMessageID: messageID,
		ModelVersion: "test-model-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := service.Claim(ctx, first.ID, "first-secret-with-at-least-32-chars"); err != nil || !found {
		t.Fatalf("first claim = %v, %v", found, err)
	}
	firstRun, err := service.Started(ctx, first.ID, "first-secret-with-at-least-32-chars", job.ID)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(application.DefaultLease + time.Second)
	if err := service.Heartbeat(ctx, second.ID, "second-secret-with-at-least-32-chars", ready); err != nil {
		t.Fatal(err)
	}
	reclaimed, found, err := service.Claim(ctx, second.ID, "second-secret-with-at-least-32-chars")
	if err != nil || !found || reclaimed.ID != job.ID || reclaimed.Attempts != 2 {
		t.Fatalf("reclaim = %#v, %v, %v", reclaimed, found, err)
	}
	oldOutput := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"` + messageID + `","summary":"Старый результат.","facts":[]}`
	if _, err := service.Complete(ctx, first.ID, "first-secret-with-at-least-32-chars", job.ID, firstRun.ID, oldOutput); !errors.Is(err, application.ErrLeaseLost) {
		t.Fatalf("old owner complete error = %v", err)
	}
	secondRun, err := service.Started(ctx, second.ID, "second-secret-with-at-least-32-chars", job.ID)
	if err != nil {
		t.Fatal(err)
	}
	newOutput := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"` + messageID + `","summary":"Новый результат.","facts":[]}`
	if _, err := service.Complete(ctx, second.ID, "second-secret-with-at-least-32-chars", job.ID, secondRun.ID, newOutput); err != nil {
		t.Fatal(err)
	}
	var oldStatus domain.RunStatus
	var oldError string
	if err := pool.QueryRow(ctx, `SELECT status, error_code FROM ai_runs WHERE id = $1`, firstRun.ID).Scan(&oldStatus, &oldError); err != nil {
		t.Fatal(err)
	}
	if oldStatus != domain.RunFailed || oldError != "LEASE_EXPIRED" {
		t.Fatalf("old run = %s/%s", oldStatus, oldError)
	}
}

func TestPostgresNodeSecretRotationAndRevocation(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	service := application.NewService(
		infrastructure.NewPostgresStore(pool), ids.Generator{}, func() time.Time { return now }, application.DefaultLease,
	)
	oldSecret := "old-secret-with-at-least-32-characters"
	newSecret := "new-secret-with-at-least-32-characters"
	node, err := service.RegisterNode(ctx, "AI-NODE-ROTATION", oldSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RotateNodeSecret(ctx, node.ID, newSecret); err != nil {
		t.Fatal(err)
	}
	ready := application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: "test-model", AvailableSlots: 1}
	if err := service.Heartbeat(ctx, node.ID, oldSecret, ready); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("old secret error = %v", err)
	}
	if err := service.Heartbeat(ctx, node.ID, newSecret, ready); err != nil {
		t.Fatal(err)
	}
	if err := service.RevokeNode(ctx, node.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Heartbeat(ctx, node.ID, newSecret, ready); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("revoked node error = %v", err)
	}
	var status domain.NodeStatus
	var revokedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, revoked_at FROM ai_nodes WHERE id = $1`, node.ID).Scan(&status, &revokedAt); err != nil {
		t.Fatal(err)
	}
	if status != domain.NodeRevoked || revokedAt == nil {
		t.Fatalf("node state = %s, %v", status, revokedAt)
	}
}

func TestPostgresClaimRequiresMatchingModelVersion(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	conversationID, messageID := insertAIConversation(t, pool, tenant, "Нужен расчёт")
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	service := application.NewService(
		infrastructure.NewPostgresStore(pool), ids.Generator{}, func() time.Time { return now }, application.DefaultLease,
	)
	secret := "model-secret-with-at-least-32-characters"
	node, err := service.RegisterNode(ctx, "AI-NODE-MODEL", secret)
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Enqueue(ctx, application.EnqueueCommand{
		TenantID: tenant.TenantID, ConversationID: conversationID,
		Prompt:                   `{"analysisThroughMessageId":"` + messageID + `"}`,
		BaseConversationRevision: 1, AnalysisThroughMessageID: messageID,
		ModelVersion: "required-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	ready := application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: "wrong-model", AvailableSlots: 1}
	if err := service.Heartbeat(ctx, node.ID, secret, ready); err != nil {
		t.Fatal(err)
	}
	if claimed, found, err := service.Claim(ctx, node.ID, secret); err != nil || found {
		t.Fatalf("mismatched claim = %#v, %v, %v", claimed, found, err)
	}
	ready.ModelVersion = "required-model"
	if err := service.Heartbeat(ctx, node.ID, secret, ready); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := service.Claim(ctx, node.ID, secret)
	if err != nil || !found || claimed.ID != job.ID {
		t.Fatalf("matching claim = %#v, %v, %v", claimed, found, err)
	}
}

func TestPostgresFinalizationAtomicallyRejectsFreshnessRace(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	conversationID, messageID := insertAIConversation(t, pool, tenant, "Есть ли место завтра?")
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	postgresStore := infrastructure.NewPostgresStore(pool)
	store := &finalizationMutatingStore{
		Store: postgresStore, pool: pool, tenantID: tenant.TenantID, conversationID: conversationID,
	}
	builder := infrastructure.NewPostgresAnalysisJobBuilder(pool, "test-model-v1")
	service := application.NewService(store, ids.Generator{}, func() time.Time { return now }, application.DefaultLease).
		WithStaleJobBuilder(builder)
	secret := "freshness-secret-with-at-least-32-characters"
	node, err := service.RegisterNode(ctx, "AI-NODE-FRESHNESS", secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Heartbeat(ctx, node.ID, secret, application.HeartbeatCommand{
		Status: domain.NodeReady, ModelVersion: "test-model-v1", AvailableSlots: 1,
	}); err != nil {
		t.Fatal(err)
	}
	command, err := builder.BuildAnalysisJob(ctx, tenant.TenantID, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Enqueue(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := service.Claim(ctx, node.ID, secret); err != nil || !found {
		t.Fatalf("claim = %v, %v", found, err)
	}
	run, err := service.Started(ctx, node.ID, secret, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	output := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"` + messageID + `","summary":"Уже устарело.","facts":[]}`
	completed, err := service.Complete(ctx, node.ID, secret, job.ID, run.ID, output)
	if err != nil || completed.ApplicationStatus != domain.ApplicationStale {
		t.Fatalf("complete = %#v, %v", completed, err)
	}
	var summaries, replacements int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM conversation_summaries
		WHERE tenant_id = $1 AND conversation_id = $2`, tenant.TenantID, conversationID).Scan(&summaries); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM ai_jobs
		WHERE tenant_id = $1 AND entity_id = $2 AND base_conversation_revision = 2 AND status = 'PENDING'`,
		tenant.TenantID, conversationID).Scan(&replacements); err != nil {
		t.Fatal(err)
	}
	if summaries != 0 || replacements != 1 {
		t.Fatalf("summaries = %d, replacements = %d", summaries, replacements)
	}
}

func insertAIConversation(t *testing.T, pool *pgxpool.Pool, tenant testsupport.TenantFixture, text string) (string, string) {
	t.Helper()
	ctx := context.Background()
	connectionID := mustID(t)
	contactID := mustID(t)
	conversationID := mustID(t)
	messageID := mustID(t)
	now := time.Date(2026, 8, 26, 11, 0, 0, 0, time.UTC)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO channel_connections(
			id, tenant_id, location_id, provider, name, status, capabilities, verification_secret_hash
		) VALUES ($1, $2, $3, 'TEST', 'AI test', 'ACTIVE', '["CAN_RECEIVE_MESSAGES"]', $4)`,
		connectionID, tenant.TenantID, tenant.LocationID, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO contacts(id, tenant_id, display_name) VALUES ($1, $2, 'AI customer')`, contactID, tenant.TenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO conversations(
			id, tenant_id, location_id, connection_id, contact_id, external_id, status,
			first_message_at, last_message_at, last_message_direction, revision
		) VALUES ($1, $2, $3, $4, $5, $6, 'ACTIVE', $7, $7, 'INCOMING', 1)`,
		conversationID, tenant.TenantID, tenant.LocationID, connectionID, contactID, "conversation-"+conversationID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO messages(
			id, tenant_id, conversation_id, connection_id, external_id, direction,
			type, text, sent_at, received_at
		) VALUES ($1, $2, $3, $4, $5, 'INCOMING', 'TEXT', $6, $7, $7)`,
		messageID, tenant.TenantID, conversationID, connectionID, "message-"+messageID, text, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return conversationID, messageID
}

func mustID(t *testing.T) string {
	t.Helper()
	id, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
