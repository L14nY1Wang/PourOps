package inventory

import (
	"strings"
	"testing"
	"time"

	"pourops/internal/catalog"
	"pourops/internal/settings"
)

func TestKegPricingAndRecommendation(t *testing.T) {
	if got := SellableML(20000); got != 16000 {
		t.Fatalf("SellableML(20000) = %d, want 16000", got)
	}
	if got := LossBudgetML(20000); got != 4000 {
		t.Fatalf("LossBudgetML(20000) = %d, want 4000", got)
	}

	// ¥1200 / 20L -> 80% 可售 16000ml -> 有效单杯成本 ¥11.25 / ¥24.75 / ¥37.50 -> 50% 推荐价 ¥23 / ¥50 / ¥75
	cases1200 := []struct {
		ml       int
		effCost  int
		recCents int
		sellCups int
	}{
		{150, 1125, 2300, 106},
		{330, 2475, 5000, 48},
		{500, 3750, 7500, 32},
	}
	for _, c := range cases1200 {
		if got := CupEffectiveCostCents(120000, 20000, c.ml); got != c.effCost {
			t.Errorf("%dml effCost = %d, want %d", c.ml, got, c.effCost)
		}
		if got := RecommendedCupPriceCents(120000, 20000, c.ml); got != c.recCents {
			t.Errorf("%dml recommended = %d, want %d", c.ml, got, c.recCents)
		}
		if got := SellableCups(20000, c.ml); got != c.sellCups {
			t.Errorf("%dml sellableCups = %d, want %d", c.ml, got, c.sellCups)
		}
	}

	// ¥1282.5 / 20L -> ¥25 / ¥53 / ¥81
	want12825 := map[int]int{150: 2500, 330: 5300, 500: 8100}
	for ml, want := range want12825 {
		if got := RecommendedCupPriceCents(128250, 20000, ml); got != want {
			t.Errorf("1282.5 元 %dml recommended = %d, want %d", ml, got, want)
		}
	}

	keg := Keg{CapacityML: 20000, CostCents: 120000}
	beer := catalog.Beer{Price150: 2500, Price330: 4500, Price500: 7500}
	rows := keg.CupPricing(beer)
	if len(rows) != 3 {
		t.Fatalf("len(rows) = %d", len(rows))
	}
	if !rows[0].TargetMet || rows[0].BreakEvenCups != 48 || rows[0].DiffText() != "达标" {
		t.Fatalf("150ml row 不对: %+v", rows[0])
	}
	if rows[1].TargetMet || rows[1].BreakEvenCups != 27 || rows[1].DiffText() != "差 ¥ 5" {
		t.Fatalf("330ml row 不对: %+v", rows[1])
	}
	if !rows[2].TargetMet || rows[2].BreakEvenCups != 16 || rows[2].QuickCents() != 7500 {
		t.Fatalf("500ml row 不对: %+v", rows[2])
	}

	// 未填成本与未定价时不填虚构兜底价
	noCost := (Keg{CapacityML: 20000}).CupPricing(catalog.Beer{})
	if noCost[1].RecommendedText() != "—" || noCost[1].QuickCents() != 0 ||
		noCost[1].QuickPriceText() != "未定价" || noCost[1].QuickAmountInput() != "" {
		t.Fatalf("无成本未定价展示不对: %+v", noCost[1])
	}
}

func TestKegPricingWithCustomConfig(t *testing.T) {
	cfg := settings.PricingConfig{SellablePct: 85, MarginPct: 55}
	// 20L * 85% = 17000ml，损耗 3000ml
	if got := SellableMLWithConfig(20000, cfg); got != 17000 {
		t.Fatalf("SellableMLWithConfig(85%%) = %d, want 17000", got)
	}
	if got := LossBudgetMLWithConfig(20000, cfg); got != 3000 {
		t.Fatalf("LossBudgetMLWithConfig(85%%) = %d, want 3000", got)
	}
	// 目标营收：120000 / 0.45 向上取整 = 266667 分
	if got := TargetRevenueCentsWithConfig(120000, cfg); got != 266667 {
		t.Fatalf("TargetRevenueCentsWithConfig = %d, want 266667", got)
	}
	// 330ml 有效成本 = round(120000*330/17000) = 2329 分；55% 推荐价 = ceil(120000*330/(17000*45))*100 = 5200 分
	if got := CupEffectiveCostCentsWithConfig(120000, 20000, 330, cfg); got != 2329 {
		t.Fatalf("CupEffectiveCostCentsWithConfig = %d, want 2329", got)
	}
	if got := RecommendedCupPriceCentsWithConfig(120000, 20000, 330, cfg); got != 5200 {
		t.Fatalf("RecommendedCupPriceCentsWithConfig = %d, want 5200", got)
	}
	now := time.Date(2026, 11, 20, 21, 0, 0, 0, time.Local)
	k := Keg{CapacityML: 20000, CostCents: 120000, TappedAt: &now, SoldML: 17000, RevenueCents: 266667}
	econ := k.EconomicsWithConfig(nil, cfg)
	if econ.SellablePct != 85 || econ.TargetMarginPct != 55 || econ.LossPct() != 15 || !econ.TargetMet ||
		econ.Verdict != "target_met" || !strings.Contains(econ.Diagnosis, "55%") {
		t.Fatalf("EconomicsWithConfig(85/55) 不对: %+v", econ)
	}
}

func TestKegEconomicsVerdictsAndDiagnosis(t *testing.T) {
	now := time.Date(2026, 11, 20, 21, 0, 0, 0, time.Local)

	// 1. 未填成本
	eNoCost := (Keg{CapacityML: 20000, SoldML: 3300, RevenueCents: 45000, TappedAt: &now}).Economics(nil)
	if eNoCost.Verdict != "no_cost" || eNoCost.HasCost {
		t.Fatalf("未填成本判定不对: %+v", eNoCost)
	}

	// 2. 库存待开
	eUntapped := (Keg{CapacityML: 20000, CostCents: 120000, PlacedAt: &now}).Economics(nil)
	if eUntapped.Verdict != "untapped" || eUntapped.TheoreticalBreakEven != 8000 || eUntapped.TargetRevenueCents != 240000 {
		t.Fatalf("待开桶判定不对: %+v", eUntapped)
	}

	// 3. 在售：回本中 -> 已回本盈利中 -> 已达 50% 目标
	eRec := (Keg{CapacityML: 20000, CostCents: 120000, TappedAt: &now, SoldML: 4000, RevenueCents: 60000}).Economics(nil)
	if eRec.Verdict != "recovering" || eRec.BreakEvenPct != 50 || eRec.BreakEvenGapCents != 60000 || eRec.EstimatedBreakEvenML != 8000 {
		t.Fatalf("在售回本中判定不对: %+v", eRec)
	}
	eProf := (Keg{CapacityML: 20000, CostCents: 120000, TappedAt: &now, SoldML: 10000, RevenueCents: 150000}).Economics(nil)
	if eProf.Verdict != "profitable" || !eProf.BreakEvenReached || eProf.TargetMet || eProf.TargetGapCents != 90000 {
		t.Fatalf("在售盈利中判定不对: %+v", eProf)
	}
	eActMet := (Keg{CapacityML: 20000, CostCents: 120000, TappedAt: &now, SoldML: 16000, RevenueCents: 240000}).Economics(nil)
	if eActMet.Verdict != "target_met" || !eActMet.TargetMet || eActMet.TargetProgressPct != 100 {
		t.Fatalf("在售达标判定不对: %+v", eActMet)
	}

	// 4. 下架结桶：达标盈利
	kFinMet := Keg{CapacityML: 20000, CostCents: 120000, TappedAt: &now, FinishedAt: &now, SoldML: 16000, RevenueCents: 240000}
	eFinMet := kFinMet.Economics(nil)
	if eFinMet.Verdict != "target_met" || eFinMet.ActualYieldPct != 80 || eFinMet.MarginText() != "50.0%" {
		t.Fatalf("结桶达标判定不对: %+v", eFinMet)
	}
	if !strings.Contains(kFinMet.UntapSummaryText(), "达标盈利") {
		t.Fatalf("下架摘要不对: %q", kFinMet.UntapSummaryText())
	}

	// 5. 下架结桶：盈利未达标 —— 区分「出酒率偏低」与「实售均价偏低」
	eLowYield := (Keg{CapacityML: 20000, CostCents: 120000, TappedAt: &now, FinishedAt: &now, SoldML: 11000, RevenueCents: 180000}).Economics(nil)
	if eLowYield.Verdict != "below_target" || !strings.Contains(eLowYield.Diagnosis, "出酒率") {
		t.Fatalf("低出酒率归因不对: %+v", eLowYield)
	}
	eLowPrice := (Keg{CapacityML: 20000, CostCents: 120000, TappedAt: &now, FinishedAt: &now, SoldML: 16500, RevenueCents: 180000}).Economics(nil)
	if eLowPrice.Verdict != "below_target" || !strings.Contains(eLowPrice.Diagnosis, "实售均价") {
		t.Fatalf("低均价归因不对: %+v", eLowPrice)
	}

	// 6. 下架结桶：刚好回本 & 亏损未回本（含零卖出）
	kEven := Keg{CapacityML: 20000, CostCents: 120000, TappedAt: &now, FinishedAt: &now, SoldML: 10000, RevenueCents: 120000}
	if kEven.Economics(nil).Verdict != "breakeven" || !strings.Contains(kEven.UntapSummaryText(), "刚好回本") {
		t.Fatalf("刚好回本判定不对: %+v / %q", kEven.Economics(nil), kEven.UntapSummaryText())
	}
	eZero := (Keg{CapacityML: 20000, CostCents: 120000, TappedAt: &now, FinishedAt: &now}).Economics(nil)
	if eZero.Verdict != "loss" || !strings.Contains(eZero.Diagnosis, "无售出") {
		t.Fatalf("零卖出亏损判定不对: %+v", eZero)
	}
	kLoss := Keg{CapacityML: 20000, CostCents: 120000, TappedAt: &now, FinishedAt: &now, SoldML: 8000, RevenueCents: 90000}
	if kLoss.Economics(nil).Verdict != "loss" || !strings.Contains(kLoss.UntapSummaryText(), "亏损") {
		t.Fatalf("亏损摘要不对: %+v / %q", kLoss.Economics(nil), kLoss.UntapSummaryText())
	}
}

func TestSummarizeKegs(t *testing.T) {
	now := time.Date(2026, 11, 20, 21, 0, 0, 0, time.Local)
	kegs := []Keg{
		{CapacityML: 20000, CostCents: 100000, TappedAt: &now, SoldML: 12000, RevenueCents: 180000},
		{CapacityML: 20000, CostCents: 100000, TappedAt: &now, FinishedAt: &now, SoldML: 16000, RevenueCents: 200000},
		{CapacityML: 20000, CostCents: 100000, TappedAt: &now, FinishedAt: &now, SoldML: 12000, RevenueCents: 140000},
		{CapacityML: 20000, CostCents: 100000, TappedAt: &now, FinishedAt: &now, SoldML: 8000, RevenueCents: 80000},
		{CapacityML: 20000, CostCents: 100000, PlacedAt: &now},
		{CapacityML: 20000, CostCents: 0, TappedAt: &now, SoldML: 3300, RevenueCents: 45000},
		{CapacityML: 20000, CostCents: 0, TappedAt: &now, FinishedAt: &now, SoldML: 4000, RevenueCents: 55000},
	}
	sum := SummarizeKegs(kegs)
	if sum.TotalCount != 7 || sum.ActiveCount != 2 || sum.FinishedCount != 4 || sum.UntappedCount != 1 {
		t.Fatalf("桶数统计不对: %+v", sum)
	}
	if sum.TotalRevenueCents != 600000 || sum.TotalCostCents != 400000 || sum.TotalProfitCents != 200000 ||
		sum.StockCostCents != 100000 || sum.StockCostText() != "¥ 1000" || sum.MarginText() != "33.3%" {
		t.Fatalf("金额汇总不对: %+v", sum)
	}
	if sum.UncostedCount != 2 || sum.FinishedUncostedCount != 1 {
		t.Fatalf("待填成本桶统计不对: %+v", sum)
	}
	if sum.FinishedTargetMetCount != 1 || sum.FinishedBelowTargetCount != 1 || sum.FinishedLossCount != 1 || sum.AvgFinishedYieldPct != 50 {
		t.Fatalf("已结桶分布或平均出酒率不对: %+v", sum)
	}
	distSum := sum.FinishedTargetMetCount + sum.FinishedBelowTargetCount + sum.FinishedBreakevenCount + sum.FinishedLossCount + sum.FinishedUncostedCount
	if distSum != sum.FinishedCount {
		t.Fatalf("已结桶各分档之和 (%d) 不等于 FinishedCount (%d)", distSum, sum.FinishedCount)
	}
}
