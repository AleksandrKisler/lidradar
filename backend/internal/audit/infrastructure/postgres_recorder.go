// Package infrastructure пишет аудит в append-only таблицы PostgreSQL.
package infrastructure

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/audit/application"
	"lidradar/backend/internal/audit/domain"
)

type IDs interface{ NewID() (string, error) }

type PostgresRecorder struct {
	pool *pgxpool.Pool
	ids  IDs
}

func NewPostgresRecorder(pool *pgxpool.Pool, ids IDs) *PostgresRecorder {
	return &PostgresRecorder{pool: pool, ids: ids}
}

// Tenant пишет в журнал организации; составной внешний ключ требует, чтобы
// актор был участником этой организации.
func (recorder *PostgresRecorder) Tenant(ctx context.Context, entry domain.Entry) error {
	if recorder == nil || recorder.pool == nil || recorder.ids == nil {
		return domain.ErrInvalid
	}
	id, err := recorder.ids.NewID()
	if err != nil {
		return err
	}
	entry.ID = id
	if err := entry.Validate(); err != nil {
		return err
	}
	if _, err := recorder.pool.Exec(ctx, `
		INSERT INTO audit_log(id, tenant_id, actor_user_id, operation, entity_type, entity_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		entry.ID, entry.TenantID, entry.ActorID, entry.Operation, entry.EntityType, entry.EntityID, entry.At); err != nil {
		return fmt.Errorf("запись аудита организации: %w", err)
	}
	return nil
}

func (recorder *PostgresRecorder) Auth(ctx context.Context, entry domain.AuthEntry) error {
	if recorder == nil || recorder.pool == nil || recorder.ids == nil {
		return domain.ErrInvalid
	}
	id, err := recorder.ids.NewID()
	if err != nil {
		return err
	}
	entry.ID = id
	if err := entry.Validate(); err != nil {
		return err
	}
	if _, err := recorder.pool.Exec(ctx, `
		INSERT INTO auth_audit_log(id, user_id, operation, ip_address, created_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5)`,
		entry.ID, entry.UserID, entry.Operation, entry.IPAddress, entry.At); err != nil {
		return fmt.Errorf("запись аудита входа: %w", err)
	}
	return nil
}

var _ application.Recorder = (*PostgresRecorder)(nil)
