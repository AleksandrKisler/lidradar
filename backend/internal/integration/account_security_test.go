package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mailapplication "lidradar/backend/internal/mail/application"
)

// Безопасность учётной записи через весь HTTP-стек на настоящей PostgreSQL (ADR 0058): письма ставит API,
// доставляет рабочая роль, ссылки несут токен во фрагменте, смена и сброс пароля завершают сессии.

const (
	integrationWebBaseURL = "https://app.integration.test"
	newPassword           = "brand-new-secure-password"
)

// sentMail заменяет SMTP: всё, что транспорт «отправил», остаётся в памяти теста.
type sentMail struct {
	mu   sync.Mutex
	list []mailapplication.Outgoing
}

func (s *sentMail) Send(_ context.Context, message mailapplication.Outgoing) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, message)
	return nil
}

func (s *sentMail) all() []mailapplication.Outgoing {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mailapplication.Outgoing(nil), s.list...)
}

// deliver доставляет всё, что накопилось в очереди, и возвращает новые письма.
func deliver(t *testing.T, fixture apiFixture) []mailapplication.Outgoing {
	t.Helper()
	before := len(fixture.sent.all())
	for {
		progressed, err := fixture.mailDelivery.DispatchOne(context.Background(), "integration-mail", time.Minute)
		if err != nil {
			t.Fatalf("доставка письма: %v", err)
		}
		if !progressed {
			break
		}
	}
	return fixture.sent.all()[before:]
}

// mailTo возвращает единственное письмо адресату из пачки.
func mailTo(t *testing.T, batch []mailapplication.Outgoing, address string) mailapplication.Outgoing {
	t.Helper()
	var found []mailapplication.Outgoing
	for _, message := range batch {
		if message.To == address {
			found = append(found, message)
		}
	}
	if len(found) != 1 {
		t.Fatalf("писем для %s: %d из %d, ожидалось одно", address, len(found), len(batch))
	}
	return found[0]
}

// linkToken достаёт токен из ссылки письма и проверяет её форму: адрес клиента, путь и токен во фрагменте.
func linkToken(t *testing.T, message mailapplication.Outgoing, path string) string {
	t.Helper()
	prefix := integrationWebBaseURL + path + "#token="
	for _, line := range strings.Split(message.Text, "\n") {
		if strings.HasPrefix(line, prefix) {
			token := strings.TrimPrefix(line, prefix)
			if len(token) != 43 || strings.ContainsAny(token, " ?&") {
				t.Fatalf("токен в ссылке выглядит неверно: %q", token)
			}
			return token
		}
	}
	t.Fatalf("в письме нет ссылки %s…: %q", prefix, message.Text)
	return ""
}

func jsonBody(pairs ...string) string {
	values := map[string]string{}
	for index := 0; index+1 < len(pairs); index += 2 {
		values[pairs[index]] = pairs[index+1]
	}
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

func post(t *testing.T, fixture apiFixture, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return request(t, fixture.handler, http.MethodPost, "/api/v1/auth"+path, body, cookie, "")
}

func loginWith(t *testing.T, fixture apiFixture, email, password string) *httptest.ResponseRecorder {
	t.Helper()
	return post(t, fixture, "/login", jsonBody("email", email, "password", password), nil)
}

func meStatus(t *testing.T, fixture apiFixture, cookie *http.Cookie) int {
	t.Helper()
	return request(t, fixture.handler, http.MethodGet, "/api/v1/auth/me", "", cookie, "").Code
}

func emailVerifiedAt(t *testing.T, fixture apiFixture, cookie *http.Cookie) *string {
	t.Helper()
	response := request(t, fixture.handler, http.MethodGet, "/api/v1/auth/me", "", cookie, "")
	requireStatus(t, response, http.StatusOK)
	var view struct {
		User struct {
			EmailVerifiedAt *string `json:"emailVerifiedAt"`
		} `json:"user"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatalf("разбор /auth/me: %v: %s", err, response.Body.String())
	}
	return view.User.EmailVerifiedAt
}

func TestRegistrationQueuesAVerificationMailAndItsLinkConfirmsTheAddress(t *testing.T) {
	fixture := newAPIFixture(t)
	user := register(t, fixture.handler, "verify@example.com", "Verify")
	if emailVerifiedAt(t, fixture, user.Cookie) != nil {
		t.Fatal("новый адрес не подтверждён")
	}
	mail := mailTo(t, deliver(t, fixture), "verify@example.com")
	if mail.Subject != "Подтвердите почту в LidRadar" {
		t.Fatalf("тема = %q", mail.Subject)
	}
	token := linkToken(t, mail, "/verify-email")

	// Ссылку открывают в любом браузере: сессия не нужна.
	requireStatus(t, post(t, fixture, "/email/verify", jsonBody("token", token), nil), http.StatusNoContent)
	if emailVerifiedAt(t, fixture, user.Cookie) == nil {
		t.Fatal("адрес должен стать подтверждённым")
	}
	requireErrorCode(t, post(t, fixture, "/email/verify", jsonBody("token", token), nil), http.StatusBadRequest, "VERIFICATION_TOKEN_INVALID")
	requireErrorCode(t, post(t, fixture, "/email/verify", jsonBody("token", strings.Repeat("a", 43)), nil), http.StatusBadRequest, "VERIFICATION_TOKEN_INVALID")

	// После отправки ни адреса, ни ссылки в очереди не остаётся.
	var leftovers int
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM mail_outbox WHERE recipient IS NOT NULL OR body IS NOT NULL`).Scan(&leftovers); err != nil || leftovers != 0 {
		t.Fatalf("в очереди остались адрес или ссылка: %d, %v", leftovers, err)
	}
	var plain int
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM account_tokens WHERE token_hash = $1`, token).Scan(&plain); err != nil || plain != 0 {
		t.Fatalf("открытый токен найден в таблице токенов: %d, %v", plain, err)
	}
	var audited int
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM auth_audit_log WHERE operation = 'EMAIL_VERIFIED'`).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("записей EMAIL_VERIFIED = %d, %v", audited, err)
	}
}

func TestPasswordResetEndsEverySessionAndTheLinkWorksOnce(t *testing.T) {
	fixture := newAPIFixture(t)
	first := register(t, fixture.handler, "reset@example.com", "Reset")
	second := login(t, fixture.handler, "reset@example.com")
	deliver(t, fixture) // письмо подтверждения с регистрации

	requireStatus(t, post(t, fixture, "/password/forgot", jsonBody("email", " RESET@example.com "), nil), http.StatusNoContent)
	mail := mailTo(t, deliver(t, fixture), "reset@example.com")
	if mail.Subject != "Сброс пароля LidRadar" {
		t.Fatalf("тема = %q", mail.Subject)
	}
	token := linkToken(t, mail, "/reset-password")

	requireErrorCode(t, post(t, fixture, "/password/reset", jsonBody("token", token, "newPassword", "short"), nil), http.StatusBadRequest, "INVALID_ARGUMENT")
	requireStatus(t, post(t, fixture, "/password/reset", jsonBody("token", token, "newPassword", newPassword), nil), http.StatusNoContent)

	for name, cookie := range map[string]*http.Cookie{"первая": first.Cookie, "вторая": second} {
		if status := meStatus(t, fixture, cookie); status != http.StatusUnauthorized {
			t.Fatalf("%s сессия после сброса: %d, ожидалось 401", name, status)
		}
	}
	requireErrorCode(t, loginWith(t, fixture, "reset@example.com", "very-secure-password"), http.StatusUnauthorized, "INVALID_CREDENTIALS")
	response := loginWith(t, fixture, "reset@example.com", newPassword)
	requireStatus(t, response, http.StatusOK)
	if emailVerifiedAt(t, fixture, response.Result().Cookies()[0]) == nil {
		t.Fatal("ссылка сброса дошла до ящика: адрес должен считаться подтверждённым")
	}
	requireErrorCode(t, post(t, fixture, "/password/reset", jsonBody("token", token, "newPassword", "another-secure-password"), nil), http.StatusBadRequest, "RESET_TOKEN_INVALID")

	notice := mailTo(t, deliver(t, fixture), "reset@example.com")
	if notice.Subject != "Пароль LidRadar изменён" || strings.Contains(notice.Text, "#token=") {
		t.Fatalf("уведомление = %#v", notice)
	}
	var operations []string
	rows, err := fixture.pool.Query(context.Background(), `SELECT operation FROM auth_audit_log WHERE operation LIKE 'PASSWORD_%' ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var operation string
		if err := rows.Scan(&operation); err != nil {
			t.Fatal(err)
		}
		operations = append(operations, operation)
	}
	if strings.Join(operations, ",") != "PASSWORD_RESET_REQUESTED,PASSWORD_RESET_COMPLETED" {
		t.Fatalf("аудит = %v", operations)
	}
}

// Ответ сброса не должен выдавать, зарегистрирован ли адрес: ни статусом, ни телом, ни заголовками.
func TestForgotPasswordAnswersIdenticallyForKnownAndUnknownAddresses(t *testing.T) {
	fixture := newAPIFixture(t)
	register(t, fixture.handler, "known@example.com", "Known")
	deliver(t, fixture)

	known := post(t, fixture, "/password/forgot", jsonBody("email", "known@example.com"), nil)
	unknown := post(t, fixture, "/password/forgot", jsonBody("email", "unknown@example.com"), nil)
	malformed := post(t, fixture, "/password/forgot", jsonBody("email", "not-an-address"), nil)
	for name, response := range map[string]*httptest.ResponseRecorder{"известный": known, "неизвестный": unknown, "неверный": malformed} {
		if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
			t.Fatalf("%s: %d %q", name, response.Code, response.Body.String())
		}
	}
	headers := func(response *httptest.ResponseRecorder) string {
		var names []string
		for name, values := range response.Header() {
			if name == "X-Request-Id" || name == "Date" {
				continue
			}
			names = append(names, name+"="+strings.Join(values, ","))
		}
		return strings.Join(sortStrings(names), ";")
	}
	if headers(known) != headers(unknown) || headers(known) != headers(malformed) {
		t.Fatalf("заголовки различаются:\n%s\n%s\n%s", headers(known), headers(unknown), headers(malformed))
	}
	batch := deliver(t, fixture)
	if len(batch) != 1 || batch[0].To != "known@example.com" {
		t.Fatalf("письмо должно уйти только зарегистрированному адресу: %#v", batch)
	}
}

func sortStrings(values []string) []string {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
	return values
}

func TestForgotPasswordLimitsRequestsWithoutRevealingExistence(t *testing.T) {
	fixture := newAPIFixture(t)
	register(t, fixture.handler, "limited@example.com", "Limited")
	for _, email := range []string{"limited@example.com", "nobody@example.com"} {
		for attempt := 1; attempt <= 3; attempt++ {
			requireStatus(t, post(t, fixture, "/password/forgot", jsonBody("email", email), nil), http.StatusNoContent)
		}
		blocked := post(t, fixture, "/password/forgot", jsonBody("email", email), nil)
		requireErrorCode(t, blocked, http.StatusTooManyRequests, "RATE_LIMITED")
		if blocked.Header().Get("Retry-After") == "" {
			t.Fatalf("%s: нет Retry-After", email)
		}
	}
}

func TestChangePasswordKeepsTheCurrentSessionAndEndsTheOthers(t *testing.T) {
	fixture := newAPIFixture(t)
	current := register(t, fixture.handler, "change@example.com", "Change")
	other := login(t, fixture.handler, "change@example.com")
	deliver(t, fixture)

	// Неверный текущий пароль: 403, а не 401 — клиент не должен считать это потерей сессии.
	wrong := post(t, fixture, "/password/change", jsonBody("currentPassword", "definitely-wrong-password", "newPassword", newPassword), current.Cookie)
	requireErrorCode(t, wrong, http.StatusForbidden, "INVALID_CURRENT_PASSWORD")
	if meStatus(t, fixture, current.Cookie) != http.StatusOK {
		t.Fatal("опечатка в пароле не должна разлогинивать")
	}
	requireErrorCode(t, post(t, fixture, "/password/change", jsonBody("currentPassword", "very-secure-password", "newPassword", "short"), current.Cookie), http.StatusBadRequest, "INVALID_ARGUMENT")
	requireErrorCode(t, post(t, fixture, "/password/change", jsonBody("currentPassword", "very-secure-password", "newPassword", "very-secure-password"), current.Cookie), http.StatusBadRequest, "INVALID_ARGUMENT")

	requireStatus(t, post(t, fixture, "/password/change", jsonBody("currentPassword", "very-secure-password", "newPassword", newPassword), current.Cookie), http.StatusNoContent)
	if meStatus(t, fixture, current.Cookie) != http.StatusOK {
		t.Fatal("текущая сессия должна пережить смену пароля")
	}
	if status := meStatus(t, fixture, other); status != http.StatusUnauthorized {
		t.Fatalf("прочие сессии должны завершиться, получено %d", status)
	}
	requireErrorCode(t, loginWith(t, fixture, "change@example.com", "very-secure-password"), http.StatusUnauthorized, "INVALID_CREDENTIALS")
	requireStatus(t, loginWith(t, fixture, "change@example.com", newPassword), http.StatusOK)
	if notice := mailTo(t, deliver(t, fixture), "change@example.com"); notice.Subject != "Пароль LidRadar изменён" {
		t.Fatalf("уведомление = %#v", notice)
	}
}

func TestChangePasswordRequiresASession(t *testing.T) {
	fixture := newAPIFixture(t)
	register(t, fixture.handler, "nosession@example.com", "NoSession")
	body := jsonBody("currentPassword", "very-secure-password", "newPassword", newPassword)
	requireErrorCode(t, post(t, fixture, "/password/change", body, nil), http.StatusUnauthorized, "UNAUTHENTICATED")
	requireErrorCode(t, post(t, fixture, "/email/verification", "", nil), http.StatusUnauthorized, "UNAUTHENTICATED")
	// Пароль не изменился.
	requireStatus(t, loginWith(t, fixture, "nosession@example.com", "very-secure-password"), http.StatusOK)
}

// Подбор текущего пароля украденной сессией упирается в предел 5 попыток за 15 минут.
func TestChangePasswordBruteForceIsLimited(t *testing.T) {
	fixture := newAPIFixture(t)
	user := register(t, fixture.handler, "brute@example.com", "Brute")
	for attempt := 1; attempt <= 5; attempt++ {
		requireErrorCode(t, post(t, fixture, "/password/change", jsonBody("currentPassword", "guess-number-"+strings.Repeat("x", attempt), "newPassword", newPassword), user.Cookie), http.StatusForbidden, "INVALID_CURRENT_PASSWORD")
	}
	blocked := post(t, fixture, "/password/change", jsonBody("currentPassword", "very-secure-password", "newPassword", newPassword), user.Cookie)
	requireErrorCode(t, blocked, http.StatusTooManyRequests, "RATE_LIMITED")
	requireStatus(t, loginWith(t, fixture, "brute@example.com", "very-secure-password"), http.StatusOK)
}

func TestResendVerificationIsLimitedAndSkippedWhenVerified(t *testing.T) {
	fixture := newAPIFixture(t)
	user := register(t, fixture.handler, "resend@example.com", "Resend")
	deliver(t, fixture)
	for attempt := 1; attempt <= 3; attempt++ {
		requireStatus(t, post(t, fixture, "/email/verification", "", user.Cookie), http.StatusNoContent)
	}
	requireErrorCode(t, post(t, fixture, "/email/verification", "", user.Cookie), http.StatusTooManyRequests, "RATE_LIMITED")
	batch := deliver(t, fixture)
	if len(batch) != 3 {
		t.Fatalf("писем подтверждения = %d, ожидалось 3", len(batch))
	}
	// Действует только последняя ссылка: предыдущие отозваны выпуском новой.
	requireErrorCode(t, post(t, fixture, "/email/verify", jsonBody("token", linkToken(t, batch[0], "/verify-email")), nil), http.StatusBadRequest, "VERIFICATION_TOKEN_INVALID")
	requireStatus(t, post(t, fixture, "/email/verify", jsonBody("token", linkToken(t, batch[2], "/verify-email")), nil), http.StatusNoContent)

	// Подтверждённому адресу письмо не нужно, и предел он не расходует.
	for attempt := 1; attempt <= 5; attempt++ {
		requireStatus(t, post(t, fixture, "/email/verification", "", user.Cookie), http.StatusNoContent)
	}
	if got := deliver(t, fixture); len(got) != 0 {
		t.Fatalf("подтверждённому адресу ушли письма: %#v", got)
	}
}

// Токены в письмах сделаны так, чтобы утечка базы или журнала не дала войти в чужую учётную запись.
func TestNoTokenSurvivesInTheDatabaseOrTheQueueAfterTheFlow(t *testing.T) {
	fixture := newAPIFixture(t)
	register(t, fixture.handler, "secret@example.com", "Secret")
	deliver(t, fixture)
	requireStatus(t, post(t, fixture, "/password/forgot", jsonBody("email", "secret@example.com"), nil), http.StatusNoContent)
	mail := mailTo(t, deliver(t, fixture), "secret@example.com")
	token := linkToken(t, mail, "/reset-password")
	for table, column := range map[string]string{"account_tokens": "token_hash", "mail_outbox": "COALESCE(body, '')", "auth_audit_log": "operation || COALESCE(ip_address, '')"} {
		var found int
		query := `SELECT count(*) FROM ` + table + ` WHERE ` + column + ` LIKE '%' || $1 || '%'`
		if err := fixture.pool.QueryRow(context.Background(), query, token).Scan(&found); err != nil || found != 0 {
			t.Fatalf("токен найден в %s: %d, %v", table, found, err)
		}
	}
}
