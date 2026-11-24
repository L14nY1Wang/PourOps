package ui

import (
	"html/template"
	"io/fs"
	"log"
	"net/http"

	"pourops/internal/platform"
)

const AdminLayoutFile = "templates/layouts/admin.html"

var DefaultPageFiles = []string{
	"admin/login.html",
	"admin/dashboard.html",
	"admin/analytics.html",
	"admin/beers.html",
	"admin/beer_form.html",
	"admin/purchase.html",
	"admin/kegs.html",
	"admin/keg.html",
	"admin/keg_form.html",
	"admin/retail.html",
	"admin/retail_form.html",
	"admin/records.html",
	"admin/users.html",
	"admin/user_form.html",
	"admin/account_security.html",
	"admin/settings.html",
	"pos/pos.html",
	"pos/pos_pay.html",
	"screen/screen.html",
}

var standaloneShells = map[string]string{
	"pos/pos.html":       "pos",
	"pos/pos_pay.html":   "pos_pay",
	"screen/screen.html": "screen",
}

// Renderer 封装内嵌 HTML 模板、{{asset}} 指纹解析、动态酒款分类及当前登录用户信息注入。
type Renderer struct {
	pages        map[string]*template.Template
	brandFn      func() string
	categoriesFn func() []platform.Option
	currentUser  func(*http.Request) *UserView
}

func NewRenderer(templateFS fs.FS, assets *AssetManifest, brandFn func() string, pageFiles ...string) (*Renderer, error) {
	if len(pageFiles) == 0 {
		pageFiles = DefaultPageFiles
	}
	funcs := template.FuncMap{
		"sub": func(a, b int) int { return a - b },
		"asset": func(p string) string {
			if assets == nil {
				return p
			}
			return assets.URL(p)
		},
	}

	pages := make(map[string]*template.Template, len(pageFiles))
	for _, f := range pageFiles {
		if _, ok := standaloneShells[f]; ok {
			t, err := template.New(f).Funcs(funcs).ParseFS(templateFS, "templates/"+f)
			if err != nil {
				return nil, err
			}
			pages[f] = t
			continue
		}
		t, err := template.New(f).Funcs(funcs).ParseFS(templateFS, AdminLayoutFile, "templates/"+f)
		if err != nil {
			return nil, err
		}
		pages[f] = t
	}
	return &Renderer{pages: pages, brandFn: brandFn}, nil
}

func MustRenderer(templateFS fs.FS, assets *AssetManifest, brandFn func() string, pageFiles ...string) *Renderer {
	r, err := NewRenderer(templateFS, assets, brandFn, pageFiles...)
	if err != nil {
		log.Fatalf("加载模板失败: %v", err)
	}
	return r
}

func (re *Renderer) SetCurrentUserFn(fn func(*http.Request) *UserView) {
	re.currentUser = fn
}

func (re *Renderer) SetCategoriesFn(fn func() []platform.Option) {
	re.categoriesFn = fn
}

func (re *Renderer) Render(w http.ResponseWriter, req *http.Request, page string, data Page) {
	re.exec(w, req, page, "layout", data)
}

func (re *Renderer) RenderShell(w http.ResponseWriter, req *http.Request, page, tmplName string, data Page) {
	re.exec(w, req, page, tmplName, data)
}

func (re *Renderer) exec(w http.ResponseWriter, req *http.Request, page, tmplName string, data Page) {
	t, ok := re.pages[page]
	if !ok {
		log.Printf("template %s not found", page)
		http.Error(w, "页面模板不存在", http.StatusInternalServerError)
		return
	}
	if data != nil {
		l := data.LayoutPtr()
		if l.Brand == "" && re.brandFn != nil {
			l.Brand = re.brandFn()
		}
		if len(l.Categories) == 0 {
			if re.categoriesFn != nil {
				l.Categories = re.categoriesFn()
			}
			if len(l.Categories) == 0 {
				l.Categories = platform.Categories
			}
		}
		if l.Me == nil && re.currentUser != nil && req != nil {
			l.Me = re.currentUser(req)
		}
		if l.Me != nil && len(l.Roles) == 0 {
			l.Roles = Roles
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, tmplName, data); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}
