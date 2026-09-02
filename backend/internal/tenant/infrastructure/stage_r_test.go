package infrastructure

import (
	"context"
	"testing"
	"time"

	"lidradar/backend/internal/tenant/application"
	"lidradar/backend/internal/tenant/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

// LR-BE-RM-009: отзыв доступа не удаляет членство физически, потому что на
// него ссылаются неизменяемые факты. Сотрудник теряет доступ сразу, а
// подтверждённая им выручка остаётся на месте.
func TestPostgresMembershipRevocationKeepsImmutableFacts(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	repository := NewPostgresRepository(pool)
	permissions := application.NewPermissionService(repository)
	at := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	// Минимальная цепочка до подтверждённой выручки от имени сотрудника.
	newID := func() string {
		id, err := (ids.Generator{}).NewID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	connectionID, contactID, conversationID, opportunityID, eventID := newID(), newID(), newID(), newID(), newID()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO channel_connections(id, tenant_id, location_id, provider, name, status, capabilities, verification_secret_hash, created_at, updated_at)
		  VALUES ($1,$2,$3,'TEST','Канал','ACTIVE','["MESSAGES"]',repeat('0',64),$4,$4)`,
			[]any{connectionID, pair.A.TenantID, pair.A.LocationID, at}},
		{`INSERT INTO contacts(id, tenant_id, display_name, created_at, updated_at) VALUES ($1,$2,'Клиент',$3,$3)`,
			[]any{contactID, pair.A.TenantID, at}},
		{`INSERT INTO conversations(id, tenant_id, location_id, connection_id, contact_id, external_id, status, revision, created_at, updated_at)
		  VALUES ($1,$2,$3,$4,$5,$6,'ACTIVE',1,$7,$7)`,
			[]any{conversationID, pair.A.TenantID, pair.A.LocationID, connectionID, contactID, "conversation-" + conversationID, at}},
		{`INSERT INTO opportunities(id, tenant_id, conversation_id, stage, currency, opened_at, created_at, updated_at)
		  VALUES ($1,$2,$3,'NEW','RUB',$4,$4,$4)`,
			[]any{opportunityID, pair.A.TenantID, conversationID, at}},
		{`INSERT INTO revenue_events(id, tenant_id, opportunity_id, amount, currency, status, source, confirmed_by_user_id, confirmed_at, created_at)
		  VALUES ($1,$2,$3,'5000.00','RUB','CONFIRMED','USER_CONFIRMED',$4,$5,$5)`,
			[]any{eventID, pair.A.TenantID, opportunityID, pair.A.UserID, at}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	if allowed, err := permissions.Allowed(ctx, pair.A.UserID, pair.A.TenantID, application.PermissionRevenueConfirm); err != nil || !allowed {
		t.Fatalf("до отзыва Allowed() = %v, %v", allowed, err)
	}
	revoked, err := repository.RevokeMembership(ctx, pair.A.TenantID, pair.A.UserID, at.Add(time.Minute))
	if err != nil || !revoked {
		t.Fatalf("RevokeMembership() = %v, %v", revoked, err)
	}
	if again, err := repository.RevokeMembership(ctx, pair.A.TenantID, pair.A.UserID, at.Add(2*time.Minute)); err != nil || again {
		t.Fatalf("повторный отзыв = %v, %v; ожидался безопасный no-op", again, err)
	}
	if allowed, err := permissions.Allowed(ctx, pair.A.UserID, pair.A.TenantID, application.PermissionRevenueConfirm); err != nil || allowed {
		t.Fatalf("после отзыва Allowed() = %v, %v", allowed, err)
	}
	membership, found, err := repository.Membership(ctx, pair.A.TenantID, pair.A.UserID)
	if err != nil || !found || membership.Status != domain.MembershipDisabled || membership.RevokedAt == nil {
		t.Fatalf("членство после отзыва = %#v, найдено %v, ошибка %v", membership, found, err)
	}

	var events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM revenue_events WHERE tenant_id = $1 AND confirmed_by_user_id = $2`,
		pair.A.TenantID, pair.A.UserID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("выручка сотрудника затронута отзывом: событий %d", events)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, pair.A.TenantID, pair.A.UserID); err == nil {
		t.Fatal("физическое удаление членства прошло")
	}
}
