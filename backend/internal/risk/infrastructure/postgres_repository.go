package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/risk/domain"
)

// PostgresRepository — производственное хранилище агрегатов Risk. Частичный
// уникальный индекс в миграции является последней защитой от гонок worker.
type PostgresRepository struct{ pool *pgxpool.Pool }

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (repository *PostgresRepository) UpsertActive(
	ctx context.Context,
	candidate domain.Risk,
) (domain.Risk, bool, error) {
	if repository == nil || repository.pool == nil || !candidate.Active() || candidate.Validate() != nil {
		return domain.Risk{}, false, domain.ErrInvalidRisk
	}
	// INSERT и последующий UPDATE выполняются отдельными операторами: после
	// ожидания конкурентного INSERT следующий снимок READ COMMITTED уже видит
	// победившую строку. Повтор нужен для редкой гонки с ResolveActive.
	for range 3 {
		stored, err := scanRisk(repository.pool.QueryRow(ctx, `
			INSERT INTO risk_signals(
				id, tenant_id, opportunity_id, location_id, type, severity, status,
				reason_code, reason_text, source, risk_engine_version,
				trigger_message_id, detected_at, due_at, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, 'OPEN', $7, $8, $9, $10, $11, $12, $13, $12, $14)
			ON CONFLICT (tenant_id, opportunity_id, type)
			WHERE status IN ('OPEN', 'ACKNOWLEDGED', 'ACTED')
			DO NOTHING
			RETURNING id, tenant_id, opportunity_id, location_id, type, severity, status,
			          source, risk_engine_version, trigger_message_id, reason_code,
			          reason_text, detected_at, due_at, updated_at,
			          acknowledged_at, acted_at, resolved_at`,
			candidate.ID, candidate.TenantID, candidate.OpportunityID, candidate.LocationID,
			candidate.Type, candidate.Severity, candidate.ReasonCode, candidate.Reason,
			candidate.Source, candidate.PolicyVersion, candidate.TriggerMessageID,
			candidate.DetectedAt, candidate.DueAt, candidate.UpdatedAt,
		))
		if err == nil {
			return stored, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.Risk{}, false, mapRiskError("создание риска", err)
		}

		stored, err = scanRisk(repository.pool.QueryRow(ctx, `
			UPDATE risk_signals
			SET location_id = $4, severity = $5, reason_code = $6, reason_text = $7,
			    source = $8, risk_engine_version = $9, trigger_message_id = $10,
			    detected_at = $11, due_at = $12, updated_at = $13
			WHERE tenant_id = $1 AND opportunity_id = $2 AND type = $3
			  AND status IN ('OPEN', 'ACKNOWLEDGED', 'ACTED')
			RETURNING id, tenant_id, opportunity_id, location_id, type, severity, status,
			          source, risk_engine_version, trigger_message_id, reason_code,
			          reason_text, detected_at, due_at, updated_at,
			          acknowledged_at, acted_at, resolved_at`,
			candidate.TenantID, candidate.OpportunityID, candidate.Type, candidate.LocationID,
			candidate.Severity, candidate.ReasonCode, candidate.Reason, candidate.Source,
			candidate.PolicyVersion, candidate.TriggerMessageID, candidate.DetectedAt,
			candidate.DueAt, candidate.UpdatedAt,
		))
		if err == nil {
			return stored, false, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.Risk{}, false, mapRiskError("обновление активного риска", err)
		}
	}
	return domain.Risk{}, false, fmt.Errorf("состояние активного риска непрерывно изменяется")
}

func (repository *PostgresRepository) FindActive(
	ctx context.Context,
	tenantID, opportunityID string,
	riskType domain.Type,
) (domain.Risk, bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || opportunityID == "" || riskType != domain.TypeNoResponse {
		return domain.Risk{}, false, domain.ErrInvalidRisk
	}
	risk, err := scanRisk(repository.pool.QueryRow(ctx, `
		SELECT id, tenant_id, opportunity_id, location_id, type, severity, status,
		       source, risk_engine_version, trigger_message_id, reason_code,
		       reason_text, detected_at, due_at, updated_at,
		       acknowledged_at, acted_at, resolved_at
		FROM risk_signals
		WHERE tenant_id = $1 AND opportunity_id = $2 AND type = $3
		  AND status IN ('OPEN', 'ACKNOWLEDGED', 'ACTED')`, tenantID, opportunityID, riskType))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Risk{}, false, nil
	}
	if err != nil {
		return domain.Risk{}, false, mapRiskError("чтение активного риска", err)
	}
	return risk, true, nil
}

func (repository *PostgresRepository) ResolveActive(
	ctx context.Context,
	tenantID, opportunityID string,
	riskType domain.Type,
	at time.Time,
) (bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || opportunityID == "" ||
		riskType != domain.TypeNoResponse || at.IsZero() {
		return false, domain.ErrInvalidRisk
	}
	result, err := repository.pool.Exec(ctx, `
		UPDATE risk_signals
		SET status = 'RESOLVED', resolved_at = $4, updated_at = $4
		WHERE tenant_id = $1 AND opportunity_id = $2 AND type = $3
		  AND status IN ('OPEN', 'ACKNOWLEDGED', 'ACTED')`, tenantID, opportunityID, riskType, at.UTC())
	if err != nil {
		return false, mapRiskError("автоматическое закрытие риска", err)
	}
	return result.RowsAffected() == 1, nil
}

type riskRow interface{ Scan(...any) error }

func scanRisk(row riskRow) (domain.Risk, error) {
	var risk domain.Risk
	if err := row.Scan(
		&risk.ID, &risk.TenantID, &risk.OpportunityID, &risk.LocationID,
		&risk.Type, &risk.Severity, &risk.Status, &risk.Source, &risk.PolicyVersion,
		&risk.TriggerMessageID, &risk.ReasonCode, &risk.Reason, &risk.DetectedAt,
		&risk.DueAt, &risk.UpdatedAt, &risk.AcknowledgedAt, &risk.ActedAt, &risk.ResolvedAt,
	); err != nil {
		return domain.Risk{}, err
	}
	if risk.Validate() != nil {
		return domain.Risk{}, domain.ErrInvalidRisk
	}
	return risk, nil
}

func mapRiskError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "23503", "23514", "22P02", "22003":
			return domain.ErrInvalidRisk
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}

var _ domain.Repository = (*PostgresRepository)(nil)
