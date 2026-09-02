package infrastructure_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
	"lidradar/backend/internal/ai/infrastructure"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

const (
	stageRFirstSecret  = "first-secret-with-at-least-32-chars"
	stageRSecondSecret = "second-secret-with-at-least-32-chars"
)

// LR-BE-RM-016: зависший inference с живым heartbeat удерживает скользящую
// аренду, но абсолютный потолок отдаёт задание другому узлу, а поздний
// ответ зависшего узла отбрасывается.
func TestPostgresLeaseCapReclaimsJobWithLiveHeartbeat(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	conversationID, messageID := insertAIConversation(t, pool, tenant, "Запишите меня на завтра")
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	store := infrastructure.NewPostgresStore(pool)
	service := application.NewService(store, ids.Generator{}, func() time.Time { return now }, application.DefaultLease).
		WithAnalysisDebounce(0)
	first, err := service.RegisterNode(ctx, tenant.TenantID, "AI-NODE-HUNG", stageRFirstSecret)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.RegisterNode(ctx, tenant.TenantID, "AI-NODE-BACKUP", stageRSecondSecret)
	if err != nil {
		t.Fatal(err)
	}
	ready := application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: "test-model-v1", AvailableSlots: 1}
	if err := service.Heartbeat(ctx, first.ID, stageRFirstSecret, ready); err != nil {
		t.Fatal(err)
	}
	job, err := service.Enqueue(ctx, application.EnqueueCommand{
		TenantID: tenant.TenantID, ConversationID: conversationID,
		Prompt:                   `{"analysisThroughMessageId":"` + messageID + `"}`,
		BaseConversationRevision: 1, AnalysisThroughMessageID: messageID, ModelVersion: "test-model-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := service.Claim(ctx, first.ID, stageRFirstSecret); err != nil || !found {
		t.Fatalf("захват = %v, %v", found, err)
	}
	hungRun, err := service.Started(ctx, first.ID, stageRFirstSecret, job.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Heartbeat каждые 10 секунд продлевает lease_until, но не leased_at.
	busy := application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: "test-model-v1", AvailableSlots: 0}
	for elapsed := time.Duration(0); elapsed < application.LeaseCap; elapsed += 10 * time.Second {
		now = now.Add(10 * time.Second)
		if err := service.Heartbeat(ctx, first.ID, stageRFirstSecret, busy); err != nil {
			t.Fatal(err)
		}
	}
	var leaseUntil, leasedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT lease_until, leased_at FROM ai_jobs WHERE id = $1`, job.ID).Scan(&leaseUntil, &leasedAt); err != nil {
		t.Fatal(err)
	}
	if !leaseUntil.After(now) || !leasedAt.Equal(time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("скользящая аренда до %s, захват %s: потолок должен считаться от захвата", leaseUntil, leasedAt)
	}

	if err := service.Heartbeat(ctx, second.ID, stageRSecondSecret, ready); err != nil {
		t.Fatal(err)
	}
	reclaimed, found, err := service.Claim(ctx, second.ID, stageRSecondSecret)
	if err != nil || !found || reclaimed.ID != job.ID || reclaimed.Attempts != 2 || !reclaimed.LeasedAt.Equal(now) {
		t.Fatalf("перехват по потолку = %#v, %v, %v", reclaimed, found, err)
	}
	var hungStatus domain.RunStatus
	var hungCode string
	if err := pool.QueryRow(ctx, `SELECT status, error_code FROM ai_runs WHERE id = $1`, hungRun.ID).Scan(&hungStatus, &hungCode); err != nil {
		t.Fatal(err)
	}
	if hungStatus != domain.RunFailed || hungCode != "LEASE_CAP_EXCEEDED" {
		t.Fatalf("зависший run = %s/%s", hungStatus, hungCode)
	}
	output := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"` + messageID + `","summary":"Поздний ответ.","facts":[]}`
	if _, err := service.Complete(ctx, first.ID, stageRFirstSecret, job.ID, hungRun.ID, output); !errors.Is(err, application.ErrLeaseLost) {
		t.Fatalf("поздний complete зависшего узла = %v; ожидался ErrLeaseLost", err)
	}
	backupRun, err := service.Started(ctx, second.ID, stageRSecondSecret, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed, err := service.Complete(ctx, second.ID, stageRSecondSecret, job.ID, backupRun.ID, output); err != nil ||
		completed.ApplicationStatus != domain.ApplicationApplied {
		t.Fatalf("завершение резервным узлом = %#v, %v", completed, err)
	}
}

// LR-BE-RM-006: всплеск сообщений порождает одно ожидающее задание с последним
// снимком; захват возможен не раньше, чем через паузу дебаунса от первого
// сообщения, а более старый снимок не откатывает ожидающее задание.
func TestPostgresBurstOfMessagesQueuesSingleAnalysisJob(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	conversationID, firstMessageID := insertAIConversation(t, pool, tenant, "Первое сообщение")
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	store := infrastructure.NewPostgresStore(pool)
	service := application.NewService(store, ids.Generator{}, func() time.Time { return now }, application.DefaultLease)
	node, err := service.RegisterNode(ctx, tenant.TenantID, "AI-NODE-BURST", stageRFirstSecret)
	if err != nil {
		t.Fatal(err)
	}
	ready := application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: "test-model-v1", AvailableSlots: 1}

	messageIDs := []string{firstMessageID}
	first, err := service.Enqueue(ctx, burstCommand(tenant.TenantID, conversationID, 1, firstMessageID))
	if err != nil {
		t.Fatal(err)
	}
	for revision := int64(2); revision <= 5; revision++ {
		now = now.Add(4 * time.Second)
		messageID := appendAIMessage(t, pool, tenant.TenantID, conversationID, fmt.Sprintf("Сообщение %d", revision), now)
		messageIDs = append(messageIDs, messageID)
		if _, err := service.Enqueue(ctx, burstCommand(tenant.TenantID, conversationID, revision, messageID)); err != nil {
			t.Fatal(err)
		}
	}
	// Запоздавший старый снимок ничего не откатывает.
	if _, err := service.Enqueue(ctx, burstCommand(tenant.TenantID, conversationID, 3, messageIDs[2])); err != nil {
		t.Fatal(err)
	}

	var queued int
	var revision int64
	var throughMessageID string
	var availableAt time.Time
	if err := pool.QueryRow(ctx, `
		SELECT count(*), max(base_conversation_revision), max(analysis_through_message_id::text), min(available_at)
		FROM ai_jobs
		WHERE tenant_id = $1 AND entity_id = $2 AND status IN ('PENDING', 'RETRY')`,
		tenant.TenantID, conversationID).Scan(&queued, &revision, &throughMessageID, &availableAt); err != nil {
		t.Fatal(err)
	}
	if queued != 1 || revision != 5 || throughMessageID != messageIDs[4] ||
		!availableAt.Equal(first.AvailableAt) || !first.AvailableAt.Equal(first.CreatedAt.Add(application.AnalysisDebounce)) {
		t.Fatalf("ожидающих %d, ревизия %d, сообщение %s, доступно с %s (первое задание %s)",
			queued, revision, throughMessageID, availableAt, first.AvailableAt)
	}

	if err := service.Heartbeat(ctx, node.ID, stageRFirstSecret, ready); err != nil {
		t.Fatal(err)
	}
	if _, found, err := service.Claim(ctx, node.ID, stageRFirstSecret); err != nil || found {
		t.Fatalf("захват до истечения дебаунса = %v, %v", found, err)
	}
	now = first.CreatedAt.Add(application.AnalysisDebounce)
	if err := service.Heartbeat(ctx, node.ID, stageRFirstSecret, ready); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := service.Claim(ctx, node.ID, stageRFirstSecret)
	if err != nil || !found || claimed.ID != first.ID || claimed.BaseConversationRevision != 5 {
		t.Fatalf("захват после дебаунса = %#v, %v, %v", claimed, found, err)
	}
	// Новое сообщение во время выполнения ставит отдельное ожидающее задание, а
	// не меняет снимок захваченного: узел уже получил его инструкцию.
	sixth := appendAIMessage(t, pool, tenant.TenantID, conversationID, "Сообщение 6", now.Add(time.Second))
	late, err := service.Enqueue(ctx, burstCommand(tenant.TenantID, conversationID, 6, sixth))
	if err != nil || late.ID == first.ID || late.BaseConversationRevision != 6 {
		t.Fatalf("задание во время выполнения = %#v, %v", late, err)
	}
	var runningRevision int64
	if err := pool.QueryRow(ctx, `SELECT base_conversation_revision FROM ai_jobs WHERE id = $1`, first.ID).Scan(&runningRevision); err != nil {
		t.Fatal(err)
	}
	if runningRevision != 5 {
		t.Fatalf("снимок захваченного задания изменился: %d", runningRevision)
	}
}

// LR-BE-RM-005: два run с ревизиями 5 и 7 в любом порядке фиксации оставляют в
// проекции результат ревизии 7; устаревший run получает STALE.
func TestPostgresOlderRunNeverOverwritesNewerSummary(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	conversationID, firstMessageID := insertAIConversation(t, pool, tenant, "Первое сообщение")
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	store := infrastructure.NewPostgresStore(pool)
	service := application.NewService(store, ids.Generator{}, func() time.Time { return now }, application.DefaultLease).
		WithAnalysisDebounce(0).
		WithStaleJobBuilder(infrastructure.NewPostgresAnalysisJobBuilder(pool, "test-model-v1"))
	first, err := service.RegisterNode(ctx, tenant.TenantID, "AI-NODE-A", stageRFirstSecret)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.RegisterNode(ctx, tenant.TenantID, "AI-NODE-B", stageRSecondSecret)
	if err != nil {
		t.Fatal(err)
	}
	ready := application.HeartbeatCommand{Status: domain.NodeReady, ModelVersion: "test-model-v1", AvailableSlots: 1}

	// Ревизия 5: четыре дополнительных сообщения.
	messageID := firstMessageID
	for revision := 2; revision <= 5; revision++ {
		now = now.Add(time.Second)
		messageID = appendAIMessage(t, pool, tenant.TenantID, conversationID, fmt.Sprintf("Сообщение %d", revision), now)
	}
	oldJob, err := service.Enqueue(ctx, burstCommand(tenant.TenantID, conversationID, 5, messageID))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Heartbeat(ctx, first.ID, stageRFirstSecret, ready); err != nil {
		t.Fatal(err)
	}
	if _, found, err := service.Claim(ctx, first.ID, stageRFirstSecret); err != nil || !found {
		t.Fatalf("захват ревизии 5 = %v, %v", found, err)
	}
	oldRun, err := service.Started(ctx, first.ID, stageRFirstSecret, oldJob.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Ревизия 7 появляется, пока ревизия 5 выполняется; её анализирует второй узел.
	for revision := 6; revision <= 7; revision++ {
		now = now.Add(time.Second)
		messageID = appendAIMessage(t, pool, tenant.TenantID, conversationID, fmt.Sprintf("Сообщение %d", revision), now)
	}
	newJob, err := service.Enqueue(ctx, burstCommand(tenant.TenantID, conversationID, 7, messageID))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Heartbeat(ctx, second.ID, stageRSecondSecret, ready); err != nil {
		t.Fatal(err)
	}
	if claimed, found, err := service.Claim(ctx, second.ID, stageRSecondSecret); err != nil || !found || claimed.ID != newJob.ID {
		t.Fatalf("захват ревизии 7 = %#v, %v, %v", claimed, found, err)
	}
	newRun, err := service.Started(ctx, second.ID, stageRSecondSecret, newJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	newOutput := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"` + messageID +
		`","summary":"Результат ревизии 7.","facts":[]}`
	if completed, err := service.Complete(ctx, second.ID, stageRSecondSecret, newJob.ID, newRun.ID, newOutput); err != nil ||
		completed.ApplicationStatus != domain.ApplicationApplied {
		t.Fatalf("завершение ревизии 7 = %#v, %v", completed, err)
	}
	oldOutput := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"` + oldJob.AnalysisThroughMessageID +
		`","summary":"Результат ревизии 5.","facts":[]}`
	completed, err := service.Complete(ctx, first.ID, stageRFirstSecret, oldJob.ID, oldRun.ID, oldOutput)
	if err != nil || completed.ApplicationStatus != domain.ApplicationStale {
		t.Fatalf("поздний run ревизии 5 = %#v, %v; ожидался STALE", completed, err)
	}
	var summaryRevision int64
	var summaryText string
	if err := pool.QueryRow(ctx, `
		SELECT base_conversation_revision, summary_text FROM conversation_summaries
		WHERE tenant_id = $1 AND conversation_id = $2`, tenant.TenantID, conversationID).Scan(&summaryRevision, &summaryText); err != nil {
		t.Fatal(err)
	}
	if summaryRevision != 7 || summaryText != "Результат ревизии 7." {
		t.Fatalf("проекция = ревизия %d, %q", summaryRevision, summaryText)
	}
}

// LR-BE-RM-017: слабый факт (0.65–0.849) сохраняется с trusted = false и не
// виден запросам, которые открывают Risk.
func TestPostgresWeakFactIsStoredUntrusted(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	tenant := testsupport.TwoTenants(t, ctx, pool).A
	conversationID, messageID := insertAIConversation(t, pool, tenant, "Наверное, запишусь")
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	store := infrastructure.NewPostgresStore(pool)
	service := application.NewService(store, ids.Generator{}, func() time.Time { return now }, application.DefaultLease).
		WithAnalysisDebounce(0)
	node, err := service.RegisterNode(ctx, tenant.TenantID, "AI-NODE-WEAK", stageRFirstSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Heartbeat(ctx, node.ID, stageRFirstSecret, application.HeartbeatCommand{
		Status: domain.NodeReady, ModelVersion: "test-model-v1", AvailableSlots: 1,
	}); err != nil {
		t.Fatal(err)
	}
	job, err := service.Enqueue(ctx, burstCommand(tenant.TenantID, conversationID, 1, messageID))
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := service.Claim(ctx, node.ID, stageRFirstSecret); err != nil || !found {
		t.Fatalf("захват = %v, %v", found, err)
	}
	run, err := service.Started(ctx, node.ID, stageRFirstSecret, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	output := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"` + messageID +
		`","summary":"Клиент колеблется.","facts":[` +
		`{"type":"BOOKING_INTENT","value":true,"confidence":0.70,"evidenceMessageIds":["` + messageID + `"]},` +
		`{"type":"FOLLOW_UP_CANDIDATE","value":true,"confidence":0.90,"evidenceMessageIds":["` + messageID + `"]},` +
		`{"type":"PRICE_MENTIONED","value":false,"confidence":0.50,"evidenceMessageIds":["` + messageID + `"]}]}`
	if completed, err := service.Complete(ctx, node.ID, stageRFirstSecret, job.ID, run.ID, output); err != nil ||
		completed.ApplicationStatus != domain.ApplicationApplied {
		t.Fatalf("завершение = %#v, %v", completed, err)
	}
	var weakStored, strongStored, untrustedDropped, trustedBooking bool
	if err := pool.QueryRow(ctx, `
		SELECT semantic_facts @> '[{"type":"BOOKING_INTENT","trusted":false}]'::jsonb,
		       semantic_facts @> '[{"type":"FOLLOW_UP_CANDIDATE","trusted":true}]'::jsonb,
		       NOT semantic_facts @> '[{"type":"PRICE_MENTIONED"}]'::jsonb,
		       EXISTS (
			SELECT 1 FROM jsonb_array_elements(semantic_facts) AS fact(value)
			WHERE fact.value ->> 'type' = 'BOOKING_INTENT' AND (fact.value ->> 'trusted')::boolean
		       )
		FROM conversation_summaries WHERE tenant_id = $1 AND conversation_id = $2`,
		tenant.TenantID, conversationID).Scan(&weakStored, &strongStored, &untrustedDropped, &trustedBooking); err != nil {
		t.Fatal(err)
	}
	if !weakStored || !strongStored || !untrustedDropped || trustedBooking {
		t.Fatalf("слабый сохранён=%v, сильный доверенный=%v, ненадёжный отброшен=%v, слабый виден как доверенный=%v",
			weakStored, strongStored, untrustedDropped, trustedBooking)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE conversation_summaries SET semantic_facts = '[{"type":"BOOKING_INTENT","value":true,"confidence":0.9}]'::jsonb
		WHERE tenant_id = $1 AND conversation_id = $2`, tenant.TenantID, conversationID); err == nil {
		t.Fatal("факт без признака trusted принят базой")
	}
}

func burstCommand(tenantID, conversationID string, revision int64, messageID string) application.EnqueueCommand {
	return application.EnqueueCommand{
		TenantID: tenantID, ConversationID: conversationID,
		Prompt:                   fmt.Sprintf(`{"revision":%d,"analysisThroughMessageId":"%s"}`, revision, messageID),
		BaseConversationRevision: revision, AnalysisThroughMessageID: messageID, ModelVersion: "test-model-v1",
	}
}

// appendAIMessage добавляет входящее сообщение и поднимает ревизию переписки,
// как это делает канонический контур.
func appendAIMessage(t *testing.T, pool *pgxpool.Pool, tenantID, conversationID, text string, at time.Time) string {
	t.Helper()
	ctx := context.Background()
	messageID := mustID(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO messages(
			id, tenant_id, conversation_id, connection_id, external_id, direction,
			type, text, sent_at, received_at
		)
		SELECT $1, $2, $3, connection_id, $4, 'INCOMING', 'TEXT', $5, $6, $6
		FROM conversations WHERE tenant_id = $2 AND id = $3`,
		messageID, tenantID, conversationID, "message-"+messageID, text, at); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE conversations
		SET revision = revision + 1, last_message_at = $3, last_message_direction = 'INCOMING', updated_at = $3
		WHERE tenant_id = $1 AND id = $2`, tenantID, conversationID, at); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return messageID
}
