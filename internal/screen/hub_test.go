package screen

import (
	"context"
	"testing"

	"pourops/internal/event"
)

func TestHubSubscribesToScreenChangedEvent(t *testing.T) {
	bus := event.NewEventBus()
	hub := NewHub(bus)
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	// 其他领域事件不会误触发电视酒单刷新
	bus.Publish(context.Background(), event.EventOrderPaid, "吧台 1", nil)
	select {
	case <-ch:
		t.Fatal("EventOrderPaid 不应触发电视 SSE 刷新")
	default:
	}

	bus.Publish(context.Background(), event.EventScreenChanged, "01", nil)
	select {
	case <-ch:
	default:
		t.Fatal("EventScreenChanged 应唤醒电视 SSE 订阅者")
	}
}
