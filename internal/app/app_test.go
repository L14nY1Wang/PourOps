package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pourops/internal/auth"
	"pourops/internal/catalog"
	"pourops/internal/event"
	"pourops/internal/inventory"
	"pourops/internal/platform"
	"pourops/internal/pos"
	"pourops/internal/report"
	"pourops/internal/screen"
	"pourops/internal/settings"
	"pourops/internal/ui"
)

// TestAllTemplatesRender 验证全部 20 个内嵌模板均可在真实静态资源清单下零报错渲染并输出资源哈希。
func TestAllTemplatesRender(t *testing.T) {
	assets, err := NewAssets()
	if err != nil {
		t.Fatal(err)
	}
	renderer := NewRenderer(assets, func() string { return "PourOps" })

	now := time.Date(2026, 11, 23, 21, 30, 0, 0, platform.ReportZone)
	abv, ut := 6.8, 4.47
	ibu := 72
	me := &ui.UserView{ID: 1, Username: "boss", Role: ui.RoleBoss, Active: true, TwoFactor: true}
	pricing := settings.PricingConfig{SellablePct: 85, MarginPct: 55}

	beer := catalog.Beer{
		ID: 1, No: "01", Brewery: "树屋", Name: "尤利乌斯", Style: "IPA - Hazy", Category: "hop",
		ABV: &abv, IBU: &ibu, UT: &ut, Price150: 2800, Price330: 5800, Price500: 7800,
		Tags: []string{"柑橘", "果汁感"}, SalesTags: []string{"本周最佳"}, OnScreen: true,
	}
	catItem := catalog.CatalogBeer{
		ID: 3, Brewery: "树屋", Name: "尤利乌斯", Style: "IPA - Hazy", Category: "hop",
		ABV: &abv, IBU: &ibu, UT: &ut, CapacityML: 20000, CostCents: 120000,
		Price150: 2800, Price330: 5800, Price500: 7800,
		Tags: []string{"柑橘", "果汁感"}, PosterURL: "https://assets.untappd.com/x.jpeg",
		UpdatedAt: now,
	}
	keg := inventory.Keg{
		ID: 10, BeerID: 1, BeerNo: "01", Brewery: "树屋", BeerName: "尤利乌斯", KegNo: "K-01",
		CapacityML: 20000, CostCents: 120000, PlacedAt: &now, TappedAt: &now,
		SoldCups: 22, SoldML: 7260, RevenueCents: 127600, PourCount: 14,
	}
	finKeg := keg
	finKeg.FinishedAt = &now
	finKeg.SoldML = 16000
	finKeg.RevenueCents = 240000

	item := inventory.RetailItem{ID: 5, Name: "健力士罐装", Category: "罐装", Spec: "440ml", PriceCents: 3800, CostCents: 2100, Stock: 2, Active: true}
	ord := report.RecordOrder{ID: 88, Spot: "bar-1", OpenedAt: now, Total: 14600, Paid: 8800, Pending: 5800, Status: "partial"}

	cases := []struct {
		name      string
		file      string
		shell     string
		data      ui.Page
		wantSubs  []string
		notLayout bool
	}{
		{"login", "admin/login.html", "layout", &auth.LoginPage{Layout: ui.Layout{Title: "登录", Auth: true}}, []string{`/static/brand/pourops.svg?v=`, `/static/brand/pourops-light.svg?v=`, `/static/brand/pourops-dark.svg?v=`}, false},
		{"login-2fa", "admin/login.html", "layout", &auth.LoginPage{Layout: ui.Layout{Title: "登录", Auth: true}, TwoFactor: true, Username: "boss"}, nil, false},
		{"dashboard", "admin/dashboard.html", "layout", &report.DashboardPage{
			Layout:       ui.Layout{Title: "概览", Nav: "dashboard", Me: me},
			ReportPeriod: "7d", ReportStart: "2026-11-17", ReportEnd: "2026-11-23", Pricing: pricing,
			Sales: report.SalesInsights{
				Current: report.SalesTotals{Revenue: 520000, Cost: 210000, Qty: 86},
				Draft:   report.SalesTotals{Revenue: 380000, Cost: 150000, Qty: 62},
				Retail:  report.SalesTotals{Revenue: 140000, Cost: 60000, Qty: 24},
				Trend:   []report.SalesTrendDay{{Date: "2026-11-22", Revenue: 240000, Cost: 95000}, {Date: "2026-11-23", Revenue: 280000, Cost: 115000}},
				Top:     []report.SalesTopItem{{Name: "树屋 · 尤利乌斯", Unit: "杯", Qty: 32, Revenue: 185600, Cost: 64000}},
			},
			POS:      report.POSInsights{Received: 480000, Orders: 24, Payments: 29, Hours: []report.POSHour{{Hour: 20, Received: 180000, Payments: 10}, {Hour: 21, Received: 300000, Payments: 19}}},
			Overview: report.Overview{LowCount: 1, OutCount: 0, Untapped: 2, ActiveKegs: 4, RetailCount: 12, StockValue: 340000, LowStock: []inventory.RetailItem{item}},
			Kegs:     []inventory.Keg{keg},
		}, []string{"含 85% 出酒回本进度", `/static/brand/pourops.svg?v=`, `/static/brand/pourops-light.svg?v=`, `/static/brand/pourops-dark.svg?v=`}, false},
		{"beers", "admin/beers.html", "layout", &inventory.BeerListPage{
			Layout: ui.Layout{Title: "酒单 · 桶装", Nav: "beers", Me: me}, NextNo: 2, Pricing: pricing,
			Rows: []inventory.BeerRow{{Beer: beer, Keg: &keg, Stock: 2}, {Beer: beer, LastKeg: &finKeg}},
		}, nil, false},
		{"beer_form", "admin/beer_form.html", "layout", &inventory.BeerFormPage{
			Layout: ui.Layout{Title: "编辑酒款", Nav: "beers", Me: me, Categories: platform.Categories}, Beer: beer, Keg: keg, CostInput: "1200", Pricing: pricing,
		}, []string{`data-default-sellable="85"`, `/static/js/beer-lookup.js?v=`, `/static/js/keg-pricing.js?v=`}, false},
		{"purchase", "admin/purchase.html", "layout", &inventory.PurchasePage{
			Layout: ui.Layout{Title: "新增酒款", Nav: "beers", Me: me, Categories: platform.Categories}, Beer: beer, Keg: keg, KegCount: 2, CostInput: "1200", Pricing: pricing,
		}, []string{`data-default-margin="55"`, `/static/js/beer-lookup.js?v=`, `/static/js/keg-pricing.js?v=`}, false},
		{"catalog", "admin/catalog.html", "layout", &catalog.CatalogListPage{
			Layout: ui.Layout{Title: "历史酒单", Nav: "catalog", Me: me, Categories: platform.Categories}, Catalog: []catalog.CatalogBeer{catItem}, CatalogTotal: 1,
		}, nil, false},
		{"catalog_form", "admin/catalog_form.html", "layout", &catalog.CatalogFormPage{
			Layout: ui.Layout{Title: "编辑历史酒款", Nav: "catalog", Me: me, Categories: platform.Categories}, CatalogItem: catItem, CostInput: "1200", Pricing: pricing,
		}, []string{`/static/js/beer-lookup.js?v=`, `/static/js/keg-pricing.js?v=`}, false},
		{"kegs", "admin/kegs.html", "layout", &inventory.KegListPage{
			Layout: ui.Layout{Title: "桶装盈亏", Nav: "kegs", Me: me}, Kegs: []inventory.Keg{keg, finKeg}, KegSummary: inventory.SummarizeKegsWithConfig([]inventory.Keg{keg, finKeg}, pricing), Status: "all", Verdict: "all", Pricing: pricing,
		}, []string{"核算标准：85% 可售（预留 15% 损耗）", "55% 目标营收"}, false},
		{"keg", "admin/keg.html", "layout", &inventory.KegDetailPage{
			Layout: ui.Layout{Title: keg.Title(), Nav: "kegs", Me: me}, Keg: keg, Beer: beer, KegPricing: keg.CupPricingWithConfig(beer, pricing), Pours: []inventory.Pour{{ID: 1, KegID: 10, Cups: 2, ML: 660, AmountCents: 11600, PouredAt: now}}, Pricing: pricing,
		}, []string{`data-default-sellable="85"`, `data-default-margin="55"`, `/static/js/keg-pricing.js?v=`}, false},
		{"keg-finished", "admin/keg.html", "layout", &inventory.KegDetailPage{
			Layout: ui.Layout{Title: finKeg.Title(), Nav: "kegs", Me: me}, Keg: finKeg, Beer: beer, KegPricing: finKeg.CupPricingWithConfig(beer, pricing), Pricing: pricing,
		}, nil, false},
		{"keg_form", "admin/keg_form.html", "layout", &inventory.KegFormPage{
			Layout: ui.Layout{Title: "编辑酒桶", Nav: "kegs", Me: me}, Keg: keg, Beers: []catalog.Beer{beer},
		}, nil, false},
		{"retail", "admin/retail.html", "layout", &inventory.RetailListPage{
			Layout: ui.Layout{Title: "零售", Nav: "retail", Me: me}, Retail: []inventory.RetailItem{item}, RetailCats: inventory.RetailCategories,
			Stats: inventory.RetailStats{TodayRev: 68000, TodayCost: 32000, MonthRev: 540000, MonthCost: 250000, StockValue: 340000, LowCount: 1, SKUs: 8},
		}, nil, false},
		{"retail_form", "admin/retail_form.html", "layout", &inventory.RetailFormPage{
			Layout: ui.Layout{Title: "编辑零售商品", Nav: "retail", Me: me}, Item: item, RetailCats: inventory.RetailCategories, Moves: []inventory.StockMove{{Delta: 12, Reason: "入库", CreatedAt: now}},
		}, nil, false},
		{"records-orders", "admin/records.html", "layout", &report.RecordsViewPage{
			Layout:  ui.Layout{Title: "销售订单", Nav: "orders", Me: me},
			Records: &report.RecordsPage{Mode: "orders", Statuses: report.RecordOrderStatuses, Orders: []report.RecordOrder{ord}, Page: 1},
		}, nil, false},
		{"records-order", "admin/records.html", "layout", &report.RecordsViewPage{
			Layout: ui.Layout{Title: "订单 #88", Nav: "orders", Me: me},
			Records: &report.RecordsPage{
				Mode: "order", Order: &ord,
				Checkouts: []report.RecordCheckout{{Payment: report.RecordPayment{ID: 301, Amount: 8800, Items: 2, Actor: "boss", CreatedAt: now}, Items: []report.RecordItem{{Kind: "beer", Name: "尤利乌斯", UnitCents: 5800, Qty: 1, ML: 330}}}},
				Unpaid:    []report.RecordItem{{Kind: "retail", Name: "健力士罐装", UnitCents: 3800, Qty: 1}},
			},
		}, nil, false},
		{"records-ops", "admin/records.html", "layout", &report.RecordsViewPage{
			Layout: ui.Layout{Title: "营运流水", Nav: "operations", Me: me},
			Records: &report.RecordsPage{
				Mode: "operations", Kinds: report.RecordKindOptions, Page: 1,
				Events: []report.LedgerEvent{{ID: 1, OccurredAt: now, Kind: platform.OpTap, Subject: "树屋 · 尤利乌斯", Detail: "上架 01", Actor: "boss"}},
			},
		}, nil, false},
		{"users", "admin/users.html", "layout", &auth.UsersPage{
			Layout: ui.Layout{Title: "账号", Nav: "users", Me: me}, Users: []auth.User{{ID: 1, Username: "boss", Role: auth.RoleBoss, Active: true, TwoFactor: true}},
		}, nil, false},
		{"user_form", "admin/user_form.html", "layout", &auth.UserFormPage{
			Layout: ui.Layout{Title: "编辑账号", Nav: "users", Me: me, Roles: ui.Roles}, User: auth.User{ID: 1, Username: "boss", Role: auth.RoleBoss, Active: true},
		}, nil, false},
		{"security", "admin/account_security.html", "layout", &auth.SecurityPage{
			Layout: ui.Layout{Title: "账号安全", Nav: "security", Me: me}, Sec: auth.SecurityView{TwoFactor: false, Setup: true, Secret: "JBSWY3DPEHPK3PXP"},
		}, nil, false},
		{"security-recovery", "admin/account_security.html", "layout", &auth.SecurityPage{
			Layout: ui.Layout{Title: "账号安全", Nav: "security", Me: me}, Sec: auth.SecurityView{Recovery: []string{"ABCD-1234", "EFGH-5678"}},
		}, nil, false},
		{"settings", "admin/settings.html", "layout", &settings.SettingsPage{
			Layout:         ui.Layout{Title: "系统设置", Nav: "settings", Me: me},
			Settings:       settings.StoreSettings{Brand: "PourOps", DefaultSellablePct: 80, DefaultMarginPct: 50, WebhookEvents: []string{string(event.EventOrderPaid)}},
			EventOptions:   event.WebhookEventOptions,
			HasSecret:      true,
			SelectedEvents: map[string]bool{string(event.EventOrderPaid): true},
		}, []string{`name="default_sellable_pct"`, `name="webhook_url"`}, false},
		{"pos-spots", "pos/pos.html", "pos", &pos.PosPage{
			Layout:   ui.Layout{Me: me},
			Spots:    []pos.Spot{{Key: "bar-1", Kind: "bar", Label: "吧台 1", OrderID: 88, Items: 2, Total: 9600}, {Key: "table-1", Kind: "table", Label: "小桌 1"}},
			PosBeers: []pos.PosBeer{{KegID: 10, No: "01", Brewery: "树屋", Name: "尤利乌斯", Style: "Hazy IPA", Price150: 2800, Price330: 5800, Price500: 7800}},
			PosItems: []inventory.RetailItem{item},
		}, []string{`/static/brand/pourops.svg?v=`, `/static/brand/pourops-dark.svg?v=`}, true},
		{"pos-order", "pos/pos.html", "pos", &pos.PosPage{
			Layout:     ui.Layout{Me: me},
			PosOrderID: 88,
			Order:      &pos.OrderView{ID: 88, Spot: "bar-1", Label: "吧台 1", Unpaid: []pos.OrderItem{{ID: 1, Kind: "beer", Name: "尤利乌斯", UnitCents: 5800, Qty: 2, ML: 330}}, Total: 11600},
			PosBeers:   []pos.PosBeer{{KegID: 10, No: "01", Brewery: "树屋", Name: "尤利乌斯", Style: "Hazy IPA", Price150: 2800, Price330: 5800, Price500: 7800}},
			PosItems:   []inventory.RetailItem{item},
		}, nil, true},
		{"pos_pay", "pos/pos_pay.html", "pos_pay", &pos.PayPage{
			Layout: ui.Layout{Me: me},
			Pay:    &pos.Payment{ID: 301, OrderID: 88, Label: "吧台 1", Amount: 11600, Items: 2},
		}, []string{`/static/brand/pourops.svg?v=`, `/static/brand/pourops-dark.svg?v=`}, true},
		{"screen", "screen/screen.html", "screen", &screen.ScreenPage{
			Layout: ui.Layout{Brand: "北岸精酿"},
		}, []string{`<title>酒单 - 北岸精酿</title>`, `/static/brand/pourops.svg?v=`, `/static/brand/pourops-dark.svg?v=`}, true},
	}

	covered := make(map[string]bool, len(ui.DefaultPageFiles))
	for _, tc := range cases {
		covered[tc.file] = true
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if tc.shell == "layout" {
				renderer.Render(rec, nil, tc.file, tc.data)
			} else {
				renderer.RenderShell(rec, nil, tc.file, tc.shell, tc.data)
			}
			body := rec.Body.String()
			if rec.Code != http.StatusOK || len(body) == 0 {
				t.Fatalf("%s 渲染失败: status=%d len=%d", tc.file, rec.Code, len(body))
			}
			if !tc.notLayout {
				for _, wantAsset := range []string{
					"/static/css/admin.bundle.css?v=",
					"/static/js/theme.js?v=",
					"/static/js/admin.js?v=",
				} {
					if !strings.Contains(body, wantAsset) {
						t.Fatalf("%s 未通过 {{asset}} 输出 %s", tc.file, wantAsset)
					}
				}
			}
			if strings.Contains(body, `<svg class="brand-mark"`) {
				t.Fatalf("%s 不应再包含内联 brand-mark SVG", tc.file)
			}
			lower := strings.ToLower(body)
			for _, legacy := range []string{"ioe", "heroui", "jquery", "sweetalert"} {
				if strings.Contains(lower, legacy) {
					t.Fatalf("%s 不应包含历史残留词 %q", tc.file, legacy)
				}
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(body, sub) {
					t.Fatalf("%s 缺少预期片段 %q", tc.file, sub)
				}
			}
		})
	}

	for _, f := range ui.DefaultPageFiles {
		if !covered[f] {
			t.Fatalf("DefaultPageFiles 中的模板 %s 未被 TestAllTemplatesRender 覆盖", f)
		}
	}
}

func TestNewHandlerSmoke(t *testing.T) {
	assets, err := NewAssets()
	if err != nil {
		t.Fatal(err)
	}
	settingsStore := settings.NewSettingsStore(nil, "PourOps")
	bus := event.NewEventBus()
	defer bus.Close(t.Context())
	screenHub := screen.NewHub(bus)
	renderer := NewRenderer(assets, settingsStore.Brand)

	h := NewHandler(Deps{
		Settings:     settingsStore,
		Bus:          bus,
		ScreenHub:    screenHub,
		Assets:       assets,
		Renderer:     renderer,
		Secret:       []byte("0123456789abcdef0123456789abcdef"),
		JiuheBaseURL: "https://mall.jiuhehe.com/api",
	})

	// 1. CSS bundle & alias + SVG brand assets + JS
	for _, path := range []string{"/static/css/admin.bundle.css", "/static/css/admin.css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/css") || rec.Header().Get("ETag") == "" {
			t.Fatalf("GET %s = %d ct=%q etag=%q", path, rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("ETag"))
		}
	}
	for _, path := range []string{"/static/brand/pourops.svg", "/static/brand/pourops-light.svg", "/static/brand/pourops-dark.svg"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "image/svg+xml") || !strings.Contains(rec.Body.String(), "<svg") {
			t.Fatalf("GET %s = %d ct=%q bodyLen=%d", path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
		}
	}
	for _, path := range []string{"/static/js/theme.js", "/static/js/admin.js", "/static/js/beer-lookup.js", "/static/js/keg-pricing.js"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Fatalf("GET %s = %d bodyLen=%d", path, rec.Code, rec.Body.Len())
		}
	}

	// 2. 内嵌电视酒单页面与兜底 JSON；测试脚本不暴露
	recScreen := httptest.NewRecorder()
	h.ServeHTTP(recScreen, httptest.NewRequest(http.MethodGet, "/screen/", nil))
	if recScreen.Code != http.StatusOK ||
		!strings.Contains(recScreen.Body.String(), "/static/brand/pourops-dark.svg?v=") ||
		!strings.Contains(recScreen.Body.String(), "<title>酒单 - PourOps</title>") {
		t.Fatalf("GET /screen/ = %d body=%q", recScreen.Code, recScreen.Body.String()[:min(len(recScreen.Body.String()), 300)])
	}
	recPlaylist := httptest.NewRecorder()
	h.ServeHTTP(recPlaylist, httptest.NewRequest(http.MethodGet, "/screen/playlist.json", nil))
	if recPlaylist.Code != http.StatusOK || !strings.Contains(recPlaylist.Body.String(), `"items"`) {
		t.Fatalf("GET /screen/playlist.json = %d", recPlaylist.Code)
	}
	for _, secretTestPath := range []string{"/screen/screen_test.mjs", "/static/tests/screen_test.mjs"} {
		recTest := httptest.NewRecorder()
		h.ServeHTTP(recTest, httptest.NewRequest(http.MethodGet, secretTestPath, nil))
		if recTest.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", secretTestPath, recTest.Code)
		}
	}
	recScreenRedir := httptest.NewRecorder()
	h.ServeHTTP(recScreenRedir, httptest.NewRequest(http.MethodGet, "/screen", nil))
	if recScreenRedir.Code != http.StatusMovedPermanently || recScreenRedir.Header().Get("Location") != "/screen/" {
		t.Fatalf("GET /screen = %d loc=%q", recScreenRedir.Code, recScreenRedir.Header().Get("Location"))
	}

	// 3. GET /admin/login -> 200
	recLogin := httptest.NewRecorder()
	h.ServeHTTP(recLogin, httptest.NewRequest(http.MethodGet, "/admin/login", nil))
	if recLogin.Code != http.StatusOK || !strings.Contains(recLogin.Body.String(), "/static/css/admin.bundle.css?v=") {
		t.Fatalf("GET /admin/login = %d", recLogin.Code)
	}

	// 4. 未知后台路径必须 404（验证 {$} 精确匹配）
	rec404 := httptest.NewRecorder()
	h.ServeHTTP(rec404, httptest.NewRequest(http.MethodGet, "/admin/does-not-exist", nil))
	if rec404.Code != http.StatusNotFound {
		t.Fatalf("GET /admin/does-not-exist = %d, want 404", rec404.Code)
	}

	// 5. 跨站 POST 被 CrossOriginProtect 拦截 -> 403
	reqCSRF := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader("username=boss"))
	reqCSRF.Host = "pourops.example:8080"
	reqCSRF.Header.Set("Origin", "https://evil.example")
	recCSRF := httptest.NewRecorder()
	h.ServeHTTP(recCSRF, reqCSRF)
	if recCSRF.Code != http.StatusForbidden {
		t.Fatalf("跨站 POST 应 403，得到 %d", recCSRF.Code)
	}
}
