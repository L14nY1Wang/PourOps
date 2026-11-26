package inventory

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/catalog"
	"pourops/internal/platform"
	"pourops/migrations"
)

func invSeedBeer(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`insert into beers (brewery, name, style) values ('测试厂', $1, 'IPA') returning id`, name).Scan(&id); err != nil {
		t.Fatalf("建酒款失败: %v", err)
	}
	return id
}

// 在售 / 未开库存 / 总桶数。
func invKegCounts(t *testing.T, pool *pgxpool.Pool, beerID int64) (active, stock, total int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `select
		count(*) filter (where tapped_at is not null and finished_at is null),
		count(*) filter (where tapped_at is null and finished_at is null),
		count(*) from kegs where beer_id=$1`, beerID).Scan(&active, &stock, &total); err != nil {
		t.Fatalf("统计酒桶失败: %v", err)
	}
	return active, stock, total
}

func invKegID(t *testing.T, pool *pgxpool.Pool, beerID int64, active bool) int64 {
	t.Helper()
	cond := `tapped_at is not null and finished_at is null`
	if !active {
		cond = `tapped_at is null and finished_at is null`
	}
	var id int64
	if err := pool.QueryRow(context.Background(),
		`select id from kegs where beer_id=$1 and `+cond+` order by id limit 1`, beerID).Scan(&id); err != nil {
		t.Fatalf("找不到酒桶: %v", err)
	}
	return id
}

// 重复点「上架」不能多开桶；下架只结在售那桶，库存桶留着下次开。
func TestTapBeerSingleActiveKeg(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	beerID := invSeedBeer(t, pool, "测试 IPA")

	if err := PurchaseKegs(ctx, pool, beerID, 3, 20000, 50000); err != nil {
		t.Fatalf("进货失败: %v", err)
	}
	if err := TapBeer(ctx, pool, beerID, "A1", 20000, 60000); err != nil {
		t.Fatalf("上架失败: %v", err)
	}
	if active, stock, total := invKegCounts(t, pool, beerID); active != 1 || stock != 2 || total != 3 {
		t.Fatalf("首次上架后 在售/库存/总数 = %d/%d/%d，应为 1/2/3", active, stock, total)
	}
	if err := TapBeer(ctx, pool, beerID, "A1", 20000, 60000); err != nil {
		t.Fatalf("再次上架失败: %v", err)
	}
	if active, stock, total := invKegCounts(t, pool, beerID); active != 1 || stock != 2 || total != 3 {
		t.Fatalf("重复上架开出了多余的桶: 在售/库存/总数 = %d/%d/%d，应为 1/2/3", active, stock, total)
	}

	activeKegs, err := ListKegs(ctx, pool, true)
	if err != nil {
		t.Fatalf("ListKegs 失败: %v", err)
	}
	if len(activeKegs) != 1 {
		t.Fatalf("在售桶 = %d，只该算已开未停售的 1 桶", len(activeKegs))
	}

	if err := UntapBeer(ctx, pool, beerID); err != nil {
		t.Fatalf("下架失败: %v", err)
	}
	if active, stock, total := invKegCounts(t, pool, beerID); active != 0 || stock != 2 || total != 3 {
		t.Fatalf("下架后 在售/库存/总数 = %d/%d/%d，库存桶不该被报废", active, stock, total)
	}
	if err := TapBeer(ctx, pool, beerID, "A1", 20000, 60000); err != nil {
		t.Fatalf("重新上架失败: %v", err)
	}
	if active, stock, total := invKegCounts(t, pool, beerID); active != 1 || stock != 1 || total != 3 {
		t.Fatalf("重新上架应开库存桶而不是新建: 在售/库存/总数 = %d/%d/%d，应为 1/1/3", active, stock, total)
	}
}

// 只有已开未结的桶能记售出；未开、已结、负数一律拒绝。
func TestAddPourOnlyOnActiveKeg(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	beerID := invSeedBeer(t, pool, "测试世涛")

	if err := PurchaseKegs(ctx, pool, beerID, 1, 20000, 50000); err != nil {
		t.Fatalf("进货失败: %v", err)
	}
	stockKeg := invKegID(t, pool, beerID, false)
	if err := AddPour(ctx, pool, stockKeg, 1, 330, 4500); err == nil {
		t.Fatal("未开桶不该能记售出")
	}

	if err := TapBeer(ctx, pool, beerID, "B2", 20000, 60000); err != nil {
		t.Fatalf("上架失败: %v", err)
	}
	kegID := invKegID(t, pool, beerID, true)
	if err := AddPour(ctx, pool, kegID, 1, 330, 4500); err != nil {
		t.Fatalf("在售桶记售出失败: %v", err)
	}
	for _, bad := range []struct {
		cups, ml, amount int
	}{{0, 330, 4500}, {1, -330, 4500}, {1, 330, -1}, {1, 0, 0}} {
		if err := AddPour(ctx, pool, kegID, bad.cups, bad.ml, bad.amount); err == nil {
			t.Errorf("非法售出 %+v 应该被拒绝", bad)
		}
	}

	if err := UntapBeer(ctx, pool, beerID); err != nil {
		t.Fatalf("下架失败: %v", err)
	}
	if err := AddPour(ctx, pool, kegID, 1, 330, 4500); err == nil {
		t.Fatal("已结桶不该能记售出")
	}
	pours, err := ListPours(ctx, pool, kegID)
	if err != nil {
		t.Fatalf("查售出失败: %v", err)
	}
	if len(pours) != 1 || pours[0].ML != 330 || pours[0].AmountCents != 4500 {
		t.Fatalf("只该留下那笔合法售出，实际 %+v", pours)
	}
}

// 保存零售商品时，库存变化要落成真实流水；只改名字不记流水。
func TestSaveRetailRecordsStockMoves(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()

	id, _, err := SaveRetail(ctx, pool, RetailItem{
		Name: "测试罐装", Category: "罐装", PriceCents: 3000, CostCents: 2000, Stock: 5, Active: true,
	})
	if err != nil {
		t.Fatalf("新建商品失败: %v", err)
	}
	moves, err := ListMoves(ctx, pool, id, 10)
	if err != nil {
		t.Fatalf("查流水失败: %v", err)
	}
	if len(moves) != 1 || moves[0].Delta != 5 || moves[0].Reason != "入库" {
		t.Fatalf("新建带库存应记一条 +5 入库，实际 %+v", moves)
	}

	it, err := GetRetail(ctx, pool, id)
	if err != nil {
		t.Fatalf("读商品失败: %v", err)
	}
	it.Stock = 2
	if _, low, err := SaveRetail(ctx, pool, it); err != nil {
		t.Fatalf("改库存失败: %v", err)
	} else if low == nil || low.Stock != 2 {
		t.Fatalf("库存降至 %d 应返回低库存告警，实际 %+v", it.Stock, low)
	}
	moves, _ = ListMoves(ctx, pool, id, 10)
	if len(moves) != 2 || moves[0].Delta != -3 {
		t.Fatalf("库存 5→2 应记一条 -3，实际 %+v", moves)
	}

	it.Stock = 0
	if _, _, err := SaveRetail(ctx, pool, it); err != nil {
		t.Fatalf("清零库存失败: %v", err)
	}
	got, err := GetRetail(ctx, pool, id)
	if err != nil {
		t.Fatalf("读商品失败: %v", err)
	}
	if got.Active || got.Stock != 0 {
		t.Fatalf("零库存应自动下架: active=%v stock=%d", got.Active, got.Stock)
	}

	moves, _ = ListMoves(ctx, pool, id, 10)
	before := len(moves)
	it.Name = "测试罐装（改名）"
	if _, _, err := SaveRetail(ctx, pool, it); err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	moves, _ = ListMoves(ctx, pool, id, 10)
	if len(moves) != before {
		t.Fatalf("库存没变不该记流水: %d → %d 条", before, len(moves))
	}
}

// 后台手工编辑不能绕过单在售桶：SaveKeg 与 TapBeer 共用同一把锁和同一条规则。
func TestSaveKegCannotOpenSecondActiveKeg(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	beerID := invSeedBeer(t, pool, "测试波特")
	now := time.Now()

	firstID, err := SaveKeg(ctx, pool, Keg{BeerID: beerID, CapacityML: 20000, CostCents: 50000, TappedAt: &now})
	if err != nil {
		t.Fatalf("新建在售桶失败: %v", err)
	}
	if _, err := SaveKeg(ctx, pool, Keg{BeerID: beerID, CapacityML: 20000, CostCents: 50000, TappedAt: &now}); err == nil {
		t.Fatal("手工新建第二桶在售应该被拒绝")
	}
	if err := TapBeer(ctx, pool, beerID, "C3", 20000, 60000); err != nil {
		t.Fatalf("上架失败: %v", err)
	}
	if active, _, total := invKegCounts(t, pool, beerID); active != 1 || total != 1 {
		t.Fatalf("上架后 在售/总数 = %d/%d，应为 1/1", active, total)
	}

	stockID, err := SaveKeg(ctx, pool, Keg{BeerID: beerID, CapacityML: 20000, CostCents: 50000, PlacedAt: &now})
	if err != nil {
		t.Fatalf("新建库存桶失败: %v", err)
	}
	if _, err := SaveKeg(ctx, pool, Keg{ID: stockID, BeerID: beerID, CapacityML: 20000, CostCents: 50000, PlacedAt: &now, TappedAt: &now}); err == nil {
		t.Fatal("库存桶手工填开桶时间应该被拒绝")
	}
	if _, err := SaveKeg(ctx, pool, Keg{ID: firstID, BeerID: beerID, CapacityML: 20000, CostCents: 50000, TappedAt: &now, Note: "改个备注"}); err != nil {
		t.Fatalf("在售桶自己再保存不该被自己挡住: %v", err)
	}
	if active, stock, total := invKegCounts(t, pool, beerID); active != 1 || stock != 1 || total != 2 {
		t.Fatalf("在售/库存/总数 = %d/%d/%d，应为 1/1/2", active, stock, total)
	}

	// 负成本/售价挡在保存这一步，否则结账时销售校验会把整单拒掉。
	if _, err := SaveKeg(ctx, pool, Keg{BeerID: beerID, CapacityML: 20000, CostCents: -1}); err == nil {
		t.Fatal("负成本应该被拒绝")
	}
	if _, _, err := SaveRetail(ctx, pool, RetailItem{Name: "负价商品", PriceCents: -1, Stock: 1}); err == nil {
		t.Fatal("负售价应该被拒绝")
	}
}

// 有售出记录的酒款禁止删除，以免 cascade 抹掉历史营收；无售出记录的酒款可正常删除。
func TestDeleteBeerForbiddenWhenHasPours(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	svc, _, _ := newTestService(t, pool)
	ctx := platform.WithActor(context.Background(), "boss")

	soldBeerID := invSeedBeer(t, pool, "有售出禁止删")
	if err := PurchaseKegs(ctx, pool, soldBeerID, 1, 20000, 100000); err != nil {
		t.Fatal(err)
	}
	if err := TapBeer(ctx, pool, soldBeerID, "01", 20000, 100000); err != nil {
		t.Fatal(err)
	}
	kegID := invKegID(t, pool, soldBeerID, true)
	if err := AddPour(ctx, pool, kegID, 1, 330, 4500); err != nil {
		t.Fatal(err)
	}

	if err := catalog.DeleteBeer(ctx, pool, soldBeerID); err == nil {
		t.Fatal("有售出记录的酒款不应被删除")
	} else if msg, ok := platform.UserErrorMessage(err); !ok || msg != "已有酒桶或售出记录的酒款不能删除，下架保留即可" {
		t.Fatalf("错误信息不对: ok=%v err=%v", ok, err)
	}

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/beers/%d/delete", soldBeerID), nil)
	req.SetPathValue("id", fmt.Sprintf("%d", soldBeerID))
	rec := httptest.NewRecorder()
	svc.DeleteBeerHandler(rec, req)
	_, errFlash := platform.ReadFlashCookies(rec.Result().Cookies())
	if rec.Code != http.StatusSeeOther || !strings.Contains(errFlash, "已有酒桶或售出记录的酒款不能删除") {
		t.Fatalf("HTTP 删除应带 Flash 错误提示，实际 code=%d errFlash=%q loc=%q", rec.Code, errFlash, rec.Header().Get("Location"))
	}

	// 验证酒款与售出记录完好保留。
	if _, err := catalog.GetBeer(ctx, pool, soldBeerID); err != nil {
		t.Fatalf("酒款不应被删: %v", err)
	}
	pours, err := ListPours(ctx, pool, kegID)
	if err != nil || len(pours) != 1 {
		t.Fatalf("售出记录不应被删: len=%d err=%v", len(pours), err)
	}

	// 不存在的酒款返回 UserErr。
	if err := catalog.DeleteBeer(ctx, pool, 999999); err == nil {
		t.Fatal("不存在的酒款应返回错误")
	} else if msg, ok := platform.UserErrorMessage(err); !ok || msg != "酒款不存在" {
		t.Fatalf("不存在酒款错误不对: ok=%v err=%v", ok, err)
	}

	// 仅进货未开桶（有 kegs 行但 0 pours）的酒款同样禁止删除，防止级联丢成本。
	unusedBeerID := invSeedBeer(t, pool, "有未开桶禁删")
	if err := PurchaseKegs(ctx, pool, unusedBeerID, 1, 20000, 100000); err != nil {
		t.Fatal(err)
	}
	if err := catalog.DeleteBeer(ctx, pool, unusedBeerID); err == nil {
		t.Fatal("存在进货未开桶记录的酒款不应允许删除")
	}

	// 没有任何酒桶记录的空酒款允许删除并写入 OpStock 流水。
	cleanBeerID := invSeedBeer(t, pool, "无桶可删")
	if err := catalog.DeleteBeer(ctx, pool, cleanBeerID); err != nil {
		t.Fatalf("无任何酒桶记录酒款应允许删除: %v", err)
	}
}
