package infrastructure_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
	"lidradar/backend/internal/ai/infrastructure"
	"lidradar/backend/internal/ai/transport"
	"lidradar/backend/platform/ids"
)

func TestCloudClientRunsSignedLifecycleWithoutPersistingLocally(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store := infrastructure.NewTestMemoryStore()
	service := application.NewService(store, ids.Generator{}, func() time.Time { return now }, application.DefaultLease)
	secret := "secret-with-at-least-32-characters"
	node, err := service.RegisterNode(ctx, "tenant", "home", secret)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(transport.Handler{Service: service})
	defer server.Close()
	client := infrastructure.CloudClient{
		BaseURL: server.URL, NodeID: node.ID, Secret: secret,
		Client: server.Client(), IDs: ids.Generator{}, Now: func() time.Time { return now },
	}
	if err := client.Heartbeat(ctx, domain.NodeReady, "test-model", 1); err != nil {
		t.Fatal(err)
	}
	job, err := service.Enqueue(ctx, application.EnqueueCommand{
		TenantID: "tenant", ConversationID: "conversation",
		Prompt:                   `{"analysisThroughMessageId":"message"}`,
		BaseConversationRevision: 1, AnalysisThroughMessageID: "message",
		ModelVersion: "test-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := client.Claim(ctx)
	if err != nil || !found || claimed.ID != job.ID {
		t.Fatalf("claim = %#v, %v, %v", claimed, found, err)
	}
	runID, err := client.Started(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	output := `{"schemaVersion":"analyze-conversation.v1","analysisThroughMessageId":"message","summary":"Проверка клиента.","facts":[]}`
	if err := client.Complete(ctx, job.ID, runID, output); err != nil {
		t.Fatal(err)
	}
	summary, ok := store.Summary("tenant", "conversation")
	if !ok || summary.Text != "Проверка клиента." {
		t.Fatalf("summary = %#v, %v", summary, ok)
	}
}
