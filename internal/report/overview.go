package report

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/inventory"
	"pourops/internal/platform"
)

// Overview 是经营概览的当前库存快照，不受销售日期筛选影响。
type Overview struct {
	ActiveKegs, RetailCount, LowCount, OutCount, Untapped int
	StockValue                                            int
	LowStock                                              []inventory.RetailItem
}

func (o Overview) StockText() string { return platform.PriceText(o.StockValue) }

func LoadOverview(ctx context.Context, pool *pgxpool.Pool) (Overview, error) {
	var o Overview
	err := pool.QueryRow(ctx, `
		select (select count(*) from kegs where tapped_at is not null and finished_at is null),
		       (select count(*) from retail_items where active),
		       (select count(*) from retail_items where stock between 1 and $1),
		       (select count(*) from retail_items where stock <= 0),
		       (select count(*) from kegs where tapped_at is null and finished_at is null),
		       (select coalesce(sum(stock::bigint * cost_cents),0) from retail_items)
		         + (select coalesce(sum(cost_cents),0) from kegs where tapped_at is null and finished_at is null)`,
		inventory.LowStockThreshold,
	).Scan(&o.ActiveKegs, &o.RetailCount, &o.LowCount, &o.OutCount, &o.Untapped, &o.StockValue)
	if err != nil {
		return o, err
	}
	rows, err := pool.Query(ctx, `select id, name, category, spec, price_cents, cost_cents, stock, active
		from retail_items where stock <= $1 order by stock, name limit 8`, inventory.LowStockThreshold)
	if err != nil {
		return o, err
	}
	defer rows.Close()
	for rows.Next() {
		var it inventory.RetailItem
		if err := rows.Scan(&it.ID, &it.Name, &it.Category, &it.Spec, &it.PriceCents, &it.CostCents, &it.Stock, &it.Active); err != nil {
			return o, err
		}
		o.LowStock = append(o.LowStock, it)
	}
	return o, rows.Err()
}

func ReportDates(now time.Time, period, from, to string) (string, time.Time, time.Time, string) {
	local := now.In(platform.ReportZone)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, platform.ReportZone)
	end := today.AddDate(0, 0, 1)
	if from != "" || to != "" || period == "custom" {
		start, startErr := time.ParseInLocation(time.DateOnly, from, platform.ReportZone)
		last, endErr := time.ParseInLocation(time.DateOnly, to, platform.ReportZone)
		if startErr != nil || endErr != nil || start.Year() < 1 || last.Year() < 1 || last.Before(start) || !last.Before(start.AddDate(0, 0, 366)) {
			return "custom", time.Time{}, time.Time{}, "请选择有效的起止日期，区间最多 366 天。"
		}
		return "custom", start, last.AddDate(0, 0, 1), ""
	}
	switch period {
	case "today":
		return period, today, end, ""
	case "30d":
		return period, today.AddDate(0, 0, -29), end, ""
	case "month":
		return period, today.AddDate(0, 0, 1-today.Day()), end, ""
	case "", "7d":
		return "7d", today.AddDate(0, 0, -6), end, ""
	default:
		return "", time.Time{}, time.Time{}, "请选择有效的统计周期。"
	}
}
