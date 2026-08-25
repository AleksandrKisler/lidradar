package transport

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

type Signal struct{ Type, ResourceID string }

// Hub distributes ephemeral invalidation signals. It intentionally stores no
// business state; slow/disconnected clients recover through REST refetches.
type Hub struct {
	mu   sync.RWMutex
	next uint64
	subs map[string]map[uint64]chan Signal
}

func NewHub() *Hub { return &Hub{subs: make(map[string]map[uint64]chan Signal)} }

func (h *Hub) Publish(tenantID, eventType, resourceID string) {
	if h == nil || tenantID == "" || !validSignalType(eventType) || resourceID == "" {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ch := range h.subs[tenantID] {
		select {
		case ch <- Signal{eventType, resourceID}:
		default:
		}
	}
}

func validSignalType(eventType string) bool {
	switch eventType {
	case "risk.changed", "risk.acknowledged", "risk.resolved":
		return true
	default:
		return false
	}
}
func (h *Hub) subscribe(tenant string) (<-chan Signal, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	id := h.next
	ch := make(chan Signal, 16)
	if h.subs[tenant] == nil {
		h.subs[tenant] = make(map[uint64]chan Signal)
	}
	h.subs[tenant][id] = ch
	return ch, func() {
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
		writeError(w, r, 500, "INTERNAL", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, cancel := h.events.subscribe(t)
	defer cancel()
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case signal := <-ch:
			_, _ = fmt.Fprintf(w, "event: %s\ndata: {\"resourceId\":%q}\n\n", signal.Type, signal.ResourceID)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}
