package catalog

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/internal/ui"
	"pourops/migrations"
)

func testCatalogRenderer() *ui.Renderer {
	fsys := fstest.MapFS{
		ui.AdminLayoutFile:                  {Data: []byte(`{{define "layout"}}{{.Err}}{{template "content" .}}{{end}}`)},
		"templates/admin/catalog_form.html": {Data: []byte(`{{define "content"}}<input name="name" value="{{.CatalogItem.Name}}"><input name="capacity_ml" value="{{.CatalogItem.CapacityML}}"><input name="cost" value="{{.CostInput}}">{{end}}`)},
		"templates/admin/catalog.html":      {Data: []byte(`{{define "content"}}{{range .Catalog}}{{.Name}}{{end}}{{end}}`)},
	}
	return ui.MustRenderer(fsys, nil, func() string { return "PourOps" }, "admin/catalog_form.html", "admin/catalog.html")
}

func postForm(h http.HandlerFunc, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
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
	svc := NewService(pool, testCatalogRenderer(), cfg, nil)
	ctx := context.Background()

	_, err := SaveCatalogBeer(ctx, pool, CatalogBeer{
		Brewery:    "树屋",
		Name:       "尤利乌斯",
		Style:      "IPA - Hazy",
		Category:   "hop",
		PosterURL:  "https://assets.untappd.com/julius.jpg",
		CapacityML: 20000,
		CostCents:  130000,
		Price330:   5800,
	})
	if err != nil {
		t.Fatal(err)
	}

	partialCSV := "厂牌,酒名,330ml售价元\n树屋,尤利乌斯,66\n新厂,新品拉格,42\n"
	partialItems, err := ParseCatalogImport("part.csv", []byte(partialCSV))
	if err != nil {
		t.Fatal(err)
	}

	resSkip, err := ImportCatalogBeers(ctx, pool, partialItems, "skip")
	if err != nil {
		t.Fatal(err)
	}
	if resSkip.Inserted != 1 || resSkip.Updated != 0 || resSkip.Skipped != 1 {
		t.Fatalf("skip 统计不对: %+v", resSkip)
	}

	resMerge, err := ImportCatalogBeers(ctx, pool, partialItems, "merge")
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
	if after[0].Price330 != 6600 || after[0].PosterURL != "https://assets.untappd.com/julius.jpg" || after[0].CostCents != 130000 {
		t.Fatalf("部分列 merge 不该清除原有海报或进价: %+v", after[0])
	}

	recExp := httptest.NewRecorder()
	svc.CatalogExport(recExp, httptest.NewRequest(http.MethodGet, "/admin/beers/catalog/export?format=csv", nil))
	if recExp.Code != http.StatusOK || !strings.Contains(recExp.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("导出 CSV 响应不对: code=%d ct=%q", recExp.Code, recExp.Header().Get("Content-Type"))
	}

	var mpBody bytes.Buffer
	mw := multipart.NewWriter(&mpBody)
	fw, err := mw.CreateFormFile("file", "roundtrip.csv")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(recExp.Body.Bytes())
	_ = mw.WriteField("mode", "merge")
	_ = mw.Close()

	reqImp := httptest.NewRequest(http.MethodPost, "/admin/beers/catalog/import", &mpBody)
	reqImp.Header.Set("Content-Type", mw.FormDataContentType())
	recImp := httptest.NewRecorder()
	svc.CatalogImport(recImp, reqImp)
	if recImp.Code != http.StatusSeeOther {
		t.Fatalf("导入应 303 重定向，实际 %d: %s", recImp.Code, recImp.Body.String())
	}
	loc, _ := url.QueryUnescape(recImp.Header().Get("Location"))
	if !strings.Contains(loc, "新增 0 款，更新 2 款，跳过 0 款") {
		t.Fatalf("导入提示不对: %s", loc)
	}

	badBatch := []ImportItem{
		{Beer: CatalogBeer{Brewery: "回滚厂", Name: "好行", CapacityML: 20000}},
		{Beer: CatalogBeer{Brewery: "回滚厂", Name: ""}},
	}
	if _, err := ImportCatalogBeers(ctx, pool, badBatch, "merge"); err == nil {
		t.Fatal("包含空酒名的批次应失败")
	}
	if found, err := ListCatalogBeers(ctx, pool, "回滚厂", ""); err != nil || len(found) != 0 {
		t.Fatalf("批内失败必须整批回滚，实际 found=%d err=%v", len(found), err)
	}
}

func TestCatalogSaveStrictValidation(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	svc := NewService(pool, testCatalogRenderer(), nil, nil)

	for _, tc := range []struct {
		field, val, want string
	}{
		{"name", "", "酒款名称不能为空"},
		{"capacity_ml", "2147483648", "超出范围"},
		{"cost", "30000000", "金额过大"},
		{"abv", "101", "ABV"},
		{"ibu", "-1", "IBU"},
		{"ut_rating", "5.5", "UT"},
		{"ut_count", "2147483648", "评分人数"},
		{"category", "rainbow", "分类色"},
		{"poster_url", "javascript:alert(1)", "酒标"},
		{"price_330", "30000000", "330ml"},
	} {
		form := url.Values{"name": {"历史酒测试"}, "capacity_ml": {"20000"}}
		form.Set(tc.field, tc.val)
		w := postForm(svc.CatalogSave, form)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
			t.Fatalf("%s=%q 应返回 400 且包含 %q，实际 code=%d body=%s", tc.field, tc.val, tc.want, w.Code, w.Body.String())
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
				Category:   "hop",
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
				b := Beer{Brewery: brewery, Name: name, Style: "IPA - Hazy", Category: "hop", Price330: 5000}
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

	badSvc := NewService(closedPool, testCatalogRenderer(), nil, nil)

	wCat := postForm(badSvc.CatalogSave, url.Values{"name": {"测试"}})
	if wCat.Code != http.StatusInternalServerError || !strings.Contains(wCat.Body.String(), "保存失败，请稍后重试") || strings.Contains(wCat.Body.String(), "closed pool") {
		t.Fatalf("CatalogSave DB 错误未脱敏: code=%d body=%s", wCat.Code, wCat.Body.String())
	}

	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	fw, _ := mw.CreateFormFile("file", "test.csv")
	_, _ = fw.Write([]byte("酒名\n测试\n"))
	_ = mw.Close()
	reqImp := httptest.NewRequest(http.MethodPost, "/admin/beers/catalog/import", &mp)
	reqImp.Header.Set("Content-Type", mw.FormDataContentType())
	wImp := httptest.NewRecorder()
	badSvc.CatalogImport(wImp, reqImp)
	locImp, _ := url.QueryUnescape(wImp.Header().Get("Location"))
	if wImp.Code != http.StatusSeeOther || !strings.Contains(locImp, "导入失败，请稍后重试") || strings.Contains(locImp, "closed pool") {
		t.Fatalf("CatalogImport DB 错误未脱敏: code=%d loc=%q", wImp.Code, locImp)
	}
}
