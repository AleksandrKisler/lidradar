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

type denyAll struct{}

func (denyAll) Allowed(context.Context, string, string, string) (bool, error) { return false, nil }

func TestRiskHTTPListAndTenantIsNotExposed(t *testing.T) {
	repo := infrastructure.NewTestMemoryRepository()
	at := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	risk, err := domain.NewNoResponse("risk", domain.Finding{TenantID: "tenant", OpportunityID: "opp", LocationID: "loc", TriggerMessageID: "msg", Severity: domain.SeverityHigh, PolicyVersion: "v1", ReasonCode: "NO_RESPONSE_THRESHOLD_EXCEEDED", Reason: "ожидание ответа", DueAt: at}, at)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = repo.UpsertActive(context.Background(), risk)
	h := NewHandler(application.NewRadar(repo, allowAll{}, NewHub(), func() time.Time { return at }), testPrincipal{"user", "tenant"}, NewHub()).Router()
	req := httptest.NewRequest(http.MethodGet, "/risks", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"risk"`) || strings.Contains(rec.Body.String(), "TenantID") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRiskHTTPRequiresPrincipal(t *testing.T) {
	h := NewHandler(application.NewRadar(nil, allowAll{}, nil, time.Now), nil, nil).Router()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/risks", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestRiskHTTPChecksPermissionsAndFilters(t *testing.T) {
	repository := infrastructure.NewTestMemoryRepository()
	denied := NewHandler(
		application.NewRadar(repository, denyAll{}, nil, time.Now),
		testPrincipal{"user", "tenant"}, nil,
	).Router()
	for _, endpoint := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/risks"},
		{http.MethodGet, "/radar"},
		{http.MethodPost, "/risks/risk-id/acknowledge"},
	} {
		recorder := httptest.NewRecorder()
		denied.ServeHTTP(recorder, httptest.NewRequest(endpoint.method, endpoint.path, nil))
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("%s %s: status=%d", endpoint.method, endpoint.path, recorder.Code)
		}
	}

	allowed := NewHandler(
		application.NewRadar(repository, allowAll{}, nil, time.Now),
		testPrincipal{"user", "tenant"}, nil,
	).Router()
	for _, path := range []string{
		"/risks?status=UNKNOWN",
		"/risks?severity=URGENT",
		"/risks?cursor=not-a-cursor",
		"/radar?riskType=UNKNOWN",
	} {
		recorder := httptest.NewRecorder()
		allowed.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestSSEPublishesTenantInvalidation(t *testing.T) {
	hub := NewHub()
	handler := NewHandler(application.NewRadar(infrastructure.NewTestMemoryRepository(), allowAll{}, hub, time.Now), testPrincipal{"user", "tenant"}, hub).Router()
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/events", nil)
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
	hub.Publish("other-tenant", "risk.changed", "foreign-risk")
	hub.Publish("tenant", "risk.changed\ninjected", "invalid-risk")
	hub.Publish("tenant", "risk.changed", "risk-1")
	line, err := reader.ReadString('\n')
	if err != nil || line != "event: risk.changed\n" {
		t.Fatalf("event line=%q err=%v", line, err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || !strings.Contains(line, `"resourceId":"risk-1"`) {
		t.Fatalf("data line=%q err=%v", line, err)
	}
	_, _ = reader.ReadString('\n')
	// Вердикт о ложном срабатывании закрывает риск: клиент обязан получить
	// сигнал и перечитать Radar (ADR 0038).
	hub.Publish("tenant", "risk.false_positive", "risk-2")
	line, err = reader.ReadString('\n')
	if err != nil || line != "event: risk.false_positive\n" {
		t.Fatalf("false positive event line=%q err=%v", line, err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || !strings.Contains(line, `"resourceId":"risk-2"`) {
		t.Fatalf("false positive data line=%q err=%v", line, err)
	}
}

func TestRiskHTTPStatusFiltersAndActiveShortcut(t *testing.T) {
	repository := infrastructure.NewTestMemoryRepository()
	at := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	open, err := domain.NewNoResponse("open", domain.Finding{TenantID: "tenant", OpportunityID: "opp-1", LocationID: "loc", TriggerMessageID: "msg", Severity: domain.SeverityHigh, PolicyVersion: "v1", ReasonCode: "NO_RESPONSE_THRESHOLD_EXCEEDED", Reason: "ожидание ответа", DueAt: at}, at)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := domain.NewNoResponse("resolved", domain.Finding{TenantID: "tenant", OpportunityID: "opp-2", LocationID: "loc", TriggerMessageID: "msg", Severity: domain.SeverityHigh, PolicyVersion: "v1", ReasonCode: "NO_RESPONSE_THRESHOLD_EXCEEDED", Reason: "ожидание ответа", DueAt: at}, at)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = repository.UpsertActive(context.Background(), open)
	_, _, _ = repository.UpsertActive(context.Background(), resolved)
	if _, err := repository.ResolveActive(context.Background(), "tenant", "opp-2", domain.TypeNoResponse, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(application.NewRadar(repository, allowAll{}, nil, func() time.Time { return at }), testPrincipal{"user", "tenant"}, nil).Router()
	for path, want := range map[string]string{
		"/risks?active=true":                     `"id":"open"`,
		"/risks?status=RESOLVED":                 `"id":"resolved"`,
		"/risks?status=OPEN,RESOLVED":            `"id":"resolved"`,
		"/risks?status=OPEN&status=ACKNOWLEDGED": `"id":"open"`,
		"/risks?active=false":                    `"id":"resolved"`,
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("%s: status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/risks?active=true", nil))
	if strings.Contains(recorder.Body.String(), `"id":"resolved"`) || !strings.Contains(recorder.Body.String(), `"nextCursor":null`) ||
		!strings.Contains(recorder.Body.String(), `"externalLink":{"url":null,"kind":null,"unavailableReason":null}`) {
		t.Fatalf("активная лента = %s", recorder.Body.String())
	}
	for _, path := range []string{
		"/risks?active=true&status=OPEN",
		"/risks?active=maybe",
		"/risks?status=OPEN,",
		"/risks?status=CLOSED",
		"/radar?active=1",
		"/radar?status=OPEN&active=false",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "INVALID_ARGUMENT") {
			t.Fatalf("%s: status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/radar?status=RESOLVED", nil))
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"openRisks":0`) {
		t.Fatalf("сводка по закрытым: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// Переполнение буфера подписчика не теряет сигнал молча: клиент получает
// маркер resync.required и перечитывает Radar целиком (GAP-RELIABILITY-020).
func TestSSEEmitsResyncMarkerWhenSubscriberBufferOverflows(t *testing.T) {
	hub := NewHub()
	hub.buffer = 1
	handler := NewHandler(application.NewRadar(infrastructure.NewTestMemoryRepository(), allowAll{}, hub, time.Now), testPrincipal{"user", "tenant"}, hub).Router()
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/events", nil)
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
	for index := 0; index < 200; index++ {
		hub.Publish("tenant", "risk.changed", "risk-1")
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("маркер ресинхронизации не получен: %v", err)
		}
		if line == "event: "+ResyncEvent+"\n" {
			data, _ := reader.ReadString('\n')
			if data != "data: {\"reason\":\"BUFFER_OVERFLOW\"}\n" {
				t.Fatalf("тело маркера = %q", data)
			}
			break
		}
	}
	hub.Publish("tenant", "risk.resolved", "risk-2")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("поток не восстановился после маркера: %v", err)
		}
		if line == "event: risk.resolved\n" {
			break
		}
	}
}
