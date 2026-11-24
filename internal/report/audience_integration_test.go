package report

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/internal/ui"
	"pourops/migrations"
	"pourops/web"
)

func TestAudienceCategoryColorAndFormatting(t *testing.T) {
	if c1, c2 := AudienceCategoryColor("赛松"), AudienceCategoryColor("赛松"); c1 == "" || !strings.HasPrefix(c1, "#") || c1 != c2 {
		t.Fatalf("自定义分类应生成稳定十六进制色: c1=%q c2=%q", c1, c2)
	}
	if AudienceCategoryColor("ipa") != AudienceCategoryColor("IPA") {
		t.Fatal("分类色映射应忽略大小写")
	}

	seenColors := make(map[string]string, len(platform.DefaultBeerCategories))
	for _, cat := range platform.DefaultBeerCategories {
		c := AudienceCategoryColor(cat)
		if len(c) != 7 || !strings.HasPrefix(c, "#") {
			t.Fatalf("默认分类 %s 颜色格式无效: %q", cat, c)
		}
		if prev, dup := seenColors[strings.ToLower(c)]; dup {
			t.Fatalf("默认分类 %s 与 %s 颜色重复: %s", prev, cat, c)
		}
		seenColors[strings.ToLower(c)] = cat
	}
	for i := 0; i < len(platform.DefaultBeerCategories); i++ {
		for j := i + 1; j < len(platform.DefaultBeerCategories); j++ {
			c1, c2 := platform.DefaultBeerCategories[i], platform.DefaultBeerCategories[j]
			d := colorLabDistance(AudienceCategoryColor(c1), AudienceCategoryColor(c2))
			if d < 30.0 {
				t.Fatalf("默认分类 %s(%s) 与 %s(%s) 色彩区分度不足: Lab ΔE=%.1f < 30",
					c1, AudienceCategoryColor(c1), c2, AudienceCategoryColor(c2), d)
			}
		}
	}

	in := AudienceInsights{
		TotalCups:    10,
		TotalVolume:  3250,
		TotalRevenue: 52000,
		TotalCost:    18000,
		Segments: []CategorySegment{
			{Category: "IPA", Color: AudienceCategoryColor("IPA"), Cups: 6, PrevCups: 4, VolumeML: 1980, Revenue: 30000, Cost: 10000, AvgABV: 6.8, HasABV: true, CupShare: 60, RevShare: 57.7, ProfitShare: 58.8},
			{Category: "世涛", Color: AudienceCategoryColor("世涛"), Cups: 4, PrevCups: 0, VolumeML: 1270, Revenue: 22000, Cost: 8000, AvgABV: 8.5, HasABV: true, CupShare: 40, RevShare: 42.3, ProfitShare: 41.2},
		},
		CupSizes: CupSizePreference{SmallCups: 2, MediumCups: 6, LargeCups: 2},
		Spots:    SpotPreference{BarCups: 4, BarTopCat: "IPA", TableCups: 3, TableTopCat: "世涛"},
	}
	if !in.HasData() || in.CanRadar() {
		t.Fatalf("HasData=%v CanRadar=%v", in.HasData(), in.CanRadar())
	}
	if top := in.TopCategory(); top == nil || top.Category != "IPA" || top.CupsChangeText() != "+50.0%" {
		t.Fatalf("TopCategory = %+v", top)
	}
	if high := in.HighestAvgCupCategory(); high == nil || high.Category != "世涛" || high.AvgCupText() != "¥ 55" || high.CupsChangeText() != "+4 杯" {
		t.Fatalf("HighestAvgCupCategory = %+v", high)
	}
	if got := in.CupSizes.DominantLabel(); !strings.Contains(got, "330ml") || !strings.Contains(got, "60.0%") {
		t.Fatalf("DominantLabel = %q", got)
	}
	if got := in.Spots.SummaryText(); !strings.Contains(got, "吧台偏爱 IPA（4 杯）") || !strings.Contains(got, "桌台偏爱 世涛（3 杯）") {
		t.Fatalf("Spots.SummaryText = %q", got)
	}

	if in.TotalProfit() != 34000 || in.Segments[0].ProfitShareText() != "58.8%" {
		t.Fatalf("TotalProfit=%d ProfitShareText=%q", in.TotalProfit(), in.Segments[0].ProfitShareText())
	}

	var pts []AudienceChartPoint
	if err := json.Unmarshal([]byte(in.ChartJSON()), &pts); err != nil || len(pts) != 2 ||
		pts[0].Revenue != 300 || pts[0].Profit != 200 || pts[0].ProfitShare != 58.8 || pts[0].Margin != "66.7%" ||
		pts[1].AvgCup != 55 || pts[1].ProfitShare != 41.2 {
		t.Fatalf("ChartJSON = %s (err=%v)", in.ChartJSON(), err)
	}

	// 含亏损分类时：总毛利与该分类有符号毛利保留负值，但 ProfitShare 保持为 0。
	withLoss := AudienceInsights{
		TotalCups:    11,
		TotalRevenue: 57000,
		TotalCost:    26000,
		Segments: []CategorySegment{
			{Category: "IPA", Cups: 6, Revenue: 30000, Cost: 10000, ProfitShare: 58.8},
			{Category: "世涛", Cups: 4, Revenue: 22000, Cost: 8000, ProfitShare: 41.2},
			{Category: "果泥", Cups: 1, Revenue: 5000, Cost: 8000, ProfitShare: 0},
		},
	}
	if withLoss.TotalProfit() != 31000 || withLoss.Segments[2].Profit() != -3000 || withLoss.Segments[2].ProfitShare != 0 {
		t.Fatalf("亏损分类毛利或占比异常: total=%d seg=%+v", withLoss.TotalProfit(), withLoss.Segments[2])
	}
	var lossPts []AudienceChartPoint
	if err := json.Unmarshal([]byte(withLoss.ChartJSON()), &lossPts); err != nil || len(lossPts) != 3 ||
		lossPts[2].Profit != -30 || lossPts[2].ProfitShare != 0 || lossPts[2].Margin != "-60.0%" {
		t.Fatalf("亏损 ChartJSON = %s (err=%v)", withLoss.ChartJSON(), err)
	}
}

func TestAssignDistinctSegmentColorsAvoidsCollisions(t *testing.T) {
	cats := []string{"IPA", "西打", "世涛", "赛松", "博克", "社交艾尔", "蜂蜜麦酒", "冷萃精酿", "烟熏风味"}
	segsA := make([]CategorySegment, len(cats))
	for i, c := range cats {
		segsA[i] = CategorySegment{Category: c, Cups: int64(20 - i)}
	}
	assignDistinctSegmentColors(segsA)

	colorByCat := make(map[string]string, len(segsA))
	used := make(map[string]string, len(segsA))
	for _, s := range segsA {
		if len(s.Color) != 7 || !strings.HasPrefix(s.Color, "#") {
			t.Fatalf("%s 颜色无效: %q", s.Category, s.Color)
		}
		low := strings.ToLower(s.Color)
		if prev, dup := used[low]; dup {
			t.Fatalf("%s 与 %s 分到了相同颜色 %s", prev, s.Category, s.Color)
		}
		used[low] = s.Category
		colorByCat[s.Category] = s.Color
	}
	for _, def := range []string{"IPA", "西打", "世涛"} {
		if colorByCat[def] != AudienceCategoryColor(def) {
			t.Fatalf("预设分类 %s 颜色应保持主色 %s，实际 %s", def, AudienceCategoryColor(def), colorByCat[def])
		}
	}
	for i := 0; i < len(segsA); i++ {
		for j := i + 1; j < len(segsA); j++ {
			d := colorLabDistance(segsA[i].Color, segsA[j].Color)
			if d < 18.0 {
				t.Fatalf("分类 %s(%s) 与 %s(%s) 颜色太接近: Lab ΔE=%.1f < 18",
					segsA[i].Category, segsA[i].Color, segsA[j].Category, segsA[j].Color, d)
			}
		}
	}

	// 换序后同一批分类应拿到完全一致的稳定颜色。
	segsB := make([]CategorySegment, len(cats))
	for i := range cats {
		segsB[i] = CategorySegment{Category: cats[len(cats)-1-i], Cups: int64(i + 1)}
	}
	assignDistinctSegmentColors(segsB)
	for _, s := range segsB {
		if s.Color != colorByCat[s.Category] {
			t.Fatalf("分类 %s 排序变化后颜色不稳定: 原 %s, 现 %s", s.Category, colorByCat[s.Category], s.Color)
		}
	}
}

func TestLoadAudienceInsightsAndAnalyticsHTTP(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	cfg := settings.NewSettingsStore(pool, "PourOps")
	if err := cfg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddBeerCategory(ctx, "赛松"); err != nil {
		t.Fatal(err)
	}

	ts := func(day, hour, min int) time.Time {
		return time.Date(2026, 4, day, hour, min, 0, 0, platform.ReportZone)
	}

	type beerSpec struct {
		brewery, name, style, cat string
		abv                       float64
		costCents                 int
	}
	specs := []beerSpec{
		{"树屋", "尤利乌斯", "IPA - Hazy", "IPA", 6.8, 120000},
		{"高岩", "苹果西打", "Dry Cider", "西打", 5.0, 90000},
		{"农舍", "经典赛松", "Saison", "赛松", 6.2, 100000},
		{"创始者", "帝国世涛", "Stout - Imperial", "", 9.0, 140000}, // 空分类按 style 推断为 世涛
	}
	kegs := make(map[string]int64, len(specs))
	for _, s := range specs {
		var beerID, kegID int64
		if err := pool.QueryRow(ctx,
			`insert into beers (brewery, name, style, category, abv) values ($1, $2, $3, $4, $5) returning id`,
			s.brewery, s.name, s.style, s.cat, s.abv).Scan(&beerID); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx,
			`insert into kegs (beer_id, capacity_ml, cost_cents, tapped_at) values ($1, 20000, $2, $3) returning id`,
			beerID, s.costCents, ts(1, 7, 0)).Scan(&kegID); err != nil {
			t.Fatal(err)
		}
		kegs[s.name] = kegID
	}

	insertPour := func(beerName string, cups, ml, amt int, at time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`insert into keg_pours (keg_id, cups, ml, amount_cents, poured_at) values ($1, $2, $3, $4, $5)`,
			kegs[beerName], cups, ml, amt, at); err != nil {
			t.Fatal(err)
		}
	}

	// 上期（4月9日营业日）：2 杯 IPA（用来校验环比）
	insertPour("尤利乌斯", 2, 660, 11000, ts(9, 21, 0))
	// 本期边界前（4月10日 06:59 属于4月9日营业日，不计入4月10-11日）
	insertPour("尤利乌斯", 1, 330, 5500, ts(10, 6, 59))

	// 本期（4月10-11日营业日：4月10日 07:00 至 4月12日 07:00）：
	// IPA: 4 杯（含跨午夜 4月12日 01:30）= 1320ml, 220元
	insertPour("尤利乌斯", 3, 990, 16500, ts(10, 22, 0))
	insertPour("尤利乌斯", 1, 330, 5500, ts(12, 1, 30))
	// 西打: 3 杯大杯(500ml) = 1500ml, 120元
	insertPour("苹果西打", 3, 1500, 12000, ts(11, 20, 0))
	// 赛松: 2 杯标准杯(330ml) = 660ml, 96元
	insertPour("经典赛松", 2, 660, 9600, ts(11, 21, 0))
	// 世涛: 1 杯小杯(150ml) = 150ml, 68元（最高均价）
	insertPour("帝国世涛", 1, 150, 6800, ts(11, 22, 0))

	// POS 台位与挂账过滤验证：已结账 bar-1 选 IPA，已结账 table-2 选西打，未结账订单不计入。
	var ordBar, ordTable, ordUnpaid int64
	_ = pool.QueryRow(ctx, `insert into orders (spot, closed_at) values ('bar-1', $1) returning id`, ts(11, 22, 10)).Scan(&ordBar)
	_ = pool.QueryRow(ctx, `insert into orders (spot, closed_at) values ('table-2', $1) returning id`, ts(11, 22, 20)).Scan(&ordTable)
	_ = pool.QueryRow(ctx, `insert into orders (spot) values ('table-3') returning id`).Scan(&ordUnpaid)
	if _, err := pool.Exec(ctx, `insert into order_items (order_id, kind, ref_id, name, unit_cents, ml, qty, paid_at) values
		($1, 'beer', $4, '尤利乌斯', 5500, 330, 3, $5),
		($2, 'beer', $6, '苹果西打', 4000, 500, 2, $5),
		($3, 'beer', $7, '帝国世涛', 6800, 150, 5, null)`,
		ordBar, ordTable, ordUnpaid, kegs["尤利乌斯"], ts(11, 22, 30), kegs["苹果西打"], kegs["帝国世涛"]); err != nil {
		t.Fatal(err)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	start, end := ts(10, 7, 0), ts(12, 7, 0)
	sales, err := LoadSalesInsights(ctx, tx, start, end)
	if err != nil {
		t.Fatal(err)
	}
	aud, err := LoadAudienceInsights(ctx, tx, start, end, cfg.BeerCategories())
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)

	if aud.TotalCups != sales.Draft.Qty || aud.TotalRevenue != sales.Draft.Revenue {
		t.Fatalf("客户群合计 %d 杯 / %d 分 与桶装销售额 %d / %d 对不上", aud.TotalCups, aud.TotalRevenue, sales.Draft.Qty, sales.Draft.Revenue)
	}
	if len(aud.Segments) != 4 || !aud.CanRadar() {
		t.Fatalf("应返回 4 个分类并开启雷达图，实际 len=%d", len(aud.Segments))
	}
	if top := aud.TopCategory(); top == nil || top.Category != "IPA" || top.Cups != 4 || top.PrevCups != 3 || top.VolumeML != 1320 {
		t.Fatalf("主力客群不对: %+v", top)
	}
	if high := aud.HighestAvgCupCategory(); high == nil || high.Category != "世涛" || high.AvgCupCents() != 6800 {
		t.Fatalf("高客单偏好应为 世涛(6800)，实际 %+v", high)
	}
	if aud.Segments[2].Category != "赛松" || aud.Segments[1].Category != "西打" {
		t.Fatalf("分类排序或自定义分类不对: %+v", aud.Segments)
	}
	if aud.CupSizes.SmallCups != 1 || aud.CupSizes.MediumCups != 6 || aud.CupSizes.LargeCups != 3 {
		t.Fatalf("杯型分布不对: %+v", aud.CupSizes)
	}
	var profitShareSum float64
	for _, seg := range aud.Segments {
		profitShareSum += seg.ProfitShare
	}
	if aud.TotalProfit() <= 0 || profitShareSum < 99.9 || profitShareSum > 100.1 {
		t.Fatalf("盈利分类毛利占比之和应接近 100%%: totalProfit=%d sum=%.2f segs=%+v", aud.TotalProfit(), profitShareSum, aud.Segments)
	}
	if aud.Spots.BarCups != 3 || aud.Spots.BarTopCat != "IPA" || aud.Spots.TableCups != 2 || aud.Spots.TableTopCat != "西打" {
		t.Fatalf("POS 台位偏爱不对: %+v", aud.Spots)
	}

	// 验证 /admin/analytics HTTP 页面真实渲染出客户群分析图和 JSON 数据。
	renderer := ui.MustRenderer(web.TemplatesFS, nil, cfg.Brand)
	svc := NewService(pool, renderer, cfg)
	req := httptest.NewRequest(http.MethodGet, "/admin/analytics?start=2026-04-10&end=2026-04-11", nil)
	rec := httptest.NewRecorder()
	svc.Analytics(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/analytics = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"客户群分析", `id="audienceChart"`, `id="audience-data"`, `data-audience-type="radar"`, `data-audience-metric="profit"`, "按毛利", "IPA", "西打", "赛松", "世涛", "吧台偏爱 IPA（3 杯）", "桌台偏爱 西打（2 杯）"} {
		if !strings.Contains(body, want) {
			t.Fatalf("数据分析页面缺少 %q", want)
		}
	}
	if strings.Contains(body, "ZgotmplZ") {
		t.Fatal("页面出现被 html/template 拦截的 ZgotmplZ 样式")
	}
}
