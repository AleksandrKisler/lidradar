// Package infrastructure provides PostgreSQL persistence for the service catalog.
package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/catalog/domain"
)

type PostgresRepository struct{ pool *pgxpool.Pool }

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (repository *PostgresRepository) List(ctx context.Context, tenantID string) ([]domain.ServiceCatalogItem, error) {
	if repository == nil || repository.pool == nil || tenantID == "" {
		return nil, domain.ErrInvalid
	}
	rows, err := repository.pool.Query(ctx, `
		SELECT id, tenant_id, location_id, name, normalized_name,
		       COALESCE(price_from::text, ''), COALESCE(price_to::text, ''),
		       currency, active, created_at, updated_at
		FROM service_catalog_items
		WHERE tenant_id = $1
		ORDER BY active DESC, normalized_name, created_at, id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list service catalog items: %w", err)
	}
	defer rows.Close()
	items := make([]domain.ServiceCatalogItem, 0)
	for rows.Next() {
		item, scanErr := scanItemValues(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate service catalog items: %w", err)
	}
	return items, nil
}

func (repository *PostgresRepository) Item(ctx context.Context, tenantID, itemID string) (domain.ServiceCatalogItem, bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || itemID == "" {
		return domain.ServiceCatalogItem{}, false, domain.ErrInvalid
	}
	return scanItem(repository.pool.QueryRow(ctx, `
		SELECT id, tenant_id, location_id, name, normalized_name,
		       COALESCE(price_from::text, ''), COALESCE(price_to::text, ''),
		       currency, active, created_at, updated_at
		FROM service_catalog_items
		WHERE tenant_id = $1 AND id = $2`, tenantID, itemID))
}

func (repository *PostgresRepository) Create(ctx context.Context, tenantID string, item domain.ServiceCatalogItem) error {
	if repository == nil || repository.pool == nil || tenantID == "" || tenantID != item.TenantID || item.Validate() != nil {
		return domain.ErrInvalid
	}
	_, err := repository.pool.Exec(ctx, `
		INSERT INTO service_catalog_items(
			id, tenant_id, location_id, name, normalized_name, price_from, price_to,
			currency, active, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6::numeric, $7::numeric, $8, $9, $10, $11)`,
		item.ID, tenantID, item.LocationID, item.Name, item.NormalizedName,
		databasePrice(item.PriceFrom), databasePrice(item.PriceTo), item.Currency, item.Active, item.CreatedAt, item.UpdatedAt,
	)
	if err != nil {
		return mapPostgresError("insert service catalog item", err)
	}
	return nil
}

func (repository *PostgresRepository) Update(
	ctx context.Context,
	tenantID, itemID string,
	item domain.ServiceCatalogItem,
) (domain.ServiceCatalogItem, bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || tenantID != item.TenantID ||
		itemID == "" || itemID != item.ID || item.Validate() != nil {
		return domain.ServiceCatalogItem{}, false, domain.ErrInvalid
	}
	return scanItem(repository.pool.QueryRow(ctx, `
		UPDATE service_catalog_items
		SET location_id = $3, name = $4, normalized_name = $5,
		    price_from = $6::numeric, price_to = $7::numeric,
		    currency = $8, active = $9, updated_at = $10
		WHERE tenant_id = $1 AND id = $2
		RETURNING id, tenant_id, location_id, name, normalized_name,
		          COALESCE(price_from::text, ''), COALESCE(price_to::text, ''),
		          currency, active, created_at, updated_at`,
		tenantID, itemID, item.LocationID, item.Name, item.NormalizedName,
		databasePrice(item.PriceFrom), databasePrice(item.PriceTo), item.Currency, item.Active, item.UpdatedAt,
	))
}

func (repository *PostgresRepository) Deactivate(ctx context.Context, tenantID, itemID string, at time.Time) (bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || itemID == "" || at.IsZero() {
		return false, domain.ErrInvalid
	}
	result, err := repository.pool.Exec(ctx, `
		UPDATE service_catalog_items
		SET active = FALSE, updated_at = $3
		WHERE tenant_id = $1 AND id = $2 AND active = TRUE`, tenantID, itemID, at.UTC())
	if err != nil {
		return false, mapPostgresError("deactivate service catalog item", err)
	}
	if result.RowsAffected() == 1 {
		return true, nil
	}
	var exists bool
	if err := repository.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM service_catalog_items WHERE tenant_id = $1 AND id = $2)`, tenantID, itemID,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("check service catalog item: %w", err)
	}
	return exists, nil
}

type rowScanner interface{ Scan(...any) error }

func scanItem(row rowScanner) (domain.ServiceCatalogItem, bool, error) {
	item, err := scanItemValues(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ServiceCatalogItem{}, false, nil
	}
	if err != nil {
		return domain.ServiceCatalogItem{}, false, err
	}
	return item, true, nil
}

func scanItemValues(row rowScanner) (domain.ServiceCatalogItem, error) {
	var item domain.ServiceCatalogItem
	var priceFrom, priceTo string
	if err := row.Scan(
		&item.ID, &item.TenantID, &item.LocationID, &item.Name, &item.NormalizedName,
		&priceFrom, &priceTo, &item.Currency, &item.Active, &item.CreatedAt, &item.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ServiceCatalogItem{}, err
		}
		return domain.ServiceCatalogItem{}, fmt.Errorf("scan service catalog item: %w", err)
	}
	var err error
	item.PriceFrom, err = parsedDatabasePrice(priceFrom)
	if err != nil {
		return domain.ServiceCatalogItem{}, fmt.Errorf("scan service catalog price_from: %w", err)
	}
	item.PriceTo, err = parsedDatabasePrice(priceTo)
	if err != nil {
		return domain.ServiceCatalogItem{}, fmt.Errorf("scan service catalog price_to: %w", err)
	}
	if item.Validate() != nil {
		return domain.ServiceCatalogItem{}, fmt.Errorf("scan service catalog item: %w", domain.ErrInvalid)
	}
	return item, nil
}

func databasePrice(price *domain.Price) any {
	if price == nil {
		return nil
	}
	return price.String()
}

func parsedDatabasePrice(raw string) (*domain.Price, error) {
	if raw == "" {
		return nil, nil
	}
	price, err := domain.ParsePrice(raw)
	if err != nil {
		return nil, err
	}
	return &price, nil
}

func mapPostgresError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "23505":
			return domain.ErrConflict
		case "23503":
			return domain.ErrNotFound
		case "23514", "22P02", "22003":
			return domain.ErrInvalid
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}
