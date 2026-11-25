package settings

import (
	"context"
	"encoding/json"
	"slices"
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

// StoreSettings 保存运营参数、酒款分类与 Webhook 出站通知设置。
type StoreSettings struct {
	Brand              string
	DefaultSellablePct int
	DefaultMarginPct   int
	BeerCategories     []string
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

	writeMu sync.Mutex
	mu      sync.RWMutex
	cur     StoreSettings
}

func defaultCategoriesCopy() []string {
	return slices.Clone(platform.DefaultBeerCategories)
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
			BeerCategories:     defaultCategoriesCopy(),
			WebhookEvents:      []string{},
		},
	}
}

func (s *SettingsStore) Refresh(ctx context.Context) error {
	base := StoreSettings{
		Brand:              s.defaultBrand,
		DefaultSellablePct: DefaultSellablePct,
		DefaultMarginPct:   DefaultMarginPct,
		BeerCategories:     defaultCategoriesCopy(),
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
		case "beer_categories":
			var raw []string
			if err := json.Unmarshal([]byte(v), &raw); err == nil {
				if clean, normErr := platform.NormalizeBeerCategories(raw); normErr == nil {
					base.BeerCategories = clean
				}
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
	cp.WebhookEvents = slices.Clone(s.cur.WebhookEvents)
	if len(s.cur.BeerCategories) > 0 {
		cp.BeerCategories = slices.Clone(s.cur.BeerCategories)
	} else {
		cp.BeerCategories = defaultCategoriesCopy()
	}
	return cp
}

func (s *SettingsStore) Pricing() PricingConfig {
	return s.Get().Pricing()
}

func (s *SettingsStore) Brand() string {
	return s.Get().Brand
}

func (s *SettingsStore) BeerCategories() []string {
	return s.Get().BeerCategories
}

func (s *SettingsStore) BeerCategoryOptions() []platform.Option {
	return platform.CategoryOptions(s.BeerCategories())
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

func (s *SettingsStore) upsertPairs(ctx context.Context, pairs map[string]string) error {
	if s.pool == nil {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for k, v := range pairs {
		if _, err := tx.Exec(ctx, `
			insert into store_settings (key, value, updated_at)
			values ($1, $2, now())
			on conflict (key) do update set value = excluded.value, updated_at = now()`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *SettingsStore) normalizeGeneral(rawBrand string, sellablePct, marginPct int) (string, int, int, error) {
	brand := strings.TrimSpace(rawBrand)
	if brand == "" {
		brand = s.defaultBrand
	}
	if len([]rune(brand)) > 48 {
		return "", 0, 0, platform.UserErr("门店名称最多 48 个字符")
	}
	if sellablePct < MinSellablePct || sellablePct > MaxSellablePct {
		return "", 0, 0, platform.UserErrf("默认可售比例需在 %d%% 到 %d%% 之间", MinSellablePct, MaxSellablePct)
	}
	if marginPct < MinMarginPct || marginPct > MaxMarginPct {
		return "", 0, 0, platform.UserErrf("默认目标毛利率需在 %d%% 到 %d%% 之间", MinMarginPct, MaxMarginPct)
	}
	return brand, sellablePct, marginPct, nil
}

func normalizeWebhook(rawURL, rawSecret string, rawEvents []string) (string, string, []string, error) {
	urlStr := strings.TrimSpace(rawURL)
	if err := event.ValidateWebhookURL(urlStr); err != nil {
		return "", "", nil, err
	}
	secret := strings.TrimSpace(rawSecret)
	if len(secret) > 256 {
		return "", "", nil, platform.UserErr("签名密钥过长（最多 256 字符）")
	}
	cleanEvents := make([]string, 0, len(rawEvents))
	seen := make(map[string]bool)
	for _, ev := range rawEvents {
		ev = strings.TrimSpace(ev)
		if ev == "" {
			continue
		}
		if !event.ValidWebhookEvent(ev) {
			return "", "", nil, platform.UserErrf("不支持的 Webhook 事件类型：%s", ev)
		}
		if !seen[ev] {
			seen[ev] = true
			cleanEvents = append(cleanEvents, ev)
		}
	}
	return urlStr, secret, cleanEvents, nil
}

func (s *SettingsStore) Save(ctx context.Context, in StoreSettings) (StoreSettings, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	brand, sellable, margin, err := s.normalizeGeneral(in.Brand, in.DefaultSellablePct, in.DefaultMarginPct)
	if err != nil {
		return StoreSettings{}, err
	}
	in.Brand = brand
	in.DefaultSellablePct = sellable
	in.DefaultMarginPct = margin

	if len(in.BeerCategories) == 0 {
		in.BeerCategories = s.BeerCategories()
	} else {
		cleanCats, err := platform.NormalizeBeerCategories(in.BeerCategories)
		if err != nil {
			return StoreSettings{}, err
		}
		in.BeerCategories = cleanCats
	}

	urlStr, secret, cleanEvents, err := normalizeWebhook(in.WebhookURL, in.WebhookSecret, in.WebhookEvents)
	if err != nil {
		return StoreSettings{}, err
	}
	in.WebhookURL = urlStr
	in.WebhookSecret = secret
	in.WebhookEvents = cleanEvents

	catsJSON, err := json.Marshal(in.BeerCategories)
	if err != nil {
		return StoreSettings{}, err
	}
	if err := s.upsertPairs(ctx, map[string]string{
		"brand":                in.Brand,
		"default_sellable_pct": strconv.Itoa(in.DefaultSellablePct),
		"default_margin_pct":   strconv.Itoa(in.DefaultMarginPct),
		"beer_categories":      string(catsJSON),
		"webhook_url":          in.WebhookURL,
		"webhook_secret":       in.WebhookSecret,
		"webhook_events":       strings.Join(in.WebhookEvents, ","),
	}); err != nil {
		return StoreSettings{}, err
	}

	s.mu.Lock()
	s.cur = StoreSettings{
		Brand:              in.Brand,
		DefaultSellablePct: in.DefaultSellablePct,
		DefaultMarginPct:   in.DefaultMarginPct,
		BeerCategories:     slices.Clone(in.BeerCategories),
		WebhookURL:         in.WebhookURL,
		WebhookSecret:      in.WebhookSecret,
		WebhookEvents:      slices.Clone(in.WebhookEvents),
	}
	s.mu.Unlock()
	return s.Get(), nil
}

// SaveGeneral 仅更新门店名称与桶装默认核算参数，不影响酒款分类和 Webhook 配置。
func (s *SettingsStore) SaveGeneral(ctx context.Context, rawBrand string, sellablePct, marginPct int) (StoreSettings, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	brand, sellable, margin, err := s.normalizeGeneral(rawBrand, sellablePct, marginPct)
	if err != nil {
		return StoreSettings{}, err
	}
	if err := s.upsertPairs(ctx, map[string]string{
		"brand":                brand,
		"default_sellable_pct": strconv.Itoa(sellable),
		"default_margin_pct":   strconv.Itoa(margin),
	}); err != nil {
		return StoreSettings{}, err
	}

	s.mu.Lock()
	s.cur.Brand = brand
	s.cur.DefaultSellablePct = sellable
	s.cur.DefaultMarginPct = margin
	s.mu.Unlock()
	return s.Get(), nil
}

// SaveWebhook 仅更新 Webhook 接收地址、签名密钥与订阅事件，不影响门店基础与核算参数。
func (s *SettingsStore) SaveWebhook(ctx context.Context, rawURL, rawSecret string, clearSecret bool, rawEvents []string) (StoreSettings, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.RLock()
	effectiveSecret := s.cur.WebhookSecret
	s.mu.RUnlock()
	if clearSecret {
		effectiveSecret = ""
	} else if trimmed := strings.TrimSpace(rawSecret); trimmed != "" {
		effectiveSecret = trimmed
	}

	urlStr, secret, cleanEvents, err := normalizeWebhook(rawURL, effectiveSecret, rawEvents)
	if err != nil {
		return StoreSettings{}, err
	}
	if err := s.upsertPairs(ctx, map[string]string{
		"webhook_url":    urlStr,
		"webhook_secret": secret,
		"webhook_events": strings.Join(cleanEvents, ","),
	}); err != nil {
		return StoreSettings{}, err
	}

	s.mu.Lock()
	s.cur.WebhookURL = urlStr
	s.cur.WebhookSecret = secret
	s.cur.WebhookEvents = slices.Clone(cleanEvents)
	s.mu.Unlock()
	return s.Get(), nil
}

func (s *SettingsStore) saveCategoriesLocked(ctx context.Context, next []string) ([]string, error) {
	clean, err := platform.NormalizeBeerCategories(next)
	if err != nil {
		return nil, err
	}
	catsJSON, err := json.Marshal(clean)
	if err != nil {
		return nil, err
	}
	if err := s.upsertPairs(ctx, map[string]string{
		"beer_categories": string(catsJSON),
	}); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cur.BeerCategories = slices.Clone(clean)
	s.mu.Unlock()
	return slices.Clone(clean), nil
}

// AddBeerCategory 向当前酒款分类列表追加一个自定义分类（大小写不敏感查重）并持久化。
func (s *SettingsStore) AddBeerCategory(ctx context.Context, rawName string) ([]string, error) {
	name, err := platform.ValidateBeerCategoryName(rawName)
	if err != nil {
		return nil, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cur := s.BeerCategories()
	for _, c := range cur {
		if strings.EqualFold(c, name) {
			return nil, platform.UserErrf("分类「%s」已存在", c)
		}
	}
	if len(cur) >= platform.MaxBeerCategories {
		return nil, platform.UserErrf("酒款分类最多 %d 个", platform.MaxBeerCategories)
	}
	return s.saveCategoriesLocked(ctx, append(cur, name))
}

// DeleteBeerCategory 从当前预设列表删除指定酒款分类；至少保留 1 个。
func (s *SettingsStore) DeleteBeerCategory(ctx context.Context, rawName string) ([]string, error) {
	target := strings.TrimSpace(rawName)
	if target == "" {
		return nil, platform.UserErr("请指定要删除的分类")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cur := s.BeerCategories()
	next := make([]string, 0, len(cur))
	found := false
	for _, c := range cur {
		if strings.EqualFold(c, target) {
			found = true
			continue
		}
		next = append(next, c)
	}
	if !found {
		return nil, platform.UserErrf("找不到分类「%s」", target)
	}
	if len(next) == 0 {
		return nil, platform.UserErr("至少保留 1 个酒款分类")
	}
	return s.saveCategoriesLocked(ctx, next)
}

// ResetBeerCategories 将门店酒款分类恢复为系统默认分类列表。
func (s *SettingsStore) ResetBeerCategories(ctx context.Context) ([]string, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.saveCategoriesLocked(ctx, platform.DefaultBeerCategories)
}
