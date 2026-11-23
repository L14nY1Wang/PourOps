package report

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"time"

	"github.com/jackc/pgx/v5"

	"pourops/internal/platform"
)

type POSInsights struct {
	Received int64 // 实收，分
	Orders   int64 // 有收款的订单数
	Payments int64 // 收款笔数
	Hours    []POSHour
}

type POSHour struct {
	Hour     int // 0-23，北京时间
	Received int64
	Payments int64
}

func (i POSInsights) ReceivedText() string { return platform.PriceText(int(i.Received)) }

func (i POSInsights) AverageText() string {
	if i.Orders == 0 {
		return "—"
	}
	return platform.PriceText(int(i.Received / i.Orders))
}

func (h POSHour) ReceivedText() string { return platform.PriceText(int(h.Received)) }
func (h POSHour) Label() string        { return fmt.Sprintf("%02d:00", h.Hour) }

// LoadPOSInsights 在只读快照里读汇总和 24 小时分布。
func LoadPOSInsights(ctx context.Context, tx pgx.Tx, start, end time.Time) (POSInsights, error) {
	var out POSInsights
	err := tx.QueryRow(ctx, `
		select coalesce(sum(amount_cents), 0)::bigint,
		       count(distinct order_id)::bigint,
		       count(*)::bigint
		from payments
		where created_at >= $1 and created_at < $2`, start, end).
		Scan(&out.Received, &out.Orders, &out.Payments)
	if err != nil {
		return out, err
	}

	rows, err := tx.Query(ctx, `
		select extract(hour from created_at at time zone 'Asia/Shanghai')::int,
		       sum(amount_cents)::bigint,
		       count(*)::bigint
		from payments
		where created_at >= $1 and created_at < $2
		group by 1`, start, end)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	out.Hours = make([]POSHour, 24)
	for i := range out.Hours {
		out.Hours[i].Hour = i
	}
	for rows.Next() {
		var h POSHour
		if err := rows.Scan(&h.Hour, &h.Received, &h.Payments); err != nil {
			return out, err
		}
		out.Hours[h.Hour].Received = h.Received
		out.Hours[h.Hour].Payments = h.Payments
	}
	return out, rows.Err()
}

type posHourPoint struct {
	Hour     string  `json:"hour"`
	Received float64 `json:"received"` // 元
	Payments int64   `json:"payments"`
}

func (i POSInsights) HoursJSON() template.JS {
	points := make([]posHourPoint, 0, len(i.Hours))
	for _, h := range i.Hours {
		points = append(points, posHourPoint{
			Hour:     h.Label(),
			Received: float64(h.Received) / 100,
			Payments: h.Payments,
		})
	}
	b, _ := json.Marshal(points)
	return template.JS(b)
}
