package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/corrective/application"
	"lidradar/backend/internal/corrective/domain"
	"lidradar/backend/platform/ids"
)

const (
	actionOperation  = "corrective.action.create"
	outcomeOperation = "corrective.outcome.create"
)

// PostgresStore хранит рекомендации и корректирующие факты в авторитетной
// базе. Факт, ключ идемпотентности и запись аудита фиксируются одной
// транзакцией.
type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func (store *PostgresStore) Risk(
	ctx context.Context,
	tenantID, riskID string,
) (application.RiskReference, bool, error) {
	if store == nil || store.pool == nil || !ids.Valid(tenantID) || !ids.Valid(riskID) {
		return application.RiskReference{}, false, application.ErrInvalid
	}
	var reference application.RiskReference
	err := store.pool.QueryRow(ctx, `
		SELECT opportunity_id, type
		FROM risk_signals
		WHERE tenant_id = $1 AND id = $2`, tenantID, riskID,
	).Scan(&reference.OpportunityID, &reference.Type)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.RiskReference{}, false, nil
	}
	if err != nil {
		return application.RiskReference{}, false, mapCorrectiveError("чтение риска", err)
	}
	return reference, true, nil
}

func (store *PostgresStore) OpportunityExists(
	ctx context.Context,
	tenantID, opportunityID string,
) (bool, error) {
	if store == nil || store.pool == nil || !ids.Valid(tenantID) || !ids.Valid(opportunityID) {
		return false, application.ErrInvalid
	}
	var exists bool
	if err := store.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM opportunities WHERE tenant_id = $1 AND id = $2
		)`, tenantID, opportunityID,
	).Scan(&exists); err != nil {
		return false, mapCorrectiveError("проверка возможности", err)
	}
	return exists, nil
}

func (store *PostgresStore) EnsureRecommendation(
	ctx context.Context,
	recommendation domain.Recommendation,
) (domain.Recommendation, bool, error) {
	if store == nil || store.pool == nil || !ids.Valid(recommendation.ID) ||
		!ids.Valid(recommendation.TenantID) || !ids.Valid(recommendation.RiskID) {
		return domain.Recommendation{}, false, application.ErrInvalid
	}
	if _, err := domain.NewRecommendation(
		recommendation.ID, recommendation.TenantID, recommendation.RiskID,
		recommendation.Text, recommendation.CreatedAt,
	); err != nil || recommendation.Source != "TEMPLATE" {
		return domain.Recommendation{}, false, application.ErrInvalid
	}
	stored, err := scanRecommendation(store.pool.QueryRow(ctx, `
		INSERT INTO recommendations(id, tenant_id, risk_id, text, source, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, risk_id, source) DO NOTHING
		RETURNING id, tenant_id, risk_id, text, source, created_at`,
		recommendation.ID, recommendation.TenantID, recommendation.RiskID,
		recommendation.Text, recommendation.Source, recommendation.CreatedAt,
	))
	if err == nil {
		return stored, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Recommendation{}, false, mapCorrectiveError("создание рекомендации", err)
	}
	stored, err = scanRecommendation(store.pool.QueryRow(ctx, `
		SELECT id, tenant_id, risk_id, text, source, created_at
		FROM recommendations
		WHERE tenant_id = $1 AND risk_id = $2 AND source = 'TEMPLATE'`,
		recommendation.TenantID, recommendation.RiskID,
	))
	if err != nil {
		return domain.Recommendation{}, false, mapCorrectiveError("чтение рекомендации", err)
	}
	return stored, false, nil
}

func (store *PostgresStore) AppendAction(
	ctx context.Context,
	action domain.Action,
	key string,
	requestHash [32]byte,
	audit application.AuditRecord,
) (domain.Action, bool, error) {
	if store == nil || store.pool == nil || !validActionCommand(action, audit) {
		return domain.Action{}, false, application.ErrInvalid
	}
	responseBody, err := json.Marshal(action)
	if err != nil {
		return domain.Action{}, false, fmt.Errorf("кодирование ответа действия: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.Action{}, false, fmt.Errorf("начало записи действия: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, storedBody, err := reserveIdempotency(
		ctx, tx, action.TenantID, key, actionOperation, requestHash,
		httpCreated, responseBody, action.CreatedAt,
	)
	if err != nil {
		return domain.Action{}, false, err
	}
	if !created {
		stored, decodeErr := decodeStoredAction(storedBody, action.TenantID)
		if decodeErr != nil {
			return domain.Action{}, false, decodeErr
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.Action{}, false, fmt.Errorf("фиксация повтора действия: %w", err)
		}
		return stored, false, nil
	}
	inserted, err := tx.Exec(ctx, `
		INSERT INTO actions(id, tenant_id, risk_id, opportunity_id, actor_user_id, type, note, created_at)
		SELECT $1, $2, $3, risk.opportunity_id, $4, $5, $6, $7
		FROM risk_signals AS risk
		WHERE risk.tenant_id = $2 AND risk.id = $3
		  AND EXISTS (
			SELECT 1 FROM memberships
			WHERE tenant_id = $2 AND user_id = $4 AND status = 'ACTIVE'
		  )`,
		action.ID, action.TenantID, action.RiskID, action.ActorID,
		action.Type, action.Note, action.CreatedAt,
	)
	if err != nil {
		return domain.Action{}, false, mapCorrectiveError("запись действия", err)
	}
	if inserted.RowsAffected() != 1 {
		return domain.Action{}, false, application.ErrForbidden
	}
	if _, err := tx.Exec(ctx, `
		UPDATE risk_signals
		SET status = 'ACTED',
		    acknowledged_at = COALESCE(acknowledged_at, $3),
		    acted_at = COALESCE(acted_at, $3),
		    updated_at = CASE WHEN status IN ('OPEN', 'ACKNOWLEDGED') THEN $3 ELSE updated_at END
		WHERE tenant_id = $1 AND id = $2 AND status IN ('OPEN', 'ACKNOWLEDGED', 'ACTED')`,
		action.TenantID, action.RiskID, action.CreatedAt,
	); err != nil {
		return domain.Action{}, false, mapCorrectiveError("отметка выполненного действия", err)
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return domain.Action{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Action{}, false, fmt.Errorf("фиксация действия: %w", err)
	}
	return action, true, nil
}

func (store *PostgresStore) AppendOutcome(
	ctx context.Context,
	outcome domain.Outcome,
	key string,
	requestHash [32]byte,
	audit application.AuditRecord,
) (domain.Outcome, bool, error) {
	if store == nil || store.pool == nil || !validOutcomeCommand(outcome, audit) {
		return domain.Outcome{}, false, application.ErrInvalid
	}
	responseBody, err := json.Marshal(outcome)
	if err != nil {
		return domain.Outcome{}, false, fmt.Errorf("кодирование ответа исхода: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return domain.Outcome{}, false, fmt.Errorf("начало записи исхода: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, storedBody, err := reserveIdempotency(
		ctx, tx, outcome.TenantID, key, outcomeOperation, requestHash,
		httpCreated, responseBody, outcome.CreatedAt,
	)
	if err != nil {
		return domain.Outcome{}, false, err
	}
	if !created {
		stored, decodeErr := decodeStoredOutcome(storedBody, outcome.TenantID)
		if decodeErr != nil {
			return domain.Outcome{}, false, decodeErr
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.Outcome{}, false, fmt.Errorf("фиксация повтора исхода: %w", err)
		}
		return stored, false, nil
	}
	inserted, err := tx.Exec(ctx, `
		INSERT INTO outcomes(id, tenant_id, opportunity_id, actor_user_id, status, note, created_at)
		SELECT $1, $2, $3, $4, $5, $6, $7
		WHERE EXISTS (
			SELECT 1 FROM memberships
			WHERE tenant_id = $2 AND user_id = $4 AND status = 'ACTIVE'
		)`,
		outcome.ID, outcome.TenantID, outcome.OpportunityID, outcome.ActorID,
		outcome.Status, outcome.Note, outcome.CreatedAt,
	)
	if err != nil {
		return domain.Outcome{}, false, mapCorrectiveError("запись исхода", err)
	}
	if inserted.RowsAffected() != 1 {
		return domain.Outcome{}, false, application.ErrForbidden
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return domain.Outcome{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Outcome{}, false, fmt.Errorf("фиксация исхода: %w", err)
	}
	return outcome, true, nil
}

const httpCreated = 201

func reserveIdempotency(
	ctx context.Context,
	tx pgx.Tx,
	tenantID, key, operation string,
	requestHash [32]byte,
	responseStatus int,
	responseBody []byte,
	createdAt time.Time,
) (bool, []byte, error) {
	var storedBody []byte
	err := tx.QueryRow(ctx, `
		INSERT INTO idempotency_keys(
			tenant_id, key, operation, request_hash, response_status, response_body, created_at
		) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)
		ON CONFLICT (tenant_id, key, operation) DO NOTHING
		RETURNING response_body`,
		tenantID, key, operation, requestHash[:], responseStatus, string(responseBody), createdAt,
	).Scan(&storedBody)
	if err == nil {
		return true, storedBody, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, nil, mapCorrectiveError("резервирование ключа идемпотентности", err)
	}
	var existingHash []byte
	if err := tx.QueryRow(ctx, `
		SELECT request_hash, response_body
		FROM idempotency_keys
		WHERE tenant_id = $1 AND key = $2 AND operation = $3`,
		tenantID, key, operation,
	).Scan(&existingHash, &storedBody); err != nil {
		return false, nil, mapCorrectiveError("чтение ключа идемпотентности", err)
	}
	if !bytes.Equal(existingHash, requestHash[:]) {
		return false, nil, application.ErrConflict
	}
	return false, storedBody, nil
}

func insertAudit(ctx context.Context, tx pgx.Tx, audit application.AuditRecord) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_log(
			id, tenant_id, actor_user_id, operation, entity_type, entity_id, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		audit.ID, audit.TenantID, audit.ActorID, audit.Operation,
		audit.ResourceType, audit.ResourceID, audit.At,
	); err != nil {
		return mapCorrectiveError("запись аудита", err)
	}
	return nil
}

func scanRecommendation(row pgx.Row) (domain.Recommendation, error) {
	var recommendation domain.Recommendation
	if err := row.Scan(
		&recommendation.ID, &recommendation.TenantID, &recommendation.RiskID,
		&recommendation.Text, &recommendation.Source, &recommendation.CreatedAt,
	); err != nil {
		return domain.Recommendation{}, err
	}
	if _, err := domain.NewRecommendation(
		recommendation.ID, recommendation.TenantID, recommendation.RiskID,
		recommendation.Text, recommendation.CreatedAt,
	); err != nil || recommendation.Source != "TEMPLATE" {
		return domain.Recommendation{}, domain.ErrInvalid
	}
	return recommendation, nil
}

func decodeStoredAction(body []byte, tenantID string) (domain.Action, error) {
	var action domain.Action
	if err := json.Unmarshal(body, &action); err != nil {
		return domain.Action{}, fmt.Errorf("чтение сохранённого ответа действия: %w", err)
	}
	action.TenantID = tenantID
	validated, err := domain.NewAction(
		action.ID, action.TenantID, action.RiskID, action.ActorID,
		action.Type, action.Note, action.CreatedAt,
	)
	if err != nil {
		return domain.Action{}, fmt.Errorf("проверка сохранённого ответа действия: %w", err)
	}
	return validated, nil
}

func decodeStoredOutcome(body []byte, tenantID string) (domain.Outcome, error) {
	var outcome domain.Outcome
	if err := json.Unmarshal(body, &outcome); err != nil {
		return domain.Outcome{}, fmt.Errorf("чтение сохранённого ответа исхода: %w", err)
	}
	outcome.TenantID = tenantID
	validated, err := domain.NewOutcome(
		outcome.ID, outcome.TenantID, outcome.OpportunityID, outcome.ActorID,
		outcome.Status, outcome.Note, outcome.CreatedAt,
	)
	if err != nil {
		return domain.Outcome{}, fmt.Errorf("проверка сохранённого ответа исхода: %w", err)
	}
	return validated, nil
}

func validActionCommand(action domain.Action, audit application.AuditRecord) bool {
	if !ids.Valid(action.ID) || !ids.Valid(action.TenantID) || !ids.Valid(action.RiskID) || !ids.Valid(action.ActorID) ||
		!validAudit(audit, action.TenantID, action.ActorID, "ACTION_RECORDED", "ACTION", action.ID, action.CreatedAt) {
		return false
	}
	_, err := domain.NewAction(
		action.ID, action.TenantID, action.RiskID, action.ActorID,
		action.Type, action.Note, action.CreatedAt,
	)
	return err == nil
}

func validOutcomeCommand(outcome domain.Outcome, audit application.AuditRecord) bool {
	if !ids.Valid(outcome.ID) || !ids.Valid(outcome.TenantID) || !ids.Valid(outcome.OpportunityID) || !ids.Valid(outcome.ActorID) ||
		!validAudit(audit, outcome.TenantID, outcome.ActorID, "OUTCOME_RECORDED", "OUTCOME", outcome.ID, outcome.CreatedAt) {
		return false
	}
	_, err := domain.NewOutcome(
		outcome.ID, outcome.TenantID, outcome.OpportunityID, outcome.ActorID,
		outcome.Status, outcome.Note, outcome.CreatedAt,
	)
	return err == nil
}

func validAudit(
	audit application.AuditRecord,
	tenantID, actorID, operation, resourceType, resourceID string,
	at time.Time,
) bool {
	return ids.Valid(audit.ID) && audit.TenantID == tenantID && audit.ActorID == actorID &&
		audit.Operation == operation && audit.ResourceType == resourceType &&
		audit.ResourceID == resourceID && audit.At.Equal(at)
}

func mapCorrectiveError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "22P02", "22001", "22003", "23514":
			return application.ErrInvalid
		case "23503":
			return application.ErrNotFound
		case "23505":
			return application.ErrConflict
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}

var _ application.Store = (*PostgresStore)(nil)
