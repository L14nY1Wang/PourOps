package report

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
	"pourops/internal/pos"
	"pourops/internal/settings"
	"pourops/internal/ui"
	"pourops/migrations"
	"pourops/web"
)

func seedBeerKeg(t *testing.T, pool *pgxpool.Pool, brewery, name, style, category string, abv float64, costCents int) int64 {
	t.Helper()
	var beerID, kegID int64
	if err := pool.QueryRow(context.Background(),
		`insert into beers (brewery, name, style, category, abv) values ($1, $2, $3, $4, $5) returning id`,
		brewery, name, style, category, abv).Scan(&beerID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(),
		`insert into kegs (beer_id, capacity_ml, cost_cents, tapped_at) values ($1, 20000, $2, now()) returning id`,
		beerID, costCents).Scan(&kegID); err != nil {
		t.Fatal(err)
	}
	return kegID
}

func insertKegPour(t *testing.T, pool *pgxpool.Pool, kegID int64, cups, ml, amount int, at time.Time) {
	t.Helper()
	execSQL(t, pool, `insert into keg_pours (keg_id, cups, ml, amount_cents, poured_at) values ($1, $2, $3, $4, $5)`,
		kegID, cups, ml, amount, at)
}

// P3-18：概览排除手动停用的有库存商品与软删除商品，但保留库存归零自动下架的售罄告警；LoadOverviewTx 在调用方快照里读，事务外新增的商品不可见。
func TestLoadOverviewTxFiltersAndSnapshot(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()

	low := seedRetail(t, pool, 1000, 300, 2) // 低库存 + 成本 300
	out := seedRetail(t, pool, 1000, 900, 0) // 真实售罄会自动置 active=false
	execSQL(t, pool, `update retail_items set active=false where id=$1`, out)
	off := seedRetail(t, pool, 1000, 500, 4) // 手动停用但有库存
	execSQL(t, pool, `update retail_items set active=false where id=$1`, off)
	archived := seedRetail(t, pool, 1000, 700, 4) // 软删除（有库存）
	execSQL(t, pool, `update retail_items set archived_at=now() where id=$1`, archived)
	archivedOut := seedRetail(t, pool, 1000, 800, 0) // 软删除（已售罄）
	execSQL(t, pool, `update retail_items set active=false, archived_at=now() where id=$1`, archivedOut)

	var beerID int64
	if err := pool.QueryRow(ctx, `insert into beers (brewery, name) values ('测试酒厂', '未上架') returning id`).
		Scan(&beerID); err != nil {
		t.Fatal(err)
	}
	execSQL(t, pool, `insert into kegs (beer_id, capacity_ml, cost_cents) values ($1, 20000, 5000)`, beerID)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	o1, err := LoadOverviewTx(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if o1.RetailCount != 1 || o1.LowCount != 1 || o1.OutCount != 1 { // 仅 low 在架，out 已归零自动下架
		t.Fatalf("在售零售品统计不对: %+v", o1)
	}
	if o1.StockValue != 2*300+5000 {
		t.Fatalf("库存成本 = %d, want %d（停用/软删除不计）", o1.StockValue, 2*300+5000)
	}
	// 告警列表含低库存与自动下架售罄品，但手动停用有库存与软删除商品不能混进来。
	if o1.Untapped != 1 || len(o1.LowStock) != 2 {
		t.Fatalf("低库存列表应只含在架低库存与非归档售罄品: %+v", o1.LowStock)
	}
	seenLow, seenOut := false, false
	for _, it := range o1.LowStock {
		if it.ID == off || it.ID == archived || it.ID == archivedOut {
			t.Fatalf("停用/软删除商品出现在告警里: %+v", it)
		}
		if it.ID == low {
			seenLow = true
		}
		if it.ID == out {
			seenOut = true
		}
	}
	if !seenLow || !seenOut {
		t.Fatalf("低库存或售罄商品没进告警列表: seenLow=%v seenOut=%v list=%+v", seenLow, seenOut, o1.LowStock)
	}

	// 事务外新增一件自动下架的售罄商品，同一个只读快照里不应出现。
	extraOut := seedRetail(t, pool, 1000, 100, 0)
	execSQL(t, pool, `update retail_items set active=false where id=$1`, extraOut)
	o2, err := LoadOverviewTx(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if o2.RetailCount != o1.RetailCount || o2.OutCount != o1.OutCount || o2.StockValue != o1.StockValue {
		t.Fatalf("同一事务快照应稳定: 前 %+v 后 %+v", o1, o2)
	}

	// 包装版自己开事务，结果应与 tx 版一致（此时已能看到新售罄商品）。
	o3, err := LoadOverview(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if o3.OutCount != 2 || o3.RetailCount != 1 {
		t.Fatalf("LoadOverview 包装结果不对: %+v", o3)
	}
}

// P3-19：没记毫升/杯数的记录归入 UnknownCups，杯型合计仍等于总杯数，成本不受影响。
func TestAudienceUnknownCupsKeepTotalAligned(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	keg := seedBeerKeg(t, pool, "测试酒厂", "测试IPA", "IPA - Hazy", "IPA", 6.0, 10000)

	ts := func(day, hour int) time.Time { return time.Date(2026, 5, day, hour, 0, 0, 0, platform.ReportZone) }
	insertKegPour(t, pool, keg, 1, 330, 3000, ts(10, 21))
	insertKegPour(t, pool, keg, 2, 0, 6000, ts(10, 22)) // 老数据没记毫升

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	start, end := ts(10, 7), ts(11, 7)
	aud, err := LoadAudienceInsights(ctx, tx, start, end, platform.DefaultBeerCategories)
	if err != nil {
		t.Fatal(err)
	}
	if aud.TotalCups != 3 || aud.CupSizes.MediumCups != 1 || aud.CupSizes.UnknownCups != 2 {
		t.Fatalf("杯型分布不对: total=%d cups=%+v", aud.TotalCups, aud.CupSizes)
	}
	if aud.CupSizes.Total() != aud.TotalCups {
		t.Fatalf("CupSizes.Total()=%d 应等于 TotalCups=%d", aud.CupSizes.Total(), aud.TotalCups)
	}
	if got := aud.CupSizes.UnknownShareText(); got != "66.7%" {
		t.Fatalf("未记录杯型占比 = %q, want 66.7%%", got)
	}
	if aud.TotalCost != 165 { // 10000 分 / 20000ml × 330ml
		t.Fatalf("总成本 = %d, want 165", aud.TotalCost)
	}
	if aud.TotalRevenue != 9000 {
		t.Fatalf("总营收 = %d, want 9000", aud.TotalRevenue)
	}
}

// P3-19：AudienceInsights 的杯装成本与 SalesInsights.Draft.Cost 同口径，
// 只在「营业日 × 分类」粒度舍入，所以最多差 1 分/分类·日（sales 只对日总成本舍入一次）。
func TestAudienceCostMatchesSalesPourCost(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	ipa := seedBeerKeg(t, pool, "测试酒厂", "测试IPA", "IPA - Hazy", "IPA", 6.0, 10000)
	cider := seedBeerKeg(t, pool, "测试酒厂", "测试西打", "Dry Cider", "西打", 5.0, 10000)

	at := time.Date(2026, 6, 1, 20, 0, 0, 0, platform.ReportZone)
	// 667ml × 0.5 = 333.5 分：按分类舍入得 334，合计舍入得 667，正好暴露粒度差。
	insertKegPour(t, pool, ipa, 1, 667, 5000, at)
	insertKegPour(t, pool, cider, 1, 667, 5000, at)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	start, end := time.Date(2026, 6, 1, 7, 0, 0, 0, platform.ReportZone), time.Date(2026, 6, 2, 7, 0, 0, 0, platform.ReportZone)
	sales, err := LoadSalesInsights(ctx, tx, start, end)
	if err != nil {
		t.Fatal(err)
	}
	aud, err := LoadAudienceInsights(ctx, tx, start, end, platform.DefaultBeerCategories)
	if err != nil {
		t.Fatal(err)
	}
	if aud.TotalCups != sales.Draft.Qty || aud.TotalRevenue != sales.Draft.Revenue {
		t.Fatalf("客户群 %d 杯 / %d 分 与桶装 %d 杯 / %d 分 对不上",
			aud.TotalCups, aud.TotalRevenue, sales.Draft.Qty, sales.Draft.Revenue)
	}
	if diff := math.Abs(float64(aud.TotalCost - sales.Draft.Cost)); diff > 1 {
		t.Fatalf("成本口径偏差 %.0f 分，应 ≤ 1 分/分类·日: audience=%d sales=%d", diff, aud.TotalCost, sales.Draft.Cost)
	}
}

// P3-20：客单价四舍五入（不是截断），amount_cents = 0 的赠送单不计入 Orders。
func TestPOSAverageRoundingAndZeroPayments(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	cst := platform.ReportZone
	start := time.Date(2026, 7, 1, 7, 0, 0, 0, cst)
	end := start.AddDate(0, 0, 1)

	a := newPOSOrder(t, pool, "bar-1")
	newPOSPayment(t, pool, a, 2501, time.Date(2026, 7, 1, 20, 0, 0, 0, cst))
	b := newPOSOrder(t, pool, "bar-2")
	newPOSPayment(t, pool, b, 2500, time.Date(2026, 7, 1, 21, 0, 0, 0, cst))
	zero := newPOSOrder(t, pool, "bar-3")
	newPOSPayment(t, pool, zero, 0, time.Date(2026, 7, 1, 22, 0, 0, 0, cst)) // 赠送单

	in := loadPOSInsightsSnapshot(t, pool, start, end)
	if in.Received != 5001 || in.Orders != 2 || in.Payments != 3 {
		t.Fatalf("汇总不对: received=%d orders=%d payments=%d", in.Received, in.Orders, in.Payments)
	}
	if got := in.AverageText(); got != "¥ 25.01" { // 50.01 / 2 = 25.005 → 25.01
		t.Fatalf("客单价 = %q, want ¥ 25.01", got)
	}
}

// P3-21：自定义区间拒绝 2000 年以前（SQL 的 UTC+8 推算在远古年份没有意义）。
func TestReportDatesRejectsPre2000(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, platform.ReportZone)
	for _, tc := range []struct{ from, to string }{
		{"1999-12-31", "2000-01-01"},
		{"2000-01-01", "1999-12-31"},
		{"0123-01-01", "2000-01-02"},
	} {
		if _, _, _, problem := ReportDates(now, "custom", tc.from, tc.to); problem == "" {
			t.Fatalf("%s ~ %s 应被拒绝", tc.from, tc.to)
		}
	}
	period, start, end, problem := ReportDates(now, "custom", "2000-01-01", "2000-01-02")
	if problem != "" || period != "custom" {
		t.Fatalf("合法区间被拒: period=%q problem=%q", period, problem)
	}
	want := time.Date(2000, 1, 1, 7, 0, 0, 0, platform.ReportZone)
	if !start.Equal(want) || !end.Equal(want.AddDate(0, 0, 2)) {
		t.Fatalf("边界 = %v ~ %v, want %v ~ %v", start, end, want, want.AddDate(0, 0, 2))
	}
}

// P3-21：1991 年中国夏令时（UTC+9）期间，营业日边界必须仍按固定 UTC+8 与 Go 的 platform.ReportZone 一致。
func TestSalesBusinessDayFixedUTC8BeforeDSTAbolished(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	keg := seedBeerKeg(t, pool, "测试酒厂", "老酒", "IPA - Hazy", "IPA", 6.0, 10000)

	at := time.Date(1991, 7, 1, 6, 30, 0, 0, platform.ReportZone) // 北京 06:30，属 06-30 营业日
	insertKegPour(t, pool, keg, 1, 330, 3300, at)

	start := time.Date(1991, 6, 30, 7, 0, 0, 0, platform.ReportZone)
	sales, err := LoadSalesInsights(context.Background(), beginTestTx(t, pool), start, start.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := platform.BusinessDayKey(at); got != "1991-06-30" {
		t.Fatalf("Go 营业日 = %q, want 1991-06-30", got)
	}
	if len(sales.Trend) != 1 || sales.Trend[0].Date != "1991-06-30" {
		t.Fatalf("SQL 营业日 = %+v, want 1991-06-30（Asia/Shanghai 夏令时会给成 07-01）", sales.Trend)
	}
	if sales.Current.Revenue != 3300 {
		t.Fatalf("营收 = %d, want 3300", sales.Current.Revenue)
	}
}

// P3-21：跨年边界 2027-01-01 06:59 属 2026-12-31 营业日，07:00 起才归 2027-01-01。
func TestSalesBusinessDayCrossYearBoundary(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	keg := seedBeerKeg(t, pool, "测试酒厂", "跨年酒", "IPA - Hazy", "IPA", 6.0, 10000)

	before := time.Date(2027, 1, 1, 6, 59, 0, 0, platform.ReportZone)
	after := time.Date(2027, 1, 1, 7, 0, 0, 0, platform.ReportZone)
	insertKegPour(t, pool, keg, 1, 330, 3000, before)
	insertKegPour(t, pool, keg, 1, 330, 4000, after)

	start := time.Date(2026, 12, 31, 7, 0, 0, 0, platform.ReportZone)
	tx := beginTestTx(t, pool)
	first, err := LoadSalesInsights(context.Background(), tx, start, start.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Trend) != 1 || first.Trend[0].Date != "2026-12-31" || first.Current.Revenue != 3000 {
		t.Fatalf("跨年夜营业日不对: %+v", first.Trend)
	}
	second, err := LoadSalesInsights(context.Background(), tx, start.AddDate(0, 0, 1), start.AddDate(0, 0, 2))
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Trend) != 1 || second.Trend[0].Date != "2027-01-01" || second.Current.Revenue != 4000 {
		t.Fatalf("元旦营业日不对: %+v", second.Trend)
	}
}

// beginTestTx 开一个只读快照事务，测试结束自动回滚。
func beginTestTx(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.BeginTx(context.Background(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// P3-19/20：真实走 pos.PayItemsWithGateway 只结一部分商品时，销售营收与 POS 实收必须一致。
func TestPartialCheckoutReconcilesSalesAndPOS(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()

	keg := seedKeg(t, pool, 4500)
	retail := seedRetail(t, pool, 3000, 1000, 5)
	orderID, err := pos.EnsureOrder(ctx, pool, "bar-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := pos.AddOrderItem(ctx, pool, orderID, "beer", keg, "测试酒厂 · 测试IPA", 4500, 330); err != nil {
		t.Fatal(err)
	}
	if err := pos.AddOrderItem(ctx, pool, orderID, "retail", retail, "薯片 原味", 3000, 0); err != nil {
		t.Fatal(err)
	}

	var beerLine int64
	if err := pool.QueryRow(ctx, `select id from order_items where order_id=$1 and kind='beer'`, orderID).
		Scan(&beerLine); err != nil {
		t.Fatal(err)
	}
	res, err := pos.PayItemsWithGateway(ctx, pool, pos.ManualGateway{}, orderID, []int64{beerLine})
	if err != nil {
		t.Fatal(err)
	}
	if res.AmountCents != 4500 {
		t.Fatalf("本次收款 = %d, want 4500（只结了啤酒）", res.AmountCents)
	}

	start := platform.BusinessDayStart(time.Now())
	tx := beginTestTx(t, pool)
	sales, err := LoadSalesInsights(ctx, tx, start, start.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	posIn, err := LoadPOSInsights(ctx, tx, start, start.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if sales.Current.Revenue != 4500 || posIn.Received != 4500 {
		t.Fatalf("部分结账后 Sales.Revenue=%d POS.Received=%d, want 4500/4500", sales.Current.Revenue, posIn.Received)
	}
	if sales.Draft.Qty != 1 || posIn.Orders != 1 || posIn.Payments != 1 {
		t.Fatalf("杯数/订单数不对: qty=%d orders=%d payments=%d", sales.Draft.Qty, posIn.Orders, posIn.Payments)
	}
}

// P3-14：大额订单（单价 × 数量超过 int32）在列表和详情里都不能溢出。
func TestRecordOrdersInt64LargeAmounts(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()

	orderID := newPOSOrder(t, pool, "bar-1")
	execSQL(t, pool, `insert into order_items (order_id, kind, ref_id, name, unit_cents, qty)
		values ($1, 'retail', 1, '整箱', 2000000000, 2)`, orderID)

	orders, _, err := ListRecordOrdersCursor(ctx, pool, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 {
		t.Fatalf("应返回 1 张单，实际 %d", len(orders))
	}
	const want = int64(4_000_000_000)
	if orders[0].Total != want || orders[0].Pending != want || orders[0].Paid != 0 {
		t.Fatalf("订单金额溢出: total=%d pending=%d paid=%d", orders[0].Total, orders[0].Pending, orders[0].Paid)
	}
	if got := orders[0].TotalText(); got != "¥ 40000000" {
		t.Fatalf("合计展示 = %q", got)
	}

	o, _, _, _, err := LoadRecordDetail(ctx, pool, orderID)
	if err != nil {
		t.Fatal(err)
	}
	if o.Total != want {
		t.Fatalf("详情合计 = %d, want %d", o.Total, want)
	}
}

// P3-23：客户端断开（context.Canceled）不写日志也不回 500，页面静默放弃。
func TestHandlersSilentOnContextCanceled(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	svc := NewService(pool, ui.MustRenderer(web.TemplatesFS, nil, func() string { return "PourOps" }),
		settings.NewSettingsStore(nil, "PourOps"))

	canceled := func() *http.Request {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return httptest.NewRequest(http.MethodGet, "/admin/report", nil).WithContext(ctx)
	}
	cases := []struct {
		name string
		fn   http.HandlerFunc
		path string
	}{
		{"dashboard", svc.Dashboard, "/admin"},
		{"analytics", svc.Analytics, "/admin/analytics"},
		{"records-orders", svc.RecordsOrders, "/admin/records"},
		{"records-detail", func(w http.ResponseWriter, r *http.Request) {
			r.SetPathValue("id", "1")
			svc.RecordsOrderDetail(w, r)
		}, "/admin/records/orders/1"},
		{"records-operations", svc.RecordsOperations, "/admin/records/operations"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := canceled()
		req.URL.Path = tc.path
		tc.fn(rec, req)
		if rec.Body.Len() != 0 || rec.Code != http.StatusOK {
			t.Fatalf("%s 取消后应静默返回，实际 status=%d body=%q", tc.name, rec.Code, rec.Body.String())
		}
	}
}

// P3-19：只记录了未知杯型时，主导杯型不能伪造一个 0 杯的 330ml 标准杯。
func TestCupSizeDominantLabelUnknownOnly(t *testing.T) {
	if got := (CupSizePreference{UnknownCups: 3}).DominantLabel(); got != "未记录杯型" {
		t.Fatalf("DominantLabel = %q, want 未记录杯型", got)
	}
}

// P3-19：同一天同一分类的两款酒，成本必须先按 (day, cat) 合计再舍入，才能与 SalesInsights 对齐。
func TestAudienceCostRoundsPerDayCategory(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	a := seedBeerKeg(t, pool, "测试酒厂", "IPA甲", "IPA - Hazy", "IPA", 6.0, 10000)
	b := seedBeerKeg(t, pool, "测试酒厂", "IPA乙", "IPA - Hazy", "IPA", 6.0, 10000)

	at := time.Date(2026, 6, 2, 20, 0, 0, 0, platform.ReportZone)
	// 667ml × 0.5 = 333.5 分，两款合计 667；逐行舍入会得到 334+334=668。
	insertKegPour(t, pool, a, 1, 667, 5000, at)
	insertKegPour(t, pool, b, 1, 667, 5000, at)

	start := time.Date(2026, 6, 2, 7, 0, 0, 0, platform.ReportZone)
	tx := beginTestTx(t, pool)
	sales, err := LoadSalesInsights(context.Background(), tx, start, start.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	aud, err := LoadAudienceInsights(context.Background(), tx, start, start.AddDate(0, 0, 1), platform.DefaultBeerCategories)
	if err != nil {
		t.Fatal(err)
	}
	if aud.TotalCost != 667 {
		t.Fatalf("客群成本 = %d, want 667（按营业日×分类合计后舍入）", aud.TotalCost)
	}
	if aud.TotalCost != sales.Draft.Cost {
		t.Fatalf("客群成本 %d 与桶装成本 %d 不一致", aud.TotalCost, sales.Draft.Cost)
	}
}

func TestRecordsIgnoreOverflowPageQuery(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	svc := NewService(pool, ui.MustRenderer(web.TemplatesFS, nil, func() string { return "PourOps" }),
		settings.NewSettingsStore(nil, "PourOps"))

	for _, tc := range []struct {
		fn   http.HandlerFunc
		path string
	}{
		{svc.RecordsOrders, "/admin/records?page=9223372036854775807"},
		{svc.RecordsOperations, "/admin/records/operations?page=9223372036854775807"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		tc.fn(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 应忽略旧版 page 参数正常返回 200，实际 %d: %s", tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestDashboardFlashErrDoesNotHideSalesAndInvalidPeriodReturns400(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	svc := NewService(pool, ui.MustRenderer(web.TemplatesFS, nil, func() string { return "PourOps" }),
		settings.NewSettingsStore(nil, "PourOps"))

	flashRec := httptest.NewRecorder()
	platform.RedirectErr(flashRec, httptest.NewRequest(http.MethodPost, "/admin/", nil), "/admin/", "跨页操作提示")

	reqFlash := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	for _, c := range flashRec.Result().Cookies() {
		reqFlash.AddCookie(c)
	}
	recFlash := httptest.NewRecorder()
	svc.Dashboard(recFlash, reqFlash)
	if recFlash.Code != http.StatusOK {
		t.Fatalf("合法日期带 flash Err 应返回 200，实际 %d", recFlash.Code)
	}
	bodyFlash := recFlash.Body.String()
	if !strings.Contains(bodyFlash, "跨页操作提示") {
		t.Fatalf("概览页应展示 flash Err 文案")
	}
	if !strings.Contains(bodyFlash, "销售事实") {
		t.Fatalf("flash Err 不应隐藏概览页销售事实区块")
	}

	reqBad := httptest.NewRequest(http.MethodGet, "/admin/?period=custom&start=bad&end=bad", nil)
	recBad := httptest.NewRecorder()
	svc.Dashboard(recBad, reqBad)
	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("非法日期筛选应返回 400，实际 %d", recBad.Code)
	}
	if strings.Contains(recBad.Body.String(), "销售事实") {
		t.Fatalf("非法日期筛选应隐藏销售事实区块")
	}
}

func TestSalesEmptyBreweryTopAndPreGroupedRetailQty(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	kegID := seedBeerKeg(t, pool, "", "无厂牌裸酒", "Lager", "拉格", 4.5, 80000)
	at := time.Date(2026, 6, 4, 21, 0, 0, 0, platform.ReportZone)
	insertKegPour(t, pool, kegID, 2, 660, 7000, at)

	var saleID int64
	if err := pool.QueryRow(ctx, `insert into retail_sales (total_cents, cost_cents, created_at) values (6000, 2000, $1) returning id`, at).Scan(&saleID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into retail_sale_items (sale_id, name, qty, price_cents, cost_cents) values
		($1, '花生', 2, 1500, 500),
		($1, '薯片', 3, 1000, 333)`, saleID); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 6, 4, 7, 0, 0, 0, platform.ReportZone)
	tx := beginTestTx(t, pool)
	sales, err := LoadSalesInsights(ctx, tx, start, start.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if sales.Retail.Qty != 5 {
		t.Fatalf("Retail.Qty = %d, want 5", sales.Retail.Qty)
	}
	foundEmptyBrewery := false
	for _, top := range sales.Top {
		if strings.Contains(top.Name, "无厂牌裸酒") {
			foundEmptyBrewery = true
			if top.Name != "无厂牌裸酒" {
				t.Fatalf("empty brewery should not leave leading dot, got %q", top.Name)
			}
		}
	}
	if !foundEmptyBrewery {
		t.Fatalf("expected 无厂牌裸酒 in sales.Top, got %+v", sales.Top)
	}
}
