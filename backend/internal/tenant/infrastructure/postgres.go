// Package infrastructure provides PostgreSQL persistence for tenant setup.
package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/tenant/domain"
)

type PostgresRepository struct{ pool *pgxpool.Pool }

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (r *PostgresRepository) CreateOrganizationWithOwner(ctx context.Context, organization domain.Organization, membership domain.Membership) error {
	if r == nil || r.pool == nil || organization.ID != membership.TenantID || membership.Role != domain.RoleOwner {
		return domain.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin organization creation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO organizations(id, name, default_timezone, default_currency, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		organization.ID, organization.Name, organization.DefaultTimezone, organization.DefaultCurrency,
		organization.Status, organization.CreatedAt, organization.UpdatedAt,
	); err != nil {
		return mapPostgresError("insert organization", err)
	}
	if err := insertMembership(ctx, tx, membership); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit organization creation: %w", err)
	}
	return nil
}

func (r *PostgresRepository) Organization(ctx context.Context, tenantID string) (domain.Organization, bool, error) {
	if r == nil || r.pool == nil {
		return domain.Organization{}, false, domain.ErrInvalid
	}
	return scanOrganization(r.pool.QueryRow(ctx, `
		SELECT id, name, default_timezone, default_currency, status, created_at, updated_at
		FROM organizations WHERE id = $1`, tenantID))
}

func (r *PostgresRepository) UpdateOrganization(ctx context.Context, tenantID string, organization domain.Organization) (domain.Organization, bool, error) {
	if r == nil || r.pool == nil || tenantID != organization.ID {
		return domain.Organization{}, false, domain.ErrInvalid
	}
	return scanOrganization(r.pool.QueryRow(ctx, `
		UPDATE organizations
		SET name = $2, default_timezone = $3, default_currency = $4, updated_at = $5
		WHERE id = $1
		RETURNING id, name, default_timezone, default_currency, status, created_at, updated_at`,
		tenantID, organization.Name, organization.DefaultTimezone, organization.DefaultCurrency, organization.UpdatedAt,
	))
}

func (r *PostgresRepository) Membership(ctx context.Context, tenantID, userID string) (domain.Membership, bool, error) {
	if r == nil || r.pool == nil {
		return domain.Membership{}, false, domain.ErrInvalid
	}
	return scanMembership(r.pool.QueryRow(ctx, `
		SELECT m.id, m.tenant_id, m.user_id, m.role, m.status, m.created_at, m.updated_at
		FROM memberships m
		JOIN organizations o ON o.id = m.tenant_id
		WHERE m.tenant_id = $1 AND m.user_id = $2 AND o.status = 'ACTIVE'`, tenantID, userID))
}

func (r *PostgresRepository) MembershipsForUser(ctx context.Context, userID string) ([]domain.AccountMembership, error) {
	if r == nil || r.pool == nil {
		return nil, domain.ErrInvalid
	}
	rows, err := r.pool.Query(ctx, `
		SELECT m.id, m.tenant_id, m.user_id, m.role, m.status, m.created_at, m.updated_at,
		       o.id, o.name, o.default_timezone, o.default_currency, o.status, o.created_at, o.updated_at
		FROM memberships m
		JOIN organizations o ON o.id = m.tenant_id
		WHERE m.user_id = $1
		ORDER BY o.created_at, o.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list user memberships: %w", err)
	}
	defer rows.Close()
	result := make([]domain.AccountMembership, 0)
	for rows.Next() {
		var item domain.AccountMembership
		if err := rows.Scan(
			&item.Membership.ID, &item.Membership.TenantID, &item.Membership.UserID, &item.Membership.Role,
			&item.Membership.Status, &item.Membership.CreatedAt, &item.Membership.UpdatedAt,
			&item.Organization.ID, &item.Organization.Name, &item.Organization.DefaultTimezone,
			&item.Organization.DefaultCurrency, &item.Organization.Status, &item.Organization.CreatedAt, &item.Organization.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan user membership: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user memberships: %w", err)
	}
	return result, nil
}

func (r *PostgresRepository) CreateMembership(ctx context.Context, tenantID string, membership domain.Membership) error {
	if r == nil || r.pool == nil || tenantID != membership.TenantID {
		return domain.ErrInvalid
	}
	return insertMembership(ctx, r.pool, membership)
}

func (r *PostgresRepository) ListLocations(ctx context.Context, tenantID string) ([]domain.Location, error) {
	if r == nil || r.pool == nil {
		return nil, domain.ErrInvalid
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, name, timezone, response_threshold_minutes, active, created_at, updated_at
		FROM locations
		WHERE tenant_id = $1
		ORDER BY created_at, id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list locations: %w", err)
	}
	locations := make([]domain.Location, 0)
	for rows.Next() {
		location, scanErr := scanLocationValues(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		locations = append(locations, location)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate locations: %w", err)
	}
	rows.Close()
	for index := range locations {
		hours, err := r.businessHours(ctx, tenantID, locations[index].ID)
		if err != nil {
			return nil, err
		}
		locations[index].BusinessHours = hours
	}
	return locations, nil
}

func (r *PostgresRepository) Location(ctx context.Context, tenantID, locationID string) (domain.Location, bool, error) {
	if r == nil || r.pool == nil {
		return domain.Location{}, false, domain.ErrInvalid
	}
	location, found, err := scanLocation(r.pool.QueryRow(ctx, `
		SELECT id, tenant_id, name, timezone, response_threshold_minutes, active, created_at, updated_at
		FROM locations
		WHERE tenant_id = $1 AND id = $2`, tenantID, locationID))
	if err != nil || !found {
		return location, found, err
	}
	location.BusinessHours, err = r.businessHours(ctx, tenantID, locationID)
	return location, true, err
}

func (r *PostgresRepository) CreateLocation(ctx context.Context, tenantID string, location domain.Location) error {
	if r == nil || r.pool == nil || tenantID != location.TenantID {
		return domain.ErrInvalid
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO locations(id, tenant_id, name, timezone, response_threshold_minutes, active, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		location.ID, tenantID, location.Name, location.Timezone, location.ResponseThresholdMinutes,
		location.Active, location.CreatedAt, location.UpdatedAt,
	)
	if err != nil {
		return mapPostgresError("insert location", err)
	}
	return nil
}

func (r *PostgresRepository) UpdateLocation(ctx context.Context, tenantID, locationID string, location domain.Location) (domain.Location, bool, error) {
	if r == nil || r.pool == nil || tenantID != location.TenantID || locationID != location.ID {
		return domain.Location{}, false, domain.ErrInvalid
	}
	updated, found, err := scanLocation(r.pool.QueryRow(ctx, `
		UPDATE locations
		SET name = $3, timezone = $4, response_threshold_minutes = $5, active = $6, updated_at = $7
		WHERE tenant_id = $1 AND id = $2
		RETURNING id, tenant_id, name, timezone, response_threshold_minutes, active, created_at, updated_at`,
		tenantID, locationID, location.Name, location.Timezone, location.ResponseThresholdMinutes, location.Active, location.UpdatedAt,
	))
	if err != nil || !found {
		return updated, found, err
	}
	updated.BusinessHours, err = r.businessHours(ctx, tenantID, locationID)
	return updated, true, err
}

func (r *PostgresRepository) ReplaceBusinessHours(ctx context.Context, tenantID, locationID, timezone string, hours []domain.BusinessHour, at time.Time) (domain.Location, bool, error) {
	if r == nil || r.pool == nil || len(hours) != 7 {
		return domain.Location{}, false, domain.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.Location{}, false, fmt.Errorf("begin business hours replacement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	location, found, err := scanLocation(tx.QueryRow(ctx, `
		UPDATE locations SET timezone = $3, updated_at = $4
		WHERE tenant_id = $1 AND id = $2
		RETURNING id, tenant_id, name, timezone, response_threshold_minutes, active, created_at, updated_at`,
		tenantID, locationID, timezone, at,
	))
	if err != nil || !found {
		return domain.Location{}, found, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM location_business_hours WHERE tenant_id = $1 AND location_id = $2`, tenantID, locationID); err != nil {
		return domain.Location{}, false, fmt.Errorf("delete business hours: %w", err)
	}
	for _, hour := range hours {
		if hour.TenantID != tenantID || hour.LocationID != locationID {
			return domain.Location{}, false, domain.ErrInvalid
		}
		var opensAt any
		var closesAt any
		if !hour.Closed {
			opensAt = hour.OpensAt
			closesAt = hour.ClosesAt
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO location_business_hours(
				id, tenant_id, location_id, weekday, is_closed, opens_at, closes_at, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6::time, $7::time, $8, $8)`,
			hour.ID, tenantID, locationID, hour.Weekday, hour.Closed, opensAt, closesAt, at,
		); err != nil {
			return domain.Location{}, false, mapPostgresError("insert business hour", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Location{}, false, fmt.Errorf("commit business hours replacement: %w", err)
	}
	location.BusinessHours = append([]domain.BusinessHour(nil), hours...)
	sort.Slice(location.BusinessHours, func(i, j int) bool { return location.BusinessHours[i].Weekday < location.BusinessHours[j].Weekday })
	return location, true, nil
}

func (r *PostgresRepository) businessHours(ctx context.Context, tenantID, locationID string) ([]domain.BusinessHour, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, location_id, weekday, is_closed,
		       COALESCE(to_char(opens_at, 'HH24:MI'), ''), COALESCE(to_char(closes_at, 'HH24:MI'), '')
		FROM location_business_hours
		WHERE tenant_id = $1 AND location_id = $2
		ORDER BY weekday`, tenantID, locationID)
	if err != nil {
		return nil, fmt.Errorf("list business hours: %w", err)
	}
	defer rows.Close()
	result := make([]domain.BusinessHour, 0, 7)
	for rows.Next() {
		var hour domain.BusinessHour
		if err := rows.Scan(&hour.ID, &hour.TenantID, &hour.LocationID, &hour.Weekday, &hour.Closed, &hour.OpensAt, &hour.ClosesAt); err != nil {
			return nil, fmt.Errorf("scan business hour: %w", err)
		}
		result = append(result, hour)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate business hours: %w", err)
	}
	return result, nil
}

type rowScanner interface{ Scan(...any) error }

func scanOrganization(row rowScanner) (domain.Organization, bool, error) {
	var organization domain.Organization
	err := row.Scan(&organization.ID, &organization.Name, &organization.DefaultTimezone, &organization.DefaultCurrency, &organization.Status, &organization.CreatedAt, &organization.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Organization{}, false, nil
	}
	if err != nil {
		return domain.Organization{}, false, fmt.Errorf("scan organization: %w", err)
	}
	return organization, true, nil
}

func scanMembership(row rowScanner) (domain.Membership, bool, error) {
	var membership domain.Membership
	err := row.Scan(&membership.ID, &membership.TenantID, &membership.UserID, &membership.Role, &membership.Status, &membership.CreatedAt, &membership.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Membership{}, false, nil
	}
	if err != nil {
		return domain.Membership{}, false, fmt.Errorf("scan membership: %w", err)
	}
	return membership, true, nil
}

func scanLocation(row rowScanner) (domain.Location, bool, error) {
	location, err := scanLocationValues(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Location{}, false, nil
	}
	if err != nil {
		return domain.Location{}, false, err
	}
	return location, true, nil
}

func scanLocationValues(row rowScanner) (domain.Location, error) {
	var location domain.Location
	if err := row.Scan(&location.ID, &location.TenantID, &location.Name, &location.Timezone, &location.ResponseThresholdMinutes, &location.Active, &location.CreatedAt, &location.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Location{}, err
		}
		return domain.Location{}, fmt.Errorf("scan location: %w", err)
	}
	location.BusinessHours = []domain.BusinessHour{}
	return location, nil
}

type membershipExecer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func insertMembership(ctx context.Context, executor membershipExecer, membership domain.Membership) error {
	_, err := executor.Exec(ctx, `
		INSERT INTO memberships(id, tenant_id, user_id, role, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		membership.ID, membership.TenantID, membership.UserID, membership.Role, membership.Status, membership.CreatedAt, membership.UpdatedAt,
	)
	if err != nil {
		return mapPostgresError("insert membership", err)
	}
	return nil
}

func mapPostgresError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "23505":
			return domain.ErrConflict
		case "23503":
			return domain.ErrNotFound
		case "23514", "22P02":
			return domain.ErrInvalid
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}
