package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"pourops/internal/platform"
	"pourops/migrations"
)

func TestLoginThrottleCapEvictionAndTrustedProxyIPv6(t *testing.T) {
	th := newLoginThrottle(2, time.Minute)
	for i := range 4096 {
		th.recordFailure(fmt.Sprintf("ip-%04d", i))
	}
	// 新 key 插入第 4097 个时应淘汰旧条目，而不是放弃限速
	th.recordFailure("attacker")
	th.recordFailure("attacker")
	if th.allowCheck("attacker") {
		t.Fatalf("expected 4097th key to be throttled after cap eviction")
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.5:4321"
	req.Header.Set("X-Forwarded-For", "10.0.0.5, 2001:db8:1234:5678::1")
	if got := clientIPKey(req); got != "10.0.0.5" {
		t.Fatalf("without TRUST_PROXY clientIPKey = %q, want 10.0.0.5", got)
	}

	t.Setenv("TRUST_PROXY", "1")
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.RemoteAddr = "10.0.0.5:4321"
	req2.Header.Set("X-Forwarded-For", "198.51.100.77, 2001:db8:1234:5678::99")
	if clientIPKey(req) != clientIPKey(req2) {
		t.Fatalf("expected rightmost XFF IPv6 /64 prefix grouping: %q vs %q", clientIPKey(req), clientIPKey(req2))
	}

	// 客户端在最左侧伪造假 IP 时，仍必须取反代追加在最右侧的真实 IP
	spoofA := httptest.NewRequest("GET", "/", nil)
	spoofA.Header.Set("X-Forwarded-For", "1.1.1.1, 203.0.113.5")
	spoofB := httptest.NewRequest("GET", "/", nil)
	spoofB.Header.Set("X-Forwarded-For", "2.2.2.2, 203.0.113.5")
	if clientIPKey(spoofA) != "203.0.113.5" || clientIPKey(spoofA) != clientIPKey(spoofB) {
		t.Fatalf("spoofed leftmost XFF bypassed IP key: %q vs %q", clientIPKey(spoofA), clientIPKey(spoofB))
	}

	realIPReq := httptest.NewRequest("GET", "/", nil)
	realIPReq.Header.Set("X-Real-IP", "198.51.100.88")
	realIPReq.Header.Set("X-Forwarded-For", "9.9.9.9")
	if got := clientIPKey(realIPReq); got != "198.51.100.88" {
		t.Fatalf("expected X-Real-IP to take precedence under TRUST_PROXY=1, got %q", got)
	}
}

func TestValidateUsernameAndDuplicateUserErr(t *testing.T) {
	for _, bad := range []string{"", "   ", "bad name", "a/b", "<script>", strings.Repeat("x", 33)} {
		if _, err := validateUsername(bad); err == nil {
			t.Errorf("validateUsername(%q) = nil, want error", bad)
		}
	}
	if got, err := validateUsername(" bar_01 "); err != nil || got != "bar_01" {
		t.Fatalf("validateUsername(' bar_01 ') = (%q, %v), want ('bar_01', nil)", got, err)
	}

	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	if _, err := SaveUser(ctx, pool, User{Username: "boss1", Role: RoleBoss, Active: true}, "secret1"); err != nil {
		t.Fatalf("first SaveUser: %v", err)
	}
	_, err := SaveUser(ctx, pool, User{Username: "boss1", Role: RoleStaff, Active: true}, "secret1")
	if err == nil {
		t.Fatalf("expected duplicate username error")
	}
	if _, ok := platform.UserErrorMessage(err); !ok {
		t.Fatalf("expected UserErr for duplicate username, got %T (%v)", err, err)
	}
}

func TestSecondFactorRateLimitAndProgressiveLockout(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	svc := login2FAServer(pool)
	ctx := context.Background()

	u, _ := login2FAEnable(t, pool, "lockuser", "pass1234", RoleBoss)

	// 1) 通过 HTTP action=2fa 校验连续失败触发账号限流（acctLimiter limit=10）
	for i := range 12 {
		// 每次先通过密码登录取得 challenge cookie（清掉 db lock 以聚焦 HTTP 层限流）
		if _, err := pool.Exec(ctx, `update users set otp_failures=0, otp_blocked_until=null, otp_lock_level=0 where id=$1`, u.ID); err != nil {
			t.Fatalf("reset otp failures: %v", err)
		}
		rec1 := httptest.NewRecorder()
		svc.Login(rec1, login2FAPost("/login", url.Values{"username": {"lockuser"}, "password": {"pass1234"}}, "198.51.100."+strconv.Itoa(i+10)))
		chCookie := login2FACookie(rec1, ChallengeCookie)
		if chCookie == nil {
			t.Fatalf("iter %d: expected challenge cookie, got nil (flash=%q)", i, rec1.Header().Get("Location"))
		}
		rec2 := httptest.NewRecorder()
		req2 := login2FAWithCookie(login2FAPost("/login", url.Values{"action": {"2fa"}, "code": {"000000"}}, "198.51.100."+strconv.Itoa(i+10)), chCookie)
		svc.Login(rec2, req2)
		if i >= 10 && rec2.Code != http.StatusTooManyRequests {
			t.Fatalf("iter %d: expected HTTP 429 rate-limit on 2FA submit, got status=%d", i, rec2.Code)
		}
	}

	// 2) 直接验证 ConsumeSecondFactor 渐进锁定升级到第 4 级会递增 auth_version
	if _, err := pool.Exec(ctx, `update users set otp_failures=0, otp_blocked_until=null, otp_lock_level=3, auth_version=1 where id=$1`, u.ID); err != nil {
		t.Fatalf("seed lock level 3: %v", err)
	}
	for i := range 5 {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		ok, err := svc.ConsumeSecondFactor(ctx, tx, u.ID, "000000")
		if ok || err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("attempt %d: ok=%v err=%v", i, ok, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	var lockLevel, ver int64
	var blockedUntil *time.Time
	if err := pool.QueryRow(ctx, `select otp_lock_level, auth_version, otp_blocked_until from users where id=$1`, u.ID).Scan(&lockLevel, &ver, &blockedUntil); err != nil {
		t.Fatalf("query user lock state: %v", err)
	}
	if lockLevel < 4 || ver != 2 || blockedUntil == nil || !blockedUntil.After(time.Now().Add(20*time.Minute)) {
		t.Fatalf("expected level>=4, auth_version=2, >=20m block; got level=%d ver=%d blocked=%v", lockLevel, ver, blockedUntil)
	}
}

func TestAccountLogoutAllBossReset2FAAndCookieSecure(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	svc := NewService(pool, []byte("test-secret"), testLoginRenderer())
	mux := http.NewServeMux()
	svc.RegisterRoutes(mux)
	ctx := context.Background()

	t.Setenv("COOKIE_SECURE", "1")
	bossID, err := SaveUser(ctx, pool, User{Username: "theboss", Role: RoleBoss, Active: true}, "pass1234")
	if err != nil {
		t.Fatalf("SaveUser boss: %v", err)
	}
	mgr, _ := login2FAEnable(t, pool, "mgr1", "pass1234", RoleManager)

	// 密码登录验证 Secure cookie 与 logout-all 失效全部会话
	recLogin := httptest.NewRecorder()
	mux.ServeHTTP(recLogin, login2FAPost("/admin/login", url.Values{"username": {"theboss"}, "password": {"pass1234"}}, "203.0.113.10"))
	sess := login2FACookie(recLogin, SessionCookie)
	if sess == nil || !sess.Secure {
		t.Fatalf("expected Secure session cookie under COOKIE_SECURE=1, got %+v", sess)
	}

	reqLogoutAll := httptest.NewRequest("POST", "http://bar.local/account/security/logout-all", nil)
	reqLogoutAll.AddCookie(sess)
	recLogoutAll := httptest.NewRecorder()
	mux.ServeHTTP(recLogoutAll, reqLogoutAll)
	if recLogoutAll.Code != http.StatusSeeOther {
		t.Fatalf("logout-all status = %d, want 303", recLogoutAll.Code)
	}
	reqCheck := httptest.NewRequest("GET", "http://bar.local/account/security", nil)
	reqCheck.AddCookie(sess)
	if svc.CurrentUser(reqCheck) != nil {
		t.Fatalf("expected old session invalidated after logout-all")
	}

	// BOSS 重置 mgr1 的 2FA：先重新签发 BOSS 新会话
	freshBoss, err := GetUser(ctx, pool, bossID)
	if err != nil {
		t.Fatalf("GetUser boss: %v", err)
	}
	bossCookie := &http.Cookie{
		Name:  SessionCookie,
		Value: svc.SignSession(freshBoss.ID, freshBoss.AuthVersion, time.Now().Add(time.Hour)),
	}
	reqReset := httptest.NewRequest("POST", "http://bar.local/admin/users/"+strconv.FormatInt(mgr.ID, 10)+"/reset-2fa", nil)
	reqReset.AddCookie(bossCookie)
	recReset := httptest.NewRecorder()
	mux.ServeHTTP(recReset, reqReset)
	if recReset.Code != http.StatusSeeOther {
		t.Fatalf("reset-2fa status = %d, want 303", recReset.Code)
	}
	_, msgErr := platform.ReadFlashCookies(recReset.Result().Cookies())
	if msgErr != "" {
		t.Fatalf("reset-2fa flash err = %q", msgErr)
	}
	afterMgr, err := GetUser(ctx, pool, mgr.ID)
	if err != nil {
		t.Fatalf("GetUser mgr: %v", err)
	}
	if afterMgr.TwoFactor || afterMgr.AuthVersion != mgr.AuthVersion+1 {
		t.Fatalf("expected mgr 2FA disabled & auth_version bumped; got %+v", afterMgr)
	}
}
