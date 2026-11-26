package inventory

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/catalog"
	"pourops/internal/event"
	"pourops/internal/platform"
	"pourops/migrations"
)

func purchasePostForm(handler http.HandlerFunc, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

// 进货表单按真实模板的字段发 kegs=2：酒款和桶必须一起落库，容量与进价照填，并发布事件。
func TestPurchaseBeerHandlerCommitsKegs(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	svc, _, bus := newTestService(t, pool)

	var mu sync.Mutex
	var events []event.EventType
	bus.Subscribe(func(ev event.Event) {
		mu.Lock()
		events = append(events, ev.Type)
		mu.Unlock()
	})

	form := url.Values{
		"brewery": {"测试厂"}, "name": {"进货 IPA"}, "style": {"IPA"}, "category": {"IPA"},
		"abv": {"5.2"}, "kegs": {"2"}, "capacity_ml": {"30000"},
		"cost": {"1200"}, "price_330": {"45"},
	}
	w := purchasePostForm(svc.PurchaseBeer, form)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("进货应重定向，得到 %d: %s", w.Code, w.Body.String())
	}

	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(ctx, `select id from beers where name='进货 IPA'`).Scan(&id); err != nil {
		t.Fatalf("酒款没落库: %v", err)
	}
	var kegs, capacity, cost int
	if err := pool.QueryRow(ctx,
		`select count(*)::int, coalesce(min(capacity_ml),0), coalesce(min(cost_cents),0) from kegs where beer_id=$1`,
		id).Scan(&kegs, &capacity, &cost); err != nil {
		t.Fatalf("统计酒桶失败: %v", err)
	}
	if kegs != 2 || capacity != 30000 || cost != 120000 {
		t.Fatalf("kegs=%d capacity=%d cost=%d, want 2/30000/120000", kegs, capacity, cost)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0] != event.EventScreenChanged || events[1] != event.EventBeerPurchased {
		t.Fatalf("进货事件发布不对: %+v", events)
	}
}

// 进货表单的非法值必须在写库前拒绝：不留空酒款，也不留下错的桶。
func TestPurchaseBeerRejectsBadInput(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	svc, _, _ := newTestService(t, pool)

	base := url.Values{"name": {"校验测试"}, "kegs": {"1"}, "capacity_ml": {"20000"}, "cost": {"1200"}}
	cases := []struct {
		field, value, wantErr string
	}{
		{"name", "", "酒款名称不能为空"},
		{"kegs", "0", "进货桶数"},
		{"kegs", "101", "进货桶数"},
		{"capacity_ml", "0", "每桶容量"},
		{"capacity_ml", "2147483648", "超出范围"},
		{"cost", "abc", "金额必须是数字"},
		{"cost", "-1", "金额不能为负"},
		{"cost", "30000000", "金额过大"},
		{"abv", "101", "ABV"},
		{"ibu", "-1", "IBU"},
		{"ibu", "2147483648", "IBU"},
		{"ut_rating", "5.5", "UT"},
		{"ut_count", "-1", "评分人数"},
		{"category", "rainbow", "酒款分类"},
		{"poster_url", "javascript:alert(1)", "酒标"},
		{"price_330", "-5", "330ml"},
		{"price_500", "30000000", "500ml"},
	}
	for _, c := range cases {
		form := url.Values{}
		for k, v := range base {
			form[k] = v
		}
		form.Set(c.field, c.value)
		w := purchasePostForm(svc.PurchaseBeer, form)
		if w.Code == http.StatusSeeOther {
			t.Errorf("%s=%q 不该成功", c.field, c.value)
		}
		if !strings.Contains(w.Body.String(), c.wantErr) {
			t.Errorf("%s=%q 页面里没有提示 %q", c.field, c.value, c.wantErr)
		}
	}

	ctx := context.Background()
	var beers, kegs int
	if err := pool.QueryRow(ctx, `select count(*)::int from beers`).Scan(&beers); err != nil {
		t.Fatalf("查酒款失败: %v", err)
	}
	if err := pool.QueryRow(ctx, `select count(*)::int from kegs`).Scan(&kegs); err != nil {
		t.Fatalf("查酒桶失败: %v", err)
	}
	if beers != 0 || kegs != 0 {
		t.Fatalf("非法进货不该写库: beers=%d kegs=%d", beers, kegs)
	}
}

// 第二步失败（容量超出 Postgres int4）必须把第一步插入的酒款一起回滚，
// 这样用户重试时不会多出一款没有库存的空酒。
func TestPurchaseBeerTxRollsBackOnKegFailure(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	if _, err := PurchaseBeerTx(ctx, pool, catalog.Beer{Name: "回滚测试"}, 1, 1<<31, 0); err == nil {
		t.Fatal("容量超出 int4 应失败")
	}
	var beers, kegs int
	if err := pool.QueryRow(ctx, `select count(*)::int from beers where name='回滚测试'`).Scan(&beers); err != nil {
		t.Fatalf("查酒款失败: %v", err)
	}
	if err := pool.QueryRow(ctx, `select count(*)::int from kegs`).Scan(&kegs); err != nil {
		t.Fatalf("查酒桶失败: %v", err)
	}
	if beers != 0 || kegs != 0 {
		t.Fatalf("进货失败后不该留下数据: beers=%d kegs=%d", beers, kegs)
	}
}

// 保存失败重绘表单时必须带回原 ID，否则再点保存会新增一款。
func TestSaveBeerFailureKeepsID(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	svc, _, _ := newTestService(t, pool)

	w := purchasePostForm(svc.SaveBeer, url.Values{"id": {"4242"}, "name": {""}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("空酒名应 400，得到 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `name="id" value="4242"`) {
		t.Fatalf("重绘表单丢了 ID: %s", w.Body.String())
	}
}

// 补货已有酒款（同名+同厂牌）时复用 beers 行，保留号位/上屏/排序/售卖标签并累加库存桶。
func TestPurchaseBeerTxReusesExistingBeer(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := platform.WithActor(context.Background(), "boss")

	oldID, err := catalog.SaveBeer(ctx, pool, catalog.Beer{
		No:        "05",
		Brewery:   "树屋",
		Name:      "尤利乌斯",
		Style:     "IPA",
		Category:  "IPA",
		Price330:  4500,
		SalesTags: []string{"招牌"},
		OnScreen:  true,
		SortOrder: 7,
	})
	if err != nil {
		t.Fatalf("初始化酒款失败: %v", err)
	}
	if err := PurchaseKegs(ctx, pool, oldID, 1, 20000, 100000); err != nil {
		t.Fatal(err)
	}

	// 不填杯价补货 2 桶：复用 oldID，保留原号位/上屏/售卖标签/杯价，更新非空资料字段。
	abv := 6.8
	reID, err := PurchaseBeerTx(ctx, pool, catalog.Beer{
		Brewery: " 树屋 ",
		Name:    "尤利乌斯",
		Style:   "IPA - Hazy",
		ABV:     &abv,
		Tags:    []string{"柑橘", "果汁感"},
	}, 2, 30000, 150000)
	if err != nil {
		t.Fatalf("补货失败: %v", err)
	}
	if reID != oldID {
		t.Fatalf("补货新建了酒款 id=%d，应复用 oldID=%d", reID, oldID)
	}

	var totalBeers int
	if err := pool.QueryRow(ctx, `select count(*)::int from beers`).Scan(&totalBeers); err != nil || totalBeers != 1 {
		t.Fatalf("beers 行数 = %d (err=%v)，应只保留 1 行", totalBeers, err)
	}
	got, err := catalog.GetBeer(ctx, pool, oldID)
	if err != nil {
		t.Fatal(err)
	}
	if got.No != "05" || !got.OnScreen || got.SortOrder != 7 || len(got.SalesTags) != 1 || got.SalesTags[0] != "招牌" ||
		got.Price330 != 4500 || got.Style != "IPA - Hazy" || got.ABV == nil || *got.ABV != 6.8 || len(got.Tags) != 2 {
		t.Fatalf("补货后酒款保留/更新字段不对: %+v", got)
	}
	var kegCount int
	if err := pool.QueryRow(ctx, `select count(*)::int from kegs where beer_id=$1`, oldID).Scan(&kegCount); err != nil || kegCount != 3 {
		t.Fatalf("桶数应累加为 3，实际 %d (err=%v)", kegCount, err)
	}

	// 带新杯价再次补货：任一杯价 > 0 时更新三档杯价。
	reID2, err := PurchaseBeerTx(ctx, pool, catalog.Beer{
		Brewery:  "树屋",
		Name:     "尤利乌斯",
		Price150: 2800,
		Price330: 5800,
	}, 1, 20000, 120000)
	if err != nil || reID2 != oldID {
		t.Fatalf("带杯价补货失败: id=%d err=%v", reID2, err)
	}
	got2, err := catalog.GetBeer(ctx, pool, oldID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Price150 != 2800 || got2.Price330 != 5800 || got2.Price500 != 0 {
		t.Fatalf("带杯价补货未正确更新三档杯价: %+v", got2)
	}
}

// 编辑酒款严格校验与 DB 内部错误脱敏。
func TestSaveBeerStrictValidationAndErrorSanitization(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	svc, _, _ := newTestService(t, pool)

	for _, tc := range []struct {
		field, val, want string
	}{
		{"abv", "105", "ABV"},
		{"ibu", "2147483648", "IBU"},
		{"ut_rating", "6", "UT"},
		{"category", "bad", "酒款分类"},
		{"poster_url", "javascript:alert(1)", "酒标"},
		{"price_150", "-10", "150ml"},
		{"sort_order", "2147483648", "排序"},
	} {
		form := url.Values{"id": {"4242"}, "name": {"校验酒款"}}
		form.Set(tc.field, tc.val)
		w := purchasePostForm(svc.SaveBeer, form)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s=%q 状态码 = %d, want 400", tc.field, tc.val, w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, tc.want) || !strings.Contains(body, `name="id" value="4242"`) {
			t.Fatalf("%s=%q 未保留 ID 或提示 %q", tc.field, tc.val, tc.want)
		}
	}

	// 内部 DB 错误不得把底层详情直接透传给页面或 URL。
	closedPool, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err == nil {
		closedPool.Close()
		badSvc, _, _ := newTestService(t, closedPool)

		wPur := purchasePostForm(badSvc.PurchaseBeer, url.Values{"name": {"测试"}, "kegs": {"1"}, "capacity_ml": {"20000"}})
		if wPur.Code != http.StatusInternalServerError || !strings.Contains(wPur.Body.String(), "入库失败，请稍后重试") || strings.Contains(wPur.Body.String(), "closed pool") {
			t.Fatalf("PurchaseBeer DB 错误未脱敏: code=%d body=%s", wPur.Code, wPur.Body.String())
		}

		wSave := purchasePostForm(badSvc.SaveBeer, url.Values{"id": {"1"}, "name": {"测试"}})
		if wSave.Code != http.StatusInternalServerError || !strings.Contains(wSave.Body.String(), "保存失败，请稍后重试") || strings.Contains(wSave.Body.String(), "closed pool") {
			t.Fatalf("SaveBeer DB 错误未脱敏: code=%d body=%s", wSave.Code, wSave.Body.String())
		}

		reqDel := httptest.NewRequest(http.MethodPost, "/admin/beers/1/delete", nil)
		reqDel.SetPathValue("id", fmt.Sprintf("%d", 1))
		wDel := httptest.NewRecorder()
		badSvc.DeleteBeerHandler(wDel, reqDel)
		_, delErrMsg := platform.ReadFlashCookies(wDel.Result().Cookies())
		if wDel.Code != http.StatusSeeOther || !strings.Contains(delErrMsg, "删除失败，请稍后重试") || strings.Contains(delErrMsg, "closed pool") {
			t.Fatalf("DeleteBeerHandler DB 错误未脱敏: code=%d err=%q loc=%q", wDel.Code, delErrMsg, wDel.Header().Get("Location"))
		}
	}
}

func TestDynamicCategoriesPurchaseEditAndDeleteFallback(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	svc, cfg, _ := newTestService(t, pool)
	ctx := context.Background()
	if err := cfg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddBeerCategory(ctx, "赛松"); err != nil {
		t.Fatal(err)
	}

	wPur := purchasePostForm(svc.PurchaseBeer, url.Values{
		"brewery": {"农舍厂"}, "name": {"农舍赛松"}, "style": {"Saison"}, "category": {"赛松"},
		"kegs": {"1"}, "capacity_ml": {"20000"}, "cost": {"1000"}, "price_330": {"48"},
	})
	if wPur.Code != http.StatusSeeOther {
		t.Fatalf("自定义分类进货应 303，实际 %d: %s", wPur.Code, wPur.Body.String())
	}
	var beerID int64
	if err := pool.QueryRow(ctx, `select id from beers where name='农舍赛松'`).Scan(&beerID); err != nil {
		t.Fatal(err)
	}

	if _, err := cfg.DeleteBeerCategory(ctx, "赛松"); err != nil {
		t.Fatal(err)
	}
	wSaveOld := purchasePostForm(svc.SaveBeer, url.Values{
		"id": {fmt.Sprintf("%d", beerID)}, "brewery": {"农舍厂"}, "name": {"农舍赛松"}, "style": {"Saison"}, "category": {"赛松"}, "price_330": {"50"},
	})
	if wSaveOld.Code != http.StatusSeeOther {
		t.Fatalf("删除预设分类后编辑既有酒款保留原分类应 303，实际 %d: %s", wSaveOld.Code, wSaveOld.Body.String())
	}

	wNewRejected := purchasePostForm(svc.PurchaseBeer, url.Values{
		"name": {"另一款赛松"}, "category": {"赛松"}, "kegs": {"1"}, "capacity_ml": {"20000"}, "cost": {"1000"},
	})
	if wNewRejected.Code != http.StatusBadRequest || !strings.Contains(wNewRejected.Body.String(), "酒款分类") {
		t.Fatalf("删除预设分类后新进货应返回 400 且提示酒款分类，实际 code=%d", wNewRejected.Code)
	}
}

type invRoundTripFn func(*http.Request) (*http.Response, error)

func (f invRoundTripFn) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func multipartBeerPost(handler http.HandlerFunc, fields url.Values, fileField, fileName string, fileData []byte) *httptest.ResponseRecorder {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, vs := range fields {
		for _, v := range vs {
			_ = mw.WriteField(k, v)
		}
	}
	if fileField != "" && len(fileData) > 0 {
		fw, _ := mw.CreateFormFile(fileField, fileName)
		_, _ = fw.Write(fileData)
	}
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func countRegularFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestPosterUploadRemoteFetchReplaceAndCleanupKeepsOnlyLatest(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	svc, _, _ := newTestService(t, pool)
	ctx := context.Background()

	pngBytes := []byte{
		0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n',
		0x00, 0x00, 0x00, 0x0D, 'I', 'H', 'D', 'R',
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89,
	}
	// 1×1 基线 JPEG：带 SOF0，才能通过 image.DecodeConfig 的尺寸校验。
	jpegBytes := []byte{
		0xFF, 0xD8,
		0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00,
		0xFF, 0xC0, 0x00, 0x11, 0x08, 0x00, 0x01, 0x00, 0x01, 0x03, 0x01, 0x11, 0x00, 0x02, 0x11, 0x01, 0x03, 0x11, 0x01,
		0xFF, 0xD9,
	}

	svc.posters.SetHTTPClientForTest(&http.Client{Transport: invRoundTripFn(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/png"}},
			Body:       io.NopCloser(bytes.NewReader(pngBytes)),
		}, nil
	})}, false)

	// 1. 上传本地 PNG 进货 -> 生成唯一 /posters/*.png 并写入 beers 与 beer_catalog。
	w1 := multipartBeerPost(svc.PurchaseBeer, url.Values{
		"brewery": {"测试厂"}, "name": {"海豹"}, "style": {"IPA"}, "category": {"IPA"},
		"kegs": {"1"}, "capacity_ml": {"20000"}, "cost": {"1000"}, "price_330": {"45"},
	}, "poster_file", "logo.png", pngBytes)
	if w1.Code != http.StatusSeeOther {
		t.Fatalf("上传酒标进货失败: code=%d body=%s", w1.Code, w1.Body.String())
	}
	var beerID int64
	var poster1, catPoster1 string
	if err := pool.QueryRow(ctx, `select id, poster_url from beers where name='海豹'`).Scan(&beerID, &poster1); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select poster_url from beer_catalog where name='海豹'`).Scan(&catPoster1); err != nil {
		t.Fatal(err)
	}
	if !catalog.IsLocalPosterURL(poster1) || poster1 != catPoster1 {
		t.Fatalf("上传酒标未同步保存到两表: beer=%q catalog=%q", poster1, catPoster1)
	}
	if files := countRegularFiles(t, svc.posters.Dir()); len(files) != 1 || filepath.Base(poster1) != files[0] {
		t.Fatalf("磁盘应仅有 1 个酒标文件，实际 %+v", files)
	}

	// 2. 编辑更换为 JPEG -> 新图生成唯一文件名，旧 PNG 被清理只留最新一张。
	w2 := multipartBeerPost(svc.SaveBeer, url.Values{
		"id": {fmt.Sprintf("%d", beerID)}, "brewery": {"测试厂"}, "name": {"海豹"}, "style": {"IPA"}, "category": {"IPA"},
		"poster_url": {poster1}, "price_330": {"48"},
	}, "poster_file", "new.jpg", jpegBytes)
	if w2.Code != http.StatusSeeOther {
		t.Fatalf("替换酒标编辑失败: code=%d body=%s", w2.Code, w2.Body.String())
	}
	var poster2, catPoster2 string
	if err := pool.QueryRow(ctx, `select poster_url from beers where id=$1`, beerID).Scan(&poster2); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select poster_url from beer_catalog where name='海豹'`).Scan(&catPoster2); err != nil {
		t.Fatal(err)
	}
	if !catalog.IsLocalPosterURL(poster2) || poster2 == poster1 || poster2 != catPoster2 || !strings.HasSuffix(poster2, ".jpg") {
		t.Fatalf("替换后酒标路径不对: p1=%q p2=%q cat=%q", poster1, poster2, catPoster2)
	}
	if svc.posters.Exists(poster1) {
		t.Fatalf("替换后旧酒标 %s 应被删除", poster1)
	}
	if files := countRegularFiles(t, svc.posters.Dir()); len(files) != 1 || filepath.Base(poster2) != files[0] {
		t.Fatalf("替换后磁盘应只留最新 1 个酒标文件，实际 %+v", files)
	}

	// 3. 编辑清空 poster_url -> 两表均清空且最新 JPEG 也被清理。
	w3 := purchasePostForm(svc.SaveBeer, url.Values{
		"id": {fmt.Sprintf("%d", beerID)}, "brewery": {"测试厂"}, "name": {"海豹"}, "style": {"IPA"}, "category": {"IPA"},
		"poster_url": {""}, "price_330": {"48"},
	})
	if w3.Code != http.StatusSeeOther {
		t.Fatalf("清空酒标保存失败: code=%d", w3.Code)
	}
	var poster3, catPoster3 string
	_ = pool.QueryRow(ctx, `select poster_url from beers where id=$1`, beerID).Scan(&poster3)
	_ = pool.QueryRow(ctx, `select poster_url from beer_catalog where name='海豹'`).Scan(&catPoster3)
	if poster3 != "" || catPoster3 != "" {
		t.Fatalf("清空酒标应同步清除两表: beer=%q catalog=%q", poster3, catPoster3)
	}
	if files := countRegularFiles(t, svc.posters.Dir()); len(files) != 0 {
		t.Fatalf("清空后磁盘应无残留文件，实际 %+v", files)
	}

	// 4. 填写外链 URL 进货 -> 提交时自动拉取并保存为本地唯一文件名。
	w4 := purchasePostForm(svc.PurchaseBeer, url.Values{
		"brewery": {"远端厂"}, "name": {"海浪"}, "style": {"Lager"}, "category": {"拉格"},
		"poster_url": {"https://cdn.example/wave.png"}, "kegs": {"1"}, "capacity_ml": {"20000"}, "cost": {"900"}, "price_330": {"38"},
	})
	if w4.Code != http.StatusSeeOther {
		t.Fatalf("外链进货失败: code=%d body=%s", w4.Code, w4.Body.String())
	}
	var poster4 string
	if err := pool.QueryRow(ctx, `select poster_url from beers where name='海浪'`).Scan(&poster4); err != nil {
		t.Fatal(err)
	}
	if !catalog.IsLocalPosterURL(poster4) || !svc.posters.Exists(poster4) {
		t.Fatalf("外链未转为本地保存: %q", poster4)
	}
}
