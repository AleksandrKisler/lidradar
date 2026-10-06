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

func (i *ids) NewID() (string, error) { i.n++; return fmt.Sprintf("id-%d", i.n), nil }
func TestHTTPFlowAndRequiredIdempotencyKey(t *testing.T) {
	store := infrastructure.NewTestMemoryStore()
	store.AddRisk("tenant", "risk", "opportunity")
	handler := transport.NewHandler(application.NewService(store, allow{}, &ids{}, func() time.Time { return time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC) }), principal{}).Router()
	req := httptest.NewRequest("POST", "/risks/risk/recommendation", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Ответить клиенту сейчас") {
		t.Fatalf("recommendation status=%d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest("POST", "/risks/risk/actions", strings.NewReader(`{"type":"MARK_CONTACTED"}`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("missing key status=%d", rec.Code)
	}
	req = httptest.NewRequest("POST", "/opportunities/opportunity/outcomes", strings.NewReader(`{"status":"BOOKED"}`))
	req.Header.Set("Idempotency-Key", "outcome")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"status":"BOOKED"`) {
		t.Fatalf("outcome status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHTTPRejectsUnknownAndTrailingJSON(t *testing.T) {
	store := infrastructure.NewTestMemoryStore()
	store.AddRisk("tenant", "risk", "opportunity")
	handler := transport.NewHandler(application.NewService(store, allow{}, &ids{}, time.Now), principal{}).Router()
	for _, body := range []string{
		`{"type":"CALL","unexpected":true}`,
		`{"type":"CALL"}{"type":"OTHER"}`,
	} {
		req := httptest.NewRequest("POST", "/risks/risk/actions", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", "untrusted-input")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%q status=%d response=%s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestHTTPClosedRiskRejectsNewActionsButKeepsIdempotentReplay(t *testing.T) {
	for _, status := range []string{"OPEN", "ACKNOWLEDGED", "ACTED", "RESOLVED", "FALSE_POSITIVE", "IGNORED", "EXPIRED"} {
		t.Run(status, func(t *testing.T) {
			store := infrastructure.NewTestMemoryStore()
			store.AddRisk("tenant", "risk", "opportunity")
			handler := transport.NewHandler(application.NewService(store, allow{}, &ids{}, time.Now), principal{}).Router()
			send := func(key, kind string) *httptest.ResponseRecorder {
				req := httptest.NewRequest("POST", "/risks/risk/actions", strings.NewReader(`{"type":"`+kind+`"}`))
				req.Header.Set("Idempotency-Key", key)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				return rec
			}
			original := send("original", "CALL")
			if original.Code != http.StatusCreated {
				t.Fatal(original.Body.String())
			}
			store.SetRiskStatus("tenant", "risk", status)
			for _, kind := range []string{"OPEN_CONVERSATION", "COPY_REPLY", "MARK_CONTACTED", "CALL", "SEND_MESSAGE", "OTHER"} {
				response := send("new-"+kind, kind)
				if status == "OPEN" || status == "ACKNOWLEDGED" || status == "ACTED" {
					if response.Code != http.StatusCreated {
						t.Fatalf("active risk rejected: %s", response.Body.String())
					}
				} else {
					if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"RISK_CLOSED"`) {
						t.Fatalf("closed risk: status=%d body=%s", response.Code, response.Body.String())
					}
					if len(store.Actions()) != 1 || len(store.Audits()) != 1 {
						t.Fatal("closed request wrote action or audit")
					}
				}
			}
			if replay := send("original", "CALL"); replay.Code != http.StatusOK || replay.Body.String() != original.Body.String() {
				t.Fatalf("replay: %d %s", replay.Code, replay.Body.String())
			}
			if conflict := send("original", "OTHER"); conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "IDEMPOTENCY_CONFLICT") {
				t.Fatal(conflict.Body.String())
			}
		})
	}
}
