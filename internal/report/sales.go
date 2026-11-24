package report

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"pourops/internal/platform"
)

// 营业额的唯一口径：桶装 keg_pours.amount_cents + 零售 retail_sales.total_cents，
// payments 只是收款方式，不能重复计。start/end 是北京时间营业日（07:00 至次日 07:00）的
// [start, end) 边界；成本按桶成本 × 售出毫升摊销，零售取已售成本，按营业日分品类先舍入到分，
// 保证趋势逐日相加与 KPI 完全对账。

type SalesTotals struct {
	Revenue int64
	Cost    int64
	Qty     int64
}

func (t SalesTotals) Profit() int64       { return t.Revenue - t.Cost }
func (t SalesTotals) RevenueText() string { return platform.PriceText(int(t.Revenue)) }
func (t SalesTotals) CostText() string    { return platform.PriceText(int(t.Cost)) }
func (t SalesTotals) ProfitText() string  { return platform.MoneyText(int(t.Profit())) }

func ratioPercentText(part, total int64) string {
	if total <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", float64(part)*100/float64(total))
}

func (t SalesTotals) MarginText() string {
	return ratioPercentText(t.Profit(), t.Revenue)
}

func ChangeText(now, prev int64) string {
	if prev > 0 {
		pct := math.Round(float64(now-prev)*1000/float64(prev)) / 10
		if pct == 0 {
			return "0%"
		}
		return fmt.Sprintf("%+.1f%%", pct)
	}
	diff := now - prev
	switch {
	case diff > 0:
		return "+" + platform.PriceText(int(diff))
	case diff < 0:
		return platform.MoneyText(int(diff))
	default:
		return "—"
	}
}

type SalesTrendDay struct {
	Date    string
	Revenue int64
	Cost    int64
}

func (d SalesTrendDay) RevenueText() string { return platform.PriceText(int(d.Revenue)) }
func (d SalesTrendDay) CostText() string    { return platform.PriceText(int(d.Cost)) }
func (d SalesTrendDay) ProfitText() string  { return platform.MoneyText(int(d.Revenue - d.Cost)) }
func (d SalesTrendDay) MarginText() string  { return ratioPercentText(d.Revenue-d.Cost, d.Revenue) }

type SalesTopItem struct {
	Name    string
	Unit    string
	Qty     int64
	Revenue int64
	Cost    int64
}

func (i SalesTopItem) RevenueText() string { return platform.PriceText(int(i.Revenue)) }
func (i SalesTopItem) ProfitText() string  { return platform.MoneyText(int(i.Revenue - i.Cost)) }

type SalesInsights struct {
	Current  SalesTotals
	Previous SalesTotals
	Draft    SalesTotals
	Retail   SalesTotals
	Trend    []SalesTrendDay
	Top      []SalesTopItem
}

func (i SalesInsights) RevenueChangeText() string {
	return ChangeText(i.Current.Revenue, i.Previous.Revenue)
}

func (i SalesInsights) ProfitChangeText() string {
	return ChangeText(i.Current.Profit(), i.Previous.Profit())
}

func (i SalesInsights) HasData() bool {
	return len(i.Top) > 0 || i.Current.Revenue > 0 || i.Current.Cost > 0
}

func (i SalesInsights) DaysCount() int { return len(i.Trend) }

func (i SalesInsights) AvgDailyRevenueText() string {
	if len(i.Trend) == 0 {
		return "—"
	}
	return platform.PriceText(int(math.Round(float64(i.Current.Revenue) / float64(len(i.Trend)))))
}

func (i SalesInsights) AvgDailyProfitText() string {
	if len(i.Trend) == 0 {
		return "—"
	}
	return platform.MoneyText(int(math.Round(float64(i.Current.Profit()) / float64(len(i.Trend)))))
}

func (i SalesInsights) DraftShareText() string {
	return ratioPercentText(i.Draft.Revenue, i.Current.Revenue)
}

func (i SalesInsights) RetailShareText() string {
	return ratioPercentText(i.Retail.Revenue, i.Current.Revenue)
}

type SalesTrendPoint struct {
	Date    string  `json:"date"`
	Revenue float64 `json:"revenue"`
	Profit  float64 `json:"profit"`
}

func (i SalesInsights) TrendJSON() template.JS {
	points := make([]SalesTrendPoint, 0, len(i.Trend))
	for _, d := range i.Trend {
		points = append(points, SalesTrendPoint{
			Date:    d.Date,
			Revenue: float64(d.Revenue) / 100,
			Profit:  float64(d.Revenue-d.Cost) / 100,
		})
	}
	b, _ := json.Marshal(points)
	return template.JS(b)
}

// LoadSalesInsights 在只读快照里读本期、等长上期、日趋势和商品前 10。
func LoadSalesInsights(ctx context.Context, tx pgx.Tx, start, end time.Time) (SalesInsights, error) {
	var out SalesInsights
	prevStart := start.Add(-end.Sub(start))

	draftByDay := map[string]SalesTotals{}
	retailByDay := map[string]SalesTotals{}
	rows, err := tx.Query(ctx, `
		select to_char(t.day, 'YYYY-MM-DD'), t.kind, t.revenue, t.cost, t.qty
		from (
			select ((p.poured_at at time zone 'Asia/Shanghai') - interval '7 hours')::date as day,
			       'draft'::text as kind,
			       sum(p.amount_cents)::bigint as revenue,
			       coalesce(round(sum(k.cost_cents::numeric * p.ml / nullif(k.capacity_ml, 0))), 0)::bigint as cost,
			       sum(p.cups)::bigint as qty
			from keg_pours p
			join kegs k on k.id = p.keg_id
			where p.poured_at >= $1 and p.poured_at < $2
			group by 1
			union all
			select d.day,
			       'retail'::text,
			       sum(d.total_cents)::bigint,
			       sum(d.cost_cents)::bigint,
			       sum(d.qty)::bigint
			from (
				select ((s.created_at at time zone 'Asia/Shanghai') - interval '7 hours')::date as day,
				       s.total_cents, s.cost_cents,
				       (select coalesce(sum(i.qty), 0) from retail_sale_items i where i.sale_id = s.id) as qty
				from retail_sales s
				where s.created_at >= $1 and s.created_at < $2
			) d
			group by 1
		) t
		order by t.day, t.kind`, prevStart, end)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var day, kind string
		var t SalesTotals
		if err := rows.Scan(&day, &kind, &t.Revenue, &t.Cost, &t.Qty); err != nil {
			rows.Close()
			return out, err
		}
		if kind == "draft" {
			draftByDay[day] = t
		} else {
			retailByDay[day] = t
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	startDay, endDay := start.In(platform.ReportZone), end.In(platform.ReportZone)
	for d := startDay; d.Before(endDay); d = d.AddDate(0, 0, 1) {
		key := platform.BusinessDayKey(d)
		draft, retail := draftByDay[key], retailByDay[key]
		day := SalesTotals{
			Revenue: draft.Revenue + retail.Revenue,
			Cost:    draft.Cost + retail.Cost,
			Qty:     draft.Qty + retail.Qty,
		}
		out.Trend = append(out.Trend, SalesTrendDay{Date: key, Revenue: day.Revenue, Cost: day.Cost})
		out.Current.Revenue += day.Revenue
		out.Current.Cost += day.Cost
		out.Current.Qty += day.Qty
		out.Draft.Revenue += draft.Revenue
		out.Draft.Cost += draft.Cost
		out.Draft.Qty += draft.Qty
		out.Retail.Revenue += retail.Revenue
		out.Retail.Cost += retail.Cost
		out.Retail.Qty += retail.Qty
	}
	for d := prevStart.In(platform.ReportZone); d.Before(startDay); d = d.AddDate(0, 0, 1) {
		key := platform.BusinessDayKey(d)
		draft, retail := draftByDay[key], retailByDay[key]
		out.Previous.Revenue += draft.Revenue + retail.Revenue
		out.Previous.Cost += draft.Cost + retail.Cost
		out.Previous.Qty += draft.Qty + retail.Qty
	}

	rows, err = tx.Query(ctx, `
		with draft as (
			select 'beer:' || k.beer_id as grp,
			       b.brewery || ' · ' || b.name as name,
			       '杯'::text as unit,
			       sum(p.cups)::bigint as qty,
			       sum(p.amount_cents)::bigint as revenue,
			       coalesce(round(sum(k.cost_cents::numeric * p.ml / nullif(k.capacity_ml, 0))), 0)::bigint as cost
			from keg_pours p
			join kegs k on k.id = p.keg_id
			join beers b on b.id = k.beer_id
			where p.poured_at >= $1 and p.poured_at < $2
			group by k.beer_id, b.brewery, b.name
		), retail as (
			select x.grp,
			       (array_agg(x.name order by x.created_at desc, x.sale_id desc, x.line_id desc))[1] as name,
			       '件'::text as unit,
			       sum(x.qty)::bigint as qty,
			       sum(x.price_cents::bigint * x.qty)::bigint as revenue,
			       sum(x.cost_cents::bigint * x.qty)::bigint as cost
			from (
				select case when i.item_id is null then 'name:' || i.name else 'item:' || i.item_id::text end as grp,
				       i.name, s.created_at, s.id as sale_id, i.id as line_id,
				       i.qty, i.price_cents, i.cost_cents
				from retail_sale_items i
				join retail_sales s on s.id = i.sale_id
				where s.created_at >= $1 and s.created_at < $2
			) x
			group by x.grp
		)
		select name, unit, qty, revenue, cost
		from (select * from draft union all select * from retail) t
		order by revenue desc, qty desc, name, grp
		limit 10`, start, end)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var it SalesTopItem
		if err := rows.Scan(&it.Name, &it.Unit, &it.Qty, &it.Revenue, &it.Cost); err != nil {
			return out, err
		}
		out.Top = append(out.Top, it)
	}
	return out, rows.Err()
}
