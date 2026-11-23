package auth

import (
	"bytes"
	"context"
	"crypto/tls"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"pourops/internal/platform"
	"pourops/migrations"
)

func TestSeedBossNeedsExplicitPassword(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `delete from users`); err != nil {
		t.Fatalf("清空账号失败: %v", err)
	}

	t.Setenv("BOSS_PASSWORD", "")
	if err := SeedBoss(ctx, pool); err == nil {
		t.Fatal("没设 BOSS_PASSWORD 时不该用内置默认密码建账号")
	}
	t.Setenv("BOSS_PASSWORD", "12345")
	if err := SeedBoss(ctx, pool); err == nil {
		t.Fatal("密码短于 6 位应该拒绝")
	}

	pw := "{{SECRET_lz9x2qhd}}"
	t.Setenv("BOSS_PASSWORD", pw)
	orig := log.Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(orig)
	if err := SeedBoss(ctx, pool); err != nil {
		t.Fatalf("SeedBoss: %v", err)
	}
	if strings.Contains(buf.String(), pw) {
		t.Fatalf("日志泄露初始密码: %q", buf.String())
	}
	u, err := UserByName(ctx, pool, "boss")
	if err != nil || u.Role != RoleBoss || !u.Active || !CheckPassword(pw, u.PasswordHash) {
		t.Fatalf("BOSS 账号不对: %+v err=%v", u, err)
	}

	s := NewService(pool, []byte("test-secret"), nil)
	login := func(secure bool) *http.Cookie {
		form := url.Values{"username": {"boss"}, "password": {pw}}
		req := httptest.NewRequest("POST", "http://bar.local/admin/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if secure {
			req.TLS = &tls.ConnectionState{}
		}
		rec := httptest.NewRecorder()
		s.Login(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("登录应成功，得到 %d", rec.Code)
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == SessionCookie {
				return c
			}
		}
		t.Fatal("登录没下发会话 cookie")
		return nil
	}
	if c := login(true); !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("TLS 下会话 cookie 属性不对: %+v", c)
	}
	if c := login(false); c.Secure {
		t.Fatal("非 TLS 下会话 cookie 不该带 Secure")
	}
}

func TestSaveUserConflictKeepsPassword(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `delete from users`); err != nil {
		t.Fatalf("清空账号失败: %v", err)
	}
	oldPw, newPw := "{{SECRET_4hq7wzxc}}", "{{SECRET_g8m2rktv}}"
	if _, err := SaveUser(ctx, pool, User{Username: "alice", Role: RoleManager, Active: true}, "{{SECRET_ps9c7vnb}}"); err != nil {
		t.Fatalf("建 alice: %v", err)
	}
	bobID, err := SaveUser(ctx, pool, User{Username: "bob", Role: RoleStaff, Active: true}, oldPw)
	if err != nil {
		t.Fatalf("建 bob: %v", err)
	}
	before, err := GetUser(ctx, pool, bobID)
	if err != nil {
		t.Fatalf("读 bob: %v", err)
	}

	if _, err := SaveUser(ctx, pool, User{ID: bobID, Username: "ALICE", Role: RoleManager, Active: false}, newPw); err == nil {
		t.Fatal("用户名冲突应报错")
	}
	after, err := GetUser(ctx, pool, bobID)
	if err != nil {
		t.Fatalf("再读 bob: %v", err)
	}
	if after.PasswordHash != before.PasswordHash || !CheckPassword(oldPw, after.PasswordHash) || CheckPassword(newPw, after.PasswordHash) {
		t.Fatal("失败的更新把密码改掉了")
	}
	if after.Username != "bob" || after.Role != RoleStaff || !after.Active {
		t.Fatalf("失败的更新留下了半截资料: %+v", after)
	}
}

func TestLogoutCookieAttributes(t *testing.T) {
	s := NewService(nil, nil, nil)
	for _, secure := range []bool{false, true} {
		req := httptest.NewRequest("POST", "http://bar.local/admin/logout", nil)
		if secure {
			req.TLS = &tls.ConnectionState{}
		}
		rec := httptest.NewRecorder()
		s.Logout(rec, req)
		cookies := rec.Result().Cookies()
		if len(cookies) != 2 {
			t.Fatalf("退出应清会话与待验证两个 cookie，得到 %d", len(cookies))
		}
		byName := map[string]*http.Cookie{}
		for _, c := range cookies {
			byName[c.Name] = c
			if c.Value != "" || c.MaxAge >= 0 || !c.HttpOnly ||
				c.SameSite != http.SameSiteLaxMode || c.Secure != secure {
				t.Fatalf("secure=%v 时退出 cookie 属性不对: %+v", secure, c)
			}
		}
		if c := byName[SessionCookie]; c == nil || c.Path != "/" {
			t.Fatalf("退出没清会话 cookie: %+v", c)
		}
		if c := byName[ChallengeCookie]; c == nil || c.Path != "/admin/login" {
			t.Fatalf("退出没清待验证 cookie: %+v", c)
		}
	}
}
