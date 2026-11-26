package ui

import (
	"net/http"
	"strings"

	"pourops/internal/platform"
)

const (
	RoleBoss    = "boss"
	RoleManager = "manager"
	RoleStaff   = "staff"
)

var Roles = []platform.Option{
	{Value: RoleBoss, Label: "BOSS"},
	{Value: RoleManager, Label: "店长"},
	{Value: RoleStaff, Label: "店员"},
}

func RoleLabel(v string) string {
	for _, r := range Roles {
		if r.Value == v {
			return r.Label
		}
	}
	return v
}

func ValidRole(v string) bool {
	return v == RoleBoss || v == RoleManager || v == RoleStaff
}

// UserView 是跨模块页面布局与鉴权守卫共用的只读账号视图（不含密码哈希与 2FA 密钥）。
type UserView struct {
	ID          int64
	Username    string
	Role        string
	Active      bool
	AuthVersion int64
	TwoFactor   bool
}

func (u UserView) RoleName() string { return RoleLabel(u.Role) }
func (u UserView) IsBoss() bool     { return u.Role == RoleBoss }
func (u UserView) CanBackend() bool { return u.Role == RoleBoss || u.Role == RoleManager }

// Initial 按 Unicode 字符提取头像首字母，避免中文用户名按字节截断产生乱码。
func (u UserView) Initial() string {
	s := strings.TrimSpace(u.Username)
	for _, r := range s {
		return string(r)
	}
	return "?"
}

func (u UserView) Landing() string {
	if u.CanBackend() {
		return "/admin/"
	}
	return "/pos/"
}

// Layout 是所有后台与独立页面共用的外壳字段集合，具体页面只需嵌入本结构即可实现 Page。
type Layout struct {
	Title      string
	Brand      string
	Nav        string
	Auth       bool
	Msg        string
	Err        string
	Me         *UserView
	Roles      []platform.Option
	Categories []platform.Option
}

func (l *Layout) LayoutPtr() *Layout { return l }

// HasCategory 判断分类值是否仍存在于当前可选预设列表中，供编辑页保留历史自定义分类。
func (l *Layout) HasCategory(v string) bool {
	target := strings.TrimSpace(v)
	if target == "" {
		return true
	}
	for _, c := range l.Categories {
		if strings.EqualFold(c.Value, target) {
			return true
		}
	}
	return false
}

// Page 让各个强类型页面 ViewModel 统一接入 Renderer。
type Page interface {
	LayoutPtr() *Layout
}

// Guards 由 auth 模块构建，分发到业务路由注册处。
type Guards struct {
	Auth func(http.HandlerFunc) http.HandlerFunc
	Boss func(http.HandlerFunc) http.HandlerFunc
	Any  func(http.HandlerFunc) http.HandlerFunc
}
