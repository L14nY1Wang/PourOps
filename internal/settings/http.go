package settings

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"pourops/internal/event"
	"pourops/internal/platform"
	"pourops/internal/ui"
)

type SettingsPage struct {
	ui.Layout
	Settings       StoreSettings
	EventOptions   []platform.Option
	HasSecret      bool
	SelectedEvents map[string]bool
	CatalogTotal   int
}

// Service 提供 BOSS 专属门店系统参数、历史酒单导出导入入口与 Webhook 出站通知管理界面。
type Service struct {
	store          *SettingsStore
	webhooks       *event.WebhookDispatcher
	renderer       *ui.Renderer
	catalogCountFn func(context.Context) (int, error)
}

func NewService(store *SettingsStore, webhooks *event.WebhookDispatcher, renderer *ui.Renderer) *Service {
	return &Service{store: store, webhooks: webhooks, renderer: renderer}
}

func (s *Service) SetCatalogCountFn(fn func(context.Context) (int, error)) {
	s.catalogCountFn = fn
}

func (s *Service) catalogTotal(ctx context.Context) int {
	if s.catalogCountFn == nil {
		return 0
	}
	n, err := s.catalogCountFn(ctx)
	if err != nil {
		log.Printf("count catalog beers: %v", err)
		return 0
	}
	return n
}

func (s *Service) RegisterRoutes(mux *http.ServeMux, guards ui.Guards) {
	mux.HandleFunc("GET /admin/settings", guards.Boss(s.Page))
	mux.HandleFunc("POST /admin/settings", guards.Boss(s.Save))
	mux.HandleFunc("POST /admin/settings/general", guards.Boss(s.SaveGeneral))
	mux.HandleFunc("POST /admin/settings/webhook", guards.Boss(s.SaveWebhook))
	mux.HandleFunc("POST /admin/settings/categories/add", guards.Boss(s.AddCategory))
	mux.HandleFunc("POST /admin/settings/categories/delete", guards.Boss(s.DeleteCategory))
	mux.HandleFunc("POST /admin/settings/categories/reset", guards.Boss(s.ResetCategories))
	mux.HandleFunc("POST /admin/settings/webhook-test", guards.Boss(s.TestWebhook))
}

func buildSelectedMap(events []string) map[string]bool {
	out := make(map[string]bool, len(events))
	for _, ev := range events {
		out[ev] = true
	}
	return out
}

func (s *Service) Page(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cfg := s.store.Get()
	s.renderer.Render(w, r, "admin/settings.html", &SettingsPage{
		Layout:         ui.Layout{Title: "系统设置", Nav: "settings", Msg: q.Get("msg"), Err: q.Get("err")},
		Settings:       cfg,
		EventOptions:   event.WebhookEventOptions,
		HasSecret:      cfg.WebhookSecret != "",
		SelectedEvents: buildSelectedMap(cfg.WebhookEvents),
		CatalogTotal:   s.catalogTotal(r.Context()),
	})
}

func (s *Service) renderFormError(w http.ResponseWriter, r *http.Request, cfg StoreSettings, hasSecret bool, errMsg string) {
	w.WriteHeader(http.StatusBadRequest)
	s.renderer.Render(w, r, "admin/settings.html", &SettingsPage{
		Layout:         ui.Layout{Title: "系统设置", Nav: "settings", Err: errMsg},
		Settings:       cfg,
		EventOptions:   event.WebhookEventOptions,
		HasSecret:      hasSecret,
		SelectedEvents: buildSelectedMap(cfg.WebhookEvents),
		CatalogTotal:   s.catalogTotal(r.Context()),
	})
}

func parsePricingFields(r *http.Request, dst *StoreSettings) string {
	sellPtr, sellErr := platform.ParseStrictInt(r.FormValue("default_sellable_pct"), "默认可售比例", MinSellablePct, MaxSellablePct)
	marginPtr, marginErr := platform.ParseStrictInt(r.FormValue("default_margin_pct"), "默认目标毛利率", MinMarginPct, MaxMarginPct)
	if sellPtr != nil {
		dst.DefaultSellablePct = *sellPtr
	}
	if marginPtr != nil {
		dst.DefaultMarginPct = *marginPtr
	}
	if sellErr != nil {
		return sellErr.Error()
	}
	if sellPtr == nil {
		return "请填写默认可售比例（50%–100%）"
	}
	if marginErr != nil {
		return marginErr.Error()
	}
	if marginPtr == nil {
		return "请填写默认目标毛利率（10%–85%）"
	}
	return ""
}

func saveErrorMessage(err error, fallback string) string {
	if msg, ok := platform.UserErrorMessage(err); ok {
		return msg
	}
	return fallback
}

func (s *Service) Save(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单格式不正确", http.StatusBadRequest)
		return
	}
	existing := s.store.Get()

	next := StoreSettings{
		Brand:          strings.TrimSpace(r.FormValue("brand")),
		BeerCategories: existing.BeerCategories,
		WebhookURL:     strings.TrimSpace(r.FormValue("webhook_url")),
		WebhookSecret:  existing.WebhookSecret,
		WebhookEvents:  r.Form["webhook_events"],
	}
	if r.FormValue("clear_webhook_secret") != "" {
		next.WebhookSecret = ""
	} else if sec := strings.TrimSpace(r.FormValue("webhook_secret")); sec != "" {
		next.WebhookSecret = sec
	}

	if errMsg := parsePricingFields(r, &next); errMsg != "" {
		s.renderFormError(w, r, next, existing.WebhookSecret != "", errMsg)
		return
	}

	if _, err := s.store.Save(r.Context(), next); err != nil {
		s.renderFormError(w, r, next, existing.WebhookSecret != "", saveErrorMessage(err, "保存系统设置失败，请稍后重试"))
		return
	}

	http.Redirect(w, r, "/admin/settings?msg="+platform.URLMsg("系统设置已保存并实时生效"), http.StatusSeeOther)
}

func (s *Service) SaveGeneral(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单格式不正确", http.StatusBadRequest)
		return
	}
	draft := s.store.Get()
	hasSecret := draft.WebhookSecret != ""
	draft.Brand = strings.TrimSpace(r.FormValue("brand"))

	if errMsg := parsePricingFields(r, &draft); errMsg != "" {
		s.renderFormError(w, r, draft, hasSecret, errMsg)
		return
	}

	if _, err := s.store.SaveGeneral(r.Context(), draft.Brand, draft.DefaultSellablePct, draft.DefaultMarginPct); err != nil {
		s.renderFormError(w, r, draft, hasSecret, saveErrorMessage(err, "保存门店与核算设置失败，请稍后重试"))
		return
	}

	http.Redirect(w, r, "/admin/settings?msg="+platform.URLMsg("门店与核算设置已保存并实时生效"), http.StatusSeeOther)
}

func (s *Service) SaveWebhook(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单格式不正确", http.StatusBadRequest)
		return
	}
	draft := s.store.Get()
	hasSecret := draft.WebhookSecret != ""
	draft.WebhookURL = strings.TrimSpace(r.FormValue("webhook_url"))
	draft.WebhookEvents = r.Form["webhook_events"]

	if _, err := s.store.SaveWebhook(r.Context(), draft.WebhookURL, r.FormValue("webhook_secret"), r.FormValue("clear_webhook_secret") != "", draft.WebhookEvents); err != nil {
		s.renderFormError(w, r, draft, hasSecret, saveErrorMessage(err, "保存 Webhook 设置失败，请稍后重试"))
		return
	}

	http.Redirect(w, r, "/admin/settings?msg="+platform.URLMsg("Webhook 配置已保存"), http.StatusSeeOther)
}

func (s *Service) AddCategory(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单格式不正确", http.StatusBadRequest)
		return
	}
	raw := strings.TrimSpace(r.FormValue("name"))
	if _, err := s.store.AddBeerCategory(r.Context(), raw); err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			http.Redirect(w, r, "/admin/settings?err="+platform.URLMsg(msg), http.StatusSeeOther)
			return
		}
		log.Printf("add beer category (%q): %v", raw, err)
		http.Redirect(w, r, "/admin/settings?err="+platform.URLMsg("添加分类失败，请稍后重试"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/settings?msg="+platform.URLMsg(fmt.Sprintf("已添加分类「%s」", raw)), http.StatusSeeOther)
}

func (s *Service) DeleteCategory(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单格式不正确", http.StatusBadRequest)
		return
	}
	raw := strings.TrimSpace(r.FormValue("name"))
	if _, err := s.store.DeleteBeerCategory(r.Context(), raw); err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			http.Redirect(w, r, "/admin/settings?err="+platform.URLMsg(msg), http.StatusSeeOther)
			return
		}
		log.Printf("delete beer category (%q): %v", raw, err)
		http.Redirect(w, r, "/admin/settings?err="+platform.URLMsg("删除分类失败，请稍后重试"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/settings?msg="+platform.URLMsg(fmt.Sprintf("已从预设列表移除分类「%s」", raw)), http.StatusSeeOther)
}

func (s *Service) ResetCategories(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.ResetBeerCategories(r.Context()); err != nil {
		log.Printf("reset beer categories: %v", err)
		http.Redirect(w, r, "/admin/settings?err="+platform.URLMsg("恢复默认分类失败，请稍后重试"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/settings?msg="+platform.URLMsg("已恢复默认酒款分类"), http.StatusSeeOther)
}

func (s *Service) TestWebhook(w http.ResponseWriter, r *http.Request) {
	actor := platform.OperationActor(r.Context())
	if err := s.webhooks.SendTest(r.Context(), actor); err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			http.Redirect(w, r, "/admin/settings?err="+platform.URLMsg(msg), http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/admin/settings?err="+platform.URLMsg("发送测试 Webhook 失败"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/settings?msg="+platform.URLMsg("测试 Webhook 发送成功（HTTP 2xx）"), http.StatusSeeOther)
}
