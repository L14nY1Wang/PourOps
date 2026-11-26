package loyalty

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
)

// ---------- 会员等级 ----------

type Tier struct {
	ID            int64
	Name          string
	SortOrder     int
	MinSpentCents int
	DiscountBPS   int
	ApplyScope    string
	PointsRateBPS int
	Active        bool
	CreatedAt     time.Time
}

func (t Tier) DiscountText() string  { return DiscountBPSLabel(t.DiscountBPS) }
func (t Tier) MinSpentText() string  { return platform.PriceText(t.MinSpentCents) }
func (t Tier) MinSpentInput() string { return platform.MoneyInput(t.MinSpentCents) }
func (t Tier) ScopeText() string     { return ScopeLabel(t.ApplyScope) }
func (t Tier) PointsRateText() string {
	return fmt.Sprintf("%.2gx", float64(t.PointsRateBPS)/10000)
}

func ListTiers(ctx context.Context, q platform.Querier, onlyActive bool) ([]Tier, error) {
	sqlStr := `select id, name, sort_order, min_spent_cents, discount_bps, apply_scope, points_rate_bps, active, created_at
		from member_tiers`
	if onlyActive {
		sqlStr += ` where active`
	}
	sqlStr += ` order by min_spent_cents asc, sort_order asc, id asc`
	rows, err := q.Query(ctx, sqlStr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tier
	for rows.Next() {
		var t Tier
		if err := rows.Scan(&t.ID, &t.Name, &t.SortOrder, &t.MinSpentCents, &t.DiscountBPS, &t.ApplyScope, &t.PointsRateBPS, &t.Active, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func SaveTier(ctx context.Context, pool *pgxpool.Pool, t Tier) (int64, error) {
	t.Name = strings.TrimSpace(t.Name)
	t.ApplyScope = strings.TrimSpace(t.ApplyScope)
	if t.ApplyScope == "" {
		t.ApplyScope = "all"
	}
	if t.Name == "" {
		return 0, platform.UserErr("等级名称不能为空")
	}
	if utf8.RuneCountInString(t.Name) > 40 {
		return 0, platform.UserErr("等级名称最多 40 字")
	}
	if t.MinSpentCents < 0 {
		return 0, platform.UserErr("升级累计消费门槛不能为负")
	}
	if t.DiscountBPS < 100 || t.DiscountBPS > 10000 {
		return 0, platform.UserErr("会员折率需在 0.1 折到无折扣(10000)之间")
	}
	if t.ApplyScope != "all" && t.ApplyScope != "beer" && t.ApplyScope != "retail" {
		return 0, platform.UserErr("适用范围无效")
	}
	if t.PointsRateBPS < 0 || t.PointsRateBPS > 100000 {
		return 0, platform.UserErr("积分倍率超出范围")
	}

	var id int64
	var err error
	if t.ID == 0 {
		err = pool.QueryRow(ctx, `insert into member_tiers
			(name, sort_order, min_spent_cents, discount_bps, apply_scope, points_rate_bps, active)
			values ($1,$2,$3,$4,$5,$6,$7) returning id`,
			t.Name, t.SortOrder, t.MinSpentCents, t.DiscountBPS, t.ApplyScope, t.PointsRateBPS, t.Active).Scan(&id)
	} else {
		err = pool.QueryRow(ctx, `update member_tiers
			set name=$2, sort_order=$3, min_spent_cents=$4, discount_bps=$5, apply_scope=$6, points_rate_bps=$7, active=$8
			where id=$1 returning id`,
			t.ID, t.Name, t.SortOrder, t.MinSpentCents, t.DiscountBPS, t.ApplyScope, t.PointsRateBPS, t.Active).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, platform.UserErr("找不到这个会员等级")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return 0, platform.UserErr("等级名称已存在")
	}
	return id, err
}

func ToggleTier(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	ct, err := pool.Exec(ctx, `update member_tiers set active = not active where id=$1`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return platform.UserErr("找不到这个会员等级")
	}
	return nil
}

// ---------- 预设折扣规则 ----------

func ListDiscountRules(ctx context.Context, q platform.Querier, onlyActive bool) ([]DiscountRule, error) {
	sqlStr := `select id, name, kind, discount_bps, fixed_cents, round_unit_cents, scope,
		min_amount_cents, allow_staff, stack_with_member, active, sort_order
		from discount_rules`
	if onlyActive {
		sqlStr += ` where active`
	}
	sqlStr += ` order by sort_order asc, id asc`
	rows, err := q.Query(ctx, sqlStr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DiscountRule
	for rows.Next() {
		var r DiscountRule
		if err := rows.Scan(&r.ID, &r.Name, &r.Kind, &r.DiscountBPS, &r.FixedCents, &r.RoundUnitCents,
			&r.Scope, &r.MinAmountCents, &r.AllowStaff, &r.StackWithMember, &r.Active, &r.SortOrder); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func GetDiscountRule(ctx context.Context, q platform.Querier, id int64) (DiscountRule, error) {
	var r DiscountRule
	err := q.QueryRow(ctx, `select id, name, kind, discount_bps, fixed_cents, round_unit_cents, scope,
		min_amount_cents, allow_staff, stack_with_member, active, sort_order
		from discount_rules where id=$1`, id).
		Scan(&r.ID, &r.Name, &r.Kind, &r.DiscountBPS, &r.FixedCents, &r.RoundUnitCents,
			&r.Scope, &r.MinAmountCents, &r.AllowStaff, &r.StackWithMember, &r.Active, &r.SortOrder)
	return r, err
}

func SaveDiscountRule(ctx context.Context, pool *pgxpool.Pool, r DiscountRule) (int64, error) {
	r.Name = strings.TrimSpace(r.Name)
	r.Kind = strings.TrimSpace(r.Kind)
	r.Scope = strings.TrimSpace(r.Scope)
	if r.Scope == "" {
		r.Scope = "all"
	}
	if r.Name == "" {
		return 0, platform.UserErr("折扣名称不能为空")
	}
	if utf8.RuneCountInString(r.Name) > 60 {
		return 0, platform.UserErr("折扣名称最多 60 字")
	}
	if r.Scope != "all" && r.Scope != "beer" && r.Scope != "retail" {
		return 0, platform.UserErr("适用范围无效")
	}
	if r.MinAmountCents < 0 {
		return 0, platform.UserErr("门槛金额不能为负")
	}
	switch r.Kind {
	case "percent":
		if r.DiscountBPS < 100 || r.DiscountBPS > 10000 {
			return 0, platform.UserErr("折率需在 0.1 折到 10 折之间")
		}
		r.FixedCents = 0
		r.RoundUnitCents = 100
		r.StackWithMember = false
	case "fixed":
		if r.FixedCents <= 0 {
			return 0, platform.UserErr("立减金额必须大于 0")
		}
		r.DiscountBPS = 10000
		r.RoundUnitCents = 100
		r.StackWithMember = false
	case "round_down":
		if r.RoundUnitCents <= 0 {
			r.RoundUnitCents = 100
		}
		r.DiscountBPS = 10000
		r.FixedCents = 0
	case "comp":
		r.DiscountBPS = 0
		r.FixedCents = 0
		r.RoundUnitCents = 100
		r.StackWithMember = false
	default:
		return 0, platform.UserErr("不支持的折扣类型")
	}

	var id int64
	var err error
	if r.ID == 0 {
		err = pool.QueryRow(ctx, `insert into discount_rules
			(name, kind, discount_bps, fixed_cents, round_unit_cents, scope, min_amount_cents, allow_staff, stack_with_member, active, sort_order)
			values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) returning id`,
			r.Name, r.Kind, r.DiscountBPS, r.FixedCents, r.RoundUnitCents, r.Scope, r.MinAmountCents,
			r.AllowStaff, r.StackWithMember, r.Active, r.SortOrder).Scan(&id)
	} else {
		err = pool.QueryRow(ctx, `update discount_rules
			set name=$2, kind=$3, discount_bps=$4, fixed_cents=$5, round_unit_cents=$6, scope=$7,
			    min_amount_cents=$8, allow_staff=$9, stack_with_member=$10, active=$11, sort_order=$12
			where id=$1 returning id`,
			r.ID, r.Name, r.Kind, r.DiscountBPS, r.FixedCents, r.RoundUnitCents, r.Scope, r.MinAmountCents,
			r.AllowStaff, r.StackWithMember, r.Active, r.SortOrder).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, platform.UserErr("找不到这个折扣规则")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return 0, platform.UserErr("折扣名称已存在")
	}
	return id, err
}

func ToggleDiscountRule(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	ct, err := pool.Exec(ctx, `update discount_rules set active = not active where id=$1`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return platform.UserErr("找不到这个折扣规则")
	}
	return nil
}

func DeleteDiscountRule(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	ct, err := pool.Exec(ctx, `delete from discount_rules where id=$1`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return platform.UserErr("找不到这个折扣规则")
	}
	return nil
}

// ---------- 月卡方案模板 ----------

type PassPlan struct {
	ID           int64
	Name         string
	PriceCents   int
	DurationDays int
	Scope        string // beer | all
	BenefitType  string // free_quota | discount | hybrid
	DailyLimit   int
	TotalLimit   int
	MaxML        int
	MaxUnitCents int
	CapMode      string // pay_diff | block
	DiscountBPS  int
	Active       bool
	SortOrder    int
	Note         string
	CreatedAt    time.Time
}

func (p PassPlan) PriceText() string       { return platform.PriceText(p.PriceCents) }
func (p PassPlan) PriceInput() string      { return platform.MoneyInput(p.PriceCents) }
func (p PassPlan) ScopeText() string       { return ScopeLabel(p.Scope) }
func (p PassPlan) BenefitTypeText() string { return BenefitTypeLabel(p.BenefitType) }
func (p PassPlan) DiscountText() string    { return DiscountBPSLabel(p.DiscountBPS) }
func (p PassPlan) MaxUnitText() string {
	if p.MaxUnitCents <= 0 {
		return "不限"
	}
	return platform.PriceText(p.MaxUnitCents)
}
func (p PassPlan) RuleSummary() string {
	var parts []string
	parts = append(parts, fmt.Sprintf("%d天", p.DurationDays))
	parts = append(parts, ScopeLabel(p.Scope))
	if p.BenefitType == "free_quota" || p.BenefitType == "hybrid" {
		if p.DailyLimit > 0 {
			parts = append(parts, fmt.Sprintf("每日%d杯", p.DailyLimit))
		}
		if p.TotalLimit > 0 {
			parts = append(parts, fmt.Sprintf("共%d杯", p.TotalLimit))
		}
		if p.MaxML > 0 {
			parts = append(parts, fmt.Sprintf("≤%dml", p.MaxML))
		}
		if p.MaxUnitCents > 0 {
			mode := "超额补差"
			if p.CapMode == "block" {
				mode = "超额不可用"
			}
			parts = append(parts, fmt.Sprintf("封顶%s(%s)", platform.PriceText(p.MaxUnitCents), mode))
		}
	}
	if p.BenefitType == "discount" || p.BenefitType == "hybrid" {
		parts = append(parts, DiscountBPSLabel(p.DiscountBPS))
	}
	return strings.Join(parts, " · ")
}

func ListPassPlans(ctx context.Context, q platform.Querier, onlyActive bool) ([]PassPlan, error) {
	sqlStr := `select id, name, price_cents, duration_days, scope, benefit_type, daily_limit, total_limit,
		max_ml, max_unit_cents, cap_mode, discount_bps, active, sort_order, note, created_at
		from pass_plans`
	if onlyActive {
		sqlStr += ` where active`
	}
	sqlStr += ` order by sort_order asc, id asc`
	rows, err := q.Query(ctx, sqlStr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PassPlan
	for rows.Next() {
		var p PassPlan
		if err := rows.Scan(&p.ID, &p.Name, &p.PriceCents, &p.DurationDays, &p.Scope, &p.BenefitType,
			&p.DailyLimit, &p.TotalLimit, &p.MaxML, &p.MaxUnitCents, &p.CapMode, &p.DiscountBPS,
			&p.Active, &p.SortOrder, &p.Note, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func GetPassPlan(ctx context.Context, q platform.Querier, id int64) (PassPlan, error) {
	var p PassPlan
	err := q.QueryRow(ctx, `select id, name, price_cents, duration_days, scope, benefit_type, daily_limit, total_limit,
		max_ml, max_unit_cents, cap_mode, discount_bps, active, sort_order, note, created_at
		from pass_plans where id=$1`, id).
		Scan(&p.ID, &p.Name, &p.PriceCents, &p.DurationDays, &p.Scope, &p.BenefitType,
			&p.DailyLimit, &p.TotalLimit, &p.MaxML, &p.MaxUnitCents, &p.CapMode, &p.DiscountBPS,
			&p.Active, &p.SortOrder, &p.Note, &p.CreatedAt)
	return p, err
}

func SavePassPlan(ctx context.Context, pool *pgxpool.Pool, p PassPlan) (int64, error) {
	p.Name = strings.TrimSpace(p.Name)
	p.Note = strings.TrimSpace(p.Note)
	p.Scope = strings.TrimSpace(p.Scope)
	p.BenefitType = strings.TrimSpace(p.BenefitType)
	p.CapMode = strings.TrimSpace(p.CapMode)
	if p.Scope == "" {
		p.Scope = "beer"
	}
	if p.BenefitType == "" {
		p.BenefitType = "free_quota"
	}
	if p.CapMode == "" {
		p.CapMode = "pay_diff"
	}
	if p.DurationDays == 0 {
		p.DurationDays = 31
	}
	if p.Name == "" {
		return 0, platform.UserErr("月卡方案名称不能为空")
	}
	if utf8.RuneCountInString(p.Name) > 80 {
		return 0, platform.UserErr("月卡名称最多 80 字")
	}
	if p.PriceCents < 0 {
		return 0, platform.UserErr("月卡售价不能为负")
	}
	if p.DurationDays < 1 || p.DurationDays > 3650 {
		return 0, platform.UserErr("有效天数需在 1 到 3650 天之间")
	}
	if p.Scope != "beer" && p.Scope != "all" {
		return 0, platform.UserErr("月卡适用范围无效")
	}
	if p.BenefitType != "free_quota" && p.BenefitType != "discount" && p.BenefitType != "hybrid" {
		return 0, platform.UserErr("月卡权益类型无效")
	}
	if p.DailyLimit < 0 || p.DailyLimit > 99 || p.TotalLimit < 0 || p.TotalLimit > 9999 {
		return 0, platform.UserErr("月卡次数限制超出范围")
	}
	if p.MaxML < 0 || p.MaxUnitCents < 0 {
		return 0, platform.UserErr("杯型和单杯封顶金额不能为负")
	}
	if p.CapMode != "pay_diff" && p.CapMode != "block" {
		return 0, platform.UserErr("超额处理方式无效")
	}
	if (p.BenefitType == "free_quota" || p.BenefitType == "hybrid") && p.DailyLimit == 0 && p.TotalLimit == 0 {
		return 0, platform.UserErr("配额免单型月卡至少需设置每日次数或总次数上限")
	}
	if p.BenefitType == "free_quota" {
		p.DiscountBPS = 10000
	} else if p.DiscountBPS < 100 || p.DiscountBPS > 9999 {
		return 0, platform.UserErr("包月折率需在 0.1 折到 9.99 折之间")
	}

	var id int64
	var err error
	if p.ID == 0 {
		err = pool.QueryRow(ctx, `insert into pass_plans
			(name, price_cents, duration_days, scope, benefit_type, daily_limit, total_limit,
			 max_ml, max_unit_cents, cap_mode, discount_bps, active, sort_order, note)
			values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) returning id`,
			p.Name, p.PriceCents, p.DurationDays, p.Scope, p.BenefitType, p.DailyLimit, p.TotalLimit,
			p.MaxML, p.MaxUnitCents, p.CapMode, p.DiscountBPS, p.Active, p.SortOrder, p.Note).Scan(&id)
	} else {
		err = pool.QueryRow(ctx, `update pass_plans
			set name=$2, price_cents=$3, duration_days=$4, scope=$5, benefit_type=$6, daily_limit=$7,
			    total_limit=$8, max_ml=$9, max_unit_cents=$10, cap_mode=$11, discount_bps=$12,
			    active=$13, sort_order=$14, note=$15
			where id=$1 returning id`,
			p.ID, p.Name, p.PriceCents, p.DurationDays, p.Scope, p.BenefitType, p.DailyLimit,
			p.TotalLimit, p.MaxML, p.MaxUnitCents, p.CapMode, p.DiscountBPS,
			p.Active, p.SortOrder, p.Note).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, platform.UserErr("找不到这个月卡方案")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return 0, platform.UserErr("月卡方案名称已存在")
	}
	return id, err
}

func TogglePassPlan(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	ct, err := pool.Exec(ctx, `update pass_plans set active = not active where id=$1`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return platform.UserErr("找不到这个月卡方案")
	}
	return nil
}

// ---------- 会员档案、储值、积分与持卡 ----------

type Member struct {
	ID                    int64
	MemberNo              string
	Name                  string
	Phone                 string
	TierID                *int64
	TierName              string
	TierDiscountBPS       int
	BalanceCents          int
	PrincipalBalanceCents int
	Points                int
	LifetimeSpentCents    int64
	LifetimeOrders        int
	Birthday              string
	Note                  string
	Active                bool
	CreatedAt             time.Time
	UpdatedAt             time.Time
	ActivePass            *PassInfo
}

func (m Member) BalanceText() string       { return platform.PriceText(m.BalanceCents) }
func (m Member) PrincipalText() string     { return platform.PriceText(m.PrincipalBalanceCents) }
func (m Member) LifetimeSpentText() string { return platform.PriceText(int(m.LifetimeSpentCents)) }
func (m Member) CreatedText() string       { return platform.DateTimeText(&m.CreatedAt) }
func (m Member) TierIDVal() int64 {
	if m.TierID == nil {
		return 0
	}
	return *m.TierID
}
func (m Member) TierDiscountText() string {
	if m.TierDiscountBPS <= 0 || m.TierDiscountBPS >= 10000 {
		return "无折扣"
	}
	return DiscountBPSLabel(m.TierDiscountBPS)
}

type BalanceTxn struct {
	ID                  int64
	MemberID            int64
	Kind                string
	DeltaCents          int
	PrincipalDeltaCents int
	BalanceAfterCents   int
	PayMethod           string
	PaymentID           *int64
	Actor               string
	Note                string
	CreatedAt           time.Time
}

func (t BalanceTxn) KindText() string {
	switch t.Kind {
	case "recharge":
		return "充值本金"
	case "recharge_bonus":
		return "充值赠金"
	case "consume":
		return "消费扣款"
	case "adjust":
		return "人工调账"
	case "refund":
		return "退回余额"
	case "pass_buy":
		return "余额购月卡"
	default:
		return t.Kind
	}
}
func (t BalanceTxn) DeltaText() string        { return platform.MoneyText(t.DeltaCents) }
func (t BalanceTxn) BalanceAfterText() string { return platform.PriceText(t.BalanceAfterCents) }
func (t BalanceTxn) CreatedText() string      { return platform.DateTimeText(&t.CreatedAt) }

type PointTxn struct {
	ID          int64
	MemberID    int64
	Kind        string
	DeltaPoints int
	PointsAfter int
	PaymentID   *int64
	Actor       string
	Note        string
	CreatedAt   time.Time
}

func (t PointTxn) KindText() string {
	switch t.Kind {
	case "earn":
		return "消费攒分"
	case "redeem":
		return "积分抵现"
	case "adjust":
		return "人工调整"
	default:
		return t.Kind
	}
}
func (t PointTxn) DeltaText() string {
	if t.DeltaPoints > 0 {
		return "+" + strconv.Itoa(t.DeltaPoints)
	}
	return strconv.Itoa(t.DeltaPoints)
}
func (t PointTxn) CreatedText() string { return platform.DateTimeText(&t.CreatedAt) }

type PassUsage struct {
	ID          int64
	PassID      int64
	PlanName    string
	MemberID    int64
	MemberName  string
	MemberPhone string
	OrderID     int64
	PaymentID   int64
	BusinessDay string
	Cups        int
	SavedCents  int
	CreatedAt   time.Time
}

func (u PassUsage) SavedText() string   { return platform.PriceText(u.SavedCents) }
func (u PassUsage) CreatedText() string { return platform.DateTimeText(&u.CreatedAt) }

type MemberDetail struct {
	Member      Member
	Passes      []PassInfo
	BalanceTxns []BalanceTxn
	PointTxns   []PointTxn
	PassUsages  []PassUsage
}

type MemberSummary struct {
	TotalCount        int
	ActiveCount       int
	ActivePassCount   int
	TotalBalanceCents int64
	TotalPoints       int64
}

func (s MemberSummary) TotalBalanceText() string { return platform.MoneyText64(s.TotalBalanceCents) }

func cleanPhone(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	if s == "" {
		return "", platform.UserErr("手机号不能为空")
	}
	if len(s) < 6 || len(s) > 20 {
		return "", platform.UserErr("手机号长度需在 6 到 20 位之间")
	}
	for i, r := range s {
		if r >= '0' && r <= '9' {
			continue
		}
		if i == 0 && r == '+' {
			continue
		}
		return "", platform.UserErr("手机号只能包含数字或 + 号")
	}
	return s, nil
}

func validateMemberFields(name, phone, memberNo, birthday, note string) (string, string, string, string, string, error) {
	n := strings.TrimSpace(name)
	if n == "" {
		return "", "", "", "", "", platform.UserErr("会员姓名/昵称不能为空")
	}
	if utf8.RuneCountInString(n) > 40 {
		return "", "", "", "", "", platform.UserErr("会员姓名/昵称最多 40 字")
	}
	p, err := cleanPhone(phone)
	if err != nil {
		return "", "", "", "", "", err
	}
	no := strings.TrimSpace(memberNo)
	if utf8.RuneCountInString(no) > 32 {
		return "", "", "", "", "", platform.UserErr("会员卡号过长（最多 32 字）")
	}
	bd := strings.TrimSpace(birthday)
	if utf8.RuneCountInString(bd) > 20 {
		return "", "", "", "", "", platform.UserErr("生日格式过长")
	}
	nt := strings.TrimSpace(note)
	if utf8.RuneCountInString(nt) > 300 {
		return "", "", "", "", "", platform.UserErr("备注最多 300 字")
	}
	return n, p, no, bd, nt, nil
}

func normalizeRechargePayMethod(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "", "qr":
		return "qr"
	case "cash":
		return "cash"
	case "card":
		return "card"
	case "other":
		return "other"
	case "balance":
		return "balance"
	default:
		return "qr"
	}
}

func loadActivePassForMember(ctx context.Context, q platform.Querier, memberID int64, now time.Time, lockForUpdate bool) (*PassInfo, error) {
	bizDay := platform.BusinessDayKey(now)
	lockClause := ""
	if lockForUpdate {
		lockClause = " for update"
	}
	var p PassInfo
	err := q.QueryRow(ctx, `
		select id, member_id, plan_id, plan_name, scope, benefit_type, daily_limit, total_limit,
		       max_ml, max_unit_cents, cap_mode, discount_bps, price_cents, pay_method,
		       starts_at, expires_at, used_total, status, actor, note, created_at
		from member_passes
		where member_id=$1 and status='active' and starts_at <= $2 and expires_at > $2
		order by expires_at asc, id asc
		limit 1`+lockClause, memberID, now).
		Scan(&p.ID, &p.MemberID, &p.PlanID, &p.PlanName, &p.Scope, &p.BenefitType, &p.DailyLimit, &p.TotalLimit,
			&p.MaxML, &p.MaxUnitCents, &p.CapMode, &p.DiscountBPS, &p.PriceCents, &p.PayMethod,
			&p.StartsAt, &p.ExpiresAt, &p.UsedTotal, &p.Status, &p.Actor, &p.Note, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := q.QueryRow(ctx, `select coalesce(sum(cups), 0)::int
		from member_pass_usages where member_pass_id=$1 and business_day=$2`, p.ID, bizDay).
		Scan(&p.UsedToday); err != nil {
		return nil, err
	}
	return &p, nil
}

func loadPassByIDForMember(ctx context.Context, q platform.Querier, memberID, passID int64, now time.Time, lockForUpdate bool) (*PassInfo, error) {
	bizDay := platform.BusinessDayKey(now)
	lockClause := ""
	if lockForUpdate {
		lockClause = " for update"
	}
	var p PassInfo
	err := q.QueryRow(ctx, `
		select id, member_id, plan_id, plan_name, scope, benefit_type, daily_limit, total_limit,
		       max_ml, max_unit_cents, cap_mode, discount_bps, price_cents, pay_method,
		       starts_at, expires_at, used_total, status, actor, note, created_at
		from member_passes
		where id=$1 and member_id=$2`+lockClause, passID, memberID).
		Scan(&p.ID, &p.MemberID, &p.PlanID, &p.PlanName, &p.Scope, &p.BenefitType, &p.DailyLimit, &p.TotalLimit,
			&p.MaxML, &p.MaxUnitCents, &p.CapMode, &p.DiscountBPS, &p.PriceCents, &p.PayMethod,
			&p.StartsAt, &p.ExpiresAt, &p.UsedTotal, &p.Status, &p.Actor, &p.Note, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platform.UserErr("找不到这张月卡或月卡不属于当前会员")
	}
	if err != nil {
		return nil, err
	}
	if !p.IsActiveAt(now) {
		return nil, platform.UserErr("该月卡已作废、已过期或尚未生效")
	}
	if err := q.QueryRow(ctx, `select coalesce(sum(cups), 0)::int
		from member_pass_usages where member_pass_id=$1 and business_day=$2`, p.ID, bizDay).
		Scan(&p.UsedToday); err != nil {
		return nil, err
	}
	return &p, nil
}

// LoadCheckoutMemberSnapshot 读取结账或账单展示所需的会员档案与有效月卡（lockForUpdate=true 时严格按 members -> member_passes 顺序加行锁）。
func LoadCheckoutMemberSnapshot(ctx context.Context, q platform.Querier, memberID, passID int64, now time.Time, lockForUpdate bool) (*MemberSnapshot, error) {
	if memberID <= 0 {
		return nil, nil
	}
	lockClause := ""
	if lockForUpdate {
		lockClause = " for update of m"
	}
	var snap MemberSnapshot
	var tierName *string
	var tierDisc, tierPts *int
	var tierScope *string
	err := q.QueryRow(ctx, `
		select m.id, m.member_no, m.name, m.phone, m.tier_id,
		       t.name, t.discount_bps, t.apply_scope, t.points_rate_bps,
		       m.balance_cents, m.principal_balance_cents, m.points,
		       m.lifetime_spent_cents, m.lifetime_orders, m.birthday, m.note, m.active
		from members m
		left join member_tiers t on t.id = m.tier_id and t.active
		where m.id = $1`+lockClause, memberID).
		Scan(&snap.ID, &snap.MemberNo, &snap.Name, &snap.Phone, &snap.TierID,
			&tierName, &tierDisc, &tierScope, &tierPts,
			&snap.BalanceCents, &snap.PrincipalBalanceCents, &snap.Points,
			&snap.LifetimeSpentCents, &snap.LifetimeOrders, &snap.Birthday, &snap.Note, &snap.Active)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platform.UserErr("找不到这个会员")
	}
	if err != nil {
		return nil, err
	}
	if tierName != nil {
		snap.TierName = *tierName
	}
	if tierDisc != nil {
		snap.TierDiscountBPS = *tierDisc
	} else {
		snap.TierDiscountBPS = 10000
	}
	if tierScope != nil {
		snap.TierScope = *tierScope
	} else {
		snap.TierScope = "all"
	}
	if tierPts != nil {
		snap.TierPointsRateBPS = *tierPts
	} else {
		snap.TierPointsRateBPS = 10000
	}

	if passID > 0 {
		pass, err := loadPassByIDForMember(ctx, q, memberID, passID, now, lockForUpdate)
		if err != nil {
			return nil, err
		}
		snap.ActivePass = pass
	} else {
		pass, err := loadActivePassForMember(ctx, q, memberID, now, lockForUpdate)
		if err != nil {
			return nil, err
		}
		snap.ActivePass = pass
	}
	return &snap, nil
}

func MemberSummaryStats(ctx context.Context, pool *pgxpool.Pool) (MemberSummary, error) {
	var s MemberSummary
	now := time.Now()
	err := pool.QueryRow(ctx, `
		select count(*)::int,
		       count(*) filter (where active)::int,
		       coalesce(sum(balance_cents), 0)::bigint,
		       coalesce(sum(points), 0)::bigint,
		       (select count(*)::int from member_passes where status='active' and starts_at <= $1 and expires_at > $1)
		from members`, now).
		Scan(&s.TotalCount, &s.ActiveCount, &s.TotalBalanceCents, &s.TotalPoints, &s.ActivePassCount)
	return s, err
}

func ListMembers(ctx context.Context, pool *pgxpool.Pool, q string, tierID int64, passFilter string) ([]Member, error) {
	q = strings.TrimSpace(q)
	now := time.Now()
	bizDay := platform.BusinessDayKey(now)
	rows, err := pool.Query(ctx, `
		select m.id, m.member_no, m.name, m.phone, m.tier_id,
		       coalesce(t.name, '默认等级'), coalesce(t.discount_bps, 10000),
		       m.balance_cents, m.principal_balance_cents, m.points,
		       m.lifetime_spent_cents, m.lifetime_orders, m.birthday, m.note, m.active,
		       m.created_at, m.updated_at,
		       ap.id, ap.plan_name, ap.scope, ap.benefit_type, ap.daily_limit, ap.total_limit,
		       ap.max_ml, ap.max_unit_cents, ap.cap_mode, ap.discount_bps,
		       ap.starts_at, ap.expires_at, ap.used_total, ap.status, ap.used_today
		from members m
		left join member_tiers t on t.id = m.tier_id
		left join lateral (
			select mp.id, mp.plan_name, mp.scope, mp.benefit_type, mp.daily_limit, mp.total_limit,
			       mp.max_ml, mp.max_unit_cents, mp.cap_mode, mp.discount_bps,
			       mp.starts_at, mp.expires_at, mp.used_total, mp.status,
			       coalesce((select sum(u.cups)::int from member_pass_usages u where u.member_pass_id = mp.id and u.business_day = $4), 0) as used_today
			from member_passes mp
			where mp.member_id = m.id and mp.status='active' and mp.starts_at <= $3 and mp.expires_at > $3
			order by mp.expires_at asc, mp.id asc
			limit 1
		) ap on true
		where ($1 = '' or m.name ilike '%'||$1||'%' or m.phone ilike '%'||$1||'%' or m.member_no ilike '%'||$1||'%')
		  and ($2 = 0 or m.tier_id = $2)
		  and ($5 = '' or ($5 = 'has_pass' and ap.id is not null) or ($5 = 'no_pass' and ap.id is null))
		order by m.active desc, m.id desc
		limit 200`, q, tierID, now, bizDay, passFilter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		var passID *int64
		var planName, scope, benefitType, capMode, passStatus *string
		var dailyLimit, totalLimit, maxML, maxUnit, discBPS, usedTotal, usedToday *int
		var startsAt, expiresAt *time.Time
		if err := rows.Scan(&m.ID, &m.MemberNo, &m.Name, &m.Phone, &m.TierID,
			&m.TierName, &m.TierDiscountBPS,
			&m.BalanceCents, &m.PrincipalBalanceCents, &m.Points,
			&m.LifetimeSpentCents, &m.LifetimeOrders, &m.Birthday, &m.Note, &m.Active,
			&m.CreatedAt, &m.UpdatedAt,
			&passID, &planName, &scope, &benefitType, &dailyLimit, &totalLimit,
			&maxML, &maxUnit, &capMode, &discBPS, &startsAt, &expiresAt, &usedTotal, &passStatus, &usedToday); err != nil {
			return nil, err
		}
		if passID != nil {
			m.ActivePass = &PassInfo{
				ID:           *passID,
				MemberID:     m.ID,
				PlanName:     *planName,
				Scope:        *scope,
				BenefitType:  *benefitType,
				DailyLimit:   *dailyLimit,
				TotalLimit:   *totalLimit,
				MaxML:        *maxML,
				MaxUnitCents: *maxUnit,
				CapMode:      *capMode,
				DiscountBPS:  *discBPS,
				StartsAt:     *startsAt,
				ExpiresAt:    *expiresAt,
				UsedTotal:    *usedTotal,
				UsedToday:    *usedToday,
				Status:       *passStatus,
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SearchMembers 供 POS 会员检索框使用，支持手机号/后四位/姓名/卡号快搜，返回含有效月卡状态的会员快照。
func SearchMembers(ctx context.Context, pool *pgxpool.Pool, q string, limit int) ([]MemberSnapshot, error) {
	q = strings.TrimSpace(q)
	if limit <= 0 || limit > 30 {
		limit = 12
	}
	now := time.Now()
	bizDay := platform.BusinessDayKey(now)
	rows, err := pool.Query(ctx, `
		select m.id, m.member_no, m.name, m.phone, m.tier_id,
		       coalesce(t.name, '普通会员'), coalesce(t.discount_bps, 10000), coalesce(t.apply_scope, 'all'), coalesce(t.points_rate_bps, 10000),
		       m.balance_cents, m.principal_balance_cents, m.points, m.lifetime_spent_cents, m.lifetime_orders, m.active,
		       ap.id, ap.plan_name, ap.scope, ap.benefit_type, ap.daily_limit, ap.total_limit,
		       ap.max_ml, ap.max_unit_cents, ap.cap_mode, ap.discount_bps, ap.starts_at, ap.expires_at, ap.used_total, ap.used_today
		from members m
		left join member_tiers t on t.id = m.tier_id and t.active
		left join lateral (
			select mp.id, mp.plan_name, mp.scope, mp.benefit_type, mp.daily_limit, mp.total_limit,
			       mp.max_ml, mp.max_unit_cents, mp.cap_mode, mp.discount_bps, mp.starts_at, mp.expires_at, mp.used_total,
			       coalesce((select sum(u.cups)::int from member_pass_usages u where u.member_pass_id = mp.id and u.business_day = $3), 0) as used_today
			from member_passes mp
			where mp.member_id = m.id and mp.status='active' and mp.starts_at <= $2 and mp.expires_at > $2
			order by mp.expires_at asc, mp.id asc
			limit 1
		) ap on true
		where m.active
		  and ($1 = '' or m.phone ilike '%'||$1||'%' or m.name ilike '%'||$1||'%' or m.member_no ilike '%'||$1||'%')
		order by (btrim(m.phone) = $1) desc, (m.phone like '%'||$1) desc, m.lifetime_spent_cents desc, m.id desc
		limit $4`, q, now, bizDay, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemberSnapshot
	for rows.Next() {
		var m MemberSnapshot
		var passID *int64
		var planName, scope, benefitType, capMode *string
		var dailyLimit, totalLimit, maxML, maxUnit, discBPS, usedTotal, usedToday *int
		var startsAt, expiresAt *time.Time
		if err := rows.Scan(&m.ID, &m.MemberNo, &m.Name, &m.Phone, &m.TierID,
			&m.TierName, &m.TierDiscountBPS, &m.TierScope, &m.TierPointsRateBPS,
			&m.BalanceCents, &m.PrincipalBalanceCents, &m.Points, &m.LifetimeSpentCents, &m.LifetimeOrders, &m.Active,
			&passID, &planName, &scope, &benefitType, &dailyLimit, &totalLimit,
			&maxML, &maxUnit, &capMode, &discBPS, &startsAt, &expiresAt, &usedTotal, &usedToday); err != nil {
			return nil, err
		}
		if passID != nil {
			m.ActivePass = &PassInfo{
				ID:           *passID,
				MemberID:     m.ID,
				PlanName:     *planName,
				Scope:        *scope,
				BenefitType:  *benefitType,
				DailyLimit:   *dailyLimit,
				TotalLimit:   *totalLimit,
				MaxML:        *maxML,
				MaxUnitCents: *maxUnit,
				CapMode:      *capMode,
				DiscountBPS:  *discBPS,
				StartsAt:     *startsAt,
				ExpiresAt:    *expiresAt,
				UsedTotal:    *usedTotal,
				UsedToday:    *usedToday,
				Status:       "active",
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func GetMemberDetail(ctx context.Context, pool *pgxpool.Pool, id int64) (MemberDetail, error) {
	var d MemberDetail
	now := time.Now()
	bizDay := platform.BusinessDayKey(now)
	err := pool.QueryRow(ctx, `
		select m.id, m.member_no, m.name, m.phone, m.tier_id,
		       coalesce(t.name, '默认等级'), coalesce(t.discount_bps, 10000),
		       m.balance_cents, m.principal_balance_cents, m.points,
		       m.lifetime_spent_cents, m.lifetime_orders, m.birthday, m.note, m.active,
		       m.created_at, m.updated_at
		from members m
		left join member_tiers t on t.id = m.tier_id
		where m.id=$1`, id).
		Scan(&d.Member.ID, &d.Member.MemberNo, &d.Member.Name, &d.Member.Phone, &d.Member.TierID,
			&d.Member.TierName, &d.Member.TierDiscountBPS,
			&d.Member.BalanceCents, &d.Member.PrincipalBalanceCents, &d.Member.Points,
			&d.Member.LifetimeSpentCents, &d.Member.LifetimeOrders, &d.Member.Birthday, &d.Member.Note, &d.Member.Active,
			&d.Member.CreatedAt, &d.Member.UpdatedAt)
	if err != nil {
		return d, err
	}

	prows, err := pool.Query(ctx, `
		select mp.id, mp.member_id, mp.plan_id, mp.plan_name, mp.scope, mp.benefit_type, mp.daily_limit, mp.total_limit,
		       mp.max_ml, mp.max_unit_cents, mp.cap_mode, mp.discount_bps, mp.price_cents, mp.pay_method,
		       mp.starts_at, mp.expires_at, mp.used_total, mp.status, mp.actor, mp.note, mp.created_at,
		       coalesce((select sum(u.cups)::int from member_pass_usages u where u.member_pass_id = mp.id and u.business_day = $2), 0)
		from member_passes mp
		where mp.member_id=$1
		order by (mp.status='active' and mp.expires_at > $3) desc, mp.starts_at desc, mp.id desc
		limit 50`, id, bizDay, now)
	if err != nil {
		return d, err
	}
	for prows.Next() {
		var p PassInfo
		if err := prows.Scan(&p.ID, &p.MemberID, &p.PlanID, &p.PlanName, &p.Scope, &p.BenefitType,
			&p.DailyLimit, &p.TotalLimit, &p.MaxML, &p.MaxUnitCents, &p.CapMode, &p.DiscountBPS,
			&p.PriceCents, &p.PayMethod, &p.StartsAt, &p.ExpiresAt, &p.UsedTotal, &p.Status,
			&p.Actor, &p.Note, &p.CreatedAt, &p.UsedToday); err != nil {
			prows.Close()
			return d, err
		}
		if d.Member.ActivePass == nil && p.IsActiveAt(now) {
			cp := p
			d.Member.ActivePass = &cp
		}
		d.Passes = append(d.Passes, p)
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return d, err
	}

	brows, err := pool.Query(ctx, `
		select id, member_id, kind, delta_cents, principal_delta_cents, balance_after_cents,
		       pay_method, payment_id, actor, note, created_at
		from member_balance_txns where member_id=$1 order by id desc limit 50`, id)
	if err != nil {
		return d, err
	}
	for brows.Next() {
		var t BalanceTxn
		if err := brows.Scan(&t.ID, &t.MemberID, &t.Kind, &t.DeltaCents, &t.PrincipalDeltaCents,
			&t.BalanceAfterCents, &t.PayMethod, &t.PaymentID, &t.Actor, &t.Note, &t.CreatedAt); err != nil {
			brows.Close()
			return d, err
		}
		d.BalanceTxns = append(d.BalanceTxns, t)
	}
	brows.Close()
	if err := brows.Err(); err != nil {
		return d, err
	}

	ptrows, err := pool.Query(ctx, `
		select id, member_id, kind, delta_points, points_after, payment_id, actor, note, created_at
		from member_point_txns where member_id=$1 order by id desc limit 50`, id)
	if err != nil {
		return d, err
	}
	for ptrows.Next() {
		var t PointTxn
		if err := ptrows.Scan(&t.ID, &t.MemberID, &t.Kind, &t.DeltaPoints, &t.PointsAfter,
			&t.PaymentID, &t.Actor, &t.Note, &t.CreatedAt); err != nil {
			ptrows.Close()
			return d, err
		}
		d.PointTxns = append(d.PointTxns, t)
	}
	ptrows.Close()
	if err := ptrows.Err(); err != nil {
		return d, err
	}

	urows, err := pool.Query(ctx, `
		select u.id, u.member_pass_id, mp.plan_name, u.member_id, m.name, m.phone,
		       u.order_id, u.payment_id, u.business_day, u.cups, u.saved_cents, u.created_at
		from member_pass_usages u
		join member_passes mp on mp.id = u.member_pass_id
		join members m on m.id = u.member_id
		where u.member_id=$1
		order by u.id desc limit 50`, id)
	if err != nil {
		return d, err
	}
	defer urows.Close()
	for urows.Next() {
		var u PassUsage
		if err := urows.Scan(&u.ID, &u.PassID, &u.PlanName, &u.MemberID, &u.MemberName, &u.MemberPhone,
			&u.OrderID, &u.PaymentID, &u.BusinessDay, &u.Cups, &u.SavedCents, &u.CreatedAt); err != nil {
			return d, err
		}
		d.PassUsages = append(d.PassUsages, u)
	}
	return d, urows.Err()
}

// SaveMember 新建或编辑会员档案；新建未指定等级时自动归入门槛最低的启用等级，未填卡号则自动生成 M000001。
func SaveMember(ctx context.Context, pool *pgxpool.Pool, m Member) (int64, error) {
	name, phone, no, bd, note, err := validateMemberFields(m.Name, m.Phone, m.MemberNo, m.Birthday, m.Note)
	if err != nil {
		return 0, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var tierID *int64
	if m.TierID != nil && *m.TierID > 0 {
		var exists bool
		if err := tx.QueryRow(ctx, `select exists(select 1 from member_tiers where id=$1)`, *m.TierID).Scan(&exists); err != nil {
			return 0, err
		}
		if !exists {
			return 0, platform.UserErr("所选会员等级不存在")
		}
		tid := *m.TierID
		tierID = &tid
	} else {
		var defaultTierID int64
		err := tx.QueryRow(ctx, `select id from member_tiers where active order by min_spent_cents asc, sort_order asc, id asc limit 1`).Scan(&defaultTierID)
		if err == nil {
			tierID = &defaultTierID
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
	}

	if m.ID == 0 {
		var id int64
		if err := tx.QueryRow(ctx, `
			insert into members (member_no, name, phone, tier_id, birthday, note, active)
			values ($1,$2,$3,$4,$5,$6,$7) returning id`,
			no, name, phone, tierID, bd, note, m.Active).Scan(&id); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				if strings.Contains(pgErr.ConstraintName, "no") {
					return 0, platform.UserErr("该会员卡号已被使用")
				}
				return 0, platform.UserErr("该手机号已注册会员")
			}
			return 0, err
		}
		if no == "" {
			autoNo := fmt.Sprintf("M%06d", id)
			if _, err := tx.Exec(ctx, `update members set member_no=$2 where id=$1`, id, autoNo); err != nil {
				return 0, err
			}
		}
		if err := platform.LogOperation(ctx, tx, platform.OpMemberCreate, platform.MemberSubject(name, phone), "新建会员档案"); err != nil {
			return 0, err
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, err
		}
		return id, nil
	}

	ct, err := tx.Exec(ctx, `
		update members
		set member_no=$2, name=$3, phone=$4, tier_id=$5, birthday=$6, note=$7, active=$8, updated_at=now()
		where id=$1`,
		m.ID, no, name, phone, tierID, bd, note, m.Active)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if strings.Contains(pgErr.ConstraintName, "no") {
				return 0, platform.UserErr("该会员卡号已被使用")
			}
			return 0, platform.UserErr("该手机号已被其他会员占用")
		}
		return 0, err
	}
	if ct.RowsAffected() == 0 {
		return 0, platform.UserErr("找不到这个会员")
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return m.ID, nil
}

func QuickCreateMember(ctx context.Context, pool *pgxpool.Pool, name, phone string) (int64, error) {
	return SaveMember(ctx, pool, Member{Name: name, Phone: phone, Active: true})
}

func ToggleMember(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	ct, err := pool.Exec(ctx, `update members set active = not active, updated_at = now() where id=$1`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return platform.UserErr("找不到这个会员")
	}
	return nil
}

type RechargeResult struct {
	MemberID          int64
	MemberName        string
	MemberPhone       string
	PayCents          int
	BonusCents        int
	BalanceAfterCents int
	PayMethod         string
	Replayed          bool
}

// RechargeMember 会员储值充值（本金 + 可选赠金），支持幂等键并同事务记营运流水。
func RechargeMember(ctx context.Context, pool *pgxpool.Pool, memberID int64, payCents, bonusCents int, payMethod, note, idemKey string) (RechargeResult, error) {
	if payCents <= 0 {
		return RechargeResult{}, platform.UserErr("充值实收金额必须大于 0")
	}
	if bonusCents < 0 {
		return RechargeResult{}, platform.UserErr("充值赠金不能为负")
	}
	method := normalizeRechargePayMethod(payMethod)
	if method == "balance" {
		return RechargeResult{}, platform.UserErr("储值充值不能用储值余额支付")
	}
	note = strings.TrimSpace(note)
	idemKey = strings.TrimSpace(idemKey)
	if len(idemKey) > 128 {
		idemKey = idemKey[:128]
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return RechargeResult{}, err
	}
	defer tx.Rollback(ctx)

	var name, phone string
	var balance, principal int
	var active bool
	err = tx.QueryRow(ctx, `select name, phone, balance_cents, principal_balance_cents, active
		from members where id=$1 for update`, memberID).Scan(&name, &phone, &balance, &principal, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return RechargeResult{}, platform.UserErr("找不到这个会员")
	}
	if err != nil {
		return RechargeResult{}, err
	}
	if !active {
		return RechargeResult{}, platform.UserErr("该会员已停用，无法充值")
	}

	if idemKey != "" {
		var prevBal int
		var prevDelta int
		err := tx.QueryRow(ctx, `select delta_cents, balance_after_cents from member_balance_txns
			where member_id=$1 and idempotency_key=$2`, memberID, idemKey).Scan(&prevDelta, &prevBal)
		if err == nil {
			if err := tx.Commit(ctx); err != nil {
				return RechargeResult{}, err
			}
			return RechargeResult{
				MemberID:          memberID,
				MemberName:        name,
				MemberPhone:       phone,
				PayCents:          prevDelta,
				BalanceAfterCents: balance,
				PayMethod:         method,
				Replayed:          true,
			}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return RechargeResult{}, err
		}
	}

	actor := platform.OperationActor(ctx)
	afterPay := balance + payCents
	afterBonus := afterPay + bonusCents
	newPrincipal := principal + payCents

	if _, err := tx.Exec(ctx, `update members
		set balance_cents=$2, principal_balance_cents=$3, updated_at=now()
		where id=$1`, memberID, afterBonus, newPrincipal); err != nil {
		return RechargeResult{}, err
	}

	var idemPtr *string
	if idemKey != "" {
		idemPtr = &idemKey
	}
	if _, err := tx.Exec(ctx, `insert into member_balance_txns
		(member_id, kind, delta_cents, principal_delta_cents, balance_after_cents, pay_method, actor, note, idempotency_key)
		values ($1, 'recharge', $2, $2, $3, $4, $5, $6, $7)`,
		memberID, payCents, afterPay, method, actor, note, idemPtr); err != nil {
		return RechargeResult{}, err
	}
	if bonusCents > 0 {
		if _, err := tx.Exec(ctx, `insert into member_balance_txns
			(member_id, kind, delta_cents, principal_delta_cents, balance_after_cents, pay_method, actor, note)
			values ($1, 'recharge_bonus', $2, 0, $3, $4, $5, $6)`,
			memberID, bonusCents, afterBonus, method, actor, note); err != nil {
			return RechargeResult{}, err
		}
	}

	detail := fmt.Sprintf("实充 %s", platform.PriceText(payCents))
	if bonusCents > 0 {
		detail += fmt.Sprintf(" · 赠金 %s", platform.PriceText(bonusCents))
	}
	detail += fmt.Sprintf(" · 充后余额 %s", platform.PriceText(afterBonus))
	if err := platform.LogOperation(ctx, tx, platform.OpMemberRecharge, platform.MemberSubject(name, phone), detail); err != nil {
		return RechargeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RechargeResult{}, err
	}
	return RechargeResult{
		MemberID:          memberID,
		MemberName:        name,
		MemberPhone:       phone,
		PayCents:          payCents,
		BonusCents:        bonusCents,
		BalanceAfterCents: afterBonus,
		PayMethod:         method,
	}, nil
}

// AdjustMemberBalance 人工调账储值余额（正增负减，禁止减成负数），写不可变台账与营运流水。
func AdjustMemberBalance(ctx context.Context, pool *pgxpool.Pool, memberID int64, deltaCents int, note string) error {
	if deltaCents == 0 {
		return platform.UserErr("调账金额不能为 0")
	}
	note = strings.TrimSpace(note)
	if note == "" {
		return platform.UserErr("人工调整余额必须填写原因备注")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var name, phone string
	var bal, principal int
	err = tx.QueryRow(ctx, `select name, phone, balance_cents, principal_balance_cents from members where id=$1 for update`, memberID).
		Scan(&name, &phone, &bal, &principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.UserErr("找不到这个会员")
	}
	if err != nil {
		return err
	}
	newBal := bal + deltaCents
	if newBal < 0 {
		return platform.UserErr("调整后余额不能为负")
	}
	newPrincipal := principal
	if deltaCents < 0 && newPrincipal > newBal {
		newPrincipal = newBal
	}
	principalDelta := newPrincipal - principal

	if _, err := tx.Exec(ctx, `update members set balance_cents=$2, principal_balance_cents=$3, updated_at=now() where id=$1`,
		memberID, newBal, newPrincipal); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `insert into member_balance_txns
		(member_id, kind, delta_cents, principal_delta_cents, balance_after_cents, actor, note)
		values ($1, 'adjust', $2, $3, $4, $5, $6)`,
		memberID, deltaCents, principalDelta, newBal, platform.OperationActor(ctx), note); err != nil {
		return err
	}
	detail := fmt.Sprintf("余额调整 %s · 调后 %s · %s", platform.MoneyText(deltaCents), platform.PriceText(newBal), note)
	if err := platform.LogOperation(ctx, tx, platform.OpMemberAdjust, platform.MemberSubject(name, phone), detail); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AdjustMemberPoints 人工调整积分（正增负减，禁止减成负数），写不可变台账与营运流水。
func AdjustMemberPoints(ctx context.Context, pool *pgxpool.Pool, memberID int64, deltaPoints int, note string) error {
	if deltaPoints == 0 {
		return platform.UserErr("调整积分不能为 0")
	}
	note = strings.TrimSpace(note)
	if note == "" {
		return platform.UserErr("人工调整积分必须填写原因备注")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var name, phone string
	var points int
	err = tx.QueryRow(ctx, `select name, phone, points from members where id=$1 for update`, memberID).
		Scan(&name, &phone, &points)
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.UserErr("找不到这个会员")
	}
	if err != nil {
		return err
	}
	newPoints := points + deltaPoints
	if newPoints < 0 {
		return platform.UserErr("调整后积分不能为负")
	}
	if _, err := tx.Exec(ctx, `update members set points=$2, updated_at=now() where id=$1`, memberID, newPoints); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `insert into member_point_txns
		(member_id, kind, delta_points, points_after, actor, note)
		values ($1, 'adjust', $2, $3, $4, $5)`,
		memberID, deltaPoints, newPoints, platform.OperationActor(ctx), note); err != nil {
		return err
	}
	detail := fmt.Sprintf("积分调整 %+d · 调后 %d · %s", deltaPoints, newPoints, note)
	if err := platform.LogOperation(ctx, tx, platform.OpMemberAdjust, platform.MemberSubject(name, phone), detail); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type IssuePassResult struct {
	Pass        PassInfo
	MemberName  string
	MemberPhone string
	Renewed     bool
	Replayed    bool
}

// IssuePass 为会员办理或续费月卡：
// - 支持扫码/现金/刷卡/其他，也支持直接从会员储值余额扣款（pay_method="balance"）；
// - 若会员已有同方案在效月卡，自动顺延 starts_at = 原 expires_at，确保天数不浪费；
// - 新开卡以酒吧营业日（07:00-次日07:00）为边界计算 expires_at，现场即买即用。
func IssuePass(ctx context.Context, pool *pgxpool.Pool, memberID, planID int64, payMethod, note, idemKey string) (IssuePassResult, error) {
	method := normalizeRechargePayMethod(payMethod)
	note = strings.TrimSpace(note)
	idemKey = strings.TrimSpace(idemKey)
	if len(idemKey) > 128 {
		idemKey = idemKey[:128]
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return IssuePassResult{}, err
	}
	defer tx.Rollback(ctx)

	var name, phone string
	var balance, principal int
	var active bool
	err = tx.QueryRow(ctx, `select name, phone, balance_cents, principal_balance_cents, active
		from members where id=$1 for update`, memberID).Scan(&name, &phone, &balance, &principal, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return IssuePassResult{}, platform.UserErr("找不到这个会员")
	}
	if err != nil {
		return IssuePassResult{}, err
	}
	if !active {
		return IssuePassResult{}, platform.UserErr("该会员已停用，无法办卡")
	}

	if idemKey != "" {
		var existing PassInfo
		err := tx.QueryRow(ctx, `select id, member_id, plan_id, plan_name, scope, benefit_type, daily_limit, total_limit,
			max_ml, max_unit_cents, cap_mode, discount_bps, price_cents, pay_method, starts_at, expires_at,
			used_total, status, actor, note, created_at
			from member_passes where member_id=$1 and idempotency_key=$2`, memberID, idemKey).
			Scan(&existing.ID, &existing.MemberID, &existing.PlanID, &existing.PlanName, &existing.Scope, &existing.BenefitType,
				&existing.DailyLimit, &existing.TotalLimit, &existing.MaxML, &existing.MaxUnitCents, &existing.CapMode, &existing.DiscountBPS,
				&existing.PriceCents, &existing.PayMethod, &existing.StartsAt, &existing.ExpiresAt,
				&existing.UsedTotal, &existing.Status, &existing.Actor, &existing.Note, &existing.CreatedAt)
		if err == nil {
			if err := tx.Commit(ctx); err != nil {
				return IssuePassResult{}, err
			}
			return IssuePassResult{Pass: existing, MemberName: name, MemberPhone: phone, Replayed: true}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return IssuePassResult{}, err
		}
	}

	plan, err := GetPassPlan(ctx, tx, planID)
	if errors.Is(err, pgx.ErrNoRows) || !plan.Active {
		return IssuePassResult{}, platform.UserErr("月卡方案不存在或已停用")
	}
	if err != nil {
		return IssuePassResult{}, err
	}

	if method == "balance" && plan.PriceCents > 0 {
		if balance < plan.PriceCents {
			return IssuePassResult{}, platform.UserErrf("储值余额不足购买「%s」（余额 %s，卡价 %s）",
				plan.Name, platform.PriceText(balance), platform.PriceText(plan.PriceCents))
		}
		newBal := balance - plan.PriceCents
		newPrincipal := principal
		if balance > 0 {
			newPrincipal = principal - int(int64(principal)*int64(plan.PriceCents)/int64(balance))
		}
		newPrincipal = max(0, min(newPrincipal, newBal))
		principalDelta := newPrincipal - principal
		if _, err := tx.Exec(ctx, `update members set balance_cents=$2, principal_balance_cents=$3, updated_at=now() where id=$1`,
			memberID, newBal, newPrincipal); err != nil {
			return IssuePassResult{}, err
		}
		if _, err := tx.Exec(ctx, `insert into member_balance_txns
			(member_id, kind, delta_cents, principal_delta_cents, balance_after_cents, pay_method, actor, note)
			values ($1, 'pass_buy', $2, $3, $4, 'balance', $5, $6)`,
			memberID, -plan.PriceCents, principalDelta, newBal, platform.OperationActor(ctx), "购买 "+plan.Name); err != nil {
			return IssuePassResult{}, err
		}
	}

	now := time.Now()
	var prevExpires time.Time
	renewed := false
	err = tx.QueryRow(ctx, `select expires_at from member_passes
		where member_id=$1 and plan_id=$2 and status='active' and expires_at > $3
		order by expires_at desc limit 1 for update`, memberID, plan.ID, now).Scan(&prevExpires)
	var startsAt, expiresAt time.Time
	if err == nil {
		renewed = true
		startsAt = prevExpires
		expiresAt = prevExpires.AddDate(0, 0, plan.DurationDays)
	} else if errors.Is(err, pgx.ErrNoRows) {
		startsAt = now
		expiresAt = platform.BusinessDayStart(now).AddDate(0, 0, plan.DurationDays)
		if !expiresAt.After(startsAt) {
			expiresAt = startsAt.AddDate(0, 0, plan.DurationDays)
		}
	} else {
		return IssuePassResult{}, err
	}

	var idemPtr *string
	if idemKey != "" {
		idemPtr = &idemKey
	}
	actor := platform.OperationActor(ctx)
	var pass PassInfo
	err = tx.QueryRow(ctx, `insert into member_passes
		(member_id, plan_id, plan_name, scope, benefit_type, daily_limit, total_limit,
		 max_ml, max_unit_cents, cap_mode, discount_bps, price_cents, pay_method,
		 starts_at, expires_at, status, actor, note, idempotency_key)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'active',$16,$17,$18)
		returning id, member_id, plan_id, plan_name, scope, benefit_type, daily_limit, total_limit,
		          max_ml, max_unit_cents, cap_mode, discount_bps, price_cents, pay_method,
		          starts_at, expires_at, used_total, status, actor, note, created_at`,
		memberID, plan.ID, plan.Name, plan.Scope, plan.BenefitType, plan.DailyLimit, plan.TotalLimit,
		plan.MaxML, plan.MaxUnitCents, plan.CapMode, plan.DiscountBPS, plan.PriceCents, method,
		startsAt, expiresAt, actor, note, idemPtr).
		Scan(&pass.ID, &pass.MemberID, &pass.PlanID, &pass.PlanName, &pass.Scope, &pass.BenefitType,
			&pass.DailyLimit, &pass.TotalLimit, &pass.MaxML, &pass.MaxUnitCents, &pass.CapMode, &pass.DiscountBPS,
			&pass.PriceCents, &pass.PayMethod, &pass.StartsAt, &pass.ExpiresAt,
			&pass.UsedTotal, &pass.Status, &pass.Actor, &pass.Note, &pass.CreatedAt)
	if err != nil {
		return IssuePassResult{}, err
	}

	verb := "办卡"
	if renewed {
		verb = "续费"
	}
	detail := fmt.Sprintf("%s「%s」· 实收 %s (%s) · 有效期至 %s", verb, plan.Name, platform.PriceText(plan.PriceCents), method, pass.ExpiresText())
	if err := platform.LogOperation(ctx, tx, platform.OpPassIssue, platform.MemberSubject(name, phone), detail); err != nil {
		return IssuePassResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return IssuePassResult{}, err
	}
	return IssuePassResult{
		Pass:        pass,
		MemberName:  name,
		MemberPhone: phone,
		Renewed:     renewed,
	}, nil
}

func RevokePass(ctx context.Context, pool *pgxpool.Pool, memberID, passID int64, note string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var name, phone string
	err = tx.QueryRow(ctx, `select name, phone from members where id=$1 for update`, memberID).Scan(&name, &phone)
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.UserErr("找不到这个会员")
	}
	if err != nil {
		return err
	}

	var planName, status string
	err = tx.QueryRow(ctx, `select plan_name, status from member_passes where id=$1 and member_id=$2 for update`, passID, memberID).
		Scan(&planName, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.UserErr("找不到这张月卡")
	}
	if err != nil {
		return err
	}
	if status == "revoked" {
		return platform.UserErr("这张月卡已经作废")
	}

	if _, err := tx.Exec(ctx, `update member_passes set status='revoked', note=case when $2<>'' then $2 else note end where id=$1`,
		passID, strings.TrimSpace(note)); err != nil {
		return err
	}
	detail := fmt.Sprintf("作废「%s」#%d", planName, passID)
	if strings.TrimSpace(note) != "" {
		detail += " · " + strings.TrimSpace(note)
	}
	if err := platform.LogOperation(ctx, tx, platform.OpPassRevoke, platform.MemberSubject(name, phone), detail); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type MemberPassSummary struct {
	Pass        PassInfo
	MemberName  string
	MemberPhone string
}

func ListPassesOverview(ctx context.Context, pool *pgxpool.Pool, statusFilter string) ([]MemberPassSummary, error) {
	now := time.Now()
	bizDay := platform.BusinessDayKey(now)
	rows, err := pool.Query(ctx, `
		select mp.id, mp.member_id, mp.plan_id, mp.plan_name, mp.scope, mp.benefit_type, mp.daily_limit, mp.total_limit,
		       mp.max_ml, mp.max_unit_cents, mp.cap_mode, mp.discount_bps, mp.price_cents, mp.pay_method,
		       mp.starts_at, mp.expires_at, mp.used_total, mp.status, mp.actor, mp.note, mp.created_at,
		       coalesce((select sum(u.cups)::int from member_pass_usages u where u.member_pass_id = mp.id and u.business_day = $2), 0),
		       m.name, m.phone
		from member_passes mp
		join members m on m.id = mp.member_id
		where ($1 = ''
		       or ($1 = 'active' and mp.status = 'active' and mp.expires_at > $3)
		       or ($1 = 'expired' and (mp.status = 'revoked' or mp.expires_at <= $3)))
		order by (mp.status = 'active' and mp.expires_at > $3) desc, mp.expires_at asc, mp.id desc
		limit 200`, statusFilter, bizDay, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemberPassSummary
	for rows.Next() {
		var row MemberPassSummary
		if err := rows.Scan(&row.Pass.ID, &row.Pass.MemberID, &row.Pass.PlanID, &row.Pass.PlanName, &row.Pass.Scope, &row.Pass.BenefitType,
			&row.Pass.DailyLimit, &row.Pass.TotalLimit, &row.Pass.MaxML, &row.Pass.MaxUnitCents, &row.Pass.CapMode, &row.Pass.DiscountBPS,
			&row.Pass.PriceCents, &row.Pass.PayMethod, &row.Pass.StartsAt, &row.Pass.ExpiresAt,
			&row.Pass.UsedTotal, &row.Pass.Status, &row.Pass.Actor, &row.Pass.Note, &row.Pass.CreatedAt,
			&row.Pass.UsedToday, &row.MemberName, &row.MemberPhone); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func ListRecentPassUsages(ctx context.Context, pool *pgxpool.Pool, limit int) ([]PassUsage, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	rows, err := pool.Query(ctx, `
		select u.id, u.member_pass_id, mp.plan_name, u.member_id, m.name, m.phone,
		       u.order_id, u.payment_id, u.business_day, u.cups, u.saved_cents, u.created_at
		from member_pass_usages u
		join member_passes mp on mp.id = u.member_pass_id
		join members m on m.id = u.member_id
		order by u.id desc limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PassUsage
	for rows.Next() {
		var u PassUsage
		if err := rows.Scan(&u.ID, &u.PassID, &u.PlanName, &u.MemberID, &u.MemberName, &u.MemberPhone,
			&u.OrderID, &u.PaymentID, &u.BusinessDay, &u.Cups, &u.SavedCents, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CheckoutApplyRequest 描述 POS 结账事务写入会员扣款、积分、月卡核销与折扣审计的完整载荷。
type CheckoutApplyRequest struct {
	OrderID   int64
	PaymentID int64
	SpotLabel string
	Member    *MemberSnapshot
	Pricing   PricingResult
	Now       time.Time
}

// ApplyCheckoutTx 在 POS 结账事务内完成会员余额扣减、月卡核销记次、积分变更、自动升级与优惠审计落账。
// 调用方必须已按全局锁序先锁住 members 和 member_passes。
func ApplyCheckoutTx(ctx context.Context, tx pgx.Tx, req CheckoutApplyRequest) error {
	actor := platform.OperationActor(ctx)
	p := req.Pricing
	if p.ManualDiscountCents > 0 || (p.DiscountRuleID != nil && p.DiscountLabel != "") {
		subj := req.SpotLabel
		if req.Member != nil {
			subj += " · " + req.Member.Name
		}
		detail := p.DiscountLabel
		if detail == "" {
			detail = "人工折扣"
		}
		if p.ManualDiscountCents > 0 {
			detail += fmt.Sprintf("（优惠 %s）", platform.PriceText(p.ManualDiscountCents))
		}
		if p.DiscountNote != "" {
			detail += " · " + p.DiscountNote
		}
		if err := platform.LogOperation(ctx, tx, platform.OpDiscount, subj, detail); err != nil {
			return err
		}
	}

	if req.Member == nil {
		return nil
	}
	m := req.Member

	// 1. 复核并写入月卡核销
	if p.PassCupsUsed > 0 && p.MemberPassID != nil {
		if m.ActivePass == nil || m.ActivePass.ID != *p.MemberPassID {
			return platform.UserErr("月卡状态已变更，请刷新后重试")
		}
		pass := m.ActivePass
		if pass.Status != "active" || !pass.IsActiveAt(req.Now) {
			return platform.UserErr("月卡已失效或尚未生效")
		}
		if pass.TotalLimit > 0 && pass.UsedTotal+p.PassCupsUsed > pass.TotalLimit {
			return platform.UserErr("月卡总核销次数不足")
		}
		if pass.DailyLimit > 0 && pass.UsedToday+p.PassCupsUsed > pass.DailyLimit {
			return platform.UserErr("月卡今日可用免单杯数已用完")
		}
		if _, err := tx.Exec(ctx, `update member_passes set used_total = used_total + $2 where id=$1`,
			pass.ID, p.PassCupsUsed); err != nil {
			return err
		}
		bizDay := platform.BusinessDayKey(req.Now)
		if _, err := tx.Exec(ctx, `insert into member_pass_usages
			(member_pass_id, member_id, order_id, payment_id, business_day, cups, saved_cents)
			values ($1,$2,$3,$4,$5,$6,$7)`,
			pass.ID, m.ID, req.OrderID, req.PaymentID, bizDay, p.PassCupsUsed, p.PassDiscountCents); err != nil {
			return err
		}
	}

	// 2. 扣减储值余额并按比例扣减本金
	newBal := m.BalanceCents
	newPrincipal := m.PrincipalBalanceCents
	if p.BalancePaidCents > 0 {
		if m.BalanceCents < p.BalancePaidCents {
			return platform.UserErr("会员储值余额不足，请刷新核对")
		}
		newBal = m.BalanceCents - p.BalancePaidCents
		if m.BalanceCents > 0 {
			newPrincipal = m.PrincipalBalanceCents - int(int64(m.PrincipalBalanceCents)*int64(p.BalancePaidCents)/int64(m.BalanceCents))
		}
		newPrincipal = max(0, min(newPrincipal, newBal))
		principalDelta := newPrincipal - m.PrincipalBalanceCents
		if _, err := tx.Exec(ctx, `insert into member_balance_txns
			(member_id, kind, delta_cents, principal_delta_cents, balance_after_cents, pay_method, payment_id, actor, note)
			values ($1, 'consume', $2, $3, $4, 'balance', $5, $6, $7)`,
			m.ID, -p.BalancePaidCents, principalDelta, newBal, req.PaymentID, actor, req.SpotLabel); err != nil {
			return err
		}
	}

	// 3. 积分抵扣与消费攒分
	curPoints := m.Points
	if p.PointsSpent > 0 {
		if curPoints < p.PointsSpent {
			return platform.UserErr("会员积分不足，请刷新核对")
		}
		curPoints -= p.PointsSpent
		if _, err := tx.Exec(ctx, `insert into member_point_txns
			(member_id, kind, delta_points, points_after, payment_id, actor, note)
			values ($1, 'redeem', $2, $3, $4, $5, $6)`,
			m.ID, -p.PointsSpent, curPoints, req.PaymentID, actor, req.SpotLabel+" 抵现"); err != nil {
			return err
		}
	}
	if p.PointsEarned > 0 {
		curPoints += p.PointsEarned
		if _, err := tx.Exec(ctx, `insert into member_point_txns
			(member_id, kind, delta_points, points_after, payment_id, actor, note)
			values ($1, 'earn', $2, $3, $4, $5, $6)`,
			m.ID, p.PointsEarned, curPoints, req.PaymentID, actor, req.SpotLabel+" 消费"); err != nil {
			return err
		}
	}

	// 4. 更新会员累计消费、订单数与自动升档（绝不降级手工指定的更高 VIP）
	newLifetime := m.LifetimeSpentCents + p.LifetimeSpentDelta
	nextTierID := m.TierID
	var curMinSpent int
	if m.TierID != nil {
		if err := tx.QueryRow(ctx, `select coalesce(min_spent_cents, 0) from member_tiers where id=$1`, *m.TierID).Scan(&curMinSpent); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	var bestTierID int64
	var bestMinSpent int
	err := tx.QueryRow(ctx, `select id, min_spent_cents from member_tiers
		where active and min_spent_cents <= $1
		order by min_spent_cents desc, discount_bps asc, id asc limit 1`, newLifetime).Scan(&bestTierID, &bestMinSpent)
	if err == nil {
		if m.TierID == nil || bestMinSpent > curMinSpent {
			nextTierID = &bestTierID
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	_, err = tx.Exec(ctx, `update members
		set balance_cents=$2, principal_balance_cents=$3, points=$4,
		    lifetime_spent_cents=$5, lifetime_orders=lifetime_orders+1,
		    tier_id=$6, updated_at=now()
		where id=$1`,
		m.ID, newBal, newPrincipal, curPoints, newLifetime, nextTierID)
	return err
}
