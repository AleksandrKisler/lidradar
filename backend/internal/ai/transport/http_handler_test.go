package transport_test

import (
	"context"
	"fmt"
	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/infrastructure"
	"lidradar/backend/internal/ai/transport"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type ids struct{ n int }

func (i *ids) NewID() string { i.n++; return fmt.Sprint(i.n) }
func TestClaimAPIAuthenticatesNode(t *testing.T) {
	ctx := context.Background()
	s := application.NewService(infrastructure.NewMemoryStore(), &ids{}, func() time.Time { return time.Now().UTC() }, 0)
	n, err := s.RegisterNode(ctx, "home", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Enqueue(ctx, application.EnqueueCommand{TenantID: "t", ConversationID: "c", Prompt: "p", BaseConversationRevision: 1, AnalysisThroughMessageID: "m"}); err != nil {
		t.Fatal(err)
	}
	h := transport.Handler{Service: s}
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/ai/jobs/claim", nil)
	req.Header.Set("X-AI-Node-ID", n.ID)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	bad := httptest.NewRequest(http.MethodPost, "/internal/v1/ai/nodes/heartbeat", nil)
	bad.Header.Set("X-AI-Node-ID", n.ID)
	bad.Header.Set("Authorization", "Bearer wrong")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, bad)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", w.Code)
	}
}
