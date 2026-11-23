package inventory

import (
	"fmt"
	"strconv"

	"pourops/internal/catalog"
	"pourops/internal/platform"
	"pourops/internal/settings"
)

func SellableMLWithConfig(capacityML int, cfg settings.PricingConfig) int {
	if capacityML <= 0 {
		return 0
	}
	cfg = cfg.Normalized()
	return int((int64(capacityML)*int64(cfg.SellablePct) + 50) / 100)
}

func LossBudgetMLWithConfig(capacityML int, cfg settings.PricingConfig) int {
	if capacityML <= 0 {
		return 0
	}
	return max(capacityML-SellableMLWithConfig(capacityML, cfg), 0)
}

func CupEffectiveCostCentsWithConfig(costCents, capacityML, cupML int, cfg settings.PricingConfig) int {
	sml := SellableMLWithConfig(capacityML, cfg)
	if costCents <= 0 || sml <= 0 || cupML <= 0 {
		return 0
	}
	return int((int64(costCents)*int64(cupML) + int64(sml)/2) / int64(sml))
}

// RecommendedCupPriceCentsWithConfig 按可售出酒率与目标毛利率向上取整到整元，
// 保证按推荐价售完可售量时整桶毛利率不低于目标毛利率。
func RecommendedCupPriceCentsWithConfig(costCents, capacityML, cupML int, cfg settings.PricingConfig) int {
	cfg = cfg.Normalized()
	sml := SellableMLWithConfig(capacityML, cfg)
	costRatio := int64(100 - cfg.MarginPct)
	if costCents <= 0 || sml <= 0 || cupML <= 0 || costRatio <= 0 {
		return 0
	}
	denom := int64(sml) * costRatio
	yuan := (int64(costCents)*int64(cupML) + denom - 1) / denom
	return int(yuan * 100)
}

func TargetRevenueCentsWithConfig(costCents int, cfg settings.PricingConfig) int {
	cfg = cfg.Normalized()
	costRatio := int64(100 - cfg.MarginPct)
	if costCents <= 0 || costRatio <= 0 {
		return 0
	}
	return int((int64(costCents)*100 + costRatio - 1) / costRatio)
}

func BreakEvenCups(costCents, cupPriceCents int) int {
	if costCents <= 0 || cupPriceCents <= 0 {
		return 0
	}
	return int((int64(costCents) + int64(cupPriceCents) - 1) / int64(cupPriceCents))
}

func SellableCupsWithConfig(capacityML, cupML int, cfg settings.PricingConfig) int {
	if capacityML <= 0 || cupML <= 0 {
		return 0
	}
	return SellableMLWithConfig(capacityML, cfg) / cupML
}

func MarginPctText(revenueCents, costCents int) string {
	if revenueCents <= 0 {
		return "—"
	}
	pct := float64(revenueCents-costCents) * 100 / float64(revenueCents)
	return fmt.Sprintf("%.1f%%", pct)
}

func SellableML(capacityML int) int {
	return SellableMLWithConfig(capacityML, settings.DefaultPricingConfig())
}

func LossBudgetML(capacityML int) int {
	return LossBudgetMLWithConfig(capacityML, settings.DefaultPricingConfig())
}

func CupEffectiveCostCents(costCents, capacityML, cupML int) int {
	return CupEffectiveCostCentsWithConfig(costCents, capacityML, cupML, settings.DefaultPricingConfig())
}

func RecommendedCupPriceCents(costCents, capacityML, cupML int) int {
	return RecommendedCupPriceCentsWithConfig(costCents, capacityML, cupML, settings.DefaultPricingConfig())
}

func TargetRevenueCents(costCents int) int {
	return TargetRevenueCentsWithConfig(costCents, settings.DefaultPricingConfig())
}

func SellableCups(capacityML, cupML int) int {
	return SellableCupsWithConfig(capacityML, cupML, settings.DefaultPricingConfig())
}

// CupPricingRow 描述单一杯型（150/330/500ml）在当前出酒假设下的成本、推荐价与回本杯数。
type CupPricingRow struct {
	ML                 int
	EffectiveCostCents int
	RecommendedCents   int
	ActualCents        int
	BreakEvenCups      int
	SellableCups       int
	HasCost            bool
	TargetMet          bool
}

func (r CupPricingRow) MLText() string { return strconv.Itoa(r.ML) + "ml" }

func (r CupPricingRow) EffectiveCostText() string {
	if r.EffectiveCostCents <= 0 {
		return "—"
	}
	return platform.PriceText(r.EffectiveCostCents)
}

func (r CupPricingRow) RecommendedText() string {
	if r.RecommendedCents <= 0 {
		return "—"
	}
	return platform.PriceText(r.RecommendedCents)
}

func (r CupPricingRow) ActualText() string {
	if r.ActualCents <= 0 {
		return "未定价"
	}
	return platform.PriceText(r.ActualCents)
}

func (r CupPricingRow) MarginText() string {
	if !r.HasCost || r.ActualCents <= 0 {
		return "—"
	}
	return MarginPctText(r.ActualCents, r.EffectiveCostCents)
}

func (r CupPricingRow) BreakEvenText() string {
	if r.BreakEvenCups <= 0 {
		return "—"
	}
	return strconv.Itoa(r.BreakEvenCups) + " 杯"
}

func (r CupPricingRow) SellableCupsText() string {
	if r.SellableCups <= 0 {
		return "—"
	}
	return strconv.Itoa(r.SellableCups) + " 杯"
}

func (r CupPricingRow) DiffText() string {
	if !r.HasCost || r.RecommendedCents <= 0 {
		return "—"
	}
	if r.ActualCents <= 0 {
		return "待定价"
	}
	if r.ActualCents >= r.RecommendedCents {
		return "达标"
	}
	return "差 " + platform.PriceText(r.RecommendedCents-r.ActualCents)
}

func (r CupPricingRow) QuickCents() int {
	if r.ActualCents > 0 {
		return r.ActualCents
	}
	if r.RecommendedCents > 0 {
		return r.RecommendedCents
	}
	return 0
}

func (r CupPricingRow) QuickPriceText() string {
	c := r.QuickCents()
	if c <= 0 {
		return "未定价"
	}
	return platform.PriceText(c)
}

func (r CupPricingRow) QuickAmountInput() string {
	return platform.MoneyInput(r.QuickCents())
}

func (k Keg) CupPricingWithConfig(b catalog.Beer, cfg settings.PricingConfig) []CupPricingRow {
	cfg = cfg.Normalized()
	rows := make([]CupPricingRow, 0, len(catalog.CupSizes))
	for _, ml := range catalog.CupSizes {
		effCost := CupEffectiveCostCentsWithConfig(k.CostCents, k.CapacityML, ml, cfg)
		rec := RecommendedCupPriceCentsWithConfig(k.CostCents, k.CapacityML, ml, cfg)
		actual := b.CupAt(ml).Cents
		refPrice := actual
		if refPrice <= 0 {
			refPrice = rec
		}
		rows = append(rows, CupPricingRow{
			ML:                 ml,
			EffectiveCostCents: effCost,
			RecommendedCents:   rec,
			ActualCents:        actual,
			BreakEvenCups:      BreakEvenCups(k.CostCents, refPrice),
			SellableCups:       SellableCupsWithConfig(k.CapacityML, ml, cfg),
			HasCost:            k.CostCents > 0 && k.CapacityML > 0,
			TargetMet:          k.CostCents > 0 && rec > 0 && actual >= rec,
		})
	}
	return rows
}

func (k Keg) CupPricing(b catalog.Beer) []CupPricingRow {
	return k.CupPricingWithConfig(b, settings.DefaultPricingConfig())
}

// KegEconomics 是一桶酒基于可售率与目标毛利率配置的完整经营核算结果。
type KegEconomics struct {
	CapacityML           int
	SellablePct          int
	TargetMarginPct      int
	SellableML           int
	LossBudgetML         int
	CostCents            int
	RevenueCents         int
	ProfitCents          int
	TargetRevenueCents   int
	TargetProfitCents    int
	BreakEvenGapCents    int
	TargetGapCents       int
	ActualYieldPct       int // 已售 ml / 整桶容量 × 100（四舍五入）
	RecoveryPct          int // 营收 / 成本 × 100（0-100 限幅，供进度条/标题使用）
	BreakEvenPct         int // 营收 / 成本 × 100（不封顶，盈利后可超 100%）
	TargetProgressPct    int // 营收 / 目标营收 × 100（0-100 限幅）
	BreakEvenTickPct     int // 进度条上回本线刻度位置（100 - TargetMarginPct）
	TheoreticalBreakEven int // 按目标价算的理论回本出酒量
	EstimatedBreakEvenML int // 有实售均价时按实际均价折算的预计回本出酒量
	BreakEvenReached     bool
	TargetMet            bool
	Finished             bool
	HasCost              bool
	Verdict              string // no_cost | target_met | below_target | breakeven | loss | profitable | recovering | untapped
	VerdictLabel         string
	VerdictTone          string // success | warning | danger | info | secondary
	Diagnosis            string
}

func (e KegEconomics) LossPct() int          { return max(100-e.SellablePct, 0) }
func (e KegEconomics) HasProfit() bool       { return e.ProfitCents >= 0 }
func (e KegEconomics) ProfitText() string    { return platform.MoneyText(e.ProfitCents) }
func (e KegEconomics) CostText() string      { return platform.PriceText(e.CostCents) }
func (e KegEconomics) RevenueText() string   { return platform.PriceText(e.RevenueCents) }
func (e KegEconomics) TargetRevText() string { return platform.PriceText(e.TargetRevenueCents) }
func (e KegEconomics) BreakEvenGapText() string {
	return platform.PriceText(e.BreakEvenGapCents)
}
func (e KegEconomics) TargetGapText() string { return platform.PriceText(e.TargetGapCents) }
func (e KegEconomics) MarginText() string    { return MarginPctText(e.RevenueCents, e.CostCents) }

func (e KegEconomics) VerdictPillClass() string {
	switch e.VerdictTone {
	case "success":
		return "verdict-pill--success"
	case "warning":
		return "verdict-pill--warn"
	case "danger":
		return "verdict-pill--danger"
	case "info":
		return "verdict-pill--info"
	default:
		return "verdict-pill--muted"
	}
}

func (k Keg) EconomicsWithConfig(b *catalog.Beer, cfg settings.PricingConfig) KegEconomics {
	cfg = cfg.Normalized()
	sml := SellableMLWithConfig(k.CapacityML, cfg)
	lml := LossBudgetMLWithConfig(k.CapacityML, cfg)
	targetRev := TargetRevenueCentsWithConfig(k.CostCents, cfg)
	costRatio := 100 - cfg.MarginPct
	theoBreakEven := int((int64(sml) * int64(costRatio)) / 100)

	var actualYield int
	if k.CapacityML > 0 && k.SoldML > 0 {
		actualYield = int((int64(k.SoldML)*100 + int64(k.CapacityML)/2) / int64(k.CapacityML))
	}

	var estBreakEvenML int
	if k.CostCents > 0 && k.RevenueCents > 0 && k.SoldML > 0 {
		estBreakEvenML = int((int64(k.CostCents)*int64(k.SoldML) + int64(k.RevenueCents) - 1) / int64(k.RevenueCents))
	}

	e := KegEconomics{
		CapacityML:           k.CapacityML,
		SellablePct:          cfg.SellablePct,
		TargetMarginPct:      cfg.MarginPct,
		SellableML:           sml,
		LossBudgetML:         lml,
		CostCents:            k.CostCents,
		RevenueCents:         k.RevenueCents,
		ProfitCents:          k.RevenueCents - k.CostCents,
		TargetRevenueCents:   targetRev,
		TargetProfitCents:    targetRev - k.CostCents,
		ActualYieldPct:       actualYield,
		TheoreticalBreakEven: theoBreakEven,
		EstimatedBreakEvenML: estBreakEvenML,
		Finished:             k.FinishedAt != nil,
		HasCost:              k.CostCents > 0,
	}

	if k.CostCents > 0 {
		e.BreakEvenGapCents = max(k.CostCents-k.RevenueCents, 0)
		e.TargetGapCents = max(targetRev-k.RevenueCents, 0)
		e.BreakEvenPct = int(int64(k.RevenueCents) * 100 / int64(k.CostCents))
		e.RecoveryPct = platform.ClampPct(e.BreakEvenPct)
		if targetRev > 0 {
			e.TargetProgressPct = platform.ClampPct(int(int64(k.RevenueCents) * 100 / int64(targetRev)))
		}
		e.BreakEvenTickPct = costRatio
		e.BreakEvenReached = k.RevenueCents >= k.CostCents
		e.TargetMet = k.RevenueCents >= targetRev
	}

	_ = b // 预留与酒款定价联动扩展

	if k.CostCents <= 0 {
		e.Verdict = "no_cost"
		e.VerdictLabel = "未填成本"
		e.VerdictTone = "secondary"
		e.Diagnosis = fmt.Sprintf("尚未填写本桶进价，按 %d%% 可售共 %dml（预留损耗 %dml）。补填本桶成本后即可核算回本线与 %d%% 毛利推荐售价。",
			cfg.SellablePct, sml, lml, cfg.MarginPct)
		return e
	}

	lowYield := k.CapacityML > 0 && int64(k.SoldML)*100 < int64(k.CapacityML)*int64(cfg.SellablePct)
	// 实售均价是否低于目标毛利率均价：Revenue/SoldML < TargetRev/SellableML
	lowMarginPrice := k.SoldML > 0 && sml > 0 && int64(k.RevenueCents)*int64(sml) < int64(targetRev)*int64(k.SoldML)
	// 实售均价是否低于可售出酒成本均价：Revenue/SoldML < Cost/SellableML
	belowCostPrice := k.SoldML > 0 && sml > 0 && int64(k.RevenueCents)*int64(sml) < int64(k.CostCents)*int64(k.SoldML)

	switch {
	case k.FinishedAt != nil:
		switch {
		case k.RevenueCents >= targetRev:
			e.Verdict = "target_met"
			e.VerdictLabel = "达标盈利"
			e.VerdictTone = "success"
			e.Diagnosis = fmt.Sprintf("已达 %d%% 毛利目标：整桶实收 %s，净毛利 %s（实际毛利率 %s），实际出酒 %d%%（%dml/%dml）。",
				cfg.MarginPct, platform.PriceText(k.RevenueCents), platform.MoneyText(e.ProfitCents), e.MarginText(), actualYield, k.SoldML, k.CapacityML)
		case k.RevenueCents > k.CostCents:
			e.Verdict = "below_target"
			e.VerdictLabel = "盈利未达标"
			e.VerdictTone = "warning"
			var cause string
			switch {
			case lowYield && lowMarginPrice:
				cause = fmt.Sprintf("主因出酒率仅 %d%%（低于 %d%% 假设）且实售均价低于 %d%% 毛利线。", actualYield, cfg.SellablePct, cfg.MarginPct)
			case lowYield:
				cause = fmt.Sprintf("主因出酒率仅 %d%%（低于 %d%% 目标，打酒损耗或漏记偏高）。", actualYield, cfg.SellablePct)
			case lowMarginPrice:
				cause = fmt.Sprintf("出酒率已达标，主因优惠/实售均价低于 %d%% 毛利推荐价。", cfg.MarginPct)
			default:
				cause = "差额较小，可复核末段损耗与优惠情况。"
			}
			e.Diagnosis = fmt.Sprintf("已回本并盈利 %s（毛利率 %s），但距 %d%% 目标线 %s 仍差 %s。%s",
				platform.MoneyText(e.ProfitCents), e.MarginText(), cfg.MarginPct, platform.PriceText(targetRev), platform.PriceText(e.TargetGapCents), cause)
		case k.RevenueCents == k.CostCents:
			e.Verdict = "breakeven"
			e.VerdictLabel = "刚好回本"
			e.VerdictTone = "info"
			e.Diagnosis = fmt.Sprintf("实收 %s 与成本持平、零毛利，实际出酒 %d%%（%dml/%dml）；距 %d%% 目标线还差 %s。",
				platform.PriceText(k.RevenueCents), actualYield, k.SoldML, k.CapacityML, cfg.MarginPct, platform.PriceText(e.TargetGapCents))
		default:
			e.Verdict = "loss"
			e.VerdictLabel = "亏损未回本"
			e.VerdictTone = "danger"
			var cause string
			switch {
			case k.SoldML == 0:
				cause = "本桶下架前无售出记录，未能收回任何成本。"
			case lowYield && belowCostPrice:
				cause = fmt.Sprintf("出酒率仅 %d%%（低于 %d%%）且实售均价低于成本线。", actualYield, cfg.SellablePct)
			case lowYield:
				cause = fmt.Sprintf("主因出酒率仅 %d%%（预期 %d%%），管路损耗/漏记或提前下架导致未回本。", actualYield, cfg.SellablePct)
			case belowCostPrice:
				cause = fmt.Sprintf("实售均价低于 %d%% 出酒成本线，建议按 %d%% 推荐价上调单杯售价。", cfg.SellablePct, cfg.MarginPct)
			default:
				cause = "接近回本但微亏，建议核对临近结桶损耗与赠饮。"
			}
			e.Diagnosis = fmt.Sprintf("下架结算亏损 %s（仅回本 %d%%，距回本线差 %s）。%s",
				platform.PriceText(-e.ProfitCents), e.BreakEvenPct, platform.PriceText(e.BreakEvenGapCents), cause)
		}
	case k.TappedAt != nil:
		switch {
		case k.RevenueCents >= targetRev:
			e.Verdict = "target_met"
			e.VerdictLabel = fmt.Sprintf("已达 %d%% 目标", cfg.MarginPct)
			e.VerdictTone = "success"
			e.Diagnosis = fmt.Sprintf("在售中且已超越 %d%% 毛利目标线 %s，当前盈利 %s（毛利率 %s），出酒 %d%%。",
				cfg.MarginPct, platform.PriceText(targetRev), platform.MoneyText(e.ProfitCents), e.MarginText(), actualYield)
		case k.RevenueCents >= k.CostCents:
			e.Verdict = "profitable"
			e.VerdictLabel = "已回本 · 盈利中"
			e.VerdictTone = "info"
			e.Diagnosis = fmt.Sprintf("已过回本线 %s（当前净赚 %s，毛利率 %s），再卖 %s 即可达成 %d%% 毛利目标 %s。",
				platform.PriceText(k.CostCents), platform.MoneyText(e.ProfitCents), e.MarginText(), platform.PriceText(e.TargetGapCents), cfg.MarginPct, platform.PriceText(targetRev))
		default:
			e.Verdict = "recovering"
			e.VerdictLabel = fmt.Sprintf("回本中 %d%%", e.RecoveryPct)
			e.VerdictTone = "warning"
			mlNote := fmt.Sprintf("按 %d%% 毛利约售至 %dml 回本", cfg.MarginPct, e.TheoreticalBreakEven)
			if e.EstimatedBreakEvenML > 0 {
				mlNote = fmt.Sprintf("按当前实售均价约需售至 %dml 回本", e.EstimatedBreakEvenML)
			}
			e.Diagnosis = fmt.Sprintf("已回本 %d%%（%s / %s），距回本还差 %s；%s。",
				e.RecoveryPct, platform.PriceText(k.RevenueCents), platform.PriceText(k.CostCents), platform.PriceText(e.BreakEvenGapCents), mlNote)
		}
	default:
		e.Verdict = "untapped"
		e.VerdictLabel = "待开桶"
		e.VerdictTone = "secondary"
		e.Diagnosis = fmt.Sprintf("库存待开桶：按 %d%% 可售 %dml 测算，回本线 %s（约售 %dml），%d%% 毛利目标营收 %s。",
			cfg.SellablePct, sml, platform.PriceText(k.CostCents), e.TheoreticalBreakEven, cfg.MarginPct, platform.PriceText(targetRev))
	}

	return e
}

func (k Keg) Economics(b *catalog.Beer) KegEconomics {
	return k.EconomicsWithConfig(b, settings.DefaultPricingConfig())
}

func (k Keg) UntapSummaryTextWithConfig(cfg settings.PricingConfig) string {
	cfg = cfg.Normalized()
	e := k.EconomicsWithConfig(nil, cfg)
	switch e.Verdict {
	case "no_cost":
		return fmt.Sprintf("已下架：本桶累计营收 %s（已售 %dml），未填成本可点进酒桶页补填核算",
			platform.PriceText(e.RevenueCents), k.SoldML)
	case "target_met":
		return fmt.Sprintf("已下架：本桶达标盈利 %s（毛利率 %s，出酒 %d%%）",
			platform.MoneyText(e.ProfitCents), e.MarginText(), e.ActualYieldPct)
	case "below_target":
		return fmt.Sprintf("已下架：本桶盈利 %s（毛利率 %s，未达 %d%% 目标，出酒 %d%%）",
			platform.MoneyText(e.ProfitCents), e.MarginText(), cfg.MarginPct, e.ActualYieldPct)
	case "breakeven":
		return fmt.Sprintf("已下架：本桶刚好回本 %s（零毛利，出酒 %d%%）",
			platform.PriceText(e.RevenueCents), e.ActualYieldPct)
	case "loss":
		return fmt.Sprintf("已下架：本桶亏损 %s（回本率 %d%%，出酒 %d%%）",
			platform.PriceText(-e.ProfitCents), e.BreakEvenPct, e.ActualYieldPct)
	default:
		return "已下架，本桶记停售"
	}
}

func (k Keg) UntapSummaryText() string {
	return k.UntapSummaryTextWithConfig(settings.DefaultPricingConfig())
}

// KegSummary 是多桶汇总概览（供 /admin/kegs 页 KPI 卡片使用）。
// 营收/进价/盈亏/毛利率仅统计已开或已结且已填成本的桶，未开桶进价单独计入在库待开成本。
type KegSummary struct {
	TotalCount               int
	ActiveCount              int
	FinishedCount            int
	UntappedCount            int
	TotalRevenueCents        int
	TotalCostCents           int
	TotalProfitCents         int
	StockCostCents           int
	UncostedCount            int
	FinishedUncostedCount    int
	FinishedTargetMetCount   int
	FinishedBelowTargetCount int
	FinishedBreakevenCount   int
	FinishedLossCount        int
	AvgFinishedYieldPct      int
}

func (s KegSummary) TotalRevenueText() string { return platform.PriceText(s.TotalRevenueCents) }
func (s KegSummary) TotalCostText() string    { return platform.PriceText(s.TotalCostCents) }
func (s KegSummary) StockCostText() string    { return platform.PriceText(s.StockCostCents) }
func (s KegSummary) TotalProfitText() string  { return platform.MoneyText(s.TotalProfitCents) }
func (s KegSummary) TotalProfitable() bool    { return s.TotalProfitCents >= 0 }
func (s KegSummary) MarginText() string {
	return MarginPctText(s.TotalRevenueCents, s.TotalCostCents)
}
func (s KegSummary) AvgFinishedYieldText() string {
	if s.FinishedCount == 0 {
		return "—"
	}
	return fmt.Sprintf("%d%%", s.AvgFinishedYieldPct)
}

func (k Keg) SummaryEconomics() KegEconomics { return k.Economics(nil) }

func SummarizeKegsWithConfig(kegs []Keg, cfg settings.PricingConfig) KegSummary {
	cfg = cfg.Normalized()
	var sum KegSummary
	var finSoldML, finCapML int64
	for _, k := range kegs {
		sum.TotalCount++
		started := k.TappedAt != nil || k.FinishedAt != nil
		if started {
			if k.CostCents > 0 {
				sum.TotalRevenueCents += k.RevenueCents
				sum.TotalCostCents += k.CostCents
			} else {
				sum.UncostedCount++
			}
		} else if k.CostCents > 0 {
			sum.StockCostCents += k.CostCents
		}

		e := k.EconomicsWithConfig(nil, cfg)
		switch {
		case k.FinishedAt != nil:
			sum.FinishedCount++
			if k.CapacityML > 0 {
				finSoldML += int64(k.SoldML)
				finCapML += int64(k.CapacityML)
			}
			if k.CostCents <= 0 {
				sum.FinishedUncostedCount++
			} else {
				switch e.Verdict {
				case "target_met":
					sum.FinishedTargetMetCount++
				case "below_target":
					sum.FinishedBelowTargetCount++
				case "breakeven":
					sum.FinishedBreakevenCount++
				case "loss":
					sum.FinishedLossCount++
				}
			}
		case k.TappedAt != nil:
			sum.ActiveCount++
		default:
			sum.UntappedCount++
		}
	}
	sum.TotalProfitCents = sum.TotalRevenueCents - sum.TotalCostCents
	if finCapML > 0 {
		sum.AvgFinishedYieldPct = int((finSoldML*100 + finCapML/2) / finCapML)
	}
	return sum
}

func SummarizeKegs(kegs []Keg) KegSummary {
	return SummarizeKegsWithConfig(kegs, settings.DefaultPricingConfig())
}
