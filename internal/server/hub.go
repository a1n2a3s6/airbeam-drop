package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Hub manages Server-Sent Events (SSE) subscribers and broadcasts real-time updates
// across all connected phones and computers on the local network.
type Hub struct {
	mu      sync.RWMutex
	clients map[chan []byte]struct{}
}

// NewHub constructs an initialized SSE broadcaster.
func NewHub() *Hub {
	return &Hub{
		clients: make(map[chan []byte]struct{}),
	}
}

// ClientCount returns the current number of active SSE connections.
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Broadcast serializes payload as JSON and pushes an SSE event to all connected clients.
func (h *Hub) Broadcast(event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	frame := formatSSE(event, data)

	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.clients {
		select {
		case ch <- frame:
		default:
			// Drop message for stalled clients so fast LAN clients are never blocked.
		}
	}
}

// ServeHTTP handles GET /api/events SSE connections.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan []byte, 32)
	count := h.register(ch)
	defer func() {
		remaining := h.unregister(ch)
		h.Broadcast("devices", map[string]int{"count": remaining})
	}()

	// Send immediate welcome event with current device count.
	if initData, err := json.Marshal(map[string]int{"count": count}); err == nil {
		_, _ = w.Write(formatSSE("devices", initData))
		flusher.Flush()
	}
	// Notify all other connected devices about the updated count.
	h.Broadcast("devices", map[string]int{"count": count})

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, open := <-ch:
			if !open {
				return
			}
			if _, err := w.Write(msg); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := w.Write([]byte(": heartbeat\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (h *Hub) register(ch chan []byte) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[ch] = struct{}{}
	return len(h.clients)
}

func (h *Hub) unregister(ch chan []byte) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.clients[ch]; exists {
		delete(h.clients, ch)
		close(ch)
	}
	return len(h.clients)
}

func formatSSE(event string, data []byte) []byte {
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, data))
}
