package infrastructure

import (
	"context"
	"testing"
	"time"

	"lidradar/backend/internal/audit/application"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

// Записи аудита организации и входа сохраняются append-only; актор обязан
// быть участником организации, а чужой пользователь отвергается схемой.
func TestPostgresRecorderWritesAppendOnlyAuditEntries(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	recorder := NewPostgresRecorder(pool, ids.Generator{})
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := recorder.Tenant(ctx, application.TenantEntry(pair.A.TenantID, pair.A.UserID, "ORGANIZATION_UPDATED", "ORGANIZATION", pair.A.TenantID, now)); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Tenant(ctx, application.TenantEntry(pair.A.TenantID, pair.B.UserID, "ORGANIZATION_UPDATED", "ORGANIZATION", pair.A.TenantID, now)); err == nil {
		t.Fatal("не участник записан в аудит организации")
	}
	if err := recorder.Tenant(ctx, application.TenantEntry(pair.A.TenantID, pair.A.UserID, "lower case", "ORGANIZATION", pair.A.TenantID, now)); err == nil {
		t.Fatal("некорректная операция принята")
	}
	if err := recorder.Auth(ctx, application.AuthEvent(pair.A.UserID, "USER_LOGGED_IN", "203.0.113.7", now)); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Auth(ctx, application.AuthEvent(pair.A.UserID, "USER_LOGGED_OUT", "", now.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	var tenantRows, authRows int
	var address *string
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND operation = 'ORGANIZATION_UPDATED'),
		       (SELECT count(*) FROM auth_audit_log WHERE user_id = $2),
		       (SELECT ip_address FROM auth_audit_log WHERE user_id = $2 AND operation = 'USER_LOGGED_IN')`,
		pair.A.TenantID, pair.A.UserID).Scan(&tenantRows, &authRows, &address); err != nil {
		t.Fatal(err)
	}
	if tenantRows != 1 || authRows != 2 || address == nil || *address != "203.0.113.7" {
		t.Fatalf("аудит: организация=%d вход=%d адрес=%v", tenantRows, authRows, address)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM auth_audit_log WHERE user_id = $1`, pair.A.UserID); err == nil {
		t.Fatal("аудит входа удалён вопреки append-only")
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_log SET operation = 'X' WHERE tenant_id = $1`, pair.A.TenantID); err == nil {
		t.Fatal("аудит организации изменён вопреки append-only")
	}
}
