package server

import (
	"encoding/json"
	"sync"
)

// event is a message from the server to the browser.
type event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// Hub fans events out to the open browsers over SSE.
//
// It is what replaces the bubbletea loop: since the review runs outside the
// request that asked for it, this is the only way the page learns it finished.
type Hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

// NewHub creates an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: map[chan []byte]struct{}{}}
}

// Subscribe opens an event channel for a client.
func (h *Hub) Subscribe() chan []byte {
	ch := make(chan []byte, 32)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

// Unsubscribe closes the channel of a client that went away.
func (h *Hub) Unsubscribe(ch chan []byte) {
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// Broadcast sends an event to everyone. A slow client is skipped instead of
// holding up the worker that is publishing.
func (h *Hub) Broadcast(kind string, data any) {
	payload, err := json.Marshal(event{Type: kind, Data: data})
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- payload:
		default:
		}
	}
}

// Clients says how many browsers are listening.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
