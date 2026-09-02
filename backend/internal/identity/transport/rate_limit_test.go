package transport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/identity/application"
)

func TestRateLimitErrorReturnsRetryAfterWithoutAccountDetails(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	if !handleError(response, request, application.RateLimitError{RetryAfter: 90 * time.Second}) {
		t.Fatal("ошибка ограничения частоты не обработана")
	}
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "90" {
		t.Fatalf("ответ = %d, Retry-After=%q", response.Code, response.Header().Get("Retry-After"))
	}
	if body := response.Body.String(); !strings.Contains(body, `"code":"RATE_LIMITED"`) || strings.Contains(body, "owner@example.com") {
		t.Fatalf("небезопасное тело ответа: %s", body)
	}
}
