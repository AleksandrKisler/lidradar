package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	httpplatform "lidradar/backend/platform/http"
)

// ADR 0049 / H-10 на настоящем стенде с постоянными ограничителями в PostgreSQL.
// За доверенным proxy клиенты не делят предел, присланный напрямую заголовок
// его не обходит и не переносит на постороннего, а сеанс и журнал входа хранят
// адрес клиента, а не proxy. Предел регистрации (5 в час на адрес) выбран ради
// скорости: он проходит тем же путём client() → take(), что вход и обновление
// сеанса, но не требует десятков дорогих проверок пароля.
func TestPersistentAuthLimitsUseTheClientBehindTrustedProxy(t *testing.T) {
	fixture := newAPIFixtureWithRouterOptions(t,
		httpplatform.WithTrustedProxies([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}))
	const proxy = "10.0.0.5:44321"

	call := func(remote, forwarded, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "http://api.example"+path, bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		request.RemoteAddr = remote
		if forwarded != "" {
			request.Header.Set("X-Forwarded-For", forwarded)
		}
		response := httptest.NewRecorder()
		fixture.handler.ServeHTTP(response, request)
		return response
	}
	registerAs := func(remote, forwarded, email string) *httptest.ResponseRecorder {
		t.Helper()
		return call(remote, forwarded, "/api/v1/auth/register",
			fmt.Sprintf(`{"email":%q,"password":"very-secure-password","displayName":"Клиент"}`, email))
	}

	// Клиент A исчерпывает предел регистрации с адреса; следующая попытка — 429.
	for attempt := 1; attempt <= 5; attempt++ {
		requireStatus(t, registerAs(proxy, "198.51.100.7", fmt.Sprintf("a%d@example.com", attempt)), http.StatusCreated)
	}
	blocked := registerAs(proxy, "198.51.100.7", "a6@example.com")
	requireStatus(t, blocked, http.StatusTooManyRequests)
	if blocked.Header().Get("Retry-After") == "" || !strings.Contains(blocked.Body.String(), `"code":"RATE_LIMITED"`) {
		t.Fatalf("ограничение клиента A: Retry-After=%q, тело=%s", blocked.Header().Get("Retry-After"), blocked.Body.String())
	}

	// Клиент B за тем же proxy регистрируется свободно: до исправления все
	// пользователи за proxy делили один предел.
	requireStatus(t, registerAs(proxy, "203.0.113.9", "bob@example.com"), http.StatusCreated)

	// Сеанс и журнал входа хранят адрес клиента B, а не proxy.
	requireStatus(t, call(proxy, "203.0.113.9", "/api/v1/auth/login", `{"email":"bob@example.com","password":"very-secure-password"}`), http.StatusOK)
	addresses := func(operation string) string {
		t.Helper()
		var address string
		if err := fixture.pool.QueryRow(context.Background(), `
			SELECT l.ip_address FROM auth_audit_log l JOIN users u ON u.id = l.user_id
			WHERE u.email = 'bob@example.com' AND l.operation = $1 ORDER BY l.created_at DESC LIMIT 1`, operation).Scan(&address); err != nil {
			t.Fatalf("журнал входа %s: %v", operation, err)
		}
		return address
	}
	var sessionAddress string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT host(s.ip) FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE u.email = 'bob@example.com' ORDER BY s.created_at DESC LIMIT 1`).Scan(&sessionAddress); err != nil {
		t.Fatal(err)
	}
	if sessionAddress != "203.0.113.9" || addresses("USER_REGISTERED") != "203.0.113.9" || addresses("USER_LOGGED_IN") != "203.0.113.9" {
		t.Fatalf("адрес в сеансе = %q, при регистрации = %q, при входе = %q; нужен адрес клиента 203.0.113.9",
			sessionAddress, addresses("USER_REGISTERED"), addresses("USER_LOGGED_IN"))
	}

	// Недоверенный узел напрямую: меняя значение заголовка, предел не обойти.
	for attempt := 1; attempt <= 5; attempt++ {
		requireStatus(t, registerAs("203.0.113.50:5000", fmt.Sprintf("192.0.2.%d", attempt), fmt.Sprintf("s%d@example.com", attempt)), http.StatusCreated)
	}
	requireStatus(t, registerAs("203.0.113.50:5000", "192.0.2.200", "s6@example.com"), http.StatusTooManyRequests)

	// Тот же приём не переносит предел на постороннего: недоверенный узел,
	// выдающий себя за клиента C, исчерпывает только собственный предел, а сам C
	// за proxy регистрируется свободно.
	for attempt := 1; attempt <= 6; attempt++ {
		registerAs("203.0.113.51:5000", "198.51.100.33", fmt.Sprintf("f%d@example.com", attempt))
	}
	requireStatus(t, registerAs(proxy, "198.51.100.33", "carol@example.com"), http.StatusCreated)
}
