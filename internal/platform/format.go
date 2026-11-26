package platform

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ReportZone 固定按北京时间 UTC+8 划分日报与导出时间。
var ReportZone = time.FixedZone("UTC+8", 8*60*60)

// 酒吧营业日从北京时间 07:00 开始，至次日 07:00 结束（如 01-11 实际统计 01-11 07:00 至 01-12 07:00）。
const (
	BusinessDayStartHour = 7
	BusinessDayCutoff    = BusinessDayStartHour * time.Hour
)

// BusinessDayStart 返回 t 所在营业日的开始时刻（北京时间 07:00）。
func BusinessDayStart(now time.Time) time.Time {
	biz := now.In(ReportZone).Add(-BusinessDayCutoff)
	return time.Date(biz.Year(), biz.Month(), biz.Day(), BusinessDayStartHour, 0, 0, 0, ReportZone)
}

// BusinessDayStartOfDate 返回指定日历日对应的营业日开始时刻（北京时间当日 07:00）。
func BusinessDayStartOfDate(day time.Time) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), BusinessDayStartHour, 0, 0, 0, ReportZone)
}

// BusinessDayKey 返回 t 所属营业日日期（YYYY-MM-DD，北京时间减 7 小时后取日期）。
func BusinessDayKey(t time.Time) string {
	return t.In(ReportZone).Add(-BusinessDayCutoff).Format(time.DateOnly)
}

// Option 是后台下拉/筛选控件共用的键值选项。
type Option struct {
	Value string
	Label string
}

const (
	MaxBeerCategories    = 50
	MaxBeerCategoryRunes = 20
	MaxBeerIBU           = 1000
)

// DefaultBeerCategories 定义酒款默认预设风格分类；可在系统设置中增删维护。
var DefaultBeerCategories = []string{
	"IPA", "拉格", "皮尔森", "小麦", "酸小麦", "酸艾尔", "果泥", "西打", "世涛",
}

// Categories 保持默认酒款分类选项列表（未传入动态分类配置时兜底使用）。
var Categories = CategoryOptions(DefaultBeerCategories)

// CategoryOptions 把分类名称列表转成表单与筛选共用的 Option 切片。
func CategoryOptions(names []string) []Option {
	if len(names) == 0 {
		names = DefaultBeerCategories
	}
	out := make([]Option, 0, len(names))
	for _, n := range names {
		trimmed := strings.TrimSpace(n)
		if trimmed == "" {
			continue
		}
		out = append(out, Option{Value: trimmed, Label: trimmed})
	}
	if len(out) == 0 {
		return CategoryOptions(DefaultBeerCategories)
	}
	return out
}

// ValidateBeerCategoryName 校验单个酒款分类名称并去除首尾空白。
func ValidateBeerCategoryName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", UserErr("分类名称不能为空")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", UserErr("分类名称不能包含控制字符")
		}
	}
	if utf8.RuneCountInString(name) > MaxBeerCategoryRunes {
		return "", UserErrf("分类名称最多 %d 个字", MaxBeerCategoryRunes)
	}
	return name, nil
}

// NormalizeBeerCategories 对分类列表做严格校验并按大小写不敏感去重。
func NormalizeBeerCategories(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		if strings.TrimSpace(item) == "" {
			continue
		}
		name, err := ValidateBeerCategoryName(item)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, UserErr("至少保留 1 个酒款分类")
	}
	if len(out) > MaxBeerCategories {
		return nil, UserErrf("酒款分类最多 %d 个", MaxBeerCategories)
	}
	return out, nil
}

var legacyCategoryAliases = map[string]string{
	"hop":    "IPA",
	"酒花":     "IPA",
	"fresh":  "拉格",
	"清爽":     "拉格",
	"sour":   "酸艾尔",
	"酸野菌":    "酸艾尔",
	"dark":   "世涛",
	"深色":     "世涛",
	"strong": "世涛",
	"烈性":     "世涛",
}

// CategoryLabel 返回适合展示的酒款分类名称；历史旧编码会自动映射为新版中文分类名。
func CategoryLabel(v string) string {
	s := strings.TrimSpace(v)
	if s == "" {
		return ""
	}
	if mapped, ok := legacyCategoryAliases[strings.ToLower(s)]; ok {
		return mapped
	}
	return s
}

// ParseCategoryWithAllowed 按门店动态分类列表校验分类输入（支持历史旧分类别名自动转换）。
func ParseCategoryWithAllowed(raw string, allowed []string, extraAllowed ...string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if len(allowed) == 0 {
		allowed = DefaultBeerCategories
	}
	candidates := make([]string, 0, len(allowed)+len(extraAllowed))
	for _, c := range allowed {
		if c = strings.TrimSpace(c); c != "" {
			candidates = append(candidates, c)
		}
	}
	for _, c := range extraAllowed {
		if c = strings.TrimSpace(c); c != "" {
			candidates = append(candidates, c)
		}
	}
	for _, cand := range candidates {
		if strings.EqualFold(v, cand) {
			return cand, nil
		}
	}
	if alias, ok := legacyCategoryAliases[strings.ToLower(v)]; ok {
		for _, cand := range candidates {
			if strings.EqualFold(alias, cand) {
				return cand, nil
			}
		}
	}
	return "", UserErrf("不支持的酒款分类 %q（可在系统设置中添加分类）", v)
}

// styleCategoryRules 将中英文风格描述映射到精酿风格分类，按从具体到通用顺序匹配。
var styleCategoryRules = []struct {
	Category string
	Re       *regexp.Regexp
}{
	{"西打", regexp.MustCompile(`cider|perry|pomme|苹果酒|西打`)},
	{"果泥", regexp.MustCompile(`smoothie|puree|slush|fruited|pastry sour|果泥|奶昔`)},
	{"酸小麦", regexp.MustCompile(`berliner|gose|酸小麦|柏林|古斯`)},
	{"酸艾尔", regexp.MustCompile(`lambic|gueuze|geuze|sour|wild|flanders|oude|brett|酸艾尔|兰比克|贵兹|野菌|法兰德斯|酸啤|酸`)},
	{"IPA", regexp.MustCompile(`\bipa\b|neipa|dipa|tipa|india pale|pale ale|\bapa\b|hazy|印度淡|浑浊|酒花`)},
	{"波特", regexp.MustCompile(`porter|baltic|波特`)},
	{"世涛", regexp.MustCompile(`stout|porter|baltic|irish dry|imperial black|世涛|黑啤|波特`)},
	{"皮尔森", regexp.MustCompile(`pilsner|pilsener|\bpils\b|bohemian|皮尔森|比尔森`)},
	{"小麦", regexp.MustCompile(`wheat|witbier|hefeweizen|weizen|weisse|weissbier|blanche|white ale|小麦|白啤`)},
	{"修道院", regexp.MustCompile(`trappist|abbey|tripel|dubbel|quadrupel|belgian strong|\bquad\b|修道院|三料|双料|四料|烈性`)},
	{"拉格", regexp.MustCompile(`lager|helles|dunkel|marzen|märzen|bock|kolsch|kölsch|vienna|schwarz|拉格|海勒|博克|科隆|清爽`)},
}

// MatchCategory 根据酒款风格与当前可用分类列表尝试匹配分类名；不猜中时返回空串。
func MatchCategory(style string, available []string) string {
	s := strings.ToLower(strings.TrimSpace(style))
	if s == "" {
		return ""
	}
	if len(available) == 0 {
		available = DefaultBeerCategories
	}
	clean := make([]string, 0, len(available))
	normMap := make(map[string]string, len(available))
	for _, a := range available {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		key := strings.ToLower(a)
		if _, ok := normMap[key]; !ok {
			normMap[key] = a
			clean = append(clean, a)
		}
	}
	// 先按字符长度降序尝试直接子串匹配（如老板新增的「赛松」「科隆」等分类）
	byLen := slices.Clone(clean)
	slices.SortStableFunc(byLen, func(a, b string) int {
		return utf8.RuneCountInString(b) - utf8.RuneCountInString(a)
	})
	for _, name := range byLen {
		if strings.Contains(s, strings.ToLower(name)) {
			return name
		}
	}
	for _, r := range styleCategoryRules {
		if r.Re.MatchString(s) {
			if canonical, ok := normMap[strings.ToLower(r.Category)]; ok {
				return canonical
			}
		}
	}
	return ""
}

// InferCategory 按风格推断酒款分类；未匹配到规则时默认回退到可用列表中的「拉格」或首项。
func InferCategory(style string, available []string) string {
	if len(available) == 0 {
		available = DefaultBeerCategories
	}
	if c := MatchCategory(style, available); c != "" {
		return c
	}
	for _, a := range available {
		if strings.TrimSpace(a) == "拉格" {
			return "拉格"
		}
	}
	for _, a := range available {
		if t := strings.TrimSpace(a); t != "" {
			return t
		}
	}
	return DefaultBeerCategories[0]
}

// ValidRemotePosterURL 仅允许无控制字符与反斜杠的 http(s):// 外链，用于过滤远端检索返回的酒标地址。
func ValidRemotePosterURL(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" || strings.ContainsRune(s, '\\') {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return (scheme == "http" || scheme == "https") && u.Host != ""
}

// ValidPosterURL 仅允许空串、/posters/ 与 /static/ 站内路径（拦截 ..、\、//）或 http(s):// 外链。
func ValidPosterURL(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" {
		return true
	}
	if strings.ContainsRune(s, '\\') {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	if strings.HasPrefix(s, "/") {
		if strings.HasPrefix(s, "//") || strings.Contains(s, "..") {
			return false
		}
		return strings.HasPrefix(s, "/posters/") || strings.HasPrefix(s, "/static/")
	}
	return ValidRemotePosterURL(s)
}

// PriceText 把分转成展示价；整数元不带小数。
func PriceText(cents int) string {
	if cents%100 == 0 {
		return fmt.Sprintf("¥ %d", cents/100)
	}
	return fmt.Sprintf("¥ %.2f", float64(cents)/100)
}

// MoneyText 同 PriceText，负数带减号。
func MoneyText(cents int) string {
	if cents < 0 {
		return "-" + PriceText(-cents)
	}
	return PriceText(cents)
}

// MoneyInput 把正数分值格式化为表单编辑用文本；<=0 返回空串。
func MoneyInput(cents int) string {
	if cents <= 0 {
		return ""
	}
	return strconv.FormatFloat(float64(cents)/100, 'f', -1, 64)
}

func NumText[T float64 | int](v *T, digits int) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(float64(*v), 'f', digits, 64)
}

func StatText[T float64 | int](v *T, unit string, digits int) string {
	if v == nil {
		return "—"
	}
	return strconv.FormatFloat(float64(*v), 'f', digits, 64) + unit
}

func ClampPct(v int) int {
	return ClampRange(v, 0, 100)
}

func ClampRange(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func ParseLocalTime(v string) *time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, ReportZone); err == nil {
			return &t
		}
	}
	return nil
}

func TimeInput(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.In(ReportZone).Format("2006-01-02T15:04")
}

func TimeText(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.In(ReportZone).Format("01-02 15:04")
}

func DateTimeText(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.In(ReportZone).Format("2006-01-02 15:04")
}

func IntValue(v string) int {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		return n
	}
	return 0
}

// MoneyCents 严格解析金额：空串算 0，仅允许规范十进制与合法千分位格式，禁止 12,5 / 科学计数法 / 超两位小数 / 超 int4。
func MoneyCents(v string) (int, error) {
	v = strings.NewReplacer("¥", "", "￥", "", " ", "").Replace(strings.TrimSpace(v))
	if v == "" {
		return 0, nil
	}
	negative := false
	if strings.HasPrefix(v, "-") {
		negative = true
		v = v[1:]
	}
	if v == "" {
		return 0, UserErr("金额必须是数字")
	}
	intPart, fracPart := v, ""
	if dot := strings.IndexByte(v, '.'); dot >= 0 {
		if strings.IndexByte(v[dot+1:], '.') >= 0 {
			return 0, UserErr("金额必须是数字")
		}
		intPart = v[:dot]
		fracPart = v[dot+1:]
		if fracPart == "" || strings.ContainsRune(fracPart, ',') {
			return 0, UserErr("金额必须是数字")
		}
	}
	if strings.ContainsRune(intPart, ',') {
		groups := strings.Split(intPart, ",")
		if len(groups[0]) < 1 || len(groups[0]) > 3 {
			return 0, UserErr("金额必须是数字")
		}
		for i := 1; i < len(groups); i++ {
			if len(groups[i]) != 3 {
				return 0, UserErr("金额必须是数字")
			}
		}
		intPart = strings.Join(groups, "")
	}
	if intPart == "" && fracPart == "" {
		return 0, UserErr("金额必须是数字")
	}
	for i := 0; i < len(intPart); i++ {
		if intPart[i] < '0' || intPart[i] > '9' {
			return 0, UserErr("金额必须是数字")
		}
	}
	for i := 0; i < len(fracPart); i++ {
		if fracPart[i] < '0' || fracPart[i] > '9' {
			return 0, UserErr("金额必须是数字")
		}
	}
	if len(fracPart) > 2 {
		return 0, UserErr("金额最多保留两位小数")
	}
	if negative {
		return 0, UserErr("金额不能为负")
	}
	var whole uint64
	if intPart != "" {
		parsed, err := strconv.ParseUint(intPart, 10, 64)
		if err != nil {
			return 0, UserErr("金额过大")
		}
		whole = parsed
	}
	var frac uint64
	switch len(fracPart) {
	case 1:
		frac = uint64(fracPart[0]-'0') * 10
	case 2:
		frac = uint64(fracPart[0]-'0')*10 + uint64(fracPart[1]-'0')
	}
	if whole > uint64(math.MaxInt32)/100 {
		return 0, UserErr("金额过大")
	}
	total := whole*100 + frac
	if total > uint64(math.MaxInt32) {
		return 0, UserErr("金额过大")
	}
	return int(total), nil
}

func MoneyText64(cents int64) string {
	neg := ""
	u := uint64(cents)
	if cents < 0 {
		neg = "-"
		u = uint64(-(cents + 1)) + 1
	}
	if u%100 == 0 {
		return fmt.Sprintf("%s¥ %d", neg, u/100)
	}
	return fmt.Sprintf("%s¥ %d.%02d", neg, u/100, u%100)
}

func ParseStrictFloat(raw, label string, minVal, maxVal float64, allowPercent bool) (*float64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	v := trimmed
	if allowPercent {
		v = strings.TrimSpace(strings.TrimSuffix(v, "%"))
		if v == "" {
			return nil, UserErrf("%s「%s」格式不正确", label, trimmed)
		}
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, UserErrf("%s「%s」格式不正确", label, trimmed)
	}
	if f < minVal || (maxVal > 0 && f > maxVal) {
		return nil, UserErrf("%s「%s」需在 %g 到 %g 之间", label, trimmed, minVal, maxVal)
	}
	return &f, nil
}

func ParseStrictInt(raw, label string, minVal, maxVal int) (*int, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(v, "-") {
			return nil, UserErrf("%s「%s」超出范围", label, v)
		}
		if minVal >= 0 {
			return nil, UserErrf("%s「%s」必须是非负整数", label, v)
		}
		return nil, UserErrf("%s「%s」必须是整数", label, v)
	}
	if n < minVal {
		if minVal == 0 {
			return nil, UserErrf("%s「%s」必须是非负整数", label, v)
		}
		if minVal == 1 {
			return nil, UserErrf("%s必须大于 0", label)
		}
		return nil, UserErrf("%s「%s」超出范围", label, v)
	}
	if n > maxVal {
		return nil, UserErrf("%s「%s」超出范围", label, v)
	}
	return &n, nil
}

func ParseSortOrder(raw string) (int, error) {
	n, err := ParseStrictInt(raw, "排序", -math.MaxInt32, math.MaxInt32)
	if err != nil {
		return 0, err
	}
	if n == nil {
		return 0, nil
	}
	return *n, nil
}

// ValidateBeerFieldsWithCategories 按指定分类白名单校验酒款与历史酒单共有字段。
func ValidateBeerTexts(brewery, name, style, category, posterURL string, tags []string) error {
	if len([]rune(strings.TrimSpace(brewery))) > 100 {
		return UserErr("厂牌名称过长（最多 100 字）")
	}
	if len([]rune(strings.TrimSpace(name))) > 120 {
		return UserErr("酒款名称过长（最多 120 字）")
	}
	if len([]rune(strings.TrimSpace(style))) > 100 {
		return UserErr("风格过长（最多 100 字）")
	}
	if len([]rune(strings.TrimSpace(category))) > 50 {
		return UserErr("分类名称过长（最多 50 字）")
	}
	if len([]rune(strings.TrimSpace(posterURL))) > 1000 {
		return UserErr("酒标地址过长（最多 1000 字符）")
	}
	if len(tags) > 20 {
		return UserErr("风味标签过多（最多 20 个）")
	}
	for _, tag := range tags {
		if len([]rune(strings.TrimSpace(tag))) > 40 {
			return UserErr("单个风味标签过长（最多 40 字）")
		}
	}
	return nil
}

func ValidateBeerFieldsWithCategories(name, category, posterURL string, abv, ut *float64, ibu, utCount *int, p150, p330, p500, sortOrder int, allowed []string, extraAllowed ...string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", UserErr("酒款名称不能为空")
	}
	if err := ValidateBeerTexts("", name, "", category, posterURL, nil); err != nil {
		return "", err
	}
	if !ValidPosterURL(posterURL) {
		return "", UserErr("酒标图片地址需为 http://、https:// 或站内路径 / 开头")
	}
	cat, err := ParseCategoryWithAllowed(category, allowed, extraAllowed...)
	if err != nil {
		return "", err
	}
	if abv != nil && (math.IsNaN(*abv) || math.IsInf(*abv, 0) || *abv < 0 || *abv > 100) {
		return "", UserErr("ABV 必须是 0 到 100 之间的数字")
	}
	if ibu != nil && (*ibu < 0 || *ibu > MaxBeerIBU) {
		return "", UserErrf("IBU 必须是 0 到 %d 之间的整数", MaxBeerIBU)
	}
	if ut != nil && (math.IsNaN(*ut) || math.IsInf(*ut, 0) || *ut < 0 || *ut > 5) {
		return "", UserErr("UT 评分必须是 0 到 5 之间的数字")
	}
	if utCount != nil && (*utCount < 0 || *utCount > math.MaxInt32) {
		return "", UserErr("评分人数必须是 0 到 2147483647 之间的整数")
	}
	if p150 < 0 || p330 < 0 || p500 < 0 {
		return "", UserErr("金额不能为负")
	}
	if p150 > math.MaxInt32 || p330 > math.MaxInt32 || p500 > math.MaxInt32 {
		return "", UserErr("金额过大")
	}
	if sortOrder < -math.MaxInt32 || sortOrder > math.MaxInt32 {
		return "", UserErr("排序超出范围")
	}
	return cat, nil
}

// BeerFormInput 保存酒款表单与历史酒单表单共享的严格校验结果。
type BeerFormInput struct {
	Brewery   string
	Name      string
	Style     string
	Category  string
	PosterURL string
	ABV       *float64
	IBU       *int
	UT        *float64
	UTCount   *int
	Price150  int
	Price330  int
	Price500  int
	Tags      []string
}

// ParseBeerFormWithCategories 使用当前动态酒款分类白名单解析并校验酒款表单。
func ParseBeerFormWithCategories(r *http.Request, allowed []string, extraAllowed ...string) (BeerFormInput, error) {
	in := BeerFormInput{
		Brewery:   strings.TrimSpace(r.FormValue("brewery")),
		Name:      strings.TrimSpace(r.FormValue("name")),
		Style:     strings.TrimSpace(r.FormValue("style")),
		Category:  strings.TrimSpace(r.FormValue("category")),
		PosterURL: strings.TrimSpace(r.FormValue("poster_url")),
		Tags:      SplitList(r.FormValue("tags")),
	}
	abv, abvErr := ParseStrictFloat(r.FormValue("abv"), "ABV", 0, 100, true)
	if abvErr == nil {
		in.ABV = abv
	}
	ibu, ibuErr := ParseStrictInt(r.FormValue("ibu"), "IBU", 0, MaxBeerIBU)
	if ibuErr == nil {
		in.IBU = ibu
	}
	ut, utErr := ParseStrictFloat(r.FormValue("ut_rating"), "UT 评分", 0, 5, false)
	if utErr == nil {
		in.UT = ut
	}
	utCount, utCountErr := ParseStrictInt(r.FormValue("ut_count"), "评分人数", 0, math.MaxInt32)
	if utCountErr == nil {
		in.UTCount = utCount
	}
	p150, p150Err := MoneyCents(r.FormValue("price_150"))
	if p150Err == nil {
		in.Price150 = p150
	}
	p330, p330Err := MoneyCents(r.FormValue("price_330"))
	if p330Err == nil {
		in.Price330 = p330
	}
	p500, p500Err := MoneyCents(r.FormValue("price_500"))
	if p500Err == nil {
		in.Price500 = p500
	}

	if err := ValidateBeerTexts(in.Brewery, in.Name, in.Style, in.Category, in.PosterURL, in.Tags); err != nil {
		return in, err
	}
	if in.Name == "" {
		return in, UserErr("酒款名称不能为空")
	}
	if !ValidPosterURL(in.PosterURL) {
		return in, UserErr("酒标图片地址需为 http://、https:// 或站内路径 / 开头")
	}
	cat, catErr := ParseCategoryWithAllowed(in.Category, allowed, extraAllowed...)
	if catErr != nil {
		return in, catErr
	}
	in.Category = cat
	for _, err := range []error{abvErr, ibuErr, utErr, utCountErr} {
		if err != nil {
			return in, err
		}
	}
	if p150Err != nil {
		return in, UserErr("150ml 售价：" + p150Err.Error())
	}
	if p330Err != nil {
		return in, UserErr("330ml 售价：" + p330Err.Error())
	}
	if p500Err != nil {
		return in, UserErr("500ml 售价：" + p500Err.Error())
	}
	return in, nil
}

func SplitList(v string) []string {
	out := []string{}
	for _, p := range strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == '，' || r == '、' || r == ';' || r == '；' || r == '\n'
	}) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
