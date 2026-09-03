package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/identity/application"
	"lidradar/backend/internal/identity/domain"
)

type stubAuthentication struct{}

func (stubAuthentication) Register(context.Context, string, string, string, application.Client) (application.Authenticated, error) {
	return application.Authenticated{}, application.ErrInvalidCredentials
}
func (stubAuthentication) Login(context.Context, string, string, application.Client) (application.Authenticated, error) {
	return application.Authenticated{User: domain.User{ID: "user", Email: "u@example.com"}, Token: "opaque-token"}, nil
}
func (stubAuthentication) Authenticate(context.Context, string) (domain.User, error) {
	return domain.User{ID: "user"}, nil
}
func (stubAuthentication) Logout(context.Context, string) error { return nil }
func (stubAuthentication) Refresh(context.Context, string, application.Client) (application.Authenticated, error) {
	return application.Authenticated{}, application.ErrUnauthenticated
}

// LR-BE-2405: сессионная cookie всегда HttpOnly, SameSite=Strict, Path=/;
// признак Secure включается настройкой для staging/production, выход её стирает.
func TestSessionCookieFlagsFollowSecureConfiguration(t *testing.T) {
	for _, secure := range []bool{false, true} {
		handler := NewHandler(stubAuthentication{}, nil, CookieConfiguration{Secure: secure, TTL: time.Hour}).Router()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"email":"u@example.com","password":"secret-password"}`)))
		if response.Code != http.StatusOK {
			t.Fatalf("secure=%v: вход = %d %s", secure, response.Code, response.Body.String())
		}
		cookies := response.Result().Cookies()
		if len(cookies) != 1 || cookies[0].Name != SessionCookieName || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode ||
			cookies[0].Path != "/" || cookies[0].Secure != secure || cookies[0].MaxAge != 3600 {
			t.Fatalf("secure=%v: cookie = %+v", secure, cookies)
		}
		if strings.Contains(response.Body.String(), "opaque-token") {
			t.Fatal("сессионный токен попал в тело ответа")
		}
		logout := httptest.NewRecorder()
		handler.ServeHTTP(logout, httptest.NewRequest(http.MethodPost, "/logout", nil))
		cleared := logout.Result().Cookies()
		if len(cleared) != 1 || cleared[0].MaxAge != -1 || cleared[0].Value != "" || !cleared[0].HttpOnly || cleared[0].Secure != secure {
			t.Fatalf("secure=%v: cookie выхода = %+v", secure, cleared)
		}
	}
}
