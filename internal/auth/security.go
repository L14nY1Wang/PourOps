package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"pourops/internal/platform"
	"pourops/internal/ui"
)

const (
	totpPeriod      = 30 // 秒
	totpDigits      = 6
	totpSkew        = 1 // 允许前后各一个时间步
	otpMaxFailures  = 5
	otpBlockFor     = 30 * time.Second
	recoveryCount   = 8
	recoveryBytes   = 16 // 128 bit 熵
	setupCookie     = "bar_2fa_setup"
	setupCookieTTL  = 10 * time.Minute
	twoFactorKeyEnv = "TWO_FACTOR_KEY"
)

// SecurityView 只放页面需要的状态，密文和明文种子都不进模板以外的任何地方。
type SecurityView struct {
	TwoFactor bool
	Setup     bool     // 正在确认首个动态码
	Secret    string   // 分组后的明文种子，仅设置页一次性展示
	Recovery  []string // 明文恢复码，仅启用成功那一次展示
}

type securityPageData struct {
	ui.Layout
	Sec SecurityView
}

type SecurityPage = securityPageData

var totpBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// ---------- TOTP ----------

func newTOTPSecret() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return totpBase32.EncodeToString(raw), nil
}

func totpCode(secret string, t time.Time, digits int) (int64, string, error) {
	key, err := totpBase32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil || len(key) == 0 {
		return 0, "", errors.New("动态码种子格式不对")
	}
	step := t.Unix() / totpPeriod
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	val := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for range digits {
		mod *= 10
	}
	return step, fmt.Sprintf("%0*d", digits, val%mod), nil
}

func verifyTOTP(secret, code string, now time.Time) (int64, bool, error) {
	code = strings.TrimSpace(code)
	if !isTOTPCode(code) {
		return 0, false, nil
	}
	base := now.Unix() / totpPeriod
	for i := int64(-totpSkew); i <= totpSkew; i++ {
		step := base + i
		_, want, err := totpCode(secret, time.Unix(step*totpPeriod, 0), totpDigits)
		if err != nil {
			return 0, false, err
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return step, true, nil
		}
	}
	return 0, false, nil
}

func isTOTPCode(code string) bool {
	if len(code) != totpDigits {
		return false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ---------- 密文：AES-GCM，密钥来自独立的 TWO_FACTOR_KEY（32 字节十六进制） ----------

func twoFactorKey() ([]byte, error) {
	raw := strings.TrimSpace(platform.Env(twoFactorKeyEnv, ""))
	if raw == "" {
		return nil, errors.New("未设置 " + twoFactorKeyEnv)
	}
	key, err := hex.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return nil, errors.New(twoFactorKeyEnv + " 必须是 32 字节的十六进制（64 个字符）")
	}
	return key, nil
}

func twoFactorAAD(uid int64) []byte {
	return []byte("bar-2fa:" + strconv.FormatInt(uid, 10))
}

func seal2FA(uid int64, plain string) (string, error) {
	key, err := twoFactorKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := gcm.Seal(nonce, nonce, []byte(plain), twoFactorAAD(uid))
	return base64.RawStdEncoding.EncodeToString(out), nil
}

func open2FA(uid int64, blob string) (string, error) {
	key, err := twoFactorKey()
	if err != nil {
		return "", err
	}
	raw, err := base64.RawStdEncoding.DecodeString(blob)
	if err != nil {
		return "", errors.New("两步验证密钥损坏")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("两步验证密钥损坏")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], twoFactorAAD(uid))
	if err != nil {
		return "", errors.New("两步验证密钥无法解密（TWO_FACTOR_KEY 换过？）")
	}
	return string(plain), nil
}

// ---------- 恢复码：128 bit 随机，只存 SHA-256，一次性消费 ----------

func recoveryNorm(code string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(code) {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func recoveryHash(code string) string {
	sum := sha256.Sum256([]byte(recoveryNorm(code)))
	return hex.EncodeToString(sum[:])
}

func newRecoveryCodes() ([]string, []string, error) {
	codes := make([]string, 0, recoveryCount)
	hashes := make([]string, 0, recoveryCount)
	for range recoveryCount {
		raw := make([]byte, recoveryBytes)
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, err
		}
		plain := hex.EncodeToString(raw)
		groups := make([]string, 0, len(plain)/4)
		for j := 0; j < len(plain); j += 4 {
			groups = append(groups, plain[j:j+4])
		}
		code := strings.Join(groups, "-")
		codes = append(codes, code)
		hashes = append(hashes, recoveryHash(code))
	}
	return codes, hashes, nil
}

// ConsumeSecondFactor 在调用方已锁住的 users 行上消费一次动态码或恢复码。
// 绝不降级成单密码放行。
func (s *Service) ConsumeSecondFactor(ctx context.Context, tx pgx.Tx, id int64, code string) (bool, error) {
	code = strings.TrimSpace(code)
	var secret string
	var lastStep int64
	var hashes []string
	var failures int
	var blocked *time.Time
	err := tx.QueryRow(ctx,
		`select totp_secret, totp_last_step, recovery_hashes, otp_failures, otp_blocked_until
		   from users where id=$1`, id).
		Scan(&secret, &lastStep, &hashes, &failures, &blocked)
	if err != nil {
		return false, err
	}
	if secret == "" && len(hashes) == 0 {
		return false, errors.New("该账号没有开启两步验证")
	}
	if blocked != nil && blocked.After(time.Now()) {
		return false, nil
	}

	if !isTOTPCode(code) {
		if got := recoveryHash(code); got != "" {
			for _, h := range hashes {
				if subtle.ConstantTimeCompare([]byte(h), []byte(got)) == 1 {
					_, err := tx.Exec(ctx,
						`update users set recovery_hashes = array_remove(recovery_hashes, $2),
						        otp_failures = 0, otp_blocked_until = null where id=$1`, id, h)
					return err == nil, err
				}
			}
		}
	}

	if isTOTPCode(code) && secret != "" {
		plain, err := open2FA(id, secret)
		if err != nil {
			return false, err
		}
		step, ok, err := verifyTOTP(plain, code, time.Now())
		if err != nil {
			return false, err
		}
		if ok && step > lastStep {
			_, err := tx.Exec(ctx,
				`update users set totp_last_step = $2, otp_failures = 0, otp_blocked_until = null where id=$1`,
				id, step)
			return err == nil, err
		}
	}

	failures++
	var until any
	if failures >= otpMaxFailures {
		until = time.Now().Add(otpBlockFor)
		failures = 0
	}
	_, err = tx.Exec(ctx,
		`update users set otp_failures = $2, otp_blocked_until = coalesce($3::timestamptz, otp_blocked_until) where id=$1`,
		id, failures, until)
	return false, err
}

// ---------- 页面 ----------

func (s *Service) securityPage(w http.ResponseWriter, r *http.Request, v SecurityView, errMsg string) {
	w.Header().Set("Cache-Control", "no-store")
	if s.renderer != nil {
		s.renderer.Render(w, r, "admin/account_security.html", &securityPageData{
			Layout: ui.Layout{Title: "账号安全", Nav: "security", Err: errMsg},
			Sec:    v,
		})
	}
}

func (s *Service) AccountSecurity(w http.ResponseWriter, r *http.Request) {
	u := s.CurrentUser(r)
	if u == nil {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	s.securityPage(w, r, SecurityView{TwoFactor: u.TwoFactor}, "")
}

func (s *Service) AccountChangePassword(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	u := s.CurrentUser(r)
	if u == nil {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	view := SecurityView{TwoFactor: u.TwoFactor}
	nw := r.FormValue("new_password")
	switch {
	case !CheckPassword(r.FormValue("current_password"), u.PasswordHash):
		s.securityPage(w, r, view, "当前密码不对")
		return
	case len(nw) < 6:
		s.securityPage(w, r, view, "新密码至少 6 位")
		return
	case nw != r.FormValue("confirm_password"):
		s.securityPage(w, r, view, "两次输入的新密码不一致")
		return
	}

	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		http.Error(w, "修改密码暂时不可用", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	var hash string
	var enabled bool
	var version int64
	err = tx.QueryRow(ctx,
		`select password_hash, totp_secret <> '', auth_version from users where id=$1 and active for update`, u.ID).
		Scan(&hash, &enabled, &version)
	if err != nil {
		http.Error(w, "账号状态已变化，请重新登录", http.StatusConflict)
		return
	}
	view.TwoFactor = enabled
	if version != u.AuthVersion {
		clearCookie(w, r, SessionCookie, "/")
		http.Redirect(w, r, "/admin/login?msg="+platform.URLMsg("账号状态已变化，请重新登录"), http.StatusSeeOther)
		return
	}
	if !CheckPassword(r.FormValue("current_password"), hash) {
		s.securityPage(w, r, view, "当前密码不对")
		return
	}
	if enabled {
		ok, err := s.ConsumeSecondFactor(ctx, tx, u.ID, r.FormValue("code"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			if err := tx.Commit(ctx); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			s.securityPage(w, r, view, "动态码或恢复码不对")
			return
		}
	}
	if _, err := tx.Exec(ctx,
		`update users set password_hash=$2, auth_version=auth_version+1 where id=$1`, u.ID, HashPassword(nw)); err != nil {
		http.Error(w, "保存失败", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "保存失败", http.StatusInternalServerError)
		return
	}
	clearCookie(w, r, SessionCookie, "/")
	http.Redirect(w, r, "/admin/login?msg="+platform.URLMsg("密码已更新，请重新登录"), http.StatusSeeOther)
}

func groupSecret(secret string) string {
	groups := make([]string, 0, len(secret)/4+1)
	for len(secret) > 4 {
		groups = append(groups, secret[:4])
		secret = secret[4:]
	}
	groups = append(groups, secret)
	return strings.Join(groups, " ")
}

func (s *Service) signSetup(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte("2fa-setup|" + payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

func (s *Service) openSetup(cookie *http.Cookie, uid, version int64) (string, bool) {
	if cookie == nil {
		return "", false
	}
	i := strings.LastIndex(cookie.Value, ".")
	if i < 0 {
		return "", false
	}
	payload := cookie.Value[:i]
	if !hmac.Equal([]byte(s.signSetup(payload)), []byte(cookie.Value)) {
		return "", false
	}
	parts := strings.Split(payload, ".")
	if len(parts) != 4 {
		return "", false
	}
	gotUID, err1 := strconv.ParseInt(parts[0], 10, 64)
	gotVer, err2 := strconv.ParseInt(parts[1], 10, 64)
	exp, err3 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || gotUID != uid || gotVer != version || time.Now().Unix() > exp {
		return "", false
	}
	secret, err := open2FA(uid, parts[3])
	if err != nil {
		return "", false
	}
	return secret, true
}

func (s *Service) setSetupCookie(w http.ResponseWriter, r *http.Request, uid, version int64, secret string, exp time.Time) error {
	sealed, err := seal2FA(uid, secret)
	if err != nil {
		return err
	}
	payload := strconv.FormatInt(uid, 10) + "." + strconv.FormatInt(version, 10) + "." +
		strconv.FormatInt(exp.Unix(), 10) + "." + sealed
	http.SetCookie(w, &http.Cookie{
		Name: setupCookie, Value: s.signSetup(payload), Path: "/account/security",
		MaxAge: int(setupCookieTTL / time.Second), HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil,
	})
	return nil
}

func clearSetupCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: setupCookie, Value: "", Path: "/account/security", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil,
	})
}

func (s *Service) AccountStart2FA(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	u := s.CurrentUser(r)
	if u == nil {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	view := SecurityView{TwoFactor: u.TwoFactor}
	if u.TwoFactor {
		s.securityPage(w, r, view, "两步验证已经开启")
		return
	}
	if _, err := twoFactorKey(); err != nil {
		s.securityPage(w, r, view, "服务器未配置 TWO_FACTOR_KEY，暂时不能开启两步验证")
		return
	}
	if !CheckPassword(r.FormValue("password"), u.PasswordHash) {
		s.securityPage(w, r, view, "当前密码不对")
		return
	}
	secret, err := newTOTPSecret()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.setSetupCookie(w, r, u.ID, u.AuthVersion, secret, time.Now().Add(setupCookieTTL)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.securityPage(w, r, SecurityView{Setup: true, Secret: groupSecret(secret)}, "")
}

func (s *Service) AccountEnable2FA(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	u := s.CurrentUser(r)
	if u == nil {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	if u.TwoFactor {
		s.securityPage(w, r, SecurityView{TwoFactor: true}, "两步验证已经开启")
		return
	}
	cookie, _ := r.Cookie(setupCookie)
	secret, ok := s.openSetup(cookie, u.ID, u.AuthVersion)
	if !ok {
		clearSetupCookie(w, r)
		s.securityPage(w, r, SecurityView{}, "设置已过期，请重新开始")
		return
	}
	step, matched, err := verifyTOTP(secret, r.FormValue("code"), time.Now())
	if err != nil {
		clearSetupCookie(w, r)
		s.securityPage(w, r, SecurityView{}, "种子无效，请重新开始")
		return
	}
	if !matched {
		s.securityPage(w, r, SecurityView{Setup: true, Secret: groupSecret(secret)}, "动态码不对，请用验证器里最新的 6 位")
		return
	}

	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sealed, err := seal2FA(u.ID, secret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	var version int64
	var enabled bool
	err = tx.QueryRow(ctx,
		`select auth_version, totp_secret <> '' from users where id=$1 and active for update`, u.ID).
		Scan(&version, &enabled)
	if err != nil {
		http.Error(w, "账号状态已变化，请重新登录", http.StatusConflict)
		return
	}
	if enabled || version != u.AuthVersion {
		clearSetupCookie(w, r)
		http.Error(w, "账号状态已变化，请重新开始", http.StatusConflict)
		return
	}
	if _, err := tx.Exec(ctx,
		`update users set totp_secret=$2, totp_last_step=$3, recovery_hashes=$4,
		        otp_failures=0, otp_blocked_until=null, auth_version=auth_version+1
		  where id=$1`, u.ID, sealed, step, hashes); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	clearSetupCookie(w, r)
	clearCookie(w, r, SessionCookie, "/")
	s.securityPage(w, r, SecurityView{TwoFactor: true, Recovery: codes}, "")
}

func (s *Service) AccountDisable2FA(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	u := s.CurrentUser(r)
	if u == nil {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	view := SecurityView{TwoFactor: u.TwoFactor}
	if !u.TwoFactor {
		s.securityPage(w, r, view, "两步验证没有开启")
		return
	}

	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	var hash string
	var enabled bool
	var version int64
	err = tx.QueryRow(ctx,
		`select password_hash, totp_secret <> '', auth_version from users where id=$1 and active for update`, u.ID).
		Scan(&hash, &enabled, &version)
	if err != nil {
		http.Error(w, "账号状态已变化，请重新登录", http.StatusConflict)
		return
	}
	if version != u.AuthVersion {
		clearCookie(w, r, SessionCookie, "/")
		http.Redirect(w, r, "/admin/login?msg="+platform.URLMsg("账号状态已变化，请重新登录"), http.StatusSeeOther)
		return
	}
	if !CheckPassword(r.FormValue("password"), hash) {
		s.securityPage(w, r, view, "当前密码不对")
		return
	}
	if enabled {
		ok, err := s.ConsumeSecondFactor(ctx, tx, u.ID, r.FormValue("code"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			if err := tx.Commit(ctx); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			s.securityPage(w, r, view, "动态码或恢复码不对")
			return
		}
	}
	if _, err := tx.Exec(ctx,
		`update users set totp_secret='', totp_last_step=-1, recovery_hashes='{}',
		        otp_failures=0, otp_blocked_until=null, auth_version=auth_version+1 where id=$1`, u.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	clearSetupCookie(w, r)
	clearCookie(w, r, SessionCookie, "/")
	http.Redirect(w, r, "/admin/login?msg="+platform.URLMsg("两步验证已关闭，请重新登录"), http.StatusSeeOther)
}
