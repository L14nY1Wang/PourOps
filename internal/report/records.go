package report

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
	"pourops/internal/pos"
)

const (
	recordPageSize = 50
	recordMaxPage  = 1000
)

type RecordOption struct{ Value, Label string }

type RecordsPage struct {
	Mode string // orders | order | operations

	Status   string
	Statuses []RecordOption
	Orders   []RecordOrder

	Order     *RecordOrder
	Checkouts []RecordCheckout
	Unpaid    []RecordItem
	Unlinked  []RecordItem

	Kind   string
	Kinds  []RecordOption
	Events []LedgerEvent

	Page    int
	PrevURL string
	NextURL string
}

type RecordOrder struct {
	ID       int64
	Spot     string
	OpenedAt time.Time
	ClosedAt *time.Time
	Total    int
	Paid     int
	Pending  int
	Status   string
}

func (o RecordOrder) Label() string       { return pos.SpotLabel(o.Spot) }
func (o RecordOrder) TotalText() string   { return platform.PriceText(o.Total) }
func (o RecordOrder) PaidText() string    { return platform.PriceText(o.Paid) }
func (o RecordOrder) PendingText() string { return platform.PriceText(o.Pending) }

func (o RecordOrder) OpenedText() string {
	return o.OpenedAt.Format("2006-01-02 15:04")
}

func (o RecordOrder) ClosedText() string {
	if o.ClosedAt == nil {
		return "未结"
	}
	return o.ClosedAt.Format("2006-01-02 15:04")
}

func (o RecordOrder) StatusText() string {
	switch o.Status {
	case "settled":
		return "已结"
	case "partial":
		return "部分结"
	default:
		return "未结"
	}
}

type RecordItem struct {
	ID        int64
	Kind      string
	Name      string
	UnitCents int
	Qty       int
	ML        int
	PaidAt    *time.Time
	PaymentID *int64
}

func (i RecordItem) Amount() int        { return i.UnitCents * i.Qty }
func (i RecordItem) AmountText() string { return platform.PriceText(i.Amount()) }
func (i RecordItem) QtyText() string    { return "×" + strconv.Itoa(i.Qty) }

func (i RecordItem) KindText() string {
	switch i.Kind {
	case "beer":
		return "桶装"
	case "retail":
		return "零售"
	}
	return i.Kind
}

func (i RecordItem) Detail() string {
	if i.Kind == "beer" && i.ML > 0 {
		return strconv.Itoa(i.ML) + "ml/杯 · " + platform.PriceText(i.UnitCents)
	}
	return platform.PriceText(i.UnitCents)
}

type RecordPayment struct {
	ID        int64
	Amount    int
	Items     int
	Actor     string
	CreatedAt time.Time
}

func (p RecordPayment) AmountText() string { return platform.PriceText(p.Amount) }
func (p RecordPayment) CreatedText() string {
	return p.CreatedAt.Format("2006-01-02 15:04")
}

func (p RecordPayment) ActorText() string {
	if p.Actor == "" {
		return "未记录"
	}
	return p.Actor
}

type RecordCheckout struct {
	Payment RecordPayment
	Items   []RecordItem
}

type LedgerEvent struct {
	ID         int64
	OccurredAt time.Time
	Kind       string
	Subject    string
	Detail     string
	Actor      string
}

func (e LedgerEvent) OccurredText() string {
	return e.OccurredAt.Format("2006-01-02 15:04")
}

func (e LedgerEvent) ActorText() string {
	if e.Actor == "" {
		return "未记录"
	}
	return e.Actor
}

var RecordKinds = []RecordOption{
	{platform.OpPurchase, "进货"},
	{platform.OpTap, "上架"},
	{platform.OpUntap, "下架"},
	{platform.OpManualSale, "手工售出"},
	{platform.OpSaleVoid, "撤销售出"},
	{platform.OpStock, "库存调整"},
	{platform.OpRetailOn, "零售上架"},
	{platform.OpRetailOff, "零售下架"},
	{platform.OpScreenOn, "酒单上屏"},
	{platform.OpScreenOff, "酒单下屏"},
}

var RecordKindOptions = append([]RecordOption{{"", "全部"}}, RecordKinds...)

func RecordKindLabel(v string) string {
	for _, k := range RecordKinds {
		if k.Value == v {
			return k.Label
		}
	}
	return v
}

func (e LedgerEvent) KindText() string { return RecordKindLabel(e.Kind) }

var RecordOrderStatuses = []RecordOption{
	{"", "全部"},
	{"unsettled", "未结"},
	{"partial", "部分结"},
	{"settled", "已结"},
}

func validOrderStatus(v string) bool {
	switch v {
	case "", "unsettled", "partial", "settled":
		return true
	}
	return false
}

func validOperationKind(v string) bool {
	if v == "" {
		return true
	}
	for _, k := range RecordKinds {
		if k.Value == v {
			return true
		}
	}
	return false
}

const recordItemsAgg = `
	select (sum(unit_cents * qty))::int as total,
	       (sum(unit_cents * qty) filter (where paid_at is null))::int as pending
	from order_items where order_id = o.id`

const recordPaysAgg = `
	select (sum(amount_cents))::int as paid, count(*)::int as pays
	from payments where order_id = o.id`

const recordOrderCols = `
	coalesce(s.total, 0)::int as total,
	coalesce(s.pending, 0)::int as pending,
	coalesce(p.paid, 0)::int as paid,
	case when o.closed_at is not null then 'settled'
	     when coalesce(p.pays, 0) > 0 then 'partial'
	     else 'unsettled' end as status`

const recordDetailSelect = `
	select o.id, o.spot, o.opened_at, o.closed_at,` + recordOrderCols + `
	from orders o
	left join lateral (` + recordItemsAgg + `) s on true
	left join lateral (` + recordPaysAgg + `) p on true
	where o.id = $1`

const recordListSelect = `
	with page as (
		select o.id, o.spot, o.opened_at, o.closed_at
		from orders o
		where case $1
			when '' then true
			when 'settled' then o.closed_at is not null
			when 'partial' then o.closed_at is null and exists (select 1 from payments p where p.order_id = o.id)
			else o.closed_at is null and not exists (select 1 from payments p where p.order_id = o.id)
		end
		order by o.id desc
		limit $2 offset $3
	)
	select o.id, o.spot, o.opened_at, o.closed_at,` + recordOrderCols + `
	from page o
	left join lateral (` + recordItemsAgg + `) s on true
	left join lateral (` + recordPaysAgg + `) p on true
	order by o.id desc`

func scanRecordOrder(row pgx.Row) (RecordOrder, error) {
	var o RecordOrder
	err := row.Scan(&o.ID, &o.Spot, &o.OpenedAt, &o.ClosedAt, &o.Total, &o.Pending, &o.Paid, &o.Status)
	return o, err
}

func recordPageParam(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if n < 1 {
		return 1
	}
	if n > recordMaxPage {
		return recordMaxPage
	}
	return n
}

func recordPageOffset(page int) int {
	if page < 1 {
		page = 1
	}
	if page > recordMaxPage {
		page = recordMaxPage
	}
	return (page - 1) * recordPageSize
}

func ListRecordOrders(ctx context.Context, pool *pgxpool.Pool, status string, page int) ([]RecordOrder, bool, error) {
	rows, err := pool.Query(ctx, recordListSelect, status, recordPageSize+1, recordPageOffset(page))
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []RecordOrder
	for rows.Next() {
		o, err := scanRecordOrder(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasNext := len(out) > recordPageSize
	if hasNext {
		out = out[:recordPageSize]
	}
	return out, hasNext, nil
}

func LoadRecordDetail(ctx context.Context, pool *pgxpool.Pool, id int64) (RecordOrder, []RecordCheckout, []RecordItem, []RecordItem, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return RecordOrder{}, nil, nil, nil, err
	}
	defer tx.Rollback(ctx)

	o, err := scanRecordOrder(tx.QueryRow(ctx, recordDetailSelect, id))
	if err != nil {
		return o, nil, nil, nil, err
	}
	prows, err := tx.Query(ctx, `select id, amount_cents, items_count, actor, created_at
		from payments where order_id=$1 order by id`, id)
	if err != nil {
		return o, nil, nil, nil, err
	}
	var checkouts []RecordCheckout
	at := make(map[int64]int)
	for prows.Next() {
		var p RecordPayment
		if err := prows.Scan(&p.ID, &p.Amount, &p.Items, &p.Actor, &p.CreatedAt); err != nil {
			prows.Close()
			return o, nil, nil, nil, err
		}
		at[p.ID] = len(checkouts)
		checkouts = append(checkouts, RecordCheckout{Payment: p})
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return o, nil, nil, nil, err
	}

	irows, err := tx.Query(ctx, `select id, kind, name, unit_cents, qty, ml, paid_at, payment_id
		from order_items where order_id=$1 order by id`, id)
	if err != nil {
		return o, nil, nil, nil, err
	}
	defer irows.Close()
	var unpaid, unlinked []RecordItem
	for irows.Next() {
		var it RecordItem
		if err := irows.Scan(&it.ID, &it.Kind, &it.Name, &it.UnitCents, &it.Qty, &it.ML, &it.PaidAt, &it.PaymentID); err != nil {
			return o, nil, nil, nil, err
		}
		if it.PaidAt == nil {
			unpaid = append(unpaid, it)
			continue
		}
		if it.PaymentID != nil {
			if i, ok := at[*it.PaymentID]; ok {
				checkouts[i].Items = append(checkouts[i].Items, it)
				continue
			}
		}
		unlinked = append(unlinked, it)
	}
	if err := irows.Err(); err != nil {
		return o, nil, nil, nil, err
	}
	return o, checkouts, unpaid, unlinked, nil
}

func ListLedgerEvents(ctx context.Context, pool *pgxpool.Pool, kind string, page int) ([]LedgerEvent, bool, error) {
	rows, err := pool.Query(ctx, `select id, occurred_at, kind, subject, detail, actor
		from operation_events
		where ($1 = '' or kind = $1)
		order by occurred_at desc, id desc
		limit $2 offset $3`, kind, recordPageSize+1, recordPageOffset(page))
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []LedgerEvent
	for rows.Next() {
		var e LedgerEvent
		if err := rows.Scan(&e.ID, &e.OccurredAt, &e.Kind, &e.Subject, &e.Detail, &e.Actor); err != nil {
			return nil, false, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasNext := len(out) > recordPageSize
	if hasNext {
		out = out[:recordPageSize]
	}
	return out, hasNext, nil
}

func recordURL(base string, q url.Values, page int) string {
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	if len(q) == 0 {
		return base
	}
	return base + "?" + q.Encode()
}
