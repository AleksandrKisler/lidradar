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

// stubAuthentication подменяет службу identity в тестах транспорта. Нулевое значение ведёт себя
// как закрытый режим без права на организацию; поля нужны тестам регистрации по приглашению.
type stubAuthentication struct {
	mode        application.RegistrationMode
	rights      int
	registerErr error
	registered  *application.RegistrationInput
	// accountErr — ответ команд безопасности учётной записи (ADR 0058); accountCalls собирает их вызовы.
	accountErr   error
	accountCalls *[]accountCall
}

// accountCall — один вызов команды безопасности: что именно получила служба.
type accountCall struct {
	Name, Token, First, Second string
	Client                     application.Client
}

func (stub stubAuthentication) recordAccount(call accountCall) error {
	if stub.accountCalls != nil {
		*stub.accountCalls = append(*stub.accountCalls, call)
	}
	return stub.accountErr
}

func (stub stubAuthentication) ChangePassword(_ context.Context, sessionToken, current, next string, client application.Client) error {
	return stub.recordAccount(accountCall{Name: "change", Token: sessionToken, First: current, Second: next, Client: client})
}
func (stub stubAuthentication) RequestPasswordReset(_ context.Context, email string, client application.Client) error {
	return stub.recordAccount(accountCall{Name: "forgot", First: email, Client: client})
}
func (stub stubAuthentication) ResetPassword(_ context.Context, token, next string, client application.Client) error {
	return stub.recordAccount(accountCall{Name: "reset", First: token, Second: next, Client: client})
}
func (stub stubAuthentication) RequestEmailVerification(_ context.Context, sessionToken string, client application.Client) error {
	return stub.recordAccount(accountCall{Name: "resend", Token: sessionToken, Client: client})
}
func (stub stubAuthentication) VerifyEmail(_ context.Context, token string, client application.Client) error {
	return stub.recordAccount(accountCall{Name: "verify", First: token, Client: client})
}

func (stub stubAuthentication) Register(_ context.Context, input application.RegistrationInput, _ application.Client) (application.Authenticated, error) {
	if stub.registered != nil {
		*stub.registered = input
	}
	if stub.registerErr != nil {
		return application.Authenticated{}, stub.registerErr
	}
	return application.Authenticated{User: domain.User{ID: "user", Email: input.Email}, Token: "opaque-token"}, nil
}
func (stubAuthentication) Login(context.Context, string, string, application.Client) (application.Authenticated, error) {
	return application.Authenticated{User: domain.User{ID: "user", Email: "u@example.com"}, Token: "opaque-token"}, nil
}
func (stub stubAuthentication) Authenticate(context.Context, string) (domain.User, error) {
	return domain.User{ID: "user", OrganizationRights: stub.rights}, nil
}
func (stubAuthentication) Logout(context.Context, string) error { return nil }
func (stubAuthentication) Refresh(context.Context, string, application.Client) (application.Authenticated, error) {
	return application.Authenticated{}, application.ErrUnauthenticated
}
func (stub stubAuthentication) Mode() application.RegistrationMode {
	if stub.mode == application.RegistrationOpen {
		return application.RegistrationOpen
	}
	return application.RegistrationInviteOnly
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
