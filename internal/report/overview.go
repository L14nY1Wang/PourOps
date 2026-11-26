package report

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/inventory"
	"pourops/internal/platform"
)

// Overview 是经营概览的当前库存快照，不受销售日期筛选影响。
type Overview struct {
	ActiveKegs, RetailCount, LowCount, OutCount, Untapped, WarnKegCount int
	StockValue                                                          int
	LowStock                                                            []inventory.RetailItem
	WarnKegs                                                            []inventory.Keg
}

func (o Overview) StockText() string { return platform.PriceText(o.StockValue) }

// LoadOverview 单独开一个只读快照读概览；需要和销售/POS 统计同源时用 LoadOverviewTx。
func LoadOverview(ctx context.Context, pool *pgxpool.Pool) (Overview, error) {
	tx, err := beginReadOnlyTimeoutTx(ctx, pool)
	if err != nil {
		return Overview{}, err
	}
	defer tx.Rollback(ctx)
	o, err := LoadOverviewTx(ctx, tx)
	if err != nil {
		return o, err
	}
	return o, tx.Commit(ctx)
}

// LoadOverviewTx 在调用方事务里读概览快照；手动停用的有库存商品与软删除商品不再挤占库存告警，库存归零自动下架的售罄品仍计入售罄告警。
func LoadOverviewTx(ctx context.Context, tx pgx.Tx) (Overview, error) {
	var o Overview
	err := tx.QueryRow(ctx, `
		select (select count(*) from kegs where tapped_at is not null and finished_at is null),
		       (select count(*) from retail_items where active and archived_at is null),
		       (select count(*) from retail_items where active and archived_at is null and stock between 1 and $1),
		       (select count(*) from retail_items where archived_at is null and stock <= 0),
		       (select count(*) from kegs where tapped_at is null and finished_at is null),
		       (select coalesce(sum(stock::bigint * cost_cents),0) from retail_items where active and archived_at is null)
		         + (select coalesce(sum(cost_cents),0) from kegs where tapped_at is null and finished_at is null)`,
		inventory.LowStockThreshold,
	).Scan(&o.ActiveKegs, &o.RetailCount, &o.LowCount, &o.OutCount, &o.Untapped, &o.StockValue)
	if err != nil {
		return o, err
	}
	rows, err := tx.Query(ctx, `select id, name, category, spec, price_cents, cost_cents, stock, active
		from retail_items where archived_at is null and ((active and stock between 1 and $1) or stock <= 0)
		order by stock, name limit 8`, inventory.LowStockThreshold)
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
	if err := rows.Err(); err != nil {
		return o, err
	}

	activeKegs, err := inventory.ListKegs(ctx, tx, true)
	if err != nil {
		return o, err
	}
	for _, k := range activeKegs {
		if k.IsSoldWarning() {
			o.WarnKegs = append(o.WarnKegs, k)
		}
	}
	slices.SortFunc(o.WarnKegs, func(a, b inventory.Keg) int {
		if d := b.SoldPct() - a.SoldPct(); d != 0 {
			return d
		}
		if d := a.RemainingML() - b.RemainingML(); d != 0 {
			return d
		}
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		default:
			return 0
		}
	})
	o.WarnKegCount = len(o.WarnKegs)
	return o, nil
}

// ReportDates 按酒吧营业日（北京时间 07:00 至次日 07:00）计算统计起止边界 [start, end)。
func ReportDates(now time.Time, period, from, to string) (string, time.Time, time.Time, string) {
	today := platform.BusinessDayStart(now)
	end := today.AddDate(0, 0, 1)
	if from != "" || to != "" || period == "custom" {
		startDay, startErr := time.ParseInLocation(time.DateOnly, from, platform.ReportZone)
		lastDay, endErr := time.ParseInLocation(time.DateOnly, to, platform.ReportZone)
		if startErr != nil || endErr != nil || startDay.Year() < 2000 || lastDay.Year() < 2000 || lastDay.Before(startDay) || !lastDay.Before(startDay.AddDate(0, 0, 366)) {
			return "custom", time.Time{}, time.Time{}, "请选择有效的起止日期，区间最多 366 天。"
		}
		start := platform.BusinessDayStartOfDate(startDay)
		last := platform.BusinessDayStartOfDate(lastDay)
		return "custom", start, last.AddDate(0, 0, 1), ""
	}
	switch period {
	case "today":
		return period, today, end, ""
	case "30d":
		return period, today.AddDate(0, 0, -29), end, ""
	case "month":
		monthStart := time.Date(today.Year(), today.Month(), 1, platform.BusinessDayStartHour, 0, 0, 0, platform.ReportZone)
		return period, monthStart, end, ""
	case "", "7d":
		return "7d", today.AddDate(0, 0, -6), end, ""
	default:
		return "", time.Time{}, time.Time{}, "请选择有效的统计周期。"
	}
}
