package transport_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/domain"
	"lidradar/backend/internal/ai/infrastructure"
	"lidradar/backend/internal/ai/transport"
	"lidradar/backend/platform/ids"
)

func TestClaimAPIAuthenticatesSignedNode(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	service := application.NewService(infrastructure.NewTestMemoryStore(), ids.Generator{}, func() time.Time { return now }, 0).WithAnalysisDebounce(0)
	node, err := service.RegisterNode(ctx, "t", "home", "secret-with-at-least-32-characters")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Heartbeat(ctx, node.ID, "secret-with-at-least-32-characters", application.HeartbeatCommand{
		Status: domain.NodeReady, ModelVersion: "test-model", AvailableSlots: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Enqueue(ctx, application.EnqueueCommand{
		TenantID: "t", ConversationID: "c", Prompt: "p",
		BaseConversationRevision: 1, AnalysisThroughMessageID: "m",
		ModelVersion: "test-model",
	}); err != nil {
		t.Fatal(err)
	}
	handler := transport.Handler{Service: service}
	request := signedRequest(t, node.ID, "secret-with-at-least-32-characters", now, "/internal/v1/ai/jobs/claim", nil)
	writer := httptest.NewRecorder()
	handler.ServeHTTP(writer, request)
	if writer.Code != http.StatusOK {
		t.Fatalf("status %d: %s", writer.Code, writer.Body.String())
	}

	badBody := []byte(`{"status":"READY","modelVersion":"test-model","availableSlots":1}`)
	bad := signedRequest(t, node.ID, "wrong-secret-with-at-least-32-chars", now, "/internal/v1/ai/nodes/heartbeat", badBody)
	writer = httptest.NewRecorder()
	handler.ServeHTTP(writer, bad)
	if writer.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", writer.Code)
	}
}

func TestHandlerWorksWhenMountedAtInternalPrefix(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	service := application.NewService(infrastructure.NewTestMemoryStore(), ids.Generator{}, func() time.Time { return now }, 0).WithAnalysisDebounce(0)
	secret := "secret-with-at-least-32-characters"
	node, err := service.RegisterNode(ctx, "tenant", "home", secret)
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Mount("/internal/v1/ai", transport.Handler{Service: service})
	body := []byte(`{"status":"READY","modelVersion":"test-model","availableSlots":1}`)
	request := signedRequest(t, node.ID, secret, now, "/internal/v1/ai/nodes/heartbeat", body)
	writer := httptest.NewRecorder()
	router.ServeHTTP(writer, request)
	if writer.Code != http.StatusNoContent {
		t.Fatalf("mounted status = %d: %s", writer.Code, writer.Body.String())
	}
}

func TestSignedRequestCannotBeReplayedOrMutated(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	service := application.NewService(infrastructure.NewTestMemoryStore(), ids.Generator{}, func() time.Time { return now }, 0).WithAnalysisDebounce(0)
	node, err := service.RegisterNode(ctx, "tenant", "home", "secret-with-at-least-32-characters")
	if err != nil {
		t.Fatal(err)
	}
	handler := transport.Handler{Service: service}
	body := []byte(`{"status":"READY","modelVersion":"test-model","availableSlots":1}`)
	request := signedRequest(t, node.ID, "secret-with-at-least-32-characters", now, "/internal/v1/ai/nodes/heartbeat", body)
	requestID := request.Header.Get(application.RequestIDHeader)
	signature := request.Header.Get(application.SignatureHeader)
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, request)
	if first.Code != http.StatusNoContent {
		t.Fatalf("first status = %d: %s", first.Code, first.Body.String())
	}
	replayed := signedRequest(t, node.ID, "secret-with-at-least-32-characters", now, "/internal/v1/ai/nodes/heartbeat", body)
	replayed.Header.Set(application.RequestIDHeader, requestID)
	replayed.Header.Set(application.SignatureHeader, signature)
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, replayed)
	if second.Code != http.StatusUnauthorized {
		t.Fatalf("replay status = %d", second.Code)
	}

	mutated := signedRequest(t, node.ID, "secret-with-at-least-32-characters", now, "/internal/v1/ai/nodes/heartbeat", []byte(`{}`))
	mutated.Header.Set(application.SignatureHeader, signature)
	third := httptest.NewRecorder()
	handler.ServeHTTP(third, mutated)
	if third.Code != http.StatusUnauthorized {
		t.Fatalf("mutated status = %d", third.Code)
	}
}

func signedRequest(t *testing.T, nodeID, secret string, now time.Time, path string, body []byte) *http.Request {
	t.Helper()
	requestID, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	timestamp := now.UTC().Format(time.RFC3339Nano)
	request.Header.Set("Authorization", "Bearer "+secret)
	request.Header.Set(application.NodeIDHeader, nodeID)
	request.Header.Set(application.TimestampHeader, timestamp)
	request.Header.Set(application.RequestIDHeader, requestID)
	request.Header.Set(application.SignatureHeader, application.SignMachineRequest(secret, http.MethodPost, path, timestamp, requestID, body))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}
