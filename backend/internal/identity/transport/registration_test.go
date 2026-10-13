package transport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/identity/application"
)

func call(t *testing.T, stub stubAuthentication, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	handler := NewHandler(stub, nil, CookieConfiguration{TTL: time.Hour}).Router()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "opaque-token"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func errorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("ответ не разобрался: %v: %s", err, response.Body.String())
	}
	return payload.Error.Code
}

// Интерфейсу нужен режим регистрации до входа в систему (ADR 0057): страница регистрации решает по нему,
// показывать ли поле кода. Режим без настройки считается закрытым.
func TestRegistrationEndpointReportsTheModeWithoutASession(t *testing.T) {
	for name, tc := range map[string]struct {
		mode application.RegistrationMode
		want string
	}{
		"открытый":               {application.RegistrationOpen, "open"},
		"закрытый":               {application.RegistrationInviteOnly, "invite_only"},
		"не настроен — закрытый": {"", "invite_only"},
	} {
		handler := NewHandler(stubAuthentication{mode: tc.mode}, nil, CookieConfiguration{TTL: time.Hour}).Router()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/registration", nil)) // без cookie
		var payload map[string]string
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload["mode"] != tc.want {
			t.Errorf("%s: %d %s, want mode %q", name, response.Code, response.Body.String(), tc.want)
		}
	}
}

func TestRegisterPassesTheInviteCodeAndMapsInvitationErrors(t *testing.T) {
	code := strings.Repeat("A", 43)
	var seen application.RegistrationInput
	response := call(t, stubAuthentication{registered: &seen}, http.MethodPost, "/register",
		`{"email":"new@example.com","password":"very-secure-password","displayName":"New","inviteCode":"`+code+`"}`)
	if response.Code != http.StatusCreated || seen.InviteCode != code || seen.Email != "new@example.com" || seen.DisplayName != "New" {
		t.Fatalf("регистрация с кодом: %d %s, служба получила %+v", response.Code, response.Body.String(), seen)
	}
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"код обязателен":     {application.ErrInvitationRequired, "INVITATION_REQUIRED"},
		"код недействителен": {application.ErrInvitationInvalid, "INVITATION_INVALID"},
		"почта занята":       {application.ErrEmailConflict, "EMAIL_ALREADY_REGISTERED"},
	} {
		response := call(t, stubAuthentication{registerErr: tc.err}, http.MethodPost, "/register",
			`{"email":"new@example.com","password":"very-secure-password","displayName":"New"}`)
		wantStatus := http.StatusForbidden
		if tc.want == "EMAIL_ALREADY_REGISTERED" {
			wantStatus = http.StatusConflict
		}
		if response.Code != wantStatus || errorCode(t, response) != tc.want {
			t.Errorf("%s: %d %s, want %d %s", name, response.Code, response.Body.String(), wantStatus, tc.want)
		}
		if len(response.Result().Cookies()) != 0 {
			t.Errorf("%s: отказ не должен выдавать сессию", name)
		}
	}
}

// canCreateOrganization решает, куда интерфейс ведёт пользователя без организации: к созданию
// или к вводу кода сотрудника. В open право не проверяется, в invite_only его даёт код платформы.
func TestMeReportsWhetherTheUserMayCreateAnOrganization(t *testing.T) {
	for name, tc := range map[string]struct {
		stub stubAuthentication
		want bool
	}{
		"открытый режим без права":     {stubAuthentication{mode: application.RegistrationOpen}, true},
		"закрытый без права":           {stubAuthentication{mode: application.RegistrationInviteOnly}, false},
		"закрытый с правом":            {stubAuthentication{mode: application.RegistrationInviteOnly, rights: 1}, true},
		"режим не настроен, права нет": {stubAuthentication{}, false},
	} {
		response := call(t, tc.stub, http.MethodGet, "/me", "")
		var payload struct {
			CanCreateOrganization *bool `json:"canCreateOrganization"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &payload) != nil ||
			payload.CanCreateOrganization == nil || *payload.CanCreateOrganization != tc.want {
			t.Errorf("%s: %d %s, want canCreateOrganization=%v", name, response.Code, response.Body.String(), tc.want)
		}
		if strings.Contains(response.Body.String(), "organizationRights") {
			t.Errorf("%s: счётчик права не должен выходить наружу: %s", name, response.Body.String())
		}
	}
}
