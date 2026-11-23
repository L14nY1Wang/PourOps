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
