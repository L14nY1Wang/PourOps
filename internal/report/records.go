package report

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
	"pourops/internal/pos"
)

const recordPageSize = 50

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

	PrevURL string
	NextURL string
}

type RecordOrder struct {
	ID         int64
	Spot       string
	OpenedAt   time.Time
	ClosedAt   *time.Time
	Total      int64
	Gross      int64
	Discount   int64
	Paid       int64
	Pending    int64
	MemberName string
	Status     string
}

func (o RecordOrder) Label() string { return pos.SpotLabel(o.Spot) }

// 金额用 int64 版本；MoneyText64 保留原来模板上的「¥」前缀，PriceText64 只有裸数字。
func (o RecordOrder) TotalText() string    { return platform.MoneyText64(o.Total) }
func (o RecordOrder) GrossText() string    { return platform.MoneyText64(o.Gross) }
func (o RecordOrder) DiscountText() string { return platform.MoneyText64(o.Discount) }
func (o RecordOrder) PaidText() string     { return platform.MoneyText64(o.Paid) }
func (o RecordOrder) PendingText() string  { return platform.MoneyText64(o.Pending) }
func (o RecordOrder) HasDiscount() bool    { return o.Discount > 0 }

func (o RecordOrder) OpenedText() string {
	return platform.DateTimeText(&o.OpenedAt)
}

func (o RecordOrder) ClosedText() string {
	if o.ClosedAt == nil {
		return "未结"
	}
	return platform.DateTimeText(o.ClosedAt)
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
	ID              int64
	Kind            string
	Name            string
	UnitCents       int
	Qty             int
	ML              int
	DiscountCents   int
	PaidCents       *int
	PassRedeemedQty int
	PaidAt          *time.Time
	PaymentID       *int64
}

func (i RecordItem) Amount() int { return i.UnitCents * i.Qty }
func (i RecordItem) NetAmount() int {
	if i.PaidCents != nil {
		return *i.PaidCents
	}
	return i.Amount()
}
func (i RecordItem) AmountText() string    { return platform.PriceText(i.Amount()) }
func (i RecordItem) NetAmountText() string { return platform.PriceText(i.NetAmount()) }
func (i RecordItem) QtyText() string       { return "×" + strconv.Itoa(i.Qty) }

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
	ID            int64
	Amount        int
	Gross         int
	Discount      int
	BalancePaid   int
	ExternalPaid  int
	Items         int
	Actor         string
	Method        string
	DiscountLabel string
	DiscountNote  string
	MemberName    string
	PointsSpent   int
	PointsEarned  int
	PassCups      int
	CreatedAt     time.Time
}

func (p RecordPayment) AmountText() string       { return platform.PriceText(p.Amount) }
func (p RecordPayment) GrossText() string        { return platform.PriceText(p.Gross) }
func (p RecordPayment) DiscountText() string     { return platform.PriceText(p.Discount) }
func (p RecordPayment) BalancePaidText() string  { return platform.PriceText(p.BalancePaid) }
func (p RecordPayment) ExternalPaidText() string { return platform.PriceText(p.ExternalPaid) }
func (p RecordPayment) HasDiscount() bool        { return p.Discount > 0 || p.DiscountLabel != "" }
func (p RecordPayment) MethodText() string       { return pos.PayMethodLabel(p.Method) }
func (p RecordPayment) CreatedText() string {
	return platform.DateTimeText(&p.CreatedAt)
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
	return platform.DateTimeText(&e.OccurredAt)
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
	{platform.OpMemberCreate, "新增会员"},
	{platform.OpMemberRecharge, "会员充值"},
	{platform.OpMemberAdjust, "会员调账"},
	{platform.OpPassIssue, "办理月卡"},
	{platform.OpPassRevoke, "作废月卡"},
	{platform.OpDiscount, "人工折扣"},
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

// 单价 × 数量按 bigint 相乘后再聚合，避免 int32 溢出把大额订单算成负数。
const recordItemsAgg = `
	select (sum(coalesce(paid_cents, unit_cents::bigint * qty)))::bigint as total,
	       (sum(unit_cents::bigint * qty))::bigint as gross,
	       (sum(discount_cents))::bigint as discount,
	       (sum(unit_cents::bigint * qty) filter (where paid_at is null))::bigint as pending
	from order_items where order_id = o.id`

const recordPaysAgg = `
	select (sum(amount_cents))::bigint as paid, count(*)::int as pays
	from payments where order_id = o.id`

const recordOrderCols = `
	coalesce(s.total, 0)::bigint as total,
	coalesce(s.gross, 0)::bigint as gross,
	coalesce(s.discount, 0)::bigint as discount,
	coalesce(s.pending, 0)::bigint as pending,
	coalesce(p.paid, 0)::bigint as paid,
	coalesce(m.name, '') as member_name,
	case when o.closed_at is not null then 'settled'
	     when coalesce(p.pays, 0) > 0 then 'partial'
	     else 'unsettled' end as status`

const recordDetailSelect = `
	select o.id, o.spot, o.opened_at, o.closed_at,` + recordOrderCols + `
	from orders o
	left join members m on m.id = o.member_id
	left join lateral (` + recordItemsAgg + `) s on true
	left join lateral (` + recordPaysAgg + `) p on true
	where o.id = $1`

func scanRecordOrder(row pgx.Row) (RecordOrder, error) {
	var o RecordOrder
	err := row.Scan(&o.ID, &o.Spot, &o.OpenedAt, &o.ClosedAt, &o.Total, &o.Gross, &o.Discount, &o.Pending, &o.Paid, &o.MemberName, &o.Status)
	return o, err
}

func parseOrderBeforeCursor(raw string) int64 {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

func parseEventCursor(raw string) (*time.Time, int64) {
	tsStr, idStr, ok := strings.Cut(strings.TrimSpace(raw), "_")
	if !ok {
		return nil, 0
	}
	nanos, err1 := strconv.ParseInt(tsStr, 10, 64)
	id, err2 := strconv.ParseInt(idStr, 10, 64)
	if err1 != nil || err2 != nil || id <= 0 {
		return nil, 0
	}
	t := time.Unix(0, nanos).UTC()
	return &t, id
}

func formatEventCursor(at time.Time, id int64) string {
	return fmt.Sprintf("%d_%d", at.UTC().UnixNano(), id)
}

func beginReadOnlyTimeoutTx(ctx context.Context, pool *pgxpool.Pool) (pgx.Tx, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '5s'`); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

func recordCursorListQuery(status string, beforeID int64) (string, []any) {
	conds := make([]string, 0, 2)
	args := make([]any, 0, 2)
	switch status {
	case "":
	case "settled":
		conds = append(conds, "o.closed_at is not null")
	case "partial":
		conds = append(conds, "o.closed_at is null and exists (select 1 from payments p where p.order_id = o.id)")
	default:
		conds = append(conds, "o.closed_at is null and not exists (select 1 from payments p where p.order_id = o.id)")
	}
	if beforeID > 0 {
		args = append(args, beforeID)
		conds = append(conds, fmt.Sprintf("o.id < $%d", len(args)))
	}
	whereClause := ""
	if len(conds) > 0 {
		whereClause = "where " + strings.Join(conds, "\n\t\t  and ")
	}
	args = append(args, recordPageSize+1)
	limitParam := fmt.Sprintf("$%d", len(args))
	q := `
	with page as (
		select o.id, o.spot, o.opened_at, o.closed_at, o.member_id
		from orders o
		` + whereClause + `
		order by o.id desc
		limit ` + limitParam + `
	)
	select o.id, o.spot, o.opened_at, o.closed_at,` + recordOrderCols + `
	from page o
	left join members m on m.id = o.member_id
	left join lateral (` + recordItemsAgg + `) s on true
	left join lateral (` + recordPaysAgg + `) p on true
	order by o.id desc`
	return q, args
}

func ListRecordOrdersCursor(ctx context.Context, pool *pgxpool.Pool, status string, beforeID int64) ([]RecordOrder, bool, error) {
	tx, err := beginReadOnlyTimeoutTx(ctx, pool)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	query, args := recordCursorListQuery(status, beforeID)
	rows, err := tx.Query(ctx, query, args...)
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
	tx, err := beginReadOnlyTimeoutTx(ctx, pool)
	if err != nil {
		return RecordOrder{}, nil, nil, nil, err
	}
	defer tx.Rollback(ctx)

	o, err := scanRecordOrder(tx.QueryRow(ctx, recordDetailSelect, id))
	if err != nil {
		return o, nil, nil, nil, err
	}
	prows, err := tx.Query(ctx, `select p.id, p.amount_cents, coalesce(p.gross_cents, p.amount_cents), p.discount_cents,
		p.balance_paid_cents, coalesce(p.external_paid_cents, p.amount_cents), p.items_count, p.actor, p.method,
		p.discount_label, p.discount_note, coalesce(m.name, ''), p.points_spent, p.points_earned,
		coalesce((select sum(u.cups)::int from member_pass_usages u where u.payment_id = p.id), 0), p.created_at
		from payments p
		left join members m on m.id = p.member_id
		where p.order_id=$1 order by p.id`, id)
	if err != nil {
		return o, nil, nil, nil, err
	}
	var checkouts []RecordCheckout
	at := make(map[int64]int)
	for prows.Next() {
		var p RecordPayment
		if err := prows.Scan(&p.ID, &p.Amount, &p.Gross, &p.Discount, &p.BalancePaid, &p.ExternalPaid,
			&p.Items, &p.Actor, &p.Method, &p.DiscountLabel, &p.DiscountNote, &p.MemberName,
			&p.PointsSpent, &p.PointsEarned, &p.PassCups, &p.CreatedAt); err != nil {
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

	irows, err := tx.Query(ctx, `select id, kind, name, unit_cents, qty, ml, discount_cents, paid_cents, pass_redeemed_qty, paid_at, payment_id
		from order_items where order_id=$1 order by id`, id)
	if err != nil {
		return o, nil, nil, nil, err
	}
	defer irows.Close()
	var unpaid, unlinked []RecordItem
	for irows.Next() {
		var it RecordItem
		if err := irows.Scan(&it.ID, &it.Kind, &it.Name, &it.UnitCents, &it.Qty, &it.ML,
			&it.DiscountCents, &it.PaidCents, &it.PassRedeemedQty, &it.PaidAt, &it.PaymentID); err != nil {
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

func ListLedgerEventsCursor(ctx context.Context, pool *pgxpool.Pool, kind string, beforeAt *time.Time, beforeID int64) ([]LedgerEvent, bool, error) {
	tx, err := beginReadOnlyTimeoutTx(ctx, pool)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `select id, occurred_at, kind, subject, detail, actor
		from operation_events
		where ($1 = '' or kind = $1)
		  and ($2::timestamptz is null or (occurred_at, id) < ($2, $3))
		order by occurred_at desc, id desc
		limit $4`, kind, beforeAt, beforeID, recordPageSize+1)
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

func recordCursorURL(base string, q url.Values, before string) string {
	cloned := make(url.Values, len(q)+1)
	for k, v := range q {
		cloned[k] = v
	}
	if before != "" {
		cloned.Set("before", before)
	}
	if len(cloned) == 0 {
		return base
	}
	return base + "?" + cloned.Encode()
}
