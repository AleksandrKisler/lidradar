package infrastructure_test

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgxpool"
	aiapplication "lidradar/backend/internal/ai/application"
	aiinfrastructure "lidradar/backend/internal/ai/infrastructure"
)

// Legacy suites exercise persisted v1 jobs even after new jobs default to v2.
// New v2 storage and agreement behavior have separate coverage.
type legacyJobBuilder struct {
	current *aiinfrastructure.PostgresAnalysisJobBuilder
}

func legacyAnalysisJobBuilder(pool *pgxpool.Pool, model string) legacyJobBuilder {
	return legacyJobBuilder{aiinfrastructure.NewPostgresAnalysisJobBuilder(pool, model)}
}
func (builder legacyJobBuilder) BuildAnalysisJob(ctx context.Context, tenant, conversation string) (aiapplication.EnqueueCommand, error) {
	command, err := builder.current.BuildAnalysisJob(ctx, tenant, conversation)
	if err != nil {
		return command, err
	}
	var request aiapplication.AnalyzeConversationRequestV1
	if err = json.Unmarshal([]byte(command.Prompt), &request); err != nil {
		return command, err
	}
	request.SchemaVersion = aiapplication.AnalysisSchemaV1
	request.PromptVersion = aiapplication.AnalysisPromptV6
	command.SchemaVersion = request.SchemaVersion
	command.PromptVersion = request.PromptVersion
	command.Prompt, err = aiapplication.EncodeAnalysisRequest(request)
	return command, err
}
