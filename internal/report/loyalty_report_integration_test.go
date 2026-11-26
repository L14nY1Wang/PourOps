package report

import (
	"context"
	"testing"
	"time"

	"pourops/internal/loyalty"
	"pourops/internal/platform"
	"pourops/internal/pos"
	"pourops/internal/ui"
	"pourops/migrations"
)

func TestLoyaltyReportReconciliationAndOrderRecords(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	mgrCtx := platform.WithActorRole(platform.WithActor(context.Background(), "mgr1"), ui.RoleManager)

	keg := seedKeg(t, pool, 5800)
	retail := seedRetail(t, pool, 3000, 1000, 10)
	memberID, err := loyalty.QuickCreateMember(mgrCtx, pool, "测试会员", "13800001111")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loyalty.RechargeMember(mgrCtx, pool, memberID, 20000, 2000, "qr", "测试充值", "rc-rep-1"); err != nil {
		t.Fatal(err)
	}
	plans, err := loyalty.ListPassPlans(mgrCtx, pool, true)
	if err != nil || len(plans) == 0 {
		t.Fatal(err)
	}
	if _, err := loyalty.IssuePass(mgrCtx, pool, memberID, plans[0].ID, "cash", "买月卡", "ps-rep-1"); err != nil {
		t.Fatal(err)
	}

	orderID, err := pos.EnsureOrder(mgrCtx, pool, "bar-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := pos.BindOrderMember(mgrCtx, pool, orderID, memberID); err != nil {
		t.Fatal(err)
	}
	if err := pos.AddOrderItem(mgrCtx, pool, orderID, "beer", keg, "测试酒厂 · 测试IPA", 5800, 330); err != nil {
		t.Fatal(err)
	}
	if err := pos.AddOrderItem(mgrCtx, pool, orderID, "retail", retail, "薯片 原味", 3000, 0); err != nil {
		t.Fatal(err)
	}
	ids := unpaidIDs(t, pool, orderID)

	// 月卡免 5800 啤酒 + 85折折让零售 3000 -> 折450 -> 实收 2550
	var rule85 int64
	if err := pool.QueryRow(mgrCtx, `select id from discount_rules where allow_staff=false and kind='percent' limit 1`).Scan(&rule85); err != nil {
		t.Fatal(err)
	}
	res, err := pos.PayItemsWithOptions(mgrCtx, pool, pos.ManualGateway{}, orderID, ids, pos.PayOptions{
		MemberID:       &memberID,
		UsePass:        true,
		DiscountRuleID: &rule85,
		Method:         "balance",
		IdempotencyKey: "pay-rep-1",
	})
	if err != nil {
		t.Fatalf("结账失败: %v", err)
	}
	if res.AmountCents != 2550 || res.PassDiscountCents != 5800 || res.ManualDiscountCents != 450 {
		t.Fatalf("res = %+v", res)
	}

	now := time.Now()
	start := platform.BusinessDayStart(now)
	end := start.AddDate(0, 0, 1)
	tx, err := beginReadOnlyTimeoutTx(mgrCtx, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(mgrCtx)

	sales, err := LoadSalesInsights(mgrCtx, tx, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if sales.Current.Revenue != 2550 || sales.Retail.Revenue != 2550 || sales.Draft.Revenue != 0 {
		t.Fatalf("营业额应只统计折后商品净收入 2550（不含充值20000和月卡29900），实际 %+v", sales)
	}
	if len(sales.Top) != 2 || sales.Top[0].Revenue+sales.Top[1].Revenue != 2550 {
		t.Fatalf("商品榜营收合计应与净营业额 2550 严格对账，实际 %+v", sales.Top)
	}

	li, err := LoadLoyaltyInsights(mgrCtx, tx, start, end, now)
	if err != nil {
		t.Fatal(err)
	}
	if li.DiscountTotal != 6250 || li.PassCupsRedeemed != 1 || li.BalanceConsumedCents != 2550 ||
		li.ExternalReceivedCents != 0 || li.RechargeCashCents != 20000 || li.RechargeBonusCents != 2000 ||
		li.PassSalesCashCents != int64(plans[0].PriceCents) || li.TotalCashInflowCents() != 20000+int64(plans[0].PriceCents) {
		t.Fatalf("会员预收与优惠统计不对: %+v", li)
	}
	_ = tx.Rollback(mgrCtx)

	recOrd, checkouts, _, _, err := LoadRecordDetail(mgrCtx, pool, orderID)
	if err != nil {
		t.Fatal(err)
	}
	if recOrd.MemberName != "测试会员" || recOrd.Gross != 8800 || recOrd.Discount != 6250 || recOrd.Total != 2550 ||
		len(checkouts) != 1 || checkouts[0].Payment.BalancePaid != 2550 || checkouts[0].Payment.PassCups != 1 {
		t.Fatalf("订单详情报表记录不对: order=%+v checkouts=%+v", recOrd, checkouts)
	}
}
