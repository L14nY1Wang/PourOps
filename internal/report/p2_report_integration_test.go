package report

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"pourops/internal/inventory"
	"pourops/internal/platform"
	"pourops/internal/pos"
	"pourops/internal/settings"
	"pourops/internal/ui"
	"pourops/migrations"
	"pourops/web"
)

func TestReportP2ErrorSanitizationAndTimezoneUTC8(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	t.Setenv("TZ", "UTC")

	ctx := platform.WithActor(context.Background(), "tester")
	ordID, err := pos.EnsureOrder(ctx, pool, "bar-1")
	if err != nil {
		t.Fatal(err)
	}
	itemID := seedRetail(t, pool, 3000, 1000, 5)
	if err := pos.AddOrderItem(ctx, pool, ordID, "retail", itemID, "薯片", 3000, 0); err != nil {
		t.Fatal(err)
	}
	var lineID int64
	if err := pool.QueryRow(ctx, `select id from order_items where order_id=$1`, ordID).Scan(&lineID); err != nil {
		t.Fatal(err)
	}
	if _, err := pos.PayItems(ctx, pool, ordID, []int64{lineID}); err != nil {
		t.Fatal(err)
	}

	// UTC 18:30 = 北京次日 02:30，强设 TZ=UTC 时记录页仍必须格式化成北京时间
	fixed := time.Date(2026, 5, 1, 18, 30, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `update orders set opened_at=$2, closed_at=$2 where id=$1`, ordID, fixed); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update payments set created_at=$2 where order_id=$1`, ordID, fixed); err != nil {
		t.Fatal(err)
	}
	evTime := fixed
	le := LedgerEvent{OccurredAt: evTime}
	if le.OccurredText() != "2026-05-02 02:30" {
		t.Fatalf("OccurredText under TZ=UTC = %q, want 2026-05-02 02:30", le.OccurredText())
	}

	o, checkouts, _, _, err := LoadRecordDetail(ctx, pool, ordID)
	if err != nil {
		t.Fatal(err)
	}
	if o.OpenedText() != "2026-05-02 02:30" || o.ClosedText() != "2026-05-02 02:30" {
		t.Fatalf("order timestamps under TZ=UTC = %q / %q", o.OpenedText(), o.ClosedText())
	}
	if len(checkouts) != 1 || checkouts[0].Payment.CreatedText() != "2026-05-02 02:30" {
		t.Fatalf("payment timestamp under TZ=UTC = %+v", checkouts)
	}

	// P2-5：关闭连接池验证 /admin/records* 不把底层 pgx/postgres 报错暴露给前端
	pool.Close()
	svc := NewService(pool, ui.MustRenderer(web.TemplatesFS, nil, func() string { return "PourOps" }), settings.NewSettingsStore(nil, "PourOps"))
	for _, tc := range []struct {
		url string
		fn  http.HandlerFunc
	}{
		{"/admin/records", svc.RecordsOrders},
		{"/admin/records/orders/1", func(w http.ResponseWriter, r *http.Request) {
			r.SetPathValue("id", "1")
			svc.RecordsOrderDetail(w, r)
		}},
		{"/admin/records/operations", svc.RecordsOperations},
	} {
		rec := httptest.NewRecorder()
		tc.fn(rec, httptest.NewRequest(http.MethodGet, tc.url, nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s status = %d, want 500", tc.url, rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(strings.ToLower(body), "closed pool") || strings.Contains(body, "pgx") {
			t.Fatalf("%s leaked internal error: %q", tc.url, body)
		}
	}
}

func TestReportRecordsKeysetPaginationBeyondPage1000(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()

	// 插入 55 条订单与流水验证 keyset 翻页不依赖 OFFSET 且完整覆盖第二页
	for i := range 55 {
		var oid int64
		if err := pool.QueryRow(ctx, `insert into orders (spot, closed_at) values ($1, now()) returning id`,
			"table-"+string(rune('a'+i%26))).Scan(&oid); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `insert into operation_events (kind, subject, detail, actor) values ('stock', $1, 'd', 'u')`,
			"item-"+string(rune('a'+i%26))); err != nil {
			t.Fatal(err)
		}
	}

	page1, hasNext1, err := ListRecordOrdersCursor(ctx, pool, "", 0)
	if err != nil || len(page1) != recordPageSize || !hasNext1 {
		t.Fatalf("page1 len=%d hasNext=%v err=%v", len(page1), hasNext1, err)
	}
	page2, hasNext2, err := ListRecordOrdersCursor(ctx, pool, "", page1[len(page1)-1].ID)
	if err != nil || len(page2) != 5 || hasNext2 {
		t.Fatalf("page2 len=%d hasNext=%v err=%v", len(page2), hasNext2, err)
	}

	ev1, evNext1, err := ListLedgerEventsCursor(ctx, pool, "", nil, 0)
	if err != nil || len(ev1) != recordPageSize || !evNext1 {
		t.Fatalf("ev1 len=%d hasNext=%v err=%v", len(ev1), evNext1, err)
	}
	lastEv := ev1[len(ev1)-1]
	ev2, evNext2, err := ListLedgerEventsCursor(ctx, pool, "", &lastEv.OccurredAt, lastEv.ID)
	if err != nil || len(ev2) != 5 || evNext2 {
		t.Fatalf("ev2 len=%d hasNext=%v err=%v", len(ev2), evNext2, err)
	}

	// HTTP /admin/records 返回带 before= 的下一页链接
	svc := NewService(pool, ui.MustRenderer(web.TemplatesFS, nil, func() string { return "PourOps" }), settings.NewSettingsStore(nil, "PourOps"))
	rec := httptest.NewRecorder()
	svc.RecordsOrders(rec, httptest.NewRequest(http.MethodGet, "/admin/records", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "?before=") {
		t.Fatalf("expected cursor NextURL (?before=) in /admin/records, code=%d", rec.Code)
	}
}

func TestReportP2CostSnapshotFinishedWasteAndUncostedRevenue(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	ts := func(day, hour int) time.Time {
		return time.Date(2026, 6, day, hour, 0, 0, 0, platform.ReportZone)
	}

	var beerID, kegFinishedID, kegUncostedID int64
	if err := pool.QueryRow(ctx, `insert into beers (brewery, name, style, category, price_330)
		values ('厂A', '损耗IPA', 'IPA', 'IPA', 5000) returning id`).Scan(&beerID); err != nil {
		t.Fatal(err)
	}
	// 1) 一桶 10000ml / 进价 10000 分：售出 4000ml（快照成本 4000 分），随后结桶
	if err := pool.QueryRow(ctx, `insert into kegs (beer_id, capacity_ml, cost_cents, tapped_at)
		values ($1, 10000, 10000, $2) returning id`, beerID, ts(10, 10)).Scan(&kegFinishedID); err != nil {
		t.Fatal(err)
	}
	txW, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := inventory.AddPourTx(ctx, txW, kegFinishedID, 8, 4000, 20000); err != nil {
		t.Fatal(err)
	}
	if err := txW.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	execSQL(t, pool, `update keg_pours set poured_at=$2 where keg_id=$1`, kegFinishedID, ts(10, 20))
	execSQL(t, pool, `update kegs set finished_at=$2 where id=$1`, kegFinishedID, ts(11, 20))

	var kegSnapID int64
	if err := pool.QueryRow(ctx, `insert into kegs (beer_id, capacity_ml, cost_cents, tapped_at)
		values ($1, 10000, 20000, $2) returning id`, beerID, ts(10, 12)).Scan(&kegSnapID); err != nil {
		t.Fatal(err)
	}
	txS, _ := pool.Begin(ctx)
	if err := inventory.AddPourTx(ctx, txS, kegSnapID, 1, 500, 4500); err != nil {
		t.Fatal(err)
	}
	_ = txS.Commit(ctx)
	execSQL(t, pool, `update keg_pours set poured_at=$2 where keg_id=$1`, kegSnapID, ts(10, 21))
	// 事后直接改桶成本（P2-26：历史售出仍按落账时快照 1000 分计，不被篡改为 5000 分）
	execSQL(t, pool, `update kegs set cost_cents=100000 where id=$1`, kegSnapID)

	// 2) 为第二款酒建未填成本在售桶（cost_cents=0），售出 5500 分（P2-25 UncostedRevenue）
	var beer2ID int64
	if err := pool.QueryRow(ctx, `insert into beers (brewery, name, style, category, price_330)
		values ('厂B', '无成本IPA', 'IPA', 'IPA', 5500) returning id`).Scan(&beer2ID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `insert into kegs (beer_id, capacity_ml, cost_cents, tapped_at)
		values ($1, 20000, 0, $2) returning id`, beer2ID, ts(10, 15)).Scan(&kegUncostedID); err != nil {
		t.Fatal(err)
	}
	execSQL(t, pool, `insert into keg_pours (keg_id, cups, ml, amount_cents, cost_cents, poured_at)
		values ($1, 1, 330, 5500, 0, $2)`, kegUncostedID, ts(11, 21))

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	sales, err := LoadSalesInsights(ctx, tx, ts(10, 7), ts(12, 7))
	if err != nil {
		t.Fatal(err)
	}
	aud, err := LoadAudienceInsights(ctx, tx, ts(10, 7), ts(12, 7), platform.DefaultBeerCategories)
	if err != nil {
		t.Fatal(err)
	}

	// 验证成本：10号 4000(finished keg) + 1000(snap keg) = 5000；11号 结桶损耗 6000；总成本 = 11000
	if sales.FinishedWasteCost != 6000 {
		t.Fatalf("FinishedWasteCost = %d, want 6000", sales.FinishedWasteCost)
	}
	if sales.Draft.Cost != 11000 || sales.Current.Cost != 11000 {
		t.Fatalf("Draft.Cost=%d Current.Cost=%d, want 11000 (4000+1000 snap + 6000 waste)", sales.Draft.Cost, sales.Current.Cost)
	}
	// 未填成本营收只在 SalesInsights 口径暴露；AudienceInsights 不再重复查询。
	if sales.UncostedRevenue != 5500 {
		t.Fatalf("UncostedRevenue sales=%d, want 5500", sales.UncostedRevenue)
	}
	if aud.TotalCost != 5000 {
		t.Fatalf("Audience poured snapshot cost = %d, want 5000 (immutable after keg cost change)", aud.TotalCost)
	}
}
