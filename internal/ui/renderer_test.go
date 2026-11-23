package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestRendererLayoutAndAssetHelper(t *testing.T) {
	staticFS := fstest.MapFS{
		"js/beer-lookup.js": {Data: []byte("alert(1)")},
	}
	assets, err := NewAssetManifest(staticFS)
	if err != nil {
		t.Fatal(err)
	}

	tmplFS := fstest.MapFS{
		AdminLayoutFile:             {Data: []byte(`{{define "layout"}}<title>{{.Title}}-{{.Brand}}</title><script src="{{asset "/static/js/beer-lookup.js"}}"></script>{{template "content" .}}{{end}}`)},
		"templates/admin/page.html": {Data: []byte(`{{define "content"}}user={{if .Me}}{{.Me.Username}}({{.Me.RoleName}}){{end}}|items={{.ItemsCount}}{{end}}`)},
	}

	r, err := NewRenderer(tmplFS, assets, func() string { return "北岸精酿" }, "admin/page.html")
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	r.SetCurrentUserFn(func(*http.Request) *UserView {
		return &UserView{ID: 1, Username: "boss", Role: RoleBoss, Active: true}
	})

	type pageModel struct {
		Layout
		ItemsCount int
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Render(rec, req, "admin/page.html", &pageModel{
		Layout:     Layout{Title: "测试页"},
		ItemsCount: 7,
	})
	body := rec.Body.String()
	if !strings.Contains(body, "<title>测试页-北岸精酿</title>") ||
		!strings.Contains(body, `/static/js/beer-lookup.js?v=`) ||
		!strings.Contains(body, "user=boss(BOSS)|items=7") {
		t.Fatalf("unexpected rendered body: %s", body)
	}
}
