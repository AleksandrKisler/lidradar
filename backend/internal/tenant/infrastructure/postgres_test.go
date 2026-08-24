package infrastructure

import (
	"context"
	"testing"

	"lidradar/backend/internal/tenant/application"
	"lidradar/backend/internal/testsupport"
)

func TestPostgresRepositoryEnforcesTenantScopedLocations(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	repository := NewPostgresRepository(pool)

	if _, found, err := repository.Location(ctx, pair.A.TenantID, pair.B.LocationID); err != nil || found {
		t.Fatalf("cross-tenant Location() = found %v, error %v", found, err)
	}
	location, found, err := repository.Location(ctx, pair.B.TenantID, pair.B.LocationID)
	if err != nil || !found || location.TenantID != pair.B.TenantID {
		t.Fatalf("own-tenant Location() = %#v, %v, %v", location, found, err)
	}

	permissions := application.NewPermissionService(repository)
	allowed, err := permissions.Allowed(ctx, pair.B.UserID, pair.A.TenantID, application.PermissionLocationManage)
	if err != nil || allowed {
		t.Fatalf("cross-tenant Allowed() = %v, %v", allowed, err)
	}
}
