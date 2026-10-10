package httpplatform

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"lidradar/backend/platform/observability"
)

func prefixes(t *testing.T, values ...string) []netip.Prefix {
	t.Helper()
	result := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, prefix)
	}
	return result
}

func resolveFor(t *testing.T, trusted []netip.Prefix, remote string, forwarded ...string) (string, forwardedOutcome) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = remote
	for _, value := range forwarded {
		request.Header.Add("X-Forwarded-For", value)
	}
	return (&clientResolver{trusted: trusted}).resolve(request)
}

// ADR 0049: заголовок читается только от доверенного соседа, цепочка
// обходится справа налево, первый недоверенный адрес и есть клиент.
func TestClientAddressResolution(t *testing.T) {
	proxies := prefixes(t, "10.0.0.0/8", "2001:db8:f::/48", "192.168.1.7/32")
	cases := []struct {
		name      string
		trusted   []netip.Prefix
		remote    string
		forwarded []string
		want      string
		outcome   forwardedOutcome
	}{
		{"нет списка и заголовка", nil, "203.0.113.5:4000", nil, "203.0.113.5", forwardedAbsent},
		{"нет списка, заголовок игнорируется", nil, "10.0.0.5:4000", []string{"198.51.100.7"}, "10.0.0.5", forwardedIgnoredNoTrustedProxies},
		{"доверенный сосед без заголовка", proxies, "10.0.0.5:4000", nil, "10.0.0.5", forwardedAbsent},
		{"один клиент", proxies, "10.0.0.5:4000", []string{"198.51.100.7"}, "198.51.100.7", forwardedUsed},
		{"недоверенный сосед подделывает заголовок", proxies, "203.0.113.5:4000", []string{"198.51.100.7"}, "203.0.113.5", forwardedIgnoredUntrustedPeer},
		{"подделка слева от записи proxy", proxies, "10.0.0.5:4000", []string{"1.1.1.1, 198.51.100.7"}, "198.51.100.7", forwardedUsed},
		{"цепочка из двух доверенных узлов", proxies, "10.0.0.5:4000", []string{"198.51.100.7, 10.0.0.9"}, "198.51.100.7", forwardedUsed},
		{"подделка перед цепочкой", proxies, "10.0.0.5:4000", []string{"1.1.1.1, 198.51.100.7, 10.0.0.9"}, "198.51.100.7", forwardedUsed},
		{"клиент сам подставил доверенный адрес слева", proxies, "10.0.0.5:4000", []string{"10.1.1.1, 198.51.100.7"}, "198.51.100.7", forwardedUsed},
		{"несколько строк заголовка как одна цепочка", proxies, "10.0.0.5:4000", []string{"1.1.1.1", "198.51.100.7, 10.0.0.9"}, "198.51.100.7", forwardedUsed},
		{"порт у записи", proxies, "10.0.0.5:4000", []string{"198.51.100.7:51234"}, "198.51.100.7", forwardedUsed},
		{"IPv6 клиент", proxies, "10.0.0.5:4000", []string{"2001:db8:1::42"}, "2001:db8:1::42", forwardedUsed},
		{"IPv6 в скобках с портом", proxies, "10.0.0.5:4000", []string{"[2001:db8:1::42]:443"}, "2001:db8:1::42", forwardedUsed},
		{"IPv6 в скобках без порта", proxies, "10.0.0.5:4000", []string{"[2001:db8:1::42]"}, "2001:db8:1::42", forwardedUsed},
		{"IPv4 внутри IPv6 приводится к IPv4", proxies, "10.0.0.5:4000", []string{"::ffff:198.51.100.7"}, "198.51.100.7", forwardedUsed},
		{"зона IPv6 отбрасывается", proxies, "10.0.0.5:4000", []string{"fe80::1%eth0"}, "fe80::1", forwardedUsed},
		{"доверенный IPv6 сосед", proxies, "[2001:db8:f::1]:443", []string{"198.51.100.7"}, "198.51.100.7", forwardedUsed},
		{"одиночный доверенный адрес", proxies, "192.168.1.7:80", []string{"198.51.100.7"}, "198.51.100.7", forwardedUsed},
		{"соседний адрес вне одиночной записи", proxies, "192.168.1.8:80", []string{"198.51.100.7"}, "192.168.1.8", forwardedIgnoredUntrustedPeer},
		{"пустые записи пропускаются", proxies, "10.0.0.5:4000", []string{"198.51.100.7, , 10.0.0.9,"}, "198.51.100.7", forwardedUsed},
		{"все записи доверенные: самый дальний узел", proxies, "10.0.0.5:4000", []string{"10.0.0.2, 10.0.0.9"}, "10.0.0.2", forwardedUsed},
		{"мусор справа: ближайший доверенный узел", proxies, "10.0.0.5:4000", []string{"198.51.100.7, unknown"}, "10.0.0.5", forwardedMalformed},
		{"мусор после доверенного узла", proxies, "10.0.0.5:4000", []string{"198.51.100.7, garbage, 10.0.0.9"}, "10.0.0.9", forwardedMalformed},
		{"мусор левее найденного клиента не читается", proxies, "10.0.0.5:4000", []string{"garbage, 198.51.100.7"}, "198.51.100.7", forwardedUsed},
		{"пустой заголовок", proxies, "10.0.0.5:4000", []string{" "}, "10.0.0.5", forwardedMalformed},
		{"сосед без порта", proxies, "10.0.0.5", []string{"198.51.100.7"}, "198.51.100.7", forwardedUsed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, outcome := resolveFor(t, c.trusted, c.remote, c.forwarded...)
			if got != c.want || outcome != c.outcome {
				t.Fatalf("адрес = %q (%v), нужно %q (%v)", got, outcome, c.want, c.outcome)
			}
		})
	}
}

// Длинная цепочка доверенных узлов не тратит время запроса: разбор ограничен.
func TestClientAddressResolutionBoundsLongChains(t *testing.T) {
	proxies := prefixes(t, "10.0.0.0/8")
	chain := "198.51.100.7" + strings.Repeat(", 10.0.0.1", 5000)
	got, outcome := resolveFor(t, proxies, "10.0.0.5:4000", chain)
	// Клиент слева за пределом разбора не достижим: остаётся самый дальний из
	// просмотренных доверенных узлов, а не адрес из начала цепочки.
	if got != "10.0.0.1" || outcome != forwardedMalformed {
		t.Fatalf("адрес при слишком длинной цепочке = %q (%v), нужен 10.0.0.1 (forwardedMalformed)", got, outcome)
	}
}

func TestClientAddressFallsBackToConnectionWithoutResolver(t *testing.T) {
	for remote, want := range map[string]string{
		"203.0.113.5:4000":   "203.0.113.5",
		"[2001:db8::1]:443":  "2001:db8::1",
		"203.0.113.5":        "203.0.113.5",
		" 203.0.113.5:4000 ": "203.0.113.5",
		"":                   "",
		"@":                  "@",
		"[2001:db8::1]":      "2001:db8::1",
	} {
		request := &http.Request{RemoteAddr: remote, Header: http.Header{}}
		request.Header.Set("X-Forwarded-For", "198.51.100.7")
		if got := ClientAddress(request); got != want {
			t.Errorf("ClientAddress(%q) = %q, нужно %q", remote, got, want)
		}
	}
}

// Адрес определяется один раз на запрос и виден всем потребителям через контекст.
func TestRouterStoresResolvedClientAddressInContext(t *testing.T) {
	var seen string
	router := NewRouter("lidradar-api", observability.NewLogger(&bytes.Buffer{}, "test", "test"), readinessStub{},
		WithTrustedProxies(prefixes(t, "10.0.0.0/8")))
	router.Get("/probe", func(_ http.ResponseWriter, r *http.Request) { seen = ClientAddress(r) })
	call := func(remote, forwarded string) string {
		seen = ""
		request := httptest.NewRequest(http.MethodGet, "/probe", nil)
		request.RemoteAddr = remote
		if forwarded != "" {
			request.Header.Set("X-Forwarded-For", forwarded)
		}
		router.ServeHTTP(httptest.NewRecorder(), request)
		return seen
	}
	if got := call("10.0.0.5:4000", "198.51.100.7"); got != "198.51.100.7" {
		t.Fatalf("через доверенный proxy = %q", got)
	}
	if got := call("203.0.113.5:4000", "198.51.100.7"); got != "203.0.113.5" {
		t.Fatalf("подделка от недоверенного узла = %q", got)
	}
	if got := call("10.0.0.5:4000", ""); got != "10.0.0.5" {
		t.Fatalf("доверенный узел без заголовка = %q", got)
	}
}

// Ограничитель считает клиентов за proxy по отдельности, а присланный
// напрямую заголовок не позволяет ни обойти предел, ни перенести его на другого.
func TestRateLimitUsesClientBehindTrustedProxy(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	router := NewRouter("lidradar-api", observability.NewLogger(&bytes.Buffer{}, "test", "test"), readinessStub{},
		WithRateLimit(RateLimit{Requests: 3, Window: time.Minute, Prefixes: []string{"/api/v1/auth/"}}),
		WithTrustedProxies(prefixes(t, "10.0.0.0/8")), WithClock(func() time.Time { return now }))
	call := func(remote, forwarded string) int {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
		request.RemoteAddr = remote
		if forwarded != "" {
			request.Header.Set("X-Forwarded-For", forwarded)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response.Code
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if code := call("10.0.0.5:1000", "198.51.100.7"); code == http.StatusTooManyRequests {
			t.Fatalf("клиент A ограничен преждевременно на попытке %d", attempt)
		}
	}
	if code := call("10.0.0.5:1001", "198.51.100.7"); code != http.StatusTooManyRequests {
		t.Fatalf("четвёртая попытка клиента A = %d, нужно 429", code)
	}
	if code := call("10.0.0.5:1002", "203.0.113.200"); code == http.StatusTooManyRequests {
		t.Fatal("клиент B за тем же proxy ограничен попытками клиента A")
	}
	// Тот же proxy с другого порта и без заголовка — это сам proxy, не клиент A.
	if code := call("10.0.0.6:1003", ""); code == http.StatusTooManyRequests {
		t.Fatal("proxy без заголовка ограничен чужими попытками")
	}
	// Недоверенный узел: заголовок не меняет ключ, смена значения не обходит предел.
	for attempt := 1; attempt <= 3; attempt++ {
		call("203.0.113.5:2000", "198.51.100."+string(rune('0'+attempt)))
	}
	if code := call("203.0.113.5:2001", "192.0.2.77"); code != http.StatusTooManyRequests {
		t.Fatalf("подмена заголовка обошла предел недоверенного узла: %d", code)
	}
	// Подделка не переносит предел на постороннего: A не исчерпан чужим заголовком.
	if code := call("198.51.100.77:3000", "198.51.100.7"); code == http.StatusTooManyRequests {
		t.Fatal("недоверенный узел с чужим адресом в заголовке исчерпал предел клиента A")
	}
}

// Заголовок, который не принят, не должен молча превращать всех клиентов за
// proxy в один адрес: приложение предупреждает не чаще раза в минуту и не
// пишет в журнал ни адресов, ни значений заголовка.
func TestIgnoredForwardedHeaderIsReportedOncePerMinuteWithoutAddresses(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	var logs bytes.Buffer
	build := func(trusted []netip.Prefix) http.Handler {
		return NewRouter("lidradar-api", observability.NewLogger(&logs, "test", "test"), readinessStub{},
			WithTrustedProxies(trusted), WithClock(func() time.Time { return now }))
	}
	call := func(router http.Handler, remote, forwarded string) {
		request := httptest.NewRequest(http.MethodGet, "/health/live", nil)
		request.RemoteAddr = remote
		if forwarded != "" {
			request.Header.Set("X-Forwarded-For", forwarded)
		}
		router.ServeHTTP(httptest.NewRecorder(), request)
	}

	warnings := func() int { return strings.Count(logs.String(), `"event":"http.forwarded_header_ignored"`) }

	unset := build(nil)
	call(unset, "10.0.0.5:4000", "198.51.100.7")
	call(unset, "10.0.0.5:4001", "198.51.100.8")
	if got := warnings(); got != 1 {
		t.Fatalf("предупреждений за минуту = %d, нужно 1\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), `"reason":"no_trusted_proxies"`) || !strings.Contains(logs.String(), "LIDRADAR_TRUSTED_PROXIES") {
		t.Fatalf("предупреждение без причины или подсказки: %s", logs.String())
	}
	for _, secret := range []string{"10.0.0.5", "198.51.100.7", "198.51.100.8"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("журнал содержит адрес %s: %s", secret, logs.String())
		}
	}
	now = now.Add(61 * time.Second)
	call(unset, "10.0.0.5:4002", "198.51.100.9")
	if got := warnings(); got != 2 {
		t.Fatalf("после минуты предупреждений = %d, нужно 2", got)
	}

	logs.Reset()
	configured := build(prefixes(t, "10.0.0.0/8"))
	call(configured, "10.0.0.5:4000", "198.51.100.7") // принят
	if warnings() != 0 {
		t.Fatalf("принятый заголовок вызвал предупреждение: %s", logs.String())
	}
	call(configured, "203.0.113.5:4000", "198.51.100.7") // чужой узел
	if !strings.Contains(logs.String(), `"reason":"peer_not_trusted"`) {
		t.Fatalf("нет причины peer_not_trusted: %s", logs.String())
	}
	if strings.Contains(logs.String(), "203.0.113.5") {
		t.Fatalf("журнал содержит адрес соединения: %s", logs.String())
	}
	logs.Reset()
	call(configured, "203.0.113.5:4000", "") // без заголовка тишина
	if warnings() != 0 {
		t.Fatalf("запрос без заголовка вызвал предупреждение: %s", logs.String())
	}
}
