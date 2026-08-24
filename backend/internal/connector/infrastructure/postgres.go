// Package infrastructure provides PostgreSQL persistence and provider adapters for Connector Core.
package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/connector/domain"
)

type PostgresRepository struct{ pool *pgxpool.Pool }

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (repository *PostgresRepository) ListConnections(ctx context.Context, tenantID string) ([]domain.ChannelConnection, error) {
	if repository == nil || repository.pool == nil || tenantID == "" {
		return nil, domain.ErrInvalid
	}
	rows, err := repository.pool.Query(ctx, `
		SELECT id, tenant_id, location_id, provider, name, status, capabilities,
		       verification_secret_hash, last_event_at, last_success_at, last_error_at,
		       last_error_code, created_at, updated_at
		FROM channel_connections
		WHERE tenant_id = $1
		ORDER BY created_at, id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list channel connections: %w", err)
	}
	defer rows.Close()
	connections := make([]domain.ChannelConnection, 0)
	for rows.Next() {
		connection, scanErr := scanConnectionValues(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		connections = append(connections, connection)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate channel connections: %w", err)
	}
	return connections, nil
}

func (repository *PostgresRepository) Connection(ctx context.Context, tenantID, connectionID string) (domain.ChannelConnection, bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || connectionID == "" {
		return domain.ChannelConnection{}, false, domain.ErrInvalid
	}
	return scanConnection(repository.pool.QueryRow(ctx, `
		SELECT id, tenant_id, location_id, provider, name, status, capabilities,
		       verification_secret_hash, last_event_at, last_success_at, last_error_at,
		       last_error_code, created_at, updated_at
		FROM channel_connections
		WHERE tenant_id = $1 AND id = $2`, tenantID, connectionID))
}

func (repository *PostgresRepository) CreateConnection(ctx context.Context, tenantID string, connection domain.ChannelConnection) error {
	if repository == nil || repository.pool == nil || tenantID == "" || tenantID != connection.TenantID || connection.Validate() != nil {
		return domain.ErrInvalid
	}
	capabilities, err := json.Marshal(connection.Capabilities)
	if err != nil {
		return domain.ErrInvalid
	}
	_, err = repository.pool.Exec(ctx, `
		INSERT INTO channel_connections(
			id, tenant_id, location_id, provider, name, status, capabilities,
			verification_secret_hash, last_event_at, last_success_at, last_error_at,
			last_error_code, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11, $12, $13, $14)`,
		connection.ID, tenantID, connection.LocationID, connection.Provider, connection.Name,
		connection.Status, string(capabilities), connection.VerificationSecretHash,
		connection.LastEventAt, connection.LastSuccessAt, connection.LastErrorAt,
		connection.LastErrorCode, connection.CreatedAt, connection.UpdatedAt,
	)
	if err != nil {
		return mapPostgresError("insert channel connection", err)
	}
	return nil
}

func (repository *PostgresRepository) DisconnectConnection(
	ctx context.Context,
	tenantID, connectionID string,
	at time.Time,
) (domain.ChannelConnection, bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || connectionID == "" || at.IsZero() {
		return domain.ChannelConnection{}, false, domain.ErrInvalid
	}
	return scanConnection(repository.pool.QueryRow(ctx, `
		UPDATE channel_connections
		SET status = 'DISCONNECTED', updated_at = $3
		WHERE tenant_id = $1 AND id = $2
		RETURNING id, tenant_id, location_id, provider, name, status, capabilities,
		          verification_secret_hash, last_event_at, last_success_at, last_error_at,
		          last_error_code, created_at, updated_at`, tenantID, connectionID, at.UTC()))
}

func (repository *PostgresRepository) PersistEvent(
	ctx context.Context,
	tenantID, connectionID string,
	event domain.RawEvent,
	work *domain.NormalizationWork,
	providerHealth domain.ConnectionHealth,
) (domain.PersistResult, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || connectionID == "" ||
		event.TenantID != tenantID || event.ConnectionID != connectionID || event.Validate() != nil || providerHealth.Validate() != nil {
		return domain.PersistResult{}, domain.ErrInvalid
	}
	if event.Status == domain.RawEventReceived {
		if work == nil || work.Validate(event) != nil {
			return domain.PersistResult{}, domain.ErrInvalid
		}
	} else if work != nil {
		return domain.PersistResult{}, domain.ErrInvalid
	}

	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return domain.PersistResult{}, fmt.Errorf("begin raw event receipt: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	connection, found, err := scanConnection(tx.QueryRow(ctx, `
		SELECT id, tenant_id, location_id, provider, name, status, capabilities,
		       verification_secret_hash, last_event_at, last_success_at, last_error_at,
		       last_error_code, created_at, updated_at
		FROM channel_connections
		WHERE tenant_id = $1 AND id = $2
		FOR UPDATE`, tenantID, connectionID))
	if err != nil {
		return domain.PersistResult{}, err
	}
	if !found {
		return domain.PersistResult{}, domain.ErrNotFound
	}
	if connection.Status == domain.ConnectionDisconnected {
		return domain.PersistResult{}, domain.ErrDisconnected
	}
	if connection.Provider != event.Provider {
		return domain.PersistResult{}, domain.ErrInvalid
	}

	persisted, inserted, err := insertRawEvent(ctx, tx, event)
	if err != nil {
		return domain.PersistResult{}, err
	}
	if !inserted {
		persisted, found, err = rawEvent(ctx, tx, tenantID, connectionID, event.ExternalEventID)
		if err != nil {
			return domain.PersistResult{}, err
		}
		if !found {
			return domain.PersistResult{}, fmt.Errorf("read duplicate raw event: %w", domain.ErrNotFound)
		}
		if persisted.PayloadHash != event.PayloadHash {
			return domain.PersistResult{}, domain.ErrConflict
		}
	}

	if inserted && work != nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO raw_event_normalization_work(
				id, tenant_id, connection_id, raw_event_id, status, created_at
			) VALUES ($1, $2, $3, $4, $5, $6)`,
			work.ID, tenantID, connectionID, event.ID, work.Status, work.CreatedAt,
		); err != nil {
			return domain.PersistResult{}, mapPostgresError("insert raw event normalization work", err)
		}
	}

	if event.Status == domain.RawEventFailed {
		if err := connection.RecordFailure(valueOrEmpty(event.ErrorCode), event.ReceivedAt); err != nil {
			return domain.PersistResult{}, err
		}
	} else if err := connection.RecordSuccess(event.ReceivedAt, providerHealth); err != nil {
		return domain.PersistResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE channel_connections
		SET status = $3, last_event_at = $4, last_success_at = $5,
		    last_error_at = $6, last_error_code = $7, updated_at = $8
		WHERE tenant_id = $1 AND id = $2`,
		tenantID, connectionID, connection.Status, connection.LastEventAt,
		connection.LastSuccessAt, connection.LastErrorAt, connection.LastErrorCode, connection.UpdatedAt,
	); err != nil {
		return domain.PersistResult{}, mapPostgresError("update channel connection health", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.PersistResult{}, fmt.Errorf("commit raw event receipt: %w", err)
	}
	return domain.PersistResult{Event: persisted, Inserted: inserted}, nil
}

type rowScanner interface{ Scan(...any) error }

func scanConnection(row rowScanner) (domain.ChannelConnection, bool, error) {
	connection, err := scanConnectionValues(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ChannelConnection{}, false, nil
	}
	if err != nil {
		return domain.ChannelConnection{}, false, err
	}
	return connection, true, nil
}

func scanConnectionValues(row rowScanner) (domain.ChannelConnection, error) {
	var connection domain.ChannelConnection
	var capabilitiesJSON []byte
	if err := row.Scan(
		&connection.ID, &connection.TenantID, &connection.LocationID, &connection.Provider,
		&connection.Name, &connection.Status, &capabilitiesJSON, &connection.VerificationSecretHash,
		&connection.LastEventAt, &connection.LastSuccessAt, &connection.LastErrorAt,
		&connection.LastErrorCode, &connection.CreatedAt, &connection.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ChannelConnection{}, err
		}
		return domain.ChannelConnection{}, fmt.Errorf("scan channel connection: %w", err)
	}
	if err := json.Unmarshal(capabilitiesJSON, &connection.Capabilities); err != nil || connection.Validate() != nil {
		return domain.ChannelConnection{}, fmt.Errorf("scan channel connection: %w", domain.ErrInvalid)
	}
	return connection, nil
}

func insertRawEvent(ctx context.Context, tx pgx.Tx, event domain.RawEvent) (domain.RawEvent, bool, error) {
	persisted, inserted, err := scanRawEvent(tx.QueryRow(ctx, `
		INSERT INTO raw_events(
			id, tenant_id, connection_id, provider, external_event_id, payload,
			payload_hash, status, error_code, received_at, processed_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (connection_id, external_event_id) DO NOTHING
		RETURNING id, tenant_id, connection_id, provider, external_event_id, payload,
		          payload_hash, status, error_code, received_at, processed_at, created_at`,
		event.ID, event.TenantID, event.ConnectionID, event.Provider, event.ExternalEventID,
		string(event.Payload), event.PayloadHash, event.Status, event.ErrorCode,
		event.ReceivedAt, event.ProcessedAt, event.CreatedAt,
	))
	if err != nil {
		return domain.RawEvent{}, false, mapPostgresError("insert raw event", err)
	}
	return persisted, inserted, nil
}

func rawEvent(
	ctx context.Context,
	queryer interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	},
	tenantID, connectionID, externalEventID string,
) (domain.RawEvent, bool, error) {
	return scanRawEvent(queryer.QueryRow(ctx, `
		SELECT id, tenant_id, connection_id, provider, external_event_id, payload,
		       payload_hash, status, error_code, received_at, processed_at, created_at
		FROM raw_events
		WHERE tenant_id = $1 AND connection_id = $2 AND external_event_id = $3`,
		tenantID, connectionID, externalEventID))
}

func scanRawEvent(row rowScanner) (domain.RawEvent, bool, error) {
	var event domain.RawEvent
	if err := row.Scan(
		&event.ID, &event.TenantID, &event.ConnectionID, &event.Provider, &event.ExternalEventID,
		&event.Payload, &event.PayloadHash, &event.Status, &event.ErrorCode,
		&event.ReceivedAt, &event.ProcessedAt, &event.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.RawEvent{}, false, nil
		}
		return domain.RawEvent{}, false, fmt.Errorf("scan raw event: %w", err)
	}
	if event.Validate() != nil {
		return domain.RawEvent{}, false, fmt.Errorf("scan raw event: %w", domain.ErrInvalid)
	}
	return event, true, nil
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func mapPostgresError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "23505":
			return domain.ErrConflict
		case "23503":
			return domain.ErrNotFound
		case "23514", "22P02", "22001", "22003":
			return domain.ErrInvalid
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}
