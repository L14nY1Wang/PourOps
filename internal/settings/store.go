package settings

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/event"
	"pourops/internal/platform"
)

const (
	DefaultSellablePct = 80
	DefaultMarginPct   = 50
	MinSellablePct     = 50
	MaxSellablePct     = 100
	MinMarginPct       = 10
	MaxMarginPct       = 85
)

// PricingConfig 控制桶装可售出酒率与目标毛利率（默认 80% / 50%）。
type PricingConfig struct {
	SellablePct int
	MarginPct   int
}

func DefaultPricingConfig() PricingConfig {
	return PricingConfig{
		SellablePct: DefaultSellablePct,
		MarginPct:   DefaultMarginPct,
	}
}

// Normalized 返回钳位到合法范围的核算配置。
func (c PricingConfig) Normalized() PricingConfig {
	if c.SellablePct <= 0 {
		c.SellablePct = DefaultSellablePct
	} else {
		c.SellablePct = platform.ClampRange(c.SellablePct, MinSellablePct, MaxSellablePct)
	}
	if c.MarginPct <= 0 {
		c.MarginPct = DefaultMarginPct
	} else {
		c.MarginPct = platform.ClampRange(c.MarginPct, MinMarginPct, MaxMarginPct)
	}
	return c
}

// StoreSettings 保存运营参数与 Webhook 出站通知设置。
type StoreSettings struct {
	Brand              string
	DefaultSellablePct int
	DefaultMarginPct   int
	WebhookURL         string
	WebhookSecret      string
	WebhookEvents      []string
}

func (s StoreSettings) Pricing() PricingConfig {
	return PricingConfig{
		SellablePct: s.DefaultSellablePct,
		MarginPct:   s.DefaultMarginPct,
	}.Normalized()
}

func (s StoreSettings) HasWebhookEvent(typ event.EventType) bool {
	if typ == event.EventWebhookTest {
		return true
	}
	want := string(typ)
	for _, ev := range s.WebhookEvents {
		if ev == want {
			return true
		}
	}
	return false
}

// SettingsStore 在进程内存缓存当前门店参数，并同步持久化到 Postgres store_settings 表。
type SettingsStore struct {
	pool         *pgxpool.Pool
	defaultBrand string

	mu  sync.RWMutex
	cur StoreSettings
}

func NewSettingsStore(pool *pgxpool.Pool, defaultBrand string) *SettingsStore {
	if strings.TrimSpace(defaultBrand) == "" {
		defaultBrand = "PourOps"
	}
	return &SettingsStore{
		pool:         pool,
		defaultBrand: defaultBrand,
		cur: StoreSettings{
			Brand:              defaultBrand,
			DefaultSellablePct: DefaultSellablePct,
			DefaultMarginPct:   DefaultMarginPct,
			WebhookEvents:      []string{},
		},
	}
}

func (s *SettingsStore) Refresh(ctx context.Context) error {
	base := StoreSettings{
		Brand:              s.defaultBrand,
		DefaultSellablePct: DefaultSellablePct,
		DefaultMarginPct:   DefaultMarginPct,
		WebhookEvents:      []string{},
	}
	if s.pool == nil {
		s.mu.Lock()
		s.cur = base
		s.mu.Unlock()
		return nil
	}

	rows, err := s.pool.Query(ctx, `select key, value from store_settings`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		switch k {
		case "brand":
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				base.Brand = trimmed
			}
		case "default_sellable_pct":
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= MinSellablePct && n <= MaxSellablePct {
				base.DefaultSellablePct = n
			}
		case "default_margin_pct":
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= MinMarginPct && n <= MaxMarginPct {
				base.DefaultMarginPct = n
			}
		case "webhook_url":
			base.WebhookURL = strings.TrimSpace(v)
		case "webhook_secret":
			base.WebhookSecret = v
		case "webhook_events":
			var clean []string
			seen := make(map[string]bool)
			for _, item := range strings.Split(v, ",") {
				item = strings.TrimSpace(item)
				if event.ValidWebhookEvent(item) && !seen[item] {
					seen[item] = true
					clean = append(clean, item)
				}
			}
			base.WebhookEvents = clean
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	s.cur = base
	s.mu.Unlock()
	return nil
}

func (s *SettingsStore) Get() StoreSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := s.cur
	cp.WebhookEvents = append([]string(nil), s.cur.WebhookEvents...)
	return cp
}

func (s *SettingsStore) Pricing() PricingConfig {
	return s.Get().Pricing()
}

func (s *SettingsStore) Brand() string {
	return s.Get().Brand
}

// WebhookConfig 实现 event.WebhookConfigProvider 接口。
func (s *SettingsStore) WebhookConfig() event.WebhookConfig {
	cfg := s.Get()
	return event.WebhookConfig{
		Brand:  cfg.Brand,
		URL:    cfg.WebhookURL,
		Secret: cfg.WebhookSecret,
		Events: cfg.WebhookEvents,
	}
}

func (s *SettingsStore) Save(ctx context.Context, in StoreSettings) (StoreSettings, error) {
	in.Brand = strings.TrimSpace(in.Brand)
	if in.Brand == "" {
		in.Brand = s.defaultBrand
	}
	if len([]rune(in.Brand)) > 48 {
		return StoreSettings{}, platform.UserErr("门店名称最多 48 个字符")
	}
	if in.DefaultSellablePct < MinSellablePct || in.DefaultSellablePct > MaxSellablePct {
		return StoreSettings{}, platform.UserErrf("默认可售比例需在 %d%% 到 %d%% 之间", MinSellablePct, MaxSellablePct)
	}
	if in.DefaultMarginPct < MinMarginPct || in.DefaultMarginPct > MaxMarginPct {
		return StoreSettings{}, platform.UserErrf("默认目标毛利率需在 %d%% 到 %d%% 之间", MinMarginPct, MaxMarginPct)
	}
	in.WebhookURL = strings.TrimSpace(in.WebhookURL)
	if err := event.ValidateWebhookURL(in.WebhookURL); err != nil {
		return StoreSettings{}, err
	}
	in.WebhookSecret = strings.TrimSpace(in.WebhookSecret)
	if len(in.WebhookSecret) > 256 {
		return StoreSettings{}, platform.UserErr("签名密钥过长（最多 256 字符）")
	}
	cleanEvents := make([]string, 0, len(in.WebhookEvents))
	seen := make(map[string]bool)
	for _, ev := range in.WebhookEvents {
		ev = strings.TrimSpace(ev)
		if ev == "" {
			continue
		}
		if !event.ValidWebhookEvent(ev) {
			return StoreSettings{}, platform.UserErrf("不支持的 Webhook 事件类型：%s", ev)
		}
		if !seen[ev] {
			seen[ev] = true
			cleanEvents = append(cleanEvents, ev)
		}
	}
	in.WebhookEvents = cleanEvents

	if s.pool != nil {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return StoreSettings{}, err
		}
		defer tx.Rollback(ctx)

		pairs := map[string]string{
			"brand":                in.Brand,
			"default_sellable_pct": strconv.Itoa(in.DefaultSellablePct),
			"default_margin_pct":   strconv.Itoa(in.DefaultMarginPct),
			"webhook_url":          in.WebhookURL,
			"webhook_secret":       in.WebhookSecret,
			"webhook_events":       strings.Join(in.WebhookEvents, ","),
		}
		for k, v := range pairs {
			if _, err := tx.Exec(ctx, `
				insert into store_settings (key, value, updated_at)
				values ($1, $2, now())
				on conflict (key) do update set value = excluded.value, updated_at = now()`, k, v); err != nil {
				return StoreSettings{}, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return StoreSettings{}, err
		}
	}

	s.mu.Lock()
	s.cur = StoreSettings{
		Brand:              in.Brand,
		DefaultSellablePct: in.DefaultSellablePct,
		DefaultMarginPct:   in.DefaultMarginPct,
		WebhookURL:         in.WebhookURL,
		WebhookSecret:      in.WebhookSecret,
		WebhookEvents:      append([]string(nil), in.WebhookEvents...),
	}
	s.mu.Unlock()
	return s.Get(), nil
}
