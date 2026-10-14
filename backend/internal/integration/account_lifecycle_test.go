package integration_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	mailapplication "lidradar/backend/internal/mail/application"
	"lidradar/backend/platform/ids"
)

// Выгрузка данных и закрытие учётной записи через весь HTTP-стек на настоящей PostgreSQL (ADR 0059): личная
// выгрузка, закрытие с отсрочкой и исполнением, блокировки и выгрузка данных организации.

const ownerPassword = "very-secure-password"

func newUUIDString(t *testing.T) string {
	t.Helper()
	id, err := ids.Generator{}.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func accountRequest(t *testing.T, fixture apiFixture, method, path, body string, cookie *http.Cookie) (code int, header http.Header, payload []byte) {
	t.Helper()
	response := request(t, fixture.handler, method, "/api/v1/auth/account"+path, body, cookie, "")
	return response.Code, response.Header(), response.Body.Bytes()
}

func passwordBody(password string) string { return jsonBody("password", password) }

func authMe(t *testing.T, fixture apiFixture, cookie *http.Cookie) (status int, dueAt *string, body string) {
	t.Helper()
	response := request(t, fixture.handler, http.MethodGet, "/api/v1/auth/me", "", cookie, "")
	var view struct {
		User struct {
			DeletionDueAt *string `json:"deletionDueAt"`
		} `json:"user"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &view)
	return response.Code, view.User.DeletionDueAt, response.Body.String()
}

func countRows(t *testing.T, fixture apiFixture, query string, arguments ...any) int {
	t.Helper()
	var count int
	if err := fixture.pool.QueryRow(context.Background(), query, arguments...).Scan(&count); err != nil {
		t.Fatalf("%s: %v", strings.Join(strings.Fields(query), " "), err)
	}
	return count
}

// mailWithSubject возвращает письма пачки с данной темой.
func mailWithSubject(batch []mailapplication.Outgoing, subject string) []mailapplication.Outgoing {
	var found []mailapplication.Outgoing
	for _, message := range batch {
		if message.Subject == subject {
			found = append(found, message)
		}
	}
	return found
}

// joinOrganization приглашает нового пользователя в организацию владельца ролью MANAGER и принимает приглашение.
func joinOrganization(t *testing.T, fixture apiFixture, owner registeredUser, tenantID, email, name string) registeredUser {
	t.Helper()
	invited := request(t, fixture.handler, http.MethodPost, "/api/v1/organization/invitations", `{"role":"MANAGER"}`, owner.Cookie, tenantID)
	requireStatus(t, invited, http.StatusCreated)
	var issued struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(invited.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	member := register(t, fixture.handler, email, name)
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/invitations/accept", `{"code":"`+issued.Code+`"}`, member.Cookie, ""), http.StatusOK)
	return member
}

func TestPersonalExportThroughTheWholeStack(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "export-owner@example.com", "Владелец выгрузки")
	createOrganization(t, fixture, owner, "Организация выгрузки")
	other := register(t, fixture.handler, "export-other@example.com", "Посторонний")
	createOrganization(t, fixture, other, "Чужая организация")

	if code, _, _ := accountRequest(t, fixture, http.MethodPost, "/export", passwordBody(ownerPassword), nil); code != http.StatusUnauthorized {
		t.Fatalf("без сессии: %d", code)
	}
	code, _, body := accountRequest(t, fixture, http.MethodPost, "/export", passwordBody("wrong-password-value"), owner.Cookie)
	if code != http.StatusForbidden || !strings.Contains(string(body), `"code":"INVALID_CURRENT_PASSWORD"`) {
		t.Fatalf("неверный пароль: %d %s", code, body)
	}
	if code, _, _ := accountRequest(t, fixture, http.MethodPost, "/export", `{"password":""}`, owner.Cookie); code != http.StatusBadRequest {
		t.Fatalf("пустой пароль: %d", code)
	}

	code, header, body := accountRequest(t, fixture, http.MethodPost, "/export", passwordBody(ownerPassword), owner.Cookie)
	if code != http.StatusOK {
		t.Fatalf("выгрузка: %d %s", code, body)
	}
	if header.Get("Content-Type") != "application/json; charset=utf-8" || header.Get("Cache-Control") != "no-store" ||
		!strings.HasPrefix(header.Get("Content-Disposition"), `attachment; filename="lidradar-account-`) {
		t.Errorf("заголовки: %v", header)
	}
	var document struct {
		Format  string `json:"format"`
		Account struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"account"`
		Memberships struct {
			Items []struct {
				Organization string `json:"organization"`
				Role         string `json:"role"`
			} `json:"items"`
		} `json:"memberships"`
		Sessions struct {
			Items []struct {
				IPAddress string `json:"ipAddress"`
			} `json:"items"`
		} `json:"sessions"`
		AuthEvents struct {
			Items []struct {
				Operation string `json:"operation"`
			} `json:"items"`
		} `json:"authEvents"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("документ не разобрался: %v", err)
	}
	if document.Format != "lidradar.account-export/1" || document.Account.ID != owner.ID || document.Account.Email != "export-owner@example.com" ||
		len(document.Memberships.Items) != 1 || document.Memberships.Items[0].Organization != "Организация выгрузки" || document.Memberships.Items[0].Role != "OWNER" ||
		len(document.Sessions.Items) == 0 || document.Sessions.Items[0].IPAddress == "" {
		t.Errorf("документ = %s", body)
	}
	var operations []string
	for _, event := range document.AuthEvents.Items {
		operations = append(operations, event.Operation)
	}
	if !strings.Contains(strings.Join(operations, ","), "USER_REGISTERED") {
		t.Errorf("события входа: %v", operations)
	}
	for _, forbidden := range []string{"export-other", "Чужая организация", "Посторонний", owner.Cookie.Value, "argon2", "tenant", "passwordHash", "tokenHash"} {
		if strings.Contains(string(body), forbidden) {
			t.Errorf("в личной выгрузке есть %q", forbidden)
		}
	}
	// След выгрузки записан, и он идёт раньше выдачи данных (запись есть к моменту ответа).
	if got := countRows(t, fixture, `SELECT count(*) FROM auth_audit_log WHERE user_id = $1 AND operation = 'ACCOUNT_DATA_EXPORTED' AND ip_address IS NOT NULL`, owner.ID); got != 1 {
		t.Errorf("записей ACCOUNT_DATA_EXPORTED: %d", got)
	}

	// Три выгрузки в час: четвёртая получает 429 с сроком повтора, чужой счётчик не затронут.
	for attempt := 2; attempt <= 3; attempt++ {
		if code, _, _ := accountRequest(t, fixture, http.MethodPost, "/export", passwordBody(ownerPassword), owner.Cookie); code != http.StatusOK {
			t.Fatalf("выгрузка %d: %d", attempt, code)
		}
	}
	code, header, _ = accountRequest(t, fixture, http.MethodPost, "/export", passwordBody(ownerPassword), owner.Cookie)
	if code != http.StatusTooManyRequests || header.Get("Retry-After") == "" {
		t.Errorf("четвёртая выгрузка: %d, Retry-After=%q", code, header.Get("Retry-After"))
	}
	if code, _, _ := accountRequest(t, fixture, http.MethodPost, "/export", passwordBody(ownerPassword), other.Cookie); code != http.StatusOK {
		t.Errorf("предел одного пользователя затронул другого: %d", code)
	}
}

func TestAccountClosureFromRequestToAnonymization(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "closure-owner@example.com", "Владелец закрытия")
	tenantID := createOrganization(t, fixture, owner, "Организация закрытия")
	member := joinOrganization(t, fixture, owner, tenantID, "closure-member@example.com", "Закрывающий")
	deliver(t, fixture) // письма регистрации не нужны

	// Неверный пароль, затем верный: срок на 30 суток вперёд.
	code, _, body := accountRequest(t, fixture, http.MethodPost, "/deletion", passwordBody("wrong-password-value"), member.Cookie)
	if code != http.StatusForbidden {
		t.Fatalf("неверный пароль: %d %s", code, body)
	}
	started := time.Now()
	code, _, body = accountRequest(t, fixture, http.MethodPost, "/deletion", passwordBody(ownerPassword), member.Cookie)
	if code != http.StatusOK {
		t.Fatalf("запрос закрытия: %d %s", code, body)
	}
	var scheduled struct {
		DeletionDueAt time.Time `json:"deletionDueAt"`
	}
	if err := json.Unmarshal(body, &scheduled); err != nil {
		t.Fatal(err)
	}
	if want := started.Add(30 * 24 * time.Hour); scheduled.DeletionDueAt.Before(want.Add(-time.Minute)) || scheduled.DeletionDueAt.After(want.Add(time.Minute)) {
		t.Fatalf("срок = %s, ожидалось около %s", scheduled.DeletionDueAt, want)
	}
	if status, due, _ := authMe(t, fixture, member.Cookie); status != http.StatusOK || due == nil {
		t.Fatalf("/auth/me во время отсрочки: %d, срок %v", status, due)
	}
	requested := mailWithSubject(deliver(t, fixture), "Запрошено удаление аккаунта LidRadar")
	if len(requested) != 1 || requested[0].To != "closure-member@example.com" ||
		!strings.Contains(requested[0].Text, integrationWebBaseURL+"/settings/account") {
		t.Fatalf("письмо о запросе: %+v", requested)
	}

	// Повтор возвращает прежний срок и не шлёт писем; вход во время отсрочки работает.
	code, _, again := accountRequest(t, fixture, http.MethodPost, "/deletion", passwordBody(ownerPassword), member.Cookie)
	var repeated struct {
		DeletionDueAt time.Time `json:"deletionDueAt"`
	}
	if err := json.Unmarshal(again, &repeated); err != nil || code != http.StatusOK || !repeated.DeletionDueAt.Equal(scheduled.DeletionDueAt) {
		t.Fatalf("повторный запрос: %d %s", code, again)
	}
	if extra := deliver(t, fixture); len(extra) != 0 {
		t.Errorf("повторный запрос отправил письма: %+v", extra)
	}
	requireStatus(t, loginWith(t, fixture, "closure-member@example.com", ownerPassword), http.StatusOK)

	// Отмена снимает назначение, пишет письмо и идемпотентна.
	if code, _, _ := accountRequest(t, fixture, http.MethodDelete, "/deletion", "", member.Cookie); code != http.StatusNoContent {
		t.Fatalf("отмена: %d", code)
	}
	if _, due, _ := authMe(t, fixture, member.Cookie); due != nil {
		t.Errorf("срок после отмены: %v", due)
	}
	if cancelled := mailWithSubject(deliver(t, fixture), "Удаление аккаунта LidRadar отменено"); len(cancelled) != 1 {
		t.Errorf("писем об отмене: %d", len(cancelled))
	}
	if code, _, _ := accountRequest(t, fixture, http.MethodDelete, "/deletion", "", member.Cookie); code != http.StatusNoContent {
		t.Fatalf("повторная отмена: %d", code)
	}
	if extra := deliver(t, fixture); len(extra) != 0 {
		t.Errorf("повторная отмена отправила письма: %+v", extra)
	}

	// Новый запрос и исполнение, когда срок наступил.
	if code, _, body := accountRequest(t, fixture, http.MethodPost, "/deletion", passwordBody(ownerPassword), member.Cookie); code != http.StatusOK {
		t.Fatalf("повторный запрос после отмены: %d %s", code, body)
	}
	deliver(t, fixture)
	processed, err := fixture.executor.ProcessDueDeletions(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("ProcessDueDeletions() = %d, %v", processed, err)
	}

	// Учётной записи больше нет: вход, старая сессия, пароль сброса.
	requireStatus(t, loginWith(t, fixture, "closure-member@example.com", ownerPassword), http.StatusUnauthorized)
	if status, _, _ := authMe(t, fixture, member.Cookie); status != http.StatusUnauthorized {
		t.Errorf("старая сессия после обезличивания: %d", status)
	}
	requireStatus(t, post(t, fixture, "/password/forgot", jsonBody("email", "closure-member@example.com"), nil), http.StatusNoContent)
	if mails := mailTo(t, deliver(t, fixture), "closure-member@example.com"); mails.Subject != "Аккаунт LidRadar удалён" {
		t.Errorf("после исполнения ушло письмо %q, а не «Аккаунт LidRadar удалён»", mails.Subject)
	}
	var email, name, status string
	if err := fixture.pool.QueryRow(context.Background(), `SELECT email, display_name, status FROM users WHERE id = $1`, member.ID).Scan(&email, &name, &status); err != nil {
		t.Fatal(err)
	}
	if email != member.ID+"@deleted.invalid" || name != "Удалён" || status != "DISABLED" {
		t.Errorf("пользователь после обезличивания: %q %q %q", email, name, status)
	}
	for _, query := range []string{
		`SELECT count(*) FROM sessions WHERE user_id = $1`, `SELECT count(*) FROM account_tokens WHERE user_id = $1`,
		`SELECT count(*) FROM memberships WHERE user_id = $1 AND status = 'ACTIVE'`,
	} {
		if got := countRows(t, fixture, query, member.ID); got != 0 {
			t.Errorf("%s: осталось %d", query, got)
		}
	}
	for _, operation := range []string{"ACCOUNT_DELETION_REQUESTED", "ACCOUNT_DELETION_CANCELLED", "ACCOUNT_ANONYMIZED"} {
		if got := countRows(t, fixture, `SELECT count(*) FROM auth_audit_log WHERE user_id = $1 AND operation = $2`, member.ID, operation); got == 0 {
			t.Errorf("в журнале нет %s", operation)
		}
	}

	// Организация продолжает работать, а закрытый участник виден в списке без адреса.
	members := request(t, fixture.handler, http.MethodGet, "/api/v1/organization/members", "", owner.Cookie, tenantID)
	requireStatus(t, members, http.StatusOK)
	var team struct {
		Items []struct {
			UserID      string `json:"userId"`
			Email       string `json:"email"`
			DisplayName string `json:"displayName"`
			Status      string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(members.Body.Bytes(), &team); err != nil {
		t.Fatal(err)
	}
	foundClosed := false
	for _, item := range team.Items {
		if item.UserID == member.ID {
			foundClosed = true
			if item.Email != "" || item.DisplayName != "Удалён" || item.Status != "DISABLED" {
				t.Errorf("закрытый участник в списке: %+v", item)
			}
		}
	}
	if !foundClosed || strings.Contains(members.Body.String(), "deleted.invalid") {
		t.Errorf("список участников: %s", members.Body.String())
	}
	// Освободившийся адрес можно зарегистрировать заново.
	reborn := register(t, fixture.handler, "closure-member@example.com", "Новый человек")
	if reborn.ID == member.ID {
		t.Error("повторная регистрация вернула прежнюю учётную запись")
	}
}

func TestSoleOwnerCannotCloseTheAccountUntilOwnershipIsHandedOver(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "sole-owner@example.com", "Единственный владелец")
	tenantID := createOrganization(t, fixture, owner, "Салон Лотос")
	member := joinOrganization(t, fixture, owner, tenantID, "sole-member@example.com", "Менеджер")
	deliver(t, fixture)

	code, _, body := accountRequest(t, fixture, http.MethodPost, "/deletion", passwordBody(ownerPassword), owner.Cookie)
	if code != http.StatusConflict {
		t.Fatalf("запрос единственного владельца: %d %s", code, body)
	}
	var blocked struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Blockers []struct {
					Code             string `json:"code"`
					OrganizationName string `json:"organizationName"`
					HasOtherMembers  bool   `json:"hasOtherMembers"`
				} `json:"blockers"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &blocked); err != nil {
		t.Fatal(err)
	}
	if blocked.Error.Code != "ACCOUNT_DELETION_BLOCKED" || len(blocked.Error.Details.Blockers) != 1 ||
		blocked.Error.Details.Blockers[0].Code != "SOLE_OWNER" || blocked.Error.Details.Blockers[0].OrganizationName != "Салон Лотос" ||
		!blocked.Error.Details.Blockers[0].HasOtherMembers {
		t.Fatalf("блокировка = %s", body)
	}
	if strings.Contains(string(body), tenantID) {
		t.Errorf("идентификатор организации в ответе: %s", body)
	}
	if _, due, _ := authMe(t, fixture, owner.Cookie); due != nil {
		t.Errorf("отклонённый запрос назначил срок: %v", due)
	}
	if extra := deliver(t, fixture); len(extra) != 0 {
		t.Errorf("отклонённый запрос отправил письма: %+v", extra)
	}

	// Владение передано: второй владелец снимает блокировку.
	requireStatus(t, request(t, fixture.handler, http.MethodPatch, "/api/v1/organization/members/"+member.ID, `{"role":"OWNER"}`, owner.Cookie, tenantID), http.StatusOK)
	if code, _, body := accountRequest(t, fixture, http.MethodPost, "/deletion", passwordBody(ownerPassword), owner.Cookie); code != http.StatusOK {
		t.Fatalf("запрос после передачи владения: %d %s", code, body)
	}
	deliver(t, fixture)

	// До срока второго владельца понизили: исполнитель снимает запрос и пишет, почему.
	requireStatus(t, request(t, fixture.handler, http.MethodPatch, "/api/v1/organization/members/"+member.ID, `{"role":"MANAGER"}`, owner.Cookie, tenantID), http.StatusOK)
	processed, err := fixture.executor.ProcessDueDeletions(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("ProcessDueDeletions() = %d, %v", processed, err)
	}
	if status, due, _ := authMe(t, fixture, owner.Cookie); status != http.StatusOK || due != nil {
		t.Errorf("владелец после блокировки при исполнении: %d, срок %v", status, due)
	}
	requireStatus(t, loginWith(t, fixture, "sole-owner@example.com", ownerPassword), http.StatusOK)
	notice := mailWithSubject(deliver(t, fixture), "Удаление аккаунта LidRadar не выполнено")
	if len(notice) != 1 || notice[0].To != "sole-owner@example.com" || !strings.Contains(notice[0].Text, "«Салон Лотос»") {
		t.Fatalf("письмо о блокировке: %+v", notice)
	}
	if got := countRows(t, fixture, `SELECT count(*) FROM auth_audit_log WHERE user_id = $1 AND operation = 'ACCOUNT_DELETION_BLOCKED'`, owner.ID); got != 1 {
		t.Errorf("записей ACCOUNT_DELETION_BLOCKED: %d", got)
	}
}

func TestSoleMemberOwnerIsToldThatNobodyCanTakeOverTheOrganization(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "alone@example.com", "Один")
	createOrganization(t, fixture, owner, "Одноместная")
	code, _, body := accountRequest(t, fixture, http.MethodPost, "/deletion", passwordBody(ownerPassword), owner.Cookie)
	if code != http.StatusConflict || !strings.Contains(string(body), `"organizationName":"Одноместная"`) || strings.Contains(string(body), `"hasOtherMembers":true`) {
		t.Fatalf("%d %s", code, body)
	}
}

func readZip(t *testing.T, body []byte) (map[string][]byte, []string) {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("архив не открывается: %v", err)
	}
	files := map[string][]byte{}
	var names []string
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[file.Name] = content
		names = append(names, file.Name)
	}
	return files, names
}

func TestOrganizationExportThroughTheWholeStack(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "org-export-owner@example.com", "Владелец")
	tenantID := createOrganization(t, fixture, owner, "Организация выгрузки")
	locationID := createLocation(t, fixture, owner, tenantID, "Основная точка")
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/services", `{"name":"Полировка"}`, owner.Cookie, tenantID), http.StatusCreated)
	connected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{"name":"Форма сайта","locationId":"`+locationID+`"}`, owner.Cookie, tenantID)
	requireStatus(t, connected, http.StatusCreated)
	var connection struct {
		ID            string `json:"id"`
		WebhookSecret string `json:"webhookSecret"`
	}
	if err := json.Unmarshal(connected.Body.Bytes(), &connection); err != nil {
		t.Fatal(err)
	}
	member := joinOrganization(t, fixture, owner, tenantID, "org-export-member@example.com", "Менеджер")

	// Переписка организации: контакт, разговор, сообщение с меткой.
	marker := "МАРКЕР-ВЫГРУЗКИ-ORG"
	seedConversation := func(tenant, connectionID, locationID, label string) {
		contact, conversation, message := newUUIDString(t), newUUIDString(t), newUUIDString(t)
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`INSERT INTO contacts(id, tenant_id, display_name) VALUES ($1, $2, $3)`, []any{contact, tenant, "Клиент " + label}},
			{`INSERT INTO conversations(id, tenant_id, location_id, connection_id, contact_id, external_id, status, first_message_at, last_message_at, last_message_direction, revision)
			  VALUES ($1, $2, $3, $4, $5, $6, 'ACTIVE', now(), now(), 'INCOMING', 1)`, []any{conversation, tenant, locationID, connectionID, contact, "conv-" + label}},
			{`INSERT INTO messages(id, tenant_id, conversation_id, connection_id, external_id, direction, type, text, sent_at, received_at)
			  VALUES ($1, $2, $3, $4, $5, 'INCOMING', 'TEXT', $6, now(), now())`, []any{message, tenant, conversation, connectionID, "msg-" + label, "Привет. " + label}},
		} {
			if _, err := fixture.pool.Exec(context.Background(), statement.query, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	seedConversation(tenantID, connection.ID, locationID, marker)

	// Соседняя организация со своим сообщением.
	foreign := register(t, fixture.handler, "org-export-foreign@example.com", "Чужой")
	foreignTenant := createOrganization(t, fixture, foreign, "Соседняя организация")
	foreignLocation := createLocation(t, fixture, foreign, foreignTenant, "Чужая точка")
	foreignConnected := request(t, fixture.handler, http.MethodPost, "/api/v1/integrations/GENERIC_WEBHOOK/connect", `{"name":"Чужой канал","locationId":"`+foreignLocation+`"}`, foreign.Cookie, foreignTenant)
	requireStatus(t, foreignConnected, http.StatusCreated)
	var foreignConnection struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(foreignConnected.Body.Bytes(), &foreignConnection); err != nil {
		t.Fatal(err)
	}
	seedConversation(foreignTenant, foreignConnection.ID, foreignLocation, "ЧУЖОЙ-МАРКЕР")

	export := func(cookie *http.Cookie, tenant string) *http.Response {
		recorder := request(t, fixture.handler, http.MethodPost, "/api/v1/organization/export", "", cookie, tenant)
		return recorder.Result()
	}
	// Менеджер и чужой владелец права не имеют; без сессии и без организации — 401.
	for name, response := range map[string]*http.Response{
		"менеджер":       export(member.Cookie, tenantID),
		"чужой владелец": export(foreign.Cookie, tenantID),
	} {
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d, ожидалось 403", name, response.StatusCode)
		}
	}
	if response := export(nil, tenantID); response.StatusCode != http.StatusUnauthorized {
		t.Errorf("без сессии: %d", response.StatusCode)
	}
	if response := export(owner.Cookie, ""); response.StatusCode != http.StatusUnauthorized {
		t.Errorf("без организации: %d", response.StatusCode)
	}
	if got := countRows(t, fixture, `SELECT count(*) FROM audit_log WHERE operation = 'ORGANIZATION_DATA_EXPORTED'`); got != 0 {
		t.Fatalf("отказы оставили %d записей аудита выгрузки", got)
	}

	response := export(owner.Cookie, tenantID)
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("выгрузка владельца: %d %v", response.StatusCode, err)
	}
	if response.Header.Get("Content-Type") != "application/zip" || response.Header.Get("Cache-Control") != "no-store" ||
		!strings.HasPrefix(response.Header.Get("Content-Disposition"), `attachment; filename="lidradar-organization-`) {
		t.Errorf("заголовки: %v", response.Header)
	}
	files, names := readZip(t, body)
	if names[0] != "README.txt" || names[1] != "organization.json" || names[len(names)-1] != "manifest.json" {
		t.Fatalf("состав архива: %v", names)
	}
	if !strings.Contains(string(files["data/messages.jsonl"]), marker) || !strings.Contains(string(files["data/services.jsonl"]), "Полировка") ||
		!strings.Contains(string(files["data/locations.jsonl"]), "Основная точка") || !strings.Contains(string(files["data/channels.jsonl"]), "Форма сайта") {
		t.Errorf("в архиве нет содержимого организации")
	}
	var everything strings.Builder
	for _, content := range files {
		everything.Write(content)
	}
	output := everything.String()
	for _, forbidden := range []string{
		"ЧУЖОЙ-МАРКЕР", "Соседняя организация", "org-export-foreign", "Чужая точка", "Чужой канал", // чужая организация
		connection.WebhookSecret, "verification_secret_hash", "encrypted_credentials", "code_hash", // секреты
		"tenant_id", "tenantId", "argon2", "password", // идентификатор организации и пароли
	} {
		if strings.Contains(output, forbidden) {
			t.Errorf("в выгрузке организации есть %q", forbidden)
		}
	}
	if !strings.Contains(string(files["data/team.jsonl"]), "org-export-member@example.com") {
		t.Errorf("команда не выгружена: %s", files["data/team.jsonl"])
	}
	if got := countRows(t, fixture, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND actor_user_id = $2 AND operation = 'ORGANIZATION_DATA_EXPORTED' AND entity_type = 'ORGANIZATION'`, tenantID, owner.ID); got != 1 {
		t.Errorf("записей аудита выгрузки: %d", got)
	}

	// Три выгрузки в час на организацию: четвёртая — 429.
	for attempt := 2; attempt <= 3; attempt++ {
		if r := export(owner.Cookie, tenantID); r.StatusCode != http.StatusOK {
			t.Fatalf("выгрузка %d: %d", attempt, r.StatusCode)
		} else {
			_, _ = io.Copy(io.Discard, r.Body)
		}
	}
	limited := export(owner.Cookie, tenantID)
	if limited.StatusCode != http.StatusTooManyRequests || limited.Header.Get("Retry-After") == "" {
		t.Errorf("четвёртая выгрузка: %d, Retry-After=%q", limited.StatusCode, limited.Header.Get("Retry-After"))
	}
}
