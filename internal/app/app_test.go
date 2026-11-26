package app

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// TestAllTemplatesRender 验证全部 19 个内嵌模板均可在真实静态资源清单下零报错渲染并输出资源哈希。
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
	staff := &ui.UserView{ID: 2, Username: "staff", Role: ui.RoleStaff, Active: true}
	pricing := settings.PricingConfig{SellablePct: 85, MarginPct: 55}

	beer := catalog.Beer{
		ID: 1, No: "01", Brewery: "树屋", Name: "尤利乌斯", Style: "IPA - Hazy", Category: "IPA",
		ABV: &abv, IBU: &ibu, UT: &ut, Price150: 2800, Price330: 5800, Price500: 7800,
		Tags: []string{"柑橘", "果汁感"}, SalesTags: []string{"本周最佳"}, OnScreen: true,
	}
	keg := inventory.Keg{
		ID: 10, BeerID: 1, BeerNo: "01", Brewery: "树屋", BeerName: "尤利乌斯", KegNo: "K-01",
		CapacityML: 20000, CostCents: 120000, PlacedAt: &now, TappedAt: &now,
		SoldCups: 22, SoldML: 7260, RevenueCents: 127600, PourCount: 14,
	}
	warnKeg := keg
	warnKeg.SoldML = 13000
	warnKeg.SoldCups = 40
	warnKeg.RevenueCents = 232000
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
			ReportPeriod: "7d", ReportStart: "2026-11-17", ReportEnd: "2026-11-23",
			Sales: report.SalesInsights{
				Current: report.SalesTotals{Revenue: 520000, Cost: 210000, Qty: 86},
				Draft:   report.SalesTotals{Revenue: 380000, Cost: 150000, Qty: 62},
				Retail:  report.SalesTotals{Revenue: 140000, Cost: 60000, Qty: 24},
				Trend:   []report.SalesTrendDay{{Date: "2026-11-22", Revenue: 240000, Cost: 95000}, {Date: "2026-11-23", Revenue: 280000, Cost: 115000}},
				Top:     []report.SalesTopItem{{Name: "树屋 · 尤利乌斯", Unit: "杯", Qty: 32, Revenue: 185600, Cost: 64000}},
			},
			POS: report.POSInsights{Received: 480000, Orders: 24, Payments: 29, Hours: []report.POSHour{{Hour: 20, Received: 180000, Payments: 10}, {Hour: 21, Received: 300000, Payments: 19}}},
			Overview: report.Overview{
				LowCount: 1, OutCount: 0, Untapped: 2, WarnKegCount: 1, ActiveKegs: 4, RetailCount: 12, StockValue: 340000,
				LowStock: []inventory.RetailItem{item},
				WarnKegs: []inventory.Keg{warnKeg},
			},
		}, []string{"库存状态与告警", "已售 &gt; 50%", "已售 65%", `/admin/analytics`, `/static/brand/pourops.svg?v=`, `/static/brand/pourops-light.svg?v=`, `/static/brand/pourops-dark.svg?v=`}, false},
		{"analytics", "admin/analytics.html", "layout", &report.AnalyticsPage{
			Layout:       ui.Layout{Title: "数据分析", Nav: "analytics", Me: me},
			ReportPeriod: "7d", ReportStart: "2026-11-17", ReportEnd: "2026-11-23",
			Sales: report.SalesInsights{
				Current: report.SalesTotals{Revenue: 520000, Cost: 210000, Qty: 86},
				Draft:   report.SalesTotals{Revenue: 380000, Cost: 150000, Qty: 62},
				Retail:  report.SalesTotals{Revenue: 140000, Cost: 60000, Qty: 24},
				Trend:   []report.SalesTrendDay{{Date: "2026-11-22", Revenue: 240000, Cost: 95000}, {Date: "2026-11-23", Revenue: 280000, Cost: 115000}},
				Top:     []report.SalesTopItem{{Name: "树屋 · 尤利乌斯", Unit: "杯", Qty: 32, Revenue: 185600, Cost: 64000}},
			},
			Audience: report.AudienceInsights{
				TotalCups:    62,
				TotalVolume:  20460,
				TotalRevenue: 380000,
				TotalCost:    150000,
				Segments: []report.CategorySegment{
					{Category: "IPA", Color: report.AudienceCategoryColor("IPA"), Cups: 32, PrevCups: 25, VolumeML: 10560, Revenue: 185600, Cost: 64000, AvgABV: 6.8, HasABV: true, CupShare: 51.6, RevShare: 48.8, ProfitShare: 52.9},
					{Category: "西打", Color: report.AudienceCategoryColor("西打"), Cups: 18, PrevCups: 12, VolumeML: 5940, Revenue: 108000, Cost: 45000, AvgABV: 5.0, HasABV: true, CupShare: 29.0, RevShare: 28.4, ProfitShare: 27.4},
					{Category: "世涛", Color: report.AudienceCategoryColor("世涛"), Cups: 12, PrevCups: 0, VolumeML: 3960, Revenue: 86400, Cost: 41000, AvgABV: 9.2, HasABV: true, CupShare: 19.4, RevShare: 22.7, ProfitShare: 19.7},
				},
				CupSizes: report.CupSizePreference{SmallCups: 10, MediumCups: 42, LargeCups: 10},
				Spots:    report.SpotPreference{BarCups: 24, BarTopCat: "IPA", TableCups: 20, TableTopCat: "西打"},
			},
		}, []string{"销售走势", "客户群分析", "按日销售明细", `id="salesChart"`, `id="audienceChart"`, `id="audience-data"`, `data-audience-type="radar"`, `data-audience-metric="profit"`, "按毛利", "IPA", "西打", "2026-11-23"}, false},
		{"beers", "admin/beers.html", "layout", &inventory.BeerListPage{
			Layout: ui.Layout{Title: "酒单 · 桶装", Nav: "beers", Me: me}, NextNo: 2, Pricing: pricing,
			Rows: []inventory.BeerRow{{Beer: beer, Keg: &keg, Stock: 2}, {Beer: beer, LastKeg: &finKeg}},
		}, nil, false},
		{"beer_form", "admin/beer_form.html", "layout", &inventory.BeerFormPage{
			Layout: ui.Layout{Title: "编辑酒款", Nav: "beers", Me: me, Categories: platform.Categories}, Beer: beer, Keg: keg, CostInput: "1200", Pricing: pricing,
		}, []string{`酒款分类`, `enctype="multipart/form-data"`, `name="poster_file"`, `data-default-sellable="85"`, `/static/js/beer-lookup.js?v=`, `/static/js/keg-pricing.js?v=`}, false},
		{"beer_form-removed-cat", "admin/beer_form.html", "layout", &inventory.BeerFormPage{
			Layout: ui.Layout{Title: "编辑酒款", Nav: "beers", Me: me, Categories: platform.Categories},
			Beer:   catalog.Beer{ID: 9, Name: "老酒款", Category: "已删分类"}, Keg: keg, Pricing: pricing,
		}, []string{`已删分类（已不在预设列表）`}, false},
		{"purchase", "admin/purchase.html", "layout", &inventory.PurchasePage{
			Layout: ui.Layout{Title: "新增酒款", Nav: "beers", Me: me, Categories: platform.Categories}, Beer: beer, Keg: keg, KegCount: 2, CostInput: "1200", Pricing: pricing,
		}, []string{`enctype="multipart/form-data"`, `name="poster_file"`, `data-default-margin="55"`, `/static/js/beer-lookup.js?v=`, `/static/js/keg-pricing.js?v=`}, false},
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
			Layout: ui.Layout{Title: "编辑零售商品", Nav: "retail", Me: me}, Item: item, ExpectedStock: item.Stock, RetailCats: inventory.RetailCategories, Moves: []inventory.StockMove{{Delta: 12, Reason: "入库", CreatedAt: now}},
		}, nil, false},
		{"records-orders", "admin/records.html", "layout", &report.RecordsViewPage{
			Layout:  ui.Layout{Title: "销售订单", Nav: "orders", Me: me},
			Records: &report.RecordsPage{Mode: "orders", Statuses: report.RecordOrderStatuses, Orders: []report.RecordOrder{ord}},
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
				Mode: "operations", Kinds: report.RecordKindOptions,
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
		{"security-staff", "admin/account_security.html", "layout", &auth.SecurityPage{
			Layout: ui.Layout{Title: "账号安全", Nav: "security", Me: staff}, Sec: auth.SecurityView{TwoFactor: false},
		}, nil, false},
		{"settings", "admin/settings.html", "layout", &settings.SettingsPage{
			Layout:         ui.Layout{Title: "系统设置", Nav: "settings", Me: me},
			Settings:       settings.StoreSettings{Brand: "PourOps", DefaultSellablePct: 80, DefaultMarginPct: 50, BarCount: 3, TableCount: 4, BeerCategories: []string{"IPA", "赛松"}, WebhookEvents: []string{string(event.EventOrderPaid)}},
			EventOptions:   event.WebhookEventOptions,
			HasSecret:      true,
			SelectedEvents: map[string]bool{string(event.EventOrderPaid): true},
			CatalogTotal:   12,
		}, []string{`/admin/settings/general`, `/admin/settings/spots`, `/admin/settings/webhook`, `/admin/settings/webhook-test`, `name="default_sellable_pct"`, `name="bar_count"`, `name="table_count"`, `name="webhook_url"`, `酒款分类管理`, `/admin/settings/categories/add`, `赛松`, `历史酒单导入与导出`, `12`, `/admin/settings/catalog/export`, `/admin/settings/catalog/import`, `accept=".zip,application/zip"`}, false},
		{"pos-spots", "pos/pos.html", "pos", &pos.PosPage{
			Layout:   ui.Layout{Me: me},
			Spots:    []pos.Spot{{Key: "bar-1", Kind: "bar", Label: "吧台 1", OrderID: 88, Items: 2, Total: 9600}, {Key: "table-1", Kind: "table", Label: "小桌 1"}, {Key: "temp-1", Kind: "temp", Label: "临时 1"}},
			PosBeers: []pos.PosBeer{{KegID: 10, No: "01", Brewery: "树屋", Name: "尤利乌斯", Style: "Hazy IPA", Price150: 2800, Price330: 5800, Price500: 7800}},
			PosItems: []inventory.RetailItem{item},
		}, []string{`/static/brand/pourops.svg?v=`, `/static/brand/pourops-dark.svg?v=`, `固定 1 位`, `固定 1 桌`, `/pos/temp/add`, `/pos/temp/remove`, `临时 1`}, true},
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
		}, []string{`<title>酒单 - 北岸精酿</title>`, `/static/brand/pourops.svg?v=`, `/static/brand/pourops-light.svg?v=`, `mix-blend-mode:multiply`, `mask-image:radial-gradient`, `cup-tier`}, true},
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
			if strings.Contains(body, "ZgotmplZ") {
				t.Fatalf("%s 出现被 html/template 过滤的 ZgotmplZ", tc.file)
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
			if tc.name == "dashboard" {
				for _, removed := range []string{"在售的酒桶", `id="salesChart"`} {
					if strings.Contains(body, removed) {
						t.Fatalf("dashboard 不应包含已移除板块 %q", removed)
					}
				}
			}
			if !tc.notLayout && strings.Contains(body, `/admin/beers/catalog`) {
				t.Fatalf("%s 不应再包含已取消的历史酒单页面链接", tc.file)
			}

			actions := extractSection(body, `<div class="heading-actions">`, `</div>`)
			dropdown := extractSection(body, `<ul class="dropdown-menu`, `</ul>`)
			switch tc.name {
			case "dashboard":
				for _, gone := range []string{"/admin/analytics", "/admin/records", "/admin/beers", "/admin/retail/new"} {
					if strings.Contains(actions, gone) {
						t.Fatalf("dashboard 右上角不应再包含重复跳转 %q", gone)
					}
				}
			case "analytics":
				for _, gone := range []string{`href="/admin/"`, "/admin/records"} {
					if strings.Contains(actions, gone) {
						t.Fatalf("analytics 右上角不应再包含重复跳转 %q", gone)
					}
				}
			case "beers":
				if strings.Contains(actions, "/admin/kegs") {
					t.Fatalf("beers 右上角不应再包含 /admin/kegs")
				}
				if !strings.Contains(actions, "/admin/beers/purchase") {
					t.Fatalf("beers 右上角应保留新增进货入口")
				}
			case "kegs":
				if strings.Contains(actions, `href="/admin/beers"`) {
					t.Fatalf("kegs 右上角不应再包含 href=\"/admin/beers\"")
				}
				if !strings.Contains(actions, "/admin/beers/purchase") {
					t.Fatalf("kegs 右上角应保留新增进货入口")
				}
			case "settings":
				if strings.Contains(body, `class="heading-actions"`) {
					t.Fatalf("settings 不应再渲染空 heading-actions")
				}
				if strings.Contains(body, "security-card") {
					t.Fatalf("settings 不应再使用错位的 security-card")
				}
				general := extractSection(body, `id="settings-general"`, `</form>`)
				for _, want := range []string{`action="/admin/settings/general"`, `name="brand"`, `name="default_sellable_pct"`, `name="default_margin_pct"`} {
					if !strings.Contains(general, want) {
						t.Fatalf("settings-general 缺少 %q", want)
					}
				}
				for _, gone := range []string{`name="webhook_url"`, `href="/admin/"`} {
					if strings.Contains(general, gone) {
						t.Fatalf("settings-general 不应包含 %q", gone)
					}
				}
				webhook := extractSection(body, `id="settings-webhook"`, `</main>`)
				for _, want := range []string{`action="/admin/settings/webhook"`, `name="webhook_url"`, `action="/admin/settings/webhook-test"`, `连通性测试`} {
					if !strings.Contains(webhook, want) {
						t.Fatalf("settings-webhook 缺少 %q", want)
					}
				}
				if strings.Contains(webhook, `name="default_sellable_pct"`) {
					t.Fatalf("settings-webhook 不应混入核算参数字段")
				}
			case "security":
				if strings.Contains(body, `class="heading-actions"`) || strings.Contains(body, "回后台") {
					t.Fatalf("后台角色访问账号安全页不应显示回后台入口")
				}
			case "security-staff":
				if !strings.Contains(actions, `href="/pos/"`) || !strings.Contains(actions, "回点餐端") {
					t.Fatalf("店员访问账号安全页右上角应保留回点餐端")
				}
				for _, want := range []string{`href="/pos/"`, `href="/screen/"`, `href="/account/security"`} {
					if !strings.Contains(dropdown, want) {
						t.Fatalf("店员账号下拉菜单缺少 %q", want)
					}
				}
			case "keg":
				for _, want := range []string{`href="/admin/kegs/10/edit"`, `href="/admin/kegs"`, "返回桶装盈亏"} {
					if !strings.Contains(actions, want) {
						t.Fatalf("keg 右上角缺少 %q", want)
					}
				}
				for _, gone := range []string{`href="/admin/beers"`, "button--primary"} {
					if strings.Contains(actions, gone) {
						t.Fatalf("keg 右上角不应包含 %q", gone)
					}
				}
			case "keg_form":
				if !strings.Contains(actions, `href="/admin/kegs/10"`) || !strings.Contains(actions, "返回酒桶详情") {
					t.Fatalf("keg_form 右上角应返回酒桶详情")
				}
				if strings.Contains(actions, `href="/admin/beers"`) {
					t.Fatalf("keg_form 右上角不应再包含 href=\"/admin/beers\"")
				}
			case "screen":
				if strings.Contains(body, `invert(1) grayscale(1)`) {
					t.Fatalf("screen 不应再使用暗黑反相灰度酒标滤镜")
				}
			case "pos-spots":
				for _, gone := range []string{`/pos/bar/add`, `/pos/table/add`, `/pos/bar/remove`, `/pos/table/remove`} {
					if strings.Contains(body, gone) {
						t.Fatalf("pos-spots 不应再包含固定台位增删路由 %q", gone)
					}
				}
			}

			if !tc.notLayout && tc.name != "login" && tc.name != "login-2fa" && tc.name != "security-staff" {
				for _, want := range []string{`href="/account/security"`, "/admin/logout"} {
					if !strings.Contains(dropdown, want) {
						t.Fatalf("%s 账号下拉菜单缺少 %q", tc.name, want)
					}
				}
				for _, gone := range []string{`href="/admin/"`, `href="/admin/analytics"`, `href="/pos/"`, `href="/screen/"`} {
					if strings.Contains(dropdown, gone) {
						t.Fatalf("%s 后台账号下拉菜单不应再包含左侧菜单已有项 %q", tc.name, gone)
					}
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

func extractSection(body, startMarker, endMarker string) string {
	start := strings.Index(body, startMarker)
	if start < 0 {
		return ""
	}
	rest := body[start:]
	end := strings.Index(rest, endMarker)
	if end < 0 {
		return ""
	}
	return rest[:end+len(endMarker)]
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

	posters, err := catalog.NewPosterStore(filepath.Join(t.TempDir(), "posters"))
	if err != nil {
		t.Fatal(err)
	}
	samplePNG := []byte{
		0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n',
		0x00, 0x00, 0x00, 0x0D, 'I', 'H', 'D', 'R',
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89,
	}
	posterURL, err := posters.SaveBytes(samplePNG)
	if err != nil {
		t.Fatal(err)
	}

	h := NewHandler(Deps{
		Settings:     settingsStore,
		Bus:          bus,
		ScreenHub:    screenHub,
		Assets:       assets,
		Renderer:     renderer,
		Posters:      posters,
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
		!strings.Contains(recScreen.Body.String(), "/static/brand/pourops-light.svg?v=") ||
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

	// 3. GET /admin/login -> 200; 受保护路由 /admin/analytics 未登录应重定向到 /admin/login
	recLogin := httptest.NewRecorder()
	h.ServeHTTP(recLogin, httptest.NewRequest(http.MethodGet, "/admin/login", nil))
	if recLogin.Code != http.StatusOK || !strings.Contains(recLogin.Body.String(), "/static/css/admin.bundle.css?v=") {
		t.Fatalf("GET /admin/login = %d", recLogin.Code)
	}
	for _, path := range []string{"/admin/analytics", "/admin/settings", "/admin/settings/catalog/export"} {
		recAnon := httptest.NewRecorder()
		h.ServeHTTP(recAnon, httptest.NewRequest(http.MethodGet, path, nil))
		if recAnon.Code != http.StatusSeeOther || recAnon.Header().Get("Location") != "/admin/login" {
			t.Fatalf("GET %s 未登录应 303 -> /admin/login，实际 %d loc=%q", path, recAnon.Code, recAnon.Header().Get("Location"))
		}
	}
	for _, postPath := range []string{"/admin/settings/general", "/admin/settings/spots", "/admin/settings/webhook", "/admin/settings/categories/add", "/admin/settings/catalog/import", "/pos/temp/add", "/pos/temp/remove"} {
		recCatAnon := httptest.NewRecorder()
		h.ServeHTTP(recCatAnon, httptest.NewRequest(http.MethodPost, postPath, nil))
		if recCatAnon.Code != http.StatusSeeOther || recCatAnon.Header().Get("Location") != "/admin/login" {
			t.Fatalf("POST %s 未登录应 303 -> /admin/login，实际 %d", postPath, recCatAnon.Code)
		}
	}

	// 本地酒标服务 & 路径穿越 404 & 已取消的历史酒单页面 404。
	recPoster := httptest.NewRecorder()
	h.ServeHTTP(recPoster, httptest.NewRequest(http.MethodGet, posterURL, nil))
	if recPoster.Code != http.StatusOK || recPoster.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("GET %s = %d nosniff=%q", posterURL, recPoster.Code, recPoster.Header().Get("X-Content-Type-Options"))
	}
	recTraversal := httptest.NewRecorder()
	h.ServeHTTP(recTraversal, httptest.NewRequest(http.MethodGet, "/posters/..%2Fetc%2Fpasswd", nil))
	if recTraversal.Code != http.StatusNotFound {
		t.Fatalf("路径穿越应 404，实际 %d", recTraversal.Code)
	}

	// 4. 未知或已取消的后台路径必须 404
	for _, gonePath := range []string{"/admin/does-not-exist", "/admin/beers/catalog"} {
		rec404 := httptest.NewRecorder()
		h.ServeHTTP(rec404, httptest.NewRequest(http.MethodGet, gonePath, nil))
		if rec404.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", gonePath, rec404.Code)
		}
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
