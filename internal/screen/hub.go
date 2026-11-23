package screen

import (
	"sync"

	"pourops/internal/event"
)

// Hub 维护在线电视页面 SSE 连接，并订阅领域事件 EventScreenChanged 自动通知刷新酒单。
type Hub struct {
	mu      sync.Mutex
	clients map[chan struct{}]struct{}
}

func NewHub(bus *event.EventBus) *Hub {
	h := &Hub{clients: make(map[chan struct{}]struct{})}
	if bus != nil {
		bus.Subscribe(func(event.Event) {
			h.Broadcast()
		}, event.EventScreenChanged)
	}
	return h
}

func (h *Hub) Subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) Unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

func (h *Hub) Broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
