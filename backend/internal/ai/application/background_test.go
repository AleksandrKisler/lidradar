package application_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
	"lidradar/backend/internal/ai/infrastructure"
	eventsdomain "lidradar/backend/internal/events/domain"
)

type backgroundBuilder struct{ command application.EnqueueCommand }

type sequenceIDs struct{ next int }

func (ids *sequenceIDs) NewID() (string, error) {
	ids.next++
	return "generated-" + string(rune('0'+ids.next)), nil
}

func (builder backgroundBuilder) BuildAnalysisJob(context.Context, string, string) (application.EnqueueCommand, error) {
	return builder.command, nil
}

func TestConversationChangedEnqueuesAIAnalysisOnce(t *testing.T) {
	store := infrastructure.NewTestMemoryStore()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	service := application.NewService(store, &sequenceIDs{}, func() time.Time { return now }, time.Minute)
	command := application.EnqueueCommand{
		TenantID: "tenant", ConversationID: "conversation", Prompt: `{"task":"ANALYZE_CONVERSATION"}`,
		BaseConversationRevision: 3, AnalysisThroughMessageID: "message",
		ModelVersion: "model", PromptVersion: application.CurrentAnalysisPrompt,
		SchemaVersion: application.AnalysisSchemaV1,
	}
	handler := application.ConversationChangedEventHandler(service, backgroundBuilder{command: command})
	data, _ := json.Marshal(map[string]any{"conversationId": "conversation", "messageId": "message", "revision": 3})
	event, err := eventsdomain.NewEvent(
		"event", "conversation.changed", 1, "tenant", "conversation", "conversation", "trace", data, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if err := handler(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	job, ok := store.Job("generated-1")
	if !ok || job.Status != domain.JobPending || job.BaseConversationRevision != 3 {
		t.Fatalf("AI-задание = %#v, найдено=%v", job, ok)
	}
	if _, duplicate := store.Job("generated-2"); duplicate {
		t.Fatal("повтор события создал второе AI-задание")
	}
}
