package platform

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier 是 *pgxpool.Pool 与 pgx.Tx 的共同子集，供跨包事务复用。
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// MigrationLockKey 是启动时序列化迁移的 advisory lock 键。
const MigrationLockKey int64 = 0x6e6f727468706965 // "northpie"

// Migrate 按文件名顺序执行迁移目录下的 *.sql，已执行过的记在 schema_migrations。
// 同一时刻只允许一个实例迁移；每个文件的 DDL 与记录在同一个事务里，失败整体回滚。
func Migrate(ctx context.Context, pool *pgxpool.Pool, migrationFS fs.FS) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1)`, MigrationLockKey); err != nil {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `select pg_advisory_unlock($1)`, MigrationLockKey)

	if _, err := conn.Exec(ctx, `create table if not exists schema_migrations (
		name text primary key, applied_at timestamptz not null default now())`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationFS, ".")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		raw, err := fs.ReadFile(migrationFS, name)
		if err != nil {
			return err
		}
		if err := migrateOne(ctx, conn, name, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

func MigrateOneForTest(ctx context.Context, conn *pgxpool.Conn, name, sqlText string) error {
	return migrateOne(ctx, conn, name, sqlText)
}

// migrateOne 在单个事务里跑一个文件的 SQL 并记下它；出错回滚，下次启动可重跑。
// 事务里放不了 create index concurrently，以后要加这种迁移得单独走一条路径。
func migrateOne(ctx context.Context, conn *pgxpool.Conn, name, sqlText string) error {
	var done bool
	if err := conn.QueryRow(ctx,
		`select exists(select 1 from schema_migrations where name=$1)`, name).Scan(&done); err != nil {
		return err
	}
	if done {
		return nil
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, sqlText); err != nil {
		return fmt.Errorf("迁移 %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx, `insert into schema_migrations (name) values ($1)`, name); err != nil {
		return fmt.Errorf("迁移 %s: %w", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

var testSchemaSeq atomic.Int64

// NewTestPool 给每个集成测试建一个独立 schema 并跑完迁移，用完删掉。
// 没设 TEST_DATABASE_URL 就跳过：默认 go test 不碰数据库。
func NewTestPool(t *testing.T, migrationFS fs.FS) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过数据库测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	schema := fmt.Sprintf("test_%d_%d", os.Getpid(), testSchemaSeq.Add(1))
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接 TEST_DATABASE_URL 失败: %v", err)
	}
	if _, err := admin.Exec(ctx, `create schema `+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatalf("建测试 schema 失败: %v", err)
	}
	admin.Close()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("解析 TEST_DATABASE_URL 失败: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := Migrate(ctx, pool, migrationFS); err != nil {
		pool.Close()
		t.Fatalf("测试 schema 迁移失败: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if a, err := pgxpool.New(cctx, dsn); err == nil {
			a.Exec(cctx, `drop schema if exists `+pgx.Identifier{schema}.Sanitize()+` cascade`)
			a.Close()
		}
	})
	return pool
}
