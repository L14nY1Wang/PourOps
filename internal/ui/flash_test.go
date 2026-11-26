package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"pourops/internal/platform"
)

func TestRendererPopFlashForAdminAndShell(t *testing.T) {
	assets, err := NewAssetManifest(fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	tmplFS := fstest.MapFS{
		AdminLayoutFile:             {Data: []byte(`{{define "layout"}}ADMIN:{{.Msg}}|{{.Err}}{{template "content" .}}{{end}}`)},
		"templates/admin/page.html": {Data: []byte(`{{define "content"}}!{{end}}`)},
		"templates/pos/pos.html":    {Data: []byte(`{{define "pos"}}SHELL:{{.Msg}}|{{.Err}}{{end}}`)},
	}
	r, err := NewRenderer(tmplFS, assets, func() string { return "PourOps" }, "admin/page.html", "pos/pos.html")
	if err != nil {
		t.Fatal(err)
	}

	// Test Admin Render pops flash cookie
	postRec := httptest.NewRecorder()
	postReq := httptest.NewRequest(http.MethodPost, "/admin/beers", nil)
	platform.RedirectMsg(postRec, postReq, "/admin/beers", "保存成功")

	getReq := httptest.NewRequest(http.MethodGet, "/admin/beers", nil)
	for _, c := range postRec.Result().Cookies() {
		getReq.AddCookie(c)
	}
	getRec := httptest.NewRecorder()
	r.Render(getRec, getReq, "admin/page.html", &Layout{})
	if !strings.Contains(getRec.Body.String(), "ADMIN:保存成功|!") {
		t.Fatalf("admin render missing flash message: %q", getRec.Body.String())
	}

	// Test RenderShell pops flash cookie too
	shellPostRec := httptest.NewRecorder()
	platform.RedirectErr(shellPostRec, postReq, "/pos/", "库存不足")
	shellReq := httptest.NewRequest(http.MethodGet, "/pos/", nil)
	for _, c := range shellPostRec.Result().Cookies() {
		shellReq.AddCookie(c)
	}
	shellRec := httptest.NewRecorder()
	r.RenderShell(shellRec, shellReq, "pos/pos.html", "pos", &Layout{})
	if !strings.Contains(shellRec.Body.String(), "SHELL:|库存不足") {
		t.Fatalf("shell render missing flash err: %q", shellRec.Body.String())
	}
}

func TestRendererStatusSetsContentTypeAndClearsFlash(t *testing.T) {
	assets, err := NewAssetManifest(fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	tmplFS := fstest.MapFS{
		AdminLayoutFile:             {Data: []byte(`{{define "layout"}}STATUS:{{.Err}}{{template "content" .}}{{end}}`)},
		"templates/admin/page.html": {Data: []byte(`{{define "content"}}ok{{end}}`)},
	}
	r, err := NewRenderer(tmplFS, assets, func() string { return "PourOps" }, "admin/page.html")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/login", nil)
	req.AddCookie(&http.Cookie{Name: platform.FlashCookieName, Value: "stale"})
	rec := httptest.NewRecorder()
	r.RenderStatus(rec, req, http.StatusTooManyRequests, "admin/page.html", &Layout{Err: "尝试太频繁"})

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == platform.FlashCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("RenderStatus must clear stale flash cookie before WriteHeader")
	}
}
