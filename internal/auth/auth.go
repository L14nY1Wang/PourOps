package auth

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
	"pourops/internal/ui"
)

const (
	RoleBoss    = ui.RoleBoss
	RoleManager = ui.RoleManager
	RoleStaff   = ui.RoleStaff
)

type User struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string
	Active       bool
	AuthVersion  int64 // 改密码、启停 2FA、停用等安全变更时递增，旧票据随之作废
	TwoFactor    bool  // users.totp_secret 非空；密文不进 User
}

func (u User) RoleName() string { return ui.RoleLabel(u.Role) }
func (u User) IsBoss() bool     { return u.Role == RoleBoss }
func (u User) CanBackend() bool { return u.Role == RoleBoss || u.Role == RoleManager }
func (u User) Initial() string  { return ui.UserView{Username: u.Username}.Initial() }

func (u User) Landing() string {
	if u.CanBackend() {
		return "/admin/"
	}
	return "/pos/"
}

func (u User) View() *ui.UserView {
	return &ui.UserView{
		ID:          u.ID,
		Username:    u.Username,
		Role:        u.Role,
		Active:      u.Active,
		AuthVersion: u.AuthVersion,
		TwoFactor:   u.TwoFactor,
	}
}

// ---------- 密码哈希：pbkdf2-sha256 ----------

const pbkdf2Iters = 120000

func HashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, _ := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iters, 32)
	return "pbkdf2$" + strconv.Itoa(pbkdf2Iters) + "$" + hex.EncodeToString(salt) + "$" + hex.EncodeToString(key)
}

func CheckPassword(pw, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iters, err := strconv.Atoi(parts[1])
	salt, err1 := hex.DecodeString(parts[2])
	want, err2 := hex.DecodeString(parts[3])
	if err != nil || iters < 1 || err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iters, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash 用于账号不存在时也做一次同代价校验，避免用响应时间判断账号是否存在。
var dummyHash = HashPassword("timing-only")

// ---------- 账号读写 ----------

const userCols = `select id, username, password_hash, role, active, auth_version, totp_secret <> '' from users`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Active, &u.AuthVersion, &u.TwoFactor)
	return u, err
}

func ListUsers(ctx context.Context, pool *pgxpool.Pool) ([]User, error) {
	rows, err := pool.Query(ctx, userCols+` order by case role when 'boss' then 0 when 'manager' then 1 else 2 end, username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func GetUser(ctx context.Context, pool *pgxpool.Pool, id int64) (User, error) {
	return scanUser(pool.QueryRow(ctx, userCols+` where id=$1`, id))
}

func UserByName(ctx context.Context, pool *pgxpool.Pool, name string) (User, error) {
	return scanUser(pool.QueryRow(ctx, userCols+` where lower(username)=lower($1)`, name))
}

func validateUsername(name string) (string, error) {
	u := strings.TrimSpace(name)
	if u == "" {
		return "", platform.UserErr("用户名不能为空")
	}
	if utf8.RuneCountInString(u) > 32 {
		return "", platform.UserErr("用户名最多 32 个字符")
	}
	for _, r := range u {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", platform.UserErr("用户名不能包含空白或控制字符")
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' {
			continue
		}
		return "", platform.UserErr("用户名只能包含字母、数字、中文及 ._ -")
	}
	return u, nil
}

// SaveUser 新增或修改账号；ID 为 0 时要求给密码，改账号时密码留空表示不改。
func SaveUser(ctx context.Context, pool *pgxpool.Pool, u User, password string) (int64, error) {
	cleanName, err := validateUsername(u.Username)
	if err != nil {
		return 0, err
	}
	u.Username = cleanName
	if !ui.ValidRole(u.Role) {
		return 0, platform.UserErr("角色不对")
	}
	if u.ID == 0 {
		if len(password) < 6 {
			return 0, platform.UserErr("密码至少 6 位")
		}
		if len(password) > 1024 {
			return 0, platform.UserErr("密码过长")
		}
		var id int64
		err := pool.QueryRow(ctx, `insert into users (username, password_hash, role, active) values ($1,$2,$3,$4) returning id`,
			u.Username, HashPassword(password), u.Role, u.Active).Scan(&id)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return 0, platform.UserErr("用户名已存在")
		}
		return id, err
	}
	if password != "" && len(password) < 6 {
		return 0, platform.UserErr("密码至少 6 位")
	}
	if len(password) > 1024 {
		return 0, platform.UserErr("密码过长")
	}
	// 资料和密码合成一条 update：用户名冲突等失败时密码不会先被改掉。
	hash := ""
	if password != "" {
		hash = HashPassword(password)
	}
	tag, err := pool.Exec(ctx, `update users set username=$2, role=$3, active=$4,
		password_hash=coalesce(nullif($5,''), password_hash),
		auth_version = auth_version + case when $5 <> '' or active is distinct from $4 then 1 else 0 end
		where id=$1`,
		u.ID, u.Username, u.Role, u.Active, hash)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return 0, platform.UserErr("用户名已存在")
	}
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, platform.UserErr("找不到这个账号")
	}
	return u.ID, nil
}

func DeleteUser(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	_, err := pool.Exec(ctx, `delete from users where id=$1`, id)
	return err
}

func ToggleUser(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	_, err := pool.Exec(ctx, `update users set active = not active, auth_version = auth_version + 1 where id=$1`, id)
	return err
}

// SeedBoss: 一个账号都没有时建 BOSS；初始密码必须显式给 BOSS_PASSWORD，不落日志。
func SeedBoss(ctx context.Context, pool *pgxpool.Pool) error {
	var n int
	if err := pool.QueryRow(ctx, `select count(*) from users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	pw := platform.Env("BOSS_PASSWORD", "")
	if len(pw) < 6 {
		return errors.New("首次启动请设置 BOSS_PASSWORD（至少 6 位）")
	}
	if _, err := SaveUser(ctx, pool, User{Username: "boss", Role: RoleBoss, Active: true}, pw); err != nil {
		return err
	}
	log.Print("已创建 BOSS 账号 boss，初始密码取自 BOSS_PASSWORD，请登录后立即修改")
	return nil
}

// ---------- 会话与认证服务 ----------

const (
	SessionCookie   = "bar_session"
	ChallengeCookie = "bar_2fa"

	sessionTTL   = 12 * time.Hour
	challengeTTL = 5 * time.Minute
)

type loginThrottle struct {
	mu     sync.Mutex
	hits   map[string]*throttleEntry
	limit  int
	window time.Duration
	maxCap int
}

type throttleEntry struct {
	n     int
	start time.Time
	last  time.Time
}

func newLoginThrottle(limit int, window time.Duration) *loginThrottle {
	return &loginThrottle{
		hits:   make(map[string]*throttleEntry),
		limit:  limit,
		window: window,
		maxCap: 4096,
	}
}

func normalizeIPKey(addr netip.Addr) string {
	addr = addr.Unmap()
	if addr.Is6() {
		return netip.PrefixFrom(addr, 64).Masked().String()
	}
	return addr.String()
}

func clientIPKey(r *http.Request) string {
	if r == nil {
		return "unknown"
	}
	if platform.TrustProxy() {
		if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
			// 反代把真实来路追加在 XFF 最右侧；从右向左取首个合法 IP，优先于可由客户端透传的 X-Real-IP。
			parts := strings.Split(xff, ",")
			for i := len(parts) - 1; i >= 0; i-- {
				if cand := strings.TrimSpace(parts[i]); cand != "" {
					if addr, err := netip.ParseAddr(cand); err == nil {
						return normalizeIPKey(addr)
					}
				}
			}
		}
		if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
			if addr, err := netip.ParseAddr(xri); err == nil {
				return normalizeIPKey(addr)
			}
		}
	}
	raw := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	if addr, err := netip.ParseAddr(raw); err == nil {
		return normalizeIPKey(addr)
	}
	return raw
}

func (t *loginThrottle) pruneLocked(now time.Time) {
	for k, e := range t.hits {
		if now.Sub(e.start) > t.window {
			delete(t.hits, k)
		}
	}
}

func (t *loginThrottle) allowCheck(key string) bool {
	if t == nil {
		return true
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(now)
	e := t.hits[key]
	if e == nil {
		return true
	}
	return e.n < t.limit
}

func (t *loginThrottle) recordFailure(key string) {
	if t == nil {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(now)
	if e := t.hits[key]; e != nil {
		e.n++
		e.last = now
		return
	}
	// 表满时淘汰最久未更新条目而非无条件放行，防止洪泛填满表后绕过限速。
	for len(t.hits) >= t.maxCap {
		var oldestKey string
		var oldestTime time.Time
		for k, e := range t.hits {
			if oldestKey == "" || e.last.Before(oldestTime) {
				oldestKey = k
				oldestTime = e.last
			}
		}
		if oldestKey == "" {
			break
		}
		delete(t.hits, oldestKey)
	}
	t.hits[key] = &throttleEntry{n: 1, start: now, last: now}
}

func (t *loginThrottle) reset(key string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.hits, key)
	t.mu.Unlock()
}

// Service 管理登录会话、角色守卫、账号列表与两步验证安全设置。
type Service struct {
	pool        *pgxpool.Pool
	secret      []byte
	renderer    *ui.Renderer
	limiter     *loginThrottle
	acctLimiter *loginThrottle
}

func NewService(pool *pgxpool.Pool, secret []byte, renderer *ui.Renderer) *Service {
	return &Service{
		pool:        pool,
		secret:      secret,
		renderer:    renderer,
		limiter:     newLoginThrottle(20, 5*time.Minute),
		acctLimiter: newLoginThrottle(10, 5*time.Minute),
	}
}

func (s *Service) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/login", s.LoginPage)
	mux.HandleFunc("POST /admin/login", s.Login)
	mux.HandleFunc("POST /admin/logout", s.Logout)

	mux.HandleFunc("GET /admin/users", s.RequireBoss(s.UsersList))
	mux.HandleFunc("GET /admin/users/new", s.RequireBoss(s.UserNew))
	mux.HandleFunc("GET /admin/users/{id}/edit", s.RequireBoss(s.UserEdit))
	mux.HandleFunc("POST /admin/users", s.RequireBoss(s.UserSave))
	mux.HandleFunc("POST /admin/users/{id}/toggle", s.RequireBoss(s.UserToggle))
	mux.HandleFunc("POST /admin/users/{id}/reset-2fa", s.RequireBoss(s.UserReset2FA))
	mux.HandleFunc("POST /admin/users/{id}/delete", s.RequireBoss(s.UserDelete))

	mux.HandleFunc("GET /account/security", s.RequireAny(s.AccountSecurity))
	mux.HandleFunc("POST /account/security/password", s.RequireAny(s.AccountChangePassword))
	mux.HandleFunc("POST /account/security/logout-all", s.RequireAny(s.AccountLogoutAll))
	mux.HandleFunc("POST /account/security/2fa/setup", s.RequireAny(s.AccountStart2FA))
	mux.HandleFunc("POST /account/security/2fa/enable", s.RequireAny(s.AccountEnable2FA))
	mux.HandleFunc("POST /account/security/2fa/disable", s.RequireAny(s.AccountDisable2FA))
}

func (s *Service) Guards() ui.Guards {
	return ui.Guards{
		Auth: s.RequireAuth,
		Boss: s.RequireBoss,
		Any:  s.RequireAny,
	}
}

func (s *Service) signToken(domain string, uid, ver int64, exp time.Time) string {
	v := strconv.FormatInt(uid, 10) + "." + strconv.FormatInt(ver, 10) + "." + strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(domain + "|" + v))
	return v + "." + hex.EncodeToString(mac.Sum(nil))
}

func (s *Service) SignSession(uid, ver int64, exp time.Time) string {
	return s.signToken("session", uid, ver, exp)
}

func (s *Service) parseToken(domain, value string) (uid, ver int64, ok bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 4 {
		return 0, 0, false
	}
	uid, err1 := strconv.ParseInt(parts[0], 10, 64)
	ver, err2 := strconv.ParseInt(parts[1], 10, 64)
	exp, err3 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || uid <= 0 || ver < 0 || time.Now().Unix() > exp {
		return 0, 0, false
	}
	if !hmac.Equal([]byte(s.signToken(domain, uid, ver, time.Unix(exp, 0))), []byte(value)) {
		return 0, 0, false
	}
	return uid, ver, true
}

func (s *Service) setChallenge(w http.ResponseWriter, r *http.Request, u User) {
	http.SetCookie(w, &http.Cookie{
		Name: ChallengeCookie, Value: s.signToken("challenge", u.ID, u.AuthVersion, time.Now().Add(challengeTTL)),
		Path: "/admin/login", MaxAge: int(challengeTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: platform.IsSecureRequest(r),
	})
}

func (s *Service) challenge(r *http.Request) (uid, ver int64, ok bool) {
	c, err := r.Cookie(ChallengeCookie)
	if err != nil {
		return 0, 0, false
	}
	return s.parseToken("challenge", c.Value)
}

func clearCookie(w http.ResponseWriter, r *http.Request, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: path, MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: platform.IsSecureRequest(r),
	})
}

type ctxUserKey struct{}

// CurrentUser 解析 cookie 并优先复用本次请求上下文内已查库的 User，减少重复回库。
func (s *Service) CurrentUser(r *http.Request) *User {
	if s == nil || s.pool == nil || r == nil {
		return nil
	}
	if cached, ok := r.Context().Value(ctxUserKey{}).(*User); ok {
		return cached
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return nil
	}
	uid, ver, ok := s.parseToken("session", c.Value)
	if !ok {
		return nil
	}
	u, err := GetUser(r.Context(), s.pool, uid)
	if err != nil || !u.Active || u.AuthVersion != ver {
		return nil
	}
	return &u
}

func (s *Service) CurrentUserView(r *http.Request) *ui.UserView {
	u := s.CurrentUser(r)
	if u == nil {
		return nil
	}
	return u.View()
}

func (s *Service) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.CurrentUser(r)
		if u == nil {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		if !u.CanBackend() {
			http.Redirect(w, r, u.Landing(), http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(platform.WithActor(r.Context(), u.Username), ctxUserKey{}, u)
		next(w, r.WithContext(ctx))
	}
}

func (s *Service) RequireBoss(next http.HandlerFunc) http.HandlerFunc {
	return s.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		if u := s.CurrentUser(r); u == nil || !u.IsBoss() {
			platform.RedirectErr(w, r, "/admin/", "只有 BOSS 有权限执行此操作")
			return
		}
		next(w, r)
	})
}

func (s *Service) RequireAny(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.CurrentUser(r)
		if u == nil {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(platform.WithActor(r.Context(), u.Username), ctxUserKey{}, u)
		next(w, r.WithContext(ctx))
	}
}

type loginPageData struct {
	ui.Layout
	Username  string
	TwoFactor bool
}

type usersPageData struct {
	ui.Layout
	Users []User
}

type userFormPageData struct {
	ui.Layout
	User User
}

type (
	LoginPage    = loginPageData
	UsersPage    = usersPageData
	UserFormPage = userFormPageData
)

func noStore(w http.ResponseWriter) { w.Header().Set("Cache-Control", "no-store") }

func (s *Service) LoginPage(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.URL.Query().Get("back") != "" {
		clearCookie(w, r, ChallengeCookie, "/admin/login")
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	if u := s.CurrentUser(r); u != nil {
		clearCookie(w, r, ChallengeCookie, "/admin/login")
		http.Redirect(w, r, u.Landing(), http.StatusSeeOther)
		return
	}
	if uid, ver, ok := s.challenge(r); ok {
		if u, err := GetUser(r.Context(), s.pool, uid); err == nil && u.Active && u.TwoFactor && u.AuthVersion == ver {
			if s.renderer != nil {
				s.renderer.Render(w, r, "admin/login.html", &loginPageData{
					Layout:    ui.Layout{Title: "两步验证", Auth: true},
					TwoFactor: true,
					Username:  u.Username,
				})
			}
			return
		}
		clearCookie(w, r, ChallengeCookie, "/admin/login")
	}
	if s.renderer != nil {
		s.renderer.Render(w, r, "admin/login.html", &loginPageData{Layout: ui.Layout{Title: "登录", Auth: true}})
	}
}

func (s *Service) Login(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.FormValue("action") == "back" {
		clearCookie(w, r, ChallengeCookie, "/admin/login")
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	ipKey := clientIPKey(r)
	if !s.limiter.allowCheck(ipKey) {
		if s.renderer != nil {
			s.renderer.RenderStatus(w, r, http.StatusTooManyRequests, "admin/login.html", &loginPageData{Layout: ui.Layout{Title: "登录", Auth: true, Err: "尝试太频繁，请稍后再试"}})
		} else {
			w.WriteHeader(http.StatusTooManyRequests)
		}
		return
	}
	if r.FormValue("action") == "2fa" {
		s.loginSecondFactor(w, r, ipKey)
		return
	}
	name := strings.TrimSpace(r.FormValue("username"))
	pw := r.FormValue("password")
	acctKey := "u:" + strings.ToLower(name)
	if name != "" && !s.acctLimiter.allowCheck(acctKey) {
		if s.renderer != nil {
			s.renderer.RenderStatus(w, r, http.StatusTooManyRequests, "admin/login.html", &loginPageData{Layout: ui.Layout{Title: "登录", Auth: true, Err: "该账号尝试太频繁，请稍后再试"}, Username: name})
		} else {
			w.WriteHeader(http.StatusTooManyRequests)
		}
		return
	}
	if name == "" || utf8.RuneCountInString(name) > 32 || pw == "" || len(pw) > 1024 {
		s.limiter.recordFailure(ipKey)
		if name != "" {
			s.acctLimiter.recordFailure(acctKey)
		}
		s.loginFailed(w, r, name, "用户名或密码不对")
		return
	}
	u, err := UserByName(r.Context(), s.pool, name)
	if err != nil {
		CheckPassword(pw, dummyHash)
		s.limiter.recordFailure(ipKey)
		s.acctLimiter.recordFailure(acctKey)
		s.loginFailed(w, r, name, "用户名或密码不对")
		return
	}
	// 停用账号同样走完整 PBKDF2 校验耗时，消除时间侧信道。
	pwOK := CheckPassword(pw, u.PasswordHash)
	if !pwOK || !u.Active {
		s.limiter.recordFailure(ipKey)
		s.acctLimiter.recordFailure(acctKey)
		s.loginFailed(w, r, name, "用户名或密码不对")
		return
	}
	if u.TwoFactor {
		s.setChallenge(w, r, u)
		if s.renderer != nil {
			s.renderer.Render(w, r, "admin/login.html", &loginPageData{
				Layout:    ui.Layout{Title: "两步验证", Auth: true},
				TwoFactor: true,
				Username:  u.Username,
			})
		}
		return
	}
	s.limiter.reset(ipKey)
	s.acctLimiter.reset(acctKey)
	s.issueSession(w, r, u)
	http.Redirect(w, r, u.Landing(), http.StatusSeeOther)
}

func (s *Service) loginFailed(w http.ResponseWriter, r *http.Request, name, msg string) {
	if s.renderer != nil {
		s.renderer.RenderStatus(w, r, http.StatusUnauthorized, "admin/login.html", &loginPageData{
			Layout:   ui.Layout{Title: "登录", Auth: true, Err: msg},
			Username: name,
		})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
}

func (s *Service) loginSecondFactor(w http.ResponseWriter, r *http.Request, ipKey string) {
	noStore(w)
	uid, ver, ok := s.challenge(r)
	if !ok {
		s.limiter.recordFailure(ipKey)
		clearCookie(w, r, ChallengeCookie, "/admin/login")
		s.loginFailed(w, r, "", "验证已过期，请重新登录")
		return
	}
	acctKey := fmt.Sprintf("id:%d", uid)
	if !s.acctLimiter.allowCheck(acctKey) {
		if s.renderer != nil {
			s.renderer.RenderStatus(w, r, http.StatusTooManyRequests, "admin/login.html", &loginPageData{
				Layout:    ui.Layout{Title: "两步验证", Auth: true, Err: "验证尝试太频繁，请稍后再试"},
				TwoFactor: true,
			})
		} else {
			w.WriteHeader(http.StatusTooManyRequests)
		}
		return
	}
	code := strings.TrimSpace(r.FormValue("code"))
	if code == "" || len(code) > 64 {
		s.limiter.recordFailure(ipKey)
		s.acctLimiter.recordFailure(acctKey)
		if s.renderer != nil {
			s.renderer.RenderStatus(w, r, http.StatusUnauthorized, "admin/login.html", &loginPageData{
				Layout:    ui.Layout{Title: "两步验证", Auth: true, Err: "请输入动态码或恢复码"},
				TwoFactor: true,
			})
		} else {
			w.WriteHeader(http.StatusUnauthorized)
		}
		return
	}
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		log.Printf("2fa begin tx: %v", err)
		http.Error(w, "验证暂时不可用，请稍后再试", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)
	var u User
	err = tx.QueryRow(ctx, `select id, username, role, auth_version, active, totp_secret <> '' from users where id=$1 for update`, uid).
		Scan(&u.ID, &u.Username, &u.Role, &u.AuthVersion, &u.Active, &u.TwoFactor)
	if err != nil || !u.Active || !u.TwoFactor || u.AuthVersion != ver {
		clearCookie(w, r, ChallengeCookie, "/admin/login")
		s.loginFailed(w, r, "", "验证已过期，请重新登录")
		return
	}
	passed, err := s.ConsumeSecondFactor(ctx, tx, uid, code)
	if err != nil {
		log.Printf("两步验证失败: %v", err)
		http.Error(w, "验证暂时不可用，请稍后再试", http.StatusInternalServerError)
		return
	}
	if !passed {
		var newVer int64
		_ = tx.QueryRow(ctx, `select auth_version from users where id=$1`, uid).Scan(&newVer)
		if err := tx.Commit(ctx); err != nil {
			log.Printf("两步验证失败计数落库: %v", err)
		}
		s.limiter.recordFailure(ipKey)
		s.acctLimiter.recordFailure(acctKey)
		if newVer > u.AuthVersion {
			clearCookie(w, r, ChallengeCookie, "/admin/login")
			s.loginFailed(w, r, "", "失败次数过多，验证挑战已作废，请重新登录")
			return
		}
		if s.renderer != nil {
			s.renderer.RenderStatus(w, r, http.StatusUnauthorized, "admin/login.html", &loginPageData{
				Layout:    ui.Layout{Title: "两步验证", Auth: true, Err: "动态码或恢复码不对"},
				TwoFactor: true,
			})
		} else {
			w.WriteHeader(http.StatusUnauthorized)
		}
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("2fa commit: %v", err)
		http.Error(w, "验证暂时不可用，请稍后再试", http.StatusInternalServerError)
		return
	}
	s.limiter.reset(ipKey)
	s.acctLimiter.reset(acctKey)
	s.acctLimiter.reset("u:" + strings.ToLower(u.Username))
	s.issueSession(w, r, u)
	http.Redirect(w, r, u.Landing(), http.StatusSeeOther)
}

func (s *Service) issueSession(w http.ResponseWriter, r *http.Request, u User) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: s.SignSession(u.ID, u.AuthVersion, time.Now().Add(sessionTTL)),
		Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: platform.IsSecureRequest(r),
	})
	clearCookie(w, r, ChallengeCookie, "/admin/login")
}

func (s *Service) Logout(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, r, SessionCookie, "/")
	clearCookie(w, r, ChallengeCookie, "/admin/login")
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// ---------- 账号管理页面（只有 BOSS） ----------

func (s *Service) UsersList(w http.ResponseWriter, r *http.Request) {
	users, err := ListUsers(r.Context(), s.pool)
	if err != nil {
		log.Printf("list users: %v", err)
		http.Error(w, "读取账号列表失败", http.StatusInternalServerError)
		return
	}
	s.renderer.Render(w, r, "admin/users.html", &usersPageData{
		Layout: ui.Layout{Title: "账号", Nav: "users"},
		Users:  users,
	})
}

func (s *Service) UserNew(w http.ResponseWriter, r *http.Request) {
	s.renderer.Render(w, r, "admin/user_form.html", &userFormPageData{
		Layout: ui.Layout{Title: "新增账号", Nav: "users"},
		User:   User{Active: true, Role: RoleStaff},
	})
}

func (s *Service) UserEdit(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	u, err := GetUser(r.Context(), s.pool, id)
	if err != nil {
		http.Error(w, "找不到这个账号", http.StatusNotFound)
		return
	}
	s.renderer.Render(w, r, "admin/user_form.html", &userFormPageData{
		Layout: ui.Layout{Title: "编辑账号", Nav: "users"},
		User:   u,
	})
}

func (s *Service) UserSave(w http.ResponseWriter, r *http.Request) {
	me := s.CurrentUser(r)
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	u := User{
		ID:       id,
		Username: r.FormValue("username"),
		Role:     r.FormValue("role"),
		Active:   r.FormValue("active") != "",
	}
	renderFail := func(status int, msg string) {
		if s.renderer != nil {
			s.renderer.RenderStatus(w, r, status, "admin/user_form.html", &userFormPageData{
				Layout: ui.Layout{Title: "账号", Nav: "users", Err: msg},
				User:   u,
			})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
	}
	if me != nil && id == me.ID && (u.Role != RoleBoss || !u.Active) {
		renderFail(http.StatusBadRequest, "不能改掉自己的 BOSS 角色或停用自己")
		return
	}
	if _, err := SaveUser(r.Context(), s.pool, u, r.FormValue("password")); err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			renderFail(http.StatusBadRequest, msg)
			return
		}
		log.Printf("save user (%d): %v", id, err)
		renderFail(http.StatusInternalServerError, "保存账号失败，请稍后重试")
		return
	}
	platform.RedirectMsg(w, r, "/admin/users", "账号已保存")
}

func (s *Service) UserToggle(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if me := s.CurrentUser(r); me != nil && me.ID == id {
		platform.RedirectErr(w, r, "/admin/users", "不能停用自己")
		return
	}
	if err := ToggleUser(r.Context(), s.pool, id); err != nil {
		log.Printf("toggle user (%d): %v", id, err)
		http.Error(w, "操作失败", http.StatusInternalServerError)
		return
	}
	platform.RedirectMsg(w, r, "/admin/users", "状态已切换")
}

func (s *Service) UserReset2FA(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var username string
	err := s.pool.QueryRow(r.Context(), `
		update users set totp_secret='', totp_last_step=-1, recovery_hashes='{}',
		                 otp_failures=0, otp_lock_level=0, otp_blocked_until=null,
		                 auth_version=auth_version+1
		where id=$1
		returning username`, id).Scan(&username)
	if errors.Is(err, pgx.ErrNoRows) {
		platform.RedirectErr(w, r, "/admin/users", "找不到这个账号")
		return
	}
	if err != nil {
		log.Printf("reset 2fa user (%d): %v", id, err)
		platform.RedirectErr(w, r, "/admin/users", "重置两步验证失败，请稍后重试")
		return
	}
	log.Printf("security audit: admin %q reset 2fa for user %d (%q)", platform.OperationActor(r.Context()), id, username)
	platform.RedirectMsg(w, r, "/admin/users", fmt.Sprintf("已重置账号「%s」的两步验证", username))
}

func (s *Service) UserDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if me := s.CurrentUser(r); me != nil && me.ID == id {
		platform.RedirectErr(w, r, "/admin/users", "不能删自己")
		return
	}
	if err := DeleteUser(r.Context(), s.pool, id); err != nil {
		log.Printf("delete user (%d): %v", id, err)
		http.Error(w, "删除失败", http.StatusInternalServerError)
		return
	}
	platform.RedirectMsg(w, r, "/admin/users", "账号已删除")
}
