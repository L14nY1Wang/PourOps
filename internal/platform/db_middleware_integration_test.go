package platform_test

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pourops/internal/platform"
	"pourops/migrations"
)

// 每个迁移的 DDL 与记录同事务：失败回滚，重启后能重跑，重复启动幂等。
func TestMigrateRollbackAndIdempotent(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	if err := platform.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("重复迁移应该幂等: %v", err)
	}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("读迁移目录: %v", err)
	}
	wantMigrations := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			wantMigrations++
		}
	}
	var n1 int
	if err := pool.QueryRow(ctx, `select count(*) from schema_migrations`).Scan(&n1); err != nil {
		t.Fatalf("数迁移记录: %v", err)
	}
	if n1 != wantMigrations {
		t.Fatalf("迁移数量 = %d, want %d", n1, wantMigrations)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("取连接: %v", err)
	}
	defer conn.Release()

	const name = "9999_test_rollback"
	if _, err := conn.Exec(ctx, `delete from schema_migrations where name='9999_test_rollback'`); err != nil {
		t.Fatalf("清理探针记录: %v", err)
	}
	if _, err := conn.Exec(ctx, `drop table if exists rollback_probe`); err != nil {
		t.Fatalf("清理探针表: %v", err)
	}
	if err := platform.MigrateOneForTest(ctx, conn, name, "create table rollback_probe (id int); select 1/0;"); err == nil {
		t.Fatal("坏迁移应该报错")
	}
	var exists bool
	if err := conn.QueryRow(ctx, `select to_regclass('rollback_probe') is not null`).Scan(&exists); err != nil {
		t.Fatalf("查表: %v", err)
	}
	if exists {
		t.Fatal("失败的迁移不该留下建好的表")
	}
	var recorded bool
	if err := conn.QueryRow(ctx, `select exists(select 1 from schema_migrations where name=$1)`, name).Scan(&recorded); err != nil {
		t.Fatalf("查迁移记录: %v", err)
	}
	if recorded {
		t.Fatal("失败的迁移不该记进 schema_migrations")
	}

	if err := platform.MigrateOneForTest(ctx, conn, name, "create table rollback_probe (id int);"); err != nil {
		t.Fatalf("修好的迁移应能执行: %v", err)
	}
	if err := conn.QueryRow(ctx, `select to_regclass('rollback_probe') is not null`).Scan(&exists); err != nil || !exists {
		t.Fatalf("修好的迁移没建表: exists=%v err=%v", exists, err)
	}
}

func TestCrossOriginProtect(t *testing.T) {
	h := platform.CrossOriginProtect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	cases := []struct {
		name      string
		method    string
		origin    string
		fetchSite string
		want      int
	}{
		{"跨站 POST", "POST", "https://evil.example", "", http.StatusForbidden},
		{"Sec-Fetch-Site 跨站 POST", "POST", "", "cross-site", http.StatusForbidden},
		{"同源表单 POST", "POST", "http://bar.local", "", http.StatusNoContent},
		{"没有 Origin 的本机 POST", "POST", "", "", http.StatusNoContent},
		{"跨站 GET", "GET", "https://evil.example", "", http.StatusNoContent},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "http://bar.local/pos/open", nil)
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if c.fetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", c.fetchSite)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: 状态 %d，想要 %d", c.name, rec.Code, c.want)
		}
	}
}
