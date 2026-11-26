package loyalty

import (
	"context"
	"strings"
	"testing"
	"time"

	"pourops/internal/platform"
	"pourops/migrations"
)

func TestMemberLifecycleRechargePassAndRenewal(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := platform.WithActorRole(platform.WithActor(context.Background(), "boss"), "boss")

	// 1. 建档自动生成卡号，重复手机号拦截
	mID, err := QuickCreateMember(ctx, pool, "阿杰", "139-0013-9000")
	if err != nil {
		t.Fatalf("QuickCreateMember: %v", err)
	}
	if _, err := QuickCreateMember(ctx, pool, "重复号", "13900139000"); err == nil {
		t.Fatal("重复手机号应被拒绝")
	}

	detail, err := GetMemberDetail(ctx, pool, mID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Member.Phone != "13900139000" || !strings.HasPrefix(detail.Member.MemberNo, "M") || detail.Member.TierName != "普通会员" {
		t.Fatalf("unexpected member detail: %+v", detail.Member)
	}

	// 2. 充值 + 赠金 + 幂等重放
	rec, err := RechargeMember(ctx, pool, mID, 50000, 5000, "qr", "首充", "rc-idem-1")
	if err != nil {
		t.Fatalf("RechargeMember: %v", err)
	}
	if rec.BalanceAfterCents != 55000 || rec.Replayed {
		t.Fatalf("rec = %+v", rec)
	}
	recReplay, err := RechargeMember(ctx, pool, mID, 50000, 5000, "qr", "首充", "rc-idem-1")
	if err != nil || !recReplay.Replayed || recReplay.BalanceAfterCents != 55000 {
		t.Fatalf("recharge replay = %+v err=%v", recReplay, err)
	}

	// 3. 人工调账校验（无备注失败、负超扣失败、正常调账记账）
	if err := AdjustMemberBalance(ctx, pool, mID, -1000, ""); err == nil {
		t.Fatal("调账无备注应拒绝")
	}
	if err := AdjustMemberBalance(ctx, pool, mID, -999999, "超扣"); err == nil {
		t.Fatal("余额调负应拒绝")
	}
	if err := AdjustMemberPoints(ctx, pool, mID, 200, "老客赠分"); err != nil {
		t.Fatalf("AdjustMemberPoints: %v", err)
	}

	// 4. 办理默认 31 天月卡（扣储值余额），并测试同方案续费顺延
	plans, err := ListPassPlans(ctx, pool, true)
	if err != nil || len(plans) == 0 {
		t.Fatalf("默认 31 天月卡方案未初始化: %v len=%d", err, len(plans))
	}
	plan := plans[0]
	if plan.DurationDays != 31 || plan.DailyLimit != 1 {
		t.Fatalf("默认月卡方案参数不对: %+v", plan)
	}

	p1, err := IssuePass(ctx, pool, mID, plan.ID, "balance", "余额开卡", "ps-1")
	if err != nil {
		t.Fatalf("IssuePass balance: %v", err)
	}
	if p1.Renewed || p1.Pass.Status != "active" {
		t.Fatalf("p1 = %+v", p1)
	}
	p1Replay, err := IssuePass(ctx, pool, mID, plan.ID, "balance", "余额开卡", "ps-1")
	if err != nil || !p1Replay.Replayed || p1Replay.Pass.ID != p1.Pass.ID {
		t.Fatalf("IssuePass replay = %+v err=%v", p1Replay, err)
	}

	// 顺延续费 31 天（cash 支付）
	p2, err := IssuePass(ctx, pool, mID, plan.ID, "cash", "续费", "ps-2")
	if err != nil {
		t.Fatalf("IssuePass renew: %v", err)
	}
	if !p2.Renewed || !p2.Pass.StartsAt.Equal(p1.Pass.ExpiresAt) || !p2.Pass.ExpiresAt.Equal(p1.Pass.ExpiresAt.AddDate(0, 0, 31)) {
		t.Fatalf("续费起始和结束时间应紧接上一张: p1.Exp=%v p2=%+v", p1.Pass.ExpiresAt, p2.Pass)
	}

	// 5. 营业日 07:00 计次验证与作废
	if err := RevokePass(ctx, pool, mID, p2.Pass.ID, "退卡"); err != nil {
		t.Fatalf("RevokePass: %v", err)
	}
	snap, err := LoadCheckoutMemberSnapshot(ctx, pool, mID, 0, time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if snap.BalanceCents != 55000-plan.PriceCents || snap.Points != 200 || snap.ActivePass == nil || snap.ActivePass.ID != p1.Pass.ID {
		t.Fatalf("snap mismatch: %+v", snap)
	}
}
