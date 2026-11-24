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

	// 1. 合法保存
	form := url.Values{
		"brand":                {"北岸精酿"},
		"default_sellable_pct": {"85"},
		"default_margin_pct":   {"55"},
		"webhook_url":          {"https://example.com/hook"},
		"webhook_secret":       {"top-secret"},
		"webhook_events":       {"order.paid", "keg.finished"},
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	svc.Save(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("Save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cfg := store.Get()
	if cfg.Brand != "北岸精酿" || cfg.DefaultSellablePct != 85 || cfg.DefaultMarginPct != 55 || cfg.WebhookSecret != "top-secret" {
		t.Fatalf("saved cfg = %+v", cfg)
	}

	// 2. 不重填 secret 时保留现有密钥
	formKeep := url.Values{
		"brand":                {"北岸精酿"},
		"default_sellable_pct": {"82"},
		"default_margin_pct":   {"52"},
		"webhook_url":          {"https://example.com/hook"},
		"webhook_events":       {"order.paid"},
	}
	reqKeep := httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(formKeep.Encode()))
	reqKeep.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recKeep := httptest.NewRecorder()
	svc.Save(recKeep, reqKeep)
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
	reqBad := httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(badForm.Encode()))
	reqBad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recBad := httptest.NewRecorder()
	svc.Save(recBad, reqBad)
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
	if recAdd.Code != http.StatusSeeOther || strings.Contains(recAdd.Header().Get("Location"), "err=") {
		t.Fatalf("AddCategory failed: code=%d loc=%q", recAdd.Code, recAdd.Header().Get("Location"))
	}
	if cats := store.BeerCategories(); cats[len(cats)-1] != "赛松" {
		t.Fatalf("expected 赛松 appended, got %v", cats)
	}

	dupReq := httptest.NewRequest(http.MethodPost, "/admin/settings/categories/add", strings.NewReader("name=ipa"))
	dupReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recDup := httptest.NewRecorder()
	svc.AddCategory(recDup, dupReq)
	if !strings.Contains(recDup.Header().Get("Location"), "err=") {
		t.Fatalf("duplicate category ipa should redirect with err, got %q", recDup.Header().Get("Location"))
	}

	// 2. 保存品牌/Webhook 设置不覆盖现有分类
	saveForm := url.Values{
		"brand":                {"北岸精酿"},
		"default_sellable_pct": {"80"},
		"default_margin_pct":   {"50"},
	}
	saveReq := httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(saveForm.Encode()))
	saveReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recSave := httptest.NewRecorder()
	svc.Save(recSave, saveReq)
	if recSave.Code != http.StatusSeeOther {
		t.Fatalf("Save status = %d", recSave.Code)
	}
	if cats := store.BeerCategories(); cats[len(cats)-1] != "赛松" {
		t.Fatalf("Save should preserve custom category 赛松, got %v", cats)
	}

	// 3. 删除分类与至少保留 1 个约束
	for _, cat := range append([]string(nil), store.BeerCategories()[:len(store.BeerCategories())-1]...) {
		delReq := httptest.NewRequest(http.MethodPost, "/admin/settings/categories/delete", strings.NewReader(url.Values{"name": {cat}}.Encode()))
		delReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recDel := httptest.NewRecorder()
		svc.DeleteCategory(recDel, delReq)
		if strings.Contains(recDel.Header().Get("Location"), "err=") {
			t.Fatalf("DeleteCategory(%q) unexpected err: %q", cat, recDel.Header().Get("Location"))
		}
	}
	lastReq := httptest.NewRequest(http.MethodPost, "/admin/settings/categories/delete", strings.NewReader(url.Values{"name": {store.BeerCategories()[0]}}.Encode()))
	lastReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recLast := httptest.NewRecorder()
	svc.DeleteCategory(recLast, lastReq)
	if !strings.Contains(recLast.Header().Get("Location"), "err=") || len(store.BeerCategories()) != 1 {
		t.Fatalf("deleting last category must fail cleanly, loc=%q cats=%v", recLast.Header().Get("Location"), store.BeerCategories())
	}

	// 4. 恢复默认分类
	recReset := httptest.NewRecorder()
	svc.ResetCategories(recReset, httptest.NewRequest(http.MethodPost, "/admin/settings/categories/reset", nil))
	if len(store.BeerCategories()) != len(platform.DefaultBeerCategories) {
		t.Fatalf("ResetCategories len = %d, want %d", len(store.BeerCategories()), len(platform.DefaultBeerCategories))
	}
}
