package settings

import (
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
}

// Service 提供 BOSS 专属门店系统参数与 Webhook 出站通知管理界面。
type Service struct {
	store    *SettingsStore
	webhooks *event.WebhookDispatcher
	renderer *ui.Renderer
}

func NewService(store *SettingsStore, webhooks *event.WebhookDispatcher, renderer *ui.Renderer) *Service {
	return &Service{store: store, webhooks: webhooks, renderer: renderer}
}

func (s *Service) RegisterRoutes(mux *http.ServeMux, guards ui.Guards) {
	mux.HandleFunc("GET /admin/settings", guards.Boss(s.Page))
	mux.HandleFunc("POST /admin/settings", guards.Boss(s.Save))
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
	})
}

func (s *Service) Save(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单格式不正确", http.StatusBadRequest)
		return
	}
	existing := s.store.Get()

	next := StoreSettings{
		Brand:         strings.TrimSpace(r.FormValue("brand")),
		WebhookURL:    strings.TrimSpace(r.FormValue("webhook_url")),
		WebhookSecret: existing.WebhookSecret,
		WebhookEvents: r.Form["webhook_events"],
	}
	if r.FormValue("clear_webhook_secret") != "" {
		next.WebhookSecret = ""
	} else if sec := strings.TrimSpace(r.FormValue("webhook_secret")); sec != "" {
		next.WebhookSecret = sec
	}

	sellPtr, sellErr := platform.ParseStrictInt(r.FormValue("default_sellable_pct"), "默认可售比例", MinSellablePct, MaxSellablePct)
	marginPtr, marginErr := platform.ParseStrictInt(r.FormValue("default_margin_pct"), "默认目标毛利率", MinMarginPct, MaxMarginPct)

	if sellPtr != nil {
		next.DefaultSellablePct = *sellPtr
	}
	if marginPtr != nil {
		next.DefaultMarginPct = *marginPtr
	}

	renderFail := func(errMsg string) {
		w.WriteHeader(http.StatusBadRequest)
		s.renderer.Render(w, r, "admin/settings.html", &SettingsPage{
			Layout:         ui.Layout{Title: "系统设置", Nav: "settings", Err: errMsg},
			Settings:       next,
			EventOptions:   event.WebhookEventOptions,
			HasSecret:      existing.WebhookSecret != "",
			SelectedEvents: buildSelectedMap(next.WebhookEvents),
		})
	}

	if sellErr != nil {
		renderFail(sellErr.Error())
		return
	}
	if sellPtr == nil {
		renderFail("请填写默认可售比例（50%–100%）")
		return
	}
	if marginErr != nil {
		renderFail(marginErr.Error())
		return
	}
	if marginPtr == nil {
		renderFail("请填写默认目标毛利率（10%–85%）")
		return
	}

	if _, err := s.store.Save(r.Context(), next); err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			renderFail(msg)
			return
		}
		renderFail("保存系统设置失败，请稍后重试")
		return
	}

	http.Redirect(w, r, "/admin/settings?msg="+platform.URLMsg("系统设置已保存并实时生效"), http.StatusSeeOther)
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
