package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lidradar/backend/internal/ai/application"
)

// PostgresAnalysisJobBuilder строит свежую ограниченную инструкцию из одного
// согласованного снимка repeatable read. Идентификатор организации никогда не
// попадает в контекст модели.
type PostgresAnalysisJobBuilder struct {
	pool         *pgxpool.Pool
	modelVersion string
}

func NewPostgresAnalysisJobBuilder(pool *pgxpool.Pool, modelVersion string) *PostgresAnalysisJobBuilder {
	return &PostgresAnalysisJobBuilder{pool: pool, modelVersion: modelVersion}
}

func (builder *PostgresAnalysisJobBuilder) BuildAnalysisJob(
	ctx context.Context,
	tenantID, conversationID string,
) (application.EnqueueCommand, error) {
	if builder == nil || builder.pool == nil || tenantID == "" || conversationID == "" {
		return application.EnqueueCommand{}, application.ErrInvalid
	}
	tx, err := builder.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return application.EnqueueCommand{}, fmt.Errorf("начало построения AI-контекста: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var organizationName, defaultTimezone, defaultCurrency string
	var locationName, locationTimezone, existingSummary *string
	var revision int64
	err = tx.QueryRow(ctx, `
		SELECT organization.name, organization.default_timezone, organization.default_currency,
		       location.name, location.timezone, conversation.revision, summary.summary_text
		FROM conversations AS conversation
		JOIN organizations AS organization ON organization.id = conversation.tenant_id
		LEFT JOIN locations AS location
		  ON location.tenant_id = conversation.tenant_id AND location.id = conversation.location_id
		LEFT JOIN conversation_summaries AS summary
		  ON summary.tenant_id = conversation.tenant_id AND summary.conversation_id = conversation.id
		WHERE conversation.tenant_id = $1 AND conversation.id = $2`, tenantID, conversationID).
		Scan(&organizationName, &defaultTimezone, &defaultCurrency, &locationName, &locationTimezone, &revision, &existingSummary)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.EnqueueCommand{}, application.ErrNotFound
	}
	if err != nil {
		return application.EnqueueCommand{}, mapAIStoreError("чтение переписки для AI-контекста", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT message.id::text, message.direction, message.text
		FROM messages AS message
		WHERE message.tenant_id = $1 AND message.conversation_id = $2
		  AND message.direction IN ('INCOMING', 'OUTGOING')
		  AND message.provider_deleted_at IS NULL
		  AND message.text IS NOT NULL AND btrim(message.text) <> ''
		ORDER BY message.sent_at DESC, message.id DESC
		LIMIT $3`, tenantID, conversationID, application.MaxContextMessages)
	if err != nil {
		return application.EnqueueCommand{}, mapAIStoreError("чтение сообщений для AI-контекста", err)
	}
	messagesDescending := make([]application.ContextMessage, 0, application.MaxContextMessages)
	for rows.Next() {
		var message application.ContextMessage
		if err := rows.Scan(&message.ID, &message.Direction, &message.Body); err != nil {
			rows.Close()
			return application.EnqueueCommand{}, mapAIStoreError("чтение сообщения AI-контекста", err)
		}
		messagesDescending = append(messagesDescending, message)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return application.EnqueueCommand{}, mapAIStoreError("обход сообщений AI-контекста", err)
	}
	rows.Close()
	if len(messagesDescending) == 0 {
		return application.EnqueueCommand{}, application.ErrInvalid
	}
	messages := make([]application.ContextMessage, len(messagesDescending))
	for index := range messagesDescending {
		messages[len(messagesDescending)-1-index] = messagesDescending[index]
	}
	companyContext, err := json.Marshal(map[string]any{
		"organizationName": organizationName,
		"defaultTimezone":  defaultTimezone,
		"defaultCurrency":  defaultCurrency,
		"locationName":     locationName,
		"locationTimezone": locationTimezone,
	})
	if err != nil {
		return application.EnqueueCommand{}, fmt.Errorf("кодирование контекста организации: %w", err)
	}
	summary := ""
	if existingSummary != nil {
		summary = *existingSummary
	}
	request, err := application.BuildAnalysisContext(application.ConversationContext{
		TenantID: tenantID, ConversationID: conversationID,
		CompanyContext: string(companyContext), ExistingSummary: summary,
		Revision: revision, Messages: messages,
	})
	if err != nil {
		return application.EnqueueCommand{}, err
	}
	prompt, err := application.EncodeAnalysisRequest(request)
	if err != nil {
		return application.EnqueueCommand{}, fmt.Errorf("кодирование AI-контекста: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return application.EnqueueCommand{}, fmt.Errorf("фиксация снимка AI-контекста: %w", err)
	}
	return application.EnqueueCommand{
		TenantID: tenantID, ConversationID: conversationID, Prompt: prompt,
		BaseConversationRevision: request.BaseConversationRevision,
		AnalysisThroughMessageID: request.AnalysisThroughMessageID,
		ModelVersion:             builder.modelVersion, PromptVersion: request.PromptVersion,
		SchemaVersion: request.SchemaVersion,
	}, nil
}
