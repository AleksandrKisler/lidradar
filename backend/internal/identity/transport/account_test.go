package transport

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/identity/application"
)

// Транспорт команд безопасности учётной записи (ADR 0058): маршруты, разбор тела, соответствие ошибок
// кодам и статусам. Правила самих команд проверяют тесты службы.

func TestAccountCommandsPassTheirArgumentsAndAnswerWithoutABody(t *testing.T) {
	token := strings.Repeat("T", 43)
	for name, tc := range map[string]struct {
		path, body string
		want       accountCall
	}{
		"смена пароля": {
			"/password/change", `{"currentPassword":"old-password-value","newPassword":"new-password-value"}`,
			accountCall{Name: "change", Token: "opaque-token", First: "old-password-value", Second: "new-password-value"},
		},
		"запрос сброса": {
			"/password/forgot", `{"email":"user@example.com"}`,
			accountCall{Name: "forgot", First: "user@example.com"},
		},
		"завершение сброса": {
			"/password/reset", `{"token":"` + token + `","newPassword":"new-password-value"}`,
			accountCall{Name: "reset", First: token, Second: "new-password-value"},
		},
		"повторное письмо": {
			"/email/verification", ``,
			accountCall{Name: "resend", Token: "opaque-token"},
		},
		"подтверждение почты": {
			"/email/verify", `{"token":"` + token + `"}`,
			accountCall{Name: "verify", First: token},
		},
	} {
		var calls []accountCall
		response := call(t, stubAuthentication{accountCalls: &calls}, http.MethodPost, tc.path, tc.body)
		if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
			t.Errorf("%s: %d %q, want 204 without a body", name, response.Code, response.Body.String())
		}
		if len(response.Result().Cookies()) != 0 {
			t.Errorf("%s: команда не должна менять cookie: %+v", name, response.Result().Cookies())
		}
		tc.want.Client.UserAgent = "" // httptest не задаёт User-Agent
		if len(calls) != 1 || calls[0].Name != tc.want.Name || calls[0].Token != tc.want.Token ||
			calls[0].First != tc.want.First || calls[0].Second != tc.want.Second {
			t.Errorf("%s: служба получила %+v, want %+v", name, calls, tc.want)
		}
	}
}

func TestAccountCommandsMapErrorsToStableCodes(t *testing.T) {
	for name, tc := range map[string]struct {
		path, body string
		err        error
		status     int
		code       string
	}{
		"неверный текущий пароль — не 401": {
			"/password/change", `{"currentPassword":"a","newPassword":"b"}`,
			application.ErrInvalidCurrentPassword, http.StatusForbidden, "INVALID_CURRENT_PASSWORD",
		},
		"смена без сессии": {
			"/password/change", `{"currentPassword":"a","newPassword":"b"}`,
			application.ErrUnauthenticated, http.StatusUnauthorized, "UNAUTHENTICATED",
		},
		"слабый новый пароль": {
			"/password/change", `{"currentPassword":"a","newPassword":"b"}`,
			application.ErrInvalidInput, http.StatusBadRequest, "INVALID_ARGUMENT",
		},
		"негодный токен сброса": {
			"/password/reset", `{"token":"a","newPassword":"b"}`,
			application.ErrResetTokenInvalid, http.StatusBadRequest, "RESET_TOKEN_INVALID",
		},
		"негодный токен подтверждения": {
			"/email/verify", `{"token":"a"}`,
			application.ErrVerificationTokenInvalid, http.StatusBadRequest, "VERIFICATION_TOKEN_INVALID",
		},
		"почта не настроена": {
			"/email/verification", ``,
			application.ErrMailUnavailable, http.StatusServiceUnavailable, "MAIL_UNAVAILABLE",
		},
		"сбой хранилища не раскрывает деталей": {
			"/password/forgot", `{"email":"a@example.com"}`,
			errors.New("connection refused to 10.0.0.5"), http.StatusInternalServerError, "INTERNAL_ERROR",
		},
	} {
		response := call(t, stubAuthentication{accountErr: tc.err}, http.MethodPost, tc.path, tc.body)
		if response.Code != tc.status || errorCode(t, response) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", name, response.Code, response.Body.String(), tc.status, tc.code)
		}
		if strings.Contains(response.Body.String(), "10.0.0.5") {
			t.Errorf("%s: внутренняя ошибка попала в ответ: %s", name, response.Body.String())
		}
	}
}

func TestAccountCommandsReportRateLimitWithRetryAfter(t *testing.T) {
	for _, path := range []string{"/password/forgot", "/password/reset", "/email/verify", "/password/change"} {
		body := `{"email":"a@example.com","token":"t","newPassword":"n","currentPassword":"c"}`
		switch path {
		case "/password/forgot":
			body = `{"email":"a@example.com"}`
		case "/password/reset":
			body = `{"token":"t","newPassword":"n"}`
		case "/email/verify":
			body = `{"token":"t"}`
		case "/password/change":
			body = `{"currentPassword":"c","newPassword":"n"}`
		}
		limited := application.RateLimitError{RetryAfter: 90*time.Minute + time.Second}
		response := call(t, stubAuthentication{accountErr: limited}, http.MethodPost, path, body)
		if response.Code != http.StatusTooManyRequests || errorCode(t, response) != "RATE_LIMITED" {
			t.Errorf("%s: %d %s", path, response.Code, response.Body.String())
		}
		if got := response.Header().Get("Retry-After"); got != "5401" {
			t.Errorf("%s: Retry-After = %q, want 5401", path, got)
		}
	}
}

// Тело проверяется так же строго, как у входа: лишние поля, мусор и пустое тело — 400, и служба не вызывается.
func TestAccountCommandsRejectMalformedBodiesBeforeCallingTheService(t *testing.T) {
	for _, path := range []string{"/password/change", "/password/forgot", "/password/reset", "/email/verify"} {
		for name, body := range map[string]string{
			"пустое тело":  ``,
			"не JSON":      `nope`,
			"лишнее поле":  `{"email":"a@example.com","token":"t","newPassword":"n","currentPassword":"c","extra":1}`,
			"два значения": `{} {}`,
			"массив":       `[]`,
		} {
			var calls []accountCall
			response := call(t, stubAuthentication{accountCalls: &calls}, http.MethodPost, path, body)
			if response.Code != http.StatusBadRequest || errorCode(t, response) != "INVALID_ARGUMENT" {
				t.Errorf("%s %s: %d %s", path, name, response.Code, response.Body.String())
			}
			if len(calls) != 0 {
				t.Errorf("%s %s: служба не должна вызываться: %+v", path, name, calls)
			}
		}
	}
}

func TestAccountCommandsAcceptOnlyPost(t *testing.T) {
	handler := NewHandler(stubAuthentication{}, nil, CookieConfiguration{TTL: time.Hour}).Router()
	for _, path := range []string{"/password/change", "/password/forgot", "/password/reset", "/email/verification", "/email/verify"} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(method, path, nil))
			if response.Code != http.StatusMethodNotAllowed && response.Code != http.StatusNotFound {
				t.Errorf("%s %s: %d", method, path, response.Code)
			}
		}
	}
}

// Пользователь из ответа несёт признак подтверждения почты, но не хэш пароля и не счётчик права.
func TestMeShowsEmailVerificationWithoutSecrets(t *testing.T) {
	response := call(t, stubAuthentication{}, http.MethodGet, "/me", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"emailVerifiedAt":null`) {
		t.Fatalf("/me: %d %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"passwordHash", "PasswordHash", "password"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("/me раскрывает %q: %s", secret, response.Body.String())
		}
	}
}
