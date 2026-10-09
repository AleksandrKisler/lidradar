package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
)

// mergeStoredAgreements runs under the same transaction and conversation lock
// as Finalize. Every carried entry is checked against its own immutable job
// context and current canonical message state.
func mergeStoredAgreements(ctx context.Context, tx pgx.Tx, summary *domain.ConversationSummary, currentPrompt string) error {
	var priorJSON []byte
	var priorRunID string
	err := tx.QueryRow(ctx, `
		SELECT agreements, ai_run_id::text
		FROM conversation_summaries
		WHERE tenant_id = $1 AND conversation_id = $2
		FOR UPDATE`, summary.TenantID, summary.ConversationID).Scan(&priorJSON, &priorRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		summary.Agreements = application.MergeAgreementProjection(nil, summary.Agreements, nil, "", summary.RunID, currentPrompt)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read previous agreement projection: %w", err)
	}
	var old []domain.Agreement
	if err := json.Unmarshal(priorJSON, &old); err != nil {
		return fmt.Errorf("decode previous agreement projection: %w", err)
	}
	valid := make([]bool, len(old))
	if len(old) == 0 {
		summary.Agreements = application.MergeAgreementProjection(old, summary.Agreements, valid, priorRunID, summary.RunID, currentPrompt)
		return nil
	}
	ids := make([]string, 0, len(old)*2)
	for _, agreement := range old {
		ids = append(ids, agreement.TriggerMessageID)
		ids = append(ids, agreement.EvidenceMessageIDs...)
	}
	canonical := make(map[string]application.ContextMessage, len(ids))
	rows, err := tx.Query(ctx, `
		SELECT id::text, direction, text
		FROM messages
		WHERE tenant_id = $1 AND conversation_id = $2
		  AND id::text = ANY($3::text[])
		  AND provider_deleted_at IS NULL AND text IS NOT NULL`, summary.TenantID, summary.ConversationID, ids)
	if err != nil {
		return fmt.Errorf("read canonical agreement evidence: %w", err)
	}
	for rows.Next() {
		var message application.ContextMessage
		if err := rows.Scan(&message.ID, &message.Direction, &message.Body); err != nil {
			rows.Close()
			return fmt.Errorf("scan canonical agreement evidence: %w", err)
		}
		canonical[message.ID] = message
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("read canonical agreement evidence: %w", err)
	}
	prompts := make(map[string]map[string]application.ContextMessage)
	for i, agreement := range old {
		source := agreement.SourceRunID
		if source == "" {
			source = priorRunID
		}
		original, loaded := prompts[source]
		if !loaded {
			original = map[string]application.ContextMessage{}
			var prompt string
			err := tx.QueryRow(ctx, `
				SELECT COALESCE(job.payload ->> 'prompt', '')
				FROM ai_runs AS run
				JOIN ai_jobs AS job ON job.id = run.job_id
				  AND job.tenant_id = run.tenant_id AND job.entity_id = run.entity_id
				WHERE run.id::text = $1 AND run.tenant_id = $2
				  AND run.entity_id = $3 AND run.status = 'SUCCEEDED'
				  AND run.application_status = 'APPLIED'
				  AND run.schema_version = $4`, source, summary.TenantID, summary.ConversationID, application.AnalysisSchemaV2).Scan(&prompt)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read original agreement context: %w", err)
			}
			if err == nil {
				var request application.AnalyzeConversationRequestV1
				if json.Unmarshal([]byte(prompt), &request) == nil && request.SchemaVersion == application.AnalysisSchemaV2 && request.ConversationID == summary.ConversationID {
					for _, message := range request.Messages {
						original[message.ID] = message
					}
				}
			}
			prompts[source] = original
		}
		valid[i] = len(agreement.EvidenceMessageIDs) > 0
		for _, id := range append([]string{agreement.TriggerMessageID}, agreement.EvidenceMessageIDs...) {
			was, inPrompt := original[id]
			now, exists := canonical[id]
			if !inPrompt || !exists || was.Direction != now.Direction || was.Body != now.Body {
				valid[i] = false
				break
			}
		}
	}
	summary.Agreements = application.MergeAgreementProjection(old, summary.Agreements, valid, priorRunID, summary.RunID, currentPrompt)
	return nil
}
