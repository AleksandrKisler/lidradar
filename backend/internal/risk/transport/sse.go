package transport

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

type Signal struct{ Type, ResourceID string }

// ResyncEvent — маркер потери сигналов: буфер подписчика переполнился, часть
// сигналов отброшена, клиент обязан перечитать Radar и открытые списки целиком
// (GAP-RELIABILITY-020). Сам маркер не несёт бизнес-состояния.
const ResyncEvent = "resync.required"

// subscriberBuffer — число сигналов, которые подписчик может не успеть
// прочитать до того, как получит маркер ресинхронизации.
const subscriberBuffer = 16

// heartbeatInterval — период комментариев heartbeat: они держат соединение
// живым за прокси и позволяют клиенту отличить тишину от обрыва.
const heartbeatInterval = 20 * time.Second

// streamWriteTimeout — предел одной записи в поток: столько ждём клиента,
// который перестал читать, прежде чем закрыть соединение.
const streamWriteTimeout = 30 * time.Second

type subscriber struct {
	signals chan Signal
	resync  chan struct{}
}

// Hub distributes ephemeral invalidation signals. It intentionally stores no
// business state; slow/disconnected clients recover through REST refetches.
type Hub struct {
	mu        sync.RWMutex
	next      uint64
	buffer    int
	heartbeat time.Duration
	subs      map[string]map[uint64]*subscriber
}

func NewHub() *Hub {
	return &Hub{buffer: subscriberBuffer, heartbeat: heartbeatInterval, subs: make(map[string]map[uint64]*subscriber)}
}

func (h *Hub) Publish(tenantID, eventType, resourceID string) {
	if h == nil || tenantID == "" || !validSignalType(eventType) || resourceID == "" {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, sub := range h.subs[tenantID] {
		select {
		case sub.signals <- Signal{eventType, resourceID}:
		default:
			// Буфер полон: сигнал теряется, вместо него подписчик получит
			// один маркер ресинхронизации.
			select {
			case sub.resync <- struct{}{}:
			default:
			}
		}
	}
}

func validSignalType(eventType string) bool {
	switch eventType {
	case "risk.changed", "risk.acknowledged", "risk.resolved", "risk.false_positive":
		return true
	default:
		return false
	}
}
func (h *Hub) subscribe(tenant string) (*subscriber, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	id := h.next
	sub := &subscriber{signals: make(chan Signal, h.buffer), resync: make(chan struct{}, 1)}
	if h.subs[tenant] == nil {
		h.subs[tenant] = make(map[uint64]*subscriber)
	}
	h.subs[tenant][id] = sub
	return sub, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if group := h.subs[tenant]; group != nil {
			delete(group, id)
			if len(group) == 0 {
				delete(h.subs, tenant)
			}
		}
	}
}

func (h Handler) stream(w http.ResponseWriter, r *http.Request) {
	a, t, ok := h.principal(w, r)
	if !ok {
		return
	}
	if err := h.radar.CanRead(r.Context(), a, t); handleError(w, r, err) {
		return
	}
	if h.events == nil {
		writeError(w, r, 503, "UNAVAILABLE", "event stream unavailable")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, 500, "INTERNAL_ERROR", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	sub, cancel := h.events.subscribe(t)
	defer cancel()
	controller := http.NewResponseController(w)
	// WriteTimeout сервера отсчитывается от начала запроса и без продления
	// оборвал бы поток вскоре после второго heartbeat. Дедлайн переносится перед
	// каждой записью: живой клиент не прерывается, а клиент, перестав читать,
	// отсекается через streamWriteTimeout. Писатели без поддержки дедлайнов
	// (тестовый recorder) возвращают ошибку, которую можно не учитывать.
	send := func(format string, args ...any) {
		_ = controller.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
		_, _ = fmt.Fprintf(w, format, args...)
		flusher.Flush()
	}
	send(": connected\n\n")
	heartbeat := time.NewTicker(h.events.heartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.resync:
			// Накопленные сигналы избыточны после полного перечитывания.
			drainSignals(sub.signals)
			send("event: %s\ndata: {\"reason\":\"BUFFER_OVERFLOW\"}\n\n", ResyncEvent)
		case signal := <-sub.signals:
			send("event: %s\ndata: {\"resourceId\":%q}\n\n", signal.Type, signal.ResourceID)
		case <-heartbeat.C:
			send(": heartbeat\n\n")
		}
	}
}

func drainSignals(signals <-chan Signal) {
	for {
		select {
		case <-signals:
		default:
			return
		}
	}
}
