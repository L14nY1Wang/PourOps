package auth

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
	"pourops/internal/ui"
	"pourops/migrations"
)

func testLoginRenderer() *ui.Renderer {
	fs := fstest.MapFS{
		ui.AdminLayoutFile:           {Data: []byte(`{{define "layout"}}{{template "content" .}}{{end}}`)},
		"templates/admin/login.html": {Data: []byte(`{{define "content"}}{{if .TwoFactor}}两步验证<input name="code">{{else}}<input name="password">{{end}}{{end}}`)},
	}
	return ui.MustRenderer(fs, nil, func() string { return "PourOps" }, "admin/login.html")
}

func login2FAServer(pool *pgxpool.Pool) *Service {
	return NewService(pool, []byte("test-secret"), testLoginRenderer())
}

func login2FAPost(target string, form url.Values, ip string) *http.Request {
	req := httptest.NewRequest("POST", "http://bar.local"+target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = ip + ":1234"
	return req
}

func login2FACookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name && c.Value != "" {
			return c
		}
	}
	return nil
}

func login2FAWithCookie(req *http.Request, c *http.Cookie) *http.Request {
	req.AddCookie(c)
	return req
}

func login2FAEnable(t *testing.T, pool *pgxpool.Pool, username, pw string, role string) (User, string) {
	t.Helper()
	t.Setenv(twoFactorKeyEnv, strings.Repeat("ab", 32))
	ctx := context.Background()
	id, err := SaveUser(ctx, pool, User{Username: username, Role: role, Active: true}, pw)
	if err != nil {
		t.Fatalf("建账号: %v", err)
	}
	secret, err := newTOTPSecret()
	if err != nil {
		t.Fatalf("生成种子: %v", err)
	}
	blob, err := seal2FA(id, secret)
	if err != nil {
		t.Fatalf("加密种子: %v", err)
	}
	if _, err := pool.Exec(ctx, `update users set totp_secret=$2, totp_last_step=-1 where id=$1`, id, blob); err != nil {
		t.Fatalf("写入 2FA: %v", err)
	}
	u, err := GetUser(ctx, pool, id)
	if err != nil || !u.TwoFactor {
		t.Fatalf("读回账号: %+v err=%v", u, err)
	}
	return u, secret
}

func login2FAStep1(t *testing.T, s *Service, username, pw, ip string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Login(rec, login2FAPost("/admin/login", url.Values{"username": {username}, "password": {pw}}, ip))
	if rec.Code != http.StatusOK {
		t.Fatalf("密码步应停在 2FA 页，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if c := login2FACookie(rec, SessionCookie); c != nil {
		t.Fatal("第二因素没通过就发了正式会话")
	}
	ch := login2FACookie(rec, ChallengeCookie)
	if ch == nil {
		t.Fatal("密码步没发待验证挑战")
	}
	return ch
}

func login2FACode(t *testing.T, secret string) string {
	t.Helper()
	_, code, err := totpCode(secret, time.Now(), totpDigits)
	if err != nil {
		t.Fatalf("算动态码: %v", err)
	}
	return code
}

func TestTwoFactorLoginHoldsSessionUntilSecondStep(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	u, secret := login2FAEnable(t, pool, "boss", "boss-pass-1", RoleBoss)
	s := login2FAServer(pool)
	ctx := context.Background()

	rec := httptest.NewRecorder()
	s.Login(rec, login2FAPost("/admin/login", url.Values{"username": {"boss"}, "password": {"boss-pass-1"}}, "192.0.2.31"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "两步验证") {
		t.Fatalf("密码对但 2FA 开启时应显示验证页，得到 %d", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("2FA 页必须 no-store，得到 %q", rec.Header().Get("Cache-Control"))
	}
	if c := login2FACookie(rec, SessionCookie); c != nil {
		t.Fatal("密码步就发了正式会话")
	}
	ch := login2FACookie(rec, ChallengeCookie)
	if ch == nil || !ch.HttpOnly || ch.SameSite != http.SameSiteLaxMode || ch.Secure {
		t.Fatalf("待验证 cookie 属性不对: %+v", ch)
	}
	if ch.MaxAge <= 0 || ch.MaxAge > 360 {
		t.Fatalf("待验证 cookie 不该是长效的: %+v", ch)
	}

	recTLS := httptest.NewRecorder()
	reqTLS := login2FAPost("/admin/login", url.Values{"username": {"boss"}, "password": {"boss-pass-1"}}, "192.0.2.32")
	reqTLS.TLS = &tls.ConnectionState{}
	s.Login(recTLS, reqTLS)
	if ch := login2FACookie(recTLS, ChallengeCookie); ch == nil || !ch.Secure {
		t.Fatalf("TLS 下待验证 cookie 应带 Secure: %+v", ch)
	}

	get := httptest.NewRequest("GET", "http://bar.local/admin/login", nil)
	get.AddCookie(ch)
	recGet := httptest.NewRecorder()
	s.LoginPage(recGet, get)
	if recGet.Code != http.StatusOK || !strings.Contains(recGet.Body.String(), "两步验证") ||
		strings.Contains(recGet.Body.String(), `name="password"`) {
		t.Fatalf("带挑战的 GET 该显示验证页: %d", recGet.Code)
	}
	if recGet.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("验证页 GET 也必须 no-store")
	}

	recBad := httptest.NewRecorder()
	bad := "000000"
	if bad == login2FACode(t, secret) {
		bad = "000001"
	}
	s.Login(recBad, login2FAWithCookie(login2FAPost("/admin/login", url.Values{"action": {"2fa"}, "code": {bad}}, "192.0.2.33"), ch))
	if recBad.Code != http.StatusUnauthorized || login2FACookie(recBad, SessionCookie) != nil {
		t.Fatalf("错误动态码不该换到会话: %d", recBad.Code)
	}
	var failures int
	if err := pool.QueryRow(ctx, `select otp_failures from users where id=$1`, u.ID).Scan(&failures); err != nil || failures < 1 {
		t.Fatalf("失败次数没落库: %d err=%v", failures, err)
	}

	code := login2FACode(t, secret)
	recOK := httptest.NewRecorder()
	s.Login(recOK, login2FAWithCookie(login2FAPost("/admin/login", url.Values{"action": {"2fa"}, "code": {code}}, "192.0.2.34"), ch))
	if recOK.Code != http.StatusSeeOther {
		t.Fatalf("正确动态码应登录成功，得到 %d: %s", recOK.Code, recOK.Body.String())
	}
	sess := login2FACookie(recOK, SessionCookie)
	if sess == nil || !sess.HttpOnly || sess.SameSite != http.SameSiteLaxMode {
		t.Fatalf("正式会话 cookie 属性不对: %+v", sess)
	}
	me := login2FACurrentUser(s, sess)
	if me == nil || me.ID != u.ID || me.AuthVersion != u.AuthVersion {
		t.Fatalf("会话没绑定当前账号/版本: %+v", me)
	}
	var after int
	if err := pool.QueryRow(ctx, `select otp_failures from users where id=$1`, u.ID).Scan(&after); err != nil || after != 0 {
		t.Fatalf("成功后失败计数应清零: %d err=%v", after, err)
	}

	ch2 := login2FAStep1(t, s, "boss", "boss-pass-1", "192.0.2.35")
	recReplay := httptest.NewRecorder()
	s.Login(recReplay, login2FAWithCookie(login2FAPost("/admin/login", url.Values{"action": {"2fa"}, "code": {code}}, "192.0.2.36"), ch2))
	if recReplay.Code != http.StatusUnauthorized || login2FACookie(recReplay, SessionCookie) != nil {
		t.Fatalf("用过的动态码不能重放: %d", recReplay.Code)
	}
}

func login2FACurrentUser(s *Service, c *http.Cookie) *User {
	req := httptest.NewRequest("GET", "http://bar.local/admin/", nil)
	req.AddCookie(c)
	return s.CurrentUser(req)
}

func TestSecondFactorChallengeDiesWithAuthVersion(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	u, secret := login2FAEnable(t, pool, "boss", "boss-pass-2", RoleBoss)
	s := login2FAServer(pool)
	ctx := context.Background()

	ch := login2FAStep1(t, s, "boss", "boss-pass-2", "192.0.2.41")
	if _, err := pool.Exec(ctx, `update users set auth_version = auth_version + 1 where id=$1`, u.ID); err != nil {
		t.Fatalf("递增版本: %v", err)
	}

	rec := httptest.NewRecorder()
	s.Login(rec, login2FAWithCookie(login2FAPost("/admin/login", url.Values{"action": {"2fa"}, "code": {login2FACode(t, secret)}}, "192.0.2.42"), ch))
	if rec.Code != http.StatusUnauthorized || login2FACookie(rec, SessionCookie) != nil {
		t.Fatalf("旧版本挑战不该通过: %d", rec.Code)
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == ChallengeCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("旧挑战应被清掉")
	}

	if old := login2FACurrentUser(s, &http.Cookie{Name: SessionCookie, Value: s.SignSession(u.ID, u.AuthVersion, time.Now().Add(time.Hour))}); old != nil {
		t.Fatal("旧版本会话不该还能用")
	}
	if now := login2FACurrentUser(s, &http.Cookie{Name: SessionCookie, Value: s.SignSession(u.ID, u.AuthVersion+1, time.Now().Add(time.Hour))}); now == nil {
		t.Fatal("当前版本会话应有效")
	}
}

func TestChallengeAndSessionSignaturesAreSeparated(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	u, _ := login2FAEnable(t, pool, "boss", "boss-pass-3", RoleBoss)
	s := login2FAServer(pool)

	ch := login2FAStep1(t, s, "boss", "boss-pass-3", "192.0.2.51")
	if u2 := login2FACurrentUser(s, &http.Cookie{Name: SessionCookie, Value: ch.Value}); u2 != nil {
		t.Fatal("待验证挑战不能当正式会话用")
	}

	req := httptest.NewRequest("GET", "http://bar.local/admin/login", nil)
	req.AddCookie(&http.Cookie{Name: ChallengeCookie, Value: s.SignSession(u.ID, u.AuthVersion, time.Now().Add(time.Hour))})
	rec := httptest.NewRecorder()
	s.LoginPage(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `name="password"`) {
		t.Fatalf("会话票据不该被当成 2FA 挑战: %d", rec.Code)
	}

	legacy := strconv.FormatInt(u.ID, 10) + "." + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + ".deadbeef"
	if me := login2FACurrentUser(s, &http.Cookie{Name: SessionCookie, Value: legacy}); me != nil {
		t.Fatal("旧格式会话 cookie 应失效，升级后必须重新登录")
	}
}

func TestPasswordOnlyLoginKeepsRoleLanding(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	s := login2FAServer(pool)
	ctx := context.Background()
	if _, err := SaveUser(ctx, pool, User{Username: "clerk", Role: RoleStaff, Active: true}, "clerk-pass-1"); err != nil {
		t.Fatalf("建店员: %v", err)
	}

	rec := httptest.NewRecorder()
	s.Login(rec, login2FAPost("/admin/login", url.Values{"username": {"clerk"}, "password": {"wrong-pass-1"}}, "192.0.2.61"))
	if rec.Code != http.StatusUnauthorized || login2FACookie(rec, SessionCookie) != nil {
		t.Fatalf("密码不对不该发会话: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.Login(rec, login2FAPost("/admin/login", url.Values{"username": {"clerk"}, "password": {"clerk-pass-1"}}, "192.0.2.62"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/pos/" {
		t.Fatalf("店员登录后应进点餐端，得到 %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if login2FACookie(rec, ChallengeCookie) != nil {
		t.Fatal("未启用 2FA 的账号不该收到挑战 cookie")
	}
	sess := login2FACookie(rec, SessionCookie)
	if sess == nil {
		t.Fatal("登录没下发会话 cookie")
	}
	if me := login2FACurrentUser(s, sess); me == nil || me.Username != "clerk" {
		t.Fatalf("会话无效: %+v", me)
	}
}
