package screen

import (
	"sync"

	"pourops/internal/event"
)

const maxScreenSSEClients = 64

// Hub 维护在线电视页面 SSE 连接，并订阅领域事件 EventScreenChanged 自动通知刷新酒单。
type Hub struct {
	mu      sync.Mutex
	clients map[chan struct{}]struct{}
	done    chan struct{}
	closed  bool
}

func NewHub(bus *event.EventBus) *Hub {
	h := &Hub{
		clients: make(map[chan struct{}]struct{}),
		done:    make(chan struct{}),
	}
	if bus != nil {
		bus.Subscribe(func(event.Event) {
			h.Broadcast()
		}, event.EventScreenChanged)
	}
	return h
}

// SubscribeChecked 注册一个 SSE 订阅通道；当连接达上限或 Hub 已关闭时返回 ok=false。
func (h *Hub) SubscribeChecked() (chan struct{}, <-chan struct{}, bool) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done == nil {
		h.done = make(chan struct{})
	}
	if h.closed || len(h.clients) >= maxScreenSSEClients {
		return nil, h.done, false
	}
	h.clients[ch] = struct{}{}
	return ch, h.done, true
}

func (h *Hub) Unsubscribe(ch chan struct{}) {
	if ch == nil {
		return
	}
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

// Close 主动唤醒并断开全部 SSE 连接，使 HTTP Server 优雅关停不再等待长连接过期。
func (h *Hub) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	if h.done != nil {
		close(h.done)
	}
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
