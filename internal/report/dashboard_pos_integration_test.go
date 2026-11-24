package report

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
	"pourops/migrations"
)

func execSQL(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func loadPOSInsightsSnapshot(t *testing.T, pool *pgxpool.Pool, start, end time.Time) POSInsights {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	in, err := LoadPOSInsights(ctx, tx, start, end)
	if err != nil {
		t.Fatalf("读 POS 概览失败: %v", err)
	}
	return in
}

func newPOSOrder(t *testing.T, pool *pgxpool.Pool, spot string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`insert into orders (spot) values ($1) returning id`, spot).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func newPOSPayment(t *testing.T, pool *pgxpool.Pool, order int64, cents int, at time.Time) {
	t.Helper()
	execSQL(t, pool, `insert into payments (order_id, amount_cents, items_count, created_at)
		values ($1, $2, 1, $3)`, order, cents, at)
}

func TestPOSInsightsPaymentsOnly(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	cst := platform.ReportZone
	start := time.Date(2026, 3, 10, 7, 0, 0, 0, cst)
	end := start.AddDate(0, 0, 1) // 次日 07:00

	split := newPOSOrder(t, pool, "bar-1")
	newPOSPayment(t, pool, split, 3000, time.Date(2026, 3, 10, 10, 15, 0, 0, cst))
	newPOSPayment(t, pool, split, 2000, time.Date(2026, 3, 10, 10, 45, 0, 0, cst))

	late := newPOSOrder(t, pool, "bar-2")
	newPOSPayment(t, pool, late, 2500, time.Date(2026, 3, 11, 2, 30, 0, 0, cst)) // 次日凌晨 02:30 归 03-10 营业日

	edge := newPOSOrder(t, pool, "table-1")
	newPOSPayment(t, pool, edge, 1000, start)
	newPOSPayment(t, pool, edge, 7000, end)

	prev := newPOSOrder(t, pool, "table-2")
	newPOSPayment(t, pool, prev, 9900, start.Add(-time.Minute))

	unpaid := newPOSOrder(t, pool, "table-3")
	execSQL(t, pool, `insert into order_items (order_id, kind, ref_id, name, unit_cents)
		values ($1, 'retail', 1, '未付商品', 5000)`, unpaid)

	in := loadPOSInsightsSnapshot(t, pool, start, end)
	if in.Received != 8500 || in.Orders != 3 || in.Payments != 4 {
		t.Fatalf("汇总不对: received=%d orders=%d payments=%d", in.Received, in.Orders, in.Payments)
	}
	if got := in.ReceivedText(); got != "¥ 85" {
		t.Fatalf("实收展示 = %q", got)
	}
	if got := in.AverageText(); got != "¥ 28.33" {
		t.Fatalf("客单价展示 = %q", got)
	}

	if len(in.Hours) != 24 {
		t.Fatalf("小时分布应固定 24 行，实际 %d", len(in.Hours))
	}
	var hourSum int64
	for i, h := range in.Hours {
		wantHour := (platform.BusinessDayStartHour + i) % 24
		if h.Hour != wantHour {
			t.Fatalf("第 %d 行的小时是 %d，应为 %d（07:00 起顺序）", i, h.Hour, wantHour)
		}
		hourSum += h.Received
	}
	if hourSum != in.Received {
		t.Fatalf("小时金额之和 %d 与实收 %d 不一致", hourSum, in.Received)
	}
	// index 0 -> 07:00, index 3 -> 10:00, index 19 -> 次日 02:00, index 23 -> 次日 06:00
	if in.Hours[0].Received != 1000 || in.Hours[0].Payments != 1 {
		t.Fatalf("07 点行不对: %+v", in.Hours[0])
	}
	if in.Hours[3].Received != 5000 || in.Hours[3].Payments != 2 {
		t.Fatalf("10 点行不对: %+v", in.Hours[3])
	}
	if in.Hours[19].Received != 2500 || in.Hours[19].Payments != 1 {
		t.Fatalf("02 点行不对: %+v", in.Hours[19])
	}
	for _, i := range []int{1, 2, 16, 17, 23} {
		if in.Hours[i].Received != 0 || in.Hours[i].Payments != 0 {
			t.Fatalf("空小时 index=%d 应为 0: %+v", i, in.Hours[i])
		}
	}
	if got := in.Hours[0].Label(); got != "07:00" {
		t.Fatalf("首行标签 = %q", got)
	}
	if got := in.Hours[23].Label(); got != "06:00" {
		t.Fatalf("末行标签 = %q", got)
	}
	if got := in.Hours[3].ReceivedText(); got != "¥ 50" {
		t.Fatalf("10 点实收展示 = %q", got)
	}

	var points []struct {
		Hour     string  `json:"hour"`
		Received float64 `json:"received"`
		Payments int64   `json:"payments"`
	}
	if err := json.Unmarshal([]byte(in.HoursJSON()), &points); err != nil {
		t.Fatalf("小时 JSON 解析失败: %v", err)
	}
	if len(points) != 24 {
		t.Fatalf("小时 JSON 应 24 项，实际 %d", len(points))
	}
	if points[0].Hour != "07:00" || points[3].Hour != "10:00" || points[3].Received != 50 || points[3].Payments != 2 || points[19].Hour != "02:00" {
		t.Fatalf("小时 JSON 不对: [0]=%+v [3]=%+v [19]=%+v", points[0], points[3], points[19])
	}
}

func TestPOSInsightsEmptyDay(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	cst := platform.ReportZone
	day := time.Date(2026, 3, 10, 7, 0, 0, 0, cst)

	unpaid := newPOSOrder(t, pool, "bar-1")
	execSQL(t, pool, `insert into order_items (order_id, kind, ref_id, name, unit_cents)
		values ($1, 'beer', 1, '未付酒', 4500)`, unpaid)

	in := loadPOSInsightsSnapshot(t, pool, day, day.AddDate(0, 0, 1))
	if in.Received != 0 || in.Orders != 0 || in.Payments != 0 {
		t.Fatalf("无收款时应全 0: %+v", in)
	}
	if got := in.AverageText(); got != "—" {
		t.Fatalf("无订单客单价 = %q", got)
	}
	if len(in.Hours) != 24 {
		t.Fatalf("小时分布应固定 24 行，实际 %d", len(in.Hours))
	}
	for i, h := range in.Hours {
		if want := (platform.BusinessDayStartHour + i) % 24; h.Hour != want || h.Received != 0 || h.Payments != 0 {
			t.Fatalf("第 %d 小时应为空且 hour=%d: %+v", i, want, h)
		}
	}
}
