package infrastructure

import (
	"context"
	"errors"
	"testing"
	"time"

	"lidradar/backend/internal/catalog/domain"
	"lidradar/backend/internal/testsupport"
	"lidradar/backend/platform/ids"
)

func TestPostgresRepositoryEnforcesTenantAndLocationScope(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	repository := NewPostgresRepository(pool)
	generator := ids.Generator{}

	foreignLocationItem := newItem(t, generator, pair.A.TenantID, &pair.B.LocationID)
	if err := repository.Create(ctx, pair.A.TenantID, foreignLocationItem); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant location Create() error = %v", err)
	}

	item := newItem(t, generator, pair.A.TenantID, &pair.A.LocationID)
	if err := repository.Create(ctx, pair.A.TenantID, item); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, found, err := repository.Item(ctx, pair.B.TenantID, item.ID); err != nil || found {
		t.Fatalf("cross-tenant Item() = found %v, error %v", found, err)
	}
	if found, err := repository.Deactivate(ctx, pair.B.TenantID, item.ID, time.Now()); err != nil || found {
		t.Fatalf("cross-tenant Deactivate() = found %v, error %v", found, err)
	}
	stored, found, err := repository.Item(ctx, pair.A.TenantID, item.ID)
	if err != nil || !found || !stored.Active {
		t.Fatalf("own-tenant Item() = %#v, found %v, error %v", stored, found, err)
	}
}

func TestMigrationRejectsInvalidPriceValues(t *testing.T) {
	pool := testsupport.Postgres(t)
	ctx := context.Background()
	pair := testsupport.TwoTenants(t, ctx, pool)
	generator := ids.Generator{}

	for _, prices := range [][2]string{{"-0.01", "1.00"}, {"2.00", "1.99"}} {
		id, err := generator.NewID()
		if err != nil {
			t.Fatal(err)
		}
		_, err = pool.Exec(ctx, `
			INSERT INTO service_catalog_items(
				id, tenant_id, name, normalized_name, price_from, price_to, currency
			) VALUES ($1, $2, 'Invalid', 'invalid', $3::numeric, $4::numeric, 'RUB')`,
			id, pair.A.TenantID, prices[0], prices[1],
		)
		if err == nil {
			t.Fatalf("database accepted invalid prices %q to %q", prices[0], prices[1])
		}
	}
}

func newItem(t *testing.T, generator ids.Generator, tenantID string, locationID *string) domain.ServiceCatalogItem {
	t.Helper()
	id, err := generator.NewID()
	if err != nil {
		t.Fatal(err)
	}
	from, err := domain.ParsePrice("1000.00")
	if err != nil {
		t.Fatal(err)
	}
	item, err := domain.NewServiceCatalogItem(id, tenantID, "Service", locationID, &from, nil, "RUB", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return item
}
