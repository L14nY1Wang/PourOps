package lookup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"pourops/internal/platform"
)

const (
	DefaultJiuheBaseURL  = "https://mall.jiuhehe.com/api"
	jiuheHTTPTimeout     = 6 * time.Second
	jiuheMaxBodyBytes    = 1 << 20 // 1 MiB
	jiuheCacheTTL        = 5 * time.Minute
	jiuheCacheMaxEntries = 128
	// jiuheFailThreshold/jiuheFailCooldown：连续多次超时或 5xx 后进入 30s 冷却，
	// 冷却期内直接快速失败，避免持续等待已宕机的上游。
	jiuheFailThreshold = 2
	jiuheFailCooldown  = 30 * time.Second
)

// jiuheHTTPError 保留 HTTP 状态码，供失败冷却区分 5xx 与 4xx。
type jiuheHTTPError int

func (e jiuheHTTPError) Error() string { return fmt.Sprintf("HTTP %d", int(e)) }

// errJiuheCoolingDown 是上游失败冷却期内返回的错误，不发起任何网络请求。
var errJiuheCoolingDown = errors.New("在线 UT 库暂时不可用")

var reKegCap = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(ml|l|升)`)

type jiuheCacheEntry[T any] struct {
	value     T
	expiresAt time.Time
}

type JiuheCache struct {
	mu     sync.Mutex
	search map[string]jiuheCacheEntry[[]Candidate]
	detail map[int64]jiuheCacheEntry[Detail]
}

func NewJiuheCache() *JiuheCache {
	return &JiuheCache{
		search: make(map[string]jiuheCacheEntry[[]Candidate]),
		detail: make(map[int64]jiuheCacheEntry[Detail]),
	}
}

func (c *JiuheCache) getSearch(key string) ([]Candidate, bool) {
	if c == nil {
		return nil, false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	ent, ok := c.search[key]
	if !ok {
		return nil, false
	}
	if !now.Before(ent.expiresAt) {
		delete(c.search, key)
		return nil, false
	}
	cp := make([]Candidate, len(ent.value))
	copy(cp, ent.value)
	return cp, true
}

func (c *JiuheCache) setSearch(key string, items []Candidate) {
	if c == nil {
		return
	}
	now := time.Now()
	cp := make([]Candidate, len(items))
	copy(cp, items)

	c.mu.Lock()
	defer c.mu.Unlock()
	pruneOldest(c.search, now, jiuheCacheMaxEntries)
	c.search[key] = jiuheCacheEntry[[]Candidate]{value: cp, expiresAt: now.Add(jiuheCacheTTL)}
}

func (c *JiuheCache) getDetail(id int64) (Detail, bool) {
	if c == nil {
		return Detail{}, false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	ent, ok := c.detail[id]
	if !ok {
		return Detail{}, false
	}
	if !now.Before(ent.expiresAt) {
		delete(c.detail, id)
		return Detail{}, false
	}
	return ent.value, true
}

func (c *JiuheCache) setDetail(id int64, d Detail) {
	if c == nil {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	pruneOldest(c.detail, now, jiuheCacheMaxEntries)
	c.detail[id] = jiuheCacheEntry[Detail]{value: d, expiresAt: now.Add(jiuheCacheTTL)}
}

func pruneOldest[K comparable, V any](m map[K]jiuheCacheEntry[V], now time.Time, limit int) {
	for k, e := range m {
		if !now.Before(e.expiresAt) {
			delete(m, k)
		}
	}
	for len(m) >= limit {
		var oldestK K
		var oldestT time.Time
		first := true
		for k, e := range m {
			if first || e.expiresAt.Before(oldestT) {
				oldestK = k
				oldestT = e.expiresAt
				first = false
			}
		}
		delete(m, oldestK)
	}
}

// JiuheProvider 对接酒核 UT 同步库接口，支持生啤优先级合并与 5 分钟缓存。
type JiuheProvider struct {
	baseURL      string
	client       *http.Client
	cache        *JiuheCache
	categoriesFn func() []string
	now          func() time.Time // 可注入时钟，测试用；nil 时使用 time.Now

	failMu   sync.Mutex
	failures int
	cooldown time.Time
}

func NewJiuheProvider(baseURL string, client *http.Client) *JiuheProvider {
	if client == nil {
		client = &http.Client{Timeout: jiuheHTTPTimeout}
	}
	return &JiuheProvider{
		baseURL: strings.TrimSpace(baseURL),
		client:  client,
		cache:   NewJiuheCache(),
	}
}

func (p *JiuheProvider) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// coolingDown 报告是否处于失败冷却期；冷却中直接失败，不再排队等待上游。
func (p *JiuheProvider) coolingDown() bool {
	p.failMu.Lock()
	defer p.failMu.Unlock()
	return p.clock().Before(p.cooldown)
}

// recordFetchResult 累计连续瞬时失败；达到阈值后开启冷却，成功则立即恢复。
func (p *JiuheProvider) recordFetchResult(err error) {
	p.failMu.Lock()
	defer p.failMu.Unlock()
	if err == nil {
		p.failures = 0
		p.cooldown = time.Time{}
		return
	}
	if !isJiuheTransient(err) {
		return
	}
	p.failures++
	if p.failures >= jiuheFailThreshold {
		p.failures = 0
		p.cooldown = p.clock().Add(jiuheFailCooldown)
	}
}

// isJiuheTransient 仅把超时/网络故障/5xx/坏响应体视为上游不可用；404 等业务错误不触发冷却。
func isJiuheTransient(err error) bool {
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, context.Canceled) {
		return false
	}
	var status jiuheHTTPError
	if errors.As(err, &status) {
		return int(status) >= 500
	}
	return true
}

func (p *JiuheProvider) SetCategoriesFn(fn func() []string) {
	if p != nil {
		p.categoriesFn = fn
	}
}

func (p *JiuheProvider) categories() []string {
	if p != nil && p.categoriesFn != nil {
		if cats := p.categoriesFn(); len(cats) > 0 {
			return cats
		}
	}
	return platform.DefaultBeerCategories
}

func (p *JiuheProvider) Name() string             { return "jiuhe" }
func (p *JiuheProvider) Label() string            { return "酒核 · UT 同步库" }
func (p *JiuheProvider) SupportsEmptyQuery() bool { return false }

func (p *JiuheProvider) Enabled() bool {
	if p == nil {
		return false
	}
	base := strings.TrimSpace(p.baseURL)
	return base != "" && !strings.EqualFold(base, "off") && !strings.EqualFold(base, "none")
}

func (p *JiuheProvider) base() string {
	base := strings.TrimSpace(p.baseURL)
	if base == "" {
		base = DefaultJiuheBaseURL
	}
	return strings.TrimRight(base, "/")
}

func (p *JiuheProvider) httpClient() *http.Client {
	if p.client != nil {
		return p.client
	}
	return &http.Client{Timeout: jiuheHTTPTimeout}
}

func (p *JiuheProvider) fetchJSON(ctx context.Context, targetURL string, dst any) error {
	if p.coolingDown() {
		return errJiuheCoolingDown
	}
	err := p.doFetchJSON(ctx, targetURL, dst)
	p.recordFetchResult(err)
	return err
}

func (p *JiuheProvider) doFetchJSON(ctx context.Context, targetURL string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; PourOps/1.0)")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 读空剩余响应体，让底层连接可以复用（keep-alive），而不是被直接丢弃。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusNotFound {
			return ErrNotFound
		}
		return jiuheHTTPError(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, jiuheMaxBodyBytes+1))
	if err != nil {
		return err
	}
	if len(body) > jiuheMaxBodyBytes {
		return errors.New("上游响应体积超出上限")
	}
	return json.Unmarshal(body, dst)
}

type jiuheSearchResp struct {
	Items []jiuheProductItem `json:"items"`
	Total int                `json:"total"`
}

type jiuheProductItem struct {
	Product struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		CoverURL string `json:"coverUrl"`
		Category struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"category"`
	} `json:"product"`
	Price struct {
		Min     *float64 `json:"min"`
		Display *float64 `json:"display"`
	} `json:"price"`
	Stock struct {
		AvailableQuantity int `json:"availableQuantity"`
	} `json:"stock"`
	Wine struct {
		ID            int64    `json:"id"`
		NameZh        string   `json:"nameZh"`
		NameEn        string   `json:"nameEn"`
		Style         string   `json:"style"`
		ABV           *float64 `json:"abv"`
		UntappdRating *float64 `json:"untappdRating"`
		Brand         struct {
			ID         int64  `json:"id"`
			NameZh     string `json:"nameZh"`
			NameEn     string `json:"nameEn"`
			OriginCode string `json:"originCode"`
		} `json:"brand"`
	} `json:"wine"`
}

type jiuheSKU struct {
	CategoryName      string   `json:"categoryName"`
	CapacityLabel     string   `json:"capacityLabel"`
	Price             *float64 `json:"price"`
	AvailableQuantity int      `json:"availableQuantity"`
}

type jiuheDetailResp struct {
	Product struct {
		Product struct {
			ID       int64  `json:"id"`
			Name     string `json:"name"`
			CoverURL string `json:"coverUrl"`
			Category struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"category"`
		} `json:"product"`
		Price struct {
			Min     *float64 `json:"min"`
			Display *float64 `json:"display"`
		} `json:"price"`
		Wine struct {
			NameZh string `json:"nameZh"`
			NameEn string `json:"nameEn"`
			Style  string `json:"style"`
			Brand  struct {
				NameZh string `json:"nameZh"`
				NameEn string `json:"nameEn"`
			} `json:"brand"`
		} `json:"wine"`
		SKUs   []jiuheSKU `json:"skus"`
		Images []string   `json:"images"`
	} `json:"product"`
	WineDetail *struct {
		ID                 int64    `json:"id"`
		NameZh             string   `json:"nameZh"`
		NameEn             string   `json:"nameEn"`
		BrandNameZh        string   `json:"brandNameZh"`
		BrandNameEn        string   `json:"brandNameEn"`
		ABV                *float64 `json:"abv"`
		ImageURL           string   `json:"imageUrl"`
		IBU                *int     `json:"ibu"`
		Style              string   `json:"style"`
		UntappdID          string   `json:"untappdId"`
		UntappdRatingCount *int     `json:"untappdRatingCount"`
		UntappdRating      *float64 `json:"untappdRating"`
		UntappdStyle       string   `json:"untappdStyle"`
		DescriptionZh      *string  `json:"descriptionZh"`
		DescriptionEn      *string  `json:"descriptionEn"`
	} `json:"wineDetail"`
}

func isJiuheDraftItem(it jiuheProductItem) bool {
	return it.Product.Category.ID == 2 || strings.TrimSpace(it.Product.Category.Name) == "生啤"
}

func (p *JiuheProvider) Search(ctx context.Context, q string, limit int) ([]Candidate, error) {
	if !p.Enabled() || strings.TrimSpace(q) == "" {
		return nil, nil
	}
	maxOut := 10
	if limit > 0 && limit < maxOut {
		maxOut = limit
	}
	// 缓存整份未截断结果，按 limit 切片返回：不同 limit 复用同一次上游请求。
	cacheKey := strings.ToLower(strings.TrimSpace(q))
	if cached, ok := p.cache.getSearch(cacheKey); ok {
		return slices.Clone(cached[:min(len(cached), maxOut)]), nil
	}

	u := p.base() + "/mall/products?keyword=" + url.QueryEscape(q) + "&size=15&page=1"
	var resp jiuheSearchResp
	if err := p.fetchJSON(ctx, u, &resp); err != nil {
		return nil, err
	}

	type pick struct {
		item    jiuheProductItem
		isDraft bool
		inStock bool
	}
	byKey := make(map[string]pick, len(resp.Items))
	order := make([]string, 0, len(resp.Items))
	for _, it := range resp.Items {
		if it.Product.ID <= 0 {
			continue
		}
		key := fmt.Sprintf("w:%d", it.Wine.ID)
		if it.Wine.ID <= 0 {
			key = fmt.Sprintf("p:%d", it.Product.ID)
		}
		isDraft := isJiuheDraftItem(it)
		inStock := it.Stock.AvailableQuantity > 0
		if cur, exists := byKey[key]; exists {
			betterDraft := isDraft && !cur.isDraft
			sameDraftBetterStock := (isDraft == cur.isDraft) && inStock && !cur.inStock
			if !betterDraft && !sameDraftBetterStock {
				continue
			}
		} else {
			order = append(order, key)
		}
		byKey[key] = pick{item: it, isDraft: isDraft, inStock: inStock}
	}

	var drafts, nonDrafts []Candidate
	for _, key := range order {
		pk := byKey[key]
		it := pk.item
		brewery := strings.TrimSpace(it.Wine.Brand.NameZh)
		if brewery == "" {
			brewery = strings.TrimSpace(it.Wine.Brand.NameEn)
		}
		rawName := strings.TrimSpace(it.Wine.NameZh)
		if rawName == "" {
			rawName = strings.TrimSpace(it.Product.Name)
		}
		if rawName == "" {
			rawName = strings.TrimSpace(it.Wine.NameEn)
		}
		name := StripBreweryPrefix(brewery, rawName)
		if name == "" {
			continue
		}
		nameEN := strings.TrimSpace(it.Wine.NameEn)
		if nameEN != "" {
			nameEN = StripBreweryPrefix(it.Wine.Brand.NameEn, nameEN)
			nameEN = StripBreweryPrefix(brewery, nameEN)
			if strings.EqualFold(nameEN, name) {
				nameEN = ""
			}
		}

		rating := ""
		if it.Wine.UntappdRating != nil && *it.Wine.UntappdRating > 0 {
			rating = fmt.Sprintf("%.2f", *it.Wine.UntappdRating)
		}
		poster := strings.TrimSpace(it.Product.CoverURL)
		if !platform.ValidRemotePosterURL(poster) {
			poster = ""
		}
		c := Candidate{
			Source:    "jiuhe",
			ID:        strconv.FormatInt(it.Product.ID, 10),
			Brewery:   brewery,
			Name:      name,
			NameEN:    nameEN,
			Style:     strings.TrimSpace(it.Wine.Style),
			Rating:    rating,
			PosterURL: poster,
			IsDraft:   pk.isDraft,
		}
		if pk.isDraft {
			drafts = append(drafts, c)
		} else {
			nonDrafts = append(nonDrafts, c)
		}
	}

	merged := make([]Candidate, 0, len(drafts)+len(nonDrafts))
	merged = append(merged, drafts...)
	merged = append(merged, nonDrafts...)
	p.cache.setSearch(cacheKey, merged)
	return slices.Clone(merged[:min(len(merged), maxOut)]), nil
}

func (p *JiuheProvider) Detail(ctx context.Context, rawID string) (Detail, error) {
	if !p.Enabled() {
		return Detail{}, platform.UserErr("在线 UT 库未启用")
	}
	productID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || productID <= 0 {
		return Detail{}, ErrInvalidID
	}
	if cached, ok := p.cache.getDetail(productID); ok {
		return cached, nil
	}

	u := p.base() + "/mall/products/" + strconv.FormatInt(productID, 10) + "/detail"
	var resp jiuheDetailResp
	if err := p.fetchJSON(ctx, u, &resp); err != nil {
		return Detail{}, err
	}

	if resp.Product.Product.ID == 0 && resp.WineDetail == nil {
		return Detail{}, ErrNotFound
	}

	var brewery, rawName, zhStyle, utStyle, abv, ibu, utRating, utCount, poster, descZh string
	if wd := resp.WineDetail; wd != nil {
		brewery = strings.TrimSpace(wd.BrandNameZh)
		if brewery == "" {
			brewery = strings.TrimSpace(wd.BrandNameEn)
		}
		rawName = strings.TrimSpace(wd.NameZh)
		if rawName == "" {
			rawName = strings.TrimSpace(wd.NameEn)
		}
		zhStyle = strings.TrimSpace(wd.Style)
		utStyle = strings.TrimSpace(wd.UntappdStyle)
		if wd.ABV != nil && *wd.ABV > 0 {
			abv = platform.NumText(wd.ABV, 1)
		}
		if wd.IBU != nil && *wd.IBU > 0 {
			ibu = strconv.Itoa(*wd.IBU)
		}
		if wd.UntappdRating != nil && *wd.UntappdRating > 0 {
			utRating = fmt.Sprintf("%.2f", *wd.UntappdRating)
		}
		if wd.UntappdRatingCount != nil && *wd.UntappdRatingCount > 0 {
			utCount = strconv.Itoa(*wd.UntappdRatingCount)
		}
		if wd.DescriptionZh != nil {
			descZh = strings.TrimSpace(*wd.DescriptionZh)
		}
		poster = strings.TrimSpace(wd.ImageURL)
	}

	if brewery == "" {
		brewery = strings.TrimSpace(resp.Product.Wine.Brand.NameZh)
		if brewery == "" {
			brewery = strings.TrimSpace(resp.Product.Wine.Brand.NameEn)
		}
	}
	if rawName == "" {
		rawName = strings.TrimSpace(resp.Product.Wine.NameZh)
		if rawName == "" {
			rawName = strings.TrimSpace(resp.Product.Product.Name)
		}
		if rawName == "" {
			rawName = strings.TrimSpace(resp.Product.Wine.NameEn)
		}
	}
	if zhStyle == "" {
		zhStyle = strings.TrimSpace(resp.Product.Wine.Style)
	}
	if poster == "" {
		poster = strings.TrimSpace(resp.Product.Product.CoverURL)
	}
	if poster == "" && len(resp.Product.Images) > 0 {
		poster = strings.TrimSpace(resp.Product.Images[0])
	}
	if !platform.ValidRemotePosterURL(poster) {
		poster = ""
	}

	name := StripBreweryPrefix(brewery, rawName)
	if name == "" {
		return Detail{}, ErrNotFound
	}

	style := utStyle
	if style == "" {
		style = zhStyle
	}
	cats := p.categories()
	cat := ""
	if utStyle != "" {
		cat = platform.MatchCategory(utStyle, cats)
	}
	if cat == "" {
		cat = platform.MatchCategory(zhStyle, cats)
	}
	if cat == "" {
		cat = platform.InferCategory("", cats)
	}

	capML, cost := extractDraftKegSpec(resp)
	tags := ExtractFlavorTags(zhStyle, descZh)

	detail := Detail{
		Source:      "jiuhe",
		SourceLabel: p.Label(),
		Brewery:     brewery,
		Name:        name,
		Style:       style,
		Category:    cat,
		ABV:         abv,
		IBU:         ibu,
		UTRating:    utRating,
		UTCount:     utCount,
		Tags:        tags,
		PosterURL:   poster,
		CapacityML:  capML,
		Cost:        cost,
	}
	p.cache.setDetail(productID, detail)
	return detail, nil
}

// StripBreweryPrefix 剥离「FEVER酿造 星空领航员:Citra」「树屋 · 尤利乌斯」前重复的厂牌前缀。
func StripBreweryPrefix(brewery, rawName string) string {
	b := strings.TrimSpace(brewery)
	n := strings.TrimSpace(rawName)
	if b == "" || n == "" {
		return n
	}
	rb := []rune(b)
	rn := []rune(n)
	if len(rn) <= len(rb) || !strings.EqualFold(string(rn[:len(rb)]), b) {
		return n
	}
	const seps = " \t·-—/・"
	if !strings.ContainsRune(seps, rn[len(rb)]) {
		return n
	}
	trimmed := strings.TrimSpace(strings.TrimLeft(string(rn[len(rb):]), seps))
	if trimmed == "" {
		return n
	}
	return trimmed
}

func ParseKegCapacityML(label string) int {
	m := reKegCap.FindStringSubmatch(strings.TrimSpace(label))
	if len(m) < 3 {
		return 0
	}
	val, err := strconv.ParseFloat(m[1], 64)
	if err != nil || math.IsNaN(val) || math.IsInf(val, 0) || val <= 0 {
		return 0
	}
	unit := strings.ToLower(m[2])
	if unit == "ml" {
		return int(math.Round(val))
	}
	return int(math.Round(val * 1000))
}

func formatYuanFloat(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return ""
	}
	rounded := math.Round(v*100) / 100
	return strconv.FormatFloat(rounded, 'f', -1, 64)
}

func extractDraftKegSpec(resp jiuheDetailResp) (capML, cost string) {
	productIsDraft := resp.Product.Product.Category.ID == 2 ||
		strings.TrimSpace(resp.Product.Product.Category.Name) == "生啤"

	pickSKU := func(requireStock bool) (int, string, bool) {
		for _, sku := range resp.Product.SKUs {
			if requireStock && sku.AvailableQuantity <= 0 {
				continue
			}
			skuDraft := strings.TrimSpace(sku.CategoryName) == "生啤" ||
				(strings.TrimSpace(sku.CategoryName) == "" && productIsDraft)
			if !skuDraft {
				continue
			}
			ml := ParseKegCapacityML(sku.CapacityLabel)
			if ml < 5000 {
				continue
			}
			price := ""
			if sku.Price != nil && *sku.Price > 0 {
				price = formatYuanFloat(*sku.Price)
			} else if resp.Product.Price.Display != nil && *resp.Product.Price.Display > 0 {
				price = formatYuanFloat(*resp.Product.Price.Display)
			} else if resp.Product.Price.Min != nil && *resp.Product.Price.Min > 0 {
				price = formatYuanFloat(*resp.Product.Price.Min)
			}
			return ml, price, true
		}
		return 0, "", false
	}

	if ml, price, ok := pickSKU(true); ok {
		return strconv.Itoa(ml), price
	}
	if ml, price, ok := pickSKU(false); ok {
		return strconv.Itoa(ml), price
	}
	return "", ""
}

var flavorKeywords = []string{
	"浓郁柑橘", "热带水果", "白色水果", "核果",
	"浓缩咖啡", "白葡萄酒", "威士忌桶", "橡木桶", "红酒桶", "波本桶",
	"西柚", "葡萄柚", "柑橘", "百香果", "芒果", "菠萝", "凤梨", "荔枝",
	"水蜜桃", "油桃", "桃子", "杏子", "番石榴", "芭乐", "木瓜", "哈密瓜",
	"白葡萄", "葡萄", "青柠", "柠檬", "血橙", "甜橙", "香橙", "橙皮", "柚子",
	"青苹果", "红苹果", "草莓", "覆盆子", "树莓", "蓝莓", "黑莓", "樱桃", "黑加仑", "酸梅",
	"花香", "茉莉", "接骨木花", "松针", "松脂", "青草", "树脂",
	"黑巧", "巧克力", "可可", "咖啡", "烘焙", "烤麦芽", "烤面包", "饼干", "麦香",
	"太妃糖", "焦糖", "蜂蜜", "枫糖", "香草", "椰子", "肉桂", "豆蔻", "胡椒", "芫荽籽", "香料",
	"香蕉", "丁香", "燕麦", "奶油", "香滑", "乳糖", "奶昔", "棉花糖", "山核桃", "榛子",
	"波本", "威士忌", "过桶", "烟熏", "泥煤",
	"酸爽", "野菌", "皮革", "海盐", "矿物感",
	"果汁感", "浑浊感", "苦爽", "回甘", "干爽", "清爽", "顺滑", "醇厚", "绵密",
}

func ExtractFlavorTags(zhStyle, desc string) string {
	type hit struct {
		pos  int
		word string
	}
	var hits []hit
	for _, kw := range flavorKeywords {
		if idx := strings.Index(desc, kw); idx >= 0 {
			hits = append(hits, hit{pos: idx, word: kw})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].pos != hits[j].pos {
			return hits[i].pos < hits[j].pos
		}
		return len(hits[i].word) > len(hits[j].word)
	})

	picked := make([]string, 0, 4)
	isDuplicateOrSub := func(cand string) bool {
		for _, p := range picked {
			if p == cand || strings.Contains(p, cand) || strings.Contains(cand, p) {
				return true
			}
		}
		return false
	}
	for _, h := range hits {
		if isDuplicateOrSub(h.word) {
			continue
		}
		picked = append(picked, h.word)
		if len(picked) >= 4 {
			return strings.Join(picked, "、")
		}
	}

	style := strings.TrimSpace(zhStyle)
	if len(picked) < 2 && style != "" && len([]rune(style)) <= 10 && !isDuplicateOrSub(style) {
		picked = append([]string{style}, picked...)
	}
	if len(picked) > 4 {
		picked = picked[:4]
	}
	return strings.Join(picked, "、")
}
