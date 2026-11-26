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

	"pourops/internal/loyalty"
	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/internal/ui"
	"pourops/migrations"
	"pourops/web"
)

func TestPosLoyaltyDiscountsPermissionsAndReconciliation(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	staffCtx := platform.WithActorRole(platform.WithActor(context.Background(), "staff1"), ui.RoleStaff)
	mgrCtx := platform.WithActorRole(platform.WithActor(context.Background(), "mgr1"), ui.RoleManager)

	keg := seedKeg(t, pool, 5800)
	retail := seedRetail(t, pool, 3000, 1000, 10)
	memberID, err := loyalty.QuickCreateMember(mgrCtx, pool, "测试会员", "13800001111")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loyalty.RechargeMember(mgrCtx, pool, memberID, 20000, 2000, "qr", "测试充值", "rc-test-1"); err != nil {
		t.Fatal(err)
	}
	plans, err := loyalty.ListPassPlans(mgrCtx, pool, true)
	if err != nil || len(plans) == 0 {
		t.Fatal(err)
	}
	if _, err := loyalty.IssuePass(mgrCtx, pool, memberID, plans[0].ID, "cash", "买月卡", "ps-test-1"); err != nil {
		t.Fatal(err)
	}

	// 找到 85 折（仅店长/BOSS）规则
	var restrictedRuleID int64
	if err := pool.QueryRow(mgrCtx, `select id from discount_rules where allow_staff = false and kind='percent' limit 1`).Scan(&restrictedRuleID); err != nil {
		t.Fatal(err)
	}

	orderID, err := EnsureOrder(staffCtx, pool, "bar-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := BindOrderMember(staffCtx, pool, orderID, memberID); err != nil {
		t.Fatal(err)
	}
	if err := AddOrderItem(staffCtx, pool, orderID, "beer", keg, "测试酒厂 · 测试IPA", 5800, 330); err != nil {
		t.Fatal(err)
	}
	if err := AddOrderItem(staffCtx, pool, orderID, "retail", retail, "薯片 原味", 3000, 0); err != nil {
		t.Fatal(err)
	}
	ids := unpaidIDs(t, pool, orderID)

	// 1. 店员越权选 85 折或自定义折扣必须被后端拒绝
	if _, err := PayItemsWithOptions(staffCtx, pool, ManualGateway{}, orderID, ids, PayOptions{
		DiscountRuleID: &restrictedRuleID,
	}); err == nil {
		t.Fatal("店员不应能用受限 85 折结账")
	}
	if _, err := PayItemsWithOptions(staffCtx, pool, ManualGateway{}, orderID, ids, PayOptions{
		CustomDiscount: &loyalty.CustomDiscount{Kind: "fixed", FixedCents: 500, Scope: "all"},
	}); err == nil {
		t.Fatal("店员不应能用自定义立减结账")
	}

	// 2. 用 HTTP 服务测店员请求被拒、以及店长用月卡+储值结账成功
	renderer := ui.MustRenderer(web.TemplatesFS, nil, func() string { return "PourOps" })
	svc := NewService(pool, renderer, nil, ManualGateway{}, NoopPrinter{})
	svc.SetSettingsStore(settings.NewSettingsStore(pool, "PourOps"))

	form := url.Values{
		"items":                 {strconv.FormatInt(ids[0], 10), strconv.FormatInt(ids[1], 10)},
		"member_id":             {strconv.FormatInt(memberID, 10)},
		"custom_discount_kind":  {"fixed"},
		"custom_discount_value": {"5"},
	}
	reqStaff := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode())).WithContext(staffCtx)
	reqStaff.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqStaff.SetPathValue("id", strconv.FormatInt(orderID, 10))
	recStaff := httptest.NewRecorder()
	svc.PosPreviewPay(recStaff, reqStaff)
	if recStaff.Code != http.StatusBadRequest {
		t.Fatalf("店员请求自定义折扣预览应返回 400，实际 %d body=%s", recStaff.Code, recStaff.Body.String())
	}

	// 店长使用月卡免 1 杯（5800）+ 储值付剩余（3000），营业额应收 3000，让利 5800
	res, err := PayItemsWithOptions(mgrCtx, pool, ManualGateway{}, orderID, ids, PayOptions{
		MemberID:       &memberID,
		UsePass:        true,
		Method:         "balance",
		IdempotencyKey: "pay-loyalty-1",
	})
	if err != nil {
		t.Fatalf("店长结账失败: %v", err)
	}
	if res.GrossCents != 8800 || res.PassDiscountCents != 5800 || res.AmountCents != 3000 ||
		res.BalancePaidCents != 3000 || res.ExternalPaidCents != 0 || res.PassCupsUsed != 1 {
		t.Fatalf("结账结果不对: %+v", res)
	}

	// 校验酒水与零售账目分摊对账
	if got := qInt(t, pool, `select amount_cents from keg_pours where payment_id=$1`, res.PaymentID); got != 0 {
		t.Fatalf("月卡免单的那杯啤酒 keg_pours.amount_cents 应为 0，实际 %d", got)
	}
	if got := qInt(t, pool, `select total_cents from retail_sales`); got != 3000 {
		t.Fatalf("零售实收应为 3000，实际 %d", got)
	}
}

func TestConcurrentBalanceAndPassDailyQuota(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := platform.WithActorRole(platform.WithActor(context.Background(), "boss"), ui.RoleBoss)

	keg := seedKeg(t, pool, 4000)
	memberID, err := loyalty.QuickCreateMember(ctx, pool, "并发会员", "13700002222")
	if err != nil {
		t.Fatal(err)
	}
	// 余额仅够付 1 单（5000 分，每单 4000 分）
	if _, err := loyalty.RechargeMember(ctx, pool, memberID, 5000, 0, "qr", "充值", "rc-conc"); err != nil {
		t.Fatal(err)
	}
	plans, err := loyalty.ListPassPlans(ctx, pool, true)
	if err != nil || len(plans) == 0 {
		t.Fatal(err)
	}
	if _, err := loyalty.IssuePass(ctx, pool, memberID, plans[0].ID, "qr", "办卡", "ps-conc"); err != nil {
		t.Fatal(err)
	}

	// A. 4 个台位同时用同一张月卡（每日限 1 杯）+ ExpectedTotalCents=0 核销，严格只有 1 单能免单成功
	passSpots := []string{"bar-1", "bar-2", "bar-3", "table-1"}
	passOrders := make([]int64, len(passSpots))
	passIDs := make([][]int64, len(passSpots))
	for i, sp := range passSpots {
		oid, err := EnsureOrder(ctx, pool, sp)
		if err != nil {
			t.Fatal(err)
		}
		if err := AddOrderItem(ctx, pool, oid, "beer", keg, "测试IPA", 4000, 330); err != nil {
			t.Fatal(err)
		}
		passOrders[i] = oid
		passIDs[i] = unpaidIDs(t, pool, oid)
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(passSpots))
	zero := 0
	for i := range passSpots {
		wg.Add(1)
		go func(oid int64, ids []int64) {
			defer wg.Done()
			_, err := PayItemsWithOptions(ctx, pool, ManualGateway{}, oid, ids, PayOptions{
				MemberID:           &memberID,
				UsePass:            true,
				ExpectedTotalCents: &zero,
			})
			errs <- err
		}(passOrders[i], passIDs[i])
	}
	wg.Wait()
	close(errs)
	passOK := 0
	for err := range errs {
		if err == nil {
			passOK++
		}
	}
	if passOK != 1 {
		t.Fatalf("每日限1杯月卡并发核销只能成功 1 单，实际 %d", passOK)
	}
	if got := qInt(t, pool, `select coalesce(sum(cups),0)::int from member_pass_usages where member_id=$1`, memberID); got != 1 {
		t.Fatalf("月卡核销记录应严格为 1 杯，实际 %d", got)
	}

	// B. 3 个台位同时扣同一会员 5000 分储值余额（每单 4000 分），严格只有 1 单能成功且余额不能变负
	balSpots := []string{"table-2", "table-3", "table-4"}
	balOrders := make([]int64, len(balSpots))
	balIDs := make([][]int64, len(balSpots))
	for i, sp := range balSpots {
		oid, err := EnsureOrder(ctx, pool, sp)
		if err != nil {
			t.Fatal(err)
		}
		if err := AddOrderItem(ctx, pool, oid, "beer", keg, "测试IPA", 4000, 330); err != nil {
			t.Fatal(err)
		}
		balOrders[i] = oid
		balIDs[i] = unpaidIDs(t, pool, oid)
	}

	var wg2 sync.WaitGroup
	errs2 := make(chan error, len(balSpots))
	for i := range balSpots {
		wg2.Add(1)
		go func(oid int64, ids []int64) {
			defer wg2.Done()
			_, err := PayItemsWithOptions(ctx, pool, ManualGateway{}, oid, ids, PayOptions{
				MemberID: &memberID,
				Method:   "balance",
			})
			errs2 <- err
		}(balOrders[i], balIDs[i])
	}
	wg2.Wait()
	close(errs2)
	balOK := 0
	for err := range errs2 {
		if err == nil {
			balOK++
		}
	}
	if balOK != 1 {
		t.Fatalf("余额 5000 并发付 4000 只能成功 1 单，实际 %d", balOK)
	}
	if got := qInt(t, pool, `select balance_cents from members where id=$1`, memberID); got != 1000 {
		t.Fatalf("余额应严格剩 1000，实际 %d", got)
	}
}

func TestLoyaltyCheckoutRollbackOnGatewayFailure(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := platform.WithActorRole(platform.WithActor(context.Background(), "boss"), ui.RoleBoss)

	keg := seedKeg(t, pool, 4000)
	memberID, err := loyalty.QuickCreateMember(ctx, pool, "回滚会员", "13600003333")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loyalty.RechargeMember(ctx, pool, memberID, 10000, 0, "qr", "充值", "rc-rb"); err != nil {
		t.Fatal(err)
	}
	plans, _ := loyalty.ListPassPlans(ctx, pool, true)
	if _, err := loyalty.IssuePass(ctx, pool, memberID, plans[0].ID, "cash", "开卡", "ps-rb"); err != nil {
		t.Fatal(err)
	}

	oid, _ := EnsureOrder(ctx, pool, "bar-1")
	_ = AddOrderItem(ctx, pool, oid, "beer", keg, "测试IPA", 4000, 330)
	_ = AddOrderItem(ctx, pool, oid, "beer", keg, "测试IPA", 4000, 330)
	ids := unpaidIDs(t, pool, oid)

	if _, err := PayItemsWithOptions(ctx, pool, stubGateway{err: platform.UserErr("网关拒绝")}, oid, ids, PayOptions{
		MemberID: &memberID,
		UsePass:  true,
		Method:   "balance",
	}); err == nil {
		t.Fatal("网关报错应回滚整笔交易")
	}

	if got := qInt(t, pool, `select balance_cents from members where id=$1`, memberID); got != 10000 {
		t.Fatalf("回滚后余额应保持 10000，实际 %d", got)
	}
	if got := qInt(t, pool, `select count(*)::int from member_pass_usages where member_id=$1`, memberID); got != 0 {
		t.Fatalf("回滚后不应记录月卡核销: %d", got)
	}
	if got := qInt(t, pool, `select used_total from member_passes where member_id=$1`, memberID); got != 0 {
		t.Fatalf("回滚后月卡 used_total 应保持 0，实际 %d", got)
	}
}
