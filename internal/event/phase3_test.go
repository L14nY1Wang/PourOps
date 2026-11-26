package event

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pourops/internal/platform"
)

func TestIsBlockedWebhookIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1",
		"10.0.0.1",
		"172.16.0.1",
		"192.168.1.1",
		"100.64.0.1",
		"169.254.169.254",
		"192.0.2.1",
		"198.18.0.1",
		"255.255.255.255",
		"::1",
		"fe80::1",
		"fc00::1",
		"64:ff9b::10.0.0.1",
		"2002:0a00:0001::1",
		"::ffff:127.0.0.1",
		"::10.0.0.1",
		"fd00:ec2::254",
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("failed to parse IP %q", s)
		}
		if !platform.IsBlockedOutboundIP(ip, false) {
			t.Errorf("IsBlockedOutboundIP(%q) = false, want true", s)
		}
	}

	for _, pub := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		ip := net.ParseIP(pub)
		if platform.IsBlockedOutboundIP(ip, false) {
			t.Errorf("IsBlockedOutboundIP(%q) = true, want false", pub)
		}
	}

	// allowPrivate 仍必须拦截云元数据地址，不允许绕过
	if !platform.IsBlockedOutboundIP(net.ParseIP("169.254.169.254"), true) {
		t.Errorf("metadata IPv4 should remain blocked when allowPrivate=true")
	}
	if !platform.IsBlockedOutboundIP(net.ParseIP("fd00:ec2::254"), true) {
		t.Errorf("metadata IPv6 should remain blocked when allowPrivate=true")
	}
	if platform.IsBlockedOutboundIP(net.ParseIP("127.0.0.1"), true) {
		t.Errorf("loopback should be allowed when allowPrivate=true")
	}

	for _, badURL := range []string{
		"http://[::1%25eth0]/hook",
		"http://100.64.1.2/hook",
		"http://[64:ff9b::10.0.0.1]/hook",
		"http://[2002:7f00:0001::1]/hook",
	} {
		if err := ValidateWebhookURL(badURL); err == nil {
			t.Errorf("ValidateWebhookURL(%q) = nil, want error", badURL)
		}
	}
}

func TestSafeWebhookClientBlocksDNSAndLoopbackDial(t *testing.T) {
	// localhost 绕过字面 IP 检查测试，DialContext 应在连接阶段拦截并输出脱敏错误
	cfg := staticWebhookConfig{
		Brand:  "PourOps",
		URL:    "http://localhost:8089/hook",
		Events: []string{string(EventOrderPaid)},
	}
	whCfg := WebhookConfig{
		Brand:  cfg.Brand,
		URL:    cfg.URL,
		Events: cfg.Events,
	}
	d := NewWebhookDispatcher(cfg, nil)
	err := d.deliverOnce(context.Background(), whCfg, Event{Type: EventOrderPaid})
	if err == nil {
		t.Fatalf("expected deliverOnce to localhost without WEBHOOK_ALLOW_PRIVATE=1 to fail")
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "::1") {
		t.Errorf("deliverOnce leaked internal IP in error: %v", err)
	}
}

func TestWebhookDispatcherCircuitBreaker(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{
		Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Body:       io.NopCloser(strings.NewReader("bad gateway")),
				Header:     make(http.Header),
			}, nil
		}),
	}
	cfg := staticWebhookConfig{
		Brand:  "PourOps",
		URL:    "https://example.com/hook",
		Events: []string{string(EventOrderPaid)},
	}
	d := &WebhookDispatcher{
		cfg:          cfg,
		client:       client,
		retryBackoff: time.Millisecond,
		queue:        make(chan Event, webhookQueueSize),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
	go d.worker()

	// 连续入队 6 个必定失败的事件：前 5 个触发阈值（每次含重试最多 2 次请求，共 10 次），第 6 个应被熔断跳过
	for i := range 6 {
		d.Enqueue(Event{
			ID:   fmt.Sprintf("cb-%d", i),
			Type: EventOrderPaid,
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := calls.Load(); got != 10 {
		t.Fatalf("expected open circuit breaker to skip 6th queued event (5*2=10 calls), got calls=%d", got)
	}

	d.mu.Lock()
	consec := d.consecFail
	openUntil := d.openUntil
	d.mu.Unlock()
	if consec < webhookBreakerThreshold || !openUntil.After(time.Now()) {
		t.Fatalf("expected breaker open; consec=%d openUntil=%v", consec, openUntil)
	}

	d.recordOutcome(true)
	d.mu.Lock()
	consec = d.consecFail
	openUntil = d.openUntil
	d.mu.Unlock()
	if consec != 0 || !openUntil.IsZero() {
		t.Fatalf("expected circuit breaker reset after success; consec=%d openUntil=%v", consec, openUntil)
	}
}
