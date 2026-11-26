package inventory

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"pourops/internal/event"
	"pourops/internal/platform"
	"pourops/migrations"
)

func TestUntapSummaryKegListFiltersAndEventBus(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	svc, cfgStore, bus := newTestService(t, pool)
	ctx := platform.WithActor(context.Background(), "boss")

	var mu sync.Mutex
	var gotEvents []event.Event
	bus.Subscribe(func(ev event.Event) {
		mu.Lock()
		gotEvents = append(gotEvents, ev)
		mu.Unlock()
	})

	beerID := invSeedBeer(t, pool, "核算测试 IPA")
	if _, err := pool.Exec(ctx, `update beers set price_150=2500, price_330=5000, price_500=7500 where id=$1`, beerID); err != nil {
		t.Fatal(err)
	}
	// 进 2 桶：20L，进价 ¥1200
	if err := PurchaseKegs(ctx, pool, beerID, 2, 20000, 120000); err != nil {
		t.Fatal(err)
	}

	// 上架开第 1 桶并售出达到 50% 目标营收（¥2400 / 16000ml）
	reqTap := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/beers/%d/tap", beerID),
		strings.NewReader("no=01&capacity_ml=20000&cost=1200"))
	reqTap.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqTap.SetPathValue("id", fmt.Sprintf("%d", beerID))
	recTap := httptest.NewRecorder()
	svc.TapBeerHandler(recTap, reqTap)
	if recTap.Code != http.StatusSeeOther {
		t.Fatalf("tap status = %d", recTap.Code)
	}
	keg1ID := invKegID(t, pool, beerID, true)
	if err := AddPour(ctx, pool, keg1ID, 48, 16000, 240000); err != nil {
		t.Fatal(err)
	}

	// HTTP 下架第 1 桶，跳转 Flash Cookie 应带出整桶达标盈利复盘摘要，并发布 EventKegFinished。
	reqUntap := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/beers/%d/untap", beerID), nil)
	reqUntap.SetPathValue("id", fmt.Sprintf("%d", beerID))
	recUntap := httptest.NewRecorder()
	svc.UntapBeerHandler(recUntap, reqUntap)
	if recUntap.Code != http.StatusSeeOther {
		t.Fatalf("untap status = %d", recUntap.Code)
	}
	flashMsg, _ := platform.ReadFlashCookies(recUntap.Result().Cookies())
	if !strings.Contains(flashMsg, "达标盈利") || !strings.Contains(flashMsg, "1200") || !strings.Contains(flashMsg, "50.0%") {
		t.Fatalf("下架跳转提示未带盈利复盘摘要: %q", flashMsg)
	}

	mu.Lock()
	var sawTapped, sawFinished, sawScreen int
	for _, ev := range gotEvents {
		switch ev.Type {
		case event.EventBeerTapped:
			sawTapped++
		case event.EventKegFinished:
			sawFinished++
		case event.EventScreenChanged:
			sawScreen++
		}
	}
	mu.Unlock()
	if sawTapped != 1 || sawFinished != 1 || sawScreen < 2 {
		t.Fatalf("EventBus 捕获事件不对: tapped=%d finished=%d screen=%d", sawTapped, sawFinished, sawScreen)
	}

	// LatestFinishedKegs 应返回第 1 桶。
	lastFin, err := LatestFinishedKegs(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if lk := lastFin[beerID]; lk == nil || lk.ID != keg1ID {
		t.Fatalf("LatestFinishedKegs 不对: %+v", lastFin[beerID])
	}

	// 开第 2 桶记少量售出（在售回本中），同时新增一款已下架亏损桶验证 /admin/kegs 筛选。
	if err := TapBeer(ctx, pool, beerID, "01", 20000, 120000); err != nil {
		t.Fatal(err)
	}
	keg2ID := invKegID(t, pool, beerID, true)
	if err := AddPour(ctx, pool, keg2ID, 3, 990, 15000); err != nil {
		t.Fatal(err)
	}

	beer2ID := invSeedBeer(t, pool, "亏损复盘黑啤")
	if err := PurchaseKegs(ctx, pool, beer2ID, 1, 20000, 100000); err != nil {
		t.Fatal(err)
	}
	if err := TapBeer(ctx, pool, beer2ID, "02", 20000, 100000); err != nil {
		t.Fatal(err)
	}
	keg3ID := invKegID(t, pool, beer2ID, true)
	if err := AddPour(ctx, pool, keg3ID, 2, 1000, 12000); err != nil {
		t.Fatal(err)
	}
	if err := UntapBeer(ctx, pool, beer2ID); err != nil {
		t.Fatal(err)
	}

	// 1. /admin/kegs?status=finished&verdict=loss 只保留亏损黑啤桶
	recLoss := httptest.NewRecorder()
	svc.KegList(recLoss, httptest.NewRequest(http.MethodGet, "/admin/kegs?status=finished&verdict=loss", nil))
	if recLoss.Code != http.StatusOK {
		t.Fatalf("KegList loss status = %d", recLoss.Code)
	}
	lossBody := recLoss.Body.String()
	if !strings.Contains(lossBody, "亏损复盘黑啤") || strings.Contains(lossBody, "核算测试 IPA") {
		t.Fatalf("亏损筛选结果不对: %s", lossBody)
	}

	// 2. /admin/kegs?status=active 只保留在售第 2 桶
	recAct := httptest.NewRecorder()
	svc.KegList(recAct, httptest.NewRequest(http.MethodGet, "/admin/kegs?status=active", nil))
	if recAct.Code != http.StatusOK || !strings.Contains(recAct.Body.String(), "回本中") {
		t.Fatalf("在售筛选应展示回本进度，status=%d", recAct.Code)
	}

	// 3. /admin/kegs/{id} 详情页应包含单杯 50% 推荐售价对照与结桶诊断卡片
	reqDet := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/admin/kegs/%d", keg1ID), nil)
	reqDet.SetPathValue("id", fmt.Sprintf("%d", keg1ID))
	recDet := httptest.NewRecorder()
	svc.KegDetail(recDet, reqDet)
	if recDet.Code != http.StatusOK {
		t.Fatalf("KegDetail status = %d", recDet.Code)
	}
	detBody := recDet.Body.String()
	if !strings.Contains(detBody, "单杯成本、回本杯数与 50% 毛利率推荐售价") ||
		!strings.Contains(detBody, "¥ 23") || !strings.Contains(detBody, "¥ 50") || !strings.Contains(detBody, "¥ 75") ||
		!strings.Contains(detBody, `id="keg-detail-pricing"`) ||
		!strings.Contains(detBody, `id="keg-detail-pricing-controls"`) ||
		!strings.Contains(detBody, `/static/js/keg-pricing.js`) ||
		!strings.Contains(detBody, "下架结桶盈利复盘") {
		t.Fatalf("KegDetail 缺少推荐价对照、可调滑块或结桶诊断卡片")
	}

	// 修改 SettingsStore 默认口径后，KegDetail HTML 应直接响应新配置
	if _, err := cfgStore.SaveGeneral(ctx, "PourOps", 85, 55); err != nil {
		t.Fatal(err)
	}
	recDetCustom := httptest.NewRecorder()
	svc.KegDetail(recDetCustom, reqDet)
	detCustomBody := recDetCustom.Body.String()
	if !strings.Contains(detCustomBody, "¥ 52") ||
		!strings.Contains(detCustomBody, `data-default-sellable="85"`) ||
		!strings.Contains(detCustomBody, `data-default-margin="55"`) {
		t.Fatalf("自定义 85%%/55%% 口径后 KegDetail 应渲染 ¥ 52 与默认滑块属性")
	}

	// 未开桶详情页不应渲染记售出表单（CanPour 为 false），并区分未开桶提示；待填成本桶应在汇总页标出。
	if err := PurchaseKegs(ctx, pool, beer2ID, 1, 20000, 80000); err != nil {
		t.Fatal(err)
	}
	untappedID := invKegID(t, pool, beer2ID, false)
	reqUntapDet := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/admin/kegs/%d", untappedID), nil)
	reqUntapDet.SetPathValue("id", fmt.Sprintf("%d", untappedID))
	recUntapDet := httptest.NewRecorder()
	svc.KegDetail(recUntapDet, reqUntapDet)
	if recUntapDet.Code != http.StatusOK ||
		strings.Contains(recUntapDet.Body.String(), fmt.Sprintf(`/admin/kegs/%d/pours`, untappedID)) ||
		!strings.Contains(recUntapDet.Body.String(), "这桶还没开桶") {
		t.Fatalf("未开桶详情页不应允许记售出或缺少未开桶提示")
	}

	recAll := httptest.NewRecorder()
	svc.KegList(recAll, httptest.NewRequest(http.MethodGet, "/admin/kegs", nil))
	if recAll.Code != http.StatusOK || !strings.Contains(recAll.Body.String(), "在库待开") {
		t.Fatalf("kegs 汇总页应展示在库待开成本，status=%d", recAll.Code)
	}

	// 4. /admin/beers 列表应正常渲染并在未开桶行或已开桶行带上盈利判定
	recBeers := httptest.NewRecorder()
	svc.BeerList(recBeers, httptest.NewRequest(http.MethodGet, "/admin/beers", nil))
	if recBeers.Code != http.StatusOK || !strings.Contains(recBeers.Body.String(), "上次结桶") {
		t.Fatalf("BeerList 未展示上次结桶复盘或状态异常: %d", recBeers.Code)
	}
}

func TestEditBeerUnifiedLayoutAndKegRefSpec(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	svc, _, _ := newTestService(t, pool)
	ctx := platform.WithActor(context.Background(), "boss")

	// 无在店酒桶时应回退读取历史酒单默认容量与进价
	if _, err := pool.Exec(ctx, `
		insert into beer_catalog (brewery, name, style, capacity_ml, cost_cents)
		values ('测试厂牌', '统一编辑酒款', 'IPA - American', 30000, 180000)`); err != nil {
		t.Fatal(err)
	}
	var beerID int64
	if err := pool.QueryRow(ctx, `
		insert into beers (brewery, name, style, price_330, on_screen)
		values ('测试厂牌', '统一编辑酒款', 'IPA - American', 4800, true)
		returning id`).Scan(&beerID); err != nil {
		t.Fatal(err)
	}

	reqCatalog := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/admin/beers/%d/edit", beerID), nil)
	reqCatalog.SetPathValue("id", fmt.Sprintf("%d", beerID))
	recCatalog := httptest.NewRecorder()
	svc.EditBeer(recCatalog, reqCatalog)
	if recCatalog.Code != http.StatusOK {
		t.Fatalf("EditBeer status = %d", recCatalog.Code)
	}
	bodyCatalog := recCatalog.Body.String()
	for _, want := range []string{
		`class="basic-info-layout"`,
		`class="poster-stage"`,
		`id="beer-lookup-wrap" data-mode="edit"`,
		`id="beer-lookup-btn"`,
		`id="keg-pricing-guide"`,
		`id="capacity_ml" value="30000"`,
		`id="cost" value="1800"`,
		`name="on_screen"`,
		`/static/js/beer-lookup.js`,
		`/static/js/keg-pricing.js`,
	} {
		if !strings.Contains(bodyCatalog, want) {
			t.Fatalf("EditBeer(目录回退) 缺少预期元素 %q", want)
		}
	}
	if strings.Contains(bodyCatalog, `name="capacity_ml"`) || strings.Contains(bodyCatalog, `name="cost"`) {
		t.Fatalf("EditBeer 测算字段不应提交 name=capacity_ml 或 name=cost")
	}

	// 进桶开桶后应优先带入实际在用酒桶规格，且展示查看/编辑酒桶链接
	if err := TapBeer(ctx, pool, beerID, "08", 20000, 120000); err != nil {
		t.Fatal(err)
	}
	kegID := invKegID(t, pool, beerID, true)
	reqKeg := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/admin/beers/%d/edit", beerID), nil)
	reqKeg.SetPathValue("id", fmt.Sprintf("%d", beerID))
	recKeg := httptest.NewRecorder()
	svc.EditBeer(recKeg, reqKeg)
	if recKeg.Code != http.StatusOK {
		t.Fatalf("EditBeer(关联酒桶) status = %d", recKeg.Code)
	}
	bodyKeg := recKeg.Body.String()
	for _, want := range []string{
		`id="capacity_ml" value="20000"`,
		`id="cost" value="1200"`,
		fmt.Sprintf(`/admin/kegs/%d`, kegID),
		fmt.Sprintf(`/admin/kegs/%d/edit`, kegID),
	} {
		if !strings.Contains(bodyKeg, want) {
			t.Fatalf("EditBeer(关联酒桶) 缺少预期元素 %q", want)
		}
	}

	// 保存失败重绘应保留 ID 与关联桶测算规格
	form := url.Values{
		"id":   {fmt.Sprintf("%d", beerID)},
		"name": {""},
	}
	reqFail := httptest.NewRequest(http.MethodPost, "/admin/beers", strings.NewReader(form.Encode()))
	reqFail.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recFail := httptest.NewRecorder()
	svc.SaveBeer(recFail, reqFail)
	if recFail.Code != http.StatusBadRequest {
		t.Fatalf("SaveBeer 失败状态码 = %d, want 400", recFail.Code)
	}
	bodyFail := recFail.Body.String()
	if !strings.Contains(bodyFail, fmt.Sprintf(`name="id" value="%d"`, beerID)) ||
		!strings.Contains(bodyFail, `id="capacity_ml" value="20000"`) ||
		!strings.Contains(bodyFail, `id="cost" value="1200"`) {
		t.Fatalf("SaveBeer 失败重绘未保留 ID 或酒桶测算规格")
	}
}
