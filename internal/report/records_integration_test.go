package report

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
	"pourops/internal/pos"
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
	o, err := pos.LoadOrder(context.Background(), pool, orderID)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, len(o.Unpaid))
	for _, it := range o.Unpaid {
		ids = append(ids, it.ID)
	}
	return ids
}

// TestRecordOrdersAggregateSplitAndSnapshot 验证拆单后列表金额不因 join 翻倍、
// 每笔结账只归属自己结掉的行，并保留下单时的名称/杯型/单价快照。
func TestRecordOrdersAggregateSplitAndSnapshot(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := platform.WithActor(context.Background(), "店长阿明")
	keg := seedKeg(t, pool, 4500)
	retailID := seedRetail(t, pool, 3000, 800, 3)
	order, err := pos.EnsureOrder(ctx, pool, "bar-2")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := pos.AddOrderItem(ctx, pool, order, "beer", keg, "测试酒厂 · 测试IPA", 4500, 330); err != nil {
			t.Fatal(err)
		}
	}
	if err := pos.AddOrderItem(ctx, pool, order, "retail", retailID, "薯片 原味", 3000, 0); err != nil {
		t.Fatal(err)
	}
	execSQL(t, pool, `update retail_items set name='改过的薯片', price_cents=9900 where id=$1`, retailID)

	var beerLine int64
	if err := pool.QueryRow(ctx, `select id from order_items where order_id=$1 and kind='beer'`, order).Scan(&beerLine); err != nil {
		t.Fatal(err)
	}
	pay1, err := pos.PayItems(ctx, pool, order, []int64{beerLine})
	if err != nil {
		t.Fatalf("拆单结账失败: %v", err)
	}

	recs, hasNext, err := ListRecordOrders(ctx, pool, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if hasNext {
		t.Fatal("只有一张单却报告还有下一页")
	}
	var rec RecordOrder
	for _, o := range recs {
		if o.ID == order {
			rec = o
		}
	}
	if rec.ID != order {
		t.Fatalf("列表里找不到订单 %d", order)
	}
	if rec.Total != 12000 || rec.Paid != 9000 || rec.Pending != 3000 || rec.Status != "partial" {
		t.Fatalf("聚合不对: total=%d paid=%d pending=%d status=%s", rec.Total, rec.Paid, rec.Pending, rec.Status)
	}

	partial, _, err := ListRecordOrders(ctx, pool, "partial", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) != 1 || partial[0].ID != order {
		t.Fatalf("部分结筛选应只返回订单 %d，实际 %+v", order, partial)
	}
	if settled, _, err := ListRecordOrders(ctx, pool, "settled", 1); err != nil || len(settled) != 0 {
		t.Fatalf("未结完却出现在已结筛选: %+v err=%v", settled, err)
	}

	o, checkouts, unpaid, unlinked, err := LoadRecordDetail(ctx, pool, order)
	if err != nil {
		t.Fatal(err)
	}
	if o.Total != 12000 || o.Paid != 9000 || o.Pending != 3000 || o.Status != "partial" {
		t.Fatalf("详情汇总不对: %+v", o)
	}
	if len(checkouts) != 1 || len(unpaid) != 1 || len(unlinked) != 0 {
		t.Fatalf("应各有 1 笔结账、1 条未结、0 条无法关联，实际 checkouts=%d unpaid=%d unlinked=%d", len(checkouts), len(unpaid), len(unlinked))
	}
	p := checkouts[0].Payment
	if p.ID != pay1 || p.Amount != 9000 || p.Items != 2 {
		t.Fatalf("结账记录不对: %+v", p)
	}
	if p.Actor != "店长阿明" || p.ActorText() != "店长阿明" {
		t.Fatalf("结账应记下操作者快照，实际 actor=%q text=%q", p.Actor, p.ActorText())
	}
	paid := checkouts[0].Items
	if len(paid) != 1 {
		t.Fatalf("这笔结账应只归属 1 行，实际 %d", len(paid))
	}
	if it := paid[0]; it.Name != "测试酒厂 · 测试IPA" || it.UnitCents != 4500 || it.Qty != 2 || it.ML != 330 || it.Amount() != 9000 {
		t.Fatalf("已结明细快照不对: %+v", it)
	}
	if it := paid[0]; it.PaymentID == nil || *it.PaymentID != pay1 || it.PaidAt == nil {
		t.Fatalf("已结明细没有归到支付 %d: %+v", pay1, it)
	}
	if it := unpaid[0]; it.Name != "薯片 原味" || it.UnitCents != 3000 || it.Qty != 1 || it.Amount() != 3000 {
		t.Fatalf("未结明细应保留下单时快照: %+v", it)
	}
	if unpaid[0].PaymentID != nil || unpaid[0].PaidAt != nil {
		t.Fatalf("未结明细不该带支付归属: %+v", unpaid[0])
	}

	if _, err := pos.PayItems(context.Background(), pool, order, unpaidIDs(t, pool, order)); err != nil {
		t.Fatalf("结清剩余商品失败: %v", err)
	}
	o, checkouts, unpaid, unlinked, err = LoadRecordDetail(ctx, pool, order)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != "settled" || o.Paid != 12000 || o.Pending != 0 || len(checkouts) != 2 || len(unpaid) != 0 || len(unlinked) != 0 {
		t.Fatalf("结清后不对: order=%+v checkouts=%d unpaid=%d unlinked=%d", o, len(checkouts), len(unpaid), len(unlinked))
	}
	if got := qInt(t, pool, `select count(*)::int from payments where order_id=$1 and actor='店长阿明'`, order); got != 1 {
		t.Fatalf("应有一笔支付记下操作者，实际 %d", got)
	}
	if got := qInt(t, pool, `select count(*)::int from payments where order_id=$1 and actor=''`, order); got != 1 {
		t.Fatalf("应有一笔支付没有操作者快照，实际 %d", got)
	}
}

func TestRecordZeroPaymentAndUnlinkedHistoricalRow(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	order, err := pos.EnsureOrder(ctx, pool, "table-3")
	if err != nil {
		t.Fatal(err)
	}
	freeID := seedRetail(t, pool, 0, 0, 3)
	if err := pos.AddOrderItem(ctx, pool, order, "retail", freeID, "赠品 杯垫", 0, 0); err != nil {
		t.Fatal(err)
	}
	keg := seedKeg(t, pool, 4500)
	if err := pos.AddOrderItem(ctx, pool, order, "beer", keg, "测试酒厂 · 测试IPA", 4500, 330); err != nil {
		t.Fatal(err)
	}
	var freeLine int64
	if err := pool.QueryRow(ctx, `select id from order_items where order_id=$1 and kind='retail'`, order).Scan(&freeLine); err != nil {
		t.Fatal(err)
	}
	if _, err := pos.PayItems(ctx, pool, order, []int64{freeLine}); err != nil {
		t.Fatalf("零元结账失败: %v", err)
	}

	o, checkouts, unpaid, unlinked, err := LoadRecordDetail(ctx, pool, order)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != "partial" || o.Paid != 0 || o.Pending != 4500 {
		t.Fatalf("零元支付也算部分结: %+v", o)
	}
	if len(checkouts) != 1 || checkouts[0].Payment.Amount != 0 {
		t.Fatalf("零元结账应单独一条支付: %+v", checkouts)
	}
	if len(unpaid) != 1 || unpaid[0].Kind != "beer" || len(unlinked) != 0 {
		t.Fatalf("未结/无法关联列表不对: unpaid=%+v unlinked=%+v", unpaid, unlinked)
	}
	if listed, _, err := ListRecordOrders(ctx, pool, "partial", 1); err != nil || len(listed) != 1 || listed[0].ID != order || listed[0].Status != "partial" {
		t.Fatalf("列表也应把零元支付当部分结: %+v err=%v", listed, err)
	}

	beerLine := unpaid[0].ID
	execSQL(t, pool, `update order_items set paid_at=now(), payment_id=999999 where id=$1`, beerLine)
	o, checkouts, unpaid, unlinked, err = LoadRecordDetail(ctx, pool, order)
	if err != nil {
		t.Fatal(err)
	}
	if len(unpaid) != 0 || len(unlinked) != 1 || unlinked[0].ID != beerLine || unlinked[0].Amount() != 4500 || unlinked[0].PaidAt == nil {
		t.Fatalf("无本单支付关联的已付行应单列: unpaid=%+v unlinked=%+v", unpaid, unlinked)
	}
	if len(checkouts) != 1 || checkouts[0].Payment.Amount != 0 {
		t.Fatalf("历史行不应凭空多出一笔结账: %+v", checkouts)
	}
}
