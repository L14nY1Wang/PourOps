package app

import (
	"context"
	"io/fs"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/auth"
	"pourops/internal/catalog"
	"pourops/internal/event"
	"pourops/internal/inventory"
	"pourops/internal/lookup"
	"pourops/internal/platform"
	"pourops/internal/pos"
	"pourops/internal/report"
	"pourops/internal/screen"
	"pourops/internal/settings"
	"pourops/internal/ui"
	"pourops/web"
)

// Deps 汇总应用 HTTP 处理器装配所需的运行期依赖。
type Deps struct {
	Pool         *pgxpool.Pool
	Settings     *settings.SettingsStore
	Bus          *event.EventBus
	Webhooks     *event.WebhookDispatcher
	ScreenHub    *screen.Hub
	Assets       *ui.AssetManifest
	Renderer     *ui.Renderer
	Posters      *catalog.PosterStore
	Secret       []byte
	JiuheBaseURL string
}

// NewAssets 基于内嵌静态资源构建带内容短指纹的 AssetManifest。
func NewAssets() (*ui.AssetManifest, error) {
	staticRoot, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		return nil, err
	}
	return ui.NewAssetManifest(staticRoot)
}

// NewRenderer 基于内嵌模板构建 HTML 渲染器。
func NewRenderer(assets *ui.AssetManifest, brandFn func() string) *ui.Renderer {
	return ui.MustRenderer(web.TemplatesFS, assets, brandFn)
}

// NewHandler 装配全部领域服务、注册路由并套上跨站写请求防护中间件。
func NewHandler(deps Deps) http.Handler {
	mux := http.NewServeMux()
	if deps.Assets != nil {
		mux.Handle("GET /static/", deps.Assets.Handler())
	}

	authSvc := auth.NewService(deps.Pool, deps.Secret, deps.Renderer)
	var catsFn func() []string
	if deps.Settings != nil {
		catsFn = deps.Settings.BeerCategories
	}
	if deps.Renderer != nil {
		deps.Renderer.SetCurrentUserFn(authSvc.CurrentUserView)
		if deps.Settings != nil {
			deps.Renderer.SetCategoriesFn(deps.Settings.BeerCategoryOptions)
		}
	}
	guards := authSvc.Guards()

	catalogSvc := catalog.NewService(deps.Pool, deps.Settings, catalog.NewDefaultCodecRegistryWithOptions(catsFn, deps.Posters), deps.Posters)
	localLookup := lookup.NewLocalCatalogProvider(deps.Pool)
	localLookup.SetCategoriesFn(catsFn)
	jiuheLookup := lookup.NewJiuheProvider(deps.JiuheBaseURL, nil)
	jiuheLookup.SetCategoriesFn(catsFn)
	lookupSvc := lookup.NewService(localLookup, jiuheLookup)
	// screen.Service.PlaylistJSON 从 static/screen/playlist.json 读取，需传入根 StaticFS。
	screenSvc := screen.NewService(deps.Pool, deps.ScreenHub, deps.Settings, deps.Renderer, web.StaticFS)
	invSvc := inventory.NewService(deps.Pool, deps.Renderer, deps.Settings, deps.Bus, deps.Posters)
	posSvc := pos.NewService(deps.Pool, deps.Renderer, deps.Bus, pos.ManualGateway{}, pos.NoopPrinter{})
	reportSvc := report.NewService(deps.Pool, deps.Renderer, deps.Settings)
	settingsSvc := settings.NewService(deps.Settings, deps.Webhooks, deps.Renderer)
	settingsSvc.SetCatalogCountFn(func(ctx context.Context) (int, error) {
		if deps.Pool == nil {
			return 0, nil
		}
		return catalog.CountCatalogBeers(ctx, deps.Pool)
	})

	authSvc.RegisterRoutes(mux)
	screenSvc.RegisterRoutes(mux)
	catalogSvc.RegisterRoutes(mux, guards)
	lookupSvc.RegisterRoutes(mux, guards)
	invSvc.RegisterRoutes(mux, guards)
	posSvc.RegisterRoutes(mux, guards)
	reportSvc.RegisterRoutes(mux, guards)
	settingsSvc.RegisterRoutes(mux, guards)

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if deps.Pool != nil {
			if u := authSvc.CurrentUserView(r); u != nil {
				http.Redirect(w, r, u.Landing(), http.StatusSeeOther)
				return
			}
		}
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
	})

	return platform.CrossOriginProtect(mux)
}
