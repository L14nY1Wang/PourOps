package loyalty

import (
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/event"
	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/internal/ui"
)

func DiscountBPSInput(bps int) string {
	if bps <= 0 {
		return "0"
	}
	return strconv.FormatFloat(float64(bps)/1000, 'f', -1, 64)
}

func ParseDiscountFoldBPS(raw, label string, minBPS, maxBPS int) (int, error) {
	v, err := platform.ParseStrictFloat(raw, label, float64(minBPS)/1000, float64(maxBPS)/1000, false)
	if err != nil {
		return 0, err
	}
	if v == nil {
		return maxBPS, nil
	}
	bps := int(math.Round(*v * 1000))
	if bps < minBPS || bps > maxBPS {
		return 0, platform.UserErrf("%s超出范围", label)
	}
	return bps, nil
}

func (t Tier) DiscountInput() string { return DiscountBPSInput(t.DiscountBPS) }
func (t Tier) PointsRateInput() string {
	return strconv.FormatFloat(float64(t.PointsRateBPS)/10000, 'f', -1, 64)
}
func (r DiscountRule) DiscountInput() string { return DiscountBPSInput(r.DiscountBPS) }
func (r DiscountRule) FixedInput() string    { return platform.MoneyInput(r.FixedCents) }
func (r DiscountRule) MinInput() string      { return platform.MoneyInput(r.MinAmountCents) }
func (r DiscountRule) RoundInput() string    { return platform.MoneyInput(r.RoundUnitCents) }
func (p PassPlan) DiscountInput() string     { return DiscountBPSInput(p.DiscountBPS) }
func (p PassPlan) MaxUnitInput() string      { return platform.MoneyInput(p.MaxUnitCents) }

// ---------- 后台页面 ViewModels ----------

type MembersPage struct {
	ui.Layout
	Q          string
	TierID     int64
	PassFilter string
	Members    []Member
	Tiers      []Tier
	Summary    MemberSummary
}

type MemberFormPage struct {
	ui.Layout
	Member Member
	Tiers  []Tier
}

type MemberDetailPage struct {
	ui.Layout
	Detail          MemberDetail
	Tiers           []Tier
	Plans           []PassPlan
	RechargePresets []settings.RechargePreset
	Token           string
}

type PassesPage struct {
	ui.Layout
	Status string
	Plans  []PassPlan
	Passes []MemberPassSummary
	Usages []PassUsage
}

type DiscountsPage struct {
	ui.Layout
	Rules []DiscountRule
	Tiers []Tier
	Cfg   settings.LoyaltyConfig
}

// Service 提供会员档案、储值积分、月卡与折扣规则的运营后台管理能力。
type Service struct {
	pool     *pgxpool.Pool
	renderer *ui.Renderer
	settings *settings.SettingsStore
	bus      *event.EventBus
}

func NewService(pool *pgxpool.Pool, renderer *ui.Renderer, cfg *settings.SettingsStore, bus *event.EventBus) *Service {
	return &Service{
		pool:     pool,
		renderer: renderer,
		settings: cfg,
		bus:      bus,
	}
}

func (s *Service) RegisterRoutes(mux *http.ServeMux, guards ui.Guards) {
	mux.HandleFunc("GET /admin/members", guards.Auth(s.Members))
	mux.HandleFunc("GET /admin/members/new", guards.Auth(s.MemberNew))
	mux.HandleFunc("POST /admin/members", guards.Auth(s.MemberSave))
	mux.HandleFunc("GET /admin/members/{id}", guards.Auth(s.MemberDetail))
	mux.HandleFunc("GET /admin/members/{id}/edit", guards.Auth(s.MemberEdit))
	mux.HandleFunc("POST /admin/members/{id}/toggle", guards.Auth(s.MemberToggle))
	mux.HandleFunc("POST /admin/members/{id}/recharge", guards.Auth(s.MemberRecharge))
	mux.HandleFunc("POST /admin/members/{id}/passes", guards.Auth(s.MemberIssuePass))
	mux.HandleFunc("POST /admin/members/{id}/passes/{passID}/revoke", guards.Auth(s.MemberRevokePass))
	mux.HandleFunc("POST /admin/members/{id}/adjust-balance", guards.Auth(s.MemberAdjustBalance))
	mux.HandleFunc("POST /admin/members/{id}/adjust-points", guards.Auth(s.MemberAdjustPoints))

	mux.HandleFunc("GET /admin/passes", guards.Auth(s.Passes))
	mux.HandleFunc("POST /admin/passes/plans", guards.Auth(s.PassPlanSave))
	mux.HandleFunc("POST /admin/passes/plans/{id}/toggle", guards.Auth(s.PassPlanToggle))

	mux.HandleFunc("GET /admin/discounts", guards.Auth(s.Discounts))
	mux.HandleFunc("POST /admin/discounts/rules", guards.Auth(s.DiscountRuleSave))
	mux.HandleFunc("POST /admin/discounts/rules/{id}/toggle", guards.Auth(s.DiscountRuleToggle))
	mux.HandleFunc("POST /admin/discounts/rules/{id}/delete", guards.Auth(s.DiscountRuleDelete))
	mux.HandleFunc("POST /admin/discounts/tiers", guards.Auth(s.TierSave))
	mux.HandleFunc("POST /admin/discounts/tiers/{id}/toggle", guards.Auth(s.TierToggle))
	mux.HandleFunc("POST /admin/discounts/config", guards.Auth(s.LoyaltyConfigSave))
}

func (s *Service) loyaltyConfig() settings.LoyaltyConfig {
	if s.settings != nil {
		return s.settings.Loyalty()
	}
	return settings.DefaultLoyaltyConfig()
}

func redirectUserErr(w http.ResponseWriter, r *http.Request, target, fallback string, err error) {
	if msg, ok := platform.UserErrorMessage(err); ok {
		platform.RedirectErr(w, r, target, msg)
		return
	}
	log.Printf("%s: %v", target, err)
	platform.RedirectErr(w, r, target, fallback)
}

// ---------- 会员后台 ----------

func (s *Service) Members(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	tierID, _ := strconv.ParseInt(r.URL.Query().Get("tier"), 10, 64)
	passFilter := r.URL.Query().Get("pass")
	if passFilter != "has_pass" && passFilter != "no_pass" {
		passFilter = ""
	}

	tiers, err := ListTiers(ctx, s.pool, false)
	if err != nil {
		log.Printf("list tiers: %v", err)
		http.Error(w, "加载会员等级失败", http.StatusInternalServerError)
		return
	}
	members, err := ListMembers(ctx, s.pool, q, tierID, passFilter)
	if err != nil {
		log.Printf("list members: %v", err)
		http.Error(w, "加载会员列表失败", http.StatusInternalServerError)
		return
	}
	summary, err := MemberSummaryStats(ctx, s.pool)
	if err != nil {
		log.Printf("member summary: %v", err)
		http.Error(w, "加载会员统计失败", http.StatusInternalServerError)
		return
	}

	s.renderer.Render(w, r, "admin/members.html", &MembersPage{
		Layout:     ui.Layout{Title: "会员管理", Nav: "members"},
		Q:          q,
		TierID:     tierID,
		PassFilter: passFilter,
		Members:    members,
		Tiers:      tiers,
		Summary:    summary,
	})
}

func (s *Service) MemberNew(w http.ResponseWriter, r *http.Request) {
	tiers, err := ListTiers(r.Context(), s.pool, true)
	if err != nil {
		log.Printf("member new tiers: %v", err)
		http.Error(w, "加载会员等级失败", http.StatusInternalServerError)
		return
	}
	s.renderer.Render(w, r, "admin/member_form.html", &MemberFormPage{
		Layout: ui.Layout{Title: "新增会员", Nav: "members"},
		Member: Member{Active: true},
		Tiers:  tiers,
	})
}

func (s *Service) MemberEdit(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	d, err := GetMemberDetail(r.Context(), s.pool, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "找不到这个会员", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("member edit (%d): %v", id, err)
		http.Error(w, "加载失败", http.StatusInternalServerError)
		return
	}
	tiers, err := ListTiers(r.Context(), s.pool, false)
	if err != nil {
		http.Error(w, "加载等级失败", http.StatusInternalServerError)
		return
	}
	s.renderer.Render(w, r, "admin/member_form.html", &MemberFormPage{
		Layout: ui.Layout{Title: "编辑会员 · " + d.Member.Name, Nav: "members"},
		Member: d.Member,
		Tiers:  tiers,
	})
}

func (s *Service) MemberSave(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	var tierPtr *int64
	if tid, err := strconv.ParseInt(r.FormValue("tier_id"), 10, 64); err == nil && tid > 0 {
		tierPtr = &tid
	}
	active := r.FormValue("active") == "1"
	if id == 0 && r.FormValue("active") == "" {
		active = true
	}
	m := Member{
		ID:       id,
		MemberNo: r.FormValue("member_no"),
		Name:     r.FormValue("name"),
		Phone:    r.FormValue("phone"),
		TierID:   tierPtr,
		Birthday: r.FormValue("birthday"),
		Note:     r.FormValue("note"),
		Active:   active,
	}
	savedID, err := SaveMember(r.Context(), s.pool, m)
	if err != nil {
		back := "/admin/members/new"
		if id > 0 {
			back = fmt.Sprintf("/admin/members/%d/edit", id)
		}
		redirectUserErr(w, r, back, "保存会员失败，请稍后重试", err)
		return
	}
	platform.RedirectMsg(w, r, fmt.Sprintf("/admin/members/%d", savedID), "会员档案已保存")
}

func (s *Service) MemberDetail(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	d, err := GetMemberDetail(r.Context(), s.pool, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "找不到这个会员", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("member detail (%d): %v", id, err)
		http.Error(w, "加载会员详情失败", http.StatusInternalServerError)
		return
	}
	tiers, err := ListTiers(r.Context(), s.pool, true)
	if err != nil {
		http.Error(w, "加载等级失败", http.StatusInternalServerError)
		return
	}
	plans, err := ListPassPlans(r.Context(), s.pool, true)
	if err != nil {
		http.Error(w, "加载月卡方案失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderer.Render(w, r, "admin/member_detail.html", &MemberDetailPage{
		Layout:          ui.Layout{Title: d.Member.Name + " · 会员详情", Nav: "members"},
		Detail:          d,
		Tiers:           tiers,
		Plans:           plans,
		RechargePresets: s.loyaltyConfig().RechargePresets,
		Token:           platform.RandomHex(16),
	})
}

func (s *Service) MemberToggle(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	back := fmt.Sprintf("/admin/members/%d", id)
	if err := ToggleMember(r.Context(), s.pool, id); err != nil {
		redirectUserErr(w, r, back, "切换状态失败", err)
		return
	}
	platform.RedirectMsg(w, r, back, "会员状态已更新")
}

func (s *Service) MemberRecharge(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	back := fmt.Sprintf("/admin/members/%d", id)

	payCents, err := platform.MoneyCents(r.FormValue("pay_amount"))
	if err != nil {
		platform.RedirectErr(w, r, back, "充值金额格式不正确："+err.Error())
		return
	}
	bonusCents, err := platform.MoneyCents(r.FormValue("bonus_amount"))
	if err != nil {
		platform.RedirectErr(w, r, back, "赠金金额格式不正确："+err.Error())
		return
	}
	res, err := RechargeMember(r.Context(), s.pool, id, payCents, bonusCents,
		r.FormValue("pay_method"), r.FormValue("note"), r.FormValue("idempotency_key"))
	if err != nil {
		redirectUserErr(w, r, back, "储值充值失败，请稍后重试", err)
		return
	}
	if !res.Replayed && s.bus != nil {
		s.bus.Publish(r.Context(), event.EventMemberRecharged, platform.MemberSubject(res.MemberName, res.MemberPhone), map[string]any{
			"member_id":           res.MemberID,
			"pay_cents":           res.PayCents,
			"bonus_cents":         res.BonusCents,
			"balance_after_cents": res.BalanceAfterCents,
			"pay_method":          res.PayMethod,
		})
	}
	platform.RedirectMsg(w, r, back, fmt.Sprintf("充值成功，当前储值余额 %s", platform.PriceText(res.BalanceAfterCents)))
}

func (s *Service) MemberIssuePass(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	back := fmt.Sprintf("/admin/members/%d", id)
	planID, _ := strconv.ParseInt(r.FormValue("plan_id"), 10, 64)
	if planID <= 0 {
		platform.RedirectErr(w, r, back, "请选择月卡方案")
		return
	}
	res, err := IssuePass(r.Context(), s.pool, id, planID,
		r.FormValue("pay_method"), r.FormValue("note"), r.FormValue("idempotency_key"))
	if err != nil {
		redirectUserErr(w, r, back, "办理月卡失败，请稍后重试", err)
		return
	}
	if !res.Replayed && s.bus != nil {
		s.bus.Publish(r.Context(), event.EventPassPurchased, platform.MemberSubject(res.MemberName, res.MemberPhone), map[string]any{
			"member_id":   res.Pass.MemberID,
			"pass_id":     res.Pass.ID,
			"plan_name":   res.Pass.PlanName,
			"price_cents": res.Pass.PriceCents,
			"pay_method":  res.Pass.PayMethod,
			"renewed":     res.Renewed,
			"expires_at":  res.Pass.ExpiresAt,
		})
	}
	msg := fmt.Sprintf("已办理「%s」，有效期至 %s", res.Pass.PlanName, res.Pass.ExpiresText())
	if res.Renewed {
		msg = fmt.Sprintf("已顺延续费「%s」，有效期至 %s", res.Pass.PlanName, res.Pass.ExpiresText())
	}
	platform.RedirectMsg(w, r, back, msg)
}

func (s *Service) MemberRevokePass(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	passID, _ := strconv.ParseInt(r.PathValue("passID"), 10, 64)
	back := fmt.Sprintf("/admin/members/%d", id)
	if err := RevokePass(r.Context(), s.pool, id, passID, r.FormValue("note")); err != nil {
		redirectUserErr(w, r, back, "作废月卡失败", err)
		return
	}
	platform.RedirectMsg(w, r, back, "月卡已作废")
}

func (s *Service) MemberAdjustBalance(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	back := fmt.Sprintf("/admin/members/%d", id)
	raw := strings.TrimSpace(r.FormValue("delta_amount"))
	sign := 1
	if strings.HasPrefix(raw, "-") {
		sign = -1
		raw = strings.TrimSpace(strings.TrimPrefix(raw, "-"))
	} else {
		raw = strings.TrimSpace(strings.TrimPrefix(raw, "+"))
	}
	cents, err := platform.MoneyCents(raw)
	if err != nil || cents <= 0 {
		platform.RedirectErr(w, r, back, "请输入有效的调整金额（非 0 元数）")
		return
	}
	if err := AdjustMemberBalance(r.Context(), s.pool, id, sign*cents, r.FormValue("note")); err != nil {
		redirectUserErr(w, r, back, "调整余额失败", err)
		return
	}
	platform.RedirectMsg(w, r, back, "储值余额已调整")
}

func (s *Service) MemberAdjustPoints(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	back := fmt.Sprintf("/admin/members/%d", id)
	deltaPtr, err := platform.ParseStrictInt(r.FormValue("delta_points"), "积分变动", -1000000, 1000000)
	if err != nil || deltaPtr == nil || *deltaPtr == 0 {
		platform.RedirectErr(w, r, back, "请输入非 0 整数积分变动")
		return
	}
	if err := AdjustMemberPoints(r.Context(), s.pool, id, *deltaPtr, r.FormValue("note")); err != nil {
		redirectUserErr(w, r, back, "调整积分失败", err)
		return
	}
	platform.RedirectMsg(w, r, back, "会员积分已调整")
}

// ---------- 月卡方案后台 ----------

func (s *Service) Passes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	status := r.URL.Query().Get("status")
	if status != "active" && status != "expired" {
		status = ""
	}
	plans, err := ListPassPlans(ctx, s.pool, false)
	if err != nil {
		http.Error(w, "加载月卡方案失败", http.StatusInternalServerError)
		return
	}
	passes, err := ListPassesOverview(ctx, s.pool, status)
	if err != nil {
		http.Error(w, "加载持卡名单失败", http.StatusInternalServerError)
		return
	}
	usages, err := ListRecentPassUsages(ctx, s.pool, 30)
	if err != nil {
		http.Error(w, "加载核销记录失败", http.StatusInternalServerError)
		return
	}
	s.renderer.Render(w, r, "admin/passes.html", &PassesPage{
		Layout: ui.Layout{Title: "月卡管理", Nav: "passes"},
		Status: status,
		Plans:  plans,
		Passes: passes,
		Usages: usages,
	})
}

func (s *Service) PassPlanSave(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	priceCents, err := platform.MoneyCents(r.FormValue("price"))
	if err != nil {
		platform.RedirectErr(w, r, "/admin/passes", "月卡售价格式不正确："+err.Error())
		return
	}
	daysPtr, err := platform.ParseStrictInt(r.FormValue("duration_days"), "有效天数", 1, 3650)
	if err != nil {
		platform.RedirectErr(w, r, "/admin/passes", err.Error())
		return
	}
	days := 31
	if daysPtr != nil {
		days = *daysPtr
	}
	dailyPtr, err := platform.ParseStrictInt(r.FormValue("daily_limit"), "每日限杯", 0, 99)
	if err != nil {
		platform.RedirectErr(w, r, "/admin/passes", err.Error())
		return
	}
	daily := 1
	if dailyPtr != nil {
		daily = *dailyPtr
	}
	totalPtr, err := platform.ParseStrictInt(r.FormValue("total_limit"), "总限杯", 0, 9999)
	if err != nil {
		platform.RedirectErr(w, r, "/admin/passes", err.Error())
		return
	}
	total := 0
	if totalPtr != nil {
		total = *totalPtr
	}
	maxMLPtr, err := platform.ParseStrictInt(r.FormValue("max_ml"), "杯型上限", 0, 5000)
	if err != nil {
		platform.RedirectErr(w, r, "/admin/passes", err.Error())
		return
	}
	maxML := 0
	if maxMLPtr != nil {
		maxML = *maxMLPtr
	}
	maxUnit, err := platform.MoneyCents(r.FormValue("max_unit"))
	if err != nil {
		platform.RedirectErr(w, r, "/admin/passes", "单杯封顶金额格式不正确")
		return
	}
	bps, err := ParseDiscountFoldBPS(r.FormValue("discount_fold"), "包月折率", 100, 10000)
	if err != nil {
		platform.RedirectErr(w, r, "/admin/passes", err.Error())
		return
	}
	sortOrder, err := platform.ParseSortOrder(r.FormValue("sort_order"))
	if err != nil {
		platform.RedirectErr(w, r, "/admin/passes", err.Error())
		return
	}
	active := r.FormValue("active") == "1"
	if id == 0 && r.FormValue("active") == "" {
		active = true
	}

	_, err = SavePassPlan(r.Context(), s.pool, PassPlan{
		ID:           id,
		Name:         r.FormValue("name"),
		PriceCents:   priceCents,
		DurationDays: days,
		Scope:        r.FormValue("scope"),
		BenefitType:  r.FormValue("benefit_type"),
		DailyLimit:   daily,
		TotalLimit:   total,
		MaxML:        maxML,
		MaxUnitCents: maxUnit,
		CapMode:      r.FormValue("cap_mode"),
		DiscountBPS:  bps,
		Active:       active,
		SortOrder:    sortOrder,
		Note:         r.FormValue("note"),
	})
	if err != nil {
		redirectUserErr(w, r, "/admin/passes", "保存月卡方案失败", err)
		return
	}
	platform.RedirectMsg(w, r, "/admin/passes", "月卡方案已保存")
}

func (s *Service) PassPlanToggle(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := TogglePassPlan(r.Context(), s.pool, id); err != nil {
		redirectUserErr(w, r, "/admin/passes", "切换方案状态失败", err)
		return
	}
	platform.RedirectMsg(w, r, "/admin/passes", "月卡方案状态已更新")
}

// ---------- 优惠、等级与积分规则中心 ----------

func (s *Service) Discounts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rules, err := ListDiscountRules(ctx, s.pool, false)
	if err != nil {
		http.Error(w, "加载折扣规则失败", http.StatusInternalServerError)
		return
	}
	tiers, err := ListTiers(ctx, s.pool, false)
	if err != nil {
		http.Error(w, "加载会员等级失败", http.StatusInternalServerError)
		return
	}
	s.renderer.Render(w, r, "admin/discounts.html", &DiscountsPage{
		Layout: ui.Layout{Title: "优惠与会员规则", Nav: "discounts"},
		Rules:  rules,
		Tiers:  tiers,
		Cfg:    s.loyaltyConfig(),
	})
}

func (s *Service) DiscountRuleSave(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	bps, err := ParseDiscountFoldBPS(r.FormValue("discount_fold"), "折扣折率", 100, 10000)
	if err != nil {
		platform.RedirectErr(w, r, "/admin/discounts", err.Error())
		return
	}
	fixedCents, err := platform.MoneyCents(r.FormValue("fixed_amount"))
	if err != nil {
		platform.RedirectErr(w, r, "/admin/discounts", "立减金额格式不正确")
		return
	}
	minAmount, err := platform.MoneyCents(r.FormValue("min_amount"))
	if err != nil {
		platform.RedirectErr(w, r, "/admin/discounts", "最低消费门槛格式不正确")
		return
	}
	roundUnit, err := platform.MoneyCents(r.FormValue("round_unit"))
	if err != nil {
		platform.RedirectErr(w, r, "/admin/discounts", "抹零单位格式不正确")
		return
	}
	if roundUnit <= 0 {
		roundUnit = 100
	}
	sortOrder, err := platform.ParseSortOrder(r.FormValue("sort_order"))
	if err != nil {
		platform.RedirectErr(w, r, "/admin/discounts", err.Error())
		return
	}
	active := r.FormValue("active") == "1"
	if id == 0 && r.FormValue("active") == "" {
		active = true
	}

	_, err = SaveDiscountRule(r.Context(), s.pool, DiscountRule{
		ID:              id,
		Name:            r.FormValue("name"),
		Kind:            r.FormValue("kind"),
		DiscountBPS:     bps,
		FixedCents:      fixedCents,
		RoundUnitCents:  roundUnit,
		Scope:           r.FormValue("scope"),
		MinAmountCents:  minAmount,
		AllowStaff:      r.FormValue("allow_staff") == "1",
		StackWithMember: r.FormValue("stack_with_member") == "1",
		Active:          active,
		SortOrder:       sortOrder,
	})
	if err != nil {
		redirectUserErr(w, r, "/admin/discounts", "保存折扣规则失败", err)
		return
	}
	platform.RedirectMsg(w, r, "/admin/discounts", "折扣规则已保存")
}

func (s *Service) DiscountRuleToggle(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := ToggleDiscountRule(r.Context(), s.pool, id); err != nil {
		redirectUserErr(w, r, "/admin/discounts", "更新折扣状态失败", err)
		return
	}
	platform.RedirectMsg(w, r, "/admin/discounts", "折扣状态已更新")
}

func (s *Service) DiscountRuleDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := DeleteDiscountRule(r.Context(), s.pool, id); err != nil {
		redirectUserErr(w, r, "/admin/discounts", "删除折扣规则失败", err)
		return
	}
	platform.RedirectMsg(w, r, "/admin/discounts", "折扣规则已删除")
}

func (s *Service) TierSave(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	minSpent, err := platform.MoneyCents(r.FormValue("min_spent"))
	if err != nil {
		platform.RedirectErr(w, r, "/admin/discounts", "升级门槛金额格式不正确")
		return
	}
	bps, err := ParseDiscountFoldBPS(r.FormValue("discount_fold"), "会员折率", 100, 10000)
	if err != nil {
		platform.RedirectErr(w, r, "/admin/discounts", err.Error())
		return
	}
	ratePtr, err := platform.ParseStrictFloat(r.FormValue("points_rate"), "积分倍率", 0, 10, false)
	if err != nil {
		platform.RedirectErr(w, r, "/admin/discounts", err.Error())
		return
	}
	pointsRateBPS := 10000
	if ratePtr != nil {
		pointsRateBPS = int(math.Round(*ratePtr * 10000))
	}
	sortOrder, err := platform.ParseSortOrder(r.FormValue("sort_order"))
	if err != nil {
		platform.RedirectErr(w, r, "/admin/discounts", err.Error())
		return
	}
	active := r.FormValue("active") == "1"
	if id == 0 && r.FormValue("active") == "" {
		active = true
	}

	_, err = SaveTier(r.Context(), s.pool, Tier{
		ID:            id,
		Name:          r.FormValue("name"),
		SortOrder:     sortOrder,
		MinSpentCents: minSpent,
		DiscountBPS:   bps,
		ApplyScope:    r.FormValue("apply_scope"),
		PointsRateBPS: pointsRateBPS,
		Active:        active,
	})
	if err != nil {
		redirectUserErr(w, r, "/admin/discounts", "保存会员等级失败", err)
		return
	}
	platform.RedirectMsg(w, r, "/admin/discounts", "会员等级已保存")
}

func (s *Service) TierToggle(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := ToggleTier(r.Context(), s.pool, id); err != nil {
		redirectUserErr(w, r, "/admin/discounts", "更新会员等级状态失败", err)
		return
	}
	platform.RedirectMsg(w, r, "/admin/discounts", "会员等级状态已更新")
}

func (s *Service) LoyaltyConfigSave(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		platform.RedirectErr(w, r, "/admin/discounts", "系统设置未初始化")
		return
	}
	_ = r.ParseForm()
	ppyPtr, err := platform.ParseStrictInt(r.FormValue("points_per_yuan"), "每元攒积分", 0, settings.MaxPointsPerYuan)
	if err != nil {
		platform.RedirectErr(w, r, "/admin/discounts", err.Error())
		return
	}
	ratePtr, err := platform.ParseStrictInt(r.FormValue("points_redeem_rate"), "抵现积分汇率", 0, settings.MaxPointsRedeemRate)
	if err != nil {
		platform.RedirectErr(w, r, "/admin/discounts", err.Error())
		return
	}
	maxPctPtr, err := platform.ParseStrictInt(r.FormValue("points_max_pct"), "单笔积分抵现上限", 0, 100)
	if err != nil {
		platform.RedirectErr(w, r, "/admin/discounts", err.Error())
		return
	}

	cfg := s.loyaltyConfig()
	if ppyPtr != nil {
		cfg.PointsPerYuan = *ppyPtr
	}
	if ratePtr != nil {
		cfg.PointsRedeemRate = *ratePtr
	}
	if maxPctPtr != nil {
		cfg.PointsMaxPct = *maxPctPtr
	}

	pays := r.Form["preset_pay"]
	bonuses := r.Form["preset_bonus"]
	if len(pays) > 0 {
		presets := make([]settings.RechargePreset, 0, len(pays))
		for i, rawPay := range pays {
			if strings.TrimSpace(rawPay) == "" {
				continue
			}
			pc, perr := platform.MoneyCents(rawPay)
			if perr != nil || pc <= 0 {
				platform.RedirectErr(w, r, "/admin/discounts", "快捷充值档位的实充金额必须大于 0")
				return
			}
			var bc int
			if i < len(bonuses) && strings.TrimSpace(bonuses[i]) != "" {
				bc, perr = platform.MoneyCents(bonuses[i])
				if perr != nil {
					platform.RedirectErr(w, r, "/admin/discounts", "快捷充值档位的赠金格式不正确")
					return
				}
			}
			presets = append(presets, settings.RechargePreset{PayCents: pc, BonusCents: bc})
		}
		if len(presets) > 0 {
			cfg.RechargePresets = presets
		}
	}

	if _, err := s.settings.SaveLoyalty(r.Context(), cfg); err != nil {
		redirectUserErr(w, r, "/admin/discounts", "保存积分与充值规则失败", err)
		return
	}
	platform.RedirectMsg(w, r, "/admin/discounts", "积分与充值档位规则已保存")
}
