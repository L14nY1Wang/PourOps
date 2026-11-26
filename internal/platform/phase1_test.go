package platform_test

import (
	"context"
	"crypto/tls"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pourops/internal/platform"
	"pourops/migrations"
)

func TestMigrateExactCountAndChecksumVerification(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("ReadDir migrations: %v", err)
	}
	wantName := ""
	wantCount := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			if wantName == "" {
				wantName = e.Name()
			}
			wantCount++
		}
	}
	if wantCount != 13 || wantName == "" {
		t.Fatalf("expected 13 sql migrations, got %d", wantCount)
	}

	var gotCount int
	if err := pool.QueryRow(context.Background(), `select count(*) from schema_migrations where checksum <> ''`).Scan(&gotCount); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if gotCount != wantCount {
		t.Fatalf("schema_migrations count with checksum = %d, want %d", gotCount, wantCount)
	}

	// 验证表 schema_migrations 校验和篡改检查：模拟已应用文件被修改后拒绝启动
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer conn.Release()
	if err := platform.MigrateOneForTest(context.Background(), conn, wantName, "-- tampered sql"); err == nil {
		t.Fatalf("expected checksum mismatch error for tampered migration %s", wantName)
	}
}

func TestTrustProxyAndLimitRequestBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if platform.IsSecureRequest(req) {
		t.Fatalf("plain HTTP request must not be secure")
	}
	req.Header.Set("X-Forwarded-Proto", "https")
	if platform.IsSecureRequest(req) {
		t.Fatalf("X-Forwarded-Proto must be ignored when TRUST_PROXY is unset")
	}
	t.Setenv("TRUST_PROXY", "1")
	if !platform.IsSecureRequest(req) {
		t.Fatalf("expected X-Forwarded-Proto=https to be secure when TRUST_PROXY=1")
	}

	t.Setenv("COOKIE_SECURE", "1")
	if !platform.IsSecureRequest(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Fatalf("COOKIE_SECURE=1 should force IsSecureRequest=true")
	}
	t.Setenv("COOKIE_SECURE", "")

	tlsReq := httptest.NewRequest(http.MethodGet, "/", nil)
	tlsReq.TLS = &tls.ConnectionState{}
	t.Setenv("TRUST_PROXY", "")
	if !platform.IsSecureRequest(tlsReq) {
		t.Fatalf("TLS request must be secure")
	}

	h := platform.LimitRequestBody(16, map[string]int64{"/big": 64})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	recSmall := httptest.NewRecorder()
	h.ServeHTTP(recSmall, httptest.NewRequest(http.MethodPost, "/normal", strings.NewReader("1234567890")))
	if recSmall.Code != http.StatusOK {
		t.Fatalf("small body status = %d, want 200", recSmall.Code)
	}

	recTooBig := httptest.NewRecorder()
	h.ServeHTTP(recTooBig, httptest.NewRequest(http.MethodPost, "/normal", strings.NewReader(strings.Repeat("a", 32))))
	if recTooBig.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413", recTooBig.Code)
	}

	recOverride := httptest.NewRecorder()
	h.ServeHTTP(recOverride, httptest.NewRequest(http.MethodPost, "/big", strings.NewReader(strings.Repeat("a", 32))))
	if recOverride.Code != http.StatusOK {
		t.Fatalf("override route status = %d, want 200", recOverride.Code)
	}
}

func TestFlashCookieRoundTripAndClear(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/beers", nil)
	platform.RedirectErr(rec, req, "/admin/beers", "价格无效\n<script>alert(1)</script>")

	res := rec.Result()
	if res.Header.Get("Location") != "/admin/beers" {
		t.Fatalf("redirect Location = %q, want clean /admin/beers without query params", res.Header.Get("Location"))
	}
	msg, errMsg := platform.ReadFlashCookies(res.Cookies())
	if msg != "" || !strings.Contains(errMsg, "价格无效") {
		t.Fatalf("ReadFlashCookies = (%q, %q)", msg, errMsg)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/admin/beers", nil)
	for _, c := range res.Cookies() {
		if c.Name == platform.FlashCookieName {
			if !c.HttpOnly {
				t.Fatalf("flash cookie must be HttpOnly")
			}
			getReq.AddCookie(c)
		}
	}
	getRec := httptest.NewRecorder()
	pMsg, pErr := platform.PopFlash(getRec, getReq)
	if pMsg != "" || !strings.Contains(pErr, "价格无效") {
		t.Fatalf("PopFlash = (%q, %q)", pMsg, pErr)
	}
	cleared := false
	for _, c := range getRec.Result().Cookies() {
		if c.Name == platform.FlashCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("PopFlash should clear flash cookie")
	}
}

func TestFlashTruncatesHugeUTF8Payload(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/beers", nil)
	longMsg := strings.Repeat("酒", 100)
	longErr := strings.Repeat("错", 800)
	platform.SetFlash(rec, req, longMsg, longErr) // 不得 panic

	getReq := httptest.NewRequest(http.MethodGet, "/admin/beers", nil)
	found := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == platform.FlashCookieName {
			found = true
			getReq.AddCookie(c)
		}
	}
	if !found {
		t.Fatalf("超长 flash 也应写入 %s cookie", platform.FlashCookieName)
	}
	msg, errMsg := platform.PopFlash(httptest.NewRecorder(), getReq)
	if msg != longMsg {
		t.Fatalf("256 rune 以内的消息不应被截断，len=%d", len([]rune(msg)))
	}
	if want := string([]rune(longErr)[:256]) + "..."; errMsg != want {
		t.Fatalf("错误消息应按 rune 截断到 256 字 + ...，实际 len=%d 结尾=%q", len([]rune(errMsg)), errMsg[max(0, len(errMsg)-3):])
	}
}

func TestMoneyCentsStrictAndPosterAndTimezoneHardening(t *testing.T) {
	badMoney := []string{"1e2", "0x10", "1_000", "12.345", "12.", "+12", "-5", "abc", "12,5", ",100", "1234,567", "12,,345", "12.3,4"}
	for _, raw := range badMoney {
		if _, err := platform.MoneyCents(raw); err == nil {
			t.Fatalf("MoneyCents(%q) should fail", raw)
		}
	}
	goodMoney := map[string]int{
		"":          0,
		"0":         0,
		"¥1,234.50": 123450,
		"12,345":    1234500,
		"12.5":      1250,
		"0.05":      5,
	}
	for raw, want := range goodMoney {
		got, err := platform.MoneyCents(raw)
		if err != nil || got != want {
			t.Fatalf("MoneyCents(%q) = (%d, %v), want (%d, nil)", raw, got, err, want)
		}
	}

	// ValidPosterURL 安全路径检查
	if !platform.ValidPosterURL("/posters/abc.jpg") || !platform.ValidPosterURL("/static/brand/pourops.svg") {
		t.Fatalf("expected /posters/ and /static/ relative URLs to pass")
	}
	badURLs := []string{"/admin/users", "/posters/../etc/passwd", "/posters/a\\b.jpg", "//evil.com/x.png", "javascript:alert(1)"}
	for _, u := range badURLs {
		if platform.ValidPosterURL(u) {
			t.Fatalf("ValidPosterURL(%q) should reject unsafe URL", u)
		}
	}
	if platform.ValidRemotePosterURL("/posters/abc.jpg") {
		t.Fatalf("ValidRemotePosterURL must reject relative path")
	}
	if !platform.ValidRemotePosterURL("https://cdn.example.com/beer.jpg") {
		t.Fatalf("ValidRemotePosterURL should allow https URL")
	}

	// 时区固定为东八区（即便进程 TZ=UTC）
	t.Setenv("TZ", "UTC")
	parsed := platform.ParseLocalTime("2026-05-01T14:30")
	if parsed == nil {
		t.Fatalf("ParseLocalTime returned nil")
	}
	if got := platform.TimeInput(parsed); got != "2026-05-01T14:30" {
		t.Fatalf("TimeInput = %q, want 2026-05-01T14:30", got)
	}
	if got := platform.TimeText(parsed); got != "05-01 14:30" {
		t.Fatalf("TimeText = %q, want 05-01 14:30", got)
	}
	if got := platform.DateTimeText(parsed); got != "2026-05-01 14:30" {
		t.Fatalf("DateTimeText = %q, want 2026-05-01 14:30", got)
	}

	// 字段长度校验
	if err := platform.ValidateBeerTexts(strings.Repeat("厂", 101), "Beer", "", "", "", nil); err == nil {
		t.Fatalf("expected error for oversized brewery")
	}
	if err := platform.ValidateBeerTexts("Brewery", strings.Repeat("名", 121), "", "", "", nil); err == nil {
		t.Fatalf("expected error for oversized name")
	}
	if err := platform.ValidateBeerTexts("Brewery", "Beer", "", "", "", make([]string, 21)); err == nil {
		t.Fatalf("expected error for >20 tags")
	}
}

func TestOutboundSSRFAndEnvBool(t *testing.T) {
	t.Setenv("FLAG_TRUE", "yes")
	if !platform.EnvBool("FLAG_TRUE") {
		t.Fatalf("EnvBool('yes') = false, want true")
	}
	if platform.EnvBool("FLAG_UNSET") {
		t.Fatalf("EnvBool(unset) = true, want false")
	}

	for _, raw := range []string{
		"127.0.0.1", "10.0.0.1", "100.64.0.1", "100.100.100.200",
		"169.254.169.254", "192.0.2.1", "0.0.0.0", "255.255.255.255",
		"::1", "::ffff:127.0.0.1", "64:ff9b::7f00:1", "2002:0a00:0001::1", "fd00:ec2::254", "fe80::1",
	} {
		if !platform.IsBlockedOutboundIP(net.ParseIP(raw), false) {
			t.Errorf("IsBlockedOutboundIP(%q, false) = false, want true", raw)
		}
	}
	// 云元数据即便 allowPrivate=true 也必须无条件拦截
	for _, meta := range []string{"169.254.169.254", "100.100.100.200", "fd00:ec2::254"} {
		if !platform.IsBlockedOutboundIP(net.ParseIP(meta), true) {
			t.Errorf("IsBlockedOutboundIP(%q, true) = false, want true", meta)
		}
	}
	if platform.IsBlockedOutboundIP(net.ParseIP("127.0.0.1"), true) {
		t.Errorf("loopback should be allowed when allowPrivate=true")
	}

	for _, badURL := range []string{
		"http://[2606:4700:4700::1111%25eth0]/a.png",
		"http://[::1%25lo0]/a.png",
		"http://localhost./a.png",
		"http://metadata.google.internal./",
		"https://[fd00:ec2::254]/latest/meta-data",
	} {
		if err := platform.ValidateOutboundURL(badURL, "测试地址", false); err == nil {
			t.Errorf("ValidateOutboundURL(%q) = nil, want error", badURL)
		}
	}
	if err := platform.ValidateOutboundURL("https://cdn.example/a.png", "测试地址", false); err != nil {
		t.Errorf("valid public URL rejected: %v", err)
	}
}
