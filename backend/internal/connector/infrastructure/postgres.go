// Package infrastructure содержит PostgreSQL-хранилище и адаптеры каналов.
package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
		       verification_secret_hash, encrypted_credentials, last_event_at, last_success_at, last_error_at,
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
		       verification_secret_hash, encrypted_credentials, last_event_at, last_success_at, last_error_at,
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
			verification_secret_hash, encrypted_credentials, last_event_at, last_success_at, last_error_at,
			last_error_code, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11, $12, $13, $14, $15)`,
		connection.ID, tenantID, connection.LocationID, connection.Provider, connection.Name,
		connection.Status, string(capabilities), connection.VerificationSecretHash,
		nullableBytes(connection.EncryptedCredentials), connection.LastEventAt, connection.LastSuccessAt, connection.LastErrorAt,
		connection.LastErrorCode, connection.CreatedAt, connection.UpdatedAt,
	)
	if err != nil {
		return mapPostgresError("insert channel connection", err)
	}
	return nil
}

func (repository *PostgresRepository) UpdateConnectionHealth(
	ctx context.Context,
	tenantID, connectionID string,
	health domain.ConnectionHealth,
) (domain.ChannelConnection, bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || connectionID == "" || health.Validate() != nil {
		return domain.ChannelConnection{}, false, domain.ErrInvalid
	}
	return scanConnection(repository.pool.QueryRow(ctx, `
		UPDATE channel_connections
		SET status = $3,
		    last_success_at = CASE WHEN $3 = 'ACTIVE' THEN $4 ELSE last_success_at END,
		    last_error_at = CASE WHEN $3 = 'ACTIVE' THEN NULL ELSE COALESCE($5, $4) END,
		    last_error_code = CASE WHEN $3 = 'ACTIVE' THEN NULL ELSE $6 END,
		    updated_at = $4
		WHERE tenant_id = $1 AND id = $2
		RETURNING id, tenant_id, location_id, provider, name, status, capabilities,
		          verification_secret_hash, encrypted_credentials, last_event_at, last_success_at, last_error_at,
		          last_error_code, created_at, updated_at`,
		tenantID, connectionID, health.Status, health.CheckedAt.UTC(), health.LastErrorAt, health.LastErrorCode,
	))
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
		          verification_secret_hash, encrypted_credentials, last_event_at, last_success_at, last_error_at,
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
		       verification_secret_hash, encrypted_credentials, last_event_at, last_success_at, last_error_at,
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

func (repository *PostgresRepository) PendingNormalization(
	ctx context.Context,
	limit int,
) ([]domain.NormalizationItem, error) {
	if repository == nil || repository.pool == nil || limit < 1 || limit > 100 {
		return nil, domain.ErrInvalid
	}
	rows, err := repository.pool.Query(ctx, `
		SELECT
			w.id, w.tenant_id, w.connection_id, w.raw_event_id, w.status, w.created_at,
			c.id, c.tenant_id, c.location_id, c.provider, c.name, c.status, c.capabilities,
			c.verification_secret_hash, c.last_event_at, c.last_success_at, c.last_error_at,
			c.last_error_code, c.created_at, c.updated_at,
			r.id, r.tenant_id, r.connection_id, r.provider, r.external_event_id, r.payload,
			r.payload_hash, r.status, r.error_code, r.received_at, r.processed_at, r.created_at
		FROM raw_event_normalization_work w
		JOIN channel_connections c
		  ON c.tenant_id = w.tenant_id AND c.id = w.connection_id
		JOIN raw_events r
		  ON r.tenant_id = w.tenant_id AND r.id = w.raw_event_id AND r.connection_id = w.connection_id
		WHERE w.status = 'PENDING' AND r.status = 'RECEIVED'
		ORDER BY w.created_at, w.id
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("список заданий канонизации: %w", err)
	}
	defer rows.Close()
	items := make([]domain.NormalizationItem, 0)
	for rows.Next() {
		var item domain.NormalizationItem
		var capabilitiesJSON []byte
		if err := rows.Scan(
			&item.Work.ID, &item.Work.TenantID, &item.Work.ConnectionID, &item.Work.RawEventID,
			&item.Work.Status, &item.Work.CreatedAt,
			&item.Connection.ID, &item.Connection.TenantID, &item.Connection.LocationID,
			&item.Connection.Provider, &item.Connection.Name, &item.Connection.Status, &capabilitiesJSON,
			&item.Connection.VerificationSecretHash, &item.Connection.LastEventAt, &item.Connection.LastSuccessAt,
			&item.Connection.LastErrorAt, &item.Connection.LastErrorCode, &item.Connection.CreatedAt, &item.Connection.UpdatedAt,
			&item.Event.ID, &item.Event.TenantID, &item.Event.ConnectionID, &item.Event.Provider,
			&item.Event.ExternalEventID, &item.Event.Payload, &item.Event.PayloadHash, &item.Event.Status,
			&item.Event.ErrorCode, &item.Event.ReceivedAt, &item.Event.ProcessedAt, &item.Event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("чтение задания канонизации: %w", err)
		}
		if err := json.Unmarshal(capabilitiesJSON, &item.Connection.Capabilities); err != nil ||
			item.Connection.Validate() != nil || item.Event.Validate() != nil || item.Work.Validate(item.Event) != nil {
			return nil, domain.ErrInvalid
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("обход заданий канонизации: %w", err)
	}
	return items, nil
}

func (repository *PostgresRepository) CompleteNormalization(
	ctx context.Context,
	tenantID, rawEventID string,
	at time.Time,
) error {
	return repository.finishNormalization(ctx, tenantID, rawEventID, domain.RawEventProcessed, nil, at)
}

func (repository *PostgresRepository) FailNormalization(
	ctx context.Context,
	tenantID, rawEventID, errorCode string,
	at time.Time,
) error {
	errorCode = strings.TrimSpace(errorCode)
	if errorCode == "" || len(errorCode) > 100 {
		return domain.ErrInvalid
	}
	return repository.finishNormalization(ctx, tenantID, rawEventID, domain.RawEventFailed, &errorCode, at)
}

func (repository *PostgresRepository) finishNormalization(
	ctx context.Context,
	tenantID, rawEventID string,
	status domain.RawEventStatus,
	errorCode *string,
	at time.Time,
) error {
	if repository == nil || repository.pool == nil || tenantID == "" || rawEventID == "" || at.IsZero() ||
		(status != domain.RawEventProcessed && status != domain.RawEventFailed) {
		return domain.ErrInvalid
	}
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начало завершения канонизации: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var connectionID string
	err = tx.QueryRow(ctx, `
		UPDATE raw_events
		SET status = $3, error_code = $4, processed_at = $5
		WHERE tenant_id = $1 AND id = $2 AND status = 'RECEIVED'
		RETURNING connection_id`, tenantID, rawEventID, status, errorCode, at.UTC()).Scan(&connectionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return mapPostgresError("завершение RawEvent", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM raw_event_normalization_work
		WHERE tenant_id = $1 AND raw_event_id = $2`, tenantID, rawEventID); err != nil {
		return mapPostgresError("удаление задания канонизации", err)
	}
	if status == domain.RawEventFailed {
		if _, err := tx.Exec(ctx, `
			UPDATE channel_connections
			SET status = 'ERROR', last_error_at = $4, last_error_code = $3, updated_at = $4
			WHERE tenant_id = $1 AND id = $2`, tenantID, connectionID, *errorCode, at.UTC()); err != nil {
			return mapPostgresError("обновление ошибки подключения", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("фиксация завершения канонизации: %w", err)
	}
	return nil
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
		&connection.EncryptedCredentials, &connection.LastEventAt, &connection.LastSuccessAt, &connection.LastErrorAt,
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

func nullableBytes(value []byte) []byte {
	if len(value) == 0 {
		return nil
	}
	return value
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
