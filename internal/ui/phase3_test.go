package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestStaticHandlerBlocksDirectoriesAndSupportsETag304(t *testing.T) {
	staticFS := fstest.MapFS{
		"css/a.css":    {Data: []byte("body{color:#111;}")},
		"js/app.js":    {Data: []byte("console.log('ok');")},
		"sub/note.txt": {Data: []byte("hello")},
	}
	manifest, err := NewAssetManifest(staticFS)
	if err != nil {
		t.Fatalf("NewAssetManifest: %v", err)
	}
	h := manifest.Handler()

	for _, dirPath := range []string{"/static/", "/static/sub/", "/static/css/"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", dirPath, nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404 (directory listing blocked)", dirPath, rec.Code)
		}
	}

	for _, filePath := range []string{"/static/js/app.js", "/static/sub/note.txt", AdminBundlePath, "/static/css/admin.css"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", filePath, nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", filePath, rec.Code)
		}
		etag := rec.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("GET %s missing ETag header", filePath)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache", filePath, cc)
		}

		rec304 := httptest.NewRecorder()
		req304 := httptest.NewRequest("GET", filePath, nil)
		req304.Header.Set("If-None-Match", etag)
		h.ServeHTTP(rec304, req304)
		if rec304.Code != http.StatusNotModified {
			t.Errorf("GET %s with If-None-Match=%s status = %d, want 304", filePath, etag, rec304.Code)
		}
	}
}
