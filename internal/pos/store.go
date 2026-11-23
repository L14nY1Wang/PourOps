package pos

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/catalog"
	"pourops/internal/inventory"
	"pourops/internal/platform"
)

// ---------- 台位 ----------

type Spot struct {
	Key     string // bar-1 / table-3
	Kind    string // bar | table
	Label   string // 吧台 1 / 小桌 3
	OrderID int64
	Items   int
	Total   int
}

func (s Spot) Busy() bool        { return s.Items > 0 }
func (s Spot) TotalText() string { return platform.PriceText(s.Total) }

func SpotLabel(key string) string {
	kind, num, ok := strings.Cut(key, "-")
	if !ok {
		return key
	}
	switch kind {
	case "bar":
		return "吧台 " + num
	case "table":
		return "小桌 " + num
	}
	return key
}

func settingInt(ctx context.Context, pool *pgxpool.Pool, key string, def int) (int, error) {
	var v string
	err := pool.QueryRow(ctx, `select value from settings where key=$1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return def, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, nil
	}
	return n, nil
}

// AddSpot 添加吧台或小桌位：只改计数，不预建账单。
func AddSpot(ctx context.Context, pool *pgxpool.Pool, key string) error {
	_, err := pool.Exec(ctx, `insert into settings (key, value) values ($1, '1')
		on conflict (key) do update set value = (coalesce(settings.value, '0')::int + 1)::text`, key)
	return err
}

// ListSpots 把吧台、小桌和各自未结账单凑成一张台位表。
func ListSpots(ctx context.Context, pool *pgxpool.Pool) ([]Spot, error) {
	bar, err := settingInt(ctx, pool, "bar_count", 3)
	if err != nil {
		return nil, err
	}
	table, err := settingInt(ctx, pool, "table_count", 4)
	if err != nil {
		return nil, err
	}
	out := make([]Spot, 0, bar+table)
	for i := 1; i <= bar; i++ {
		out = append(out, Spot{Key: fmt.Sprintf("bar-%d", i), Kind: "bar", Label: SpotLabel(fmt.Sprintf("bar-%d", i))})
	}
	for i := 1; i <= table; i++ {
		out = append(out, Spot{Key: fmt.Sprintf("table-%d", i), Kind: "table", Label: SpotLabel(fmt.Sprintf("table-%d", i))})
	}

	rows, err := pool.Query(ctx, `
		select o.spot, o.id,
		       count(i.id) filter (where i.paid_at is null),
		       coalesce(sum(i.unit_cents * i.qty) filter (where i.paid_at is null), 0)
		from orders o left join order_items i on i.order_id = o.id
		where o.closed_at is null
		group by o.spot, o.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	open := map[string]Spot{}
	for rows.Next() {
		var sp Spot
		if err := rows.Scan(&sp.Key, &sp.OrderID, &sp.Items, &sp.Total); err != nil {
			return nil, err
		}
		open[sp.Key] = sp
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, sp := range out {
		if o, ok := open[sp.Key]; ok {
			out[i].OrderID, out[i].Items, out[i].Total = o.OrderID, o.Items, o.Total
		}
	}
	return out, nil
}

// EnsureOrder 给台位开一张未结账单；已经在用就返回那张。
func EnsureOrder(ctx context.Context, pool *pgxpool.Pool, key string) (int64, error) {
	kind, num, ok := strings.Cut(key, "-")
	n, err := strconv.Atoi(num)
	if !ok || err != nil || n < 1 || (kind != "bar" && kind != "table") {
		return 0, platform.UserErr("没有这个台位")
	}
	def := 4
	if kind == "bar" {
		def = 3
	}
	limit, err := settingInt(ctx, pool, kind+"_count", def)
	if err != nil {
		return 0, err
	}
	if n > limit {
		return 0, platform.UserErr("没有这个台位")
	}
	if key != kind+"-"+strconv.Itoa(n) {
		return 0, platform.UserErr("没有这个台位")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var id int64
	err = tx.QueryRow(ctx, `select id from orders where spot=$1 and closed_at is null`, key).Scan(&id)
	if err == nil {
		return id, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	err = tx.QueryRow(ctx, `insert into orders (spot) values ($1)
		on conflict (spot) where closed_at is null do nothing returning id`, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// 并发下别人刚建好，读回来
		err = tx.QueryRow(ctx, `select id from orders where spot=$1 and closed_at is null`, key).Scan(&id)
	}
	if err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// ---------- 账单 ----------

type OrderItem struct {
	ID        int64
	Kind      string // beer | retail
	Name      string
	UnitCents int
	Qty       int
	ML        int
}

func (i OrderItem) Amount() int        { return i.UnitCents * i.Qty }
func (i OrderItem) AmountText() string { return platform.PriceText(i.Amount()) }
func (i OrderItem) QtyText() string    { return fmt.Sprintf("×%d", i.Qty) }
func (i OrderItem) Detail() string {
	if i.Kind == "beer" && i.ML > 0 {
		return fmt.Sprintf("%dml/杯 · %s", i.ML, platform.PriceText(i.UnitCents))
	}
	return platform.PriceText(i.UnitCents)
}

type OrderView struct {
	ID     int64
	Spot   string
	Label  string
	Closed bool
	Unpaid []OrderItem
	Paid   []OrderItem
	Total  int
}

// ItemGroup 把同一款酒的不同杯型并在一条账单里显示，钱各自按杯型算。
type ItemGroup struct {
	Name  string
	Lines []OrderItem
}

func (g ItemGroup) Total() int {
	sum := 0
	for _, l := range g.Lines {
		sum += l.Amount()
	}
	return sum
}

func (g ItemGroup) TotalText() string { return platform.PriceText(g.Total()) }

// UnpaidGroups 未结行按酒名合并（同款酒的 150ml 和 500ml 归到一条）。
func (o OrderView) UnpaidGroups() []ItemGroup {
	groups := make([]ItemGroup, 0, len(o.Unpaid))
	at := make(map[string]int, len(o.Unpaid))
	for _, it := range o.Unpaid {
		if i, ok := at[it.Name]; ok {
			groups[i].Lines = append(groups[i].Lines, it)
			continue
		}
		at[it.Name] = len(groups)
		groups = append(groups, ItemGroup{Name: it.Name, Lines: []OrderItem{it}})
	}
	return groups
}

func (o OrderView) TotalText() string { return platform.PriceText(o.Total) }
func (o OrderView) Empty() bool       { return len(o.Unpaid) == 0 && len(o.Paid) == 0 }

func LoadOrder(ctx context.Context, pool *pgxpool.Pool, id int64) (OrderView, error) {
	var o OrderView
	err := pool.QueryRow(ctx, `select id, spot, closed_at is not null from orders where id=$1`, id).
		Scan(&o.ID, &o.Spot, &o.Closed)
	o.Label = SpotLabel(o.Spot)
	if err != nil {
		return o, err
	}
	rows, err := pool.Query(ctx, `select id, kind, name, unit_cents, qty, ml, paid_at is null
		from order_items where order_id=$1 order by id`, id)
	if err != nil {
		return o, err
	}
	defer rows.Close()
	for rows.Next() {
		var it OrderItem
		var unpaid bool
		if err := rows.Scan(&it.ID, &it.Kind, &it.Name, &it.UnitCents, &it.Qty, &it.ML, &unpaid); err != nil {
			return o, err
		}
		if unpaid {
			o.Unpaid = append(o.Unpaid, it)
			o.Total += it.Amount()
		} else {
			o.Paid = append(o.Paid, it)
		}
	}
	return o, rows.Err()
}

// lockOpenOrder 锁住未结账单：加购、改量、结账都排这同一把行锁，返回台位 key。
func lockOpenOrder(ctx context.Context, tx pgx.Tx, orderID int64) (string, error) {
	var spotKey string
	err := tx.QueryRow(ctx, `select spot from orders where id=$1 and closed_at is null for update`, orderID).Scan(&spotKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", platform.UserErr("这张单已经结了")
	}
	return spotKey, err
}

// AddOrderItem 点一下加一件；同样的未结商品合并数量。
func AddOrderItem(ctx context.Context, pool *pgxpool.Pool, orderID int64, kind string, refID int64, name string, price, ml int) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := lockOpenOrder(ctx, tx, orderID); err != nil {
		return err
	}
	ct, err := tx.Exec(ctx, `update order_items set qty = qty + 1
		where order_id=$1 and paid_at is null and kind=$2 and ref_id=$3 and unit_cents=$4 and ml=$5`,
		orderID, kind, refID, price, ml)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		if _, err := tx.Exec(ctx, `insert into order_items (order_id, kind, ref_id, name, unit_cents, ml)
			values ($1, $2, $3, $4, $5, $6)`, orderID, kind, refID, name, price, ml); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func LessOrderItem(ctx context.Context, pool *pgxpool.Pool, orderID, itemID int64) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := lockOpenOrder(ctx, tx, orderID); err != nil {
		return err
	}
	ct, err := tx.Exec(ctx, `update order_items set qty = qty - 1
		where id=$1 and order_id=$2 and paid_at is null and qty > 1`, itemID, orderID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		if _, err := tx.Exec(ctx, `delete from order_items
			where id=$1 and order_id=$2 and paid_at is null`, itemID, orderID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func MoreOrderItem(ctx context.Context, pool *pgxpool.Pool, orderID, itemID int64) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := lockOpenOrder(ctx, tx, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update order_items set qty = qty + 1
		where id=$1 and order_id=$2 and paid_at is null`, itemID, orderID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------- 结账 ----------

type Payment struct {
	ID      int64
	OrderID int64
	Label   string
	Amount  int
	Items   int
}

func (p Payment) AmountText() string { return platform.PriceText(p.Amount) }

// PayResult 是结账事务提交后的完整摘要，供小票打印扩展与领域事件广播使用。
type PayResult struct {
	PaymentID   int64
	OrderID     int64
	Spot        string
	SpotLabel   string
	AmountCents int
	ItemsCount  int
	Closed      bool
	Lines       []ReceiptLine
	LowStock    []inventory.LowStockItem
}

func PayItems(ctx context.Context, pool *pgxpool.Pool, orderID int64, ids []int64) (int64, error) {
	res, err := PayItemsWithGateway(ctx, pool, ManualGateway{}, orderID, ids)
	return res.PaymentID, err
}

// PayItemsWithGateway 把勾选的商品结掉：锁住账单，售出、网关确认、支付、标记已结都在同一事务里。
// 只处理属于这张单的未结行；中途任何一步（包含 gateway.Settle）失败，整笔回滚。
func PayItemsWithGateway(ctx context.Context, pool *pgxpool.Pool, gw PaymentGateway, orderID int64, ids []int64) (PayResult, error) {
	if len(ids) == 0 {
		return PayResult{}, platform.UserErr("先勾选要结账的商品")
	}
	if gw == nil {
		gw = ManualGateway{}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return PayResult{}, err
	}
	defer tx.Rollback(ctx)

	spotKey, err := lockOpenOrder(ctx, tx, orderID)
	if err != nil {
		return PayResult{}, err
	}

	rows, err := tx.Query(ctx, `select i.id, i.kind, i.ref_id, i.name, i.unit_cents, i.qty, i.ml
		from order_items i
		where i.order_id=$1 and i.paid_at is null and i.id = any($2::bigint[])
		order by i.id
		for update`, orderID, ids)
	if err != nil {
		return PayResult{}, err
	}
	type line struct {
		id, refID     int64
		kind, name    string
		unit, qty, ml int
	}
	var lines []line
	for rows.Next() {
		var l line
		if err := rows.Scan(&l.id, &l.kind, &l.refID, &l.name, &l.unit, &l.qty, &l.ml); err != nil {
			rows.Close()
			return PayResult{}, err
		}
		lines = append(lines, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PayResult{}, err
	}
	if len(lines) == 0 {
		return PayResult{}, platform.UserErr("这些商品已经结过账了")
	}

	total, count := 0, 0
	lineIDs := make([]int64, 0, len(lines))
	receiptLines := make([]ReceiptLine, 0, len(lines))
	var retail []inventory.SaleLine
	for _, l := range lines {
		switch l.kind {
		case "beer":
			if err := inventory.AddPourTx(ctx, tx, l.refID, l.qty, l.ml*l.qty, l.unit*l.qty); err != nil {
				return PayResult{}, err
			}
		case "retail":
			retail = append(retail, inventory.SaleLine{
				Item: inventory.RetailItem{ID: l.refID, Name: l.name, PriceCents: l.unit},
				Qty:  l.qty,
			})
		default:
			return PayResult{}, platform.UserErr("账单里有不认识的商品")
		}
		lineIDs = append(lineIDs, l.id)
		receiptLines = append(receiptLines, ReceiptLine{
			Name:      l.name,
			Kind:      l.kind,
			ML:        l.ml,
			Qty:       l.qty,
			UnitCents: l.unit,
		})
		total += l.unit * l.qty
		count += l.qty
	}
	var lowStock []inventory.LowStockItem
	if len(retail) > 0 {
		_, lowStock, err = inventory.SellRetailTx(ctx, tx, retail)
		if err != nil {
			return PayResult{}, err
		}
	}

	actor := platform.OperationActor(ctx)
	spotLbl := SpotLabel(spotKey)
	if _, err := gw.Settle(ctx, PaymentRequest{
		OrderID:     orderID,
		SpotLabel:   spotLbl,
		AmountCents: total,
		ItemsCount:  count,
		Actor:       actor,
	}); err != nil {
		return PayResult{}, err
	}

	var payID int64
	if err := tx.QueryRow(ctx, `insert into payments (order_id, amount_cents, items_count, actor)
		values ($1, $2, $3, $4) returning id`, orderID, total, count, actor).Scan(&payID); err != nil {
		return PayResult{}, err
	}
	if _, err := tx.Exec(ctx, `update order_items set paid_at = now(), payment_id = $2
		where order_id = $3 and paid_at is null and id = any($1::bigint[])`, lineIDs, payID, orderID); err != nil {
		return PayResult{}, err
	}
	ct, err := tx.Exec(ctx, `update orders set closed_at = now()
		where id=$1 and not exists (select 1 from order_items where order_id=$1 and paid_at is null)`, orderID)
	if err != nil {
		return PayResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PayResult{}, err
	}
	return PayResult{
		PaymentID:   payID,
		OrderID:     orderID,
		Spot:        spotKey,
		SpotLabel:   spotLbl,
		AmountCents: total,
		ItemsCount:  count,
		Closed:      ct.RowsAffected() > 0,
		Lines:       receiptLines,
		LowStock:    lowStock,
	}, nil
}

func LoadPayment(ctx context.Context, pool *pgxpool.Pool, id int64) (Payment, error) {
	var p Payment
	var spotKey string
	err := pool.QueryRow(ctx, `select p.id, p.order_id, p.amount_cents, p.items_count, o.spot
		from payments p join orders o on o.id = p.order_id where p.id=$1`, id).
		Scan(&p.ID, &p.OrderID, &p.Amount, &p.Items, &spotKey)
	p.Label = SpotLabel(spotKey)
	return p, err
}

// ---------- 菜单 ----------

type PosBeer struct {
	KegID    int64 // 0 表示这桶酒不在售，位置留着但不可点
	No       string
	Brewery  string
	Name     string
	Style    string
	Price150 int
	Price330 int
	Price500 int
}

func (p PosBeer) Sold() bool    { return p.KegID > 0 }
func (p PosBeer) Label() string { return p.Brewery + " · " + p.Name }
func (p PosBeer) Cups() []catalog.Cup {
	return catalog.Beer{Price150: p.Price150, Price330: p.Price330, Price500: p.Price500}.Cups()
}

func ListPosBeers(ctx context.Context, pool *pgxpool.Pool) ([]PosBeer, error) {
	rows, err := pool.Query(ctx, `
		select coalesce(k.id, 0), b.no, b.brewery, b.name, b.style,
			coalesce(b.price_150, 0), coalesce(b.price_330, 0), coalesce(b.price_500, 0)
		from beers b
		left join kegs k on k.beer_id = b.id
			and b.on_screen
			and k.tapped_at is not null and k.finished_at is null
		where b.no <> ''
		order by b.no, b.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PosBeer
	for rows.Next() {
		var p PosBeer
		if err := rows.Scan(&p.KegID, &p.No, &p.Brewery, &p.Name, &p.Style,
			&p.Price150, &p.Price330, &p.Price500); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListPosItems 只列能卖的：上架且有库存。
func ListPosItems(ctx context.Context, pool *pgxpool.Pool) ([]inventory.RetailItem, error) {
	rows, err := pool.Query(ctx, `select id, name, category, spec, price_cents, cost_cents, stock, active
		from retail_items where active and stock > 0 order by name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []inventory.RetailItem
	for rows.Next() {
		var it inventory.RetailItem
		if err := rows.Scan(&it.ID, &it.Name, &it.Category, &it.Spec, &it.PriceCents, &it.CostCents, &it.Stock, &it.Active); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
