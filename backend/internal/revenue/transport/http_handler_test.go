package transport

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/revenue/application"
	"lidradar/backend/internal/revenue/infrastructure"
)

type allowAll struct{}

func (allowAll) Allowed(context.Context, string, string, string) (bool, error) { return true, nil }

type sequenceIDs struct{ next int }

func (ids *sequenceIDs) NewID() (string, error) {
	ids.next++
	return fmt.Sprintf("id-%d", ids.next), nil
}

type principal struct{ ok bool }

func (value principal) Principal(*http.Request) (string, string, bool) {
	return "actor", "tenant", value.ok
}

func TestRevenueHTTPConfirmationReplayTotalAndStrictInput(t *testing.T) {
	now := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)
	store := infrastructure.NewTestMemoryStore()
	store.AddOpportunity("tenant", "opportunity")
	fact := application.RelatedFact{OpportunityID: "opportunity", At: now.Add(-time.Hour)}
	store.AddRisk("tenant", "risk", fact)
	store.AddAction("tenant", "action", application.RelatedFact{OpportunityID: "opportunity", RiskID: "risk", At: fact.At})
	store.AddOutcome("tenant", "outcome", fact)
	service := application.NewService(store, allowAll{}, &sequenceIDs{}, func() time.Time { return now })
	handler := NewHandler(service, principal{ok: true}).Router()
	body := `{"amount":"47000","currency":"rub","attributionType":"RECOVERED","riskId":"risk","actionId":"action","outcomeId":"outcome"}`

	created := revenueRequest(handler, http.MethodPost, "/opportunities/opportunity/revenue", body, "payment-1")
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"source":"USER_CONFIRMED"`) {
		t.Fatalf("создание: status=%d body=%s", created.Code, created.Body.String())
	}
	replayed := revenueRequest(handler, http.MethodPost, "/opportunities/opportunity/revenue", body, "payment-1")
	if replayed.Code != http.StatusOK || replayed.Body.String() != created.Body.String() {
		t.Fatalf("повтор: status=%d body=%s", replayed.Code, replayed.Body.String())
	}
	total := revenueRequest(handler, http.MethodGet, "/revenue/confirmed-recovered?currency=rub", "", "")
	if total.Code != http.StatusOK || total.Body.String() != "{\"amount\":\"47000.00\",\"currency\":\"RUB\"}\n" {
		t.Fatalf("итог: status=%d body=%s", total.Code, total.Body.String())
	}
	for _, invalid := range []string{
		body + `{}`,
		`{"amount":"1","currency":"RUB","attributionType":"ORGANIC","лишнее":true}`,
	} {
		response := revenueRequest(handler, http.MethodPost, "/opportunities/opportunity/revenue", invalid, "invalid")
		if response.Code != http.StatusBadRequest {
			t.Errorf("небезопасное тело принято: status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestRevenueHTTPRequiresPrincipal(t *testing.T) {
	response := revenueRequest(
		NewHandler(application.Service{}, principal{}).Router(), http.MethodGet,
		"/revenue/confirmed-recovered?currency=RUB", "", "",
	)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func revenueRequest(handler http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
