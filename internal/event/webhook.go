package event

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"pourops/internal/platform"
)

const (
	webhookQueueSize    = 256
	webhookHTTPTimeout  = 5 * time.Second
	webhookRetryBackoff = 200 * time.Millisecond
)

// WebhookEventOptions 是系统设置页与 Webhook 订阅器共享的领域事件选项。
var WebhookEventOptions = []platform.Option{
	{Value: string(EventOrderPaid), Label: "POS 结账收款 (order.paid)"},
	{Value: string(EventBeerPurchased), Label: "进货入库 (beer.purchased)"},
	{Value: string(EventBeerTapped), Label: "酒款开桶上架 (beer.tapped)"},
	{Value: string(EventKegFinished), Label: "下架结桶复盘 (keg.finished)"},
	{Value: string(EventRetailLowStock), Label: "零售低库存 / 售罄预警 (retail.low_stock)"},
}

func ValidWebhookEvent(name string) bool {
	for _, opt := range WebhookEventOptions {
		if opt.Value == name {
			return true
		}
	}
	return false
}

// WebhookConfig 描述 Webhook 投递所需的当前配置快照。
type WebhookConfig struct {
	Brand  string
	URL    string
	Secret string
	Events []string
}

func (c WebhookConfig) HasEvent(typ EventType) bool {
	if typ == EventWebhookTest {
		return true
	}
	want := string(typ)
	for _, ev := range c.Events {
		if ev == want {
			return true
		}
	}
	return false
}

// WebhookConfigProvider 解耦事件发送器与门店参数持久化，避免循环依赖。
type WebhookConfigProvider interface {
	WebhookConfig() WebhookConfig
}

// ValidateWebhookURL 仅允许 http(s)://，拦截控制字符与云厂商元数据/链路本地地址（SSRF 基本防护）。
func ValidateWebhookURL(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return platform.UserErr("Webhook 地址包含非法控制字符")
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return platform.UserErr("Webhook 地址格式不正确")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return platform.UserErr("Webhook 地址必须以 http:// 或 https:// 开头")
	}
	host := strings.ToLower(u.Hostname())
	if host == "metadata.google.internal" || strings.HasSuffix(host, ".metadata.google.internal") || host == "169.254.169.254" {
		return platform.UserErr("Webhook 地址不能指向云元数据服务")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
			return platform.UserErr("Webhook 地址不能指向链路本地或无效网络地址")
		}
	}
	return nil
}

// WebhookDispatcher 把领域事件异步投递到门店配置的签名 HTTP Webhook，失败不影响主交易链路。
type WebhookDispatcher struct {
	cfg    WebhookConfigProvider
	client *http.Client

	queue chan Event
	stop  chan struct{}
	done  chan struct{}

	mu     sync.Mutex
	closed bool
}

func NewWebhookDispatcher(cfg WebhookConfigProvider, client *http.Client) *WebhookDispatcher {
	if client == nil {
		client = &http.Client{
			Timeout: webhookHTTPTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	d := &WebhookDispatcher{
		cfg:    cfg,
		client: client,
		queue:  make(chan Event, webhookQueueSize),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go d.worker()
	return d
}

// Attach 挂载到 EventBus 并注册优雅关闭钩子。
func (d *WebhookDispatcher) Attach(bus *EventBus) {
	if d == nil || bus == nil {
		return
	}
	bus.Subscribe(d.Enqueue,
		EventOrderPaid,
		EventBeerPurchased,
		EventBeerTapped,
		EventKegFinished,
		EventRetailLowStock,
	)
	bus.RegisterCloser(d.Close)
}

// Enqueue 非阻塞入队；队列满或已停机时丢弃并记日志，绝不阻塞结账/进货主流程。
func (d *WebhookDispatcher) Enqueue(ev Event) {
	if d == nil || d.cfg == nil {
		return
	}
	cfg := d.cfg.WebhookConfig()
	if cfg.URL == "" || !cfg.HasEvent(ev.Type) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	select {
	case d.queue <- ev:
	default:
		log.Printf("webhook: queue full, dropped %s (%s)", ev.Type, ev.ID)
	}
}

func (d *WebhookDispatcher) worker() {
	defer close(d.done)
	for ev := range d.queue {
		ctx, cancel := context.WithTimeout(context.Background(), webhookHTTPTimeout*2)
		if err := d.deliverWithRetry(ctx, ev); err != nil {
			log.Printf("webhook deliver %s (%s): %v", ev.Type, ev.ID, err)
		}
		cancel()
	}
}

// Close 停止接收入队并等待已入队事件发送完成（或等待 ctx 超时）。
func (d *WebhookDispatcher) Close(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.queue)
	}
	d.mu.Unlock()

	select {
	case <-d.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SendTest 在设置页触发一次同步测试推送，向管理员返回可读的成功或失败诊断。
func (d *WebhookDispatcher) SendTest(ctx context.Context, actor string) error {
	if d == nil || d.cfg == nil {
		return platform.UserErr("Webhook 调度器未初始化")
	}
	cfg := d.cfg.WebhookConfig()
	if cfg.URL == "" {
		return platform.UserErr("请先填写并保存有效的 Webhook URL")
	}
	brand := cfg.Brand
	if brand == "" {
		brand = "PourOps"
	}
	ev := Event{
		ID:         fmt.Sprintf("test-%d", time.Now().UTC().UnixNano()),
		Type:       EventWebhookTest,
		OccurredAt: time.Now().UTC(),
		Actor:      actor,
		Subject:    brand + " Webhook 连通性测试",
		Payload: map[string]any{
			"brand":   brand,
			"message": "Webhook test event from " + brand,
		},
	}
	return d.deliverOnce(ctx, cfg, ev)
}

func (d *WebhookDispatcher) deliverWithRetry(ctx context.Context, ev Event) error {
	cfg := d.cfg.WebhookConfig()
	if cfg.URL == "" {
		return nil
	}
	err := d.deliverOnce(ctx, cfg, ev)
	if err == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return err
	case <-time.After(webhookRetryBackoff):
	}
	return d.deliverOnce(ctx, cfg, ev)
}

func (d *WebhookDispatcher) deliverOnce(ctx context.Context, cfg WebhookConfig, ev Event) error {
	if err := ValidateWebhookURL(cfg.URL); err != nil {
		return err
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return platform.UserErr("创建 Webhook 请求失败")
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "PourOps-Webhook/1.0")
	req.Header.Set("X-Bar-Event", string(ev.Type))
	req.Header.Set("X-Bar-Delivery", ev.ID)
	if cfg.Secret != "" {
		req.Header.Set("X-Bar-Signature", SignWebhookBody(cfg.Secret, body))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return platform.UserErrf("请求 Webhook 失败：%v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return platform.UserErrf("Webhook 返回状态码 %d", resp.StatusCode)
	}
	return nil
}

// SignWebhookBody 计算标准 HMAC-SHA256 请求头值（sha256=<hex>）。
func SignWebhookBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
