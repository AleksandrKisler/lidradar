package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/risk/application"
	"lidradar/backend/internal/risk/domain"
	"lidradar/backend/platform/ids"
)

// PostgresFeedbackStore хранит обратную связь append-only вместе со снимком
// риска и аудитом; метрика точности читает последний вердикт на риск.
type PostgresFeedbackStore struct{ pool *pgxpool.Pool }

func NewPostgresFeedbackStore(pool *pgxpool.Pool) *PostgresFeedbackStore {
	return &PostgresFeedbackStore{pool: pool}
}

const riskWithStageColumns = `
	risk.id, risk.tenant_id, risk.opportunity_id, risk.location_id, risk.type, risk.severity, risk.status,
	risk.source, risk.confidence, risk.ai_run_id, risk.risk_engine_version, risk.trigger_message_id, risk.reason_code,
	risk.reason_text, risk.detected_at, risk.due_at, risk.updated_at, risk.acknowledged_at, risk.acted_at, risk.resolved_at,
	opportunity.stage`

const riskWithStageJoin = `
	FROM risk_signals AS risk
	JOIN opportunities AS opportunity ON opportunity.tenant_id = risk.tenant_id AND opportunity.id = risk.opportunity_id`

func (store *PostgresFeedbackStore) RiskForFeedback(ctx context.Context, tenantID, riskID string) (domain.Risk, string, bool, error) {
	if store == nil || store.pool == nil || tenantID == "" {
		return domain.Risk{}, "", false, application.ErrInvalidCommand
	}
	if !ids.Valid(riskID) {
		return domain.Risk{}, "", false, nil
	}
	risk, stage, err := scanRiskWithStage(store.pool.QueryRow(ctx, `
		SELECT `+riskWithStageColumns+riskWithStageJoin+`
		WHERE risk.tenant_id = $1 AND risk.id = $2`, tenantID, riskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Risk{}, "", false, nil
	}
	if err != nil {
		return domain.Risk{}, "", false, mapRiskError("чтение риска для обратной связи", err)
	}
	return risk, stage, true, nil
}

func (store *PostgresFeedbackStore) DatasetConsentActive(ctx context.Context, tenantID string) (bool, error) {
	if store == nil || store.pool == nil || tenantID == "" {
		return false, application.ErrInvalidCommand
	}
	var active bool
	if err := store.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM ml_consents WHERE tenant_id = $1 AND scope = 'DATASETS' AND revoked_at IS NULL
		)`, tenantID).Scan(&active); err != nil {
		return false, mapRiskError("проверка ML-согласия", err)
	}
	return active, nil
}

func (store *PostgresFeedbackStore) AppendFeedback(
	ctx context.Context,
	feedback domain.Feedback,
	audit application.AuditRecord,
) (domain.Feedback, domain.Risk, bool, error) {
	if store == nil || store.pool == nil || feedback.Validate() != nil || audit.ID == "" || audit.TenantID != feedback.TenantID ||
		audit.ActorID != feedback.ActorID || audit.Operation == "" || audit.EntityType == "" || audit.EntityID == "" || audit.At.IsZero() {
		return domain.Feedback{}, domain.Risk{}, false, application.ErrInvalidCommand
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.Feedback{}, domain.Risk{}, false, fmt.Errorf("начало записи обратной связи: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var reason *string
	if feedback.Reason != "" {
		value := string(feedback.Reason)
		reason = &value
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO risk_feedback(
			id, tenant_id, risk_id, opportunity_id, actor_user_id, verdict, reason, note,
			risk_type, severity, risk_status, source, risk_engine_version, ai_run_id, trigger_message_id,
			opportunity_stage, detected_at, dataset_eligible, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`,
		feedback.ID, feedback.TenantID, feedback.RiskID, feedback.OpportunityID, feedback.ActorID, feedback.Verdict, reason,
		feedback.Note, feedback.Context.Type, feedback.Context.Severity, feedback.Context.Status, feedback.Context.Source,
		feedback.Context.PolicyVersion, feedback.Context.AIRunID, feedback.Context.TriggerMessageID,
		feedback.Context.OpportunityStage, feedback.Context.DetectedAt, feedback.DatasetEligible, feedback.CreatedAt,
	); err != nil {
		return domain.Feedback{}, domain.Risk{}, false, mapRiskError("запись обратной связи", err)
	}
	changed := false
	if feedback.Verdict == domain.VerdictFalsePositive {
		result, err := tx.Exec(ctx, `
			UPDATE risk_signals
			SET status = 'FALSE_POSITIVE', resolved_at = $3, updated_at = $3
			WHERE tenant_id = $1 AND id = $2 AND status IN ('OPEN', 'ACKNOWLEDGED', 'ACTED')`,
			feedback.TenantID, feedback.RiskID, feedback.CreatedAt)
		if err != nil {
			return domain.Feedback{}, domain.Risk{}, false, mapRiskError("закрытие ложного срабатывания", err)
		}
		changed = result.RowsAffected() == 1
	}
	risk, _, err := scanRiskWithStage(tx.QueryRow(ctx, `
		SELECT `+riskWithStageColumns+riskWithStageJoin+`
		WHERE risk.tenant_id = $1 AND risk.id = $2`, feedback.TenantID, feedback.RiskID))
	if err != nil {
		return domain.Feedback{}, domain.Risk{}, false, mapRiskError("чтение риска после обратной связи", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_log(id, tenant_id, actor_user_id, operation, entity_type, entity_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		audit.ID, audit.TenantID, audit.ActorID, audit.Operation, audit.EntityType, audit.EntityID, audit.At,
	); err != nil {
		return domain.Feedback{}, domain.Risk{}, false, mapRiskError("запись аудита обратной связи", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Feedback{}, domain.Risk{}, false, fmt.Errorf("фиксация обратной связи: %w", err)
	}
	return feedback, risk, changed, nil
}

// Precision считает по рискам, обнаруженным в окне [from, to): каждый риск
// учитывается один раз по последнему вердикту (LR-BE-RM-019), а покрытие —
// доля таких рисков с хотя бы одной записью.
func (store *PostgresFeedbackStore) Precision(ctx context.Context, tenantID string, from, to time.Time) ([]domain.PrecisionRow, error) {
	if store == nil || store.pool == nil || tenantID == "" || from.IsZero() || to.IsZero() || !from.Before(to) {
		return nil, application.ErrInvalidCommand
	}
	rows, err := store.pool.Query(ctx, `
		WITH risks AS (
			SELECT id, type FROM risk_signals
			WHERE tenant_id = $1 AND detected_at >= $2 AND detected_at < $3
		), latest AS (
			SELECT DISTINCT ON (feedback.risk_id) feedback.risk_id, feedback.verdict
			FROM risk_feedback AS feedback
			JOIN risks ON risks.id = feedback.risk_id
			WHERE feedback.tenant_id = $1
			ORDER BY feedback.risk_id, feedback.created_at DESC, feedback.id DESC
		)
		SELECT risks.type,
		       count(*) AS total_risks,
		       count(latest.risk_id) AS with_feedback,
		       count(*) FILTER (WHERE latest.verdict = 'TRUE_POSITIVE') AS true_positives,
		       count(*) FILTER (WHERE latest.verdict = 'FALSE_POSITIVE') AS false_positives
		FROM risks
		LEFT JOIN latest ON latest.risk_id = risks.id
		GROUP BY risks.type
		ORDER BY risks.type`, tenantID, from.UTC(), to.UTC())
	if err != nil {
		return nil, mapRiskError("расчёт точности рисков", err)
	}
	defer rows.Close()
	result := make([]domain.PrecisionRow, 0, len(domain.Types()))
	for rows.Next() {
		var row domain.PrecisionRow
		if err := rows.Scan(&row.Type, &row.TotalRisks, &row.WithFeedback, &row.TruePositives, &row.FalsePositives); err != nil {
			return nil, fmt.Errorf("чтение строки точности: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("обход строк точности: %w", err)
	}
	return result, nil
}

func scanRiskWithStage(row pgx.Row) (domain.Risk, string, error) {
	var risk domain.Risk
	var stage string
	if err := row.Scan(
		&risk.ID, &risk.TenantID, &risk.OpportunityID, &risk.LocationID,
		&risk.Type, &risk.Severity, &risk.Status, &risk.Source, &risk.Confidence,
		&risk.AIRunID, &risk.PolicyVersion,
		&risk.TriggerMessageID, &risk.ReasonCode, &risk.Reason, &risk.DetectedAt,
		&risk.DueAt, &risk.UpdatedAt, &risk.AcknowledgedAt, &risk.ActedAt, &risk.ResolvedAt,
		&stage,
	); err != nil {
		return domain.Risk{}, "", err
	}
	if risk.Validate() != nil {
		return domain.Risk{}, "", domain.ErrInvalidRisk
	}
	return risk, stage, nil
}

var _ application.FeedbackStore = (*PostgresFeedbackStore)(nil)
