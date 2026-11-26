package pos

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/event"
	"pourops/internal/inventory"
	"pourops/internal/platform"
	"pourops/migrations"
)

func qInt(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var v int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

func execSQL(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func seedKeg(t *testing.T, pool *pgxpool.Pool, price330 int) int64 {
	t.Helper()
	var beerID, kegID int64
	if err := pool.QueryRow(context.Background(), `insert into beers (brewery, name, price_330, on_screen)
		values ('测试酒厂','测试IPA',$1,true) returning id`, price330).Scan(&beerID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `insert into kegs (beer_id, capacity_ml, cost_cents, placed_at, tapped_at)
		values ($1, 20000, 0, now(), now()) returning id`, beerID).Scan(&kegID); err != nil {
		t.Fatal(err)
	}
	return kegID
}

func seedRetail(t *testing.T, pool *pgxpool.Pool, price, cost, stock int) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `insert into retail_items (name, spec, price_cents, cost_cents, stock, active)
		values ('薯片','原味',$1,$2,$3,true) returning id`, price, cost, stock).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func unpaidIDs(t *testing.T, pool *pgxpool.Pool, orderID int64) []int64 {
	t.Helper()
	o, err := LoadOrder(context.Background(), pool, orderID)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, len(o.Unpaid))
	for _, it := range o.Unpaid {
		ids = append(ids, it.ID)
	}
	return ids
}

// TestPayOrderAtomicAndIdempotent 一次结账要么全成、要么全不成；重复提交不会再卖一遍。
func TestPayOrderAtomicAndIdempotent(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	keg := seedKeg(t, pool, 4500)
	item := seedRetail(t, pool, 3000, 800, 2)
	order, err := EnsureOrder(ctx, pool, "bar-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := AddOrderItem(ctx, pool, order, "beer", keg, "测试酒厂 · 测试IPA", 4500, 330); err != nil {
		t.Fatal(err)
	}
	if err := AddOrderItem(ctx, pool, order, "retail", item, "薯片 原味", 3000, 0); err != nil {
		t.Fatal(err)
	}
	// 下单后店主改价：这单仍按下单价，成本按现在数据库里的值。
	execSQL(t, pool, `update retail_items set price_cents=9900, cost_cents=1000 where id=$1`, item)

	ids := unpaidIDs(t, pool, order)
	if len(ids) != 2 {
		t.Fatalf("未结行应该 2 行，实际 %d", len(ids))
	}
	payID, err := PayItems(ctx, pool, order, ids)
	if err != nil {
		t.Fatalf("结账失败: %v", err)
	}
	if got := qInt(t, pool, `select coalesce(sum(ml),0)::int from keg_pours where keg_id=$1`, keg); got != 330 {
		t.Fatalf("毫升应该 330，实际 %d", got)
	}
	if got := qInt(t, pool, `select coalesce(sum(amount_cents),0)::int from keg_pours where keg_id=$1`, keg); got != 4500 {
		t.Fatalf("酒水金额应该 4500，实际 %d", got)
	}
	if got := qInt(t, pool, `select amount_cents from payments where id=$1`, payID); got != 7500 {
		t.Fatalf("整单金额应该 7500，实际 %d", got)
	}
	if got := qInt(t, pool, `select items_count from payments where id=$1`, payID); got != 2 {
		t.Fatalf("件数应该 2，实际 %d", got)
	}
	if got := qInt(t, pool, `select total_cents from retail_sales`); got != 3000 {
		t.Fatalf("零售营收应按单行单价 3000，实际 %d", got)
	}
	if got := qInt(t, pool, `select cost_cents from retail_sales`); got != 1000 {
		t.Fatalf("零售成本应取数据库当前值 1000，实际 %d", got)
	}
	if got := qInt(t, pool, `select stock from retail_items where id=$1`, item); got != 1 {
		t.Fatalf("库存应该剩 1，实际 %d", got)
	}
	o, err := LoadOrder(ctx, pool, order)
	if err != nil {
		t.Fatal(err)
	}
	if !o.Closed || len(o.Unpaid) != 0 || len(o.Paid) != 2 {
		t.Fatalf("结完应该关单且 2 行已结，实际 closed=%v unpaid=%d paid=%d", o.Closed, len(o.Unpaid), len(o.Paid))
	}

	// 收银员点了两次提交：第二次必须什么都不做。
	if _, err := PayItems(ctx, pool, order, ids); err == nil {
		t.Fatal("已结的单还能再结一次")
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`select count(*)::int from keg_pours`, nil},
		{`select count(*)::int from payments`, nil},
		{`select count(*)::int from retail_sales`, nil},
		{`select count(*)::int from retail_moves where reason='售出'`, nil},
	} {
		if got := qInt(t, pool, q.sql, q.args...); got != 1 {
			t.Fatalf("重复结账写多了：%s = %d", q.sql, got)
		}
	}
}

// TestPayOrderRollsBackOnShortStock 中途库存不够，整单回滚，酒也不能算卖掉。
func TestPayOrderRollsBackOnShortStock(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	keg := seedKeg(t, pool, 4500)
	item := seedRetail(t, pool, 3000, 1000, 1)
	order, err := EnsureOrder(ctx, pool, "table-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := AddOrderItem(ctx, pool, order, "beer", keg, "测试酒厂 · 测试IPA", 4500, 330); err != nil {
		t.Fatal(err)
	}
	if err := AddOrderItem(ctx, pool, order, "retail", item, "薯片 原味", 3000, 0); err != nil {
		t.Fatal(err)
	}
	execSQL(t, pool, `update retail_items set stock=0 where id=$1`, item)

	if _, err := PayItems(ctx, pool, order, unpaidIDs(t, pool, order)); err == nil {
		t.Fatal("库存不够还能结账")
	}
	if got := qInt(t, pool, `select count(*)::int from keg_pours where keg_id=$1`, keg); got != 0 {
		t.Fatalf("回滚后不该有酒水流水，实际 %d", got)
	}
	if got := qInt(t, pool, `select count(*)::int from payments`); got != 0 {
		t.Fatalf("回滚后不该有支付记录，实际 %d", got)
	}
	if got := qInt(t, pool, `select count(*)::int from order_items where order_id=$1 and paid_at is not null`, order); got != 0 {
		t.Fatalf("回滚后不该有已结行，实际 %d", got)
	}
	if got := qInt(t, pool, `select count(*)::int from orders where id=$1 and closed_at is null`, order); got != 1 {
		t.Fatal("回滚后订单应该还开着")
	}

	execSQL(t, pool, `update retail_items set stock=1 where id=$1`, item)
	if _, err := PayItems(ctx, pool, order, unpaidIDs(t, pool, order)); err != nil {
		t.Fatalf("补库存后重试失败: %v", err)
	}
	if got := qInt(t, pool, `select count(*)::int from keg_pours where keg_id=$1`, keg); got != 1 {
		t.Fatalf("重试后酒水流水应为 1，实际 %d", got)
	}
	if got := qInt(t, pool, `select count(*)::int from payments`); got != 1 {
		t.Fatalf("重试后支付应为 1 笔，实际 %d", got)
	}
}

// TestConcurrentOrdersCompeteForLastRetail 两张单抢最后一件零售：只有一单能成，输的那单连酒水一起回滚。
func TestConcurrentOrdersCompeteForLastRetail(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	keg := seedKeg(t, pool, 4500)
	item := seedRetail(t, pool, 3000, 1000, 1)
	var orders []int64
	for _, key := range []string{"bar-1", "bar-2"} {
		id, err := EnsureOrder(ctx, pool, key)
		if err != nil {
			t.Fatal(err)
		}
		if err := AddOrderItem(ctx, pool, id, "beer", keg, "测试酒厂 · 测试IPA", 4500, 330); err != nil {
			t.Fatal(err)
		}
		if err := AddOrderItem(ctx, pool, id, "retail", item, "薯片 原味", 3000, 0); err != nil {
			t.Fatal(err)
		}
		orders = append(orders, id)
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(orders))
	for _, id := range orders {
		ids := unpaidIDs(t, pool, id) // t.Fatal 只能在测试主 goroutine 里调
		wg.Add(1)
		go func(id int64, ids []int64) {
			defer wg.Done()
			_, err := PayItems(ctx, pool, id, ids)
			errs <- err
		}(id, ids)
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("抢最后一件零售成功了 %d 单", ok)
	}
	if got := qInt(t, pool, `select stock from retail_items where id=$1`, item); got != 0 {
		t.Fatalf("库存应剩 0，实际 %d", got)
	}
	if got := qInt(t, pool, `select count(*)::int from payments`); got != 1 {
		t.Fatalf("应只有 1 笔支付，实际 %d", got)
	}
	if got := qInt(t, pool, `select count(*)::int from keg_pours where keg_id=$1`, keg); got != 1 {
		t.Fatalf("失败那单的酒水没回滚，卖出 %d 笔", got)
	}
}

// TestPayOrderIgnoresForeignItems 别的单的行 id 混进勾选，不能被结掉。
func TestPayOrderIgnoresForeignItems(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	kegA := seedKeg(t, pool, 4500)
	kegB := seedKeg(t, pool, 4500)
	a, err := EnsureOrder(ctx, pool, "bar-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := EnsureOrder(ctx, pool, "bar-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := AddOrderItem(ctx, pool, a, "beer", kegA, "A", 4500, 330); err != nil {
		t.Fatal(err)
	}
	if err := AddOrderItem(ctx, pool, b, "beer", kegB, "B", 4500, 330); err != nil {
		t.Fatal(err)
	}
	aIDs := unpaidIDs(t, pool, a)
	bIDs := unpaidIDs(t, pool, b)

	if _, err := PayItems(ctx, pool, a, bIDs); err == nil {
		t.Fatal("拿别的单的行结账居然成功了")
	}
	if got := qInt(t, pool, `select count(*)::int from order_items where order_id=$1 and paid_at is null`, b); got != 1 {
		t.Fatal("别的单的行被污染了")
	}
	if got := qInt(t, pool, `select count(*)::int from keg_pours where keg_id=$1`, kegB); got != 0 {
		t.Fatalf("别的单的酒被卖出去了：%d", got)
	}

	if _, err := PayItems(ctx, pool, a, append(append([]int64{}, aIDs...), bIDs...)); err != nil {
		t.Fatalf("结账失败: %v", err)
	}
	if got := qInt(t, pool, `select count(*)::int from order_items where order_id=$1 and paid_at is null`, b); got != 1 {
		t.Fatal("混进来的外单行被标成已结")
	}
	if got := qInt(t, pool, `select count(*)::int from keg_pours where keg_id=$1`, kegB); got != 0 {
		t.Fatalf("外单的酒被卖掉了：%d", got)
	}
	if got := qInt(t, pool, `select count(*)::int from orders where id=$1 and closed_at is null`, b); got != 1 {
		t.Fatal("外单被关掉了")
	}
}

// TestOrderLinesSeparateCupSizes 同价不同杯型不合并；more/less 只能改本单未结行。
func TestOrderLinesSeparateCupSizes(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	keg := seedKeg(t, pool, 4500)
	order, err := EnsureOrder(ctx, pool, "bar-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, ml := range []int{330, 330, 150} {
		if err := AddOrderItem(ctx, pool, order, "beer", keg, "测试酒厂 · 测试IPA", 4500, ml); err != nil {
			t.Fatal(err)
		}
	}
	o, err := LoadOrder(ctx, pool, order)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Unpaid) != 2 {
		t.Fatalf("同价不同杯型应该 2 行，实际 %d", len(o.Unpaid))
	}
	line := map[int]OrderItem{}
	for _, it := range o.Unpaid {
		line[it.ML] = it
	}
	if line[330].Qty != 2 || line[150].Qty != 1 {
		t.Fatalf("份数不对：330=%d 150=%d", line[330].Qty, line[150].Qty)
	}

	other, err := EnsureOrder(ctx, pool, "bar-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := MoreOrderItem(ctx, pool, other, line[150].ID); err != nil {
		t.Fatal(err)
	}
	if err := LessOrderItem(ctx, pool, other, line[150].ID); err != nil {
		t.Fatal(err)
	}
	if got := qInt(t, pool, `select qty from order_items where id=$1`, line[150].ID); got != 1 {
		t.Fatalf("别的单改动了这一行，qty=%d", got)
	}
	if err := MoreOrderItem(ctx, pool, order, line[150].ID); err != nil {
		t.Fatal(err)
	}

	if _, err := PayItems(ctx, pool, order, []int64{line[330].ID}); err != nil {
		t.Fatalf("结账失败: %v", err)
	}
	if got := qInt(t, pool, `select coalesce(sum(ml),0)::int from keg_pours where keg_id=$1`, keg); got != 660 {
		t.Fatalf("卖出毫升应为 660，实际 %d", got)
	}
	if got := qInt(t, pool, `select coalesce(sum(amount_cents),0)::int from keg_pours where keg_id=$1`, keg); got != 9000 {
		t.Fatalf("卖出金额应为 9000，实际 %d", got)
	}
	o, err = LoadOrder(ctx, pool, order)
	if err != nil {
		t.Fatal(err)
	}
	if o.Closed || len(o.Unpaid) != 1 || o.Unpaid[0].ML != 150 || o.Unpaid[0].Qty != 2 {
		t.Fatalf("部分结账后状态不对：closed=%v unpaid=%+v", o.Closed, o.Unpaid)
	}

	if _, err := PayItems(ctx, pool, order, unpaidIDs(t, pool, order)); err != nil {
		t.Fatalf("结清失败: %v", err)
	}
	if err := MoreOrderItem(ctx, pool, order, line[330].ID); err == nil {
		t.Fatal("已结的单还能加量")
	}
	if err := AddOrderItem(ctx, pool, order, "beer", keg, "测试酒厂 · 测试IPA", 4500, 330); err == nil {
		t.Fatal("已结的单还能加购")
	}
}

// TestPaySameOrderConcurrently 同一个单被连点，只能有一笔支付。
func TestPaySameOrderConcurrently(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	keg := seedKeg(t, pool, 4500)
	order, err := EnsureOrder(ctx, pool, "bar-3")
	if err != nil {
		t.Fatal(err)
	}
	if err := AddOrderItem(ctx, pool, order, "beer", keg, "测试酒厂 · 测试IPA", 4500, 330); err != nil {
		t.Fatal(err)
	}
	ids := unpaidIDs(t, pool, order)

	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := PayItems(ctx, pool, order, ids)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("同一单并发结账成功了 %d 次", ok)
	}
	if got := qInt(t, pool, `select count(*)::int from keg_pours where keg_id=$1`, keg); got != 1 {
		t.Fatalf("应只卖出 1 笔，实际 %d", got)
	}
	if got := qInt(t, pool, `select count(*)::int from payments`); got != 1 {
		t.Fatalf("应只有 1 笔支付，实际 %d", got)
	}
}

// TestAddItemConcurrentlyKeepsOneLine 连点加购不能丢更新，也不能多出行。
func TestAddItemConcurrentlyKeepsOneLine(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	keg := seedKeg(t, pool, 4500)
	order, err := EnsureOrder(ctx, pool, "table-1")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- AddOrderItem(ctx, pool, order, "beer", keg, "测试酒厂 · 测试IPA", 4500, 330)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发加购失败: %v", err)
		}
	}
	if got := qInt(t, pool, `select count(*)::int from order_items where order_id=$1`, order); got != 1 {
		t.Fatalf("同一杯型应该只有 1 行，实际 %d", got)
	}
	if got := qInt(t, pool, `select qty from order_items where order_id=$1`, order); got != 8 {
		t.Fatalf("加购丢了更新，qty=%d", got)
	}
}

// TestEnsureOrderConcurrentSameSpot 同一台位并发点开只开一张单；假台位一律拒绝。
func TestEnsureOrderConcurrentSameSpot(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	const n = 6
	ids := make([]int64, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = EnsureOrder(ctx, pool, "table-4")
		}(i)
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("第 %d 个并发开单失败: %v", i, errs[i])
		}
		if ids[i] != ids[0] || ids[i] == 0 {
			t.Fatalf("并发开单拿到了不同的 id：%v", ids)
		}
	}
	if got := qInt(t, pool, `select count(*)::int from orders where spot='table-4' and closed_at is null`); got != 1 {
		t.Fatalf("同一台位开了 %d 张未结单", got)
	}
	for _, key := range []string{"bar-4", "table-5", "vip-1", "bar-0", "bar-01", "bar-x", "bar-1x", "bar"} {
		if _, err := EnsureOrder(ctx, pool, key); err == nil {
			t.Fatalf("假台位 %q 也能开单", key)
		}
	}
}

// TestRetailStockIntegrity 零售：并发撤销只退一次；下架商品卖完还是下架；超扣直接拒绝。
func TestRetailStockIntegrity(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	item := seedRetail(t, pool, 3000, 1200, 5)

	saleID, err := inventory.SellRetail(ctx, pool, []inventory.SaleLine{{
		Item: inventory.RetailItem{ID: item, Name: "薯片 原味", PriceCents: 2500},
		Qty:  2,
	}})
	if err != nil {
		t.Fatalf("零售结账失败: %v", err)
	}
	if got := qInt(t, pool, `select stock from retail_items where id=$1`, item); got != 3 {
		t.Fatalf("库存应剩 3，实际 %d", got)
	}
	if got := qInt(t, pool, `select total_cents from retail_sales where id=$1`, saleID); got != 5000 {
		t.Fatalf("营收应按传入单价 2500×2=5000，实际 %d", got)
	}
	if got := qInt(t, pool, `select cost_cents from retail_sales where id=$1`, saleID); got != 2400 {
		t.Fatalf("成本应为 2400，实际 %d", got)
	}

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = inventory.DeleteSale(ctx, pool, saleID)
		}()
	}
	wg.Wait()
	if got := qInt(t, pool, `select stock from retail_items where id=$1`, item); got != 5 {
		t.Fatalf("并发撤销退了多次库存，剩 %d", got)
	}
	if got := qInt(t, pool, `select count(*)::int from retail_moves where reason='撤销'`); got != 1 {
		t.Fatalf("撤销流水应只有 1 条，实际 %d", got)
	}
	if err := inventory.DeleteSale(ctx, pool, saleID); err != nil {
		t.Fatalf("重复撤销应无害，返回 %v", err)
	}
	if got := qInt(t, pool, `select stock from retail_items where id=$1`, item); got != 5 {
		t.Fatalf("重复撤销又退了库存，剩 %d", got)
	}

	execSQL(t, pool, `update retail_items set active=false, stock=4 where id=$1`, item)
	if _, err := inventory.SellRetail(ctx, pool, []inventory.SaleLine{{
		Item: inventory.RetailItem{ID: item, Name: "薯片 原味", PriceCents: 3000},
		Qty:  1,
	}}); err != nil {
		t.Fatalf("下架商品结账失败: %v", err)
	}
	if got := qInt(t, pool, `select case when active then 1 else 0 end from retail_items where id=$1`, item); got != 0 {
		t.Fatal("下架商品被卖出后自动上架了")
	}

	if _, err := inventory.AddStock(ctx, pool, item, -10, "调整"); err == nil {
		t.Fatal("超扣居然成功了")
	}
	if got := qInt(t, pool, `select stock from retail_items where id=$1`, item); got != 3 {
		t.Fatalf("超扣后库存被改了：%d", got)
	}
	if got := qInt(t, pool, `select count(*)::int from retail_moves where reason='调整'`); got != 0 {
		t.Fatal("超扣失败却写了流水")
	}
	if _, err := inventory.AddStock(ctx, pool, item, -3, "调整"); err != nil {
		t.Fatalf("正常扣减失败: %v", err)
	}
	if got := qInt(t, pool, `select stock from retail_items where id=$1`, item); got != 0 {
		t.Fatalf("库存应为 0，实际 %d", got)
	}
	if got := qInt(t, pool, `select delta from retail_moves where reason='调整' order by id desc limit 1`); got != -3 {
		t.Fatalf("流水 delta 应记真实变动 -3，实际 %d", got)
	}
	if got := qInt(t, pool, `select case when active then 1 else 0 end from retail_items where id=$1`, item); got != 0 {
		t.Fatal("库存归零后应下架")
	}
}

// TestPosAddItemRejectsDelistedBeer 下架（不在电视酒单）的酒，即使桶还开着也不能加点。
func TestPosAddItemRejectsDelistedBeer(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	svc := NewService(pool, nil, nil, nil, nil)
	keg := seedKeg(t, pool, 4500)
	order, err := EnsureOrder(ctx, pool, "bar-1")
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, pool, `update beers set on_screen=false where id=(select beer_id from kegs where id=$1)`, keg)

	add := func() *httptest.ResponseRecorder {
		form := url.Values{"kind": {"beer"}, "ref": {strconv.FormatInt(keg, 10)}, "ml": {"330"}}
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetPathValue("id", strconv.FormatInt(order, 10))
		w := httptest.NewRecorder()
		svc.PosAddItem(w, req)
		return w
	}

	if w := add(); w.Code != http.StatusSeeOther {
		t.Fatalf("下架酒加购状态 = %d, want 303", w.Code)
	} else if _, errMsg := platform.ReadFlashCookies(w.Result().Cookies()); errMsg == "" {
		t.Fatalf("下架酒加购应带 Flash 错误提示，实际空")
	}
	if got := qInt(t, pool, `select count(*)::int from order_items where order_id=$1`, order); got != 0 {
		t.Fatalf("下架酒被加进账单：%d 行", got)
	}

	execSQL(t, pool, `update beers set on_screen=true where id=(select beer_id from kegs where id=$1)`, keg)
	if w := add(); w.Code != http.StatusSeeOther {
		t.Fatalf("上架后加购失败：%d %q", w.Code, w.Header().Get("Location"))
	} else if _, errMsg := platform.ReadFlashCookies(w.Result().Cookies()); errMsg != "" {
		t.Fatalf("上架后加购不应有错误提示：%q", errMsg)
	}
	if got := qInt(t, pool, `select count(*)::int from order_items where order_id=$1`, order); got != 1 {
		t.Fatalf("上架酒没加进账单：%d 行", got)
	}
}

type stubGateway struct {
	err error
}

func (g stubGateway) Name() string { return "stub-gw" }
func (g stubGateway) Settle(context.Context, PaymentRequest) (PaymentResult, error) {
	if g.err != nil {
		return PaymentResult{}, g.err
	}
	return PaymentResult{Provider: "stub-gw", Reference: "ref-1"}, nil
}

type stubPrinter struct {
	err      error
	receipts []Receipt
}

func (p *stubPrinter) Name() string { return "stub-printer" }
func (p *stubPrinter) Print(_ context.Context, r Receipt) error {
	p.receipts = append(p.receipts, r)
	return p.err
}

// TestPaymentGatewayAndPrinterExtensionPoints 验证收款网关失败回滚、打印非阻塞以及 EventOrderPaid/EventRetailLowStock 事件发布。
func TestPaymentGatewayAndPrinterExtensionPoints(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := platform.WithActor(context.Background(), "bar1")
	keg := seedKeg(t, pool, 4500)
	item := seedRetail(t, pool, 3000, 1000, 4)
	order, err := EnsureOrder(ctx, pool, "bar-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := AddOrderItem(ctx, pool, order, "beer", keg, "测试酒厂 · 测试IPA", 4500, 330); err != nil {
		t.Fatal(err)
	}
	if err := AddOrderItem(ctx, pool, order, "retail", item, "薯片 原味", 3000, 0); err != nil {
		t.Fatal(err)
	}
	ids := unpaidIDs(t, pool, order)

	// 1. 网关拒绝必须回滚整笔结账，不留已结记录与流水
	bus := event.NewEventBus()
	var events []event.Event
	var mu sync.Mutex
	bus.Subscribe(func(ev event.Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	printerFail := &stubPrinter{err: errors.New("paper out")}
	svcReject := NewService(pool, nil, bus, stubGateway{err: platform.UserErr("扫码渠道超时")}, printerFail)

	form := url.Values{}
	for _, id := range ids {
		form.Add("items", strconv.FormatInt(id, 10))
	}
	reqFail := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	reqFail = reqFail.WithContext(ctx)
	reqFail.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqFail.SetPathValue("id", strconv.FormatInt(order, 10))
	recFail := httptest.NewRecorder()
	svcReject.PosPay(recFail, reqFail)
	_, failErrMsg := platform.ReadFlashCookies(recFail.Result().Cookies())
	if !strings.Contains(failErrMsg, "扫码渠道超时") {
		t.Fatalf("网关失败应跳转 Flash 错误提示: %q", failErrMsg)
	}
	if got := qInt(t, pool, `select count(*)::int from payments`); got != 0 {
		t.Fatalf("网关失败应回滚支付: %d", got)
	}
	if len(printerFail.receipts) != 0 || len(events) != 0 {
		t.Fatalf("回滚后不应打印或发布事件: receipts=%d events=%d", len(printerFail.receipts), len(events))
	}

	// 2. 网关成功且打印机返回错误时：结账仍成功，小票仍传出，EventOrderPaid 与 EventRetailLowStock 都应发布
	svcOK := NewService(pool, nil, bus, stubGateway{}, printerFail)
	reqOK := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	reqOK = reqOK.WithContext(ctx)
	reqOK.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqOK.SetPathValue("id", strconv.FormatInt(order, 10))
	recOK := httptest.NewRecorder()
	svcOK.PosPay(recOK, reqOK)
	if recOK.Code != http.StatusSeeOther || !strings.HasPrefix(recOK.Header().Get("Location"), "/pos/pay/") {
		t.Fatalf("打印报错不应阻断结账跳转: code=%d loc=%q", recOK.Code, recOK.Header().Get("Location"))
	}
	if len(printerFail.receipts) != 1 || printerFail.receipts[0].AmountCents != 7500 || len(printerFail.receipts[0].Lines) != 2 {
		t.Fatalf("ReceiptPrinter 收到小票不对: %+v", printerFail.receipts)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0].Type != event.EventOrderPaid || events[1].Type != event.EventRetailLowStock {
		t.Fatalf("应发布 EventOrderPaid + EventRetailLowStock，实际 %+v", events)
	}
	if events[0].Actor != "bar1" {
		t.Fatalf("事件 Actor 应为 bar1，实际 %q", events[0].Actor)
	}
}
