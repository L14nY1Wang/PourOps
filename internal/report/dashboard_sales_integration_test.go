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

// salesSeed 造一组跨边界的数据：桶成本 10001 分 / 20000ml，单毫升成本是
// 0.50005，两笔 333ml 按日先加总再舍入得 333 分，逐笔舍入会得 334——用来钉住
// 「按日分品类舍入」的口径。
func salesSeed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	cst := time.FixedZone("UTC+8", 8*60*60)
	ts := func(month, day, hour, min, sec int) time.Time {
		return time.Date(2026, time.Month(month), day, hour, min, sec, 0, cst)
	}

	var beerID, kegID int64
	if err := pool.QueryRow(ctx,
		`insert into beers (brewery, name, style) values ('测试厂', '边界IPA', 'IPA') returning id`).Scan(&beerID); err != nil {
		t.Fatalf("建酒款失败: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`insert into kegs (beer_id, capacity_ml, cost_cents, tapped_at) values ($1, 20000, 10001, $2) returning id`,
		beerID, ts(2, 1, 0, 0, 0)).Scan(&kegID); err != nil {
		t.Fatalf("建酒桶失败: %v", err)
	}
	pour := func(at time.Time, ml, amount int) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`insert into keg_pours (keg_id, cups, ml, amount_cents, poured_at) values ($1, 1, $2, $3, $4)`,
			kegID, ml, amount, at); err != nil {
			t.Fatalf("记桶装售出失败: %v", err)
		}
	}

	pour(ts(2, 26, 23, 59, 59), 100, 900)
	pour(ts(2, 28, 0, 0, 0), 1000, 10000)
	pour(ts(3, 1, 12, 0, 0), 500, 5000)
	pour(ts(3, 2, 0, 0, 0), 333, 3000)
	pour(ts(3, 2, 23, 59, 59), 333, 3000)
	pour(ts(3, 3, 12, 0, 0), 700, 7000)
	pour(ts(3, 4, 0, 0, 0), 100, 1000)
	pour(ts(3, 10, 12, 0, 0), 200, 2500)
	pour(ts(3, 20, 12, 0, 0), 100, 1100)

	var itemX, itemY int64
	if err := pool.QueryRow(ctx,
		`insert into retail_items (name, spec, price_cents, cost_cents, stock) values ('测试罐装', '330ml', 600, 350, 100) returning id`).Scan(&itemX); err != nil {
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

	sale(ts(2, 26, 23, 59, 59), 999, 500, line{itemX, "测试罐装", 1, 999, 500})
	sale(ts(3, 1, 10, 0, 0), 600, 350, line{itemX, "测试罐装", 1, 600, 350})
	sale(ts(3, 2, 23, 0, 0), 1200, 700, line{itemX, "测试罐装", 2, 600, 350})
	sale(ts(3, 3, 12, 0, 0), 500, 300, line{itemX, "测试罐装·新名", 1, 500, 300})
	sale(ts(3, 3, 18, 0, 0), 400, 200, line{itemY, "测试薯片", 1, 400, 200})
	sale(ts(3, 4, 0, 0, 0), 888, 400, line{itemY, "测试薯片", 1, 888, 400})
}

func TestLoadSalesInsightsAndDashboardHTTP(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	salesSeed(t, pool)

	cst := time.FixedZone("UTC+8", 8*60*60)
	at := func(day, hour int) time.Time { return time.Date(2026, 3, day, hour, 0, 0, 0, cst) }

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

	in := insights(at(2, 0), at(4, 0))

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

	in = insights(at(8, 0), at(11, 0))
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

	in = insights(at(12, 0), at(13, 0))
	if in.Current.Revenue != 0 || in.Previous.Revenue != 0 || len(in.Top) != 0 || len(in.Trend) != 1 {
		t.Fatalf("空窗口 = %+v", in)
	}
	if in.RevenueChangeText() != "—" || in.ProfitChangeText() != "—" || in.Current.MarginText() != "—" {
		t.Fatalf("空窗口文本 = %q/%q/%q", in.RevenueChangeText(), in.ProfitChangeText(), in.Current.MarginText())
	}

	// 验证 Overview 与 Dashboard HTTP handler
	ov, err := LoadOverview(ctx, pool)
	if err != nil || ov.ActiveKegs != 1 {
		t.Fatalf("LoadOverview = %+v, err=%v", ov, err)
	}
	renderer := ui.MustRenderer(web.TemplatesFS, nil, func() string { return "PourOps" })
	svc := NewService(pool, renderer, settings.NewSettingsStore(pool, "PourOps"))
	rec := httptest.NewRecorder()
	svc.Dashboard(rec, httptest.NewRequest(http.MethodGet, "/admin/?period=custom&start=2026-03-02&end=2026-03-03", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "边界IPA") {
		t.Fatalf("Dashboard HTTP 响应不对: code=%d", rec.Code)
	}
}
