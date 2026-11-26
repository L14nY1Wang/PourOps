package settings

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"pourops/internal/event"
	"pourops/internal/platform"
	"pourops/internal/ui"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testRenderer() *ui.Renderer {
	fsys := fstest.MapFS{
		ui.AdminLayoutFile:              {Data: []byte(`{{define "layout"}}{{.Err}}|{{.Msg}}|{{template "content" .}}{{end}}`)},
		"templates/admin/settings.html": {Data: []byte(`{{define "content"}}brand={{.Settings.Brand}}|sell={{.Settings.DefaultSellablePct}}|margin={{.Settings.DefaultMarginPct}}|url={{.Settings.WebhookURL}}{{end}}`)},
	}
	return ui.MustRenderer(fsys, nil, func() string { return "PourOps" }, "admin/settings.html")
}

func TestSettingsHTTPHandlerSaveAndTestWebhook(t *testing.T) {
	store := NewSettingsStore(nil, "PourOps")
	var gotEvent string
	var gotSig string
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		gotEvent = r.Header.Get("X-Bar-Event")
		gotSig = r.Header.Get("X-Bar-Signature")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     make(http.Header),
		}, nil
	})}
	webhooks := event.NewWebhookDispatcher(store, client)
	defer webhooks.Close(context.Background())

	svc := NewService(store, webhooks, testRenderer())

	// 1. 合法保存常规与 Webhook 设置
	genInit := url.Values{
		"brand":                {"北岸精酿"},
		"default_sellable_pct": {"85"},
		"default_margin_pct":   {"55"},
	}
	reqGenInit := httptest.NewRequest(http.MethodPost, "/admin/settings/general", strings.NewReader(genInit.Encode()))
	reqGenInit.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recGenInit := httptest.NewRecorder()
	svc.SaveGeneral(recGenInit, reqGenInit)
	if recGenInit.Code != http.StatusSeeOther {
		t.Fatalf("SaveGeneral status = %d, body = %s", recGenInit.Code, recGenInit.Body.String())
	}

	whInit := url.Values{
		"webhook_url":    {"https://example.com/hook"},
		"webhook_secret": {"top-secret"},
		"webhook_events": {"order.paid", "keg.finished"},
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/settings/webhook", strings.NewReader(whInit.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	svc.SaveWebhook(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("SaveWebhook status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cfg := store.Get()
	if cfg.Brand != "北岸精酿" || cfg.DefaultSellablePct != 85 || cfg.DefaultMarginPct != 55 || cfg.WebhookSecret != "top-secret" {
		t.Fatalf("saved cfg = %+v", cfg)
	}

	// 2. 不重填 secret 时保留现有密钥
	formKeep := url.Values{
		"webhook_url":    {"https://example.com/hook"},
		"webhook_events": {"order.paid"},
	}
	reqKeep := httptest.NewRequest(http.MethodPost, "/admin/settings/webhook", strings.NewReader(formKeep.Encode()))
	reqKeep.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recKeep := httptest.NewRecorder()
	svc.SaveWebhook(recKeep, reqKeep)
	if store.Get().WebhookSecret != "top-secret" {
		t.Fatalf("blank secret should keep existing secret, got %q", store.Get().WebhookSecret)
	}

	// 3. 测试发送 Webhook
	recTest := httptest.NewRecorder()
	svc.TestWebhook(recTest, httptest.NewRequest(http.MethodPost, "/admin/settings/webhook-test", nil))
	if recTest.Code != http.StatusSeeOther || gotEvent != string(event.EventWebhookTest) || !strings.HasPrefix(gotSig, "sha256=") {
		t.Fatalf("TestWebhook code=%d event=%q sig=%q loc=%s", recTest.Code, gotEvent, gotSig, recTest.Header().Get("Location"))
	}

	// 4. 非法比例拒绝并返回 400
	badForm := url.Values{
		"brand":                {"北岸精酿"},
		"default_sellable_pct": {"30"},
		"default_margin_pct":   {"50"},
	}
	reqBad := httptest.NewRequest(http.MethodPost, "/admin/settings/general", strings.NewReader(badForm.Encode()))
	reqBad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recBad := httptest.NewRecorder()
	svc.SaveGeneral(recBad, reqBad)
	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("bad sellable status = %d", recBad.Code)
	}
}

func TestSettingsHTTPCategoryManagement(t *testing.T) {
	store := NewSettingsStore(nil, "PourOps")
	webhooks := event.NewWebhookDispatcher(store, nil)
	defer webhooks.Close(context.Background())
	svc := NewService(store, webhooks, testRenderer())

	// 1. 添加自定义分类（含大小写不敏感去重）
	addReq := httptest.NewRequest(http.MethodPost, "/admin/settings/categories/add", strings.NewReader("name=赛松"))
	addReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recAdd := httptest.NewRecorder()
	svc.AddCategory(recAdd, addReq)
	if _, errMsg := platform.ReadFlashCookies(recAdd.Result().Cookies()); recAdd.Code != http.StatusSeeOther || errMsg != "" {
		t.Fatalf("AddCategory failed: code=%d loc=%q", recAdd.Code, recAdd.Header().Get("Location"))
	}
	if cats := store.BeerCategories(); cats[len(cats)-1] != "赛松" {
		t.Fatalf("expected 赛松 appended, got %v", cats)
	}

	dupReq := httptest.NewRequest(http.MethodPost, "/admin/settings/categories/add", strings.NewReader("name=ipa"))
	dupReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recDup := httptest.NewRecorder()
	svc.AddCategory(recDup, dupReq)
	if _, errMsg := platform.ReadFlashCookies(recDup.Result().Cookies()); errMsg == "" {
		t.Fatalf("duplicate category ipa should redirect with err, got %q", recDup.Header().Get("Location"))
	}

	// 2. 保存品牌设置不覆盖现有分类
	saveForm := url.Values{
		"brand":                {"北岸精酿"},
		"default_sellable_pct": {"80"},
		"default_margin_pct":   {"50"},
	}
	saveReq := httptest.NewRequest(http.MethodPost, "/admin/settings/general", strings.NewReader(saveForm.Encode()))
	saveReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recSave := httptest.NewRecorder()
	svc.SaveGeneral(recSave, saveReq)
	if recSave.Code != http.StatusSeeOther {
		t.Fatalf("SaveGeneral status = %d", recSave.Code)
	}
	if cats := store.BeerCategories(); cats[len(cats)-1] != "赛松" {
		t.Fatalf("SaveGeneral should preserve custom category 赛松, got %v", cats)
	}

	// 3. 删除分类与至少保留 1 个约束
	for _, cat := range append([]string(nil), store.BeerCategories()[:len(store.BeerCategories())-1]...) {
		delReq := httptest.NewRequest(http.MethodPost, "/admin/settings/categories/delete", strings.NewReader(url.Values{"name": {cat}}.Encode()))
		delReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recDel := httptest.NewRecorder()
		svc.DeleteCategory(recDel, delReq)
		if _, errMsg := platform.ReadFlashCookies(recDel.Result().Cookies()); errMsg != "" {
			t.Fatalf("DeleteCategory(%q) unexpected err: %q", cat, recDel.Header().Get("Location"))
		}
	}
	lastReq := httptest.NewRequest(http.MethodPost, "/admin/settings/categories/delete", strings.NewReader(url.Values{"name": {store.BeerCategories()[0]}}.Encode()))
	lastReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recLast := httptest.NewRecorder()
	svc.DeleteCategory(recLast, lastReq)
	if _, errMsg := platform.ReadFlashCookies(recLast.Result().Cookies()); errMsg == "" || len(store.BeerCategories()) != 1 {
		t.Fatalf("deleting last category must fail cleanly, loc=%q cats=%v", recLast.Header().Get("Location"), store.BeerCategories())
	}

	// 4. 恢复默认分类
	recReset := httptest.NewRecorder()
	svc.ResetCategories(recReset, httptest.NewRequest(http.MethodPost, "/admin/settings/categories/reset", nil))
	if len(store.BeerCategories()) != len(platform.DefaultBeerCategories) {
		t.Fatalf("ResetCategories len = %d, want %d", len(store.BeerCategories()), len(platform.DefaultBeerCategories))
	}
}

func TestSettingsHTTPSaveGeneralAndWebhookIsolated(t *testing.T) {
	store := NewSettingsStore(nil, "PourOps")
	webhooks := event.NewWebhookDispatcher(store, nil)
	defer webhooks.Close(context.Background())
	svc := NewService(store, webhooks, testRenderer())

	// 先预设自定义分类与 Webhook，验证分模块保存互不覆盖。
	if _, err := store.AddBeerCategory(context.Background(), "赛松"); err != nil {
		t.Fatalf("AddBeerCategory: %v", err)
	}
	if _, err := store.SaveWebhook(context.Background(), "https://example.com/init", "init-secret", false, []string{string(event.EventOrderPaid)}); err != nil {
		t.Fatalf("preset SaveWebhook: %v", err)
	}

	// 1. SaveGeneral 仅更新品牌与默认核算参数，保留 Webhook 与自定义分类
	genForm := url.Values{
		"brand":                {"北岸精酿"},
		"default_sellable_pct": {"86"},
		"default_margin_pct":   {"58"},
	}
	reqGen := httptest.NewRequest(http.MethodPost, "/admin/settings/general", strings.NewReader(genForm.Encode()))
	reqGen.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recGen := httptest.NewRecorder()
	svc.SaveGeneral(recGen, reqGen)
	if _, errMsg := platform.ReadFlashCookies(recGen.Result().Cookies()); recGen.Code != http.StatusSeeOther || errMsg != "" {
		t.Fatalf("SaveGeneral status=%d loc=%q", recGen.Code, recGen.Header().Get("Location"))
	}
	cfg := store.Get()
	if cfg.Brand != "北岸精酿" || cfg.DefaultSellablePct != 86 || cfg.DefaultMarginPct != 58 {
		t.Fatalf("SaveGeneral updated fields = %+v", cfg)
	}
	if cfg.WebhookURL != "https://example.com/init" || cfg.WebhookSecret != "init-secret" || len(cfg.WebhookEvents) != 1 || cfg.WebhookEvents[0] != string(event.EventOrderPaid) {
		t.Fatalf("SaveGeneral should preserve webhook settings, got %+v", cfg)
	}
	if cfg.BeerCategories[len(cfg.BeerCategories)-1] != "赛松" {
		t.Fatalf("SaveGeneral should preserve custom categories, got %v", cfg.BeerCategories)
	}

	// 2. SaveGeneral 非法比例返回 400 且不破坏已有配置
	badGenForm := url.Values{
		"brand":                {"误改店名"},
		"default_sellable_pct": {"25"},
		"default_margin_pct":   {"50"},
	}
	reqBadGen := httptest.NewRequest(http.MethodPost, "/admin/settings/general", strings.NewReader(badGenForm.Encode()))
	reqBadGen.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recBadGen := httptest.NewRecorder()
	svc.SaveGeneral(recBadGen, reqBadGen)
	if recBadGen.Code != http.StatusBadRequest {
		t.Fatalf("bad SaveGeneral status=%d, want 400", recBadGen.Code)
	}
	if got := store.Get(); got.Brand != "北岸精酿" || got.WebhookURL != "https://example.com/init" {
		t.Fatalf("failed SaveGeneral should not mutate store, got %+v", got)
	}

	// 3. SaveWebhook 仅更新 Webhook，留空密钥保持不变，且不改门店/核算/分类
	whForm := url.Values{
		"webhook_url":    {"https://example.com/v2"},
		"webhook_events": {string(event.EventKegFinished), string(event.EventRetailLowStock)},
	}
	reqWH := httptest.NewRequest(http.MethodPost, "/admin/settings/webhook", strings.NewReader(whForm.Encode()))
	reqWH.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recWH := httptest.NewRecorder()
	svc.SaveWebhook(recWH, reqWH)
	if _, errMsg := platform.ReadFlashCookies(recWH.Result().Cookies()); recWH.Code != http.StatusSeeOther || errMsg != "" {
		t.Fatalf("SaveWebhook status=%d loc=%q", recWH.Code, recWH.Header().Get("Location"))
	}
	cfg = store.Get()
	if cfg.WebhookURL != "https://example.com/v2" || cfg.WebhookSecret != "init-secret" || len(cfg.WebhookEvents) != 2 {
		t.Fatalf("SaveWebhook got %+v", cfg)
	}
	if cfg.Brand != "北岸精酿" || cfg.DefaultSellablePct != 86 || cfg.DefaultMarginPct != 58 {
		t.Fatalf("SaveWebhook should preserve brand & pricing, got %+v", cfg)
	}

	// 4. SaveWebhook 清空密钥生效
	whClearForm := url.Values{
		"webhook_url":          {"https://example.com/v2"},
		"clear_webhook_secret": {"1"},
		"webhook_events":       {string(event.EventKegFinished)},
	}
	reqClear := httptest.NewRequest(http.MethodPost, "/admin/settings/webhook", strings.NewReader(whClearForm.Encode()))
	reqClear.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recClear := httptest.NewRecorder()
	svc.SaveWebhook(recClear, reqClear)
	if recClear.Code != http.StatusSeeOther {
		t.Fatalf("SaveWebhook clear status=%d", recClear.Code)
	}
	if store.Get().WebhookSecret != "" {
		t.Fatalf("expected cleared webhook secret, got %q", store.Get().WebhookSecret)
	}

	// 5. SaveWebhook 拦截 SSRF 地址并返回 400，保留原设置
	whBadForm := url.Values{
		"webhook_url": {"http://169.254.169.254/latest/meta-data"},
	}
	reqBadWH := httptest.NewRequest(http.MethodPost, "/admin/settings/webhook", strings.NewReader(whBadForm.Encode()))
	reqBadWH.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recBadWH := httptest.NewRecorder()
	svc.SaveWebhook(recBadWH, reqBadWH)
	if recBadWH.Code != http.StatusBadRequest {
		t.Fatalf("bad SaveWebhook status=%d, want 400", recBadWH.Code)
	}
	if got := store.Get(); got.WebhookURL != "https://example.com/v2" || got.Brand != "北岸精酿" {
		t.Fatalf("failed SaveWebhook should preserve existing state, got %+v", got)
	}
}
