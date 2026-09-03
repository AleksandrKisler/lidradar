package infrastructure

import (
	"context"
	"testing"
	"time"

	"lidradar/backend/internal/tenant/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

// ML-согласие явное, активное и отзываемое (ТЗ §70): одно действующее на
// организацию, отзыв сохраняет историю, повторная выдача создаёт новую строку,
// а прямое удаление или повторное изменение отвергается схемой.
func TestPostgresMLConsentGrantRevokeHistory(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	repository := NewPostgresRepository(pool)
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	newID := func() string {
		value, err := (ids.Generator{}).NewID()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	audit := func(operation string) domain.AuditEntry {
		return domain.AuditEntry{ID: newID(), ActorID: pair.A.UserID, Operation: operation, EntityType: "ML_CONSENT", At: now}
	}
	if _, active, err := repository.ActiveMLConsent(ctx, pair.A.TenantID); err != nil || active {
		t.Fatalf("согласие до выдачи: active=%v err=%v", active, err)
	}
	consent, err := domain.NewMLConsent(newID(), pair.A.TenantID, pair.A.UserID, now)
	if err != nil {
		t.Fatal(err)
	}
	granted, created, err := repository.GrantMLConsent(ctx, consent, audit("ML_CONSENT_GRANTED"))
	if err != nil || !created || granted.ID != consent.ID || !granted.Active() {
		t.Fatalf("выдача согласия: %#v, created=%v, %v", granted, created, err)
	}
	replay, _ := domain.NewMLConsent(newID(), pair.A.TenantID, pair.A.UserID, now.Add(time.Second))
	if again, created, err := repository.GrantMLConsent(ctx, replay, audit("ML_CONSENT_GRANTED")); err != nil || created || again.ID != consent.ID {
		t.Fatalf("повторная выдача создала второе согласие: %#v, created=%v, %v", again, created, err)
	}
	if _, active, err := repository.ActiveMLConsent(ctx, pair.B.TenantID); err != nil || active {
		t.Fatalf("согласие утекло в другую организацию: active=%v err=%v", active, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM ml_consents WHERE id = $1`, consent.ID); err == nil {
		t.Fatal("согласие удалено физически")
	}
	revoked, found, err := repository.RevokeMLConsent(ctx, pair.A.TenantID, pair.A.UserID, now.Add(time.Minute), audit("ML_CONSENT_REVOKED"))
	if err != nil || !found || revoked.Active() || revoked.RevokedBy == nil || *revoked.RevokedBy != pair.A.UserID {
		t.Fatalf("отзыв согласия: %#v, found=%v, %v", revoked, found, err)
	}
	if _, found, err := repository.RevokeMLConsent(ctx, pair.A.TenantID, pair.A.UserID, now.Add(2*time.Minute), audit("ML_CONSENT_REVOKED")); err != nil || found {
		t.Fatalf("повторный отзыв нашёл согласие: found=%v err=%v", found, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ml_consents SET revoked_at = NULL, revoked_by = NULL WHERE id = $1`, consent.ID); err == nil {
		t.Fatal("отозванное согласие восстановлено напрямую")
	}
	second, _ := domain.NewMLConsent(newID(), pair.A.TenantID, pair.A.UserID, now.Add(time.Hour))
	if regranted, created, err := repository.GrantMLConsent(ctx, second, audit("ML_CONSENT_GRANTED")); err != nil || !created || regranted.ID != second.ID {
		t.Fatalf("повторная выдача после отзыва: %#v, created=%v, %v", regranted, created, err)
	}
	var rows, activeRows, audits int
	if err := pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE revoked_at IS NULL),
		       (SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND entity_type = 'ML_CONSENT')
		FROM ml_consents WHERE tenant_id = $1`, pair.A.TenantID).Scan(&rows, &activeRows, &audits); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || activeRows != 1 || audits != 3 {
		t.Fatalf("история согласий: строк=%d активных=%d аудит=%d", rows, activeRows, audits)
	}
}
