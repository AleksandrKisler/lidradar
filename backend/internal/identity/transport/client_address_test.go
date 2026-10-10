package transport

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"lidradar/backend/internal/identity/application"
	httpplatform "lidradar/backend/platform/http"
	"lidradar/backend/platform/observability"
)

// ADR 0049: сеанс, журнал входа и постоянные ограничители получают адрес
// клиента, который определил роутер, а не адрес соединения (за proxy это адрес
// proxy, одинаковый у всех пользователей).
func TestClientCarriesTheAddressResolvedByTheRouter(t *testing.T) {
	var seen application.Client
	router := httpplatform.NewRouter("lidradar-api", observability.NewLogger(&bytes.Buffer{}, "test", "test"), nil,
		httpplatform.WithTrustedProxies([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}))
	router.Post("/probe", func(_ http.ResponseWriter, r *http.Request) { seen = client(r) })
	call := func(remote, forwarded string) application.Client {
		seen = application.Client{}
		request := httptest.NewRequest(http.MethodPost, "/probe", nil)
		request.RemoteAddr = remote
		request.Header.Set("User-Agent", "probe/1.0")
		if forwarded != "" {
			request.Header.Set("X-Forwarded-For", forwarded)
		}
		router.ServeHTTP(httptest.NewRecorder(), request)
		return seen
	}
	if got := call("10.0.0.5:4000", "198.51.100.7"); got.IPAddress != "198.51.100.7" || got.UserAgent != "probe/1.0" {
		t.Fatalf("за доверенным proxy: %#v", got)
	}
	if got := call("203.0.113.5:4000", "198.51.100.7"); got.IPAddress != "203.0.113.5" {
		t.Fatalf("подделанный заголовок от недоверенного узла: %#v", got)
	}
	if got := call("[2001:db8::9]:443", ""); got.IPAddress != "2001:db8::9" {
		t.Fatalf("IPv6 без заголовка: %#v", got)
	}
	// Без роутера (прямой вызов обработчика) остаётся адрес соединения.
	direct := httptest.NewRequest(http.MethodPost, "/probe", nil)
	direct.RemoteAddr = "192.0.2.10:1234"
	direct.Header.Set("X-Forwarded-For", "198.51.100.7")
	if got := client(direct); got.IPAddress != "192.0.2.10" {
		t.Fatalf("без роутера: %#v", got)
	}
}
