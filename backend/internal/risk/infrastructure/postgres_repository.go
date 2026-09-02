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

	eventsdomain "lidradar/backend/internal/events/domain"
	eventsinfrastructure "lidradar/backend/internal/events/infrastructure"
	"lidradar/backend/internal/risk/domain"
)

// PostgresRepository — производственное хранилище агрегатов Risk. Частичный
// уникальный индекс в миграции является последней защитой от гонок worker.
type PostgresRepository struct {
	pool   *pgxpool.Pool
	events *eventsinfrastructure.PostgresStore
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool, events: eventsinfrastructure.NewPostgresStore(pool)}
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
		tx, beginErr := repository.pool.Begin(ctx)
		if beginErr != nil {
			return domain.Risk{}, false, fmt.Errorf("начало создания риска: %w", beginErr)
		}
		stored, err := scanRisk(tx.QueryRow(ctx, `
			INSERT INTO risk_signals(
				id, tenant_id, opportunity_id, location_id, type, severity, status,
				reason_code, reason_text, confidence, source, risk_engine_version,
				ai_run_id, trigger_message_id, detected_at, due_at, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, 'OPEN', $7, $8, $9, $10, $11, $12, $13, $14, $15, $14, $16)
			ON CONFLICT (tenant_id, opportunity_id, type)
			WHERE status IN ('OPEN', 'ACKNOWLEDGED', 'ACTED')
			DO NOTHING
			RETURNING id, tenant_id, opportunity_id, location_id, type, severity, status,
			          source, confidence, ai_run_id, risk_engine_version, trigger_message_id, reason_code,
			          reason_text, detected_at, due_at, updated_at,
			          acknowledged_at, acted_at, resolved_at`,
			candidate.ID, candidate.TenantID, candidate.OpportunityID, candidate.LocationID,
			candidate.Type, candidate.Severity, candidate.ReasonCode, candidate.Reason,
			candidate.Confidence, candidate.Source, candidate.PolicyVersion, candidate.AIRunID,
			candidate.TriggerMessageID, candidate.DetectedAt, candidate.DueAt, candidate.UpdatedAt,
		))
		if err == nil {
			if appendErr := repository.appendOpenedEvent(ctx, tx, stored); appendErr != nil {
				_ = tx.Rollback(ctx)
				return domain.Risk{}, false, appendErr
			}
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return domain.Risk{}, false, fmt.Errorf("фиксация создания риска: %w", commitErr)
			}
			return stored, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(ctx)
			return domain.Risk{}, false, mapRiskError("создание риска", err)
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return domain.Risk{}, false, fmt.Errorf("фиксация конкурентной проверки риска: %w", commitErr)
		}

		stored, err = scanRisk(repository.pool.QueryRow(ctx, `
			UPDATE risk_signals
			SET location_id = $4, severity = $5, reason_code = $6, reason_text = $7,
			    confidence = $8, source = $9, risk_engine_version = $10,
			    ai_run_id = $11, trigger_message_id = $12,
			    detected_at = $13, due_at = $14, updated_at = $15
			WHERE tenant_id = $1 AND opportunity_id = $2 AND type = $3
			  AND status IN ('OPEN', 'ACKNOWLEDGED', 'ACTED')
			RETURNING id, tenant_id, opportunity_id, location_id, type, severity, status,
			          source, confidence, ai_run_id, risk_engine_version, trigger_message_id, reason_code,
			          reason_text, detected_at, due_at, updated_at,
			          acknowledged_at, acted_at, resolved_at`,
			candidate.TenantID, candidate.OpportunityID, candidate.Type, candidate.LocationID,
			candidate.Severity, candidate.ReasonCode, candidate.Reason, candidate.Confidence,
			candidate.Source, candidate.PolicyVersion, candidate.AIRunID,
			candidate.TriggerMessageID, candidate.DetectedAt, candidate.DueAt, candidate.UpdatedAt,
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

type riskOpenedEventData struct {
	RiskID        string          `json:"riskId"`
	OpportunityID string          `json:"opportunityId"`
	LocationID    string          `json:"locationId"`
	Type          domain.Type     `json:"type"`
	Severity      domain.Severity `json:"severity"`
}

func (repository *PostgresRepository) appendOpenedEvent(ctx context.Context, tx pgx.Tx, risk domain.Risk) error {
	if repository.events == nil {
		return errors.New("исходящий журнал риска не настроен")
	}
	data, err := json.Marshal(riskOpenedEventData{
		RiskID: risk.ID, OpportunityID: risk.OpportunityID, LocationID: risk.LocationID,
		Type: risk.Type, Severity: risk.Severity,
	})
	if err != nil {
		return fmt.Errorf("подготовка события риска: %w", err)
	}
	event, err := eventsdomain.NewEvent(
		risk.ID, "risk.opened", 1, risk.TenantID, "risk", risk.ID, risk.ID, data, risk.DetectedAt,
	)
	if err != nil {
		return fmt.Errorf("проверка события риска: %w", err)
	}
	if _, _, err := repository.events.AppendTx(ctx, tx, event); err != nil {
		return fmt.Errorf("добавление события риска: %w", err)
	}
	return nil
}

func (repository *PostgresRepository) FindActive(
	ctx context.Context,
	tenantID, opportunityID string,
	riskType domain.Type,
) (domain.Risk, bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || opportunityID == "" || !supportedRiskType(riskType) {
		return domain.Risk{}, false, domain.ErrInvalidRisk
	}
	risk, err := scanRisk(repository.pool.QueryRow(ctx, `
		SELECT id, tenant_id, opportunity_id, location_id, type, severity, status,
		       source, confidence, ai_run_id, risk_engine_version, trigger_message_id, reason_code,
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
		!supportedRiskType(riskType) || at.IsZero() {
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
		&risk.Type, &risk.Severity, &risk.Status, &risk.Source, &risk.Confidence,
		&risk.AIRunID, &risk.PolicyVersion,
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

func supportedRiskType(riskType domain.Type) bool {
	return riskType == domain.TypeNoResponse || riskType == domain.TypeBookingNotConfirmed
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
