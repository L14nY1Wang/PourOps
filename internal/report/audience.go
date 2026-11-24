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
	"IPA": "#20b27c",
	"拉格":  "#e0a938",
	"皮尔森": "#38b2ac",
	"小麦":  "#d69e2e",
	"酸小麦": "#8b6fe8",
	"果泥":  "#e06392",
	"西打":  "#52b788",
	"世涛":  "#b86b35",
	"波特":  "#8c564b",
	"修道院": "#d95d4a",
}

var audienceFallbackPalette = []string{
	"#3b82f6",
	"#ec4899",
	"#14b8a6",
	"#f59e0b",
	"#8b5cf6",
	"#10b981",
	"#f97316",
	"#6366f1",
}

// AudienceCategoryColor 为预设与后台自定义酒款分类分配稳定的十六进制图表色。
func AudienceCategoryColor(cat string) string {
	key := strings.TrimSpace(cat)
	if hex, ok := defaultAudienceColors[key]; ok {
		return hex
	}
	for k, hex := range defaultAudienceColors {
		if strings.EqualFold(k, key) {
			return hex
		}
	}
	var h uint32
	for _, r := range strings.ToLower(key) {
		h = h*31 + uint32(r)
	}
	return audienceFallbackPalette[int(h)%len(audienceFallbackPalette)]
}

type CategorySegment struct {
	Category string
	Color    string
	Cups     int64
	PrevCups int64
	VolumeML int64
	Revenue  int64
	Cost     int64
	AvgABV   float64
	HasABV   bool
	CupShare float64
	RevShare float64
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
	SmallCups  int64 // ≤200ml
	MediumCups int64 // 201–400ml
	LargeCups  int64 // >400ml
}

func (c CupSizePreference) Total() int64 { return c.SmallCups + c.MediumCups + c.LargeCups }

func (c CupSizePreference) SmallShareText() string {
	return ratioPercentText(c.SmallCups, c.Total())
}

func (c CupSizePreference) MediumShareText() string {
	return ratioPercentText(c.MediumCups, c.Total())
}

func (c CupSizePreference) LargeShareText() string {
	return ratioPercentText(c.LargeCups, c.Total())
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
	return fmt.Sprintf("%s（%s）", best.label, ratioPercentText(best.cups, total))
}

type SpotPreference struct {
	BarCups     int64
	BarTopCat   string
	TableCups   int64
	TableTopCat string
}

func (p SpotPreference) HasData() bool { return p.BarCups > 0 || p.TableCups > 0 }

func (p SpotPreference) SummaryText() string {
	switch {
	case p.BarCups > 0 && p.TableCups > 0:
		return fmt.Sprintf("吧台偏爱 %s（%d 杯）· 桌台偏爱 %s（%d 杯）", p.BarTopCat, p.BarCups, p.TableTopCat, p.TableCups)
	case p.BarCups > 0:
		return fmt.Sprintf("吧台客群偏爱 %s（%d 杯）", p.BarTopCat, p.BarCups)
	case p.TableCups > 0:
		return fmt.Sprintf("桌台客群偏爱 %s（%d 杯）", p.TableTopCat, p.TableCups)
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

type AudienceChartPoint struct {
	Category string  `json:"category"`
	Color    string  `json:"color"`
	Cups     int64   `json:"cups"`
	Revenue  float64 `json:"revenue"`
	Profit   float64 `json:"profit"`
	AvgCup   float64 `json:"avg_cup"`
	CupShare float64 `json:"cup_share"`
	RevShare float64 `json:"rev_share"`
}

func (i AudienceInsights) ChartJSON() template.JS {
	points := make([]AudienceChartPoint, 0, len(i.Segments))
	for _, s := range i.Segments {
		points = append(points, AudienceChartPoint{
			Category: s.Category,
			Color:    s.Color,
			Cups:     s.Cups,
			Revenue:  math.Round(float64(s.Revenue)) / 100,
			Profit:   math.Round(float64(s.Profit())) / 100,
			AvgCup:   math.Round(float64(s.AvgCupCents())) / 100,
			CupShare: math.Round(s.CupShare*10) / 10,
			RevShare: math.Round(s.RevShare*10) / 10,
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
		costExact   float64
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

	rows, err := tx.Query(ctx, `
		select case when p.poured_at >= $2 then 'cur' else 'prev' end as period,
		       b.category,
		       b.style,
		       b.abv,
		       coalesce(sum(p.cups), 0)::bigint as cups,
		       coalesce(sum(p.ml), 0)::bigint as ml,
		       coalesce(sum(p.amount_cents), 0)::bigint as revenue,
		       coalesce(sum(k.cost_cents::numeric * p.ml / nullif(k.capacity_ml, 0)), 0)::double precision as cost_exact
		from keg_pours p
		join kegs k on k.id = p.keg_id
		join beers b on b.id = k.beer_id
		where p.poured_at >= $1 and p.poured_at < $3
		group by 1, 2, 3, 4`, prevStart, start, end)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var period, rawCat, style string
		var abv *float64
		var cups, ml, revenue int64
		var costExact float64
		if err := rows.Scan(&period, &rawCat, &style, &abv, &cups, &ml, &revenue, &costExact); err != nil {
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
		a.costExact += costExact
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
			Cost:     int64(math.Round(a.costExact)),
		}
		out.TotalCost += seg.Cost
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
	slices.SortFunc(out.Segments, func(a, b CategorySegment) int {
		if a.Cups != b.Cups {
			return cmpInt64Desc(a.Cups, b.Cups)
		}
		if a.Revenue != b.Revenue {
			return cmpInt64Desc(a.Revenue, b.Revenue)
		}
		return strings.Compare(a.Category, b.Category)
	})

	if err := tx.QueryRow(ctx, `
		select coalesce(sum(case when p.ml::numeric / p.cups <= 200 then p.cups else 0 end), 0)::bigint,
		       coalesce(sum(case when p.ml::numeric / p.cups > 200 and p.ml::numeric / p.cups <= 400 then p.cups else 0 end), 0)::bigint,
		       coalesce(sum(case when p.ml::numeric / p.cups > 400 then p.cups else 0 end), 0)::bigint
		from keg_pours p
		where p.poured_at >= $1 and p.poured_at < $2
		  and p.cups > 0 and p.ml > 0`, start, end).
		Scan(&out.CupSizes.SmallCups, &out.CupSizes.MediumCups, &out.CupSizes.LargeCups); err != nil {
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
	out.Spots.BarTopCat = pickTopCategory(barCats)
	out.Spots.TableTopCat = pickTopCategory(tableCats)
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

func pickTopCategory(m map[string]int64) string {
	bestCat := ""
	var bestCups int64
	for cat, cups := range m {
		if cups > bestCups || (cups == bestCups && (bestCat == "" || cat < bestCat)) {
			bestCat = cat
			bestCups = cups
		}
	}
	return bestCat
}
