package inventory

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/catalog"
	"pourops/internal/event"
	"pourops/internal/platform"
	"pourops/migrations"
)

func ledgerCount(t *testing.T, pool *pgxpool.Pool, kind string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`select count(*)::int from operation_events where kind=$1`, kind).Scan(&n); err != nil {
		t.Fatalf("统计流水失败: %v", err)
	}
	return n
}

func ledgerLast(t *testing.T, pool *pgxpool.Pool) (kind, subject, detail, actor string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`select kind, subject, detail, actor from operation_events order by id desc limit 1`).
		Scan(&kind, &subject, &detail, &actor); err != nil {
		t.Fatalf("读最后一条流水失败: %v", err)
	}
	return
}

func TestOperationActorFromContext(t *testing.T) {
	if got := platform.OperationActor(context.Background()); got != "" {
		t.Fatalf("没有 context 时操作者应为空，得到 %q", got)
	}
	ctx := platform.WithActor(context.Background(), "boss")
	if got := platform.OperationActor(ctx); got != "boss" {
		t.Fatalf("操作者 = %q，应为 boss", got)
	}
}

func TestPurchaseLogsAndRollsBack(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := platform.WithActor(context.Background(), "boss")

	id, err := PurchaseBeerTx(ctx, pool, catalog.Beer{Brewery: "测试厂", Name: "流水进货 IPA"}, 2, 20000, 12345)
	if err != nil {
		t.Fatalf("进货失败: %v", err)
	}
	if id == 0 {
		t.Fatal("进货没返回酒款 id")
	}
	if n := ledgerCount(t, pool, platform.OpPurchase); n != 1 {
		t.Fatalf("进货流水 %d 条，应为 1", n)
	}
	kind, subject, detail, actor := ledgerLast(t, pool)
	if kind != platform.OpPurchase || !strings.Contains(subject, "流水进货 IPA") ||
		!strings.Contains(detail, "2 桶") || !strings.Contains(detail, "123.45") || actor != "boss" {
		t.Fatalf("进货流水内容不对: %s %s %s %s", kind, subject, detail, actor)
	}

	if _, err := PurchaseBeerTx(ctx, pool, catalog.Beer{Brewery: "测试厂", Name: "失败进货"}, 0, 20000, 100); err == nil {
		t.Fatal("0 桶进货应当失败")
	}
	var beers int
	if err := pool.QueryRow(ctx, `select count(*)::int from beers where name='失败进货'`).Scan(&beers); err != nil {
		t.Fatal(err)
	}
	if beers != 0 || ledgerCount(t, pool, platform.OpPurchase) != 1 {
		t.Fatalf("失败进货留下了数据: beers=%d 流水=%d", beers, ledgerCount(t, pool, platform.OpPurchase))
	}
}

func TestManualSaleAndVoid(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := platform.WithActor(context.Background(), "manager")
	beerID := invSeedBeer(t, pool, "流水售出 IPA")
	if err := PurchaseKegs(ctx, pool, beerID, 1, 20000, 5000); err != nil {
		t.Fatal(err)
	}
	if err := TapBeer(ctx, pool, beerID, "B2", 20000, 5000); err != nil {
		t.Fatal(err)
	}
	kegID := invKegID(t, pool, beerID, true)
	base := ledgerCount(t, pool, platform.OpManualSale)

	if err := AddPour(ctx, pool, kegID, 2, 660, 8800); err != nil {
		t.Fatalf("记售出失败: %v", err)
	}
	if n := ledgerCount(t, pool, platform.OpManualSale); n != base+1 {
		t.Fatalf("手工售出流水 %d 条，应为 %d", n, base+1)
	}
	_, subject, detail, actor := ledgerLast(t, pool)
	if !strings.Contains(subject, "流水售出 IPA") || !strings.Contains(detail, "2 杯") || actor != "manager" {
		t.Fatalf("手工售出流水内容不对: %s %s %s", subject, detail, actor)
	}

	var pourID int64
	if err := pool.QueryRow(ctx, `select id from keg_pours order by id desc limit 1`).Scan(&pourID); err != nil {
		t.Fatal(err)
	}
	if err := DeletePour(ctx, pool, pourID); err != nil {
		t.Fatalf("删售出失败: %v", err)
	}
	if n := ledgerCount(t, pool, platform.OpSaleVoid); n != 1 {
		t.Fatalf("撤销流水 %d 条，应为 1", n)
	}
	_, _, detail, _ = ledgerLast(t, pool)
	if !strings.Contains(detail, "删除售出 2 杯") {
		t.Fatalf("撤销流水没有保留被删快照: %s", detail)
	}
	if err := DeletePour(ctx, pool, pourID); err == nil {
		t.Fatal("删不存在的售出应当报错")
	}
	if n := ledgerCount(t, pool, platform.OpSaleVoid); n != 1 {
		t.Fatalf("删不存在记录不该凭空写流水，现有 %d 条", n)
	}
}

func TestTapUntapOnlyRealTransitions(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := platform.WithActor(context.Background(), "boss")
	beerID := invSeedBeer(t, pool, "流水开桶 IPA")
	if err := PurchaseKegs(ctx, pool, beerID, 2, 20000, 5000); err != nil {
		t.Fatal(err)
	}
	if err := TapBeer(ctx, pool, beerID, "C3", 20000, 5000); err != nil {
		t.Fatal(err)
	}
	if n := ledgerCount(t, pool, platform.OpTap); n != 1 {
		t.Fatalf("开桶流水 %d 条，应为 1", n)
	}
	_, subject, detail, _ := ledgerLast(t, pool)
	if !strings.Contains(subject, "流水开桶 IPA") || !strings.Contains(detail, "C3") {
		t.Fatalf("开桶流水内容不对: %s %s", subject, detail)
	}
	// 已有在售桶时再次上架只是改号位，不能伪造第二次开桶。
	if err := TapBeer(ctx, pool, beerID, "C4", 20000, 5000); err != nil {
		t.Fatal(err)
	}
	if n := ledgerCount(t, pool, platform.OpTap); n != 1 {
		t.Fatalf("重复上架多记了开桶流水: %d", n)
	}

	if err := UntapBeer(ctx, pool, beerID); err != nil {
		t.Fatal(err)
	}
	if n := ledgerCount(t, pool, platform.OpUntap); n != 1 {
		t.Fatalf("结桶流水 %d 条，应为 1", n)
	}
	if err := UntapBeer(ctx, pool, beerID); err != nil {
		t.Fatal(err)
	}
	if n := ledgerCount(t, pool, platform.OpUntap); n != 1 {
		t.Fatalf("重复下架多记了结桶流水: %d", n)
	}

	if n := ledgerCount(t, pool, platform.OpScreenOn); n != 1 {
		t.Fatalf("上架开桶上屏流水 %d 条，应为 1", n)
	}
	if n := ledgerCount(t, pool, platform.OpScreenOff); n != 1 {
		t.Fatalf("下架撤屏流水 %d 条，应为 1", n)
	}

	if _, err := catalog.ToggleBeer(ctx, pool, beerID); err != nil {
		t.Fatal(err)
	}
	if n := ledgerCount(t, pool, platform.OpScreenOn); n != 2 {
		t.Fatalf("上屏流水 %d 条，应为 2", n)
	}
	if _, err := catalog.ToggleBeer(ctx, pool, beerID); err != nil {
		t.Fatal(err)
	}
	if n := ledgerCount(t, pool, platform.OpScreenOff); n != 2 {
		t.Fatalf("下屏流水 %d 条，应为 2", n)
	}
}

func TestRetailLedgerAndLowStockEvent(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	svc, _, bus := newTestService(t, pool)
	ctx := platform.WithActor(context.Background(), "boss")

	var mu sync.Mutex
	var lowEvents []event.Event
	bus.Subscribe(func(ev event.Event) {
		mu.Lock()
		lowEvents = append(lowEvents, ev)
		mu.Unlock()
	}, event.EventRetailLowStock)

	id, _, err := SaveRetail(ctx, pool, RetailItem{Name: "流水可乐", PriceCents: 500, CostCents: 200, Stock: 5, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := ledgerCount(t, pool, platform.OpStock); n != 1 {
		t.Fatalf("初始库存流水 %d 条，应为 1", n)
	}
	if n := ledgerCount(t, pool, platform.OpRetailOn); n != 1 {
		t.Fatalf("初始上架流水 %d 条，应为 1", n)
	}
	_, subject, _, _ := ledgerLast(t, pool)
	if !strings.Contains(subject, "流水可乐") {
		t.Fatalf("零售流水没有保留商品名快照: %s", subject)
	}

	// 只改库存：记一条库存流水，不动上下架。
	if _, _, err := SaveRetail(ctx, pool, RetailItem{ID: id, Name: "流水可乐", PriceCents: 500, CostCents: 200, Stock: 8, Active: true}); err != nil {
		t.Fatal(err)
	}
	if n := ledgerCount(t, pool, platform.OpStock); n != 2 {
		t.Fatalf("编辑库存后流水 %d 条，应为 2", n)
	}
	if n := ledgerCount(t, pool, platform.OpRetailOff); n != 0 {
		t.Fatalf("改库存不该写下架流水: %d", n)
	}

	// 库存改到 0：记库存变动 + 下架。
	if _, _, err := SaveRetail(ctx, pool, RetailItem{ID: id, Name: "流水可乐", PriceCents: 500, CostCents: 200, Stock: 0, Active: true}); err != nil {
		t.Fatal(err)
	}
	if n := ledgerCount(t, pool, platform.OpRetailOff); n != 1 {
		t.Fatalf("库存归零没记下架流水: %d", n)
	}
	if err := ToggleRetail(ctx, pool, id); err == nil {
		t.Fatal("库存 0 上架应当失败")
	}
	if n := ledgerCount(t, pool, platform.OpRetailOn); n != 1 {
		t.Fatalf("失败上架写了流水: %d", n)
	}

	if _, err := AddStock(ctx, pool, id, 4, "入库"); err != nil {
		t.Fatal(err)
	}
	if n := ledgerCount(t, pool, platform.OpStock); n != 4 {
		t.Fatalf("入库后库存流水 %d 条，应为 4", n)
	}
	if err := ToggleRetail(ctx, pool, id); err != nil {
		t.Fatal(err)
	}
	if n := ledgerCount(t, pool, platform.OpRetailOn); n != 2 {
		t.Fatalf("补货后上架流水 %d 条，应为 2", n)
	}
	// 通过 HTTP RetailStock 扣减到 <= 3（4 -> 2）应触发低库存事件
	reqStock := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/retail/%d/stock", id), strings.NewReader("delta=-2"))
	reqStock.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqStock.SetPathValue("id", fmt.Sprintf("%d", id))
	recStock := httptest.NewRecorder()
	svc.RetailStock(recStock, reqStock)
	if recStock.Code != http.StatusSeeOther {
		t.Fatalf("RetailStock status = %d", recStock.Code)
	}
	mu.Lock()
	if len(lowEvents) != 1 || lowEvents[0].Subject != platform.RetailSubject("流水可乐") {
		t.Fatalf("扣减到 2 件应发布 EventRetailLowStock，实际 %+v", lowEvents)
	}
	mu.Unlock()

	if _, err := AddStock(ctx, pool, id, -2, "调整"); err != nil {
		t.Fatal(err)
	}
	if n := ledgerCount(t, pool, platform.OpRetailOff); n != 2 {
		t.Fatalf("扣到 0 没记自动下架流水: %d", n)
	}
}

func TestKegEditTransitions(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := platform.WithActor(context.Background(), "boss")
	beerID := invSeedBeer(t, pool, "流水编辑桶 IPA")

	id, err := SaveKeg(ctx, pool, Keg{BeerID: beerID, CapacityML: 20000, CostCents: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if n := ledgerCount(t, pool, platform.OpStock); n != 1 {
		t.Fatalf("新建库存桶流水 %d 条，应为 1", n)
	}

	k, err := GetKeg(ctx, pool, id)
	if err != nil {
		t.Fatal(err)
	}
	k.Note = "只改备注"
	if _, err := SaveKeg(ctx, pool, k); err != nil {
		t.Fatal(err)
	}
	if n := ledgerCount(t, pool, platform.OpStock); n != 1 {
		t.Fatalf("改 metadata 凭空记了库存流水: %d", n)
	}

	now := time.Now()
	k.TappedAt = &now
	if _, err := SaveKeg(ctx, pool, k); err != nil {
		t.Fatal(err)
	}
	if n := ledgerCount(t, pool, platform.OpTap); n != 1 {
		t.Fatalf("编辑开桶流水 %d 条，应为 1", n)
	}

	k, err = GetKeg(ctx, pool, id)
	if err != nil {
		t.Fatal(err)
	}
	k.FinishedAt = &now
	if _, err := SaveKeg(ctx, pool, k); err != nil {
		t.Fatal(err)
	}
	if n := ledgerCount(t, pool, platform.OpUntap); n != 1 {
		t.Fatalf("编辑结桶流水 %d 条，应为 1", n)
	}

	k, err = GetKeg(ctx, pool, id)
	if err != nil {
		t.Fatal(err)
	}
	k.Note = "结桶后改备注"
	if _, err := SaveKeg(ctx, pool, k); err != nil {
		t.Fatal(err)
	}
	if n := ledgerCount(t, pool, platform.OpUntap); n != 1 || ledgerCount(t, pool, platform.OpTap) != 1 {
		t.Fatalf("没有状态变化却写了流水: tap=%d untap=%d", ledgerCount(t, pool, platform.OpTap), ledgerCount(t, pool, platform.OpUntap))
	}
}
