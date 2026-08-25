package httpplatform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lidradar/backend/platform/health"
	"lidradar/backend/platform/observability"
)

type readinessStub struct{ err error }

func (s readinessStub) Check(context.Context) (health.Status, error) {
	return health.Status{DatabaseMigration: "000008_opportunity_domain"}, s.err
}

func TestHealthEndpoints(t *testing.T) {
	for _, test := range []struct {
		name      string
		path      string
		readiness health.Checker
		status    int
	}{
		{name: "live", path: "/health/live", readiness: readinessStub{}, status: http.StatusOK},
		{name: "ready", path: "/health/ready", readiness: readinessStub{}, status: http.StatusOK},
		{name: "not ready", path: "/health/ready", readiness: readinessStub{err: errors.New("down")}, status: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()
			NewRouter("lidradar-api", observability.NewLogger(&bytes.Buffer{}, "test", "test"), test.readiness).ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.status, response.Body.String())
			}
			if response.Header().Get(requestIDHeader) == "" {
				t.Fatal("response has no request ID")
			}
			if test.path == "/health/ready" && test.status == http.StatusOK {
				var body struct {
					DatabaseMigration string `json:"databaseMigration"`
					Build             struct {
						Version  string `json:"version"`
						Revision string `json:"revision"`
					} `json:"build"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.DatabaseMigration == "" || body.Build.Version == "" || body.Build.Revision == "" {
					t.Fatalf("readiness metadata = %#v", body)
				}
			}
		})
	}
}

func TestErrorEnvelopeIncludesTraceID(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/missing", nil)
	request.Header.Set(traceparentHeader, "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
	response := httptest.NewRecorder()
	NewRouter("lidradar-api", observability.NewLogger(&bytes.Buffer{}, "test", "test"), readinessStub{}).ServeHTTP(response, request)

	var envelope ErrorEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if envelope.Error.Code != "ROUTE_NOT_FOUND" || envelope.Error.TraceID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("error envelope = %#v", envelope)
	}
}

func TestRecoveryHidesPanicDetails(t *testing.T) {
	logger := observability.NewLogger(&bytes.Buffer{}, "test", "test")
	router := NewRouter("lidradar-api", logger, readinessStub{})
	router.Get("/panic", func(http.ResponseWriter, *http.Request) { panic("secret detail") })
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if response.Code != http.StatusInternalServerError || bytes.Contains(response.Body.Bytes(), []byte("secret detail")) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRouterRejectsUntrustedMutationOrigin(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://api.example/api/v1/test", nil)
	request.Header.Set("Origin", "https://evil.example")
	response := httptest.NewRecorder()
	NewRouter("lidradar-api", observability.NewLogger(&bytes.Buffer{}, "test", "test"), readinessStub{}).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "ORIGIN_NOT_ALLOWED") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestRouterAllowsConfiguredMutationOrigin(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://api.example/missing", nil)
	request.Header.Set("Origin", "https://app.example")
	response := httptest.NewRecorder()
	NewRouter("lidradar-api", observability.NewLogger(&bytes.Buffer{}, "test", "test"), readinessStub{}, WithAllowedOrigins([]string{"https://app.example"})).ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusNotFound)
	}
}
