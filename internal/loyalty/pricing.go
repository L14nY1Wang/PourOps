package loyalty

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/internal/ui"
)

// DiscountBPSLabel 将万分比折率格式化为中文折扣文案（如 9500 -> 95折，9000 -> 9折）。
func DiscountBPSLabel(bps int) string {
	switch {
	case bps <= 0:
		return "免单"
	case bps >= 10000:
		return "无折扣"
	case bps%1000 == 0:
		return strconv.Itoa(bps/1000) + "折"
	case bps%100 == 0:
		return strconv.Itoa(bps/100) + "折"
	default:
		return strconv.FormatFloat(float64(bps)/1000, 'f', -1, 64) + "折"
	}
}

// ScopeLabel 返回适用范围的中文标签。
func ScopeLabel(scope string) string {
	switch strings.TrimSpace(scope) {
	case "beer":
		return "仅桶装"
	case "retail":
		return "仅零售"
	default:
		return "全场"
	}
}

// BenefitTypeLabel 返回月卡类型中文标签。
func BenefitTypeLabel(t string) string {
	switch strings.TrimSpace(t) {
	case "discount":
		return "包月折扣"
	case "hybrid":
		return "免单+折扣"
	default:
		return "配额免单"
	}
}

// DiscountKindLabel 返回折扣类型中文标签。
func DiscountKindLabel(k string) string {
	switch strings.TrimSpace(k) {
	case "percent":
		return "百分比折扣"
	case "fixed":
		return "立减金额"
	case "round_down":
		return "抹零"
	case "comp":
		return "全额免单"
	default:
		return k
	}
}

func scopeMatches(scope, kind string) bool {
	switch strings.TrimSpace(scope) {
	case "beer":
		return kind == "beer"
	case "retail":
		return kind == "retail"
	default:
		return true
	}
}

// CanCustomDiscount 判断角色是否允许自定义打折、立减或使用受限折扣。
func CanCustomDiscount(role string) bool {
	return role == ui.RoleBoss || role == ui.RoleManager
}

// PassInfo 表示会员当前持有的一张月卡快照及其本日/总计核销进度。
type PassInfo struct {
	ID           int64
	MemberID     int64
	PlanID       *int64
	PlanName     string
	Scope        string // beer | all
	BenefitType  string // free_quota | discount | hybrid
	DailyLimit   int
	TotalLimit   int
	MaxML        int
	MaxUnitCents int
	CapMode      string // pay_diff | block
	DiscountBPS  int
	PriceCents   int
	PayMethod    string
	StartsAt     time.Time
	ExpiresAt    time.Time
	UsedTotal    int
	UsedToday    int
	Status       string // active | revoked
	Actor        string
	Note         string
	CreatedAt    time.Time
}

func (p PassInfo) HasFreeQuota() bool {
	return p.BenefitType == "free_quota" || p.BenefitType == "hybrid"
}

func (p PassInfo) HasDiscount() bool {
	return (p.BenefitType == "discount" || p.BenefitType == "hybrid") && p.DiscountBPS > 0 && p.DiscountBPS < 10000
}

// RemainingToday 返回该月卡当前营业日还能核销的免单杯数（无每日限制且有总次数时返回剩余总次数）。
func (p PassInfo) RemainingToday() int {
	if p.Status != "active" || !p.HasFreeQuota() {
		return 0
	}
	if p.DailyLimit == 0 && p.TotalLimit == 0 {
		return 0
	}
	rem := -1
	if p.DailyLimit > 0 {
		rem = max(p.DailyLimit-p.UsedToday, 0)
	}
	if p.TotalLimit > 0 {
		totRem := max(p.TotalLimit-p.UsedTotal, 0)
		if rem < 0 || totRem < rem {
			rem = totRem
		}
	}
	return max(rem, 0)
}

func (p PassInfo) ScopeText() string       { return ScopeLabel(p.Scope) }
func (p PassInfo) BenefitTypeText() string { return BenefitTypeLabel(p.BenefitType) }
func (p PassInfo) DiscountText() string    { return DiscountBPSLabel(p.DiscountBPS) }
func (p PassInfo) PriceText() string       { return platform.PriceText(p.PriceCents) }
func (p PassInfo) MaxUnitText() string {
	if p.MaxUnitCents <= 0 {
		return "不限"
	}
	return platform.PriceText(p.MaxUnitCents)
}
func (p PassInfo) CapModeText() string {
	if p.CapMode == "block" {
		return "超额不可用"
	}
	return "超额补差"
}
func (p PassInfo) StartsText() string  { return platform.DateTimeText(&p.StartsAt) }
func (p PassInfo) ExpiresText() string { return platform.DateTimeText(&p.ExpiresAt) }
func (p PassInfo) IsActiveAt(now time.Time) bool {
	return p.Status == "active" && !p.StartsAt.After(now) && p.ExpiresAt.After(now)
}
func (p PassInfo) IsPendingAt(now time.Time) bool {
	return p.Status == "active" && p.StartsAt.After(now) && p.ExpiresAt.After(now)
}
func (p PassInfo) IsExpired() bool {
	return p.Status == "active" && !p.ExpiresAt.After(time.Now())
}
func (p PassInfo) StatusLabel() string {
	if p.Status == "revoked" {
		return "已作废"
	}
	now := time.Now()
	if !p.ExpiresAt.After(now) {
		return "已过期"
	}
	if p.StartsAt.After(now) {
		return "待生效（续费排队）"
	}
	if p.HasFreeQuota() {
		if rem := p.RemainingToday(); rem > 0 {
			return fmt.Sprintf("生效中 · 今剩 %d 杯", rem)
		}
		return "今日次数已用完"
	}
	return "生效中"
}
func (p PassInfo) StatusBadgeClass() string {
	if p.Status == "revoked" || !p.ExpiresAt.After(time.Now()) {
		return "bg-secondary"
	}
	if p.StartsAt.After(time.Now()) {
		return "bg-info text-dark"
	}
	if p.HasFreeQuota() && p.RemainingToday() > 0 {
		return "bg-success"
	}
	return "bg-primary"
}

// MemberSnapshot 汇聚结账与前端预览所需的会员实时快照。
type MemberSnapshot struct {
	ID                    int64
	MemberNo              string
	Name                  string
	Phone                 string
	TierID                *int64
	TierName              string
	TierDiscountBPS       int
	TierScope             string
	TierPointsRateBPS     int
	BalanceCents          int
	PrincipalBalanceCents int
	Points                int
	LifetimeSpentCents    int64
	LifetimeOrders        int
	Birthday              string
	Note                  string
	Active                bool
	ActivePass            *PassInfo
}

func (m MemberSnapshot) BalanceText() string { return platform.PriceText(m.BalanceCents) }
func (m MemberSnapshot) TierDiscountText() string {
	if m.TierDiscountBPS <= 0 || m.TierDiscountBPS >= 10000 {
		return "无折扣"
	}
	return DiscountBPSLabel(m.TierDiscountBPS)
}

// DiscountRule 描述后台配置的一条预设折扣方案。
type DiscountRule struct {
	ID              int64
	Name            string
	Kind            string // percent | fixed | round_down | comp
	DiscountBPS     int
	FixedCents      int
	RoundUnitCents  int
	Scope           string // all | beer | retail
	MinAmountCents  int
	AllowStaff      bool
	StackWithMember bool
	Active          bool
	SortOrder       int
}

func (r DiscountRule) KindText() string  { return DiscountKindLabel(r.Kind) }
func (r DiscountRule) ScopeText() string { return ScopeLabel(r.Scope) }
func (r DiscountRule) Summary() string {
	switch r.Kind {
	case "percent":
		return DiscountBPSLabel(r.DiscountBPS)
	case "fixed":
		return "立减 " + platform.PriceText(r.FixedCents)
	case "round_down":
		return "抹零至 " + platform.PriceText(r.RoundUnitCents) + " 整倍"
	case "comp":
		return "全额免单"
	default:
		return r.Name
	}
}

// CustomDiscount 表示店长/BOSS 在点餐台临时录入的自定义打折或立减。
type CustomDiscount struct {
	Kind        string // percent | fixed
	DiscountBPS int
	FixedCents  int
	Scope       string // all | beer | retail
	Note        string
}

func (c CustomDiscount) Summary() string {
	switch c.Kind {
	case "fixed":
		return "自定义立减 " + platform.PriceText(c.FixedCents)
	default:
		return "自定义" + DiscountBPSLabel(c.DiscountBPS)
	}
}

type OrderLineInput struct {
	ItemID    int64
	Kind      string // beer | retail
	Name      string
	UnitCents int
	Qty       int
	ML        int
}

type LinePricing struct {
	ItemID          int64  `json:"item_id"`
	Kind            string `json:"kind"`
	GrossCents      int    `json:"gross_cents"`
	DiscountCents   int    `json:"discount_cents"`
	PaidCents       int    `json:"paid_cents"`
	PassRedeemedQty int    `json:"pass_redeemed_qty"`
	PassDiscCents   int    `json:"pass_disc_cents"`
	TierDiscCents   int    `json:"tier_disc_cents"`
	ManualDiscCents int    `json:"manual_disc_cents"`
	PointsDiscCents int    `json:"points_disc_cents"`
}

type PricingRequest struct {
	Lines             []OrderLineInput
	Member            *MemberSnapshot
	UsePass           bool
	PassCupsRequested int // <=0 表示按今日可用配额自动核销符合条件最高价杯
	Rule              *DiscountRule
	Custom            *CustomDiscount
	DiscountNote      string
	UsePoints         bool
	UseBalance        bool
	PayMethod         string
	ActorRole         string
	LoyaltyCfg        settings.LoyaltyConfig
}

type PricingResult struct {
	GrossCents          int           `json:"gross_cents"`
	AmountCents         int           `json:"amount_cents"`
	DiscountCents       int           `json:"discount_cents"`
	PassDiscountCents   int           `json:"pass_discount_cents"`
	MemberDiscountCents int           `json:"member_discount_cents"`
	ManualDiscountCents int           `json:"manual_discount_cents"`
	PointsDiscountCents int           `json:"points_discount_cents"`
	PassCupsUsed        int           `json:"pass_cups_used"`
	MemberPassID        *int64        `json:"member_pass_id,omitempty"`
	DiscountRuleID      *int64        `json:"discount_rule_id,omitempty"`
	DiscountLabel       string        `json:"discount_label"`
	DiscountNote        string        `json:"discount_note"`
	PointsSpent         int           `json:"points_spent"`
	PointsEarned        int           `json:"points_earned"`
	BalancePaidCents    int           `json:"balance_paid_cents"`
	ExternalPaidCents   int           `json:"external_paid_cents"`
	LifetimeSpentDelta  int64         `json:"lifetime_spent_delta"`
	Lines               []LinePricing `json:"lines"`
}

// allocateProRata 使用最大余数法把整单优惠分摊到符合条件且仍有余款的明细行，严格保证分摊和零尾差。
func allocateProRata(rems []int, indices []int, discount int) []int {
	alloc := make([]int, len(rems))
	if discount <= 0 || len(indices) == 0 {
		return alloc
	}
	var totalRem int64
	for _, idx := range indices {
		if rems[idx] > 0 {
			totalRem += int64(rems[idx])
		}
	}
	if totalRem <= 0 {
		return alloc
	}
	if int64(discount) >= totalRem {
		for _, idx := range indices {
			if rems[idx] > 0 {
				alloc[idx] = rems[idx]
			}
		}
		return alloc
	}

	type remPart struct {
		idx int
		rem int64
	}
	parts := make([]remPart, 0, len(indices))
	assigned := 0
	for _, idx := range indices {
		if rems[idx] <= 0 {
			continue
		}
		num := int64(discount) * int64(rems[idx])
		base := int(num / totalRem)
		if base > rems[idx] {
			base = rems[idx]
		}
		alloc[idx] = base
		assigned += base
		parts = append(parts, remPart{idx: idx, rem: num % totalRem})
	}
	leftover := discount - assigned
	if leftover > 0 {
		sort.SliceStable(parts, func(i, j int) bool {
			if parts[i].rem != parts[j].rem {
				return parts[i].rem > parts[j].rem
			}
			return parts[i].idx < parts[j].idx
		})
		for _, p := range parts {
			if leftover == 0 {
				break
			}
			if alloc[p.idx] < rems[p.idx] {
				alloc[p.idx]++
				leftover--
			}
		}
	}
	return alloc
}

// CalculateCheckout 是 POS 预览与结账事务共用的确定性纯算价管线。
// 顺序：权限校验 -> 月卡免单核销(免单规则跳过月卡) -> 会员折/包月折/手动折(抹零可叠加) -> 积分抵现 -> 最大余数逐行分摊 -> 余额/补差拆分 -> 攒积分。
func CalculateCheckout(req PricingRequest) (PricingResult, error) {
	if len(req.Lines) == 0 {
		return PricingResult{}, platform.UserErr("先勾选要结账的商品")
	}
	if req.Rule != nil && req.Custom != nil {
		return PricingResult{}, platform.UserErr("不能同时选择预设折扣和自定义折扣")
	}

	privileged := CanCustomDiscount(req.ActorRole)
	if req.Rule != nil {
		if !req.Rule.Active {
			return PricingResult{}, platform.UserErr("该折扣方案已停用")
		}
		if !req.Rule.AllowStaff && !privileged {
			return PricingResult{}, platform.UserErr("当前账号无权使用「" + req.Rule.Name + "」，请联系店长或 BOSS")
		}
	}
	if req.Custom != nil {
		if !privileged {
			return PricingResult{}, platform.UserErr("只有店长或 BOSS 可使用自定义打折或立减")
		}
		switch req.Custom.Kind {
		case "percent":
			if req.Custom.DiscountBPS < 100 || req.Custom.DiscountBPS > 10000 {
				return PricingResult{}, platform.UserErr("自定义折率需在 0.1 折到 10 折之间")
			}
		case "fixed":
			if req.Custom.FixedCents < 0 {
				return PricingResult{}, platform.UserErr("自定义立减金额不能为负")
			}
		default:
			return PricingResult{}, platform.UserErr("不支持的自定义折扣类型")
		}
	}
	if req.Member != nil && !req.Member.Active {
		return PricingResult{}, platform.UserErr("该会员已停用")
	}

	cfg := req.LoyaltyCfg.Normalized()
	rems := make([]int, len(req.Lines))
	lineOut := make([]LinePricing, len(req.Lines))
	grossTotal := 0
	for i, l := range req.Lines {
		if l.UnitCents < 0 || l.Qty <= 0 {
			return PricingResult{}, platform.UserErr("账单商品单价或数量异常")
		}
		g := l.UnitCents * l.Qty
		rems[i] = g
		grossTotal += g
		lineOut[i] = LinePricing{
			ItemID:     l.ItemID,
			Kind:       l.Kind,
			GrossCents: g,
		}
	}

	var res PricingResult
	res.GrossCents = grossTotal
	res.DiscountNote = strings.TrimSpace(req.DiscountNote)
	if req.Custom != nil && res.DiscountNote == "" {
		res.DiscountNote = strings.TrimSpace(req.Custom.Note)
	}
	if len([]rune(res.DiscountNote)) > 200 {
		return PricingResult{}, platform.UserErr("优惠备注过长（最多 200 字）")
	}

	var labels []string
	isComp := req.Rule != nil && req.Rule.Kind == "comp"

	// 1. 月卡配额免单（若选择了免单规则 comp，则保留月卡今日配额不浪费）
	if !isComp && req.UsePass && req.Member != nil && req.Member.ActivePass != nil {
		pass := req.Member.ActivePass
		avail := pass.RemainingToday()
		if req.PassCupsRequested > 0 && req.PassCupsRequested < avail {
			avail = req.PassCupsRequested
		}
		if avail > 0 {
			type cupSlot struct {
				lineIdx   int
				unitCents int
			}
			var slots []cupSlot
			for i, l := range req.Lines {
				if l.UnitCents <= 0 || !scopeMatches(pass.Scope, l.Kind) {
					continue
				}
				if pass.MaxML > 0 && l.Kind == "beer" && l.ML > pass.MaxML {
					continue
				}
				if pass.CapMode == "block" && pass.MaxUnitCents > 0 && l.UnitCents > pass.MaxUnitCents {
					continue
				}
				for q := 0; q < l.Qty; q++ {
					slots = append(slots, cupSlot{lineIdx: i, unitCents: l.UnitCents})
				}
			}
			sort.SliceStable(slots, func(i, j int) bool {
				if slots[i].unitCents != slots[j].unitCents {
					return slots[i].unitCents > slots[j].unitCents
				}
				return slots[i].lineIdx < slots[j].lineIdx
			})
			if len(slots) > avail {
				slots = slots[:avail]
			}
			for _, s := range slots {
				saved := s.unitCents
				if pass.MaxUnitCents > 0 && saved > pass.MaxUnitCents {
					saved = pass.MaxUnitCents
				}
				if saved > rems[s.lineIdx] {
					saved = rems[s.lineIdx]
				}
				if saved <= 0 {
					continue
				}
				rems[s.lineIdx] -= saved
				lineOut[s.lineIdx].PassRedeemedQty++
				lineOut[s.lineIdx].PassDiscCents += saved
				res.PassCupsUsed++
				res.PassDiscountCents += saved
			}
			if res.PassCupsUsed > 0 {
				pid := pass.ID
				res.MemberPassID = &pid
				labels = append(labels, fmt.Sprintf("%s免%d杯", pass.PlanName, res.PassCupsUsed))
			}
		}
	}

	// 工具函数：按范围筛选尚有余款的行索引并计算余款和
	eligibleIndices := func(scope string) ([]int, int) {
		var idxs []int
		sum := 0
		for i, l := range req.Lines {
			if rems[i] > 0 && scopeMatches(scope, l.Kind) {
				idxs = append(idxs, i)
				sum += rems[i]
			}
		}
		return idxs, sum
	}

	applyAutoDiscount := func() {
		var tierDisc, passPctDisc int
		var tierIdxs, passIdxs []int
		if req.Member != nil && req.Member.TierDiscountBPS > 0 && req.Member.TierDiscountBPS < 10000 {
			var sum int
			tierIdxs, sum = eligibleIndices(req.Member.TierScope)
			tierDisc = (sum*(10000-req.Member.TierDiscountBPS) + 5000) / 10000
		}
		if req.UsePass && req.Member != nil && req.Member.ActivePass != nil && req.Member.ActivePass.HasDiscount() {
			var sum int
			passIdxs, sum = eligibleIndices(req.Member.ActivePass.Scope)
			passPctDisc = (sum*(10000-req.Member.ActivePass.DiscountBPS) + 5000) / 10000
		}
		if passPctDisc > tierDisc && passPctDisc > 0 {
			alloc := allocateProRata(rems, passIdxs, passPctDisc)
			for i, d := range alloc {
				rems[i] -= d
				lineOut[i].PassDiscCents += d
			}
			res.PassDiscountCents += passPctDisc
			if res.MemberPassID == nil {
				pid := req.Member.ActivePass.ID
				res.MemberPassID = &pid
			}
			labels = append(labels, fmt.Sprintf("%s(%s)", req.Member.ActivePass.PlanName, DiscountBPSLabel(req.Member.ActivePass.DiscountBPS)))
			return
		}
		if tierDisc > 0 {
			alloc := allocateProRata(rems, tierIdxs, tierDisc)
			for i, d := range alloc {
				rems[i] -= d
				lineOut[i].TierDiscCents += d
			}
			res.MemberDiscountCents += tierDisc
			tierName := strings.TrimSpace(req.Member.TierName)
			if tierName == "" {
				tierName = "会员"
			}
			labels = append(labels, fmt.Sprintf("%s(%s)", tierName, DiscountBPSLabel(req.Member.TierDiscountBPS)))
		}
	}

	applyRuleDiscount := func(rule *DiscountRule) error {
		idxs, sum := eligibleIndices(rule.Scope)
		if rule.MinAmountCents > 0 && sum < rule.MinAmountCents {
			return platform.UserErrf("未达到「%s」使用门槛（需满 %s）", rule.Name, platform.PriceText(rule.MinAmountCents))
		}
		var disc int
		switch rule.Kind {
		case "percent":
			disc = (sum*(10000-rule.DiscountBPS) + 5000) / 10000
		case "fixed":
			disc = min(rule.FixedCents, sum)
		case "round_down":
			if rule.RoundUnitCents > 0 && sum > 0 {
				disc = sum % rule.RoundUnitCents
			}
		case "comp":
			disc = sum
		}
		rid := rule.ID
		res.DiscountRuleID = &rid
		if disc > 0 {
			alloc := allocateProRata(rems, idxs, disc)
			for i, d := range alloc {
				rems[i] -= d
				lineOut[i].ManualDiscCents += d
			}
			res.ManualDiscountCents += disc
			labels = append(labels, rule.Name)
		} else if rule.Kind == "comp" {
			labels = append(labels, rule.Name)
		}
		return nil
	}

	// 2. 剩余金额折扣（默认不折上折；round_down 且开启 stack_with_member 时可叠在会员折后）
	switch {
	case isComp:
		if err := applyRuleDiscount(req.Rule); err != nil {
			return PricingResult{}, err
		}
	case req.Rule != nil && req.Rule.Kind == "round_down" && req.Rule.StackWithMember:
		applyAutoDiscount()
		if err := applyRuleDiscount(req.Rule); err != nil {
			return PricingResult{}, err
		}
	case req.Rule != nil:
		if err := applyRuleDiscount(req.Rule); err != nil {
			return PricingResult{}, err
		}
	case req.Custom != nil:
		scope := req.Custom.Scope
		if scope == "" {
			scope = "all"
		}
		idxs, sum := eligibleIndices(scope)
		var disc int
		switch req.Custom.Kind {
		case "percent":
			disc = (sum*(10000-req.Custom.DiscountBPS) + 5000) / 10000
		case "fixed":
			disc = min(req.Custom.FixedCents, sum)
		}
		if disc > 0 {
			alloc := allocateProRata(rems, idxs, disc)
			for i, d := range alloc {
				rems[i] -= d
				lineOut[i].ManualDiscCents += d
			}
			res.ManualDiscountCents += disc
			labels = append(labels, req.Custom.Summary())
		}
	default:
		applyAutoDiscount()
	}

	// 3. 积分抵现
	if req.UsePoints && req.Member != nil && req.Member.Points > 0 && cfg.PointsRedeemRate > 0 && cfg.PointsMaxPct > 0 {
		idxs, sum := eligibleIndices("all")
		if sum > 0 {
			maxByPct := (sum * cfg.PointsMaxPct) / 100
			maxAffordable := int((int64(req.Member.Points) * 100) / int64(cfg.PointsRedeemRate))
			ptsCents := min(maxByPct, maxAffordable)
			if ptsCents > 0 {
				pointsSpent := int((int64(ptsCents)*int64(cfg.PointsRedeemRate) + 99) / 100)
				if pointsSpent > req.Member.Points {
					pointsSpent = req.Member.Points
				}
				alloc := allocateProRata(rems, idxs, ptsCents)
				for i, d := range alloc {
					rems[i] -= d
					lineOut[i].PointsDiscCents += d
				}
				res.PointsDiscountCents = ptsCents
				res.PointsSpent = pointsSpent
				labels = append(labels, fmt.Sprintf("积分抵%s", platform.PriceText(ptsCents)))
			}
		}
	}

	// 4. 汇总每行最终净实收与总让利
	for i := range lineOut {
		lineOut[i].PaidCents = rems[i]
		lineOut[i].DiscountCents = lineOut[i].GrossCents - rems[i]
		res.AmountCents += rems[i]
	}
	res.DiscountCents = res.GrossCents - res.AmountCents
	res.DiscountLabel = strings.Join(labels, " + ")
	res.Lines = lineOut

	// 5. 储值余额与外部补差拆分
	method := strings.ToLower(strings.TrimSpace(req.PayMethod))
	if method == "balance" || req.UseBalance {
		if req.Member == nil {
			return PricingResult{}, platform.UserErr("请先绑定会员再使用储值余额结账")
		}
		if res.AmountCents > 0 {
			if method == "balance" && req.Member.BalanceCents < res.AmountCents {
				return PricingResult{}, platform.UserErrf("会员储值余额不足（当前 %s，应付 %s），请改选扫码/现金/刷卡并勾选余额抵扣补差",
					platform.PriceText(req.Member.BalanceCents), platform.PriceText(res.AmountCents))
			}
			balPaid := min(res.AmountCents, req.Member.BalanceCents)
			res.BalancePaidCents = balPaid
			res.ExternalPaidCents = res.AmountCents - balPaid
		}
	} else {
		res.ExternalPaidCents = res.AmountCents
	}

	// 6. 实付消费攒积分与累计消费额
	if req.Member != nil && res.AmountCents > 0 {
		res.LifetimeSpentDelta = int64(res.AmountCents)
		if cfg.PointsPerYuan > 0 {
			rateBPS := req.Member.TierPointsRateBPS
			if req.Member.TierID == nil && rateBPS == 0 {
				rateBPS = 10000
			}
			if rateBPS > 0 {
				res.PointsEarned = int(int64(res.AmountCents) * int64(cfg.PointsPerYuan) * int64(rateBPS) / 1000000)
			}
		}
	}

	return res, nil
}
