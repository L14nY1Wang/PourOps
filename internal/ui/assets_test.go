package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestAssetManifestBundleAndHashing(t *testing.T) {
	fsys := fstest.MapFS{
		"js/beer-lookup.js": {Data: []byte("console.log('lookup');")},
		"css/01-tokens.css": {Data: []byte(":root { --a: 1; }")},
		"css/02-layout.css": {Data: []byte(".box { color: red; }")},
		"brand/pourops.svg": {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)},
	}

	m, err := NewAssetManifest(fsys)
	if err != nil {
		t.Fatalf("NewAssetManifest: %v", err)
	}

	jsURL := m.URL("/static/js/beer-lookup.js")
	if !strings.HasPrefix(jsURL, "/static/js/beer-lookup.js?v=") || len(jsURL) != len("/static/js/beer-lookup.js?v=")+8 {
		t.Fatalf("unexpected versioned js URL: %q", jsURL)
	}

	bundleURL := m.URL(AdminBundlePath)
	aliasURL := m.URL("/static/css/admin.css")
	if !strings.HasPrefix(bundleURL, AdminBundlePath+"?v=") || aliasURL != bundleURL {
		t.Fatalf("bundle=%q alias=%q", bundleURL, aliasURL)
	}

	h := m.Handler()

	// 请求 bundle（携带准确 ?v= 时应强缓存且拼接两份 CSS）
	req := httptest.NewRequest(http.MethodGet, bundleURL, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET bundle status = %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Result().Body)
	if !strings.Contains(string(body), "--a: 1") || !strings.Contains(string(body), ".box") {
		t.Fatalf("bundle missing css rules: %s", body)
	}
	if !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("versioned request should be immutable, got %q", rec.Header().Get("Cache-Control"))
	}

	// 兼容路径 /static/css/admin.css 无 ?v= 输出且返回 no-cache
	recAlias := httptest.NewRecorder()
	h.ServeHTTP(recAlias, httptest.NewRequest(http.MethodGet, "/static/css/admin.css", nil))
	if recAlias.Code != http.StatusOK || !strings.Contains(recAlias.Body.String(), ".box") {
		t.Fatalf("alias route failed: %d", recAlias.Code)
	}
	if recAlias.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("unversioned bundle Cache-Control = %q", recAlias.Header().Get("Cache-Control"))
	}
}
