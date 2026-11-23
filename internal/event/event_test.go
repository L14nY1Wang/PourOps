package event

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pourops/internal/platform"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type staticWebhookConfig WebhookConfig

func (c staticWebhookConfig) WebhookConfig() WebhookConfig { return WebhookConfig(c) }

func TestEventBusSubscribeFilterAndPanicIsolation(t *testing.T) {
	bus := NewEventBus()
	var paidCount, allCount atomic.Int32

	bus.Subscribe(func(Event) {
		panic("boom")
	}, EventOrderPaid)

	bus.Subscribe(func(ev Event) {
		paidCount.Add(1)
		if ev.Actor != "bar1" {
			t.Errorf("Actor = %q, want bar1", ev.Actor)
		}
	}, EventOrderPaid)

	bus.Subscribe(func(Event) {
		allCount.Add(1)
	})

	ctx := platform.WithActor(context.Background(), "bar1")
	bus.Publish(ctx, EventScreenChanged, "01", nil)
	bus.Publish(ctx, EventOrderPaid, "吧台 1", map[string]int{"total": 5000})

	if paidCount.Load() != 1 || allCount.Load() != 2 {
		t.Fatalf("paid=%d all=%d", paidCount.Load(), allCount.Load())
	}
}

func TestWebhookDispatcherSignatureFilterRetryAndDrain(t *testing.T) {
	var attempts atomic.Int32
	received := make(chan struct {
		ev  Event
		sig string
	}, 4)

	mockClient := &http.Client{
		Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			n := attempts.Add(1)
			body, _ := io.ReadAll(r.Body)
			var ev Event
			_ = json.Unmarshal(body, &ev)
			if n == 1 {
				return &http.Response{
					StatusCode: http.StatusInternalServerError,
					Body:       io.NopCloser(strings.NewReader("err")),
					Header:     make(http.Header),
				}, nil
			}
			received <- struct {
				ev  Event
				sig string
			}{ev: ev, sig: r.Header.Get("X-Bar-Signature")}
			if got := SignWebhookBody("sec-123", body); got != r.Header.Get("X-Bar-Signature") {
				t.Errorf("signature mismatch: got %q want %q", r.Header.Get("X-Bar-Signature"), got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
				Header:     make(http.Header),
			}, nil
		}),
	}

	cfg := staticWebhookConfig{
		Brand:  "PourOps",
		URL:    "https://example.com/hook",
		Secret: "sec-123",
		Events: []string{string(EventOrderPaid)},
	}

	bus := NewEventBus()
	dispatcher := NewWebhookDispatcher(cfg, mockClient)
	dispatcher.Attach(bus)

	// 未订阅事件不会发出 HTTP 请求
	bus.Publish(context.Background(), EventBeerTapped, "01", nil)
	// 已订阅事件触发，并在首包 500 后自动重试一次成功
	bus.Publish(platform.WithActor(context.Background(), "bar1"), EventOrderPaid, "吧台 1", map[string]int{"amount": 5000})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := bus.Close(ctx); err != nil {
		t.Fatalf("bus.Close: %v", err)
	}

	select {
	case got := <-received:
		if got.ev.Type != EventOrderPaid || got.sig == "" {
			t.Fatalf("unexpected received webhook: %+v", got)
		}
	default:
		t.Fatal("expected webhook delivery after close drain")
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2 (1 initial + 1 retry)", attempts.Load())
	}
}

func TestValidateWebhookURLBlocksSSRF(t *testing.T) {
	for _, okURL := range []string{"", "https://example.com/hook", "http://127.0.0.1:9000/hook"} {
		if err := ValidateWebhookURL(okURL); err != nil {
			t.Errorf("ValidateWebhookURL(%q) = %v, want nil", okURL, err)
		}
	}
	for _, badURL := range []string{
		"ftp://example.com/hook",
		"http://169.254.169.254/latest/meta-data",
		"http://metadata.google.internal/computeMetadata",
		"https://example.com/\nhook",
	} {
		if err := ValidateWebhookURL(badURL); err == nil {
			t.Errorf("ValidateWebhookURL(%q) = nil, want error", badURL)
		}
	}
}
