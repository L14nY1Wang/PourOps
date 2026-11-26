package pos

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pourops/internal/inventory"
	"pourops/internal/platform"
	"pourops/internal/ui"
	"pourops/migrations"
	"pourops/web"
)

// TestPreUntapOrderCheckoutAndPostUntapRejected 验证 P1-1：换桶结桶前已加购的挂单仍可核销，结桶后新加购不能核销到旧桶，且记录成本快照。
func TestPreUntapOrderCheckoutAndPostUntapRejected(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := platform.WithActor(context.Background(), "staff")

	var beerID, kegID int64
	if err := pool.QueryRow(ctx, `insert into beers (brewery, name, price_330, on_screen)
		values ('山厂', '山泉拉格', 4000, true) returning id`).Scan(&beerID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `insert into kegs (beer_id, capacity_ml, cost_cents, placed_at, tapped_at)
		values ($1, 20000, 100000, now(), now()) returning id`, beerID).Scan(&kegID); err != nil {
		t.Fatal(err)
	}

	orderID, err := EnsureOrder(ctx, pool, "bar-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := AddOrderItem(ctx, pool, orderID, "beer", kegID, "山厂 · 山泉拉格", 4000, 330); err != nil {
		t.Fatal(err)
	}

	// 下架结桶后，旧桶未结账单依然能完成结账并落入该旧桶及成本快照。
	if err := inventory.UntapBeer(ctx, pool, beerID); err != nil {
		t.Fatal(err)
	}
	payID, err := PayItems(ctx, pool, orderID, unpaidIDs(t, pool, orderID))
	if err != nil {
		t.Fatalf("结桶前已点订单应能顺利结账，实际报错: %v", err)
	}
	var source string
	var costSnap int
	if err := pool.QueryRow(ctx, `select source, cost_cents from keg_pours where payment_id=$1`, payID).Scan(&source, &costSnap); err != nil {
		t.Fatalf("未记录 POS 售出快照: %v", err)
	}
	wantCost := (100000*330 + 10000) / 20000
	if source != "pos" || costSnap != wantCost {
		t.Fatalf("source=%q cost_cents=%d, want pos/%d", source, costSnap, wantCost)
	}

	// 若下单时间晚于结桶时间，则拒绝核销到已结桶。
	lateOrder, err := EnsureOrder(ctx, pool, "bar-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := AddOrderItem(ctx, pool, lateOrder, "beer", kegID, "山厂 · 山泉拉格", 4000, 330); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update order_items set added_at = now() + interval '1 minute' where order_id=$1`, lateOrder); err != nil {
		t.Fatal(err)
	}
	if _, err := PayItems(ctx, pool, lateOrder, unpaidIDs(t, pool, lateOrder)); err == nil || !strings.Contains(err.Error(), "晚于结桶") {
		t.Fatalf("晚于结桶的条目不应允许继续核销，实际 err=%v", err)
	}
}

// TestCrossOrderDeterministicLockingNoDeadlock 验证 P1-2：交叉顺序点相同两桶与两零售 SKU 并发结账绝不死锁。
func TestCrossOrderDeterministicLockingNoDeadlock(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()

	kegA := seedKeg(t, pool, 4000)
	kegB := seedKeg(t, pool, 4500)
	ret1 := seedRetail(t, pool, 3000, 1000, 10)
	ret2 := seedRetail(t, pool, 2000, 800, 10)

	order1, _ := EnsureOrder(ctx, pool, "table-1")
	order2, _ := EnsureOrder(ctx, pool, "table-2")

	// 相反添加顺序：order1 = [A, B, r1, r2]; order2 = [B, A, r2, r1]
	for _, fn := range []error{
		AddOrderItem(ctx, pool, order1, "beer", kegA, "A", 4000, 330),
		AddOrderItem(ctx, pool, order1, "beer", kegB, "B", 4500, 330),
		AddOrderItem(ctx, pool, order1, "retail", ret1, "R1", 3000, 0),
		AddOrderItem(ctx, pool, order1, "retail", ret2, "R2", 2000, 0),
		AddOrderItem(ctx, pool, order2, "beer", kegB, "B", 4500, 330),
		AddOrderItem(ctx, pool, order2, "beer", kegA, "A", 4000, 330),
		AddOrderItem(ctx, pool, order2, "retail", ret2, "R2", 2000, 0),
		AddOrderItem(ctx, pool, order2, "retail", ret1, "R1", 3000, 0),
	} {
		if fn != nil {
			t.Fatal(fn)
		}
	}

	ids1 := unpaidIDs(t, pool, order1)
	ids2 := unpaidIDs(t, pool, order2)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := PayItems(ctx, pool, order1, ids1)
		errs <- err
	}()
	go func() {
		defer wg.Done()
		_, err := PayItems(ctx, pool, order2, ids2)
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("交叉并发结账不应失败/死锁: %v", err)
		}
	}
}

// TestPayIdempotencyExpectedTotalAndSpotBounds 验证 P2-7, P2-9, P2-20, P3-15, P3-22 与超卖提醒。
func TestPayIdempotencyExpectedTotalAndSpotBounds(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := platform.WithActor(context.Background(), "boss")

	var beerID, kegID int64
	if err := pool.QueryRow(ctx, `insert into beers (brewery, name, price_330, on_screen)
		values ('测试', '小桶', 4000, true) returning id`).Scan(&beerID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `insert into kegs (beer_id, capacity_ml, cost_cents, placed_at, tapped_at)
		values ($1, 500, 10000, now(), now()) returning id`, beerID).Scan(&kegID); err != nil {
		t.Fatal(err)
	}

	orderID, err := EnsureOrder(ctx, pool, "bar-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := AddOrderItem(ctx, pool, orderID, "beer", kegID, "小桶", 4000, 330); err != nil {
		t.Fatal(err)
	}
	if err := AddOrderItem(ctx, pool, orderID, "beer", kegID, "小桶", 4000, 330); err != nil {
		t.Fatal(err)
	}
	if err := AddOrderItem(ctx, pool, orderID, "retail", seedRetail(t, pool, 3000, 1000, 5), "小吃", 3000, 0); err != nil {
		t.Fatal(err)
	}

	o, err := LoadOrder(ctx, pool, orderID)
	if err != nil || len(o.Unpaid) != 2 {
		t.Fatal(err)
	}
	// 期待金额不符必须拒绝（P2-9）
	wrongExp := 9999
	if _, err := PayItemsWithOptions(ctx, pool, ManualGateway{}, orderID, []int64{o.Unpaid[0].ID}, PayOptions{
		ExpectedTotalCents: &wrongExp,
	}); err == nil || !strings.Contains(err.Error(), "金额已变动") {
		t.Fatalf("应拒绝总额不符的结账: %v", err)
	}

	// 结部分（啤酒 2 杯 660ml > 500ml 容量 -> 触发 OverSoldCount，幂等键重放不二次收费）
	beerLine := o.Unpaid[0]
	retailLine := o.Unpaid[1]
	expBeer := 8000
	res1, err := PayItemsWithOptions(ctx, pool, ManualGateway{}, orderID, []int64{beerLine.ID}, PayOptions{
		Method:             "cash",
		IdempotencyKey:     "idem-keg-1",
		ExpectedTotalCents: &expBeer,
	})
	if err != nil {
		t.Fatalf("结账失败: %v", err)
	}
	if res1.Replayed || res1.OverSoldCount != 1 || res1.Method != "cash" {
		t.Fatalf("res1 = %+v", res1)
	}
	resReplay, err := PayItemsWithOptions(ctx, pool, ManualGateway{}, orderID, []int64{beerLine.ID}, PayOptions{
		Method:         "cash",
		IdempotencyKey: "idem-keg-1",
	})
	if err != nil || !resReplay.Replayed || resReplay.PaymentID != res1.PaymentID {
		t.Fatalf("幂等重放不对: %+v err=%v", resReplay, err)
	}

	// P3-22：部分结账后，删掉最后一条未付明细应自动关单！
	if err := LessOrderItem(ctx, pool, orderID, retailLine.ID); err != nil {
		t.Fatal(err)
	}
	oAfter, err := LoadOrder(ctx, pool, orderID)
	if err != nil {
		t.Fatal(err)
	}
	if !oAfter.Closed || len(oAfter.Unpaid) != 0 {
		t.Fatalf("部分结单删掉最后未付项后应自动关单: closed=%v unpaid=%d", oAfter.Closed, len(oAfter.Unpaid))
	}

	// 临时台位从 0 起增删与 50 上限，减到 0 后再次减少报错
	for range 50 {
		if err := AddTempSpot(ctx, pool); err != nil {
			t.Fatalf("AddTempSpot failed: %v", err)
		}
	}
	if err := AddTempSpot(ctx, pool); err == nil {
		t.Fatal("临时台位应限制最多 50 个")
	}
	if err := RemoveTempSpot(ctx, pool); err != nil {
		t.Fatalf("RemoveTempSpot failed: %v", err)
	}

	// P3-16: slow printer timeout bound doesn't crash HTTP handler
	svc := NewService(pool, nil, nil, ManualGateway{}, slowPrinter{})
	orderSlow, _ := EnsureOrder(ctx, pool, "table-1")
	retSlow := seedRetail(t, pool, 1000, 500, 3)
	_ = AddOrderItem(ctx, pool, orderSlow, "retail", retSlow, "慢打印", 1000, 0)
	slowIDs := unpaidIDs(t, pool, orderSlow)
	form := url.Values{"items": {strconv.FormatInt(slowIDs[0], 10)}}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", strconv.FormatInt(orderSlow, 10))
	rec := httptest.NewRecorder()
	svc.PosPay(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/pos/pay/") {
		t.Fatalf("PosPay redirect = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

type slowPrinter struct{}

func (slowPrinter) Name() string { return "slow" }
func (slowPrinter) Print(ctx context.Context, _ Receipt) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(100 * time.Millisecond):
		return nil
	}
}

func TestPosSpotEmptyDeleteStaleIdempotencyAndNoStore(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := platform.WithActor(context.Background(), "staff")

	if got := SpotLabel("temp-2"); got != "临时 2" {
		t.Fatalf("SpotLabel(temp-2) = %q, want 临时 2", got)
	}

	// 初始 temp_count=0：直接减位失败；新增 2 个临时台位后，temp-2 仅空挂单时可减位清理，temp-1 有未付明细时禁止减位。
	if err := RemoveTempSpot(ctx, pool); err == nil {
		t.Fatal("temp_count=0 时应禁止继续减位")
	}
	if _, err := EnsureOrder(ctx, pool, "temp-1"); err == nil {
		t.Fatal("未新增临时台位前不应允许直接开 temp-1")
	}
	if err := AddTempSpot(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := AddTempSpot(ctx, pool); err != nil {
		t.Fatal(err)
	}
	emptyOrder, err := EnsureOrder(ctx, pool, "temp-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveTempSpot(ctx, pool); err != nil {
		t.Fatalf("empty open order on last temp spot should be deletable: %v", err)
	}
	var cnt int
	if err := pool.QueryRow(ctx, `select count(*)::int from orders where id=$1`, emptyOrder).Scan(&cnt); err != nil || cnt != 0 {
		t.Fatalf("empty open order should be deleted, cnt=%d err=%v", cnt, err)
	}

	nonEmptyOrder, err := EnsureOrder(ctx, pool, "temp-1")
	if err != nil {
		t.Fatal(err)
	}
	rID := seedRetail(t, pool, 1500, 600, 10)
	if err := AddOrderItem(ctx, pool, nonEmptyOrder, "retail", rID, "花生", 1500, 0); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTempSpot(ctx, pool); err == nil {
		t.Fatal("non-empty open order on last temp spot must block RemoveTempSpot")
	}

	// 若先结了一行（幂等键 k1），随后选中的条目集合仍包含未付项时，同一幂等键不能静默返回成功。
	if err := AddOrderItem(ctx, pool, nonEmptyOrder, "retail", rID, "可乐", 1200, 0); err != nil {
		t.Fatal(err)
	}
	ids := unpaidIDs(t, pool, nonEmptyOrder)
	res1, err := PayItemsWithOptions(ctx, pool, ManualGateway{}, nonEmptyOrder, []int64{ids[0]}, PayOptions{
		Method:         "cash",
		IdempotencyKey: "k1",
	})
	if err != nil {
		t.Fatalf("first pay failed: %v", err)
	}
	if _, err := PayItemsWithOptions(ctx, pool, ManualGateway{}, nonEmptyOrder, []int64{ids[0], ids[1]}, PayOptions{
		Method:         "cash",
		IdempotencyKey: "k1",
	}); err == nil || !strings.Contains(err.Error(), "页面状态已过期") {
		t.Fatalf("stale idempotency key with unpaid items selected should fail, got %v", err)
	}

	// 验证 bar_count=0 / table_count=0 时，ListSpots 隐藏固定区且 EnsureOrder 禁止开固定台，仅保留临时台位。
	if _, err := pool.Exec(ctx, `
		insert into store_settings (key, value, updated_at)
		values ('bar_count', '0', now()), ('table_count', '0', now())
		on conflict (key) do update set value = excluded.value, updated_at = now()`); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureOrder(ctx, pool, "bar-1"); err == nil {
		t.Fatal("bar_count=0 时不应允许开 bar-1")
	}
	if _, err := EnsureOrder(ctx, pool, "table-1"); err == nil {
		t.Fatal("table_count=0 时不应允许开 table-1")
	}
	spots, err := ListSpots(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(spots) != 1 || spots[0].Key != "temp-1" || spots[0].Label != "临时 1" {
		t.Fatalf("ListSpots with bar=0 table=0 temp=1 got %+v", spots)
	}

	// POS 订单与小票页响应必须携带 Cache-Control: no-store；店员可直接操作 /pos/temp/add|remove，旧 /pos/bar/add 已移除。
	renderer := ui.MustRenderer(web.TemplatesFS, nil, func() string { return "PourOps" })
	svc := NewService(pool, renderer, nil, ManualGateway{}, NoopPrinter{})
	mux := http.NewServeMux()
	svc.RegisterRoutes(mux, ui.Guards{
		Any:  func(h http.HandlerFunc) http.HandlerFunc { return h },
		Auth: func(h http.HandlerFunc) http.HandlerFunc { return h },
		Boss: func(h http.HandlerFunc) http.HandlerFunc { return h },
	})

	recAddTemp := httptest.NewRecorder()
	mux.ServeHTTP(recAddTemp, httptest.NewRequest(http.MethodPost, "/pos/temp/add", nil))
	if recAddTemp.Code != http.StatusSeeOther {
		t.Fatalf("POST /pos/temp/add status = %d", recAddTemp.Code)
	}
	recRemTemp := httptest.NewRecorder()
	mux.ServeHTTP(recRemTemp, httptest.NewRequest(http.MethodPost, "/pos/temp/remove", nil))
	if recRemTemp.Code != http.StatusSeeOther {
		t.Fatalf("POST /pos/temp/remove status = %d", recRemTemp.Code)
	}
	recOldRoute := httptest.NewRecorder()
	mux.ServeHTTP(recOldRoute, httptest.NewRequest(http.MethodPost, "/pos/bar/add", nil))
	if recOldRoute.Code != http.StatusNotFound && recOldRoute.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /pos/bar/add should be gone, got %d", recOldRoute.Code)
	}

	reqOrder := httptest.NewRequest(http.MethodGet, "/pos/order/"+strconv.FormatInt(nonEmptyOrder, 10), nil)
	reqOrder.SetPathValue("id", strconv.FormatInt(nonEmptyOrder, 10))
	recOrder := httptest.NewRecorder()
	svc.PosOrder(recOrder, reqOrder)
	if recOrder.Code != http.StatusOK || recOrder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("PosOrder code=%d cache=%q", recOrder.Code, recOrder.Header().Get("Cache-Control"))
	}

	reqPay := httptest.NewRequest(http.MethodGet, "/pos/pay/"+strconv.FormatInt(res1.PaymentID, 10), nil)
	reqPay.SetPathValue("id", strconv.FormatInt(res1.PaymentID, 10))
	recPay := httptest.NewRecorder()
	svc.PosPayPage(recPay, reqPay)
	if recPay.Code != http.StatusOK || recPay.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("PosPayPage code=%d cache=%q", recPay.Code, recPay.Header().Get("Cache-Control"))
	}
}
