package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	adminapplication "lidradar/backend/internal/admin/application"
	admininfrastructure "lidradar/backend/internal/admin/infrastructure"
	"lidradar/backend/platform/ids"
)

// Регистрация по приглашениям через весь HTTP-стек в режиме invite_only (ADR 0057): оба вида кодов,
// право на организацию, одинаковый отказ для любой причины и отсутствие следов у несостоявшейся регистрации.

func adminService(fixture apiFixture) adminapplication.Service {
	return adminapplication.NewService(admininfrastructure.NewPostgresStore(fixture.pool), ids.Generator{}, time.Now)
}

// issuePlatformCode выпускает код тем же путём, что и команда platform-admin invite.
func issuePlatformCode(t *testing.T, fixture apiFixture, note string) string {
	t.Helper()
	_, code, err := adminService(fixture).IssuePlatformInvitationFromCLI(context.Background(), note, 14)
	if err != nil {
		t.Fatalf("выпуск кода платформы: %v", err)
	}
	return code
}

func registerRequest(t *testing.T, fixture apiFixture, email, code string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"email":"` + email + `","password":"very-secure-password","displayName":"Приглашённый"`
	if code != "" {
		body += `,"inviteCode":"` + code + `"`
	}
	return request(t, fixture.handler, http.MethodPost, "/api/v1/auth/register", body+`}`, nil, "")
}

func registeredWith(t *testing.T, fixture apiFixture, email, code string) registeredUser {
	t.Helper()
	response := registerRequest(t, fixture, email, code)
	requireStatus(t, response, http.StatusCreated)
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookie регистрации = %#v", cookies)
	}
	return registeredUser{ID: nestedUserID(t, response), Cookie: cookies[0]}
}

type meView struct {
	CanCreateOrganization bool `json:"canCreateOrganization"`
	Memberships           []struct {
		TenantID string `json:"tenantId"`
		Role     string `json:"role"`
	} `json:"memberships"`
}

func fetchMe(t *testing.T, fixture apiFixture, user registeredUser) meView {
	t.Helper()
	response := request(t, fixture.handler, http.MethodGet, "/api/v1/auth/me", "", user.Cookie, "")
	requireStatus(t, response, http.StatusOK)
	var view meView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatalf("разбор /auth/me: %v: %s", err, response.Body.String())
	}
	return view
}

func requireErrorCode(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	requireStatus(t, response, status)
	if !strings.Contains(response.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("ожидался код %s, тело = %s", code, response.Body.String())
	}
	if cookies := response.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("отказ не должен выдавать сессию: %#v", cookies)
	}
}

func userCount(t *testing.T, fixture apiFixture) int {
	t.Helper()
	var count int
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestInviteOnlyRegistrationRequiresAnInvitationCode(t *testing.T) {
	fixture := newInviteOnlyAPIFixture(t)

	mode := request(t, fixture.handler, http.MethodGet, "/api/v1/auth/registration", "", nil, "") // без сессии
	requireStatus(t, mode, http.StatusOK)
	if !strings.Contains(mode.Body.String(), `"mode":"invite_only"`) {
		t.Fatalf("режим = %s", mode.Body.String())
	}

	requireErrorCode(t, registerRequest(t, fixture, "nobody@example.com", ""), http.StatusForbidden, "INVITATION_REQUIRED")
	requireErrorCode(t, registerRequest(t, fixture, "nobody@example.com", "short"), http.StatusForbidden, "INVITATION_INVALID")
	requireErrorCode(t, registerRequest(t, fixture, "nobody@example.com", strings.Repeat("A", 43)), http.StatusForbidden, "INVITATION_INVALID")
	if got := userCount(t, fixture); got != 0 {
		t.Fatalf("отказы создали пользователей: %d", got)
	}
}

func TestPlatformCodeGivesOneOrganizationAndIsSingleUse(t *testing.T) {
	fixture := newInviteOnlyAPIFixture(t)
	code := issuePlatformCode(t, fixture, "Салон «Линия»")

	owner := registeredWith(t, fixture, "owner@example.com", code)
	if view := fetchMe(t, fixture, owner); !view.CanCreateOrganization || len(view.Memberships) != 0 {
		t.Fatalf("после регистрации по коду платформы: %+v", view)
	}

	// Тот же код другому человеку не подходит, и аккаунт при этом не создаётся.
	requireErrorCode(t, registerRequest(t, fixture, "second@example.com", code), http.StatusForbidden, "INVITATION_INVALID")
	if got := userCount(t, fixture); got != 1 {
		t.Fatalf("пользователей %d, ожидался один", got)
	}

	createOrganization(t, fixture, owner, "Организация владельца")
	view := fetchMe(t, fixture, owner)
	if view.CanCreateOrganization || len(view.Memberships) != 1 || view.Memberships[0].Role != "OWNER" {
		t.Fatalf("после создания организации: %+v", view)
	}
	second := request(t, fixture.handler, http.MethodPost, "/api/v1/organizations", `{
		"name":"Вторая","defaultTimezone":"Europe/Moscow","defaultCurrency":"RUB"
	}`, owner.Cookie, "")
	requireStatus(t, second, http.StatusForbidden)
	if !strings.Contains(second.Body.String(), `"code":"ORGANIZATION_CREATION_NOT_ALLOWED"`) {
		t.Fatalf("вторая организация: %s", second.Body.String())
	}

	// Команда оператора allow-organization возвращает право ровно на одну организацию.
	if _, rights, err := adminService(fixture).AllowOrganizationFromCLI(context.Background(), "owner@example.com"); err != nil || rights != 1 {
		t.Fatalf("allow-organization: прав %d, %v", rights, err)
	}
	createOrganization(t, fixture, owner, "Вторая организация")

	var issued, granted int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE operation = 'PLATFORM_INVITATION_ISSUED' AND source = 'CLI'),
		       count(*) FILTER (WHERE operation = 'ORGANIZATION_RIGHT_GRANTED' AND source = 'CLI')
		FROM admin_audit_log`).Scan(&issued, &granted); err != nil || issued != 1 || granted != 1 {
		t.Fatalf("аудит оператора: выпусков %d, выдач %d, %v", issued, granted, err)
	}
	var leaked int
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM admin_audit_log WHERE details::text LIKE '%' || $1 || '%'`, code).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("код платформы попал в аудит: %d, %v", leaked, err)
	}
}

func TestRevokedAndExpiredPlatformCodesAreRefused(t *testing.T) {
	fixture := newInviteOnlyAPIFixture(t)
	revoked, expired := issuePlatformCode(t, fixture, "отзовём"), issuePlatformCode(t, fixture, "истечёт")

	invitations, err := adminService(fixture).PlatformInvitations(context.Background())
	if err != nil || len(invitations) != 2 {
		t.Fatalf("PlatformInvitations() = %+v, %v", invitations, err)
	}
	for _, invitation := range invitations {
		switch *invitation.Note {
		case "отзовём":
			if _, err := adminService(fixture).RevokePlatformInvitationFromCLI(context.Background(), invitation.ID); err != nil {
				t.Fatal(err)
			}
		case "истечёт":
			if _, err := fixture.pool.Exec(context.Background(), `
				UPDATE platform_invitations SET created_at = now() - interval '3 days', expires_at = now() - interval '1 day' WHERE id = $1`, invitation.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	requireErrorCode(t, registerRequest(t, fixture, "late@example.com", revoked), http.StatusForbidden, "INVITATION_INVALID")
	requireErrorCode(t, registerRequest(t, fixture, "late@example.com", expired), http.StatusForbidden, "INVITATION_INVALID")
	if got := userCount(t, fixture); got != 0 {
		t.Fatalf("отказы создали пользователей: %d", got)
	}
}

func TestMemberCodeRegistersAndJoinsWithoutTheRightToCreateOrganizations(t *testing.T) {
	fixture := newInviteOnlyAPIFixture(t)
	owner := registeredWith(t, fixture, "owner@example.com", issuePlatformCode(t, fixture, ""))
	tenantID := createOrganization(t, fixture, owner, "Студия")

	issue := func(role string) string {
		response := request(t, fixture.handler, http.MethodPost, "/api/v1/organization/invitations", `{"role":"`+role+`"}`, owner.Cookie, tenantID)
		requireStatus(t, response, http.StatusCreated)
		var issued struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &issued); err != nil || issued.Code == "" {
			t.Fatalf("код сотрудника: %v: %s", err, response.Body.String())
		}
		return issued.Code
	}
	code := issue("MANAGER")

	staff := registeredWith(t, fixture, "staff@example.com", code)
	view := fetchMe(t, fixture, staff)
	if view.CanCreateOrganization || len(view.Memberships) != 1 || view.Memberships[0].TenantID != tenantID || view.Memberships[0].Role != "MANAGER" {
		t.Fatalf("после регистрации по коду сотрудника: %+v", view)
	}
	forbidden := request(t, fixture.handler, http.MethodPost, "/api/v1/organizations", `{
		"name":"Чужая","defaultTimezone":"Europe/Moscow","defaultCurrency":"RUB"
	}`, staff.Cookie, "")
	requireStatus(t, forbidden, http.StatusForbidden)
	if !strings.Contains(forbidden.Body.String(), `"code":"ORGANIZATION_CREATION_NOT_ALLOWED"`) {
		t.Fatalf("сотрудник создал организацию: %s", forbidden.Body.String())
	}

	// Использованный код не принимается снова, а отказ не оставляет аккаунта: тот же адрес
	// позже спокойно регистрируется по новому коду, а не получает «почта занята».
	requireErrorCode(t, registerRequest(t, fixture, "other@example.com", code), http.StatusForbidden, "INVITATION_INVALID")
	revokedCode := issue("MANAGER")
	listed := request(t, fixture.handler, http.MethodGet, "/api/v1/organization/invitations", "", owner.Cookie, tenantID)
	requireStatus(t, listed, http.StatusOK)
	var invitations struct {
		Items []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &invitations); err != nil {
		t.Fatal(err)
	}
	for _, item := range invitations.Items {
		if item.Status == "PENDING" {
			requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/organization/invitations/"+item.ID, "", owner.Cookie, tenantID), http.StatusNoContent)
		}
	}
	requireErrorCode(t, registerRequest(t, fixture, "other@example.com", revokedCode), http.StatusForbidden, "INVITATION_INVALID")
	registeredWith(t, fixture, "other@example.com", issue("OWNER"))
}

// Занятая почта не сжигает код сотрудника: владельцу не нужно выпускать новый из-за опечатки.
func TestEmailConflictDoesNotBurnAMemberCode(t *testing.T) {
	fixture := newInviteOnlyAPIFixture(t)
	owner := registeredWith(t, fixture, "owner@example.com", issuePlatformCode(t, fixture, ""))
	tenantID := createOrganization(t, fixture, owner, "Студия")
	response := request(t, fixture.handler, http.MethodPost, "/api/v1/organization/invitations", `{"role":"MANAGER"}`, owner.Cookie, tenantID)
	requireStatus(t, response, http.StatusCreated)
	var issued struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}

	conflict := registerRequest(t, fixture, "OWNER@example.com", issued.Code)
	requireStatus(t, conflict, http.StatusConflict)
	if !strings.Contains(conflict.Body.String(), `"code":"EMAIL_ALREADY_REGISTERED"`) {
		t.Fatalf("занятая почта: %s", conflict.Body.String())
	}
	registeredWith(t, fixture, "free@example.com", issued.Code) // код всё ещё действует
}

// Регистрация по приглашению не мешает обычному входу и записывается в журнал входов.
func TestInvitedUserLogsInNormallyAndRegistrationIsAudited(t *testing.T) {
	fixture := newInviteOnlyAPIFixture(t)
	user := registeredWith(t, fixture, "owner@example.com", issuePlatformCode(t, fixture, ""))
	if cookie := login(t, fixture.handler, "owner@example.com"); cookie == nil {
		t.Fatal("вход не выдал сессию")
	}
	var registered int
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM auth_audit_log WHERE user_id = $1 AND operation = 'USER_REGISTERED'`, user.ID).Scan(&registered); err != nil || registered != 1 {
		t.Fatalf("записей USER_REGISTERED %d, %v", registered, err)
	}
}

// В открытом режиме пользователь без права по-прежнему создаёт организации, а режим сообщается как open.
func TestOpenModeKeepsTheLegacyFlow(t *testing.T) {
	fixture := newAPIFixture(t)
	mode := request(t, fixture.handler, http.MethodGet, "/api/v1/auth/registration", "", nil, "")
	requireStatus(t, mode, http.StatusOK)
	if !strings.Contains(mode.Body.String(), `"mode":"open"`) {
		t.Fatalf("режим = %s", mode.Body.String())
	}
	user := register(t, fixture.handler, "plain@example.com", "Обычный")
	if view := fetchMe(t, fixture, user); !view.CanCreateOrganization {
		t.Fatalf("в открытом режиме пользователь вправе создавать организации: %+v", view)
	}
	createOrganization(t, fixture, user, "Открытая")
	createOrganization(t, fixture, user, "Ещё одна")
}
