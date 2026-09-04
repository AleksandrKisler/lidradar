// Package infrastructure хранит коммерческие возможности в PostgreSQL.
package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	eventsdomain "lidradar/backend/internal/events/domain"
	eventsinfrastructure "lidradar/backend/internal/events/infrastructure"
	"lidradar/backend/internal/opportunity/domain"
)

type PostgresRepository struct {
	pool   *pgxpool.Pool
	events *eventsinfrastructure.PostgresStore
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool, events: eventsinfrastructure.NewPostgresStore(pool)}
}

// Create атомарно создаёт возможность и первую запись истории. При гонке
// кандидатов частичный уникальный индекс возвращает уже созданную возможность.
func (repository *PostgresRepository) Create(
	ctx context.Context,
	opportunity domain.Opportunity,
	history domain.StageHistory,
) (domain.Opportunity, bool, error) {
	if repository == nil || repository.pool == nil || opportunity.Validate() != nil || history.Validate() != nil ||
		history.TenantID != opportunity.TenantID || history.OpportunityID != opportunity.ID ||
		history.FromStage != nil || history.ToStage != domain.StageNew || opportunity.Stage != domain.StageNew {
		return domain.Opportunity{}, false, domain.ErrInvalid
	}
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return domain.Opportunity{}, false, fmt.Errorf("начало создания возможности: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, inserted, err := insertOpportunity(ctx, tx, opportunity)
	if err != nil {
		return domain.Opportunity{}, false, err
	}
	if !inserted {
		existing, found, readErr := activeByConversation(ctx, tx, opportunity.TenantID, opportunity.ConversationID)
		if readErr != nil {
			return domain.Opportunity{}, false, readErr
		}
		if !found {
			return domain.Opportunity{}, false, domain.ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.Opportunity{}, false, fmt.Errorf("фиксация повторного кандидата: %w", err)
		}
		return existing, false, nil
	}
	if err := insertHistory(ctx, tx, history); err != nil {
		return domain.Opportunity{}, false, err
	}
	if err := repository.appendEvent(ctx, tx, created, history, domain.CreatedEventName, nil); err != nil {
		return domain.Opportunity{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Opportunity{}, false, fmt.Errorf("фиксация создания возможности: %w", err)
	}
	return created, true, nil
}

func (repository *PostgresRepository) Detail(
	ctx context.Context,
	tenantID, opportunityID string,
) (domain.Detail, bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || opportunityID == "" {
		return domain.Detail{}, false, domain.ErrInvalid
	}
	opportunity, found, err := opportunityByID(ctx, repository.pool, tenantID, opportunityID, false)
	if err != nil || !found {
		return domain.Detail{}, found, err
	}
	history, err := historyByOpportunity(ctx, repository.pool, tenantID, opportunityID)
	if err != nil {
		return domain.Detail{}, false, err
	}
	return domain.Detail{Opportunity: opportunity, History: history}, true, nil
}

func (repository *PostgresRepository) ActiveByConversation(
	ctx context.Context,
	tenantID, conversationID string,
) (domain.Opportunity, bool, error) {
	if repository == nil || repository.pool == nil || tenantID == "" || conversationID == "" {
		return domain.Opportunity{}, false, domain.ErrInvalid
	}
	opportunity, found, err := scanOpportunityFound(repository.pool.QueryRow(ctx, `
		SELECT id, tenant_id, conversation_id, service_id, stage,
		       COALESCE(estimated_amount::text, ''), COALESCE(estimated_amount_confidence::text, ''),
		       currency, opened_at, closed_at, created_at, updated_at
		FROM opportunities
		WHERE tenant_id = $1 AND conversation_id = $2
		  AND stage NOT IN ('WON', 'LOST', 'ARCHIVED')`, tenantID, conversationID))
	if err != nil {
		return domain.Opportunity{}, false, mapReadError("чтение активной возможности переписки", err)
	}
	return opportunity, found, nil
}

func (repository *PostgresRepository) UpdateEstimate(ctx context.Context, update domain.EstimateUpdate) (bool, error) {
	if repository == nil || repository.pool == nil || update.TenantID == "" || update.OpportunityID == "" ||
		len(update.Currency) != 3 || update.At.IsZero() {
		return false, domain.ErrInvalid
	}
	result, err := repository.pool.Exec(ctx, `
		UPDATE opportunities
		SET estimated_amount = $3::numeric, estimated_amount_confidence = $4::numeric, updated_at = $5
		WHERE tenant_id = $1 AND id = $2
		  AND currency = $6
		  AND stage NOT IN ('WON', 'LOST', 'ARCHIVED')
		  AND (estimated_amount IS NULL OR estimated_amount_confidence IS NULL
		       OR estimated_amount_confidence <= $4::numeric)`,
		update.TenantID, update.OpportunityID, update.Amount.String(), update.Confidence.String(),
		update.At.UTC(), update.Currency)
	if err != nil {
		return false, mapPostgresError("обновление оценки выручки", err)
	}
	return result.RowsAffected() == 1, nil
}

// Transition блокирует агрегат, проверяет переход по актуальному этапу и в
// одной транзакции обновляет возможность вместе с добавлением истории.
func (repository *PostgresRepository) Transition(
	ctx context.Context,
	command domain.TransitionCommand,
) (domain.Opportunity, bool, error) {
	if repository == nil || repository.pool == nil || command.Validate() != nil {
		return domain.Opportunity{}, false, domain.ErrInvalid
	}
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return domain.Opportunity{}, false, fmt.Errorf("начало перехода этапа: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	opportunity, found, err := opportunityByID(ctx, tx, command.TenantID, command.OpportunityID, true)
	if err != nil {
		return domain.Opportunity{}, false, err
	}
	if !found {
		return domain.Opportunity{}, false, domain.ErrNotFound
	}
	if opportunity.Stage == command.ToStage {
		if err := tx.Commit(ctx); err != nil {
			return domain.Opportunity{}, false, fmt.Errorf("фиксация повторного перехода: %w", err)
		}
		return opportunity, false, nil
	}
	if !opportunity.Stage.CanTransitionTo(command.ToStage) {
		return domain.Opportunity{}, false, domain.ErrInvalidTransition
	}
	from := opportunity.Stage
	history, err := domain.NewHistory(
		command.HistoryID, command.TenantID, command.OpportunityID, &from, command.ToStage,
		command.Source, command.Confidence, command.AIRunID, command.ActorUserID, command.At,
	)
	if err != nil {
		return domain.Opportunity{}, false, err
	}
	if command.ToStage.Active() {
		opportunity.ClosedAt = nil
	} else if opportunity.ClosedAt == nil {
		closedAt := command.At.UTC()
		opportunity.ClosedAt = &closedAt
	}
	opportunity.Stage = command.ToStage
	opportunity.UpdatedAt = command.At.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE opportunities
		SET stage = $3, closed_at = $4, updated_at = $5
		WHERE tenant_id = $1 AND id = $2`,
		command.TenantID, command.OpportunityID, opportunity.Stage, opportunity.ClosedAt, opportunity.UpdatedAt,
	); err != nil {
		return domain.Opportunity{}, false, mapPostgresError("обновление этапа возможности", err)
	}
	if err := insertHistory(ctx, tx, history); err != nil {
		return domain.Opportunity{}, false, err
	}
	if err := repository.appendEvent(ctx, tx, opportunity, history, domain.StageChangedEventName, &from); err != nil {
		return domain.Opportunity{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Opportunity{}, false, fmt.Errorf("фиксация перехода этапа: %w", err)
	}
	return opportunity, true, nil
}

type opportunityEventData struct {
	ConversationID string        `json:"conversationId"`
	FromStage      *domain.Stage `json:"fromStage,omitempty"`
	ToStage        domain.Stage  `json:"toStage"`
}

func (repository *PostgresRepository) appendEvent(
	ctx context.Context,
	tx pgx.Tx,
	opportunity domain.Opportunity,
	history domain.StageHistory,
	eventType string,
	fromStage *domain.Stage,
) error {
	data, err := json.Marshal(opportunityEventData{
		ConversationID: opportunity.ConversationID,
		FromStage:      fromStage,
		ToStage:        opportunity.Stage,
	})
	if err != nil {
		return fmt.Errorf("подготовка события возможности: %w", err)
	}
	event, err := eventsdomain.NewEvent(
		history.ID, eventType, 1, opportunity.TenantID, "opportunity", opportunity.ID,
		history.ID, data, history.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("проверка события возможности: %w", err)
	}
	if repository.events == nil {
		return errors.New("исходящий журнал возможности не настроен")
	}
	if _, _, err := repository.events.AppendTx(ctx, tx, event); err != nil {
		return fmt.Errorf("добавление события возможности: %w", err)
	}
	return nil
}

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func insertOpportunity(ctx context.Context, tx pgx.Tx, opportunity domain.Opportunity) (domain.Opportunity, bool, error) {
	created, err := scanOpportunity(tx.QueryRow(ctx, `
		INSERT INTO opportunities(
			id, tenant_id, conversation_id, service_id, stage, estimated_amount,
			estimated_amount_confidence, currency, opened_at, closed_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6::numeric, $7::numeric, $8, $9, $10, $11, $12)
		ON CONFLICT (tenant_id, conversation_id)
		WHERE stage NOT IN ('WON', 'LOST', 'ARCHIVED')
		DO NOTHING
		RETURNING id, tenant_id, conversation_id, service_id, stage,
		          COALESCE(estimated_amount::text, ''), COALESCE(estimated_amount_confidence::text, ''),
		          currency, opened_at, closed_at, created_at, updated_at`,
		opportunity.ID, opportunity.TenantID, opportunity.ConversationID, opportunity.ServiceID, opportunity.Stage,
		databaseRevenue(opportunity.EstimatedAmount), databaseConfidence(opportunity.EstimatedAmountConfidence),
		opportunity.Currency, opportunity.OpenedAt, opportunity.ClosedAt, opportunity.CreatedAt, opportunity.UpdatedAt,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Opportunity{}, false, nil
	}
	if err != nil {
		return domain.Opportunity{}, false, mapPostgresError("создание возможности", err)
	}
	return created, true, nil
}

func activeByConversation(
	ctx context.Context,
	queryer queryRower,
	tenantID, conversationID string,
) (domain.Opportunity, bool, error) {
	opportunity, found, err := scanOpportunityFound(queryer.QueryRow(ctx, `
		SELECT id, tenant_id, conversation_id, service_id, stage,
		       COALESCE(estimated_amount::text, ''), COALESCE(estimated_amount_confidence::text, ''),
		       currency, opened_at, closed_at, created_at, updated_at
		FROM opportunities
		WHERE tenant_id = $1 AND conversation_id = $2
		  AND stage NOT IN ('WON', 'LOST', 'ARCHIVED')
		FOR UPDATE`, tenantID, conversationID))
	if err != nil {
		return domain.Opportunity{}, false, mapReadError("чтение активной возможности", err)
	}
	return opportunity, found, nil
}

func opportunityByID(
	ctx context.Context,
	queryer queryRower,
	tenantID, opportunityID string,
	lock bool,
) (domain.Opportunity, bool, error) {
	query := `
		SELECT id, tenant_id, conversation_id, service_id, stage,
		       COALESCE(estimated_amount::text, ''), COALESCE(estimated_amount_confidence::text, ''),
		       currency, opened_at, closed_at, created_at, updated_at
		FROM opportunities WHERE tenant_id = $1 AND id = $2`
	if lock {
		query += " FOR UPDATE"
	}
	opportunity, found, err := scanOpportunityFound(queryer.QueryRow(ctx, query, tenantID, opportunityID))
	if err != nil {
		return domain.Opportunity{}, false, mapReadError("чтение возможности", err)
	}
	return opportunity, found, nil
}

func insertHistory(ctx context.Context, tx pgx.Tx, history domain.StageHistory) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO opportunity_stage_history(
			id, tenant_id, opportunity_id, from_stage, to_stage, source,
			confidence, ai_run_id, actor_user_id, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9, $10)`,
		history.ID, history.TenantID, history.OpportunityID, history.FromStage, history.ToStage, history.Source,
		databaseConfidence(history.Confidence), history.AIRunID, history.ActorUserID, history.CreatedAt,
	)
	if err != nil {
		return mapPostgresError("добавление истории этапа", err)
	}
	return nil
}

func historyByOpportunity(
	ctx context.Context,
	pool *pgxpool.Pool,
	tenantID, opportunityID string,
) ([]domain.StageHistory, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, tenant_id, opportunity_id, from_stage, to_stage, source,
		       COALESCE(confidence::text, ''), ai_run_id, actor_user_id, created_at
		FROM opportunity_stage_history
		WHERE tenant_id = $1 AND opportunity_id = $2
		ORDER BY created_at, id`, tenantID, opportunityID)
	if err != nil {
		return nil, fmt.Errorf("чтение истории этапов: %w", err)
	}
	defer rows.Close()
	history := make([]domain.StageHistory, 0)
	for rows.Next() {
		var item domain.StageHistory
		var confidence string
		if err := rows.Scan(
			&item.ID, &item.TenantID, &item.OpportunityID, &item.FromStage, &item.ToStage,
			&item.Source, &confidence, &item.AIRunID, &item.ActorUserID, &item.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("разбор истории этапа: %w", err)
		}
		if confidence != "" {
			parsed, parseErr := domain.ParseConfidence(confidence)
			if parseErr != nil {
				return nil, fmt.Errorf("разбор уверенности этапа: %w", parseErr)
			}
			item.Confidence = &parsed
		}
		if item.Validate() != nil {
			return nil, fmt.Errorf("проверка истории этапа: %w", domain.ErrInvalid)
		}
		history = append(history, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("обход истории этапов: %w", err)
	}
	return history, nil
}

type rowScanner interface{ Scan(...any) error }

func scanOpportunityFound(row rowScanner) (domain.Opportunity, bool, error) {
	opportunity, err := scanOpportunity(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Opportunity{}, false, nil
	}
	if err != nil {
		return domain.Opportunity{}, false, err
	}
	return opportunity, true, nil
}

func scanOpportunity(row rowScanner) (domain.Opportunity, error) {
	var opportunity domain.Opportunity
	var amount, confidence string
	if err := row.Scan(
		&opportunity.ID, &opportunity.TenantID, &opportunity.ConversationID, &opportunity.ServiceID,
		&opportunity.Stage, &amount, &confidence, &opportunity.Currency, &opportunity.OpenedAt,
		&opportunity.ClosedAt, &opportunity.CreatedAt, &opportunity.UpdatedAt,
	); err != nil {
		return domain.Opportunity{}, err
	}
	if amount != "" {
		parsed, err := domain.ParsePotentialRevenue(amount)
		if err != nil {
			return domain.Opportunity{}, fmt.Errorf("разбор денежного потенциала: %w", err)
		}
		opportunity.EstimatedAmount = &parsed
	}
	if confidence != "" {
		parsed, err := domain.ParseConfidence(confidence)
		if err != nil {
			return domain.Opportunity{}, fmt.Errorf("разбор уверенности суммы: %w", err)
		}
		opportunity.EstimatedAmountConfidence = &parsed
	}
	if opportunity.Validate() != nil {
		return domain.Opportunity{}, fmt.Errorf("проверка возможности из базы: %w", domain.ErrInvalid)
	}
	return opportunity, nil
}

func databaseRevenue(revenue *domain.PotentialRevenue) any {
	if revenue == nil {
		return nil
	}
	return revenue.String()
}

func databaseConfidence(confidence *domain.Confidence) any {
	if confidence == nil {
		return nil
	}
	return confidence.String()
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

func mapReadError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		return mapPostgresError(operation, err)
	}
	return err
}
