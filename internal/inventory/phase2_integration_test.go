package inventory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"pourops/internal/catalog"
	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/internal/ui"
	"pourops/migrations"
	"pourops/web"
)

func TestDeleteKegAndDeletePourSafety(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := platform.WithActor(context.Background(), "boss")
	beerID := invSeedBeer(t, pool, "保护测试 IPA")

	if err := PurchaseKegs(ctx, pool, beerID, 2, 20000, 100000); err != nil {
		t.Fatal(err)
	}
	if err := TapBeer(ctx, pool, beerID, "01", 20000, 100000); err != nil {
		t.Fatal(err)
	}
	activeKegID := invKegID(t, pool, beerID, true)
	untappedKegID := invKegID(t, pool, beerID, false)

	// 挂在未结 POS 订单上的酒桶禁止删除。
	var orderID int64
	if err := pool.QueryRow(ctx, `insert into orders (spot) values ('bar-1') returning id`).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into order_items (order_id, kind, ref_id, name, unit_cents, ml, qty)
		values ($1, 'beer', $2, '保护测试', 4500, 330, 1)`, orderID, untappedKegID); err != nil {
		t.Fatal(err)
	}
	if err := DeleteKeg(ctx, pool, untappedKegID); err == nil {
		t.Fatal("有未结 POS 订单引用的酒桶不应允许删除")
	}
	if _, err := pool.Exec(ctx, `delete from order_items where order_id=$1`, orderID); err != nil {
		t.Fatal(err)
	}

	// 无售出无挂单的空库存桶允许删除，且写入 OpStock 流水。
	if err := DeleteKeg(ctx, pool, untappedKegID); err != nil {
		t.Fatalf("干净空桶应允许删除: %v", err)
	}
	var opCount int
	if err := pool.QueryRow(ctx, `select count(*)::int from operation_events where kind=$1 and detail like '%删除酒桶%'`, platform.OpStock).Scan(&opCount); err != nil || opCount != 1 {
		t.Fatalf("删除空桶应写 1 条 OpStock 流水，实际 %d (err=%v)", opCount, err)
	}

	// 有售出记录的酒桶禁止删除。
	if err := AddPour(ctx, pool, activeKegID, 1, 330, 4500); err != nil {
		t.Fatal(err)
	}
	if err := DeleteKeg(ctx, pool, activeKegID); err == nil {
		t.Fatal("有售出记录的酒桶不应允许删除")
	}

	// POS 售出记录禁止被 DeletePour 直接抹除。
	var payID, posPourID int64
	if err := pool.QueryRow(ctx, `insert into payments (order_id, amount_cents, items_count, actor)
		values ($1, 4500, 1, 'staff') returning id`, orderID).Scan(&payID); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AddPourForOrderTx(ctx, tx, activeKegID, 1, 330, 4500, payID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select id from keg_pours where payment_id=$1`, payID).Scan(&posPourID); err != nil {
		t.Fatal(err)
	}
	if err := DeletePour(ctx, pool, posPourID); err == nil {
		t.Fatal("POS 生成的售出记录不应被 DeletePour 删除")
	}
	pours, err := ListPours(ctx, pool, activeKegID)
	if err != nil || len(pours) != 2 {
		t.Fatalf("Pours len = %d, want 2", len(pours))
	}

	// 已结桶后手工记录也不能删除。
	if err := UntapBeer(ctx, pool, beerID); err != nil {
		t.Fatal(err)
	}
	for _, p := range pours {
		if p.CanVoid() {
			if err := DeletePour(ctx, pool, p.ID); err == nil {
				t.Fatal("已结桶的手工售出记录不应允许删除")
			}
		}
	}
}

func TestSaveRetailOptimisticConcurrencyAndSoftDelete(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := platform.WithActor(context.Background(), "boss")

	// 新建 0 库存商品不应误触发 LowStockItem。
	zeroID, zeroLow, err := SaveRetail(ctx, pool, RetailItem{Name: "零库存新建", PriceCents: 2000, CostCents: 1000, Stock: 0, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if zeroLow != nil || zeroID == 0 {
		t.Fatalf("0 库存新建不应触发低库存预警: low=%+v", zeroLow)
	}

	id, _, err := SaveRetail(ctx, pool, RetailItem{Name: "薯条", PriceCents: 3000, CostCents: 1000, Stock: 10, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	// 并发 POS 卖出 2 件，库存变为 8。
	if _, err := SellRetail(ctx, pool, []SaleLine{{Item: RetailItem{ID: id, Name: "薯条", PriceCents: 3000}, Qty: 2}}); err != nil {
		t.Fatal(err)
	}

	// 店长未改库存（form stock == expectedStock == 10），仅调价为 3500：必须保留实时库存 8！
	exp10 := 10
	_, _, err = SaveRetailWithExpected(ctx, pool, RetailItem{
		ID: id, Name: "薯条", PriceCents: 3500, CostCents: 1000, Stock: 10, Active: true,
	}, &exp10)
	if err != nil {
		t.Fatalf("保留实时库存改价失败: %v", err)
	}
	got, err := GetRetail(ctx, pool, id)
	if err != nil || got.Stock != 8 || got.PriceCents != 3500 {
		t.Fatalf("并发库存被吞或改价失败: %+v err=%v", got, err)
	}

	// 店长尝试改库存（form stock=15 != expectedStock=10），但实际库存已变成 8：必须冲突拒绝！
	if _, _, err := SaveRetailWithExpected(ctx, pool, RetailItem{
		ID: id, Name: "薯条", PriceCents: 3500, CostCents: 1000, Stock: 15, Active: true,
	}, &exp10); err == nil || !strings.Contains(err.Error(), "已变动") {
		t.Fatalf("期望拒绝过期库存覆盖，实际 err=%v", err)
	}

	// 软删除保留 retail_moves 历史并在后台列表隐藏，且扣减库存对已删除商品返回明确错误。
	if err := DeleteRetail(ctx, pool, id); err != nil {
		t.Fatalf("软删除失败: %v", err)
	}
	list, err := ListRetail(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range list {
		if it.ID == id {
			t.Fatalf("软删除商品不应再出现在活跃列表")
		}
	}
	var movesCnt int
	if err := pool.QueryRow(ctx, `select count(*)::int from retail_moves where item_id=$1`, id).Scan(&movesCnt); err != nil || movesCnt == 0 {
		t.Fatalf("软删除应保留 retail_moves 历史，实际 %d", movesCnt)
	}
	if _, err := SellRetail(ctx, pool, []SaleLine{{Item: RetailItem{ID: id, Name: "薯条", PriceCents: 3500}, Qty: 1}}); err == nil || !strings.Contains(err.Error(), "已停用删除") {
		t.Fatalf("卖已删除商品应明确提示已停用删除，实际 %v", err)
	}
}

func TestSaveKegStateMachineAndCapacityCostLog(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := platform.WithActor(context.Background(), "boss")
	beerID := invSeedBeer(t, pool, "状态机测试")
	otherBeerID := invSeedBeer(t, pool, "另一款酒")

	now := time.Now()
	later := now.Add(time.Hour)
	earlier := now.Add(-time.Hour)

	if _, err := SaveKeg(ctx, pool, Keg{BeerID: beerID, CapacityML: 20000, FinishedAt: &now}); err == nil {
		t.Fatal("未填开桶时间不能直接结桶")
	}
	if _, err := SaveKeg(ctx, pool, Keg{BeerID: beerID, CapacityML: 20000, PlacedAt: &later, TappedAt: &now}); err == nil {
		t.Fatal("开桶时间早于入库时间应失败")
	}
	if _, err := SaveKeg(ctx, pool, Keg{BeerID: beerID, CapacityML: 20000, TappedAt: &now, FinishedAt: &earlier}); err == nil {
		t.Fatal("结桶时间早于开桶时间应失败")
	}

	kegID, err := SaveKeg(ctx, pool, Keg{BeerID: beerID, CapacityML: 20000, CostCents: 100000, PlacedAt: &earlier, TappedAt: &now})
	if err != nil {
		t.Fatal(err)
	}
	if err := AddPour(ctx, pool, kegID, 2, 1000, 9000); err != nil {
		t.Fatal(err)
	}

	k, err := GetKeg(ctx, pool, kegID)
	if err != nil {
		t.Fatal(err)
	}
	k.BeerID = otherBeerID
	if _, err := SaveKeg(ctx, pool, k); err == nil {
		t.Fatal("有售出记录的桶不能换酒款")
	}
	k.BeerID = beerID
	k.CapacityML = 500
	if _, err := SaveKeg(ctx, pool, k); err == nil {
		t.Fatal("容量不能小于已售出毫升")
	}
	k.CapacityML = 20000
	k.CostCents = 120000
	if _, err := SaveKeg(ctx, pool, k); err != nil {
		t.Fatalf("改成本失败: %v", err)
	}
	var specLogs int
	if err := pool.QueryRow(ctx, `select count(*)::int from operation_events where kind=$1 and detail like '%修改桶%'`, platform.OpStock).Scan(&specLogs); err != nil || specLogs != 1 {
		t.Fatalf("修改桶成本/容量应记 OpStock，实际 %d", specLogs)
	}

	k.FinishedAt = &later
	if _, err := SaveKeg(ctx, pool, k); err != nil {
		t.Fatal(err)
	}
	k.FinishedAt = nil
	if _, err := SaveKeg(ctx, pool, k); err == nil {
		t.Fatal("已结桶不能重新打开为在售")
	}
	_ = catalog.DefaultCapacityML
}

func TestRetailSaveFailPreservesExpectedStockAndKegCostInput(t *testing.T) {
	if got := (Keg{CostCents: 120000}).CostInput(); got != "1200" {
		t.Fatalf("CostInput(120000) = %q, want 1200", got)
	}
	if got := (Keg{CostCents: 0}).CostInput(); got != "" {
		t.Fatalf("CostInput(0) = %q, want empty", got)
	}

	pool := platform.NewTestPool(t, migrations.FS)
	svc := NewService(pool, ui.MustRenderer(web.TemplatesFS, nil, func() string { return "PourOps" }),
		settings.NewSettingsStore(nil, "PourOps"), nil, nil)

	form := url.Values{
		"id":             {"42"},
		"expected_stock": {"10"},
		"name":           {""},
		"category":       {"零食"},
		"price":          {"30"},
		"cost":           {"10"},
		"stock":          {"15"},
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/retail", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	svc.RetailSave(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("校验失败应返回 400，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="expected_stock" value="10"`) {
		t.Fatalf("重绘失败表单应保留 expected_stock=10，未在新表单值 15 上覆写: %s", body)
	}
}

func TestKegSaveScreenOffPaginationInvalidTimeAndRetailEmptyStock(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := platform.WithActor(context.Background(), "boss")
	renderer := ui.MustRenderer(web.TemplatesFS, nil, func() string { return "PourOps" })
	svc := NewService(pool, renderer, settings.NewSettingsStore(nil, "PourOps"), nil, nil)

	beerID := invSeedBeer(t, pool, "编辑撤屏酒")
	if err := TapBeer(ctx, pool, beerID, "01", 20000, 100000); err != nil {
		t.Fatal(err)
	}
	activeKegID := invKegID(t, pool, beerID, true)
	k, err := GetKeg(ctx, pool, activeKegID)
	if err != nil {
		t.Fatal(err)
	}

	// 在未结账 POS 订单占用时，禁止清空 TappedAt 回退为未开桶。
	var orderID int64
	if err := pool.QueryRow(ctx, `insert into orders (spot) values ('bar-1') returning id`).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into order_items (order_id, kind, ref_id, name, unit_cents, ml, qty)
		values ($1, 'beer', $2, '编辑撤屏酒', 4500, 330, 1)`, orderID, activeKegID); err != nil {
		t.Fatal(err)
	}
	k.TappedAt = nil
	if _, err := SaveKegResultFn(ctx, pool, k); err == nil {
		t.Fatal("有未结账 POS 订单时不应允许清空开桶时间")
	}
	if _, err := pool.Exec(ctx, `delete from order_items where order_id=$1`, orderID); err != nil {
		t.Fatal(err)
	}

	res, err := SaveKegResultFn(ctx, pool, k)
	if err != nil {
		t.Fatalf("无占单清空开桶时间失败: %v", err)
	}
	if !res.ScreenChanged {
		t.Fatal("唯一在售桶撤回未开桶应标记 ScreenChanged=true")
	}
	var onScreen bool
	if err := pool.QueryRow(ctx, `select on_screen from beers where id=$1`, beerID).Scan(&onScreen); err != nil || onScreen {
		t.Fatalf("唯一在售桶撤回未开桶后 beers.on_screen 应为 false (onScreen=%v err=%v)", onScreen, err)
	}

	// KegSave 非法 placed_at 应返回 400。
	badTimeForm := url.Values{
		"beer_id":     {strconv.FormatInt(beerID, 10)},
		"capacity_ml": {"20000"},
		"cost":        {"1000"},
		"placed_at":   {"not-a-date"},
	}
	reqBadTime := httptest.NewRequest(http.MethodPost, "/admin/kegs", strings.NewReader(badTimeForm.Encode()))
	reqBadTime.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recBadTime := httptest.NewRecorder()
	svc.KegSave(recBadTime, reqBadTime)
	if recBadTime.Code != http.StatusBadRequest || !strings.Contains(recBadTime.Body.String(), "入库时间格式不对") {
		t.Fatalf("KegSave invalid placed_at = %d %q", recBadTime.Code, recBadTime.Body.String())
	}

	// 零售表单提交空 stock 应报错而不是静默置 0 下架。
	itemID, _, err := SaveRetail(ctx, pool, RetailItem{Name: "不许清空库存", Category: "罐装", PriceCents: 3000, CostCents: 1500, Stock: 5, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	emptyStockForm := url.Values{
		"id":             {strconv.FormatInt(itemID, 10)},
		"expected_stock": {"5"},
		"name":           {"不许清空库存"},
		"category":       {"罐装"},
		"price":          {"30"},
		"cost":           {"15"},
		"stock":          {""},
		"active":         {"1"},
	}
	reqEmptyStock := httptest.NewRequest(http.MethodPost, "/admin/retail", strings.NewReader(emptyStockForm.Encode()))
	reqEmptyStock.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recEmptyStock := httptest.NewRecorder()
	svc.RetailSave(recEmptyStock, reqEmptyStock)
	if recEmptyStock.Code != http.StatusBadRequest || !strings.Contains(recEmptyStock.Body.String(), "库存不能为空") {
		t.Fatalf("empty stock should return 400, got %d", recEmptyStock.Code)
	}
	afterRetail, err := GetRetail(ctx, pool, itemID)
	if err != nil || afterRetail.Stock != 5 || !afterRetail.Active {
		t.Fatalf("empty stock submission corrupted retail item: %+v err=%v", afterRetail, err)
	}

	// 桶装分页：插入 54 桶（合计 55 桶），验证第 1 页 50 条和第 2 页 5 条。
	if err := PurchaseKegs(ctx, pool, beerID, 54, 20000, 100000); err != nil {
		t.Fatal(err)
	}
	recP1 := httptest.NewRecorder()
	svc.KegList(recP1, httptest.NewRequest(http.MethodGet, "/admin/kegs", nil))
	if recP1.Code != http.StatusOK || !strings.Contains(recP1.Body.String(), "第 1 / 2 页（共 55 桶）") || !strings.Contains(recP1.Body.String(), "下一页") {
		t.Fatalf("KegList page 1 missing pagination, code=%d", recP1.Code)
	}
	recP2 := httptest.NewRecorder()
	svc.KegList(recP2, httptest.NewRequest(http.MethodGet, "/admin/kegs?page=2", nil))
	if recP2.Code != http.StatusOK || !strings.Contains(recP2.Body.String(), "第 2 / 2 页（共 55 桶）") || !strings.Contains(recP2.Body.String(), "上一页") {
		t.Fatalf("KegList page 2 missing pagination, code=%d", recP2.Code)
	}
}
