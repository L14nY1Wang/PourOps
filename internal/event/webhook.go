package event

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"pourops/internal/platform"
)

const (
	webhookQueueSize        = 256
	webhookHTTPTimeout      = 5 * time.Second
	webhookRetryBackoff     = 200 * time.Millisecond
	webhookBreakerThreshold = 5
	webhookBreakerCooldown  = 30 * time.Second
)

// WebhookEventOptions 是系统设置页与 Webhook 订阅器共享的领域事件选项。
var WebhookEventOptions = []platform.Option{
	{Value: string(EventOrderPaid), Label: "POS 结账收款 (order.paid)"},
	{Value: string(EventBeerPurchased), Label: "进货入库 (beer.purchased)"},
	{Value: string(EventBeerTapped), Label: "酒款开桶上架 (beer.tapped)"},
	{Value: string(EventKegFinished), Label: "下架结桶复盘 (keg.finished)"},
	{Value: string(EventRetailLowStock), Label: "零售低库存 / 售罄预警 (retail.low_stock)"},
	{Value: string(EventMemberRecharged), Label: "会员储值充值 (member.recharged)"},
	{Value: string(EventPassPurchased), Label: "办理或续费月卡 (pass.purchased)"},
	{Value: string(EventPassRedeemed), Label: "月卡核销免单 (pass.redeemed)"},
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

// ValidateWebhookURL 仅允许 http(s)://，拦截控制字符、云元数据、内网与特殊保留地址（除非设置 WEBHOOK_ALLOW_PRIVATE=1）。
func ValidateWebhookURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return platform.ValidateOutboundURL(raw, "Webhook 地址", platform.EnvBool("WEBHOOK_ALLOW_PRIVATE"))
}

// WebhookDispatcher 把领域事件异步投递到门店配置的签名 HTTP Webhook，失败不影响主交易链路。
type WebhookDispatcher struct {
	cfg          WebhookConfigProvider
	client       *http.Client
	retryBackoff time.Duration

	queue        chan Event
	workerCtx    context.Context
	cancelWorker context.CancelFunc
	done         chan struct{}

	mu         sync.Mutex
	closed     bool
	consecFail int
	openUntil  time.Time
}

func newSafeWebhookHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   webhookHTTPTimeout,
		KeepAlive: 30 * time.Second,
	}
	tr := &http.Transport{
		Proxy:       nil,
		DialContext: platform.NewSafeDialContext(dialer, func() bool { return platform.EnvBool("WEBHOOK_ALLOW_PRIVATE") }),
	}
	return &http.Client{
		Timeout:   webhookHTTPTimeout,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func NewWebhookDispatcher(cfg WebhookConfigProvider, client *http.Client) *WebhookDispatcher {
	if client == nil {
		client = newSafeWebhookHTTPClient()
	}
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	d := &WebhookDispatcher{
		cfg:          cfg,
		client:       client,
		retryBackoff: webhookRetryBackoff,
		queue:        make(chan Event, webhookQueueSize),
		workerCtx:    workerCtx,
		cancelWorker: cancelWorker,
		done:         make(chan struct{}),
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
		EventMemberRecharged,
		EventPassPurchased,
		EventPassRedeemed,
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
		d.mu.Lock()
		circuitOpen := !d.openUntil.IsZero() && time.Now().Before(d.openUntil)
		d.mu.Unlock()
		if circuitOpen {
			log.Printf("webhook breaker open: skipped %s (%s)", ev.Type, ev.ID)
			continue
		}
		parentCtx := d.workerCtx
		if parentCtx == nil {
			parentCtx = context.Background()
		}
		ctx, cancel := context.WithTimeout(parentCtx, webhookHTTPTimeout*2)
		if err := d.deliverWithRetry(ctx, ev); err != nil {
			log.Printf("webhook deliver %s (%s): %v", ev.Type, ev.ID, err)
			d.recordOutcome(false)
		} else {
			d.recordOutcome(true)
		}
		cancel()
	}
}

func (d *WebhookDispatcher) recordOutcome(ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ok {
		d.consecFail = 0
		d.openUntil = time.Time{}
		return
	}
	d.consecFail++
	if d.consecFail >= webhookBreakerThreshold {
		d.openUntil = time.Now().Add(webhookBreakerCooldown)
		log.Printf("webhook circuit breaker opened for %v after %d consecutive failures", webhookBreakerCooldown, d.consecFail)
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
		if d.cancelWorker != nil {
			d.cancelWorker()
		}
		return nil
	case <-ctx.Done():
		if d.cancelWorker != nil {
			d.cancelWorker()
		}
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
	err := d.deliverOnce(ctx, cfg, ev)
	if err == nil {
		d.recordOutcome(true)
	}
	return err
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
	backoff := d.retryBackoff
	if backoff <= 0 {
		backoff = webhookRetryBackoff
	}
	select {
	case <-ctx.Done():
		return err
	case <-time.After(backoff):
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
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "PourOps-Webhook/1.0")
	req.Header.Set("X-Bar-Event", string(ev.Type))
	req.Header.Set("X-Bar-Delivery", ev.ID)
	req.Header.Set("X-Bar-Timestamp", ts)
	if cfg.Secret != "" {
		req.Header.Set("X-Bar-Signature", SignWebhookBody(cfg.Secret, body))
		req.Header.Set("X-Bar-Signature-V2", SignWebhookBodyWithTimestamp(cfg.Secret, ts, body))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		// 提取内层 UserError.Msg，避免把 net/http *url.Error 包裹的含秘钥查询参数 URL 写进错误横幅或 Cookie。
		var ue platform.UserError
		if errors.As(err, &ue) {
			return ue
		}
		log.Printf("webhook request (%s): %v", cfg.URL, err)
		return platform.UserErr("无法连接到 Webhook 地址，请检查网络或目标地址")
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

// SignWebhookBodyWithTimestamp 计算带时间戳防重放的 HMAC-SHA256 签名（HMAC(secret, ts + "." + body)）。
func SignWebhookBodyWithTimestamp(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
