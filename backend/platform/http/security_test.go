package httpplatform

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lidradar/backend/platform/observability"
)

// LR-BE-2405: ответы API несут заголовки безопасности; HSTS появляется только
// за TLS либо при включённых Secure cookie.
func TestSecurityHeadersAreAlwaysPresent(t *testing.T) {
	logger := observability.NewLogger(&bytes.Buffer{}, "test", "test")
	plain := NewRouter("lidradar-api", logger, readinessStub{})
	response := httptest.NewRecorder()
	plain.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	header := response.Header()
	if header.Get("X-Content-Type-Options") != "nosniff" || header.Get("X-Frame-Options") != "DENY" ||
		header.Get("Referrer-Policy") != "no-referrer" || header.Get("Cache-Control") != "no-store" ||
		header.Get("Content-Security-Policy") == "" || header.Get("Strict-Transport-Security") != "" {
		t.Fatalf("заголовки = %v", header)
	}
	strict := NewRouter("lidradar-api", logger, readinessStub{}, WithStrictTransport(true))
	response = httptest.NewRecorder()
	strict.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if response.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("HSTS отсутствует при включённом строгом транспорте")
	}
	missing := httptest.NewRecorder()
	plain.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/nowhere", nil))
	if missing.Header().Get("X-Content-Type-Options") != "nosniff" || missing.Code != http.StatusNotFound {
		t.Fatalf("заголовки на 404: %v (%d)", missing.Header(), missing.Code)
	}
}

// LR-BE-2404: ограничение по адресу действует только на маршруты без сессии,
// отвечает 429 с Retry-After и сбрасывается после окна.
func TestRateLimitProtectsUnauthenticatedPrefixes(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	router := NewRouter("lidradar-api", observability.NewLogger(&bytes.Buffer{}, "test", "test"), readinessStub{},
		WithRateLimit(RateLimit{Requests: 3, Window: time.Minute, Prefixes: []string{"/api/v1/auth/"}}), WithClock(clock))
	call := func(path, remote string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		request.RemoteAddr = remote
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if response := call("/api/v1/auth/login", "10.0.0.1:5000"); response.Code == http.StatusTooManyRequests {
			t.Fatalf("попытка %d ограничена преждевременно", attempt)
		}
	}
	limited := call("/api/v1/auth/login", "10.0.0.1:5001")
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("четвёртая попытка = %d, Retry-After=%q", limited.Code, limited.Header().Get("Retry-After"))
	}
	if response := call("/api/v1/auth/login", "10.0.0.2:5000"); response.Code == http.StatusTooManyRequests {
		t.Fatal("другой адрес ограничен чужими попытками")
	}
	if response := call("/api/v1/risks", "10.0.0.1:5002"); response.Code == http.StatusTooManyRequests {
		t.Fatal("маршрут вне префикса ограничен")
	}
	now = now.Add(61 * time.Second)
	if response := call("/api/v1/auth/login", "10.0.0.1:5003"); response.Code == http.StatusTooManyRequests {
		t.Fatal("окно не сбросилось")
	}
	if address := clientAddress(&http.Request{RemoteAddr: "[2001:db8::1]:443"}); address != "2001:db8::1" {
		t.Fatalf("адрес IPv6 = %q", address)
	}
}
