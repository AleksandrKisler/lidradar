package httpplatform

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"
)

// Адрес клиента (ADR 0049). API работает за proxy, который завершает TLS: адрес
// соединения принадлежит proxy, и без разбора заголовка все клиенты делят один
// предел частоты. Заголовок X-Forwarded-For принимается только от узлов из
// LIDRADAR_TRUSTED_PROXIES; от остальных он игнорируется, потому что клиент
// подделывает его свободно. Другие заголовки (X-Real-IP, Forwarded и подобные)
// намеренно не читаются: порядок разбора должен быть однозначным.

const (
	forwardedForHeader = "X-Forwarded-For"
	// maxForwardedHops ограничивает разбор цепочки: честная цепочка короче
	// десятка узлов, а длинная не должна расходовать время запроса.
	maxForwardedHops = 64
	// ignoredHeaderWarningPeriod — не чаще одного предупреждения за период.
	ignoredHeaderWarningPeriod = time.Minute
)

type clientAddressKey struct{}

// ClientAddress возвращает сетевой адрес клиента без порта: адрес из
// X-Forwarded-For, если его прислал доверенный proxy, иначе адрес соединения.
// Его же используют ограничители частоты, учёт сеансов и журнал входа.
func ClientAddress(r *http.Request) string {
	if address, ok := r.Context().Value(clientAddressKey{}).(string); ok && address != "" {
		return address
	}
	return connectionAddress(r)
}

// connectionAddress — адрес сетевого соединения без порта.
func connectionAddress(r *http.Request) string {
	address := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(address); err == nil {
		address = host
	}
	return strings.Trim(address, "[]")
}

type forwardedOutcome int

const (
	// forwardedAbsent — заголовка нет, клиентом считается сосед по соединению.
	forwardedAbsent forwardedOutcome = iota
	// forwardedUsed — адрес клиента взят из заголовка доверенного proxy.
	forwardedUsed
	// forwardedMalformed — цепочка повреждена или слишком длинна; клиентом
	// остаётся ближайший к API доверенный узел, то есть предел не обходится.
	forwardedMalformed
	// forwardedIgnoredNoTrustedProxies — заголовок пришёл, но список пуст.
	forwardedIgnoredNoTrustedProxies
	// forwardedIgnoredUntrustedPeer — заголовок пришёл от узла вне списка.
	forwardedIgnoredUntrustedPeer
)

func (outcome forwardedOutcome) String() string {
	switch outcome {
	case forwardedAbsent:
		return "absent"
	case forwardedUsed:
		return "used"
	case forwardedMalformed:
		return "malformed"
	case forwardedIgnoredNoTrustedProxies:
		return "ignored_no_trusted_proxies"
	case forwardedIgnoredUntrustedPeer:
		return "ignored_untrusted_peer"
	}
	return "unknown"
}

type clientResolver struct {
	trusted []netip.Prefix
	logger  *slog.Logger
	now     func() time.Time
	// lastWarning — время последнего предупреждения (UnixNano), 0 — не было.
	lastWarning atomic.Int64
}

// resolve определяет адрес клиента запроса. Заголовок читается только от
// доверенного соседа; записи обходятся справа налево: каждый proxy дописывает
// адрес своего соседа в конец, поэтому правый край честен, а всё левее первого
// недоверенного адреса подконтрольно клиенту и не читается.
func (resolver *clientResolver) resolve(r *http.Request) (string, forwardedOutcome) {
	connection := connectionAddress(r)
	values := r.Header.Values(forwardedForHeader)
	if len(values) == 0 {
		return connection, forwardedAbsent
	}
	peer, ok := parseAddress(connection)
	if !ok || !resolver.isTrusted(peer) {
		if len(resolver.trusted) == 0 {
			return connection, forwardedIgnoredNoTrustedProxies
		}
		return connection, forwardedIgnoredUntrustedPeer
	}
	rest := strings.Join(values, ",")
	current, seen := peer, 0
	for hops := 0; hops < maxForwardedHops; hops++ {
		cut := strings.LastIndexByte(rest, ',')
		entry := strings.TrimSpace(rest[cut+1:])
		if cut >= 0 {
			rest = rest[:cut]
		}
		if entry != "" {
			seen++
			address, ok := parseAddress(entry)
			if !ok {
				return current.String(), forwardedMalformed
			}
			if !resolver.isTrusted(address) {
				return address.String(), forwardedUsed
			}
			current = address
		}
		if cut < 0 {
			if seen == 0 {
				return peer.String(), forwardedMalformed
			}
			// Все записи — доверенные узлы: клиентом считается самый дальний.
			return current.String(), forwardedUsed
		}
	}
	return current.String(), forwardedMalformed
}

func (resolver *clientResolver) isTrusted(address netip.Addr) bool {
	for _, prefix := range resolver.trusted {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

// parseAddress разбирает адрес соединения или запись заголовка: «ip»,
// «ip:port», «[ip]» и «[ip]:port». IPv4 внутри IPv6 приводится к IPv4, зона
// IPv6 отбрасывается, чтобы один клиент не получал несколько ключей.
func parseAddress(text string) (netip.Addr, bool) {
	text = strings.TrimSpace(text)
	if address, err := netip.ParseAddr(text); err == nil {
		return normalizeAddress(address), true
	}
	if withPort, err := netip.ParseAddrPort(text); err == nil {
		return normalizeAddress(withPort.Addr()), true
	}
	if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
		if address, err := netip.ParseAddr(text[1 : len(text)-1]); err == nil {
			return normalizeAddress(address), true
		}
	}
	return netip.Addr{}, false
}

func normalizeAddress(address netip.Addr) netip.Addr {
	return address.Unmap().WithZone("")
}

// middleware определяет адрес один раз на запрос и кладёт его в контекст.
func (resolver *clientResolver) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		address, outcome := resolver.resolve(r)
		resolver.report(outcome)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientAddressKey{}, address)))
	})
}

// report предупреждает, что заголовок не принят: за proxy без настройки все
// клиенты делят один адрес, а при прямом обращении мимо proxy его подделывают.
// Адреса и значения заголовка в журнал не пишутся.
func (resolver *clientResolver) report(outcome forwardedOutcome) {
	var reason, message string
	switch outcome {
	case forwardedIgnoredNoTrustedProxies:
		reason = "no_trusted_proxies"
		message = "X-Forwarded-For получен, но LIDRADAR_TRUSTED_PROXIES не задан: заголовок проигнорирован, клиенты за proxy делят один адрес"
	case forwardedIgnoredUntrustedPeer:
		reason = "peer_not_trusted"
		message = "X-Forwarded-For получен от узла вне LIDRADAR_TRUSTED_PROXIES и проигнорирован: прямой доступ к API мимо proxy либо в списке нет адреса proxy"
	default:
		return
	}
	now := resolver.now().UnixNano()
	for {
		last := resolver.lastWarning.Load()
		if last != 0 && now-last < int64(ignoredHeaderWarningPeriod) {
			return
		}
		if resolver.lastWarning.CompareAndSwap(last, now) {
			break
		}
	}
	resolver.logger.Warn(message, "event", "http.forwarded_header_ignored", "reason", reason)
}
