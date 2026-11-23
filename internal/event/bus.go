package event

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"pourops/internal/platform"
)

// EventType 标识进程内广播与 Webhook 可订阅的领域事件类型。
type EventType string

const (
	EventScreenChanged  EventType = "screen.changed"
	EventBeerPurchased  EventType = "beer.purchased"
	EventBeerTapped     EventType = "beer.tapped"
	EventKegFinished    EventType = "keg.finished"
	EventOrderPaid      EventType = "order.paid"
	EventRetailLowStock EventType = "retail.low_stock"
	EventWebhookTest    EventType = "webhook.test"
)

// Event 是领域事件的标准封装，所有敏感业务由调用方在事务提交后发布。
type Event struct {
	ID         string    `json:"id"`
	Type       EventType `json:"type"`
	OccurredAt time.Time `json:"occurred_at"`
	Actor      string    `json:"actor,omitempty"`
	Subject    string    `json:"subject,omitempty"`
	Payload    any       `json:"payload,omitempty"`
}

type subscriber struct {
	types map[EventType]bool // nil 表示订阅全部事件
	fn    func(Event)
}

// EventBus 提供轻量的进程内事件总线：隔离 SSE 广播与异步 Webhook 等后续扩展。
type EventBus struct {
	seq     atomic.Uint64
	mu      sync.RWMutex
	subs    []subscriber
	closers []func(context.Context) error
}

func NewEventBus() *EventBus {
	return &EventBus{}
}

// Subscribe 注册事件处理回调；不传 types 时订阅全部事件。
func (b *EventBus) Subscribe(fn func(Event), types ...EventType) {
	if fn == nil {
		return
	}
	var set map[EventType]bool
	if len(types) > 0 {
		set = make(map[EventType]bool, len(types))
		for _, t := range types {
			set[t] = true
		}
	}
	b.mu.Lock()
	b.subs = append(b.subs, subscriber{types: set, fn: fn})
	b.mu.Unlock()
}

// RegisterCloser 注册关停钩子（如异步 Webhook 队列排水）。
func (b *EventBus) RegisterCloser(fn func(context.Context) error) {
	if fn == nil {
		return
	}
	b.mu.Lock()
	b.closers = append(b.closers, fn)
	b.mu.Unlock()
}

// Publish 构建事件并派发给所有匹配的订阅者（单订阅者 panic 不影响主业务和其他订阅者）。
func (b *EventBus) Publish(ctx context.Context, typ EventType, subject string, payload any) {
	if b == nil {
		return
	}
	id := fmt.Sprintf("%d-%d", time.Now().UTC().UnixNano(), b.seq.Add(1))
	ev := Event{
		ID:         id,
		Type:       typ,
		OccurredAt: time.Now().UTC(),
		Actor:      platform.OperationActor(ctx),
		Subject:    subject,
		Payload:    payload,
	}

	b.mu.RLock()
	subs := append([]subscriber(nil), b.subs...)
	b.mu.RUnlock()

	for _, s := range subs {
		if s.types != nil && !s.types[typ] {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("event %s subscriber panic: %v", typ, r)
				}
			}()
			s.fn(ev)
		}()
	}
}

// Close 在服务停机时依序排水关闭所有已注册的异步订阅者。
func (b *EventBus) Close(ctx context.Context) error {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	closers := append([]func(context.Context) error(nil), b.closers...)
	b.mu.RUnlock()

	var firstErr error
	for _, fn := range closers {
		if err := fn(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
