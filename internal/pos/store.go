package pos

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/catalog"
	"pourops/internal/inventory"
	"pourops/internal/platform"
)

// ---------- 台位 ----------

type Spot struct {
	Key     string // bar-1 / table-3 / temp-1
	Kind    string // bar | table | temp
	Label   string // 吧台 1 / 小桌 3 / 临时 1
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
	case "temp":
		return "临时 " + num
	}
	return key
}

const (
	defaultBarSpots   = 3
	defaultTableSpots = 4
	defaultTempSpots  = 0
	maxSpotsPerKind   = 50
	maxOrderLineQty   = 999
)

func settingInt(ctx context.Context, pool *pgxpool.Pool, key string, def int) (int, error) {
	var v string
	err := pool.QueryRow(ctx, `select value from store_settings where key=$1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return def, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def, nil
	}
	return max(0, min(n, maxSpotsPerKind)), nil
}

// AddTempSpot 添加一个临时台位：只改计数，最多 50 个，不预建账单。
func AddTempSpot(ctx context.Context, pool *pgxpool.Pool) error {
	return mutateTempCount(ctx, pool, +1)
}

// RemoveTempSpot 减少最后一个临时台位：允许减到 0；最后一个台位有消费记录或付款时阻止删除，空挂单自动清理。
func RemoveTempSpot(ctx context.Context, pool *pgxpool.Pool) error {
	return mutateTempCount(ctx, pool, -1)
}

func mutateTempCount(ctx context.Context, pool *pgxpool.Pool, delta int) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `insert into store_settings (key, value, updated_at)
		values ('temp_count', '0', now()) on conflict (key) do nothing`); err != nil {
		return err
	}
	cur := defaultTempSpots
	var v string
	err = tx.QueryRow(ctx, `select value from store_settings where key='temp_count' for update`).Scan(&v)
	if err != nil {
		return err
	}
	if n, convErr := strconv.Atoi(strings.TrimSpace(v)); convErr == nil {
		cur = max(0, min(n, maxSpotsPerKind))
	}
	if delta > 0 {
		if cur >= maxSpotsPerKind {
			return platform.UserErr(fmt.Sprintf("临时台位数量已达上限（最多 %d 个）", maxSpotsPerKind))
		}
	} else {
		if cur <= 0 {
			return platform.UserErr("当前没有可减少的临时台位")
		}
		lastSpot := fmt.Sprintf("temp-%d", cur)
		var openOrderID int64
		err = tx.QueryRow(ctx, `select id from orders where spot=$1 and closed_at is null for update`, lastSpot).Scan(&openOrderID)
		if err == nil {
			var busy bool
			if err := tx.QueryRow(ctx, `select exists(select 1 from order_items where order_id=$1)
				or exists(select 1 from payments where order_id=$1)`, openOrderID).Scan(&busy); err != nil {
				return err
			}
			if busy {
				return platform.UserErr(fmt.Sprintf("%s 还有未结账单，先结清再删除台位", SpotLabel(lastSpot)))
			}
			if _, err := tx.Exec(ctx, `delete from orders where id=$1`, openOrderID); err != nil {
				return err
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `insert into store_settings (key, value, updated_at) values ('temp_count', $1, now())
		on conflict (key) do update set value = $1, updated_at = now()`, strconv.Itoa(cur+delta)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListSpots 把固定吧台、固定小桌、临时台位和各自未结账单凑成一张台位表。
func ListSpots(ctx context.Context, pool *pgxpool.Pool) ([]Spot, error) {
	bar, err := settingInt(ctx, pool, "bar_count", defaultBarSpots)
	if err != nil {
		return nil, err
	}
	table, err := settingInt(ctx, pool, "table_count", defaultTableSpots)
	if err != nil {
		return nil, err
	}
	temp, err := settingInt(ctx, pool, "temp_count", defaultTempSpots)
	if err != nil {
		return nil, err
	}
	out := make([]Spot, 0, bar+table+temp)
	for i := 1; i <= bar; i++ {
		key := fmt.Sprintf("bar-%d", i)
		out = append(out, Spot{Key: key, Kind: "bar", Label: SpotLabel(key)})
	}
	for i := 1; i <= table; i++ {
		key := fmt.Sprintf("table-%d", i)
		out = append(out, Spot{Key: key, Kind: "table", Label: SpotLabel(key)})
	}
	for i := 1; i <= temp; i++ {
		key := fmt.Sprintf("temp-%d", i)
		out = append(out, Spot{Key: key, Kind: "temp", Label: SpotLabel(key)})
	}

	rows, err := pool.Query(ctx, `
		select o.spot, o.id,
		       coalesce(sum(i.qty) filter (where i.paid_at is null), 0)::int,
		       coalesce(sum(i.unit_cents::bigint * i.qty::bigint) filter (where i.paid_at is null), 0)::bigint
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
		var total64 int64
		if err := rows.Scan(&sp.Key, &sp.OrderID, &sp.Items, &total64); err != nil {
			return nil, err
		}
		sp.Total = int(total64)
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
	if !ok || err != nil || n < 1 || key != kind+"-"+strconv.Itoa(n) {
		return 0, platform.UserErr("没有这个台位")
	}
	var def int
	switch kind {
	case "bar":
		def = defaultBarSpots
	case "table":
		def = defaultTableSpots
	case "temp":
		def = defaultTempSpots
	default:
		return 0, platform.UserErr("没有这个台位")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	limit := def
	var rawLimit string
	err = tx.QueryRow(ctx, `select value from store_settings where key=$1 for share`, kind+"_count").Scan(&rawLimit)
	if err == nil {
		if v, convErr := strconv.Atoi(strings.TrimSpace(rawLimit)); convErr == nil && v >= 0 {
			limit = min(v, maxSpotsPerKind)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if n > limit {
		return 0, platform.UserErr("没有这个台位")
	}

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

func upsertOrderItemTx(ctx context.Context, tx pgx.Tx, orderID int64, kind string, refID int64, name string, price, ml int) error {
	var existingQty int
	err := tx.QueryRow(ctx, `select qty from order_items
		where order_id=$1 and paid_at is null and kind=$2 and ref_id=$3 and unit_cents=$4 and ml=$5
		for update`, orderID, kind, refID, price, ml).Scan(&existingQty)
	if err == nil {
		if existingQty >= maxOrderLineQty {
			return platform.UserErr(fmt.Sprintf("单行数量最多 %d 份", maxOrderLineQty))
		}
		_, err := tx.Exec(ctx, `update order_items set qty = qty + 1
			where order_id=$1 and paid_at is null and kind=$2 and ref_id=$3 and unit_cents=$4 and ml=$5`,
			orderID, kind, refID, price, ml)
		return err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	_, err = tx.Exec(ctx, `insert into order_items (order_id, kind, ref_id, name, unit_cents, ml)
		values ($1, $2, $3, $4, $5, $6)`, orderID, kind, refID, name, price, ml)
	return err
}

// AddOrderItem 点一下加一件；同样的未结商品合并数量，上限 999。
func AddOrderItem(ctx context.Context, pool *pgxpool.Pool, orderID int64, kind string, refID int64, name string, price, ml int) error {
	if price < 0 || ml < 0 {
		return platform.UserErr("单价和容量不能为负")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := lockOpenOrder(ctx, tx, orderID); err != nil {
		return err
	}
	if err := upsertOrderItemTx(ctx, tx, orderID, kind, refID, name, price, ml); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AddBeerOrderItem 在同一事务里加购在售桶装啤酒（持有开单锁与 FOR SHARE 桶/酒锁，消除加购与下架竞态）。
func AddBeerOrderItem(ctx context.Context, pool *pgxpool.Pool, orderID, kegID int64, ml int) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := lockOpenOrder(ctx, tx, orderID); err != nil {
		return err
	}
	var name string
	var price int
	err = tx.QueryRow(ctx, `
		select case when btrim(b.brewery) <> '' then btrim(b.brewery) || ' · ' || b.name else b.name end,
			case $2::int when 150 then coalesce(b.price_150, 0)
				when 330 then coalesce(b.price_330, 0)
				when 500 then coalesce(b.price_500, 0)
				else 0 end
		from kegs k join beers b on b.id = k.beer_id
		where k.id=$1 and b.on_screen and k.tapped_at is not null and k.finished_at is null
		for share of k, b`, kegID, ml).Scan(&name, &price)
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.UserErr("这桶酒不在售")
	}
	if err != nil {
		return err
	}
	if price <= 0 {
		return platform.UserErr("这桶酒没开这个杯型")
	}
	if err := upsertOrderItemTx(ctx, tx, orderID, "beer", kegID, name, price, ml); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AddRetailOrderItem 在同一事务里加购在架零售商品（持有开单锁与 FOR SHARE 商品锁）。
func AddRetailOrderItem(ctx context.Context, pool *pgxpool.Pool, orderID, retailID int64) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := lockOpenOrder(ctx, tx, orderID); err != nil {
		return err
	}
	var name, spec string
	var price, stock int
	var active, archived bool
	err = tx.QueryRow(ctx, `select name, spec, price_cents, stock, active, archived_at is not null
		from retail_items where id=$1 for share`, retailID).Scan(&name, &spec, &price, &stock, &active, &archived)
	if errors.Is(err, pgx.ErrNoRows) || archived || !active || stock < 1 {
		return platform.UserErr("这件商品不能卖了")
	}
	if err != nil {
		return err
	}
	if spec != "" {
		name += " " + spec
	}
	if err := upsertOrderItemTx(ctx, tx, orderID, "retail", retailID, name, price, 0); err != nil {
		return err
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
		delCt, err := tx.Exec(ctx, `delete from order_items
			where id=$1 and order_id=$2 and paid_at is null`, itemID, orderID)
		if err != nil {
			return err
		}
		if delCt.RowsAffected() > 0 {
			// 若订单已有结账记录且删完最后一条未付明细，则自动关闭账单避免永远滞留「部分结 · 待付 ¥0」。
			if _, err := tx.Exec(ctx, `update orders set closed_at = now()
				where id=$1 and closed_at is null
				  and not exists (select 1 from order_items where order_id=$1 and paid_at is null)
				  and (exists (select 1 from payments where order_id=$1)
				       or exists (select 1 from order_items where order_id=$1 and paid_at is not null))`, orderID); err != nil {
				return err
			}
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
	var kind string
	var refID int64
	var qty int
	err = tx.QueryRow(ctx, `select kind, ref_id, qty from order_items where id=$1 and order_id=$2 and paid_at is null for update`,
		itemID, orderID).Scan(&kind, &refID, &qty)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	switch kind {
	case "beer":
		var sellable bool
		if err := tx.QueryRow(ctx, `select exists(
			select 1 from kegs k join beers b on b.id = k.beer_id
			where k.id=$1 and b.on_screen and k.tapped_at is not null and k.finished_at is null
		)`, refID).Scan(&sellable); err != nil {
			return err
		}
		if !sellable {
			return platform.UserErr("这桶酒已停售或已换桶，请从右侧菜单重新点单")
		}
	case "retail":
		var sellable bool
		if err := tx.QueryRow(ctx, `select exists(
			select 1 from retail_items
			where id=$1 and archived_at is null and active and stock > $2
		)`, refID, qty).Scan(&sellable); err != nil {
			return err
		}
		if !sellable {
			return platform.UserErr("这件商品已下架或库存不足")
		}
	}
	if qty >= maxOrderLineQty {
		return platform.UserErr(fmt.Sprintf("单行数量最多 %d 份", maxOrderLineQty))
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

type PayOptions struct {
	Method             string
	IdempotencyKey     string
	ExpectedTotalCents *int
}

// PayResult 是结账事务提交后的完整摘要，供小票打印扩展与领域事件广播使用。
type PayResult struct {
	PaymentID     int64
	OrderID       int64
	Spot          string
	SpotLabel     string
	AmountCents   int
	ItemsCount    int
	Closed        bool
	Method        string
	SkippedCount  int
	OverSoldCount int
	Replayed      bool
	Lines         []ReceiptLine
	LowStock      []inventory.LowStockItem
}

func normalizePayMethod(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "", "qr":
		return "qr"
	case "cash":
		return "cash"
	case "card":
		return "card"
	case "other":
		return "other"
	default:
		return "qr"
	}
}

func PayItems(ctx context.Context, pool *pgxpool.Pool, orderID int64, ids []int64) (int64, error) {
	res, err := PayItemsWithGateway(ctx, pool, ManualGateway{}, orderID, ids)
	return res.PaymentID, err
}

// PayItemsWithGateway 把勾选的商品结掉：锁住账单，售出、网关确认、支付、标记已结都在同一事务里。
func PayItemsWithGateway(ctx context.Context, pool *pgxpool.Pool, gw PaymentGateway, orderID int64, ids []int64) (PayResult, error) {
	return PayItemsWithOptions(ctx, pool, gw, orderID, ids, PayOptions{})
}

// PayItemsWithOptions 支持幂等键、支付方式、应收金额校验与跨桶/零售确定性升序加锁，彻底消除死锁与重复收款。
func PayItemsWithOptions(ctx context.Context, pool *pgxpool.Pool, gw PaymentGateway, orderID int64, ids []int64, opts PayOptions) (PayResult, error) {
	if len(ids) == 0 {
		return PayResult{}, platform.UserErr("先勾选要结账的商品")
	}
	if gw == nil {
		gw = ManualGateway{}
	}
	method := normalizePayMethod(opts.Method)
	idemKey := strings.TrimSpace(opts.IdempotencyKey)
	if len(idemKey) > 128 {
		idemKey = idemKey[:128]
	}

	seenIDs := make(map[int64]bool, len(ids))
	uniqIDs := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id > 0 && !seenIDs[id] {
			seenIDs[id] = true
			uniqIDs = append(uniqIDs, id)
		}
	}
	if len(uniqIDs) == 0 {
		return PayResult{}, platform.UserErr("先勾选要结账的商品")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return PayResult{}, err
	}
	defer tx.Rollback(ctx)

	var spotKey string
	var closedAt *time.Time
	err = tx.QueryRow(ctx, `select spot, closed_at from orders where id=$1 for update`, orderID).Scan(&spotKey, &closedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PayResult{}, platform.UserErr("找不到这张账单")
	}
	if err != nil {
		return PayResult{}, err
	}
	spotLbl := SpotLabel(spotKey)

	if idemKey != "" {
		var existingPayID int64
		var existingAmt, existingCnt int
		var existingMethod string
		err := tx.QueryRow(ctx, `select id, amount_cents, items_count, method
			from payments where order_id=$1 and idempotency_key=$2`, orderID, idemKey).
			Scan(&existingPayID, &existingAmt, &existingCnt, &existingMethod)
		if err == nil {
			if len(uniqIDs) > 0 {
				var hasSelectedUnpaid bool
				if uerr := tx.QueryRow(ctx, `select exists(
					select 1 from order_items where order_id=$1 and paid_at is null and id = any($2::bigint[])
				)`, orderID, uniqIDs).Scan(&hasSelectedUnpaid); uerr != nil {
					return PayResult{}, uerr
				}
				if hasSelectedUnpaid {
					return PayResult{}, platform.UserErr("页面状态已过期，请刷新账单后再结账")
				}
			}
			if err := tx.Commit(ctx); err != nil {
				return PayResult{}, err
			}
			return PayResult{
				PaymentID:   existingPayID,
				OrderID:     orderID,
				Spot:        spotKey,
				SpotLabel:   spotLbl,
				AmountCents: existingAmt,
				ItemsCount:  existingCnt,
				Closed:      closedAt != nil,
				Method:      existingMethod,
				Replayed:    true,
			}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return PayResult{}, err
		}
	}
	if closedAt != nil {
		return PayResult{}, platform.UserErr("这张单已经结了")
	}

	rows, err := tx.Query(ctx, `select i.id, i.kind, i.ref_id, i.name, i.unit_cents, i.qty, i.ml, i.added_at
		from order_items i
		where i.order_id=$1 and i.paid_at is null and i.id = any($2::bigint[])
		order by i.id
		for update`, orderID, uniqIDs)
	if err != nil {
		return PayResult{}, err
	}
	type line struct {
		id, refID     int64
		kind, name    string
		unit, qty, ml int
		addedAt       time.Time
	}
	var lines []line
	for rows.Next() {
		var l line
		if err := rows.Scan(&l.id, &l.kind, &l.refID, &l.name, &l.unit, &l.qty, &l.ml, &l.addedAt); err != nil {
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

	var skippedCount int
	if err := tx.QueryRow(ctx, `select count(*)::int from order_items
		where order_id=$1 and paid_at is not null and id = any($2::bigint[])`, orderID, uniqIDs).Scan(&skippedCount); err != nil {
		return PayResult{}, err
	}

	total, count := 0, 0
	lineIDs := make([]int64, 0, len(lines))
	receiptLines := make([]ReceiptLine, 0, len(lines))
	var retail []inventory.SaleLine
	kegLockMap := make(map[int64]bool)
	retailLockMap := make(map[int64]bool)
	var kegLockIDs, retailLockIDs []int64

	for _, l := range lines {
		switch l.kind {
		case "beer":
			if !kegLockMap[l.refID] {
				kegLockMap[l.refID] = true
				kegLockIDs = append(kegLockIDs, l.refID)
			}
		case "retail":
			if !retailLockMap[l.refID] {
				retailLockMap[l.refID] = true
				retailLockIDs = append(retailLockIDs, l.refID)
			}
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
	if opts.ExpectedTotalCents != nil && *opts.ExpectedTotalCents != total {
		return PayResult{}, platform.UserErr("账单金额已变动，请刷新核对后再收款")
	}

	// 按固定顺序先锁所有涉及的酒桶，零售 SKU 在 SellRetailTx 内按 ID 升序统一加锁。
	sort.Slice(kegLockIDs, func(i, j int) bool { return kegLockIDs[i] < kegLockIDs[j] })
	kegSnaps, err := inventory.LockKegsForOrderTx(ctx, tx, kegLockIDs)
	if err != nil {
		return PayResult{}, err
	}

	for _, l := range lines {
		if l.kind == "beer" {
			snap, ok := kegSnaps[l.refID]
			if err := inventory.CheckKegOrderSnap(snap, ok, l.qty, l.ml*l.qty, l.unit*l.qty, l.addedAt); err != nil {
				return PayResult{}, err
			}
		}
	}
	var lowStock []inventory.LowStockItem
	if len(retail) > 0 {
		_, lowStock, err = inventory.SellRetailTx(ctx, tx, retail)
		if err != nil {
			return PayResult{}, err
		}
	}

	actor := platform.OperationActor(ctx)
	gwRes, err := gw.Settle(ctx, PaymentRequest{
		OrderID:        orderID,
		SpotLabel:      spotLbl,
		AmountCents:    total,
		ItemsCount:     count,
		Actor:          actor,
		Method:         method,
		IdempotencyKey: idemKey,
	})
	if err != nil {
		return PayResult{}, err
	}
	provider := strings.TrimSpace(gwRes.Provider)
	if provider == "" {
		provider = gw.Name()
	}
	var idemPtr *string
	if idemKey != "" {
		idemPtr = &idemKey
	}

	var payID int64
	if err := tx.QueryRow(ctx, `insert into payments
		(order_id, amount_cents, items_count, actor, method, provider, reference, idempotency_key)
		values ($1, $2, $3, $4, $5, $6, $7, $8) returning id`,
		orderID, total, count, actor, method, provider, strings.TrimSpace(gwRes.Reference), idemPtr).Scan(&payID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return PayResult{}, platform.UserErr("这笔支付已提交，请勿重复操作")
		}
		return PayResult{}, err
	}

	overSoldCount := 0
	for _, l := range lines {
		if l.kind == "beer" {
			over, err := inventory.AddPourForOrderWithSnapTx(ctx, tx, kegSnaps[l.refID], l.qty, l.ml*l.qty, l.unit*l.qty, payID)
			if err != nil {
				return PayResult{}, err
			}
			if over {
				overSoldCount++
			}
		}
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
		PaymentID:     payID,
		OrderID:       orderID,
		Spot:          spotKey,
		SpotLabel:     spotLbl,
		AmountCents:   total,
		ItemsCount:    count,
		Closed:        ct.RowsAffected() > 0,
		Method:        method,
		SkippedCount:  skippedCount,
		OverSoldCount: overSoldCount,
		Lines:         receiptLines,
		LowStock:      lowStock,
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

// ListPosItems 只列能卖的：上架且有库存且未归档。
func ListPosItems(ctx context.Context, pool *pgxpool.Pool) ([]inventory.RetailItem, error) {
	rows, err := pool.Query(ctx, `select id, name, category, spec, price_cents, cost_cents, stock, active
		from retail_items where archived_at is null and active and stock > 0 order by name`)
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
