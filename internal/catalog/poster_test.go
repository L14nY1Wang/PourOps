package catalog

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type posterRoundTripFunc func(*http.Request) (*http.Response, error)

func (f posterRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func samplePNGBytes() []byte {
	return []byte{
		0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n',
		0x00, 0x00, 0x00, 0x0D, 'I', 'H', 'D', 'R',
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89,
	}
}

func sampleJPEGBytes() []byte {
	return []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0xFF, 0xD9}
}

func sampleWebPBytes() []byte {
	return []byte{'R', 'I', 'F', 'F', 0x10, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P', 'V', 'P', '8', ' ', 0x04, 0x00, 0x00, 0x00}
}

func sampleGIFBytes() []byte {
	return []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;")
}

func TestPosterStoreDetectSaveServeAndValidation(t *testing.T) {
	store, err := NewPosterStore(filepath.Join(t.TempDir(), "posters"))
	if err != nil {
		t.Fatal(err)
	}

	// 四种常见图片格式均应按魔数准确保存为唯一文件名。
	cases := []struct {
		name     string
		data     []byte
		wantExt  string
		wantMime string
	}{
		{"png", samplePNGBytes(), ".png", "image/png"},
		{"jpg", sampleJPEGBytes(), ".jpg", "image/jpeg"},
		{"webp", sampleWebPBytes(), ".webp", "image/webp"},
		{"gif", sampleGIFBytes(), ".gif", "image/gif"},
	}
	seenURLs := make(map[string]bool)
	for _, tc := range cases {
		u1, err := store.SaveBytes(tc.data)
		if err != nil {
			t.Fatalf("SaveBytes(%s): %v", tc.name, err)
		}
		u2, err := store.SaveBytes(tc.data)
		if err != nil {
			t.Fatalf("SaveBytes(%s #2): %v", tc.name, err)
		}
		if !strings.HasPrefix(u1, PosterURLPrefix) || !strings.HasSuffix(u1, tc.wantExt) {
			t.Fatalf("SaveBytes(%s) URL=%q", tc.name, u1)
		}
		if u1 == u2 {
			t.Fatalf("两次保存必须生成唯一文件名: %q == %q", u1, u2)
		}
		if seenURLs[u1] || seenURLs[u2] {
			t.Fatal("唯一文件名发生碰撞")
		}
		seenURLs[u1] = true
		seenURLs[u2] = true

		name, raw, ok := store.Read(u1)
		if !ok || !bytes.Equal(raw, tc.data) || !strings.HasSuffix(name, tc.wantExt) {
			t.Fatalf("Read(%s) ok=%v name=%q", u1, ok, name)
		}

		req := httptest.NewRequest(http.MethodGet, u1, nil)
		req.SetPathValue("name", name)
		rec := httptest.NewRecorder()
		store.ServePoster(rec, req)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != tc.wantMime || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("ServePoster(%s) code=%d ct=%q nosniff=%q", u1, rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("X-Content-Type-Options"))
		}
		if !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") || !bytes.Equal(rec.Body.Bytes(), tc.data) {
			t.Fatalf("ServePoster(%s) cache=%q bodyLen=%d", u1, rec.Header().Get("Cache-Control"), rec.Body.Len())
		}

		if err := store.Delete(u2); err != nil {
			t.Fatalf("Delete(%s): %v", u2, err)
		}
		if store.Exists(u2) {
			t.Fatalf("删除后文件不应存在: %s", u2)
		}
	}

	// 拒绝 SVG、HTML、空数据以及超大文件。
	for _, bad := range []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)},
		{"html", []byte("<html><body>hello</body></html>")},
		{"oversize", bytes.Repeat([]byte("A"), MaxPosterBytes+1)},
	} {
		if _, err := store.SaveReader(bytes.NewReader(bad.data)); err == nil {
			t.Fatalf("SaveReader(%s) 应被拒绝", bad.name)
		}
	}

	// 拒绝路径穿越、非规范后缀与不存在文件。
	for _, badPath := range []string{
		"/posters/../etc/passwd",
		"/posters/evil.svg",
		"/posters/0123456789abcdef0123456789abcdef.exe",
		"/posters/not-hex.png",
		"/static/brand/pourops.svg",
	} {
		if IsLocalPosterURL(badPath) {
			t.Fatalf("非法本地路径 %q 不应通过校验", badPath)
		}
	}
	reqBad := httptest.NewRequest(http.MethodGet, "/posters/evil.svg", nil)
	reqBad.SetPathValue("name", "../evil.svg")
	recBad := httptest.NewRecorder()
	store.ServePoster(recBad, reqBad)
	if recBad.Code != http.StatusNotFound {
		t.Fatalf("非法文件访问应 404，实际 %d", recBad.Code)
	}
}

func TestPosterStoreRemoteFetchAndSSRFProtection(t *testing.T) {
	store, err := NewPosterStore(filepath.Join(t.TempDir(), "posters"))
	if err != nil {
		t.Fatal(err)
	}

	// 默认 SSRF 防护拦截本机回环、元数据与 CGNAT/私网地址。
	for _, blockedURL := range []string{
		"http://127.0.0.1:8080/a.png",
		"http://localhost/a.png",
		"http://169.254.169.254/latest/meta-data",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://100.64.0.1:8080/a.png",
		"http://192.168.1.10/a.png",
		"ftp://example.com/a.png",
	} {
		if _, err := store.SaveFromURL(context.Background(), blockedURL); err == nil {
			t.Fatalf("默认存储应对受限地址 %q 报错", blockedURL)
		}
	}

	// 验证重定向策略同样阻断内网目标与过多跳数。
	defaultClient := store.defaultHTTPClient()
	redirTarget, _ := url.Parse("http://100.64.0.1/secret.png")
	if err := defaultClient.CheckRedirect(&http.Request{URL: redirTarget}, make([]*http.Request, 1)); err == nil {
		t.Fatal("重定向至 CGNAT 地址应被阻断")
	}
	okTarget, _ := url.Parse("https://cdn.example/a.png")
	if err := defaultClient.CheckRedirect(&http.Request{URL: okTarget}, make([]*http.Request, maxPosterRedirects)); err == nil {
		t.Fatal("超过最大跳数应被阻断")
	}

	client := &http.Client{Transport: posterRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/ok.png":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(bytes.NewReader(samplePNGBytes())),
			}, nil
		case "/fake.png":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(strings.NewReader("not a real png")),
			}, nil
		default:
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader("not found")),
			}, nil
		}
	})}
	store.SetHTTPClientForTest(client, false)
	savedURL, err := store.SaveFromURL(context.Background(), "https://cdn.example/ok.png")
	if err != nil {
		t.Fatalf("SaveFromURL(ok.png): %v", err)
	}
	if _, data, ok := store.Read(savedURL); !ok || !bytes.Equal(data, samplePNGBytes()) {
		t.Fatalf("本地落盘内容不对: ok=%v len=%d", ok, len(data))
	}
	if _, err := store.SaveFromURL(context.Background(), "https://cdn.example/fake.png"); err == nil {
		t.Fatal("伪造 image/png 头但非 PNG 魔数应拒绝")
	}
	if _, err := store.SaveFromURL(context.Background(), "https://cdn.example/404.png"); err == nil {
		t.Fatal("404 应报错")
	}
	// 确认 tmp 文件未残留。
	entries, err := os.ReadDir(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".poster-") {
			t.Fatalf("发现未清理临时文件: %s", e.Name())
		}
	}
}
