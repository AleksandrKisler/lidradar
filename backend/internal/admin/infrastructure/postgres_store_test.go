package infrastructure

import (
	"context"
	"errors"
	"testing"
	"time"

	"lidradar/backend/internal/admin/application"
	"lidradar/backend/internal/admin/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

func newID(t *testing.T) string {
	t.Helper()
	value, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func cliAudit(t *testing.T, operation, entityID string, at time.Time) domain.AuditEntry {
	t.Helper()
	return domain.AuditEntry{ID: newID(t), Source: domain.SourceCLI, Operation: operation, EntityType: "PLATFORM_ADMIN", EntityID: entityID, Details: map[string]any{}, At: at}
}

func apiAudit(t *testing.T, actor, operation, entityType, entityID string, at time.Time) domain.AuditEntry {
	t.Helper()
	return domain.AuditEntry{ID: newID(t), ActorUserID: &actor, Source: domain.SourceAPI, Operation: operation, EntityType: entityType, EntityID: entityID, Details: map[string]any{}, At: at}
}

// LR-BE-RM-008: Grant → Revoke → Grant для одного пользователя проходит; в
// таблице две строки, активная одна; удаление и повторное изменение отвергаются.
func TestPostgresPlatformAdminGrantRevokeRegrant(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	store := NewPostgresStore(pool)
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	if admin, err := store.IsAdmin(ctx, pair.A.UserID); err != nil || admin {
		t.Fatalf("право до выдачи: %v, %v", admin, err)
	}
	user, found, err := store.UserByEmail(ctx, "a@tenant.test")
	if err != nil || !found || user.ID != pair.A.UserID {
		t.Fatalf("пользователь по почте: %#v, found=%v, %v", user, found, err)
	}
	first, _ := domain.NewAdmin(newID(t), pair.A.UserID, nil, "bootstrap", now)
	granted, created, err := store.GrantAdmin(ctx, first, cliAudit(t, "PLATFORM_ADMIN_GRANTED", first.ID, now))
	if err != nil || !created || granted.ID != first.ID || granted.Email != "a@tenant.test" || granted.GrantedBy != nil {
		t.Fatalf("выдача: %#v, created=%v, %v", granted, created, err)
	}
	if admin, err := store.IsAdmin(ctx, pair.A.UserID); err != nil || !admin {
		t.Fatalf("право после выдачи: %v, %v", admin, err)
	}
	replay, _ := domain.NewAdmin(newID(t), pair.A.UserID, nil, "", now.Add(time.Second))
	if again, created, err := store.GrantAdmin(ctx, replay, cliAudit(t, "PLATFORM_ADMIN_GRANTED", replay.ID, now)); err != nil || created || again.ID != first.ID {
		t.Fatalf("повторная выдача: %#v, created=%v, %v", again, created, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM platform_admins WHERE id = $1`, first.ID); err == nil {
		t.Fatal("строка права удалена физически")
	}
	actor := pair.B.UserID
	if revoked, err := store.RevokeAdmin(ctx, pair.A.UserID, &actor, now.Add(time.Minute), apiAudit(t, actor, "PLATFORM_ADMIN_REVOKED", "PLATFORM_ADMIN", pair.A.UserID, now)); err != nil || !revoked {
		t.Fatalf("отзыв: %v, %v", revoked, err)
	}
	if revoked, err := store.RevokeAdmin(ctx, pair.A.UserID, &actor, now.Add(2*time.Minute), apiAudit(t, actor, "PLATFORM_ADMIN_REVOKED", "PLATFORM_ADMIN", pair.A.UserID, now)); err != nil || revoked {
		t.Fatalf("повторный отзыв: %v, %v", revoked, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE platform_admins SET revoked_at = NULL WHERE id = $1`, first.ID); err == nil {
		t.Fatal("отозванное право восстановлено напрямую")
	}
	second, _ := domain.NewAdmin(newID(t), pair.A.UserID, &actor, "повторно", now.Add(time.Hour))
	if regranted, created, err := store.GrantAdmin(ctx, second, apiAudit(t, actor, "PLATFORM_ADMIN_GRANTED", "PLATFORM_ADMIN", second.ID, now)); err != nil || !created || regranted.ID != second.ID {
		t.Fatalf("повторная выдача после отзыва: %#v, created=%v, %v", regranted, created, err)
	}
	admins, err := store.Admins(ctx)
	if err != nil || len(admins) != 2 {
		t.Fatalf("история выдач: %#v, %v", admins, err)
	}
	var rows, active, audits int
	if err := pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE revoked_at IS NULL),
		       (SELECT count(*) FROM admin_audit_log WHERE entity_type = 'PLATFORM_ADMIN')
		FROM platform_admins WHERE user_id = $1`, pair.A.UserID).Scan(&rows, &active, &audits); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || active != 1 || audits != 3 {
		t.Fatalf("строк=%d активных=%d аудит=%d", rows, active, audits)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM admin_audit_log`); err == nil {
		t.Fatal("журнал административных команд удалён")
	}
}

// Мёртвое задание и событие видны в панели, повтор возвращает их в очередь,
// отложенное уходит из панели, каждая команда пишет аудит с организацией.
func TestPostgresAdminDeadLettersRetryDiscardAndReadModels(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	store := NewPostgresStore(pool)
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	deadJob, liveJob, deadEvent := newID(t), newID(t), newID(t)
	for _, job := range []struct {
		id, status, dedup string
		attempts          int
		completed         *time.Time
	}{
		{deadJob, "DEAD", "dead-1", 5, &now}, {liveJob, "DEAD", "dead-2", 5, &now},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO jobs(id, tenant_id, job_type, dedup_key, payload, status, priority, available_at, attempt_count, max_attempts,
			                 last_error_code, completed_at, created_at, updated_at)
			VALUES ($1, $2, 'risk.evaluate-no-response.v1', $3, '{"opportunityId":"x"}'::jsonb, $4, 0, $5, $6, 5, 'SIMULATED_FAILURE', $7, $5, $5)`,
			job.id, pair.A.TenantID, job.dedup, job.status, now, job.attempts, job.completed); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO outbox_events(id, tenant_id, event_type, event_version, aggregate_type, aggregate_id, trace_id, data, status,
		                          available_at, attempt_count, max_attempts, last_error_code, occurred_at, completed_at, created_at, updated_at)
		VALUES ($1, $2, 'risk.opened', 1, 'risk', $3, $4, '{}'::jsonb, 'DEAD', $5, 5, 5, 'HANDLER_CRASHED', $5, $5, $5, $5)`,
		deadEvent, pair.A.TenantID, newID(t), newID(t), now); err != nil {
		t.Fatal(err)
	}
	letters, err := store.DeadLetters(ctx, 50)
	if err != nil || len(letters.Jobs) != 2 || len(letters.Outbox) != 1 || len(letters.AIJobs) != 0 || len(letters.Deliveries) != 0 ||
		*letters.Outbox[0].LastErrorCode != "HANDLER_CRASHED" {
		t.Fatalf("панель мёртвых: %#v, %v", letters, err)
	}
	stats, err := store.QueueStats(ctx, now)
	if err != nil || stats.Jobs.Dead != 2 || stats.Outbox.Dead != 1 || stats.DeadUnhandled != 3 || stats.AIJobs.NodesReady != 0 {
		t.Fatalf("состояние очередей: %#v, %v", stats, err)
	}
	actor := pair.A.UserID
	retried, err := store.RetryJob(ctx, deadJob, now.Add(time.Minute), apiAudit(t, actor, "ADMIN_JOB_RETRIED", "JOB", deadJob, now))
	if err != nil || retried.Status != "PENDING" || retried.AttemptCount != 0 || retried.CompletedAt != nil || retried.LeasedBy != nil {
		t.Fatalf("повтор задания: %#v, %v", retried, err)
	}
	if _, err := store.RetryJob(ctx, deadJob, now.Add(time.Minute), apiAudit(t, actor, "ADMIN_JOB_RETRIED", "JOB", deadJob, now)); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("повтор живого задания: %v", err)
	}
	if _, err := store.RetryJob(ctx, newID(t), now, apiAudit(t, actor, "ADMIN_JOB_RETRIED", "JOB", deadJob, now)); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("повтор отсутствующего задания: %v", err)
	}
	discarded, err := store.DiscardJob(ctx, liveJob, actor, now.Add(time.Minute), apiAudit(t, actor, "ADMIN_JOB_DISCARDED", "JOB", liveJob, now))
	if err != nil || discarded.DiscardedAt == nil || discarded.Status != "DEAD" {
		t.Fatalf("откладывание задания: %#v, %v", discarded, err)
	}
	if _, err := store.DiscardJob(ctx, liveJob, actor, now.Add(time.Minute), apiAudit(t, actor, "ADMIN_JOB_DISCARDED", "JOB", liveJob, now)); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("повторное откладывание: %v", err)
	}
	replayed, err := store.ReplayEvent(ctx, deadEvent, now.Add(time.Minute), apiAudit(t, actor, "ADMIN_EVENT_REPLAYED", "OUTBOX_EVENT", deadEvent, now))
	if err != nil || replayed.Status != "PENDING" || replayed.AttemptCount != 0 {
		t.Fatalf("повтор события: %#v, %v", replayed, err)
	}
	letters, err = store.DeadLetters(ctx, 50)
	if err != nil || len(letters.Jobs) != 0 || len(letters.Outbox) != 0 {
		t.Fatalf("панель после команд: %#v, %v", letters, err)
	}
	var audits int
	var auditTenant string
	if err := pool.QueryRow(ctx, `
		SELECT count(*), min(tenant_id::text) FROM admin_audit_log WHERE entity_type IN ('JOB', 'OUTBOX_EVENT')`).Scan(&audits, &auditTenant); err != nil {
		t.Fatal(err)
	}
	if audits != 3 || auditTenant != pair.A.TenantID {
		t.Fatalf("аудит команд: count=%d tenant=%s", audits, auditTenant)
	}
	jobs, err := store.Jobs(ctx, application.JobFilter{TenantID: pair.A.TenantID, Status: "PENDING", Limit: 10})
	if err != nil || len(jobs) != 1 || jobs[0].ID != deadJob {
		t.Fatalf("фильтр заданий: %#v, %v", jobs, err)
	}
	organizations, err := store.Organizations(ctx, now)
	if err != nil || len(organizations) < 2 {
		t.Fatalf("организации: %#v, %v", organizations, err)
	}
	for _, organization := range organizations {
		if organization.ID == pair.A.TenantID && (organization.Members != 1 || organization.Locations != 1 || organization.Timezone != "Europe/Moscow") {
			t.Fatalf("организация A: %#v", organization)
		}
	}
	usage, err := store.Usage(ctx, now.Add(-24*time.Hour), now.Add(time.Hour))
	if err != nil || len(usage) < 2 {
		t.Fatalf("потребление: %#v, %v", usage, err)
	}
	for _, tenant := range usage {
		if tenant.TenantID == pair.A.TenantID && tenant.Jobs != 2 {
			t.Fatalf("потребление A: %#v", tenant)
		}
	}
	if nodes, err := store.AINodes(ctx); err != nil || len(nodes) != 0 {
		t.Fatalf("AI-узлы: %#v, %v", nodes, err)
	}
	if _, found, err := store.Trace(ctx, pair.A.TenantID, newID(t)); err != nil || found {
		t.Fatalf("трасса несуществующего сообщения: found=%v, %v", found, err)
	}
}
