package server

import (
	"encoding/json"
	"sync"
)

// event é uma mensagem do servidor para o navegador.
type event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// Hub distribui eventos para os navegadores abertos via SSE.
//
// É o que substitui o loop do bubbletea: como o review roda fora da
// requisição que o pediu, a página só descobre que ele terminou por aqui.
type Hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

// NewHub cria um Hub vazio.
func NewHub() *Hub {
	return &Hub{subs: map[chan []byte]struct{}{}}
}

// Subscribe abre um canal de eventos para um cliente.
func (h *Hub) Subscribe() chan []byte {
	ch := make(chan []byte, 32)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

// Unsubscribe fecha o canal de um cliente que foi embora.
func (h *Hub) Unsubscribe(ch chan []byte) {
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// Broadcast manda um evento para todo mundo. Cliente lento é pulado em vez de
// segurar o worker que está publicando.
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

// Clients diz quantos navegadores estão ouvindo.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
