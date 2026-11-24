package platform

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
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

var Categories = []Option{
	{Value: "fresh", Label: "清爽"},
	{Value: "hop", Label: "酒花"},
	{Value: "sour", Label: "酸野菌"},
	{Value: "dark", Label: "深色"},
	{Value: "strong", Label: "烈性"},
}

func CategoryLabel(v string) string {
	for _, c := range Categories {
		if c.Value == v {
			return c.Label
		}
	}
	return v
}

func ParseCategory(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	lower := strings.ToLower(v)
	for _, c := range Categories {
		if lower == c.Value || v == c.Label {
			return c.Value, nil
		}
	}
	return "", UserErrf("不支持的分类色 %q（可选：清爽/fresh、酒花/hop、酸野菌/sour、深色/dark、烈性/strong）", v)
}

// 风格 -> 分类色，规则与 web/templates/screen/screen.html 里的 tone() 保持一致（支持中英文风格名）。
var ToneRules = []struct {
	Tone string
	Re   *regexp.Regexp
}{
	{"sour", regexp.MustCompile(`lambic|gueuze|geuze|sour|berliner|wild|brett|gose|酸|贵兹|兰比克|野菌|柏林|古斯`)},
	{"hop", regexp.MustCompile(`ipa|pale ale|pilsner|pale lager|皮尔森|比尔森|淡色拉格|淡色艾尔|淡艾|酒花|印度淡`)},
	{"dark", regexp.MustCompile(`stout|porter|quadrupel|dubbel|barleywine|brown ale|schwarz|世涛|波特|四料|双料|黑啤|棕艾|大麦酒|烤麦`)},
	{"strong", regexp.MustCompile(`tripel|strong|barley wine|abbey|trappist|imperial|barrel|三料|烈性|修道院|帝国|过桶|高度`)},
	{"fresh", regexp.MustCompile(`lager|wheat|weiss|witbier|kölsch|kolsch|blonde|saison|pale|拉格|小麦|白啤|科隆|金色|赛松|清爽`)},
}

func MatchTone(style string) string {
	s := strings.ToLower(style)
	for _, r := range ToneRules {
		if r.Re.MatchString(s) {
			return r.Tone
		}
	}
	return ""
}

func ToneOf(style string) string {
	if t := MatchTone(style); t != "" {
		return t
	}
	return "fresh"
}

// ValidPosterURL 仅允许空串、http(s):// 或站内相对路径 /...，拦截 javascript: 等危险协议。
func ValidPosterURL(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" {
		return true
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	if strings.HasPrefix(s, "/") {
		return !strings.HasPrefix(s, "//")
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return (scheme == "http" || scheme == "https") && u.Host != ""
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

func TwoDigit(n int) string { return fmt.Sprintf("%02d", n) }

func ParseLocalTime(v string) *time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return &t
		}
	}
	return nil
}

func TimeInput(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02T15:04")
}

func TimeText(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Format("01-02 15:04")
}

func OptFloat(v string) *float64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return &f
}

func OptInt(v string) *int {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > math.MaxInt32 {
		return nil
	}
	return &n
}

func IntValue(v string) int {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		return n
	}
	return 0
}

// MoneyCents 严格解析金额：空串算 0，支持去千分位逗号与币符，拒绝负数、非数字或超出 int4 上限。
func MoneyCents(v string) (int, error) {
	trimmed := strings.TrimSpace(v)
	v = strings.NewReplacer("¥", "", "￥", "", ",", "", " ", "").Replace(trimmed)
	if v == "" {
		if trimmed != "" && strings.Contains(trimmed, ",") {
			return 0, UserErr("金额必须是数字")
		}
		return 0, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, UserErr("金额必须是数字")
	}
	if f < 0 {
		return 0, UserErr("金额不能为负")
	}
	if f*100 > float64(math.MaxInt32) {
		return 0, UserErr("金额过大")
	}
	return int(math.Round(f * 100)), nil
}

func PriceCents(v string) int {
	cents, _ := MoneyCents(v)
	return cents
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

func ValidateBeerFields(name, category, posterURL string, abv, ut *float64, ibu, utCount *int, p150, p330, p500, sortOrder int) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", UserErr("酒款名称不能为空")
	}
	if !ValidPosterURL(posterURL) {
		return "", UserErr("酒标图片地址需为 http://、https:// 或站内路径 / 开头")
	}
	cat, err := ParseCategory(category)
	if err != nil {
		return "", err
	}
	if abv != nil && (math.IsNaN(*abv) || math.IsInf(*abv, 0) || *abv < 0 || *abv > 100) {
		return "", UserErr("ABV 必须是 0 到 100 之间的数字")
	}
	if ibu != nil && (*ibu < 0 || *ibu > math.MaxInt32) {
		return "", UserErr("IBU 必须是 0 到 2147483647 之间的整数")
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

func ParseBeerForm(r *http.Request) (BeerFormInput, error) {
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
	ibu, ibuErr := ParseStrictInt(r.FormValue("ibu"), "IBU", 0, math.MaxInt32)
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

	if in.Name == "" {
		return in, UserErr("酒款名称不能为空")
	}
	if !ValidPosterURL(in.PosterURL) {
		return in, UserErr("酒标图片地址需为 http://、https:// 或站内路径 / 开头")
	}
	cat, catErr := ParseCategory(in.Category)
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

func URLMsg(m string) string {
	return url.QueryEscape(m)
}
