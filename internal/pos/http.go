package pos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/event"
	"pourops/internal/inventory"
	"pourops/internal/loyalty"
	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/internal/ui"
)

type PosPage struct {
	ui.Layout
	Spots             []Spot
	PosBeers          []PosBeer
	PosItems          []inventory.RetailItem
	Order             *OrderView
	PosOrderID        int64
	PayToken          string
	DiscountRules     []loyalty.DiscountRule
	CanCustomDiscount bool
	PassPlans         []loyalty.PassPlan
	LoyaltyCfg        settings.LoyaltyConfig
}

func (p PosPage) spotsByKind(kind string) []Spot {
	var out []Spot
	for _, sp := range p.Spots {
		if sp.Kind == kind {
			out = append(out, sp)
		}
	}
	return out
}

func (p PosPage) BarSpots() []Spot   { return p.spotsByKind("bar") }
func (p PosPage) TableSpots() []Spot { return p.spotsByKind("table") }
func (p PosPage) TempSpots() []Spot  { return p.spotsByKind("temp") }

// BoundMemberJSON 返回当前订单绑定会员的 JSON 供前端初始化（不含敏感信息）。
func (p PosPage) BoundMemberJSON() template.JS {
	if p.Order == nil || p.Order.Member == nil {
		return template.JS("null")
	}
	b, err := json.Marshal(toMemberDTO(p.Order.Member))
	if err != nil {
		return template.JS("null")
	}
	return template.JS(b)
}

type PayPage struct {
	ui.Layout
	Pay *Payment
}

// Service 提供台位管理、加减单品、会员/月卡/折扣核销、分拆/合单结账以及收款/打印扩展接口。
type Service struct {
	pool     *pgxpool.Pool
	renderer *ui.Renderer
	bus      *event.EventBus
	gateway  PaymentGateway
	printer  ReceiptPrinter
	settings *settings.SettingsStore
}

func NewService(pool *pgxpool.Pool, renderer *ui.Renderer, bus *event.EventBus, gw PaymentGateway, printer ReceiptPrinter) *Service {
	if gw == nil {
		gw = ManualGateway{}
	}
	if printer == nil {
		printer = NoopPrinter{}
	}
	return &Service{
		pool:     pool,
		renderer: renderer,
		bus:      bus,
		gateway:  gw,
		printer:  printer,
	}
}

func (s *Service) SetSettingsStore(cfg *settings.SettingsStore) {
	s.settings = cfg
}

func (s *Service) loyaltyConfig() settings.LoyaltyConfig {
	if s.settings != nil {
		return s.settings.Loyalty()
	}
	return settings.DefaultLoyaltyConfig()
}

func (s *Service) RegisterRoutes(mux *http.ServeMux, guards ui.Guards) {
	mux.HandleFunc("GET /pos/{$}", guards.Any(s.PosSpots))
	mux.HandleFunc("POST /pos/temp/add", guards.Any(s.PosAddTemp))
	mux.HandleFunc("POST /pos/temp/remove", guards.Any(s.PosRemoveTemp))
	mux.HandleFunc("POST /pos/open", guards.Any(s.PosOpen))
	mux.HandleFunc("GET /pos/order/{id}", guards.Any(s.PosOrder))
	mux.HandleFunc("POST /pos/order/{id}/add", guards.Any(s.PosAddItem))
	mux.HandleFunc("POST /pos/order/{id}/item/{item}/less", guards.Any(s.PosLessItem))
	mux.HandleFunc("POST /pos/order/{id}/item/{item}/more", guards.Any(s.PosMoreItem))
	mux.HandleFunc("POST /pos/order/{id}/member", guards.Any(s.PosBindOrderMember))
	mux.HandleFunc("POST /pos/order/{id}/preview", guards.Any(s.PosPreviewPay))
	mux.HandleFunc("POST /pos/order/{id}/pay", guards.Any(s.PosPay))
	mux.HandleFunc("GET /pos/pay/{id}", guards.Any(s.PosPayPage))

	mux.HandleFunc("GET /pos/members/search", guards.Any(s.PosMemberSearch))
	mux.HandleFunc("POST /pos/members/quick", guards.Any(s.PosMemberQuickCreate))
	mux.HandleFunc("POST /pos/members/{id}/recharge", guards.Any(s.PosMemberRecharge))
	mux.HandleFunc("POST /pos/members/{id}/pass", guards.Any(s.PosMemberIssuePass))
}

func (s *Service) posMenu(r *http.Request) (*PosPage, error) {
	beers, err := ListPosBeers(r.Context(), s.pool)
	if err != nil {
		return nil, err
	}
	items, err := ListPosItems(r.Context(), s.pool)
	if err != nil {
		return nil, err
	}
	return &PosPage{
		PosBeers:   beers,
		PosItems:   items,
		LoyaltyCfg: s.loyaltyConfig(),
	}, nil
}

func (s *Service) PosSpots(w http.ResponseWriter, r *http.Request) {
	data, err := s.posMenu(r)
	if err != nil {
		log.Printf("pos spots menu: %v", err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	spots, err := ListSpots(r.Context(), s.pool)
	if err != nil {
		log.Printf("pos spots list: %v", err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	data.Title = "点餐"
	data.Spots = spots
	s.renderer.RenderShell(w, r, "pos/pos.html", "pos", data)
}

func (s *Service) PosAddTemp(w http.ResponseWriter, r *http.Request) {
	s.tempSpotMutate(w, r, AddTempSpot)
}

func (s *Service) PosRemoveTemp(w http.ResponseWriter, r *http.Request) {
	s.tempSpotMutate(w, r, RemoveTempSpot)
}

func (s *Service) tempSpotMutate(w http.ResponseWriter, r *http.Request, fn func(context.Context, *pgxpool.Pool) error) {
	if err := fn(r.Context(), s.pool); err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, "/pos/", msg)
			return
		}
		log.Printf("pos temp spot mutate: %v", err)
		platform.RedirectErr(w, r, "/pos/", "调整临时台位失败，请稍后重试")
		return
	}
	http.Redirect(w, r, "/pos/", http.StatusSeeOther)
}

func (s *Service) PosOpen(w http.ResponseWriter, r *http.Request) {
	key := r.FormValue("spot")
	if !strings.HasPrefix(key, "bar-") && !strings.HasPrefix(key, "table-") && !strings.HasPrefix(key, "temp-") {
		platform.RedirectErr(w, r, "/pos/", "没有这个台位")
		return
	}
	id, err := EnsureOrder(r.Context(), s.pool, key)
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, "/pos/", msg)
			return
		}
		log.Printf("pos open (%s): %v", key, err)
		platform.RedirectErr(w, r, "/pos/", "开单失败，请稍后重试")
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/pos/order/%d", id), http.StatusSeeOther)
}

func (s *Service) PosOrder(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	o, err := LoadOrder(ctx, s.pool, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "找不到这张账单", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("pos order (%d): %v", id, err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	data, err := s.posMenu(r)
	if err != nil {
		log.Printf("pos order menu (%d): %v", id, err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	role := platform.ActorRole(ctx)
	canCustom := loyalty.CanCustomDiscount(role)
	allRules, err := loyalty.ListDiscountRules(ctx, s.pool, true)
	if err != nil {
		log.Printf("pos discount rules (%d): %v", id, err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	for _, rule := range allRules {
		if rule.AllowStaff || canCustom {
			data.DiscountRules = append(data.DiscountRules, rule)
		}
	}
	plans, err := loyalty.ListPassPlans(ctx, s.pool, true)
	if err != nil {
		log.Printf("pos pass plans (%d): %v", id, err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	data.PassPlans = plans
	data.CanCustomDiscount = canCustom
	data.Title = o.Label
	data.Order = &o
	if !o.Closed {
		data.PosOrderID = o.ID
		data.PayToken = platform.RandomHex(16)
	}
	s.renderer.RenderShell(w, r, "pos/pos.html", "pos", data)
}

func (s *Service) PosAddItem(w http.ResponseWriter, r *http.Request) {
	orderID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	back := fmt.Sprintf("/pos/order/%d", orderID)
	refID, _ := strconv.ParseInt(r.FormValue("ref"), 10, 64)

	var err error
	switch r.FormValue("kind") {
	case "beer":
		ml, _ := strconv.Atoi(r.FormValue("ml"))
		err = AddBeerOrderItem(r.Context(), s.pool, orderID, refID, ml)
	case "retail":
		err = AddRetailOrderItem(r.Context(), s.pool, orderID, refID)
	default:
		platform.RedirectErr(w, r, back, "不认识的商品")
		return
	}
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, back, msg)
			return
		}
		log.Printf("pos add item (%d): %v", orderID, err)
		platform.RedirectErr(w, r, back, "加购失败，请稍后重试")
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Service) PosLessItem(w http.ResponseWriter, r *http.Request) {
	s.adjustOrderItem(w, r, LessOrderItem)
}

func (s *Service) PosMoreItem(w http.ResponseWriter, r *http.Request) {
	s.adjustOrderItem(w, r, MoreOrderItem)
}

func (s *Service) adjustOrderItem(w http.ResponseWriter, r *http.Request, op func(context.Context, *pgxpool.Pool, int64, int64) error) {
	orderID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	itemID, _ := strconv.ParseInt(r.PathValue("item"), 10, 64)
	if err := op(r.Context(), s.pool, orderID, itemID); err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, fmt.Sprintf("/pos/order/%d", orderID), msg)
			return
		}
		log.Printf("pos adjust item (%d/%d): %v", orderID, itemID, err)
		platform.RedirectErr(w, r, fmt.Sprintf("/pos/order/%d", orderID), "修改数量失败，请稍后重试")
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/pos/order/%d", orderID), http.StatusSeeOther)
}

// ---------- POS 会员与算价预览 JSON API ----------

type passDTO struct {
	ID             int64  `json:"id"`
	PlanName       string `json:"plan_name"`
	BenefitType    string `json:"benefit_type"`
	Scope          string `json:"scope"`
	ScopeText      string `json:"scope_text"`
	DailyLimit     int    `json:"daily_limit"`
	UsedToday      int    `json:"used_today"`
	RemainingToday int    `json:"remaining_today"`
	MaxML          int    `json:"max_ml"`
	MaxUnitCents   int    `json:"max_unit_cents"`
	CapMode        string `json:"cap_mode"`
	DiscountBPS    int    `json:"discount_bps"`
	DiscountText   string `json:"discount_text"`
	ExpiresText    string `json:"expires_text"`
}

type memberDTO struct {
	ID               int64    `json:"id"`
	MemberNo         string   `json:"member_no"`
	Name             string   `json:"name"`
	Phone            string   `json:"phone"`
	TierName         string   `json:"tier_name"`
	TierDiscountBPS  int      `json:"tier_discount_bps"`
	TierDiscountText string   `json:"tier_discount_text"`
	BalanceCents     int      `json:"balance_cents"`
	BalanceText      string   `json:"balance_text"`
	Points           int      `json:"points"`
	ActivePass       *passDTO `json:"active_pass"`
}

func toMemberDTO(m *loyalty.MemberSnapshot) *memberDTO {
	if m == nil {
		return nil
	}
	dto := &memberDTO{
		ID:               m.ID,
		MemberNo:         m.MemberNo,
		Name:             m.Name,
		Phone:            m.Phone,
		TierName:         m.TierName,
		TierDiscountBPS:  m.TierDiscountBPS,
		TierDiscountText: m.TierDiscountText(),
		BalanceCents:     m.BalanceCents,
		BalanceText:      m.BalanceText(),
		Points:           m.Points,
	}
	if m.ActivePass != nil {
		p := m.ActivePass
		dto.ActivePass = &passDTO{
			ID:             p.ID,
			PlanName:       p.PlanName,
			BenefitType:    p.BenefitType,
			Scope:          p.Scope,
			ScopeText:      p.ScopeText(),
			DailyLimit:     p.DailyLimit,
			UsedToday:      p.UsedToday,
			RemainingToday: p.RemainingToday(),
			MaxML:          p.MaxML,
			MaxUnitCents:   p.MaxUnitCents,
			CapMode:        p.CapMode,
			DiscountBPS:    p.DiscountBPS,
			DiscountText:   p.DiscountText(),
			ExpiresText:    p.ExpiresText(),
		}
	}
	return dto
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONUserErr(w http.ResponseWriter, err error, fallback string) {
	if msg, ok := platform.UserErrorMessage(err); ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	log.Printf("pos api error: %v", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fallback})
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json") ||
		r.Header.Get("X-Requested-With") == "fetch" ||
		r.URL.Query().Get("format") == "json"
}

func (s *Service) PosMemberSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	list, err := loyalty.SearchMembers(r.Context(), s.pool, q, 12)
	if err != nil {
		writeJSONUserErr(w, err, "检索会员失败")
		return
	}
	dtos := make([]*memberDTO, 0, len(list))
	for i := range list {
		dtos = append(dtos, toMemberDTO(&list[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": dtos})
}

func (s *Service) validatePosRechargeBonus(role string, payCents, bonusCents int) error {
	if bonusCents == 0 || loyalty.CanCustomDiscount(role) {
		return nil
	}
	cfg := s.loyaltyConfig()
	for _, p := range cfg.RechargePresets {
		if p.PayCents == payCents && p.BonusCents == bonusCents {
			return nil
		}
	}
	return platform.UserErr("店员充值仅可使用后台预设档位赠金，自定义赠金请联系店长或 BOSS")
}

func (s *Service) PosMemberQuickCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = r.ParseForm()
	role := platform.ActorRole(ctx)

	var payCents, bonusCents int
	if rawPay := strings.TrimSpace(r.FormValue("recharge_pay_amount")); rawPay != "" {
		c, err := platform.MoneyCents(rawPay)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "充值金额格式不正确"})
			return
		}
		payCents = c
	}
	if rawBonus := strings.TrimSpace(r.FormValue("recharge_bonus_amount")); rawBonus != "" {
		c, err := platform.MoneyCents(rawBonus)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "赠金金额格式不正确"})
			return
		}
		bonusCents = c
	}
	if payCents > 0 {
		if err := s.validatePosRechargeBonus(role, payCents, bonusCents); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
	}

	memberID, err := loyalty.QuickCreateMember(ctx, s.pool, r.FormValue("name"), r.FormValue("phone"))
	if err != nil {
		writeJSONUserErr(w, err, "创建会员失败，请稍后重试")
		return
	}

	idem := strings.TrimSpace(r.FormValue("idempotency_key"))
	if payCents > 0 {
		rIdem := ""
		if idem != "" {
			rIdem = idem + ":recharge"
		}
		rec, err := loyalty.RechargeMember(ctx, s.pool, memberID, payCents, bonusCents, r.FormValue("recharge_method"), "POS快捷开户充值", rIdem)
		if err != nil {
			writeJSONUserErr(w, err, "开户成功但充值失败，请进入会员继续充值")
			return
		}
		if !rec.Replayed && s.bus != nil {
			s.bus.Publish(ctx, event.EventMemberRecharged, platform.MemberSubject(rec.MemberName, rec.MemberPhone), map[string]any{
				"member_id":           rec.MemberID,
				"pay_cents":           rec.PayCents,
				"bonus_cents":         rec.BonusCents,
				"balance_after_cents": rec.BalanceAfterCents,
				"pay_method":          rec.PayMethod,
			})
		}
	}

	if planID, _ := strconv.ParseInt(r.FormValue("pass_plan_id"), 10, 64); planID > 0 {
		pIdem := ""
		if idem != "" {
			pIdem = idem + ":pass"
		}
		passRes, err := loyalty.IssuePass(ctx, s.pool, memberID, planID, r.FormValue("pass_method"), "POS快捷办卡", pIdem)
		if err != nil {
			writeJSONUserErr(w, err, "会员已建档，但办理月卡失败")
			return
		}
		if !passRes.Replayed && s.bus != nil {
			s.bus.Publish(ctx, event.EventPassPurchased, platform.MemberSubject(passRes.MemberName, passRes.MemberPhone), map[string]any{
				"member_id":   passRes.Pass.MemberID,
				"pass_id":     passRes.Pass.ID,
				"plan_name":   passRes.Pass.PlanName,
				"price_cents": passRes.Pass.PriceCents,
				"pay_method":  passRes.Pass.PayMethod,
				"renewed":     passRes.Renewed,
				"expires_at":  passRes.Pass.ExpiresAt,
			})
		}
	}

	if orderID, _ := strconv.ParseInt(r.FormValue("order_id"), 10, 64); orderID > 0 {
		_ = BindOrderMember(ctx, s.pool, orderID, memberID)
	}

	snap, err := loyalty.LoadCheckoutMemberSnapshot(ctx, s.pool, memberID, 0, time.Now(), false)
	if err != nil {
		writeJSONUserErr(w, err, "读取会员信息失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"member": toMemberDTO(snap)})
}

func (s *Service) PosMemberRecharge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	memberID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	_ = r.ParseForm()
	payCents, err := platform.MoneyCents(r.FormValue("pay_amount"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "充值金额格式不正确"})
		return
	}
	bonusCents, err := platform.MoneyCents(r.FormValue("bonus_amount"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "赠金金额格式不正确"})
		return
	}
	if err := s.validatePosRechargeBonus(platform.ActorRole(ctx), payCents, bonusCents); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}

	rec, err := loyalty.RechargeMember(ctx, s.pool, memberID, payCents, bonusCents,
		r.FormValue("pay_method"), r.FormValue("note"), r.FormValue("idempotency_key"))
	if err != nil {
		writeJSONUserErr(w, err, "充值失败，请稍后重试")
		return
	}
	if !rec.Replayed && s.bus != nil {
		s.bus.Publish(ctx, event.EventMemberRecharged, platform.MemberSubject(rec.MemberName, rec.MemberPhone), map[string]any{
			"member_id":           rec.MemberID,
			"pay_cents":           rec.PayCents,
			"bonus_cents":         rec.BonusCents,
			"balance_after_cents": rec.BalanceAfterCents,
			"pay_method":          rec.PayMethod,
		})
	}
	snap, err := loyalty.LoadCheckoutMemberSnapshot(ctx, s.pool, memberID, 0, time.Now(), false)
	if err != nil {
		writeJSONUserErr(w, err, "读取会员信息失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"member": toMemberDTO(snap)})
}

func (s *Service) PosMemberIssuePass(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	memberID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	_ = r.ParseForm()
	planID, _ := strconv.ParseInt(r.FormValue("plan_id"), 10, 64)
	if planID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请选择月卡方案"})
		return
	}
	passRes, err := loyalty.IssuePass(ctx, s.pool, memberID, planID,
		r.FormValue("pay_method"), r.FormValue("note"), r.FormValue("idempotency_key"))
	if err != nil {
		writeJSONUserErr(w, err, "办理月卡失败，请稍后重试")
		return
	}
	if !passRes.Replayed && s.bus != nil {
		s.bus.Publish(ctx, event.EventPassPurchased, platform.MemberSubject(passRes.MemberName, passRes.MemberPhone), map[string]any{
			"member_id":   passRes.Pass.MemberID,
			"pass_id":     passRes.Pass.ID,
			"plan_name":   passRes.Pass.PlanName,
			"price_cents": passRes.Pass.PriceCents,
			"pay_method":  passRes.Pass.PayMethod,
			"renewed":     passRes.Renewed,
			"expires_at":  passRes.Pass.ExpiresAt,
		})
	}
	snap, err := loyalty.LoadCheckoutMemberSnapshot(ctx, s.pool, memberID, 0, time.Now(), false)
	if err != nil {
		writeJSONUserErr(w, err, "读取会员信息失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"member":  toMemberDTO(snap),
		"renewed": passRes.Renewed,
	})
}

func (s *Service) PosBindOrderMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orderID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	back := fmt.Sprintf("/pos/order/%d", orderID)
	_ = r.ParseForm()
	memberID, _ := strconv.ParseInt(r.FormValue("member_id"), 10, 64)
	if err := BindOrderMember(ctx, s.pool, orderID, memberID); err != nil {
		if wantsJSON(r) {
			writeJSONUserErr(w, err, "绑定会员失败")
			return
		}
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, back, msg)
			return
		}
		platform.RedirectErr(w, r, back, "绑定会员失败")
		return
	}
	if wantsJSON(r) {
		var snap *loyalty.MemberSnapshot
		if memberID > 0 {
			snap, _ = loyalty.LoadCheckoutMemberSnapshot(ctx, s.pool, memberID, 0, time.Now(), false)
		}
		writeJSON(w, http.StatusOK, map[string]any{"member": toMemberDTO(snap)})
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func parsePayRequest(r *http.Request, cfg settings.LoyaltyConfig) ([]int64, PayOptions, error) {
	_ = r.ParseForm()
	ids := make([]int64, 0, len(r.Form["items"]))
	for _, v := range r.Form["items"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	opts := PayOptions{
		Method:         r.FormValue("method"),
		IdempotencyKey: r.FormValue("idempotency_key"),
		UsePass:        r.FormValue("use_pass") == "1",
		UsePoints:      r.FormValue("use_points") == "1",
		UseBalance:     r.FormValue("use_balance") == "1",
		DiscountNote:   r.FormValue("discount_note"),
		ActorRole:      platform.ActorRole(r.Context()),
		LoyaltyCfg:     &cfg,
	}
	if cups, err := strconv.Atoi(strings.TrimSpace(r.FormValue("pass_cups"))); err == nil && cups > 0 {
		opts.PassCupsRequested = cups
	}
	if rawExp := strings.TrimSpace(r.FormValue("expected_total_cents")); rawExp != "" {
		if v, err := strconv.Atoi(rawExp); err == nil && v >= 0 {
			opts.ExpectedTotalCents = &v
		}
	}
	if r.Form.Has("member_id") {
		mid, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("member_id")), 10, 64)
		opts.MemberID = &mid
	}
	if pid, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("pass_id")), 10, 64); err == nil && pid > 0 {
		opts.MemberPassID = &pid
	}
	if rid, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("discount_rule_id")), 10, 64); err == nil && rid > 0 {
		opts.DiscountRuleID = &rid
	}
	cKind := strings.TrimSpace(r.FormValue("custom_discount_kind"))
	cVal := strings.TrimSpace(r.FormValue("custom_discount_value"))
	if cKind != "" && cVal != "" {
		scope := strings.TrimSpace(r.FormValue("custom_discount_scope"))
		if scope == "" {
			scope = "all"
		}
		switch cKind {
		case "percent":
			bps, err := loyalty.ParseDiscountFoldBPS(cVal, "自定义折率", 100, 10000)
			if err != nil {
				return nil, opts, err
			}
			opts.CustomDiscount = &loyalty.CustomDiscount{
				Kind:        "percent",
				DiscountBPS: bps,
				Scope:       scope,
				Note:        opts.DiscountNote,
			}
		case "fixed":
			cents, err := platform.MoneyCents(cVal)
			if err != nil {
				return nil, opts, platform.UserErr("自定义立减金额格式不正确")
			}
			opts.CustomDiscount = &loyalty.CustomDiscount{
				Kind:       "fixed",
				FixedCents: cents,
				Scope:      scope,
				Note:       opts.DiscountNote,
			}
		default:
			return nil, opts, platform.UserErr("不支持的自定义折扣类型")
		}
	}
	return ids, opts, nil
}

func (s *Service) PosPreviewPay(w http.ResponseWriter, r *http.Request) {
	orderID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	ids, opts, err := parsePayRequest(r, s.loyaltyConfig())
	if err != nil {
		writeJSONUserErr(w, err, "参数格式不正确")
		return
	}
	pricing, snap, err := PreviewCheckout(r.Context(), s.pool, orderID, ids, opts)
	if err != nil {
		writeJSONUserErr(w, err, "计算应付金额失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pricing": pricing,
		"member":  toMemberDTO(snap),
	})
}

func (s *Service) PosPay(w http.ResponseWriter, r *http.Request) {
	orderID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	back := fmt.Sprintf("/pos/order/%d", orderID)
	ids, opts, err := parsePayRequest(r, s.loyaltyConfig())
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, back, msg)
			return
		}
		platform.RedirectErr(w, r, back, "结账参数不正确")
		return
	}
	res, err := PayItemsWithOptions(r.Context(), s.pool, s.gateway, orderID, ids, opts)
	if err != nil {
		msg := "结账失败，请稍后重试"
		if um, ok := platform.UserErrorMessage(err); ok {
			msg = um
		} else {
			log.Printf("pos pay (%d): %v", orderID, err)
		}
		platform.RedirectErr(w, r, back, msg)
		return
	}

	if !res.Replayed {
		if s.printer != nil {
			receipt := Receipt{
				PaymentID:         res.PaymentID,
				OrderID:           res.OrderID,
				SpotLabel:         res.SpotLabel,
				GrossCents:        res.GrossCents,
				DiscountCents:     res.DiscountCents,
				AmountCents:       res.AmountCents,
				BalancePaidCents:  res.BalancePaidCents,
				ExternalPaidCents: res.ExternalPaidCents,
				DiscountLabel:     res.DiscountLabel,
				MemberName:        res.MemberName,
				PointsEarned:      res.PointsEarned,
				PassCupsUsed:      res.PassCupsUsed,
				ItemsCount:        res.ItemsCount,
				Actor:             platform.OperationActor(r.Context()),
				Lines:             res.Lines,
			}
			printCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			if err := s.printer.Print(printCtx, receipt); err != nil {
				log.Printf("pos receipt print (%s) payment %d failed: %v", s.printer.Name(), res.PaymentID, err)
			}
			cancel()
		}
		if s.bus != nil {
			s.bus.Publish(r.Context(), event.EventOrderPaid, res.SpotLabel, map[string]any{
				"payment_id":          res.PaymentID,
				"order_id":            res.OrderID,
				"spot":                res.Spot,
				"spot_label":          res.SpotLabel,
				"gross_cents":         res.GrossCents,
				"discount_cents":      res.DiscountCents,
				"amount_cents":        res.AmountCents,
				"balance_paid_cents":  res.BalancePaidCents,
				"external_paid_cents": res.ExternalPaidCents,
				"discount_label":      res.DiscountLabel,
				"member_id":           res.MemberID,
				"member_name":         res.MemberName,
				"pass_cups_used":      res.PassCupsUsed,
				"points_spent":        res.PointsSpent,
				"points_earned":       res.PointsEarned,
				"items_count":         res.ItemsCount,
				"closed":              res.Closed,
				"method":              res.Method,
				"lines":               res.Lines,
			})
			if res.PassCupsUsed > 0 {
				s.bus.Publish(r.Context(), event.EventPassRedeemed, platform.MemberSubject(res.MemberName, res.MemberPhone), map[string]any{
					"payment_id":  res.PaymentID,
					"order_id":    res.OrderID,
					"member_id":   res.MemberID,
					"pass_id":     res.MemberPassID,
					"cups":        res.PassCupsUsed,
					"saved_cents": res.PassDiscountCents,
				})
			}
			for _, low := range res.LowStock {
				s.bus.Publish(r.Context(), event.EventRetailLowStock, platform.RetailSubject(low.Name), map[string]any{
					"item_id": low.ItemID,
					"name":    low.Name,
					"stock":   low.Stock,
					"active":  low.Active,
				})
			}
		}
	}

	var notes []string
	if res.SkippedCount > 0 {
		notes = append(notes, fmt.Sprintf("已自动跳过 %d 项已结商品", res.SkippedCount))
	}
	if res.OverSoldCount > 0 {
		notes = append(notes, fmt.Sprintf("提醒：%d 款酒水累计售出已超标称桶容量", res.OverSoldCount))
	}
	if len(notes) > 0 {
		platform.RedirectMsg(w, r, fmt.Sprintf("/pos/pay/%d", res.PaymentID), strings.Join(notes, "；"))
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/pos/pay/%d", res.PaymentID), http.StatusSeeOther)
}

func (s *Service) PosPayPage(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	p, err := LoadPayment(r.Context(), s.pool, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "找不到这笔账", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("pos pay page (%d): %v", id, err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderer.RenderShell(w, r, "pos/pos_pay.html", "pos_pay", &PayPage{
		Layout: ui.Layout{Title: "收款"},
		Pay:    &p,
	})
}
