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
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/internal/ui"
	"pourops/migrations"
	"pourops/web"
)

// salesSeed 造一组跨营业日 07:00 边界的数据：桶成本 10001 分 / 20000ml，单毫升成本是
// 0.50005，两笔 333ml 按营业日先加总再舍入得 333 分，逐笔舍入会得 334——用来钉住
// 「按营业日（07:00–次日 07:00）分品类舍入」的口径。
func salesSeed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	ts := func(month, day, hour, min, sec int) time.Time {
		return time.Date(2026, time.Month(month), day, hour, min, sec, 0, platform.ReportZone)
	}

	var beerID, kegID int64
	if err := pool.QueryRow(ctx,
		`insert into beers (brewery, name, style) values ('测试厂', '边界IPA', 'IPA') returning id`).Scan(&beerID); err != nil {
		t.Fatalf("建酒款失败: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`insert into kegs (beer_id, capacity_ml, cost_cents, tapped_at) values ($1, 20000, 10001, $2) returning id`,
		beerID, ts(2, 1, 7, 0, 0)).Scan(&kegID); err != nil {
		t.Fatalf("建酒桶失败: %v", err)
	}
	pourOn := func(kID int64, at time.Time, ml, amount int) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`insert into keg_pours (keg_id, cups, ml, amount_cents, poured_at) values ($1, 1, $2, $3, $4)`,
			kID, ml, amount, at); err != nil {
			t.Fatalf("记桶装售出失败: %v", err)
		}
	}
	pour := func(at time.Time, ml, amount int) {
		pourOn(kegID, at, ml, amount)
	}

	pour(ts(2, 28, 6, 59, 59), 100, 900)  // 归 02-27 营业日（上期之前）
	pour(ts(2, 28, 7, 0, 0), 1000, 10000) // 归 02-28 营业日
	pour(ts(3, 1, 12, 0, 0), 500, 5000)   // 归 03-01 营业日
	pour(ts(3, 2, 7, 0, 0), 333, 3000)    // 归 03-02 营业日
	pour(ts(3, 3, 2, 30, 0), 333, 3000)   // 次日凌晨 2:30 仍归 03-02 营业日
	pour(ts(3, 3, 12, 0, 0), 700, 7000)   // 归 03-03 营业日
	pour(ts(3, 4, 7, 0, 0), 100, 1000)    // 03-04 07:00 切日，不计入 03-03
	pour(ts(3, 10, 12, 0, 0), 200, 2500)
	pour(ts(3, 20, 12, 0, 0), 100, 1100)

	// 另建一桶在售且售出超过 50%（13000 / 20000 = 65%），测试库存告警聚合（流水放在 4 月不影响 3 月区间）。
	var warnBeerID, warnKegID int64
	if err := pool.QueryRow(ctx,
		`insert into beers (no, brewery, name, style) values ('08', '预警厂', '预警世涛', 'Stout') returning id`).Scan(&warnBeerID); err != nil {
		t.Fatalf("建告警酒款失败: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`insert into kegs (beer_id, keg_no, capacity_ml, cost_cents, tapped_at) values ($1, 'K-WARN', 20000, 80000, $2) returning id`,
		warnBeerID, ts(4, 1, 12, 0, 0)).Scan(&warnKegID); err != nil {
		t.Fatalf("建告警酒桶失败: %v", err)
	}
	pourOn(warnKegID, ts(4, 1, 20, 0, 0), 13000, 90000)

	// 未开桶和已结桶不应进桶装在售告警（即使历史售出超 50%）。
	if _, err := pool.Exec(ctx,
		`insert into kegs (beer_id, capacity_ml, cost_cents, tapped_at, finished_at) values ($1, 20000, 80000, $2, $3)`,
		warnBeerID, ts(1, 1, 12, 0, 0), ts(1, 5, 12, 0, 0)); err != nil {
		t.Fatalf("建已结桶失败: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`insert into kegs (beer_id, capacity_ml, cost_cents, placed_at) values ($1, 20000, 80000, $2)`,
		warnBeerID, ts(4, 2, 12, 0, 0)); err != nil {
		t.Fatalf("建未开桶失败: %v", err)
	}

	var itemX, itemY int64
	if err := pool.QueryRow(ctx,
		`insert into retail_items (name, spec, price_cents, cost_cents, stock) values ('测试罐装', '330ml', 600, 350, 2) returning id`).Scan(&itemX); err != nil {
		t.Fatalf("建零售商品失败: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`insert into retail_items (name, spec, price_cents, cost_cents, stock) values ('测试薯片', '40g', 400, 200, 100) returning id`).Scan(&itemY); err != nil {
		t.Fatalf("建零售商品失败: %v", err)
	}

	type line struct {
		itemID int64
		name   string
		qty    int
		price  int
		cost   int
	}
	sale := func(at time.Time, total, cost int, lines ...line) {
		t.Helper()
		var saleID int64
		if err := pool.QueryRow(ctx,
			`insert into retail_sales (total_cents, cost_cents, created_at) values ($1, $2, $3) returning id`,
			total, cost, at).Scan(&saleID); err != nil {
			t.Fatalf("建零售订单失败: %v", err)
		}
		for _, l := range lines {
			if _, err := pool.Exec(ctx,
				`insert into retail_sale_items (sale_id, item_id, name, qty, price_cents, cost_cents)
				 values ($1, $2, $3, $4, $5, $6)`,
				saleID, l.itemID, l.name, l.qty, l.price, l.cost); err != nil {
				t.Fatalf("建零售明细失败: %v", err)
			}
		}
	}

	sale(ts(2, 28, 6, 59, 59), 999, 500, line{itemX, "测试罐装", 1, 999, 500}) // 02-27 营业日（上期之前）
	sale(ts(3, 1, 10, 0, 0), 600, 350, line{itemX, "测试罐装", 1, 600, 350})   // 03-01 营业日
	sale(ts(3, 2, 23, 0, 0), 1200, 700, line{itemX, "测试罐装", 2, 600, 350})  // 03-02 营业日
	sale(ts(3, 3, 12, 0, 0), 500, 300, line{itemX, "测试罐装·新名", 1, 500, 300})
	sale(ts(3, 4, 6, 59, 59), 400, 200, line{itemY, "测试薯片", 1, 400, 200}) // 早 06:59:59 归 03-03 营业日
	sale(ts(3, 4, 7, 0, 0), 888, 400, line{itemY, "测试薯片", 1, 888, 400})   // 早 07:00:00 归 03-04 营业日
}

func TestLoadSalesInsightsAndDashboardHTTP(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	salesSeed(t, pool)

	at := func(day, hour int) time.Time { return time.Date(2026, 3, day, hour, 0, 0, 0, platform.ReportZone) }

	insights := func(start, end time.Time) SalesInsights {
		t.Helper()
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatalf("开只读快照失败: %v", err)
		}
		defer tx.Rollback(ctx)
		in, err := LoadSalesInsights(ctx, tx, start, end)
		if err != nil {
			t.Fatalf("读销售概览失败: %v", err)
		}
		return in
	}

	in := insights(at(2, 7), at(4, 7))

	if in.Draft.Revenue != 13000 || in.Draft.Cost != 683 || in.Draft.Qty != 3 {
		t.Fatalf("桶装本期 营收/成本/数量 = %d/%d/%d，应为 13000/683/3",
			in.Draft.Revenue, in.Draft.Cost, in.Draft.Qty)
	}
	if in.Retail.Revenue != 2100 || in.Retail.Cost != 1200 || in.Retail.Qty != 4 {
		t.Fatalf("零售本期 营收/成本/数量 = %d/%d/%d，应为 2100/1200/4",
			in.Retail.Revenue, in.Retail.Cost, in.Retail.Qty)
	}
	if in.Current.Revenue != 15100 || in.Current.Cost != 1883 || in.Current.Qty != 7 {
		t.Fatalf("本期合计 营收/成本/数量 = %d/%d/%d，应为 15100/1883/7",
			in.Current.Revenue, in.Current.Cost, in.Current.Qty)
	}

	if in.Current.RevenueText() != "¥ 151" || in.Current.CostText() != "¥ 18.83" ||
		in.Current.ProfitText() != "¥ 132.17" || in.Current.MarginText() != "87.5%" {
		t.Fatalf("本期文本 = %q/%q/%q/%q",
			in.Current.RevenueText(), in.Current.CostText(), in.Current.ProfitText(), in.Current.MarginText())
	}
	if in.RevenueChangeText() != "-3.2%" || in.ProfitChangeText() != "-8.8%" {
		t.Fatalf("环比文本 = %q/%q，应为 -3.2%%/-8.8%%", in.RevenueChangeText(), in.ProfitChangeText())
	}

	if in.Previous.Revenue != 15600 || in.Previous.Cost != 1100 || in.Previous.Qty != 3 {
		t.Fatalf("上期 营收/成本/数量 = %d/%d/%d，应为 15600/1100/3",
			in.Previous.Revenue, in.Previous.Cost, in.Previous.Qty)
	}

	if len(in.Trend) != 2 || in.Trend[0].Date != "2026-03-02" || in.Trend[0].Revenue != 7200 ||
		in.Trend[0].Cost != 1033 || in.Trend[1].Date != "2026-03-03" || in.Trend[1].Revenue != 7900 ||
		in.Trend[1].Cost != 850 {
		t.Fatalf("日趋势 = %+v，应为 03-02 7200/1033、03-03 7900/850", in.Trend)
	}
	var trendRev, trendCost int64
	for _, d := range in.Trend {
		trendRev += d.Revenue
		trendCost += d.Cost
	}
	if trendRev != in.Current.Revenue || trendCost != in.Current.Cost {
		t.Fatalf("趋势合计 %d/%d 与 KPI %d/%d 对不上", trendRev, trendCost, in.Current.Revenue, in.Current.Cost)
	}

	if len(in.Top) != 3 {
		t.Fatalf("Top 行数 = %d，应为 3: %+v", len(in.Top), in.Top)
	}
	if top := in.Top[0]; top.Name != "测试厂 · 边界IPA" || top.Unit != "杯" || top.Qty != 3 || top.Revenue != 13000 || top.Cost != 683 {
		t.Fatalf("Top[0] = %+v", top)
	}
	if top := in.Top[1]; top.Name != "测试罐装·新名" || top.Unit != "件" || top.Qty != 3 || top.Revenue != 1700 || top.Cost != 1000 {
		t.Fatalf("同 ID 应合并且用最近快照名，Top[1] = %+v", top)
	}
	if top := in.Top[2]; top.Name != "测试薯片" || top.Unit != "件" || top.Qty != 1 || top.Revenue != 400 || top.Cost != 200 {
		t.Fatalf("Top[2] = %+v", top)
	}

	var points []SalesTrendPoint
	if err := json.Unmarshal([]byte(in.TrendJSON()), &points); err != nil {
		t.Fatalf("TrendJSON 不是合法 JSON: %v", err)
	}
	if len(points) != 2 || points[0].Date != "2026-03-02" || points[0].Revenue != 72 || points[0].Profit != 61.67 {
		t.Fatalf("TrendJSON = %+v", points)
	}

	in = insights(at(8, 7), at(11, 7))
	if len(in.Trend) != 3 || in.Trend[0].Revenue != 0 || in.Trend[0].Cost != 0 ||
		in.Trend[1].Revenue != 0 || in.Trend[2].Date != "2026-03-10" ||
		in.Trend[2].Revenue != 2500 || in.Trend[2].Cost != 100 {
		t.Fatalf("补零趋势 = %+v", in.Trend)
	}
	if in.Current.Revenue != 2500 || in.Previous.Revenue != 0 {
		t.Fatalf("空日窗口 本期/上期 = %d/%d", in.Current.Revenue, in.Previous.Revenue)
	}
	if in.RevenueChangeText() != "+¥ 25" || in.ProfitChangeText() != "+¥ 24" || in.Current.MarginText() != "96.0%" {
		t.Fatalf("无上期文本 = %q/%q/%q", in.RevenueChangeText(), in.ProfitChangeText(), in.Current.MarginText())
	}

	in = insights(at(12, 7), at(13, 7))
	if in.Current.Revenue != 0 || in.Previous.Revenue != 0 || len(in.Top) != 0 || len(in.Trend) != 1 {
		t.Fatalf("空窗口 = %+v", in)
	}
	if in.RevenueChangeText() != "—" || in.ProfitChangeText() != "—" || in.Current.MarginText() != "—" {
		t.Fatalf("空窗口文本 = %q/%q/%q", in.RevenueChangeText(), in.ProfitChangeText(), in.Current.MarginText())
	}

	// 验证 Overview 合并告警（含在售超 50% 酒桶与零售低库存）与 Dashboard / Analytics HTTP handler
	ov, err := LoadOverview(ctx, pool)
	if err != nil || ov.ActiveKegs != 2 || ov.WarnKegCount != 1 || len(ov.WarnKegs) != 1 || ov.WarnKegs[0].SoldPct() != 65 || ov.LowCount != 1 {
		t.Fatalf("LoadOverview = %+v, err=%v", ov, err)
	}
	renderer := ui.MustRenderer(web.TemplatesFS, nil, func() string { return "PourOps" })
	svc := NewService(pool, renderer, settings.NewSettingsStore(pool, "PourOps"))

	rec := httptest.NewRecorder()
	svc.Dashboard(rec, httptest.NewRequest(http.MethodGet, "/admin/?period=custom&start=2026-03-02&end=2026-03-03", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK ||
		!strings.Contains(body, "库存状态与告警") ||
		!strings.Contains(body, "已售 &gt; 50%") ||
		!strings.Contains(body, "预警世涛") ||
		!strings.Contains(body, "测试罐装") ||
		!strings.Contains(body, "边界IPA") {
		t.Fatalf("Dashboard HTTP 响应不对: code=%d", rec.Code)
	}
	if strings.Contains(body, "在售的酒桶") || strings.Contains(body, `id="salesChart"`) {
		t.Fatalf("Dashboard 不应再包含在售酒桶模块或销售走势图")
	}

	recA := httptest.NewRecorder()
	svc.Analytics(recA, httptest.NewRequest(http.MethodGet, "/admin/analytics?period=custom&start=2026-03-02&end=2026-03-03", nil))
	bodyA := recA.Body.String()
	if recA.Code != http.StatusOK ||
		!strings.Contains(bodyA, "销售走势") ||
		!strings.Contains(bodyA, `id="salesChart"`) ||
		!strings.Contains(bodyA, "按日销售明细") ||
		!strings.Contains(bodyA, "2026-03-02") ||
		!strings.Contains(bodyA, "¥ 151") {
		t.Fatalf("Analytics HTTP 响应不对: code=%d", recA.Code)
	}

	recBad := httptest.NewRecorder()
	svc.Analytics(recBad, httptest.NewRequest(http.MethodGet, "/admin/analytics?period=custom&start=2026-03-05&end=2026-03-01", nil))
	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("Analytics 非法日期应返回 400，实际 %d", recBad.Code)
	}
}
