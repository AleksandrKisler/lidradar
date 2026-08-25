package transport_test

import (
	"context"
	"fmt"
	"lidradar/backend/internal/corrective/application"
	"lidradar/backend/internal/corrective/infrastructure"
	"lidradar/backend/internal/corrective/transport"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type principal struct{}

func (principal) Principal(*http.Request) (string, string, bool) { return "actor", "tenant", true }

type allow struct{}

func (allow) Allowed(context.Context, string, string, string) (bool, error) { return true, nil }

type ids struct{ n int }

func (i *ids) NewID() string { i.n++; return fmt.Sprintf("id-%d", i.n) }
func TestHTTPFlowAndRequiredIdempotencyKey(t *testing.T) {
	store := infrastructure.NewTestMemoryStore()
	store.AddRisk("tenant", "risk", "opportunity")
	handler := transport.NewHandler(application.NewService(store, allow{}, &ids{}, func() time.Time { return time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC) }), principal{}).Router()
	req := httptest.NewRequest("POST", "/api/v1/risks/risk/recommendation", strings.NewReader(`{"riskType":"NO_RESPONSE"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Ответить клиенту сейчас") {
		t.Fatalf("recommendation status=%d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest("POST", "/api/v1/risks/risk/actions", strings.NewReader(`{"type":"MARK_CONTACTED"}`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("missing key status=%d", rec.Code)
	}
	req = httptest.NewRequest("POST", "/api/v1/opportunities/opportunity/outcomes", strings.NewReader(`{"status":"BOOKED"}`))
	req.Header.Set("Idempotency-Key", "outcome")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"status":"BOOKED"`) {
		t.Fatalf("outcome status=%d body=%s", rec.Code, rec.Body.String())
	}
}
