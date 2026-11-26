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
	"pourops/internal/loyalty"
	"pourops/internal/platform"
	"pourops/internal/settings"
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
	ID              int64
	Kind            string // beer | retail
	Name            string
	UnitCents       int
	Qty             int
	ML              int
	DiscountCents   int
	PaidCents       *int
	PassRedeemedQty int
}

func (i OrderItem) Amount() int { return i.UnitCents * i.Qty }
func (i OrderItem) NetAmount() int {
	if i.PaidCents != nil {
		return *i.PaidCents
	}
	return i.Amount()
}
func (i OrderItem) AmountText() string    { return platform.PriceText(i.Amount()) }
func (i OrderItem) NetAmountText() string { return platform.PriceText(i.NetAmount()) }
func (i OrderItem) QtyText() string       { return fmt.Sprintf("×%d", i.Qty) }
func (i OrderItem) Detail() string {
	if i.Kind == "beer" && i.ML > 0 {
		return fmt.Sprintf("%dml/杯 · %s", i.ML, platform.PriceText(i.UnitCents))
	}
	return platform.PriceText(i.UnitCents)
}

type OrderView struct {
	ID       int64
	Spot     string
	Label    string
	Closed   bool
	MemberID *int64
	Member   *loyalty.MemberSnapshot
	Unpaid   []OrderItem
	Paid     []OrderItem
	Total    int
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
	err := pool.QueryRow(ctx, `select id, spot, closed_at is not null, member_id from orders where id=$1`, id).
		Scan(&o.ID, &o.Spot, &o.Closed, &o.MemberID)
	o.Label = SpotLabel(o.Spot)
	if err != nil {
		return o, err
	}
	if o.MemberID != nil && *o.MemberID > 0 {
		if snap, merr := loyalty.LoadCheckoutMemberSnapshot(ctx, pool, *o.MemberID, 0, time.Now(), false); merr == nil {
			o.Member = snap
		}
	}
	rows, err := pool.Query(ctx, `select id, kind, name, unit_cents, qty, ml, paid_at is null, discount_cents, paid_cents, pass_redeemed_qty
		from order_items where order_id=$1 order by id`, id)
	if err != nil {
		return o, err
	}
	defer rows.Close()
	for rows.Next() {
		var it OrderItem
		var unpaid bool
		if err := rows.Scan(&it.ID, &it.Kind, &it.Name, &it.UnitCents, &it.Qty, &it.ML, &unpaid,
			&it.DiscountCents, &it.PaidCents, &it.PassRedeemedQty); err != nil {
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

// BindOrderMember 给未结台位账单绑定或解绑默认会员（memberID <= 0 表示解绑）。
func BindOrderMember(ctx context.Context, pool *pgxpool.Pool, orderID, memberID int64) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := lockOpenOrder(ctx, tx, orderID); err != nil {
		return err
	}
	if memberID <= 0 {
		if _, err := tx.Exec(ctx, `update orders set member_id = null where id=$1`, orderID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	var active bool
	err = tx.QueryRow(ctx, `select active from members where id=$1`, memberID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.UserErr("找不到这个会员")
	}
	if err != nil {
		return err
	}
	if !active {
		return platform.UserErr("该会员已停用，无法绑定")
	}
	if _, err := tx.Exec(ctx, `update orders set member_id = $2 where id=$1`, orderID, memberID); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
	ID                  int64
	OrderID             int64
	Label               string
	Amount              int
	GrossCents          int
	DiscountCents       int
	PassDiscountCents   int
	MemberDiscountCents int
	ManualDiscountCents int
	PointsDiscountCents int
	BalancePaidCents    int
	ExternalPaidCents   int
	Items               int
	Method              string
	DiscountLabel       string
	DiscountNote        string
	MemberID            *int64
	MemberName          string
	MemberPhone         string
	MemberBalanceAfter  *int
	PointsSpent         int
	PointsEarned        int
	PassCupsUsed        int
}

func (p Payment) AmountText() string { return platform.PriceText(p.Amount) }
func (p Payment) GrossText() string {
	if p.GrossCents <= 0 && p.Amount > 0 {
		return platform.PriceText(p.Amount)
	}
	return platform.PriceText(p.GrossCents)
}
func (p Payment) DiscountText() string     { return platform.PriceText(p.DiscountCents) }
func (p Payment) BalancePaidText() string  { return platform.PriceText(p.BalancePaidCents) }
func (p Payment) ExternalPaidText() string { return platform.PriceText(p.ExternalPaidCents) }
func (p Payment) MemberBalanceText() string {
	if p.MemberBalanceAfter == nil {
		return "—"
	}
	return platform.PriceText(*p.MemberBalanceAfter)
}
func (p Payment) HasDiscount() bool { return p.DiscountCents > 0 || p.DiscountLabel != "" }
func (p Payment) MethodText() string {
	return PayMethodLabel(p.Method)
}

func PayMethodLabel(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "cash":
		return "现金"
	case "card":
		return "刷卡"
	case "balance":
		return "会员储值"
	case "pass":
		return "月卡抵扣"
	case "comp":
		return "免单"
	case "other":
		return "其他"
	default:
		return "扫码"
	}
}

type PayOptions struct {
	Method             string
	IdempotencyKey     string
	ExpectedTotalCents *int
	MemberID           *int64 // nil 表示沿用订单默认会员；<=0 表示本次按散客结账
	MemberPassID       *int64
	UsePass            bool
	PassCupsRequested  int
	DiscountRuleID     *int64
	CustomDiscount     *loyalty.CustomDiscount
	DiscountNote       string
	UsePoints          bool
	UseBalance         bool
	ActorRole          string
	LoyaltyCfg         *settings.LoyaltyConfig
}

// PayResult 是结账事务提交后的完整摘要，供小票打印扩展与领域事件广播使用。
type PayResult struct {
	PaymentID           int64
	OrderID             int64
	Spot                string
	SpotLabel           string
	GrossCents          int
	DiscountCents       int
	AmountCents         int
	PassDiscountCents   int
	MemberDiscountCents int
	ManualDiscountCents int
	PointsDiscountCents int
	BalancePaidCents    int
	ExternalPaidCents   int
	DiscountLabel       string
	MemberID            *int64
	MemberName          string
	MemberPhone         string
	MemberPassID        *int64
	PassCupsUsed        int
	PointsSpent         int
	PointsEarned        int
	ItemsCount          int
	Closed              bool
	Method              string
	SkippedCount        int
	OverSoldCount       int
	Replayed            bool
	Lines               []ReceiptLine
	LowStock            []inventory.LowStockItem
}

func normalizePayMethod(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "", "qr":
		return "qr"
	case "cash":
		return "cash"
	case "card":
		return "card"
	case "balance":
		return "balance"
	case "pass":
		return "pass"
	case "comp":
		return "comp"
	case "other":
		return "other"
	default:
		return "qr"
	}
}

func uniqPositiveIDs(ids []int64) []int64 {
	seen := make(map[int64]bool, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func effectiveLoyaltyConfig(cfg *settings.LoyaltyConfig) settings.LoyaltyConfig {
	if cfg != nil {
		return cfg.Normalized()
	}
	return settings.DefaultLoyaltyConfig()
}

// PreviewCheckout 在只读快照内试算勾选商品的折扣、月卡、积分与储值拆账结果（不锁表不落库）。
func PreviewCheckout(ctx context.Context, pool *pgxpool.Pool, orderID int64, ids []int64, opts PayOptions) (loyalty.PricingResult, *loyalty.MemberSnapshot, error) {
	uniqIDs := uniqPositiveIDs(ids)
	if len(uniqIDs) == 0 {
		return loyalty.PricingResult{}, nil, platform.UserErr("先勾选要结账的商品")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return loyalty.PricingResult{}, nil, err
	}
	defer tx.Rollback(ctx)

	var closedAt *time.Time
	var orderMemberID *int64
	err = tx.QueryRow(ctx, `select closed_at, member_id from orders where id=$1`, orderID).Scan(&closedAt, &orderMemberID)
	if errors.Is(err, pgx.ErrNoRows) {
		return loyalty.PricingResult{}, nil, platform.UserErr("找不到这张账单")
	}
	if err != nil {
		return loyalty.PricingResult{}, nil, err
	}
	if closedAt != nil {
		return loyalty.PricingResult{}, nil, platform.UserErr("这张单已经结了")
	}

	rows, err := tx.Query(ctx, `select id, kind, name, unit_cents, qty, ml
		from order_items
		where order_id=$1 and paid_at is null and id = any($2::bigint[])
		order by id`, orderID, uniqIDs)
	if err != nil {
		return loyalty.PricingResult{}, nil, err
	}
	var lineInputs []loyalty.OrderLineInput
	for rows.Next() {
		var l loyalty.OrderLineInput
		if err := rows.Scan(&l.ItemID, &l.Kind, &l.Name, &l.UnitCents, &l.Qty, &l.ML); err != nil {
			rows.Close()
			return loyalty.PricingResult{}, nil, err
		}
		lineInputs = append(lineInputs, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return loyalty.PricingResult{}, nil, err
	}
	if len(lineInputs) == 0 {
		return loyalty.PricingResult{}, nil, platform.UserErr("这些商品已经结过账了")
	}

	var effMemberID int64
	if opts.MemberID != nil {
		if *opts.MemberID > 0 {
			effMemberID = *opts.MemberID
		}
	} else if orderMemberID != nil && *orderMemberID > 0 {
		effMemberID = *orderMemberID
	}

	var passID int64
	if opts.MemberPassID != nil && *opts.MemberPassID > 0 {
		passID = *opts.MemberPassID
	}
	memberSnap, err := loyalty.LoadCheckoutMemberSnapshot(ctx, tx, effMemberID, passID, time.Now(), false)
	if err != nil {
		return loyalty.PricingResult{}, nil, err
	}

	var rule *loyalty.DiscountRule
	if opts.DiscountRuleID != nil && *opts.DiscountRuleID > 0 {
		r, rerr := loyalty.GetDiscountRule(ctx, tx, *opts.DiscountRuleID)
		if errors.Is(rerr, pgx.ErrNoRows) {
			return loyalty.PricingResult{}, nil, platform.UserErr("找不到所选的折扣规则")
		}
		if rerr != nil {
			return loyalty.PricingResult{}, nil, rerr
		}
		rule = &r
	}

	role := strings.TrimSpace(opts.ActorRole)
	if role == "" {
		role = platform.ActorRole(ctx)
	}

	res, err := loyalty.CalculateCheckout(loyalty.PricingRequest{
		Lines:             lineInputs,
		Member:            memberSnap,
		UsePass:           opts.UsePass,
		PassCupsRequested: opts.PassCupsRequested,
		Rule:              rule,
		Custom:            opts.CustomDiscount,
		DiscountNote:      opts.DiscountNote,
		UsePoints:         opts.UsePoints,
		UseBalance:        opts.UseBalance,
		PayMethod:         normalizePayMethod(opts.Method),
		ActorRole:         role,
		LoyaltyCfg:        effectiveLoyaltyConfig(opts.LoyaltyCfg),
	})
	if err != nil {
		return loyalty.PricingResult{}, memberSnap, err
	}
	return res, memberSnap, tx.Commit(ctx)
}

func PayItems(ctx context.Context, pool *pgxpool.Pool, orderID int64, ids []int64) (int64, error) {
	res, err := PayItemsWithGateway(ctx, pool, ManualGateway{}, orderID, ids)
	return res.PaymentID, err
}

// PayItemsWithGateway 把勾选的商品结掉：锁住账单，售出、网关确认、支付、标记已结都在同一事务里。
func PayItemsWithGateway(ctx context.Context, pool *pgxpool.Pool, gw PaymentGateway, orderID int64, ids []int64) (PayResult, error) {
	return PayItemsWithOptions(ctx, pool, gw, orderID, ids, PayOptions{})
}

// PayItemsWithOptions 支持幂等键、会员/月卡/折扣/积分/储值分账、应收金额校验与全局确定性加锁，消除死锁与重复收款。
// 全局行锁顺序：orders -> order_items(ASC) -> members -> member_passes -> kegs(ASC) -> retail_items(ASC)。
func PayItemsWithOptions(ctx context.Context, pool *pgxpool.Pool, gw PaymentGateway, orderID int64, ids []int64, opts PayOptions) (PayResult, error) {
	uniqIDs := uniqPositiveIDs(ids)
	if len(uniqIDs) == 0 {
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

	tx, err := pool.Begin(ctx)
	if err != nil {
		return PayResult{}, err
	}
	defer tx.Rollback(ctx)

	var spotKey string
	var closedAt *time.Time
	var orderMemberID *int64
	err = tx.QueryRow(ctx, `select spot, closed_at, member_id from orders where id=$1 for update`, orderID).
		Scan(&spotKey, &closedAt, &orderMemberID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PayResult{}, platform.UserErr("找不到这张账单")
	}
	if err != nil {
		return PayResult{}, err
	}
	spotLbl := SpotLabel(spotKey)

	if idemKey != "" {
		var existing PayResult
		existing.OrderID = orderID
		existing.Spot = spotKey
		existing.SpotLabel = spotLbl
		existing.Closed = closedAt != nil
		existing.Replayed = true
		err := tx.QueryRow(ctx, `select id, amount_cents, coalesce(gross_cents, amount_cents), discount_cents,
			pass_discount_cents, member_discount_cents, manual_discount_cents, points_discount_cents,
			balance_paid_cents, coalesce(external_paid_cents, amount_cents), discount_label,
			member_id, member_pass_id, points_spent, points_earned, items_count, method
			from payments where order_id=$1 and idempotency_key=$2`, orderID, idemKey).
			Scan(&existing.PaymentID, &existing.AmountCents, &existing.GrossCents, &existing.DiscountCents,
				&existing.PassDiscountCents, &existing.MemberDiscountCents, &existing.ManualDiscountCents, &existing.PointsDiscountCents,
				&existing.BalancePaidCents, &existing.ExternalPaidCents, &existing.DiscountLabel,
				&existing.MemberID, &existing.MemberPassID, &existing.PointsSpent, &existing.PointsEarned,
				&existing.ItemsCount, &existing.Method)
		if err == nil {
			var hasSelectedUnpaid bool
			if uerr := tx.QueryRow(ctx, `select exists(
				select 1 from order_items where order_id=$1 and paid_at is null and id = any($2::bigint[])
			)`, orderID, uniqIDs).Scan(&hasSelectedUnpaid); uerr != nil {
				return PayResult{}, uerr
			}
			if hasSelectedUnpaid {
				return PayResult{}, platform.UserErr("页面状态已过期，请刷新账单后再结账")
			}
			if err := tx.Commit(ctx); err != nil {
				return PayResult{}, err
			}
			return existing, nil
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

	// 确定本次结账绑定的会员（优先本次显式传参，否则沿用订单默认会员）并锁住 members -> member_passes。
	var effMemberID int64
	if opts.MemberID != nil {
		if *opts.MemberID > 0 {
			effMemberID = *opts.MemberID
		}
	} else if orderMemberID != nil && *orderMemberID > 0 {
		effMemberID = *orderMemberID
	}
	var passID int64
	if opts.MemberPassID != nil && *opts.MemberPassID > 0 {
		passID = *opts.MemberPassID
	}
	now := time.Now()
	memberSnap, err := loyalty.LoadCheckoutMemberSnapshot(ctx, tx, effMemberID, passID, now, true)
	if err != nil {
		return PayResult{}, err
	}

	var rule *loyalty.DiscountRule
	if opts.DiscountRuleID != nil && *opts.DiscountRuleID > 0 {
		r, rerr := loyalty.GetDiscountRule(ctx, tx, *opts.DiscountRuleID)
		if errors.Is(rerr, pgx.ErrNoRows) {
			return PayResult{}, platform.UserErr("找不到所选的折扣规则")
		}
		if rerr != nil {
			return PayResult{}, rerr
		}
		rule = &r
	}

	role := strings.TrimSpace(opts.ActorRole)
	if role == "" {
		role = platform.ActorRole(ctx)
	}

	lineInputs := make([]loyalty.OrderLineInput, len(lines))
	for i, l := range lines {
		lineInputs[i] = loyalty.OrderLineInput{
			ItemID:    l.id,
			Kind:      l.kind,
			Name:      l.name,
			UnitCents: l.unit,
			Qty:       l.qty,
			ML:        l.ml,
		}
	}
	pricing, err := loyalty.CalculateCheckout(loyalty.PricingRequest{
		Lines:             lineInputs,
		Member:            memberSnap,
		UsePass:           opts.UsePass,
		PassCupsRequested: opts.PassCupsRequested,
		Rule:              rule,
		Custom:            opts.CustomDiscount,
		DiscountNote:      opts.DiscountNote,
		UsePoints:         opts.UsePoints,
		UseBalance:        opts.UseBalance,
		PayMethod:         method,
		ActorRole:         role,
		LoyaltyCfg:        effectiveLoyaltyConfig(opts.LoyaltyCfg),
	})
	if err != nil {
		return PayResult{}, err
	}
	if opts.ExpectedTotalCents != nil && *opts.ExpectedTotalCents != pricing.AmountCents {
		return PayResult{}, platform.UserErr("账单金额已变动，请刷新核对后再收款")
	}

	// 若 0 元全免或全额储值扣款，自动修正更准确的支付渠道标记（不强改用户选定的现金/扫码/刷卡除非本来就是 0 元月卡/免单）
	if pricing.AmountCents == 0 && method == "qr" {
		if pricing.PassCupsUsed > 0 && pricing.ManualDiscountCents == 0 {
			method = "pass"
		} else if rule != nil && rule.Kind == "comp" {
			method = "comp"
		}
	}

	count := 0
	lineIDs := make([]int64, 0, len(lines))
	lineDiscs := make([]int, 0, len(lines))
	linePaids := make([]int, 0, len(lines))
	linePassQtys := make([]int, 0, len(lines))
	receiptLines := make([]ReceiptLine, 0, len(lines))
	var retail []inventory.SaleLine
	kegLockMap := make(map[int64]bool)
	var kegLockIDs []int64

	for i, l := range lines {
		lp := pricing.Lines[i]
		netCopy := lp.PaidCents
		switch l.kind {
		case "beer":
			if !kegLockMap[l.refID] {
				kegLockMap[l.refID] = true
				kegLockIDs = append(kegLockIDs, l.refID)
			}
		case "retail":
			retail = append(retail, inventory.SaleLine{
				Item:     inventory.RetailItem{ID: l.refID, Name: l.name, PriceCents: l.unit},
				Qty:      l.qty,
				NetCents: &netCopy,
			})
		default:
			return PayResult{}, platform.UserErr("账单里有不认识的商品")
		}
		lineIDs = append(lineIDs, l.id)
		lineDiscs = append(lineDiscs, lp.DiscountCents)
		linePaids = append(linePaids, lp.PaidCents)
		linePassQtys = append(linePassQtys, lp.PassRedeemedQty)
		receiptLines = append(receiptLines, ReceiptLine{
			Name:      l.name,
			Kind:      l.kind,
			ML:        l.ml,
			Qty:       l.qty,
			UnitCents: l.unit,
		})
		count += l.qty
	}

	// 按固定顺序先锁所有涉及的酒桶，零售 SKU 在 SellRetailTx 内按 ID 升序统一加锁。
	sort.Slice(kegLockIDs, func(i, j int) bool { return kegLockIDs[i] < kegLockIDs[j] })
	kegSnaps, err := inventory.LockKegsForOrderTx(ctx, tx, kegLockIDs)
	if err != nil {
		return PayResult{}, err
	}

	for i, l := range lines {
		if l.kind == "beer" {
			snap, ok := kegSnaps[l.refID]
			checkAmt := pricing.Lines[i].PaidCents
			if l.ml == 0 && checkAmt == 0 {
				checkAmt = l.unit * l.qty
			}
			if err := inventory.CheckKegOrderSnap(snap, ok, l.qty, l.ml*l.qty, checkAmt, l.addedAt); err != nil {
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

	var effMemberPtr *int64
	var memberName, memberPhone string
	if memberSnap != nil {
		idCopy := memberSnap.ID
		effMemberPtr = &idCopy
		memberName = memberSnap.Name
		memberPhone = memberSnap.Phone
	}

	actor := platform.OperationActor(ctx)
	gwRes, err := gw.Settle(ctx, PaymentRequest{
		OrderID:           orderID,
		SpotLabel:         spotLbl,
		GrossCents:        pricing.GrossCents,
		DiscountCents:     pricing.DiscountCents,
		AmountCents:       pricing.AmountCents,
		BalancePaidCents:  pricing.BalancePaidCents,
		ExternalPaidCents: pricing.ExternalPaidCents,
		ItemsCount:        count,
		Actor:             actor,
		Method:            method,
		IdempotencyKey:    idemKey,
		MemberID:          effMemberPtr,
		DiscountLabel:     pricing.DiscountLabel,
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
		(order_id, amount_cents, items_count, actor, method, provider, reference, idempotency_key,
		 member_id, gross_cents, discount_cents, pass_discount_cents, member_discount_cents,
		 manual_discount_cents, points_discount_cents, balance_paid_cents, external_paid_cents,
		 discount_rule_id, discount_label, discount_note, member_pass_id, points_spent, points_earned)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
		returning id`,
		orderID, pricing.AmountCents, count, actor, method, provider, strings.TrimSpace(gwRes.Reference), idemPtr,
		effMemberPtr, pricing.GrossCents, pricing.DiscountCents, pricing.PassDiscountCents, pricing.MemberDiscountCents,
		pricing.ManualDiscountCents, pricing.PointsDiscountCents, pricing.BalancePaidCents, pricing.ExternalPaidCents,
		pricing.DiscountRuleID, pricing.DiscountLabel, pricing.DiscountNote, pricing.MemberPassID,
		pricing.PointsSpent, pricing.PointsEarned).Scan(&payID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return PayResult{}, platform.UserErr("这笔支付已提交，请勿重复操作")
		}
		return PayResult{}, err
	}

	overSoldCount := 0
	for i, l := range lines {
		if l.kind == "beer" {
			over, err := inventory.AddPourForOrderWithSnapTx(ctx, tx, kegSnaps[l.refID], l.qty, l.ml*l.qty, pricing.Lines[i].PaidCents, payID)
			if err != nil {
				return PayResult{}, err
			}
			if over {
				overSoldCount++
			}
		}
	}

	if err := loyalty.ApplyCheckoutTx(ctx, tx, loyalty.CheckoutApplyRequest{
		OrderID:   orderID,
		PaymentID: payID,
		SpotLabel: spotLbl,
		Member:    memberSnap,
		Pricing:   pricing,
		Now:       now,
	}); err != nil {
		return PayResult{}, err
	}

	if _, err := tx.Exec(ctx, `update order_items oi
		set paid_at = now(), payment_id = $2,
		    discount_cents = v.disc, paid_cents = v.paid, pass_redeemed_qty = v.pass_qty
		from unnest($3::bigint[], $4::int[], $5::int[], $6::int[]) as v(id, disc, paid, pass_qty)
		where oi.id = v.id and oi.order_id = $1 and oi.paid_at is null`,
		orderID, payID, lineIDs, lineDiscs, linePaids, linePassQtys); err != nil {
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
		PaymentID:           payID,
		OrderID:             orderID,
		Spot:                spotKey,
		SpotLabel:           spotLbl,
		GrossCents:          pricing.GrossCents,
		DiscountCents:       pricing.DiscountCents,
		AmountCents:         pricing.AmountCents,
		PassDiscountCents:   pricing.PassDiscountCents,
		MemberDiscountCents: pricing.MemberDiscountCents,
		ManualDiscountCents: pricing.ManualDiscountCents,
		PointsDiscountCents: pricing.PointsDiscountCents,
		BalancePaidCents:    pricing.BalancePaidCents,
		ExternalPaidCents:   pricing.ExternalPaidCents,
		DiscountLabel:       pricing.DiscountLabel,
		MemberID:            effMemberPtr,
		MemberName:          memberName,
		MemberPhone:         memberPhone,
		MemberPassID:        pricing.MemberPassID,
		PassCupsUsed:        pricing.PassCupsUsed,
		PointsSpent:         pricing.PointsSpent,
		PointsEarned:        pricing.PointsEarned,
		ItemsCount:          count,
		Closed:              ct.RowsAffected() > 0,
		Method:              method,
		SkippedCount:        skippedCount,
		OverSoldCount:       overSoldCount,
		Lines:               receiptLines,
		LowStock:            lowStock,
	}, nil
}

func LoadPayment(ctx context.Context, pool *pgxpool.Pool, id int64) (Payment, error) {
	var p Payment
	var spotKey string
	var memberName, memberPhone *string
	err := pool.QueryRow(ctx, `select p.id, p.order_id, p.amount_cents,
		coalesce(p.gross_cents, p.amount_cents), p.discount_cents,
		p.pass_discount_cents, p.member_discount_cents, p.manual_discount_cents, p.points_discount_cents,
		p.balance_paid_cents, coalesce(p.external_paid_cents, p.amount_cents),
		p.items_count, p.method, p.discount_label, p.discount_note,
		p.member_id, m.name, m.phone, m.balance_cents,
		p.points_spent, p.points_earned,
		coalesce((select sum(u.cups)::int from member_pass_usages u where u.payment_id = p.id), 0),
		o.spot
		from payments p
		join orders o on o.id = p.order_id
		left join members m on m.id = p.member_id
		where p.id=$1`, id).
		Scan(&p.ID, &p.OrderID, &p.Amount, &p.GrossCents, &p.DiscountCents,
			&p.PassDiscountCents, &p.MemberDiscountCents, &p.ManualDiscountCents, &p.PointsDiscountCents,
			&p.BalancePaidCents, &p.ExternalPaidCents,
			&p.Items, &p.Method, &p.DiscountLabel, &p.DiscountNote,
			&p.MemberID, &memberName, &memberPhone, &p.MemberBalanceAfter,
			&p.PointsSpent, &p.PointsEarned, &p.PassCupsUsed, &spotKey)
	p.Label = SpotLabel(spotKey)
	if memberName != nil {
		p.MemberName = *memberName
	}
	if memberPhone != nil {
		p.MemberPhone = *memberPhone
	}
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
