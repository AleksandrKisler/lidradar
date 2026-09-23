package integration_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type onboardingStatus struct {
	Complete bool    `json:"complete"`
	NextStep *string `json:"nextStep"`
	Steps    []struct {
		Key      string `json:"key"`
		Required bool   `json:"required"`
		Done     bool   `json:"done"`
	} `json:"steps"`
	Facts struct {
		ActiveLocations       int  `json:"activeLocations"`
		LocationsWithSchedule int  `json:"locationsWithSchedule"`
		ActiveServices        int  `json:"activeServices"`
		LiveConnections       int  `json:"liveConnections"`
		TelegramLinked        bool `json:"telegramLinked"`
	} `json:"facts"`
}

func fetchOnboarding(t *testing.T, fixture apiFixture, cookie *http.Cookie, tenantID string) onboardingStatus {
	t.Helper()
	response := request(t, fixture.handler, http.MethodGet, "/api/v1/organization/onboarding", "", cookie, tenantID)
	requireStatus(t, response, http.StatusOK)
	var status onboardingStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	return status
}

// Онбординг, безопасное подключение канала и команда организации через HTTP
// (ADR 0045, GAP-API-008/009/010, GAP-CONTRACT-019).
func TestOnboardingTeamAndSecureConnectThroughAPI(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "team-owner@example.com", "Владелец команды")
	tenantID := createOrganization(t, fixture, owner, "Организация команды")

	initial := fetchOnboarding(t, fixture, owner.Cookie, tenantID)
	if initial.Complete || initial.NextStep == nil || *initial.NextStep != "LOCATION" || len(initial.Steps) != 5 ||
		!initial.Steps[0].Done || initial.Steps[4].Required {
		t.Fatalf("начальный онбординг = %+v", initial)
	}

	// GAP-CONTRACT-019: active при создании точки отклоняется, а не игнорируется.
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/locations", `{
		"name":"Точка","timezone":"Europe/Moscow","active":false
	}`, owner.Cookie, tenantID), http.StatusBadRequest)
	locationID := createLocation(t, fixture, owner, tenantID, "Основная точка")
	afterLocation := fetchOnboarding(t, fixture, owner.Cookie, tenantID)
	if afterLocation.Facts.ActiveLocations != 1 || afterLocation.Facts.LocationsWithSchedule != 0 || *afterLocation.NextStep != "LOCATION" {
		t.Fatalf("точка без графика засчитана: %+v", afterLocation)
	}
	requireStatus(t, request(t, fixture.handler, http.MethodPut, "/api/v1/locations/"+locationID+"/business-hours", `{
		"timezone":"Europe/Moscow","days":[
			{"weekday":1,"closed":false,"opensAt":"09:00","closesAt":"21:00"},
			{"weekday":2,"closed":false,"opensAt":"09:00","closesAt":"21:00"},
			{"weekday":3,"closed":false,"opensAt":"09:00","closesAt":"21:00"},
			{"weekday":4,"closed":false,"opensAt":"09:00","closesAt":"21:00"},
			{"weekday":5,"closed":false,"opensAt":"09:00","closesAt":"21:00"},
			{"weekday":6,"closed":true},
			{"weekday":7,"closed":true}
		]
	}`, owner.Cookie, tenantID), http.StatusOK)
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/services", `{"name":"Полировка"}`, owner.Cookie, tenantID), http.StatusCreated)
	if status := fetchOnboarding(t, fixture, owner.Cookie, tenantID); status.Complete || *status.NextStep != "CHANNEL" {
		t.Fatalf("онбординг перед каналом = %+v", status)
	}

	// GAP-API-010: секрет webhook выпускает сервер и показывает один раз.
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{
		"name":"Форма сайта","locationId":"`+locationID+`"
	}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	var connection struct {
		ID            string  `json:"id"`
		Status        string  `json:"status"`
		WebhookSecret *string `json:"webhookSecret"`
	}
	if err := json.Unmarshal(connected.Body.Bytes(), &connection); err != nil {
		t.Fatal(err)
	}
	if connection.WebhookSecret == nil || len(*connection.WebhookSecret) != 43 || connection.Status != "ACTIVE" {
		t.Fatalf("подключение без выпущенного секрета: %s", connected.Body.String())
	}
	listed := request(t, fixture.handler, http.MethodGet, "/api/v1/integrations", "", owner.Cookie, tenantID)
	requireStatus(t, listed, http.StatusOK)
	if strings.Contains(listed.Body.String(), *connection.WebhookSecret) || strings.Contains(listed.Body.String(), "webhookSecret") {
		t.Fatalf("список подключений раскрыл секрет: %s", listed.Body.String())
	}
	webhookPath := "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenantID + "/" + connection.ID
	payload := canonicalWebhook("team-event-1", "message.received.v1", "team-dialog", "team-message-1", "team-contact", "INCOMING", "TEXT", "Здравствуйте", "2026-09-18T10:00:00Z", "")
	requireStatus(t, webhookRequest(t, fixture.handler, webhookPath, payload, "X-LidRadar-Webhook-Secret", *connection.WebhookSecret), http.StatusAccepted)
	requireStatus(t, webhookRequest(t, fixture.handler, webhookPath, payload, "X-LidRadar-Webhook-Secret", "wrong-secret-1234567890"), http.StatusUnauthorized)
	check := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/"+connection.ID+"/health/check", "", owner.Cookie, tenantID)
	requireStatus(t, check, http.StatusOK)
	if !strings.Contains(check.Body.String(), `"verification":"LOCAL"`) || !strings.Contains(check.Body.String(), `"status":"ACTIVE"`) {
		t.Fatalf("проверка связи = %s", check.Body.String())
	}
	completed := fetchOnboarding(t, fixture, owner.Cookie, tenantID)
	if !completed.Complete || completed.NextStep == nil || *completed.NextStep != "TELEGRAM_LINK" || completed.Facts.LiveConnections != 1 {
		t.Fatalf("онбординг после канала = %+v", completed)
	}

	// Команда: список, приглашение, приём без X-Tenant-ID, роли и защита владельца.
	members := request(t, fixture.handler, http.MethodGet, "/api/v1/organization/members", "", owner.Cookie, tenantID)
	requireStatus(t, members, http.StatusOK)
	if !strings.Contains(members.Body.String(), `"email":"team-owner@example.com"`) || !strings.Contains(members.Body.String(), `"role":"OWNER"`) {
		t.Fatalf("список участников = %s", members.Body.String())
	}
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/organization/invitations", `{"role":"ADMIN"}`, owner.Cookie, tenantID), http.StatusBadRequest)
	invited := request(t, fixture.handler, http.MethodPost, "/api/v1/organization/invitations", `{"role":"MANAGER","note":"Смена 2"}`, owner.Cookie, tenantID)
	requireStatus(t, invited, http.StatusCreated)
	var issued struct {
		Invitation struct {
			ID        string `json:"id"`
			Role      string `json:"role"`
			Status    string `json:"status"`
			ExpiresAt string `json:"expiresAt"`
			CodeHash  string `json:"codeHash"`
		} `json:"invitation"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(invited.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if len(issued.Code) != 43 || issued.Invitation.Status != "PENDING" || issued.Invitation.Role != "MANAGER" || issued.Invitation.CodeHash != "" ||
		strings.Contains(invited.Body.String(), "codeHash") {
		t.Fatalf("выпущенное приглашение = %s", invited.Body.String())
	}
	invitations := request(t, fixture.handler, http.MethodGet, "/api/v1/organization/invitations", "", owner.Cookie, tenantID)
	requireStatus(t, invitations, http.StatusOK)
	if strings.Contains(invitations.Body.String(), issued.Code) || !strings.Contains(invitations.Body.String(), `"note":"Смена 2"`) {
		t.Fatalf("список приглашений = %s", invitations.Body.String())
	}

	manager := register(t, fixture.handler, "team-manager@example.com", "Менеджер команды")
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/invitations/accept", `{"code":"short"}`, manager.Cookie, ""), http.StatusBadRequest)
	unknown := request(t, fixture.handler, http.MethodPost, "/api/v1/invitations/accept", `{"code":"`+strings.Repeat("A", 43)+`"}`, manager.Cookie, "")
	requireStatus(t, unknown, http.StatusNotFound)
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/invitations/accept", `{"code":"`+issued.Code+`"}`, nil, ""), http.StatusUnauthorized)
	accepted := request(t, fixture.handler, http.MethodPost, "/api/v1/invitations/accept", `{"code":"`+issued.Code+`"}`, manager.Cookie, "")
	requireStatus(t, accepted, http.StatusOK)
	if !strings.Contains(accepted.Body.String(), `"tenantId":"`+tenantID+`"`) || !strings.Contains(accepted.Body.String(), `"role":"MANAGER"`) ||
		!strings.Contains(accepted.Body.String(), `"organizationName":"Организация команды"`) {
		t.Fatalf("приём приглашения = %s", accepted.Body.String())
	}
	me := request(t, fixture.handler, http.MethodGet, "/api/v1/auth/me", "", manager.Cookie, "")
	requireStatus(t, me, http.StatusOK)
	if !strings.Contains(me.Body.String(), tenantID) {
		t.Fatalf("/auth/me менеджера без новой организации: %s", me.Body.String())
	}
	reused := request(t, fixture.handler, http.MethodPost, "/api/v1/invitations/accept", `{"code":"`+issued.Code+`"}`, manager.Cookie, "")
	requireStatus(t, reused, http.StatusConflict)
	if !strings.Contains(reused.Body.String(), "INVITATION_USED") {
		t.Fatalf("повторный приём = %s", reused.Body.String())
	}
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/organization/members", "", manager.Cookie, tenantID), http.StatusForbidden)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/organization/invitations", "", manager.Cookie, tenantID), http.StatusForbidden)
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/organization/invitations", `{"role":"MANAGER"}`, manager.Cookie, tenantID), http.StatusForbidden)
	if status := fetchOnboarding(t, fixture, manager.Cookie, tenantID); !status.Complete || status.Facts.TelegramLinked {
		t.Fatalf("онбординг для менеджера = %+v", status)
	}
	membersAfter := request(t, fixture.handler, http.MethodGet, "/api/v1/organization/members", "", owner.Cookie, tenantID)
	requireStatus(t, membersAfter, http.StatusOK)
	if !strings.Contains(membersAfter.Body.String(), `"email":"team-manager@example.com"`) {
		t.Fatalf("новый участник отсутствует: %s", membersAfter.Body.String())
	}

	lastOwner := request(t, fixture.handler, http.MethodPatch, "/api/v1/organization/members/"+owner.ID, `{"role":"MANAGER"}`, owner.Cookie, tenantID)
	requireStatus(t, lastOwner, http.StatusConflict)
	if !strings.Contains(lastOwner.Body.String(), "LAST_OWNER") {
		t.Fatalf("понижение последнего владельца = %s", lastOwner.Body.String())
	}
	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/organization/members/"+owner.ID, "", owner.Cookie, tenantID), http.StatusConflict)
	requireStatus(t, request(t, fixture.handler, http.MethodPatch, "/api/v1/organization/members/"+manager.ID, `{"role":"CEO"}`, owner.Cookie, tenantID), http.StatusBadRequest)
	promoted := request(t, fixture.handler, http.MethodPatch, "/api/v1/organization/members/"+manager.ID, `{"role":"OWNER"}`, owner.Cookie, tenantID)
	requireStatus(t, promoted, http.StatusOK)
	if !strings.Contains(promoted.Body.String(), `"role":"OWNER"`) {
		t.Fatalf("повышение = %s", promoted.Body.String())
	}
	requireStatus(t, request(t, fixture.handler, http.MethodPatch, "/api/v1/organization/members/"+owner.ID, `{"role":"MANAGER"}`, manager.Cookie, tenantID), http.StatusOK)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/organization/members", "", owner.Cookie, tenantID), http.StatusForbidden)
	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/organization/members/"+owner.ID, "", manager.Cookie, tenantID), http.StatusNoContent)
	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/organization/members/"+owner.ID, "", manager.Cookie, tenantID), http.StatusNoContent)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/organization", "", owner.Cookie, tenantID), http.StatusForbidden)
	meRevoked := request(t, fixture.handler, http.MethodGet, "/api/v1/auth/me", "", owner.Cookie, "")
	requireStatus(t, meRevoked, http.StatusOK)
	if strings.Contains(meRevoked.Body.String(), tenantID) {
		t.Fatalf("отозванное членство осталось в /auth/me: %s", meRevoked.Body.String())
	}
	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/organization/members/"+manager.ID, "", manager.Cookie, tenantID), http.StatusConflict)

	// Отзыв приглашения и восстановление отозванного участника новым кодом.
	pending := request(t, fixture.handler, http.MethodPost, "/api/v1/organization/invitations", `{"role":"OWNER"}`, manager.Cookie, tenantID)
	requireStatus(t, pending, http.StatusCreated)
	if err := json.Unmarshal(pending.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/organization/invitations/"+issued.Invitation.ID, "", manager.Cookie, tenantID), http.StatusNoContent)
	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/organization/invitations/"+issued.Invitation.ID, "", manager.Cookie, tenantID), http.StatusNoContent)
	revokedAccept := request(t, fixture.handler, http.MethodPost, "/api/v1/invitations/accept", `{"code":"`+issued.Code+`"}`, owner.Cookie, "")
	requireStatus(t, revokedAccept, http.StatusConflict)
	if !strings.Contains(revokedAccept.Body.String(), "INVITATION_REVOKED") {
		t.Fatalf("приём отозванного = %s", revokedAccept.Body.String())
	}
	fresh := request(t, fixture.handler, http.MethodPost, "/api/v1/organization/invitations", `{"role":"OWNER"}`, manager.Cookie, tenantID)
	requireStatus(t, fresh, http.StatusCreated)
	if err := json.Unmarshal(fresh.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/invitations/accept", `{"code":"`+issued.Code+`"}`, owner.Cookie, ""), http.StatusOK)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/organization/members", "", owner.Cookie, tenantID), http.StatusOK)
	restored := request(t, fixture.handler, http.MethodGet, "/api/v1/organization/invitations", "", owner.Cookie, tenantID)
	requireStatus(t, restored, http.StatusOK)
	if !strings.Contains(restored.Body.String(), `"status":"ACCEPTED"`) || !strings.Contains(restored.Body.String(), `"status":"REVOKED"`) {
		t.Fatalf("состояния приглашений = %s", restored.Body.String())
	}

	stranger := register(t, fixture.handler, "team-stranger@example.com", "Посторонний")
	otherTenant := createOrganization(t, fixture, stranger, "Чужая организация")
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/organization/members", "", stranger.Cookie, tenantID), http.StatusForbidden)
	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/organization/invitations/"+issued.Invitation.ID, "", stranger.Cookie, otherTenant), http.StatusNotFound)

	for operation, want := range map[string]int{
		"MEMBER_INVITED": 3, "INVITATION_ACCEPTED": 2, "INVITATION_REVOKED": 1, "MEMBER_ROLE_CHANGED": 2, "MEMBER_REVOKED": 1,
	} {
		if got := auditCount(t, fixture, tenantID, operation); got != want {
			t.Fatalf("аудит %s = %d, ожидалось %d", operation, got, want)
		}
	}
}
