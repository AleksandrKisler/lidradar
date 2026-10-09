package infrastructure

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"lidradar/backend/internal/opportunity/domain"
)

// CreateFromAnalysis creates an unpriced opportunity only from a current,
// strong incoming commercial intent newer than the last closed opportunity.
func (repository *PostgresRepository) CreateFromAnalysis(ctx context.Context, command domain.SemanticCreation) (domain.Opportunity, bool, error) {
	if repository == nil || repository.pool == nil || command.TenantID == "" || command.ConversationID == "" || command.RunID == "" || command.ThroughMessageID == "" || command.OpportunityID == "" || command.HistoryID == "" || command.Revision < 1 || command.At.IsZero() {
		return domain.Opportunity{}, false, domain.ErrInvalid
	}
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return domain.Opportunity{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var revision int64
	if err = tx.QueryRow(ctx, `SELECT revision FROM conversations WHERE tenant_id=$1 AND id=$2 AND status='ACTIVE' FOR UPDATE`, command.TenantID, command.ConversationID).Scan(&revision); errors.Is(err, pgx.ErrNoRows) {
		return domain.Opportunity{}, false, nil
	} else if err != nil {
		return domain.Opportunity{}, false, err
	}
	if revision != command.Revision {
		return domain.Opportunity{}, false, nil
	}
	existing, found, err := activeByConversation(ctx, tx, command.TenantID, command.ConversationID)
	if err != nil || found {
		return existing, found, err
	}
	var currency, rawConfidence string
	err = tx.QueryRow(ctx, `
 SELECT organization.default_currency, to_char((fact.value->>'confidence')::numeric, 'FM0.000')
 FROM conversation_summaries summary
 JOIN organizations organization ON organization.id=summary.tenant_id
 CROSS JOIN LATERAL jsonb_array_elements(summary.semantic_facts) fact(value)
 WHERE summary.tenant_id=$1 AND summary.conversation_id=$2 AND summary.ai_run_id=$3
 AND summary.base_conversation_revision=$4 AND summary.analysis_through_message_id=$5
 AND summary.analysis_through_message_id=(
   SELECT id FROM messages WHERE tenant_id=$1 AND conversation_id=$2
   AND direction IN ('INCOMING','OUTGOING') AND provider_deleted_at IS NULL
   AND text IS NOT NULL AND btrim(text)<>'' ORDER BY sent_at DESC,id DESC LIMIT 1)
 AND fact.value->>'type' IN ('BOOKING_INTENT','PURCHASE_INTENT')
 AND fact.value->>'value'='true' AND fact.value->>'trusted'='true'
 AND (fact.value->>'confidence')::numeric >= 0.85
 AND EXISTS (
  SELECT 1 FROM jsonb_array_elements_text(fact.value->'evidenceMessageIds') evidence(id)
  JOIN messages message ON message.tenant_id=summary.tenant_id AND message.conversation_id=summary.conversation_id AND message.id::text=evidence.id
  WHERE message.direction='INCOMING' AND message.provider_deleted_at IS NULL
  AND message.sent_at > COALESCE((SELECT max(closed_at) FROM opportunities WHERE tenant_id=$1 AND conversation_id=$2), '-infinity'::timestamptz)
 ) ORDER BY (fact.value->>'confidence')::numeric DESC LIMIT 1`, command.TenantID, command.ConversationID, command.RunID, command.Revision, command.ThroughMessageID).Scan(&currency, &rawConfidence)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Opportunity{}, false, nil
	}
	if err != nil {
		return domain.Opportunity{}, false, fmt.Errorf("проверка коммерческого намерения: %w", err)
	}
	confidence, err := domain.ParseConfidence(rawConfidence)
	if err != nil {
		return domain.Opportunity{}, false, err
	}
	opportunity, err := domain.NewOpportunity(command.OpportunityID, command.TenantID, command.ConversationID, nil, nil, nil, currency, command.At)
	if err != nil {
		return domain.Opportunity{}, false, err
	}
	history, err := domain.NewHistory(command.HistoryID, command.TenantID, opportunity.ID, nil, domain.StageNew, domain.SourceAI, &confidence, &command.RunID, nil, command.At)
	if err != nil {
		return domain.Opportunity{}, false, err
	}
	created, inserted, err := insertOpportunity(ctx, tx, opportunity)
	if err != nil {
		return domain.Opportunity{}, false, err
	}
	if !inserted {
		return activeByConversation(ctx, tx, command.TenantID, command.ConversationID)
	}
	if err = insertHistory(ctx, tx, history); err != nil {
		return domain.Opportunity{}, false, err
	}
	if err = repository.appendEvent(ctx, tx, created, history, domain.CreatedEventName, nil); err != nil {
		return domain.Opportunity{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Opportunity{}, false, err
	}
	return created, true, nil
}

func (repository *PostgresRepository) EnrichService(ctx context.Context, update domain.ServiceEnrichment) (domain.Opportunity, bool, error) {
	if repository == nil || repository.pool == nil || update.TenantID == "" || update.OpportunityID == "" || update.ServiceID == "" || update.At.IsZero() || (update.Amount == nil && update.Confidence != nil) {
		return domain.Opportunity{}, false, domain.ErrInvalid
	}
	var amount, confidence *string
	if update.Amount != nil {
		value := update.Amount.String()
		amount = &value
	}
	if update.Confidence != nil {
		value := update.Confidence.String()
		confidence = &value
	}
	result, err := repository.pool.Exec(ctx, `UPDATE opportunities SET service_id=$3,
 estimated_amount=COALESCE(estimated_amount,$4::numeric),
 estimated_amount_confidence=CASE WHEN estimated_amount IS NULL THEN $5::numeric ELSE estimated_amount_confidence END,
 updated_at=$6 WHERE tenant_id=$1 AND id=$2 AND service_id IS NULL AND currency=$7
 AND stage NOT IN ('WON','LOST','ARCHIVED')
 AND EXISTS (SELECT 1 FROM service_catalog_items WHERE tenant_id=$1 AND id=$3 AND active)`,
		update.TenantID, update.OpportunityID, update.ServiceID, amount, confidence, update.At, update.Currency)
	if err != nil {
		return domain.Opportunity{}, false, err
	}
	opportunity, found, err := opportunityByID(ctx, repository.pool, update.TenantID, update.OpportunityID, false)
	return opportunity, found && result.RowsAffected() == 1, err
}
