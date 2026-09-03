package httpplatform

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// securityHeaders выставляет заголовки, которые API отдаёт всегда (LR-BE-2405):
// ответы не кэшируются, тип содержимого не угадывается, встраивание в фреймы
// и утечка Referer запрещены. HSTS включается только для TLS-соединений либо
// когда прокси завершает TLS и cookie помечены Secure.
func securityHeaders(strictTransport bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := w.Header()
			header.Set("X-Content-Type-Options", "nosniff")
			header.Set("X-Frame-Options", "DENY")
			header.Set("Referrer-Policy", "no-referrer")
			header.Set("Cache-Control", "no-store")
			header.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
			if strictTransport || r.TLS != nil {
				header.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimit — ограничение частоты запросов на сетевой адрес для маршрутов,
// доступных без сессии (вход, регистрация, вебхуки). Счётчик в памяти
// процесса; ограничение аутентификации по учётной записи хранится в
// PostgreSQL отдельно (этап «критические усиления»).
type RateLimit struct {
	Requests int
	Window   time.Duration
	Prefixes []string
}

type rateWindow struct {
	started time.Time
	count   int
}

type rateLimiter struct {
	limit   RateLimit
	now     func() time.Time
	mutex   sync.Mutex
	windows map[string]rateWindow
}

func newRateLimiter(limit RateLimit, now func() time.Time) *rateLimiter {
	return &rateLimiter{limit: limit, now: now, windows: make(map[string]rateWindow)}
}

// allow считает запросы в фиксированном окне и возвращает секунды до его конца.
func (limiter *rateLimiter) allow(key string) (bool, int) {
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()
	now := limiter.now()
	window, known := limiter.windows[key]
	if !known || now.Sub(window.started) >= limiter.limit.Window {
		window = rateWindow{started: now}
	}
	window.count++
	limiter.windows[key] = window
	if len(limiter.windows) > 100000 {
		for candidate, item := range limiter.windows {
			if now.Sub(item.started) >= limiter.limit.Window {
				delete(limiter.windows, candidate)
			}
		}
	}
	if window.count > limiter.limit.Requests {
		remaining := limiter.limit.Window - now.Sub(window.started)
		seconds := int(remaining.Seconds())
		if seconds < 1 {
			seconds = 1
		}
		return false, seconds
	}
	return true, 0
}

func (limiter *rateLimiter) applies(path string) bool {
	for _, prefix := range limiter.limit.Prefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func rateLimited(limiter *rateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limiter == nil || limiter.limit.Requests <= 0 || !limiter.applies(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			allowed, retryAfter := limiter.allow(clientAddress(r))
			if !allowed {
				w.Header().Set("Retry-After", itoa(retryAfter))
				WriteError(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientAddress берёт адрес соединения; заголовки прокси не читаются, пока
// доверенные прокси не заданы явно.
func clientAddress(r *http.Request) string {
	address := r.RemoteAddr
	if index := strings.LastIndex(address, ":"); index > 0 && !strings.HasSuffix(address, "]") {
		address = address[:index]
	}
	return strings.Trim(address, "[]")
}

func itoa(value int) string {
	if value <= 0 {
		return "1"
	}
	digits := make([]byte, 0, 4)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
