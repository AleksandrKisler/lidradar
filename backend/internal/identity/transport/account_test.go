package transport

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/identity/application"
	"lidradar/backend/internal/identity/domain"
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

// Выгрузка личных данных и закрытие учётной записи (ADR 0059).

func TestAccountExportReturnsTheDocumentAsANoStoreAttachment(t *testing.T) {
	var calls []accountCall
	document := []byte(`{"format":"lidradar.account-export/1"}`)
	response := call(t, stubAuthentication{accountCalls: &calls, exportDocument: document}, http.MethodPost, "/account/export", `{"password":"current-password-value"}`)
	if response.Code != http.StatusOK || response.Body.String() != string(document) {
		t.Fatalf("/account/export: %d %q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	disposition := response.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disposition, `attachment; filename="lidradar-account-`) || !strings.HasSuffix(disposition, `.json"`) {
		t.Errorf("Content-Disposition = %q", disposition)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if len(calls) != 1 || calls[0].Name != "export" || calls[0].Token != "opaque-token" || calls[0].First != "current-password-value" {
		t.Errorf("служба получила %+v", calls)
	}
	if len(response.Result().Cookies()) != 0 {
		t.Errorf("выгрузка не должна менять cookie: %+v", response.Result().Cookies())
	}
}

func TestAccountDeletionRequestAnswersWithTheDueDate(t *testing.T) {
	var calls []accountCall
	due := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	response := call(t, stubAuthentication{accountCalls: &calls, deletionDue: due}, http.MethodPost, "/account/deletion", `{"password":"current-password-value"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("/account/deletion: %d %s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload["deletionDueAt"] != "2026-11-13T09:00:00Z" || len(payload) != 1 {
		t.Fatalf("тело ответа: %s", response.Body.String())
	}
	if len(calls) != 1 || calls[0].Name != "delete" || calls[0].First != "current-password-value" || calls[0].Token != "opaque-token" {
		t.Errorf("служба получила %+v", calls)
	}
}

func TestAccountDeletionCancelAnswersWithoutABody(t *testing.T) {
	var calls []accountCall
	response := call(t, stubAuthentication{accountCalls: &calls}, http.MethodDelete, "/account/deletion", ``)
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("DELETE /account/deletion: %d %q, want 204 without a body", response.Code, response.Body.String())
	}
	if len(calls) != 1 || calls[0].Name != "cancel" || calls[0].Token != "opaque-token" {
		t.Errorf("служба получила %+v", calls)
	}
}

func TestAccountDeletionBlockedReportsEveryReason(t *testing.T) {
	blocked := application.DeletionBlockedError{Blockers: []application.DeletionBlocker{
		{Code: application.BlockerSoleOwner, OrganizationName: "Салон Лотос", HasOtherMembers: true},
		{Code: application.BlockerPlatformAdmin},
	}}
	response := call(t, stubAuthentication{accountErr: blocked}, http.MethodPost, "/account/deletion", `{"password":"current-password-value"}`)
	if response.Code != http.StatusConflict || errorCode(t, response) != "ACCOUNT_DELETION_BLOCKED" {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	var payload struct {
		Error struct {
			Details struct {
				Blockers []map[string]any `json:"blockers"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	blockers := payload.Error.Details.Blockers
	if len(blockers) != 2 || blockers[0]["code"] != "SOLE_OWNER" || blockers[0]["organizationName"] != "Салон Лотос" ||
		blockers[0]["hasOtherMembers"] != true || blockers[1]["code"] != "PLATFORM_ADMIN" {
		t.Fatalf("blockers = %v", blockers)
	}
	if _, leaked := blockers[1]["organizationName"]; leaked {
		t.Errorf("пустое название организации не должно выходить наружу: %v", blockers[1])
	}
	// Идентификаторы организаций наружу не выходят никогда.
	for _, field := range []string{"tenantId", "organizationId"} {
		if strings.Contains(response.Body.String(), field) {
			t.Errorf("ответ содержит %s: %s", field, response.Body.String())
		}
	}
}

func TestAccountDataCommandsMapErrorsToStableCodes(t *testing.T) {
	for name, tc := range map[string]struct {
		method, path, body string
		err                error
		status             int
		code               string
	}{
		"выгрузка: неверный пароль — не 401": {
			http.MethodPost, "/account/export", `{"password":"x"}`,
			application.ErrInvalidCurrentPassword, http.StatusForbidden, "INVALID_CURRENT_PASSWORD",
		},
		"выгрузка без сессии": {
			http.MethodPost, "/account/export", `{"password":"x"}`,
			application.ErrUnauthenticated, http.StatusUnauthorized, "UNAUTHENTICATED",
		},
		"закрытие: неверный пароль — не 401": {
			http.MethodPost, "/account/deletion", `{"password":"x"}`,
			application.ErrInvalidCurrentPassword, http.StatusForbidden, "INVALID_CURRENT_PASSWORD",
		},
		"закрытие: пустой пароль": {
			http.MethodPost, "/account/deletion", `{"password":""}`,
			application.ErrInvalidInput, http.StatusBadRequest, "INVALID_ARGUMENT",
		},
		"отмена без сессии": {
			http.MethodDelete, "/account/deletion", ``,
			application.ErrUnauthenticated, http.StatusUnauthorized, "UNAUTHENTICATED",
		},
		"сбой хранилища не раскрывает деталей": {
			http.MethodPost, "/account/export", `{"password":"x"}`,
			errors.New("connection refused to 10.0.0.5"), http.StatusInternalServerError, "INTERNAL_ERROR",
		},
	} {
		response := call(t, stubAuthentication{accountErr: tc.err}, tc.method, tc.path, tc.body)
		if response.Code != tc.status || errorCode(t, response) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", name, response.Code, response.Body.String(), tc.status, tc.code)
		}
		if strings.Contains(response.Body.String(), "10.0.0.5") {
			t.Errorf("%s: внутренняя ошибка попала в ответ: %s", name, response.Body.String())
		}
		if response.Header().Get("Content-Disposition") != "" {
			t.Errorf("%s: отказ не должен выглядеть как вложение", name)
		}
	}
}

func TestAccountDataCommandsReportRateLimitWithRetryAfter(t *testing.T) {
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/account/export", `{"password":"x"}`},
		{http.MethodPost, "/account/deletion", `{"password":"x"}`},
		{http.MethodDelete, "/account/deletion", ``},
	} {
		response := call(t, stubAuthentication{accountErr: application.RateLimitError{RetryAfter: time.Hour}}, tc.method, tc.path, tc.body)
		if response.Code != http.StatusTooManyRequests || errorCode(t, response) != "RATE_LIMITED" || response.Header().Get("Retry-After") != "3600" {
			t.Errorf("%s %s: %d %s Retry-After=%q", tc.method, tc.path, response.Code, response.Body.String(), response.Header().Get("Retry-After"))
		}
	}
}

func TestAccountDataCommandsRejectMalformedBodiesBeforeCallingTheService(t *testing.T) {
	for _, path := range []string{"/account/export", "/account/deletion"} {
		for name, body := range map[string]string{
			"пустое тело":  ``,
			"не JSON":      `nope`,
			"лишнее поле":  `{"password":"x","extra":1}`,
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

func TestAccountDataRoutesAcceptOnlyTheirMethods(t *testing.T) {
	handler := NewHandler(stubAuthentication{}, nil, CookieConfiguration{TTL: time.Hour}).Router()
	for path, methods := range map[string][]string{
		"/account/export":   {http.MethodGet, http.MethodPut, http.MethodDelete},
		"/account/deletion": {http.MethodGet, http.MethodPut},
	} {
		for _, method := range methods {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(method, path, nil))
			if response.Code != http.StatusMethodNotAllowed && response.Code != http.StatusNotFound {
				t.Errorf("%s %s: %d", method, path, response.Code)
			}
		}
	}
}

// Срок закрытия выходит наружу полем user.deletionDueAt, остальные отметки назначения — нет.
func TestMeShowsTheDeletionDueDateOnly(t *testing.T) {
	requested := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	due := requested.Add(30 * 24 * time.Hour)
	scheduled := domain.User{ID: "user", DeletionRequestedAt: &requested, DeletionDueAt: &due}
	response := call(t, stubAuthentication{authenticated: &scheduled}, http.MethodGet, "/me", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"deletionDueAt":"2026-11-13T09:00:00Z"`) {
		t.Fatalf("/me: %d %s", response.Code, response.Body.String())
	}
	for _, hidden := range []string{"deletionRequestedAt", "DeletionRequestedAt", "deletedAt"} {
		if strings.Contains(response.Body.String(), hidden) {
			t.Errorf("/me раскрывает %q: %s", hidden, response.Body.String())
		}
	}
	none := call(t, stubAuthentication{}, http.MethodGet, "/me", "")
	if !strings.Contains(none.Body.String(), `"deletionDueAt":null`) {
		t.Errorf("без назначения поле должно быть null: %s", none.Body.String())
	}
}
