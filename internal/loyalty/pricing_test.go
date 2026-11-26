package loyalty

import (
	"testing"
	"time"

	"pourops/internal/settings"
	"pourops/internal/ui"
)

func TestCalculateCheckoutRoleGuards(t *testing.T) {
	lines := []OrderLineInput{{ItemID: 1, Kind: "beer", Name: "IPA", UnitCents: 5000, Qty: 1, ML: 330}}
	restrictedRule := &DiscountRule{ID: 3, Name: "85折", Kind: "percent", DiscountBPS: 8500, Scope: "all", AllowStaff: false, Active: true}
	staffAllowedRule := &DiscountRule{ID: 1, Name: "95折", Kind: "percent", DiscountBPS: 9500, Scope: "all", AllowStaff: true, Active: true}
	custom := &CustomDiscount{Kind: "percent", DiscountBPS: 8000, Scope: "all"}

	if _, err := CalculateCheckout(PricingRequest{Lines: lines, Rule: restrictedRule, ActorRole: ui.RoleStaff}); err == nil {
		t.Fatal("店员不应能使用未授权折扣")
	}
	if _, err := CalculateCheckout(PricingRequest{Lines: lines, Custom: custom, ActorRole: ui.RoleStaff}); err == nil {
		t.Fatal("店员不应能使用自定义折扣")
	}
	if _, err := CalculateCheckout(PricingRequest{Lines: lines, Rule: staffAllowedRule, ActorRole: ui.RoleStaff}); err != nil {
		t.Fatalf("店员应可用 AllowStaff 规则: %v", err)
	}
	for _, role := range []string{ui.RoleManager, ui.RoleBoss} {
		res, err := CalculateCheckout(PricingRequest{Lines: lines, Rule: restrictedRule, ActorRole: role})
		if err != nil || res.AmountCents != 4250 {
			t.Fatalf("%s 应可用受限折扣: res=%+v err=%v", role, res, err)
		}
		res2, err := CalculateCheckout(PricingRequest{Lines: lines, Custom: custom, ActorRole: role})
		if err != nil || res2.AmountCents != 4000 {
			t.Fatalf("%s 应可用自定义折扣: res=%+v err=%v", role, res2, err)
		}
	}
}

func TestCalculateCheckoutPassAndDiscountsAndPoints(t *testing.T) {
	now := time.Now()
	member := &MemberSnapshot{
		ID:                1,
		Name:              "张三",
		TierName:          "黑金会员",
		TierDiscountBPS:   9000, // 9折
		TierScope:         "all",
		TierPointsRateBPS: 15000, // 1.5x
		BalanceCents:      3000,
		Points:            500,
		Active:            true,
		ActivePass: &PassInfo{
			ID:           9,
			PlanName:     "31天畅饮月卡",
			Scope:        "beer",
			BenefitType:  "free_quota",
			DailyLimit:   1,
			MaxML:        500,
			MaxUnitCents: 4800,
			CapMode:      "pay_diff",
			StartsAt:     now.Add(-time.Hour),
			ExpiresAt:    now.AddDate(0, 0, 30),
			Status:       "active",
		},
	}

	lines := []OrderLineInput{
		{ItemID: 1, Kind: "beer", Name: "世涛", UnitCents: 5800, Qty: 1, ML: 330},
		{ItemID: 2, Kind: "beer", Name: "拉格", UnitCents: 3800, Qty: 1, ML: 330},
		{ItemID: 3, Kind: "retail", Name: "薯片", UnitCents: 2000, Qty: 1, ML: 0},
	}

	// 1. 月卡先免高价杯（封顶 4800 补差 1000），剩余余款（1000+3800+2000=6800）享黑金 9 折（折680，剩6120），叠加抹零到元（抹20，剩6100）
	roundRule := &DiscountRule{
		ID: 4, Name: "抹零到元", Kind: "round_down", RoundUnitCents: 100, Scope: "all", AllowStaff: true, StackWithMember: true, Active: true,
	}
	res, err := CalculateCheckout(PricingRequest{
		Lines:      lines,
		Member:     member,
		UsePass:    true,
		Rule:       roundRule,
		ActorRole:  ui.RoleStaff,
		LoyaltyCfg: settings.DefaultLoyaltyConfig(),
	})
	if err != nil {
		t.Fatalf("CalculateCheckout failed: %v", err)
	}
	if res.GrossCents != 11600 || res.PassCupsUsed != 1 || res.PassDiscountCents != 4800 ||
		res.MemberDiscountCents != 680 || res.ManualDiscountCents != 20 || res.AmountCents != 6100 {
		t.Fatalf("unexpected pricing breakdown: %+v", res)
	}
	sumPaid, sumDisc := 0, 0
	for _, lp := range res.Lines {
		sumPaid += lp.PaidCents
		sumDisc += lp.DiscountCents
	}
	if sumPaid != res.AmountCents || sumDisc != res.DiscountCents {
		t.Fatalf("pro-rata sum mismatch: sumPaid=%d amount=%d sumDisc=%d disc=%d", sumPaid, res.AmountCents, sumDisc, res.DiscountCents)
	}

	// 2. 选择老板请客免单时跳过月卡，不浪费今日免单配额
	compRule := &DiscountRule{ID: 5, Name: "老板请客免单", Kind: "comp", Scope: "all", AllowStaff: false, Active: true}
	compRes, err := CalculateCheckout(PricingRequest{
		Lines:     lines,
		Member:    member,
		UsePass:   true,
		Rule:      compRule,
		ActorRole: ui.RoleBoss,
	})
	if err != nil {
		t.Fatalf("comp failed: %v", err)
	}
	if compRes.PassCupsUsed != 0 || compRes.AmountCents != 0 || compRes.ManualDiscountCents != 11600 {
		t.Fatalf("comp should preserve pass and zero out order: %+v", compRes)
	}

	// 3. block 模式超封顶价不核销世涛，转而核销 3800 的拉格
	memberBlock := *member
	passBlock := *member.ActivePass
	passBlock.CapMode = "block"
	memberBlock.ActivePass = &passBlock
	blockRes, err := CalculateCheckout(PricingRequest{
		Lines:      lines,
		Member:     &memberBlock,
		UsePass:    true,
		ActorRole:  ui.RoleStaff,
		LoyaltyCfg: settings.DefaultLoyaltyConfig(),
	})
	if err != nil {
		t.Fatalf("block mode failed: %v", err)
	}
	if blockRes.PassCupsUsed != 1 || blockRes.PassDiscountCents != 3800 {
		t.Fatalf("block mode should redeem 3800 lager: %+v", blockRes)
	}

	// 4. 积分抵现 + 储值余额组合补差 + 攒积分
	tierCopy := *member
	tierCopy.ActivePass = nil
	splitRes, err := CalculateCheckout(PricingRequest{
		Lines:      []OrderLineInput{{ItemID: 1, Kind: "beer", Name: "IPA", UnitCents: 10000, Qty: 1, ML: 330}},
		Member:     &tierCopy,
		UsePoints:  true, // 9折后 9000 -> 500分抵500 -> 应付8500
		UseBalance: true, // 余额3000 -> 余额扣3000 + 现金补5500
		PayMethod:  "cash",
		ActorRole:  ui.RoleStaff,
		LoyaltyCfg: settings.DefaultLoyaltyConfig(),
	})
	if err != nil {
		t.Fatalf("split pay failed: %v", err)
	}
	if splitRes.AmountCents != 8500 || splitRes.PointsSpent != 500 || splitRes.PointsDiscountCents != 500 ||
		splitRes.BalancePaidCents != 3000 || splitRes.ExternalPaidCents != 5500 || splitRes.PointsEarned != 127 {
		t.Fatalf("unexpected split result: %+v", splitRes)
	}

	// 5. 纯储值模式余额不足必须拦截
	if _, err := CalculateCheckout(PricingRequest{
		Lines:     []OrderLineInput{{ItemID: 1, Kind: "beer", Name: "IPA", UnitCents: 10000, Qty: 1, ML: 330}},
		Member:    &tierCopy,
		PayMethod: "balance",
		ActorRole: ui.RoleStaff,
	}); err == nil {
		t.Fatal("纯储值余额不足时必须拒绝")
	}
}

func TestCalculateCheckoutProRataZeroDrift(t *testing.T) {
	lines := []OrderLineInput{
		{ItemID: 1, Kind: "beer", Name: "A", UnitCents: 3333, Qty: 1, ML: 330},
		{ItemID: 2, Kind: "beer", Name: "B", UnitCents: 4777, Qty: 1, ML: 330},
		{ItemID: 3, Kind: "retail", Name: "C", UnitCents: 1999, Qty: 1, ML: 0},
	}
	rule := &DiscountRule{ID: 1, Name: "87折", Kind: "percent", DiscountBPS: 8700, Scope: "all", AllowStaff: true, Active: true}
	res, err := CalculateCheckout(PricingRequest{Lines: lines, Rule: rule, ActorRole: ui.RoleStaff})
	if err != nil {
		t.Fatal(err)
	}
	sumPaid, sumDisc := 0, 0
	for _, lp := range res.Lines {
		if lp.PaidCents < 0 || lp.PaidCents > lp.GrossCents {
			t.Fatalf("line out of bounds: %+v", lp)
		}
		sumPaid += lp.PaidCents
		sumDisc += lp.DiscountCents
	}
	if sumPaid != res.AmountCents || sumDisc != res.DiscountCents || res.AmountCents+res.DiscountCents != res.GrossCents {
		t.Fatalf("zero-drift invariant violated: %+v sumPaid=%d sumDisc=%d", res, sumPaid, sumDisc)
	}
}
