package catalog

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/migrations"
)

func TestCleanupUnusedPostersSerializedByAdvisoryLock(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	store, err := NewPosterStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	posterURL, err := store.SaveBytes(samplePNGBytes())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 事务 1 持锁准备写入引用该本地酒标；并发清理必须等待事务提交并看到引用，绝不能删图
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := LockPosterURLTx(ctx, tx, posterURL); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		CleanupUnusedPosters(ctx, pool, store, posterURL)
	}()

	time.Sleep(40 * time.Millisecond)
	if !store.Exists(posterURL) {
		_ = tx.Rollback(ctx)
		wg.Wait()
		t.Fatalf("持锁事务未提交前清理不应删掉本地酒标")
	}

	if _, err := SaveBeer(ctx, tx, Beer{Name: "锁测试IPA", Category: "IPA", PosterURL: posterURL}); err != nil {
		_ = tx.Rollback(ctx)
		wg.Wait()
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		wg.Wait()
		t.Fatal(err)
	}
	wg.Wait()

	if !store.Exists(posterURL) {
		t.Fatalf("事务提交引用后本地酒标 %s 被误删", posterURL)
	}
}

func TestZIPCodecEncodeToStreamingAndBounds(t *testing.T) {
	store, err := NewPosterStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	u, err := store.SaveBytes(samplePNGBytes())
	if err != nil {
		t.Fatal(err)
	}
	c := ZIPCodec{Posters: store}

	var buf bytes.Buffer
	err = c.EncodeTo(&buf, []CatalogBeer{{Brewery: "厂", Name: "酒A", PosterURL: u, CapacityML: 20000}}, "PourOps", time.Now())
	if err != nil {
		t.Fatalf("EncodeTo: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil || len(zr.File) != 2 {
		t.Fatalf("streaming ZIP entries = %v, err=%v", len(zr.File), err)
	}

	tooMany := make([]CatalogBeer, ImportMaxRows+1)
	for i := range tooMany {
		tooMany[i] = CatalogBeer{Name: fmt.Sprintf("B-%d", i), CapacityML: 20000}
	}
	var overBuf bytes.Buffer
	if err := c.EncodeTo(&overBuf, tooMany, "PourOps", time.Now()); err == nil || overBuf.Len() != 0 {
		t.Fatalf("over ImportMaxRows should fail before writing any bytes; err=%v len=%d", err, overBuf.Len())
	} else if _, ok := platform.UserErrorMessage(err); !ok {
		t.Fatalf("expected UserErr for oversized export, got %T (%v)", err, err)
	}
}

func TestImportWithDeletedCategoryPreservesExistingAndFallsBackNew(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	cfg := settings.NewSettingsStore(pool, "PourOps")
	if err := cfg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	allowedWithCustom, err := cfg.AddBeerCategory(ctx, "赛松")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCatalogBeerWithCategories(ctx, pool, CatalogBeer{
		Brewery:    "农舍",
		Name:       "经典赛松",
		Style:      "Saison",
		Category:   "赛松",
		CapacityML: 20000,
	}, allowedWithCustom); err != nil {
		t.Fatal(err)
	}

	// 删除“赛松”分类后导出并重新导入，同时附带一条同样带已删分类的新增酒款
	allowedAfterDel, err := cfg.DeleteBeerCategory(ctx, "赛松")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(pool, cfg, nil, nil)
	recExp := httptest.NewRecorder()
	svc.CatalogExport(recExp, httptest.NewRequest(http.MethodGet, "/admin/settings/catalog/export", nil))
	if recExp.Code != http.StatusOK {
		t.Fatalf("CatalogExport status=%d", recExp.Code)
	}

	items, err := svc.codecs.Decode("backup.zip", recExp.Body.Bytes())
	if err != nil {
		t.Fatalf("Decode deleted category backup failed (P2-17): %v", err)
	}
	items = append(items, ImportItem{
		Beer: CatalogBeer{Brewery: "新厂", Name: "已删分类新品", Style: "Hazy IPA", Category: "赛松", CapacityML: 20000},
		Mask: ImportFieldMask{Brewery: true, Style: true, Category: true, CapacityML: true},
	})

	res, err := ImportCatalogBeersWithPosters(ctx, pool, nil, items, "merge", allowedAfterDel)
	if err != nil {
		t.Fatalf("ImportCatalogBeersWithPosters failed: %v", err)
	}
	if res.Updated != 1 || res.Inserted != 1 {
		t.Fatalf("unexpected import counts: %+v", res)
	}
	all, err := ListCatalogBeers(ctx, pool, "", "")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]CatalogBeer{}
	for _, b := range all {
		byName[b.Name] = b
	}
	if byName["经典赛松"].Category != "赛松" {
		t.Fatalf("existing category lost: got %q", byName["经典赛松"].Category)
	}
	if byName["已删分类新品"].Category != "IPA" {
		t.Fatalf("new row deleted category should fall back via InferCategory(IPA), got %q", byName["已删分类新品"].Category)
	}
}

func TestExportDoesNotDialOrMutateAndImportLocalizesAsync(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	cfg := settings.NewSettingsStore(pool, "PourOps")
	store, err := NewPosterStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var dials atomic.Int32
	gate := make(chan struct{})
	doneCh := make(chan struct{}, 1)
	closed := false
	t.Cleanup(func() {
		if !closed {
			close(gate)
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = store.Close(closeCtx)
	})
	store.SetHTTPClientForTest(&http.Client{Transport: roundTripFn(func(r *http.Request) (*http.Response, error) {
		dials.Add(1)
		<-gate
		select {
		case doneCh <- struct{}{}:
		default:
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/png"}},
			Body:       io.NopCloser(bytes.NewReader(samplePNGBytes())),
		}, nil
	})}, false)

	remoteURL := "https://slow.cdn.example/p.png"
	if _, err := SaveCatalogBeer(ctx, pool, CatalogBeer{
		Brewery: "慢厂", Name: "慢图酒", PosterURL: remoteURL, CapacityML: 20000,
	}); err != nil {
		t.Fatal(err)
	}

	svc := NewService(pool, cfg, nil, store)
	rec := httptest.NewRecorder()
	svc.CatalogExport(rec, httptest.NewRequest(http.MethodGet, "/admin/settings/catalog/export", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("CatalogExport status=%d", rec.Code)
	}
	if dials.Load() != 0 {
		t.Fatalf("GET CatalogExport must not synchronously fetch remote posters, dials=%d", dials.Load())
	}

	start := time.Now()
	_, err = ImportCatalogBeersWithPosters(ctx, pool, store, []ImportItem{{
		Beer: CatalogBeer{Brewery: "导厂", Name: "导图酒", PosterURL: "https://slow.cdn.example/imp.png", CapacityML: 20000},
		Mask: ImportFieldMask{Brewery: true, PosterURL: true, CapacityML: true},
	}}, "merge", platform.DefaultBeerCategories)
	if err != nil {
		t.Fatalf("ImportCatalogBeersWithPosters: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Import blocked on remote poster download: %v", elapsed)
	}
	close(gate)
	closed = true
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("async localize did not run")
	}
	// 等待后台写入 beer_catalog 落盘完成，避免测试提前关池
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var cur string
		if err := pool.QueryRow(ctx, `select poster_url from beer_catalog where name='导图酒'`).Scan(&cur); err == nil && IsLocalPosterURL(cur) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMigration0013CategoryAlignmentParity(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过数据库测试")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("test_cat0013_%d", os.Getpid())
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = admin.Exec(ctx, `drop schema if exists `+pgx.Identifier{schema}.Sanitize()+` cascade`)
	if _, err := admin.Exec(ctx, `create schema `+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	admin.Close()
	t.Cleanup(func() {
		if a, err := pgxpool.New(context.Background(), dsn); err == nil {
			_, _ = a.Exec(context.Background(), `drop schema if exists `+pgx.Identifier{schema}.Sanitize()+` cascade`)
			a.Close()
		}
	})

	cfg, _ := pgxpool.ParseConfig(dsn)
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
	_, _ = conn.Exec(ctx, `create table if not exists schema_migrations (name text primary key, applied_at timestamptz not null default now())`)

	entries, _ := fs.ReadDir(migrations.FS, ".")
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if name >= "0013_" {
			continue
		}
		sqlBytes, _ := fs.ReadFile(migrations.FS, name)
		if err := platform.MigrateOneForTest(ctx, conn, name, string(sqlBytes)); err != nil {
			t.Fatalf("migrate %s: %v", name, err)
		}
	}

	_, err = conn.Exec(ctx, `insert into beers (name, style, category) values
		('老波特', 'Baltic Porter', '波特'),
		('三料', 'Belgian Tripel', '修道院'),
		('老兰比克', 'Lambic - Gueuze', '酸小麦'),
		('古斯', 'Gose', '酸小麦')`)
	if err != nil {
		t.Fatal(err)
	}

	sql0013, _ := fs.ReadFile(migrations.FS, "0013_integrity_indexes_and_fixes.sql")
	if err := platform.MigrateOneForTest(ctx, conn, "0013_integrity_indexes_and_fixes.sql", string(sql0013)); err != nil {
		t.Fatalf("0013: %v", err)
	}

	rows, err := conn.Query(ctx, `select name, style, category from beers`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, style, got string
		if err := rows.Scan(&name, &style, &got); err != nil {
			t.Fatal(err)
		}
		if name == "三料" {
			if got != "世涛" {
				t.Fatalf("三料 (修道院 unconfigured): SQL category=%q, want 世涛", got)
			}
			continue
		}
		want := platform.InferCategory(style, platform.DefaultBeerCategories)
		if got != want {
			t.Fatalf("%s (%s): SQL category=%q, Go InferCategory=%q", name, style, got, want)
		}
	}
}

func TestCleanupUnusedPostersRunsAfterClientCancel(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	store, err := NewPosterStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := store.SaveBytes(samplePNGBytes())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟客户端断开：HTTP 请求上下文已被取消

	CleanupUnusedPosters(ctx, pool, store, orphan)
	if store.Exists(orphan) {
		t.Fatalf("客户端断开后仍应清理无人引用的酒标 %s", orphan)
	}
}

func TestImportRollbackPreservesSharedPosterAndDeletesOrphan(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	store, err := NewPosterStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	sharedImg := samplePNGBytes()
	sharedURL, _, err := store.saveBytesDedup(sharedImg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(sharedURL); err != nil {
		t.Fatal(err)
	}
	// 先有一款在库酒引用该内容哈希对应的 URL，模拟并发已提交引用。
	if _, err := SaveBeer(ctx, pool, Beer{Name: "已引用共享酒标", Category: "IPA", PosterURL: sharedURL}); err != nil {
		t.Fatal(err)
	}

	orphanImg := sampleJPEGBytes()
	orphanURL, _, err := store.saveBytesDedup(orphanImg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(orphanURL); err != nil {
		t.Fatal(err)
	}

	items := []ImportItem{
		{
			Beer:   CatalogBeer{Brewery: "A", Name: "正常新酒1", CapacityML: DefaultCapacityML},
			Mask:   ImportFieldMask{Brewery: true, CapacityML: true},
			Poster: &PosterPayload{Filename: "shared.png", Data: sharedImg},
		},
		{
			Beer:   CatalogBeer{Brewery: "A", Name: "正常新酒2", CapacityML: DefaultCapacityML},
			Mask:   ImportFieldMask{Brewery: true, CapacityML: true},
			Poster: &PosterPayload{Filename: "orphan.jpg", Data: orphanImg},
		},
		{
			Beer: CatalogBeer{Brewery: "A", Name: "触发回滚的非法成本酒", CostCents: -1},
			Mask: ImportFieldMask{Brewery: true, CostCents: true},
		},
	}
	if _, err := ImportCatalogBeersWithPosters(ctx, pool, store, items, "merge", platform.DefaultBeerCategories); err == nil {
		t.Fatal("空酒名条目应触发整批导入回滚")
	}
	if !store.Exists(sharedURL) {
		t.Fatalf("回滚清理不得误删已被在库 beers 引用的共享酒标 %s", sharedURL)
	}
	if store.Exists(orphanURL) {
		t.Fatalf("回滚后未被引用的新建酒标 %s 应被清理", orphanURL)
	}
}
