package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pourops/internal/platform"
)

func readPosterForTest(t *testing.T, store *PosterStore, posterURL string) (string, []byte, bool) {
	t.Helper()
	name, f, _, ok := store.Open(posterURL)
	if !ok {
		return "", nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("readPosterForTest(%s): %v", posterURL, err)
	}
	return name, data, true
}

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
	// 1×1 基线 JPEG：SOI + APP0(JFIF) + SOF0(1×1) + EOI，可被 image.DecodeConfig 读取尺寸。
	return []byte{
		0xFF, 0xD8,
		0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00,
		0xFF, 0xC0, 0x00, 0x11, 0x08, 0x00, 0x01, 0x00, 0x01, 0x03, 0x01, 0x11, 0x00, 0x02, 0x11, 0x01, 0x03, 0x11, 0x01,
		0xFF, 0xD9,
	}
}

func sampleWebPBytes() []byte {
	// 1×1 无损 WebP：RIFF/WEBP + VP8L(5 字节载荷，0x2f 签名 + 14 位宽高各=1)。
	return []byte{'R', 'I', 'F', 'F', 0x12, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P',
		'V', 'P', '8', 'L', 0x05, 0x00, 0x00, 0x00, 0x2f, 0x00, 0x00, 0x00, 0x00, 0x00}
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

		name, raw, ok := readPosterForTest(t, store, u1)
		if !ok || !bytes.Equal(raw, tc.data) || !strings.HasSuffix(name, tc.wantExt) {
			t.Fatalf("Open(%s) ok=%v name=%q", u1, ok, name)
		}

		req := httptest.NewRequest(http.MethodGet, u1, nil)
		req.SetPathValue("name", name)
		rec := httptest.NewRecorder()
		store.ServePoster(rec, req)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != tc.wantMime || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("ServePoster(%s) code=%d ct=%q nosniff=%q", u1, rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("X-Content-Type-Options"))
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'none'; sandbox" {
			t.Fatalf("ServePoster(%s) 缺少 CSP 沙箱头: %q", u1, got)
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
	if _, data, ok := readPosterForTest(t, store, savedURL); !ok || !bytes.Equal(data, samplePNGBytes()) {
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

func pngWithSize(width, height uint32) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], width)
	binary.BigEndian.PutUint32(ihdr[4:], height)
	ihdr[8], ihdr[9], ihdr[10], ihdr[11], ihdr[12] = 8, 6, 0, 0, 0

	var b bytes.Buffer
	b.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'})
	chunk := append([]byte("IHDR"), ihdr...)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(ihdr)))
	b.Write(chunk)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

func webpVP8XBytes(width, height int) []byte {
	body := make([]byte, 10)
	put24 := func(dst []byte, v int) {
		dst[0], dst[1], dst[2] = byte(v), byte(v>>8), byte(v>>16)
	}
	put24(body[4:7], width-1)
	put24(body[7:10], height-1)

	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(4+8+len(body)))
	b.WriteString("WEBP")
	b.WriteString("VP8X")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(body)))
	b.Write(body)
	return b.Bytes()
}

func webpVP8Bytes(width, height int) []byte {
	body := make([]byte, 10)
	body[3], body[4], body[5] = 0x9d, 0x01, 0x2a
	binary.LittleEndian.PutUint16(body[6:8], uint16(width)&0x3FFF)
	binary.LittleEndian.PutUint16(body[8:10], uint16(height)&0x3FFF)

	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(4+8+len(body)))
	b.WriteString("WEBP")
	b.WriteString("VP8 ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(body)))
	b.Write(body)
	return b.Bytes()
}

func webpJunkChunkBombBytes() []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(64))
	b.WriteString("WEBP")
	b.WriteString("JUNK")
	_ = binary.Write(&b, binary.LittleEndian, uint32(0xffffffff))
	b.WriteString("VP8 ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(10))
	b.Write([]byte{0, 0, 0, 0x9d, 0x01, 0x2a, 10, 0, 10, 0})
	return b.Bytes()
}

func TestPosterSSRFBlockedRanges(t *testing.T) {
	for _, raw := range []string{
		"::1", "::ffff:127.0.0.1", "::127.0.0.1",
		"100.64.0.1", "100.100.100.200",
		"64:ff9b::7f00:1", "64:ff9b:1::1",
		"198.18.0.1", "192.0.2.1", "198.51.100.1", "203.0.113.1",
		"240.0.0.1", "255.255.255.255", "0.0.0.0",
		"169.254.169.254", "fd00:ec2::254", "fe80::1",
	} {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("测试地址 %q 解析失败: %v", raw, err)
		}
		if !platform.IsBlockedOutboundIP(net.IP(addr.AsSlice()), false) {
			t.Fatalf("%s 应被 SSRF 策略封禁", raw)
		}
	}
	for _, raw := range []string{"fd00:ec2::254", "169.254.169.254", "100.100.100.200"} {
		addr := netip.MustParseAddr(raw)
		if !platform.IsBlockedOutboundIP(net.IP(addr.AsSlice()), true) {
			t.Fatalf("allowPrivateHosts=true 时云元数据 %s 仍应被封禁", raw)
		}
	}
	for _, raw := range []string{"127.0.0.1", "::1"} {
		addr := netip.MustParseAddr(raw)
		if platform.IsBlockedOutboundIP(net.IP(addr.AsSlice()), true) {
			t.Fatalf("allowPrivateHosts=true 时本地回环 %s 应放行", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("测试地址 %q 解析失败: %v", raw, err)
		}
		if platform.IsBlockedOutboundIP(net.IP(addr.AsSlice()), false) {
			t.Fatalf("%s 不应被封禁", raw)
		}
	}

	store, err := NewPosterStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, rawURL := range []string{
		"http://[::1]/a.png",
		"http://[64:ff9b::7f00:1]/a.png",
		"http://[fe80::1%25eth0]/a.png",
		"http://100.100.100.200/latest/meta-data",
		"http://[fd00:ec2::254]/latest/meta-data",
		"http://localhost./a.png",
	} {
		if _, err := store.SaveFromURL(context.Background(), rawURL); err == nil {
			t.Fatalf("%s 应被 SaveFromURL SSRF 校验拒绝", rawURL)
		}
	}
	if u, err := url.Parse("https://cdn.example/a.png"); err != nil || platform.ValidateOutboundParsedURL(u, "酒标地址", false) != nil {
		t.Fatalf("公网地址不应被拒绝: %v", err)
	}
}

func TestPosterRejectsDimensionBomb(t *testing.T) {
	store, err := NewPosterStore(filepath.Join(t.TempDir(), "posters"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"png-side-overflow", pngWithSize(5000, 1)},
		{"png-bomb", pngWithSize(4097, 4096)},
		{"webp-bomb", webpVP8XBytes(5000, 5000)},
		{"webp-vp8-zero-width", webpVP8Bytes(0, 100)},
		{"webp-vp8-zero-height", webpVP8Bytes(100, 0)},
		{"webp-chunk-size-overflow", webpJunkChunkBombBytes()},
		{"truncated-jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}},
	} {
		if _, err := store.SaveBytes(tc.data); err == nil {
			t.Fatalf("%s 应被尺寸/格式校验拒绝", tc.name)
		}
	}
	if _, err := store.SaveBytes(pngWithSize(4096, 4096)); err != nil {
		t.Fatalf("4096×4096 上限内图片应可保存: %v", err)
	}
	if _, err := store.SaveBytes(webpVP8XBytes(4096, 4096)); err != nil {
		t.Fatalf("VP8X 上限内图片应可保存: %v", err)
	}
	if _, err := store.SaveBytes(webpVP8Bytes(120, 80)); err != nil {
		t.Fatalf("合法 VP8 图片应可保存: %v", err)
	}
	entries, err := os.ReadDir(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".poster-") {
			t.Fatalf("校验失败不应留下临时文件: %s", e.Name())
		}
	}
}

func TestPosterSaveBytesDedupReusesContent(t *testing.T) {
	store, err := NewPosterStore(filepath.Join(t.TempDir(), "posters"))
	if err != nil {
		t.Fatal(err)
	}
	data := samplePNGBytes()
	u1, created, err := store.saveBytesDedup(data)
	if err != nil || !created {
		t.Fatalf("saveBytesDedup: err=%v created=%v", err, created)
	}
	sum := sha256.Sum256(data)
	want := PosterURLPrefix + hex.EncodeToString(sum[:16]) + ".png"
	if u1 != want {
		t.Fatalf("去重文件名应为内容哈希: got=%q want=%q", u1, want)
	}
	u2, created2, err := store.saveBytesDedup(data)
	if err != nil || u1 != u2 || created2 {
		t.Fatalf("相同内容应复用同一 URL: u1=%q u2=%q created2=%v err=%v", u1, u2, created2, err)
	}
	if !store.Exists(u1) {
		t.Fatalf("去重文件应存在: %s", u1)
	}
	other, createdOther, err := store.saveBytesDedup(sampleJPEGBytes())
	if err != nil || !createdOther {
		t.Fatalf("saveBytesDedup(jpeg): err=%v created=%v", err, createdOther)
	}
	if other == u1 {
		t.Fatalf("不同内容不应复用同一 URL: %q", other)
	}
	if err := store.Delete(u1); err != nil {
		t.Fatal(err)
	}
	if u3, created3, err := store.saveBytesDedup(data); err != nil || u3 != u1 || !created3 || !store.Exists(u3) {
		t.Fatalf("文件被删除后应重新写入: u3=%q created3=%v err=%v", u3, created3, err)
	}
}

func TestPosterLocalizeDoesNotBreakHostOn404AndBreaksOn503(t *testing.T) {
	store, err := NewPosterStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var hits404, hits503 atomic.Int32
	client := &http.Client{Transport: posterRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "cdn-404.example":
			if r.URL.Path == "/ok.png" {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"image/png"}},
					Body:       io.NopCloser(bytes.NewReader(samplePNGBytes())),
				}, nil
			}
			hits404.Add(1)
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("missing")),
			}, nil
		case "cdn-503.example":
			hits503.Add(1)
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(strings.NewReader("down")),
			}, nil
		default:
			return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader("bad"))}, nil
		}
	})}
	store.SetHTTPClientForTest(client, false)

	// 连续 4 次 404 不应触发宿主熔断，第 5 条同宿主正常图应继续抓取成功。
	urls404 := []string{
		"https://cdn-404.example/1.png",
		"https://cdn-404.example/2.png",
		"https://cdn-404.example/3.png",
		"https://cdn-404.example/4.png",
		"https://cdn-404.example/ok.png",
	}
	if got := localizeURLsBounded(context.Background(), nil, store, urls404); got != 1 {
		t.Fatalf("404 不应熔断同宿主后续正常 URL，localized=%d want 1", got)
	}
	if hits404.Load() != 4 {
		t.Fatalf("4 条 404 URL 都应尝试请求，实际=%d", hits404.Load())
	}

	// 连续瞬态 503 达到阈值（3 次）后应对同宿主熔断，后续请求不再全部发出。
	urls503 := []string{
		"https://cdn-503.example/1.png",
		"https://cdn-503.example/2.png",
		"https://cdn-503.example/3.png",
		"https://cdn-503.example/4.png",
		"https://cdn-503.example/5.png",
		"https://cdn-503.example/6.png",
		"https://cdn-503.example/7.png",
		"https://cdn-503.example/8.png",
		"https://cdn-503.example/9.png",
		"https://cdn-503.example/10.png",
	}
	if got := localizeURLsBounded(context.Background(), nil, store, urls503); got != 0 {
		t.Fatalf("503 应全部失败，localized=%d", got)
	}
	if hits503.Load() >= int32(len(urls503)) {
		t.Fatalf("503 瞬态错误应在达到阈值后熔断，实际请求次数=%d（未熔断）", hits503.Load())
	}
}

func TestPosterStoreCloseCancelsAsyncLocalize(t *testing.T) {
	store, err := NewPosterStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	canceled := make(chan struct{})
	store.SetHTTPClientForTest(&http.Client{Transport: posterRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-r.Context().Done()
		select {
		case <-canceled:
		default:
			close(canceled)
		}
		return nil, r.Context().Err()
	})}, false)

	// 直接借助 SaveFromURL + bgCtx 验证 Close 的取消与等待语义。
	store.mu.Lock()
	store.bgWG.Add(1)
	parentCtx := store.bgCtx
	store.mu.Unlock()
	go func() {
		defer store.bgWG.Done()
		_, _ = store.SaveFromURL(parentCtx, "https://cdn.example/hang.png")
	}()

	<-started
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := store.Close(closeCtx); err != nil {
		t.Fatalf("Close 应在超时前结束异步抓取: %v", err)
	}
	select {
	case <-canceled:
	default:
		t.Fatal("Close 未取消在途抓取请求")
	}
	// 再次 Close 应幂等，且 Close 后 TriggerAsyncLocalize 不再起 goroutine。
	if err := store.Close(closeCtx); err != nil {
		t.Fatalf("重复 Close 应幂等: %v", err)
	}
}

func TestPosterSaveFromURLOmitsAVIFAcceptHeader(t *testing.T) {
	store, err := NewPosterStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var gotAccept string
	store.SetHTTPClientForTest(&http.Client{Transport: posterRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotAccept = r.Header.Get("Accept")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/png"}},
			Body:       io.NopCloser(bytes.NewReader(samplePNGBytes())),
		}, nil
	})}, false)

	localURL, err := store.SaveFromURL(context.Background(), "https://cdn.example/beer.png")
	if err != nil {
		t.Fatalf("SaveFromURL: %v", err)
	}
	if localURL == "" {
		t.Fatal("expected non-empty local poster URL")
	}
	if strings.Contains(strings.ToLower(gotAccept), "image/avif") {
		t.Fatalf("Accept header should omit unsupported image/avif, got %q", gotAccept)
	}
}
