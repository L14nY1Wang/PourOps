package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
	"pourops/migrations"
)

const rfcSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

func TestTOTPCodeMatchesRFC6238(t *testing.T) {
	cases := []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, c := range cases {
		if _, got, err := totpCode(rfcSecret, time.Unix(c.unix, 0), 8); err != nil || got != c.want {
			t.Fatalf("t=%d: got %q err=%v, want %q", c.unix, got, err, c.want)
		}
	}
}

func TestVerifyTOTPAcceptsSkewAndRejectsBadCode(t *testing.T) {
	now := time.Unix(1234567890, 0)
	for i := int64(-1); i <= 1; i++ {
		_, code, err := totpCode(rfcSecret, now.Add(time.Duration(i*totpPeriod)*time.Second), totpDigits)
		if err != nil {
			t.Fatal(err)
		}
		step, ok, err := verifyTOTP(rfcSecret, code, now)
		if err != nil || !ok || step != now.Unix()/totpPeriod+i {
			t.Fatalf("offset %d: step=%d ok=%v err=%v", i, step, ok, err)
		}
	}
	if _, ok, err := verifyTOTP(rfcSecret, "000000", now); err != nil || ok {
		t.Fatalf("错误码不该通过: ok=%v err=%v", ok, err)
	}
	if _, ok, err := verifyTOTP(rfcSecret, "abcdef", now); err != nil || ok {
		t.Fatalf("非数字不该通过: ok=%v err=%v", ok, err)
	}
}

func rfcTestKey() string { return strings.Repeat("0f", 32) }

func seed2FAUser(t *testing.T, pool *pgxpool.Pool, name, secret string, hashes []string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(ctx,
		`insert into users (username, password_hash, role, active) values ($1, 'x', 'staff', true) returning id`,
		name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if secret != "" {
		if hashes == nil {
			hashes = []string{}
		}
		sealed, err := seal2FA(id, secret)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx,
			`update users set totp_secret=$2, recovery_hashes=$3 where id=$1`, id, sealed, hashes); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func consumeForTest(t *testing.T, pool *pgxpool.Pool, id int64, code string) (bool, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var one int
	if err := tx.QueryRow(ctx, `select 1 from users where id=$1 for update`, id).Scan(&one); err != nil {
		t.Fatal(err)
	}
	svc := NewService(pool, nil, nil)
	ok, err := svc.ConsumeSecondFactor(ctx, tx, id, code)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return ok, nil
}

func TestSetupCookieEncryptsSecret(t *testing.T) {
	t.Setenv(twoFactorKeyEnv, rfcTestKey())
	secret, err := newTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/account/security/2fa/setup", nil)
	s := NewService(nil, []byte("test-session-key"), nil)
	if err := s.setSetupCookie(rec, req, 7, 3, secret, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == setupCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("没有写 setup cookie")
	}
	if strings.Contains(cookie.Value, secret) {
		t.Fatal("cookie 里不该有明文种子")
	}
	if !cookie.HttpOnly {
		t.Fatal("setup cookie 必须 HttpOnly")
	}
	if got, ok := s.openSetup(cookie, 7, 3); !ok || got != secret {
		t.Fatalf("解不开种子: ok=%v got=%q", ok, got)
	}
	if _, ok := s.openSetup(cookie, 8, 3); ok {
		t.Fatal("换个账号不该解得开")
	}
	if _, ok := s.openSetup(cookie, 7, 4); ok {
		t.Fatal("auth_version 变了不该解得开")
	}
}

func TestConsumeSecondFactorReplayAndRecovery(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	t.Setenv(twoFactorKeyEnv, rfcTestKey())
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	id := seed2FAUser(t, pool, "twofa", rfcSecret, hashes)

	_, code, err := totpCode(rfcSecret, time.Now(), totpDigits)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := consumeForTest(t, pool, id, code); err != nil || !ok {
		t.Fatalf("第一次动态码该通过: ok=%v err=%v", ok, err)
	}
	if ok, err := consumeForTest(t, pool, id, code); err != nil || ok {
		t.Fatalf("同一个时间步的码不能再用一次: ok=%v err=%v", ok, err)
	}
	if ok, err := consumeForTest(t, pool, id, codes[0]); err != nil || !ok {
		t.Fatalf("恢复码该通过: ok=%v err=%v", ok, err)
	}
	if ok, err := consumeForTest(t, pool, id, codes[0]); err != nil || ok {
		t.Fatalf("恢复码只能用一次: ok=%v err=%v", ok, err)
	}
}

func TestConsumeSecondFactorBlocksAfterRepeatedFailures(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	t.Setenv(twoFactorKeyEnv, rfcTestKey())
	id := seed2FAUser(t, pool, "blocked", rfcSecret, nil)

	for range otpMaxFailures {
		if ok, err := consumeForTest(t, pool, id, "000000"); err != nil || ok {
			t.Fatalf("错误码不该通过: ok=%v err=%v", ok, err)
		}
	}
	if ok, _ := consumeForTest(t, pool, id, "000000"); ok {
		t.Fatal("封锁期间不该通过")
	}
	_, code, err := totpCode(rfcSecret, time.Now(), totpDigits)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := consumeForTest(t, pool, id, code); ok {
		t.Fatal("封锁期间正确动态码也不能通过")
	}
	var blocked bool
	if err := pool.QueryRow(context.Background(),
		`select otp_blocked_until > now() from users where id=$1`, id).Scan(&blocked); err != nil {
		t.Fatal(err)
	}
	if !blocked {
		t.Fatal("失败次数该落库并短暂封锁")
	}
}

func TestRecoveryCodeWorksWithoutKey(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	t.Setenv(twoFactorKeyEnv, rfcTestKey())
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	id := seed2FAUser(t, pool, "nokey", rfcSecret, hashes)

	t.Setenv(twoFactorKeyEnv, "")
	_, code, err := totpCode(rfcSecret, time.Now(), totpDigits)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := consumeForTest(t, pool, id, code); err == nil || ok {
		t.Fatalf("没有密钥时必须报错、不能通过: ok=%v err=%v", ok, err)
	}
	if ok, err := consumeForTest(t, pool, id, codes[0]); err != nil || !ok {
		t.Fatalf("没有密钥时恢复码该能用: ok=%v err=%v", ok, err)
	}
}
