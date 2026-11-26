package report

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"pourops/internal/platform"
)

var defaultAudienceColors = map[string]string{
	"IPA": "#16a34a", // 酒花翠绿
	"西打":  "#dc2626", // 苹果正红
	"拉格":  "#eab308", // 明金黄
	"皮尔森": "#2563eb", // 清冽宝蓝
	"小麦":  "#f97316", // 柑橘鲜橙
	"酸小麦": "#06b6d4", // 湖水青蓝
	"酸艾尔": "#9333ea", // 野菌紫罗兰
	"果泥":  "#ec4899", // 浆果玫红
	"世涛":  "#7c4a21", // 烘焙咖啡棕
	"波特":  "#475569", // 历史兼容：冷石板灰
	"修道院": "#881337", // 历史兼容：深酒红
}

var audienceFallbackPalette = []string{
	"#84cc16", // 青柠绿
	"#0f766e", // 深松青
	"#4f46e5", // 靛蓝
	"#be185d", // 深梅红
	"#64748b", // 冷岩灰
	"#0284c7", // 天海蓝
	"#a16207", // 焦糖古铜
	"#c026d3", // 电光紫红
	"#059669", // 翡翠深绿
	"#e11d48", // 绯红
	"#b45309", // 深琥珀
	"#4338ca", // 深靛青
}

func lookupDefaultAudienceColor(cat string) (string, bool) {
	key := strings.TrimSpace(cat)
	if hex, ok := defaultAudienceColors[key]; ok {
		return hex, true
	}
	for k, hex := range defaultAudienceColors {
		if strings.EqualFold(k, key) {
			return hex, true
		}
	}
	return "", false
}

// AudienceCategoryColor 为预设与后台自定义酒款分类分配稳定的十六进制图表色。
func AudienceCategoryColor(cat string) string {
	if hex, ok := lookupDefaultAudienceColor(cat); ok {
		return hex
	}
	var h uint32
	for _, r := range strings.ToLower(strings.TrimSpace(cat)) {
		h = h*31 + uint32(r)
	}
	return audienceFallbackPalette[int(h)%len(audienceFallbackPalette)]
}

func parseHexByte(s string) (uint8, bool) {
	if len(s) != 2 {
		return 0, false
	}
	nibble := func(ch byte) (uint8, bool) {
		switch {
		case ch >= '0' && ch <= '9':
			return ch - '0', true
		case ch >= 'a' && ch <= 'f':
			return ch - 'a' + 10, true
		case ch >= 'A' && ch <= 'F':
			return ch - 'A' + 10, true
		default:
			return 0, false
		}
	}
	hi, ok1 := nibble(s[0])
	lo, ok2 := nibble(s[1])
	if !ok1 || !ok2 {
		return 0, false
	}
	return hi<<4 | lo, true
}

func hexColorLab(hex string) (l, a, b float64, ok bool) {
	raw := strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(raw) != 6 {
		return 0, 0, 0, false
	}
	r8, ok1 := parseHexByte(raw[0:2])
	g8, ok2 := parseHexByte(raw[2:4])
	b8, ok3 := parseHexByte(raw[4:6])
	if !ok1 || !ok2 || !ok3 {
		return 0, 0, 0, false
	}
	lin := func(u uint8) float64 {
		c := float64(u) / 255.0
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	lr, lg, lb := lin(r8), lin(g8), lin(b8)
	x := (lr*0.4124564 + lg*0.3575761 + lb*0.1804375) / 0.95047
	y := (lr*0.2126729 + lg*0.7151522 + lb*0.0721750) / 1.00000
	z := (lr*0.0193339 + lg*0.1191920 + lb*0.9503041) / 1.08883
	f := func(t float64) float64 {
		if t > 0.008856 {
			return math.Cbrt(t)
		}
		return 7.787*t + 16.0/116.0
	}
	fx, fy, fz := f(x), f(y), f(z)
	return 116*fy - 16, 500 * (fx - fy), 200 * (fy - fz), true
}

func colorLabDistance(hex1, hex2 string) float64 {
	l1, a1, b1, ok1 := hexColorLab(hex1)
	l2, a2, b2, ok2 := hexColorLab(hex2)
	if !ok1 || !ok2 {
		return 0
	}
	dl, da, db := l1-l2, a1-a2, b1-b2
	return math.Sqrt(dl*dl + da*da + db*db)
}

func hslToHex(h, s, l float64) string {
	h = math.Mod(h, 360)
	if h < 0 {
		h += 360
	}
	c := (1 - math.Abs(2*l-1)) * s
	x := c * (1 - math.Abs(math.Mod(h/60.0, 2)-1))
	m := l - c/2
	var r1, g1, b1 float64
	switch {
	case h < 60:
		r1, g1, b1 = c, x, 0
	case h < 120:
		r1, g1, b1 = x, c, 0
	case h < 180:
		r1, g1, b1 = 0, c, x
	case h < 240:
		r1, g1, b1 = 0, x, c
	case h < 300:
		r1, g1, b1 = x, 0, c
	default:
		r1, g1, b1 = c, 0, x
	}
	clampByte := func(v float64) uint8 {
		n := math.Round((v + m) * 255)
		if n < 0 {
			return 0
		}
		if n > 255 {
			return 255
		}
		return uint8(n)
	}
	return fmt.Sprintf("#%02x%02x%02x", clampByte(r1), clampByte(g1), clampByte(b1))
}

func minLabDistance(hex string, existing []string) float64 {
	if len(existing) == 0 {
		return math.Inf(1)
	}
	best := math.Inf(1)
	for _, other := range existing {
		if d := colorLabDistance(hex, other); d < best {
			best = d
		}
	}
	return best
}

// assignDistinctSegmentColors 先固定内置风格主色，再按确定性顺序为自定义分类分配感知差异最大的可用颜色。
func assignDistinctSegmentColors(segments []CategorySegment) {
	if len(segments) == 0 {
		return
	}
	usedColors := make([]string, 0, len(segments))
	usedSet := make(map[string]bool, len(segments))
	customIdx := make([]int, 0, len(segments))

	for i := range segments {
		if hex, ok := lookupDefaultAudienceColor(segments[i].Category); ok {
			segments[i].Color = hex
			low := strings.ToLower(hex)
			if !usedSet[low] {
				usedSet[low] = true
				usedColors = append(usedColors, hex)
			}
			continue
		}
		customIdx = append(customIdx, i)
	}
	if len(customIdx) == 0 {
		return
	}

	slices.SortFunc(customIdx, func(i, j int) int {
		ci, cj := strings.TrimSpace(segments[i].Category), strings.TrimSpace(segments[j].Category)
		li, lj := strings.ToLower(ci), strings.ToLower(cj)
		if li != lj {
			return strings.Compare(li, lj)
		}
		if ci != cj {
			return strings.Compare(ci, cj)
		}
		return i - j
	})

	candidates := make([]string, 0, len(audienceFallbackPalette)+16)
	candidates = append(candidates, audienceFallbackPalette...)
	for k := 0; k < 16; k++ {
		h := math.Mod(float64(k)*137.508+22.0, 360.0)
		s := 0.72
		if k%2 == 1 {
			s = 0.60
		}
		l := 0.44
		switch k % 3 {
		case 1:
			l = 0.36
		case 2:
			l = 0.52
		}
		candidates = append(candidates, hslToHex(h, s, l))
	}

	const minPerceptualGap = 26.0
	for _, idx := range customIdx {
		pref := AudienceCategoryColor(segments[idx].Category)
		chosen := ""
		if !usedSet[strings.ToLower(pref)] && minLabDistance(pref, usedColors) >= minPerceptualGap {
			chosen = pref
		} else {
			bestDist := -1.0
			for _, cand := range candidates {
				if usedSet[strings.ToLower(cand)] {
					continue
				}
				d := minLabDistance(cand, usedColors)
				if chosen == "" || d > bestDist {
					chosen = cand
					bestDist = d
				}
			}
			if chosen == "" {
				chosen = pref
			}
		}
		segments[idx].Color = chosen
		usedSet[strings.ToLower(chosen)] = true
		usedColors = append(usedColors, chosen)
	}
}

type CategorySegment struct {
	Category    string
	Color       string
	Cups        int64
	PrevCups    int64
	VolumeML    int64
	Revenue     int64
	Cost        int64
	AvgABV      float64
	HasABV      bool
	CupShare    float64
	RevShare    float64
	ProfitShare float64
}

func (s CategorySegment) Profit() int64       { return s.Revenue - s.Cost }
func (s CategorySegment) RevenueText() string { return platform.PriceText(int(s.Revenue)) }
func (s CategorySegment) ProfitText() string  { return platform.MoneyText(int(s.Profit())) }
func (s CategorySegment) MarginText() string  { return ratioPercentText(s.Profit(), s.Revenue) }
func (s CategorySegment) CupShareText() string {
	return fmt.Sprintf("%.1f%%", s.CupShare)
}
func (s CategorySegment) RevShareText() string {
	return fmt.Sprintf("%.1f%%", s.RevShare)
}
func (s CategorySegment) ProfitShareText() string {
	return fmt.Sprintf("%.1f%%", s.ProfitShare)
}

func (s CategorySegment) AvgCupCents() int64 {
	if s.Cups <= 0 {
		return 0
	}
	return int64(math.Round(float64(s.Revenue) / float64(s.Cups)))
}

func (s CategorySegment) AvgCupText() string {
	if s.Cups <= 0 {
		return "—"
	}
	return platform.PriceText(int(s.AvgCupCents()))
}

func (s CategorySegment) CupsChangeText() string {
	if s.PrevCups > 0 {
		pct := math.Round(float64(s.Cups-s.PrevCups)*1000/float64(s.PrevCups)) / 10
		if pct == 0 {
			return "0%"
		}
		return fmt.Sprintf("%+.1f%%", pct)
	}
	if s.Cups > 0 {
		return fmt.Sprintf("+%d 杯", s.Cups)
	}
	return "—"
}

func (s CategorySegment) VolumeText() string {
	if s.VolumeML < 1000 {
		return fmt.Sprintf("%dml", s.VolumeML)
	}
	return fmt.Sprintf("%.1fL", float64(s.VolumeML)/1000)
}

func (s CategorySegment) ABVText() string {
	if !s.HasABV {
		return ""
	}
	return fmt.Sprintf("%.1f%% ABV", s.AvgABV)
}

func (s CategorySegment) DotStyle() template.CSS {
	return template.CSS("background-color:" + s.Color)
}

func (s CategorySegment) BarStyle() template.CSS {
	w := s.CupShare
	if w < 2 {
		w = 2
	}
	if w > 100 {
		w = 100
	}
	return template.CSS(fmt.Sprintf("width:%.1f%%;background-color:%s", w, s.Color))
}

type CupSizePreference struct {
	SmallCups   int64 // ≤200ml
	MediumCups  int64 // 201–400ml
	LargeCups   int64 // >400ml
	UnknownCups int64 // 没记毫升或没记杯数的记录，只有这些能让 Total() 对齐 TotalCups
}

func (c CupSizePreference) Total() int64 {
	return c.SmallCups + c.MediumCups + c.LargeCups + c.UnknownCups
}

func (c CupSizePreference) SmallShareText() string {
	return ratioPercentText(c.SmallCups, c.Total())
}

func (c CupSizePreference) MediumShareText() string {
	return ratioPercentText(c.MediumCups, c.Total())
}

func (c CupSizePreference) LargeShareText() string {
	return ratioPercentText(c.LargeCups, c.Total())
}

// UnknownShareText 展示杯型缺失的占比，避免把「没记杯型」误读成小杯。
func (c CupSizePreference) UnknownShareText() string {
	return ratioPercentText(c.UnknownCups, c.Total())
}

func (c CupSizePreference) DominantLabel() string {
	total := c.Total()
	if total <= 0 {
		return "—"
	}
	type cupChoice struct {
		label string
		cups  int64
	}
	best := cupChoice{"330ml 标准杯", c.MediumCups}
	for _, alt := range []cupChoice{
		{"150ml 试饮杯", c.SmallCups},
		{"500ml 大杯", c.LargeCups},
	} {
		if alt.cups > best.cups {
			best = alt
		}
	}
	if best.cups <= 0 {
		return "未记录杯型"
	}
	return fmt.Sprintf("%s（%s）", best.label, ratioPercentText(best.cups, total))
}

type SpotPreference struct {
	BarCups      int64
	BarTopCat    string
	BarTopCups   int64
	TableCups    int64
	TableTopCat  string
	TableTopCups int64
}

func (p SpotPreference) HasData() bool { return p.BarCups > 0 || p.TableCups > 0 }

func (p SpotPreference) SummaryText() string {
	barCups := p.BarTopCups
	if barCups == 0 {
		barCups = p.BarCups
	}
	tableCups := p.TableTopCups
	if tableCups == 0 {
		tableCups = p.TableCups
	}
	switch {
	case p.BarCups > 0 && p.TableCups > 0:
		return fmt.Sprintf("吧台偏爱 %s（%d 杯）· 桌台偏爱 %s（%d 杯）", p.BarTopCat, barCups, p.TableTopCat, tableCups)
	case p.BarCups > 0:
		return fmt.Sprintf("吧台客群偏爱 %s（%d 杯）", p.BarTopCat, barCups)
	case p.TableCups > 0:
		return fmt.Sprintf("桌台客群偏爱 %s（%d 杯）", p.TableTopCat, tableCups)
	default:
		return "暂无 POS 台位饮用场景记录"
	}
}

type AudienceInsights struct {
	TotalCups    int64
	TotalVolume  int64
	TotalRevenue int64
	TotalCost    int64
	Segments     []CategorySegment
	CupSizes     CupSizePreference
	Spots        SpotPreference
}

func (i AudienceInsights) HasData() bool {
	return len(i.Segments) > 0 && i.TotalCups > 0
}

func (i AudienceInsights) CanRadar() bool {
	return len(i.Segments) >= 3
}

func (i AudienceInsights) TopCategory() *CategorySegment {
	if len(i.Segments) == 0 {
		return nil
	}
	seg := i.Segments[0]
	return &seg
}

func (i AudienceInsights) HighestAvgCupCategory() *CategorySegment {
	var best *CategorySegment
	bestRatio := -1.0
	for idx := range i.Segments {
		seg := i.Segments[idx]
		if seg.Cups <= 0 {
			continue
		}
		ratio := float64(seg.Revenue) / float64(seg.Cups)
		if best == nil || ratio > bestRatio ||
			(ratio == bestRatio && (seg.Cups > best.Cups || (seg.Cups == best.Cups && seg.Category < best.Category))) {
			copySeg := seg
			best = &copySeg
			bestRatio = ratio
		}
	}
	return best
}

func (i AudienceInsights) TotalRevenueText() string {
	return platform.PriceText(int(i.TotalRevenue))
}

func (i AudienceInsights) AvgCupText() string {
	if i.TotalCups <= 0 {
		return "—"
	}
	return platform.PriceText(int(math.Round(float64(i.TotalRevenue) / float64(i.TotalCups))))
}

func (i AudienceInsights) TotalVolumeText() string {
	return (CategorySegment{VolumeML: i.TotalVolume}).VolumeText()
}

func (i AudienceInsights) TotalProfit() int64 {
	return i.TotalRevenue - i.TotalCost
}

type AudienceChartPoint struct {
	Category    string  `json:"category"`
	Color       string  `json:"color"`
	Cups        int64   `json:"cups"`
	Revenue     float64 `json:"revenue"`
	Profit      float64 `json:"profit"`
	AvgCup      float64 `json:"avg_cup"`
	CupShare    float64 `json:"cup_share"`
	RevShare    float64 `json:"rev_share"`
	ProfitShare float64 `json:"profit_share"`
	Margin      string  `json:"margin"`
}

func (i AudienceInsights) ChartJSON() template.JS {
	points := make([]AudienceChartPoint, 0, len(i.Segments))
	for _, s := range i.Segments {
		points = append(points, AudienceChartPoint{
			Category:    s.Category,
			Color:       s.Color,
			Cups:        s.Cups,
			Revenue:     math.Round(float64(s.Revenue)) / 100,
			Profit:      math.Round(float64(s.Profit())) / 100,
			AvgCup:      math.Round(float64(s.AvgCupCents())) / 100,
			CupShare:    math.Round(s.CupShare*10) / 10,
			RevShare:    math.Round(s.RevShare*10) / 10,
			ProfitShare: math.Round(s.ProfitShare*10) / 10,
			Margin:      s.MarginText(),
		})
	}
	b, _ := json.Marshal(points)
	return template.JS(b)
}

func normalizeAudienceCategory(rawCat, style string, allowed []string) string {
	if c := platform.CategoryLabel(rawCat); c != "" {
		return c
	}
	return platform.InferCategory(style, allowed)
}

// LoadAudienceInsights 在只读事务快照中按酒单分类统计桶装精酿客户群口味偏好、杯型偏好与 POS 饮用场景。
func LoadAudienceInsights(ctx context.Context, tx pgx.Tx, start, end time.Time, allowedCategories []string) (AudienceInsights, error) {
	var out AudienceInsights
	if len(allowedCategories) == 0 {
		allowedCategories = platform.DefaultBeerCategories
	}
	prevStart := start.Add(-end.Sub(start))

	type catAcc struct {
		cups        int64
		prevCups    int64
		volumeML    int64
		revenue     int64
		costCents   int64
		abvWeighted float64
		abvWeight   int64
	}
	accByCat := make(map[string]*catAcc)
	getAcc := func(cat string) *catAcc {
		a, ok := accByCat[cat]
		if !ok {
			a = &catAcc{}
			accByCat[cat] = a
		}
		return a
	}

	// 成本与 sales.go 同口径：按「营业日 × 分类」把合计金额舍入到分再累加。
	// 逐笔舍入会和整日口径差出几百分；日/分类粒度舍入后，与 SalesInsights.Draft.Cost 的差
	// 最多是每多一个分类每天 1 分（sales.go 的日总成本只舍入一次）。
	rows, err := tx.Query(ctx, `
		select case when p.poured_at >= $2 then 'cur' else 'prev' end as period,
		       (p.poured_at at time zone 'UTC' + interval '1 hour')::date as day,
		       b.category,
		       b.style,
		       b.abv,
		       coalesce(sum(p.cups), 0)::bigint as cups,
		       coalesce(sum(p.ml), 0)::bigint as ml,
		       coalesce(sum(p.amount_cents), 0)::bigint as revenue,
		       coalesce(sum(
		           case when coalesce(p.cost_cents, 0) > 0 then p.cost_cents::numeric
		                when k.cost_cents > 0 and k.capacity_ml > 0 then k.cost_cents::numeric * p.ml / k.capacity_ml
		                else 0 end
		       ), 0)::double precision as cost_exact
		from keg_pours p
		join kegs k on k.id = p.keg_id
		join beers b on b.id = k.beer_id
		where p.poured_at >= $1 and p.poured_at < $3
		group by 1, 2, 3, 4, 5`, prevStart, start, end)
	if err != nil {
		return out, err
	}
	// 同一营业日同一分类可能命中多款酒，必须先按 (day, cat) 合计再舍入，否则逐行舍入与 sales.go 对不上。
	type dayCatKey struct {
		day string
		cat string
	}
	costByDayCat := make(map[dayCatKey]float64)
	for rows.Next() {
		var period, rawCat, style string
		var day time.Time
		var abv *float64
		var cups, ml, revenue int64
		var costExact float64
		if err := rows.Scan(&period, &day, &rawCat, &style, &abv, &cups, &ml, &revenue, &costExact); err != nil {
			rows.Close()
			return out, err
		}
		cat := normalizeAudienceCategory(rawCat, style, allowedCategories)
		a := getAcc(cat)
		if period == "prev" {
			a.prevCups += cups
			continue
		}
		a.cups += cups
		a.volumeML += ml
		a.revenue += revenue
		costByDayCat[dayCatKey{day: day.Format(time.DateOnly), cat: cat}] += costExact
		if abv != nil && *abv >= 0 && cups > 0 {
			a.abvWeighted += *abv * float64(cups)
			a.abvWeight += cups
		}
		out.TotalCups += cups
		out.TotalVolume += ml
		out.TotalRevenue += revenue
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	for k, v := range costByDayCat {
		accByCat[k.cat].costCents += int64(math.Round(v))
	}

	var posProfitTotal int64
	for cat, a := range accByCat {
		if a.cups <= 0 && a.revenue <= 0 {
			continue
		}
		seg := CategorySegment{
			Category: cat,
			Color:    AudienceCategoryColor(cat),
			Cups:     a.cups,
			PrevCups: a.prevCups,
			VolumeML: a.volumeML,
			Revenue:  a.revenue,
			Cost:     a.costCents,
		}
		out.TotalCost += seg.Cost
		if p := seg.Profit(); p > 0 {
			posProfitTotal += p
		}
		if a.abvWeight > 0 {
			seg.HasABV = true
			seg.AvgABV = math.Round((a.abvWeighted/float64(a.abvWeight))*10) / 10
		}
		if out.TotalCups > 0 {
			seg.CupShare = float64(seg.Cups) * 100 / float64(out.TotalCups)
		}
		if out.TotalRevenue > 0 {
			seg.RevShare = float64(seg.Revenue) * 100 / float64(out.TotalRevenue)
		}
		out.Segments = append(out.Segments, seg)
	}
	if posProfitTotal > 0 {
		for idx := range out.Segments {
			if p := out.Segments[idx].Profit(); p > 0 {
				out.Segments[idx].ProfitShare = float64(p) * 100 / float64(posProfitTotal)
			}
		}
	}
	slices.SortFunc(out.Segments, func(a, b CategorySegment) int {
		if a.Cups != b.Cups {
			return cmpInt64Desc(a.Cups, b.Cups)
		}
		if a.Revenue != b.Revenue {
			return cmpInt64Desc(a.Revenue, b.Revenue)
		}
		return strings.Compare(a.Category, b.Category)
	})
	assignDistinctSegmentColors(out.Segments)

	// 杯型按毫升/杯分档；没记毫升或杯数的记录归入 UnknownCups，保证 Total() 与 TotalCups 对得上。
	if err := tx.QueryRow(ctx, `
		select coalesce(sum(case when p.ml > 0 and p.cups > 0 and p.ml::numeric / p.cups <= 200 then p.cups else 0 end), 0)::bigint,
		       coalesce(sum(case when p.ml > 0 and p.cups > 0 and p.ml::numeric / p.cups > 200 and p.ml::numeric / p.cups <= 400 then p.cups else 0 end), 0)::bigint,
		       coalesce(sum(case when p.ml > 0 and p.cups > 0 and p.ml::numeric / p.cups > 400 then p.cups else 0 end), 0)::bigint,
		       coalesce(sum(case when p.ml <= 0 or p.cups <= 0 then p.cups else 0 end), 0)::bigint
		from keg_pours p
		where p.poured_at >= $1 and p.poured_at < $2`, start, end).
		Scan(&out.CupSizes.SmallCups, &out.CupSizes.MediumCups, &out.CupSizes.LargeCups, &out.CupSizes.UnknownCups); err != nil {
		return out, err
	}

	spotRows, err := tx.Query(ctx, `
		select case when lower(o.spot) like 'bar-%' then 'bar' else 'table' end as zone,
		       b.category,
		       b.style,
		       coalesce(sum(i.qty), 0)::bigint as cups
		from order_items i
		join orders o on o.id = i.order_id
		join kegs k on k.id = i.ref_id
		join beers b on b.id = k.beer_id
		where i.kind = 'beer'
		  and i.paid_at is not null
		  and i.paid_at >= $1 and i.paid_at < $2
		group by 1, 2, 3`, start, end)
	if err != nil {
		return out, err
	}
	barCats := make(map[string]int64)
	tableCats := make(map[string]int64)
	for spotRows.Next() {
		var zone, rawCat, style string
		var cups int64
		if err := spotRows.Scan(&zone, &rawCat, &style, &cups); err != nil {
			spotRows.Close()
			return out, err
		}
		if cups <= 0 {
			continue
		}
		cat := normalizeAudienceCategory(rawCat, style, allowedCategories)
		if zone == "bar" {
			out.Spots.BarCups += cups
			barCats[cat] += cups
		} else {
			out.Spots.TableCups += cups
			tableCats[cat] += cups
		}
	}
	spotRows.Close()
	if err := spotRows.Err(); err != nil {
		return out, err
	}
	out.Spots.BarTopCat, out.Spots.BarTopCups = pickTopCategory(barCats)
	out.Spots.TableTopCat, out.Spots.TableTopCups = pickTopCategory(tableCats)
	return out, nil
}

func cmpInt64Desc(a, b int64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	default:
		return 0
	}
}

func pickTopCategory(m map[string]int64) (string, int64) {
	bestCat := ""
	var bestCups int64
	for cat, cups := range m {
		if cups > bestCups || (cups == bestCups && (bestCat == "" || cat < bestCat)) {
			bestCat = cat
			bestCups = cups
		}
	}
	return bestCat, bestCups
}
