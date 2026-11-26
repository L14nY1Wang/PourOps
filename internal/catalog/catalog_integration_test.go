package catalog

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/migrations"
)

type roundTripFn func(*http.Request) (*http.Response, error)

func (f roundTripFn) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func mockPosterClient(png []byte) *http.Client {
	return &http.Client{Transport: roundTripFn(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/png"}},
			Body:       io.NopCloser(bytes.NewReader(png)),
		}, nil
	})}
}

func TestCatalogMigrationBackfillFromBeersAndKegs(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过数据库测试")
	}

	ctx := context.Background()
	schema := fmt.Sprintf("test_cat_mig_%d", os.Getpid())
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接 TEST_DATABASE_URL 失败: %v", err)
	}
	if _, err := admin.Exec(ctx, `drop schema if exists `+pgx.Identifier{schema}.Sanitize()+` cascade`); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `create schema `+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	admin.Close()
	t.Cleanup(func() {
		if a, err := pgxpool.New(context.Background(), dsn); err == nil {
			a.Exec(context.Background(), `drop schema if exists `+pgx.Identifier{schema}.Sanitize()+` cascade`)
			a.Close()
		}
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `create table if not exists schema_migrations (name text primary key, applied_at timestamptz not null default now())`); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if name >= "0010_" {
			continue
		}
		sqlBytes, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := platform.MigrateOneForTest(ctx, conn, name, string(sqlBytes)); err != nil {
			t.Fatalf("前置迁移 %s 失败: %v", name, err)
		}
	}

	var b1, b2 int64
	if err := conn.QueryRow(ctx, `insert into beers (brewery, name, style, category, abv, price_330, tags, poster_url)
		values ('酿酒狗', '朋克 IPA', 'IPA - American', 'hop', 5.2, 4500, '{"美式IPA","苦爽"}', 'https://assets.untappd.com/punk.jpg') returning id`,
	).Scan(&b1); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `insert into kegs (beer_id, capacity_ml, cost_cents) values ($1, 20000, 100000), ($1, 30000, 135000)`, b1); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `insert into beers (brewery, name, style, category, price_500)
		values ('唯森', '小麦白啤', 'Wheat Beer - Hefeweizen', 'fresh', 5800) returning id`,
	).Scan(&b2); err != nil {
		t.Fatal(err)
	}

	sql0010, err := fs.ReadFile(migrations.FS, "0010_beer_catalog.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.MigrateOneForTest(ctx, conn, "0010_beer_catalog.sql", string(sql0010)); err != nil {
		t.Fatalf("0010 迁移执行失败: %v", err)
	}

	items, err := ListCatalogBeers(ctx, pool, "", "")
	if err != nil {
		t.Fatalf("ListCatalogBeers: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("回填数量 = %d，应为 2", len(items))
	}
	var punk *CatalogBeer
	for i := range items {
		if items[i].Name == "朋克 IPA" {
			punk = &items[i]
		}
	}
	if punk == nil || punk.CapacityML != 30000 || punk.CostCents != 135000 || punk.Price330 != 4500 {
		t.Fatalf("朋克 IPA 迁移回填结果不对: %+v", punk)
	}
}

func TestCatalogImportModesAndHTTPExportImport(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	cfg := settings.NewSettingsStore(pool, "PourOps")
	store, err := NewPosterStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.SetHTTPClientForTest(mockPosterClient(samplePNGBytes()), false)
	svc := NewService(pool, cfg, nil, store)
	ctx := context.Background()

	oldLocalPoster, err := store.SaveBytes(samplePNGBytes())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCatalogBeer(ctx, pool, CatalogBeer{
		Brewery:    "树屋",
		Name:       "尤利乌斯",
		Style:      "IPA - Hazy",
		Category:   "IPA",
		PosterURL:  oldLocalPoster,
		CapacityML: 20000,
		CostCents:  130000,
		Price330:   5800,
	}); err != nil {
		t.Fatal(err)
	}

	partialCSV := "厂牌,酒名,330ml售价元\n树屋,尤利乌斯,66\n新厂,新品拉格,42\n"
	partialItems, err := ParseCatalogImport("part.csv", []byte(partialCSV))
	if err != nil {
		t.Fatal(err)
	}

	resSkip, err := ImportCatalogBeersWithPosters(ctx, pool, store, partialItems, "skip", platform.DefaultBeerCategories)
	if err != nil {
		t.Fatal(err)
	}
	if resSkip.Inserted != 1 || resSkip.Updated != 0 || resSkip.Skipped != 1 {
		t.Fatalf("skip 统计不对: %+v", resSkip)
	}

	resMerge, err := ImportCatalogBeersWithPosters(ctx, pool, store, partialItems, "merge", platform.DefaultBeerCategories)
	if err != nil {
		t.Fatal(err)
	}
	if resMerge.Inserted != 0 || resMerge.Updated != 2 || resMerge.Skipped != 0 {
		t.Fatalf("merge 统计不对: %+v", resMerge)
	}
	after, err := ListCatalogBeers(ctx, pool, "尤利乌斯", "")
	if err != nil || len(after) != 1 {
		t.Fatal(err)
	}
	if after[0].Price330 != 6600 || after[0].PosterURL != oldLocalPoster || after[0].CostCents != 130000 {
		t.Fatalf("部分列 merge 不该清除原有海报或进价: %+v", after[0])
	}

	// 导出为自包含 .zip 压缩包。
	recExp := httptest.NewRecorder()
	svc.CatalogExport(recExp, httptest.NewRequest(http.MethodGet, "/admin/settings/catalog/export", nil))
	if recExp.Code != http.StatusOK || !strings.Contains(recExp.Header().Get("Content-Type"), "application/zip") {
		t.Fatalf("导出 ZIP 响应不对: code=%d ct=%q", recExp.Code, recExp.Header().Get("Content-Type"))
	}
	zr, err := zip.NewReader(bytes.NewReader(recExp.Body.Bytes()), int64(recExp.Body.Len()))
	if err != nil {
		t.Fatalf("导出的响应不是有效 ZIP: %v", err)
	}
	if len(zr.File) != 2 {
		t.Fatalf("ZIP 应包含 catalog.json 与 1 张酒标，实际 %d 个文件", len(zr.File))
	}

	// 导入前替换新图片并校验导入后旧酒标已从磁盘移除，仅保留最新一张。
	postZip := func(body []byte) *httptest.ResponseRecorder {
		var mp bytes.Buffer
		mw := multipart.NewWriter(&mp)
		fw, err := mw.CreateFormFile("file", "roundtrip.zip")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(body); err != nil {
			t.Fatal(err)
		}
		_ = mw.WriteField("mode", "merge")
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/admin/settings/catalog/import", &mp)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		svc.CatalogImport(rec, req)
		return rec
	}
	assertImportFlash := func(rec *httptest.ResponseRecorder, wantMsg string) {
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/settings" {
			t.Fatalf("导入应 303 跳转到 /admin/settings，实际 code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
		}
		msg, errMsg := platform.ReadFlashCookies(rec.Result().Cookies())
		if msg != wantMsg || errMsg != "" {
			t.Fatalf("导入提示不对: msg=%q errMsg=%q", msg, errMsg)
		}
	}
	catalogPoster := func() string {
		rows, err := ListCatalogBeers(ctx, pool, "尤利乌斯", "")
		if err != nil || len(rows) != 1 {
			t.Fatalf("查询酒标引用失败: rows=%d err=%v", len(rows), err)
		}
		return rows[0].PosterURL
	}

	assertImportFlash(postZip(recExp.Body.Bytes()), "导入完成：新增 0 款，更新 2 款，跳过 0 款")
	if store.Exists(oldLocalPoster) {
		t.Fatalf("覆盖导入后旧本地酒标 %s 应被清理，仅保留最新版本", oldLocalPoster)
	}
	sum := sha256.Sum256(samplePNGBytes())
	wantDedup := PosterURLPrefix + hex.EncodeToString(sum[:16]) + ".png"
	if dedupPoster := catalogPoster(); dedupPoster != wantDedup || !store.Exists(dedupPoster) {
		t.Fatalf("酒标应按内容哈希去重落盘: got=%q want=%q exists=%v", dedupPoster, wantDedup, store.Exists(dedupPoster))
	}

	// 相同字节的 ZIP 再次导入：dedup 复用同一文件名，绝不能把文件删掉。
	assertImportFlash(postZip(recExp.Body.Bytes()), "导入完成：新增 0 款，更新 2 款，跳过 0 款")
	if again := catalogPoster(); again != wantDedup || !store.Exists(wantDedup) {
		t.Fatalf("相同字节重导入应复用去重酒标: url=%q exists=%v", again, store.Exists(wantDedup))
	}

	// 换成不同字节：生成新文件名，旧去重文件无引用后被清理。
	if _, err := ImportCatalogBeersWithPosters(ctx, pool, store, []ImportItem{{
		Beer:   CatalogBeer{Brewery: "树屋", Name: "尤利乌斯", Style: "IPA - Hazy", Category: "IPA", CapacityML: 20000},
		Poster: &PosterPayload{Filename: "julius.jpg", Data: sampleJPEGBytes()},
	}}, "merge", platform.DefaultBeerCategories); err != nil {
		t.Fatalf("更换酒标导入失败: %v", err)
	}
	newPoster := catalogPoster()
	if newPoster == wantDedup || !store.Exists(newPoster) {
		t.Fatalf("不同字节应写入新的去重文件: url=%q", newPoster)
	}
	if store.Exists(wantDedup) {
		t.Fatalf("旧去重酒标 %s 已无引用，应被清理", wantDedup)
	}

	// 非 zip 上传（如直接传 CSV）应返回明确提示。
	var csvMP bytes.Buffer
	cw := multipart.NewWriter(&csvMP)
	cfw, _ := cw.CreateFormFile("file", "bad.csv")
	_, _ = cfw.Write([]byte("酒名\n测试\n"))
	_ = cw.Close()
	reqCSV := httptest.NewRequest(http.MethodPost, "/admin/settings/catalog/import", &csvMP)
	reqCSV.Header.Set("Content-Type", cw.FormDataContentType())
	recCSV := httptest.NewRecorder()
	svc.CatalogImport(recCSV, reqCSV)
	if recCSV.Code != http.StatusSeeOther || recCSV.Header().Get("Location") != "/admin/settings" {
		t.Fatalf("非 zip 文件导入应 303 跳转到 /admin/settings，实际 code=%d loc=%q", recCSV.Code, recCSV.Header().Get("Location"))
	}
	if msg, errMsg := platform.ReadFlashCookies(recCSV.Result().Cookies()); msg != "" || !strings.Contains(errMsg, ".zip") {
		t.Fatalf("非 zip 文件导入应提示 .zip，实际 msg=%q errMsg=%q", msg, errMsg)
	}

	badBatch := []ImportItem{
		{Beer: CatalogBeer{Brewery: "回滚厂", Name: "好行", CapacityML: 20000}, Poster: &PosterPayload{Filename: "a.png", Data: samplePNGBytes()}},
		{Beer: CatalogBeer{Brewery: "回滚厂", Name: ""}},
	}
	if _, err := ImportCatalogBeersWithPosters(ctx, pool, store, badBatch, "merge", platform.DefaultBeerCategories); err == nil {
		t.Fatal("包含空酒名的批次应失败")
	}
	if found, err := ListCatalogBeers(ctx, pool, "回滚厂", ""); err != nil || len(found) != 0 {
		t.Fatalf("批内失败必须整批回滚，实际 found=%d err=%v", len(found), err)
	}
}

func TestLocalizeAllPostersDownloadsOnceAndUpdatesBothTables(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	store, err := NewPosterStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.SetHTTPClientForTest(mockPosterClient(samplePNGBytes()), false)
	ctx := context.Background()

	const remoteURL = "https://cdn.example/beer-42.png"
	b := Beer{Brewery: "高岩", Name: "苹果西打", Style: "Dry Cider", Category: "西打", PosterURL: remoteURL, Price330: 4200}
	if _, err := SaveBeerWithCategories(ctx, pool, b, platform.DefaultBeerCategories); err != nil {
		t.Fatal(err)
	}
	if err := UpsertCatalogFromBeerWithCategories(ctx, pool, b, 20000, 90000, true, platform.DefaultBeerCategories); err != nil {
		t.Fatal(err)
	}

	n := LocalizeAllPosters(ctx, pool, store)
	if n != 1 {
		t.Fatalf("LocalizeAllPosters = %d, want 1", n)
	}
	var beerPoster, catPoster string
	if err := pool.QueryRow(ctx, `select poster_url from beers where name='苹果西打'`).Scan(&beerPoster); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select poster_url from beer_catalog where name='苹果西打'`).Scan(&catPoster); err != nil {
		t.Fatal(err)
	}
	if !IsLocalPosterURL(beerPoster) || beerPoster != catPoster || !store.Exists(beerPoster) {
		t.Fatalf("远程酒标本地化同步失败: beer=%q catalog=%q", beerPoster, catPoster)
	}
}

func TestCatalogSaveStrictValidation(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	overInt := 2147483648
	badABV := 101.0
	negIBU := -1
	badUT := 5.5

	for _, tc := range []struct {
		name string
		item CatalogBeer
		want string
	}{
		{"空酒名", CatalogBeer{CapacityML: 20000}, "酒款名称"},
		{"坏分类", CatalogBeer{Name: "A", Category: "rainbow", CapacityML: 20000}, "酒款分类"},
		{"坏ABV", CatalogBeer{Name: "A", ABV: &badABV, CapacityML: 20000}, "ABV"},
		{"坏IBU", CatalogBeer{Name: "A", IBU: &negIBU, CapacityML: 20000}, "IBU"},
		{"坏UT", CatalogBeer{Name: "A", UT: &badUT, CapacityML: 20000}, "UT"},
		{"坏酒标", CatalogBeer{Name: "A", PosterURL: "javascript:alert(1)", CapacityML: 20000}, "酒标"},
		{"坏售价", CatalogBeer{Name: "A", Price330: overInt, CapacityML: 20000}, "金额过大"},
	} {
		if _, err := SaveCatalogBeerWithCategories(ctx, pool, tc.item, platform.DefaultBeerCategories); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s 应拒绝并包含 %q，实际: %v", tc.name, tc.want, err)
		}
	}
}

func TestCatalogImportConcurrentWithUpsertNo23505(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()

	for _, mode := range []string{"skip", "merge"} {
		brewery := "并发厂-" + mode
		name := "并发酒-" + mode
		items := []ImportItem{{
			Beer: CatalogBeer{
				Brewery:    brewery,
				Name:       name,
				Style:      "IPA - American",
				Category:   "IPA",
				CapacityML: 20000,
				Price330:   4800,
			},
			Mask: ImportFieldMask{Brewery: true, Style: true, Category: true, CapacityML: true, Price330: true},
		}}

		const workers = 6
		errCh := make(chan error, workers*2)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range workers {
			wg.Add(2)
			go func(idx int) {
				defer wg.Done()
				<-start
				if _, err := ImportCatalogBeers(ctx, pool, items, mode); err != nil {
					errCh <- fmt.Errorf("import(%s) #%d: %w", mode, idx, err)
				}
			}(i)
			go func(idx int) {
				defer wg.Done()
				<-start
				b := Beer{Brewery: brewery, Name: name, Style: "IPA - Hazy", Category: "IPA", Price330: 5000}
				if err := UpsertCatalogFromBeer(ctx, pool, b, 20000, 120000, true); err != nil {
					errCh <- fmt.Errorf("upsert(%s) #%d: %w", mode, idx, err)
				}
			}(i)
		}
		close(start)
		wg.Wait()
		close(errCh)

		for err := range errCh {
			t.Fatal(err)
		}
		got, err := ListCatalogBeers(ctx, pool, name, "")
		if err != nil || len(got) != 1 {
			t.Fatalf("mode=%s 并发后应仅存 1 条记录，实际 len=%d err=%v", mode, len(got), err)
		}
	}
}

func TestCatalogDBErrorSanitization(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("未设 TEST_DATABASE_URL，跳过 DB 集成用例")
	}
	closedPool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	closedPool.Close()

	badSvc := NewService(closedPool, nil, nil, nil)

	zipBuf := bytes.NewBuffer(nil)
	zw := zip.NewWriter(zipBuf)
	cw, _ := zw.Create("catalog.json")
	_, _ = cw.Write([]byte(`{"items":[{"name":"测试"}]}`))
	_ = zw.Close()

	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	fw, _ := mw.CreateFormFile("file", "test.zip")
	_, _ = fw.Write(zipBuf.Bytes())
	_ = mw.Close()
	reqImp := httptest.NewRequest(http.MethodPost, "/admin/settings/catalog/import", &mp)
	reqImp.Header.Set("Content-Type", mw.FormDataContentType())
	wImp := httptest.NewRecorder()
	badSvc.CatalogImport(wImp, reqImp)
	if wImp.Code != http.StatusSeeOther || wImp.Header().Get("Location") != "/admin/settings" {
		t.Fatalf("CatalogImport DB 错误应 303 跳转到 /admin/settings: code=%d loc=%q", wImp.Code, wImp.Header().Get("Location"))
	}
	msgImp, errImp := platform.ReadFlashCookies(wImp.Result().Cookies())
	if msgImp != "" || !strings.Contains(errImp, "导入失败，请稍后重试") || strings.Contains(errImp, "closed pool") {
		t.Fatalf("CatalogImport DB 错误未脱敏: msg=%q errMsg=%q", msgImp, errImp)
	}
}

func TestMigration0012BeerCategoriesAndDynamicSettings(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过数据库测试")
	}

	ctx := context.Background()
	schema := fmt.Sprintf("test_cat_mig12_%d", os.Getpid())
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接 TEST_DATABASE_URL 失败: %v", err)
	}
	if _, err := admin.Exec(ctx, `drop schema if exists `+pgx.Identifier{schema}.Sanitize()+` cascade`); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `create schema `+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	admin.Close()
	t.Cleanup(func() {
		if a, err := pgxpool.New(context.Background(), dsn); err == nil {
			a.Exec(context.Background(), `drop schema if exists `+pgx.Identifier{schema}.Sanitize()+` cascade`)
			a.Close()
		}
	})

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema
	migPool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer migPool.Close()

	conn, err := migPool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `create table if not exists schema_migrations (name text primary key, applied_at timestamptz not null default now())`); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if name >= "0012_" {
			continue
		}
		sqlBytes, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := platform.MigrateOneForTest(ctx, conn, name, string(sqlBytes)); err != nil {
			t.Fatalf("前置迁移 %s 失败: %v", name, err)
		}
	}

	seeds := []struct {
		name, style, oldCat, wantCat string
	}{
		{"朋克", "IPA - American", "hop", "IPA"},
		{"高岩西打", "Dry Cider", "fresh", "西打"},
		{"老贵兹", "Lambic - Gueuze", "酸野菌", "酸小麦"},
		{"早餐世涛", "Imperial Stout", "深色", "世涛"},
		{"三料修道", "Belgian Tripel", "strong", "修道院"},
		{"农舍赛松", "Farmhouse Saison", "赛松", "赛松"},
	}
	for _, s := range seeds {
		if _, err := conn.Exec(ctx, `insert into beers (name, style, category) values ($1, $2, $3)`, s.name, s.style, s.oldCat); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, `insert into beer_catalog (name, style, category) values ($1, $2, $3)`, s.name, s.style, s.oldCat); err != nil {
			t.Fatal(err)
		}
	}

	sql0012Bytes, err := fs.ReadFile(migrations.FS, "0012_beer_categories.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.MigrateOneForTest(ctx, conn, "0012_beer_categories.sql", string(sql0012Bytes)); err != nil {
		t.Fatalf("0012 迁移执行失败: %v", err)
	}
	// 再次直接重跑 0012 SQL 确保幂等。
	if _, err := conn.Exec(ctx, string(sql0012Bytes)); err != nil {
		t.Fatalf("0012 重复执行应幂等，实际报错: %v", err)
	}

	for _, s := range seeds {
		var beerCat, catCat string
		if err := conn.QueryRow(ctx, `select category from beers where name=$1`, s.name).Scan(&beerCat); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRow(ctx, `select category from beer_catalog where name=$1`, s.name).Scan(&catCat); err != nil {
			t.Fatal(err)
		}
		if beerCat != s.wantCat || catCat != s.wantCat {
			t.Fatalf("%s 迁移结果不对: beers=%q beer_catalog=%q, want %q", s.name, beerCat, catCat, s.wantCat)
		}
	}

	// 动态自定义分类：新增“赛松”可保存；删除预设后编辑原酒款仍可保留，新建则拒绝。
	pool := platform.NewTestPool(t, migrations.FS)
	cfg := settings.NewSettingsStore(pool, "PourOps")
	if err := cfg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	allowed, err := cfg.AddBeerCategory(ctx, "赛松")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := SaveCatalogBeerWithCategories(ctx, pool, CatalogBeer{Name: "农舍赛松", Category: "赛松", CapacityML: 20000}, allowed); err != nil {
		t.Fatalf("新增自定义分类历史酒款失败: %v", err)
	}
	list, err := ListCatalogBeers(ctx, pool, "农舍赛松", "赛松")
	if err != nil || len(list) != 1 || list[0].Category != "赛松" {
		t.Fatalf("按自定义分类筛选历史酒单失败: len=%d list=%+v err=%v", len(list), list, err)
	}

	allowedAfterDel, err := cfg.DeleteBeerCategory(ctx, "赛松")
	if err != nil {
		t.Fatal(err)
	}
	edited := list[0]
	edited.Name = "农舍赛松-编辑"
	if _, err := SaveCatalogBeerWithCategories(ctx, pool, edited, allowedAfterDel, list[0].Category); err != nil {
		t.Fatalf("分类被删后编辑既有历史酒款仍应成功: %v", err)
	}
	if _, err := SaveCatalogBeerWithCategories(ctx, pool, CatalogBeer{Name: "新赛松", Category: "赛松", CapacityML: 20000}, allowedAfterDel); err == nil {
		t.Fatal("分类被删后新建历史酒款应拒绝")
	}
}
