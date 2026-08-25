package transport

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/risk/application"
	"lidradar/backend/internal/risk/domain"
	"lidradar/backend/internal/risk/infrastructure"
)

type testPrincipal struct{ actor, tenant string }

func (p testPrincipal) Principal(*http.Request) (string, string, bool) {
	return p.actor, p.tenant, true
}

type allowAll struct{}

func (allowAll) Allowed(context.Context, string, string, string) (bool, error) { return true, nil }

func TestRiskHTTPListAndTenantIsNotExposed(t *testing.T) {
	repo := infrastructure.NewTestMemoryRepository()
	at := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	risk, err := domain.NewNoResponse("risk", domain.Finding{TenantID: "tenant", OpportunityID: "opp", LocationID: "loc", TriggerMessageID: "msg", Severity: domain.SeverityHigh, PolicyVersion: "v1", Reason: "waiting"}, at)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = repo.UpsertActive(context.Background(), risk)
	h := NewHandler(application.NewRadar(repo, allowAll{}, NewHub(), func() time.Time { return at }), testPrincipal{"user", "tenant"}, NewHub()).Router()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/risks", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"risk"`) || strings.Contains(rec.Body.String(), "TenantID") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRiskHTTPRequiresPrincipal(t *testing.T) {
	h := NewHandler(application.NewRadar(nil, allowAll{}, nil, time.Now), nil, nil).Router()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/risks", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSSEPublishesTenantInvalidation(t *testing.T) {
	hub := NewHub()
	handler := NewHandler(application.NewRadar(infrastructure.NewTestMemoryRepository(), allowAll{}, hub, time.Now), testPrincipal{"user", "tenant"}, hub).Router()
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/events", nil)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if line, _ := reader.ReadString('\n'); line != ": connected\n" {
		t.Fatalf("initial event = %q", line)
	}
	_, _ = reader.ReadString('\n')
	hub.Publish("tenant", "risk.changed", "risk-1")
	line, err := reader.ReadString('\n')
	if err != nil || line != "event: risk.changed\n" {
		t.Fatalf("event line=%q err=%v", line, err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || !strings.Contains(line, `"resourceId":"risk-1"`) {
		t.Fatalf("data line=%q err=%v", line, err)
	}
}
