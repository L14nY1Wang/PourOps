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
	DefaultSellablePct     = 80
	DefaultMarginPct       = 50
	MinSellablePct         = 50
	MaxSellablePct         = 100
	MinMarginPct           = 10
	MaxMarginPct           = 85
	DefaultBarCount        = 3
	DefaultTableCount      = 4
	MinFixedSpots          = 0
	MaxFixedSpots          = 50
	DefaultPointsPerYuan   = 1
	DefaultPointsRedeem    = 100
	DefaultPointsMaxPct    = 50
	MaxPointsPerYuan       = 1000
	MaxPointsRedeemRate    = 100000
	MaxRechargePresetCount = 12
)

// RechargePreset 定义会员快捷充值档位（实充金额与赠送金额，均以分为单位）。
type RechargePreset struct {
	PayCents   int `json:"pay_cents"`
	BonusCents int `json:"bonus_cents"`
}

func (p RechargePreset) PayText() string    { return platform.PriceText(p.PayCents) }
func (p RechargePreset) BonusText() string  { return platform.PriceText(p.BonusCents) }
func (p RechargePreset) PayInput() string   { return platform.MoneyInput(p.PayCents) }
func (p RechargePreset) BonusInput() string { return platform.MoneyInput(p.BonusCents) }

// LoyaltyConfig 控制会员积分获取、积分抵现比例与快捷充值档位。
type LoyaltyConfig struct {
	PointsPerYuan    int
	PointsRedeemRate int // 多少积分抵 1 元（100 分）；0 表示关闭积分抵现
	PointsMaxPct     int // 单笔最多可用积分抵扣百分比（0-100）
	RechargePresets  []RechargePreset
}

func defaultRechargePresetsCopy() []RechargePreset {
	return []RechargePreset{
		{PayCents: 30000, BonusCents: 2000},
		{PayCents: 50000, BonusCents: 5000},
		{PayCents: 100000, BonusCents: 15000},
	}
}

func DefaultLoyaltyConfig() LoyaltyConfig {
	return LoyaltyConfig{
		PointsPerYuan:    DefaultPointsPerYuan,
		PointsRedeemRate: DefaultPointsRedeem,
		PointsMaxPct:     DefaultPointsMaxPct,
		RechargePresets:  defaultRechargePresetsCopy(),
	}
}

// Normalized 返回合法范围内且去重排序后的会员积分与充值参数。
func (c LoyaltyConfig) Normalized() LoyaltyConfig {
	c.PointsPerYuan = platform.ClampRange(c.PointsPerYuan, 0, MaxPointsPerYuan)
	c.PointsRedeemRate = platform.ClampRange(c.PointsRedeemRate, 0, MaxPointsRedeemRate)
	c.PointsMaxPct = platform.ClampRange(c.PointsMaxPct, 0, 100)
	clean := make([]RechargePreset, 0, len(c.RechargePresets))
	for _, p := range c.RechargePresets {
		if p.PayCents <= 0 || p.BonusCents < 0 {
			continue
		}
		clean = append(clean, p)
		if len(clean) >= MaxRechargePresetCount {
			break
		}
	}
	if len(clean) == 0 {
		clean = defaultRechargePresetsCopy()
	}
	c.RechargePresets = clean
	return c
}

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

// StoreSettings 保存运营参数、点餐台固定台位数量、酒款分类、会员积分配置与 Webhook 出站通知设置。
type StoreSettings struct {
	Brand              string
	DefaultSellablePct int
	DefaultMarginPct   int
	BarCount           int
	TableCount         int
	BeerCategories     []string
	WebhookURL         string
	WebhookSecret      string
	WebhookEvents      []string
	Loyalty            LoyaltyConfig
}

func (s StoreSettings) Pricing() PricingConfig {
	return PricingConfig{
		SellablePct: s.DefaultSellablePct,
		MarginPct:   s.DefaultMarginPct,
	}.Normalized()
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
			BarCount:           DefaultBarCount,
			TableCount:         DefaultTableCount,
			BeerCategories:     defaultCategoriesCopy(),
			WebhookEvents:      []string{},
			Loyalty:            DefaultLoyaltyConfig(),
		},
	}
}

func (s *SettingsStore) Refresh(ctx context.Context) error {
	base := StoreSettings{
		Brand:              s.defaultBrand,
		DefaultSellablePct: DefaultSellablePct,
		DefaultMarginPct:   DefaultMarginPct,
		BarCount:           DefaultBarCount,
		TableCount:         DefaultTableCount,
		BeerCategories:     defaultCategoriesCopy(),
		WebhookEvents:      []string{},
		Loyalty:            DefaultLoyaltyConfig(),
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
		case "bar_count":
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= MinFixedSpots && n <= MaxFixedSpots {
				base.BarCount = n
			}
		case "table_count":
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= MinFixedSpots && n <= MaxFixedSpots {
				base.TableCount = n
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
		case "loyalty_points_per_yuan":
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 && n <= MaxPointsPerYuan {
				base.Loyalty.PointsPerYuan = n
			}
		case "loyalty_points_redeem_rate":
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 && n <= MaxPointsRedeemRate {
				base.Loyalty.PointsRedeemRate = n
			}
		case "loyalty_points_max_pct":
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 && n <= 100 {
				base.Loyalty.PointsMaxPct = n
			}
		case "loyalty_recharge_presets":
			var presets []RechargePreset
			if err := json.Unmarshal([]byte(v), &presets); err == nil {
				base.Loyalty.RechargePresets = presets
			}
		}
	}
	base.Loyalty = base.Loyalty.Normalized()
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
	cp.Loyalty = s.cur.Loyalty.Normalized()
	cp.Loyalty.RechargePresets = slices.Clone(cp.Loyalty.RechargePresets)
	return cp
}

func (s *SettingsStore) Loyalty() LoyaltyConfig {
	return s.Get().Loyalty
}

func (s *SettingsStore) SaveLoyalty(ctx context.Context, cfg LoyaltyConfig) (LoyaltyConfig, error) {
	if cfg.PointsPerYuan < 0 || cfg.PointsPerYuan > MaxPointsPerYuan {
		return LoyaltyConfig{}, platform.UserErrf("每元攒积分需在 0 到 %d 之间", MaxPointsPerYuan)
	}
	if cfg.PointsRedeemRate < 0 || cfg.PointsRedeemRate > MaxPointsRedeemRate {
		return LoyaltyConfig{}, platform.UserErrf("抵现积分汇率需在 0 到 %d 之间", MaxPointsRedeemRate)
	}
	if cfg.PointsMaxPct < 0 || cfg.PointsMaxPct > 100 {
		return LoyaltyConfig{}, platform.UserErr("单笔积分最高抵扣比例需在 0% 到 100% 之间")
	}
	for _, p := range cfg.RechargePresets {
		if p.PayCents <= 0 || p.BonusCents < 0 {
			return LoyaltyConfig{}, platform.UserErr("充值档位的实充金额必须大于 0，赠金不能为负")
		}
	}
	norm := cfg.Normalized()
	rawPresets, err := json.Marshal(norm.RechargePresets)
	if err != nil {
		return LoyaltyConfig{}, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if err := s.upsertPairs(ctx, map[string]string{
		"loyalty_points_per_yuan":    strconv.Itoa(norm.PointsPerYuan),
		"loyalty_points_redeem_rate": strconv.Itoa(norm.PointsRedeemRate),
		"loyalty_points_max_pct":     strconv.Itoa(norm.PointsMaxPct),
		"loyalty_recharge_presets":   string(rawPresets),
	}); err != nil {
		return LoyaltyConfig{}, err
	}

	s.mu.Lock()
	s.cur.Loyalty = norm
	s.cur.Loyalty.RechargePresets = slices.Clone(norm.RechargePresets)
	s.mu.Unlock()
	return s.Loyalty(), nil
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

// SaveSpots 仅更新点餐台固定吧台与小桌数量；调小前检查超编台位是否存在未结商品或付款。
func (s *SettingsStore) SaveSpots(ctx context.Context, barCount, tableCount int) (StoreSettings, error) {
	if barCount < MinFixedSpots || barCount > MaxFixedSpots {
		return StoreSettings{}, platform.UserErrf("吧台数量需在 %d 到 %d 之间", MinFixedSpots, MaxFixedSpots)
	}
	if tableCount < MinFixedSpots || tableCount > MaxFixedSpots {
		return StoreSettings{}, platform.UserErrf("小桌数量需在 %d 到 %d 之间", MinFixedSpots, MaxFixedSpots)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if s.pool != nil {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return StoreSettings{}, err
		}
		defer tx.Rollback(ctx)

		// 确保配置行存在并按固定顺序锁表：store_settings -> orders，与 POS EnsureOrder 同序防死锁。
		if _, err := tx.Exec(ctx, `
			insert into store_settings (key, value, updated_at)
			values ('bar_count', '3', now()), ('table_count', '4', now())
			on conflict (key) do nothing`); err != nil {
			return StoreSettings{}, err
		}
		krows, err := tx.Query(ctx, `select key from store_settings where key in ('bar_count', 'table_count') order by key for update`)
		if err != nil {
			return StoreSettings{}, err
		}
		for krows.Next() {
		}
		krows.Close()
		if err := krows.Err(); err != nil {
			return StoreSettings{}, err
		}

		type openSpotOrder struct {
			id   int64
			spot string
		}
		orows, err := tx.Query(ctx, `select id, spot from orders where closed_at is null and (spot like 'bar-%' or spot like 'table-%') order by id for update`)
		if err != nil {
			return StoreSettings{}, err
		}
		var opens []openSpotOrder
		for orows.Next() {
			var o openSpotOrder
			if err := orows.Scan(&o.id, &o.spot); err != nil {
				orows.Close()
				return StoreSettings{}, err
			}
			opens = append(opens, o)
		}
		orows.Close()
		if err := orows.Err(); err != nil {
			return StoreSettings{}, err
		}

		for _, o := range opens {
			kind, num, ok := strings.Cut(o.spot, "-")
			n, convErr := strconv.Atoi(num)
			if !ok || convErr != nil || n < 1 || o.spot != kind+"-"+strconv.Itoa(n) {
				continue
			}
			if (kind == "bar" && n > barCount) || (kind == "table" && n > tableCount) {
				var busy bool
				if err := tx.QueryRow(ctx, `select exists(select 1 from order_items where order_id=$1)
					or exists(select 1 from payments where order_id=$1)`, o.id).Scan(&busy); err != nil {
					return StoreSettings{}, err
				}
				if busy {
					label := "小桌"
					if kind == "bar" {
						label = "吧台"
					}
					return StoreSettings{}, platform.UserErrf("%s %d 还有未结账单，请先结清再调小数量", label, n)
				}
				if _, err := tx.Exec(ctx, `delete from orders where id=$1`, o.id); err != nil {
					return StoreSettings{}, err
				}
			}
		}

		for _, kv := range [2][2]string{
			{"bar_count", strconv.Itoa(barCount)},
			{"table_count", strconv.Itoa(tableCount)},
		} {
			if _, err := tx.Exec(ctx, `
				insert into store_settings (key, value, updated_at)
				values ($1, $2, now())
				on conflict (key) do update set value = excluded.value, updated_at = now()`, kv[0], kv[1]); err != nil {
				return StoreSettings{}, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return StoreSettings{}, err
		}
	}

	s.mu.Lock()
	s.cur.BarCount = barCount
	s.cur.TableCount = tableCount
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
